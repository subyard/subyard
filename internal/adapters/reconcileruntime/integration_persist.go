package reconcileruntime

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/shellquote"
)

// The persist transfer runs as a separate guest program. It never reads or
// writes the integration inventory; its state is derived from the filesystem
// and the one transfer descriptor beside each source directory.
//
//go:embed integration_persist.py
var integrationPersistProgram string

// PersistMapping is one declared directory persist link of a catalog integration.
type PersistMapping struct {
	Integration string `json:"integration"`
	Source      string `json:"source"`
	Target      string `json:"target"`
	Selected    bool   `json:"selected"`
}

// Guest text is evidence for lookups and comparisons. Formatting it must
// never disclose its contents, even if a whole response is logged by mistake.
type persistGuestText struct{ value string }

func (field *persistGuestText) UnmarshalJSON(data []byte) error {
	return json.Unmarshal(data, &field.value)
}

func (persistGuestText) Format(out fmt.State, _ rune) {
	_, _ = out.Write([]byte("[guest evidence]"))
}

type persistGuestEntry struct {
	Source        persistGuestText   `json:"source"`
	Ops           []persistGuestText `json:"ops"`
	Persist       persistGuestText   `json:"persist"`
	ExistingCount int                `json:"existing_count"`
	Retirable     bool               `json:"retirable"`
	evidence      [sha256.Size]byte
}

func (entry *persistGuestEntry) UnmarshalJSON(data []byte) error {
	type wireEntry persistGuestEntry
	if err := json.Unmarshal(data, (*wireEntry)(entry)); err != nil {
		return err
	}
	entry.evidence = sha256.Sum256(data)
	return nil
}

type persistGuestObservation struct {
	Fingerprint persistGuestText    `json:"fingerprint"`
	Entries     []persistGuestEntry `json:"entries"`
}

type persistEntry struct {
	PersistMapping
	item          integrationWorkItem
	Ops           []string
	Persist       string
	ExistingCount int
	// Retirable marks the exact own link of a deselected mapping.
	Retirable bool
	evidence  [sha256.Size]byte
}

type persistObservation struct {
	Fingerprint string
	Entries     []persistEntry
}

// The public outcome uses only the counts of discarded or retained entries.
type persistGuestOutcome struct {
	Source         persistGuestText   `json:"source"`
	NotTransferred []persistGuestText `json:"not_transferred"`
	Retained       []persistGuestText `json:"retained"`
}

// PersistRetained is a backup or stage that a finished transfer could not
// remove. It is state with a manual next step; it never blocks anything.
type PersistRetained struct {
	Integration string `json:"integration"`
	Kind        string `json:"kind"`
	Path        string `json:"path"`
	Bytes       *int64 `json:"bytes"`
}

func (observation persistObservation) changed() bool {
	return slices.ContainsFunc(observation.Entries, func(entry persistEntry) bool { return len(entry.Ops) != 0 })
}

// A batch failure can name an integration only if all planned effects
// belong to that same integration.
func (observation persistObservation) changedIntegration() string {
	id := ""
	for _, entry := range observation.Entries {
		if len(entry.Ops) == 0 {
			continue
		}
		if entry.item.Owner == "" || id != "" && id != entry.item.Owner {
			return ""
		}
		id = entry.item.Owner
	}
	return id
}

// retireUnrecordedLinks plans removing the exact link of a deselected mapping
// that the artifact inventory does not record: an adoption cut after its
// exchange but before the inventory apply. A recorded link is retired by the
// inventory itself.
func (observation persistObservation) retireUnrecordedLinks(recorded []integrationArtifact) persistObservation {
	owned := map[string]bool{}
	for _, entry := range recorded {
		if entry.Kind == "link" {
			owned[entry.Path] = true
		}
	}
	entries := slices.Clone(observation.Entries)
	for index, entry := range entries {
		if entry.Retirable && !owned[entry.Source] {
			entries[index].Ops = append(slices.Clone(entry.Ops), "retire")
		}
	}
	observation.Entries = entries
	return observation
}

// retireSources names the links whose removal the plan approved.
func (observation persistObservation) retireSources() []string {
	sources := []string{}
	for _, entry := range observation.Entries {
		if slices.Contains(entry.Ops, "retire") {
			sources = append(sources, entry.Source)
		}
	}
	return sources
}

// The inventory records exactly the links this transfer publishes or already published.
func (observation persistObservation) projection() map[string]string {
	result := map[string]string{}
	for _, entry := range observation.Entries {
		if entry.Persist != "" {
			result[entry.Source] = entry.Persist
		}
	}
	return result
}

func (observation persistObservation) facts() []integrationFact {
	var facts []integrationFact
	for _, entry := range observation.Entries {
		if len(entry.Ops) == 0 {
			continue
		}
		step := persistStep{entry.Ops, entry.Source, entry.Target, entry.Selected, entry.ExistingCount}
		desired, _ := json.Marshal(step)
		facts = append(facts, integrationFact{item: entry.item, Key: "persist:" + strings.Join(entry.Ops, "+") + ":" + entry.Source,
			Desired: string(desired), Observed: hex.EncodeToString(entry.evidence[:]), persist: &step})
	}
	return facts
}

// persistStep is the public disclosure bound in one persist plan fact.
type persistStep struct {
	Ops           []string `json:"ops"`
	Source        string   `json:"source"`
	Target        string   `json:"target"`
	Selected      bool     `json:"selected"`
	ExistingCount int      `json:"existing_count"`
}

// text states what this step does to the operator's data.
func (step persistStep) text() string {
	var parts []string
	for _, op := range step.Ops {
		switch op {
		case "adopt":
			text := "copy " + step.Source + " into " + step.Target + " without replacing target entries, then atomically exchange it for a link to that target and delete the original directory"
			switch {
			case step.ExistingCount == 1:
				text += "; 1 entry already exists in the target and will not be transferred; its source copy is deleted with the backup"
			case step.ExistingCount > 1:
				text += fmt.Sprintf("; %d entries already exist in the target and will not be transferred; their source copies are deleted with the backup", step.ExistingCount)
			}
			parts = append(parts, text)
		case "finish":
			text := "finish the completed exchange of " + step.Source + " by removing its stage copies and the original directory"
			if !step.Selected {
				text += ", then remove the link; the data remains in " + step.Target
			}
			parts = append(parts, text)
		case "cancel":
			parts = append(parts, "cancel the interrupted adoption of "+step.Source+": the directory is unchanged, staged copies are discarded and copies already published to the target are kept")
		case "retire":
			parts = append(parts, "remove the link "+step.Source+" to "+step.Target+" that an interrupted adoption published without recording it; the data remains in "+step.Target)
		}
	}
	return strings.Join(parts, "; ")
}

// PersistMappings lists every declared directory persist link of the catalog
// integrations. Selected ones come from the desired link entries.
func (runtime Runtime) persistMappings(entries []integrationArtifact) ([]PersistMapping, error) {
	home := "/home/" + runtime.devUser()
	result := []PersistMapping{}
	selected := map[string]bool{}
	for _, entry := range entries {
		if entry.Kind == "link" && !entry.FileTarget {
			result = append(result, PersistMapping{Integration: entry.ID, Source: entry.Path, Target: entry.Target, Selected: true})
			selected[entry.Path] = true
		}
	}
	definitions, err := runtime.integrationDefinitions()
	if err != nil {
		return nil, err
	}
	explicit := map[string]bool{}
	if runtime.environmentValue("INTEGRATION_HOST_LINKS") == "" {
		for _, link := range strings.Fields(runtime.environmentValue("HOST_LINKS")) {
			explicit[home+"/"+strings.Split(link, ":")[0]] = true
		}
	}
	for _, definition := range definitions {
		for _, link := range strings.Fields(runtime.environmentValue("AGENT_" + definition.Name + "_PERSIST")) {
			parts := strings.Split(link, ":")
			if len(parts) != 2 {
				continue
			}
			relative, err := safeGuestRelativePath(parts[0])
			if err != nil {
				continue
			}
			path := home + "/" + relative
			if selected[path] || explicit[path] {
				continue
			}
			selected[path] = true
			result = append(result, PersistMapping{Integration: definition.Name, Source: path, Target: parts[1]})
		}
	}
	slices.SortFunc(result, func(left, right PersistMapping) int { return strings.Compare(left.Source, right.Source) })
	return result, nil
}

func (runtime Runtime) runPersist(ctx context.Context, mode string, mappings []PersistMapping, extra map[string]any, item integrationWorkItem) ([]byte, error) {
	uid := runtime.Yard.DevUID
	if uid <= 0 {
		uid = 1000
	}
	request := map[string]any{"yard": runtime.Yard.YardName, "uid": uid, "developer": runtime.devUser(), "mappings": mappings, "start": !runtime.PersistFinishOnly}
	for name, value := range extra {
		request[name] = value
	}
	payload, _ := json.Marshal(request)
	result, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, ports.InstanceExecRequest{Command: []string{"python3", "-B", "-c", integrationPersistProgram, mode}, Stdin: payload})
	if err == nil && result.ExitCode == 0 {
		return result.Stdout, nil
	}
	if refusal := ownershipRefusal("persist transfer evidence is unavailable; data was preserved", result, err); !IntegrationOwnershipConflict(refusal) {
		if item.Input != "" {
			return nil, item.failure(refusal, ": "+refusal.Error())
		}
		return nil, refusal
	}
	return nil, runtime.persistRefusal(result.Stderr, mappings, item)
}

func (runtime Runtime) observePersist(ctx context.Context, entries []integrationArtifact, phase string) (persistObservation, error) {
	item := runtime.integrationRead(phase, "persist", "")
	mappings, err := runtime.persistMappings(entries)
	if err != nil || len(mappings) == 0 {
		return persistObservation{}, item.refusal(err)
	}
	owner := mappings[0].Integration
	if !slices.ContainsFunc(mappings, func(mapping PersistMapping) bool { return mapping.Integration != owner }) {
		item = item.withOwner(owner)
	}
	output, err := runtime.runPersist(ctx, "observe", mappings, nil, item)
	var guest persistGuestObservation
	if err != nil {
		// A refused transfer can still supply inventory projections.
		var refused persistRefused
		if errors.As(err, &refused) {
			guest.Entries, err = refused.Entries, refused.error
		}
	} else if json.Unmarshal(output, &guest) != nil || !validPersistHex(guest.Fingerprint.value, sha256.Size) {
		guest = persistGuestObservation{}
		err = item.refusal(errors.New("invalid persist transfer observation"))
	}
	return guest.bind(mappings, item, err != nil), err
}

func validPersistHex(value string, size int) bool {
	return len(value) == size*2 && strings.IndexFunc(value, func(char rune) bool {
		return !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f')
	}) == -1
}

// bind resolves guest evidence to host declarations before the observation can
// be used by a plan, a diagnostic or an inventory projection.
func (guest persistGuestObservation) bind(mappings []PersistMapping, item integrationWorkItem, refused bool) persistObservation {
	result := persistObservation{Fingerprint: guest.Fingerprint.value}
	declared := make(map[string]PersistMapping, len(mappings))
	for _, mapping := range mappings {
		declared[mapping.Source] = mapping
	}
	for _, evidence := range guest.Entries {
		mapping, known := declared[evidence.Source.value]
		if !known {
			continue
		}
		delete(declared, mapping.Source)
		entry := persistEntry{PersistMapping: mapping, ExistingCount: evidence.ExistingCount, Retirable: !mapping.Selected && evidence.Retirable, evidence: evidence.evidence}
		switch evidence.Persist.value {
		case "projected":
			entry.Persist = "projected"
		case "deferred":
			entry.Persist = "deferred"
		}
		if entry.Persist != "deferred" {
			entry.item = item.withOwner(mapping.Integration).atDestination(mapping.Source)
		}
		for _, op := range evidence.Ops {
			switch op.value {
			case "adopt":
				entry.Ops = append(entry.Ops, "adopt")
			case "finish":
				entry.Ops = append(entry.Ops, "finish")
			case "cancel":
				entry.Ops = append(entry.Ops, "cancel")
			}
		}
		result.Entries = append(result.Entries, entry)
	}
	if refused {
		// Skip inventory reads for paths whose persist evidence was unavailable.
		// The remainder of the inventory can still be observed.
		for _, mapping := range mappings {
			if _, missing := declared[mapping.Source]; missing {
				result.Entries = append(result.Entries, persistEntry{PersistMapping: mapping, Persist: "deferred"})
			}
		}
	}

	return result
}

func (runtime Runtime) applyPersist(ctx context.Context, entries []integrationArtifact, approved persistObservation) error {
	if !approved.changed() {
		return nil
	}
	item := runtime.integrationItem("apply persist", approved.changedIntegration())
	mappings, err := runtime.persistMappings(entries)
	if err != nil {
		return item.refusal(err)
	}
	output, err := runtime.runPersist(ctx, "apply", mappings, map[string]any{"expected": approved.Fingerprint, "retire": approved.retireSources()}, item)
	if err != nil {
		return err
	}
	var applied struct {
		Results []persistGuestOutcome `json:"results"`
	}
	if json.Unmarshal(output, &applied) != nil {
		return item.refusal(errors.New("invalid persist transfer result"))
	}
	if runtime.Stderr != nil {
		for _, line := range persistOutcomeLines(applied.Results, mappings) {
			fmt.Fprintln(runtime.Stderr, line)
		}
	}
	return nil
}

// persistOutcomeLines states, bounded, what a transfer left behind.
func persistOutcomeLines(results []persistGuestOutcome, mappings []PersistMapping) []string {
	var lines []string
	for _, result := range results {
		index := slices.IndexFunc(mappings, func(mapping PersistMapping) bool { return mapping.Source == result.Source.value })
		if index < 0 {
			continue
		}
		source := mappings[index].Source
		if count := len(result.NotTransferred); count != 0 {
			lines = append(lines, fmt.Sprintf("persist %q: %d entries not transferred because the target already had them", source, count))
		}
		if count := len(result.Retained); count != 0 {
			lines = append(lines, fmt.Sprintf("persist %q: %d backup or stage paths retained; see yard integration status", source, count))
		}
	}
	return lines
}

// ReadPersistRetained lists, read-only, backups and stages that no current
// transfer names, for every declared persist mapping.
func (runtime Runtime) ReadPersistRetained(ctx context.Context) ([]PersistRetained, error) {
	mappings, err := runtime.persistMappings(nil)
	if err != nil || len(mappings) == 0 {
		return nil, err
	}
	output, err := runtime.runPersist(ctx, "retained", mappings, nil, integrationWorkItem{})
	if err != nil {
		return nil, err
	}
	var report struct {
		Retained []struct {
			Kind, Path persistGuestText
			Bytes      *int64
		} `json:"retained"`
	}
	if json.Unmarshal(output, &report) != nil {
		return nil, errors.New("invalid persist retained listing")
	}
	retained := []PersistRetained{}
	for _, evidence := range report.Retained {
		for _, mapping := range mappings {
			var kind, prefix string
			switch evidence.Kind.value {
			case "backup":
				kind, prefix = "backup", mapping.Source+".subyard-backup-"
			case "stage":
				kind, prefix = "stage", filepath.Dir(mapping.Target)+"/.subyard-stage-"+runtime.Yard.YardName+"-"
			}
			id, matched := strings.CutPrefix(evidence.Path.value, prefix)
			if prefix != "" && matched && validPersistHex(id, 8) {
				retained = append(retained, PersistRetained{Integration: mapping.Integration, Kind: kind, Path: prefix + id, Bytes: evidence.Bytes})
				break
			}
		}
	}
	return retained, nil
}

// PersistRetainedLines states retained data with a manual next step, never as a warning.
func (runtime Runtime) PersistRetainedLines(retained []PersistRetained) []string {
	var lines []string
	for _, item := range retained {
		size := "size unknown"
		if item.Bytes != nil {
			size = fmt.Sprintf("%d bytes", *item.Bytes)
		}
		inspect := shellquote.Command([]string{"incus", "exec", runtime.Yard.YardInstanceName, "--project", runtime.Yard.IncusProject, "--", "ls", "-la", "--", item.Path})
		lines = append(lines, fmt.Sprintf("persist retained %s %s (%s): an adoption could not remove it. Next, inspect the directory on the owner host: %s; remove the retained data manually once it is no longer needed", item.Kind, item.Path, size, inspect))
	}
	return lines
}

// checkApprovedPersist binds execution to the confirmed persist plan: S and T
// identities, operations and the disclosed existing entries are all in the
// fingerprint. A fresh observation without work needs no binding.
func (runtime Runtime) checkApprovedPersist(fresh persistObservation) error {
	if runtime.ApprovedPersist == nil || !fresh.changed() || fresh.Fingerprint == *runtime.ApprovedPersist {
		return nil
	}
	item := runtime.integrationItem("apply persist approval", fresh.changedIntegration())
	message, recovery := "persist transfer state changed after the init plan was confirmed; nothing was transferred", "yard init"
	if item.yard != "" {
		message = fmt.Sprintf("persist transfer state in yard %s changed after the init plan was confirmed; nothing was transferred", item.yard)
		recovery = shellquote.Command([]string{"yard", "-Y", item.yard, "init"})
	}
	if item.Owner != "" {
		message = item.label() + ": " + message
	}
	item.Message, item.Recovery = message, recovery
	return integrationFailure{Finding: item.Finding, cause: domain.ErrPlanStale, pending: true}
}

// PersistFingerprint is the persist observation an operator confirms with this plan.
func (plan IntegrationPlan) PersistFingerprint() string {
	if !plan.persist.changed() {
		return ""
	}
	return plan.persist.Fingerprint
}

type persistRefused struct {
	error
	Entries []persistGuestEntry
}

func (err persistRefused) Unwrap() error { return err.error }

// persistConflictMapping resolves evidence against the request, even when the
// guest returned no entries. Only declared paths enter the public diagnostic.
func persistConflictMapping(mappings []PersistMapping, reportedOwner, reportedPath persistGuestText) (PersistMapping, string) {
	owner, path := reportedOwner.value, reportedPath.value
	var matched PersistMapping
	var matchedPath string
	for _, mapping := range mappings {
		if owner != "" && mapping.Integration != owner {
			continue
		}
		for _, root := range []string{mapping.Source, mapping.Target} {
			if path == root || strings.HasPrefix(path, root+"/") || path == mapping.Source+".subyard-transfer.json" && root == mapping.Source {
				if len(root) > len(matchedPath) {
					matched, matchedPath = mapping, root
				}
			}
		}
	}
	if matchedPath != "" {
		return matched, matchedPath
	}
	var matches int
	for _, mapping := range mappings {
		if owner == "" || owner == mapping.Integration {
			if matches != 0 && matched.Integration != mapping.Integration {
				return PersistMapping{}, ""
			}
			matched = mapping
			matches++
		}
	}
	if matches > 1 {
		return PersistMapping{Integration: matched.Integration}, ""
	}
	return matched, ""
}

var persistInspectionReasons = map[string]string{
	"unsafe persist data":          "unsafe persist data",
	"unsupported persist entry":    "unsupported persist entry",
	"unsafe persist target":        "unsafe persist target",
	"session mount is unavailable": "session mount is unavailable",
	"persist filesystem error":     "persist filesystem error",
	"persist target entry refused": "persist target entry refused",
	"persist target filesystem":    "persist target filesystem",
}

// Resolve a refused child against the current tree by identity. For target
// entries that could not be created, the corresponding source name can still
// identify the path. The command receives only declared paths and a digest.
func persistEntryInspection(mapping PersistMapping, root, evidence string, mount bool) []string {
	roots := []string{"find", root}
	if root == mapping.Target && mapping.Source != root {
		roots = append(roots, mapping.Source)
	}
	command := "namei -l --"
	if mount {
		command = "findmnt --target"
	}
	script := `root=$1; source=$2; expected=$3; shift 3; for path do candidate=$path; ` +
		`case "$path" in "$source"/*) if [ "$root" != "$source" ]; then candidate=$root/${path#"$source"/}; ` +
		`if [ -e "$candidate" ] || [ -L "$candidate" ]; then continue; fi; fi;; esac; ` +
		`digest=$(printf %s "$candidate" | sha256sum); ` +
		`if [ "${digest%% *}" = "$expected" ]; then ` + command + ` "$candidate"; fi; done`
	return append(roots, "-xdev", "-exec", "sh", "-c", script, "subyard", root,
		mapping.Source, fmt.Sprintf("%x", sha256.Sum256([]byte(evidence))), "{}", "+")
}

// Only typed persist conflicts are pending configuration.
func (runtime Runtime) persistRefusal(stderr []byte, mappings []PersistMapping, item integrationWorkItem) error {
	var report struct {
		Conflicts []struct{ Reason, Path, Integration persistGuestText } `json:"conflicts"`
		Entries   []persistGuestEntry                                    `json:"entries"`
		Error     persistGuestText                                       `json:"error"`
	}
	yard, instance, project := runtime.Yard.YardName, runtime.Yard.YardInstanceName, runtime.Yard.IncusProject
	status := shellquote.Command([]string{"yard", "-Y", yard, "integration", "status"})
	if json.Unmarshal(stderr, &report) != nil || len(report.Conflicts) == 0 {
		message := "persist transfer program failed; data was preserved"
		if report.Error.value == "MemoryError" {
			message = "persist transfer program failed (MemoryError); data was preserved"
		}
		if item.Input == "" {
			return errors.New(message)
		}
		return item.failure(errors.New(message), ": "+message)
	}
	inspect := func(command ...string) string {
		return shellquote.RecoveryCommand(append([]string{"incus", "exec", instance, "--project", project, "--"}, command...))
	}
	causes := []error{}
	for index, conflict := range report.Conflicts {
		mapping, path := persistConflictMapping(mappings, conflict.Integration, conflict.Path)
		child := path != "" && strings.HasPrefix(conflict.Path.value, path+"/")
		finding := item
		if mapping.Integration != "" {
			finding = finding.withOwner(mapping.Integration)
		}
		if path == "" && conflict.Path.value != "" {
			path = mapping.Source
		}
		message, retry := "", status
		switch reason := conflict.Reason.value; {
		case reason == "persist adoption pending":
			message = "persist adoption is pending: release activation does not start data transfers"
			retry = shellquote.Command([]string{"yard", "-Y", yard, "init"})
		case persistInspectionReasons[reason] != "":
			message = persistInspectionReasons[reason]
			if child && !runtime.PersistFinishOnly {
				retry = inspect(persistEntryInspection(mapping, path, conflict.Path.value, false)...)
			} else if path != "" {
				retry = inspect("namei", "-l", "--", path)
			}
		case reason == "persist mount boundary":
			message = "a mount point lies inside persist data"
			if child && !runtime.PersistFinishOnly {
				retry = inspect(persistEntryInspection(mapping, path, conflict.Path.value, true)...)
			} else if path != "" {
				retry = inspect("findmnt", "--submounts", "--target", path)
			}
		case reason == "persist paths overlap":
			message = "persist paths overlap"
			if finding.Owner != "" {
				retry = shellquote.Command([]string{"yard", "-Y", yard, "config", "show", "AGENT_" + finding.Owner + "_PERSIST"})
			}
		case reason == "persist space unavailable":
			message = "not enough space on the sessions volume"
			if path != "" {
				retry = inspect("df", "-h", "--", filepath.Dir(path))
			}
		case reason == "atomic exchange unsupported" || reason == "no-replace rename unsupported":
			message = "atomic exchange unsupported; nothing was copied"
			if reason == "no-replace rename unsupported" {
				message = "no-replace rename unsupported; nothing was copied"
			}
			if finding.Owner != "" && !runtime.PersistFinishOnly {
				retry = shellquote.Command([]string{"yard", "-Y", yard, "integration", "disable", finding.Owner})
			}
		case reason == "persist transfer identity unavailable":
			message = "the transfer descriptor is not a complete Subyard record"
			if mapping.Source != "" {
				path = mapping.Source + ".subyard-transfer.json"
				retry = inspect("namei", "-l", "--", path)
			}
		case reason == "integration plan is stale":
			causes = append(causes, finding.atDestination(path).failure(domain.ErrPlanStale, ": persist transfer state changed after planning"))
			continue
		default:
			message = "unrecognized persist transfer refusal"
		}
		if child && (persistInspectionReasons[conflict.Reason.value] != "" || conflict.Reason.value == "persist mount boundary") {
			message += fmt.Sprintf(" for entry %d under %q", index+1, path)
		} else if path != "" {
			message += fmt.Sprintf(" at %q", path)
		}
		finding.Message = message + "; data was preserved"
		if finding.Input != "" {
			finding.Message = finding.label() + ": " + finding.Message
		}
		finding.Input += fmt.Sprintf(": %x", sha256.Sum256([]byte(conflict.Reason.value+"\x00"+conflict.Path.value)))
		finding.Recovery = retry
		causes = append(causes, integrationFailure{Finding: finding.Finding, ownership: true, pending: true})
	}
	cause := domain.CollectRefusals(causes...)
	if len(report.Entries) != 0 {
		return persistRefused{cause, report.Entries}
	}
	return cause
}

// InspectPersistTransfers discloses pending transfers for an already running
// guest. Inspected reports whether the guest was observed: only then does the
// result bind execution (a stopped guest, or one whose integration substrate
// provisioning must first recreate, is observed while provisioning).
func (runtime Runtime) InspectPersistTransfers(ctx context.Context) (plan IntegrationPlan, inspected bool, err error) {
	// Before init installs Incus there is no yard whose data could be moved.
	if ready, err := runtime.incusReady(ctx); err != nil || !ready {
		return IntegrationPlan{}, false, err
	}
	state, err := runtime.reconcileState(ctx)
	if err != nil {
		return IntegrationPlan{}, false, err
	}
	if !state.InstanceFound || !strings.EqualFold(state.Instance.Status, "running") {
		return IntegrationPlan{}, false, nil
	}
	entries, err := runtime.integrationArtifactsPhase("observe")
	if err != nil {
		return IntegrationPlan{}, false, err
	}
	observation, err := runtime.observePersist(ctx, entries, "inspect")
	return persistInspectionPlan(observation, err, func() (IntegrationPlan, error) { return runtime.IntegrationPlan(ctx) })
}

// persistInspectionPlan returns no plan when the raw observation has no work.
// A removable link is work only once the full plan applies the inventory
// narrowing, so it falls through to that plan, which discloses and binds it.
// Inspected is false when the plan cannot bind execution yet.
func persistInspectionPlan(observation persistObservation, err error, full func() (IntegrationPlan, error)) (IntegrationPlan, bool, error) {
	if err == nil && !observation.changed() && !slices.ContainsFunc(observation.Entries, func(entry persistEntry) bool { return entry.Retirable }) {
		return IntegrationPlan{}, true, nil
	}
	plan, err := full()
	if onlyMissingIntegrationSubstrate(err) {
		return IntegrationPlan{}, false, nil
	}
	return plan, err == nil, err
}

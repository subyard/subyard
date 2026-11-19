package reconcileruntime

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

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

// persistEntry is one observed transfer, resolved against its declared
// mapping: integration, paths and selection are the host's declared values.
type persistEntry struct {
	Integration string    `json:"integration"`
	Source      string    `json:"source"`
	Target      string    `json:"target"`
	Selected    bool      `json:"selected"`
	Ops         []string  `json:"ops"`
	ID          guestText `json:"id"`
	State       string    `json:"state"`
	Persist     string    `json:"persist"`
	Files       int       `json:"files"`
	Bytes       int64     `json:"bytes"`
	// Existing target entry names stay opaque; only their count is shown.
	Existing      []guestText `json:"existing"`
	ExistingCount int         `json:"existing_count"`
	// Retirable marks the exact own link of a deselected mapping.
	Retirable bool `json:"retirable"`
}

type persistObservation struct {
	Fingerprint string         `json:"fingerprint"`
	Entries     []persistEntry `json:"entries"`
}

// The wire forms of the persist program's reports hold every guest string as
// guest text until it is resolved against the declared mappings.
type persistEntryWire struct {
	Integration   guestText   `json:"integration"`
	Source        guestText   `json:"source"`
	Target        guestText   `json:"target"`
	Selected      bool        `json:"selected"`
	Ops           []guestText `json:"ops"`
	ID            guestText   `json:"id"`
	State         guestText   `json:"state"`
	Persist       guestText   `json:"persist"`
	Files         int         `json:"files"`
	Bytes         int64       `json:"bytes"`
	Existing      []guestText `json:"existing"`
	ExistingCount int         `json:"existing_count"`
	Retirable     bool        `json:"retirable"`
}

type persistObservationWire struct {
	Fingerprint string             `json:"fingerprint"`
	Entries     []persistEntryWire `json:"entries"`
}

type persistOutcomeWire struct {
	Source         guestText   `json:"source"`
	NotTransferred []guestText `json:"not_transferred"`
	Retained       []guestText `json:"retained"`
}

type persistRetainedWire struct {
	Integration guestText `json:"integration"`
	Kind        guestText `json:"kind"`
	Path        guestText `json:"path"`
	Bytes       *int64    `json:"bytes"`
}

// persistOutcome is what one applied transfer did not move or could not remove.
type persistOutcome struct {
	Source, Target string
	NotTransferred int
	Retained       []PersistRetained
}

// PersistRetained is a backup or stage that a finished transfer could not
// remove. It is state with a manual next step; it never blocks anything.
// Its location is the declared directory and name prefix; ID names the
// entry by a short digest of its guest name.
type PersistRetained struct {
	Integration string `json:"integration"`
	Kind        string `json:"kind"`
	Directory   string `json:"directory"`
	Prefix      string `json:"prefix"`
	ID          string `json:"id"`
	Bytes       *int64 `json:"bytes"`
}

var errInvalidPersistObservation = errors.New("invalid persist transfer observation")

// resolvePersistEntries keeps only entries of declared mappings; the host's
// mapping supplies every rendered value. Strict resolution refuses any other.
func resolvePersistEntries(wire []persistEntryWire, mappings []PersistMapping, strict bool) ([]persistEntry, error) {
	entries := []persistEntry{}
	for _, item := range wire {
		index := slices.IndexFunc(mappings, func(mapping PersistMapping) bool { return item.Source.Matches(mapping.Source) })
		state, stateOK := matchDeclared(item.State, "", "none", "active", "exchanged")
		persist, persistOK := matchDeclared(item.Persist, "", "deferred", "projected")
		valid := index >= 0 && stateOK && persistOK && item.ExistingCount >= len(item.Existing)
		var ops []string
		for _, op := range item.Ops {
			host, ok := matchDeclared(op, "adopt", "finish", "cancel")
			valid = valid && ok
			ops = append(ops, host)
		}
		if !valid {
			if strict {
				return nil, errInvalidPersistObservation
			}
			continue
		}
		mapping := mappings[index]
		entries = append(entries, persistEntry{Integration: mapping.Integration, Source: mapping.Source, Target: mapping.Target, Selected: mapping.Selected,
			Ops: ops, ID: item.ID, State: state, Persist: persist, Files: item.Files, Bytes: item.Bytes,
			Existing: item.Existing, ExistingCount: item.ExistingCount, Retirable: item.Retirable})
	}
	return entries, nil
}

// resolvePersistRetained places one guest-listed entry under a declared
// mapping's backup or stage name prefix; any other entry is not listed.
func resolvePersistRetained(path guestText, kind string, mappings []PersistMapping, yard, owner string) (PersistRetained, bool) {
	for _, mapping := range mappings {
		if owner != "" && mapping.Integration != owner {
			continue
		}
		locations := []struct{ kind, directory, prefix string }{
			{"backup", filepath.Dir(mapping.Source), filepath.Base(mapping.Source) + ".subyard-backup-"},
			{"stage", filepath.Dir(mapping.Target), ".subyard-stage-" + yard + "-"},
		}
		for _, location := range locations {
			if (kind == "" || kind == location.kind) && path.childWithPrefix(location.directory, location.prefix) {
				return PersistRetained{Integration: mapping.Integration, Kind: location.kind, Directory: location.directory, Prefix: location.prefix, ID: path.label()}, true
			}
		}
	}
	return PersistRetained{}, false
}

func (observation persistObservation) changed() bool {
	return slices.ContainsFunc(observation.Entries, func(entry persistEntry) bool { return len(entry.Ops) != 0 })
}

// changedOwner names the integration only if all planned work belongs to it.
func (observation persistObservation) changedOwner() string {
	owner := ""
	for _, entry := range observation.Entries {
		if len(entry.Ops) == 0 {
			continue
		}
		if entry.Integration == "" || owner != "" && owner != entry.Integration {
			return ""
		}
		owner = entry.Integration
	}
	return owner
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

func (observation persistObservation) facts(instance, project string) []integrationFact {
	var facts []integrationFact
	for _, entry := range observation.Entries {
		if len(entry.Ops) == 0 {
			continue
		}
		list := ""
		if entry.ExistingCount != 0 {
			list = shellquote.Command([]string{"incus", "exec", instance, "--project", project, "--", "ls", "-A", "--", entry.Target})
		}
		desired, _ := json.Marshal(persistStep{entry.Ops, entry.Source, entry.Target, entry.Selected, entry.ExistingCount, list})
		observed, _ := json.Marshal(entry)
		facts = append(facts, integrationFact{Key: "persist:" + strings.Join(entry.Ops, "+") + ":" + entry.Source,
			Desired: string(desired), Observed: string(observed), owner: entry.Integration})
	}
	return facts
}

// persistStep is the public disclosure bound in one persist plan fact. Target
// entry names are guest text: only their count and a host listing command are shown.
type persistStep struct {
	Ops           []string `json:"ops"`
	Source        string   `json:"source"`
	Target        string   `json:"target"`
	Selected      bool     `json:"selected"`
	ExistingCount int      `json:"existing_count"`
	List          string   `json:"list,omitempty"`
}

// persistStepText states what one persist plan step does to the operator's data.
func persistStepText(fact integrationFact) (string, string, bool) {
	var step persistStep
	if !strings.HasPrefix(fact.Key, "persist:") || json.Unmarshal([]byte(fact.Desired), &step) != nil {
		return "", "", false
	}
	var parts []string
	for _, op := range step.Ops {
		switch op {
		case "adopt":
			text := "copy " + step.Source + " into " + step.Target + " without replacing target entries, then atomically exchange it for a link to that target and delete the original directory"
			if step.ExistingCount != 0 {
				text += fmt.Sprintf("; %d entries already exist in the target and will not be transferred; their source copies are deleted with the backup. List the target entries on the owner host: %s",
					step.ExistingCount, step.List)
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
	return step.Source, strings.Join(parts, "; "), true
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

func (runtime Runtime) runPersist(ctx context.Context, mode string, mappings []PersistMapping, extra map[string]any) ([]byte, error) {
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
		return nil, refusal
	}
	return nil, runtime.persistRefusal(result.Stderr, mappings)
}

func (runtime Runtime) observePersist(ctx context.Context, entries []integrationArtifact) (persistObservation, error) {
	mappings, err := runtime.persistMappings(entries)
	if err != nil || len(mappings) == 0 {
		return persistObservation{}, err
	}
	output, err := runtime.runPersist(ctx, "observe", mappings, nil)
	if err != nil {
		// A refused or deferred transfer still keeps its paths out of artifact conflicts.
		var refused persistRefused
		if errors.As(err, &refused) {
			return persistObservation{Entries: refused.entries}, err
		}
		return persistObservation{}, err
	}
	var wire persistObservationWire
	if json.Unmarshal(output, &wire) != nil || len(wire.Fingerprint) != 64 || strings.Trim(wire.Fingerprint, "0123456789abcdef") != "" {
		return persistObservation{}, errInvalidPersistObservation
	}
	resolved, err := resolvePersistEntries(wire.Entries, mappings, true)
	if err != nil {
		return persistObservation{}, err
	}
	return persistObservation{Fingerprint: wire.Fingerprint, Entries: resolved}, nil
}

func (runtime Runtime) applyPersist(ctx context.Context, entries []integrationArtifact, approved persistObservation) error {
	if !approved.changed() {
		return nil
	}
	mappings, err := runtime.persistMappings(entries)
	if err != nil {
		return err
	}
	output, err := runtime.runPersist(ctx, "apply", mappings, map[string]any{"expected": approved.Fingerprint, "retire": approved.retireSources()})
	if err != nil {
		return err
	}
	var applied struct {
		Results []persistOutcomeWire `json:"results"`
	}
	if json.Unmarshal(output, &applied) != nil {
		return errors.New("invalid persist transfer result")
	}
	if runtime.Stderr != nil {
		for _, line := range runtime.persistOutcomeLines(resolvePersistOutcomes(applied.Results, mappings, runtime.Yard.YardName)) {
			fmt.Fprintln(runtime.Stderr, line)
		}
	}
	return nil
}

// resolvePersistOutcomes keeps the outcomes of declared mappings; a retained
// entry outside their backup and stage names is counted without a location.
func resolvePersistOutcomes(results []persistOutcomeWire, mappings []PersistMapping, yard string) []persistOutcome {
	var outcomes []persistOutcome
	for _, result := range results {
		index := slices.IndexFunc(mappings, func(mapping PersistMapping) bool { return result.Source.Matches(mapping.Source) })
		if index < 0 {
			continue
		}
		mapping := mappings[index]
		outcome := persistOutcome{Source: mapping.Source, Target: mapping.Target, NotTransferred: len(result.NotTransferred)}
		for _, path := range result.Retained {
			item, ok := resolvePersistRetained(path, "", mappings[index:index+1], yard, "")
			if !ok {
				item = PersistRetained{Integration: mapping.Integration, Kind: "entry", ID: path.label()}
			}
			outcome.Retained = append(outcome.Retained, item)
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes
}

// persistOutcomeLines states what a transfer left behind with host values only.
func (runtime Runtime) persistOutcomeLines(results []persistOutcome) []string {
	var lines []string
	for _, result := range results {
		if result.NotTransferred != 0 {
			list := shellquote.Command([]string{"incus", "exec", runtime.Yard.YardInstanceName, "--project", runtime.Yard.IncusProject, "--", "ls", "-A", "--", result.Target})
			lines = append(lines, fmt.Sprintf("persist %s: %d entries not transferred (the target already had them). List the target entries on the owner host: %s", result.Source, result.NotTransferred, list))
		}
		for _, item := range result.Retained {
			where := item.Kind + " " + item.ID
			if item.Directory != "" {
				where = fmt.Sprintf("%s %s/%s* (%s)", item.Kind, item.Directory, item.Prefix, item.ID)
			}
			lines = append(lines, "persist "+result.Source+": retained "+where+"; see yard integration status")
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
	output, err := runtime.runPersist(ctx, "retained", mappings, nil)
	if err != nil {
		return nil, err
	}
	return runtime.resolvePersistRetainedListing(output, mappings)
}

// resolvePersistRetainedListing keeps the listed backups and stages that lie
// under a declared mapping's names, with host values only.
func (runtime Runtime) resolvePersistRetainedListing(output []byte, mappings []PersistMapping) ([]PersistRetained, error) {
	var report struct {
		Retained []persistRetainedWire `json:"retained"`
	}
	if json.Unmarshal(output, &report) != nil {
		return nil, errors.New("invalid persist retained listing")
	}
	var retained []PersistRetained
	for _, entry := range report.Retained {
		kind, _ := matchDeclared(entry.Kind, "backup", "stage")
		owner, _ := matchDeclared(entry.Integration, persistOwners(mappings)...)
		if item, ok := resolvePersistRetained(entry.Path, kind, mappings, runtime.Yard.YardName, owner); ok && kind != "" {
			item.Bytes = entry.Bytes
			retained = append(retained, item)
		}
	}
	return retained, nil
}

func persistOwners(mappings []PersistMapping) []string {
	owners := []string{}
	for _, mapping := range mappings {
		owners = append(owners, mapping.Integration)
	}
	return owners
}

// PersistRetainedLines states retained data with a manual next step, never as a warning.
func (runtime Runtime) PersistRetainedLines(retained []PersistRetained) []string {
	var lines []string
	for _, item := range retained {
		size := "size unknown"
		if item.Bytes != nil {
			size = fmt.Sprintf("%d bytes", *item.Bytes)
		}
		inspect := shellquote.Command([]string{"incus", "exec", runtime.Yard.YardInstanceName, "--project", runtime.Yard.IncusProject, "--",
			"find", item.Directory, "-maxdepth", "1", "-name", item.Prefix + "*"})
		lines = append(lines, fmt.Sprintf("persist retained %s %s/%s* (%s, %s): an adoption could not remove it. Next, inspect it on the owner host: %s; remove it manually once its data is no longer needed",
			item.Kind, item.Directory, item.Prefix, item.ID, size, inspect))
	}
	return lines
}

// persistPlanChanged refuses a transfer that differs from the confirmed plan.
type persistPlanChanged struct{ yard string }

func (err persistPlanChanged) Error() string {
	return fmt.Sprintf("%v: persist transfer state in yard %s changed after the init plan was confirmed; nothing was transferred. Next, on the owner host: %s",
		domain.ErrPlanStale, err.yard, shellquote.Command([]string{"yard", "-Y", err.yard, "init"}))
}
func (persistPlanChanged) Unwrap() error                         { return domain.ErrPlanStale }
func (persistPlanChanged) PendingIntegrationConfiguration() bool { return true }

// checkApprovedPersist binds execution to the confirmed persist plan: S and T
// identities, operations and the disclosed existing entries are all in the
// fingerprint. A fresh observation without work needs no binding.
func (runtime Runtime) checkApprovedPersist(fresh persistObservation) error {
	if runtime.ApprovedPersist == nil || !fresh.changed() || fresh.Fingerprint == *runtime.ApprovedPersist {
		return nil
	}
	return persistPlanChanged{runtime.Yard.YardName}
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
	entries []persistEntry
}

func (err persistRefused) Unwrap() error { return err.error }

// persistExceptions are the exception classes a failed persist program may name.
var persistExceptions = []string{"ValueError", "TypeError", "KeyError", "IndexError", "AttributeError", "RuntimeError", "MemoryError",
	"OSError", "PermissionError", "FileNotFoundError", "FileExistsError", "NotADirectoryError", "IsADirectoryError",
	"JSONDecodeError", "UnicodeDecodeError", "UnicodeEncodeError", "RecursionError", "OverflowError"}

// persistLocation renders a guest path: a declared path quoted, a path below a
// declared root as that root with the entry's digest, and nothing else. The
// second result is the declared path a retry may name.
func persistLocation(path guestText, mappings []PersistMapping) (string, string, bool) {
	exact, roots := []string{}, []string{}
	for _, mapping := range mappings {
		exact = append(exact, mapping.Source, mapping.Target, filepath.Dir(mapping.Source)+"/"+filepath.Base(mapping.Source)+".subyard-transfer.json")
		for ancestor := filepath.Dir(mapping.Target); ancestor != "/" && ancestor != "."; ancestor = filepath.Dir(ancestor) {
			exact = append(exact, ancestor)
		}
		roots = append(roots, mapping.Source, mapping.Target, filepath.Dir(mapping.Source), filepath.Dir(mapping.Target))
	}
	if declared, ok := matchDeclared(path, exact...); ok && declared != "" {
		return fmt.Sprintf("%q", declared), declared, true
	}
	root := ""
	for _, candidate := range roots {
		if path.below(candidate) && len(candidate) > len(root) {
			root = candidate
		}
	}
	if root != "" {
		return fmt.Sprintf("%q (an entry below it, %s)", root, path.label()), root, true
	}
	return "an unrecognized path", "", false
}

// declaredGuestOwner names the declared integration the guest reported.
func (runtime Runtime) declaredGuestOwner(owner guestText, mappings []PersistMapping) string {
	candidates := append(strings.Fields(runtime.environmentValue("CODING_TOOL_INTEGRATIONS")), persistOwners(mappings)...)
	if definitions, err := runtime.integrationDefinitions(); err == nil {
		for _, definition := range definitions {
			candidates = append(candidates, definition.Name)
		}
	}
	declared, _ := matchDeclared(owner, candidates...)
	return runtime.declaredOwner(declared)
}

// Only typed persist conflicts are pending configuration. Every rendered value
// is a declared mapping value or fixed host text.
func (runtime Runtime) persistRefusal(stderr []byte, mappings []PersistMapping) error {
	var report struct {
		Conflicts []struct{ Reason, Path, Detail, Integration guestText } `json:"conflicts"`
		Entries   []persistEntryWire                                      `json:"entries"`
		Error     guestText                                               `json:"error"`
	}
	yard, instance, project := runtime.Yard.YardName, runtime.Yard.YardInstanceName, runtime.Yard.IncusProject
	status := shellquote.Command([]string{"yard", "-Y", yard, "integration", "status"})
	if json.Unmarshal(stderr, &report) != nil || len(report.Conflicts) == 0 {
		// Only a known exception class is reported, never paths or data.
		message := "persist transfer program failed; data was preserved"
		if name, ok := matchDeclared(report.Error, persistExceptions...); ok {
			message = fmt.Sprintf("persist transfer program failed (%s); data was preserved", name)
		}
		return errors.New(message)
	}
	inspect := func(path string) string {
		return shellquote.Command([]string{"incus", "exec", instance, "--project", project, "--", "namei", "-l", "--", path})
	}
	parent := func(path string) string { return path[:strings.LastIndex(path, "/")+1] }
	reasons := []string{"persist adoption pending", "unsafe persist data", "unsupported persist entry", "unsafe persist target", "session mount is unavailable",
		"persist target entry refused", "persist mount boundary", "persist filesystem error", "persist paths overlap", "persist target filesystem",
		"persist space unavailable", "atomic exchange unsupported", "no-replace rename unsupported", "persist transfer identity unavailable", "integration plan is stale"}
	causes := []error{}
	for _, conflict := range report.Conflicts {
		message, retry := "", shellquote.Command([]string{"yard", "-Y", yard, "init"})
		reason, _ := matchDeclared(conflict.Reason, reasons...)
		at, path, validPath := persistLocation(conflict.Path, mappings)
		// A retry that names the path needs a declared one; otherwise inspect status.
		byPath := func(command func() string) string {
			if validPath {
				return command()
			}
			return status
		}
		// An entry below a declared root is inspected at that root.
		exactPath := validPath && conflict.Path.Matches(path)
		other := "an unrecognized path"
		if reason == "persist paths overlap" {
			if location, _, ok := persistLocation(conflict.Detail, mappings); ok {
				other = location
			}
		}
		detail := persistDetail(conflict.Detail)
		if reason == "persist target filesystem" && detail != "unrecognized detail" && detail != "" {
			other = fmt.Sprintf("%q", detail)
		}
		owner := runtime.declaredGuestOwner(conflict.Integration, mappings)
		switch reason {
		case "persist adoption pending":
			message = fmt.Sprintf("persist adoption of %s in yard %s is pending: release activation does not start data transfers; data was preserved", at, yard)
		case "unsafe persist data", "unsupported persist entry", "unsafe persist target", "session mount is unavailable":
			message = fmt.Sprintf("persist transfer in yard %s: %s at %s; data was preserved. Inspect it, then repeat yard init", yard, reason, at)
			if detail != "" {
				message = fmt.Sprintf("persist transfer in yard %s: %s at %s (%s); data was preserved. Inspect it, then repeat yard init", yard, reason, at, detail)
			}
			retry = byPath(func() string { return inspect(path) })
		case "persist target entry refused":
			message = fmt.Sprintf("persist transfer in yard %s: the target refused %s (%s); nothing was exchanged and the source directory is unchanged. Fix the target permissions or free space, then repeat yard init", yard, at, detail)
			retry = byPath(func() string {
				if exactPath {
					return inspect(parent(path))
				}
				return inspect(path + "/")
			})
		case "persist mount boundary":
			message = fmt.Sprintf("persist transfer in yard %s: %s is a mount point inside persist data; nothing below it was copied or removed. Unmount or move it, then repeat", yard, at)
			retry = byPath(func() string {
				if exactPath {
					return shellquote.Command([]string{"incus", "exec", instance, "--project", project, "--", "findmnt", "--target", path})
				}
				return shellquote.Command([]string{"incus", "exec", instance, "--project", project, "--", "findmnt", "--submounts", "--target", path})
			})
		case "persist filesystem error":
			message = fmt.Sprintf("persist transfer in yard %s: filesystem error at %s (%s); data was preserved. Inspect it, then repeat yard init", yard, at, detail)
			retry = byPath(func() string { return inspect(path) })
		case "persist paths overlap", "persist target filesystem":
			message = fmt.Sprintf("persist mapping in yard %s: %s for %s and %s; data was preserved. Correct the persist mapping, then repeat yard init", yard, reason, at, other)
			retry = shellquote.Command([]string{"yard", "-Y", yard, "config", "show", "AGENT_" + owner + "_PERSIST"})
			if owner == "" {
				retry = status
			}
		case "persist space unavailable":
			// The guest's byte counts are not rendered; the retry shows the free space.
			message = fmt.Sprintf("persist transfer in yard %s: not enough space for %s (%s); data was preserved. Free space on the sessions volume, then repeat yard init", yard, at, detail)
			retry = byPath(func() string {
				directory := path + "/"
				if exactPath {
					directory = parent(path)
				}
				return shellquote.Command([]string{"incus", "exec", instance, "--project", project, "--", "df", "-h", "--", directory})
			})
		case "atomic exchange unsupported", "no-replace rename unsupported":
			message = fmt.Sprintf("persist transfer for %s in yard %s: %s (%s); nothing was copied. Deselect the integration to keep its data in place", owner, yard, reason, detail)
			retry = shellquote.Command([]string{"yard", "-Y", yard, "integration", "disable", owner})
			if owner == "" {
				message = fmt.Sprintf("persist transfer in yard %s: %s (%s); nothing was copied. Deselect the integration to keep its data in place", yard, reason, detail)
				retry = status
			}
		case "persist transfer identity unavailable":
			message = fmt.Sprintf("persist transfer in yard %s: the transfer descriptor %s is not a complete Subyard record; data was preserved. Inspect it and move it aside, then repeat yard init", yard, at)
			retry = byPath(func() string { return inspect(path) })
		case "integration plan is stale":
			causes = append(causes, fmt.Errorf("%w: persist transfer state changed after planning", domain.ErrPlanStale))
			continue
		default:
			// The guest's reason is not in this release's vocabulary.
			message = fmt.Sprintf("persist transfer in yard %s refused (unrecognized reason) at %s; data was preserved. Inspect the pending transfers, then repeat", yard, at)
			retry = status
		}
		causes = append(causes, integrationOwnershipError{pendingConfiguration: true, message: message, retry: retry})
	}
	if entries, _ := resolvePersistEntries(report.Entries, mappings, false); len(entries) != 0 {
		return persistRefused{domain.CollectRefusals(causes...), entries}
	}
	return domain.CollectRefusals(causes...)
}

var persistSpaceDetail = regexp.MustCompile(`^needs [0-9]{1,20} bytes plus a 1 GiB reserve; [0-9]{1,20} available$`)

// persistDetail returns the host text of the persist program's fixed details,
// operating system error texts and exception class names; other guest text,
// including byte counts and unknown error numbers, is replaced.
func persistDetail(detail guestText) string {
	if fixed, ok := matchDeclared(detail, "", "not readable by the developer", "not writable by the developer", "a mount inside the persist target",
		"the persist source is a mount point", "the target is a mount point; its stage would be on another filesystem",
		"must be owned by the developer and not writable by group or others"); ok {
		return fixed
	}
	if name, ok := matchDeclared(detail, persistExceptions...); ok {
		return name
	}
	switch {
	case detail.fits(persistSpaceDetail):
		return "the source size plus a 1 GiB reserve is not available"
	case detail.wraps("Unknown error ", ""):
		return "unknown operating system error"
	}
	for code := syscall.Errno(1); code < 256; code++ {
		if text := code.Error(); detail.matchesFold(text) {
			return strings.ToUpper(text[:1]) + text[1:]
		}
	}
	return "unrecognized detail"
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
	observation, err := runtime.observePersist(ctx, entries)
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
	// Only the bare substrate precondition (it fails first, alone) defers to
	// provisioning, which recreates the substrate and then plans uninspected.
	if err == errIntegrationSubstrateUnavailable {
		return IntegrationPlan{}, false, nil
	}
	return plan, err == nil, err
}

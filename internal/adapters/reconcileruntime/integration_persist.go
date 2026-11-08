package reconcileruntime

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
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

type persistEntry struct {
	Integration   string   `json:"integration"`
	Source        string   `json:"source"`
	Target        string   `json:"target"`
	Selected      bool     `json:"selected"`
	Ops           []string `json:"ops"`
	ID            string   `json:"id"`
	State         string   `json:"state"`
	Persist       string   `json:"persist"`
	Files         int      `json:"files"`
	Bytes         int64    `json:"bytes"`
	Existing      []string `json:"existing"`
	ExistingCount int      `json:"existing_count"`
}

type persistObservation struct {
	Fingerprint string         `json:"fingerprint"`
	Entries     []persistEntry `json:"entries"`
}

// persistOutcome is what one applied transfer did not move or could not remove.
type persistOutcome struct {
	Source         string   `json:"source"`
	NotTransferred []string `json:"not_transferred"`
	Retained       []string `json:"retained"`
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
		desired, _ := json.Marshal(persistStep{entry.Ops, entry.Source, entry.Target, entry.Selected, entry.Existing, entry.ExistingCount})
		observed, _ := json.Marshal(entry)
		facts = append(facts, integrationFact{Key: "persist:" + strings.Join(entry.Ops, "+") + ":" + entry.Source,
			Desired: string(desired), Observed: string(observed)})
	}
	return facts
}

// persistStep is the public disclosure bound in one persist plan fact.
type persistStep struct {
	Ops           []string `json:"ops"`
	Source        string   `json:"source"`
	Target        string   `json:"target"`
	Selected      bool     `json:"selected"`
	Existing      []string `json:"existing"`
	ExistingCount int      `json:"existing_count"`
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
				text += fmt.Sprintf("; %d entries already exist in the target and will not be transferred; their source copies are deleted with the backup: %s",
					step.ExistingCount, strings.Join(step.Existing, ", "))
				if step.ExistingCount > len(step.Existing) {
					text += fmt.Sprintf(" and %d more", step.ExistingCount-len(step.Existing))
				}
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
	return nil, runtime.persistRefusal(result.Stderr)
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
	var observation persistObservation
	if json.Unmarshal(output, &observation) != nil || len(observation.Fingerprint) != 64 {
		return persistObservation{}, errors.New("invalid persist transfer observation")
	}
	return observation, nil
}

func (runtime Runtime) applyPersist(ctx context.Context, entries []integrationArtifact, approved persistObservation) error {
	if !approved.changed() {
		return nil
	}
	mappings, err := runtime.persistMappings(entries)
	if err != nil {
		return err
	}
	output, err := runtime.runPersist(ctx, "apply", mappings, map[string]any{"expected": approved.Fingerprint})
	if err != nil {
		return err
	}
	var applied struct {
		Results []persistOutcome `json:"results"`
	}
	if json.Unmarshal(output, &applied) != nil {
		return errors.New("invalid persist transfer result")
	}
	if runtime.Stderr != nil {
		for _, line := range persistOutcomeLines(applied.Results) {
			fmt.Fprintln(runtime.Stderr, line)
		}
	}
	return nil
}

// persistOutcomeLines states, bounded, what a transfer left behind.
func persistOutcomeLines(results []persistOutcome) []string {
	var lines []string
	for _, result := range results {
		if count := len(result.NotTransferred); count != 0 {
			listed := result.NotTransferred[:min(count, 20)]
			line := fmt.Sprintf("persist %s: %d entries not transferred (the target already had them): %s", result.Source, count, strings.Join(listed, ", "))
			if count > len(listed) {
				line += fmt.Sprintf(" and %d more", count-len(listed))
			}
			lines = append(lines, line)
		}
		for _, path := range result.Retained {
			lines = append(lines, "persist "+result.Source+": retained "+path+"; see yard integration status")
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
	var report struct {
		Retained []PersistRetained `json:"retained"`
	}
	if json.Unmarshal(output, &report) != nil {
		return nil, errors.New("invalid persist retained listing")
	}
	return report.Retained, nil
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
		lines = append(lines, fmt.Sprintf("persist retained %s %s (%s): an adoption could not remove it. Next, inspect it on the owner host: %s; remove it manually once its data is no longer needed", item.Kind, item.Path, size, inspect))
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

// Every persist refusal is pending configuration with a converging next step.
func (runtime Runtime) persistRefusal(stderr []byte) error {
	var report struct {
		Conflicts []struct{ Reason, Path, Detail, Integration string } `json:"conflicts"`
		Entries   []persistEntry                                       `json:"entries"`
		Error     string                                               `json:"error"`
	}
	yard, instance, project := runtime.Yard.YardName, runtime.Yard.YardInstanceName, runtime.Yard.IncusProject
	status := shellquote.Command([]string{"yard", "-Y", yard, "integration", "status"})
	if json.Unmarshal(stderr, &report) != nil || len(report.Conflicts) == 0 {
		// Only the exception class is reported, never paths or data.
		message := "persist transfer evidence is unavailable; data was preserved. Inspect the pending transfers, then repeat"
		if report.Error != "" && domain.SafeID(report.Error) {
			message = fmt.Sprintf("persist transfer evidence is unavailable (%s); data was preserved. Inspect the pending transfers, then repeat", report.Error)
		}
		return integrationOwnershipError{pendingConfiguration: true, message: message, retry: status}
	}
	inspect := func(path string) string {
		return shellquote.Command([]string{"incus", "exec", instance, "--project", project, "--", "namei", "-l", "--", path})
	}
	causes := []error{}
	for _, conflict := range report.Conflicts {
		message, retry := "", shellquote.Command([]string{"yard", "-Y", yard, "init"})
		switch conflict.Reason {
		case "persist adoption pending":
			message = fmt.Sprintf("persist adoption of %q in yard %s is pending: release activation does not start data transfers; data was preserved", conflict.Path, yard)
		case "unsafe persist data", "unsupported persist entry", "unsafe persist target", "session mount is unavailable":
			message = fmt.Sprintf("persist transfer in yard %s: %s at %q; data was preserved. Inspect it, then repeat yard init", yard, conflict.Reason, conflict.Path)
			if conflict.Detail != "" {
				message = fmt.Sprintf("persist transfer in yard %s: %s at %q (%s); data was preserved. Inspect it, then repeat yard init", yard, conflict.Reason, conflict.Path, conflict.Detail)
			}
			retry = inspect(conflict.Path)
		case "persist target entry refused":
			message = fmt.Sprintf("persist transfer in yard %s: the target refused %q (%s); nothing was exchanged and the source directory is unchanged. Fix the target permissions or free space, then repeat yard init", yard, conflict.Path, conflict.Detail)
			retry = inspect(conflict.Path[:strings.LastIndex(conflict.Path, "/")+1])
		case "persist mount boundary":
			message = fmt.Sprintf("persist transfer in yard %s: %q is a mount point inside persist data; nothing below it was copied or removed. Unmount or move it, then repeat", yard, conflict.Path)
			retry = shellquote.Command([]string{"incus", "exec", instance, "--project", project, "--", "findmnt", "--target", conflict.Path})
		case "persist filesystem error":
			message = fmt.Sprintf("persist transfer in yard %s: filesystem error at %q (%s); data was preserved. Inspect it, then repeat yard init", yard, conflict.Path, conflict.Detail)
			retry = inspect(conflict.Path)
			if conflict.Path == "" {
				retry = status
			}
		case "persist paths overlap", "persist target filesystem":
			message = fmt.Sprintf("persist mapping in yard %s: %s for %q and %q; data was preserved. Correct the persist mapping, then repeat yard init", yard, conflict.Reason, conflict.Path, conflict.Detail)
			retry = shellquote.Command([]string{"yard", "-Y", yard, "config", "show", "AGENT_" + conflict.Integration + "_PERSIST"})
		case "persist space unavailable":
			message = fmt.Sprintf("persist transfer in yard %s: not enough space for %q (%s); data was preserved. Free space on the sessions volume, then repeat yard init", yard, conflict.Path, conflict.Detail)
			retry = shellquote.Command([]string{"incus", "exec", instance, "--project", project, "--", "df", "-h", "--", conflict.Path[:strings.LastIndex(conflict.Path, "/")+1]})
		case "atomic exchange unsupported", "no-replace rename unsupported":
			message = fmt.Sprintf("persist transfer for %s in yard %s: %s (%s); nothing was copied. Deselect the integration to keep its data in place", conflict.Integration, yard, conflict.Reason, conflict.Detail)
			retry = shellquote.Command([]string{"yard", "-Y", yard, "integration", "disable", conflict.Integration})
		case "persist transfer identity unavailable":
			message = fmt.Sprintf("persist transfer in yard %s: the transfer descriptor %q is not a complete Subyard record; data was preserved. Inspect it and move it aside, then repeat yard init", yard, conflict.Path)
			retry = inspect(conflict.Path)
		case "integration plan is stale":
			causes = append(causes, fmt.Errorf("%w: persist transfer state changed after planning", domain.ErrPlanStale))
			continue
		default:
			message = fmt.Sprintf("persist transfer in yard %s refused (%s) at %q; data was preserved. Inspect the pending transfers, then repeat", yard, conflict.Reason, conflict.Path)
			retry = status
		}
		causes = append(causes, integrationOwnershipError{pendingConfiguration: true, message: message, retry: retry})
	}
	if len(report.Entries) != 0 {
		return persistRefused{domain.CollectRefusals(causes...), report.Entries}
	}
	return domain.CollectRefusals(causes...)
}

// InspectPersistTransfers discloses pending transfers for an already running
// guest. Inspected reports whether the guest was observed: only then does the
// result bind execution (a stopped guest is first observed while provisioning).
func (runtime Runtime) InspectPersistTransfers(ctx context.Context) (plan IntegrationPlan, inspected bool, err error) {
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
	if err == nil && !observation.changed() {
		return IntegrationPlan{}, true, nil
	}
	plan, err = runtime.IntegrationPlan(ctx)
	return plan, err == nil, err
}

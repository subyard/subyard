package reconcileruntime

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/configmaterial"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerroute"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/shellquote"
)

//go:embed integration_inventory.py
var integrationInventoryProgram string

// Only validated, source-owned destinations and fixed diagnostic text may cross
// the release boundary. Guest stderr and file contents remain private.
type integrationOwnershipError struct {
	message       string
	retry         string
	cleanupTarget string
	// Extra public-safe detail is for direct status, outside the bounded release diagnostic.
	detail string
}

// IntegrationCleanupTarget identifies a usable explicit cleanup contract, never
// authority to retire unknown state during ordinary integration reconciliation.
func (err integrationOwnershipError) IntegrationCleanupTarget() string { return err.cleanupTarget }

func (err integrationOwnershipError) Error() string {
	if err.detail != "" {
		return err.message + err.detail
	}
	return err.message + " Next, inspect on the owner host: " + err.retry
}

func (err integrationOwnershipError) ActivationDiagnostic() (string, string) {
	return err.message, err.retry
}

// IntegrationPlan binds the candidate selection and live ownership evidence.
// It contains no credentials or document contents.
type IntegrationPlan struct {
	Fingerprint         string   `json:"fingerprint"`
	Changed             bool     `json:"changed"`
	Steps               []string `json:"steps"`
	Adoption            []string `json:"adoption,omitempty"`
	AdoptionFingerprint string   `json:"adoption_fingerprint,omitempty"`
	Scope               string   `json:"-"`
	facts               []integrationFact
	sources             string
	// Attribution only; the existing aggregate source binding remains the guard.
	packageSources map[string]string
	instance       ports.InstanceInfo
	instanceScope  string
	fingerprints   []string
	profileRoutes  []integrationProfileRoute
}

type integrationProfileRoute struct {
	Owner, MarkerKey, DeviceName, Marker string
	Device                               map[string]string
}

type integrationFact struct {
	Key       string `json:"key"`
	Desired   string `json:"desired"`
	Observed  string `json:"observed"`
	AtDesired bool   `json:"at_desired"`
}

type integrationArtifact struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	Path         string `json:"path,omitempty"`
	Digest       string `json:"digest"`
	SourceDigest string `json:"source_digest,omitempty"`
	Format       string `json:"format,omitempty"`
	Target       string `json:"target,omitempty"`
	FileTarget   bool   `json:"file_target,omitempty"`
	Content      string `json:"content,omitempty"`
	Mode         int    `json:"mode,omitempty"`
}
type integrationObservation struct {
	Fingerprint string                `json:"fingerprint"`
	Changed     bool                  `json:"changed"`
	Retired     []integrationArtifact `json:"retired"`
	Adopted     []integrationArtifact `json:"adopted"`
	Initial     bool                  `json:"initial"`
	Facts       []integrationFact     `json:"facts"`
}

// IntegrationScope binds the selected artifact destinations and desired
// identities without probing inventory, services, or package readiness.
func (runtime Runtime) IntegrationScope() (string, []string, error) {
	return runtime.integrationScopePhase("inspect")
}

// IntegrationScopeWithSourceReader computes only desired identity from prepared
// source bytes. Mutation and native observation readers are unchanged.
func (runtime Runtime) IntegrationScopeWithSourceReader(reader func(config.MaterializedAsset) ([]byte, error)) (string, []string, error) {
	if reader == nil {
		return "", nil, errors.New("prospective integration source reader is unavailable")
	}
	return runtime.integrationScopeWithSourceReader("inspect", reader)
}

func (runtime Runtime) integrationScopePhase(phase string) (string, []string, error) {
	return runtime.integrationScopeWithSourceReader(phase, runtime.SourceReader)
}

func (runtime Runtime) integrationScopeWithSourceReader(phase string, reader func(config.MaterializedAsset) ([]byte, error)) (string, []string, error) {
	entries, err := runtime.integrationArtifactsWithSourceReader(phase, reader)
	if err != nil {
		return "", nil, err
	}
	paths := []string{}
	for index := range entries {
		entries[index].Content = ""
		if entries[index].Path != "" {
			paths = append(paths, entries[index].Path)
		}
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	sourceBinding, _, err := runtime.integrationSourceBindingsWithSourceReader(phase, reader)
	if err != nil {
		return "", nil, err
	}
	uid := runtime.Yard.DevUID
	if uid <= 0 {
		uid = 1000
	}
	payload, err := json.Marshal(struct {
		UID       int                   `json:"uid"`
		Allows    string                `json:"allows"`
		Sources   string                `json:"sources,omitempty"`
		Artifacts []integrationArtifact `json:"artifacts"`
	}{UID: uid, Sources: sourceBinding, Allows: runtime.environmentDefault("ALLOWS_CODING_TOOLS", "true"), Artifacts: entries})
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("%x", sha256.Sum256(payload)), paths, nil
}

func adoptionPlan(observed integrationObservation, structuredEvidence []string) (IntegrationPlan, bool) {
	plan := IntegrationPlan{}
	legacy := false
	for _, entry := range observed.Adopted {
		plan.Adoption = append(plan.Adoption, entry.Path)
		legacy = legacy || entry.ID != "_projects"
	}
	if legacy {
		evidence := append([]string{observed.Fingerprint}, structuredEvidence...)
		plan.AdoptionFingerprint = fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(evidence, "\x00"))))
		plan.Fingerprint = plan.AdoptionFingerprint
		plan.Changed = true
		for _, entry := range observed.Adopted {
			plan.Steps = append(plan.Steps, "adopt existing "+entry.Kind+" for "+entry.ID+" under integration management: "+entry.Path)
		}
	}
	return plan, legacy
}

func (runtime Runtime) integrationPrecondition(ctx context.Context) (ports.InstanceInfo, error) {
	state, err := runtime.reconcileState(ctx)
	if err != nil {
		return ports.InstanceInfo{}, err
	}
	if !state.InstanceFound {
		return ports.InstanceInfo{}, errors.New("integration reconcile requires an existing yard")
	}
	if !strings.EqualFold(state.Instance.Status, "running") {
		return ports.InstanceInfo{}, errors.New("integration reconcile requires a running yard; start it explicitly first")
	}
	if runtime.Executor == nil {
		return ports.InstanceInfo{}, errors.New("Incus executor is required")
	}
	if ok, err := runtime.guestCheck(ctx, []string{"sh", "-eu", "-c", `command -v python3 >/dev/null; id "$1" >/dev/null; test -d /srv/agents`, "subyard", runtime.devUser()}); err != nil || !ok {
		return ports.InstanceInfo{}, firstError(err, errors.New("integration substrate is unavailable; run yard init"))
	}
	return state.Instance, nil
}

func (runtime Runtime) integrationArtifactsPhase(phase string) ([]integrationArtifact, error) {
	return runtime.integrationArtifactsWithSourceReader(phase, runtime.SourceReader)
}

func (runtime Runtime) integrationArtifactsWithSourceReader(phase string, reader func(config.MaterializedAsset) ([]byte, error)) ([]integrationArtifact, error) {
	files, err := runtime.guestConfigFilesWithSourceReader(reader)
	if err != nil {
		return nil, err
	}
	entries := []integrationArtifact{}
	for _, file := range files {
		payload, err := file.readSource()
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		entry := integrationArtifact{ID: file.integration, Kind: "file", Path: file.destination, Digest: fmt.Sprintf("%x", sha256.Sum256(payload))}
		entry.SourceDigest, err = file.legacySourceHash()
		if err != nil {
			return nil, err
		}
		if file.ownedFormat != "" {
			entry.Kind = "structured"
			entry.Format = file.ownedFormat
		} else {
			entry.Content = base64.StdEncoding.EncodeToString(payload)
		}
		entries = append(entries, entry)
	}
	for _, link := range strings.Fields(runtime.environmentValue("INTEGRATION_HOST_LINKS")) {
		parts := strings.Split(link, ":")
		if len(parts) < 2 || len(parts) > 3 || !filepath.IsAbs(parts[1]) {
			return nil, errors.New("invalid integration link")
		}
		relative, err := safeGuestRelativePath(parts[0])
		if err != nil {
			return nil, err
		}
		id := ""
		for _, candidate := range strings.Fields(runtime.environmentValue("CODING_TOOL_INTEGRATIONS")) {
			if slices.Contains(strings.Fields(runtime.environmentValue("AGENT_"+candidate+"_PERSIST")), link) {
				id = candidate
				break
			}
		}
		if id == "" {
			return nil, errors.New("integration link has no selected owner")
		}
		entries = append(entries, integrationArtifact{ID: id, Kind: "link", Path: "/home/" + runtime.devUser() + "/" + relative, Target: parts[1], FileTarget: len(parts) == 3 && parts[2] == "file", Digest: fmt.Sprintf("%x", sha256.Sum256([]byte(parts[1])))})
	}
	definitions, err := runtime.integrationDefinitions()
	if err != nil {
		return nil, err
	}
	for _, id := range strings.Fields(runtime.environmentValue("CODING_TOOL_INTEGRATIONS")) {
		inputs := []string{id, runtime.devUser(), runtime.environmentValue("CODING_TOOL_INTEGRATIONS")}
		for _, definition := range definitions {
			if definition.Name == id {
				binding, err := runtime.integrationPackageBindingWithSourceReader(definition, reader)
				if err != nil {
					return nil, runtime.integrationDiagnostic(err, definition, phase, "native-unavailable")
				}
				inputs = append(inputs, binding)
			}
		}
		provision := runtime.environmentValue("AGENT_" + id + "_PROVISION")
		if provision != "" {
			payload, err := (guestConfigFile{readAsset: reader}).readSourceAsset(config.MaterializedAsset{Source: provision})
			if err != nil {
				return nil, err
			}
			inputs = append(inputs, fmt.Sprintf("%x", sha256.Sum256(payload)))
		}
		entries = append(entries, integrationArtifact{ID: id, Kind: "package", Digest: fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(inputs, "\x00"))))})
	}

	dispatcher, err := os.ReadFile(filepath.Join(runtime.RepositoryRoot, "config", "projects-changed.sh"))
	if err != nil {
		return nil, err
	}
	hooks := []string{}
	for _, id := range strings.Fields(runtime.environmentValue("CODING_TOOL_INTEGRATIONS")) {
		hook := runtime.environmentValue("AGENT_" + id + "_PROJECTS_CHANGED")
		if hook != "" {
			if !safeCommand(hook) {
				return nil, errors.New("invalid integration project hook")
			}
			hooks = append(hooks, hook)
		}
	}
	for _, file := range []struct {
		path    string
		payload []byte
		mode    int
	}{
		{"/usr/local/libexec/subyard/projects-changed", dispatcher, 0755},
		{"/etc/subyard/agent-project-hooks", []byte(strings.Join(hooks, "\n") + "\n"), 0644},
	} {
		entries = append(entries, integrationArtifact{ID: "_projects", Kind: "file", Path: file.path, Mode: file.mode, Content: base64.StdEncoding.EncodeToString(file.payload), Digest: fmt.Sprintf("%x", sha256.Sum256(file.payload))})
	}
	return entries, nil
}

func (runtime Runtime) integrationInventory(ctx context.Context, mode string, entries []integrationArtifact, expected string) (integrationObservation, error) {
	home := "/home/" + runtime.devUser()
	uid := runtime.Yard.DevUID
	if uid <= 0 {
		uid = 1000
	}
	legacy := []string{}
	definitions, err := runtime.integrationDefinitions()
	if err != nil {
		return integrationObservation{}, err
	}
	for _, definition := range definitions {
		for _, artifact := range definition.Artifacts {
			legacy = append(legacy, home+"/"+artifact.Destination)
		}
	}
	// Explicit operator links have no integration ownership and are preserved.
	explicit := map[string]bool{}
	if runtime.environmentValue("INTEGRATION_HOST_LINKS") == "" {
		for _, link := range strings.Fields(runtime.environmentValue("HOST_LINKS")) {
			parts := strings.Split(link, ":")
			explicit[home+"/"+parts[0]] = true
		}
	}
	for _, definition := range definitions {
		id := definition.Name
		for _, link := range strings.Fields(runtime.environmentValue("AGENT_" + id + "_PERSIST")) {
			parts := strings.Split(link, ":")
			path := home + "/" + parts[0]
			if !explicit[path] {
				legacy = append(legacy, path)
			}
		}
	}
	payload, _ := json.Marshal(map[string]any{"home": home, "uid": uid, "developer": runtime.devUser(), "entries": entries, "legacy": legacy, "adopt": runtime.AdoptLegacyIntegrations, "expected": expected, "preview_skill_cutover": !runtime.legacyPreviewInstructions})
	program := strings.ReplaceAll(integrationInventoryProgram, "@STATE_ROOT@", "/var/lib/subyard/integrations")
	program = strings.ReplaceAll(program, "@STATE_UID@", "0")
	result, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, ports.InstanceExecRequest{Command: []string{"python3", "-B", "-c", program, mode}, Stdin: payload})
	if err != nil || result.ExitCode != 0 {
		var conflict struct{ Reason, Path, Detail string }
		if json.Unmarshal(result.Stderr, &conflict) == nil {
			knownPath := slices.Contains(legacy, conflict.Path)
			for _, entry := range entries {
				knownPath = knownPath || entry.Path == conflict.Path
			}
			switch conflict.Reason {
			case "owned artifact drift", "unowned selected artifact", "legacy integration artifact has no ownership evidence":
				if knownPath && conflict.Path != "" {
					if conflict.Reason == "unowned selected artifact" &&
						(conflict.Path == "/usr/local/libexec/subyard/projects-changed" || conflict.Path == "/etc/subyard/agent-project-hooks") {
						detail := ""
						probe := []string{"namei", "-l", "--", conflict.Path}
						switch conflict.Detail {
						case "unrecognized core content":
							detail = "contents do not match the current or a recognized legacy Subyard file; compare with the shipped file before replacing it"
							probe = []string{"sha256sum", "--", conflict.Path}
						case "unsafe core metadata":
							detail = "file must be regular, owned by root:root, with mode 0755 for projects-changed or 0644 for agent-project-hooks"
						case "unsafe core ancestor":
							detail = "parent directories must be owned by root:root and not writable by group or others"
						}
						if detail != "" {
							command := append([]string{"incus", "exec", runtime.Yard.YardInstanceName, "--project", runtime.Yard.IncusProject, "--"}, probe...)
							return integrationObservation{}, integrationOwnershipError{
								message: fmt.Sprintf("integration ownership conflict in yard %s at %q: %s. File preserved.", runtime.Yard.YardName, conflict.Path, detail),
								retry:   shellquote.Command(command),
							}
						}
					}
					command := []string{"incus", "exec", runtime.Yard.YardInstanceName, "--project", runtime.Yard.IncusProject, "--", "namei", "-l", "--", conflict.Path}
					return integrationObservation{}, integrationOwnershipError{
						message: fmt.Sprintf("integration ownership conflict in yard %s: %s at %q; managed artifacts were preserved", runtime.Yard.YardName, conflict.Reason, conflict.Path),
						retry:   shellquote.Command(command),
					}
				}
			}
		}
		return integrationObservation{}, errors.New("integration ownership conflict or unavailable evidence; managed artifacts were preserved")
	}
	var observed integrationObservation
	if mode == "observe" {
		if json.Unmarshal(result.Stdout, &observed) != nil || len(observed.Fingerprint) != 64 {
			return observed, errors.New("invalid integration ownership observation")
		}
	}
	return observed, nil
}

func (runtime Runtime) observeIntegrations(ctx context.Context) (IntegrationPlan, integrationObservation, error) {
	return runtime.observeIntegrationsPhase(ctx, "inspect")
}
func (runtime Runtime) observeIntegrationsPhase(ctx context.Context, phase string) (IntegrationPlan, integrationObservation, error) {
	return runtime.observeIntegrationsWithSourceReader(ctx, phase, runtime.SourceReader)
}
func (runtime Runtime) observeIntegrationsWithSourceReader(ctx context.Context, phase string, reader func(config.MaterializedAsset) ([]byte, error)) (IntegrationPlan, integrationObservation, error) {
	instance, err := runtime.integrationPrecondition(ctx)
	if err != nil {
		return IntegrationPlan{}, integrationObservation{}, err
	}
	entries, err := runtime.integrationArtifactsWithSourceReader(phase, reader)
	if err != nil {
		return IntegrationPlan{}, integrationObservation{}, err
	}
	observed, err := runtime.integrationInventory(ctx, "observe", entries, "")
	if err != nil {
		return IntegrationPlan{}, observed, err
	}
	plan := IntegrationPlan{Changed: observed.Changed}
	plan.sources, plan.packageSources, err = runtime.integrationSourceBindingsWithSourceReader(phase, reader)
	if err != nil {
		return plan, observed, err
	}
	plan.facts = slices.Clone(observed.Facts)
	scope, _, err := runtime.integrationScopeWithSourceReader(phase, reader)
	if err != nil {
		return plan, observed, err
	}
	instanceBytes, _ := json.Marshal(instance)
	var guardInstance ports.InstanceInfo
	_ = json.Unmarshal(instanceBytes, &guardInstance)
	guardInstance.Status = ""
	guardInstance.Config = cloneIntegrationConfig(guardInstance.Config)
	guardInstance.LocalConfig = cloneIntegrationConfig(guardInstance.LocalConfig)
	definitions, err := runtime.integrationDefinitions()
	if err != nil {
		return plan, observed, err
	}
	for _, definition := range definitions {
		if definition.Lifecycle == nil {
			continue
		}
		for _, key := range definition.Lifecycle.InstanceConfigKeys {
			delete(guardInstance.Config, key)
			delete(guardInstance.LocalConfig, key)
		}
		for _, name := range definition.Lifecycle.InstanceDevices {
			delete(guardInstance.Devices, name)
			delete(guardInstance.LocalDevices, name)
		}
	}
	plan.instance, plan.instanceScope = guardInstance, scope
	plan.profileRoutes = runtime.integrationProfileRoutes(ctx, guardInstance)
	plan.Scope = integrationInstanceScope(scope, guardInstance)
	fingerprints := []string{plan.Scope, plan.sources, observed.Fingerprint, runtime.environmentDefault("ALLOWS_CODING_TOOLS", "true")}
	if len(plan.profileRoutes) != 0 {
		routeBindings, _ := json.Marshal(plan.profileRoutes)
		fingerprints = append(fingerprints, string(routeBindings))
	}
	for _, definition := range definitions {
		if definition.Lifecycle == nil {
			continue
		}
		observation, err := runtime.observeIntegrationLifecycleWithSourceReader(ctx, definition, phase, reader)
		if err != nil {
			return plan, observed, err
		}
		fact := runtime.integrationLifecycleFact(definition, observation)
		plan.facts = append(plan.facts, fact)
		plan.Changed = plan.Changed || !fact.AtDesired
		fingerprints = append(fingerprints, observation.Actual, observation.Desired, fact.Desired)
	}
	files, err := runtime.guestConfigFilesWithSourceReader(reader)
	if err != nil {
		return plan, observed, err
	}
	structuredAdoptionEvidence := []string{}
	for _, file := range files {
		if file.ownedFormat == "" {
			continue
		}
		payload, err := file.readSource()
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return plan, observed, err
		}
		mode := configmaterial.ModeObserve
		if observed.Initial && runtime.AdoptLegacyIntegrations {
			mode = configmaterial.ModeAssessAdopt
		}
		observation, err := runtime.observeIntegrationConfigFile(ctx, mode, file, payload)
		if err != nil {
			return plan, observed, err
		}
		fingerprints = append(fingerprints, observation.Fingerprint)
		plan.facts = append(plan.facts, integrationFact{Key: "structured:" + file.destination, Desired: fmt.Sprintf("%x", sha256.Sum256(payload)), Observed: observation.Fingerprint, AtDesired: observation.Converged})
		plan.Changed = plan.Changed || !observation.Converged
		if mode == configmaterial.ModeAssessAdopt && observation.Adoptable {
			observed.Adopted = append(observed.Adopted, integrationArtifact{
				ID: file.integration, Kind: "structured", Path: file.destination,
				Format: file.ownedFormat, Digest: fmt.Sprintf("%x", sha256.Sum256(payload)),
			})
			structuredAdoptionEvidence = append(structuredAdoptionEvidence, file.destination+"\x00"+observation.Fingerprint)
		}
	}
	adoption, _ := adoptionPlan(observed, structuredAdoptionEvidence)
	plan.Adoption = adoption.Adoption
	plan.AdoptionFingerprint = adoption.AdoptionFingerprint
	if observed.Initial && runtime.LegacyIntegrationFingerprint != "" && plan.AdoptionFingerprint != runtime.LegacyIntegrationFingerprint {
		return plan, observed, errors.New("legacy integration adoption changed after planning")
	}
	for _, entry := range observed.Retired {
		if entry.Kind != "structured" {
			continue
		}
		observation, err := runtime.observeIntegrationConfig(ctx, entry.Format, configmaterial.ModeAssessRetire, entry.Path, nil)
		if err != nil {
			return plan, observed, err
		}
		fingerprints = append(fingerprints, observation.Fingerprint)
		plan.facts = append(plan.facts, integrationFact{Key: "retired-structured:" + entry.Path, Desired: "absent", Observed: observation.Fingerprint, AtDesired: observation.Converged})
		plan.Changed = plan.Changed || !observation.Converged
	}
	commands, err := runtime.provisionAgentCommands()
	if err != nil {
		return plan, observed, err
	}
	for _, command := range commands {
		ready, err := runtime.guestCheck(ctx, []string{"sh", "-c", `command -v "$1" >/dev/null`, "subyard", command})
		if err != nil {
			return plan, observed, err
		}
		plan.Changed = plan.Changed || !ready
		fingerprints = append(fingerprints, fmt.Sprint(ready))
		plan.facts = append(plan.facts, integrationFact{Key: "command:" + command, Desired: "ready", Observed: fmt.Sprint(ready), AtDesired: ready})
	}
	checks, err := runtime.provisionAgentChecks()
	if err != nil {
		return plan, observed, err
	}
	for _, check := range checks {
		ready, err := runtime.guestCheck(ctx, []string{check})
		if err != nil {
			return plan, observed, err
		}
		plan.Changed = plan.Changed || !ready
		fingerprints = append(fingerprints, fmt.Sprint(ready))
		plan.facts = append(plan.facts, integrationFact{Key: "check:" + check, Desired: "ready", Observed: fmt.Sprint(ready), AtDesired: ready})
	}
	ready, err := runtime.projectHooksConverged(ctx)
	if err != nil {
		return plan, observed, err
	}
	plan.Changed = plan.Changed || !ready
	fingerprints = append(fingerprints, fmt.Sprint(ready))
	plan.facts = append(plan.facts, integrationFact{Key: "hooks", Desired: "ready", Observed: fmt.Sprint(ready), AtDesired: ready})
	serviceFingerprint, servicesChanged, err := runtime.integrationServices(ctx, "observe")
	if err != nil {
		return plan, observed, err
	}
	plan.Changed = plan.Changed || servicesChanged
	fingerprints = append(fingerprints, serviceFingerprint)
	plan.facts = append(plan.facts, integrationFact{Key: "services", Desired: "deselected managed services inactive; role-forbidden utility absent", Observed: serviceFingerprint, AtDesired: !servicesChanged})
	plan.Fingerprint = fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(fingerprints, "\x00"))))
	plan.fingerprints = slices.Clone(fingerprints)
	if plan.Changed {
		plan.Steps = []string{"reconcile selected integrations: " + strings.Join(strings.Fields(runtime.environmentValue("CODING_TOOL_INTEGRATIONS")), " ")}
		for _, entry := range observed.Retired {
			if entry.Kind == "package" {
				plan.Steps = append(plan.Steps, "disable managed startup for "+entry.ID+"; preserve binaries and user data")
			} else {
				plan.Steps = append(plan.Steps, "retire owned "+entry.Kind+" for "+entry.ID+": "+entry.Path)
			}
		}
		for _, entry := range observed.Adopted {
			plan.Steps = append(plan.Steps, "adopt existing "+entry.Kind+" for "+entry.ID+" under integration management: "+entry.Path)
		}
		if runtime.environmentValue("ALLOWS_CODING_TOOLS") == "false" {
			plan.Steps = append(plan.Steps, "remove ownership-proven ccusage utility for the special yard role")
		}

	}
	return plan, observed, nil
}

// PrepareLegacyIntegrationAdoption returns an opt-in runtime only for an
// existing running yard with exact selected legacy artifacts to adopt.
func (runtime Runtime) PrepareLegacyIntegrationAdoption(ctx context.Context) (Runtime, IntegrationPlan, error) {
	return runtime.prepareLegacyIntegrationAdoptionWithSourceReader(ctx, runtime.SourceReader)
}
func (runtime Runtime) prepareLegacyIntegrationAdoptionWithSourceReader(ctx context.Context, reader func(config.MaterializedAsset) ([]byte, error)) (Runtime, IntegrationPlan, error) {
	if ready, err := runtime.incusReady(ctx); err != nil || !ready {
		return runtime, IntegrationPlan{}, err
	}
	state, err := runtime.reconcileState(ctx)
	if err != nil {
		return runtime, IntegrationPlan{}, err
	}
	if !state.InstanceFound || !strings.EqualFold(state.Instance.Status, "running") || runtime.Executor == nil {
		return runtime, IntegrationPlan{}, nil
	}
	ready, err := runtime.guestCheck(ctx, []string{"sh", "-eu", "-c", `command -v python3 >/dev/null; id "$1" >/dev/null; test -d /srv/agents`, "subyard", runtime.devUser()})
	if err != nil {
		return runtime, IntegrationPlan{}, err
	}
	if !ready {
		return runtime, IntegrationPlan{}, nil
	}
	candidate := runtime
	candidate.AdoptLegacyIntegrations = true
	absent, err := candidate.guestCheck(ctx, []string{"test", "!", "-e", "/var/lib/subyard/integrations/inventory.json"})
	if err != nil {
		return runtime, IntegrationPlan{}, err
	}
	if !absent {
		return runtime, IntegrationPlan{}, nil
	}
	entries, err := candidate.integrationArtifactsWithSourceReader("inspect", reader)
	if err != nil {
		return runtime, IntegrationPlan{}, err
	}
	observed, err := candidate.integrationInventory(ctx, "observe", entries, "")
	if err != nil {
		return runtime, IntegrationPlan{}, err
	}
	files, err := candidate.guestConfigFilesWithSourceReader(reader)
	if err != nil {
		return runtime, IntegrationPlan{}, err
	}
	structuredEvidence := []string{}
	for _, file := range files {
		if file.ownedFormat == "" {
			continue
		}
		payload, err := file.readSource()
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return runtime, IntegrationPlan{}, err
		}
		observation, err := candidate.observeIntegrationConfigFile(ctx, configmaterial.ModeAssessAdopt, file, payload)
		if err != nil {
			return runtime, IntegrationPlan{}, err
		}
		if observation.Adoptable {
			observed.Adopted = append(observed.Adopted, integrationArtifact{
				ID: file.integration, Kind: "structured", Path: file.destination,
				Format: file.ownedFormat, Digest: fmt.Sprintf("%x", sha256.Sum256(payload)),
			})
			structuredEvidence = append(structuredEvidence, file.destination+"\x00"+observation.Fingerprint)
		}
	}
	plan, legacy := adoptionPlan(observed, structuredEvidence)
	if !observed.Initial || !legacy || plan.AdoptionFingerprint == "" {
		return runtime, IntegrationPlan{}, nil
	}
	candidate.LegacyIntegrationFingerprint = plan.AdoptionFingerprint
	return candidate, plan, nil
}

func (runtime Runtime) observeIntegrationConfigFile(ctx context.Context, mode string, file guestConfigFile, payload []byte) (configmaterial.Observation, error) {
	var templates [][]byte
	if mode == configmaterial.ModeAssessAdopt && file.ownedFormat == "toml" {
		root := filepath.Join(runtime.RepositoryRoot, "config", "agents", file.integration, "legacy-config")
		info, err := os.Lstat(root)
		if err != nil && !os.IsNotExist(err) {
			return configmaterial.Observation{}, err
		}
		if err == nil {
			if !info.IsDir() {
				return configmaterial.Observation{}, errors.New("legacy config templates must be a real directory")
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				return configmaterial.Observation{}, err
			}
			for _, entry := range entries {
				if filepath.Ext(entry.Name()) != ".toml" {
					continue
				}
				template, err := (config.MaterializedAsset{Source: filepath.Join(root, entry.Name())}).ReadSource()
				if err != nil {
					return configmaterial.Observation{}, err
				}
				templates = append(templates, template)
			}
		}
	}
	return runtime.observeIntegrationConfig(ctx, file.ownedFormat, mode, file.destination, payload, templates...)
}

func (runtime Runtime) observeIntegrationConfig(ctx context.Context, format, mode, path string, payload []byte, legacyTemplates ...[]byte) (configmaterial.Observation, error) {
	request, err := configmaterial.Request(format, mode, runtime.devUser(), path, runtime.Yard.DevUID, payload, legacyTemplates...)
	if err != nil {
		return configmaterial.Observation{}, err
	}
	result, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, request)
	if err != nil || result.ExitCode != 0 {
		return configmaterial.Observation{}, errors.New("integration structured configuration ownership conflict")
	}
	return configmaterial.ParseObservation(result.Stdout)
}

func (runtime Runtime) IntegrationPlan(ctx context.Context) (IntegrationPlan, error) {
	plan, _, err := runtime.observeIntegrations(ctx)
	return plan, err
}

func integrationInstanceScope(scope string, instance ports.InstanceInfo) string {
	identity, _ := json.Marshal(struct {
		Scope    string
		Instance ports.InstanceInfo
	}{scope, instance})
	return fmt.Sprintf("%x", sha256.Sum256(identity))
}

// Record exact declared sibling effects without changing ordinary integration guards.
func (runtime Runtime) integrationProfileRoutes(ctx context.Context, instance ports.InstanceInfo) []integrationProfileRoute {
	if runtime.Yard.YardKind == domain.YardVM {
		return nil
	}
	var routes []integrationProfileRoute
	for _, definition := range runtime.Profiles {
		if definition.Runtime == nil || definition.Dashboard == nil || definition.Dashboard.ContainerRoute == nil ||
			!slices.Contains(strings.Fields(runtime.environmentValue("ENVIRONMENT_PROFILES")), definition.Name) {
			continue
		}
		dashboard := definition.Dashboard
		route := dashboard.ContainerRoute
		marker, device := instance.Config[route.MarkerKey], instance.Devices[route.DeviceName]
		if marker != "" || len(device) != 0 {
			_, _, owned := ownerroute.Owned(marker, device, dashboard.GuestPort)
			if !owned || instance.LocalConfig[route.MarkerKey] != marker || !maps.Equal(instance.LocalDevices[route.DeviceName], device) {
				continue
			}
		} else if instance.LocalConfig[route.MarkerKey] != "" || len(instance.LocalDevices[route.DeviceName]) != 0 {
			continue
		}
		port := runtime.environmentValue(dashboard.PortSetting)
		number, err := strconv.Atoi(port)
		if err != nil || number < 1024 || number > 65535 || strconv.Itoa(number) != port {
			continue
		}
		host := ownerroute.Host(ctx)
		desiredMarker := "v1:" + port
		if host != "127.0.0.1" {
			desiredMarker = "v2:" + host + ":" + port
		}
		routes = append(routes, integrationProfileRoute{definition.Runtime.ActivationID, route.MarkerKey, route.DeviceName, desiredMarker, ownerroute.Device(host, port, dashboard.GuestPort)})
	}
	return routes
}

// Project only complete, originally captured sibling publications after their
// native owner verifies readiness. The original plan and every other instance
// field remain unchanged; the returned plan is for approval comparison only.
func (runtime Runtime) ProjectIntegrationPlanAfterProfileActivation(ctx context.Context, approved, fresh IntegrationPlan) (IntegrationPlan, error) {
	if len(approved.profileRoutes) == 0 || approved.Fingerprint == fresh.Fingerprint {
		return fresh, nil
	}
	instance := fresh.instance
	instance.Config, instance.LocalConfig = maps.Clone(instance.Config), maps.Clone(instance.LocalConfig)
	instance.Devices, instance.LocalDevices = maps.Clone(instance.Devices), maps.Clone(instance.LocalDevices)
	for _, route := range approved.profileRoutes {
		if instance.Config[route.MarkerKey] == approved.instance.Config[route.MarkerKey] &&
			instance.LocalConfig[route.MarkerKey] == approved.instance.LocalConfig[route.MarkerKey] &&
			maps.Equal(instance.Devices[route.DeviceName], approved.instance.Devices[route.DeviceName]) &&
			maps.Equal(instance.LocalDevices[route.DeviceName], approved.instance.LocalDevices[route.DeviceName]) {
			continue
		}
		if instance.Config[route.MarkerKey] != route.Marker || instance.LocalConfig[route.MarkerKey] != route.Marker ||
			!maps.Equal(instance.Devices[route.DeviceName], route.Device) || !maps.Equal(instance.LocalDevices[route.DeviceName], route.Device) {
			return fresh, domain.ErrPlanStale
		}
		owner, err := runtime.ObserveProfileRuntime(ctx, route.Owner)
		if err != nil {
			return fresh, err
		}
		if owner.State != ports.RuntimeStateCurrent || owner.Actual == "" || owner.Actual != owner.Desired {
			return fresh, domain.ErrPlanStale
		}
		for _, pair := range [][2]map[string]string{{instance.Config, approved.instance.Config}, {instance.LocalConfig, approved.instance.LocalConfig}} {
			if value, exists := pair[1][route.MarkerKey]; exists {
				pair[0][route.MarkerKey] = value
			} else {
				delete(pair[0], route.MarkerKey)
			}
		}
		for _, pair := range [][2]map[string]map[string]string{{instance.Devices, approved.instance.Devices}, {instance.LocalDevices, approved.instance.LocalDevices}} {
			if value, exists := pair[1][route.DeviceName]; exists {
				pair[0][route.DeviceName] = value
			} else {
				delete(pair[0], route.DeviceName)
			}
		}
	}
	fresh.Scope = integrationInstanceScope(fresh.instanceScope, instance)
	parts := slices.Clone(fresh.fingerprints)
	if len(parts) == 0 {
		return fresh, domain.ErrPlanStale
	}
	parts[0] = fresh.Scope
	fresh.Fingerprint = fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(parts, "\x00"))))
	return fresh, nil
}

// PrepareSourceIntegrationPlan retains the full native ownership, adoption and
// readiness plan while projecting only prepared source content.
func (runtime Runtime) PrepareSourceIntegrationPlan(ctx context.Context, reader func(config.MaterializedAsset) ([]byte, error), allowAdoption bool) (IntegrationPlan, error) {
	if reader == nil {
		return IntegrationPlan{}, errors.New("prospective integration source reader is unavailable")
	}
	candidate := runtime
	if allowAdoption {
		var err error
		candidate, _, err = runtime.prepareLegacyIntegrationAdoptionWithSourceReader(ctx, reader)
		if err != nil {
			return IntegrationPlan{}, err
		}
	}
	plan, _, err := candidate.observeIntegrationsWithSourceReader(ctx, "inspect", reader)
	return plan, err
}

type PreparedIntegrations struct {
	plan             IntegrationPlan
	configs          *PreparedConfigs
	entries          []integrationArtifact
	skipped          string
	runtime, binding string
	hooks            *ProjectHookPlan
	sources          *PreparedInstalledRuntime
}

func (p *PreparedIntegrations) calculateBinding() string {
	raw, _ := json.Marshal(struct {
		Plan, Configs, Runtime, Skipped string
		Entries                         []integrationArtifact
	}{p.plan.StateBinding(), p.configs.Binding(), p.runtime, p.skipped, p.entries})
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func (p *PreparedIntegrations) Binding() string {
	if p == nil {
		return ""
	}
	return p.binding
}

func (runtime Runtime) PrepareIntegrations(ctx context.Context, plan IntegrationPlan) (*PreparedIntegrations, error) {
	configs, err := runtime.PrepareConfigs(ctx)
	if err != nil {
		return nil, err
	}
	entries, err := runtime.integrationArtifactsPhase("apply")
	if err != nil {
		return nil, err
	}
	definitions, err := runtime.integrationDefinitions()
	if err != nil {
		return nil, err
	}
	skipped := []string{}
	for _, definition := range definitions {
		if definition.Lifecycle != nil {
			skipped = append(skipped, definition.Name)
		}
	}
	p := &PreparedIntegrations{plan: plan, configs: configs, entries: entries, skipped: strings.Join(skipped, " "), runtime: runtime.configRuntimeBinding(), hooks: runtime.HookPlan}
	raw, err := json.Marshal(struct {
		Plan, Configs, Runtime, Skipped string
		Entries                         []integrationArtifact
	}{plan.StateBinding(), configs.Binding(), p.runtime, p.skipped, entries})
	if err != nil {
		return nil, err
	}
	p.binding = fmt.Sprintf("%x", sha256.Sum256(raw))
	return p, nil
}

func (runtime Runtime) ApplyPreparedIntegrations(ctx context.Context, prepared *PreparedIntegrations) error {
	if prepared == nil || prepared.runtime != runtime.configRuntimeBinding() {
		return domain.ErrPlanStale
	}
	runtime.legacyPreviewInstructions = !prepared.configs.previewSkillCutover
	if err := runtime.CheckPreparedConfigs(ctx, prepared.configs); err != nil {
		return err
	}
	runtime.SourceReader = nil
	entries, err := runtime.integrationArtifactsPhase("apply")
	if err != nil {
		return err
	}
	before, _ := json.Marshal(prepared.entries)
	current, _ := json.Marshal(entries)
	if !slices.Equal(before, current) {
		return domain.ErrPlanStale
	}
	return runtime.applyIntegrations(ctx, prepared.plan, prepared)
}

func (runtime Runtime) ApplyIntegrations(ctx context.Context, plan IntegrationPlan) error {
	return runtime.applyIntegrations(ctx, plan, nil)
}

func (runtime Runtime) applyIntegrations(ctx context.Context, plan IntegrationPlan, prepared *PreparedIntegrations) error {
	fresh, observed, err := runtime.observeIntegrationsPhase(ctx, "apply")
	if err != nil {
		return err
	}
	compared := fresh
	if prepared != nil {
		compared, err = runtime.ProjectIntegrationPlanAfterProfileActivation(ctx, plan, fresh)
		if err != nil {
			return err
		}
	}
	if err := CheckIntegrationPlan(plan, compared); err != nil {
		return runtime.integrationPlanDiagnostic(err, plan, fresh)
	}
	if err := runtime.checkIntegrationHookScope(); err != nil {
		return err
	}
	if !fresh.Changed {
		return nil
	}
	execution := runtime
	executionCompleted := false
	if prepared != nil && prepared.sources != nil {
		execution, err = runtime.materializedIntegrationExecution(ctx, prepared.sources)
		if err != nil {
			return err
		}
		// A failed opaque command may retain descendants. Its exact marked
		// stage remains bounded and cannot be removed as a quiescence claim.
		defer func() {
			if !executionCompleted && prepared.sources.stageBinding != "" {
				prepared.sources.retain.Store(true)
			}
			prepared.sources.Close()
		}()
	}
	if err := runtime.retireIntegrationServices(ctx); err != nil {
		return err
	}
	for _, entry := range observed.Retired {
		if entry.Kind == "structured" {
			if _, err := runtime.observeIntegrationConfig(ctx, entry.Format, configmaterial.ModeRetire, entry.Path, nil); err != nil {
				return err
			}
		}
	}
	var entries []integrationArtifact
	if prepared != nil {
		entries = prepared.entries
	} else {
		entries, err = runtime.integrationArtifactsPhase("apply")
		if err != nil {
			return err
		}
	}
	expectedInventory := ""
	if observed.Initial {
		expectedInventory = observed.Fingerprint
	}
	if _, err := runtime.integrationInventory(ctx, "apply", entries, expectedInventory); err != nil {
		return err
	}
	skipped := ""
	if prepared != nil {
		var configErr error
		if prepared.sources != nil {
			configErr = runtime.applyPreparedConfigs(ctx, prepared.configs, false)
		} else {
			configErr = runtime.ApplyPreparedConfigs(ctx, prepared.configs)
		}
		if err := configErr; err != nil {
			return err
		}
		skipped = prepared.skipped
	} else {
		if err := runtime.RefreshConfigs(ctx); err != nil {
			return err
		}
		definitions, err := runtime.integrationDefinitions()
		if err != nil {
			return err
		}
		names := []string{}
		for _, definition := range definitions {
			if definition.Lifecycle != nil {
				names = append(names, definition.Name)
			}
		}
		skipped = strings.Join(names, " ")
	}
	if err := execution.runScriptEnvironment(ctx, runtime.Stderr, map[string]string{"SUBYARD_INTEGRATION_LIFECYCLES": skipped}, "reconcile-integrations.sh"); err != nil {
		return err
	}
	if err := execution.applyIntegrationLifecycles(ctx, fresh); err != nil {
		return err
	}
	// Keep the inventory pending until hooks succeed so the same requested
	// selection remains retryable after a transient hook failure.
	if err := execution.captureProvisionedProjectHooks(ctx); err != nil {
		return err
	}
	if err := execution.RunProjectHooks(ctx); err != nil {
		return err
	}
	if _, err := runtime.integrationInventory(ctx, "commit", entries, ""); err != nil {
		return err
	}
	if prepared != nil && prepared.sources != nil {
		// Refuse changed live source without ever launching its observe handler.
		// Verification itself consumes the same retained executable/input bytes.
		if err := runtime.checkMaterializedIntegrationSources(prepared.sources); err != nil {
			return err
		}
		if err := runtime.CheckPreparedConfigs(ctx, prepared.configs); err != nil {
			return err
		}
	}
	verified, _, err := execution.observeIntegrationsPhase(ctx, "verify")
	if err != nil {
		return err
	}
	if verified.Changed {
		return runtime.integrationDiagnostic(errors.New("integration reconcile verification failed; desired selection retained for retry"),
			profile.IntegrationDefinition{}, "verify", "convergence-failed")
	}
	executionCompleted = true
	return nil
}

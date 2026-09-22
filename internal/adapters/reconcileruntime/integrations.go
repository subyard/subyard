package reconcileruntime

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/configmaterial"
	"github.com/Subyard/Subyard/internal/ports"
)

//go:embed integration_inventory.py
var integrationInventoryProgram string

// IntegrationPlan binds the candidate selection and live ownership evidence.
// It contains no credentials or document contents.
type IntegrationPlan struct {
	Fingerprint         string   `json:"fingerprint"`
	Changed             bool     `json:"changed"`
	Steps               []string `json:"steps"`
	Adoption            []string `json:"adoption,omitempty"`
	AdoptionFingerprint string   `json:"adoption_fingerprint,omitempty"`
}

type integrationArtifact struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Path       string `json:"path,omitempty"`
	Digest     string `json:"digest"`
	Format     string `json:"format,omitempty"`
	Target     string `json:"target,omitempty"`
	FileTarget bool   `json:"file_target,omitempty"`
	Content    string `json:"content,omitempty"`
	Mode       int    `json:"mode,omitempty"`
}
type integrationObservation struct {
	Fingerprint string                `json:"fingerprint"`
	Changed     bool                  `json:"changed"`
	Retired     []integrationArtifact `json:"retired"`
	Adopted     []integrationArtifact `json:"adopted"`
	Initial     bool                  `json:"initial"`
}

// IntegrationScope binds the selected artifact destinations and desired
// identities without probing inventory, services, or package readiness.
func (runtime Runtime) IntegrationScope() (string, []string, error) {
	entries, err := runtime.integrationArtifacts()
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
	uid := runtime.Yard.DevUID
	if uid <= 0 {
		uid = 1000
	}
	payload, err := json.Marshal(struct {
		UID       int                   `json:"uid"`
		Allows    string                `json:"allows"`
		Artifacts []integrationArtifact `json:"artifacts"`
	}{UID: uid, Allows: runtime.environmentDefault("ALLOWS_CODING_TOOLS", "true"), Artifacts: entries})
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

func (runtime Runtime) integrationArtifacts() ([]integrationArtifact, error) {
	files, err := runtime.guestConfigFiles()
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
	for _, id := range strings.Fields(runtime.environmentValue("CODING_TOOL_INTEGRATIONS")) {
		inputs := []string{id, runtime.devUser(), runtime.environmentValue("CODING_TOOL_INTEGRATIONS")}
		provision := runtime.environmentValue("AGENT_" + id + "_PROVISION")
		if provision != "" {
			payload, err := os.ReadFile(provision)
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
	for _, relative := range []string{".claude/CLAUDE.md", ".claude/settings.json", ".codex/AGENTS.md", ".codex/config.toml", ".codex/rules/repo.rules", ".config/opencode/AGENTS.md", ".config/opencode/opencode.jsonc", ".pi/agent/settings.json"} {
		legacy = append(legacy, home+"/"+relative)
	}
	// Explicit operator links have no integration ownership and are preserved.
	explicit := map[string]bool{}
	if runtime.environmentValue("INTEGRATION_HOST_LINKS") == "" {
		for _, link := range strings.Fields(runtime.environmentValue("HOST_LINKS")) {
			parts := strings.Split(link, ":")
			explicit[home+"/"+parts[0]] = true
		}
	}
	for _, id := range []string{"claude", "codex", "opencode", "pi"} {
		for _, link := range strings.Fields(runtime.environmentValue("AGENT_" + id + "_PERSIST")) {
			parts := strings.Split(link, ":")
			path := home + "/" + parts[0]
			if !explicit[path] {
				legacy = append(legacy, path)
			}
		}
	}
	payload, _ := json.Marshal(map[string]any{"home": home, "uid": uid, "developer": runtime.devUser(), "entries": entries, "legacy": legacy, "adopt": runtime.AdoptLegacyIntegrations, "expected": expected})
	program := strings.ReplaceAll(integrationInventoryProgram, "@STATE_ROOT@", "/var/lib/subyard/integrations")
	program = strings.ReplaceAll(program, "@STATE_UID@", "0")
	result, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, ports.InstanceExecRequest{Command: []string{"python3", "-B", "-c", program, mode}, Stdin: payload})
	if err != nil || result.ExitCode != 0 {
		var conflict struct{ Reason, Path string }
		if json.Unmarshal(result.Stderr, &conflict) == nil {
			knownPath := slices.Contains(legacy, conflict.Path)
			for _, entry := range entries {
				knownPath = knownPath || entry.Path == conflict.Path
			}
			switch conflict.Reason {
			case "owned artifact drift", "unowned selected artifact", "legacy integration artifact has no ownership evidence":
				if knownPath && conflict.Path != "" {
					return integrationObservation{}, fmt.Errorf("integration ownership conflict: %s at %q; managed artifacts were preserved", conflict.Reason, conflict.Path)
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
	instance, err := runtime.integrationPrecondition(ctx)
	if err != nil {
		return IntegrationPlan{}, integrationObservation{}, err
	}
	entries, err := runtime.integrationArtifacts()
	if err != nil {
		return IntegrationPlan{}, integrationObservation{}, err
	}
	observed, err := runtime.integrationInventory(ctx, "observe", entries, "")
	if err != nil {
		return IntegrationPlan{}, observed, err
	}
	plan := IntegrationPlan{Changed: observed.Changed}
	fingerprints := []string{observed.Fingerprint, runtime.environmentDefault("ALLOWS_CODING_TOOLS", "true")}
	proxy := instance.Devices["ai-observer"]
	proxyMarker := instance.Config["user.subyard.ai_observer_proxy"]
	if proxy != nil {
		port := strings.TrimPrefix(strings.TrimPrefix(proxyMarker, "v1:"), "pending:")
		if !strings.HasPrefix(proxyMarker, "v1:") || len(proxy) != 4 || proxy["type"] != "proxy" || proxy["bind"] != "host" || proxy["listen"] != "tcp:127.0.0.1:"+port || proxy["connect"] != "tcp:127.0.0.1:8080" {
			return plan, observed, errors.New("AI Observer proxy ownership is unknown or changed")
		}
	}
	proxyEvidence, _ := json.Marshal(proxy)
	fingerprints = append(fingerprints, string(proxyEvidence))

	files, err := runtime.guestConfigFiles()
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
		observation, err := runtime.observeIntegrationConfig(ctx, file.ownedFormat, mode, file.destination, payload)
		if err != nil {
			return plan, observed, err
		}
		fingerprints = append(fingerprints, observation.Fingerprint)
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
		plan.Changed = plan.Changed || !observation.Converged
	}
	ready, err := runtime.aiObserverConverged(instance)
	if err != nil {
		return plan, observed, err
	}
	plan.Changed = plan.Changed || !ready
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
	}
	ready, err = runtime.projectHooksConverged(ctx)
	if err != nil {
		return plan, observed, err
	}
	plan.Changed = plan.Changed || !ready
	fingerprints = append(fingerprints, fmt.Sprint(ready))
	serviceFingerprint, servicesChanged, err := runtime.integrationServices(ctx, "observe")
	if err != nil {
		return plan, observed, err
	}
	plan.Changed = plan.Changed || servicesChanged
	fingerprints = append(fingerprints, serviceFingerprint)
	fingerprints = append(fingerprints, instance.Config["user.subyard.ai_observer_proxy"], instance.Config["user.subyard.ai_observer_provision"])
	plan.Fingerprint = fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(fingerprints, "\x00"))))
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
	entries, err := candidate.integrationArtifacts()
	if err != nil {
		return runtime, IntegrationPlan{}, err
	}
	observed, err := candidate.integrationInventory(ctx, "observe", entries, "")
	if err != nil {
		return runtime, IntegrationPlan{}, err
	}
	files, err := candidate.guestConfigFiles()
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
		observation, err := candidate.observeIntegrationConfig(ctx, file.ownedFormat, configmaterial.ModeAssessAdopt, file.destination, payload)
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

func (runtime Runtime) observeIntegrationConfig(ctx context.Context, format, mode, path string, payload []byte) (configmaterial.Observation, error) {
	request, err := configmaterial.Request(format, mode, runtime.devUser(), path, runtime.Yard.DevUID, payload)
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

func (runtime Runtime) ApplyIntegrations(ctx context.Context, plan IntegrationPlan) error {
	fresh, observed, err := runtime.observeIntegrations(ctx)
	if err != nil {
		return err
	}
	if fresh.Fingerprint != plan.Fingerprint || fresh.Changed != plan.Changed {
		return errors.New("integration plan is stale; reassess before applying")
	}
	if !fresh.Changed {
		return nil
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
	entries, err := runtime.integrationArtifacts()
	if err != nil {
		return err
	}
	expectedInventory := ""
	if observed.Initial {
		expectedInventory = observed.Fingerprint
	}
	if _, err := runtime.integrationInventory(ctx, "apply", entries, expectedInventory); err != nil {
		return err
	}
	if err := runtime.RefreshConfigs(ctx); err != nil {
		return err
	}
	identity, err := runtime.aiObserverProvisionIdentity()
	if err != nil {
		return err
	}
	if err := runtime.runScriptEnvironment(ctx, runtime.Stderr, map[string]string{"AI_OBSERVER_CONTEXT": identity}, "reconcile-integrations.sh"); err != nil {
		return err
	}
	// Keep the inventory pending until hooks succeed so the same requested
	// selection remains retryable after a transient hook failure.
	if err := runtime.RunProjectHooks(ctx); err != nil {
		return err
	}
	if _, err := runtime.integrationInventory(ctx, "commit", entries, ""); err != nil {
		return err
	}
	verified, err := runtime.IntegrationPlan(ctx)
	if err != nil {
		return err
	}
	if verified.Changed {
		return errors.New("integration reconcile verification failed; desired selection retained for retry")
	}
	return nil
}

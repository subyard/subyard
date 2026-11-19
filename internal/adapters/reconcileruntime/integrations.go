package reconcileruntime

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Subyard/Subyard/internal/adapters/configmaterial"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerroute"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/shellquote"
)

//go:embed integration_inventory.py
var integrationInventoryProgram string

// IntegrationInventoryReader returns the exact guest program this release runs
// to observe integration state, so test doubles can recognize it.
func IntegrationInventoryReader() string {
	program := strings.ReplaceAll(integrationInventoryProgram, "@STATE_ROOT@", "/var/lib/subyard/integrations")
	return strings.ReplaceAll(program, "@STATE_UID@", "0")
}

// Only validated, source-owned destinations and fixed diagnostic text may cross
// the release boundary. Guest stderr and file contents remain private.
type integrationOwnershipError struct {
	message              string
	retry                string
	pendingConfiguration bool
}

func (err integrationOwnershipError) Error() string {
	return err.message + " Next, on the owner host: " + err.retry
}

// Ownership findings describe pending init-owned state, not release history.
func (err integrationOwnershipError) PendingIntegrationConfiguration() bool {
	return err.pendingConfiguration
}

func (err integrationOwnershipError) ActivationDiagnostic() (string, string) {
	return err.message, err.retry
}

func (integrationOwnershipError) integrationOwnershipConflict() {}

// integrationOwnershipRefusal is a refusal the guest ownership program itself
// returned without a recognized public detail. Its text stays unchanged.
type integrationOwnershipRefusal string

func (err integrationOwnershipRefusal) Error() string             { return string(err) }
func (integrationOwnershipRefusal) integrationOwnershipConflict() {}

// ownershipRefusal labels a failed guest ownership command. Only a command the
// guest completed reports ownership state; transport and host failures remain
// inspection failures with the same text.
func ownershipRefusal(message string, result ports.InstanceExecResult, err error) error {
	var completed *ports.GuestExitError
	if err == nil && result.ExitCode != 0 || errors.As(err, &completed) {
		return integrationOwnershipRefusal(message)
	}
	return errors.New(message)
}

// IntegrationOwnershipConflict reports whether an integration observation
// failed because the guest refused unproven or changed owned state, rather than
// because the state could not be inspected.
func IntegrationOwnershipConflict(err error) bool {
	var conflict interface{ integrationOwnershipConflict() }
	return errors.As(err, &conflict)
}

// IntegrationPlan binds the candidate selection and live ownership evidence.
// It contains no credentials or document contents.
type IntegrationPlan struct {
	Fingerprint string   `json:"fingerprint"`
	Changed     bool     `json:"changed"`
	Steps       []string `json:"steps"`
	// Warnings preview bound observations about retained state and stay outside
	// the plan binding.
	Warnings            []string `json:"-"`
	Adoption            []string `json:"adoption,omitempty"`
	AdoptionFingerprint string   `json:"adoption_fingerprint,omitempty"`
	Scope               string   `json:"-"`
	facts               []integrationFact
	persist             persistObservation
	sources             string

	// ownershipReleases lists structured documents whose retirement releases
	// changed or missing owned configuration instead of removing it.
	ownershipReleases       []string
	ownershipReleaseNotices []string

	// Attribution only; the existing aggregate source binding remains the guard.
	packageSources map[string]string
	instance       ports.InstanceInfo
	instanceScope  string
	fingerprints   []string
	profileRoutes  []integrationProfileRoute
	pendingOnly    bool
}

// InstalledChanged excludes a redundant apply tail from installed readiness.
// Changed still requires that tail for authorized apply and recovery.
func (plan IntegrationPlan) InstalledChanged() bool { return plan.Changed && !plan.pendingOnly }

// OwnershipReleases lists structured documents whose retirement releases changed
// owned fields in place. Rewriting them first would discard the operator's change.
func (plan IntegrationPlan) OwnershipReleases() []string { return slices.Clone(plan.ownershipReleases) }

type integrationProfileRoute struct {
	Owner, MarkerKey, DeviceName, Marker string
	Device                               map[string]string
}

type integrationFact struct {
	Key       string `json:"key"`
	Desired   string `json:"desired"`
	Observed  string `json:"observed"`
	AtDesired bool   `json:"at_desired"`
	// owner is the declaring integration, for diagnostics only.
	owner string
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
	Fingerprint     string                         `json:"fingerprint"`
	Changed         bool                           `json:"changed"`
	Pending         bool                           `json:"pending"`
	Retired         []integrationArtifact          `json:"retired"`
	Adopted         []integrationArtifact          `json:"adopted"`
	Initial         bool                           `json:"initial"`
	Facts           []integrationFact              `json:"facts"`
	Installed       bool                           `json:"installed"`
	Selected        []string                       `json:"selected"`
	AppliedSettings map[string]string              `json:"applied_settings"`
	Diagnostics     *IntegrationDiagnosticEvidence `json:"diagnostics"`
}

type IntegrationDiagnosticEvidence struct {
	Key     string                               `json:"key"`
	Digests map[string]string                    `json:"digests"`
	Files   map[string]IntegrationDiagnosticFile `json:"files,omitempty"`
}

type IntegrationDiagnosticFile struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
}

func validIntegrationDiagnosticEvidence(catalog config.Catalog, evidence *IntegrationDiagnosticEvidence) bool {
	if evidence == nil || !catalog.ValidIntegrationDiagnosticEvidence(evidence.Key, evidence.Digests) || len(evidence.Files) > 4096 {
		return false
	}
	for name, file := range evidence.Files {
		definition, ok := catalog.LookupSetting(name)
		if !ok || definition.Sensitive || definition.Kind != config.SettingFile || definition.Application != config.SettingConfigApply || len(file.Path) > 16384 || !filepath.IsAbs(file.Path) || config.ValidateNonSecretContent(name, file.Path) != nil || len(file.Digest) != 64 || strings.Trim(file.Digest, "0123456789abcdef") != "" {
			return false
		}
	}
	return true
}

type AppliedIntegrationConfiguration struct {
	Selected    []string
	Settings    map[string]string
	Installed   bool
	Identity    map[string]string
	Diagnostics *IntegrationDiagnosticEvidence
}

// AppliedIntegrationConfiguration reads committed ownership and validated inputs,
// independently of desired settings or an incomplete enable operation.
func (runtime Runtime) AppliedIntegrationConfiguration(ctx context.Context) (AppliedIntegrationConfiguration, error) {
	if absent, err := runtime.integrationStateNeverInstalled(ctx); err != nil || absent {
		return AppliedIntegrationConfiguration{}, err
	}
	baseline, err := runtime.integrationInventory(ctx, "settings", nil, "")
	if err != nil {
		return AppliedIntegrationConfiguration{}, err
	}
	if baseline.AppliedSettings != nil {
		catalog, loadErr := config.LoadCatalog(runtime.RepositoryRoot)
		if loadErr != nil {
			return AppliedIntegrationConfiguration{}, loadErr
		}
		if err := catalog.ValidateIntegrationInitSettings(baseline.AppliedSettings); err != nil {
			return AppliedIntegrationConfiguration{}, err
		}
		if user, ok := baseline.AppliedSettings["DEV_USER"]; ok {
			runtime.Yard.DevUser = user
		}
		if uid, ok := baseline.AppliedSettings["DEV_UID"]; ok {
			value, parseErr := strconv.Atoi(uid)
			if parseErr != nil {
				return AppliedIntegrationConfiguration{}, errors.New("invalid applied developer UID")
			}
			runtime.Yard.DevUID = value
		}
	}
	identity := map[string]string{}
	if user, userOK := baseline.AppliedSettings["DEV_USER"]; userOK {
		if uid, uidOK := baseline.AppliedSettings["DEV_UID"]; uidOK {
			identity["DEV_USER"], identity["DEV_UID"] = user, uid
		}
	}
	if len(identity) == 0 {
		identity, err = runtime.appliedGuestIdentity(ctx)
		if err != nil {
			return AppliedIntegrationConfiguration{}, err
		}
		if len(identity) != 0 {
			runtime.Yard.DevUser = identity["DEV_USER"]
			runtime.Yard.DevUID, _ = strconv.Atoi(identity["DEV_UID"])
		}
	}
	observed, err := runtime.integrationInventory(ctx, "selection", nil, "")
	if err != nil {
		return AppliedIntegrationConfiguration{}, err
	}
	if observed.Installed != baseline.Installed || (observed.AppliedSettings == nil) != (baseline.AppliedSettings == nil) || !maps.Equal(observed.AppliedSettings, baseline.AppliedSettings) || (observed.Diagnostics == nil) != (baseline.Diagnostics == nil) || (observed.Diagnostics != nil && (observed.Diagnostics.Key != baseline.Diagnostics.Key || !maps.Equal(observed.Diagnostics.Digests, baseline.Diagnostics.Digests) || !maps.Equal(observed.Diagnostics.Files, baseline.Diagnostics.Files))) {
		return AppliedIntegrationConfiguration{}, domain.ErrPlanStale
	}
	settings := observed.AppliedSettings
	if settings != nil {
		catalog, loadErr := config.LoadCatalog(runtime.RepositoryRoot)
		if loadErr != nil {
			return AppliedIntegrationConfiguration{}, loadErr
		}
		settings = runtime.relocateAppliedShippedSources(catalog, settings)
	}
	return AppliedIntegrationConfiguration{Selected: observed.Selected, Settings: settings, Installed: observed.Installed, Identity: identity, Diagnostics: observed.Diagnostics}, nil
}

// withAppliedIntegrationSettings builds the runtime view of installed inputs.
func (runtime Runtime) withAppliedIntegrationSettings(settings, identity map[string]string) (Runtime, error) {
	catalog, err := config.LoadCatalog(runtime.RepositoryRoot)
	if err != nil {
		return Runtime{}, err
	}
	environment := catalog.WithAppliedIntegrationInitSettings(runtimeEnvironment(runtime.Environment), settings)
	if user, ok := identity["DEV_USER"]; ok {
		uid, err := strconv.Atoi(identity["DEV_UID"])
		if err != nil {
			return Runtime{}, errors.New("invalid applied developer UID")
		}
		runtime.Yard.DevUser, runtime.Yard.DevUID = user, uid
		maps.Copy(environment, identity)
	}
	runtime.Environment = make([]string, 0, len(environment))
	for name, value := range environment {
		runtime.Environment = append(runtime.Environment, name+"="+value)
	}
	return runtime, nil
}

// CheckAppliedGuestIdentity refuses identity authoring when no independent owner
// can keep the installed consumers observable until ordinary init applies it.
func (runtime Runtime) CheckAppliedGuestIdentity(ctx context.Context) error {
	applied, err := runtime.AppliedIntegrationConfiguration(ctx)
	if err != nil {
		return err
	}
	if len(applied.Identity) == 0 {
		return errors.New("applied developer identity cannot be verified; reconcile the existing identity with yard init before editing DEV_USER or DEV_UID")
	}
	return nil
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
	sourceBinding, _, err := runtime.integrationSourceBindingsWithSourceReader(phase, reader)
	if err != nil {
		return "", nil, err
	}
	return runtime.integrationScopeFromInputs(entries, sourceBinding)
}

func (runtime Runtime) integrationScopeFromInputs(entries []integrationArtifact, sourceBinding string) (string, []string, error) {
	entries = slices.Clone(entries)
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
		UID        int                   `json:"uid"`
		Allows     string                `json:"allows"`
		Bubblewrap string                `json:"bubblewrap,omitempty"`
		Sources    string                `json:"sources,omitempty"`
		Artifacts  []integrationArtifact `json:"artifacts"`
	}{UID: uid, Sources: sourceBinding, Allows: runtime.environmentDefault("ALLOWS_CODING_TOOLS", "true"),
		Bubblewrap: runtime.environmentValue("BWRAP_APPARMOR"), Artifacts: entries})
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

// integrationInventoryPath is the guest's integration ownership inventory.
const integrationInventoryPath = "/var/lib/subyard/integrations/inventory.json"

// guestNeverProvisioned reports a running instance whose init has never
// finished and whose guest lacks the substrate every installed owner needs
// (python3 and /srv/agents). Init's stages run in order
// and provisioning installs that substrate first, so no later stage has run in
// this guest. A working yard whose re-init failed is never such a guest.
func (runtime Runtime) guestNeverProvisioned(ctx context.Context, instance ports.InstanceInfo) (bool, error) {
	if runtime.Executor == nil || !strings.EqualFold(instance.Status, "running") || !application.InstanceNeverInitialized(instance) {
		return false, nil
	}
	// Only an explicit answer counts: a transport failure or any other exit
	// status, such as an out-of-memory kill, is not evidence of absence.
	// The substrate does not depend on the current developer name, which a
	// rename can change while installed owners still run.
	result, answered, err := runtime.guestObserve(ctx, []string{"sh", "-eu", "-c", `if command -v python3 >/dev/null && test -d /srv/agents; then echo provisioned; else echo unprovisioned; fi`})
	if err != nil || !answered {
		return false, err
	}
	return strings.TrimSpace(string(result.Stdout)) == "unprovisioned", nil
}

// integrationStateNeverInstalled reports that a yard has no applied integration
// state to read: its guest was never provisioned and has no integration
// inventory. That is absent state, observed as such; any other yard, including
// a working yard whose re-init failed, is read and refused strictly as before.
func (runtime Runtime) integrationStateNeverInstalled(ctx context.Context) (bool, error) {
	if runtime.Incus == nil || runtime.Executor == nil {
		return false, nil
	}
	instance, err := runtime.Incus.Instance(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName)
	if err != nil {
		return false, nil
	}
	if never, err := runtime.guestNeverProvisioned(ctx, instance); err != nil || !never {
		return false, err
	}
	return runtime.guestCheck(ctx, []string{"sh", "-c", `test ! -e "$1" && test ! -L "$1"`, "subyard", integrationInventoryPath})
}

// errIntegrationSubstrateUnavailable is the precondition that init provisioning repairs.
var errIntegrationSubstrateUnavailable = errors.New("integration substrate is unavailable; run yard init")

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
	var refusals []error
	for _, file := range files {
		payload, err := file.readSource()
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			refusals = append(refusals, owned(file.integration, err))
			continue
		}
		entry := integrationArtifact{ID: file.integration, Kind: "file", Path: file.destination, Digest: fmt.Sprintf("%x", sha256.Sum256(payload))}
		entry.SourceDigest, err = file.legacySourceHash()
		if err == nil && file.ownedFormat == "" && entry.SourceDigest == "" {
			entry.SourceDigest, err = runtime.legacyIntegrationConfigDigest(file)
		}
		if err != nil {
			refusals = append(refusals, owned(file.integration, err))
			continue
		}
		if file.ownedFormat != "" {
			entry.Kind = "structured"
			entry.Format = file.ownedFormat
		} else {
			entry.Content = base64.StdEncoding.EncodeToString(payload)
		}
		entries = append(entries, entry)
	}
	if err := domain.CollectRefusals(refusals...); err != nil {
		return nil, err
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
	packages, err := runtime.integrationPackageArtifacts(phase, reader)
	if err != nil {
		return nil, err
	}
	entries = append(entries, packages...)

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

// Package ownership includes the applied login as well as immutable source inputs.
func (runtime Runtime) integrationPackageArtifacts(phase string, reader func(config.MaterializedAsset) ([]byte, error)) ([]integrationArtifact, error) {
	entries := []integrationArtifact{}
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
				return nil, owned(id, err)
			}
			inputs = append(inputs, fmt.Sprintf("%x", sha256.Sum256(payload)))
			// Preserve both installed mode identities across the default change.
			// Home template changes are also tracked by their file artifact digest.
			if config.GitApprovalOff(runtime.environmentValue("CODING_TOOL_GIT_APPROVAL")) {
				inputs = append(inputs, "CODING_TOOL_GIT_APPROVAL=off")
			}
		}
		entries = append(entries, integrationArtifact{ID: id, Kind: "package", Digest: fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(inputs, "\x00"))))})
	}

	return entries, nil
}

type configDiagnosticCapture struct {
	Key   string                                 `json:"key"`
	Files map[string]configDiagnosticCaptureFile `json:"files"`
}
type configDiagnosticCaptureFile struct {
	Path        string `json:"path"`
	Digest      string `json:"digest"`
	Destination string `json:"destination"`
}

type integrationInventoryCapture struct {
	config        *configDiagnosticCapture
	baseline      *IntegrationDiagnosticEvidence
	verifySources func() error
}

func (runtime Runtime) integrationInventory(ctx context.Context, mode string, entries []integrationArtifact, expected string, captures ...integrationInventoryCapture) (integrationObservation, error) {
	var capture integrationInventoryCapture
	if len(captures) != 0 {
		capture = captures[0]
	}
	evidence, baseline, verifySources := capture.config, capture.baseline, capture.verifySources
	if mode == "observe" && runtime.appliedIntegrationInspection {
		mode = "observe-applied"
	}
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
	request := map[string]any{"home": home, "uid": uid, "developer": runtime.devUser(), "entries": entries, "legacy": legacy, "adopt": runtime.AdoptLegacyIntegrations, "expected": expected, "preview_skill_cutover": !runtime.legacyPreviewInstructions, "persist": runtime.persistProjection}
	if evidence != nil {
		request["config_diagnostics"] = evidence
	}
	if runtime.appliedIntegrationInspection {
		// The installed view never retires or forgets a pending init's intent
		// for its desired selection outside the installed scope.
		request["retain_pending_intent"] = true
	}
	if mode == "capture-settings" {
		catalog, err := config.LoadCatalog(runtime.RepositoryRoot)
		if err != nil {
			return integrationObservation{}, err
		}
		settings := catalog.IntegrationInitSettings(runtimeEnvironment(runtime.Environment))
		if err := catalog.ValidateIntegrationInitSettings(settings); err != nil {
			return integrationObservation{}, err
		}
		request["applied_settings"] = settings
		var key []byte
		if baseline != nil {
			if !validIntegrationDiagnosticEvidence(catalog, baseline) {
				return integrationObservation{}, errors.New("invalid applied diagnostics")
			}
			key, _ = hex.DecodeString(baseline.Key)
		} else {
			key = make([]byte, 32)
			if _, err := rand.Read(key); err != nil {
				return integrationObservation{}, err
			}
		}
		evidence := IntegrationDiagnosticEvidence{Key: hex.EncodeToString(key), Digests: catalog.IntegrationDiagnosticDigests(runtimeEnvironment(runtime.Environment), key, runtime.SourceReader)}
		// Capture source evidence from the same snapshot that produced the
		// converged entries, including composed/structured configuration inputs.
		files, err := runtime.guestConfigFilesWithSourceReader(runtime.SourceReader)
		if err != nil {
			return integrationObservation{}, err
		}
		evidence.Files = map[string]IntegrationDiagnosticFile{}
		for _, file := range files {
			definition, ok := catalog.LookupSetting(file.setting)
			if !ok || definition.Sensitive || definition.Kind != config.SettingFile || definition.Application != config.SettingConfigApply {
				continue
			}
			payload, err := file.readSourceAsset(config.MaterializedAsset{Source: file.source, FollowSymlinks: file.followSymlinks})
			if err != nil {
				continue
			}
			evidence.Files[file.setting] = IntegrationDiagnosticFile{Path: file.source, Digest: config.ConfigurationInputDigest(file.setting, definition.Kind, key, payload)}
		}
		request["diagnostics"] = evidence
	}
	if verifySources != nil {
		if err := verifySources(); err != nil {
			return integrationObservation{}, err
		}
	}
	payload, _ := json.Marshal(request)
	result, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, ports.InstanceExecRequest{Command: []string{"python3", "-B", "-c", IntegrationInventoryReader(), mode}, Stdin: payload})
	if err != nil || result.ExitCode != 0 {
		// Stderr reports conflicts only from a completed guest program; a
		// cancelled or failed transport may carry partial output.
		var completed *ports.GuestExitError
		if err != nil && !errors.As(err, &completed) {
			return integrationObservation{}, ownershipRefusal("integration ownership conflict or unavailable evidence; managed artifacts were preserved", result, err)
		}
		type conflictRecord struct{ Reason, Path, Detail string }
		var report struct {
			conflictRecord
			Conflicts []conflictRecord      `json:"conflicts"`
			Initial   bool                  `json:"initial"`
			Retired   []integrationArtifact `json:"retired"`
		}
		if json.Unmarshal(result.Stderr, &report) != nil {
			return integrationObservation{}, ownershipRefusal("integration ownership conflict or unavailable evidence; managed artifacts were preserved", result, err)
		}
		conflicts := report.Conflicts
		if len(conflicts) == 0 && report.Reason != "" {
			conflicts = []conflictRecord{report.conflictRecord}
		}
		causes := []error{}
		if len(conflicts) == 0 {
			return integrationObservation{}, ownershipRefusal("integration ownership conflict or unavailable evidence; managed artifacts were preserved", result, err)
		}
		for _, conflict := range conflicts {
			cause := func() error {
				knownPath := slices.Contains(legacy, conflict.Path)
				for _, entry := range entries {
					knownPath = knownPath || entry.Path == conflict.Path
				}
				switch conflict.Reason {
				case "artifact evidence unavailable":
					if knownPath {
						return integrationOwnershipError{
							message: fmt.Sprintf("integration ownership evidence in yard %s is unavailable at %q; managed artifacts were preserved", runtime.Yard.YardName, conflict.Path),
							retry:   shellquote.Command([]string{"incus", "exec", runtime.Yard.YardInstanceName, "--project", runtime.Yard.IncusProject, "--", "namei", "-l", "--", conflict.Path}),
						}
					}
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
								return integrationOwnershipError{
									message: fmt.Sprintf("integration ownership conflict in yard %s at %q: %s. File preserved.", runtime.Yard.YardName, conflict.Path, detail),
									retry:   shellquote.Command(command),
								}
							}
						}
						retry := runtime.preserveSiblingCommand(conflict.Path, ".subyard-preserved-")
						if !strings.HasPrefix(conflict.Path, home+"/") {
							retry = shellquote.Command([]string{"incus", "exec", runtime.Yard.YardInstanceName, "--project", runtime.Yard.IncusProject, "--", "namei", "-l", "--", conflict.Path})
						}
						return integrationOwnershipError{
							pendingConfiguration: strings.HasPrefix(conflict.Path, home+"/"),
							message:              fmt.Sprintf("integration ownership conflict in yard %s: %s at %q; managed artifacts were preserved", runtime.Yard.YardName, conflict.Reason, conflict.Path),
							retry:                retry,
						}
					}
				}
				return ownershipRefusal("integration ownership conflict or unavailable evidence; managed artifacts were preserved", result, err)
			}()
			causes = append(causes, cause)
		}
		return integrationObservation{Initial: report.Initial, Retired: report.Retired}, domain.CollectRefusals(causes...)
	}

	var observed integrationObservation
	if mode == "observe" || mode == "observe-applied" || mode == "selection" || mode == "settings" {
		if json.Unmarshal(result.Stdout, &observed) != nil || (mode == "observe" || mode == "observe-applied") && len(observed.Fingerprint) != 64 {
			return observed, errors.New("invalid integration ownership observation")
		}
	}
	if observed.Diagnostics != nil {
		catalog, err := config.LoadCatalog(runtime.RepositoryRoot)
		if err != nil {
			return observed, err
		}
		if !validIntegrationDiagnosticEvidence(catalog, observed.Diagnostics) {
			// A newer catalog may have captured names this reader does not
			// understand. Retain only the validated key for subsequent writes;
			// none of those values can establish installed evidence.
			key := observed.Diagnostics.Key
			observed.Diagnostics = nil
			if catalog.ValidIntegrationDiagnosticEvidence(key, nil) {
				observed.Diagnostics = &IntegrationDiagnosticEvidence{Key: key, Digests: map[string]string{}}
			}
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
func (runtime Runtime) observeIntegrationsWithSourceReader(ctx context.Context, phase string, reader func(config.MaterializedAsset) ([]byte, error)) (_ IntegrationPlan, _ integrationObservation, refusal error) {
	instance, err := runtime.integrationPrecondition(ctx)
	if err != nil {
		return IntegrationPlan{}, integrationObservation{}, err
	}
	// Apply and verify name the declaring integration of each refusal.
	step := map[string]integrationStep{"apply": stepObserve, "verify": stepVerify}[phase]
	bind := func(owner string, err error) error {
		if step == "" || err == nil {
			return err
		}
		return runtime.stepFailure(step, owner, err)
	}
	// Service ownership and the package and hook probes are independent of the
	// structured documents and integration inventory. A refusal in any of those
	// checks must still report theirs; the probes need the integration substrate.
	servicesObserved, probesObserved := false, true
	defer func() {
		if refusal != nil && !probesObserved {
			_, probeErr := runtime.observeIntegrationProbes(ctx, bind)
			refusal = domain.CollectRefusals(refusal, probeErr)
		}
		if refusal != nil && !servicesObserved {
			_, _, serviceErr := runtime.integrationServices(ctx, "observe")
			refusal = domain.CollectRefusals(refusal, bind("", serviceErr))
		}
	}()
	if ok, err := runtime.guestCheck(ctx, []string{"sh", "-eu", "-c", `command -v python3 >/dev/null; id "$1" >/dev/null; test -d /srv/agents`, "subyard", runtime.devUser()}); err != nil || !ok {
		return IntegrationPlan{}, integrationObservation{}, firstError(err, errIntegrationSubstrateUnavailable)
	}
	probesObserved = false
	entries, err := runtime.integrationArtifactsWithSourceReader(phase, reader)
	if err != nil {
		return IntegrationPlan{}, integrationObservation{}, bind("", err)
	}
	// Persist transfer state is independent of the inventory. Its projection
	// tells the inventory which links the approved transfer will publish.
	persist, persistErr := runtime.observePersist(ctx, entries)
	runtime.persistProjection = persist.projection()
	observed, err := runtime.integrationInventory(ctx, "observe", entries, "")
	if err != nil {
		return IntegrationPlan{}, observed, bind("", domain.CollectRefusals(persistErr, runtime.collectIntegrationConfigRefusals(ctx, reader, observed, err)))
	}
	if persistErr != nil {
		return IntegrationPlan{}, observed, bind("", domain.CollectRefusals(persistErr, runtime.collectIntegrationConfigRefusals(ctx, reader, observed, nil)))
	}
	persist = persist.retireUnrecordedLinks(observed.Retired)
	// Passive installed admission may ignore redundant labels, but apply must
	// still finish hooks, commit and verification before capturing settings.
	plan := IntegrationPlan{Changed: observed.Changed || persist.changed(), persist: persist}
	plan.sources, plan.packageSources, err = runtime.integrationSourceBindingsWithSourceReader(phase, reader)
	if err != nil {
		return plan, observed, err
	}
	plan.facts = append(slices.Clone(observed.Facts), persist.facts()...)
	scope, _, err := runtime.integrationScopeFromInputs(entries, plan.sources)
	if err != nil {
		return plan, observed, err
	}
	instanceBytes, _ := json.Marshal(instance)
	var guardInstance ports.InstanceInfo
	_ = json.Unmarshal(instanceBytes, &guardInstance)
	guardInstance.Status = ""
	virtualMachine := runtime.Yard.YardKind == domain.YardVM && guardInstance.Type == domain.YardVM
	devices := append(slices.Collect(maps.Keys(guardInstance.Devices)), slices.Collect(maps.Keys(guardInstance.LocalDevices))...)
	devices = append(devices, "eth0")
	guardInstance.Config = cloneIntegrationConfig(guardInstance.Config, devices, virtualMachine)
	guardInstance.LocalConfig = cloneIntegrationConfig(guardInstance.LocalConfig, devices, virtualMachine)
	plan.instance, plan.instanceScope = guardInstance, scope
	plan.profileRoutes = runtime.integrationProfileRoutes(ctx, guardInstance)
	plan.Scope = integrationInstanceScope(scope, guardInstance)
	fingerprints := []string{plan.Scope, plan.sources, observed.Fingerprint, runtime.environmentDefault("ALLOWS_CODING_TOOLS", "true"), persist.Fingerprint}
	if len(plan.profileRoutes) != 0 {
		routeBindings, _ := json.Marshal(plan.profileRoutes)
		fingerprints = append(fingerprints, string(routeBindings))
	}
	causes := []error{}
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
			causes = append(causes, bind(file.integration, err))
			continue
		}
		mode := configmaterial.ModeObserve
		if observed.Initial && runtime.AdoptLegacyIntegrations {
			mode = configmaterial.ModeAssessAdopt
		}
		observation, err := runtime.observeIntegrationConfigFile(ctx, mode, file, payload)
		if err == nil {
			err = runtime.releasedInstalledDocument(file.destination, observation)
		}
		if err != nil {
			causes = append(causes, bind(file.integration, err))
			continue
		}
		fingerprints = append(fingerprints, observation.Fingerprint)
		plan.facts = append(plan.facts, integrationFact{Key: "structured:" + file.destination, Desired: fmt.Sprintf("%x", sha256.Sum256(payload)), Observed: observation.Fingerprint, AtDesired: observation.Converged, owner: file.integration})
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
	if len(causes) == 0 && observed.Initial && runtime.LegacyIntegrationFingerprint != "" && plan.AdoptionFingerprint != runtime.LegacyIntegrationFingerprint {
		return plan, observed, errors.New("legacy integration adoption changed after planning")
	}
	for _, entry := range observed.Retired {
		if entry.Kind != "structured" {
			continue
		}
		observation, err := runtime.observeIntegrationConfig(ctx, entry.Format, configmaterial.ModeAssessRetire, entry.Path, nil)
		if err != nil {
			causes = append(causes, bind(entry.ID, err))
			continue
		}
		fingerprints = append(fingerprints, observation.Fingerprint)
		plan.facts = append(plan.facts, integrationFact{Key: "retired-structured:" + entry.Path, Desired: "absent", Observed: observation.Fingerprint, AtDesired: observation.Converged, owner: entry.ID})
		plan.Changed = plan.Changed || !observation.Converged
		if observation.Release != "" {
			notice := ownershipReleaseNotice(entry.ID, entry.Path, observation)
			plan.Warnings = append(plan.Warnings, notice)
			plan.ownershipReleases = append(plan.ownershipReleases, entry.Path)
			plan.ownershipReleaseNotices = append(plan.ownershipReleaseNotices, notice)
		}
	}
	probesObserved = true
	probes, err := runtime.observeIntegrationProbes(ctx, bind)
	causes = append(causes, err)
	servicesObserved = true
	serviceFingerprint, servicesChanged, err := runtime.integrationServices(ctx, "observe")
	causes = append(causes, bind("", err))
	if err := domain.CollectRefusals(causes...); err != nil {
		return plan, observed, err
	}
	for _, probe := range probes {
		plan.Changed = plan.Changed || !probe.AtDesired
		fingerprints = append(fingerprints, probe.Observed)
		plan.facts = append(plan.facts, probe)
	}
	plan.Changed = plan.Changed || servicesChanged
	fingerprints = append(fingerprints, serviceFingerprint)
	plan.facts = append(plan.facts, integrationFact{Key: "services", Desired: "yard Bubblewrap policy reconciled; role-forbidden utility absent", Observed: serviceFingerprint, AtDesired: !servicesChanged})
	plan.Fingerprint = fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(fingerprints, "\x00"))))
	plan.fingerprints = slices.Clone(fingerprints)
	plan.pendingOnly = observed.Pending && !plan.Changed
	plan.Changed = plan.Changed || observed.Pending
	if plan.Changed {
		plan.Steps = []string{reconcileIntegrationsStep(runtime.environmentValue("CODING_TOOL_INTEGRATIONS"))}
		switch {
		case strings.Contains(serviceFingerprint, "bwrap=retire\n"):
			plan.Steps = append(plan.Steps, "disable the yard's Bubblewrap boot loader and unload its two verified AppArmor profiles")
		case strings.Contains(serviceFingerprint, "bwrap=enable\n"), strings.Contains(serviceFingerprint, "bwrap=repair\n"):
			plan.Steps = append(plan.Steps, "load the two verified Bubblewrap AppArmor profiles inside this yard and enable their boot loader as needed")
		case strings.Contains(serviceFingerprint, "bwrap=conditional\n"):
			plan.Steps = append(plan.Steps, "if Bubblewrap still fails after package installation, load the two distribution AppArmor profiles inside this yard and enable their boot loader")
		}
		if strings.Contains(serviceFingerprint, "bwrap=cleanup\n") {
			plan.Steps = append(plan.Steps, "clear the interrupted Bubblewrap policy ownership record")
		}
		for _, step := range plan.PersistOperationSteps(runtime.Yard.IncusProject + "/" + runtime.Yard.YardInstanceName) {
			plan.Steps = append(plan.Steps, step.Consequence)
		}
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

// observeIntegrationProbes reads the package commands, package checks and
// project hook wiring as plan facts, collecting every independent refusal.
func (runtime Runtime) observeIntegrationProbes(ctx context.Context, bind func(string, error) error) ([]integrationFact, error) {
	var probes []integrationFact
	var causes []error
	probe := func(key, owner string, command []string) {
		ready, err := runtime.guestCheck(ctx, command)
		if err != nil {
			causes = append(causes, bind(owner, err))
			return
		}
		probes = append(probes, integrationFact{Key: key, Desired: "ready", Observed: fmt.Sprint(ready), AtDesired: ready, owner: owner})
	}
	commands, err := runtime.provisionAgentCommands()
	causes = append(causes, bind("", err))
	for _, command := range commands {
		probe("command:"+command, runtime.selectionOwner("COMMAND", command), []string{"sh", "-c", `command -v "$1" >/dev/null`, "subyard", command})
	}
	checks, err := runtime.provisionAgentChecks()
	causes = append(causes, bind("", err))
	for _, check := range checks {
		probe("check:"+check, runtime.selectionOwner("CHECK", check), []string{check})
	}
	ready, err := runtime.projectHooksConverged(ctx)
	if err != nil {
		causes = append(causes, bind("", err))
	} else {
		probes = append(probes, integrationFact{Key: "hooks", Desired: "ready", Observed: fmt.Sprint(ready), AtDesired: ready})
	}
	return probes, domain.CollectRefusals(causes...)
}

// reconcileIntegrationsStep names the selection. Release plans refuse text with
// surrounding whitespace, so an empty selection must not end in a separator.
func reconcileIntegrationsStep(selection string) string {
	selected := strings.Fields(selection)
	if len(selected) == 0 {
		return "reconcile selected integrations: none"
	}
	return "reconcile selected integrations: " + strings.Join(selected, " ")
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
	persist, persistErr := candidate.observePersist(ctx, entries)
	candidate.persistProjection = persist.projection()
	observed, err := candidate.integrationInventory(ctx, "observe", entries, "")
	if err != nil {
		return runtime, IntegrationPlan{}, domain.CollectRefusals(persistErr, candidate.collectIntegrationConfigRefusals(ctx, reader, observed, err))
	}
	files, err := candidate.guestConfigFilesWithSourceReader(reader)
	if err != nil {
		return runtime, IntegrationPlan{}, err
	}
	structuredEvidence := []string{}
	causes := []error{}
	for _, file := range files {
		if file.ownedFormat == "" {
			continue
		}
		payload, err := file.readSource()
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			causes = append(causes, err)
			continue
		}
		observation, err := candidate.observeIntegrationConfigFile(ctx, configmaterial.ModeAssessAdopt, file, payload)
		if err != nil {
			causes = append(causes, err)
			continue
		}
		if observation.Adoptable {
			observed.Adopted = append(observed.Adopted, integrationArtifact{
				ID: file.integration, Kind: "structured", Path: file.destination,
				Format: file.ownedFormat, Digest: fmt.Sprintf("%x", sha256.Sum256(payload)),
			})
			structuredEvidence = append(structuredEvidence, file.destination+"\x00"+observation.Fingerprint)
		}
	}
	if err := domain.CollectRefusals(causes...); err != nil {
		return runtime, IntegrationPlan{}, err
	}
	plan, legacy := adoptionPlan(observed, structuredEvidence)
	if !observed.Initial || !legacy || plan.AdoptionFingerprint == "" {
		return runtime, IntegrationPlan{}, nil
	}
	candidate.LegacyIntegrationFingerprint = plan.AdoptionFingerprint
	return candidate, plan, nil
}

// An inventory refusal does not prevent reading independent structured documents.
// Keep all causes visible, while withholding the unusable operation plan.
func (runtime Runtime) collectIntegrationConfigRefusals(ctx context.Context, reader func(config.MaterializedAsset) ([]byte, error), observed integrationObservation, inventoryError error) error {
	causes := []error{inventoryError}
	files, err := runtime.guestConfigFilesWithSourceReader(reader)
	if err != nil {
		return domain.CollectRefusals(append(causes, err)...)
	}
	for _, file := range files {
		if file.ownedFormat == "" {
			continue
		}
		payload, err := file.readSource()
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			causes = append(causes, owned(file.integration, err))
			continue
		}
		mode := configmaterial.ModeObserve
		if observed.Initial && runtime.AdoptLegacyIntegrations {
			mode = configmaterial.ModeAssessAdopt
		}
		observation, err := runtime.observeIntegrationConfigFile(ctx, mode, file, payload)
		if err == nil {
			err = runtime.releasedInstalledDocument(file.destination, observation)
		}
		causes = append(causes, owned(file.integration, err))
	}
	for _, entry := range observed.Retired {
		if entry.Kind != "structured" {
			continue
		}
		_, err := runtime.observeIntegrationConfig(ctx, entry.Format, configmaterial.ModeAssessRetire, entry.Path, nil)
		causes = append(causes, owned(entry.ID, err))
	}
	return domain.CollectRefusals(causes...)
}

func (runtime Runtime) observeIntegrationConfigFile(ctx context.Context, mode string, file guestConfigFile, payload []byte) (configmaterial.Observation, error) {
	var templates [][]byte
	if mode == configmaterial.ModeAssessAdopt {
		root, err := runtime.legacyIntegrationConfigDirectory(file.integration)
		if err != nil {
			return configmaterial.Observation{}, err
		}
		if root != "" {
			entries, err := os.ReadDir(root)
			if err != nil {
				return configmaterial.Observation{}, err
			}
			for _, entry := range entries {
				if filepath.Ext(entry.Name()) != "."+file.ownedFormat {
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

func (runtime Runtime) legacyIntegrationConfigDirectory(integration string) (string, error) {
	if integration == "" {
		return "", nil
	}
	if !domain.SafeName(integration) {
		return "", errors.New("invalid legacy config owner")
	}
	root := filepath.Join(runtime.RepositoryRoot, "config", "agents", integration, "legacy-config")
	info, err := os.Lstat(root)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("legacy config templates must be a real directory")
	}
	return root, nil
}

// A single package-owned historical raw digest reuses initial source evidence.
// It is bound by the plan and never persisted as ownership in the inventory.
func (runtime Runtime) legacyIntegrationConfigDigest(file guestConfigFile) (string, error) {
	root, err := runtime.legacyIntegrationConfigDirectory(file.integration)
	if err != nil || root == "" {
		return "", err
	}
	payload, err := file.readSourceAsset(config.MaterializedAsset{Source: filepath.Join(root, filepath.Base(file.destination)+".sha256")})
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	digest := strings.TrimSpace(string(payload))
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != sha256.Size || strings.ToLower(digest) != digest {
		return "", errors.New("invalid historical config digest")
	}
	return digest, nil
}

func (runtime Runtime) observeIntegrationConfig(ctx context.Context, format, mode, path string, payload []byte, legacyTemplates ...[]byte) (configmaterial.Observation, error) {
	request, err := configmaterial.Request(format, mode, runtime.devUser(), path, runtime.Yard.DevUID, payload, legacyTemplates...)
	if err != nil {
		return configmaterial.Observation{}, err
	}
	result, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, request)
	if err != nil || result.ExitCode != 0 {
		var completed *ports.GuestExitError
		detail := ""
		switch strings.TrimSpace(string(result.Stderr)) {
		case "config materialization: adoption ownership evidence unavailable":
			detail = "no protected baseline or exact recognized legacy template"
		case "config materialization: adoption owned fields or evidence changed":
			detail = "owned fields or protected evidence changed"
		case "config materialization: invalid current JSON":
			detail = "invalid current JSON"
		case "config materialization: invalid current TOML":
			detail = "invalid current TOML"
		}
		unownedInvalid := configmaterial.UnownedInvalidDestination(result.Stderr, format)
		if unownedInvalid {
			detail = "invalid current " + strings.ToUpper(format)
		}
		if detail != "" && result.ExitCode == 2 && (err == nil || (errors.As(err, &completed) && completed.Code == 2)) {
			retry := shellquote.Command([]string{"incus", "exec", runtime.Yard.YardInstanceName, "--project", runtime.Yard.IncusProject, "--", "sha256sum", "--", path})
			if unownedInvalid {
				retry = runtime.preserveInvalidConfigCommand(path)
			}
			return configmaterial.Observation{}, integrationOwnershipError{
				pendingConfiguration: detail == "no protected baseline or exact recognized legacy template" || detail == "owned fields or protected evidence changed",
				message:              fmt.Sprintf("integration ownership conflict in yard %s at %q: %s; file preserved. Compare with the integration's shipped legacy templates before retrying", runtime.Yard.YardName, path, detail),
				retry:                retry,
			}
		}
		return configmaterial.Observation{}, ownershipRefusal("integration structured configuration ownership conflict", result, err)
	}
	observation, err := configmaterial.ParseObservation(result.Stdout)
	if err == nil && mode == configmaterial.ModeRetire && observation.Release != "" && runtime.Stdout != nil {
		// Report the completed release once, from the call that performed it.
		fmt.Fprintln(runtime.Stdout, boundedNotice("  [ ok ] yard "+runtime.Yard.YardName+": released ownership of "+path+": ", observation, "; Subyard no longer manages it"))
	}
	return observation, err
}

// releasedInstalledDocument refuses an installed-view inspection of a document
// whose ownership a retirement released while the installed view still selects
// it, as after an init interrupted before its applied settings were stored.
// Repairing that view would overlay the operator's document; plain init of the
// desired selection finishes instead. A deliberate enable from the desired
// selection takes fresh ownership and is not refused.
func (runtime Runtime) releasedInstalledDocument(path string, observation configmaterial.Observation) error {
	if !runtime.appliedIntegrationInspection || observation.Release == "" {
		return nil
	}
	return integrationOwnershipError{
		pendingConfiguration: true,
		message: fmt.Sprintf("integration ownership of %q in yard %s was released while its installed configuration still selects it; a release repair would overwrite the released document; run yard -Y %s init instead",
			path, runtime.Yard.YardName, runtime.Yard.YardName),
		retry: shellquote.Command([]string{"yard", "-Y", runtime.Yard.YardName, "init"}),
	}
}

// ownershipReleaseNotice previews a retirement that only releases ownership:
// changed owned fields no longer prove Subyard's values, and a missing document
// has nothing to remove. The document itself stays unchanged.
func ownershipReleaseNotice(integration, path string, observation configmaterial.Observation) string {
	return boundedNotice("retiring "+integration+" releases ownership of "+path+": ", observation, "")
}

// Activation warnings share the release diagnostic bound: the notice lists the
// retained fields when the whole list fits, and otherwise only counts them.
const ownershipNoticeLimit = 384

func boundedNotice(prefix string, observation configmaterial.Observation, suffix string) string {
	reason, fields := "owned fields changed after Subyard applied them", "no owned fields remain"
	if observation.Release == configmaterial.ReleaseMissingDocument {
		reason = "the document is missing"
	}
	switch {
	case len(observation.Released) != 0:
		fields = "fields left in place: " + strings.Join(observation.Released, ", ")
	case observation.ReleasedCount != 0:
		fields = fmt.Sprintf("%d fields left in place", observation.ReleasedCount)
	case observation.Release == configmaterial.ReleaseMissingDocument:
		fields = "nothing to remove"
	}
	if maximum := ownershipNoticeLimit / 2; len(prefix) > maximum {
		prefix = truncateText(prefix, maximum-len("...: ")) + "...: "
	}
	notice := prefix + reason + "; " + fields + suffix
	if len(notice) > ownershipNoticeLimit {
		notice = prefix + reason + "; " + fmt.Sprintf("%d fields left in place", observation.ReleasedCount) + suffix
	}
	return notice
}

// truncateText cuts at a UTF-8 boundary within limit bytes.
func truncateText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}

// IntegrationOwnershipNotices previews structured ownership releases for operations
// such as init that do not render the complete integration plan.
func (runtime Runtime) IntegrationOwnershipNotices(ctx context.Context) ([]string, error) {
	if ready, err := runtime.incusReady(ctx); err != nil || !ready {
		return nil, err
	}
	state, err := runtime.reconcileState(ctx)
	if err != nil || !state.InstanceFound {
		return nil, err
	}
	if !strings.EqualFold(state.Instance.Status, "running") || runtime.Executor == nil {
		return nil, nil
	}
	ready, err := runtime.guestCheck(ctx, []string{"sh", "-eu", "-c", `command -v python3 >/dev/null; id "$1" >/dev/null; test -d /srv/agents`, "subyard", runtime.devUser()})
	if err != nil || !ready {
		return nil, nil
	}
	// Preview structured ownership releases before consent. A refused or failed
	// inspection is reported by the plan that applies it, not by this preview.
	plan, _ := runtime.IntegrationPlan(ctx)
	return plan.ownershipReleaseNotices, nil
}

// AppliedIntegrationPlan inspects committed consumers for passive release admission.
// Observation preserves retry labels; in-scope pending intent keeps Changed true
// so apply finishes its checked tail. Pending init outside this scope stays intact.
func (runtime Runtime) AppliedIntegrationPlan(ctx context.Context) (IntegrationPlan, error) {
	runtime.appliedIntegrationInspection = true
	return runtime.IntegrationPlan(ctx)
}

// WithInstalledConfiguration binds passive ownership inspection to a context
// whose init-owned inputs have already been projected by the release boundary.
func (runtime Runtime) WithInstalledConfiguration() Runtime {
	runtime.appliedIntegrationInspection = true
	return runtime
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
	return fresh.withInstance(instance)
}

// withInstance recomputes the comparison identity for a projected instance. A
// projection exists only to be compared and may be projected again.
func (plan IntegrationPlan) withInstance(instance ports.InstanceInfo) (IntegrationPlan, error) {
	plan.instance = instance
	plan.Scope = integrationInstanceScope(plan.instanceScope, instance)
	parts := slices.Clone(plan.fingerprints)
	if len(parts) == 0 {
		return plan, domain.ErrPlanStale
	}
	parts[0] = plan.Scope
	plan.Fingerprint = fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(parts, "\x00"))))
	return plan, nil
}

// VerifiedNativeMigrations names the native migrations a transition's own journal
// proves were authorized and verified for this yard after its plan was approved.
type VerifiedNativeMigrations struct{ RegisteredYard, GuestDiskTmp bool }

// ProjectIntegrationPlanAfterNativeMigrations removes only the exact instance
// metadata those verified migrations publish, and only where the approved plan
// lacked it. A foreign value, a marker no verified migration explains, or any
// other instance change stays in the comparison and refuses.
func ProjectIntegrationPlanAfterNativeMigrations(approved, fresh IntegrationPlan, verified VerifiedNativeMigrations) (IntegrationPlan, error) {
	if approved.Fingerprint == fresh.Fingerprint || !verified.RegisteredYard && !verified.GuestDiskTmp {
		return fresh, nil
	}
	instance := fresh.instance
	instance.Config, instance.LocalConfig = maps.Clone(instance.Config), maps.Clone(instance.LocalConfig)
	published := func(key, value string) bool {
		_, config := approved.instance.Config[key]
		_, local := approved.instance.LocalConfig[key]
		return !config && !local && value != "" && instance.Config[key] == value && instance.LocalConfig[key] == value
	}
	projected := false
	for key, value := range fresh.instance.LocalConfig {
		owned := verified.GuestDiskTmp && key == "user.subyard.disk_tmp" && value == "v1"
		if name, receipt := strings.CutPrefix(key, "user.subyard.resource."); receipt && verified.RegisteredYard {
			device := fresh.instance.LocalDevices[name]
			owned = device["type"] == "proxy" && maps.Equal(device, fresh.instance.Devices[name]) && value == (resource.ProxyContract{}).OwnershipValue(device)
		}
		if owned && published(key, value) {
			delete(instance.Config, key)
			delete(instance.LocalConfig, key)
			projected = true
		}
	}
	if !projected {
		return fresh, nil
	}
	return fresh.withInstance(instance)
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

func (runtime Runtime) ApplyIntegrations(ctx context.Context, plan IntegrationPlan) error {
	fresh, observed, err := runtime.observeIntegrationsPhase(ctx, "apply")
	if err != nil {
		return runtime.stepFailure(stepObserve, "", err)
	}
	if err := CheckIntegrationPlan(plan, fresh); err != nil {
		return runtime.integrationPlanDiagnostic(err, plan, fresh)
	}
	if err := runtime.checkIntegrationHookScope(); err != nil {
		return runtime.stepFailure(stepHookScope, "", err)
	}
	if !fresh.Changed {
		return runtime.stepFailure(stepSettings, "", runtime.storeAppliedIntegrationSettings(ctx))
	}
	execution := runtime
	if err := runtime.reconcileIntegrationServices(ctx); err != nil {
		return runtime.stepFailure(stepServices, "", err)
	}
	entries, err := runtime.integrationArtifactsPhase("apply")
	if err != nil {
		return runtime.stepFailure(stepArtifacts, "", err)
	}
	expectedInventory := ""
	if observed.Initial {
		expectedInventory = observed.Fingerprint
	}
	// The approved transfer runs before inventory apply, which then records the
	// exact link that transfer published (the projection bound in the plan).
	runtime.persistProjection = fresh.persist.projection()
	persistOwner := fresh.persist.changedOwner()
	if err := runtime.checkApprovedPersist(fresh.persist); err != nil {
		return runtime.stepFailure(stepPersistApproval, persistOwner, err)
	}
	if err := runtime.applyPersist(ctx, entries, fresh.persist); err != nil {
		return runtime.stepFailure(stepPersist, persistOwner, err)
	}
	if _, err := runtime.integrationInventory(ctx, "apply", entries, expectedInventory); err != nil {
		return runtime.stepFailure(stepInventoryApply, "", err)
	}
	if err := runtime.RefreshConfigs(ctx); err != nil {
		return runtime.stepFailure(stepConfigs, "", err)
	}
	if err := execution.runScript(ctx, runtime.Stderr, "reconcile-integrations.sh"); err != nil {
		return runtime.stepFailure(stepScript, "", err)
	}
	// Keep the inventory pending until hooks succeed so the same requested
	// selection remains retryable after a transient hook failure.
	if err := execution.captureProvisionedProjectHooks(ctx); err != nil {
		return runtime.stepFailure(stepHookCapture, "", err)
	}
	if err := execution.RunProjectHooks(ctx); err != nil {
		return runtime.stepFailure(stepHooks, "", err)
	}
	// Retirement effects follow the hooks: a failed init leaves the installed
	// state, which the next successful init retires.
	for _, entry := range observed.Retired {
		if entry.Kind == "structured" {
			if _, err := runtime.observeIntegrationConfig(ctx, entry.Format, configmaterial.ModeRetire, entry.Path, nil); err != nil {
				return runtime.stepFailure(stepRetire, entry.ID, err)
			}
		}
	}
	if _, err := runtime.integrationInventory(ctx, "commit", entries, ""); err != nil {
		return runtime.stepFailure(stepInventoryCommit, "", err)
	}
	verified, _, err := execution.observeIntegrationsPhase(ctx, "verify")
	if err != nil {
		return runtime.stepFailure(stepVerify, "", err)
	}
	if verified.Changed {
		return runtime.integrationDiagnostic(errors.New("integration reconcile verification failed; desired selection retained for retry"),
			profile.IntegrationDefinition{Name: runtime.driftOwner(verified)}, "verify", "convergence-failed")
	}
	return runtime.stepFailure(stepSettings, "", runtime.storeAppliedIntegrationSettings(ctx))
}

type appliedIntegrationSettingsCapture struct {
	runtime     Runtime
	plan        string
	inventory   string
	entries     []integrationArtifact
	diagnostics *IntegrationDiagnosticEvidence
	sources     *diagnosticSourceSnapshot
}

func (capture *appliedIntegrationSettingsCapture) Binding() string { return capture.plan }

func (capture *appliedIntegrationSettingsCapture) Apply(ctx context.Context) error {
	plan, _, err := capture.runtime.observeIntegrations(ctx)
	if err != nil {
		return err
	}
	if plan.Changed || plan.StateBinding() != capture.plan {
		return domain.ErrPlanStale
	}
	// The inventory fingerprint binds the persist projection it was observed with.
	runtime := capture.runtime
	runtime.SourceReader = capture.sources.read
	runtime.persistProjection = plan.persist.projection()
	_, err = runtime.integrationInventory(ctx, "capture-settings", capture.entries, capture.inventory, integrationInventoryCapture{baseline: capture.diagnostics, verifySources: capture.sources.verify})
	return err
}

func (runtime Runtime) PrepareAppliedIntegrationSettings(ctx context.Context) (ports.PreparedSettingsCapture, error) {
	configuration, err := runtime.AppliedIntegrationConfiguration(ctx)
	if err != nil {
		return nil, err
	}
	if !configuration.Installed || configuration.Settings != nil {
		return nil, nil
	}
	// Retain the installed projection for revalidation and publication as well.
	runtime, err = runtime.withAppliedIntegrationSettings(configuration.Settings, configuration.Identity)
	if err != nil {
		return nil, err
	}
	plan, observed, err := runtime.observeIntegrations(ctx)
	if err != nil {
		return nil, err
	}
	if plan.Changed {
		return nil, errors.New("legacy integration settings have no verified applied baseline; reconcile the existing settings with yard init before editing them")
	}
	sources := newDiagnosticSourceSnapshot(runtime.SourceReader)
	entries, err := runtime.integrationArtifactsWithSourceReader("inspect", sources.read)
	if err != nil {
		return nil, err
	}
	return &appliedIntegrationSettingsCapture{runtime: runtime, plan: plan.StateBinding(), inventory: observed.Fingerprint, entries: entries, diagnostics: observed.Diagnostics, sources: sources}, nil
}

func (runtime Runtime) storeAppliedIntegrationSettings(ctx context.Context) error {
	plan, observed, err := runtime.observeIntegrations(ctx)
	if err != nil {
		return err
	}
	if plan.Changed {
		return domain.ErrPlanStale
	}
	sources := newDiagnosticSourceSnapshot(runtime.SourceReader)
	entries, err := runtime.integrationArtifactsWithSourceReader("verify", sources.read)
	if err != nil {
		return err
	}
	runtime.SourceReader = sources.read
	runtime.persistProjection = plan.persist.projection()
	_, err = runtime.integrationInventory(ctx, "capture-settings", entries, observed.Fingerprint, integrationInventoryCapture{baseline: observed.Diagnostics, verifySources: sources.verify})
	return err
}

type diagnosticSourceSnapshot struct {
	reader func(config.MaterializedAsset) ([]byte, error)
	values map[configSourceKey]diagnosticSourceValue
}

type diagnosticSourceValue struct {
	payload []byte
	err     error
}

func newDiagnosticSourceSnapshot(reader func(config.MaterializedAsset) ([]byte, error)) *diagnosticSourceSnapshot {
	return &diagnosticSourceSnapshot{reader: reader, values: map[configSourceKey]diagnosticSourceValue{}}
}

func (snapshot *diagnosticSourceSnapshot) read(asset config.MaterializedAsset) ([]byte, error) {
	key := configSourceKey{path: asset.Source, follow: asset.FollowSymlinks}
	if value, ok := snapshot.values[key]; ok {
		return slices.Clone(value.payload), value.err
	}
	var payload []byte
	var err error
	if snapshot.reader == nil {
		payload, err = asset.ReadSource()
	} else {
		payload, err = snapshot.reader(asset)
	}
	snapshot.values[key] = diagnosticSourceValue{payload: slices.Clone(payload), err: err}
	return payload, err
}

func (snapshot *diagnosticSourceSnapshot) verify() error {
	for key, old := range snapshot.values {
		// Unreadable optional sources produce no diagnostic evidence.
		if old.err != nil {
			continue
		}
		asset := config.MaterializedAsset{Source: key.path, FollowSymlinks: key.follow}
		var fresh []byte
		var err error
		if snapshot.reader == nil {
			fresh, err = asset.ReadSource()
		} else {
			fresh, err = snapshot.reader(asset)
		}
		if err != nil || !bytes.Equal(old.payload, fresh) {
			return domain.ErrPlanStale
		}
	}
	return nil
}

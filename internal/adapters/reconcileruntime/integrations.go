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

// integrationOwnershipRefusal is a refusal the guest ownership program itself
// returned without a recognized public detail. Its text stays unchanged.
type integrationOwnershipRefusal string

func (err integrationOwnershipRefusal) Error() string                  { return string(err) }
func (integrationOwnershipRefusal) integrationOwnershipConflict() bool { return true }

// ownershipRefusal labels a failed guest ownership command. Only a command the
// guest completed reports ownership state; transport and host failures remain
// inspection failures with the same text.
func ownershipRefusal(message string, result ports.InstanceExecResult, err error) error {
	if completedGuestFailure(result, err) {
		return integrationOwnershipRefusal(message)
	}
	return errors.New(message)
}

func completedGuestFailure(result ports.InstanceExecResult, err error) bool {
	var completed *ports.GuestExitError
	if err == nil {
		return result.ExitCode != 0
	}
	return errors.As(err, &completed) && completed.Code != 0 && (result.ExitCode == 0 || completed.Code == result.ExitCode)
}

// IntegrationOwnershipConflict reports whether an integration observation
// failed because the guest refused unproven or changed owned state, rather than
// because the state could not be inspected.
func IntegrationOwnershipConflict(err error) bool {
	switch cause := err.(type) {
	case interface{ Unwrap() []error }:
		return slices.ContainsFunc(cause.Unwrap(), IntegrationOwnershipConflict)
	case interface{ integrationOwnershipConflict() bool }:
		if cause.integrationOwnershipConflict() {
			return true
		}
	}
	return err != nil && IntegrationOwnershipConflict(errors.Unwrap(err))
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

	// ownershipReleases lists declared destinations for the releases we observed.
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

// OwnershipReleases lists declared structured destinations whose retirement releases ownership.
func (plan IntegrationPlan) OwnershipReleases() []string { return slices.Clone(plan.ownershipReleases) }

// HasOwnershipReleases reports whether a retirement releases changed or missing
// owned configuration, including historical documents with no current declaration.
func (plan IntegrationPlan) HasOwnershipReleases() bool {
	return len(plan.ownershipReleaseNotices) != 0
}

type integrationProfileRoute struct {
	Owner, MarkerKey, DeviceName, Marker string
	Device                               map[string]string
}

type integrationFact struct {
	item      integrationWorkItem
	persist   *persistStep
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

	declaredPath, declaredOwner string
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

	adoptions []integrationAdoption
}

// An adoption is built from request declarations after matching the inventory.
type integrationAdoption struct{ owner, kind, path string }

func (adoption integrationAdoption) step() string {
	return "adopt existing " + adoption.kind + " for " + adoption.owner + " under integration management: " + adoption.path
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
	for _, adoption := range observed.adoptions {
		plan.Adoption = append(plan.Adoption, adoption.path)
		legacy = legacy || adoption.owner != "_projects"
	}
	if legacy {
		evidence := append([]string{observed.Fingerprint}, structuredEvidence...)
		plan.AdoptionFingerprint = fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(evidence, "\x00"))))
		plan.Fingerprint = plan.AdoptionFingerprint
		plan.Changed = true
		for _, adoption := range observed.adoptions {
			plan.Steps = append(plan.Steps, adoption.step())
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

// Provisioning repairs a missing substrate, but independent refusals still block init.
func onlyMissingIntegrationSubstrate(err error) bool {
	return errors.Is(err, errIntegrationSubstrateUnavailable) && len(domain.RefusalCauses(err)) == 1
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
		item := runtime.integrationRead(phase, "config source", file.integration).atDestination(file.destination)
		payload, err := file.readSource()
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			refusals = append(refusals, item.refusal(err))
			continue
		}
		entry := integrationArtifact{ID: file.integration, Kind: "file", Path: file.destination, Digest: fmt.Sprintf("%x", sha256.Sum256(payload))}
		entry.SourceDigest, err = file.legacySourceHash()
		if err != nil {
			refusals = append(refusals, item.refusal(err))
			continue
		}
		if file.ownedFormat == "" && entry.SourceDigest == "" {
			entry.SourceDigest, err = runtime.legacyIntegrationConfigDigest(file)
			if err != nil {
				refusals = append(refusals, item.refusal(err))
				continue
			}
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
			refusals = append(refusals, errors.New("invalid integration link"))
			continue
		}
		relative, err := safeGuestRelativePath(parts[0])
		if err != nil {
			refusals = append(refusals, err)
			continue
		}
		id := ""
		for _, candidate := range strings.Fields(runtime.environmentValue("CODING_TOOL_INTEGRATIONS")) {
			if slices.Contains(strings.Fields(runtime.environmentValue("AGENT_"+candidate+"_PERSIST")), link) {
				id = candidate
				break
			}
		}
		if id == "" {
			refusals = append(refusals, errors.New("integration link has no selected owner"))
			continue
		}
		entries = append(entries, integrationArtifact{ID: id, Kind: "link", Path: "/home/" + runtime.devUser() + "/" + relative, Target: parts[1], FileTarget: len(parts) == 3 && parts[2] == "file", Digest: fmt.Sprintf("%x", sha256.Sum256([]byte(parts[1])))})
	}
	packages, err := runtime.integrationPackageArtifacts(phase, reader)
	refusals = append(refusals, err)
	entries = append(entries, packages...)

	dispatcher, err := os.ReadFile(filepath.Join(runtime.RepositoryRoot, "config", "projects-changed.sh"))
	refusals = append(refusals, err)
	hooks := []string{}
	for _, id := range strings.Fields(runtime.environmentValue("CODING_TOOL_INTEGRATIONS")) {
		hook := runtime.environmentValue("AGENT_" + id + "_PROJECTS_CHANGED")
		if hook != "" {
			if !safeCommand(hook) {
				item := runtime.integrationRead(phase, "project hook declaration", id)
				refusals = append(refusals, item.refusal(errors.New("invalid integration project hook")))
				continue
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
	return entries, collectIntegrationRefusals(refusals...)
}

// Package ownership includes the applied login as well as immutable source inputs.
func (runtime Runtime) integrationPackageArtifacts(phase string, reader func(config.MaterializedAsset) ([]byte, error)) ([]integrationArtifact, error) {
	entries := []integrationArtifact{}
	definitions, err := runtime.integrationDefinitions()
	if err != nil {
		return nil, err
	}
	var refusals []error
packages:
	for _, id := range strings.Fields(runtime.environmentValue("CODING_TOOL_INTEGRATIONS")) {
		inputs := []string{id, runtime.devUser(), runtime.environmentValue("CODING_TOOL_INTEGRATIONS")}
		for _, definition := range definitions {
			if definition.Name == id {
				binding, err := runtime.integrationPackageBindingWithSourceReader(definition, reader)
				if err != nil {
					refusals = append(refusals, runtime.integrationDiagnostic(err, definition, phase, "native-unavailable"))
					continue packages
				}
				inputs = append(inputs, binding)
			}
		}
		provision := runtime.environmentValue("AGENT_" + id + "_PROVISION")
		if provision != "" {
			read := reader
			if runtime.captureSources != nil {
				read = runtime.captureSources.readerFor(runtime.integrationItem("apply inventory capture settings", id).forSource("provision source"))
			}
			payload, err := (guestConfigFile{readAsset: read}).readSourceAsset(config.MaterializedAsset{Source: provision})
			if err != nil {
				item := runtime.integrationRead(phase, "package source", id)
				refusals = append(refusals, item.refusal(err))
				continue
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

	return entries, domain.CollectRefusals(refusals...)
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
	item          integrationWorkItem
	config        *configDiagnosticCapture
	baseline      *IntegrationDiagnosticEvidence
	verifySources func(Runtime) error
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
	pathOwners := map[string][]string{}
	declaredPaths := map[string]string{}
	addOwner := func(path, owner string) {
		declaredPaths[path] = path
		if !slices.Contains(pathOwners[path], owner) {
			pathOwners[path] = append(pathOwners[path], owner)
		}
	}
	for _, entry := range entries {
		if entry.Path != "" {
			addOwner(entry.Path, entry.ID)
		}
	}
	definitions, err := runtime.integrationDefinitions()
	if err != nil {
		return integrationObservation{}, err
	}
	for _, definition := range definitions {
		configIDs := []string{"config", "rules"}
		for _, artifact := range definition.Artifacts {
			path := home + "/" + artifact.Destination
			legacy = append(legacy, path)
			addOwner(path, definition.Name)
			if artifact.Kind == "config" && !slices.Contains(configIDs, artifact.ID) {
				configIDs = append(configIDs, artifact.ID)
			}
		}
		// A deselected owner can still have a declared destination in its settings.
		for _, asset := range configIDs {
			if relative, err := safeGuestRelativePath(runtime.environmentValue("AGENT_" + definition.Name + "_" + strings.ToUpper(asset) + "_DEST")); err == nil {
				addOwner(home+"/"+relative, definition.Name)
			}
		}
	}
	bindRetiredArtifacts := func(artifacts []integrationArtifact) {
		for index := range artifacts {
			entry := &artifacts[index]
			for _, definition := range definitions {
				if definition.Name == entry.ID {
					entry.declaredOwner = definition.Name
					break
				}
			}
			for _, owner := range pathOwners[entry.Path] {
				if owner == entry.ID {
					entry.declaredOwner = owner
					entry.declaredPath = declaredPaths[entry.Path]
					break
				}
			}
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
				addOwner(path, id)
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
		if err := verifySources(runtime); err != nil {
			return integrationObservation{}, err
		}
	}
	payload, _ := json.Marshal(request)
	result, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, ports.InstanceExecRequest{Command: []string{"python3", "-B", "-c", IntegrationInventoryReader(), mode}, Stdin: payload})
	const unavailable = "integration ownership conflict or unavailable evidence; managed artifacts were preserved"
	if err != nil || result.ExitCode != 0 {
		if !completedGuestFailure(result, err) {
			return integrationObservation{}, ownershipRefusal(unavailable, result, err)
		}
		type conflictRecord struct{ Reason, Path, Detail string }
		var report struct {
			conflictRecord
			Conflicts []conflictRecord      `json:"conflicts"`
			Initial   bool                  `json:"initial"`
			Retired   []integrationArtifact `json:"retired"`
		}
		if json.Unmarshal(result.Stderr, &report) != nil {
			return integrationObservation{}, ownershipRefusal(unavailable, result, err)
		}
		conflicts := report.Conflicts
		if len(conflicts) == 0 && report.Reason != "" {
			conflicts = []conflictRecord{report.conflictRecord}
		}
		causes := []error{}
		if len(conflicts) == 0 {
			return integrationObservation{}, ownershipRefusal(unavailable, result, err)
		}
		item := capture.item
		if item.Input == "" {
			switch mode {
			case "observe", "observe-applied":
				item = runtime.integrationRead("inspect", "inventory", "")
			case "apply":
				item = runtime.integrationItem("apply inventory", "")
			default:
				item = runtime.integrationItem("apply inventory "+strings.ReplaceAll(mode, "-", " "), "")
			}
		}
		for _, conflict := range conflicts {
			owners := pathOwners[conflict.Path]
			path := declaredPaths[conflict.Path]
			cause := func() error {
				knownPath := len(owners) != 0
				switch conflict.Reason {
				case "artifact evidence unavailable":
					if knownPath {
						return integrationOwnershipFailure(
							fmt.Sprintf("integration ownership evidence in yard %s is unavailable at %q; managed artifacts were preserved", runtime.Yard.YardName, path),
							shellquote.RecoveryCommand([]string{"incus", "exec", runtime.Yard.YardInstanceName, "--project", runtime.Yard.IncusProject, "--", "namei", "-l", "--", path}), false)
					}
				case "owned artifact drift", "unowned selected artifact", "legacy integration artifact has no ownership evidence":
					if knownPath && path != "" {
						if conflict.Reason == "unowned selected artifact" &&
							(path == "/usr/local/libexec/subyard/projects-changed" || path == "/etc/subyard/agent-project-hooks") {
							detail := ""
							probe := []string{"namei", "-l", "--", path}
							switch conflict.Detail {
							case "unrecognized core content":
								detail = "contents do not match the current or a recognized legacy Subyard file; compare with the shipped file before replacing it"
								probe = []string{"sha256sum", "--", path}
							case "unsafe core metadata":
								detail = "file must be regular, owned by root:root, with mode 0755 for projects-changed or 0644 for agent-project-hooks"
							case "unsafe core ancestor":
								detail = "parent directories must be owned by root:root and not writable by group or others"
							}
							if detail != "" {
								command := append([]string{"incus", "exec", runtime.Yard.YardInstanceName, "--project", runtime.Yard.IncusProject, "--"}, probe...)
								return integrationOwnershipFailure(
									fmt.Sprintf("integration ownership conflict in yard %s at %q: %s. File preserved.", runtime.Yard.YardName, path, detail),
									shellquote.RecoveryCommand(command), false)
							}
						}
						inHome := strings.HasPrefix(path, home+"/")
						command := shellquote.RecoveryCommand([]string{"incus", "exec", runtime.Yard.YardInstanceName, "--project", runtime.Yard.IncusProject, "--", "namei", "-l", "--", path})
						if inHome {
							command = runtime.preserveOwnedArtifactCommand(path)
						}
						return integrationOwnershipFailure(
							fmt.Sprintf("integration ownership conflict in yard %s: %s at %q; managed artifacts were preserved", runtime.Yard.YardName, conflict.Reason, path),
							command, inHome)
					}
				}
				return ownershipRefusal(unavailable, result, err)
			}()
			if len(owners) == 0 {
				finding := item.forReportedArtifact(integrationArtifact{Path: conflict.Path})
				causes = append(causes, finding.refusal(cause))
				continue
			}
			finding := item.atDestination(path)
			if len(owners) == 1 {
				finding = finding.withOwner(owners[0])
			}
			causes = append(causes, finding.refusal(cause))
		}
		bindRetiredArtifacts(report.Retired)
		return integrationObservation{Initial: report.Initial, Retired: report.Retired}, domain.CollectRefusals(causes...)
	}
	var observed integrationObservation
	if mode == "observe" || mode == "observe-applied" || mode == "selection" || mode == "settings" {
		if json.Unmarshal(result.Stdout, &observed) != nil || (mode == "observe" || mode == "observe-applied") && !profileRuntimeDigest.MatchString(observed.Fingerprint) {
			return integrationObservation{}, errors.New("invalid integration ownership observation")
		}
	}
	bindRetiredArtifacts(observed.Retired)
	// Only the requested artifacts are eligible for adoption. Guest fields
	// select a declaration and supply no text to the resulting plan.
	for _, entry := range entries {
		if slices.ContainsFunc(observed.Adopted, func(evidence integrationArtifact) bool {
			return evidence.ID == entry.ID && evidence.Kind == entry.Kind && evidence.Path == entry.Path && entry.Path != ""
		}) {
			observed.adoptions = append(observed.adoptions, integrationAdoption{entry.ID, entry.Kind, entry.Path})
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
func (runtime Runtime) observeIntegrationsWithSourceReader(ctx context.Context, phase string, reader func(config.MaterializedAsset) ([]byte, error)) (IntegrationPlan, integrationObservation, error) {
	instance, err := runtime.integrationPrecondition(ctx)
	if err != nil {
		if phase == "verify" {
			err = runtime.integrationRead(phase, "", "").refusal(err)
		}
		return IntegrationPlan{}, integrationObservation{}, err
	}
	if ok, err := runtime.guestCheck(ctx, []string{"sh", "-eu", "-c", `command -v python3 >/dev/null; id "$1" >/dev/null; test -d /srv/agents`, "subyard", runtime.devUser()}); err != nil || !ok {
		_, _, serviceErr := runtime.integrationServices(ctx, "observe")
		return IntegrationPlan{}, integrationObservation{}, collectIntegrationRefusals(
			runtime.integrationRead(phase, "", "").refusal(firstError(err, errIntegrationSubstrateUnavailable)),
			runtime.integrationRead(phase, "services", "").refusal(serviceErr),
		)
	}
	entries, artifactsErr := runtime.integrationArtifactsWithSourceReader(phase, reader)
	causes := []error{runtime.integrationRead(phase, "inventory prepare", "").refusal(artifactsErr)}
	// Persist mappings are independent of config source bytes. Inventory needs
	// the complete artifact list and the projections of the mappings observed.
	persist, persistErr := runtime.observePersist(ctx, entries, phase)
	causes = append(causes, persistErr)
	var observed integrationObservation
	var inventoryErr error
	if artifactsErr == nil {
		runtime.persistProjection = persist.projection()
		inventoryItem := runtime.integrationRead(phase, "inventory", "")
		observed, inventoryErr = runtime.integrationInventory(ctx, "observe", entries, "", integrationInventoryCapture{item: inventoryItem})
		runtime.bindInventoryFacts(observed.Facts, entries, observed.Retired, runtime.persistProjection, inventoryItem)
		causes = append(causes, inventoryItem.refusal(inventoryErr))
		if inventoryErr == nil {
			persist = persist.retireUnrecordedLinks(observed.Retired)
		}
	}

	plan := IntegrationPlan{persist: persist, Changed: inventoryErr == nil && observed.Changed}
	plan.sources, plan.packageSources, err = runtime.integrationSourceBindingsWithSourceReader(phase, reader)
	causes = append(causes, runtime.integrationRead(phase, "source binding", "").refusal(err))
	plan.facts = slices.Clone(observed.Facts)
	record := func(item integrationWorkItem, fact integrationFact) {
		fact.item = item
		plan.facts = append(plan.facts, fact)
		plan.Changed = plan.Changed || !fact.AtDesired
	}
	for _, fact := range persist.facts() {
		plan.facts = append(plan.facts, fact)
		plan.Changed = plan.Changed || fact.item.Input != ""
	}

	var fingerprints []string
	structuredAdoptionEvidence := []string{}
	mode := configmaterial.ModeObserve
	if observed.Initial && runtime.AdoptLegacyIntegrations {
		mode = configmaterial.ModeAssessAdopt
	}
	filesErr := runtime.observeDesiredIntegrationConfigs(ctx, phase, mode, reader,
		func(item integrationWorkItem, file guestConfigFile, payload []byte, observation configmaterial.Observation) error {
			if err := runtime.releasedInstalledDocument(file.destination, observation); err != nil {
				return err
			}
			fingerprints = append(fingerprints, observation.Fingerprint)
			digest := fmt.Sprintf("%x", sha256.Sum256(payload))
			record(item, integrationFact{Key: "structured:" + file.destination, Desired: digest, Observed: observation.Fingerprint, AtDesired: observation.Converged})
			if mode == configmaterial.ModeAssessAdopt && observation.Adoptable {
				observed.adoptions = append(observed.adoptions, integrationAdoption{file.integration, "structured", file.destination})
				structuredAdoptionEvidence = append(structuredAdoptionEvidence, file.destination+"\x00"+observation.Fingerprint)
			}
			return nil
		})
	causes = append(causes, filesErr)
	adoption, _ := adoptionPlan(observed, structuredAdoptionEvidence)
	plan.Adoption = adoption.Adoption
	plan.AdoptionFingerprint = adoption.AdoptionFingerprint
	if artifactsErr == nil && inventoryErr == nil && filesErr == nil && observed.Initial &&
		runtime.LegacyIntegrationFingerprint != "" && plan.AdoptionFingerprint != runtime.LegacyIntegrationFingerprint {
		causes = append(causes, errors.New("legacy integration adoption changed after planning"))
	}
	retiredErr := runtime.observeRetiredIntegrationConfigs(ctx, phase, observed.Retired, reader,
		func(item integrationWorkItem, entry integrationArtifact, observation configmaterial.Observation) {
			fingerprints = append(fingerprints, observation.Fingerprint)
			record(item, integrationFact{Key: "retired-structured:" + entry.Path, Desired: "absent", Observed: observation.Fingerprint, AtDesired: observation.Converged})
			if observation.Release != "" {
				notice := ownershipReleaseNotice(entry.declaredOwner, entry.declaredPath, observation)
				plan.Warnings = append(plan.Warnings, notice)
				if entry.declaredPath != "" {
					plan.ownershipReleases = append(plan.ownershipReleases, entry.declaredPath)
				}
				plan.ownershipReleaseNotices = append(plan.ownershipReleaseNotices, notice)
			}
		})
	causes = append(causes, retiredErr)
	commands, err := runtime.provisionAgentCommands(phase)
	causes = append(causes, err)
	for _, command := range commands {
		ready, err := runtime.guestCheck(ctx, []string{"sh", "-c", `command -v "$1" >/dev/null`, "subyard", command.command})
		if err != nil {
			causes = append(causes, command.item.refusal(err))
			continue
		}
		fingerprints = append(fingerprints, fmt.Sprint(ready))
		record(command.item, integrationFact{Key: "command:" + command.command, Desired: "ready", Observed: fmt.Sprint(ready), AtDesired: ready})
	}
	checks, err := runtime.provisionAgentChecks(phase)
	causes = append(causes, err)
	for _, check := range checks {
		ready, err := runtime.guestCheck(ctx, []string{check.command})
		if err != nil {
			causes = append(causes, check.item.refusal(err))
			continue
		}
		fingerprints = append(fingerprints, fmt.Sprint(ready))
		record(check.item, integrationFact{Key: "check:" + check.command, Desired: "ready", Observed: fmt.Sprint(ready), AtDesired: ready})
	}
	hooks := runtime.integrationRead(phase, "project hooks", "")
	ready, err := runtime.projectHooksConverged(ctx)
	causes = append(causes, hooks.refusal(err))
	if err == nil {
		fingerprints = append(fingerprints, fmt.Sprint(ready))
		record(hooks, integrationFact{Key: "hooks", Desired: "ready", Observed: fmt.Sprint(ready), AtDesired: ready})
	}
	services := runtime.integrationRead(phase, "services", "")
	serviceFingerprint, servicesChanged, err := runtime.integrationServices(ctx, "observe")
	causes = append(causes, services.refusal(err))
	if err == nil {
		fingerprints = append(fingerprints, serviceFingerprint)
		record(services, integrationFact{Key: "services", Desired: "yard Bubblewrap policy reconciled; role-forbidden utility absent", Observed: serviceFingerprint, AtDesired: !servicesChanged})
	}
	// Passive installed admission may ignore redundant labels. Apply must
	// still finish hooks, commit and verification before capturing settings.
	pending := inventoryErr == nil && observed.Pending
	plan.pendingOnly = pending && !plan.Changed
	plan.Changed = plan.Changed || pending
	if err := collectIntegrationRefusals(causes...); err != nil {
		return plan, observed, err
	}
	scope, _, err := runtime.integrationScopeFromInputs(entries, plan.sources)
	if err != nil {
		return plan, observed, runtime.integrationRead(phase, "scope", "").refusal(err)
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
	bindings := []string{plan.Scope, plan.sources, observed.Fingerprint, runtime.environmentDefault("ALLOWS_CODING_TOOLS", "true"), persist.Fingerprint}
	if len(plan.profileRoutes) != 0 {
		routeBindings, _ := json.Marshal(plan.profileRoutes)
		bindings = append(bindings, string(routeBindings))
	}
	bindings = append(bindings, fingerprints...)
	plan.Fingerprint = fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(bindings, "\x00"))))
	plan.fingerprints = bindings
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
			switch {
			case entry.declaredOwner == "":
				plan.Steps = append(plan.Steps, "retire an owned integration artifact")
			case entry.Kind == "package":
				plan.Steps = append(plan.Steps, "disable managed startup for "+entry.declaredOwner+"; preserve binaries and user data")
			case entry.declaredPath == "":
				plan.Steps = append(plan.Steps, "retire an owned artifact for "+entry.declaredOwner)
			default:
				plan.Steps = append(plan.Steps, "retire an owned artifact for "+entry.declaredOwner+": "+entry.declaredPath)
			}
		}
		if !runtime.PersistFinishOnly {
			for _, adoption := range observed.adoptions {
				plan.Steps = append(plan.Steps, adoption.step())
			}
		}
		if runtime.environmentValue("ALLOWS_CODING_TOOLS") == "false" {
			plan.Steps = append(plan.Steps, "remove ownership-proven ccusage utility for the special yard role")
		}
	}
	return plan, observed, nil
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
		return runtime, IntegrationPlan{}, runtime.integrationRead("inspect", "legacy adoption prerequisites", "").refusal(err)
	}
	if !ready {
		return runtime, IntegrationPlan{}, nil
	}
	candidate := runtime
	candidate.AdoptLegacyIntegrations = true
	absent, err := candidate.guestCheck(ctx, []string{"test", "!", "-e", "/var/lib/subyard/integrations/inventory.json"})
	if err != nil {
		return runtime, IntegrationPlan{}, runtime.integrationRead("inspect", "legacy adoption inventory", "").refusal(err)
	}
	if !absent {
		return runtime, IntegrationPlan{}, nil
	}
	entries, artifactsErr := candidate.integrationArtifactsWithSourceReader("inspect", reader)
	persist, persistErr := candidate.observePersist(ctx, entries, "inspect")
	var observed integrationObservation
	var inventoryErr error
	if artifactsErr == nil {
		candidate.persistProjection = persist.projection()
		observed, inventoryErr = candidate.integrationInventory(ctx, "observe", entries, "")
	}
	structuredEvidence := []string{}
	mode := configmaterial.ModeAssessAdopt
	if artifactsErr != nil || inventoryErr != nil && !observed.Initial {
		mode = configmaterial.ModeObserve
	}
	filesErr := candidate.observeDesiredIntegrationConfigs(ctx, "inspect", mode, reader,
		func(_ integrationWorkItem, file guestConfigFile, payload []byte, observation configmaterial.Observation) error {
			if mode == configmaterial.ModeAssessAdopt && observation.Adoptable {
				observed.adoptions = append(observed.adoptions, integrationAdoption{file.integration, "structured", file.destination})
				structuredEvidence = append(structuredEvidence, file.destination+"\x00"+observation.Fingerprint)
			}
			return nil
		})
	var retiredErr error
	if inventoryErr != nil {
		retiredErr = candidate.observeRetiredIntegrationConfigs(ctx, "inspect", observed.Retired, reader, nil)
	}
	if err := collectIntegrationRefusals(artifactsErr, persistErr, inventoryErr, filesErr, retiredErr); err != nil {
		return runtime, IntegrationPlan{}, err
	}
	plan, legacy := adoptionPlan(observed, structuredEvidence)
	if !observed.Initial || !legacy || plan.AdoptionFingerprint == "" {
		return runtime, IntegrationPlan{}, nil
	}
	candidate.LegacyIntegrationFingerprint = plan.AdoptionFingerprint
	return candidate, plan, nil
}

func (runtime Runtime) observeDesiredIntegrationConfigs(ctx context.Context, phase, mode string, reader func(config.MaterializedAsset) ([]byte, error),
	visit func(integrationWorkItem, guestConfigFile, []byte, configmaterial.Observation) error,
) error {
	files, err := runtime.guestConfigFilesWithSourceReader(reader)
	if err != nil {
		return runtime.integrationRead(phase, "structured configs", "").refusal(err)
	}
	var causes []error
	for _, file := range files {
		if file.ownedFormat == "" {
			continue
		}
		item := runtime.integrationRead(phase, "structured config", file.integration).atDestination(file.destination)
		payload, err := file.readSource()
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			source := runtime.integrationRead(phase, "config source", file.integration).atDestination(file.destination)
			causes = append(causes, source.refusal(err))
			continue
		}
		observation, err := runtime.observeIntegrationConfigFile(ctx, mode, file, payload)
		if err == nil {
			err = visit(item, file, payload, observation)
		}
		causes = append(causes, item.refusal(err))
	}
	return collectIntegrationRefusals(causes...)
}

func (runtime Runtime) observeRetiredIntegrationConfigs(ctx context.Context, phase string, retired []integrationArtifact, reader func(config.MaterializedAsset) ([]byte, error),
	visit func(integrationWorkItem, integrationArtifact, configmaterial.Observation),
) error {
	var causes []error
	for _, entry := range retired {
		if entry.Kind != "structured" {
			continue
		}
		item := runtime.integrationRead(phase, "retired structured config", entry.declaredOwner).forReportedArtifact(entry)
		observation, err := runtime.observeIntegrationConfigAt(ctx, entry.Format, configmaterial.ModeAssessRetire, entry.Path, entry.declaredPath, nil)
		if err != nil {
			causes = append(causes, item.refusal(err))
		} else if visit != nil {
			observation.Released = runtime.declaredRetirementFields(entry, observation.Released, reader)
			visit(item, entry, observation)
		}
	}
	return collectIntegrationRefusals(causes...)
}

func (runtime Runtime) declaredRetirementFields(entry integrationArtifact, pointers []string, reader func(config.MaterializedAsset) ([]byte, error)) []string {
	if len(pointers) == 0 || entry.declaredPath == "" {
		return nil
	}
	definitions, err := runtime.integrationDefinitions()
	if err != nil {
		return nil
	}
	values := runtimeEnvironment(runtime.Environment)
	values["CODING_TOOL_INTEGRATIONS"] = entry.declaredOwner
	delete(values, "INTEGRATION_HOST_LINKS") // The retired document no longer participates in the desired layout.
	assets, err := config.MaterializedAssets(values, runtime.devUser(), definitions...)
	if err != nil {
		return nil
	}
	for _, asset := range assets {
		if asset.Destination == entry.declaredPath && asset.OwnedFormat == entry.Format {
			if reader == nil {
				reader = config.MaterializedAsset.ReadSource
			}
			if payload, err := reader(asset); err == nil {
				return configmaterial.DeclaredPointers(asset.OwnedFormat, payload, pointers)
			}
		}
	}
	return nil
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
	return runtime.observeIntegrationConfigAt(ctx, format, mode, path, path, payload, legacyTemplates...)
}

func (runtime Runtime) observeIntegrationConfigAt(ctx context.Context, format, mode, path, declaredPath string, payload []byte, legacyTemplates ...[]byte) (configmaterial.Observation, error) {
	request, err := configmaterial.Request(format, mode, runtime.devUser(), path, runtime.Yard.DevUID, payload, legacyTemplates...)
	if err != nil {
		return configmaterial.Observation{}, err
	}
	result, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, request)
	if err != nil || result.ExitCode != 0 {
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
		if declaredPath != "" && detail != "" && result.ExitCode == 2 && completedGuestFailure(result, err) {
			retry := shellquote.RecoveryCommand([]string{"incus", "exec", runtime.Yard.YardInstanceName, "--project", runtime.Yard.IncusProject, "--", "sha256sum", "--", declaredPath})
			if unownedInvalid {
				retry = runtime.preserveInvalidConfigCommand(declaredPath)
			}
			return configmaterial.Observation{}, integrationOwnershipFailure(
				fmt.Sprintf("integration ownership conflict in yard %s at %q: %s; file preserved. Compare with the integration's shipped legacy templates before retrying", runtime.Yard.YardName, declaredPath, detail),
				retry, detail == "no protected baseline or exact recognized legacy template" || detail == "owned fields or protected evidence changed")
		}
		return configmaterial.Observation{}, ownershipRefusal("integration structured configuration ownership conflict", result, err)
	}
	return configmaterial.ParseObservation(result.Stdout)
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
	return integrationOwnershipFailure(
		fmt.Sprintf("integration ownership of %q in yard %s was released while its installed configuration still selects it; a release repair would overwrite the released document; run yard -Y %s init instead",
			path, runtime.Yard.YardName, runtime.Yard.YardName),
		shellquote.Command([]string{"yard", "-Y", runtime.Yard.YardName, "init"}), true)
}

// ownershipReleaseNotice previews a retirement that only releases ownership:
// changed owned fields no longer prove Subyard's values, and a missing document
// has nothing to remove. The document itself stays unchanged.
func ownershipReleaseNotice(integration, path string, observation configmaterial.Observation) string {
	prefix := "retirement releases ownership of "
	if integration != "" {
		prefix = "retiring " + integration + " releases ownership of "
	}
	if path == "" {
		path = "a structured configuration"
	}
	return boundedNotice(prefix+path+": ", observation, "")
}

const ownershipNoticeLimit = 384

func boundedNotice(prefix string, observation configmaterial.Observation, suffix string) string {
	reason, fields := "owned fields changed after Subyard applied them", "no owned fields remain"
	if observation.Release == configmaterial.ReleaseMissingDocument {
		reason = "the document is missing"
	}
	switch {
	case observation.ReleasedCount == 1:
		fields = "1 field left in place"
	case observation.ReleasedCount != 0:
		fields = fmt.Sprintf("%d fields left in place", observation.ReleasedCount)
	case observation.Release == configmaterial.ReleaseMissingDocument:
		fields = "nothing to remove"
	}
	if maximum := ownershipNoticeLimit / 2; len(prefix) > maximum {
		prefix = truncateText(prefix, maximum-len("...: ")) + "...: "
	}
	if len(observation.Released) != 0 && len(observation.Released) == observation.ReleasedCount {
		notice := prefix + reason + "; fields left in place: " + strings.Join(observation.Released, ", ") + suffix
		if len(notice) <= ownershipNoticeLimit {
			return notice
		}
	}
	return prefix + reason + "; " + fields + suffix
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
		return runtime.integrationApplyDiagnostic(err, "observe")
	}
	if err := CheckIntegrationPlan(plan, fresh); err != nil {
		return runtime.integrationApplyDiagnostic(runtime.IntegrationPlanDiagnostic(err, plan, fresh, false), "compare plan")
	}
	if err := runtime.checkIntegrationHookScope(); err != nil {
		return runtime.integrationApplyDiagnostic(err, "hooks scope")
	}
	if !fresh.Changed {
		return runtime.storeAppliedIntegrationSettings(ctx)
	}
	execution := runtime
	runtime.applyingIntegrations = true
	if err := runtime.reconcileIntegrationServices(ctx); err != nil {
		return runtime.integrationApplyDiagnostic(err, "integration services")
	}
	entries, err := runtime.integrationArtifactsPhase("apply")
	if err != nil {
		return runtime.integrationApplyDiagnostic(err, "inventory prepare")
	}
	expectedInventory := ""
	if observed.Initial {
		expectedInventory = observed.Fingerprint
	}
	// The approved transfer runs before inventory apply, which then records the
	// exact link that transfer published (the projection bound in the plan).
	runtime.persistProjection = fresh.persist.projection()
	if err := runtime.checkApprovedPersist(fresh.persist); err != nil {
		return err
	}
	if err := runtime.applyPersist(ctx, entries, fresh.persist); err != nil {
		return err
	}
	if _, err := runtime.integrationInventory(ctx, "apply", entries, expectedInventory); err != nil {
		return runtime.integrationApplyDiagnostic(err, "inventory")
	}
	if err := runtime.RefreshConfigs(ctx); err != nil {
		return runtime.integrationApplyDiagnostic(err, "configs")
	}
	if err := execution.reconcileIntegrationScript(ctx); err != nil {
		return err
	}
	// Keep the inventory pending until hooks succeed so the same requested
	// selection remains retryable after a transient hook failure.
	if err := execution.captureProvisionedProjectHooks(ctx); err != nil {
		return runtime.integrationApplyDiagnostic(err, "hooks capture")
	}
	if err := execution.RunProjectHooks(ctx); err != nil {
		return runtime.integrationApplyDiagnostic(err, "hooks run")
	}
	// Retirement effects follow the hooks: a failed init leaves the installed
	// state, which the next successful init retires.
	for _, entry := range observed.Retired {
		if entry.Kind == "structured" {
			item := runtime.integrationItem("apply retire structured config", entry.declaredOwner).forReportedArtifact(entry)
			observation, err := runtime.observeIntegrationConfigAt(ctx, entry.Format, configmaterial.ModeRetire, entry.Path, entry.declaredPath, nil)
			if err != nil {
				return item.refusal(err)
			}
			if observation.Release != "" && runtime.Stdout != nil {
				observation.Released = runtime.declaredRetirementFields(entry, observation.Released, runtime.SourceReader)
				document := entry.declaredPath
				if document == "" {
					document = "a structured configuration"
				}
				fmt.Fprintln(runtime.Stdout, boundedNotice("  [ ok ] yard "+runtime.Yard.YardName+": released ownership of "+document+": ", observation, "; Subyard no longer manages it"))
			}
		}
	}
	if _, err := runtime.integrationInventory(ctx, "commit", entries, ""); err != nil {
		return runtime.integrationApplyDiagnostic(err, "inventory commit")
	}
	verified, _, err := execution.observeIntegrationsPhase(ctx, "verify")
	if err := execution.integrationVerificationRefusals(verified, err, nil); err != nil {
		return err
	}
	return runtime.storeAppliedIntegrationSettings(ctx)
}

func (runtime Runtime) reconcileIntegrationScript(ctx context.Context) error {
	var report bytes.Buffer
	err := runtime.runScript(ctx, &report, "reconcile-integrations.sh", "--activation-diagnostics")
	id, complete := strings.CutSuffix(report.String(), "\n")
	if !complete || !slices.Contains(strings.Fields(runtime.environmentValue("CODING_TOOL_INTEGRATIONS")), id) {
		id = ""
	}
	return runtime.integrationItem("apply script", id).refusal(err)
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
	plan, _, err := capture.runtime.observeIntegrationsPhase(ctx, "verify")
	if err := capture.runtime.integrationVerificationRefusals(plan, err, domain.ErrPlanStale); err != nil {
		return err
	}
	if plan.StateBinding() != capture.plan {
		return domain.ErrPlanStale
	}
	// The inventory fingerprint binds the persist projection it was observed with.
	runtime := capture.runtime
	runtime.SourceReader = capture.sources.read
	runtime.captureSources = capture.sources
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
	runtime.captureSources = sources
	entries, err := runtime.integrationArtifactsWithSourceReader("inspect", sources.read)
	if err != nil {
		return nil, err
	}
	runtime.captureSources = nil
	return &appliedIntegrationSettingsCapture{runtime: runtime, plan: plan.StateBinding(), inventory: observed.Fingerprint, entries: entries, diagnostics: observed.Diagnostics, sources: sources}, nil
}

func (runtime Runtime) storeAppliedIntegrationSettings(ctx context.Context) error {
	plan, observed, err := runtime.observeIntegrationsPhase(ctx, "verify")
	if err := runtime.integrationVerificationRefusals(plan, err, domain.ErrPlanStale); err != nil {
		return err
	}
	sources := newDiagnosticSourceSnapshot(runtime.SourceReader)
	runtime.captureSources = sources
	entries, err := runtime.integrationArtifactsWithSourceReader("verify", sources.read)
	if err != nil {
		return runtime.integrationApplyDiagnostic(err, "inventory capture settings")
	}
	runtime.SourceReader = sources.read
	runtime.persistProjection = plan.persist.projection()
	_, err = runtime.integrationInventory(ctx, "capture-settings", entries, observed.Fingerprint, integrationInventoryCapture{baseline: observed.Diagnostics, verifySources: sources.verify})
	return runtime.integrationApplyDiagnostic(err, "inventory capture settings")
}

type diagnosticSourceSnapshot struct {
	reader func(config.MaterializedAsset) ([]byte, error)
	values map[configSourceKey]diagnosticSourceValue
	items  map[configSourceKey][]integrationWorkItem
}

type diagnosticSourceValue struct {
	payload []byte
	err     error
}

func newDiagnosticSourceSnapshot(reader func(config.MaterializedAsset) ([]byte, error)) *diagnosticSourceSnapshot {
	return &diagnosticSourceSnapshot{reader: reader, values: map[configSourceKey]diagnosticSourceValue{}, items: map[configSourceKey][]integrationWorkItem{}}
}

func (snapshot *diagnosticSourceSnapshot) readerFor(item integrationWorkItem) func(config.MaterializedAsset) ([]byte, error) {
	return func(asset config.MaterializedAsset) ([]byte, error) {
		key := configSourceKey{path: asset.Source, follow: asset.FollowSymlinks}
		if !slices.Contains(snapshot.items[key], item) {
			snapshot.items[key] = append(snapshot.items[key], item)
		}
		return snapshot.read(asset)
	}
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

func (snapshot *diagnosticSourceSnapshot) verify(runtime Runtime) error {
	var findings []error
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
			items := snapshot.items[key]
			if len(items) == 0 {
				items = []integrationWorkItem{runtime.integrationItem("apply inventory capture settings", "")}
			}
			for _, item := range items {
				findings = append(findings, item.refusal(domain.ErrPlanStale))
			}
		}
	}
	return collectIntegrationRefusals(findings...)
}

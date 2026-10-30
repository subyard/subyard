package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/uninstall"
	"strings"
)

type initLegacyContinuation struct {
	adopt              bool
	adoptionConfig     map[string]string
	projectBefore      map[string]string
	grant              domain.LegacyContinuation
	path               string
	before             config.PersistentFileSnapshot
	native             string
	source             string
	published          bool
	hostBefore         config.PersistentFileSnapshot
	registrationPath   string
	registrationBefore config.PersistentFileSnapshot
	legacyHostBefore   config.PersistentFileSnapshot
}

func legacyContinuationPath(yard domain.Context) string {
	return filepath.Join(yard.Paths.ConfigHome, "ownership", "legacy-continuations", yard.YardName+".json")
}

func readLegacyContinuation(yard domain.Context) (config.PersistentFileSnapshot, error) {
	return readInitSelectionSnapshot(yard.Paths.ConfigHome, legacyContinuationPath(yard))
}

func decodeLegacyContinuation(snapshot config.PersistentFileSnapshot) (domain.LegacyContinuation, error) {
	var grant domain.LegacyContinuation
	failure := func(reason string, cause error) (domain.LegacyContinuation, error) {
		return grant, config.OwnerMetadataError("", "legacy continuation record", reason, "", cause)
	}
	if !snapshot.Exists {
		return failure("source is missing (observed metadata unavailable); required existing protected mode0600 file", nil)
	}
	var safetyDifferences []string
	if len(snapshot.Content) > 16384 {
		safetyDifferences = append(safetyDifferences, fmt.Sprintf("record exceeds its size bound; observed bytes=%d, required maximum=16384", len(snapshot.Content)))
	}
	if mode := snapshot.Identity.Mode & 0777; mode != 0600 {
		safetyDifferences = append(safetyDifferences, fmt.Sprintf("record must be a protected mode0600 file; observed mode=%04o, required mode=0600", mode))
	}
	if len(safetyDifferences) > 0 {
		return failure(strings.Join(safetyDifferences, "; "), nil)
	}
	decoder := json.NewDecoder(bytes.NewReader(snapshot.Content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&grant); err != nil {
		return failure("invalid legacy continuation record (JSON format)", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return failure("legacy continuation record has trailing data", err)
	}
	var differences []string
	if grant.Schema != 1 {
		differences = append(differences, fmt.Sprintf("schema: observed=%d, required=1", grant.Schema))
	}
	if grant.Scope != "normal-reconcile" {
		differences = append(differences, "scope: required normal-reconcile")
	}
	if grant.Provenance == "" {
		differences = append(differences, "establishmentProvenance: observed empty, required nonempty")
	}
	if grant.InstanceUUID == "" {
		differences = append(differences, "instanceUuid: observed empty, required nonempty")
	}
	if grant.EstablishmentNativeBinding == "" {
		differences = append(differences, "establishmentNativeBinding: observed empty, required nonempty")
	}
	if len(grant.SecurityPolicyBinding) != 64 {
		differences = append(differences, fmt.Sprintf("securityPolicyBinding: observed bytes=%d, required bytes=64", len(grant.SecurityPolicyBinding)))
	}
	if len(grant.Resources) != 4 {
		differences = append(differences, fmt.Sprintf("resources: observed count=%d, required count=4", len(grant.Resources)))
	}
	if len(differences) > 0 {
		return failure("unsupported legacy continuation record; "+strings.Join(differences, "; "), nil)
	}
	return grant, nil
}

func (cli *CLI) legacyContinuationSource(ctx context.Context, loaded config.Loaded) (string, string, error) {
	options, available, err := cli.mutationGateReleaseOptions()
	if err != nil {
		return "", "", config.OwnerMetadataError("", "legacy continuation installed release", "cannot verify installed release and protected history", "", err)
	}
	if !available {
		return "", "", errors.New("legacy continuation requires a verified installed release and completed protected history")
	}
	installation, err := uninstall.ReadInstallationFile(filepath.Join(options.RuntimeRoot, uninstall.ManifestName), loaded.Context.Paths.DataHome, uint32(os.Getuid()))
	if err != nil {
		if uninstall.InstallationAbsent(err) {
			return "", "", config.OwnerMetadataError(filepath.Join(options.RuntimeRoot, uninstall.ManifestName), "runtime installation inventory", "source is missing (observed metadata unavailable); run yard migrate to assess runtime installation enrollment", "", err)
		}
		return "", "", err
	}
	identity, err := cli.installationIdentity(options.RuntimeRoot, loaded.Context.Paths.ConfigHome, true)
	if err != nil {
		return "", "", err
	}
	if err := installation.CheckRoots(identity.ConfigHome, identity.RuntimeRoot, identity.CacheRoot); err != nil {
		return "", "", config.OwnerMetadataContext(filepath.Join(options.RuntimeRoot, uninstall.ManifestName), "installation inventory", err)
	}
	if err := installation.CheckRuntimeScope(); err != nil {
		return "", "", config.OwnerMetadataContext(filepath.Join(options.RuntimeRoot, uninstall.ManifestName), "runtime installation inventory", err)
	}
	inspector := releaseruntime.NewSourceInspector(cli.options.RepositoryRoot, cli.options.Stderr)
	defer inspector.Close()
	configDir, tooling, err := inspector.InspectSourceTooling(ctx, options.RuntimeRoot, cli.options.RepositoryRoot)
	if err != nil {
		return "", "", config.OwnerMetadataError(options.RuntimeRoot, "legacy continuation installed configuration assets", "cannot verify sealed installed configuration assets", "", err)
	}
	if current := filepath.Clean(loaded.Context.Paths.ConfigDir); current != configDir {
		return "", "", config.OwnerMetadataError(options.RuntimeRoot, "legacy continuation installed configuration assets", "configuration asset roots differ", "configDir: stored="+config.QuoteMetadataPath(configDir)+", current="+config.QuoteMetadataPath(current), nil)
	}
	proof, err := cli.inspectConfigSourceAdmission(ctx, inspector)
	if err != nil {
		return "", "", err
	}
	if proof.Metadata.Journal == nil || proof.Metadata.Journal.Checkpoint != releasetransition.JournalComplete {
		return "", "", errors.New("finish the installed release transition before legacy continuation")
	}
	return operationStateDigest([]string{proof.Binding, tooling}), options.RuntimeRoot, nil
}

func (cli *CLI) prepareLegacyContinuation(ctx context.Context, execution *initExecution, request initArguments) error {
	runtime, ok := execution.platform.(reconcileruntime.Runtime)
	if !ok {
		if request.continueLegacy || request.adopt {
			return errors.New("legacy continuation requires native local assessment")
		}
		return nil
	}
	if request.mode == initConfigs {
		return nil
	}
	snapshot, err := readLegacyContinuation(execution.loaded.Context)
	if err != nil {
		return err
	}
	state, err := runtime.Incus.ReconcileState(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, runtime.SRVPool, runtime.SRVVolume, runtime.Yard.IncusBridge)
	if err != nil {
		if request.continueLegacy || request.adopt || snapshot.Exists {
			return fmt.Errorf("legacy continuation needs read-only native access: %w", err)
		}
		return nil
	}
	if !snapshot.Exists && !request.continueLegacy && !request.adopt && (!state.ProjectFound || state.ProjectConfig["user.subyard.installation"] == "1" || state.ProjectConfig["user.subyard.installation"] == domain.AdoptedInstallation) {
		return nil
	}
	if snapshot.Exists && !request.adopt && state.ProjectConfig["user.subyard.installation"] == domain.AdoptedInstallation {
		return errors.New("adoption metadata is present; rerun yard init --adopt to verify publication and retire the exact continuation record")
	}
	if request.mode != initReconcile || request.profile != "" || execution.bootstrap != nil || runtime.Yard.AccessKind != domain.AccessLocal {
		return errors.New("legacy continuation permits only local plain init; reset, configs and profile expansion are not authorized")
	}
	if !snapshot.Exists && !request.continueLegacy && !request.adopt {
		return errors.New("existing unmarked project has no continuation consent; use yard init --adopt to assess registration and initialization together; alternatively, use yard init --continue-legacy to assess limited normal reconciliation without deletion ownership")
	}
	if execution.hostIDPending {
		return errors.New("legacy continuation requires the existing protected owner HostID")
	}
	path, err := configScalarAuthoringPath(execution.loaded, config.ScopeYard)
	if err != nil {
		return err
	}
	registration, err := config.ReadPersistentFileSnapshot(runtime.Yard.Paths.ConfigHome, path)
	if err != nil {
		return fmt.Errorf("legacy continuation cannot inspect the protected local yard registration: %w", err)
	}
	var legacyHost config.PersistentFileSnapshot
	if !registration.Exists && runtime.Yard.YardName == "default" {
		// Published legacy default init reads host config without creating a yard
		// registration. Bind that genuine protected source and the yard's absence;
		// an unsafe or unreadable explicit record must never select this fallback.
		legacyHost, err = config.ReadPersistentFileSnapshot(runtime.Yard.Paths.ConfigHome, filepath.Join(runtime.Yard.Paths.ConfigHome, "config.env"))
		if err != nil {
			return fmt.Errorf("legacy continuation cannot inspect the protected implicit-default host configuration: %w", err)
		}
		if !legacyHost.Exists {
			return config.OwnerMetadataError(filepath.Join(runtime.Yard.Paths.ConfigHome, "config.env"), "implicit-default host configuration", "source is missing (observed metadata unavailable); required existing protected mode0600 host configuration", "", nil)
		}
		if mode := legacyHost.Identity.Mode & 0777; mode != 0600 {
			return config.OwnerMetadataError(filepath.Join(runtime.Yard.Paths.ConfigHome, "config.env"), "implicit-default host configuration", fmt.Sprintf("requires existing protected mode0600 host configuration; observed mode=%04o, required mode=0600", mode), "", nil)
		}
	} else {
		if !registration.Exists {
			return config.OwnerMetadataError(path, "local yard registration", "source is missing (observed metadata unavailable); required existing protected mode0600 local yard registration", "", nil)
		}
		if mode := registration.Identity.Mode & 0777; mode != 0600 {
			return config.OwnerMetadataError(path, "local yard registration", fmt.Sprintf("requires existing protected mode0600 local yard registration; observed mode=%04o, required mode=0600", mode), "", nil)
		}
	}
	// Reconstruct the protected persisted context without scalar command inputs.
	// Derived context values can legitimately appear in engine child environments;
	// compare their meaning rather than treating matching values as redirects.
	persistedEnv := make(map[string]string, len(cli.baseEnv))
	for name, value := range cli.baseEnv {
		persistedEnv[name] = value
	}
	for name, trace := range execution.loaded.Settings {
		delete(persistedEnv, name)
		for _, alias := range trace.Aliases {
			delete(persistedEnv, alias)
		}
	}
	persisted, err := config.Load(config.LoadOptions{Catalog: &cli.catalog, RepositoryRoot: cli.options.RepositoryRoot, OperatorHome: runtime.Yard.Paths.OperatorHome, YardName: runtime.Yard.YardName, Environment: persistedEnv})
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(persisted.Context, execution.loaded.Context) {
		return errors.New("legacy continuation refuses temporary owner identity overrides; persist the intended configuration first")
	}

	for name, trace := range execution.loaded.Settings {
		if persisted.Settings[name].EffectiveValue != trace.EffectiveValue {
			return fmt.Errorf("legacy continuation refuses temporary %s override; persist the intended configuration first", name)
		}
	}

	source, runtimeRoot, err := cli.legacyContinuationSource(ctx, execution.loaded)
	if err != nil {
		return err
	}
	continuation, err := assessLegacyYard(ctx, runtime, execution.hostID, runtimeRoot, source, snapshot, request.adopt)
	if err != nil {
		return err
	}
	hostBefore, err := config.ReadPersistentFileSnapshot(runtime.Yard.Paths.ConfigHome, configsync.HostIDPath(runtime.Yard.Paths.ConfigHome))
	if err != nil {
		return fmt.Errorf("protected owner HostID inspection during legacy assessment: %w", err)
	}
	if !hostBefore.Exists {
		return config.OwnerMetadataError(configsync.HostIDPath(runtime.Yard.Paths.ConfigHome), "protected owner HostID", "source is missing during legacy assessment", "observed HostID unavailable", nil)
	}
	if hostBefore.Identity.Mode&0777 != 0600 {
		return config.OwnerMetadataError(configsync.HostIDPath(runtime.Yard.Paths.ConfigHome), "protected owner HostID", fmt.Sprintf("unsafe mode during legacy assessment; observed mode=%04o, required mode=0600", hostBefore.Identity.Mode&0777), "", nil)
	}
	if strings.TrimSpace(string(hostBefore.Content)) != execution.hostID {
		return config.OwnerMetadataError(configsync.HostIDPath(runtime.Yard.Paths.ConfigHome), "protected owner HostID", "HostID changed during legacy assessment", "", nil)
	}
	continuation.hostBefore = hostBefore
	continuation.registrationPath = path
	continuation.registrationBefore = registration
	continuation.legacyHostBefore = legacyHost
	execution.legacyContinuation = continuation
	if !request.adopt {
		runtime.LegacyContinuation = &continuation.grant
	}
	execution.platform = runtime
	return nil
}

func assessLegacyContinuation(ctx context.Context, runtime reconcileruntime.Runtime, hostID, runtimeRoot, source string, before config.PersistentFileSnapshot) (*initLegacyContinuation, error) {
	return assessLegacyYard(ctx, runtime, hostID, runtimeRoot, source, before, false)
}

func assessLegacyYard(ctx context.Context, runtime reconcileruntime.Runtime, hostID, runtimeRoot, source string, before config.PersistentFileSnapshot, adopt bool) (*initLegacyContinuation, error) {
	var grant domain.LegacyContinuation
	var err error
	if before.Exists {
		grant, err = decodeLegacyContinuation(before)
		if err != nil {
			return nil, config.OwnerMetadataContext(legacyContinuationPath(runtime.Yard), "legacy continuation record", err)
		}
		runtime.LegacyContinuation = &grant
	}
	var state ports.ReconcileState
	var resources []ports.TeardownResource
	var adoptionConfig, projectBefore map[string]string
	if adopt {
		state, resources, adoptionConfig, projectBefore, err = runtime.InspectYardAdoption(ctx)
	} else {
		state, resources, err = runtime.InspectLegacyContinuation(ctx)
	}
	if err != nil {
		return nil, err
	}
	var adoptionProjects []domain.ProjectRecord
	if adopt {
		adoptionProjects, err = runtime.YardAdoptionProjects(ctx, state.Instance)
		if err != nil {
			return nil, err
		}
	}
	epoch, err := uninstall.ReadOwnerEpoch(runtime.Yard.Paths.DataHome)
	if err != nil {
		return nil, err
	}
	native := operationStateDigest(struct {
		State     ports.ReconcileState
		Resources []ports.TeardownResource
		Projects  []domain.ProjectRecord
	}{state, resources, adoptionProjects})
	binding, err := runtime.LegacyContinuationSecurityBinding(state)
	if err != nil {
		return nil, err
	}
	expected := domain.LegacyContinuation{SecurityPolicyBinding: binding, EstablishmentNativeBinding: native, Schema: 1, Scope: "normal-reconcile", OperatorUID: os.Getuid(), HostID: hostID, OwnerEpoch: epoch, Context: domain.ContinuationContext(runtime.Yard), RuntimeRoot: runtimeRoot, InstanceUUID: state.Instance.Config["volatile.uuid"], Resources: reconcileruntime.LegacyContinuationResources(resources), Provenance: source}
	if before.Exists {
		expected.Provenance = grant.Provenance
		expected.EstablishmentNativeBinding = grant.EstablishmentNativeBinding
		if differences := legacyContinuationDifferences(grant, expected); len(differences) != 0 {
			return nil, config.OwnerMetadataError(legacyContinuationPath(runtime.Yard), "legacy continuation record", "belongs to another owner, epoch, roots or native identity", strings.Join(differences, "; "), domain.ErrPlanStale)
		}
	} else {
		grant = expected
	}
	return &initLegacyContinuation{adopt: adopt, adoptionConfig: adoptionConfig, projectBefore: projectBefore, grant: grant, path: legacyContinuationPath(runtime.Yard), before: before, native: native, source: source}, nil
}

func (continuation *initLegacyContinuation) check(ctx context.Context, cli *CLI, execution *initExecution) error {
	if continuation == nil || continuation.published {
		return nil
	}
	inputs := map[string]config.PersistentFileSnapshot{configsync.HostIDPath(execution.loaded.Context.Paths.ConfigHome): continuation.hostBefore, continuation.registrationPath: continuation.registrationBefore}
	if continuation.legacyHostBefore.Exists {
		inputs[filepath.Join(execution.loaded.Context.Paths.ConfigHome, "config.env")] = continuation.legacyHostBefore
	}
	for path, before := range inputs {
		current, err := config.ReadPersistentFileSnapshot(execution.loaded.Context.Paths.ConfigHome, path)
		if err != nil {
			return fmt.Errorf("%w: inspect protected legacy owner input: %w", domain.ErrPlanStale, err)
		}
		if !reflect.DeepEqual(current, before) {
			return config.OwnerMetadataError(path, "protected legacy owner input", "snapshot changed after assessment", legacySnapshotFacts(before, current), domain.ErrPlanStale)
		}
	}
	current, err := readLegacyContinuation(execution.loaded.Context)
	if err != nil {
		return fmt.Errorf("%w: inspect legacy continuation record: %w", domain.ErrPlanStale, err)
	}
	if !reflect.DeepEqual(current, continuation.before) {
		return config.OwnerMetadataError(continuation.path, "legacy continuation record", "snapshot changed after assessment", legacySnapshotFacts(continuation.before, current), domain.ErrPlanStale)
	}
	source, root, err := cli.legacyContinuationSource(ctx, execution.loaded)
	if err != nil {
		return fmt.Errorf("%w: inspect legacy continuation source: %w", domain.ErrPlanStale, err)
	}
	if source != continuation.source {
		return fmt.Errorf("%w: legacy continuation sealed source binding changed", domain.ErrPlanStale)
	}
	runtime, ok := execution.platform.(reconcileruntime.Runtime)
	if !ok {
		return domain.ErrPlanStale
	}
	actual, err := assessLegacyYard(ctx, runtime, execution.hostID, root, source, current, continuation.adopt)
	if err != nil {
		return fmt.Errorf("%w: inspect legacy continuation assessment: %w", domain.ErrPlanStale, err)
	}
	if actual.native != continuation.native || !reflect.DeepEqual(actual.grant, continuation.grant) || !reflect.DeepEqual(actual.adoptionConfig, continuation.adoptionConfig) || !reflect.DeepEqual(actual.projectBefore, continuation.projectBefore) {
		return fmt.Errorf("%w: legacy continuation assessment changed", domain.ErrPlanStale)
	}
	return nil
}

func (continuation *initLegacyContinuation) publish(execution *initExecution) error {
	if continuation == nil || continuation.before.Exists {
		return nil
	}
	payload, err := json.Marshal(continuation.grant)
	if err != nil {
		return err
	}
	if len(payload)+1 > 16384 {
		return errors.New("legacy continuation record exceeds its size bound")
	}
	if err = config.CompareAndSwapLocalSettingsFile(execution.loaded.Context.Paths.ConfigHome, continuation.path, continuation.before, append(payload, '\n')); err != nil {
		return fmt.Errorf("%w: publish legacy continuation: %w", domain.ErrPlanStale, err)
	}
	continuation.published = true
	return nil
}

func (continuation *initLegacyContinuation) binding() string {
	if continuation == nil {
		return ""
	}
	return operationStateDigest(struct {
		Adopt                          bool
		AdoptionConfig, ProjectBefore  map[string]string
		Grant                          domain.LegacyContinuation
		Before                         config.PersistentFileSnapshot
		Native, Source                 string
		Host, Registration, LegacyHost config.PersistentFileSnapshot
		RegistrationPath               string
	}{continuation.adopt, continuation.adoptionConfig, continuation.projectBefore, continuation.grant, continuation.before, continuation.native, continuation.source, continuation.hostBefore, continuation.registrationBefore, continuation.legacyHostBefore, continuation.registrationPath})
}

// Compare the existing protected grant once, field by field. Only explicitly
// allowlisted nonsecret roots and numeric identity are rendered locally; native
// policy/source bindings and resource payloads retain their opaque boundary.
func legacyContinuationDifferences(stored, current domain.LegacyContinuation) []string {
	before, after := reflect.ValueOf(stored), reflect.ValueOf(current)
	var differences []string
	for index := 0; index < before.NumField(); index++ {
		if reflect.DeepEqual(before.Field(index).Interface(), after.Field(index).Interface()) {
			continue
		}
		field := before.Type().Field(index).Name
		detail := field + ": stored and current metadata differ"
		switch field {
		case "Schema", "OperatorUID", "OwnerEpoch":
			detail = fmt.Sprintf("%s: stored=%v, current=%v", field, before.Field(index).Interface(), after.Field(index).Interface())
		case "RuntimeRoot":
			detail = "runtimeRoot: stored=" + config.QuoteMetadataPath(stored.RuntimeRoot) + ", current=" + config.QuoteMetadataPath(current.RuntimeRoot)
		case "Context":
			detail += "; configHome: stored=" + config.QuoteMetadataPath(stored.Context.Paths.ConfigHome) + ", current=" + config.QuoteMetadataPath(current.Context.Paths.ConfigHome) + "; dataHome: stored=" + config.QuoteMetadataPath(stored.Context.Paths.DataHome) + ", current=" + config.QuoteMetadataPath(current.Context.Paths.DataHome)
		}
		differences = append(differences, detail)
	}
	return differences
}

func legacySnapshotFacts(stored, current config.PersistentFileSnapshot) string {
	return fmt.Sprintf("stored exists=%t, mode=%04o, uid=%d, inode=%d; current exists=%t, mode=%04o, uid=%d, inode=%d; content omitted", stored.Exists, stored.Identity.Mode&0777, stored.Identity.UID, stored.Identity.Inode, current.Exists, current.Identity.Mode&0777, current.Identity.UID, current.Identity.Inode)
}

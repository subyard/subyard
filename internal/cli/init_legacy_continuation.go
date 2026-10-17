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
	if !snapshot.Exists || len(snapshot.Content) > 16384 || snapshot.Identity.Mode&0777 != 0600 {
		return grant, errors.New("legacy continuation record must be a protected mode0600 file")
	}
	decoder := json.NewDecoder(bytes.NewReader(snapshot.Content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&grant); err != nil {
		return grant, errors.New("invalid legacy continuation record")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || grant.Schema != 1 || grant.Scope != "normal-reconcile" || grant.Provenance == "" || grant.InstanceUUID == "" || grant.EstablishmentNativeBinding == "" || len(grant.SecurityPolicyBinding) != 64 || len(grant.Resources) != 4 {
		return grant, errors.New("unsupported legacy continuation record")
	}
	return grant, nil
}

func (cli *CLI) legacyContinuationSource(ctx context.Context, loaded config.Loaded) (string, string, error) {
	options, available, err := cli.mutationGateReleaseOptions()
	if err != nil || !available {
		return "", "", errors.New("legacy continuation requires a verified installed release and completed protected history")
	}
	if runtimeRootForRepository(cli.options.RepositoryRoot, options.RuntimeRoot) != options.RuntimeRoot {
		return "", "", errors.New("legacy continuation must use the installed owner launcher")
	}
	installation, err := uninstall.ReadInstallationFile(filepath.Join(options.RuntimeRoot, uninstall.ManifestName), loaded.Context.Paths.DataHome, uint32(os.Getuid()))
	if err != nil || installation.ConfigHome != loaded.Context.Paths.ConfigHome || installation.RuntimeRoot != options.RuntimeRoot {
		return "", "", errors.New("legacy continuation roots conflict with the installed owner context")
	}
	inspector := releaseruntime.NewSourceInspector(cli.options.RepositoryRoot, cli.options.Stderr)
	defer inspector.Close()
	configDir, tooling, err := inspector.InspectSourceTooling(ctx, options.RuntimeRoot, cli.options.RepositoryRoot)
	if err != nil || filepath.Clean(loaded.Context.Paths.ConfigDir) != configDir {
		return "", "", errors.New("legacy continuation requires sealed installed configuration assets")
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
		if request.continueLegacy {
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
		if request.continueLegacy || snapshot.Exists {
			return fmt.Errorf("legacy continuation needs read-only native access: %w", err)
		}
		return nil
	}
	if !snapshot.Exists && !request.continueLegacy && (!state.ProjectFound || state.ProjectConfig["user.subyard.installation"] == "1") {
		return nil
	}
	if request.mode != initReconcile || request.profile != "" || execution.bootstrap != nil || runtime.Yard.AccessKind != domain.AccessLocal {
		return errors.New("legacy continuation permits only local plain init; reset, configs and profile expansion are not authorized")
	}
	if !snapshot.Exists && !request.continueLegacy {
		return errors.New("existing unmarked project has no continuation consent; use yard init --continue-legacy to assess limited normal reconciliation; this grants no deletion ownership")
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
		if !legacyHost.Exists || legacyHost.Identity.Mode&0777 != 0600 {
			return errors.New("legacy continuation requires existing protected mode0600 host configuration for an implicit default yard")
		}
	} else if !registration.Exists || registration.Identity.Mode&0777 != 0600 {
		return errors.New("legacy continuation requires the existing protected mode0600 local yard registration")
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
	continuation, err := assessLegacyContinuation(ctx, runtime, execution.hostID, runtimeRoot, source, snapshot)
	if err != nil {
		return err
	}
	hostBefore, err := config.ReadPersistentFileSnapshot(runtime.Yard.Paths.ConfigHome, configsync.HostIDPath(runtime.Yard.Paths.ConfigHome))
	if err != nil || !hostBefore.Exists || hostBefore.Identity.Mode&0777 != 0600 || strings.TrimSpace(string(hostBefore.Content)) != execution.hostID {
		return errors.New("protected owner HostID changed during legacy assessment")
	}
	continuation.hostBefore = hostBefore
	continuation.registrationPath = path
	continuation.registrationBefore = registration
	continuation.legacyHostBefore = legacyHost
	execution.legacyContinuation = continuation
	runtime.LegacyContinuation = &continuation.grant
	execution.platform = runtime
	return nil
}

func assessLegacyContinuation(ctx context.Context, runtime reconcileruntime.Runtime, hostID, runtimeRoot, source string, before config.PersistentFileSnapshot) (*initLegacyContinuation, error) {
	var grant domain.LegacyContinuation
	var err error
	if before.Exists {
		grant, err = decodeLegacyContinuation(before)
		if err != nil {
			return nil, err
		}
		runtime.LegacyContinuation = &grant
	}
	state, resources, err := runtime.InspectLegacyContinuation(ctx)
	if err != nil {
		return nil, err
	}
	epoch, err := uninstall.ReadOwnerEpoch(runtime.Yard.Paths.DataHome)
	if err != nil {
		return nil, err
	}
	native := operationStateDigest(struct {
		State     ports.ReconcileState
		Resources []ports.TeardownResource
	}{state, resources})
	binding, err := runtime.LegacyContinuationSecurityBinding(state)
	if err != nil {
		return nil, err
	}
	expected := domain.LegacyContinuation{SecurityPolicyBinding: binding, EstablishmentNativeBinding: native, Schema: 1, Scope: "normal-reconcile", OperatorUID: os.Getuid(), HostID: hostID, OwnerEpoch: epoch, Context: domain.ContinuationContext(runtime.Yard), RuntimeRoot: runtimeRoot, InstanceUUID: state.Instance.Config["volatile.uuid"], Resources: reconcileruntime.LegacyContinuationResources(resources), Provenance: source}
	if before.Exists {
		expected.Provenance = grant.Provenance
		expected.EstablishmentNativeBinding = grant.EstablishmentNativeBinding
		if !reflect.DeepEqual(grant, expected) {
			return nil, fmt.Errorf("%w: legacy continuation belongs to another owner, epoch, roots or native identity", domain.ErrPlanStale)
		}
	} else {
		grant = expected
	}
	return &initLegacyContinuation{grant: grant, path: legacyContinuationPath(runtime.Yard), before: before, native: native, source: source}, nil
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
		if err != nil || !reflect.DeepEqual(current, before) {
			return fmt.Errorf("%w: protected legacy owner input changed", domain.ErrPlanStale)
		}
	}
	current, err := readLegacyContinuation(execution.loaded.Context)
	if err != nil || !reflect.DeepEqual(current, continuation.before) {
		return fmt.Errorf("%w: legacy continuation record changed", domain.ErrPlanStale)
	}
	source, root, err := cli.legacyContinuationSource(ctx, execution.loaded)
	if err != nil || source != continuation.source {
		return fmt.Errorf("%w: legacy continuation source changed", domain.ErrPlanStale)
	}
	runtime, ok := execution.platform.(reconcileruntime.Runtime)
	if !ok {
		return domain.ErrPlanStale
	}
	actual, err := assessLegacyContinuation(ctx, runtime, execution.hostID, root, source, current)
	if err != nil || actual.native != continuation.native || !reflect.DeepEqual(actual.grant, continuation.grant) {
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
		return fmt.Errorf("%w: publish legacy continuation: %v", domain.ErrPlanStale, err)
	}
	continuation.published = true
	return nil
}

func (continuation *initLegacyContinuation) binding() string {
	if continuation == nil {
		return ""
	}
	return operationStateDigest(struct {
		Grant                          domain.LegacyContinuation
		Before                         config.PersistentFileSnapshot
		Native, Source                 string
		Host, Registration, LegacyHost config.PersistentFileSnapshot
		RegistrationPath               string
	}{continuation.grant, continuation.before, continuation.native, continuation.source, continuation.hostBefore, continuation.registrationBefore, continuation.legacyHostBefore, continuation.registrationPath})
}

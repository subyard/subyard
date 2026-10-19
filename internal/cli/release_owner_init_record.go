package cli

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releaseowner/nativejson"
)

type ownerSettingRow struct {
	Name  string
	Trace config.SettingTrace
}
type ownerIntegrationReason struct {
	Name    string
	Reasons []string
}
type ownerSelectionRecord struct {
	Requested, Effective                   []string
	Present, AllowsCodingTools, Suppressed bool
	Provenance                             config.SettingResolution
	Reasons                                []ownerIntegrationReason
	ReasonsPresent                         bool
}

func describeOwnerSelection(v config.IntegrationSelection) ownerSelectionRecord {
	r := ownerSelectionRecord{Requested: v.Requested, Effective: v.Effective, Present: v.Present, AllowsCodingTools: v.AllowsCodingTools, Suppressed: v.Suppressed, Provenance: v.Provenance, ReasonsPresent: v.DependencyReasons != nil}
	for _, name := range slices.Sorted(maps.Keys(v.DependencyReasons)) {
		r.Reasons = append(r.Reasons, ownerIntegrationReason{name, v.DependencyReasons[name]})
	}
	return r
}
func (r ownerSelectionRecord) restore() config.IntegrationSelection {
	v := config.IntegrationSelection{Requested: r.Requested, Effective: r.Effective, Present: r.Present, AllowsCodingTools: r.AllowsCodingTools, Suppressed: r.Suppressed, Provenance: r.Provenance}
	if r.ReasonsPresent || len(r.Reasons) != 0 {
		v.DependencyReasons = map[string][]string{}
	}
	for _, row := range r.Reasons {
		v.DependencyReasons[row.Name] = row.Reasons
	}
	return v
}
func describeOwnerSettings(values map[string]config.SettingTrace) []ownerSettingRow {
	var rows []ownerSettingRow
	for _, key := range slices.Sorted(maps.Keys(values)) {
		rows = append(rows, ownerSettingRow{key, values[key]})
	}
	return rows
}
func restoreOwnerSettings(rows []ownerSettingRow) map[string]config.SettingTrace {
	if rows == nil {
		return nil
	}
	r := map[string]config.SettingTrace{}
	for _, row := range rows {
		r[row.Name] = row.Trace
	}
	return r
}

type ownerInitSource struct {
	Path   string
	Source initIntegrationSource
}
type ownerInitBaseline struct {
	Selection ownerSelectionRecord
	Settings  []ownerSettingRow
	Sources   []ownerInitSource
	Packages  []ownerStringPair
}
type ownerInitSelection struct {
	Write    []byte
	Path     string
	Before   config.PersistentFileSnapshot
	Original ownerSelectionRecord
}
type ownerInitContext domain.Context

// Catalog definitions remain in the verified repository. These are the exact
// selected inputs, including the legacy view that may later live in an archive.
type ownerLoadedSnapshot struct {
	Context            ownerInitContext
	Selection          ownerSelectionRecord
	Profiles           config.EnvironmentProfileSelection
	Environment        []ownerStringPair
	EnvironmentPresent bool
	Settings           []ownerSettingRow
	SettingsPresent    bool
	Layers             []config.ConfigurationLayer
}

func captureOwnerLoaded(loaded config.Loaded) ownerLoadedSnapshot {
	r := ownerLoadedSnapshot{Context: ownerInitContext(loaded.Context), Selection: describeOwnerSelection(loaded.Integrations), Profiles: loaded.EnvironmentProfiles, EnvironmentPresent: loaded.Environment != nil, Settings: describeOwnerSettings(loaded.Settings), SettingsPresent: loaded.Settings != nil, Layers: loaded.ConfigurationLayers}
	for _, key := range slices.Sorted(maps.Keys(loaded.Environment)) {
		r.Environment = append(r.Environment, ownerStringPair{[]byte(key), []byte(loaded.Environment[key])})
	}
	return r
}

func ownerRepositoryValue(value, from, to string) string {
	if from == "" || to == "" || from == to {
		return value
	}
	var out strings.Builder
	for {
		i := strings.Index(value, from)
		if i < 0 {
			out.WriteString(value)
			return out.String()
		}
		end := i + len(from)
		out.WriteString(value[:i])
		if (i == 0 || strings.ContainsRune("=:\"'\n\r\t [,(", rune(value[i-1]))) && (end == len(value) || strings.ContainsRune("/\"'\n\r\t ", rune(value[end]))) {
			out.WriteString(to)
		} else {
			out.WriteString(from)
		}
		value = value[end:]
	}
}

func ownerLoadedLabels(loaded config.Loaded, from, to string) config.Loaded {
	label := func(value string) string {
		if strings.HasPrefix(from, "/proc/self/fd/") {
			value = ownerRepositoryValue(value, strings.Replace(from, "/proc/self/", "/proc/"+strconv.Itoa(os.Getpid())+"/", 1), to)
		}
		return ownerRepositoryValue(value, from, to)
	}
	loaded.Context.Paths.RepositoryRoot = label(loaded.Context.Paths.RepositoryRoot)
	loaded.Context.Paths.ConfigDir = label(loaded.Context.Paths.ConfigDir)
	loaded.Environment = maps.Clone(loaded.Environment)
	for key, value := range loaded.Environment {
		loaded.Environment[key] = label(value)
	}
	loaded.Settings = ownerSettingsLabels(loaded.Settings, label)
	loaded.Integrations.Provenance.Path = label(loaded.Integrations.Provenance.Path)
	loaded.EnvironmentProfiles.Provenance.Path = label(loaded.EnvironmentProfiles.Provenance.Path)
	loaded.ConfigurationLayers = slices.Clone(loaded.ConfigurationLayers)
	for i := range loaded.ConfigurationLayers {
		loaded.ConfigurationLayers[i].Path = label(loaded.ConfigurationLayers[i].Path)
	}
	return loaded
}

func ownerSettingsLabels(settings map[string]config.SettingTrace, label func(string) string) map[string]config.SettingTrace {
	result := maps.Clone(settings)
	for key, trace := range result {
		if trace.Kind == config.SettingFile || trace.Type == config.SettingRegularFilePath {
			trace.EffectiveValue = label(trace.EffectiveValue)
		}
		trace.Resolutions = slices.Clone(trace.Resolutions)
		for i := range trace.Resolutions {
			trace.Resolutions[i].Path = label(trace.Resolutions[i].Path)
		}
		result[key] = trace
	}
	return result
}

func (r ownerLoadedSnapshot) restore(catalog config.Catalog) (config.Loaded, error) {
	if !r.EnvironmentPresent && len(r.Environment) != 0 {
		return config.Loaded{}, domain.ErrPlanStale
	}
	loaded := config.Loaded{Catalog: catalog, Context: domain.Context(r.Context), Integrations: r.Selection.restore(), EnvironmentProfiles: r.Profiles, ConfigurationLayers: r.Layers}
	if r.EnvironmentPresent {
		loaded.Environment = map[string]string{}
	}
	for _, row := range r.Environment {
		loaded.Environment[string(row.Name)] = string(row.Value)
	}
	if r.SettingsPresent {
		loaded.Settings = restoreOwnerSettings(r.Settings)
		if loaded.Settings == nil {
			loaded.Settings = map[string]config.SettingTrace{}
		}
	}
	before, err := nativejson.EncodeLosslessNativeJSON(r, 32<<20)
	if err != nil {
		return config.Loaded{}, err
	}
	after, err := nativejson.EncodeLosslessNativeJSON(captureOwnerLoaded(loaded), 32<<20)
	if err != nil || !bytes.Equal(before, after) {
		return config.Loaded{}, domain.ErrPlanStale
	}
	return loaded, nil
}

type ownerInitRecord struct {
	Version                                                        int
	Loaded                                                         string
	Input                                                          ownerLoadedSnapshot
	Plan, Approved, Finalize                                       application.ReconcilePlan
	NativePlans                                                    []byte
	Baseline                                                       *ownerInitBaseline
	Selection                                                      *ownerInitSelection
	PowerYards                                                     []ownerInitContext
	HookProjects                                                   []domain.ProjectRecord
	HostID                                                         string
	HostIDPending, ConfigsChanged, HooksApplicable, OrphanDeferred bool
}

func ownerLoadedBinding(loaded config.Loaded, runtime reconcileruntime.Runtime) (string, error) {
	loaded = ownerLoadedLabels(loaded, runtime.RepositoryRoot, runtime.LogicalRepositoryRoot)
	settings := describeOwnerSettings(loaded.Settings)
	layers := loaded.ConfigurationLayers
	var environment []ownerStringPair
	for _, name := range slices.Sorted(maps.Keys(loaded.Environment)) {
		environment = append(environment, ownerStringPair{[]byte(name), []byte(loaded.Environment[name])})
	}
	raw, err := nativejson.EncodeLosslessNativeJSON(struct {
		Context           ownerInitContext
		Selection         ownerSelectionRecord
		Profiles          config.EnvironmentProfileSelection
		Environment       string
		LoadedEnvironment []ownerStringPair
		Settings          []ownerSettingRow
		Layers            []config.ConfigurationLayer
	}{ownerInitContext(loaded.Context), describeOwnerSelection(loaded.Integrations), loaded.EnvironmentProfiles, runtime.OwnerEnvironmentBinding(), environment, settings, layers}, 32<<20)
	if err != nil {
		return "", err
	}
	return nativeDigest(raw), nil
}

func captureOwnerInitRecord(execution *initExecution) ([]byte, error) {
	if execution == nil || execution.mode != initReconcile || execution.bootstrap != nil || execution.resetBaseline != nil || execution.profileSetup != nil || execution.profileProvision != nil || execution.requestedProfile != "" || execution.provisionProfile != "" || execution.resourceCommand != "" || len(execution.resourceArguments) != 0 {
		return nil, errors.New("owner migration requires its original plain init handle")
	}
	// These scopes require their own original resource/continuation capsules;
	// never erase them or replace them with a freshly selected callback.
	if execution.orphanIngress != nil || execution.legacyContinuation != nil {
		return nil, errors.New("owner init resource or continuation capsule unavailable")
	}
	runtime, ok := execution.platform.(reconcileruntime.Runtime)
	if !ok {
		return nil, domain.ErrPlanStale
	}
	r := ownerInitRecord{Version: 1, Input: captureOwnerLoaded(ownerLoadedLabels(execution.loaded, runtime.RepositoryRoot, runtime.LogicalRepositoryRoot)), Plan: execution.plan, Approved: execution.approvedPlan, Finalize: execution.approvedFinalize, HookProjects: execution.hookProjects, HostID: execution.hostID, HostIDPending: execution.hostIDPending, ConfigsChanged: execution.configsChanged, HooksApplicable: execution.hooksApplicable, OrphanDeferred: execution.orphanIngressDeferred}
	if execution.powerYards != nil {
		r.PowerYards = []ownerInitContext{}
		for _, yard := range execution.powerYards {
			yard.Paths.RepositoryRoot = ownerRepositoryValue(yard.Paths.RepositoryRoot, runtime.RepositoryRoot, runtime.LogicalRepositoryRoot)
			yard.Paths.ConfigDir = ownerRepositoryValue(yard.Paths.ConfigDir, runtime.RepositoryRoot, runtime.LogicalRepositoryRoot)
			r.PowerYards = append(r.PowerYards, ownerInitContext(yard))
		}
	}
	var err error
	r.Loaded, err = ownerLoadedBinding(execution.loaded, runtime)
	if err != nil {
		return nil, err
	}
	r.NativePlans, err = runtime.CaptureOwnerNativePlans(execution.integrationAdoption)
	if err != nil {
		return nil, err
	}
	if b := execution.integrationBaseline; b != nil {
		label := func(value string) string {
			return ownerRepositoryValue(value, runtime.RepositoryRoot, runtime.LogicalRepositoryRoot)
		}
		selection := b.Selection
		selection.Provenance.Path = label(selection.Provenance.Path)
		d := &ownerInitBaseline{Selection: describeOwnerSelection(selection), Settings: describeOwnerSettings(ownerSettingsLabels(b.Settings, label))}
		for _, path := range slices.Sorted(maps.Keys(b.Sources)) {
			d.Sources = append(d.Sources, ownerInitSource{label(path), b.Sources[path]})
		}
		for _, name := range slices.Sorted(maps.Keys(b.Packages)) {
			d.Packages = append(d.Packages, ownerStringPair{[]byte(name), []byte(b.Packages[name])})
		}
		r.Baseline = d
	}
	if s := execution.integrationSelection; s != nil {
		body, err := config.RecordOwnerIntegrationWrite(s.write)
		if err != nil {
			return nil, err
		}
		r.Selection = &ownerInitSelection{body, s.path, s.before, describeOwnerSelection(s.original)}
	}
	return nativejson.EncodeLosslessNativeJSON(r, 32<<20)
}

// Restore uses current observations only to validate the original loaded input
// shape. Plans, conditional slots and CAS baselines come solely from raw.
func restoreOwnerInitRecord(raw []byte, loaded config.Loaded, runtime reconcileruntime.Runtime) (*initExecution, error) {
	var r ownerInitRecord
	if err := nativejson.DecodeLosslessNativeJSON(raw, &r, 32<<20); err != nil {
		return nil, err
	}
	binding, err := ownerLoadedBinding(loaded, runtime)
	if err != nil {
		return nil, err
	}
	if r.Version != 1 || binding != r.Loaded {
		return nil, domain.ErrPlanStale
	}
	runtime, adoption, err := runtime.RestoreOwnerNativePlans(r.NativePlans)
	if err != nil {
		return nil, err
	}
	e := &initExecution{loaded: loaded, mode: initReconcile, plan: r.Plan, approvedPlan: r.Approved, approvedFinalize: r.Finalize, hookProjects: r.HookProjects, hostID: r.HostID, hostIDPending: r.HostIDPending, configsChanged: r.ConfigsChanged, hooksApplicable: r.HooksApplicable, orphanIngressDeferred: r.OrphanDeferred, platform: runtime, runtimePlan: runtime.RuntimePlan, hookPlan: runtime.HookPlan, integrationAdoption: adoption}
	if r.PowerYards != nil {
		e.powerYards = []domain.Context{}
		for _, yard := range r.PowerYards {
			value := domain.Context(yard)
			value.Paths.RepositoryRoot = ownerRepositoryValue(value.Paths.RepositoryRoot, runtime.LogicalRepositoryRoot, runtime.RepositoryRoot)
			value.Paths.ConfigDir = ownerRepositoryValue(value.Paths.ConfigDir, runtime.LogicalRepositoryRoot, runtime.RepositoryRoot)
			e.powerYards = append(e.powerYards, value)
		}
	}
	if b := r.Baseline; b != nil {
		physical := func(value string) string {
			return ownerRepositoryValue(value, runtime.LogicalRepositoryRoot, runtime.RepositoryRoot)
		}
		selection := b.Selection.restore()
		selection.Provenance.Path = physical(selection.Provenance.Path)
		e.integrationBaseline = &initIntegrationBaseline{Selection: selection, Settings: ownerSettingsLabels(restoreOwnerSettings(b.Settings), physical), Sources: map[string]initIntegrationSource{}, Packages: map[string]string{}}
		for _, source := range b.Sources {
			e.integrationBaseline.Sources[physical(source.Path)] = source.Source
		}
		for _, row := range b.Packages {
			e.integrationBaseline.Packages[string(row.Name)] = string(row.Value)
		}
	}
	if s := r.Selection; s != nil {
		write, err := config.RestoreOwnerIntegrationWrite(s.Write, loaded.Context.Paths.ConfigDir, loaded.Context.Paths.ConfigHome, loaded.Context.YardName)
		if err != nil {
			return nil, err
		}
		e.integrationSelection = &initIntegrationSelection{write: write, path: s.Path, before: s.Before, original: s.Original.restore()}
	}
	// A canonical re-capture checks sorted rows, duplicate map keys and every
	// original container without selecting any new native work.
	canonical, err := captureOwnerInitRecord(e)
	if err != nil || !bytes.Equal(canonical, raw) {
		return nil, domain.ErrPlanStale
	}
	return e, nil
}

func (c *releaseOwnerComponents) prepareOwnerInit(ctx context.Context, operation *CLI, loaded config.Loaded, powerYards []domain.Context) (*initExecution, *reconcileruntime.PreparedInstalledRuntime, error) {
	return c.prepareOwnerInitView(ctx, operation, loaded, powerYards, nil)
}
func (c *releaseOwnerComponents) prepareOwnerInitView(ctx context.Context, operation *CLI, loaded config.Loaded, powerYards []domain.Context, planning *reconcileruntime.Runtime) (*initExecution, *reconcileruntime.PreparedInstalledRuntime, error) {
	// The fixed child has no --continue-legacy grant. Do not enter the public
	// release source inspector through a saved general-purpose continuation.
	continuation, err := readLegacyContinuation(loaded.Context)
	if err != nil {
		return nil, nil, err
	}
	if continuation.Exists {
		return nil, nil, errors.New("saved legacy continuation needs its original source-admission component facts")
	}
	preparer := *operation
	preparer.options.InitPlatform = c.platform(loaded, powerYards)
	if planning != nil {
		preparer.options.InitPlatform = *planning
		preparer.options.Incus = planning.Incus
	}
	execution, err := preparer.prepareInitExecution(ctx, loaded, []string{"--yes"}, nil)
	if err != nil {
		return nil, nil, err
	}
	execution.powerYards = slices.Clone(powerYards)
	if operation.options.InitPlatform == nil {
		execution.orphanIngress, err = preparer.prepareOrphanIngress(ctx, loaded)
		if errors.Is(err, errOrphanIngressAccessDeferred) {
			execution.orphanIngressDeferred = true
		} else if err != nil {
			return nil, nil, err
		}
	}
	runtime, ok := execution.platform.(reconcileruntime.Runtime)
	if !ok {
		return nil, nil, domain.ErrPlanStale
	}
	execution.platform = runtime
	sources, err := runtime.PrepareOwnerRuntimeSources(ctx)
	if err != nil {
		return nil, nil, err
	}
	return execution, sources, nil
}

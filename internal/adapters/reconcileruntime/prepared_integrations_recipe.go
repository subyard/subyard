package reconcileruntime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
)

type materializedIntegrationPlan struct {
	Plan           IntegrationPlan
	Scope, Sources string
	Facts          []integrationFact
	PackageSources map[string]string
	Instance       ports.InstanceInfo
	InstanceScope  string
	Fingerprints   []string
	ProfileRoutes  []integrationProfileRoute
}

func describeIntegrationPlan(p IntegrationPlan) materializedIntegrationPlan {
	return materializedIntegrationPlan{p, p.Scope, p.sources, p.facts, p.packageSources, p.instance, p.instanceScope, p.fingerprints, p.profileRoutes}
}

func (d materializedIntegrationPlan) restore() IntegrationPlan {
	p := d.Plan
	p.Scope, p.sources, p.facts, p.packageSources = d.Scope, d.Sources, d.Facts, d.PackageSources
	p.instance, p.instanceScope, p.fingerprints, p.profileRoutes = d.Instance, d.InstanceScope, d.Fingerprints, d.ProfileRoutes
	return p
}

type materializedHookPlan struct {
	Approved, Current, Provisioned               hookObservation
	Conditional, Unavailable, Completed, Stopped bool
	AllowedHooks                                 []string
	Source, ResolvedProjects, ExecutionWiring    string
	OwnerProjects                                []domain.ProjectRecord
	HookBindings                                 map[string]string
	IntegrationScope                             string
	Metadata                                     *projectMetadataPlan
	MetadataCompleted                            bool
}

func describeHookPlan(p *ProjectHookPlan) *materializedHookPlan {
	if p == nil {
		return nil
	}
	return &materializedHookPlan{p.approved, p.current, p.provisionedHooks, p.conditional, p.unavailable, p.completed, p.stopped, p.allowedHooks, p.source, p.resolvedProjects, p.executionWiring, p.ownerProjects, p.hookBindings, p.integrationScope, p.metadata, p.metadataCompleted}
}

func (d *materializedHookPlan) restore() *ProjectHookPlan {
	if d == nil {
		return nil
	}
	return &ProjectHookPlan{approved: d.Approved, current: d.Current, provisionedHooks: d.Provisioned, conditional: d.Conditional, unavailable: d.Unavailable, completed: d.Completed, stopped: d.Stopped, allowedHooks: d.AllowedHooks, source: d.Source, resolvedProjects: d.ResolvedProjects, executionWiring: d.ExecutionWiring, ownerProjects: d.OwnerProjects, hookBindings: d.HookBindings, integrationScope: d.IntegrationScope, metadata: d.Metadata, metadataCompleted: d.MetadataCompleted}
}

type materializedExecutionSource struct {
	Layout, Payload, Identity string
	Source                    []byte
	Projected                 bool
}

type materializedEnvironmentSource struct {
	Name, Payload, Identity string
	Source                  []byte
	Projected               bool
}

type materializedIntegrations struct {
	Binding, Runtime, Skipped string
	Plan                      materializedIntegrationPlan
	Entries                   []materializedArtifact
	Hooks                     *materializedHookPlan
	Sources                   []materializedExecutionSource
	Environment               []materializedEnvironmentSource
}

func (d *materializedIntegrations) sourceBinding(configs materializedConfigs) string {
	sources := slices.Clone(d.Sources)
	for index := range sources {
		if sources[index].Projected {
			sources[index].Identity = "projected"
		}
	}
	environment := slices.Clone(d.Environment)
	for index := range environment {
		if environment[index].Projected {
			environment[index].Identity = "projected"
		}
	}
	// Native baselines/progress and HookPlan observations remain in the full
	// original recipe, independently of immutable source/desired identity.
	return installedRuntimeDigest(struct {
		Configs     materializedConfigs
		Sources     []materializedExecutionSource
		Environment []materializedEnvironmentSource
		Scope       string
	}{configs.semantic(), sources, environment, d.Plan.Scope})
}

func (runtime Runtime) checkMaterializedIntegrationDescription(original, current *materializedIntegrations, files map[string][]byte) error {
	if len(original.Sources) != len(current.Sources) || len(original.Environment) != len(current.Environment) {
		return domain.ErrPlanStale
	}
	for index, guard := range original.Sources {
		value := &current.Sources[index]
		if !bytes.Equal(guard.Source, value.Source) || guard.Projected != value.Projected {
			return domain.ErrPlanStale
		}
		if err := runtime.checkProjectedMaterializedGuard(guard.Source, guard.Identity, value.Identity, guard.Projected, files[guard.Payload]); err != nil {
			return err
		}
		value.Identity = guard.Identity
	}
	for index, guard := range original.Environment {
		value := &current.Environment[index]
		if !bytes.Equal(guard.Source, value.Source) || guard.Projected != value.Projected {
			return domain.ErrPlanStale
		}
		if err := runtime.checkProjectedMaterializedGuard(guard.Source, guard.Identity, value.Identity, guard.Projected, files[guard.Payload]); err != nil {
			return err
		}
		value.Identity = guard.Identity
	}
	if installedRuntimeDigest(current.Sources) != installedRuntimeDigest(original.Sources) || installedRuntimeDigest(current.Environment) != installedRuntimeDigest(original.Environment) {
		return domain.ErrPlanStale
	}
	return nil
}

func (runtime Runtime) materializedIntegrationPaths() ([]string, error) {
	_, paths, err := runtime.hostInstallerSourcePaths("materialized-integrations")
	if err != nil {
		return nil, err
	}
	definitions, err := runtime.integrationDefinitions()
	if err != nil {
		return nil, err
	}
	addTree := func(root string) error {
		return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if entry.Name() == "tests" || entry.Name() == ".git" || entry.Name() == "__pycache__" || entry.Name() == "private" || entry.Name() == ".build" {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(entry.Name(), ".pyc") {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return errors.New("integration source must be regular")
			}
			relative, err := filepath.Rel(runtime.bindingPath(runtime.RepositoryRoot), runtime.bindingPath(path))
			if err != nil || !validMaterializedLayout(relative) {
				return errors.New("integration source root escapes selected repository")
			}
			paths = append(paths, relative)
			if len(paths) > 8192 {
				return errors.New("integration source file bound")
			}
			return nil
		})
	}
	for _, definition := range definitions {
		if err := addTree(definition.Root); err != nil {
			return nil, err
		}
		for _, native := range definition.Native {
			path, err := definition.NativeExecutablePath(native.Path)
			if err != nil {
				return nil, err
			}
			relative, err := filepath.Rel(runtime.bindingPath(runtime.RepositoryRoot), runtime.bindingPath(path))
			if err != nil || !validMaterializedLayout(relative) {
				return nil, errors.New("integration native source scope")
			}
			paths = append(paths, relative)
			if strings.HasPrefix(relative, ".build/") {
				paths = append(paths, "go.mod")
			}
		}
	}
	// Selected profile hooks may be independently verified by HookPlan. Retain
	// those exact registered observation/native sources as well, without giving
	// the integration recipe authority to apply a profile.
	profiles, err := runtime.profileRuntimeDefinitions()
	if err != nil {
		return nil, err
	}
	for _, definition := range profiles {
		needed := slices.Contains(strings.Fields(runtime.environmentValue("ENVIRONMENT_PROFILES")), definition.Name)
		if runtime.HookPlan != nil {
			for _, hook := range definition.Runtime.ProjectsChangedHooks {
				needed = needed || slices.Contains(runtime.HookPlan.approved.Hooks, hook)
			}
		}
		if !needed {
			continue
		}
		_, selected, _, err := runtime.profileSourcePaths(definition.Runtime.ActivationID)
		if err != nil {
			return nil, err
		}
		paths = append(paths, selected...)
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	if len(paths) > 8192 || !slices.Contains(paths, "scripts/reconcile-integrations.sh") || !slices.Contains(paths, "config/projects-changed.sh") {
		return nil, errors.New("integration execution source scope")
	}
	return paths, nil
}

func validMaterializedLayout(path string) bool {
	return path != "." && path != ".." && !filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.HasPrefix(path, "../") && len(path) <= 512
}

func (runtime Runtime) captureMaterializedIntegrationSources(p *PreparedInstalledRuntime, d *materializedIntegrations) error {
	paths, err := runtime.materializedIntegrationPaths()
	if err != nil {
		return err
	}
	for _, path := range paths {
		source := filepath.Join(runtime.RepositoryRoot, path)
		payload, err := (guestConfigFile{readAsset: runtime.SourceReader}).readSourceAsset(config.MaterializedAsset{Source: source})
		if err != nil {
			return err
		}
		if len(payload) > 32<<20 {
			return errors.New("integration source leaf bound")
		}
		identity, err := materializedSourceIdentity(source)
		if err != nil {
			return err
		}
		key := materializedBlobPath(payload)
		p.files[key] = payload
		projected, err := runtime.materializedProjectedSource(source)
		if err != nil {
			return err
		}
		d.Sources = append(d.Sources, materializedExecutionSource{path, key, identity, []byte(runtime.bindingPath(source)), projected})
	}
	// Only the existing owning provision/cleanup source settings are remapped.
	// Resource paths and arbitrary environment values are never rewritten.
	var names []string
	for _, entry := range runtime.Environment {
		name, value, ok := strings.Cut(entry, "=")
		if ok && value != "" && strings.HasPrefix(name, "AGENT_") && (strings.HasSuffix(name, "_PROVISION") || strings.HasSuffix(name, "_CLEANUP")) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	for _, name := range names {
		source := runtime.environmentValue(name)
		payload, err := (guestConfigFile{readAsset: runtime.SourceReader}).readSourceAsset(config.MaterializedAsset{Source: source})
		if err != nil {
			return err
		}
		if len(payload) > 32<<20 {
			return errors.New("integration provision leaf bound")
		}
		identity, err := materializedSourceIdentity(source)
		if err != nil {
			return err
		}
		key := materializedBlobPath(payload)
		p.files[key] = payload
		projected, err := runtime.materializedProjectedSource(source)
		if err != nil {
			return err
		}
		d.Environment = append(d.Environment, materializedEnvironmentSource{name, key, identity, []byte(runtime.bindingPath(source)), projected})
	}
	return nil
}

// PrepareMaterializedIntegrations adds exact executable source bytes and all
// original native/config/hook baselines to the existing prepared component.
func (runtime Runtime) PrepareMaterializedIntegrations(ctx context.Context, prepared *PreparedIntegrations) (*PreparedInstalledRuntime, error) {
	if prepared == nil || prepared.runtime != runtime.configRuntimeBinding() {
		return nil, domain.ErrPlanStale
	}
	if prepared.hooks == nil {
		var err error
		prepared.hooks, err = runtime.PrepareProjectHooks(ctx, false)
		if err != nil {
			return nil, err
		}
	}
	runtime.HookPlan = prepared.hooks
	p, err := runtime.PrepareMaterializedConfigs(ctx, prepared.configs)
	if err != nil {
		return nil, err
	}
	p.owner = "materialized-integrations"
	d := &materializedIntegrations{Binding: prepared.binding, Runtime: prepared.runtime, Skipped: prepared.skipped, Plan: describeIntegrationPlan(prepared.plan), Entries: describeMaterializedArtifacts(prepared.entries), Hooks: describeHookPlan(prepared.hooks)}
	if err := runtime.captureMaterializedIntegrationSources(p, d); err != nil {
		return nil, err
	}
	for index, e := range d.Entries {
		if e.Content == "" {
			continue
		}
		payload, err := base64.StdEncoding.Strict().DecodeString(e.Content)
		if err != nil {
			return nil, err
		}
		key := materializedBlobPath(payload)
		p.files[key] = payload
		d.Entries[index].Content = key
	}
	description, err := runtime.materializedConfigDescription(prepared.configs)
	if err != nil {
		return nil, err
	}
	for path := range p.files {
		if strings.HasPrefix(path, materializedMetadataPrefix) {
			delete(p.files, path)
		}
	}
	if err := writeMaterializedMetadata(p, materializedRecipe{Version: 1, Configs: description, Integrations: d}); err != nil {
		return nil, err
	}
	p.source = d.sourceBinding(description)
	p.integrations = prepared
	p.executionFiles = map[string][]byte{}
	for _, source := range d.Sources {
		p.executionFiles[source.Layout] = p.files[source.Payload]
	}
	for _, source := range d.Environment {
		p.executionFiles[source.Payload] = p.files[source.Payload]
	}
	if _, _, err := p.stageUsage(); err != nil {
		return nil, err
	}
	if err := checkMaterializedRecipeSize(p); err != nil {
		return nil, err
	}
	p.binding = installedRuntimeDigest(json.RawMessage(p.Recipe()))
	return p, nil
}

func decodeMaterializedMetadata(p *PreparedInstalledRuntime) (materializedRecipe, map[string]bool, error) {
	var d materializedRecipe
	raw, allowed, err := readMaterializedMetadata(p)
	if err != nil {
		return d, nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&d); err != nil {
		return d, nil, err
	}
	canonical, _ := json.Marshal(d)
	if d.Version != 1 || !bytes.Equal(raw, canonical) {
		return d, nil, errors.New("invalid materialized metadata")
	}
	return d, allowed, nil
}

func (runtime Runtime) restoreMaterializedIntegrations(ctx context.Context, p *PreparedInstalledRuntime) error {
	runtime.legacyPreviewInstructions = !p.configs.previewSkillCutover
	original, allowed, err := decodeMaterializedMetadata(p)
	if err != nil {
		return err
	}
	d := original.Integrations
	if d == nil || d.Hooks == nil || d.Runtime != runtime.configRuntimeBinding() || d.sourceBinding(original.Configs) != p.source {
		return domain.ErrPlanStale
	}
	runtime.HookPlan = d.Hooks.restore()
	// Current selection and identities validate the original source map. This
	// reader's bytes are discarded; execution below uses only original blobs.
	fresh := &PreparedInstalledRuntime{files: map[string][]byte{}}
	current := &materializedIntegrations{}
	if err := runtime.captureMaterializedIntegrationSources(fresh, current); err != nil {
		return err
	}
	if err := runtime.checkMaterializedIntegrationDescription(d, current, p.files); err != nil {
		return err
	}
	p.executionFiles = map[string][]byte{}
	use := func(path string) ([]byte, error) {
		payload, exists := p.files[path]
		if !exists || path != materializedBlobPath(payload) {
			return nil, errors.New("invalid materialized payload reference")
		}
		allowed[path] = true
		return payload, nil
	}
	for _, source := range d.Sources {
		if !validMaterializedLayout(source.Layout) {
			return domain.ErrPlanStale
		}
		payload, err := use(source.Payload)
		if err != nil {
			return err
		}
		p.executionFiles[source.Layout] = payload
	}
	for _, source := range d.Environment {
		payload, err := use(source.Payload)
		if err != nil {
			return err
		}
		p.executionFiles[source.Payload] = payload
	}
	for _, file := range original.Configs.Files {
		if file.Payload != "" {
			if _, err := use(file.Payload); err != nil {
				return err
			}
		}
	}
	for _, source := range original.Configs.Sources {
		if source.Exists {
			if _, err := use(source.Payload); err != nil {
				return err
			}
		}
	}
	entries := restoreMaterializedArtifacts(d.Entries)
	for index, e := range entries {
		if e.Content == "" {
			continue
		}
		payload, err := use(e.Content)
		if err != nil {
			return err
		}
		entries[index].Content = base64.StdEncoding.EncodeToString(payload)
	}
	if len(allowed) != len(p.files) {
		return errors.New("unrecognized integration recipe leaf")
	}
	plan, hooks, err := p.overlayAfterOwnerIntegrations(d)
	if err != nil {
		return err
	}
	runtime.HookPlan = hooks
	prepared := &PreparedIntegrations{plan: plan, configs: p.configs, entries: entries, skipped: d.Skipped, runtime: d.Runtime, hooks: runtime.HookPlan}
	expectedBinding := d.Binding
	if p.afterOwnerIntegrations != nil {
		expectedBinding = prepared.calculateBinding()
	}
	if prepared.calculateBinding() != expectedBinding {
		return domain.ErrPlanStale
	}
	prepared.binding = expectedBinding
	p.integrations = prepared
	if _, _, err := p.stageUsage(); err != nil {
		return err
	}
	return nil
}

func (runtime Runtime) checkMaterializedIntegrationSources(p *PreparedInstalledRuntime) error {
	original, _, err := decodeMaterializedMetadata(p)
	if err != nil {
		return err
	}
	if original.Integrations == nil {
		return domain.ErrPlanStale
	}
	runtime.HookPlan = original.Integrations.Hooks.restore()
	fresh := &PreparedInstalledRuntime{files: map[string][]byte{}}
	current := &materializedIntegrations{}
	if err := runtime.captureMaterializedIntegrationSources(fresh, current); err != nil {
		return err
	}
	if err := runtime.checkMaterializedIntegrationDescription(original.Integrations, current, p.files); err != nil {
		return err
	}
	return nil
}

func (runtime Runtime) materializedIntegrationExecution(ctx context.Context, p *PreparedInstalledRuntime) (Runtime, error) {
	execution, err := runtime.preparedSourceExecution(ctx, p)
	if err != nil {
		return Runtime{}, err
	}
	original, _, err := decodeMaterializedMetadata(p)
	if err != nil {
		return Runtime{}, err
	}
	mapper := runtime
	mapper.LogicalRepositoryRoot = p.directory
	definitions := slices.Clone(runtime.Integrations)
	if runtime.Integrations == nil {
		definitions, err = profile.LoadIntegrations(p.directory)
	}
	if err != nil {
		return Runtime{}, err
	}
	for index := range definitions {
		definitions[index].Root = mapper.bindingPath(definitions[index].Root)
	}
	execution.Integrations = definitions
	profiles := slices.Clone(runtime.Profiles)
	if runtime.Profiles == nil {
		profiles, err = profile.Load(p.directory)
	}
	if err != nil {
		return Runtime{}, err
	}
	for index := range profiles {
		profiles[index].Root = mapper.bindingPath(profiles[index].Root)
	}
	execution.Profiles = profiles
	execution.SourceReader = nil
	execution.SourceReader = func(asset config.MaterializedAsset) ([]byte, error) {
		value, exists := p.configs.sources[configSourceKey{asset.Source, asset.FollowSymlinks}]
		if exists {
			if !value.exists {
				return nil, fs.ErrNotExist
			}
			return value.payload, nil
		}
		for _, source := range original.Integrations.Environment {
			if asset.Source == filepath.Join(p.directory, source.Payload) {
				return p.files[source.Payload], nil
			}
		}
		for _, source := range original.Integrations.Sources {
			if runtime.bindingPath(asset.Source) == string(source.Source) || asset.Source == filepath.Join(p.directory, source.Layout) {
				return p.files[source.Payload], nil
			}
		}
		return nil, domain.ErrPlanStale
	}
	for _, source := range original.Integrations.Environment {
		execution.Environment = preparedInstalledEnvironment(execution.Environment, source.Name, filepath.Join(p.directory, source.Payload))
	}
	for key, value := range map[string]string{"SUBYARD_CONFIG_DIR": filepath.Join(p.directory, "config"), "SUBYARD_REPOSITORY_ROOT": p.directory} {
		execution.Environment = preparedInstalledEnvironment(execution.Environment, key, value)
	}
	execution.HookPlan = p.integrations.hooks
	return execution, nil
}

func (runtime Runtime) ApplyPreparedMaterializedIntegrations(ctx context.Context, p *PreparedInstalledRuntime) error {
	if p == nil || p.owner != "materialized-integrations" || p.closed.Load() || p.runtime != runtime.configRuntimeBinding() || p.resources != runtime.installedResourceBinding() {
		return domain.ErrPlanStale
	}
	var err error
	p.configs, err = runtime.restoreMaterializedConfigs(ctx, p)
	if err != nil {
		return err
	}
	if err := runtime.restoreMaterializedIntegrations(ctx, p); err != nil {
		return err
	}
	p.integrations.sources = p
	runtime.HookPlan = p.integrations.hooks
	if err := runtime.CheckProjectHookPlan(ctx, true); err != nil {
		return err
	}
	return runtime.ApplyPreparedIntegrations(ctx, p.integrations)
}

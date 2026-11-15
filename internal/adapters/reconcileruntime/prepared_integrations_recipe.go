package reconcileruntime

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
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
		if definition.Retired {
			continue
		}
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
	// Only the existing owning provision source settings are remapped.
	// Resource paths and arbitrary environment values are never rewritten.
	var names []string
	for _, entry := range runtime.Environment {
		name, value, ok := strings.Cut(entry, "=")
		if ok && value != "" && strings.HasPrefix(name, "AGENT_") && strings.HasSuffix(name, "_PROVISION") {
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

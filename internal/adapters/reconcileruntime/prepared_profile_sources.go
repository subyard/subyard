package reconcileruntime

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
)

func (runtime Runtime) preparedProfileDefinition(owner string) (profile.Definition, error) {
	definitions, err := runtime.profileRuntimeDefinitions()
	if err != nil {
		return profile.Definition{}, err
	}
	for _, definition := range definitions {
		if definition.Runtime.ActivationID == owner {
			if !domain.SafeName(definition.Name) {
				return definition, errors.New("invalid prepared profile identity")
			}
			expected, err := os.Stat(filepath.Join(runtime.RepositoryRoot, "config/profiles", definition.Name))
			if err != nil {
				return definition, err
			}
			actual, err := os.Stat(definition.Root)
			if err != nil || !os.SameFile(expected, actual) {
				return definition, errors.New("prepared profile root mismatch")
			}
			_, err = definition.ExecutablePath(definition.Runtime.Handler)
			return definition, err
		}
	}
	return profile.Definition{}, errors.New("prepared profile owner unavailable")
}

func (runtime Runtime) preparedProfileExecution(ctx context.Context, p *PreparedInstalledRuntime) (Runtime, error) {
	execution, err := runtime.preparedSourceExecution(ctx, p)
	if err != nil {
		return Runtime{}, err
	}
	definitions, err := runtime.profileDefinitions()
	if err != nil {
		return Runtime{}, err
	}
	mapper := runtime
	mapper.LogicalRepositoryRoot = p.directory
	for index := range definitions {
		definitions[index].Root = mapper.bindingPath(definitions[index].Root)
	}
	execution.Profiles = definitions
	execution.Environment = preparedInstalledEnvironment(execution.Environment, "SUBYARD_REPOSITORY_ROOT", p.directory)
	execution.Environment = preparedInstalledEnvironment(execution.Environment, "SUBYARD_CONFIG_DIR", filepath.Join(p.directory, "config"))
	hooks := runtime.environmentValue("SUBYARD_PROFILE_GUEST_ENVIRONMENT_HOOKS")
	execution.Environment = preparedInstalledEnvironment(execution.Environment, "SUBYARD_PROFILE_GUEST_ENVIRONMENT_HOOKS", mapper.bindingPath(hooks))
	return execution, nil
}

func (runtime Runtime) ApplyPreparedProfileRuntimeSources(ctx context.Context, p *PreparedInstalledRuntime, operation, actual, desired string, custody ...string) error {
	if p == nil || p.profile == "" {
		return domain.ErrPlanStale
	}
	if err := runtime.validateInstalledRecipe(p); err != nil {
		return err
	}
	before, err := runtime.ObserveProfileRuntime(ctx, p.owner)
	if err != nil {
		return err
	}
	if before.State == ports.RuntimeStateCurrent && before.Actual == desired && before.Desired == desired {
		return nil
	}
	if before.State != ports.RuntimeStateStale || before.Actual != actual || before.Desired != desired {
		return domain.ErrPlanStale
	}
	execution, err := runtime.preparedProfileExecution(ctx, p)
	if err != nil {
		return err
	}
	defer p.Close()
	if len(custody) == 0 {
		err = execution.ApplyProfileRuntime(ctx, p.owner, operation, actual, desired)
	} else if len(custody) == 2 {
		err = execution.ApplyProfileRuntimeCustody(ctx, p.owner, operation, actual, desired, custody[0], custody[1])
	} else {
		err = domain.ErrPlanStale
	}
	if err != nil && p.stageBinding != "" {
		p.retain.Store(true)
	}
	return err
}

func (runtime Runtime) FencePreparedProfileRuntimeSources(ctx context.Context, p *PreparedInstalledRuntime, expected, generation, successor string) error {
	if p == nil || p.profile == "" {
		return domain.ErrPlanStale
	}
	if err := runtime.validateInstalledRecipe(p); err != nil {
		return err
	}
	execution, err := runtime.preparedProfileExecution(ctx, p)
	if err != nil {
		return err
	}
	defer p.Close()
	err = execution.FenceProfileRuntime(ctx, p.owner, expected, generation, successor)
	if err != nil && p.stageBinding != "" {
		p.retain.Store(true)
	}
	return err
}

// profileSourcePaths selects only the registered package and its existing
// declared native layout. No request can name a profile root or executable.
func (runtime Runtime) profileSourcePaths(owner string) (profile.Definition, []string, []installedSourceGuard, error) {
	definition, err := runtime.preparedProfileDefinition(owner)
	if err != nil {
		return definition, nil, nil, err
	}
	paths := []string{}
	prefix := filepath.Join("config/profiles", definition.Name)
	err = filepath.WalkDir(definition.Root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "tests" || entry.Name() == ".git" || entry.Name() == "__pycache__" {
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
			return errors.New("prepared profile source must be regular")
		}
		relative, err := filepath.Rel(definition.Root, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.Join(prefix, relative))
		return nil
	})
	if err != nil {
		return definition, nil, nil, err
	}
	for _, leaf := range append(append([]string{}, installedRuntimeHelpers...), "native-owner-barrier.py") {
		relative := filepath.Join("scripts/lib", leaf)
		if _, err := os.Lstat(filepath.Join(runtime.RepositoryRoot, relative)); err == nil {
			paths = append(paths, relative)
		} else if !os.IsNotExist(err) {
			return definition, nil, nil, err
		}
	}
	for _, relative := range []string{"scripts/lib-power.sh", "scripts/lib-service.sh", "config/skills/deliver.py"} {
		if _, err := os.Lstat(filepath.Join(runtime.RepositoryRoot, relative)); err == nil {
			paths = append(paths, relative)
		} else if !os.IsNotExist(err) {
			return definition, nil, nil, err
		}
	}
	// Guest-environment hook paths are already declared by the same verified
	// catalog and can be consumed by a selected profile's ordinary installer.
	definitions, err := runtime.profileDefinitions()
	if err != nil {
		return definition, nil, nil, err
	}
	for _, other := range definitions {
		if other.GuestEnvironment != nil {
			if _, err := other.ExecutablePath(other.GuestEnvironment.Handler); err != nil {
				return definition, nil, nil, err
			}
			paths = append(paths, filepath.Join("config/profiles", other.Name, other.GuestEnvironment.Handler))
		}
	}
	native := []installedSourceGuard{}
	for _, entry := range definition.Native {
		relative := filepath.Join(prefix, entry.Path)
		if _, err := os.Lstat(filepath.Join(runtime.RepositoryRoot, relative)); os.IsNotExist(err) {
			relative = filepath.Join(".build/profiles", definition.Name, entry.Path)
		} else if err != nil {
			return definition, nil, nil, err
		}
		info, err := os.Lstat(filepath.Join(runtime.RepositoryRoot, relative))
		if err != nil {
			return definition, nil, nil, err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return definition, nil, nil, errors.New("unsafe prepared profile native source")
		}
		paths = append(paths, relative)
		before, err := nativeInstalledFilesLabeled([]string{filepath.Join(runtime.RepositoryRoot, relative)}, runtime.bindingPath)
		if err != nil {
			return definition, nil, nil, err
		}
		native = append(native, installedSourceGuard{relative, before})
	}
	const writer = "internal/adapters/configmaterial/tomli_w.py"
	if _, err := os.Lstat(filepath.Join(runtime.RepositoryRoot, writer)); err == nil {
		paths = append(paths, writer)
		before, err := nativeInstalledFilesLabeled([]string{filepath.Join(runtime.RepositoryRoot, writer)}, runtime.bindingPath)
		if err != nil {
			return definition, nil, nil, err
		}
		native = append(native, installedSourceGuard{writer, before})
	} else if !os.IsNotExist(err) {
		return definition, nil, nil, err
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	slices.SortFunc(native, func(a, b installedSourceGuard) int { return strings.Compare(a.Path, b.Path) })
	if len(paths) > 2048 {
		return definition, nil, nil, errors.New("prepared profile file bound")
	}
	return definition, paths, native, nil
}

func (runtime Runtime) PrepareProfileRuntimeSources(ctx context.Context, owner string) (*PreparedInstalledRuntime, error) {
	definition, paths, native, err := runtime.profileSourcePaths(owner)
	if err != nil {
		return nil, err
	}
	source, err := runtime.SelectedRuntimeInputs(owner)
	if err != nil {
		return nil, err
	}
	p := &PreparedInstalledRuntime{owner: owner, source: source, runtime: runtime.configRuntimeBinding(), resources: runtime.installedResourceBinding(), profile: definition.Name, handler: definition.Runtime.Handler, native: native, leaves: []string{definition.Runtime.Handler}, files: map[string][]byte{}}
	total := 0
	for _, relative := range paths {
		asset := config.MaterializedAsset{Source: filepath.Join(runtime.RepositoryRoot, relative)}
		var payload []byte
		if runtime.SourceReader != nil {
			payload, err = runtime.SourceReader(asset)
		} else {
			payload, err = asset.ReadSource()
		}
		if err != nil {
			return nil, err
		}
		total += len(payload)
		if total > 32<<20 {
			return nil, errors.New("prepared profile source bound")
		}
		p.files[relative] = payload
	}
	if err := runtime.validateInstalledRecipe(p); err != nil {
		return nil, err
	}
	p.binding = installedRuntimeDigest(json.RawMessage(p.Recipe()))
	return p, nil
}

func (runtime Runtime) validateProfileRuntimeSources(p *PreparedInstalledRuntime) error {
	definition, paths, native, err := runtime.profileSourcePaths(p.owner)
	if err != nil {
		return err
	}
	if definition.Name != p.profile || definition.Runtime.Handler != p.handler || !slices.Equal(native, p.native) || !slices.Equal(p.leaves, []string{p.handler}) || len(paths) != len(p.files) {
		return domain.ErrPlanStale
	}
	for _, path := range paths {
		if _, ok := p.files[path]; !ok {
			return domain.ErrPlanStale
		}
	}
	return nil
}

func (runtime Runtime) bindProfileRuntimeSources(r installedRecipe, raw []byte) (*PreparedInstalledRuntime, error) {
	if r.Profile == "" || r.Handler == "" || r.Launcher != "" || r.HookBefore != "" || len(r.Files) > 2048 {
		return nil, errors.New("invalid prepared profile recipe")
	}
	p := &PreparedInstalledRuntime{owner: r.Owner, source: r.Source, runtime: r.Runtime, resources: r.Resources, profile: r.Profile, handler: r.Handler, native: r.Native, leaves: r.Leaves, files: map[string][]byte{}}
	total := 0
	previous := ""
	for _, file := range r.Files {
		if file.Path <= previous || len(file.Path) > 512 {
			return nil, errors.New("invalid prepared profile file order")
		}
		previous = file.Path
		total += len(file.Content)
		if total > 32<<20 {
			return nil, errors.New("prepared profile source bound")
		}
		p.files[file.Path] = file.Content
	}
	if err := runtime.validateInstalledRecipe(p); err != nil {
		return nil, err
	}
	p.binding = installedRuntimeDigest(json.RawMessage(raw))
	return p, nil
}

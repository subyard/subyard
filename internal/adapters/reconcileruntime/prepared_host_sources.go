package reconcileruntime

import (
	"context"
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

// hostInstallerSourcePaths retains the existing declaration reader's complete
// public source scope. Its sealed engine still runs RequireReader against the
// actual protected ledger; no captured verdict replaces that check.
func (runtime Runtime) hostInstallerSourcePaths(owner string) (string, []string, error) {
	var handler string
	switch owner {
	case "host-power":
		handler = "install-power-reconciler.sh"
	case "test-vms-host-sink":
		handler = "install-test-vms-host-sink.sh"
	case "materialized-integrations":
		handler = "reconcile-integrations.sh"
	default:
		return "", nil, errors.New("unknown host installer source recipe")
	}
	paths := []string{}
	for _, prefix := range []string{"config", "scripts"} {
		root := filepath.Join(runtime.RepositoryRoot, prefix)
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
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
				return errors.New("host installer source must be regular")
			}
			relative, err := filepath.Rel(runtime.RepositoryRoot, path)
			if err != nil {
				return err
			}
			if len(relative) > 512 {
				return errors.New("host installer path bound")
			}
			paths = append(paths, relative)
			if len(paths) > 4096 {
				return errors.New("host installer file bound")
			}
			return nil
		})
		if err != nil {
			return "", nil, err
		}
	}
	slices.Sort(paths)
	if !slices.Contains(paths, filepath.Join("scripts", handler)) {
		return "", nil, errors.New("host installer source unavailable")
	}
	return handler, paths, nil
}

func (runtime Runtime) PrepareHostInstallerSources(ctx context.Context, owner string) (*PreparedInstalledRuntime, error) {
	handler, paths, err := runtime.hostInstallerSourcePaths(owner)
	if err != nil {
		return nil, err
	}
	source, err := runtime.SelectedRuntimeInputs("core-guest-runtime")
	if err != nil {
		return nil, err
	}
	p := &PreparedInstalledRuntime{owner: owner, source: source, runtime: runtime.configRuntimeBinding(), resources: runtime.installedResourceBinding(), handler: handler, leaves: []string{handler}, files: map[string][]byte{}}
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
			return nil, errors.New("host installer source bound")
		}
		p.files[relative] = payload
	}
	if err := runtime.validateInstalledRecipe(p); err != nil {
		return nil, err
	}
	p.binding = installedRuntimeDigest(json.RawMessage(p.Recipe()))
	return p, nil
}

func (runtime Runtime) validateHostInstallerSources(p *PreparedInstalledRuntime) error {
	handler, paths, err := runtime.hostInstallerSourcePaths(p.owner)
	if err != nil {
		return err
	}
	if p.profile != "" || p.handler != handler || p.hookBefore != "" || p.keyLauncher != "" || len(p.native) != 0 || !slices.Equal(p.leaves, []string{handler}) || len(paths) != len(p.files) {
		return domain.ErrPlanStale
	}
	for _, path := range paths {
		if _, ok := p.files[path]; !ok {
			return domain.ErrPlanStale
		}
	}
	return nil
}

func (runtime Runtime) bindHostInstallerSources(r installedRecipe, raw []byte) (*PreparedInstalledRuntime, error) {
	p := &PreparedInstalledRuntime{owner: r.Owner, source: r.Source, runtime: r.Runtime, resources: r.Resources, handler: r.Handler, profile: r.Profile, native: r.Native, leaves: r.Leaves, keyLauncher: r.Launcher, hookBefore: r.HookBefore, files: map[string][]byte{}}
	total := 0
	previous := ""
	for _, file := range r.Files {
		if file.Path <= previous || len(file.Path) > 512 || len(r.Files) > 4096 {
			return nil, errors.New("host installer source order")
		}
		previous = file.Path
		total += len(file.Content)
		if total > 32<<20 {
			return nil, errors.New("host installer source bound")
		}
		p.files[file.Path] = file.Content
	}
	if err := runtime.validateInstalledRecipe(p); err != nil {
		return nil, err
	}
	p.binding = installedRuntimeDigest(json.RawMessage(raw))
	return p, nil
}

func (runtime Runtime) hostInstallerExecution(ctx context.Context, p *PreparedInstalledRuntime) (Runtime, error) {
	if err := runtime.validateInstalledRecipe(p); err != nil {
		return Runtime{}, err
	}
	return runtime.preparedSourceExecution(ctx, p)
}

func (runtime Runtime) ApplyPreparedHostPowerRuntimeCustodySources(ctx context.Context, prepared *PreparedHostPower, sources *PreparedInstalledRuntime, expected []ports.NativeResourceGeneration, generation string) error {
	vector, err := hostPowerVector(expected)
	if err != nil || sources == nil || sources.owner != "host-power" || !profileRuntimeDigest.MatchString(generation) {
		return domain.ErrPlanStale
	}
	execution, err := runtime.hostInstallerExecution(ctx, sources)
	if err != nil {
		return err
	}
	defer sources.Close()
	err = runtime.applyPreparedHostPowerRuntime(ctx, prepared, map[string]string{"SUBYARD_NATIVE_EXPECTED_VECTOR": vector, "SUBYARD_NATIVE_GENERATION": generation}, execution)
	if err != nil && sources.stageBinding != "" {
		sources.retain.Store(true)
	}
	return err
}
func (runtime Runtime) FencePreparedHostPowerRuntimeSources(ctx context.Context, prepared *PreparedHostPower, sources *PreparedInstalledRuntime, expected []ports.NativeResourceGeneration, generation, successor string) error {
	if sources == nil || sources.owner != "host-power" {
		return domain.ErrPlanStale
	}
	execution, err := runtime.hostInstallerExecution(ctx, sources)
	if err != nil {
		return err
	}
	defer sources.Close()
	err = runtime.fencePreparedHostPowerRuntime(ctx, prepared, expected, generation, successor, execution)
	if err != nil && sources.stageBinding != "" {
		sources.retain.Store(true)
	}
	return err
}
func (runtime Runtime) ApplyPreparedTestVMBrokerRuntimeSources(ctx context.Context, prepared *PreparedTestVMBroker, sources *PreparedInstalledRuntime) error {
	if !runtime.Yard.NestedE2EVMs {
		return runtime.ApplyPreparedTestVMBrokerRuntime(ctx, prepared)
	}
	if sources == nil || sources.owner != "test-vms-host-sink" {
		return domain.ErrPlanStale
	}
	execution, err := runtime.hostInstallerExecution(ctx, sources)
	if err != nil {
		return err
	}
	defer sources.Close()
	err = runtime.applyPreparedTestVMBrokerRuntime(ctx, prepared, execution)
	if err != nil && sources.stageBinding != "" {
		sources.retain.Store(true)
	}
	return err
}

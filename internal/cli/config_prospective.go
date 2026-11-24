package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/safefs"
)

// A retained draft uses the production resolver with its normal logical paths.
// The binding includes absent layer inputs and inode/security metadata, so an
// equivalent-value replacement still requires a new assessment.
type configProspective struct {
	cli             *CLI
	loaded          config.Loaded
	request         configAuthoringRequest
	path, draft     string
	binding         string
	semantics       string
	directoryInputs map[string]bool
	checkBatch      func(locked bool) error
}

func (cli *CLI) prepareConfigProspective(loaded config.Loaded, request configAuthoringRequest, path string, desired config.PersistentFileSnapshot) (*configProspective, error) {
	plan := &configProspective{cli: cli, loaded: loaded, request: request, path: path}
	if desired.Exists {
		file, err := os.CreateTemp("/tmp", "subyard-config-draft-*") //nolint:forbidigo // scratch draft, never published
		if err != nil {
			return nil, err
		}
		plan.draft = file.Name()
		err = file.Chmod(0o600)
		if err == nil {
			_, err = file.Write(desired.Content)
		}
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			plan.close()
			return nil, err
		}
	}
	var err error
	plan.binding, plan.semantics, err = plan.inspect(true, false)
	if err != nil {
		plan.close()
		return nil, err
	}
	return plan, nil
}

func (plan *configProspective) close() {
	if plan.draft != "" {
		_ = os.Remove(plan.draft)
	}
}

func (plan *configProspective) check() error { return plan.checkLocked(false) }

func (plan *configProspective) checkLocked(locked bool) error {
	if plan.checkBatch != nil {
		if err := plan.checkBatch(locked); err != nil {
			return fmt.Errorf("%w: %w", domain.ErrPlanStale, configProspectiveError{plan.loaded.Context.Paths.ConfigHome, err})
		}
	}
	binding, semantics, err := plan.inspect(true, locked)
	if err != nil {
		return fmt.Errorf("%w: prospective configuration can no longer be validated: %w", domain.ErrPlanStale, err)
	}
	if binding != plan.binding || semantics != plan.semantics {
		return fmt.Errorf("%w: configuration dependencies changed after confirmation", domain.ErrPlanStale)
	}
	return nil
}

func (plan *configProspective) verify() error {
	_, semantics, err := plan.inspect(false, false)
	if err != nil {
		return fmt.Errorf("persistent configuration verification: %w", err)
	}
	if semantics != plan.semantics {
		return errors.New("persistent configuration verification: effective scoped configuration differs from the approved draft")
	}
	return nil
}

type configProspectiveError struct {
	home  string
	cause error
}

func (err configProspectiveError) Error() string {
	return "prospective configuration: " + config.Diagnose(err.home, err.cause).Message("an affected configuration input is invalid or unavailable")
}
func (err configProspectiveError) Unwrap() error { return err.cause }

// reportConfigRefusal prints a config refusal only through config.Diagnose.
func (cli *CLI) reportConfigRefusal(prefix, home string, err error) {
	diagnostic := config.Diagnose(home, err)
	cli.errorf("%s: %s", prefix, diagnostic.Message("an input is invalid or unavailable"))
	for _, next := range diagnostic.Next {
		cli.errorf("Next: %s", next)
	}
}

// reportConfigNext prints every fixed next step config.Diagnose finds. Callers
// pass only errors from a config command boundary.
func (cli *CLI) reportConfigNext(err error) {
	home, cause := "", err
	var prospective configProspectiveError
	if errors.As(err, &prospective) {
		home, cause = prospective.home, prospective.cause
	} else {
		home, _ = config.ResolveConfigHome(firstNonempty(cli.env["SUBYARD_OPERATOR_HOME"], cli.env["HOME"]), cli.env)
	}
	for _, next := range config.Diagnose(home, cause).Next {
		cli.errorf("Next: %s", next)
	}
}

func (cli *CLI) configProspectiveEnvironment(loaded config.Loaded) map[string]string {
	paths := loaded.Context.Paths
	environment := make(map[string]string)
	for name, value := range cli.baseEnv {
		if _, setting := loaded.Catalog.LookupSetting(name); setting || loaded.Catalog.IsLegacySetting(name) {
			continue
		}
		environment[name] = value
	}
	environment["SUBYARD_CONFIG_HOME"] = paths.ConfigHome
	environment["SUBYARD_CONFIG_DIR"] = paths.ConfigDir
	environment["SUBYARD_HOME"] = paths.DataHome
	delete(environment, "SUBYARD_YARD")
	return environment
}

func (plan *configProspective) inspect(draft, locked bool) (binding, semantics string, result error) {
	defer func() {
		if result != nil {
			result = configProspectiveError{plan.loaded.Context.Paths.ConfigHome, result}
		}
	}()
	paths := plan.loaded.Context.Paths
	var refusals []error
	checkTarget := func() error {
		switch plan.request.action {
		case "set", "unset", "selection-repair":
			current, err := configScalarAuthoringPath(plan.loaded, plan.request.scope)
			if err != nil {
				return err
			}
			if current != plan.path {
				return domain.ErrPlanStale
			}
		}
		return nil
	}
	refusals = append(refusals, checkTarget())
	names := []string{plan.loaded.Context.YardName}
	if plan.request.scope != config.ScopeYard {
		var err error
		names, err = config.LocalBatchYardNames(paths.ConfigHome)
		refusals = append(refusals, err)
	}
	environment := plan.cli.configProspectiveEnvironment(plan.loaded)
	var overrides map[string]string
	if draft {
		overrides = map[string]string{plan.path: plan.draft}
	}
	var contexts []config.Loaded
	inputs := map[string]config.PersistentFileSnapshot{}
	directories := map[string]config.PersistentFileIdentity{}
	observeDirectories := func(path string) error {
		for directory := filepath.Dir(path); directory == paths.ConfigHome || strings.HasPrefix(directory, paths.ConfigHome+string(filepath.Separator)); directory = filepath.Dir(directory) {
			_ = safefs.RepairPath(directory, 0o022)
			info, err := os.Lstat(directory)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
				return domain.NewConfigRefusal(domain.ConfigDependencyDirectoryUnsafe)
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok || stat.Uid != uint32(os.Getuid()) {
				return domain.NewConfigRefusal(domain.ConfigDependencyDirectoryNotOwned)
			}
			directories[directory] = config.PersistentFileIdentity{Device: uint64(stat.Dev), Inode: stat.Ino, Mode: stat.Mode, UID: stat.Uid, GID: stat.Gid}
		}
		return nil
	}
	refusals = append(refusals, observeDirectories(plan.path))
	readInput := func(path string) error {
		if path == "" || !filepath.IsAbs(path) || path == plan.path {
			return nil
		}
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			return nil
		}
		if err := observeDirectories(path); err != nil {
			return err
		}
		var snapshot config.PersistentFileSnapshot
		var err error
		if !strings.HasPrefix(path, paths.ConfigHome+string(filepath.Separator)) {
			// Runtime defaults may be addressed through the installed runtime link.
			resolved, resolveErr := filepath.EvalSymlinks(filepath.Dir(path))
			if resolveErr != nil && !errors.Is(resolveErr, os.ErrNotExist) {
				return resolveErr
			}
			if resolveErr == nil {
				path = filepath.Join(resolved, filepath.Base(path))
			}
			snapshot, err = readConfigDependencySnapshot(path)
		} else {
			snapshot, err = config.ReadPersistentFileSnapshot(paths.ConfigHome, path)
		}
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
		if err != nil {
			return err
		}
		inputs[path] = snapshot
		return nil
	}
	observed := map[string]bool{}
	observe := func(path string) {
		if !observed[path] {
			observed[path] = true
			refusals = append(refusals, readInput(path))
		}
	}
	for _, name := range names {
		for _, candidate := range config.LocalYardSettingsCandidates(filepath.Join(paths.ConfigHome, "yards"), name) {
			observe(candidate)
		}
		observe(filepath.Join(paths.ConfigHome, ".yard-reset-"+name))
		loaded, err := config.Load(config.LoadOptions{Resources: plan.cli.operationResourceRegistry, RepositoryRoot: plan.cli.options.RepositoryRoot,
			OperatorHome: paths.OperatorHome, YardName: name, Environment: environment, FileOverrides: overrides, ConfigLocked: locked})
		if err != nil {
			refusals = append(refusals, fmt.Errorf("yard %s: %w", name, err))
			continue
		}
		if draft && plan.request.action == "selection-repair" {
			before, err := config.Load(config.LoadOptions{Resources: plan.cli.operationResourceRegistry, RepositoryRoot: plan.cli.options.RepositoryRoot,
				OperatorHome: paths.OperatorHome, YardName: name, Environment: environment, ConfigLocked: locked})
			refusals = append(refusals, err)
			if err == nil {
				refusals = append(refusals, plan.cli.checkSelectionReduction(before, loaded, plan.request.name))
			}
		}
		contexts = append(contexts, loaded)
		for _, path := range loaded.AssetValidationInputs() {
			observe(path)
		}
		for _, layer := range loaded.ConfigurationLayers {
			if layer.Role == "scalar settings" || layer.Role == "Git scalar settings" || layer.Role == "shipped profile" {
				observe(layer.Path)
			}
			if layer.Role == "file settings" || layer.Role == "Git file settings" {
				for _, mapping := range config.SyncableFileMappings(loaded) {
					observe(filepath.Join(layer.Path, filepath.FromSlash(mapping.Relative)))
				}
			}
		}
		for _, declaration := range loaded.Catalog.Profiles() {
			for _, path := range declaration.ValidationInputs() {
				observe(path)
			}
		}
		for _, declaration := range loaded.Catalog.Integrations() {
			for _, path := range declaration.ValidationInputs() {
				observe(path)
			}
		}
		for _, setting := range loaded.Settings {
			for _, resolution := range setting.Resolutions {
				observe(resolution.Path)
			}
		}
		selectedProfiles := strings.Fields(loaded.Environment["ENVIRONMENT_PROFILES"])
		if loaded.Context.AccessKind == domain.AccessLocal && len(selectedProfiles) != 0 {
			packages, err := profile.Packages(plan.cli.options.RepositoryRoot)
			refusals = append(refusals, err)
			for _, pkg := range packages {
				for _, source := range pkg.Resources {
					observe(source.Path)
				}
			}
			registry, err := resource.Load(plan.cli.options.RepositoryRoot)
			refusals = append(refusals, err)
			for _, definition := range registry.Definitions() {
				observe(definition.HandlerPath())
				if definition.Endpoint == nil || definition.Proxy == nil || !slices.Contains(selectedProfiles, definition.Profile) {
					continue
				}
				host, port := config.ResourceEndpointOverrides(loaded, definition)
				if host != "" && port != "" {
					continue
				}
				observe(filepath.Join(paths.DataHome, "resource-endpoints", "state.json"))
			}
		}
		for _, name := range strings.Fields(loaded.Environment["ENVIRONMENT_PROFILES"]) {
			pkg, err := profile.ResolvePackageInConfig(paths.ConfigDir, name)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				refusals = append(refusals, err)
				continue
			}
			if pkg.Config != nil {
				observe(pkg.Config.Path)
			}
		}
	}
	for _, name := range []string{"incus.project.env", "subyard.env", "host.env", "agents.env", "ports.env"} {
		observe(filepath.Join(paths.ConfigDir, name))
	}
	for _, path := range []string{filepath.Join(paths.ConfigHome, ".sync", "manifest.json"), filepath.Join(paths.ConfigHome, config.SourceRecordRelativePath)} {
		observe(path)
	}
	refusals = append(refusals, checkTarget())
	if err := domain.CollectRefusals(refusals...); err != nil {
		return "", "", err
	}
	if plan.directoryInputs == nil {
		plan.directoryInputs = map[string]bool{}
		for path := range directories {
			plan.directoryInputs[path] = true
		}
	} else {
		// Creating an absent parent is part of publication. Existing parents
		// remain bound; a newly populated configuration input is bound above.
		bound := map[string]config.PersistentFileIdentity{}
		for path := range plan.directoryInputs {
			bound[path] = directories[path]
		}
		directories = bound
	}
	return operationStateDigest(struct {
		Files       map[string]config.PersistentFileSnapshot
		Directories map[string]config.PersistentFileIdentity
	}{inputs, directories}), operationStateDigest(contexts), nil
}

// Read-only sources follow the loader's reader contract, regardless of ownership
// or writable permission bits. The mutation target uses the stricter CAS reader.
func readConfigDependencySnapshot(path string) (config.PersistentFileSnapshot, error) {
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return config.PersistentFileSnapshot{}, nil
	}
	if err != nil {
		return config.PersistentFileSnapshot{}, err
	}
	content, err := (config.MaterializedAsset{Source: path}).ReadSource()
	if err != nil {
		return config.PersistentFileSnapshot{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return config.PersistentFileSnapshot{}, err
	}
	if !os.SameFile(before, info) || before.Mode() != info.Mode() || before.Size() != info.Size() || before.ModTime() != info.ModTime() {
		return config.PersistentFileSnapshot{}, domain.ErrPlanStale
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return config.PersistentFileSnapshot{}, errors.New("configuration dependency identity is unavailable")
	}
	return config.PersistentFileSnapshot{Exists: true, Content: content, Identity: config.PersistentFileIdentity{
		Device: uint64(stat.Dev), Inode: stat.Ino, Mode: stat.Mode, UID: stat.Uid, GID: stat.Gid, Links: uint64(stat.Nlink),
	}}, nil
}

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
	scalarEdits     config.PersistentScalarEdits
}

func (cli *CLI) prepareConfigProspective(loaded config.Loaded, request configAuthoringRequest, path string, desired config.PersistentFileSnapshot) (*configProspective, error) {
	plan := &configProspective{cli: cli, loaded: loaded, request: request, path: path}
	if err := plan.prepare(desired); err != nil {
		return nil, err
	}
	return plan, nil
}

func (cli *CLI) prepareConfigScalarDraft(loaded config.Loaded, request configAuthoringRequest, path string, baseline config.PersistentFileSnapshot, edits config.PersistentScalarEdits) (*configProspective, config.PersistentFileSnapshot, error) {
	plan := &configProspective{cli: cli, loaded: loaded, request: request, path: path, scalarEdits: edits}
	options := plan.loadOptions()
	names, err := plan.yardNames()
	if err != nil {
		return nil, config.PersistentFileSnapshot{}, configProspectiveError{loaded.Context.Paths.ConfigHome, err}
	}
	var before []config.Loaded
	var refusals []error
	for _, name := range names {
		options.YardName = name
		current, err := config.Load(options)
		if err != nil {
			refusals = append(refusals, fmt.Errorf("yard %s: %w", name, err))
			continue
		}
		before = append(before, current)
	}
	if err := domain.CollectRefusals(refusals...); err != nil {
		return nil, config.PersistentFileSnapshot{}, configProspectiveError{loaded.Context.Paths.ConfigHome, err}
	}
	content, err := edits.LayerContent(path, baseline.Content, before)
	if err != nil {
		return nil, config.PersistentFileSnapshot{}, err
	}
	desired := config.PersistentFileSnapshot{Exists: request.action != "unset" || len(content) != 0, Content: content}
	if err := plan.prepare(desired); err != nil {
		return nil, config.PersistentFileSnapshot{}, err
	}
	return plan, desired, nil
}

func (plan *configProspective) prepare(desired config.PersistentFileSnapshot) error {
	if desired.Exists {
		file, err := os.CreateTemp("/tmp", "subyard-config-draft-*")
		if err != nil {
			return err
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
			return err
		}
	}
	var err error
	plan.binding, plan.semantics, err = plan.inspect(true, false)
	if err != nil {
		plan.close()
	}
	return err
}

func (plan *configProspective) close() {
	if plan.draft != "" {
		_ = os.Remove(plan.draft)
	}
}

func (plan *configProspective) check() error { return plan.checkLocked(false) }

func (plan *configProspective) checkLocked(locked bool) error {
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

func (plan *configProspective) yardNames() ([]string, error) {
	paths := plan.loaded.Context.Paths
	if plan.request.scope != config.ScopeYard {
		return config.YardNames(paths.ConfigDir, paths.ConfigHome)
	}
	return []string{plan.loaded.Context.YardName}, nil
}

func (plan *configProspective) loadOptions() config.LoadOptions {
	paths := plan.loaded.Context.Paths
	environment := make(map[string]string)
	for name, value := range plan.cli.baseEnv {
		if _, setting := plan.loaded.Catalog.LookupSetting(name); setting || plan.loaded.Catalog.IsLegacySetting(name) {
			continue
		}
		environment[name] = value
	}
	environment["SUBYARD_CONFIG_HOME"] = paths.ConfigHome
	environment["SUBYARD_CONFIG_DIR"] = paths.ConfigDir
	environment["SUBYARD_HOME"] = paths.DataHome
	delete(environment, "SUBYARD_YARD")
	options := config.LoadOptions{Resources: plan.cli.operationResourceRegistry, RepositoryRoot: plan.cli.options.RepositoryRoot,
		OperatorHome: paths.OperatorHome, Environment: environment}
	if plan.scalarEdits != nil {
		options.ScalarEditPath = plan.path
	}
	return options
}

func (plan *configProspective) inspect(draft, locked bool) (binding, semantics string, result error) {
	defer func() {
		if result != nil {
			result = configProspectiveError{plan.loaded.Context.Paths.ConfigHome, result}
		}
	}()
	paths := plan.loaded.Context.Paths
	names, err := plan.yardNames()
	if err != nil {
		return "", "", err
	}
	options := plan.loadOptions()
	options.ConfigLocked = locked
	candidateOptions := options
	if draft {
		candidateOptions.FileOverrides = map[string]string{plan.path: plan.draft}
	}
	var contexts []config.Loaded
	inputs := map[string]config.PersistentFileSnapshot{}
	directories := map[string]config.PersistentFileIdentity{}
	observeDirectories := func(path string) error {
		for directory := filepath.Dir(path); directory == paths.ConfigHome || strings.HasPrefix(directory, paths.ConfigHome+string(filepath.Separator)); directory = filepath.Dir(directory) {
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
	if err := observeDirectories(plan.path); err != nil {
		return "", "", err
	}
	observe := func(path string) error {
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
	var refusals []error
	for _, name := range names {
		options.YardName, candidateOptions.YardName = name, name
		loaded, err := config.Load(candidateOptions)
		if err != nil {
			refusals = append(refusals, fmt.Errorf("yard %s: %w", name, err))
			continue
		}
		if draft && (plan.scalarEdits != nil || plan.request.action == "selection-repair") {
			before, err := config.Load(options)
			if err != nil {
				refusals = append(refusals, fmt.Errorf("yard %s: %w", name, err))
				continue
			}
			if plan.scalarEdits != nil {
				refusals = append(refusals, plan.scalarEdits.CheckLayer(plan.path, before, loaded))
			}
			if plan.request.action == "selection-repair" {
				refusals = append(refusals, plan.cli.checkSelectionReduction(before, loaded, plan.request.name))
			}
		}
		contexts = append(contexts, loaded)
		for _, path := range loaded.AssetValidationInputs() {
			if err := observe(path); err != nil {
				return "", "", err
			}
		}
		for _, layer := range loaded.ConfigurationLayers {
			if layer.Role == "scalar settings" || layer.Role == "Git scalar settings" || layer.Role == "shipped profile" {
				if err := observe(layer.Path); err != nil {
					return "", "", err
				}
			}
			if layer.Role == "file settings" || layer.Role == "Git file settings" {
				for _, mapping := range config.SyncableFileMappings(loaded) {
					if err := observe(filepath.Join(layer.Path, filepath.FromSlash(mapping.Relative))); err != nil {
						return "", "", err
					}
				}
			}
		}
		for _, declaration := range loaded.Catalog.Profiles() {
			for _, path := range declaration.ValidationInputs() {
				if err := observe(path); err != nil {
					return "", "", err
				}
			}
		}
		for _, declaration := range loaded.Catalog.Integrations() {
			for _, path := range declaration.ValidationInputs() {
				if err := observe(path); err != nil {
					return "", "", err
				}
			}
		}
		for _, setting := range loaded.Settings {
			for _, resolution := range setting.Resolutions {
				if err := observe(resolution.Path); err != nil {
					return "", "", err
				}
			}
		}
		selectedProfiles := strings.Fields(loaded.Environment["ENVIRONMENT_PROFILES"])
		if loaded.Context.AccessKind == domain.AccessLocal && len(selectedProfiles) != 0 {
			packages, err := profile.Packages(plan.cli.options.RepositoryRoot)
			if err != nil {
				return "", "", err
			}
			for _, pkg := range packages {
				for _, source := range pkg.Resources {
					if err := observe(source.Path); err != nil {
						return "", "", err
					}
				}
			}
			registry, err := resource.Load(plan.cli.options.RepositoryRoot)
			if err != nil {
				return "", "", err
			}
			for _, definition := range registry.Definitions() {
				if err := observe(definition.HandlerPath()); err != nil {
					return "", "", err
				}
				if definition.Endpoint == nil || definition.Proxy == nil || !slices.Contains(selectedProfiles, definition.Profile) {
					continue
				}
				host, port := config.ResourceEndpointOverrides(loaded, definition)
				if host != "" && port != "" {
					continue
				}
				if err := observe(filepath.Join(paths.DataHome, "resource-endpoints", "state.json")); err != nil {
					return "", "", err
				}
			}
		}
		for _, name := range strings.Fields(loaded.Environment["ENVIRONMENT_PROFILES"]) {
			pkg, err := profile.ResolvePackageInConfig(paths.ConfigDir, name)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return "", "", err
			}
			if pkg.Config != nil {
				if err := observe(pkg.Config.Path); err != nil {
					return "", "", err
				}
			}
		}
		for _, candidate := range config.YardFileCandidates(paths.ConfigDir, paths.ConfigHome, name) {
			if err := observe(candidate); err != nil {
				return "", "", err
			}
		}
		if err := observe(filepath.Join(paths.ConfigHome, ".yard-reset-"+name)); err != nil {
			return "", "", err
		}
	}
	if err := domain.CollectRefusals(refusals...); err != nil {
		return "", "", err
	}
	for _, name := range []string{"incus.project.env", "subyard.env", "host.env", "agents.env", "ports.env"} {
		if err := observe(filepath.Join(paths.ConfigDir, name)); err != nil {
			return "", "", err
		}
	}
	for _, path := range []string{filepath.Join(paths.ConfigHome, ".sync", "manifest.json"), filepath.Join(paths.ConfigHome, config.SourceRecordRelativePath)} {
		if err := observe(path); err != nil {
			return "", "", err
		}
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

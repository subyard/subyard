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
}

func (cli *CLI) prepareConfigProspective(loaded config.Loaded, request configAuthoringRequest, path string, desired config.PersistentFileSnapshot) (*configProspective, error) {
	plan := &configProspective{cli: cli, loaded: loaded, request: request, path: path}
	if desired.Exists {
		file, err := os.CreateTemp("/tmp", "subyard-config-draft-*")
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
	if diagnostic, ok := config.SourceDiagnostic(err.home, err.cause); ok {
		return "prospective configuration: " + diagnostic
	}
	return "prospective configuration: an affected configuration input is invalid or unavailable"
}
func (err configProspectiveError) Unwrap() error { return err.cause }

func (plan *configProspective) inspect(draft, locked bool) (binding, semantics string, result error) {
	defer func() {
		if result != nil {
			result = configProspectiveError{plan.loaded.Context.Paths.ConfigHome, result}
		}
	}()
	paths := plan.loaded.Context.Paths
	names := []string{plan.loaded.Context.YardName}
	if plan.request.scope != config.ScopeYard {
		var err error
		names, err = config.YardNames(paths.ConfigDir, paths.ConfigHome)
		if err != nil {
			return "", "", err
		}
	}
	environment := make(map[string]string)
	for name, value := range plan.cli.baseEnv {
		if _, setting := plan.loaded.Catalog.LookupSetting(name); setting || config.IsLegacySetting(name) {
			continue
		}
		environment[name] = value
	}
	environment["SUBYARD_CONFIG_HOME"] = paths.ConfigHome
	environment["SUBYARD_CONFIG_DIR"] = paths.ConfigDir
	environment["SUBYARD_HOME"] = paths.DataHome
	delete(environment, "SUBYARD_YARD")
	var overrides map[string]string
	if draft {
		overrides = map[string]string{plan.path: plan.draft}
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
				return errors.New("configuration dependency directory has unsafe type or permissions")
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok || stat.Uid != uint32(os.Getuid()) {
				return errors.New("configuration dependency directory is not operator-owned")
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
			resolved, err := filepath.EvalSymlinks(filepath.Dir(path))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err == nil {
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
	for _, name := range names {
		loaded, err := config.Load(config.LoadOptions{RepositoryRoot: plan.cli.options.RepositoryRoot,
			OperatorHome: paths.OperatorHome, YardName: name, Environment: environment, FileOverrides: overrides, ConfigLocked: locked})
		if err != nil {
			return "", "", fmt.Errorf("yard %s: %w", name, err)
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
			metadata, err := filepath.Glob(filepath.Join(plan.cli.options.RepositoryRoot, "config", "profiles", "*", "resources", "*.res"))
			if err != nil {
				return "", "", err
			}
			for _, path := range metadata {
				if err := observe(path); err != nil {
					return "", "", err
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
		for _, profile := range strings.Fields(loaded.Environment["ENVIRONMENT_PROFILES"]) {
			if err := observe(filepath.Join(paths.ConfigDir, "profiles", profile, "profile.conf")); err != nil {
				return "", "", err
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

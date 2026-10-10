package configsync

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

// Local inputs participate in validation without becoming Git-owned targets.
// Bind their absence, content and inode identity to the exact reviewed plan.
type localInputObservation struct {
	Path     string
	Exists   bool
	Digest   string
	Mode     uint32
	Identity config.PersistentFileIdentity
}

func gitSettingsPath(path string) string {
	return filepath.ToSlash(filepath.Join(config.GitSettingsRelativePath, path))
}

func localSettingsPath(path string) string {
	return strings.TrimPrefix(filepath.ToSlash(path), config.GitSettingsRelativePath+"/")
}

// The manifest can own only the roles produced by source import. In particular,
// a protected but malformed old manifest cannot migrate identity or journals.
func managedSettingRole(path string) (asset string, valid bool) {
	if !safeRelative(path) {
		return "", false
	}
	if path == "config.env" || path == "overrides/shared/config.env" {
		return "", true
	}
	if strings.HasPrefix(path, "yards/") {
		parts := strings.SplitN(path, "/", 3)
		if len(parts) != 3 || !domain.SafeName(parts[1]) {
			return "", false
		}
		if parts[2] == "config.env" {
			return "", true
		}
		path = parts[2]
		if !strings.HasPrefix(path, "overrides/agents/") {
			return "", false
		}
		asset = strings.TrimPrefix(path, "overrides/agents/")
	} else {
		for _, prefix := range []string{"overrides/shared/agents/", "overrides/host/agents/"} {
			if strings.HasPrefix(path, prefix) {
				asset = strings.TrimPrefix(path, prefix)
				break
			}
		}
	}
	return asset, asset != "" && safeRelative(asset) && !forbiddenPath(asset)
}

func observeLocalInput(configHome, relative string) (localInputObservation, error) {
	observed := localInputObservation{Path: relative}
	if !safeRelative(relative) {
		return observed, errors.New("local configuration input path is unsafe")
	}
	path := filepath.Join(configHome, filepath.FromSlash(relative))
	if err := validateConfigurationAncestors(configHome, path); err != nil {
		return observed, err
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return observed, nil
	} else if err != nil {
		return observed, err
	}
	snapshot, err := config.ReadPersistentFileSnapshot(configHome, path)
	if err != nil {
		return observed, fmt.Errorf("inspect local setting %s: %w", relative, err)
	}
	observed.Exists = snapshot.Exists
	if snapshot.Exists {
		observed.Digest = digestBytes(snapshot.Content)
		observed.Mode = snapshot.Identity.Mode & 0o777
		observed.Identity = snapshot.Identity
	}
	return observed, nil
}

func legacyLocalExclusions(configHome string, previous Manifest) (map[string]bool, error) {
	excluded := map[string]bool{}
	if previous.SchemaVersion != 1 {
		return excluded, nil
	}
	for _, file := range previous.Files {
		observed, err := observeLocalInput(configHome, file.Path)
		if err != nil {
			return nil, err
		}
		if observed.Exists && observed.Digest == file.Digest && observed.Mode == file.Mode {
			excluded[filepath.Join(configHome, filepath.FromSlash(file.Path))] = true
		}
	}
	return excluded, nil
}

func candidateYardNames(configHome string, source sourceSnapshot, previous Manifest) ([]string, error) {
	seen := map[string]bool{}
	for _, name := range source.yardNames {
		seen[name] = true
	}
	excluded, err := legacyLocalExclusions(configHome, previous)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(configHome, "yards"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() {
			if strings.HasSuffix(name, ".env") {
				return nil, fmt.Errorf("legacy yard input %s must be migrated before versioned sync", name)
			}
			continue
		}
		if !domain.SafeName(name) {
			return nil, fmt.Errorf("invalid live yard directory %q", name)
		}
		relative := filepath.ToSlash(filepath.Join("yards", name, "config.env"))
		observed, err := observeLocalInput(configHome, relative)
		if err != nil {
			return nil, err
		}
		if name != "default" && observed.Exists && !excluded[filepath.Join(configHome, filepath.FromSlash(relative))] {
			seen[name] = true
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func observeLocalInputs(configHome string, source sourceSnapshot, previous Manifest) ([]localInputObservation, error) {
	paths := map[string]bool{"config.env": true, "overrides/shared/config.env": true, "yards/default/config.env": true}
	paths[filepath.Base(config.YardResetMarkerPath(configHome, "default"))] = true
	yards, err := candidateYardNames(configHome, source, previous)
	if err != nil {
		return nil, err
	}
	for _, name := range yards {
		paths[filepath.ToSlash(filepath.Join("yards", name, "config.env"))] = true
		paths[filepath.Base(config.YardResetMarkerPath(configHome, name))] = true
	}
	for path := range source.files {
		paths[localSettingsPath(path)] = true
	}
	for _, file := range previous.Files {
		paths[localSettingsPath(file.Path)] = true
	}
	assetRoots := []string{"overrides/shared/agents", "overrides/host/agents", "yards/default/overrides/agents"}
	for path := range paths {
		if name, definition := deletedYardDefinition(path); definition {
			assetRoots = append(assetRoots, filepath.ToSlash(filepath.Join("yards", name, "overrides", "agents")))
		}
	}
	for _, root := range assetRoots {
		for _, mapping := range source.fileSettings {
			paths[filepath.ToSlash(filepath.Join(root, mapping.Relative))] = true
		}
	}
	names := make([]string, 0, len(paths))
	for path := range paths {
		names = append(names, path)
	}
	sort.Strings(names)
	observations := make([]localInputObservation, 0, len(names))
	for _, path := range names {
		observed, err := observeLocalInput(configHome, path)
		if err != nil {
			return nil, err
		}
		observations = append(observations, observed)
	}
	return observations, nil
}

func guardDeletedGitYard(options Options, source sourceSnapshot, previous Manifest, path string) error {
	name, definition := deletedYardDefinition(path)
	if !definition || options.YardInUse == nil {
		return nil
	}
	localPath := filepath.ToSlash(filepath.Join("yards", name, "config.env"))
	if _, present := source.files[gitSettingsPath(localPath)]; present {
		return nil
	}
	local, err := observeLocalInput(options.ConfigHome, localPath)
	if err != nil {
		return err
	}
	excluded, err := legacyLocalExclusions(options.ConfigHome, previous)
	if err != nil {
		return err
	}
	if local.Exists && !excluded[filepath.Join(options.ConfigHome, filepath.FromSlash(localPath))] {
		return nil
	}
	return guardDeletedYardDefinition(options, path)
}

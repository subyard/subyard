package profile

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
)

// CleanupCaches declares only disposable directories. CACHES keeps its existing
// project-environment sharing semantics, including installed assets. Profiles
// reference the same configured paths here only when deletion is safe.
func CleanupCaches(value, developer string) ([]string, error) {
	var paths []string
	for _, path := range strings.Fields(value) {
		if err := ValidateCachePath(path, developer); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, nil
}

func ValidateCachePath(path, developer string) error {
	if filepath.Clean(path) != path || !filepath.IsAbs(path) || strings.ContainsAny(path, ":,\x00\r\n\t ") {
		return fmt.Errorf("cache requires a clean absolute directory path: %q", path)
	}
	for _, component := range strings.Split(path, "/") {
		switch component {
		case ".git", ".ssh", ".codex", ".claude", ".agents", ".config", ".subyard-runtime", "credentials", "workspaces", "auth.json":
			return fmt.Errorf("cache overlaps protected data")
		}
	}
	if strings.HasPrefix(path, "/srv/cache/") {
		return domain.ValidateProjectCachePath(path)
	}
	if developer == "" || filepath.Base(developer) != developer || developer == "." || developer == ".." {
		return fmt.Errorf("cache requires a valid development account")
	}
	home := "/home/" + developer
	if !strings.HasPrefix(path, home+"/") || filepath.Dir(path) == home {
		return fmt.Errorf("cache must be below /srv/cache or a private developer subdirectory")
	}
	for _, component := range strings.Split(strings.TrimPrefix(path, home+"/"), "/") {
		switch component {
		case ".git", ".ssh", ".codex", ".claude", ".agents", ".config", ".subyard-runtime", "credentials", "workspaces", "auth.json":
			return fmt.Errorf("cache overlaps protected developer data")
		}
	}
	if strings.HasPrefix(path, home+"/.local/state/") || path == home+"/.local/state" {
		return fmt.Errorf("cache overlaps persistent developer state")
	}
	return nil
}

package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
)

// YardNames returns the configured yard names without evaluating their files.
// The default context is always first; duplicate private/installed entries collapse.
func YardNames(configDir, configHome string) ([]string, error) {
	seen := map[string]struct{}{}
	for _, root := range []struct {
		directory   string
		allowNested bool
	}{
		{directory: filepath.Join(configDir, "..", "private", "yards")},
		{directory: filepath.Join(configHome, "yards"), allowNested: true},
	} {
		entries, err := os.ReadDir(root.directory)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			name := ""
			if root.allowNested && entry.IsDir() {
				name = entry.Name()
			} else if strings.HasSuffix(entry.Name(), ".env") {
				name = strings.TrimSuffix(entry.Name(), ".env")
			}
			if name == "" || !domain.SafeName(name) {
				continue
			}
			seen[name] = struct{}{}
		}
	}
	discovered := make([]string, 0, len(seen))
	for name := range seen {
		if name == "default" {
			continue
		}
		discovered = append(discovered, name)
	}
	sort.Strings(discovered)
	names := []string{"default"}
	for _, name := range discovered {
		if _, err := FindYardRegistrationFile(configDir, configHome, name); err != nil {
			if errors.Is(err, ErrUnknownYard) {
				continue
			}
			return nil, err
		}
		names = append(names, name)
	}
	return names, nil
}

func YardFileCandidates(configDir, configHome, name string) []string {
	return []string{
		filepath.Join(configHome, "yards", name, "config.env"),
		filepath.Join(configDir, "..", "private", "yards", name+".env"),
		filepath.Join(configHome, "yards", name+".env"),
	}
}

// FindYardRegistrationFile returns the highest-precedence regular,
// non-symlink definition for a named yard.
func FindYardRegistrationFile(configDir, configHome, name string) (string, error) {
	if name == "default" || !domain.SafeName(name) {
		return "", fmt.Errorf("%w %q", ErrUnknownYard, name)
	}
	return findFirstYardFile(YardFileCandidates(configDir, configHome, name), name)
}

func findFirstYardFile(candidates []string, name string) (string, error) {
	for _, candidate := range candidates {
		if filepath.Base(candidate) == "config.env" {
			info, err := os.Lstat(filepath.Dir(candidate))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return "", err
			}
			if !info.Mode().IsDir() {
				continue
			}
		}
		info, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		return candidate, nil
	}
	return "", fmt.Errorf("%w %q", ErrUnknownYard, name)
}

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
	return discoverYardNames(configDir, configHome, false)
}

// LocalBatchYardNames includes the default and any other discoverable yards
// even when a registration or enumeration fails.
func LocalBatchYardNames(configDir, configHome string) ([]string, error) {
	return discoverYardNames(configDir, configHome, true)
}

func discoverYardNames(configDir, configHome string, collect bool) ([]string, error) {
	seen := map[string]struct{}{}
	var refusals []error
	for _, root := range []struct {
		directory   string
		allowNested bool
	}{
		{directory: filepath.Join(configDir, "..", "private", "yards")},
		{directory: filepath.Join(configHome, "yards"), allowNested: true},
		{directory: filepath.Join(configHome, GitSettingsRelativePath, "yards"), allowNested: true},
	} {
		entries, err := os.ReadDir(root.directory)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			if !collect {
				return nil, err
			}
			refusals = append(refusals, err)
			continue
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
	_, defaultErr := LocalYardSettingsFile(filepath.Join(configHome, "yards"), "default")
	refusals = append(refusals, defaultErr)
	for _, name := range discovered {
		if _, err := FindYardRegistrationFile(configDir, configHome, name); err != nil {
			if errors.Is(err, ErrUnknownYard) {
				continue
			}
			refusals = append(refusals, err)
			continue
		}
		names = append(names, name)
	}
	return names, domain.CollectRefusals(refusals...)
}

func YardFileCandidates(configDir, configHome, name string) []string {
	return []string{
		filepath.Join(configHome, "yards", name, "config.env"),
		filepath.Join(configDir, "..", "private", "yards", name+".env"),
		filepath.Join(configHome, "yards", name+".env"),
	}
}

// SettingsFileConflict names the two files competing for a single local scope.
type SettingsFileConflict struct {
	First, Second           string
	firstLabel, secondLabel string
}

func (err *SettingsFileConflict) Error() string {
	return fmt.Sprintf("both %s and %s exist. Merge their settings into one file and remove the other", err.firstLabel, err.secondLabel)
}

func pathExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, sourceAccessError(path, "scalar settings", err)
	}
	return true, nil
}

func selectSettingsFile(first, second, firstLabel, secondLabel string, exists func(string) (bool, error)) (string, error) {
	firstExists, firstErr := exists(first)
	secondExists, secondErr := exists(second)
	if err := domain.CollectRefusals(firstErr, secondErr); err != nil {
		return "", err
	}
	if firstExists && secondExists {
		return "", &SettingsFileConflict{first, second, firstLabel, secondLabel}
	}
	if secondExists {
		return second, nil
	}
	return first, nil
}

// LocalHostSettingsFile returns the existing host source or the path to create.
func LocalHostSettingsFile(configDir, configHome string) (string, error) {
	return localHostSettingsFile(configDir, configHome, pathExists)
}

func localHostSettingsFile(configDir, configHome string, exists func(string) (bool, error)) (string, error) {
	local := filepath.Join(configHome, "config.env")
	if configDir == "" {
		return local, nil
	}
	private := filepath.Join(configDir, "..", "private", "config.env")
	if private == local {
		return local, nil
	}
	return selectSettingsFile(local, private, "config.env", "private/config.env", exists)
}

// LocalYardSettingsFile returns the existing config-home source or the path to
// create. A prospective caller can supply its view of file existence.
func LocalYardSettingsFile(yardsDir, name string, readers ...func(string) (bool, error)) (string, error) {
	exists := pathExists
	if len(readers) > 0 {
		exists = readers[0]
	}
	return selectSettingsFile(filepath.Join(yardsDir, name, "config.env"), filepath.Join(yardsDir, name+".env"),
		"yards/"+name+"/config.env", "yards/"+name+".env", exists)
}

// FindYardRegistrationFile returns the highest-precedence regular,
// non-symlink definition for a named yard.
func FindYardRegistrationFile(configDir, configHome, name string) (string, error) {
	if name == "default" || !domain.SafeName(name) {
		return "", fmt.Errorf("%w %q", ErrUnknownYard, name)
	}
	reset, err := YardFallbackReset(configHome, name)
	if err != nil {
		return "", err
	}
	local, err := LocalYardSettingsFile(filepath.Join(configHome, "yards"), name)
	if err != nil {
		return "", err
	}
	return findFirstYardFile(yardRegistrationCandidates(configDir, configHome, name, local, reset), name)
}

func yardRegistrationCandidates(configDir, configHome, name, local string, reset bool) []string {
	candidates := []string{local}
	if !reset {
		if configDir != "" {
			private := filepath.Join(configDir, "..", "private", "yards", name+".env")
			// Preserve the repository fallback's precedence between the
			// supported config-home layouts.
			if filepath.Base(local) == name+".env" {
				candidates = []string{private, local}
			} else {
				candidates = append(candidates, private)
			}
		}
		candidates = append(candidates, filepath.Join(configHome, GitSettingsRelativePath, "yards", name, "config.env"))
	}
	return candidates
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

// NativeTargetCollisions is pure. Marker owners may be supplied from an existing
// instance read, keyed by project/instance; assessment never reads Incus itself.
func NativeTargetCollisions(yards []Loaded, owners map[string]string, selected string) error {
	ordered := append([]Loaded(nil), yards...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Context.YardName < ordered[j].Context.YardName })
	var findings []error
	for i, x := range ordered {
		if x.Context.AccessKind == domain.AccessRemote {
			continue
		}
		for _, y := range ordered[i+1:] {
			if y.Context.AccessKind == domain.AccessRemote || x.Context.YardName == y.Context.YardName || selected != "" && x.Context.YardName != selected && y.Context.YardName != selected {
				continue
			}
			project := nativeTargetValue(x, "INCUS_PROJECT")
			sameProject := project != "" && project == nativeTargetValue(y, "INCUS_PROJECT")
			instance, volume := nativeTargetValue(x, "YARD_INSTANCE_NAME"), nativeTargetValue(x, "SRV_VOLUME")
			firstOwner := owners[project+"/"+instance]
			owner := firstOwner
			otherOwner := owners[nativeTargetValue(y, "INCUS_PROJECT")+"/"+nativeTargetValue(y, "YARD_INSTANCE_NAME")]
			if owner == "" {
				owner = otherOwner
			} else if otherOwner != "" && owner != otherOwner {
				owner = ""
			}
			repair := fmt.Sprintf("one of %s or %s (the non-owner)", x.Context.YardName, y.Context.YardName)
			if owner == x.Context.YardName {
				repair = y.Context.YardName
			} else if owner == y.Context.YardName {
				repair = x.Context.YardName
			}
			for _, kind := range []struct {
				shared                      bool
				resource, value, actionKeys string
				keys                        []string
			}{
				{sameProject && instance != "" && instance == nativeTargetValue(y, "YARD_INSTANCE_NAME"), "native target", project + "/" + instance, "INCUS_PROJECT or YARD_INSTANCE_NAME", []string{"INCUS_PROJECT", "YARD_INSTANCE_NAME"}},
				{sameProject && volume != "" && volume == nativeTargetValue(y, "SRV_VOLUME"), "/srv volume", project + "/" + volume, "INCUS_PROJECT or SRV_VOLUME", []string{"INCUS_PROJECT", "SRV_VOLUME"}},
			} {
				if !kind.shared {
					continue
				}
				message := fmt.Sprintf("yards %s and %s share %s %s", x.Context.YardName, y.Context.YardName, kind.resource, kind.value)
				for _, key := range kind.keys {
					for _, yard := range []Loaded{x, y} {
						message += fmt.Sprintf("; %s %s=%q from %s", yard.Context.YardName, key, nativeTargetValue(yard, key), nativeTargetSource(yard, key))
					}
				}
				if firstOwner != "" {
					message += "; " + x.Context.YardName + " owner by user.subyard.name: " + domain.MigrationDiagnosticValue(firstOwner)
				}
				if otherOwner != "" && otherOwner != firstOwner {
					message += "; " + y.Context.YardName + " owner by user.subyard.name: " + domain.MigrationDiagnosticValue(otherOwner)
				}
				action := fmt.Sprintf("set %s at yard scope for %s", kind.actionKeys, repair)
				if kind.resource == "/srv volume" {
					action = "separate the data on the shared volume first, then " + action
				}
				summary := fmt.Sprintf("yards %s and %s share %s (%s). %s", x.Context.YardName, y.Context.YardName, kind.resource, strings.Join(kind.keys, "/"), action)
				findings = append(findings, &domain.NativeTargetRefusal{Message: message, Summary: summary, Action: action})
			}
		}
	}
	return domain.CollectRefusals(findings...)
}

func nativeTargetValue(yard Loaded, key string) string {
	switch key {
	case "INCUS_PROJECT":
		if yard.Context.IncusProject != "" {
			return yard.Context.IncusProject
		}
	case "YARD_INSTANCE_NAME":
		if yard.Context.YardInstanceName != "" {
			return yard.Context.YardInstanceName
		}
	}
	return yard.Environment[key]
}

func nativeTargetSource(yard Loaded, key string) string {
	for _, resolution := range yard.Settings[key].Resolutions {
		if resolution.Status != "effective" {
			continue
		}
		path := resolution.Path
		if resolution.Scope == "default" {
			path = "shipped defaults"
		} else if filepath.IsAbs(path) {
			relative, err := filepath.Rel(yard.Context.Paths.ConfigHome, path)
			if yard.Context.Paths.ConfigHome != "" && err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				path = filepath.ToSlash(relative)
			} else {
				path = "configuration source outside config home"
			}
		}
		return fmt.Sprintf("%q (%s scope)", path, resolution.Scope)
	}
	return "resolver default"
}

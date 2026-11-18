package reconcileruntime

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
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

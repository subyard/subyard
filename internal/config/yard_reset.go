package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Subyard/Subyard/internal/domain"
	"golang.org/x/sys/unix"
)

const yardResetMarkerContent = "subyard-yard-reset-v1\n"

// YardResetMarkerPath is local ownership state, outside the removed yard scope.
func YardResetMarkerPath(configHome, name string) string {
	return filepath.Join(configHome, ".yard-reset-"+name)
}

// YardFallbackReset suppresses only nonlocal yard settings, never host/shared layers.
// A subsequent local registration intentionally keeps this ownership boundary.
func YardFallbackReset(configHome, name string) (bool, error) {
	if !domain.SafeName(name) || !filepath.IsAbs(configHome) {
		return false, errors.New("yard reset requires an absolute configuration root and safe yard name")
	}
	if _, err := os.Lstat(configHome); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	marker, err := ReadPersistentFileSnapshot(configHome, YardResetMarkerPath(configHome, name))
	if err != nil {
		return false, err
	}
	if !marker.Exists {
		return false, nil
	}
	if marker.Identity.Mode&0o777 != 0o600 || !bytes.Equal(marker.Content, []byte(yardResetMarkerContent)) {
		return false, errors.New("yard reset ownership marker is malformed or has unsafe permissions")
	}
	return true, nil
}

// ResetYardConfiguration serializes the last metadata check, ownership marker
// publication and exact local removal with other persistent config writers.
func ResetYardConfiguration(configHome, name string, check, removeOverrides, removeRegistrations func() error) error {
	if !domain.SafeName(name) || !filepath.IsAbs(configHome) {
		return errors.New("unsafe yard configuration reset target")
	}
	if err := ensurePersistentRoot(configHome); err != nil {
		return err
	}
	root, err := unix.Open(configHome, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(root)
	if err := validatePersistentDirectoryFD(root); err != nil {
		return err
	}
	if err := unix.Flock(root, unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(root, unix.LOCK_UN)
	if err := check(); err != nil {
		return err
	}
	if err := removeOverrides(); err != nil {
		return err
	}
	reset, err := YardFallbackReset(configHome, name)
	if err != nil {
		return err
	}
	if !reset {
		marker := filepath.Base(YardResetMarkerPath(configHome, name))
		pending := marker + ".pending"
		if err := preparePersistentPendingAt(root, pending, []byte(yardResetMarkerContent)); err != nil {
			return err
		}
		if err := unix.Renameat2(root, pending, root, marker, unix.RENAME_NOREPLACE); err != nil {
			return err
		}
		if err := unix.Fsync(root); err != nil {
			return err
		}
	}
	return removeRegistrations()
}

// YardResetPaths enumerates local selected-yard entries only. Project state is
// owned by the physical teardown boundary and verified before config removal.
func YardResetPaths(configHome, name string) ([]string, error) {
	if _, err := YardFallbackReset(configHome, name); err != nil {
		return nil, err
	}
	directory := filepath.Join(configHome, "yards", name)
	for _, path := range []string{configHome, filepath.Join(configHome, "yards"), directory} {
		if err := validatePersistentDirectory(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	paths := []string{filepath.Join(configHome, "yards", name+".env")}
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return paths, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect local yard reset scope: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() != "projects" {
			paths = append(paths, filepath.Join(directory, entry.Name()))
		}
	}
	return paths, nil
}

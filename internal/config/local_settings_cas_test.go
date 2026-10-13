package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestLocalSettingsCASCreatesExactPrivateModesAndResumesUnpublishedCreation(t *testing.T) {
	for _, boundary := range []string{"after-parent-create", "after-pending-fsync"} {
		t.Run(boundary, func(t *testing.T) {
			root := testkit.TempDir(t)
			path := filepath.Join(root, "yards/default/config.env")
			content := []byte("SSH_PORT='2240'\n")
			interrupted := errors.New("synthetic creation interruption")
			err := compareAndSwapPersistentFileGuarded(root, path, PersistentFileSnapshot{}, content, nil, func(point string) error {
				if point == boundary {
					return interrupted
				}
				return nil
			}, true)
			if !errors.Is(err, interrupted) {
				t.Fatalf("creation did not interrupt: %v", err)
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unpublished creation wrote target")
			}
			if err := CompareAndSwapLocalSettingsFile(root, path, PersistentFileSnapshot{}, content); err != nil {
				t.Fatal(err)
			}
			for name, mode := range map[string]os.FileMode{"yards": 0o700, "yards/default": 0o700, "yards/default/config.env": 0o600} {
				info, err := os.Stat(filepath.Join(root, name))
				if err != nil || info.Mode().Perm() != mode {
					t.Fatalf("exact created mode changed: %s %v", name, err)
				}
			}
		})
	}
}

func TestLocalSettingsCASPreservesModeAndRefusesConcurrentFileOrSymlink(t *testing.T) {
	for _, adversary := range []string{"file", "symlink"} {
		t.Run(adversary, func(t *testing.T) {
			root := testkit.TempDir(t)
			path := filepath.Join(root, "yards/default/config.env")
			foreign := filepath.Join(root, "foreign.env")
			testkit.WriteFile(t, foreign, []byte("foreign\n"), 0o600)
			err := compareAndSwapPersistentFileGuarded(root, path, PersistentFileSnapshot{}, []byte("SSH_PORT=2240\n"), nil, func(point string) error {
				if point == "before-publish" {
					if adversary == "symlink" {
						return os.Symlink(foreign, path)
					}
					testkit.WriteFile(t, path, []byte("concurrent\n"), 0o600)
				}
				return nil
			}, true)
			if !errors.Is(err, ErrPersistentTargetStale) {
				t.Fatalf("concurrent target admitted: %v", err)
			}
			content, _ := os.ReadFile(foreign)
			if string(content) != "foreign\n" {
				t.Fatal("external file changed")
			}
		})
	}
	root := testkit.TempDir(t)
	path := filepath.Join(root, "config.env")
	testkit.WriteFile(t, path, []byte("# local\nDEV_SUDO=0\n"), 0o640)
	before, err := ReadPersistentFileSnapshot(root, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareAndSwapLocalSettingsFile(root, path, before, append(before.Content, []byte("SSH_PORT=2240\n")...)); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o640 {
		t.Fatal("existing local mode changed")
	}
}

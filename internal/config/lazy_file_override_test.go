package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestLoadLazyPreparedInputPreservesNativeFallbackAndLogicalTrace(t *testing.T) {
	root := testkit.TempDir(t)
	home := filepath.Join(root, "home")
	shipped := filepath.Join(root, "config")
	for _, path := range []string{home, shipped} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeFixture(t, filepath.Join(shipped, "incus.project.env"), "INCUS_PROJECT=subyard\n")
	writeFixture(t, filepath.Join(shipped, "subyard.env"), "YARD_INSTANCE_NAME=yard\nYARD_KIND=container\nSHIFT_MODE=shift\nFORWARD_SSH_AGENT=0\nDEV_SUDO=0\nDEV_UID=1000\nSSH_PORT=2222\n")
	options := LoadOptions{RepositoryRoot: root, OperatorHome: home, DisablePrivate: true, Environment: map[string]string{"SUBYARD_CONFIG_HOME": home, "SUBYARD_HOME": filepath.Join(root, "data"), "STORAGE_PATH": filepath.Join(root, "storage"), "HOST_BASE": root, "RESTRICTED_DISK_PATHS": root}}
	native, err := Load(options)
	if err != nil {
		t.Fatal(err)
	}
	options.ProspectiveFiles = &ProspectiveFiles{Root: home, ReadSnapshot: func(path string) (PersistentFileSnapshot, error) { return ReadPersistentFileSnapshot(home, path) }, ReadPreparedInput: func(string) (PersistentFileSnapshot, bool, error) { return PersistentFileSnapshot{}, false, nil }}
	fallback, err := Load(options)
	if err != nil || !reflect.DeepEqual(native.Context, fallback.Context) || !reflect.DeepEqual(native.Settings, fallback.Settings) {
		t.Fatal("unmatched resolver changed native configuration", err)
	}
	logical := filepath.Join(home, "yards", "named", "config.env")
	options.YardName = "named"
	options.FileOverrides = map[string]string{logical: logical}
	options.ProspectiveFiles.ReadPreparedInput = func(path string) (PersistentFileSnapshot, bool, error) {
		return PersistentFileSnapshot{Exists: true, Content: []byte("SSH_PORT=3333\n")}, path == logical, nil
	}
	loaded, err := Load(options)
	if err != nil || loaded.Context.SSHPort != 3333 {
		t.Fatal("requested draft was not loaded", err)
	}
	assertEffectiveSetting(t, loaded.Settings["SSH_PORT"], "3333", "yard", "scalar settings", logical)
	if _, err := os.Stat(logical); !os.IsNotExist(err) {
		t.Fatal("retained draft published its logical target", err)
	}
	failure := errors.New("captured requested input unavailable")
	options.ProspectiveFiles.ReadPreparedInput = func(path string) (PersistentFileSnapshot, bool, error) {
		if path == logical {
			return PersistentFileSnapshot{}, true, failure
		}
		return PersistentFileSnapshot{}, false, nil
	}
	if _, err := Load(options); !errors.Is(err, failure) {
		t.Fatal("requested-input failure was hidden by native fallback", err)
	}
}

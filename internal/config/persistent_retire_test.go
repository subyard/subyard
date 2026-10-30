package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestRemovePersistentFileSnapshotPreservesConcurrentReplacement(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "exact", true: "replaced"}[changed], func(t *testing.T) {
			root := testkit.TempDir(t)
			path := filepath.Join(root, "record.json")
			testkit.WriteFile(t, path, []byte("original"), 0600)
			before, err := ReadPersistentFileSnapshot(root, path)
			if err != nil {
				t.Fatal(err)
			}
			if changed {
				if err := CompareAndSwapLocalSettingsFile(root, path, before, []byte("replacement")); err != nil {
					t.Fatal(err)
				}
			}
			err = RemovePersistentFileSnapshot(root, path, before)
			if changed {
				if !errors.Is(err, ErrPersistentTargetStale) {
					t.Fatal(err)
				}
				payload, _ := os.ReadFile(path)
				if string(payload) != "replacement" {
					t.Fatal("replacement retired")
				}
			} else if err != nil {
				t.Fatal(err)
			} else if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("exact record survived", err)
			}
		})
	}
}

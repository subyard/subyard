package testvms_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/testkit"
)

// Release admission selects the broker yard only through its own settings.
// Recreation never targets a yard it did not admit, so each state below is
// either a definite absence or a failed read; none of them is followed,
// loaded or changed.
func TestServiceRemovedRuntimeIsAbsentForReleaseAdmission(t *testing.T) {
	for _, state := range []string{"reset-empty", "unmarked-empty", "reset-unrecognized-file", "unsafe-marker", "symlink-with-settings", "unreadable-registry"} {
		t.Run(state, func(t *testing.T) {
			root := testkit.TempDir(t)
			home := filepath.Join(root, "config")
			yards := filepath.Join(home, "yards")
			directory := filepath.Join(yards, "test-yard")
			if err := os.MkdirAll(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			if state != "unmarked-empty" {
				if err := config.ResetYardConfiguration(home, "test-yard", func() error { return nil }, func() error { return nil }, func() error { return nil }); err != nil {
					t.Fatal(err)
				}
			}
			outside := testkit.TempDir(t)
			settings := []byte("YARD_TEMPLATE=test-vms\n")
			switch state {
			case "reset-unrecognized-file":
				testkit.WriteFile(t, filepath.Join(directory, "foreign"), []byte("preserve"), 0o600)
			case "unsafe-marker":
				if err := os.Chmod(config.YardResetMarkerPath(home, "test-yard"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "symlink-with-settings":
				// Settings reached only through a symlinked yard directory are not the broker's.
				testkit.WriteFile(t, filepath.Join(outside, "config.env"), settings, 0o600)
				if err := os.RemoveAll(directory); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, directory); err != nil {
					t.Fatal(err)
				}
			}
			before := treeSnapshot(t, root)
			if state == "unreadable-registry" {
				if os.Geteuid() == 0 {
					t.Skip("root reads a mode 0000 directory")
				}
				if err := os.Chmod(yards, 0o000); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(yards, 0o700) })
			}
			names, err := config.YardNames(home)
			switch state {
			case "unsafe-marker", "unreadable-registry":
				if err == nil {
					t.Fatalf("failed read admitted %v", names)
				}
				if state == "unreadable-registry" && !errors.Is(err, os.ErrPermission) {
					t.Fatalf("unreadable registry error: %v", err)
				}
			default:
				if err != nil || !reflect.DeepEqual(names, []string{"default"}) {
					t.Fatalf("absent broker admission: %v %v", names, err)
				}
			}
			if state == "unreadable-registry" {
				if err := os.Chmod(yards, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if after := treeSnapshot(t, root); !reflect.DeepEqual(before, after) {
				t.Fatalf("admission changed %s", state)
			}
			if state == "symlink-with-settings" {
				if body, err := os.ReadFile(filepath.Join(outside, "config.env")); err != nil || string(body) != string(settings) {
					t.Fatal("admission changed the symlink target", err)
				}
			}
		})
	}
}

// treeSnapshot records every entry below root without following symlinks.
func treeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	entries := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		entry := info.Mode().String()
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			entry += " -> " + target
		case info.Mode().IsRegular():
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			entry += " " + string(body)
		}
		entries[path] = entry
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

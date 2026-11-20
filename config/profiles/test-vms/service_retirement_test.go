package testvms_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/testkit"
)

// A removed service leaves no registration, so release admission never selects
// the broker yard for recreation or update; an unsafe reset marker is a failed
// read, and nothing in the directory is changed.
func TestServiceRemovedRuntimeIsAbsentForReleaseAdmission(t *testing.T) {
	for _, state := range []string{"reset-empty", "unmarked-empty", "reset-unrecognized-file", "unsafe-marker", "symlink"} {
		t.Run(state, func(t *testing.T) {
			root := testkit.TempDir(t)
			home := filepath.Join(root, "config")
			directory := filepath.Join(home, "yards/test-yard")
			if err := os.MkdirAll(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			if state != "unmarked-empty" {
				if err := config.ResetYardConfiguration(home, "test-yard", func() error { return nil }, func() error { return nil }, func() error { return nil }); err != nil {
					t.Fatal(err)
				}
			}
			switch state {
			case "reset-unrecognized-file":
				testkit.WriteFile(t, filepath.Join(directory, "foreign"), []byte("preserve"), 0o600)
			case "unsafe-marker":
				if err := os.Chmod(config.YardResetMarkerPath(home, "test-yard"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(directory); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(testkit.TempDir(t), directory); err != nil {
					t.Fatal(err)
				}
			}
			names, err := config.YardNames(home)
			if slices.Contains(names, "test-yard") {
				t.Fatalf("removed service runtime is admitted: %s %v", state, names)
			}
			if (state == "unsafe-marker") != (err != nil) {
				t.Fatalf("release admission read: %s %v", state, err)
			}
			if state == "reset-unrecognized-file" {
				if b, err := os.ReadFile(filepath.Join(directory, "foreign")); err != nil || string(b) != "preserve" {
					t.Fatal("observation changed foreign file")
				}
			}
		})
	}
}

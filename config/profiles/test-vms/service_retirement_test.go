package testvms_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/testyardmigration"
)

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
			options := testyardmigration.Options{Executable: "/bin/false", RepositoryRoot: root, ConfigHome: home, DataHome: filepath.Join(root, "data"), Incus: "/bin/false"}
			observed, yard, err := testyardmigration.PrepareBrokerRuntimeTarget(context.Background(), options)
			if state == "reset-empty" {
				if err != nil || observed != testyardmigration.BrokerRuntimeAbsent || yard != "" {
					t.Fatalf("retired owner admission: %s %s %v", observed, yard, err)
				}
			} else if err == nil {
				t.Fatalf("unproven directory became absent: %s", state)
			}
			if state == "reset-unrecognized-file" {
				if b, err := os.ReadFile(filepath.Join(directory, "foreign")); err != nil || string(b) != "preserve" {
					t.Fatal("observation changed foreign file")
				}
			}
		})
	}
}

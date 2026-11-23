package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestDeadAssignmentsRecoverIncompleteOrUnboundPending(t *testing.T) {
	for _, pendingContent := range []string{"", "# ke", "# keep\n"} {
		t.Run("pending="+pendingContent, func(t *testing.T) {
			root, home, catalog, path := deadAssignmentFixture(t)
			original := []byte("# keep\nAGENT_sample_CONFIG=\"$SUBYARD_CONFIG_DIR/../private/agents/sample/settings.json\"\n")
			testkit.WriteFile(t, path, original, 0640)
			// Simulate a process that created staging and stopped before its payload
			// or final metadata was complete. The old target is still authoritative.
			pending := filepath.Join(home, ".config.env.release-transition.pending")
			testkit.WriteFile(t, pending, []byte(pendingContent), 0600)
			fresh, err := catalog.PlanDeadAssignments(home, filepath.Join(root, "config"), nil)
			if err != nil || len(fresh.Removed) != 1 {
				t.Fatalf("fresh plan=%+v err=%v", fresh, err)
			}
			current, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(current, original) {
				t.Fatal("recovery preview changed target", err)
			}
			if err := fresh.Apply(home); err != nil {
				t.Fatal(err)
			}
			after, err := ReadPersistentFileSnapshot(home, path)
			if err != nil || string(after.Content) != "# keep\n" || after.Identity.Mode != fresh.Expected.Identity.Mode || after.Identity.UID != fresh.Expected.Identity.UID || after.Identity.GID != fresh.Expected.Identity.GID {
				t.Fatalf("recovery=%+v err=%v", after, err)
			}
			if _, err := os.Lstat(pending); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("published staging remains: %v", err)
			}
		})
	}
}

func TestPersistentPendingRecoveryKeepsOtherChangesStale(t *testing.T) {
	for _, change := range []string{"other staging payload", "concurrent target"} {
		t.Run(change, func(t *testing.T) {
			root := testkit.TempDir(t)
			path := filepath.Join(root, "config.env")
			pending := filepath.Join(root, ".config.env.release-transition.pending")
			original := []byte("DEV_SUDO=no\n")
			desired := []byte("DEV_SUDO=prompt\n")
			testkit.WriteFile(t, path, original, 0640)
			before, err := ReadPersistentFileSnapshot(root, path)
			if err != nil {
				t.Fatal(err)
			}
			staging := []byte("UNRELATED=1\n")
			if change == "concurrent target" {
				staging = desired[:4]
				testkit.WriteFile(t, path, []byte("DEV_SUDO=yes\n"), 0640)
			}
			testkit.WriteFile(t, pending, staging, 0600)
			current, _ := os.ReadFile(path)
			if err := CompareAndSwapLocalSettingsFile(root, path, before, desired); !errors.Is(err, ErrPersistentTargetStale) {
				t.Fatalf("other change not stale: %v", err)
			}
			after, _ := os.ReadFile(path)
			retained, _ := os.ReadFile(pending)
			if !bytes.Equal(after, current) || !bytes.Equal(retained, staging) {
				t.Fatal("stale recovery changed a target or another staging payload")
			}
		})
	}
}

func TestLocalSettingsCASRecoversIncompleteCreation(t *testing.T) {
	root := testkit.TempDir(t)
	path := filepath.Join(root, "config.env")
	desired := []byte("DEV_SUDO=prompt\n")
	testkit.WriteFile(t, filepath.Join(root, ".config.env.release-transition.pending"), desired[:4], 0600)
	if err := CompareAndSwapLocalSettingsFile(root, path, PersistentFileSnapshot{}, desired); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, desired) {
		t.Fatalf("creation=%q err=%v", after, err)
	}
}

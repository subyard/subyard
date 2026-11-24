package uninstall

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestArtifactGuardOutcomesGiveReasons(t *testing.T) {
	assertStale := func(t *testing.T, mode, reason string, err error) {
		t.Helper()
		if !errors.Is(err, domain.ErrPlanStale) || !strings.Contains(err.Error(), mode+": "+reason) {
			t.Fatalf("%s: got %v, want stale with reason %q", mode, err, reason)
		}
	}
	base := testkit.TempDir(t)
	owned := filepath.Join(base, "owned")
	if err := os.Mkdir(owned, 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(owned, "data"), []byte("approved"), 0600)
	tree, err := Capture(owned)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(owned, "added"), []byte("foreign"), 0600)
	assertStale(t, "check-tree", "installation artifact changed after confirmation", tree.Check())
	assertStale(t, "remove-tree", "installation artifact changed after confirmation", tree.Remove())
	assertStale(t, "verify-tree", "approved installation artifact remains", tree.VerifyRemoved())
	if _, err := os.Stat(filepath.Join(owned, "added")); err != nil {
		t.Fatal("stale removal deleted foreign content", err)
	}

	store, plan := recoveryFixture(t)
	first, err := store.Save(plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(plan, &first); err != nil {
		t.Fatal(err)
	}
	_, err = store.Save(plan, &first)
	assertStale(t, "publish-plan", "installation artifact changed after confirmation", err)

	if err := os.Symlink(base, filepath.Join(base, "alias")); err != nil {
		t.Fatal(err)
	}
	_, err = CaptureArtifact(filepath.Join(base, "alias", "owned"))
	if err == nil || !strings.Contains(err.Error(), "capture-artifact: not a directory") || strings.Contains(err.Error(), base) {
		t.Fatalf("capture-artifact lacked a safe failure reason: %v", err)
	}
	foreign := plan
	foreign.Installation.OperatorUID++
	err = store.PublishRuntime(foreign)
	if err == nil || !strings.Contains(err.Error(), "retain-runtime: retained runtime must be copied by its owner") {
		t.Fatalf("retain-runtime lacked a failure reason: %v", err)
	}

	if os.Geteuid() == 0 {
		t.Log("permission outcomes need an unprivileged account")
		return
	}
	private := filepath.Join(base, "private")
	if err := os.MkdirAll(filepath.Join(private, "sealed"), 0700); err != nil {
		t.Fatal(err)
	}
	approved, err := Capture(private)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(private, "sealed"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(private, "sealed"), 0700) })
	if _, err = Capture(private); err != os.ErrPermission {
		t.Fatalf("capture-tree permission outcome changed: %v", err)
	}
	if _, err = CaptureArtifact(private); err != os.ErrPermission {
		t.Fatalf("capture-artifact permission outcome changed: %v", err)
	}
	assertStale(t, "check-tree", "permission denied", approved.Check())
	// A private capture gives the operator access to its own directories.
	repaired, err := CapturePrivate(private)
	if err != nil || repaired.Opaque || len(repaired.Entries) != 2 {
		t.Fatalf("private capture did not repair owner access: %+v %v", repaired, err)
	}
	if info, err := os.Stat(filepath.Join(private, "sealed")); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("sealed directory mode = %v (%v)", info.Mode(), err)
	}
	if err := repaired.Remove(); err != nil {
		t.Fatal(err)
	}
}

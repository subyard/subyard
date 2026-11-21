package uninstall

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

// The artifact guard keeps its three historical outcomes and texts: typed
// staleness, bare permission for capture modes, and an exit-status failure.
func TestArtifactGuardOutcomesKeepHistoricalTexts(t *testing.T) {
	const stale = "operation plan is stale: native installation artifact guard"
	assertStale := func(t *testing.T, mode string, err error) {
		t.Helper()
		if !errors.Is(err, domain.ErrPlanStale) || err.Error() != stale {
			t.Fatalf("%s: got %v, want %q", mode, err, stale)
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
	assertStale(t, "check-tree", tree.Check())
	assertStale(t, "remove-tree", tree.Remove())
	assertStale(t, "verify-tree", tree.VerifyRemoved())
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
	assertStale(t, "publish-plan", err)

	if err := os.Symlink(base, filepath.Join(base, "alias")); err != nil {
		t.Fatal(err)
	}
	_, err = CaptureArtifact(filepath.Join(base, "alias", "owned"))
	if err == nil || err.Error() != "native teardown artifact guard failed (capture-artifact): exit status 1" {
		t.Fatalf("capture-artifact failure text changed: %v", err)
	}
	foreign := plan
	foreign.Installation.OperatorUID++
	err = store.PublishRuntime(foreign)
	if err == nil || err.Error() != "native teardown artifact guard failed (retain-runtime): exit status 1" {
		t.Fatalf("retain-runtime failure text changed: %v", err)
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
	if opaque, err := CapturePrivate(private); err != nil || !opaque.Opaque || len(opaque.Entries) != 1 {
		t.Fatalf("private capture lost its opaque fallback: %+v %v", opaque, err)
	}
	if _, err = CaptureArtifact(private); err != os.ErrPermission {
		t.Fatalf("capture-artifact permission outcome changed: %v", err)
	}
	assertStale(t, "check-tree permission", approved.Check())
}

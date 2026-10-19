package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestNativeActivationProjectionRetainsOriginalScopeAndReadsActualAfterEffect(t *testing.T) {
	c, home := nativeActivationSourceFixture(t)
	root := c.context.RepositoryRoot
	profile, err := os.ReadFile(filepath.Join(repositoryRoot(t), "config/yards/profiles/test-vms.env"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "config/yards/profiles"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "config/yards/profiles/test-vms.env"), profile, 0o600)
	legacy := filepath.Join(home, "yards/e2e-yard/config.env")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, legacy, []byte("YARD_TEMPLATE=test-vms\nNESTED_E2E_VMS=0\n"), 0o600)
	if _, _, err := c.CaptureInventory(context.Background()); err != nil {
		t.Fatal(err)
	}
	originalNames := slices.Clone(c.yards)
	owner := newInterruptedRenameOwner(t, root, home)
	before, err := owner.Prepare(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	semantic := releaseowner.OwnerBefore{State: string(before.State), Registration: string(before.Registration), Overrides: string(before.Overrides), Controller: string(before.Controller), SharedImages: before.SharedImages}
	expected, err := c.cli.prepareSourceActivationDraft(home, releasetransition.V2SourceActivationView{Settings: c, OwnerRegistration: &before})
	if err != nil {
		t.Fatal(err)
	}
	names, err := c.ProjectActivation(context.Background(), semantic)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names, expected.names) || !slices.Equal(c.yards, originalNames) || slices.Contains(names, "e2e-yard") || !slices.Contains(names, "test-yard") {
		t.Fatal("activation projection changed source/settings inventory")
	}
	projected, err := c.loadActivation("test-yard")
	if err != nil || projected.Context.YardName != "test-yard" {
		t.Fatal("original projected registration unavailable", err)
	}
	initial, err := c.CaptureActivation(context.Background(), "core-guest-runtime", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CaptureActivation(context.Background(), "core-guest-runtime", true); err == nil {
		t.Fatal("virtual projection hid missing actual registration")
	}
	if err := owner.Commit(context.Background(), before); err != nil {
		t.Fatal(err)
	}
	fresh, err := c.CaptureActivation(context.Background(), "core-guest-runtime", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != len(initial) {
		t.Fatal("own rename changed approved activation target set")
	}
	for index, target := range initial {
		if fresh[index].Yard != target.Yard || fresh[index].Project != target.Project || fresh[index].Instance != target.Instance || fresh[index].Source != target.Source || fresh[index].Desired != target.Desired {
			t.Fatalf("own rename changed projected target %s", target.Yard)
		}
	}
	if err := os.MkdirAll(filepath.Join(home, "yards/new-yard"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(home, "yards/new-yard/config.env"), []byte("SSH_PORT=2239\n"), 0o600)
	fresh, err = c.CaptureActivation(context.Background(), "core-guest-runtime", true)
	if err != nil || len(fresh) != len(initial) {
		t.Fatal("recapture admitted a new yard", err)
	}
	// A new helper uses the original approved before, with the owning
	// projection's exact source→current fallback after the rename.
	c.activationDraft = nil
	c.activationOwner = nil
	c.inputs = map[string]config.PersistentFileSnapshot{}
	if _, err := c.ProjectActivation(context.Background(), semantic); err != nil {
		t.Fatal("original projection cannot reconstruct after rename", err)
	}
	testkit.WriteFile(t, filepath.Join(home, "yards/test-yard/config.env"), []byte("YARD_TEMPLATE=test-vms\nNESTED_E2E_VMS=0\nSSH_PORT=2249\n"), 0o600)
	c.inputs = map[string]config.PersistentFileSnapshot{}
	if _, err := c.ProjectActivation(context.Background(), semantic); err == nil {
		t.Fatal("changed original projection source admitted")
	}
}

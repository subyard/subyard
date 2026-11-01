package cli

import (
	"context"
	"fmt"
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
	c.catalog = &c.cli.catalog
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

func TestNativeFactoryCatalogRetainsRepositoryAndRereadsConfiguration(t *testing.T) {
	fixture, _ := nativeActivationSourceFixture(t)
	root := fixture.context.RepositoryRoot
	declaration := filepath.Join(root, "config/profiles/catalogprobe/profile.json")
	if err := os.MkdirAll(filepath.Dir(declaration), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, declaration, []byte(`{"schema_version":1,"settings":[{"name":"NATIVE_CATALOG_VALUE","type":"string","scopes":["shipped"],"application":"next-command","default":"retained"}]}`), 0o600)
	repository, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repository.Close() })
	context := fixture.context
	context.PhysicalRepositoryRoot = fmt.Sprintf("/proc/self/fd/%d", repository.Fd())
	context.Repository, err = releaseowner.IdentityOf(repository)
	if err != nil {
		t.Fatal(err)
	}
	component, err := newReleaseOwnerComponents(t.Context(), context, fixture.root)
	if err != nil {
		t.Fatal(err)
	}
	c := component.(*releaseOwnerComponents)
	t.Cleanup(func() { c.Close() })
	// All activity remains in this fixture, including host applicability reads.
	c.cli.env["SUBYARD_POWER_RECONCILER_PATH"] = filepath.Join(root, "absent-power")
	c.cli.env["SUBYARD_POWER_UNIT_PATH"] = filepath.Join(root, "absent-unit")
	if _, _, err := c.CaptureInventory(t.Context()); err != nil {
		t.Fatal(err)
	}
	// A logical path replacement must not substitute another repository for
	// either the retained catalog or subsequent shipped configuration reads.
	retained := root + "-retained"
	if err := os.Rename(root, retained); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "config/subyard.env"), []byte("SSH_PORT=2299\n"), 0o600)
	loaded, err := c.load("default")
	if err != nil || loaded.Context.SSHPort != 2222 {
		t.Fatal("logical replacement changed retained shipped inputs", err)
	}
	definition, ok := loaded.Catalog.LookupSetting("NATIVE_CATALOG_VALUE")
	if !ok || definition.Default != "retained" {
		t.Fatal("factory catalog was replaced by the logical repository")
	}
	// Catalog metadata is retained, but this unit boundary does not approve a
	// release: the Rust owner's independent final artifact phase rejects drift.
	testkit.WriteFile(t, filepath.Join(retained, "config/profiles/catalogprobe/profile.json"), []byte("invalid declaration\n"), 0o600)
	if _, err := config.LoadCatalog(context.PhysicalRepositoryRoot); err == nil {
		t.Fatal("catalog drift control did not change the shipped declaration")
	}
	if _, err := c.load("default"); err != nil {
		t.Fatal("native configuration reopened the retained catalog", err)
	}
	first, err := c.CaptureActivation(t.Context(), "host-power", false)
	if err != nil || len(first) != 1 || c.inputs["config.env"].Exists {
		t.Fatal("initial absent configuration was not captured", err)
	}
	if _, err := c.read("unrelated-owner.env"); err != nil {
		t.Fatal(err)
	}
	isolated, err := c.CaptureActivation(t.Context(), "host-power", false)
	if err != nil || len(isolated) != 1 || isolated[0].Source != first[0].Source {
		t.Fatal("catalog reuse retained another component's dependencies", err)
	}
	path := filepath.Join(retained, "state/config.env")
	testkit.WriteFile(t, path, []byte("SSH_PORT=2240\n"), 0o600)
	second, err := c.CaptureActivation(t.Context(), "host-power", true)
	if err != nil || len(second) != 1 || second[0].Source == first[0].Source {
		t.Fatal("catalog reuse hid a new actual configuration", err)
	}
	before := config.BindPersistentSnapshot(c.inputs["config.env"])
	replacement := path + ".replacement"
	testkit.WriteFile(t, replacement, []byte("SSH_PORT=2240\n"), 0o600)
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CaptureActivation(t.Context(), "host-power", true); err != nil {
		t.Fatal(err)
	}
	after := config.BindPersistentSnapshot(c.inputs["config.env"])
	if before.Identity == after.Identity || before.Digest != after.Digest || !after.Exists {
		t.Fatal("fresh configuration did not reread the replacement identity")
	}
	testkit.WriteFile(t, path, []byte("SSH_PORT=invalid\n"), 0o600)
	if _, err := c.CaptureActivation(t.Context(), "host-power", true); err == nil {
		t.Fatal("a previous successful load hid invalid fresh configuration")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	absent, err := c.CaptureActivation(t.Context(), "host-power", true)
	if err != nil || len(absent) != 1 || c.inputs["config.env"].Exists || absent[0].Source != first[0].Source {
		t.Fatal("fresh absence was hidden by retained loaded configuration", err)
	}
}

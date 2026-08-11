package configsync

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
)

func TestVersionedConfigSyncAppliesOnlyTypedSelectedHostSettings(t *testing.T) {
	fixture := newSyncFixture(t, "owner-a")
	fixture.writeSource("shared/config.env", "YARD_IMAGE=images:debian/13\n")
	fixture.writeSource("hosts/owner-a/config.env", "SSH_PORT=2233\n")
	fixture.writeSource("hosts/owner-a/yards/demo/config.env", "SSH_PORT=2234\n")
	fixture.writeSource(
		"hosts/owner-a/yards/demo/overrides/agents/codex/rules/repo.rules",
		"allow\n",
	)
	fixture.commit("initial")
	writeSyncTestFile(t, filepath.Join(fixture.configHome, "secrets", "sentinel"), "secret\n", 0o600)
	writeSyncTestFile(t, filepath.Join(fixture.configHome, "keys", "sentinel"), "ledger\n", 0o600)
	writeSyncTestFile(t, filepath.Join(fixture.configHome, "generated", "sentinel"), "consumer\n", 0o600)
	writeSyncTestFile(t, filepath.Join(fixture.configHome, "projects", "sentinel"), "state\n", 0o600)
	writeSyncTestFile(t, filepath.Join(fixture.configHome, "tools", "sentinel"), "tool\n", 0o600)
	writeSyncTestFile(t, filepath.Join(fixture.configHome, "desired_power"), "running\n", 0o600)
	dataHome := filepath.Join(fixture.operatorHome, ".subyard")
	for path, content := range map[string]string{
		"ssh/known_hosts": "trust\n", "logs/sentinel": "log\n",
		"exports/sentinel": "export\n", "storage/sentinel": "storage\n",
	} {
		writeSyncTestFile(t, filepath.Join(dataHome, path), content, 0o600)
	}

	plan, err := BuildPlan(fixture.options(false))
	if err != nil {
		t.Fatal(err)
	}
	if !plan.InitializeHostID || !plan.NeedsConfirmation() || len(plan.Changes) != 4 {
		t.Fatalf("unexpected initial plan: %#v", plan)
	}
	if _, err := os.Lstat(fixture.configHome + "/config.env"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only planning wrote live configuration: %v", err)
	}
	if err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	assertSyncTestFile(t, filepath.Join(fixture.configHome, "host-id"), "owner-a\n", 0o600)
	assertSyncTestFile(t, filepath.Join(fixture.configHome, "config.env"), "SSH_PORT=2233\n", 0o600)
	assertSyncTestFile(t,
		filepath.Join(fixture.configHome, "overrides", "shared", "config.env"),
		"YARD_IMAGE=images:debian/13\n", 0o600)
	assertSyncTestFile(t,
		filepath.Join(fixture.configHome, "yards", "demo", "config.env"),
		"SSH_PORT=2234\n", 0o600)
	assertSyncTestFile(t,
		filepath.Join(fixture.configHome, "yards", "demo", "overrides", "agents", "codex", "rules", "repo.rules"),
		"allow\n", 0o644)
	for _, sentinel := range []struct {
		path, content string
	}{
		{"secrets/sentinel", "secret\n"},
		{"keys/sentinel", "ledger\n"},
		{"generated/sentinel", "consumer\n"},
		{"projects/sentinel", "state\n"},
		{"tools/sentinel", "tool\n"},
		{"desired_power", "running\n"},
	} {
		assertSyncTestFile(t, filepath.Join(fixture.configHome, sentinel.path), sentinel.content, 0o600)
	}
	for path, content := range map[string]string{
		"ssh/known_hosts": "trust\n", "logs/sentinel": "log\n",
		"exports/sentinel": "export\n", "storage/sentinel": "storage\n",
	} {
		assertSyncTestFile(t, filepath.Join(dataHome, path), content, 0o600)
	}
	converged, err := BuildPlan(fixture.options(false))
	if err != nil {
		t.Fatal(err)
	}
	if converged.NeedsApply() || converged.NeedsConfirmation() {
		t.Fatalf("sync did not converge: %#v", converged)
	}
}

func TestVersionedConfigSyncAllowsOptionalSharedAndSelectedHostScopes(t *testing.T) {
	t.Run("manifest-only preserves unmanaged live settings", func(t *testing.T) {
		fixture := newSyncFixture(t, "owner-a")
		fixture.commit("manifest only")
		writeSyncTestFile(
			t, filepath.Join(fixture.configHome, "config.env"), "SSH_PORT=2299\n", 0o600,
		)

		plan, err := BuildPlan(fixture.options(false))
		if err != nil {
			t.Fatal(err)
		}
		if !plan.InitializeHostID || len(plan.Changes) != 0 {
			t.Fatalf("unexpected manifest-only plan: %#v", plan)
		}
		if err := Apply(plan); err != nil {
			t.Fatal(err)
		}
		assertSyncTestFile(
			t, filepath.Join(fixture.configHome, "config.env"), "SSH_PORT=2299\n", 0o600,
		)
		assertSyncTestFile(t, HostIDPath(fixture.configHome), "owner-a\n", 0o600)
	})

	t.Run("shared-only without hosts", func(t *testing.T) {
		fixture := newSyncFixture(t, "owner-a")
		fixture.writeSource("shared/config.env", "YARD_IMAGE=images:debian/13\n")
		fixture.commit("shared only")

		plan, err := BuildPlan(fixture.options(false))
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Changes) != 1 ||
			plan.Changes[0].Path != "overrides/shared/config.env" {
			t.Fatalf("unexpected shared-only plan: %#v", plan.Changes)
		}
		if err := Apply(plan); err != nil {
			t.Fatal(err)
		}
		assertSyncTestFile(
			t, filepath.Join(fixture.configHome, "overrides", "shared", "config.env"),
			"YARD_IMAGE=images:debian/13\n", 0o600,
		)
	})

	t.Run("host without scalar settings", func(t *testing.T) {
		fixture := newSyncFixture(t, "owner-a")
		fixture.writeSource(
			"hosts/owner-a/overrides/agents/codex/rules/repo.rules", "allow\n",
		)
		fixture.writeSource(
			"hosts/owner-a/yards/demo/config.env", "SSH_PORT=2234\n",
		)
		fixture.commit("host overlays without host config")

		plan, err := BuildPlan(fixture.options(false))
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Changes) != 2 {
			t.Fatalf("unexpected host overlay plan: %#v", plan.Changes)
		}
		if err := Apply(plan); err != nil {
			t.Fatal(err)
		}
		assertSyncTestFile(
			t,
			filepath.Join(
				fixture.configHome, "overrides", "host", "agents", "codex", "rules",
				"repo.rules",
			),
			"allow\n", 0o644,
		)
		assertSyncTestFile(
			t, filepath.Join(fixture.configHome, "yards", "demo", "config.env"),
			"SSH_PORT=2234\n", 0o600,
		)
	})

	t.Run("other host is neither applied nor inspected", func(t *testing.T) {
		fixture := newSyncFixture(t, "owner-a")
		fixture.writeSource("hosts/owner-b/config.env", "SSH_PORT=3233\n")
		fixture.commit("other host only")
		fixture.writeSource("hosts/owner-b/config.env", "SSH_PORT=3299\n")

		plan, err := BuildPlan(fixture.options(false))
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Changes) != 0 {
			t.Fatalf("other host entered selected plan: %#v", plan.Changes)
		}
		if err := Apply(plan); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(fixture.configHome, "config.env")); !errors.Is(
			err, os.ErrNotExist,
		) {
			t.Fatalf("other host settings reached live configuration: %v", err)
		}
	})
}

func TestVersionedConfigSyncKeepsOptionalScopeValidationFailClosed(t *testing.T) {
	for _, test := range []struct {
		name  string
		write func(*syncFixture)
		want  string
	}{
		{
			name: "selected host is a file",
			write: func(fixture *syncFixture) {
				fixture.writeSource("hosts/owner-a", "not a directory\n")
			},
			want: "real source directory",
		},
		{
			name: "selected host has an unknown child",
			write: func(fixture *syncFixture) {
				fixture.writeSource("hosts/owner-a/unknown.env", "SSH_PORT=2233\n")
			},
			want: "unexpected source path",
		},
		{
			name: "yard entry has no scalar definition",
			write: func(fixture *syncFixture) {
				fixture.writeSource(
					"hosts/owner-a/yards/demo/overrides/agents/codex/rules/repo.rules",
					"allow\n",
				)
			},
			want: "yard demo has no config.env definition",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSyncFixture(t, "owner-a")
			test.write(fixture)
			fixture.commit("malformed selected host")
			if _, err := BuildPlan(fixture.options(false)); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("wanted %q rejection, got %v", test.want, err)
			}
		})
	}

	t.Run("untracked selected host appearance", func(t *testing.T) {
		fixture := newSyncFixture(t, "owner-a")
		fixture.commit("manifest only")
		fixture.writeSource("hosts/owner-a/config.env", "SSH_PORT=2233\n")
		if _, err := BuildPlan(fixture.options(false)); err == nil ||
			!strings.Contains(err.Error(), "changes in managed paths") {
			t.Fatalf("untracked selected host was accepted: %v", err)
		}
	})

	t.Run("dirty shared scope", func(t *testing.T) {
		fixture := newSyncFixture(t, "owner-a")
		fixture.writeSource("shared/config.env", "YARD_IMAGE=images:debian/13\n")
		fixture.commit("shared only")
		fixture.writeSource("shared/config.env", "YARD_IMAGE=images:ubuntu/24.04\n")
		if _, err := BuildPlan(fixture.options(false)); err == nil ||
			!strings.Contains(err.Error(), "changes in managed paths") {
			t.Fatalf("dirty shared scope was accepted: %v", err)
		}
	})
}

func TestVersionedConfigSyncRejectsDirtyUnknownSecretAndLocalOnlySource(t *testing.T) {
	for _, test := range []struct {
		name, content, want string
	}{
		{"unknown", "TYPO_SSH_PORT=2233\n", "unknown setting"},
		{"secret-like", "YARD_IMAGE=password=not-config\n", "secret material"},
		{"local-only", "STORAGE_PATH=/srv/private\n", "local-only"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSyncFixture(t, "owner-a")
			fixture.writeSource("hosts/owner-a/config.env", test.content)
			fixture.commit("invalid")
			if _, err := BuildPlan(fixture.options(false)); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("wanted %q rejection, got %v", test.want, err)
			}
		})
	}

	fixture := newSyncFixture(t, "owner-a")
	fixture.writeSource("hosts/owner-a/config.env", "SSH_PORT=2233\n")
	fixture.commit("clean")
	fixture.writeSource("hosts/owner-a/config.env", "SSH_PORT=2234\n")
	if _, err := BuildPlan(fixture.options(false)); err == nil ||
		!strings.Contains(err.Error(), "changes in managed paths") {
		t.Fatalf("dirty source was accepted: %v", err)
	}

	fileFixture := newSyncFixture(t, "owner-a")
	fileFixture.writeSource("hosts/owner-a/config.env", "")
	fileFixture.writeSource(
		"hosts/owner-a/overrides/agents/codex/config.toml",
		"api_key=\"must-not-be-versioned\"\n",
	)
	fileFixture.commit("secret file setting")
	if _, err := BuildPlan(fileFixture.options(false)); err == nil ||
		!strings.Contains(err.Error(), "secret material") {
		t.Fatalf("secret-like file setting was accepted: %v", err)
	}
}

func TestVersionedConfigSyncRequiresAdoptionAndRestoresManagedDrift(t *testing.T) {
	fixture := newSyncFixture(t, "owner-a")
	fixture.writeSource("hosts/owner-a/config.env", "SSH_PORT=2233\n")
	fixture.commit("initial")
	writeSyncTestFile(t, filepath.Join(fixture.configHome, "config.env"), "SSH_PORT=2233\n", 0o600)

	if _, err := BuildPlan(fixture.options(false)); err == nil ||
		!strings.Contains(err.Error(), "--adopt") {
		t.Fatalf("unmanaged target did not require adoption: %v", err)
	}
	adopted, err := BuildPlan(fixture.options(true))
	if err != nil {
		t.Fatal(err)
	}
	if len(adopted.Changes) != 1 || adopted.Changes[0].Action != "adopt" {
		t.Fatalf("unexpected adoption plan: %#v", adopted.Changes)
	}
	if err := Apply(adopted); err != nil {
		t.Fatal(err)
	}
	writeSyncTestFile(t, filepath.Join(fixture.configHome, "config.env"), "SSH_PORT=2299\n", 0o600)
	drift, err := BuildPlan(fixture.options(false))
	if err != nil {
		t.Fatal(err)
	}
	if len(drift.Changes) != 1 || drift.Changes[0].Action != "restore-drift" {
		t.Fatalf("managed drift was not explicit: %#v", drift.Changes)
	}
	if err := Apply(drift); err != nil {
		t.Fatal(err)
	}
	assertSyncTestFile(t, filepath.Join(fixture.configHome, "config.env"), "SSH_PORT=2233\n", 0o600)
}

func TestVersionedConfigSyncGuardsYardDeletionAndKeepsSameNameHostScoped(t *testing.T) {
	fixture := newSyncFixture(t, "owner-a")
	fixture.writeSource("hosts/owner-a/config.env", "SSH_PORT=2233\n")
	fixture.writeSource("hosts/owner-a/yards/demo/config.env", "SSH_PORT=2234\n")
	fixture.writeSource("hosts/owner-b/config.env", "SSH_PORT=3233\n")
	fixture.writeSource("hosts/owner-b/yards/demo/config.env", "SSH_PORT=3234\n")
	fixture.commit("two hosts")
	ownerA, err := BuildPlan(fixture.options(false))
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(ownerA); err != nil {
		t.Fatal(err)
	}

	secondHome := filepath.Join(t.TempDir(), "config")
	second := fixture.options(false)
	second.ConfigHome = secondHome
	second.Environment = map[string]string{
		"SUBYARD_CONFIG_HOME": secondHome,
		"SUBYARD_HOME":        filepath.Join(filepath.Dir(secondHome), "data"),
		"SUBYARD_HOST_ID":     "owner-b",
	}
	ownerB, err := BuildPlan(second)
	if err != nil {
		t.Fatal(err)
	}
	if ownerA.HostID == ownerB.HostID {
		t.Fatal("two owner hosts collapsed into one identity")
	}
	if err := Apply(ownerB); err != nil {
		t.Fatal(err)
	}
	assertSyncTestFile(t,
		filepath.Join(fixture.configHome, "yards", "demo", "config.env"),
		"SSH_PORT=2234\n", 0o600)
	assertSyncTestFile(t,
		filepath.Join(secondHome, "yards", "demo", "config.env"),
		"SSH_PORT=3234\n", 0o600)

	if err := os.Remove(filepath.Join(fixture.source, "hosts", "owner-a", "yards", "demo", "config.env")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(fixture.source, "hosts", "owner-a", "yards", "demo", "overrides")); err != nil &&
		!errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(fixture.source, "hosts", "owner-a", "yards", "demo")); err != nil {
		t.Fatal(err)
	}
	fixture.commit("remove owner-a demo")
	blocked := fixture.options(false)
	blocked.YardInUse = func(string) (string, bool, error) {
		return "project state exists", true, nil
	}
	if _, err := BuildPlan(blocked); err == nil ||
		!strings.Contains(err.Error(), "project state exists") {
		t.Fatalf("in-use yard deletion was accepted: %v", err)
	}
	deletion, err := BuildPlan(fixture.options(false))
	if err != nil {
		t.Fatal(err)
	}
	if len(deletion.Changes) != 1 || deletion.Changes[0].Action != "delete" {
		t.Fatalf("unexpected deletion plan: %#v", deletion.Changes)
	}
	if err := Apply(deletion); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(fixture.configHome, "yards", "demo", "config.env")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed yard definition was not deleted: %v", err)
	}
}

func TestVersionedConfigSyncSafelyRemovesSelectedHostSubtree(t *testing.T) {
	t.Run("managed host and yard paths", func(t *testing.T) {
		fixture := newSyncFixture(t, "owner-a")
		fixture.writeSource("hosts/owner-a/config.env", "SSH_PORT=2233\n")
		fixture.writeSource("hosts/owner-a/yards/demo/config.env", "SSH_PORT=2234\n")
		fixture.commit("selected host")
		initial, err := BuildPlan(fixture.options(false))
		if err != nil {
			t.Fatal(err)
		}
		if err := Apply(initial); err != nil {
			t.Fatal(err)
		}

		if err := os.RemoveAll(filepath.Join(fixture.source, "hosts", "owner-a")); err != nil {
			t.Fatal(err)
		}
		fixture.commit("remove selected host")
		blocked := fixture.options(false)
		blocked.YardInUse = func(string) (string, bool, error) {
			return "project state exists", true, nil
		}
		if _, err := BuildPlan(blocked); err == nil ||
			!strings.Contains(err.Error(), "project state exists") {
			t.Fatalf("in-use host subtree deletion was accepted: %v", err)
		}

		plan, err := BuildPlan(fixture.options(false))
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Changes) != 2 {
			t.Fatalf("unexpected host subtree deletion plan: %#v", plan.Changes)
		}
		for _, change := range plan.Changes {
			if change.Action != "delete" {
				t.Fatalf("host subtree deletion was not explicit: %#v", plan.Changes)
			}
		}
		if err := Apply(plan); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{
			"config.env", filepath.Join("yards", "demo", "config.env"),
		} {
			if _, err := os.Lstat(filepath.Join(fixture.configHome, path)); !errors.Is(
				err, os.ErrNotExist,
			) {
				t.Fatalf("managed path %s survived host subtree deletion: %v", path, err)
			}
		}
	})

	t.Run("managed local drift", func(t *testing.T) {
		fixture := newSyncFixture(t, "owner-a")
		fixture.writeSource("hosts/owner-a/config.env", "SSH_PORT=2233\n")
		fixture.commit("selected host")
		initial, err := BuildPlan(fixture.options(false))
		if err != nil {
			t.Fatal(err)
		}
		if err := Apply(initial); err != nil {
			t.Fatal(err)
		}
		writeSyncTestFile(
			t, filepath.Join(fixture.configHome, "config.env"), "SSH_PORT=2299\n", 0o600,
		)
		if err := os.RemoveAll(filepath.Join(fixture.source, "hosts", "owner-a")); err != nil {
			t.Fatal(err)
		}
		fixture.commit("remove selected host")
		if _, err := BuildPlan(fixture.options(false)); err == nil ||
			!strings.Contains(err.Error(), "local drift and cannot be deleted") {
			t.Fatalf("drifted managed host settings were deleted: %v", err)
		}
	})
}

func TestVersionedConfigSyncGuardsMissingManagedYardDefinition(t *testing.T) {
	fixture := newSyncFixture(t, "owner-a")
	fixture.writeSource("hosts/owner-a/config.env", "SSH_PORT=2233\n")
	fixture.writeSource("hosts/owner-a/yards/demo/config.env", "SSH_PORT=2234\n")
	fixture.commit("initial")
	initial, err := BuildPlan(fixture.options(false))
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(initial); err != nil {
		t.Fatal(err)
	}
	sourceYard := filepath.Join(fixture.source, "hosts", "owner-a", "yards", "demo")
	if err := os.RemoveAll(sourceYard); err != nil {
		t.Fatal(err)
	}
	fixture.commit("remove demo")
	liveDefinition := filepath.Join(fixture.configHome, "yards", "demo", "config.env")
	if err := os.Remove(liveDefinition); err != nil {
		t.Fatal(err)
	}
	blocked := fixture.options(false)
	blocked.YardInUse = func(string) (string, bool, error) {
		return "managed Incus yard exists", true, nil
	}
	if _, err := BuildPlan(blocked); err == nil ||
		!strings.Contains(err.Error(), "managed Incus yard exists") {
		t.Fatalf("missing in-use yard definition was forgotten: %v", err)
	}
	plan, err := BuildPlan(fixture.options(false))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 1 || plan.Changes[0].Action != "record-deleted" {
		t.Fatalf("missing managed path was not explicit in the plan: %#v", plan.Changes)
	}
	if err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	converged, err := BuildPlan(fixture.options(false))
	if err != nil || converged.NeedsApply() {
		t.Fatalf("record-deleted plan did not converge: %#v %v", converged, err)
	}
}

func TestVersionedConfigSyncRecoversInterruptedApply(t *testing.T) {
	configHome := filepath.Join(t.TempDir(), "config")
	if err := ensureConfigurationRoot(configHome); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(configHome, "config.env")
	writeSyncTestFile(t, target, "SSH_PORT=2299\n", 0o600)
	tx := transaction{
		SchemaVersion: 1, ID: "1-aaaaaaaaaaaaaaaa", Phase: "applying",
		PlanDigest: strings.Repeat("a", 64), NewManifestDigest: strings.Repeat("b", 64),
		Applied: 1, Entries: []transactionEntry{{
			Path: "config.env", Action: "update", Existed: true,
			BeforeDigest: digestBytes([]byte("SSH_PORT=2233\n")),
			AfterDigest:  digestBytes([]byte("SSH_PORT=2299\n")),
			BeforeMode:   0o600, AfterMode: 0o600,
		}},
	}
	backup := filepath.Join(
		configHome, ".sync", "transactions", tx.ID, "backup", "config.env",
	)
	if err := writeFileDurable(configHome, backup, []byte("SSH_PORT=2233\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeTransaction(configHome, tx); err != nil {
		t.Fatal(err)
	}
	if err := Recover(configHome); err != nil {
		t.Fatal(err)
	}
	assertSyncTestFile(t, target, "SSH_PORT=2233\n", 0o600)
	if _, err := os.Lstat(TransactionPath(configHome)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("transaction journal survived recovery: %v", err)
	}
}

func TestVersionedConfigSyncRejectsStaleSourcePlan(t *testing.T) {
	fixture := newSyncFixture(t, "owner-a")
	fixture.writeSource("hosts/owner-a/config.env", "SSH_PORT=2233\n")
	fixture.commit("initial")
	plan, err := BuildPlan(fixture.options(false))
	if err != nil {
		t.Fatal(err)
	}
	fixture.writeSource("hosts/owner-a/config.env", "SSH_PORT=2234\n")
	fixture.commit("change after preview")
	if err := Apply(plan); !errors.Is(err, ErrPlanStale) {
		t.Fatalf("stale source plan was not identified: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(fixture.configHome, "config.env")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale plan changed live configuration: %v", err)
	}
	if _, err := os.Lstat(HostIDPath(fixture.configHome)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale plan initialized host identity: %v", err)
	}
}

func TestVersionedConfigSyncSourceFilesystemBoundary(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		fixture := newSyncFixture(t, "owner-a")
		external := filepath.Join(t.TempDir(), "config.env")
		writeSyncTestFile(t, external, "SSH_PORT=2233\n", 0o600)
		target := filepath.Join(fixture.source, "hosts", "owner-a", "config.env")
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, target); err != nil {
			t.Fatal(err)
		}
		fixture.commit("symlink")
		if _, err := BuildPlan(fixture.options(false)); err == nil ||
			(!strings.Contains(err.Error(), "regular non-symlink") &&
				!strings.Contains(err.Error(), "real regular file")) {
			t.Fatalf("source symlink was accepted: %v", err)
		}
	})

	t.Run("hardlink", func(t *testing.T) {
		fixture := newSyncFixture(t, "owner-a")
		external := filepath.Join(t.TempDir(), "config.env")
		writeSyncTestFile(t, external, "SSH_PORT=2233\n", 0o600)
		target := filepath.Join(fixture.source, "hosts", "owner-a", "config.env")
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(external, target); err != nil {
			t.Fatal(err)
		}
		fixture.commit("hardlink")
		if _, err := BuildPlan(fixture.options(false)); err == nil ||
			!strings.Contains(err.Error(), "hard-linked") {
			t.Fatalf("source hard link was accepted: %v", err)
		}
	})

	t.Run("executable", func(t *testing.T) {
		fixture := newSyncFixture(t, "owner-a")
		fixture.writeSource("hosts/owner-a/config.env", "SSH_PORT=2233\n")
		path := filepath.Join(fixture.source, "hosts", "owner-a", "config.env")
		if err := os.Chmod(path, 0o700); err != nil {
			t.Fatal(err)
		}
		fixture.commit("executable")
		if _, err := BuildPlan(fixture.options(false)); err == nil ||
			!strings.Contains(err.Error(), "must not be executable") {
			t.Fatalf("executable source setting was accepted: %v", err)
		}
	})

	t.Run("group-writable", func(t *testing.T) {
		fixture := newSyncFixture(t, "owner-a")
		fixture.writeSource("hosts/owner-a/config.env", "SSH_PORT=2233\n")
		fixture.commit("initial")
		path := filepath.Join(fixture.source, "hosts", "owner-a", "config.env")
		if err := os.Chmod(path, 0o620); err != nil {
			t.Fatal(err)
		}
		if _, err := BuildPlan(fixture.options(false)); err == nil ||
			!strings.Contains(err.Error(), "group/world writable") {
			t.Fatalf("group-writable source setting was accepted: %v", err)
		}
	})

	t.Run("ignored-untracked", func(t *testing.T) {
		fixture := newSyncFixture(t, "owner-a")
		fixture.writeSource("hosts/owner-a/config.env", "SSH_PORT=2233\n")
		fixture.writeSource(".gitignore", "hosts/owner-a/ignored.env\n")
		fixture.commit("ignore rule")
		fixture.writeSource("hosts/owner-a/ignored.env", "SSH_PORT=2299\n")
		if _, err := BuildPlan(fixture.options(false)); err == nil ||
			!strings.Contains(err.Error(), "ignored untracked") {
			t.Fatalf("ignored managed-path source was accepted: %v", err)
		}
	})

	t.Run("forbidden-role", func(t *testing.T) {
		fixture := newSyncFixture(t, "owner-a")
		fixture.writeSource("hosts/owner-a/config.env", "SSH_PORT=2233\n")
		fixture.writeSource("projects/runtime.json", "{}\n")
		fixture.commit("forbidden role")
		if _, err := BuildPlan(fixture.options(false)); err == nil ||
			!strings.Contains(err.Error(), "forbidden top-level") {
			t.Fatalf("forbidden source role was accepted: %v", err)
		}
	})
}

func TestVersionedConfigSyncRejectsLiveSymlinkAncestor(t *testing.T) {
	fixture := newSyncFixture(t, "owner-a")
	fixture.writeSource("shared/config.env", "YARD_IMAGE=images:debian/13\n")
	fixture.writeSource("hosts/owner-a/config.env", "SSH_PORT=2233\n")
	fixture.commit("initial")
	if err := os.MkdirAll(fixture.configHome, 0o700); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "external-overrides")
	if err := os.MkdirAll(filepath.Join(external, "shared"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(fixture.configHome, "overrides")); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildPlan(fixture.options(false)); err == nil ||
		!strings.Contains(err.Error(), "unsafe configuration directory") {
		t.Fatalf("live symlink ancestor was accepted: %v", err)
	}
}

func TestVersionedConfigSyncRecoveryDoesNotOverwriteExternalChange(t *testing.T) {
	configHome := filepath.Join(t.TempDir(), "config")
	if err := ensureConfigurationRoot(configHome); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(configHome, "config.env")
	writeSyncTestFile(t, target, "SSH_PORT=2299\n", 0o600)
	tx := transaction{
		SchemaVersion: 1, ID: "1-bbbbbbbbbbbbbbbb", Phase: "applying",
		PlanDigest: strings.Repeat("a", 64), NewManifestDigest: strings.Repeat("b", 64),
		Applied: 0, Entries: []transactionEntry{{
			Path: "config.env", Action: "add", Existed: false,
			AfterDigest: digestBytes([]byte("SSH_PORT=2233\n")), AfterMode: 0o600,
		}},
	}
	if err := writeTransaction(configHome, tx); err != nil {
		t.Fatal(err)
	}
	if err := Recover(configHome); err == nil ||
		!strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("recovery overwrote an external edit or returned the wrong error: %v", err)
	}
	assertSyncTestFile(t, target, "SSH_PORT=2299\n", 0o600)
	if _, err := os.Lstat(TransactionPath(configHome)); err != nil {
		t.Fatalf("failed recovery discarded its journal: %v", err)
	}
}

func TestEnsureHostIDRepairsLegacyConfigurationRootMode(t *testing.T) {
	configHome := filepath.Join(t.TempDir(), "config")
	if err := os.MkdirAll(configHome, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(configHome, 0o775); err != nil {
		t.Fatal(err)
	}
	hostID, err := EnsureHostID(configHome, map[string]string{"SUBYARD_HOST_ID": "owner-a"})
	if err != nil || hostID != "owner-a" {
		t.Fatalf("HostID bootstrap failed: hostID=%q err=%v", hostID, err)
	}
	info, err := os.Stat(configHome)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("configuration root mode = %o, want 0700", info.Mode().Perm())
	}
	if resolved, pending, err := ResolveHostID(configHome, nil); err != nil || pending || resolved != "owner-a" {
		t.Fatalf("persisted HostID did not resolve: hostID=%q pending=%v err=%v",
			resolved, pending, err)
	}
}

func TestHostIDRenameUpdatesIdentityAndManifestAtomically(t *testing.T) {
	configHome := t.TempDir()
	if err := os.Chmod(configHome, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSyncTestFile(t, HostIDPath(configHome), "owner-a\n", 0o600)
	manifest := Manifest{
		SchemaVersion: manifestSchema, Generation: 3,
		SourceID: strings.Repeat("a", 64), SourceCommit: strings.Repeat("b", 40),
		HostID: "owner-a", SourceSchema: sourceSchema, SourceDigest: strings.Repeat("c", 64),
	}
	payload, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeSyncTestFile(t, ManifestPath(configHome), string(append(payload, '\n')), 0o600)

	plan, err := PrepareHostIDRename(configHome, "owner-b")
	if err != nil {
		t.Fatal(err)
	}
	if plan.OldHostID != "owner-a" || plan.NewHostID != "owner-b" || !plan.ManifestChanged {
		t.Fatalf("unexpected rename plan: %#v", plan)
	}
	if err := ApplyHostIDRename(plan); err != nil {
		t.Fatal(err)
	}
	assertSyncTestFile(t, HostIDPath(configHome), "owner-b\n", 0o600)
	updated, err := readManifest(configHome)
	if err != nil {
		t.Fatal(err)
	}
	if updated.HostID != "owner-b" || updated.Generation != 3 || updated.SourceDigest != manifest.SourceDigest {
		t.Fatalf("manifest migration changed unrelated state: %#v", updated)
	}
	if err := ApplyHostIDRename(plan); !errors.Is(err, ErrPlanStale) {
		t.Fatalf("stale rename plan was accepted: %v", err)
	}
}

func TestHostIDRenameRejectsUnsafeAndNoopNames(t *testing.T) {
	configHome := t.TempDir()
	if err := os.Chmod(configHome, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSyncTestFile(t, HostIDPath(configHome), "owner-a\n", 0o600)
	for _, candidate := range []string{"owner-a", "", "../owner-b", "owner/b"} {
		if _, err := PrepareHostIDRename(configHome, candidate); err == nil {
			t.Fatalf("rename candidate %q was accepted", candidate)
		}
	}
	assertSyncTestFile(t, HostIDPath(configHome), "owner-a\n", 0o600)
}

func TestHostIDRenameRecoveryUsesHostIDAsCommitPoint(t *testing.T) {
	for _, test := range []struct {
		name         string
		published    bool
		wantHostID   string
		wantManifest string
	}{
		{name: "rollback before identity publish", wantHostID: "owner-a", wantManifest: "owner-a"},
		{name: "finish after identity publish", published: true, wantHostID: "owner-b", wantManifest: "owner-b"},
	} {
		t.Run(test.name, func(t *testing.T) {
			configHome := t.TempDir()
			if err := os.Chmod(configHome, 0o700); err != nil {
				t.Fatal(err)
			}
			writeSyncTestFile(t, HostIDPath(configHome), "owner-a\n", 0o600)
			manifest := Manifest{
				SchemaVersion: manifestSchema, Generation: 2,
				SourceID: strings.Repeat("a", 64), SourceCommit: strings.Repeat("b", 40),
				HostID: "owner-a", SourceSchema: sourceSchema, SourceDigest: strings.Repeat("c", 64),
			}
			payload, err := json.MarshalIndent(manifest, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			writeSyncTestFile(t, ManifestPath(configHome), string(append(payload, '\n')), 0o600)
			plan, err := PrepareHostIDRename(configHome, "owner-b")
			if err != nil {
				t.Fatal(err)
			}
			journal := hostIDRenameJournal{
				SchemaVersion: hostIDRenameSchema, OldHostID: plan.OldHostID, NewHostID: plan.NewHostID,
				HostIDDigest: plan.hostIDDigest, ManifestDigest: plan.manifestDigest,
				ManifestWasPresent: true, ManifestBefore: plan.manifestBefore, ManifestAfter: plan.manifestAfter,
			}
			if err := writeHostIDRenameJournal(configHome, journal); err != nil {
				t.Fatal(err)
			}
			writeSyncTestFile(t, ManifestPath(configHome), string(plan.manifestAfter), 0o600)
			if test.published {
				writeSyncTestFile(t, HostIDPath(configHome), "owner-b\n", 0o600)
			}
			if err := RecoverHostIDRename(configHome); err != nil {
				t.Fatal(err)
			}
			resolved, pending, err := ResolveHostID(configHome, nil)
			if err != nil || pending || resolved != test.wantHostID {
				t.Fatalf("identity recovery = %q pending=%v err=%v", resolved, pending, err)
			}
			updated, err := readManifest(configHome)
			if err != nil || updated.HostID != test.wantManifest {
				t.Fatalf("manifest recovery = %#v err=%v", updated, err)
			}
			if _, err := os.Lstat(HostIDRenameTransactionPath(configHome)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rename journal survived recovery: %v", err)
			}
		})
	}
}

func TestVersionedConfigSyncRecoveryRollsBackUnpublishedHostID(t *testing.T) {
	configHome := filepath.Join(t.TempDir(), "config")
	if err := ensureConfigurationRoot(configHome); err != nil {
		t.Fatal(err)
	}
	writeSyncTestFile(t, HostIDPath(configHome), "owner-a\n", 0o600)
	tx := transaction{
		SchemaVersion: 1, ID: "1-cccccccccccccccc", Phase: "applying",
		PlanDigest: strings.Repeat("a", 64), NewManifestDigest: strings.Repeat("b", 64),
		HostID: "owner-a", InitializeHostID: true,
	}
	if err := ensureDirectory(
		configHome, filepath.Join(configHome, ".sync", "transactions", tx.ID), 0o700,
	); err != nil {
		t.Fatal(err)
	}
	if err := writeTransaction(configHome, tx); err != nil {
		t.Fatal(err)
	}
	if err := Recover(configHome); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(HostIDPath(configHome)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unpublished host ID survived rollback: %v", err)
	}
}

func TestPublicSourceManifestSchemaMatchesRuntime(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	content, err := os.ReadFile(filepath.Join(root, "config", "subyard-config.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		AdditionalProperties bool `json:"additionalProperties"`
		Properties           struct {
			SchemaVersion struct {
				Const int `json:"const"`
			} `json:"schemaVersion"`
			Policy struct {
				MaxProperties int `json:"maxProperties"`
			} `json:"policy"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(content, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.AdditionalProperties || schema.Properties.SchemaVersion.Const != sourceSchema ||
		schema.Properties.Policy.MaxProperties != 0 {
		t.Fatalf("public source schema drifted from runtime: %#v", schema)
	}
}

type syncFixture struct {
	t            *testing.T
	root         string
	source       string
	configHome   string
	operatorHome string
	hostID       string
	fileSettings []config.FileSettingMapping
}

func newSyncFixture(t *testing.T, hostID string) *syncFixture {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	base := t.TempDir()
	source := filepath.Join(base, "source")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	runSyncGit(t, source, "init", "-q")
	writeSyncTestFile(t, filepath.Join(source, "subyard-config.json"),
		"{\n  \"schemaVersion\": 1\n}\n", 0o600)
	operatorHome := filepath.Join(base, "home")
	configHome := filepath.Join(operatorHome, ".config", "subyard")
	environment := map[string]string{
		"SUBYARD_CONFIG_HOME": configHome,
		"SUBYARD_HOME":        filepath.Join(operatorHome, ".subyard"),
		"SUBYARD_HOST_ID":     hostID,
	}
	loaded, err := config.Load(config.LoadOptions{
		RepositoryRoot: root, OperatorHome: operatorHome,
		Environment: environment, DisablePrivate: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &syncFixture{
		t: t, root: root, source: source, configHome: configHome,
		operatorHome: operatorHome, hostID: hostID,
		fileSettings: config.SyncableFileMappings(loaded),
	}
}

func (fixture *syncFixture) writeSource(relative, content string) {
	fixture.t.Helper()
	writeSyncTestFile(fixture.t, filepath.Join(fixture.source, relative), content, 0o600)
}

func (fixture *syncFixture) commit(message string) {
	fixture.t.Helper()
	runSyncGit(fixture.t, fixture.source, "add", "-A")
	runSyncGit(fixture.t, fixture.source,
		"-c", "user.name=Subyard Test", "-c", "user.email=test@invalid",
		"commit", "-q", "-m", message)
}

func (fixture *syncFixture) options(adopt bool) Options {
	return Options{
		SourceRoot: fixture.source, ConfigHome: fixture.configHome,
		RepositoryRoot: fixture.root, OperatorHome: fixture.operatorHome,
		Environment: map[string]string{
			"SUBYARD_CONFIG_HOME": fixture.configHome,
			"SUBYARD_HOME":        filepath.Join(fixture.operatorHome, ".subyard"),
			"SUBYARD_HOST_ID":     fixture.hostID,
		},
		FileSettings: fixture.fileSettings, Adopt: adopt,
	}
}

func runSyncGit(t *testing.T, directory string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, arguments...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, output)
	}
}

func writeSyncTestFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func assertSyncTestFile(t *testing.T, path, expected string, mode os.FileMode) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != expected {
		t.Fatalf("%s content=%q want=%q", path, content, expected)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != mode {
		t.Fatalf("%s mode=%04o want=%04o", path, info.Mode().Perm(), mode)
	}
}

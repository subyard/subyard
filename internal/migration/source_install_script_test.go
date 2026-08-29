package migration

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type sourceInstallFixture struct {
	home, source, bin, data, config, rc, login string
}

func TestSourceInstallMigrationAndRecovery(t *testing.T) {
	requireJQ(t)
	fixture := newSourceInstallFixture(t,
		"# Stable launcher for a release-installed native Go control-plane engine.")
	output, err := fixture.migrate()
	if err != nil {
		t.Fatalf("migration failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "migrated source installation") {
		t.Fatalf("migration result is incomplete: %s", output)
	}
	runtimeLauncher := filepath.Join(fixture.data, "runtime/current/bin/yard")
	for _, name := range []string{"yard", "sy"} {
		target, err := os.Readlink(filepath.Join(fixture.bin, name))
		if err != nil || target != runtimeLauncher {
			t.Fatalf("%s did not switch to runtime: target=%q err=%v", name, target, err)
		}
	}
	assertSameFile(t, filepath.Join(fixture.source, "private/config.env"),
		filepath.Join(fixture.config, "config.env"))
	assertSameFile(t, filepath.Join(fixture.config, "yards/named/config.env"),
		filepath.Join(fixture.data, "recovery/pre-go-source/normalized-yard-1.env"))
	if got := string(readTestFile(t,
		filepath.Join(fixture.config, "yards/named/config.env"))); got !=
		"YARD_TEMPLATE=test-vms\nSSH_PORT=3333\n" {
		t.Fatalf("retired yard template was not normalized: %q", got)
	}
	assertSameFile(t, filepath.Join(fixture.source, "private/agents/codex/repo.rules"),
		filepath.Join(fixture.config, "overrides/host/agents/codex/repo.rules"))
	assertSameFile(t, filepath.Join(fixture.data,
		"recovery/pre-go-source/legacy-operator-overlay.before/private/agents/claude/settings.json"),
		filepath.Join(fixture.config, "overrides/host/agents/claude/settings.json"))
	for source, destination := range map[string]string{
		"config/profiles/openclaw/profile.env": "secrets/profiles/openclaw/profile.env",
		"config/staging/canonical.conf":        "overrides/host/staging/canonical.conf",
		"config/staging/canonical.env":         "secrets/legacy/staging/canonical.env",
		"config/prod-fingerprints":             "overrides/host/prod-fingerprints",
		"config/qa-pool/broker.conf":           "overrides/host/qa-pool/broker.conf",
		"config/qa-pool/secrets.env":           "secrets/legacy/qa-pool/secrets.env",
		"config/qa-pool/pool.jsonl":            "secrets/legacy/qa-pool/pool.jsonl",
	} {
		assertSameFile(t, filepath.Join(fixture.source, source),
			filepath.Join(fixture.config, destination))
	}
	for _, path := range []string{
		filepath.Join(fixture.config, "config.env"),
		filepath.Join(fixture.config, "yards/named/config.env"),
		filepath.Join(fixture.config, "overrides/host/agents/codex/repo.rules"),
		filepath.Join(fixture.config, "secrets/legacy/staging/canonical.env"),
	} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("migrated file is not protected: %s mode=%v err=%v", path, info.Mode(), err)
		}
	}
	for _, path := range []string{
		filepath.Join(fixture.data, "config.env"),
		filepath.Join(fixture.data, "operator-overlay"),
	} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("legacy data source remained active after migration: %s", path)
		}
	}
	rcAfter := string(readTestFile(t, fixture.rc))
	if !strings.Contains(rcAfter, filepath.Join(fixture.data, "runtime/current/completions/yard.bash")) ||
		strings.Contains(rcAfter, filepath.Join(fixture.source, "completions/yard.bash")) {
		t.Fatalf("completion was not moved to the stable runtime: %s", rcAfter)
	}

	recovery := filepath.Join(fixture.data, "recovery/pre-go-source/restore.sh")
	command := exec.Command(recovery)
	command.Env = append(os.Environ(), "HOME="+fixture.home, "SUBYARD_HOME="+fixture.data)
	if output, err := command.CombinedOutput(); err == nil ||
		!strings.Contains(string(output), "restore would invalidate release migration history") {
		t.Fatalf("committed source import allowed legacy restore: %v\n%s", err, output)
	}
	assertRuntimeEntrypoints(t, fixture)
	assertSameFile(t, filepath.Join(fixture.source, "private/config.env"),
		filepath.Join(fixture.config, "config.env"))
	if info, err := os.Stat(runtimeLauncher); err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("recovery damaged verified runtime: %v", err)
	}
}

func TestSourceInstallLeafDoesNotOwnReleaseAuthorizationOrJournal(t *testing.T) {
	payload, err := os.ReadFile(filepath.Join("..", "..", "scripts", "migrate-source-install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"_release-transition", "SUBYARD_RELEASE_TRANSITION_GRANT", "grant-v1-",
		"release_transition()", "write_transaction applying state-migration",
		"_migrate rollback",
	} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("source leaf still owns outer transition behavior %q", forbidden)
		}
	}
}

func TestBootstrapRediscoversInterruptedSourceIngressFromRecovery(t *testing.T) {
	payload, err := os.ReadFile(filepath.Join("..", "..", "dev", "bootstrap-runtime.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(payload)
	for _, required := range []string{
		`source_recovery="$DATA_HOME/recovery/pre-go-source"`,
		`source-install-manifest.json`,
		`$'schema=1\nphase=complete\nstep=complete'`,
		`$'schema=1\nphase=applying\nstep=entrypoint-switch'`,
		`.sourceRoot | select(type == "string" and startswith("/"))`,
		`SOURCE_INGRESS_ROOT="$recovered_source"`,
		`source recovery metadata is unsafe or invalid`,
	} {
		if !strings.Contains(source, required) {
			t.Fatalf("bootstrap omits interrupted source recovery binding %q", required)
		}
	}
}

func TestSourceRecoveryAcceptsCanonicalTestYardRename(t *testing.T) {
	requireJQ(t)
	fixture := newSourceInstallFixture(t,
		"# Stable launcher for a release-installed native Go control-plane engine.")
	if output, err := fixture.migrate(); err != nil {
		t.Fatalf("migration failed: %v\n%s", err, output)
	}

	recovery := filepath.Join(fixture.data, "recovery/pre-go-source")
	created := filepath.Join(recovery, "created.tsv")
	directories := filepath.Join(recovery, "created-directories.list")
	oldFile := filepath.Join(fixture.config, "yards/e2e-yard/config.env")
	newFile := filepath.Join(fixture.config, "yards/test-yard/config.env")
	namedFile := filepath.Join(fixture.config, "yards/named/config.env")
	for path, pair := range map[string][2]string{
		created:     {namedFile, oldFile},
		directories: {filepath.Dir(namedFile), filepath.Dir(oldFile)},
	} {
		payload := strings.ReplaceAll(string(readTestFile(t, path)), pair[0], pair[1])
		writeTestFile(t, path, 0o600, payload)
	}
	if err := os.Rename(filepath.Dir(namedFile), filepath.Dir(newFile)); err != nil {
		t.Fatal(err)
	}

	restoreFixture(t, fixture)
	if _, err := os.Stat(newFile); err != nil {
		t.Fatalf("sealed recovery changed canonically renamed test-yard config: %v", err)
	}
}

func TestSourceInstallMigrationSkipsCustomRuntimeDataHome(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	temp := t.TempDir()
	home := filepath.Join(temp, "operator")
	bin := filepath.Join(home, ".local/bin")
	data := filepath.Join(temp, "runtime-data")
	runtimeRoot := filepath.Join(data, "runtime")
	rc := filepath.Join(home, ".bashrc")
	login := filepath.Join(home, ".profile")
	for _, directory := range []string{bin, filepath.Join(runtimeRoot, "current/bin")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, rc, 0o600, "# fixture\n")
	writeTestFile(t, login, 0o600, "# fixture\n")

	run := func(t *testing.T) {
		t.Helper()
		command := exec.Command(
			filepath.Join(root, "scripts/migrate-source-install.sh"),
			"--runtime-root", runtimeRoot,
			"--candidate-root", filepath.Join(home, "candidate"),
			"--source-root", filepath.Join(home, "source"),
			"--bin-dir", bin,
			"--rc", rc,
			"--login-rc", login,
			"--data-home", data,
			"--operation", "import",
			"--transaction", "tx-source-test-001",
			"--plan", "plan-source-test-001",
			"--source-plan", strings.Repeat("a", 64),
		)
		command.Env = append(os.Environ(), "HOME="+home)
		output, err := command.CombinedOutput()
		if err == nil || exitStatus(err) != 3 || len(output) != 0 {
			t.Fatalf("non-source install was not skipped: status=%d output=%s",
				exitStatus(err), output)
		}
	}

	t.Run("entrypoints-absent", run)
	t.Run("runtime-entrypoints", func(t *testing.T) {
		runtimeLauncher := filepath.Join(runtimeRoot, "current/bin/yard")
		writeTestFile(t, runtimeLauncher, 0o700, "#!/bin/sh\nexit 0\n")
		for _, name := range []string{"yard", "sy"} {
			if err := os.Symlink(runtimeLauncher, filepath.Join(bin, name)); err != nil {
				t.Fatal(err)
			}
		}
		run(t)
	})
}

func TestSourceInstallMigrationRecoversInterruptedPhases(t *testing.T) {
	requireJQ(t)
	for _, test := range []struct {
		fault, phase, step string
	}{
		{"prepared", "prepared", "none"},
		{"config-import-temporary", "applying", "config-import"},
		{"config-import", "applying", "config-import"},
		{"legacy-archive", "applying", "legacy-archive"},
		{"source-import-transaction", "applying", "source-import-ready"},
		{"source-import-ready", "applying", "source-import-ready"},
		{"shell-integration-temporary", "applying", "shell-integration"},
		{"shell-integration-rc", "applying", "shell-integration"},
		{"shell-integration", "applying", "shell-integration"},
		{"entrypoint-switch-temporary", "applying", "entrypoint-switch"},
		{"entrypoint-switch-sy", "applying", "entrypoint-switch"},
		{"entrypoint-switch", "applying", "entrypoint-switch"},
	} {
		t.Run(test.fault, func(t *testing.T) {
			fixture := newSourceInstallFixture(t,
				"# Stable launcher for a release-installed native Go control-plane engine.")
			output, err := fixture.migrateWithFault(test.fault)
			if err == nil || !strings.Contains(string(output), "fault injection after "+test.fault) {
				t.Fatalf("fault point did not interrupt migration: err=%v output=%s", err, output)
			}
			recoveryRoot := filepath.Join(fixture.data, "recovery/pre-go-source")
			transaction := string(readTestFile(t, filepath.Join(recoveryRoot, "transaction")))
			if !strings.Contains(transaction, "phase="+test.phase+"\n") ||
				!strings.Contains(transaction, "step="+test.step+"\n") {
				t.Fatalf("interrupted transaction was not durable: %s", transaction)
			}
			assertRecognizedEntrypoints(t, fixture)
			if _, err := os.Stat(filepath.Join(fixture.source, "bin/yard")); err != nil {
				t.Fatalf("interruption damaged the source checkout: %v", err)
			}

			output, err = fixture.migrate()
			if err != nil {
				t.Fatalf("installer did not recover and retry: %v\n%s", err, output)
			}
			if (!strings.Contains(string(output), "recovered incomplete source migration; retrying") &&
				!strings.Contains(string(output), "completed source entrypoint recovery")) ||
				!strings.Contains(string(output), "migrated source installation") {
				t.Fatalf("installer did not report deterministic recovery: %s", output)
			}
			transaction = string(readTestFile(t, filepath.Join(recoveryRoot, "transaction")))
			if !strings.Contains(transaction, "phase=complete\n") ||
				!strings.Contains(transaction, "step=complete\n") {
				t.Fatalf("retried transaction did not complete: %s", transaction)
			}
			assertRuntimeEntrypoints(t, fixture)
			assertNoMigrationTemps(t, fixture)
			if repeatOutput, repeatErr := fixture.migrate(); repeatErr == nil ||
				exitStatus(repeatErr) != 3 || len(repeatOutput) != 0 {
				t.Fatalf("completed retry was not idempotent: status=%d output=%s",
					exitStatus(repeatErr), repeatOutput)
			}

			restoreFixture(t, fixture)
			assertRuntimeEntrypoints(t, fixture)
			assertSameFile(t, filepath.Join(fixture.source, "private/config.env"),
				filepath.Join(fixture.config, "config.env"))
		})
	}
}

func TestSourceInstallIncompleteRecoveryRefusesOperatorDrift(t *testing.T) {
	requireJQ(t)
	fixture := newSourceInstallFixture(t,
		"# Stable launcher for a release-installed native Go control-plane engine.")
	if output, err := fixture.migrateWithFault("config-import"); err == nil {
		t.Fatalf("config fault did not interrupt migration: %s", output)
	}
	target := filepath.Join(fixture.config, "config.env")
	writeTestFile(t, target, 0o600, string(readTestFile(t, target))+"operator drift\n")

	output, err := fixture.migrate()
	if err == nil || !strings.Contains(string(output), "migrated file changed after installation") {
		t.Fatalf("operator drift did not block automatic recovery: err=%v output=%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(fixture.data, "recovery/pre-go-source/transaction")); err != nil {
		t.Fatalf("failed recovery did not retain its journal: %v", err)
	}
	assertSourceEntrypoints(t, fixture)
	if _, err := os.Stat(filepath.Join(fixture.data, "config.env")); err != nil {
		t.Fatalf("failed recovery changed the legacy source config: %v", err)
	}
}

func TestSourceInstallCompletedRecoveryRefusesOperatorDrift(t *testing.T) {
	requireJQ(t)
	fixture := newSourceInstallFixture(t,
		"# Stable launcher for a release-installed native Go control-plane engine.")
	if output, err := fixture.migrate(); err != nil {
		t.Fatalf("migration failed: %v\n%s", err, output)
	}
	writeTestFile(t, fixture.rc, 0o600, string(readTestFile(t, fixture.rc))+"# operator drift\n")

	recovery := filepath.Join(fixture.data, "recovery/pre-go-source/restore.sh")
	command := exec.Command(recovery)
	command.Env = append(os.Environ(), "HOME="+fixture.home, "SUBYARD_HOME="+fixture.data)
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "restore would invalidate release migration history") {
		t.Fatalf("completed recovery overwrote operator drift: err=%v output=%s", err, output)
	}
	assertRuntimeEntrypoints(t, fixture)
	if !strings.Contains(string(readTestFile(t, fixture.rc)), "# operator drift\n") {
		t.Fatal("failed completed recovery changed the operator shell file")
	}
	if _, err := os.Stat(filepath.Join(fixture.data, "recovery/pre-go-source/transaction")); err != nil {
		t.Fatalf("failed completed recovery did not retain its journal: %v", err)
	}
}

func TestSourceInstallRecoveryRemovesCreatedConfigRoot(t *testing.T) {
	requireJQ(t)
	fixture := newSourceInstallFixture(t,
		"# Stable launcher for a release-installed native Go control-plane engine.")
	if err := os.Remove(fixture.config); err != nil {
		t.Fatal(err)
	}
	if output, err := fixture.migrate(); err != nil {
		t.Fatalf("migration with absent config root failed: %v\n%s", err, output)
	}

	restoreFixture(t, fixture)
	if _, err := os.Stat(fixture.config); err != nil {
		t.Fatalf("sealed recovery removed the migration-created config root: %v", err)
	}
	assertRuntimeEntrypoints(t, fixture)
}

func TestSourceInstallRejectsUnknownTransactionStep(t *testing.T) {
	requireJQ(t)
	fixture := newSourceInstallFixture(t,
		"# Stable launcher for a release-installed native Go control-plane engine.")
	if output, err := fixture.migrateWithFault("prepared"); err == nil {
		t.Fatalf("prepared fault did not interrupt migration: %s", output)
	}
	transaction := filepath.Join(fixture.data, "recovery/pre-go-source/transaction")
	writeTestFile(t, transaction, 0o600, "schema=1\nphase=applying\nstep=unknown\n")

	output, err := fixture.migrate()
	if err == nil || !strings.Contains(string(output), "invalid source recovery transaction phase/step") {
		t.Fatalf("unknown transaction step did not fail closed: err=%v output=%s", err, output)
	}
	assertSourceEntrypoints(t, fixture)
	if _, err := os.Stat(transaction); err != nil {
		t.Fatalf("invalid transaction journal was not retained: %v", err)
	}
}

func TestSourceInstallMigrationConflictAndExistingIdenticalTarget(t *testing.T) {
	requireJQ(t)
	t.Run("conflict", func(t *testing.T) {
		fixture := newSourceInstallFixture(t,
			"# Stable launcher for a release-installed native Go control-plane engine.")
		target := filepath.Join(fixture.config, "secrets/legacy/staging/canonical.env")
		writeTestFile(t, target, 0o600, "different\n")
		output, err := fixture.migrate()
		if err == nil || !strings.Contains(string(output), "different content") {
			t.Fatalf("conflicting destination was not rejected: err=%v output=%s", err, output)
		}
		assertSourceEntrypoints(t, fixture)
		if string(readTestFile(t, target)) != "different\n" {
			t.Fatal("conflicting destination was changed")
		}
		if _, err := os.Lstat(filepath.Join(fixture.config, "config.env")); !os.IsNotExist(err) {
			t.Fatal("failed transaction retained an earlier created file")
		}
	})

	t.Run("identical", func(t *testing.T) {
		fixture := newSourceInstallFixture(t,
			"# Stable launcher for a release-installed native Go control-plane engine.")
		source := filepath.Join(fixture.source, "config/staging/canonical.env")
		target := filepath.Join(fixture.config, "secrets/legacy/staging/canonical.env")
		writeTestFile(t, target, 0o600, string(readTestFile(t, source)))
		if output, err := fixture.migrate(); err != nil {
			t.Fatalf("identical destination was rejected: %v\n%s", err, output)
		}
		if output, err := fixture.migrate(); err == nil || exitStatus(err) != 3 || len(output) != 0 {
			t.Fatalf("repeat migration did not report already-runtime status: status=%d output=%s",
				exitStatus(err), output)
		}
		recovery := filepath.Join(fixture.data, "recovery/pre-go-source/restore.sh")
		command := exec.Command(recovery)
		command.Env = append(os.Environ(), "HOME="+fixture.home, "SUBYARD_HOME="+fixture.data)
		if output, err := command.CombinedOutput(); err == nil ||
			!strings.Contains(string(output), "restore would invalidate release migration history") {
			t.Fatalf("committed restore was not sealed: %v\n%s", err, output)
		}
		if _, err := os.Stat(target); err != nil {
			t.Fatalf("recovery removed a pre-existing identical destination: %v", err)
		}
	})
}

func TestSourceInstallMigrationRejectsUnsafeSourceAndDestination(t *testing.T) {
	requireJQ(t)
	for _, test := range []struct {
		name string
		edit func(*testing.T, sourceInstallFixture)
		want string
	}{
		{
			name: "group-writable-source",
			edit: func(t *testing.T, fixture sourceInstallFixture) {
				if err := os.Chmod(filepath.Join(fixture.source, "config/staging/canonical.env"), 0o622); err != nil {
					t.Fatal(err)
				}
			},
			want: "group/world writable",
		},
		{
			name: "unsafe-existing-mode",
			edit: func(t *testing.T, fixture sourceInstallFixture) {
				source := filepath.Join(fixture.source, "config/staging/canonical.env")
				target := filepath.Join(fixture.config, "secrets/legacy/staging/canonical.env")
				writeTestFile(t, target, 0o644, string(readTestFile(t, source)))
			},
			want: "unsafe mode",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSourceInstallFixture(t,
				"# Stable launcher for a release-installed native Go control-plane engine.")
			test.edit(t, fixture)
			output, err := fixture.migrate()
			if err == nil || !strings.Contains(string(output), test.want) {
				t.Fatalf("unsafe input was not rejected: err=%v output=%s", err, output)
			}
			assertSourceEntrypoints(t, fixture)
		})
	}
}

func TestSourceInstallDetectorIsExact(t *testing.T) {
	requireJQ(t)
	for _, test := range []struct {
		name, marker string
		ok           bool
	}{
		{"historical-bash", "# historical thin dispatcher over scripts/", true},
		{"unknown", "# unknown launcher", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSourceInstallFixture(t, test.marker)
			output, err := fixture.migrate()
			if test.ok && err != nil {
				t.Fatalf("recognized launcher failed: %v\n%s", err, output)
			}
			if !test.ok {
				if err == nil {
					t.Fatal("unknown launcher was accepted")
				}
				if !strings.Contains(string(output), "not a recognized source-installed Subyard version") {
					t.Fatalf("unknown launcher did not fail at detector: %s", output)
				}
				target, linkErr := filepath.EvalSymlinks(filepath.Join(fixture.bin, "yard"))
				if linkErr != nil || target != filepath.Join(fixture.source, "bin/yard") {
					t.Fatalf("failed detector changed entrypoint: target=%q err=%v", target, linkErr)
				}
			}
		})
	}

	t.Run("ambiguous-entrypoint", func(t *testing.T) {
		fixture := newSourceInstallFixture(t,
			"# Stable launcher for a release-installed native Go control-plane engine.")
		sy := filepath.Join(fixture.bin, "sy")
		if err := os.Remove(sy); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, sy, 0o600, "ambiguous\n")
		if output, err := fixture.migrate(); err == nil {
			t.Fatalf("regular sy entrypoint was accepted: %s", output)
		}
		target, err := filepath.EvalSymlinks(filepath.Join(fixture.bin, "yard"))
		if err != nil || target != filepath.Join(fixture.source, "bin/yard") {
			t.Fatalf("ambiguous detector changed yard: target=%q err=%v", target, err)
		}
	})
}

func newSourceInstallFixture(t *testing.T, marker string) sourceInstallFixture {
	t.Helper()
	root := filepath.Clean(filepath.Join("..", ".."))
	temp := t.TempDir()
	fixture := sourceInstallFixture{
		home: filepath.Join(temp, "home"),
	}
	fixture.source = filepath.Join(fixture.home, "Subyard")
	fixture.bin = filepath.Join(fixture.home, ".local/bin")
	fixture.data = filepath.Join(fixture.home, ".subyard")
	fixture.config = filepath.Join(fixture.home, ".config/subyard")
	fixture.rc = filepath.Join(fixture.home, ".bashrc")
	fixture.login = filepath.Join(fixture.home, ".profile")
	candidateRoot := filepath.Join(fixture.data, "runtime/releases/release-current")
	for _, directory := range []string{
		filepath.Join(fixture.source, "bin"),
		filepath.Join(fixture.source, "scripts"),
		filepath.Join(fixture.source, "config"),
		filepath.Join(fixture.source, "completions"),
		filepath.Join(fixture.source, "private/yards"),
		filepath.Join(fixture.source, "private/agents/codex"),
		filepath.Join(fixture.source, "config/profiles/openclaw"),
		filepath.Join(fixture.source, "config/staging"),
		filepath.Join(fixture.source, "config/qa-pool"),
		filepath.Join(fixture.data, "operator-overlay/private/agents/claude"),
		fixture.bin,
		filepath.Join(candidateRoot, "bin"),
		filepath.Join(candidateRoot, "scripts"),
		filepath.Join(candidateRoot, "completions"),
		fixture.config,
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("releases/release-current", filepath.Join(fixture.data, "runtime/current")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(fixture.source, "bin/yard"), 0o700,
		"#!/bin/sh\n"+marker+"\nexit 0\n")
	writeTestFile(t, filepath.Join(fixture.source, "scripts/install-cli.sh"), 0o700,
		"#!/bin/sh\nexit 0\n")
	writeTestFile(t, filepath.Join(fixture.source, "config/commands.registry"), 0o600, "fixture\n")
	writeTestFile(t, filepath.Join(fixture.source, "completions/yard.bash"), 0o600, "fixture\n")
	writeTestFile(t, filepath.Join(fixture.source, "private/config.env"), 0o600,
		"DEV_SUDO=1\nAGENT_codex_RULES=\"$SUBYARD_CONFIG_DIR/../private/agents/codex/repo.rules\"\n")
	writeTestFile(t, filepath.Join(fixture.data, "config.env"), 0o600,
		"DEV_SUDO=1\nAGENT_codex_RULES=\"$SUBYARD_CONFIG_DIR/../private/agents/codex/repo.rules\"\n")
	writeTestFile(t, filepath.Join(fixture.data,
		"operator-overlay/private/agents/claude/settings.json"), 0o600,
		"{\"fixture\":true}\n")
	writeTestFile(t, filepath.Join(fixture.source, "private/yards/named.env"), 0o600,
		"YARD_TEMPLATE=e2e-vms\nSSH_PORT=3333\n")
	writeTestFile(t, filepath.Join(fixture.source, "private/agents/codex/repo.rules"), 0o600,
		"fixture rule\n")
	writeTestFile(t, filepath.Join(fixture.source, "config/profiles/openclaw/profile.env"), 0o600,
		"PROFILE_TOKEN=fixture\n")
	writeTestFile(t, filepath.Join(fixture.source, "config/staging/canonical.conf"), 0o600,
		"PROFILE=openclaw\n")
	writeTestFile(t, filepath.Join(fixture.source, "config/staging/canonical.env"), 0o600,
		"STAGING_TOKEN=fixture\n")
	writeTestFile(t, filepath.Join(fixture.source, "config/prod-fingerprints"), 0o600,
		"fixture-hash\n")
	writeTestFile(t, filepath.Join(fixture.source, "config/qa-pool/broker.conf"), 0o600,
		"CLOUD_PORT=3210\n")
	writeTestFile(t, filepath.Join(fixture.source, "config/qa-pool/secrets.env"), 0o600,
		"QA_SECRET=fixture\n")
	writeTestFile(t, filepath.Join(fixture.source, "config/qa-pool/pool.jsonl"), 0o600,
		"{\"fixture\":true}\n")
	writeTestFile(t, fixture.rc, 0o600, fmt.Sprintf(
		"# keep\nexport KEEP_ME=1\n# Subyard CLI completion\n[ -f %q ] && source %q\n",
		filepath.Join(fixture.source, "completions/yard.bash"),
		filepath.Join(fixture.source, "completions/yard.bash")))
	writeTestFile(t, fixture.login, 0o600, "# keep login\n")
	if err := os.Symlink(filepath.Join(fixture.source, "bin/yard"),
		filepath.Join(fixture.bin, "yard")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(fixture.source, "bin/yard"),
		filepath.Join(fixture.bin, "sy")); err != nil {
		t.Fatal(err)
	}

	candidate := filepath.Join(fixture.data, "runtime/current/bin/yard")
	writeTestFile(t, candidate, 0o700, `#!/bin/sh
case "$*" in
  --version) printf 'yard fixture\n' ;;
  '_migrate check'|'_migrate apply'|'_migrate finalize'|'_migrate cleanup'|'-Y named _migrate check'|'-Y named _migrate apply') ;;
	  '_migrate paths') printf '{"dataHome":"%s","configHome":"%s"}\n' "$TEST_DATA_HOME" "$TEST_CONFIG_HOME" ;;
	  _migrate\ overlay-manifest\ *)
	    printf '%s\n' '{"schemaVersion":2,"sourceRoot":"'"$TEST_SOURCE_ROOT"'","dataHome":"'"$TEST_DATA_HOME"'","configHome":"'"$TEST_CONFIG_HOME"'","entries":['\
'{"sourceBase":"data-home","source":"config.env","destinationRoot":"config-home","destination":"config.env","kind":"previous-host-config","mode":"0600","conflictPolicy":"identical-or-fail"},'\
'{"sourceBase":"source-root","source":"private/config.env","destinationRoot":"config-home","destination":"config.env","kind":"host-config","mode":"0600","conflictPolicy":"identical-or-fail"},'\
'{"sourceBase":"source-root","source":"private/yards/named.env","destinationRoot":"config-home","destination":"yards/named/config.env","kind":"yard-config","mode":"0600","conflictPolicy":"identical-or-fail","contentTransform":"yard-template-e2e-vms-to-test-vms"},'\
'{"sourceBase":"source-root","source":"private/agents/codex/repo.rules","destinationRoot":"config-home","destination":"overrides/host/agents/codex/repo.rules","kind":"agent-asset","mode":"0600","conflictPolicy":"identical-or-fail"},'\
'{"sourceBase":"data-home","source":"operator-overlay/private/agents/claude/settings.json","destinationRoot":"config-home","destination":"overrides/host/agents/claude/settings.json","kind":"agent-asset","mode":"0600","conflictPolicy":"identical-or-fail"},'\
'{"sourceBase":"source-root","source":"config/profiles/openclaw/profile.env","destinationRoot":"config-home","destination":"secrets/profiles/openclaw/profile.env","kind":"profile-secret","mode":"0600","conflictPolicy":"identical-or-fail"},'\
'{"sourceBase":"source-root","source":"config/staging/canonical.conf","destinationRoot":"config-home","destination":"overrides/host/staging/canonical.conf","kind":"staging-config","mode":"0600","conflictPolicy":"identical-or-fail"},'\
'{"sourceBase":"source-root","source":"config/staging/canonical.env","destinationRoot":"config-home","destination":"secrets/legacy/staging/canonical.env","kind":"legacy-staging-secret","mode":"0600","conflictPolicy":"identical-or-fail"},'\
'{"sourceBase":"source-root","source":"config/prod-fingerprints","destinationRoot":"config-home","destination":"overrides/host/prod-fingerprints","kind":"production-fingerprints","mode":"0600","conflictPolicy":"identical-or-fail"},'\
'{"sourceBase":"source-root","source":"config/qa-pool/broker.conf","destinationRoot":"config-home","destination":"overrides/host/qa-pool/broker.conf","kind":"qa-broker-config","mode":"0600","conflictPolicy":"identical-or-fail"},'\
'{"sourceBase":"source-root","source":"config/qa-pool/secrets.env","destinationRoot":"config-home","destination":"secrets/legacy/qa-pool/secrets.env","kind":"legacy-qa-secrets","mode":"0600","conflictPolicy":"identical-or-fail"},'\
'{"sourceBase":"source-root","source":"config/qa-pool/pool.jsonl","destinationRoot":"config-home","destination":"secrets/legacy/qa-pool/pool.jsonl","kind":"legacy-qa-pool","mode":"0600","conflictPolicy":"identical-or-fail"}'\
']}'
    ;;
  _migrate\ normalize-yard-config\ *)
    sed 's/^YARD_TEMPLATE=e2e-vms$/YARD_TEMPLATE=test-vms/' "$3" > "$4"
    chmod 0600 "$4"
    ;;
  *) exit 64 ;;
esac
`)
	writeTestFile(t, filepath.Join(fixture.data, "runtime/current/bin/yard-engine"), 0o700,
		`#!/bin/sh
set -eu
case "${1:-}" in
  --version) printf 'yard-engine fixture\n' ;;
  _release-transition)
    request=$(cat)
    mode=$(printf '%s' "$request" | jq -r .mode)
    target=$(printf '%s' "$request" | jq -r .target)
    if [ "$mode" = inspect ]; then
      printf '%s\n' '{"schemaVersion":1,"inspection":{"plan":"plan-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible","consequences":["converge imported state"]}}}'
      exit 0
    fi
    printf '{"schemaVersion":1,"outcome":{"status":"ready","reachedGoal":true,"active":"%s","target":"%s","code":"ready","message":"verified"}}\n' "$target" "$target"
    ;;
  *) exit 64 ;;
esac
`)
	writeTestFile(t, filepath.Join(fixture.data, "runtime/current/runtime-files.sha256"), 0o600,
		"fixture release identity\n")
	writeTestFile(t, filepath.Join(fixture.data, "runtime/current/completions/yard.bash"), 0o600,
		"fixture\n")
	for _, name := range []string{"migrate-source-install.sh", "restore-source-install.sh"} {
		payload := readTestFile(t, filepath.Join(root, "scripts", name))
		writeTestFile(t, filepath.Join(fixture.data, "runtime/current/scripts", name), 0o700,
			string(payload))
	}
	return fixture
}

func (fixture sourceInstallFixture) migrate() ([]byte, error) {
	return fixture.migrateWithFault("")
}

func (fixture sourceInstallFixture) migrateWithFault(fault string) ([]byte, error) {
	entrypointFault := strings.HasPrefix(fault, "shell-integration") ||
		strings.HasPrefix(fault, "entrypoint-switch")
	importFault := fault
	if entrypointFault {
		importFault = ""
	}
	output, err := fixture.runSourceOperation("import", importFault)
	if err != nil {
		return output, err
	}
	entrypointOutput, err := fixture.runSourceOperation("entrypoints", map[bool]string{
		true: fault, false: "",
	}[entrypointFault])
	return append(output, entrypointOutput...), err
}

func (fixture sourceInstallFixture) runSourceOperation(operation, fault string) ([]byte, error) {
	candidateRoot := filepath.Join(fixture.data, "runtime/releases/release-current")
	command := exec.Command(
		filepath.Join(fixture.data, "runtime/current/scripts/migrate-source-install.sh"),
		"--runtime-root", filepath.Join(fixture.data, "runtime"),
		"--candidate-root", candidateRoot,
		"--source-root", fixture.source,
		"--bin-dir", fixture.bin,
		"--rc", fixture.rc,
		"--login-rc", fixture.login,
		"--data-home", fixture.data,
		"--operation", operation,
		"--transaction", "tx-source-test-001",
		"--plan", "plan-source-test-001",
		"--source-plan", strings.Repeat("a", 64),
	)
	command.Env = append(os.Environ(),
		"HOME="+fixture.home,
		"TEST_DATA_HOME="+fixture.data,
		"TEST_CONFIG_HOME="+fixture.config,
		"TEST_SOURCE_ROOT="+fixture.source,
	)
	if fault != "" {
		command.Env = append(command.Env, "SUBYARD_SOURCE_MIGRATION_FAULT_AFTER="+fault)
	}
	return command.CombinedOutput()
}

func restoreFixture(t *testing.T, fixture sourceInstallFixture) {
	t.Helper()
	recovery := filepath.Join(fixture.data, "recovery/pre-go-source/restore.sh")
	command := exec.Command(recovery)
	command.Env = append(os.Environ(), "HOME="+fixture.home, "SUBYARD_HOME="+fixture.data)
	if output, err := command.CombinedOutput(); err == nil ||
		!strings.Contains(string(output), "restore would invalidate release migration history") {
		t.Fatalf("committed source recovery was not sealed: %v\n%s", err, output)
	}
}

func assertSourceEntrypoints(t *testing.T, fixture sourceInstallFixture) {
	t.Helper()
	for _, name := range []string{"yard", "sy"} {
		target, err := filepath.EvalSymlinks(filepath.Join(fixture.bin, name))
		if err != nil || target != filepath.Join(fixture.source, "bin/yard") {
			t.Fatalf("%s source entrypoint changed: target=%q err=%v", name, target, err)
		}
	}
}

func assertRuntimeEntrypoints(t *testing.T, fixture sourceInstallFixture) {
	t.Helper()
	runtimeLauncher := filepath.Join(fixture.data, "runtime/current/bin/yard")
	for _, name := range []string{"yard", "sy"} {
		target, err := os.Readlink(filepath.Join(fixture.bin, name))
		if err != nil || target != runtimeLauncher {
			t.Fatalf("%s runtime entrypoint mismatch: target=%q err=%v", name, target, err)
		}
	}
}

func assertRecognizedEntrypoints(t *testing.T, fixture sourceInstallFixture) {
	t.Helper()
	sourceLauncher := filepath.Join(fixture.source, "bin/yard")
	runtimeLauncher := filepath.Join(fixture.data, "runtime/current/bin/yard")
	for _, name := range []string{"yard", "sy"} {
		target, err := os.Readlink(filepath.Join(fixture.bin, name))
		if err != nil || (target != sourceLauncher && target != runtimeLauncher) {
			t.Fatalf("%s interrupted entrypoint is unsafe: target=%q err=%v", name, target, err)
		}
	}
}

func assertNoMigrationTemps(t *testing.T, fixture sourceInstallFixture) {
	t.Helper()
	err := filepath.WalkDir(fixture.home, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.Contains(entry.Name(), ".subyard-migrate.") {
			return fmt.Errorf("migration temporary path remained: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func exitStatus(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

func requireJQ(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is required by the production installer")
	}
}

func writeTestFile(t *testing.T, path string, mode os.FileMode, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, path string) []byte {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func assertSameFile(t *testing.T, left, right string) {
	t.Helper()
	if string(readTestFile(t, left)) != string(readTestFile(t, right)) {
		t.Fatalf("files differ: %s %s", left, right)
	}
}

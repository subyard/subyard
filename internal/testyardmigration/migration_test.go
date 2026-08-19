package testyardmigration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrationChildReloadsTheSelectedYardContext(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "yard")
	write(t, executable, `#!/bin/sh
set -eu
[ "$SUBYARD_INTERNAL_MIGRATION_CHILD" = 1 ]
[ "$SUBYARD_ENGINE_CONTEXT" = 1 ]
[ "$SUBYARD_ENGINE_CONTEXT_SCHEMA" = 1 ]
[ "${SUBYARD_CONFIG_LOADED+x}" != x ]
[ "$MIGRATION_PRESERVED" = yes ]
`)
	if err := os.Chmod(executable, 0o700); err != nil {
		t.Fatal(err)
	}
	err := run(context.Background(), Options{
		Executable: executable,
		Environment: []string{
			"SUBYARD_CONFIG_LOADED=1",
			"YARD_INSTANCE_NAME=yard",
			"MIGRATION_PRESERVED=yes",
		},
	}, LegacyYard, nil, "check")
	if err != nil {
		t.Fatalf("migration child retained the caller's loaded yard context: %v", err)
	}
}

func TestApplyRecreatesCanonicalTestYardAndRemovesLegacyController(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	dataHome := filepath.Join(root, "data")
	write(t, filepath.Join(configHome, "yards", LegacyYard, "config.env"),
		"YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n")
	write(t, filepath.Join(dataHome, "e2e", "controllers", LegacyYard, ".operator-enrollment-v1"),
		"managed\n")
	log := filepath.Join(root, "calls")
	incusLog := filepath.Join(root, "incus-calls")
	executable := fakeExecutable(t, root)

	if err := applyForTest(context.Background(), Options{
		Executable: executable, Incus: fakeIncus(t, root),
		ConfigHome: configHome, DataHome: dataHome,
		Environment: append(os.Environ(),
			"MIGRATION_CALLS="+log,
			"MIGRATION_CONFIG_HOME="+configHome,
			"MIGRATION_INCUS_CALLS="+incusLog,
		),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(configHome, "yards", CurrentYard, "config.env")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dataHome, "e2e", "controllers", LegacyYard)); !os.IsNotExist(err) {
		t.Fatalf("legacy controller state remains: %v", err)
	}
	calls := read(t, log)
	for _, expected := range []string{
		"-Y e2e-yard check",
		"-Y e2e-yard teardown --yes",
		"-Y test-yard init --yes",
		"-Y test-yard check",
	} {
		if !strings.Contains(calls, expected+"\n") {
			t.Fatalf("calls omitted %q:\n%s", expected, calls)
		}
	}
	if !strings.Contains(read(t, incusLog),
		"project create subyard-test-yard -c features.images=false\n") {
		t.Fatal("migration did not preserve the shared image namespace")
	}
}

func TestApplyIsNoopWithoutLegacyRegistration(t *testing.T) {
	root := t.TempDir()
	if err := applyForTest(context.Background(), Options{
		Executable:  fakeExecutable(t, root),
		Incus:       filepath.Join(root, "missing-incus-must-not-run"),
		ConfigHome:  filepath.Join(root, "config"),
		DataHome:    filepath.Join(root, "data"),
		Environment: os.Environ(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestApplyIsNoopForSourceLinkedProjectStateBeforeConfigImport(t *testing.T) {
	for _, yard := range []string{LegacyYard, CurrentYard} {
		t.Run(yard, func(t *testing.T) {
			root := t.TempDir()
			configHome := filepath.Join(root, "config")
			write(
				t,
				filepath.Join(configHome, "yards", yard, "projects", ".lock"),
				"",
			)
			if err := applyForTest(context.Background(), Options{
				Executable:  fakeExecutable(t, root),
				Incus:       filepath.Join(root, "missing-incus-must-not-run"),
				ConfigHome:  configHome,
				DataHome:    filepath.Join(root, "data"),
				Environment: os.Environ(),
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestApplyDiscardsLegacyProjectStateAndRollbackRecreatesIt(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	dataHome := filepath.Join(root, "data")
	oldRegistration := filepath.Join(configHome, "yards", LegacyYard, "config.env")
	oldLock := filepath.Join(configHome, "yards", LegacyYard, "projects", ".lock")
	newLock := filepath.Join(configHome, "yards", CurrentYard, "projects", ".lock")
	write(t, oldRegistration, "YARD_TEMPLATE=test-vms\n")
	write(t, oldLock, "")
	options := Options{
		Executable: fakeExecutable(t, root), Incus: fakeIncus(t, root),
		ConfigHome: configHome, DataHome: dataHome,
		Environment: append(
			os.Environ(),
			"MIGRATION_CALLS="+filepath.Join(root, "calls"),
			"MIGRATION_CONFIG_HOME="+configHome,
		),
	}
	if err := applyForTest(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(newLock); !os.IsNotExist(err) {
		t.Fatalf("legacy project state was copied into test-yard: %v", err)
	}
	if _, err := os.Lstat(oldLock); !os.IsNotExist(err) {
		t.Fatalf("teardown retained disposable legacy project state: %v", err)
	}
	if err := Rollback(context.Background(), options, StateLegacyDirectoryProjects); err != nil {
		t.Fatal(err)
	}
	if err := VerifyRollback(options, StateLegacyDirectoryProjects); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldLock); err != nil {
		t.Fatalf("rollback did not recreate legacy project state: %v", err)
	}
	if _, err := os.Lstat(newLock); !os.IsNotExist(err) {
		t.Fatalf("rollback retained canonical project state: %v", err)
	}
}

func TestRollbackUsesRetainedRuntimeToInitializeAndCheckLegacyYard(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	dataHome := filepath.Join(root, "data")
	write(t, filepath.Join(configHome, "yards", LegacyYard, "config.env"),
		"YARD_TEMPLATE=test-vms\n")
	currentCalls := filepath.Join(root, "current-calls")
	legacyCalls := filepath.Join(root, "legacy-calls")
	options := Options{
		Executable: fakeExecutable(t, root), Incus: fakeIncus(t, root),
		ConfigHome: configHome, DataHome: dataHome,
		Environment: append(
			os.Environ(),
			"MIGRATION_CALLS="+currentCalls,
			"MIGRATION_CONFIG_HOME="+configHome,
		),
	}
	if err := applyForTest(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(currentCalls); err != nil {
		t.Fatal(err)
	}
	legacyOptions := options
	legacyOptions.Environment = append(
		os.Environ(),
		"MIGRATION_CALLS="+legacyCalls,
		"MIGRATION_CONFIG_HOME="+configHome,
	)
	if err := RollbackWithLegacyRuntimeAndPower(
		context.Background(),
		options,
		legacyOptions,
		StateLegacyDirectory,
		"running",
	); err != nil {
		t.Fatal(err)
	}
	if current := read(t, currentCalls); current != "-Y test-yard teardown --yes\n" {
		t.Fatalf("current rollback runtime calls = %q", current)
	}
	if legacy := read(t, legacyCalls); legacy !=
		"-Y e2e-yard init --yes\n-Y e2e-yard check\n" {
		t.Fatalf("retained rollback runtime calls = %q", legacy)
	}
	if incusCalls := read(t, filepath.Join(root, "incus-calls")); !strings.Contains(incusCalls,
		"config set yard-e2e-yard user.subyard.desired_power running "+
			"--project subyard-e2e-yard\n") {
		t.Fatalf("rollback did not restore legacy desired power:\n%s", incusCalls)
	}
}

func TestApplyRejectsUnrecognizedIncompleteLegacyDirectory(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	write(t, filepath.Join(configHome, "yards", LegacyYard, "foreign"), "state\n")
	err := applyForTest(context.Background(), Options{
		Executable:  fakeExecutable(t, root),
		Incus:       filepath.Join(root, "missing-incus-must-not-run"),
		ConfigHome:  configHome,
		DataHome:    filepath.Join(root, "data"),
		Environment: os.Environ(),
	})
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("unrecognized incomplete registration error = %v", err)
	}
}

func TestApplyRollsBackRegistrationAndRecreatesLegacyYard(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	dataHome := filepath.Join(root, "data")
	oldRegistration := filepath.Join(configHome, "yards", LegacyYard, "config.env")
	write(t, oldRegistration, "YARD_TEMPLATE=test-vms\n")
	oldController := filepath.Join(dataHome, "e2e", "controllers", LegacyYard,
		".operator-enrollment-v1")
	write(t, oldController, "managed\n")
	log := filepath.Join(root, "calls")
	incusLog := filepath.Join(root, "incus-calls")
	err := applyForTest(context.Background(), Options{
		Executable: fakeExecutable(t, root), Incus: fakeIncus(t, root),
		ConfigHome: configHome, DataHome: dataHome,
		Environment: append(os.Environ(),
			"MIGRATION_CALLS="+log,
			"MIGRATION_INCUS_CALLS="+incusLog,
			"MIGRATION_FAIL=test-yard:init",
		),
	})
	if err == nil || !strings.Contains(err.Error(), "initialize test-yard") {
		t.Fatalf("migration failure = %v", err)
	}
	if _, err := os.Stat(oldRegistration); err != nil {
		t.Fatalf("legacy registration was not restored: %v", err)
	}
	if _, err := os.Stat(oldController); err != nil {
		t.Fatalf("legacy controller state changed before successful migration: %v", err)
	}
	if !strings.Contains(read(t, log), "-Y e2e-yard init --yes\n") {
		t.Fatal("legacy yard was not recreated during recovery")
	}
	if !strings.Contains(read(t, incusLog),
		"project create subyard-e2e-yard -c features.images=false\n") {
		t.Fatal("recovery did not restore the shared image namespace")
	}
}

func TestApplyRejectsExistingCurrentYardBeforeLifecycle(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	write(t, filepath.Join(configHome, "yards", LegacyYard, "config.env"),
		"YARD_TEMPLATE=test-vms\n")
	write(t, filepath.Join(configHome, "yards", CurrentYard, "config.env"),
		"YARD_TEMPLATE=test-vms\n")
	log := filepath.Join(root, "calls")
	err := applyForTest(context.Background(), Options{
		Executable: fakeExecutable(t, root), ConfigHome: configHome,
		DataHome:    filepath.Join(root, "data"),
		Environment: append(os.Environ(), "MIGRATION_CALLS="+log),
	})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("collision error = %v", err)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("collision reached lifecycle commands")
	}
}

func TestApplyAdoptsExistingCanonicalProjectAfterSourceRecovery(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	dataHome := filepath.Join(root, "data")
	oldRegistration := filepath.Join(configHome, "yards", LegacyYard, "config.env")
	newRegistration := filepath.Join(configHome, "yards", CurrentYard, "config.env")
	write(t, oldRegistration, "YARD_TEMPLATE=test-vms\n")
	write(t, filepath.Join(configHome, "yards", CurrentYard, "projects", ".lock"), "managed\n")
	log := filepath.Join(root, "calls")
	incusLog := filepath.Join(root, "incus-calls")
	incus := fakeIncus(t, root)
	setFakeProjects(t, root, "current")

	if err := applyForTest(context.Background(), Options{
		Executable: fakeExecutable(t, root), Incus: incus,
		ConfigHome: configHome, DataHome: dataHome,
		Environment: append(os.Environ(),
			"MIGRATION_CALLS="+log,
			"MIGRATION_INCUS_CALLS="+incusLog,
		),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(newRegistration); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldRegistration); !os.IsNotExist(err) {
		t.Fatalf("legacy registration remains: %v", err)
	}
	calls := read(t, log)
	if calls != "-Y test-yard check\n" {
		t.Fatalf("adoption unexpectedly changed lifecycle:\n%s", calls)
	}
	if _, err := os.Stat(filepath.Join(configHome, "yards", CurrentYard, "projects", ".lock")); err != nil {
		t.Fatalf("current yard state was not preserved: %v", err)
	}
}

func TestPrepareRejectsActiveLegacyLeaseBeforeLifecycle(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	write(t, filepath.Join(configHome, "yards", LegacyYard, "config.env"),
		"YARD_TEMPLATE=test-vms\n")
	log := filepath.Join(root, "calls")
	state, err := Prepare(context.Background(), Options{
		Executable: fakeExecutable(t, root), Incus: fakeIncus(t, root), ConfigHome: configHome,
		DataHome: filepath.Join(root, "data"),
		Environment: append(
			os.Environ(),
			"MIGRATION_CALLS="+log,
			"MIGRATION_TTL=60",
		),
	})
	if err == nil || state != "" || !strings.Contains(err.Error(), "active lease") {
		t.Fatalf("active lease preparation = %q, %v", state, err)
	}
	if strings.Contains(read(t, log), "teardown") {
		t.Fatal("active lease reached lifecycle mutation")
	}
}

func TestPrepareAcceptsLegacyDownFromDiagnosticStream(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	write(t, filepath.Join(configHome, "yards", LegacyYard, "config.env"),
		"YARD_TEMPLATE=test-vms\n")
	state, err := Prepare(context.Background(), Options{
		Executable: fakeExecutable(t, root), Incus: fakeIncus(t, root), ConfigHome: configHome,
		DataHome: filepath.Join(root, "data"),
		Environment: append(
			os.Environ(),
			"MIGRATION_CALLS="+filepath.Join(root, "calls"),
			"MIGRATION_STATUS_DOWN=1",
			"MIGRATION_STATUS_STDERR=1",
		),
	})
	if err != nil || state != StateLegacyDirectory {
		t.Fatalf("legacy down preparation = %q, %v", state, err)
	}
}

func TestPrepareLeavesDesiredRunningLegacyYardStoppedUntilCommitPreflight(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	write(
		t,
		filepath.Join(configHome, "yards", LegacyYard, "config.env"),
		"YARD_TEMPLATE=test-vms\n",
	)
	incus := fakeIncus(t, root)
	write(t, os.Getenv("MIGRATION_INSTANCE_STATE"), "STOPPED\n")
	options := Options{
		Executable: fakeExecutable(t, root), Incus: incus, ConfigHome: configHome,
		DataHome: filepath.Join(root, "data"),
		Environment: append(
			os.Environ(),
			"MIGRATION_CALLS="+filepath.Join(root, "calls"),
		),
	}
	state, err := Prepare(context.Background(), options)
	if err != nil || state != StateLegacyDirectory {
		t.Fatalf("stopped desired-running preparation = %q, %v", state, err)
	}
	log := read(t, filepath.Join(root, "incus-calls"))
	for _, unexpected := range []string{
		"start yard-e2e-yard --project subyard-e2e-yard",
		"stop yard-e2e-yard --project subyard-e2e-yard",
	} {
		if strings.Contains(log, unexpected+"\n") {
			t.Fatalf("read-only preparation invoked %q:\n%s", unexpected, log)
		}
	}
	if err := preflightLegacyLease(context.Background(), options, true); err != nil {
		t.Fatalf("commit lease preflight: %v", err)
	}
	if got := strings.TrimSpace(read(t, os.Getenv("MIGRATION_INSTANCE_STATE"))); got != "STOPPED" {
		t.Fatalf("commit lease preflight did not restore stopped state: %q", got)
	}
	if calls := read(t, filepath.Join(root, "calls")); !strings.Contains(
		calls,
		"-Y e2e-yard start --yes\n",
	) {
		t.Fatalf("commit lease preflight bypassed guarded yard start:\n%s", calls)
	}
	log = read(t, filepath.Join(root, "incus-calls"))
	if !strings.Contains(log, "stop yard-e2e-yard --project subyard-e2e-yard\n") {
		t.Fatalf("commit lease preflight did not restore stopped state:\n%s", log)
	}
	if strings.Contains(log, "start yard-e2e-yard --project subyard-e2e-yard\n") {
		t.Fatalf("commit lease preflight bypassed guarded yard start:\n%s", log)
	}
}

func TestEnsureNoActiveLeaseAcceptsOnlyProvenIdlePools(t *testing.T) {
	for name, test := range map[string]struct {
		payload string
		wantErr bool
	}{
		"json available": {
			payload: `{"pool":{"slots":[{"state":"available"},{"state":"available"}]}}`,
		},
		"json held": {
			payload: `{"pool":{"slots":[{"state":"held"}]}}`,
			wantErr: true,
		},
		"legacy idle": {
			payload: "e2e-vm-1\tRUNNING\t10.0.0.1\nttl_remaining_seconds\t0\n",
		},
		"legacy down": {
			payload: "test-vms: down\n",
		},
		"legacy down after lifecycle diagnostics": {
			payload: "Subyard host check\ntest-vms: down\nHost is ready.\n",
		},
		"legacy down text is not authoritative": {
			payload: "diagnostic: test-vms: down unexpectedly\n",
			wantErr: true,
		},
		"legacy active": {
			payload: "e2e-vm-1\tRUNNING\t10.0.0.1\nttl_remaining_seconds\t60\n",
			wantErr: true,
		},
		"unknown": {
			payload: "e2e-vm-1\tRUNNING\t10.0.0.1\n",
			wantErr: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := ensureNoActiveLease([]byte(test.payload))
			if (err != nil) != test.wantErr {
				t.Fatalf("ensureNoActiveLease() error = %v, wantErr=%v", err, test.wantErr)
			}
		})
	}
}

func TestRollbackRestoresLegacyRegistration(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	dataHome := filepath.Join(root, "data")
	oldRegistration := filepath.Join(configHome, "yards", LegacyYard, "config.env")
	write(t, oldRegistration, "YARD_TEMPLATE=test-vms\n")
	write(t, filepath.Join(dataHome, "e2e", "controllers", LegacyYard, ".operator-enrollment-v1"),
		"managed\n")
	log := filepath.Join(root, "calls")
	options := Options{
		Executable: fakeExecutable(t, root), Incus: fakeIncus(t, root),
		ConfigHome: configHome, DataHome: dataHome,
		Environment: append(os.Environ(), "MIGRATION_CALLS="+log),
	}
	if err := applyForTest(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	if err := Rollback(context.Background(), options, StateLegacyDirectory); err != nil {
		t.Fatal(err)
	}
	if err := VerifyRollback(options, StateLegacyDirectory); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldRegistration); err != nil {
		t.Fatalf("legacy registration was not restored: %v", err)
	}
	calls := read(t, log)
	for _, expected := range []string{
		"-Y test-yard teardown --yes",
		"-Y e2e-yard init --yes",
		"-Y e2e-yard check",
	} {
		if !strings.Contains(calls, expected+"\n") {
			t.Fatalf("rollback calls omitted %q:\n%s", expected, calls)
		}
	}
}

func TestNoopRollbackLeavesLateSourceIngressForItsOwningTransaction(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	write(
		t,
		filepath.Join(configHome, "yards", LegacyYard, "config.env"),
		"YARD_TEMPLATE=test-vms\n",
	)
	options := Options{
		Executable:  fakeExecutable(t, root),
		ConfigHome:  configHome,
		DataHome:    filepath.Join(root, "data"),
		Environment: os.Environ(),
	}
	if err := Rollback(context.Background(), options, StateAbsent); err != nil {
		t.Fatal(err)
	}
	if err := VerifyRollback(options, StateAbsent); err != nil {
		t.Fatal(err)
	}
}

func TestNoopRollbackAcceptsExistingCanonicalRegistration(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	currentRegistration := filepath.Join(
		configHome,
		"yards",
		CurrentYard,
		"config.env",
	)
	write(t, currentRegistration, "YARD_TEMPLATE=test-vms\n")
	options := Options{
		Executable:  fakeExecutable(t, root),
		ConfigHome:  configHome,
		DataHome:    filepath.Join(root, "data"),
		Environment: os.Environ(),
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := Rollback(context.Background(), options, StateCurrent); err != nil {
			t.Fatal(err)
		}
		if err := VerifyRollback(options, StateCurrent); err != nil {
			t.Fatal(err)
		}
	}
	if payload := read(t, currentRegistration); payload != "YARD_TEMPLATE=test-vms\n" {
		t.Fatalf("no-op rollback changed current registration: %q", payload)
	}
}

func TestCommitResumesAfterRegistrationMoveAndFinishesCurrentYard(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	dataHome := filepath.Join(root, "data")
	write(t, filepath.Join(configHome, "yards", CurrentYard, "config.env"),
		"YARD_TEMPLATE=test-vms\n")
	write(t, filepath.Join(dataHome, "e2e", "controllers", LegacyYard, ".operator-enrollment-v1"),
		"managed\n")
	log := filepath.Join(root, "calls")
	incus := fakeIncus(t, root)
	setFakeProjects(t, root, "current")
	options := Options{
		Executable: fakeExecutable(t, root), Incus: incus,
		ConfigHome: configHome, DataHome: dataHome,
		Environment: append(
			os.Environ(),
			"MIGRATION_CALLS="+log,
			"MIGRATION_FAIL=e2e-yard:test-vms",
		),
	}
	if err := Commit(context.Background(), options, StateLegacyDirectory); err != nil {
		t.Fatal(err)
	}
	if err := Verify(options, StateLegacyDirectory); err != nil {
		t.Fatal(err)
	}
	calls := read(t, log)
	for _, expected := range []string{
		"-Y test-yard init --yes",
		"-Y test-yard check",
	} {
		if !strings.Contains(calls, expected+"\n") {
			t.Fatalf("resumed commit omitted %q:\n%s", expected, calls)
		}
	}
	if strings.Contains(calls, "e2e-yard test-vms status") {
		t.Fatal("resumed commit queried the already-retired legacy yard")
	}
}

func TestManagedControllerCleanupResumesFromEmptyDirectory(t *testing.T) {
	root := t.TempDir()
	controller := filepath.Join(root, "controller")
	if err := os.MkdirAll(controller, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := removeManagedLegacyController(controller); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(controller); !os.IsNotExist(err) {
		t.Fatalf("empty interrupted controller directory remains: %v", err)
	}
}

func fakeExecutable(t *testing.T, root string) string {
	t.Helper()
	path := filepath.Join(root, "yard")
	write(t, path, `#!/bin/sh
set -eu
[ "$SUBYARD_INTERNAL_MIGRATION_CHILD" = 1 ]
printf '%s\n' "$*" >> "$MIGRATION_CALLS"
update_projects() {
	current="$(cat "$MIGRATION_INCUS_STATE")"
	case "$1:$current" in
		remove-legacy:legacy) printf 'none\n' > "$MIGRATION_INCUS_STATE" ;;
		remove-legacy:both) printf 'current\n' > "$MIGRATION_INCUS_STATE" ;;
		remove-current:current) printf 'none\n' > "$MIGRATION_INCUS_STATE" ;;
		remove-current:both) printf 'legacy\n' > "$MIGRATION_INCUS_STATE" ;;
		add-legacy:none) printf 'legacy\n' > "$MIGRATION_INCUS_STATE" ;;
		add-legacy:current) printf 'both\n' > "$MIGRATION_INCUS_STATE" ;;
		add-current:none) printf 'current\n' > "$MIGRATION_INCUS_STATE" ;;
		add-current:legacy) printf 'both\n' > "$MIGRATION_INCUS_STATE" ;;
	esac
}
if [ -n "${MIGRATION_CONFIG_HOME:-}" ]; then
	case "$*" in
		"-Y e2e-yard teardown --yes")
			test -f "$MIGRATION_CONFIG_HOME/yards/test-yard/config.env"
			;;
		"-Y test-yard init --yes")
			test ! -e "$MIGRATION_CONFIG_HOME/yards/e2e-yard/config.env"
			;;
	esac
fi
if [ "${MIGRATION_FAIL:-}" = "${2:-}:${3:-}" ]; then exit 1; fi
case "$*" in
	"-Y e2e-yard start --yes")
		[ -z "${MIGRATION_INSTANCE_STATE:-}" ] ||
			printf 'RUNNING\n' > "$MIGRATION_INSTANCE_STATE"
		;;
	"-Y e2e-yard teardown --yes")
		update_projects remove-legacy
		if [ -n "${MIGRATION_CONFIG_HOME:-}" ]; then
			find "$MIGRATION_CONFIG_HOME/yards/e2e-yard/projects" -depth -delete 2>/dev/null || :
		fi
		;;
	"-Y test-yard teardown --yes") update_projects remove-current ;;
	"-Y e2e-yard init --yes")
		update_projects add-legacy
		if [ -n "${MIGRATION_CONFIG_HOME:-}" ]; then
			install -d -m 0700 "$MIGRATION_CONFIG_HOME/yards/e2e-yard/projects"
			: > "$MIGRATION_CONFIG_HOME/yards/e2e-yard/projects/.lock"
			chmod 0600 "$MIGRATION_CONFIG_HOME/yards/e2e-yard/projects/.lock"
		fi
		;;
	"-Y test-yard init --yes") update_projects add-current ;;
esac
if [ "${3:-}" = test-vms ] && [ "${4:-}" = status ]; then
  if [ -n "${MIGRATION_INSTANCE_STATE:-}" ] &&
     [ "$(cat "$MIGRATION_INSTANCE_STATE")" != RUNNING ]; then
    printf 'test-vms: yard must be running\n' >&2
    exit 1
  fi
  if [ "${MIGRATION_STATUS_DOWN:-}" = 1 ]; then
    if [ "${MIGRATION_STATUS_STDERR:-}" = 1 ]; then
      printf 'test-vms: down\n' >&2
    else
      printf 'test-vms: down\n'
    fi
  else
    printf 'ttl_remaining_seconds\t%s\n' "${MIGRATION_TTL:-0}"
  fi
fi
`)
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func fakeIncus(t *testing.T, root string) string {
	t.Helper()
	state := filepath.Join(root, "incus-state")
	instanceState := filepath.Join(root, "incus-instance-state")
	write(t, state, "legacy\n")
	write(t, instanceState, "RUNNING\n")
	t.Setenv("MIGRATION_INCUS_STATE", state)
	t.Setenv("MIGRATION_INSTANCE_STATE", instanceState)
	t.Setenv("MIGRATION_INCUS_CALLS", filepath.Join(root, "incus-calls"))
	path := filepath.Join(root, "incus")
	write(t, path, `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$MIGRATION_INCUS_CALLS"
state="$(cat "$MIGRATION_INCUS_STATE")"
case "$*" in
	"list yard-e2e-yard --project subyard-e2e-yard --format=json")
		printf '[{"name":"yard-e2e-yard","status":"%s"}]\n' \
			"$(cat "$MIGRATION_INSTANCE_STATE")"
		;;
	"config get yard-e2e-yard user.subyard.desired_power --project subyard-e2e-yard")
		printf 'running\n'
		;;
	"start yard-e2e-yard --project subyard-e2e-yard")
		printf 'RUNNING\n' > "$MIGRATION_INSTANCE_STATE"
		;;
	"stop yard-e2e-yard --project subyard-e2e-yard")
		printf 'STOPPED\n' > "$MIGRATION_INSTANCE_STATE"
		;;
	"project list --format=json")
		case "$state" in
			legacy) printf '[{"name":"subyard-e2e-yard"}]\n' ;;
			current) printf '[{"name":"subyard-test-yard"}]\n' ;;
			both) printf '[{"name":"subyard-e2e-yard"},{"name":"subyard-test-yard"}]\n' ;;
			none) printf '[]\n' ;;
			*) exit 2 ;;
		esac
		;;
	"project get subyard-e2e-yard features.images")
		[ "$state" = legacy ] || [ "$state" = both ] || exit 1
		printf 'false\n'
		;;
	"project get subyard-test-yard features.images")
		[ "$state" = current ] || [ "$state" = both ] || exit 1
		printf 'false\n'
		;;
	"project create subyard-e2e-yard -c features.images=false")
		case "$state" in
			none) printf 'legacy\n' > "$MIGRATION_INCUS_STATE" ;;
			current) printf 'both\n' > "$MIGRATION_INCUS_STATE" ;;
			legacy|both) ;;
		esac
		;;
	"project create subyard-test-yard -c features.images=false")
		case "$state" in
			none) printf 'current\n' > "$MIGRATION_INCUS_STATE" ;;
			legacy) printf 'both\n' > "$MIGRATION_INCUS_STATE" ;;
			current|both) ;;
		esac
		;;
	"config set yard-e2e-yard user.subyard.desired_power running --project subyard-e2e-yard")
		;;
	*)
		exit 2
		;;
esac
`)
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func setFakeProjects(t *testing.T, root, state string) {
	t.Helper()
	write(t, filepath.Join(root, "incus-state"), state+"\n")
}

func applyForTest(ctx context.Context, options Options) error {
	before, err := Prepare(ctx, options)
	if err != nil {
		return err
	}
	return Commit(ctx, options, before)
}

func write(t *testing.T, path, payload string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

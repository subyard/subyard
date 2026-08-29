package testyardmigration

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"golang.org/x/sys/unix"
)

const round3RecoveryToken = "0123456789abcdef0123456789abcdef"

func TestMigrationUsesInjectedYardCommandRunner(t *testing.T) {
	var gotYard string
	var gotArguments []string
	err := run(context.Background(), Options{
		RunYard: func(_ context.Context, yard string, _ io.Writer, arguments ...string) error {
			gotYard = yard
			gotArguments = append([]string(nil), arguments...)
			return nil
		},
	}, LegacyYard, io.Discard, "teardown", "--yes")
	if err != nil || gotYard != LegacyYard ||
		!slices.Equal(gotArguments, []string{"teardown", "--yes"}) {
		t.Fatalf("injected yard command: yard=%q arguments=%v err=%v", gotYard, gotArguments, err)
	}
}

func TestMigrationCapturedCommandUsesInjectedYardCommandRunner(t *testing.T) {
	output, err := runCaptured(context.Background(), Options{
		RunYard: func(_ context.Context, yard string, output io.Writer, arguments ...string) error {
			if yard != LegacyYard || !slices.Equal(arguments, []string{"test-vms", "status"}) {
				t.Fatalf("injected yard command: yard=%q arguments=%v", yard, arguments)
			}
			_, err := io.WriteString(output, "captured")
			return err
		},
	}, LegacyYard, "test-vms", "status")
	if err != nil || string(output) != "captured" {
		t.Fatalf("captured injected yard command: output=%q err=%v", output, err)
	}
}

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

func TestPrepareProspectiveRegistrationIncludesSourceLinkedProjectState(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	write(t, filepath.Join(configHome, "yards", LegacyYard, "projects", ".lock"), "")
	incus := fakeIncus(t, root)
	write(t, filepath.Join(root, "incus-instance-state"), "STOPPED\n")
	registration := []byte("YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n")

	prepared, err := PrepareProspective(context.Background(), Options{
		Executable: fakeExecutable(t, root), Incus: incus,
		ConfigHome: configHome, DataHome: filepath.Join(root, "data"),
		Environment: os.Environ(),
	}, func(string) (config.PersistentFileSnapshot, error) {
		return config.PersistentFileSnapshot{Exists: true, Content: registration}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(registration)
	if prepared.State != StateLegacyDirectoryProjects ||
		prepared.RegistrationDigest != fmt.Sprintf("%x", digest[:]) {
		t.Fatalf("prospective owner = %#v", prepared)
	}
	if _, err := os.Lstat(filepath.Join(
		configHome, "yards", LegacyYard, "config.env",
	)); !os.IsNotExist(err) {
		t.Fatalf("prospective inspection wrote registration: %v", err)
	}
}

func TestPrepareProspectiveRegistrationUsesPostSettingsContent(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	registrationPath := filepath.Join(configHome, "yards", LegacyYard, "config.env")
	original := "YARD_TEMPLATE=test-vms\nNESTED_E2E_VMS=0\n"
	write(t, registrationPath, original)
	incus := fakeIncus(t, root)
	write(t, filepath.Join(root, "incus-instance-state"), "STOPPED\n")
	desired := []byte("YARD_TEMPLATE='test-vms'\n")

	prepared, err := PrepareProspective(context.Background(), Options{
		Executable: fakeExecutable(t, root), Incus: incus,
		ConfigHome: configHome, DataHome: filepath.Join(root, "data"),
		Environment: os.Environ(),
	}, func(string) (config.PersistentFileSnapshot, error) {
		return config.PersistentFileSnapshot{Exists: true, Content: desired}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(desired)
	if prepared.State != StateLegacyDirectory ||
		prepared.RegistrationDigest != fmt.Sprintf("%x", digest[:]) {
		t.Fatalf("post-settings prospective owner = %#v", prepared)
	}
	if actual := read(t, registrationPath); actual != original {
		t.Fatalf("prospective inspection changed registration to %q", actual)
	}
}

func TestApplyDiscardsLegacyProjectState(t *testing.T) {
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
		RecoveryToken: round3RecoveryToken,
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
	if err == nil || state != (Prepared{}) || !strings.Contains(err.Error(), "active lease") {
		t.Fatalf("active lease preparation = %#v, %v", state, err)
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
	if err != nil || state.State != StateLegacyDirectory {
		t.Fatalf("legacy down preparation = %#v, %v", state, err)
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
	if err != nil || state.State != StateLegacyDirectory {
		t.Fatalf("stopped desired-running preparation = %#v, %v", state, err)
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

func TestCommitResumesAfterRegistrationMoveAndFinishesCurrentYard(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	dataHome := filepath.Join(root, "data")
	write(t, filepath.Join(configHome, "yards", CurrentYard, "config.env"),
		"YARD_TEMPLATE=test-vms\n")
	write(t, filepath.Join(
		configHome, "yards", ".owner-migration-registration-archive."+round3RecoveryToken,
		"config.env",
	), "YARD_TEMPLATE=test-vms\n")
	write(t, filepath.Join(dataHome, "e2e", "controllers", LegacyYard, ".operator-enrollment-v1"),
		"managed\n")
	log := filepath.Join(root, "calls")
	incus := fakeIncus(t, root)
	setFakeProjects(t, root, "current")
	options := Options{
		Executable: fakeExecutable(t, root), Incus: incus,
		ConfigHome: configHome, DataHome: dataHome,
		RecoveryToken: round3RecoveryToken,
		Environment: append(
			os.Environ(),
			"MIGRATION_CALLS="+log,
			"MIGRATION_FAIL=e2e-yard:test-vms",
		),
	}
	before, err := preparedRegistration(
		StateLegacyDirectory,
		filepath.Join(configHome, "yards", CurrentYard, "config.env"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindPreparedAuxiliaryFacts(
		options,
		filepath.Join(configHome, "yards", CurrentYard, "config.env"),
		&before,
	); err != nil {
		t.Fatal(err)
	}
	before.SharedImages = true
	if err := Commit(context.Background(), options, before); err != nil {
		t.Fatal(err)
	}
	if err := Verify(options, before); err != nil {
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

func TestCommitRejectsRegistrationBytesChangedAfterPrepareBeforeCopy(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	oldRegistration := filepath.Join(configHome, "yards", LegacyYard, "config.env")
	currentRegistration := filepath.Join(configHome, "yards", CurrentYard, "config.env")
	write(t, oldRegistration, "YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n")
	options := Options{
		Executable: fakeExecutable(t, root), Incus: fakeIncus(t, root),
		ConfigHome: configHome, DataHome: filepath.Join(root, "data"),
		RecoveryToken: round3RecoveryToken,
		Environment: append(
			os.Environ(),
			"MIGRATION_CALLS="+filepath.Join(root, "calls"),
			"MIGRATION_CONFIG_HOME="+configHome,
		),
	}
	before, err := Prepare(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	write(t, oldRegistration, "YARD_TEMPLATE=test-vms\nSSH_PORT=2299\n")

	err = Commit(context.Background(), options, before)
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("registration drift was copied under the old preparation: %v", err)
	}
	if _, err := os.Lstat(currentRegistration); !os.IsNotExist(err) {
		t.Fatalf("registration drift published canonical state: %v", err)
	}
	if payload := read(t, oldRegistration); payload != "YARD_TEMPLATE=test-vms\nSSH_PORT=2299\n" {
		t.Fatalf("registration drift was overwritten: %q", payload)
	}
}

func TestCommitRejectsOverrideBytesChangedAfterPrepareBeforeMutation(t *testing.T) {
	options, before := ownerFaultFixture(t, true, true)
	legacyOverride := filepath.Join(
		options.ConfigHome, "yards", LegacyYard, "overrides", "runtime.env",
	)
	currentRegistration := filepath.Join(
		options.ConfigHome, "yards", CurrentYard, "config.env",
	)
	write(t, legacyOverride, "NESTED_E2E_VMS=1\n")

	err := Commit(context.Background(), options, before)
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("override drift reached the authorized mutation: %v", err)
	}
	if payload := read(t, legacyOverride); payload != "NESTED_E2E_VMS=1\n" {
		t.Fatalf("override drift was overwritten: %q", payload)
	}
	if _, err := os.Lstat(currentRegistration); !os.IsNotExist(err) {
		t.Fatalf("override drift published canonical registration: %v", err)
	}
}

func TestCommitRechecksOverrideAfterPreflightBeforeFirstMutation(t *testing.T) {
	options, before := ownerFaultFixture(t, true, true)
	legacyOverride := filepath.Join(
		options.ConfigHome, "yards", LegacyYard, "overrides", "runtime.env",
	)
	baseOptions := options
	mutated := false
	options.RunYard = func(
		ctx context.Context,
		yard string,
		output io.Writer,
		arguments ...string,
	) error {
		if !mutated {
			mutated = true
			write(t, legacyOverride, "NESTED_E2E_VMS=1\n")
		}
		return run(ctx, baseOptions, yard, output, arguments...)
	}

	err := Commit(context.Background(), options, before)
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("preflight override drift reached the authorized mutation: %v", err)
	}
	if !mutated {
		t.Fatal("preflight did not exercise the post-validation drift window")
	}
	incusCalls := read(t, filepath.Join(filepath.Dir(options.ConfigHome), "incus-calls"))
	if strings.Contains(incusCalls, "project create subyard-test-yard") {
		t.Fatalf("preflight override drift reached project mutation:\n%s", incusCalls)
	}
	assertTestPathAbsent(t, filepath.Join(
		options.ConfigHome, "yards", CurrentYard, "config.env",
	))
}

func TestPrepareRejectsOverrideRootReplacedWhileDigesting(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	write(t, filepath.Join(configHome, "yards", LegacyYard, "config.env"),
		"YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n")
	overrides := filepath.Join(configHome, "yards", LegacyYard, "overrides")
	write(t, filepath.Join(overrides, "runtime.env"), "NESTED_E2E_VMS=0\n")
	write(t, filepath.Join(
		root, "data", "e2e", "controllers", LegacyYard, ".operator-enrollment-v1",
	), "managed\n")
	t.Setenv("MIGRATION_SHARED_IMAGES", "1")
	options := ownerOptions(t, root, configHome)
	backup := overrides + ".original"
	replaced := false
	options.fault = func(point string) error {
		if point != "after-overrides-root-open-for-digest" || replaced {
			return nil
		}
		replaced = true
		if err := os.Rename(overrides, backup); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(overrides, "runtime.env"), "NESTED_E2E_VMS=1\n")
		return nil
	}

	prepared, err := Prepare(context.Background(), options)
	if !replaced {
		t.Fatal("override digest did not expose the retained-root inspection boundary")
	}
	if err == nil || prepared != (Prepared{}) || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("replacement override root was authorized: %#v, %v", prepared, err)
	}
	assertExactTestFile(t, filepath.Join(backup, "runtime.env"),
		"NESTED_E2E_VMS=0\n", 0o600)
	assertExactTestFile(t, filepath.Join(overrides, "runtime.env"),
		"NESTED_E2E_VMS=1\n", 0o600)
}

func TestCommitRejectsSameContentOverrideRootSwapAtMoveCAS(t *testing.T) {
	options, before := ownerFaultFixture(t, true, true)
	legacyOverrides := filepath.Join(options.ConfigHome, "yards", LegacyYard, "overrides")
	backup := legacyOverrides + ".original"
	replaced := false
	options.fault = func(point string) error {
		if point != "before-auxiliary-state-move-cas" || replaced {
			return nil
		}
		replaced = true
		if err := os.Rename(legacyOverrides, backup); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(legacyOverrides, "runtime.env"), "NESTED_E2E_VMS=0\n")
		return nil
	}

	err := Commit(context.Background(), options, before)
	if !replaced {
		t.Fatal("override move did not expose its identity CAS boundary")
	}
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("same-content replacement override root was moved: %v", err)
	}
	assertExactTestFile(t, filepath.Join(backup, "runtime.env"),
		"NESTED_E2E_VMS=0\n", 0o600)
	assertExactTestFile(t, filepath.Join(legacyOverrides, "runtime.env"),
		"NESTED_E2E_VMS=0\n", 0o600)
}

func TestCommitRejectsOverrideDestinationCreatedAtMoveCAS(t *testing.T) {
	options, before := ownerFaultFixture(t, true, true)
	currentOverrides := filepath.Join(options.ConfigHome, "yards", CurrentYard, "overrides")
	created := false
	options.fault = func(point string) error {
		if point != "before-auxiliary-state-move-cas" || created {
			return nil
		}
		created = true
		write(t, filepath.Join(currentOverrides, "foreign.env"), "FOREIGN=1\n")
		return nil
	}

	err := Commit(context.Background(), options, before)
	if !created {
		t.Fatal("override move did not expose its destination CAS boundary")
	}
	if err == nil || !strings.Contains(err.Error(), "destination") {
		t.Fatalf("late-created override destination was replaced: %v", err)
	}
	assertExactTestFile(t, filepath.Join(currentOverrides, "foreign.env"), "FOREIGN=1\n", 0o600)
	assertExactTestFile(t, filepath.Join(
		options.ConfigHome, "yards", LegacyYard, "overrides", "runtime.env",
	), "NESTED_E2E_VMS=0\n", 0o600)
}

func TestCommitRejectsControllerFactsAddedAfterPrepareBeforeMutation(t *testing.T) {
	options, before := ownerFaultFixture(t, true, false)
	controllerRoute := filepath.Join(
		options.DataHome, "e2e", "controllers", LegacyYard, "route.tsv",
	)
	write(t, controllerRoute, "default\teth0\n")

	err := Commit(context.Background(), options, before)
	if err == nil || !strings.Contains(err.Error(), "controller state changed") {
		t.Fatalf("controller drift reached the authorized mutation: %v", err)
	}
	assertExactTestFile(t, controllerRoute, "default\teth0\n", 0o600)
	assertTestPathAbsent(t, filepath.Join(
		options.ConfigHome, "yards", CurrentYard, "config.env",
	))
}

func TestDesiredObservationRejectsLegacyAuxiliaryLeftovers(t *testing.T) {
	options, before := ownerFaultFixture(t, true, true)
	if err := Commit(context.Background(), options, before); err != nil {
		t.Fatal(err)
	}
	legacyOverrides := filepath.Join(options.ConfigHome, "yards", LegacyYard, "overrides")
	currentOverrides := filepath.Join(options.ConfigHome, "yards", CurrentYard, "overrides")
	if err := os.MkdirAll(filepath.Dir(legacyOverrides), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(currentOverrides, legacyOverrides); err != nil {
		t.Fatal(err)
	}

	progress, err := ObserveProgress(context.Background(), options, before)
	if err == nil || progress != "" {
		t.Fatalf("legacy auxiliary leftovers were accepted as progress %q: %v", progress, err)
	}
	if err := Verify(options, before); err == nil {
		t.Fatal("legacy auxiliary leftovers passed desired verification")
	}
}

func TestDesiredObservationRejectsFlatLegacyDirectoryLeftover(t *testing.T) {
	options, before := flatOwnerFaultFixture(t)
	if err := Commit(context.Background(), options, before); err != nil {
		t.Fatal(err)
	}
	legacyOverrides := filepath.Join(
		options.ConfigHome, "yards", LegacyYard, "overrides", "runtime.env",
	)
	write(t, legacyOverrides, "FOREIGN=1\n")

	progress, err := ObserveProgress(context.Background(), options, before)
	if err == nil || progress != "" {
		t.Fatalf("flat legacy directory leftover was accepted as progress %q: %v", progress, err)
	}
	if err := Verify(options, before); err == nil {
		t.Fatal("flat legacy directory leftover passed desired verification")
	}
}

func TestDesiredVerificationRejectsManagedControllerLeftover(t *testing.T) {
	options, before := ownerFaultFixture(t, true, false)
	if err := Commit(context.Background(), options, before); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(
		options.DataHome, "e2e", "controllers", LegacyYard, ".operator-enrollment-v1",
	), "managed\n")

	if err := Verify(options, before); err == nil {
		t.Fatal("managed legacy controller passed desired verification")
	}
}

func TestPrepareRejectsCurrentRegistrationWithManagedControllerLeftover(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	dataHome := filepath.Join(root, "data")
	write(t, filepath.Join(configHome, "yards", CurrentYard, "config.env"),
		"YARD_TEMPLATE=test-vms\n")
	write(t, filepath.Join(
		dataHome, "e2e", "controllers", LegacyYard, ".operator-enrollment-v1",
	), "managed\n")
	incus := fakeIncus(t, root)
	setFakeProjects(t, root, "current")

	_, err := Prepare(context.Background(), Options{
		Executable: fakeExecutable(t, root), Incus: incus,
		ConfigHome: configHome, DataHome: dataHome,
		Environment: append(
			os.Environ(),
			"MIGRATION_CALLS="+filepath.Join(root, "calls"),
			"MIGRATION_CONFIG_HOME="+configHome,
		),
	})
	if err == nil || !strings.Contains(err.Error(), "legacy owner state remains") {
		t.Fatalf("canonical registration accepted a legacy controller: %v", err)
	}
}

func TestCommitBoundsScratchAcrossRepeatedOrdinaryFailures(t *testing.T) {
	options, before := ownerFaultFixture(t, true, true)
	options.syncRegistrationFile = func(*os.File) error {
		return errors.New("registration file sync unavailable")
	}
	for attempt := 0; attempt < 3; attempt++ {
		if err := Commit(context.Background(), options, before); err == nil {
			t.Fatalf("attempt %d unexpectedly succeeded", attempt+1)
		}
	}
	assertNoRegistrationScratch(t, filepath.Join(options.ConfigHome, "yards"))
}

func TestCommitRepairsDestinationParentSyncAfterUncertainMkdir(t *testing.T) {
	options, before := ownerFaultFixture(t, true, true)
	calls := 0
	options.syncOwnerDirectory = func(role string, directoryFD int) error {
		if role == "destination-parent" {
			calls++
			if calls == 1 {
				return errors.New("uncertain destination parent sync")
			}
		}
		return unix.Fsync(directoryFD)
	}
	if err := Commit(context.Background(), options, before); err == nil {
		t.Fatal("uncertain mkdir sync was accepted")
	}
	if err := Commit(context.Background(), options, before); err != nil {
		t.Fatalf("retry after uncertain mkdir sync: %v", err)
	}
	if calls < 2 {
		t.Fatalf("destination parent sync calls = %d, want repair sync", calls)
	}
}

func TestCommitRepairsCanonicalDirectorySyncAfterUncertainRename(t *testing.T) {
	options, before := ownerFaultFixture(t, true, true)
	calls := 0
	options.syncOwnerDirectory = func(role string, directoryFD int) error {
		if role == "registration-publication-destination" {
			calls++
			if calls == 1 {
				return errors.New("uncertain canonical directory sync")
			}
		}
		return unix.Fsync(directoryFD)
	}
	if err := Commit(context.Background(), options, before); err == nil {
		t.Fatal("uncertain canonical link sync was accepted")
	}
	if err := Commit(context.Background(), options, before); err != nil {
		t.Fatalf("retry after uncertain canonical link sync: %v", err)
	}
	if calls < 2 {
		t.Fatalf("canonical directory sync calls = %d, want repair sync", calls)
	}
}

func TestCommitRepairsStagingParentSyncAfterUncertainRename(t *testing.T) {
	options, before := ownerFaultFixture(t, true, true)
	calls := 0
	options.syncOwnerDirectory = func(role string, directoryFD int) error {
		if role == "registration-publication-source" {
			calls++
			if calls == 1 {
				return errors.New("uncertain staging parent sync")
			}
		}
		return unix.Fsync(directoryFD)
	}
	if err := Commit(context.Background(), options, before); err == nil {
		t.Fatal("uncertain staging parent sync was accepted")
	}
	if err := Commit(context.Background(), options, before); err != nil {
		t.Fatalf("retry after uncertain staging parent sync: %v", err)
	}
	if calls < 2 {
		t.Fatalf("staging parent sync calls = %d, want repair sync", calls)
	}
}

func TestCommitRepairsOverrideParentSyncsAfterInterruptedRename(t *testing.T) {
	options, before := ownerFaultFixture(t, true, true)
	stop := errors.New("stop after registration publication")
	options.fault = func(point string) error {
		if point == "after-registration-publication" {
			return stop
		}
		return nil
	}
	if err := Commit(context.Background(), options, before); !errors.Is(err, stop) {
		t.Fatalf("prepare interrupted override move: %v", err)
	}
	legacy := filepath.Join(options.ConfigHome, "yards", LegacyYard, "overrides")
	current := filepath.Join(options.ConfigHome, "yards", CurrentYard, "overrides")
	if err := os.Rename(legacy, current); err != nil {
		t.Fatal(err)
	}

	counts := map[string]int{}
	options.syncOwnerDirectory = func(role string, directoryFD int) error {
		counts[role]++
		return unix.Fsync(directoryFD)
	}
	stop = errors.New("stop after repaired override move")
	options.fault = func(point string) error {
		if point == "after-auxiliary-state-move" {
			return stop
		}
		return nil
	}
	if err := Commit(context.Background(), options, before); !errors.Is(err, stop) {
		t.Fatalf("resume interrupted override move: %v", err)
	}
	for _, role := range []string{"override-move-destination", "override-move-source"} {
		if counts[role] == 0 {
			t.Errorf("resume omitted %s repair", role)
		}
	}
}

func TestCompensationRepairsOverrideParentSyncsAfterInterruptedRename(t *testing.T) {
	options, before := ownerFaultFixture(t, true, true)
	legacyRegistration := filepath.Join(
		options.ConfigHome, "yards", LegacyYard, "config.env",
	)
	currentRegistration := filepath.Join(
		options.ConfigHome, "yards", CurrentYard, "config.env",
	)
	write(t, currentRegistration, read(t, legacyRegistration))
	counts := map[string]int{}
	options.syncOwnerDirectory = func(role string, directoryFD int) error {
		counts[role]++
		return unix.Fsync(directoryFD)
	}
	if err := restorePreparedAuxiliaryState(
		options, legacyRegistration, currentRegistration, before,
	); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"override-move-destination", "override-move-source"} {
		if counts[role] == 0 {
			t.Errorf("compensation resume omitted %s repair", role)
		}
	}
}

func TestCommitRepairsControllerParentSyncAfterUncertainMutation(t *testing.T) {
	options, before := ownerFaultFixture(t, true, true)
	calls := 0
	options.syncOwnerDirectory = func(role string, directoryFD int) error {
		if role == "controller-archive-destination" {
			calls++
			if calls == 1 {
				return errors.New("uncertain controller parent sync")
			}
		}
		return unix.Fsync(directoryFD)
	}
	if err := Commit(context.Background(), options, before); err == nil {
		t.Fatal("uncertain controller mutation sync was accepted")
	}
	if err := Commit(context.Background(), options, before); err != nil {
		t.Fatalf("retry after uncertain controller mutation sync: %v", err)
	}
	if calls < 2 {
		t.Fatalf("controller parent sync calls = %d, want repair sync", calls)
	}
}

func TestObserveRejectsUnsafeCanonicalRegistrationMode(t *testing.T) {
	options, before := ownerFaultFixture(t, true, true)
	if err := Commit(context.Background(), options, before); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(options.ConfigHome, "yards", CurrentYard, "config.env")
	if err := os.Chmod(current, 0o666); err != nil {
		t.Fatal(err)
	}
	if progress, err := ObserveProgress(context.Background(), options, before); err == nil {
		t.Fatalf("unsafe canonical mode observed as %q", progress)
	}
}

func TestPublishedCurrentRegistrationAcceptsCompatibleModes(t *testing.T) {
	for _, mode := range []os.FileMode{0o600, 0o640, 0o644} {
		t.Run(mode.String(), func(t *testing.T) {
			parent := filepath.Join(t.TempDir(), "yards")
			if err := os.Mkdir(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(parent, "config.env")
			write(t, path, "YARD_TEMPLATE=test-vms\n")
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			prepared, err := preparedRegistration(StateCurrent, path)
			if err != nil {
				t.Fatal(err)
			}
			exists, err := ownedCurrentRegistration(path)
			if err != nil || !exists {
				t.Fatalf("owned current registration: exists=%t err=%v", exists, err)
			}
			registration, err := openBoundPublishedRegistration(path, prepared)
			if err != nil {
				t.Fatal(err)
			}
			registration.close()
			if mode != 0o600 {
				registration, err = openBoundRegistration(path, prepared)
				if err == nil {
					registration.close()
					t.Fatal("mode-0644 registration accepted as private publication state")
				}
			}
		})
	}
}

func TestCommitAcceptsCompatiblePublishedRegistrationModes(t *testing.T) {
	t.Run("legacy publication becomes private", func(t *testing.T) {
		options, _ := ownerFaultFixture(t, true, true)
		legacy := filepath.Join(options.ConfigHome, "yards", LegacyYard, "config.env")
		if err := os.Chmod(legacy, 0o644); err != nil {
			t.Fatal(err)
		}
		prepared, err := Prepare(context.Background(), options)
		if err != nil {
			t.Fatal(err)
		}
		if err := Commit(context.Background(), options, prepared); err != nil {
			t.Fatal(err)
		}
		current := filepath.Join(options.ConfigHome, "yards", CurrentYard, "config.env")
		info, err := os.Stat(current)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Fatalf("new current registration mode = %o, want 600", mode)
		}
	})

	t.Run("current publication remains valid", func(t *testing.T) {
		options, prepared := ownerFaultFixture(t, true, true)
		if err := Commit(context.Background(), options, prepared); err != nil {
			t.Fatal(err)
		}
		current := filepath.Join(options.ConfigHome, "yards", CurrentYard, "config.env")
		if err := os.Chmod(current, 0o644); err != nil {
			t.Fatal(err)
		}
		prepared, err := Prepare(context.Background(), options)
		if err != nil {
			t.Fatal(err)
		}
		if err := Commit(context.Background(), options, prepared); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCommitRejectsUnsafeRetainedStagingParentMode(t *testing.T) {
	options, before := ownerFaultFixture(t, true, true)
	yards := filepath.Join(options.ConfigHome, "yards")
	options.openRegistrationStaging = func(parentFD int, name string) (int, error) {
		if err := os.Chmod(yards, 0o777); err != nil {
			return -1, err
		}
		return unix.Openat(
			parentFD, name,
			unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW,
			0o600,
		)
	}
	if err := Commit(context.Background(), options, before); err == nil {
		t.Fatal("shared-writable retained staging parent was accepted")
	}
}

func TestCommitRejectsUnsafeRetainedDestinationDirectoryMode(t *testing.T) {
	options, before := ownerFaultFixture(t, true, true)
	options.fault = func(point string) error {
		if point != "before-registration-publication-cas" {
			return nil
		}
		return os.Chmod(filepath.Join(options.ConfigHome, "yards", CurrentYard), 0o777)
	}
	if err := Commit(context.Background(), options, before); err == nil {
		t.Fatal("shared-writable retained destination directory was accepted")
	}
}

func TestCommitPostvalidatesOverrideRenameAgainstRetainedSource(t *testing.T) {
	options, before := ownerFaultFixture(t, true, true)
	source := filepath.Join(options.ConfigHome, "yards", LegacyYard, "overrides")
	authorized := source + ".authorized"
	seen := false
	options.fault = func(point string) error {
		if point != "after-auxiliary-state-validation-before-rename" {
			return nil
		}
		seen = true
		if err := os.Rename(source, authorized); err != nil {
			return err
		}
		write(t, filepath.Join(source, "replacement.env"), "REPLACED=1\n")
		return nil
	}
	if err := Commit(context.Background(), options, before); err == nil {
		t.Fatal("wrong override source rename was accepted")
	}
	if !seen {
		t.Fatal("postvalidation override rename boundary was not exercised")
	}
	assertExactTestFile(t, filepath.Join(source, "replacement.env"), "REPLACED=1\n", 0o600)
	assertExactTestFile(t, filepath.Join(authorized, "runtime.env"), "NESTED_E2E_VMS=0\n", 0o600)
}

func TestCommitPostvalidatesRegistrationArchiveAgainstRetainedSource(t *testing.T) {
	options, before := ownerFaultFixture(t, true, true)
	source := filepath.Join(options.ConfigHome, "yards", LegacyYard, "config.env")
	authorized := source + ".authorized"
	seen := false
	options.fault = func(point string) error {
		if point != "after-legacy-registration-validation-before-archive" {
			return nil
		}
		seen = true
		if err := os.Rename(source, authorized); err != nil {
			return err
		}
		write(t, source, "YARD_TEMPLATE=test-vms\nREPLACED=1\n")
		return nil
	}
	if err := Commit(context.Background(), options, before); err == nil {
		t.Fatal("wrong registration source archive was accepted")
	}
	if !seen {
		t.Fatal("postvalidation registration archive boundary was not exercised")
	}
	assertExactTestFile(t, source, "YARD_TEMPLATE=test-vms\nREPLACED=1\n", 0o600)
	assertExactTestFile(t, authorized, "YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n", 0o600)
}

func TestCommitPostvalidatesControllerArchiveAgainstRetainedSource(t *testing.T) {
	options, before := ownerFaultFixture(t, true, true)
	source := filepath.Join(options.DataHome, "e2e", "controllers", LegacyYard)
	authorized := source + ".authorized"
	seen := false
	options.fault = func(point string) error {
		if point != "after-legacy-controller-validation-before-archive" {
			return nil
		}
		seen = true
		if err := os.Rename(source, authorized); err != nil {
			return err
		}
		write(t, filepath.Join(source, ".operator-enrollment-v1"), "managed\n")
		return nil
	}
	if err := Commit(context.Background(), options, before); err == nil {
		t.Fatal("wrong controller source archive was accepted")
	}
	if !seen {
		t.Fatal("postvalidation controller archive boundary was not exercised")
	}
	assertExactTestFile(t, filepath.Join(source, ".operator-enrollment-v1"), "managed\n", 0o600)
	assertExactTestFile(t, filepath.Join(authorized, ".operator-enrollment-v1"), "managed\n", 0o600)
}

func TestMisdirectedCrossDirectoryRenamePersistsBothRestoredParents(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "source", "state")
	destinationPath := filepath.Join(root, "destination", "state")
	write(t, sourcePath, "state\n")
	if err := os.MkdirAll(filepath.Dir(destinationPath), 0o700); err != nil {
		t.Fatal(err)
	}
	source, err := openBoundObject(sourcePath, unix.O_RDONLY|unix.O_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	defer source.close()
	destinationParent, destinationName, err := openParent(destinationPath)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(destinationParent)

	var synced []string
	options := Options{syncOwnerDirectory: func(role string, _ int) error {
		synced = append(synced, role)
		return nil
	}}
	validationErr := errors.New("reject renamed object")
	err = renameBoundObject(
		options, &source, destinationParent, destinationName,
		unix.O_RDONLY|unix.O_NONBLOCK,
		func(int) error { return validationErr }, "", "test-rename", "",
	)
	if !errors.Is(err, validationErr) {
		t.Fatalf("rename error = %v, want %v", err, validationErr)
	}
	if got, want := strings.Join(synced, ","),
		"test-rename-restore-destination,test-rename-restore"; got != want {
		t.Fatalf("restored parent syncs = %q, want %q", got, want)
	}
	assertExactTestFile(t, sourcePath, "state\n", 0o600)
	if _, err := os.Lstat(destinationPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("misdirected destination remains after restore: %v", err)
	}
}

func TestPrepareIgnoresAndPreservesForeignFlatStaging(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	write(t, filepath.Join(configHome, "yards", LegacyYard+".env"),
		"YARD_TEMPLATE=test-vms\n")
	pending := filepath.Join(
		configHome, "yards", ".test-yard.env.owner-migration.pending",
	)
	write(t, pending, "foreign pending bytes\n")
	options := ownerOptions(t, root, configHome)

	prepared, err := Prepare(context.Background(), options)
	if err != nil || prepared.State != StateLegacyFlat {
		t.Fatalf("foreign flat staging blocked preparation: %#v, %v", prepared, err)
	}
	if payload := read(t, pending); payload != "foreign pending bytes\n" {
		t.Fatalf("foreign flat pending state was changed: %q", payload)
	}
}

func TestCommitTerminalCleanupRemovesBoundOwnerArchives(t *testing.T) {
	options, before := controllerFaultFixture(t)
	if err := Commit(context.Background(), options, before); err != nil {
		t.Fatal(err)
	}
	registrationState, err := preparedRegistrationArchive(options, before)
	if err != nil {
		t.Fatal(err)
	}
	registrationArchive := registrationState.archive
	controllerArchive, err := ownerArchivePath(filepath.Join(
		options.DataHome, "e2e", "controllers",
	), "controller", options.RecoveryToken)
	if err != nil {
		t.Fatal(err)
	}
	assertTestPathPresent(t, registrationArchive)
	assertTestPathPresent(t, controllerArchive)

	options.TerminalCleanup = true
	if err := Commit(context.Background(), options, before); err != nil {
		t.Fatal(err)
	}
	assertTestPathAbsent(t, registrationArchive)
	assertTestPathAbsent(t, controllerArchive)
	progress, err := ObserveProgress(context.Background(), options, before)
	if err != nil || progress != ProgressDesired {
		t.Fatalf("terminal owner progress = %q, err=%v", progress, err)
	}
}

func TestCommitTerminalCleanupAllowsAbsentControllerArchive(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	write(t, filepath.Join(configHome, "yards", LegacyYard, "config.env"),
		"YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n")
	t.Setenv("MIGRATION_SHARED_IMAGES", "1")
	options := ownerOptions(t, root, configHome)
	before, err := Prepare(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if err := Commit(context.Background(), options, before); err != nil {
		t.Fatal(err)
	}

	options.TerminalCleanup = true
	if err := Commit(context.Background(), options, before); err != nil {
		t.Fatal(err)
	}
}

func TestCommitResumesTerminalOwnerArchiveCleanup(t *testing.T) {
	for _, point := range []string{
		"after-registration-archive-cleanup",
		"after-controller-archive-cleanup",
	} {
		t.Run(point, func(t *testing.T) {
			options, before := controllerFaultFixture(t)
			if err := Commit(context.Background(), options, before); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("terminal owner cleanup interrupted")
			reached := false
			options.TerminalCleanup = true
			options.fault = func(actual string) error {
				if actual == point && !reached {
					reached = true
					return injected
				}
				return nil
			}
			if err := Commit(context.Background(), options, before); !errors.Is(err, injected) {
				t.Fatalf("terminal cleanup fault %q: %v", point, err)
			}
			if !reached {
				t.Fatalf("terminal cleanup did not reach %q", point)
			}
			options.fault = nil
			progress, err := ObserveProgress(context.Background(), options, before)
			if err != nil || progress != ProgressDesired {
				t.Fatalf("terminal cleanup progress = %q, err=%v", progress, err)
			}
			if err := Commit(context.Background(), options, before); err != nil {
				t.Fatalf("resume terminal cleanup at %q: %v", point, err)
			}
		})
	}
}

func TestCommitCleansPartiallyRemovedOwnerArchives(t *testing.T) {
	for name, partial := range map[string]func(t *testing.T, registration, controller string){
		"registration": func(t *testing.T, registration, _ string) {
			t.Helper()
			if err := os.Remove(filepath.Join(registration, "config.env")); err != nil {
				t.Fatal(err)
			}
		},
		"controller": func(t *testing.T, _, controller string) {
			t.Helper()
			if err := os.Remove(filepath.Join(controller, ".operator-enrollment-v1")); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			options, before := controllerFaultFixture(t)
			if err := Commit(context.Background(), options, before); err != nil {
				t.Fatal(err)
			}
			registration, err := preparedRegistrationArchive(options, before)
			if err != nil {
				t.Fatal(err)
			}
			controller, err := ownerArchivePath(filepath.Join(
				options.DataHome, "e2e", "controllers",
			), "controller", options.RecoveryToken)
			if err != nil {
				t.Fatal(err)
			}
			partial(t, registration.archive, controller)
			options.TerminalCleanup = true
			if err := Commit(context.Background(), options, before); err != nil {
				t.Fatal(err)
			}
			assertTestPathAbsent(t, registration.archive)
			assertTestPathAbsent(t, controller)
		})
	}
}

func TestCommitIgnoresAndPreservesUnsafeForeignFlatStaging(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	legacyRegistration := filepath.Join(configHome, "yards", LegacyYard+".env")
	currentRegistration := filepath.Join(configHome, "yards", CurrentYard+".env")
	registration := "YARD_TEMPLATE=test-vms\n"
	write(t, legacyRegistration, registration)
	options := ownerOptions(t, root, configHome)
	before, err := Prepare(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	pending := filepath.Join(
		configHome,
		"yards",
		".test-yard.env.owner-migration."+before.RegistrationDigest+".pending",
	)
	write(t, pending, registration)
	if err := os.Chmod(pending, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := Commit(context.Background(), options, before); err != nil {
		t.Fatalf("foreign staging blocked publication: %v", err)
	}
	if payload := read(t, pending); payload != registration {
		t.Fatalf("unsafe pending bytes changed: %q", payload)
	}
	info, err := os.Stat(pending)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("unsafe pending mode changed: %v", info.Mode())
	}
	assertExactTestFile(t, currentRegistration, registration, 0o600)
	progress, err := ObserveProgress(context.Background(), options, before)
	if err != nil || progress != ProgressDesired {
		t.Fatalf("foreign staging blocked desired progress: %q, %v", progress, err)
	}
}

func TestCommitDoesNotPublishWhenRegistrationFileSyncFails(t *testing.T) {
	options, before := flatOwnerFaultFixture(t)
	syncErr := errors.New("registration fsync failed")
	options.syncRegistrationFile = func(*os.File) error { return syncErr }

	err := Commit(context.Background(), options, before)
	if !errors.Is(err, syncErr) {
		t.Fatalf("registration sync error = %v, want %v", err, syncErr)
	}
	yards := filepath.Join(options.ConfigHome, "yards")
	assertExactTestFile(t, filepath.Join(yards, LegacyYard+".env"),
		"YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n", 0o600)
	assertTestPathAbsent(t, filepath.Join(yards, CurrentYard+".env"))
	assertNoRegistrationScratch(t, yards)
}

func TestCommitPortableStagingFailureDoesNotCreateCanonicalDirectory(t *testing.T) {
	options, before := ownerFaultFixture(t, true, false)
	options.openRegistrationStaging = func(int, string) (int, error) {
		return -1, syscall.EOPNOTSUPP
	}

	err := Commit(context.Background(), options, before)
	if err == nil || !strings.Contains(err.Error(), "stage owner registration") {
		t.Fatalf("portable staging failure = %v", err)
	}
	yards := filepath.Join(options.ConfigHome, "yards")
	assertExactDirectoryEntries(t, yards, []string{LegacyYard})
	assertTestPathAbsent(t, filepath.Join(yards, CurrentYard))
	assertTestPathAbsent(t, filepath.Join(yards, CurrentYard, "config.env"))

	options.openRegistrationStaging = nil
	if err := Commit(context.Background(), options, before); err != nil {
		t.Fatalf("retry after zero-state staging failure: %v", err)
	}
	assertDesiredOwnerFacts(t, options, before)
}

func TestDestinationParentSyncPrecedesDirectoryCreationHook(t *testing.T) {
	options, before := ownerFaultFixture(t, true, false)
	syncErr := errors.New("destination parent fsync failed")
	injected := errors.New("directory creation hook ran before fsync")
	hookReached := false
	options.syncOwnerDirectory = func(point string, _ int) error {
		if point == "destination-parent" {
			return syncErr
		}
		return nil
	}
	options.fault = func(point string) error {
		if point == "after-destination-directory-creation" {
			hookReached = true
			return injected
		}
		return nil
	}

	err := Commit(context.Background(), options, before)
	if !errors.Is(err, syncErr) {
		t.Fatalf("destination parent sync error = %v, want %v", err, syncErr)
	}
	if hookReached {
		t.Fatal("directory creation hook ran before its parent fsync succeeded")
	}
	currentDirectory := filepath.Join(options.ConfigHome, "yards", CurrentYard)
	assertTestPathPresent(t, currentDirectory)
	assertTestPathAbsent(t, filepath.Join(currentDirectory, "config.env"))

	options.syncOwnerDirectory = nil
	options.fault = nil
	if err := Commit(context.Background(), options, before); err != nil {
		t.Fatalf("retry after parent sync failure: %v", err)
	}
	assertDesiredOwnerFacts(t, options, before)
}

func TestRegistrationPublicationSyncPrecedesPublicationHook(t *testing.T) {
	options, before := flatOwnerFaultFixture(t)
	syncErr := errors.New("registration publication fsync failed")
	injected := errors.New("publication hook ran before fsync")
	hookReached := false
	options.syncOwnerDirectory = func(point string, _ int) error {
		if point == "registration-publication-destination" {
			return syncErr
		}
		return nil
	}
	options.fault = func(point string) error {
		if point == "after-registration-publication" {
			hookReached = true
			return injected
		}
		return nil
	}

	err := Commit(context.Background(), options, before)
	if !errors.Is(err, syncErr) {
		t.Fatalf("registration publication sync error = %v, want %v", err, syncErr)
	}
	if hookReached {
		t.Fatal("registration publication hook ran before its directory fsync succeeded")
	}
	yards := filepath.Join(options.ConfigHome, "yards")
	assertExactTestFile(t, filepath.Join(yards, CurrentYard+".env"),
		"YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n", 0o600)
	assertExactTestFile(t, filepath.Join(yards, LegacyYard+".env"),
		"YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n", 0o600)

	options.syncOwnerDirectory = nil
	options.fault = nil
	if err := Commit(context.Background(), options, before); err != nil {
		t.Fatalf("retry after publication sync failure: %v", err)
	}
	assertDesiredFlatOwnerFacts(t, options)
}

func TestCommitRejectsStagingPathSwapBeforePublication(t *testing.T) {
	options, before := flatOwnerFaultFixture(t)
	yards := filepath.Join(options.ConfigHome, "yards")
	var staging, heldName string
	swapped := false
	options.fault = func(point string) error {
		if point != "before-registration-publication-cas" || swapped {
			return nil
		}
		entries, err := os.ReadDir(yards)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".owner-migration-registration-scratch.") {
				staging = filepath.Join(yards, entry.Name())
				break
			}
		}
		if staging == "" {
			t.Fatal("publication CAS boundary had no retained staging name")
		}
		heldName = staging + ".held"
		if err := os.Rename(staging, heldName); err != nil {
			t.Fatal(err)
		}
		write(t, staging, "YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n")
		swapped = true
		return nil
	}

	err := Commit(context.Background(), options, before)
	if err == nil || !strings.Contains(err.Error(), "no longer names") {
		t.Fatalf("replacement staging path was accepted: %v", err)
	}
	if !swapped {
		t.Fatal("publication did not expose its held-inode CAS boundary")
	}
	assertTestPathAbsent(t, filepath.Join(yards, CurrentYard+".env"))
	assertExactTestFile(t, heldName, "YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n", 0o600)
	assertExactTestFile(t, staging, "YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n", 0o600)
}

func TestCommitRejectsHardlinkedStagingAtPublicationCAS(t *testing.T) {
	options, before := flatOwnerFaultFixture(t)
	yards := filepath.Join(options.ConfigHome, "yards")
	var staging, foreignLink string
	options.fault = func(point string) error {
		if point != "before-registration-publication-cas" || staging != "" {
			return nil
		}
		entries, err := os.ReadDir(yards)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ownerRecoveryPrefix+"registration-scratch.") {
				staging = filepath.Join(yards, entry.Name())
				break
			}
		}
		foreignLink = staging + ".foreign"
		if staging == "" {
			t.Fatal("publication CAS boundary had no staging file")
		}
		if err := os.Link(staging, foreignLink); err != nil {
			t.Fatal(err)
		}
		return nil
	}

	err := Commit(context.Background(), options, before)
	if staging == "" {
		t.Fatal("publication did not expose its staging link-count CAS boundary")
	}
	if err == nil || !strings.Contains(err.Error(), "staging") {
		t.Fatalf("hardlinked staging file was published: %v", err)
	}
	assertTestPathAbsent(t, filepath.Join(yards, CurrentYard+".env"))
	assertTestPathAbsent(t, staging)
	assertExactTestFile(t, foreignLink, "YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n", 0o600)
}

func TestCommitRejectsDestinationDirectorySwapAtPublicationCAS(t *testing.T) {
	options, before := ownerFaultFixture(t, true, false)
	currentDirectory := filepath.Join(options.ConfigHome, "yards", CurrentYard)
	backup := currentDirectory + ".original"
	swapped := false
	options.fault = func(point string) error {
		if point != "before-registration-publication-cas" || swapped {
			return nil
		}
		swapped = true
		if err := os.Rename(currentDirectory, backup); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(currentDirectory, 0o700); err != nil {
			t.Fatal(err)
		}
		return nil
	}

	err := Commit(context.Background(), options, before)
	if !swapped {
		t.Fatal("publication did not expose its destination directory CAS boundary")
	}
	if err == nil || !strings.Contains(err.Error(), "directory changed") {
		t.Fatalf("replacement destination directory was accepted: %v", err)
	}
	assertTestPathAbsent(t, filepath.Join(currentDirectory, "config.env"))
	assertTestPathAbsent(t, filepath.Join(backup, "config.env"))
}

func TestObserveProgressRejectsChangedCurrentRegistrationAfterLegacyRemoval(t *testing.T) {
	options, before := ownerFaultFixture(t, true, false)
	if err := Commit(context.Background(), options, before); err != nil {
		t.Fatal(err)
	}
	currentRegistration := filepath.Join(
		options.ConfigHome, "yards", CurrentYard, "config.env",
	)
	write(t, currentRegistration, "YARD_TEMPLATE=test-vms\nSSH_PORT=2299\n")

	if _, err := ObserveProgress(context.Background(), options, before); err == nil ||
		!strings.Contains(err.Error(), "changed") {
		t.Fatalf("changed desired registration was accepted: %v", err)
	}
	if err := Verify(options, before); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("changed desired registration passed verification: %v", err)
	}
}

func TestObserveProgressRejectsProjectImageNamespaceChangedAfterPrepare(t *testing.T) {
	options, before := ownerFaultFixture(t, true, false)
	options.Environment = withEnvironment(
		options.Environment,
		"MIGRATION_SHARED_IMAGES",
		"0",
	)

	if _, err := ObserveProgress(context.Background(), options, before); err == nil ||
		!strings.Contains(err.Error(), "image namespace changed") {
		t.Fatalf("changed image namespace was accepted: %v", err)
	}
	if err := Commit(context.Background(), options, before); err == nil ||
		!strings.Contains(err.Error(), "image namespace changed") {
		t.Fatalf("changed image namespace reached mutation: %v", err)
	}
	currentRegistration := filepath.Join(
		options.ConfigHome, "yards", CurrentYard, "config.env",
	)
	if _, err := os.Lstat(currentRegistration); !os.IsNotExist(err) {
		t.Fatalf("image namespace drift published canonical registration: %v", err)
	}
}

func TestCommitResumesAfterDestinationDirectoryCreation(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	write(t, filepath.Join(configHome, "yards", LegacyYard, "config.env"),
		"YARD_TEMPLATE=test-vms\n")
	options := Options{
		Executable: fakeExecutable(t, root), Incus: fakeIncus(t, root),
		ConfigHome: configHome, DataHome: filepath.Join(root, "data"),
		RecoveryToken: round3RecoveryToken,
		Environment: append(
			os.Environ(),
			"MIGRATION_CALLS="+filepath.Join(root, "calls"),
			"MIGRATION_CONFIG_HOME="+configHome,
		),
	}
	before, err := Prepare(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(configHome, "yards", CurrentYard), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := Commit(context.Background(), options, before); err != nil {
		t.Fatalf("resume after destination directory creation: %v", err)
	}
	if err := Verify(options, before); err != nil {
		t.Fatal(err)
	}
}

func TestCommitResumesCompensationWithNoOwnerProjectForPrivateImages(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	write(t, filepath.Join(configHome, "yards", LegacyYard, "config.env"),
		"YARD_TEMPLATE=test-vms\n")
	incus := fakeIncus(t, root)
	t.Setenv("MIGRATION_SHARED_IMAGES", "0")
	options := Options{
		Executable: fakeExecutable(t, root), Incus: incus,
		ConfigHome: configHome, DataHome: filepath.Join(root, "data"),
		RecoveryToken: round3RecoveryToken,
		Environment: append(
			os.Environ(),
			"MIGRATION_CALLS="+filepath.Join(root, "calls"),
			"MIGRATION_CONFIG_HOME="+configHome,
		),
	}
	before, err := Prepare(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	setFakeProjects(t, root, "none")

	if err := Commit(context.Background(), options, before); err != nil {
		t.Fatalf("resume from compensation without an owner project: %v", err)
	}
	if err := Verify(options, before); err != nil {
		t.Fatal(err)
	}
}

func TestObserveProgressAndCommitResumeAuthorizedIntermediateStates(t *testing.T) {
	for _, test := range []struct {
		name, projects string
		before         State
		legacy         bool
		controller     bool
		movedOverrides bool
	}{
		{name: "registration copied", projects: "legacy", before: StateLegacyDirectory, legacy: true},
		{name: "current project created", projects: "both", before: StateLegacyDirectory, legacy: true},
		{name: "legacy project removed", projects: "current", before: StateLegacyDirectory, legacy: true},
		{
			name: "auxiliary state moved", projects: "current",
			before: StateLegacyDirectoryOverrides, legacy: true, movedOverrides: true,
		},
		{
			name: "legacy registration removed", projects: "current",
			before: StateLegacyDirectory, controller: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			configHome := filepath.Join(root, "config")
			dataHome := filepath.Join(root, "data")
			oldRegistration := filepath.Join(configHome, "yards", LegacyYard, "config.env")
			currentRegistration := filepath.Join(configHome, "yards", CurrentYard, "config.env")
			if test.legacy {
				write(t, oldRegistration, "YARD_TEMPLATE=test-vms\n")
			}
			write(t, currentRegistration, "YARD_TEMPLATE=test-vms\n")
			if test.movedOverrides {
				write(t, filepath.Join(
					configHome, "yards", CurrentYard, "overrides", "runtime.env",
				), "NESTED_E2E_VMS=0\n")
			}
			if test.controller {
				write(t, filepath.Join(
					dataHome, "e2e", "controllers", LegacyYard, ".operator-enrollment-v1",
				), "managed\n")
			}
			incus := fakeIncus(t, root)
			setFakeProjects(t, root, test.projects)
			options := Options{
				Executable: fakeExecutable(t, root), Incus: incus,
				ConfigHome: configHome, DataHome: dataHome,
				RecoveryToken: round3RecoveryToken,
				Environment: append(
					os.Environ(),
					"MIGRATION_CALLS="+filepath.Join(root, "calls"),
					"MIGRATION_CONFIG_HOME="+configHome,
				),
			}
			prepared, err := preparedRegistration(test.before, currentRegistration)
			if err != nil {
				t.Fatal(err)
			}
			auxiliaryRegistration := oldRegistration
			if test.movedOverrides || !test.legacy {
				auxiliaryRegistration = currentRegistration
			}
			if err := bindPreparedAuxiliaryFacts(
				options, auxiliaryRegistration, &prepared,
			); err != nil {
				t.Fatal(err)
			}
			prepared.SharedImages = true
			if !test.legacy {
				write(t, filepath.Join(
					configHome, "yards",
					".owner-migration-registration-archive."+round3RecoveryToken,
					"config.env",
				), "YARD_TEMPLATE=test-vms\n")
			}
			progress, err := ObserveProgress(context.Background(), options, prepared)
			if err != nil || progress != ProgressInProgress {
				t.Fatalf("intermediate progress = %q, err=%v", progress, err)
			}
			if err := Commit(context.Background(), options, prepared); err != nil {
				t.Fatal(err)
			}
			progress, err = ObserveProgress(context.Background(), options, prepared)
			if err != nil || progress != ProgressDesired {
				t.Fatalf("completed progress = %q, err=%v", progress, err)
			}
		})
	}
}

func TestCommitResumesAfterEveryInjectedForwardMutation(t *testing.T) {
	tests := []struct {
		name         string
		point        string
		sharedImages bool
		overrides    bool
		progress     Progress
	}{
		{
			name: "registration staging fsync", point: "after-registration-staging-fsync",
			sharedImages: true, progress: ProgressExpected,
		},
		{
			name: "destination directory creation", point: "after-destination-directory-creation",
			sharedImages: true, progress: ProgressExpected,
		},
		{
			name: "registration publication", point: "after-registration-publication",
			sharedImages: true, progress: ProgressInProgress,
		},
		{
			name: "current project preparation", point: "after-current-project-prepare",
			sharedImages: true, progress: ProgressInProgress,
		},
		{
			name: "private images legacy teardown", point: "after-legacy-teardown",
			progress: ProgressInProgress,
		},
		{
			name: "auxiliary state move", point: "after-auxiliary-state-move",
			sharedImages: true, overrides: true, progress: ProgressInProgress,
		},
		{
			name: "legacy registration archive", point: "after-legacy-registration-archive",
			sharedImages: true, progress: ProgressInProgress,
		},
		{
			name: "current init", point: "after-current-init",
			sharedImages: true, progress: ProgressInProgress,
		},
		{
			name: "current check", point: "after-current-check",
			sharedImages: true, progress: ProgressInProgress,
		},
		{
			name: "legacy controller archive", point: "after-legacy-controller-archive",
			sharedImages: true, progress: ProgressDesired,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options, before := ownerFaultFixture(t, test.sharedImages, test.overrides)
			injected := errors.New("injected fail-stop after " + test.point)
			observed := false
			options.fault = func(point string) error {
				if point == test.point && !observed {
					observed = true
					return injected
				}
				return nil
			}
			if err := Commit(context.Background(), options, before); !errors.Is(err, injected) {
				t.Fatalf("fault %q error = %v", test.point, err)
			}
			if !observed {
				t.Fatalf("production mutation did not reach fault %q", test.point)
			}
			assertForwardOwnerBoundaryFacts(t, options, before, test.point)

			options.fault = nil
			progress, err := ObserveProgress(context.Background(), options, before)
			if err != nil || progress != test.progress {
				t.Fatalf("fault %q progress = %q, want %q, err=%v",
					test.point, progress, test.progress, err)
			}
			if err := Commit(context.Background(), options, before); err != nil {
				t.Fatalf("fault %q resume: %v", test.point, err)
			}
			assertDesiredOwnerFacts(t, options, before)
		})
	}
}

func TestCommitResumesAfterEveryInjectedCompensationMutation(t *testing.T) {
	tests := []struct {
		name      string
		point     string
		overrides bool
		progress  Progress
	}{
		{
			name: "legacy registration recreation", point: "after-compensation-registration-recreation",
			progress: ProgressInProgress,
		},
		{
			name: "current teardown", point: "after-compensation-current-teardown",
			progress: ProgressInProgress,
		},
		{
			name: "auxiliary state restore", point: "after-compensation-auxiliary-restore",
			overrides: true, progress: ProgressInProgress,
		},
		{
			name: "current registration removal", point: "after-compensation-current-registration-removal",
			progress: ProgressInProgress,
		},
		{
			name: "legacy project recreation", point: "after-compensation-legacy-project-recreation",
			progress: ProgressExpected,
		},
		{
			name: "legacy init", point: "after-compensation-legacy-init",
			progress: ProgressExpected,
		},
		{
			name: "legacy check", point: "after-compensation-legacy-check",
			progress: ProgressExpected,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options, before := ownerFaultFixture(t, true, test.overrides)
			options.Environment = withEnvironment(
				options.Environment,
				"MIGRATION_FAIL",
				"test-yard:init",
			)
			injected := errors.New("injected fail-stop after " + test.point)
			observed := false
			options.fault = func(point string) error {
				if point == test.point && !observed {
					observed = true
					return injected
				}
				return nil
			}
			if err := Commit(context.Background(), options, before); !errors.Is(err, injected) {
				t.Fatalf("compensation fault %q error = %v", test.point, err)
			}
			if !observed {
				t.Fatalf("production compensation did not reach fault %q", test.point)
			}
			assertCompensationOwnerBoundaryFacts(t, options, before, test.point)

			options.fault = nil
			options.Environment = withoutEnvironment(options.Environment, "MIGRATION_FAIL")
			progress, err := ObserveProgress(context.Background(), options, before)
			if err != nil || progress != test.progress {
				t.Fatalf("compensation fault %q progress = %q, want %q, err=%v",
					test.point, progress, test.progress, err)
			}
			if err := Commit(context.Background(), options, before); err != nil {
				t.Fatalf("compensation fault %q resume: %v", test.point, err)
			}
			assertDesiredOwnerFacts(t, options, before)
		})
	}
}

func TestCompensationRejectsSameContentOverrideSwapAtRestoreCAS(t *testing.T) {
	options, before := ownerFaultFixture(t, true, true)
	options.Environment = withEnvironment(
		options.Environment, "MIGRATION_FAIL", "test-yard:init",
	)
	currentOverrides := filepath.Join(options.ConfigHome, "yards", CurrentYard, "overrides")
	backup := currentOverrides + ".original"
	swapped := false
	options.fault = func(point string) error {
		if point != "before-compensation-auxiliary-restore-cas" || swapped {
			return nil
		}
		swapped = true
		if err := os.Rename(currentOverrides, backup); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(currentOverrides, "runtime.env"), "NESTED_E2E_VMS=0\n")
		return nil
	}

	err := Commit(context.Background(), options, before)
	if !swapped {
		t.Fatal("compensation override restore did not expose its identity CAS boundary")
	}
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("compensation moved a replacement override root: %v", err)
	}
	assertExactTestFile(t, filepath.Join(backup, "runtime.env"),
		"NESTED_E2E_VMS=0\n", 0o600)
	assertExactTestFile(t, filepath.Join(currentOverrides, "runtime.env"),
		"NESTED_E2E_VMS=0\n", 0o600)
}

func TestCompensationRejectsSameContentRegistrationSwapAtRemovalCAS(t *testing.T) {
	options, before := ownerFaultFixture(t, true, false)
	options.Environment = withEnvironment(
		options.Environment, "MIGRATION_FAIL", "test-yard:init",
	)
	currentRegistration := filepath.Join(
		options.ConfigHome, "yards", CurrentYard, "config.env",
	)
	backup := currentRegistration + ".original"
	swapped := false
	options.fault = func(point string) error {
		if point != "after-compensation-current-registration-validation-before-removal" || swapped {
			return nil
		}
		swapped = true
		if err := os.Rename(currentRegistration, backup); err != nil {
			t.Fatal(err)
		}
		write(t, currentRegistration, "YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n")
		return nil
	}

	err := Commit(context.Background(), options, before)
	if !swapped {
		t.Fatal("compensation registration removal did not expose its identity CAS boundary")
	}
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("compensation removed a replacement registration: %v", err)
	}
	assertExactTestFile(t, backup, "YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n", 0o600)
	assertExactTestFile(t, currentRegistration,
		"YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n", 0o600)
}

func TestCommitResumesFlatRegistrationPublicationWithExactFacts(t *testing.T) {
	for _, test := range []struct {
		name, point string
		progress    Progress
	}{
		{name: "staging fsync", point: "after-registration-staging-fsync", progress: ProgressExpected},
		{name: "canonical publication", point: "after-registration-publication", progress: ProgressInProgress},
	} {
		t.Run(test.name, func(t *testing.T) {
			options, before := flatOwnerFaultFixture(t)
			injected := errors.New("injected flat publication fault")
			options.fault = func(point string) error {
				if point == test.point {
					return injected
				}
				return nil
			}
			if err := Commit(context.Background(), options, before); !errors.Is(err, injected) {
				t.Fatalf("flat fault %q error = %v", test.point, err)
			}
			assertFlatPublicationBoundaryFacts(t, options, before, test.point)
			options.fault = nil
			progress, err := ObserveProgress(context.Background(), options, before)
			if err != nil || progress != test.progress {
				t.Fatalf("flat fault %q progress = %q, want %q, err=%v",
					test.point, progress, test.progress, err)
			}
			if err := Commit(context.Background(), options, before); err != nil {
				t.Fatalf("flat fault %q resume: %v", test.point, err)
			}
			assertDesiredFlatOwnerFacts(t, options)
		})
	}
}

func TestCommitMigratesFlatRegistrationWithGeneratedProjectState(t *testing.T) {
	options, before := flatOwnerFaultFixture(t)
	write(t, filepath.Join(
		options.ConfigHome, "yards", LegacyYard, "projects", ".lock",
	), "")

	progress, err := ObserveProgress(context.Background(), options, before)
	if err != nil || progress != ProgressExpected {
		t.Fatalf("generated flat owner progress = %q, err=%v", progress, err)
	}
	if err := Commit(context.Background(), options, before); err != nil {
		t.Fatalf("migrate generated flat owner state: %v", err)
	}
	progress, err = ObserveProgress(context.Background(), options, before)
	if err != nil || progress != ProgressDesired {
		t.Fatalf("migrated flat owner progress = %q, err=%v", progress, err)
	}
	assertDesiredFlatOwnerFacts(t, options)
}

func TestCommitResumesFlatGeneratedProjectStateAfterLegacyTeardown(t *testing.T) {
	options, before := flatOwnerFaultFixture(t)
	write(t, filepath.Join(
		options.ConfigHome, "yards", LegacyYard, "projects", ".lock",
	), "")
	injected := errors.New("injected legacy teardown boundary")
	options.fault = func(point string) error {
		if point == "after-legacy-teardown" {
			return injected
		}
		return nil
	}
	if err := Commit(context.Background(), options, before); !errors.Is(err, injected) {
		t.Fatalf("legacy teardown fault = %v, want %v", err, injected)
	}
	progress, err := ObserveProgress(context.Background(), options, before)
	if err != nil || progress != ProgressInProgress {
		t.Fatalf("flat teardown progress = %q, err=%v", progress, err)
	}
	options.fault = nil
	if err := Commit(context.Background(), options, before); err != nil {
		t.Fatalf("resume flat teardown: %v", err)
	}
	assertDesiredFlatOwnerFacts(t, options)
}

func TestCommitResumesAdoptCurrentBoundariesWithExactFacts(t *testing.T) {
	for _, test := range []struct {
		name, point string
		progress    Progress
	}{
		{name: "staging fsync", point: "after-registration-staging-fsync", progress: ProgressExpected},
		{name: "canonical publication", point: "after-registration-publication", progress: ProgressInProgress},
		{
			name: "legacy registration archive", point: "after-legacy-registration-archive",
			progress: ProgressInProgress,
		},
		{name: "current check", point: "after-current-check", progress: ProgressInProgress},
		{name: "controller archive", point: "after-legacy-controller-archive", progress: ProgressDesired},
	} {
		t.Run(test.name, func(t *testing.T) {
			options, before := adoptCurrentOwnerFaultFixture(t)
			injected := errors.New("injected adopt-current fault")
			options.fault = func(point string) error {
				if point == test.point {
					return injected
				}
				return nil
			}
			if err := Commit(context.Background(), options, before); !errors.Is(err, injected) {
				t.Fatalf("adopt-current fault %q error = %v", test.point, err)
			}
			assertAdoptCurrentBoundaryFacts(t, options, before, test.point)
			options.fault = nil
			progress, err := ObserveProgress(context.Background(), options, before)
			if err != nil || progress != test.progress {
				t.Fatalf("adopt-current fault %q progress = %q, want %q, err=%v",
					test.point, progress, test.progress, err)
			}
			if err := Commit(context.Background(), options, before); err != nil {
				t.Fatalf("adopt-current fault %q resume: %v", test.point, err)
			}
			assertDesiredAdoptCurrentFacts(t, options)
		})
	}
}

func assertForwardOwnerBoundaryFacts(
	t *testing.T,
	options Options,
	before Prepared,
	point string,
) {
	t.Helper()
	legacyRegistration := filepath.Join(options.ConfigHome, "yards", LegacyYard, "config.env")
	currentRegistration := filepath.Join(options.ConfigHome, "yards", CurrentYard, "config.env")
	legacyDirectory := filepath.Dir(legacyRegistration)
	currentDirectory := filepath.Dir(currentRegistration)
	legacyOverride := filepath.Join(legacyDirectory, "overrides", "runtime.env")
	currentOverride := filepath.Join(currentDirectory, "overrides", "runtime.env")
	controller := filepath.Join(options.DataHome, "e2e", "controllers", LegacyYard)
	registrationArchive := filepath.Join(
		filepath.Dir(legacyDirectory),
		ownerRecoveryPrefix+"registration-archive."+options.RecoveryToken,
	)
	controllerArchive := filepath.Join(
		filepath.Dir(controller),
		ownerRecoveryPrefix+"controller-archive."+options.RecoveryToken,
	)
	registration := "YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n"

	switch point {
	case "after-registration-staging-fsync":
		assertExactTestFile(t, legacyRegistration, registration, 0o600)
		assertTestPathAbsent(t, currentDirectory)
		assertSingleRegistrationScratch(t, filepath.Dir(currentDirectory), registration)
		assertFakeProjectState(t, options, "legacy")
		assertTestPathPresent(t, controller)
	case "after-destination-directory-creation":
		assertExactTestFile(t, legacyRegistration, registration, 0o600)
		assertTestPathAbsent(t, currentRegistration)
		assertExactDirectoryEntries(t, currentDirectory, nil)
		assertSingleRegistrationScratch(t, filepath.Dir(currentDirectory), registration)
		assertFakeProjectState(t, options, "legacy")
		assertTestPathPresent(t, controller)
	case "after-registration-publication":
		assertExactTestFile(t, legacyRegistration, registration, 0o600)
		assertExactTestFile(t, currentRegistration, registration, 0o600)
		assertExactDirectoryEntries(t, currentDirectory, []string{"config.env"})
		assertNoRegistrationScratch(t, filepath.Dir(currentDirectory))
		assertFakeProjectState(t, options, "legacy")
		assertTestPathPresent(t, controller)
	case "after-current-project-prepare":
		assertExactTestFile(t, legacyRegistration, registration, 0o600)
		assertExactTestFile(t, currentRegistration, registration, 0o600)
		assertFakeProjectState(t, options, "both")
		assertTestPathPresent(t, controller)
	case "after-legacy-teardown":
		assertExactTestFile(t, legacyRegistration, registration, 0o600)
		assertExactTestFile(t, currentRegistration, registration, 0o600)
		assertFakeProjectState(t, options, "none")
		assertTestPathPresent(t, controller)
	case "after-auxiliary-state-move":
		assertExactTestFile(t, legacyRegistration, registration, 0o600)
		assertExactTestFile(t, currentRegistration, registration, 0o600)
		assertTestPathAbsent(t, legacyOverride)
		assertExactTestFile(t, currentOverride, "NESTED_E2E_VMS=0\n", 0o600)
		assertFakeProjectState(t, options, "current")
		assertTestPathPresent(t, controller)
	case "after-legacy-registration-archive":
		assertTestPathAbsent(t, legacyDirectory)
		assertExactTestFile(t, filepath.Join(registrationArchive, "config.env"), registration, 0o600)
		assertExactTestFile(t, currentRegistration, registration, 0o600)
		assertFakeProjectState(t, options, "current")
		assertTestPathPresent(t, controller)
	case "after-current-init":
		assertTestPathAbsent(t, legacyDirectory)
		assertExactTestFile(t, currentRegistration, registration, 0o600)
		assertFakeProjectState(t, options, "current")
		assertCallCount(t, filepath.Join(filepath.Dir(options.ConfigHome), "calls"),
			"-Y test-yard init --yes\n", 1)
		assertTestPathPresent(t, controller)
	case "after-current-check":
		assertTestPathAbsent(t, legacyDirectory)
		assertExactTestFile(t, currentRegistration, registration, 0o600)
		assertFakeProjectState(t, options, "current")
		assertCallCount(t, filepath.Join(filepath.Dir(options.ConfigHome), "calls"),
			"-Y test-yard check\n", 1)
		assertTestPathPresent(t, controller)
	case "after-legacy-controller-archive":
		assertTestPathAbsent(t, legacyDirectory)
		assertExactTestFile(t, currentRegistration, registration, 0o600)
		assertFakeProjectState(t, options, "current")
		assertCallCount(t, filepath.Join(filepath.Dir(options.ConfigHome), "calls"),
			"-Y test-yard check\n", 1)
		assertTestPathAbsent(t, controller)
		assertTestPathPresent(t, controllerArchive)
	default:
		t.Fatalf("missing independent forward facts for %q", point)
	}
}

func assertCompensationOwnerBoundaryFacts(
	t *testing.T,
	options Options,
	before Prepared,
	point string,
) {
	t.Helper()
	legacyRegistration := filepath.Join(options.ConfigHome, "yards", LegacyYard, "config.env")
	currentRegistration := filepath.Join(options.ConfigHome, "yards", CurrentYard, "config.env")
	legacyOverride := filepath.Join(filepath.Dir(legacyRegistration), "overrides", "runtime.env")
	currentOverride := filepath.Join(filepath.Dir(currentRegistration), "overrides", "runtime.env")
	controller := filepath.Join(options.DataHome, "e2e", "controllers", LegacyYard)
	registration := "YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n"

	assertTestPathPresent(t, controller)
	switch point {
	case "after-compensation-registration-recreation":
		assertExactTestFile(t, legacyRegistration, registration, 0o600)
		assertExactTestFile(t, currentRegistration, registration, 0o600)
		assertFakeProjectState(t, options, "current")
		if hasPreparedOverrides(before.State) {
			assertTestPathAbsent(t, legacyOverride)
			assertExactTestFile(t, currentOverride, "NESTED_E2E_VMS=0\n", 0o600)
		}
	case "after-compensation-current-teardown":
		assertExactTestFile(t, legacyRegistration, registration, 0o600)
		assertExactTestFile(t, currentRegistration, registration, 0o600)
		assertFakeProjectState(t, options, "none")
	case "after-compensation-auxiliary-restore":
		assertExactTestFile(t, legacyRegistration, registration, 0o600)
		assertExactTestFile(t, currentRegistration, registration, 0o600)
		assertExactTestFile(t, legacyOverride, "NESTED_E2E_VMS=0\n", 0o600)
		assertTestPathAbsent(t, currentOverride)
		assertFakeProjectState(t, options, "none")
	case "after-compensation-current-registration-removal":
		assertExactTestFile(t, legacyRegistration, registration, 0o600)
		assertTestPathAbsent(t, currentRegistration)
		assertExactDirectoryEntries(t, filepath.Dir(currentRegistration), nil)
		assertFakeProjectState(t, options, "none")
	case "after-compensation-legacy-project-recreation":
		assertExactTestFile(t, legacyRegistration, registration, 0o600)
		assertTestPathAbsent(t, currentRegistration)
		assertFakeProjectState(t, options, "legacy")
		assertExactTestFile(t, filepath.Join(filepath.Dir(options.ConfigHome),
			"legacy-instance-present"), "0\n", 0o600)
	case "after-compensation-legacy-init":
		assertExactTestFile(t, legacyRegistration, registration, 0o600)
		assertTestPathAbsent(t, currentRegistration)
		assertFakeProjectState(t, options, "legacy")
		assertExactTestFile(t, filepath.Join(filepath.Dir(options.ConfigHome),
			"legacy-instance-present"), "1\n", 0o600)
		assertCallCount(t, filepath.Join(filepath.Dir(options.ConfigHome), "calls"),
			"-Y e2e-yard init --yes\n", 1)
	case "after-compensation-legacy-check":
		assertExactTestFile(t, legacyRegistration, registration, 0o600)
		assertTestPathAbsent(t, currentRegistration)
		assertFakeProjectState(t, options, "legacy")
		assertCallCount(t, filepath.Join(filepath.Dir(options.ConfigHome), "calls"),
			"-Y e2e-yard check\n", 3)
	default:
		t.Fatalf("missing independent compensation facts for %q", point)
	}
}

func assertFlatPublicationBoundaryFacts(
	t *testing.T,
	options Options,
	before Prepared,
	point string,
) {
	t.Helper()
	yards := filepath.Join(options.ConfigHome, "yards")
	legacyRegistration := filepath.Join(yards, LegacyYard+".env")
	currentRegistration := filepath.Join(yards, CurrentYard+".env")
	registration := "YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n"
	assertExactTestFile(t, legacyRegistration, registration, 0o600)
	assertFakeProjectState(t, options, "legacy")
	switch point {
	case "after-registration-staging-fsync":
		assertTestPathAbsent(t, currentRegistration)
		assertSingleRegistrationScratch(t, yards, registration)
	case "after-registration-publication":
		assertExactTestFile(t, currentRegistration, registration, 0o600)
		assertNoRegistrationScratch(t, yards)
		assertExactDirectoryEntries(t, yards, []string{LegacyYard + ".env", CurrentYard + ".env"})
	default:
		t.Fatalf("missing flat publication facts for %q", point)
	}
}

func assertAdoptCurrentBoundaryFacts(
	t *testing.T,
	options Options,
	before Prepared,
	point string,
) {
	t.Helper()
	legacyRegistration := filepath.Join(options.ConfigHome, "yards", LegacyYard, "config.env")
	currentDirectory := filepath.Join(options.ConfigHome, "yards", CurrentYard)
	currentRegistration := filepath.Join(currentDirectory, "config.env")
	currentLock := filepath.Join(currentDirectory, "projects", ".lock")
	yards := filepath.Dir(currentDirectory)
	controller := filepath.Join(options.DataHome, "e2e", "controllers", LegacyYard)
	registrationArchive := filepath.Join(
		filepath.Dir(filepath.Dir(legacyRegistration)),
		ownerRecoveryPrefix+"registration-archive."+options.RecoveryToken,
	)
	controllerArchive := filepath.Join(
		filepath.Dir(controller),
		ownerRecoveryPrefix+"controller-archive."+options.RecoveryToken,
	)
	registration := "YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n"
	assertExactTestFile(t, currentLock, "managed\n", 0o600)
	assertFakeProjectState(t, options, "current")
	switch point {
	case "after-registration-staging-fsync":
		assertExactTestFile(t, legacyRegistration, registration, 0o600)
		assertTestPathAbsent(t, currentRegistration)
		assertExactDirectoryEntries(t, currentDirectory, []string{"projects"})
		assertSingleRegistrationScratch(t, yards, registration)
		assertTestPathPresent(t, controller)
	case "after-registration-publication":
		assertExactTestFile(t, legacyRegistration, registration, 0o600)
		assertExactTestFile(t, currentRegistration, registration, 0o600)
		assertExactDirectoryEntries(t, currentDirectory, []string{"config.env", "projects"})
		assertNoRegistrationScratch(t, yards)
		assertTestPathPresent(t, controller)
	case "after-legacy-registration-archive":
		assertTestPathAbsent(t, filepath.Dir(legacyRegistration))
		assertExactTestFile(t, filepath.Join(registrationArchive, "config.env"), registration, 0o600)
		assertExactTestFile(t, currentRegistration, registration, 0o600)
		assertTestPathPresent(t, controller)
	case "after-current-check":
		assertTestPathAbsent(t, filepath.Dir(legacyRegistration))
		assertExactTestFile(t, currentRegistration, registration, 0o600)
		assertCallCount(t, filepath.Join(filepath.Dir(options.ConfigHome), "calls"),
			"-Y test-yard check\n", 1)
		assertTestPathPresent(t, controller)
	case "after-legacy-controller-archive":
		assertTestPathAbsent(t, filepath.Dir(legacyRegistration))
		assertExactTestFile(t, currentRegistration, registration, 0o600)
		assertTestPathAbsent(t, controller)
		assertTestPathPresent(t, controllerArchive)
	default:
		t.Fatalf("missing adopt-current facts for %q", point)
	}
}

func assertDesiredOwnerFacts(t *testing.T, options Options, before Prepared) {
	t.Helper()
	legacyDirectory := filepath.Join(options.ConfigHome, "yards", LegacyYard)
	currentDirectory := filepath.Join(options.ConfigHome, "yards", CurrentYard)
	assertTestPathAbsent(t, legacyDirectory)
	assertExactTestFile(t, filepath.Join(
		options.ConfigHome, "yards",
		ownerRecoveryPrefix+"registration-archive."+options.RecoveryToken,
		"config.env",
	), "YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n", 0o600)
	assertExactTestFile(t, filepath.Join(currentDirectory, "config.env"),
		"YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n", 0o600)
	if hasPreparedOverrides(before.State) {
		assertExactTestFile(t, filepath.Join(currentDirectory, "overrides", "runtime.env"),
			"NESTED_E2E_VMS=0\n", 0o600)
	} else {
		assertTestPathAbsent(t, filepath.Join(currentDirectory, "overrides"))
	}
	assertTestPathAbsent(t, filepath.Join(
		options.DataHome, "e2e", "controllers", LegacyYard,
	))
	assertTestPathPresent(t, filepath.Join(
		options.DataHome, "e2e", "controllers",
		ownerRecoveryPrefix+"controller-archive."+options.RecoveryToken,
	))
	assertFakeProjectState(t, options, "current")
	assertMinimumCallCount(t, filepath.Join(filepath.Dir(options.ConfigHome), "calls"),
		"-Y test-yard check\n", 1)
}

func assertDesiredFlatOwnerFacts(t *testing.T, options Options) {
	t.Helper()
	yards := filepath.Join(options.ConfigHome, "yards")
	assertTestPathAbsent(t, filepath.Join(yards, LegacyYard+".env"))
	assertExactTestFile(t, filepath.Join(yards, CurrentYard+".env"),
		"YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n", 0o600)
	entries, err := os.ReadDir(yards)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := ownerRecoveryName("registration-archive", options.RecoveryToken)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != CurrentYard+".env" && entry.Name() != archive {
			t.Fatalf("unexpected desired flat owner entry %q", entry.Name())
		}
	}
	assertTestPathAbsent(t, filepath.Join(
		options.DataHome, "e2e", "controllers", LegacyYard,
	))
	assertFakeProjectState(t, options, "current")
	assertMinimumCallCount(t, filepath.Join(filepath.Dir(options.ConfigHome), "calls"),
		"-Y test-yard check\n", 1)
}

func assertDesiredAdoptCurrentFacts(t *testing.T, options Options) {
	t.Helper()
	legacyDirectory := filepath.Join(options.ConfigHome, "yards", LegacyYard)
	currentDirectory := filepath.Join(options.ConfigHome, "yards", CurrentYard)
	assertTestPathAbsent(t, legacyDirectory)
	assertExactTestFile(t, filepath.Join(
		options.ConfigHome, "yards",
		ownerRecoveryPrefix+"registration-archive."+options.RecoveryToken,
		"config.env",
	), "YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n", 0o600)
	assertExactTestFile(t, filepath.Join(currentDirectory, "config.env"),
		"YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n", 0o600)
	assertExactTestFile(t, filepath.Join(currentDirectory, "projects", ".lock"), "managed\n", 0o600)
	assertExactDirectoryEntries(t, currentDirectory, []string{"config.env", "projects"})
	assertTestPathAbsent(t, filepath.Join(
		options.DataHome, "e2e", "controllers", LegacyYard,
	))
	assertTestPathPresent(t, filepath.Join(
		options.DataHome, "e2e", "controllers",
		ownerRecoveryPrefix+"controller-archive."+options.RecoveryToken,
	))
	assertFakeProjectState(t, options, "current")
	calls := read(t, filepath.Join(filepath.Dir(options.ConfigHome), "calls"))
	if strings.Contains(calls, "-Y e2e-yard teardown --yes\n") ||
		strings.Contains(calls, "-Y test-yard init --yes\n") {
		t.Fatalf("adopt-current resume changed lifecycle:\n%s", calls)
	}
	if strings.Count(calls, "-Y test-yard check\n") < 1 {
		t.Fatalf("adopt-current resume omitted canonical check:\n%s", calls)
	}
}

func assertExactTestFile(t *testing.T, path, payload string, mode os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("inspect %s: %v", filepath.Base(path), err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != mode {
		t.Fatalf("%s mode = %v, want regular %o", filepath.Base(path), info.Mode(), mode)
	}
	if actual := read(t, path); actual != payload {
		t.Fatalf("%s payload = %q, want %q", filepath.Base(path), actual, payload)
	}
}

func assertTestPathAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("expected %s to be absent: %v", filepath.Base(path), err)
	}
}

func assertTestPathPresent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("expected %s to exist: %v", filepath.Base(path), err)
	}
}

func assertExactDirectoryEntries(t *testing.T, path string, want []string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	actual := make([]string, 0, len(entries))
	for _, entry := range entries {
		actual = append(actual, entry.Name())
	}
	slices.Sort(actual)
	want = append([]string(nil), want...)
	slices.Sort(want)
	if !slices.Equal(actual, want) {
		t.Fatalf("%s entries = %v, want %v", filepath.Base(path), actual, want)
	}
}

func assertSingleRegistrationScratch(t *testing.T, directory, payload string) string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	var scratch []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ownerRecoveryPrefix+"registration-scratch.") {
			scratch = append(scratch, filepath.Join(directory, entry.Name()))
		}
	}
	if len(scratch) != 1 {
		t.Fatalf("registration scratch entries = %v, want exactly one", scratch)
	}
	assertExactTestFile(t, scratch[0], payload, 0o600)
	return scratch[0]
}

func assertNoRegistrationScratch(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ownerRecoveryPrefix+"registration-scratch.") {
			t.Fatalf("registration scratch remains: %s", entry.Name())
		}
	}
}

func assertSameTestFile(t *testing.T, first, second string) {
	t.Helper()
	firstInfo, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}
	secondInfo, err := os.Stat(second)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(firstInfo, secondInfo) {
		t.Fatalf("%s and %s are different inodes", first, second)
	}
}

func assertFakeProjectState(t *testing.T, options Options, want string) {
	t.Helper()
	path := filepath.Join(filepath.Dir(options.ConfigHome), "incus-state")
	if actual := strings.TrimSpace(read(t, path)); actual != want {
		t.Fatalf("Incus project state = %q, want %q", actual, want)
	}
}

func assertCallCount(t *testing.T, path, call string, want int) {
	t.Helper()
	if actual := strings.Count(read(t, path), call); actual != want {
		t.Fatalf("call %q count = %d, want %d", strings.TrimSpace(call), actual, want)
	}
}

func assertMinimumCallCount(t *testing.T, path, call string, minimum int) {
	t.Helper()
	if actual := strings.Count(read(t, path), call); actual < minimum {
		t.Fatalf("call %q count = %d, want at least %d",
			strings.TrimSpace(call), actual, minimum)
	}
}

func ownerFaultFixture(t *testing.T, sharedImages, overrides bool) (Options, Prepared) {
	t.Helper()
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	write(t, filepath.Join(configHome, "yards", LegacyYard, "config.env"),
		"YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n")
	if overrides {
		write(t, filepath.Join(
			configHome, "yards", LegacyYard, "overrides", "runtime.env",
		), "NESTED_E2E_VMS=0\n")
	}
	dataHome := filepath.Join(root, "data")
	write(t, filepath.Join(
		dataHome, "e2e", "controllers", LegacyYard, ".operator-enrollment-v1",
	), "managed\n")
	if sharedImages {
		t.Setenv("MIGRATION_SHARED_IMAGES", "1")
	} else {
		t.Setenv("MIGRATION_SHARED_IMAGES", "0")
	}
	options := Options{
		Executable: fakeExecutable(t, root), Incus: fakeIncus(t, root),
		ConfigHome: configHome, DataHome: dataHome,
		RecoveryToken: round3RecoveryToken,
		Environment: append(
			os.Environ(),
			"MIGRATION_CALLS="+filepath.Join(root, "calls"),
			"MIGRATION_CONFIG_HOME="+configHome,
		),
	}
	before, err := Prepare(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	return options, before
}

func controllerFaultFixture(t *testing.T) (Options, Prepared) {
	t.Helper()
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	write(t, filepath.Join(configHome, "yards", LegacyYard, "config.env"),
		"YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n")
	controller := filepath.Join(root, "data", "e2e", "controllers", LegacyYard)
	write(t, filepath.Join(controller, "agent-access.pub"), "ssh-ed25519 fixture\n")
	write(t, filepath.Join(controller, "known_hosts"), "host ssh-ed25519 fixture\n")
	write(t, filepath.Join(controller, "route.tsv"), "default\teth0\n")
	write(t, filepath.Join(controller, ".operator-enrollment-v1"), "managed\n")
	t.Setenv("MIGRATION_SHARED_IMAGES", "1")
	options := ownerOptions(t, root, configHome)
	before, err := Prepare(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	return options, before
}

func flatOwnerFaultFixture(t *testing.T) (Options, Prepared) {
	t.Helper()
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	write(t, filepath.Join(configHome, "yards", LegacyYard+".env"),
		"YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n")
	write(t, filepath.Join(
		root, "data", "e2e", "controllers", LegacyYard, ".operator-enrollment-v1",
	), "managed\n")
	t.Setenv("MIGRATION_SHARED_IMAGES", "1")
	options := ownerOptions(t, root, configHome)
	before, err := Prepare(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	return options, before
}

func adoptCurrentOwnerFaultFixture(t *testing.T) (Options, Prepared) {
	t.Helper()
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	write(t, filepath.Join(configHome, "yards", LegacyYard, "config.env"),
		"YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n")
	write(t, filepath.Join(configHome, "yards", CurrentYard, "projects", ".lock"),
		"managed\n")
	write(t, filepath.Join(
		root, "data", "e2e", "controllers", LegacyYard, ".operator-enrollment-v1",
	), "managed\n")
	t.Setenv("MIGRATION_SHARED_IMAGES", "1")
	options := ownerOptions(t, root, configHome)
	setFakeProjects(t, root, "current")
	before, err := Prepare(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	return options, before
}

func ownerOptions(t *testing.T, root, configHome string) Options {
	t.Helper()
	return Options{
		Executable: fakeExecutable(t, root), Incus: fakeIncus(t, root),
		ConfigHome: configHome, DataHome: filepath.Join(root, "data"),
		RecoveryToken: round3RecoveryToken,
		Environment: append(
			os.Environ(),
			"MIGRATION_CALLS="+filepath.Join(root, "calls"),
			"MIGRATION_CONFIG_HOME="+configHome,
		),
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
			test -f "$MIGRATION_CONFIG_HOME/yards/test-yard/config.env" ||
				test -f "$MIGRATION_CONFIG_HOME/yards/test-yard.env"
			;;
		"-Y test-yard init --yes")
			test ! -e "$MIGRATION_CONFIG_HOME/yards/e2e-yard/config.env"
			test ! -e "$MIGRATION_CONFIG_HOME/yards/e2e-yard.env"
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
		printf '0\n' > "$MIGRATION_LEGACY_INSTANCE_PRESENT"
		if [ -n "${MIGRATION_CONFIG_HOME:-}" ]; then
			find "$MIGRATION_CONFIG_HOME/yards/e2e-yard/projects" -depth -delete 2>/dev/null || :
		fi
		;;
	"-Y test-yard teardown --yes") update_projects remove-current ;;
	"-Y e2e-yard init --yes")
		update_projects add-legacy
		printf '1\n' > "$MIGRATION_LEGACY_INSTANCE_PRESENT"
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
	legacyInstancePresent := filepath.Join(root, "legacy-instance-present")
	write(t, state, "legacy\n")
	write(t, instanceState, "RUNNING\n")
	write(t, legacyInstancePresent, "1\n")
	t.Setenv("MIGRATION_INCUS_STATE", state)
	t.Setenv("MIGRATION_INSTANCE_STATE", instanceState)
	t.Setenv("MIGRATION_LEGACY_INSTANCE_PRESENT", legacyInstancePresent)
	t.Setenv("MIGRATION_INCUS_CALLS", filepath.Join(root, "incus-calls"))
	path := filepath.Join(root, "incus")
	write(t, path, `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$MIGRATION_INCUS_CALLS"
state="$(cat "$MIGRATION_INCUS_STATE")"
case "$*" in
	"list yard-e2e-yard --project subyard-e2e-yard --format=json")
		if [ "$(cat "$MIGRATION_LEGACY_INSTANCE_PRESENT")" = 1 ]; then
			printf '[{"name":"yard-e2e-yard","status":"%s"}]\n' \
				"$(cat "$MIGRATION_INSTANCE_STATE")"
		else
			printf '[]\n'
		fi
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
		[ "${MIGRATION_SHARED_IMAGES:-1}" = 1 ] && printf 'false\n' || printf 'true\n'
		;;
	"project get subyard-test-yard features.images")
		[ "$state" = current ] || [ "$state" = both ] || exit 1
		[ "${MIGRATION_SHARED_IMAGES:-1}" = 1 ] && printf 'false\n' || printf 'true\n'
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
	if options.RecoveryToken == "" {
		options.RecoveryToken = round3RecoveryToken
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

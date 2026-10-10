package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestTeardownResetConfigRejectsKeepData(t *testing.T) {
	if _, err := prepareTeardownExecution([]string{"--reset-config", "--keep-data"}); err == nil {
		t.Fatal("contradictory reset accepted")
	}
}

func teardownResetFixture(t *testing.T) (*CLI, config.Loaded, *testkit.Incus, string) {
	t.Helper()
	root, environment, _ := nativeFixture(t)
	helper, err := os.ReadFile("../../scripts/lib/teardown-plan.py")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "scripts", "lib"), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "scripts", "lib", "teardown-plan.py"), helper, 0600)
	incus := lifecycleIncus()
	incus.Reconcile.InstanceFound = false
	incus.Instances = map[string]ports.InstanceInfo{}
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root, Incus: incus, NetworkPolicy: allowTestNetworkPolicy()})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(loaded.Context.Paths.ConfigHome, "yards", "default", "config.env")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, []byte("SSH_PORT=3333\n"), 0600)
	return program, loaded, incus, path
}

func TestTeardownModesCrossShellAndElevatedContext(t *testing.T) {
	for _, mode := range []struct {
		name, reset string
		arguments   []string
	}{
		{name: "purge", reset: "0"},
		{name: "keep-data", reset: "0", arguments: []string{"--keep-data"}},
		{name: "reset-config", reset: "1", arguments: []string{"--reset-config"}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			program, loaded, _, registration := teardownResetFixture(t)
			root := program.options.RepositoryRoot
			helper, err := os.ReadFile("../../scripts/lib/engine-context.sh")
			if err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(root, "scripts", "lib", "engine-context.sh"), helper, 0600)
			leaf := strings.ReplaceAll(`#!/bin/bash
set -e
. "${BASH_SOURCE[0]%/*}/lib/engine-context.sh"
subyard_elevated_context
env -i "${SUBYARD_ELEVATED_ENV[@]}" /bin/bash -ec '
  test "$SUBYARD_TEARDOWN_RESET_CONFIG" = EXPECTED_RESET
  rmdir "$SUBYARD_STATE_DIR"
'
`, "EXPECTED_RESET", mode.reset)
			testkit.WriteFile(t, filepath.Join(root, "scripts", "teardown-physical.sh"), []byte(leaf), 0700)
			if err := os.MkdirAll(loaded.Context.Paths.StateDir, 0700); err != nil {
				t.Fatal(err)
			}
			definition, _ := program.manifest.Lookup("teardown")
			plan := domain.OperationPlan{OperationID: "teardown-shell-context", Confirmed: true}
			orchestrator := program.operationOrchestrator(plan.OperationID, loaded, nil, &definition)
			// Use the production runner with a disposable leaf; no host privilege preparation.
			program.options.AdapterRunner = orchestrator.Runner
			execution, err := prepareTeardownExecution(mode.arguments)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := program.executeTeardown(context.Background(), orchestrator, loaded, plan, execution, io.Discard); err != nil {
				t.Fatal(err)
			}
			_, err = os.Stat(registration)
			if (mode.reset == "1" && !errors.Is(err, os.ErrNotExist)) || (mode.reset == "0" && err != nil) {
				t.Fatalf("unexpected registration after verified shell cleanup: %v", err)
			}
		})
	}
}

func TestTeardownResetConfigRetainsSettingsUntilPhysicalVerifyAndRetries(t *testing.T) {
	program, loaded, incus, path := teardownResetFixture(t)
	incus.Reconcile.InstanceFound = true
	runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{Schema: 1, Status: "ok"}}}}
	program.options.AdapterRunner = runner
	execution, _ := prepareTeardownExecution([]string{"--reset-config"})
	if err := program.observeTeardownExecution(context.Background(), loaded, execution); err != nil {
		t.Fatal(err)
	}
	plan := domain.OperationPlan{OperationID: "reset-settings", Confirmed: true}
	_, err := program.executeTeardown(context.Background(), &application.Orchestrator{Runner: runner, Clock: wallClock{}}, loaded, plan, execution, io.Discard)
	if err == nil {
		t.Fatal("unverified physical cleanup accepted")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("registration removed before physical verification")
	}
	reset, err := config.YardFallbackReset(loaded.Context.Paths.ConfigHome, "default")
	if err != nil || reset {
		t.Fatalf("fallback suppressed on physical failure: %v", err)
	}
	incus.Reconcile.InstanceFound = false
	execution, _ = prepareTeardownExecution([]string{"--reset-config"})
	_, err = program.executeTeardown(context.Background(), &application.Orchestrator{Runner: runner, Clock: wallClock{}}, loaded, plan, execution, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("local registration survived: %v", err)
	}
	reset, err = config.YardFallbackReset(loaded.Context.Paths.ConfigHome, "default")
	if err != nil || !reset {
		t.Fatalf("reset marker missing: %v", err)
	}
}

func TestTeardownResetConfigProtectsExternalStateDirectory(t *testing.T) {
	program, loaded, _, registration := teardownResetFixture(t)
	external := testkit.TempDir(t)
	state := filepath.Join(external, "retained-state")
	testkit.WriteFile(t, state, []byte("retained"), 0600)
	loaded.Context.Paths.StateDir = external
	runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{Schema: 1, Status: "ok"}}}}
	program.options.AdapterRunner = runner
	execution, _ := prepareTeardownExecution([]string{"--reset-config"})
	_, err := program.executeTeardown(context.Background(), &application.Orchestrator{Runner: runner, Clock: wallClock{}}, loaded, domain.OperationPlan{OperationID: "external-state", Confirmed: true}, execution, io.Discard)
	if err == nil {
		t.Fatal("unverified external state allowed config reset")
	}
	for _, path := range []string{state, registration} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("protected external state or retry registration removed: %v", err)
		}
	}
}

func TestTeardownResetConfigRejectsSettingsAndNamespaceRaces(t *testing.T) {
	for _, race := range []string{"replacement", "new-entry", "namespace", "marker"} {
		t.Run(race, func(t *testing.T) {
			program, loaded, _, path := teardownResetFixture(t)
			execution, _ := prepareTeardownExecution([]string{"--reset-config"})
			if err := program.observeTeardownExecution(context.Background(), loaded, execution); err != nil {
				t.Fatal(err)
			}
			switch race {
			case "replacement":
				testkit.WriteFile(t, path+".next", []byte("SSH_PORT=3333\n"), 0600)
				if err := os.Rename(path+".next", path); err != nil {
					t.Fatal(err)
				}
			case "new-entry":
				testkit.WriteFile(t, filepath.Join(filepath.Dir(path), "new-settings"), []byte("x"), 0600)
			case "namespace":
				if err := os.Chmod(filepath.Dir(path), 0701); err != nil {
					t.Fatal(err)
				}
			case "marker":
				noOp := func() error { return nil }
				if err := config.ResetYardConfiguration(loaded.Context.Paths.ConfigHome, "default", noOp, noOp, noOp); err != nil {
					t.Fatal(err)
				}
			}
			if err := program.observeTeardownExecution(context.Background(), loaded, execution); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("%s race accepted: %v", race, err)
			}
		})
	}
}

func TestTeardownResetConfigPreservesNeighborsAndRegistrationOnConfigFailure(t *testing.T) {
	program, loaded, _, path := teardownResetFixture(t)
	root := loaded.Context.Paths.ConfigHome
	neighbor := filepath.Join(root, "yards", "neighbor", "config.env")
	if err := os.MkdirAll(filepath.Dir(neighbor), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, neighbor, []byte("SSH_PORT=4444\n"), 0600)
	for _, relative := range []string{"host-id", "config.env", "credentials/sentinel", "overrides/shared/sentinel"} {
		p := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, p, []byte(""), 0600)
	}
	override := filepath.Join(filepath.Dir(path), "overrides", "sentinel")
	if err := os.MkdirAll(filepath.Dir(override), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, override, []byte("before"), 0600)
	execution, _ := prepareTeardownExecution([]string{"--reset-config"})
	if err := program.observeTeardownExecution(context.Background(), loaded, execution); err != nil {
		t.Fatal(err)
	}
	// Cancellation interrupts local removal while retaining the registration/fallback.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := program.resetTeardownConfiguration(cancelled, loaded, execution); err == nil {
		t.Fatal("cancelled removal succeeded")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("registration removed before override cleanup succeeded")
	}
	reset, err := config.YardFallbackReset(root, "default")
	if err != nil || reset {
		t.Fatalf("marker published before override cleanup: %v", err)
	}
	if err := program.resetTeardownConfiguration(context.Background(), loaded, execution); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{"yards/neighbor/config.env", "host-id", "config.env", "credentials/sentinel", "overrides/shared/sentinel"} {
		if _, err := os.Stat(filepath.Join(root, relative)); err != nil {
			t.Fatalf("protected %s removed: %v", relative, err)
		}
	}
}

func TestTeardownCompletedNamedResetLoadsOnlyForExplicitRetryAndPreservesShared(t *testing.T) {
	program, loaded, incus, _ := teardownResetFixture(t)
	noOp := func() error { return nil }
	if err := config.ResetYardConfiguration(loaded.Context.Paths.ConfigHome, "named", noOp, noOp, noOp); err != nil {
		t.Fatal(err)
	}
	if _, err := program.loadContext("named"); !errors.Is(err, config.ErrUnknownYard) {
		t.Fatalf("ordinary load accepted absent registration: %v", err)
	}
	program.allowResetYard = true
	program.env = environmentMap(program.options.Environment)
	named, err := program.loadContext("named")
	if err != nil {
		t.Fatal(err)
	}
	incus.Reconcile.HostPoolFound = true
	incus.Reconcile.HostNetworkFound = true
	execution, _ := prepareTeardownExecution([]string{"--reset-config"})
	if err := program.observeTeardownExecution(context.Background(), named, execution); err != nil {
		t.Fatal(err)
	}
	if !execution.completedReset || execution.changed || execution.physicalChanged {
		t.Fatalf("completed reset gained work: %+v", execution)
	}
	incus.Reconcile.ProjectFound = true
	execution, _ = prepareTeardownExecution([]string{"--reset-config"})
	if err := program.observeTeardownExecution(context.Background(), named, execution); err == nil || !strings.Contains(err.Error(), "derived resource") {
		t.Fatalf("derived deletion accepted: %v", err)
	}
}

func TestTeardownResetNamedLocalShadowAndCompletedCLIRetry(t *testing.T) {
	program, loaded, incus, _ := teardownResetFixture(t)
	root := loaded.Context.Paths.ConfigHome
	for relative, content := range map[string]string{"yards/named/config.env": "SSH_PORT=3333\n", "yards/named.env": "SSH_PORT=4444\n", ".sync/settings/yards/named/config.env": "SSH_PORT=5555\n"} {
		path := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, []byte(content), 0600)
	}
	program.env = environmentMap(program.options.Environment)
	named, err := program.loadContext("named")
	if err != nil {
		t.Fatal(err)
	}
	registration, err := config.FindYardRegistrationFile(named.Context.Paths.ConfigDir, root, "named")
	if err != nil || registration != filepath.Join(root, "yards/named/config.env") {
		t.Fatalf("canonical registration precedence: %s %v", registration, err)
	}
	execution, _ := prepareTeardownExecution([]string{"--reset-config"})
	_, err = program.executeTeardown(context.Background(), &application.Orchestrator{Runner: &testkit.ScriptedAdapter{}, Clock: wallClock{}}, named, domain.OperationPlan{OperationID: "named-reset", Confirmed: true}, execution, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.FindYardRegistrationFile(named.Context.Paths.ConfigDir, root, "named"); !errors.Is(err, config.ErrUnknownYard) {
		t.Fatalf("old shadow/fallback reactivated: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".sync/settings/yards/named/config.env")); err != nil {
		t.Fatal("Git cache removed")
	}
	options := program.options
	options.Arguments = []string{"-Y", "named", "teardown", "--reset-config", "--yes"}
	options.AdapterRunner = &testkit.ScriptedAdapter{}
	options.Incus = incus
	retry, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if code := retry.Run(context.Background()); code != 0 {
		t.Fatalf("completed public CLI retry failed: %d", code)
	}
}

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/migration"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

type sampleRepairPlatformFixture struct {
	*initPlatformFixture
	observation *ports.RuntimeObservation
}

func (fixture *sampleRepairPlatformFixture) ObserveProfileRuntimes(context.Context) (map[string]ports.RuntimeObservation, error) {
	if fixture.observation != nil {
		return map[string]ports.RuntimeObservation{"sample-runtime": *fixture.observation}, nil
	}
	observation := ports.RuntimeObservation{State: "current", Actual: strings.Repeat("b", 64), Desired: strings.Repeat("b", 64)}
	if !fixture.converged[ports.ReconcileStageProfileRuntimes] {
		observation.State, observation.Actual = "stale", strings.Repeat("a", 64)
	}
	return map[string]ports.RuntimeObservation{"sample-runtime": observation}, nil
}

type initIntegrationRepairFixture struct {
	*integrationRuntimeFixture
	err error
}

func (fixture *initIntegrationRepairFixture) IntegrationPlan(context.Context) (reconcileruntime.IntegrationPlan, error) {
	return fixture.plan, fixture.err
}

func TestProfileInitRepairBindsIntegrationOnlyDrift(t *testing.T) {
	for _, scenario := range []string{"missing dispatcher", "ownership conflict", "unselected drift", "changed plan", "remaining drift"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newConfigApplyRepairFixture(t, scenario == "unselected drift")
			platform := &sampleRepairPlatformFixture{initPlatformFixture: newInitPlatformFixture()}
			for stage := range platform.converged {
				platform.converged[stage] = true
			}
			fixture.cli.options.InitPlatform = platform
			runtime := &initIntegrationRepairFixture{integrationRuntimeFixture: &integrationRuntimeFixture{
				plan: reconcileruntime.IntegrationPlan{Changed: true, Fingerprint: strings.Repeat("a", 64), Scope: strings.Repeat("b", 64)},
			}}
			if scenario == "ownership conflict" {
				runtime.err = errors.New("owned artifact drift")
			}
			fixture.cli.options.IntegrationRuntime = func(config.Loaded) IntegrationRuntime { return runtime }
			fake := fixture.cli.options.Executor.(*testkit.Incus)
			fake.ExecSteps = nil
			loaded, err := fixture.cli.resolveReleaseTransitionContext("default", fixture.cli.env["SUBYARD_CONFIG_HOME"])
			if err != nil {
				t.Fatal(err)
			}
			targets, err := fixture.cli.localConfigTargets(loaded, true)
			if err != nil {
				t.Fatal(err)
			}
			for range 4 {
				for _, target := range targets {
					appendHashSteps(t, fake, target.Loaded)
				}
			}
			journalBefore, err := os.ReadFile(fixture.journalPath)
			if err != nil {
				t.Fatal(err)
			}
			ledgerPath := filepath.Join(filepath.Dir(fixture.journalPath), "ledger.json")
			ledgerBefore, err := os.ReadFile(ledgerPath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				journalAfter, journalErr := os.ReadFile(fixture.journalPath)
				ledgerAfter, ledgerErr := os.ReadFile(ledgerPath)
				if journalErr != nil || ledgerErr != nil || !bytes.Equal(journalBefore, journalAfter) || !bytes.Equal(ledgerBefore, ledgerAfter) {
					t.Fatal("integration repair changed completed protected history")
				}
			}()
			permit, err := fixture.cli.prepareProfileInitRepair(context.Background(), "default", nil, fixture.outcome)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "ownership conflict" || scenario == "unselected drift" {
				if permit != nil {
					t.Fatal("integration conflict or unselected drift admitted init")
				}
				return
			}
			if permit == nil {
				t.Fatal("missing shared owned artifact did not admit explicit init repair")
			}
			if scenario == "changed plan" {
				// Scope changes remain stale even if an adapter repeats its fingerprint.
				runtime.plan.Scope = strings.Repeat("c", 64)
			}
			unlock, err := fixture.cli.lockConfigApplyRepair(context.Background(), permit)
			if scenario == "changed plan" {
				if err == nil {
					unlock()
					t.Fatal("replacement integration plan reused repair consent")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			if scenario != "remaining drift" {
				runtime.plan.Changed = false
				writeCLIFile(t, fixture.readyMarker, "ready\n", 0o600)
			}
			err = fixture.cli.finishConfigApplyRepair(context.Background(), permit)
			if (err != nil) != (scenario == "remaining drift") {
				t.Fatalf("post-init readiness: %v", err)
			}
		})
	}
}

func TestProfileRuntimeInitRepairUsesCompletedGateWithoutAdmittingOtherDrift(t *testing.T) {
	for _, scenario := range []string{"repair", "config drift", "unselected ProfileRuntime", "unfinished", "rollback", "unfinished rollback", "journal changed", "current convergence", "absent race", "replacement stale"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newConfigApplyRepairFixture(t, scenario == "unselected ProfileRuntime")
			platform := &sampleRepairPlatformFixture{initPlatformFixture: newInitPlatformFixture()}
			for stage := range platform.converged {
				platform.converged[stage] = true
			}
			platform.converged[ports.ReconcileStageProfileRuntimes] = false
			fixture.cli.options.InitPlatform = platform
			fake := fixture.cli.options.Executor.(*testkit.Incus)
			fake.ExecSteps = nil
			loaded, err := fixture.cli.resolveReleaseTransitionContext("default", fixture.cli.env["SUBYARD_CONFIG_HOME"])
			if err != nil {
				t.Fatal(err)
			}
			targets, err := fixture.cli.localConfigTargets(loaded, true)
			if err != nil {
				t.Fatal(err)
			}
			for range 4 {
				for _, target := range targets {
					if scenario == "config drift" {
						appendMismatchedHashSteps(t, fake, target.Loaded, "0")
					} else {
						appendHashSteps(t, fake, target.Loaded)
					}
				}
			}
			if scenario == "unfinished" || scenario == "rollback" || scenario == "unfinished rollback" {
				payload, err := os.ReadFile(fixture.journalPath)
				if err != nil {
					t.Fatal(err)
				}
				journal, err := releasetransition.ParseJournal(payload)
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "rollback" || scenario == "unfinished rollback" {
					journal.Goal.Direction = releasetransition.DirectionActivatePrevious
				}
				if scenario == "unfinished" || scenario == "unfinished rollback" {
					journal.Checkpoint = releasetransition.JournalReconciling
				}
				payload, err = releasetransition.MarshalJournal(journal)
				if err != nil {
					t.Fatal(err)
				}
				writeCLIFile(t, fixture.journalPath, string(payload), 0o600)
			}
			permit, err := fixture.cli.prepareProfileInitRepair(context.Background(), "default", nil, fixture.outcome)
			admitted := scenario != "unselected ProfileRuntime" && scenario != "unfinished" && scenario != "unfinished rollback"
			if err != nil || (permit != nil) != admitted {
				t.Fatalf("repair admission: permit=%#v err=%v", permit, err)
			}
			if !admitted {
				return
			}
			if len(platform.applied) != 0 || platform.projectHooks != 0 {
				t.Fatal("assessment applied the repair")
			}
			if scenario == "journal changed" {
				payload, err := os.ReadFile(fixture.journalPath)
				if err != nil {
					t.Fatal(err)
				}
				writeCLIFile(t, fixture.journalPath, string(payload)+"\n", 0o600)
			}
			switch scenario {
			case "current convergence":
				platform.observation = &ports.RuntimeObservation{
					State: "current", Actual: strings.Repeat("b", 64), Desired: strings.Repeat("b", 64),
				}
			case "absent race":
				platform.observation = &ports.RuntimeObservation{State: "absent"}
			case "replacement stale":
				platform.observation = &ports.RuntimeObservation{
					State: "stale", Actual: strings.Repeat("c", 64), Desired: strings.Repeat("b", 64),
				}
			}
			unlock, err := fixture.cli.lockConfigApplyRepair(context.Background(), permit)
			if scenario == "journal changed" {
				if err == nil {
					unlock()
					t.Fatal("changed recovery journal admitted repair")
				}
				return
			}
			if scenario == "absent race" || scenario == "replacement stale" {
				if err == nil {
					unlock()
					t.Fatal("changed ProfileRuntime contract admitted after confirmation")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			platform.converged[ports.ReconcileStageProfileRuntimes] = true
			writeCLIFile(t, fixture.readyMarker, "ready\n", 0o600)
			if err := fixture.cli.finishConfigApplyRepair(context.Background(), permit); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestConfigApplyRepairUsesCompletedProtectedGateAndStableLock(t *testing.T) {
	fixture := newConfigApplyRepairFixture(t, false)
	observed, err := fixture.cli.inspectMutationGate(context.Background(), "default")
	if err != nil || observed == nil {
		t.Fatalf("inspect completed drift gate: outcome=%#v err=%v", observed, err)
	}
	if !sameConfigApplyGate(fixture.outcome, *observed) {
		t.Fatalf("fixture outcome differs from protected inspection: want=%#v got=%#v",
			fixture.outcome, *observed)
	}
	options, available, err := fixture.cli.mutationGateReleaseOptions()
	if err != nil || !available {
		t.Fatalf("release options: available=%v err=%v", available, err)
	}
	legacy, err := migration.InspectMutationGate(context.Background(), options)
	if err != nil || legacy != nil {
		t.Fatalf("legacy gate blocks fixture: outcome=%#v err=%v", legacy, err)
	}
	store, err := releasetransition.NewPOSIXV2Store(options.ConfigHome)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil || !snapshot.Exists {
		t.Fatalf("read fixture journal: exists=%v err=%v", snapshot.Exists, err)
	}
	journal, err := releasetransition.ParseJournal(snapshot.Payload)
	if err != nil || !configApplyRepairJournalMatches(journal, fixture.outcome) {
		t.Fatalf("fixture journal mismatch: journal=%#v err=%v", journal, err)
	}
	registryPayload, err := readConfigApplyRegistry(fixture.cli.options.RepositoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	registry, registryDigest, err := releasetransition.ParseRegistryV2(
		registryPayload, releasetransition.BuiltinCapabilityCatalog(),
	)
	if err != nil || registryDigest != journal.RegistryDigest {
		t.Fatalf("fixture registry mismatch: got=%q want=%q err=%v", registryDigest, journal.RegistryDigest, err)
	}
	ledgerSnapshot, err := store.ReadLedger()
	if err != nil || !ledgerSnapshot.Exists {
		t.Fatalf("read fixture ledger: exists=%v err=%v", ledgerSnapshot.Exists, err)
	}
	ledger, _, err := releasetransition.ParseLedgerV2(ledgerSnapshot.Payload, registry)
	if err != nil {
		t.Fatalf("parse fixture ledger: %v", err)
	}
	pending, err := registry.PendingPath(ledger)
	if err != nil || len(pending) != 0 {
		t.Fatalf("fixture ledger incomplete: pending=%v err=%v", pending, err)
	}
	permit, err := fixture.cli.prepareConfigApplyRepair(
		context.Background(), "default", false, fixture.outcome,
	)
	if err != nil || permit == nil {
		t.Fatalf("prepare completed config repair: permit=%#v err=%v", permit, err)
	}
	before, err := os.ReadFile(fixture.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := fixture.cli.lockConfigApplyRepair(context.Background(), permit)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := os.WriteFile(fixture.readyMarker, []byte("ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fixture.cli.finishConfigApplyRepair(context.Background(), permit); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(fixture.journalPath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("config repair changed completed journal: err=%v", err)
	}
}

func TestConfigApplyRepairBindsCheckpointAuthorityAfterConversion(t *testing.T) {
	fixture := newConfigApplyRepairFixture(t, false)
	payload, err := readConfigApplyRegistry(fixture.cli.options.RepositoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	registry, digest, err := releasetransition.ParseRegistryV2(payload, releasetransition.BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	store, err := releasetransition.NewPOSIXV2Store(fixture.cli.env["SUBYARD_CONFIG_HOME"])
	if err != nil {
		t.Fatal(err)
	}
	initial, err := store.ReadMigrationLedger(registry, digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConvertMigrationLedger(initial, registry, digest, registry, digest); err != nil {
		t.Fatal(err)
	}
	permit, err := fixture.cli.prepareConfigApplyRepair(context.Background(), "default", false, fixture.outcome)
	if err != nil || permit == nil || !permit.checkpointed {
		t.Fatalf("checkpoint repair permit=%#v, err=%v", permit, err)
	}
	unlock, err := fixture.cli.lockConfigApplyRepair(context.Background(), permit)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	checkpoint, err := store.ReadMigrationCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(permit.configHome, "release-transition", "v2", "history-checkpoint.json"),
		string(checkpoint.Payload)+"\n", 0o600)
	if unlock, err := fixture.cli.lockConfigApplyRepair(context.Background(), permit); err == nil {
		unlock()
		t.Fatal("changed checkpoint authority admitted a previously assessed repair")
	}
}

func TestConfigApplyRepairRejectsUnfinishedAndCorruptProtectedJournals(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	runtimeRoot := filepath.Join(root, "runtime-v2-config-apply-pending")
	environment = append(environment,
		"YARD_RUNTIME_ROOT="+runtimeRoot,
		"V2_GATE_CAPTURE="+filepath.Join(root, "capture"),
	)
	journalPath, _ := installUnfinishedV2MutationGateFixture(t, root, environment, runtimeRoot)
	program, err := New(Options{
		RepositoryRoot: root, Environment: environment, WorkingDir: root,
		Incus: lifecycleIncus(), Executor: lifecycleIncus(),
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := program.inspectMutationGate(context.Background(), "default")
	if err != nil || outcome == nil {
		t.Fatalf("inspect pending gate: outcome=%#v err=%v", outcome, err)
	}
	permit, err := program.prepareConfigApplyRepair(context.Background(), "default", false, *outcome)
	if err != nil || permit != nil {
		t.Fatalf("pending gate permit=%#v err=%v", permit, err)
	}
	if err := os.WriteFile(journalPath, []byte("{broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	eligible := releasetransition.Outcome{
		Status: releasetransition.StatusMigrationRequired,
		Code:   releasetransition.CodeTransitionRequired,
		Active: "1.2.3-aaaaaaaaaaaa", Target: "1.2.3-aaaaaaaaaaaa",
		Retry: "run yard update",
	}
	permit, err = program.prepareConfigApplyRepair(context.Background(), "default", false, eligible)
	if err != nil || permit != nil {
		t.Fatalf("corrupt gate permit=%#v err=%v", permit, err)
	}
}

func TestConfigApplyRepairRejectsUnselectedAllLocalDrift(t *testing.T) {
	fixture := newConfigApplyRepairFixture(t, true)
	permit, err := fixture.cli.prepareConfigApplyRepair(
		context.Background(), "default", false, fixture.outcome,
	)
	if err != nil || permit != nil {
		t.Fatalf("partial all-local permit=%#v err=%v", permit, err)
	}
}

func TestConfigApplyRepairLockRejectsJournalChangedAfterConfirmation(t *testing.T) {
	fixture := newConfigApplyRepairFixture(t, false)
	permit, err := fixture.cli.prepareConfigApplyRepair(
		context.Background(), "default", false, fixture.outcome,
	)
	if err != nil || permit == nil {
		t.Fatalf("prepare config repair: permit=%#v err=%v", permit, err)
	}
	payload, err := os.ReadFile(fixture.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.journalPath, append(payload, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if unlock, err := fixture.cli.lockConfigApplyRepair(context.Background(), permit); err == nil {
		unlock()
		t.Fatal("lock accepted a journal changed after confirmation")
	}
}

func TestConfigApplyRepairRejectsMissingCorruptAndPendingLedgers(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{name: "missing", mutate: func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "corrupt", mutate: func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("{broken\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "pending", mutate: func(t *testing.T, path string) {
			registryPayload, err := os.ReadFile(filepath.Join(repositoryRoot(t), "config", "release-transition.json"))
			if err != nil {
				t.Fatal(err)
			}
			registry, _, err := releasetransition.ParseRegistryV2(
				registryPayload, releasetransition.BuiltinCapabilityCatalog(),
			)
			if err != nil {
				t.Fatal(err)
			}
			payload, _, err := releasetransition.MarshalLedgerV2(
				releasetransition.BaselineLedgerV2(registry), registry,
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, payload, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newConfigApplyRepairFixture(t, false)
			ledgerPath := filepath.Join(filepath.Dir(fixture.journalPath), "ledger.json")
			test.mutate(t, ledgerPath)
			permit, err := fixture.cli.prepareConfigApplyRepair(
				context.Background(), "default", false, fixture.outcome,
			)
			if err != nil || permit != nil {
				t.Fatalf("unsafe ledger permit=%#v err=%v", permit, err)
			}
		})
	}
}

func TestConfigApplyRepairExactSelectionMustCoverEveryDriftedYard(t *testing.T) {
	fixture := newConfigApplyRepairFixture(t, true)
	permit, err := fixture.cli.prepareConfigApplyRepairMode(
		context.Background(), "default", true, fixture.outcome, true, []string{"default"},
	)
	if err != nil || permit != nil {
		t.Fatalf("partial exact-selection permit=%#v err=%v", permit, err)
	}
}

func TestConfigApplyRepairRejectsLegacyBlockerMaskedByCompletedV2(t *testing.T) {
	fixture := newConfigApplyRepairFixture(t, false)
	options, _, err := fixture.cli.mutationGateReleaseOptions()
	if err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(options.ConfigHome, "migrations", "transactions", "legacy", "transaction.json")
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, legacyPath, "{broken\n", 0o600)
	gate, err := fixture.cli.inspectMutationGate(context.Background(), "default")
	if err != nil || gate == nil || !sameConfigApplyGate(*gate, fixture.outcome) {
		t.Fatalf("V2 should mask legacy blocker in ordinary gate: gate=%#v err=%v", gate, err)
	}
	permit, err := fixture.cli.prepareConfigApplyRepair(context.Background(), "default", false, *gate)
	if err != nil || permit != nil {
		t.Fatalf("masked legacy corruption admitted repair: permit=%#v err=%v", permit, err)
	}
}

func TestConfigApplyRepairRejectsDifferentCommandDesiredBeforeConfirmation(t *testing.T) {
	fixture := newConfigApplyRepairFixture(t, false)
	root := fixture.cli.options.RepositoryRoot
	alternate := filepath.Join(root, "alternate.rules")
	writeCLIFile(t, alternate, "different_rule()\n", 0o600)
	fixture.cli.baseEnv["AGENT_codex_RULES"] = alternate
	fixture.cli.env["AGENT_codex_RULES"] = alternate
	permit, err := fixture.cli.prepareConfigApplyRepair(context.Background(), "default", false, fixture.outcome)
	if err != nil || permit == nil {
		t.Fatalf("prepare persisted repair: permit=%#v err=%v", permit, err)
	}
	loaded, err := fixture.cli.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	target := configTarget{Name: "default", Loaded: loaded}
	assessment, err := fixture.cli.assessConfigTarget(context.Background(), target, true)
	if err != nil {
		t.Fatal(err)
	}
	if permit.matchesRequestedConfigs([]configTargetAssessment{assessment}) {
		t.Fatal("command override matched persisted repair")
	}
	fixture.cli.configApplyRepair = permit
	appendMismatchedHashSteps(t, fixture.cli.options.Executor.(*testkit.Incus), loaded, "0")
	var diagnostics bytes.Buffer
	fixture.cli.options.Stderr = &diagnostics
	selector := func() ([]configTarget, error) {
		t.Fatal("mismatching desired reached post-confirmation target revalidation")
		return nil, nil
	}
	if code := fixture.cli.applyConfig(context.Background(), []configTarget{target}, true, selector); code == 0 {
		t.Fatal("different command desired was accepted")
	}
	if !strings.Contains(diagnostics.String(), "remove differing command overrides") {
		t.Fatalf("wrong refusal: %s", diagnostics.String())
	}
}

type configApplyRepairFixture struct {
	cli         *CLI
	outcome     releasetransition.Outcome
	journalPath string
	readyMarker string
}

func newConfigApplyRepairFixture(t *testing.T, named bool) configApplyRepairFixture {
	root, environment, _ := nativeFixture(t)
	writeRuntimeProfileFixture(t, root)
	sampleHandler := filepath.Join(root, "config", "profiles", "sample", "runtime.sh")
	if err := os.MkdirAll(filepath.Dir(sampleHandler), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, sampleHandler, "#!/bin/sh\nprintf '%s\\n' '{\"state\":\"absent\",\"actual\":\"\",\"desired\":\"\"}'\n", 0o700)
	writeCLIFile(t, filepath.Join(root, "config", "subyard.env"), strings.Join([]string{
		"SHIFT_MODE=shift", "FORWARD_SSH_AGENT=0", "DEV_SUDO=0", "DEV_UID=1000",
		"DEV_USER=dev", "SSH_PORT=2222",
		"STORAGE_PATH=" + filepath.Join(root, "data", "storage"),
		"HOST_BASE=" + filepath.Join(root, "host"),
		"RESTRICTED_DISK_PATHS=" + filepath.Join(root, "host"),
	}, "\n")+"\n", 0o600)
	migrations, err := os.ReadFile(filepath.Join(repositoryRoot(t), "config", "migrations.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(root, "config", "migrations.json"), string(migrations), 0o600)
	source := filepath.Join(root, "repo.rules")
	writeCLIFile(t, source, "allow_rule()\n", 0o600)
	writeCLIFile(t, filepath.Join(root, "config", "agents.env"), strings.Join([]string{
		"CODING_TOOL_INTEGRATIONS=codex",
		"AGENT_codex_RULES=" + source,
		"AGENT_codex_RULES_DEST=.codex/rules/repo.rules",
	}, "\n")+"\n", 0o600)
	if named {
		configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
		namedPath := filepath.Join(configHome, "yards", "named", "config.env")
		if err := os.MkdirAll(filepath.Dir(namedPath), 0o700); err != nil {
			t.Fatal(err)
		}
		writeCLIFile(t, namedPath,
			"SSH_PORT=2223\n", 0o600)
		if _, err := os.Stat(namedPath); err != nil {
			t.Fatalf("named registration before gate fixture: %v", err)
		}
	}
	runtimeRoot := filepath.Join(root, "runtime-v2-config-apply")
	environment = append(environment,
		"YARD_RUNTIME_ROOT="+runtimeRoot,
		"V2_GATE_CAPTURE="+filepath.Join(root, "capture"),
		"SUBYARD_POWER_RECONCILER_PATH="+filepath.Join(root, "absent-power-reconciler"),
		"SUBYARD_POWER_UNIT_PATH="+filepath.Join(root, "absent-power-unit"),
	)
	journalPath, _ := installUnfinishedV2MutationGateFixture(t, root, environment, runtimeRoot)
	registryPayload, err := os.ReadFile(filepath.Join(repositoryRoot(t), "config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(root, "config", "release-transition.json"), string(registryPayload), 0o600)
	writeCLIFile(t, filepath.Join(runtimeRoot, "releases", "1.2.3-aaaaaaaaaaaa",
		"config", "release-transition.json"), string(registryPayload), 0o600)
	if named {
		if _, err := os.Stat(filepath.Join(environmentValue(environment, "SUBYARD_CONFIG_HOME"),
			"yards", "named", "config.env")); err != nil {
			t.Fatalf("named registration after unfinished gate fixture: %v", err)
		}
	}
	readyMarker, outcome := completeConfigApplyGateFixture(
		t, root, environment, runtimeRoot, journalPath,
	)
	fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{}}
	program, err := New(Options{
		RepositoryRoot: root, Environment: environment, WorkingDir: root,
		Incus: fake, Executor: fake,
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.resolveReleaseTransitionContext(
		"default", environmentValue(environment, "SUBYARD_CONFIG_HOME"),
	)
	if err != nil {
		t.Fatal(err)
	}
	targets, err := program.localConfigTargets(loaded, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range targets {
		fake.Instances[target.Loaded.Context.IncusProject+"/"+target.Loaded.Context.YardInstanceName] =
			ports.InstanceInfo{Name: target.Loaded.Context.YardInstanceName,
				Project: target.Loaded.Context.IncusProject, Status: "Running"}
		appendMismatchedHashSteps(t, fake, target.Loaded, "0")
	}
	if !named {
		for _, target := range targets {
			appendMismatchedHashSteps(t, fake, target.Loaded, "0")
		}
	}
	return configApplyRepairFixture{
		cli: program, outcome: outcome, journalPath: journalPath, readyMarker: readyMarker,
	}
}

func completeConfigApplyGateFixture(
	t *testing.T,
	root string,
	environment []string,
	runtimeRoot string,
	journalPath string,
) (string, releasetransition.Outcome) {
	t.Helper()
	const target = "1.2.3-aaaaaaaaaaaa"
	readyMarker := filepath.Join(root, "config-apply-ready")
	engine := filepath.Join(runtimeRoot, "releases", target, "bin", "yard-engine")
	script := fmt.Sprintf(`#!/bin/sh
case "${1:-}" in
--version) printf 'yard-engine 1.2.3\n' ;;
_release-transition)
  if [ -e %q ]; then
    printf '%%s\n' '{"schemaVersion":1,"activationReconciliationOwned":true,"inspection":{"plan":"plan-v1-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","assessment":{"action":"release.transition.v2","effect":"mutation","changed":false,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible"},"outcome":{"status":"ready","reachedGoal":true,"active":"%s","previous":"release-a","target":"%s","code":"ready","message":"verified","transaction":"tx-0123456789abcdef"}}}'
  else
    printf '%%s\n' '{"schemaVersion":1,"activationReconciliationOwned":true,"inspection":{"plan":"plan-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible","consequences":["repair release activation"]},"outcome":{"status":"migration-required","reachedGoal":false,"active":"%s","previous":"release-a","target":"%s","code":"transition-required","message":"activation drift","retry":"run yard update"}}}'
  fi ;;
*) exit 64 ;;
esac
`, readyMarker, target, target, target, target)
	writeCLIFile(t, engine, script, 0o700)
	enginePayload, err := os.ReadFile(engine)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := os.ReadFile(filepath.Join(runtimeRoot, "releases", target,
		"config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	engineDigest := sha256.Sum256(enginePayload)
	registryDigest := sha256.Sum256(registry)
	manifest := fmt.Sprintf("%x  ./bin/yard-engine\n%x  ./config/release-transition.json\n",
		engineDigest, registryDigest)
	writeCLIFile(t, filepath.Join(runtimeRoot, "releases", target, "runtime-files.sha256"),
		manifest, 0o600)
	manifestDigest := sha256.Sum256([]byte(manifest))
	journalPayload, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := releasetransition.ParseJournal(journalPayload)
	if err != nil {
		t.Fatal(err)
	}
	journal.Checkpoint = releasetransition.JournalComplete
	journal.ArtifactDigest = releasetransition.Fingerprint(fmt.Sprintf("%x", manifestDigest))
	journal.RegistryDigest = releasetransition.Fingerprint(fmt.Sprintf("%x", registryDigest))
	journalPayload, err = releasetransition.MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, journalPath, string(journalPayload), 0o600)
	parsedRegistry, _, err := releasetransition.ParseRegistryV2(
		registry, releasetransition.BuiltinCapabilityCatalog(),
	)
	if err != nil {
		t.Fatal(err)
	}
	ledger := releasetransition.BaselineLedgerV2(parsedRegistry)
	for _, migration := range parsedRegistry.Migrations {
		ledger, err = ledger.Advance(parsedRegistry, migration)
		if err != nil {
			t.Fatal(err)
		}
	}
	ledgerPayload, _, err := releasetransition.MarshalLedgerV2(ledger, parsedRegistry)
	if err != nil {
		t.Fatal(err)
	}
	store, err := releasetransition.NewPOSIXV2Store(
		environmentValue(environment, "SUBYARD_CONFIG_HOME"),
	)
	if err != nil {
		t.Fatal(err)
	}
	missingLedger, err := store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapLedger(missingLedger, ledgerPayload); err != nil {
		t.Fatal(err)
	}
	links, err := releasetransition.NewRuntimeLinkStore(runtimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := links.Activate(journal.Releases); err != nil {
		t.Fatal(err)
	}
	return readyMarker, releasetransition.Outcome{
		Status: releasetransition.StatusMigrationRequired,
		Code:   releasetransition.CodeTransitionRequired,
		Active: target, Previous: releaseIDForConfigApply("release-a"), Target: target,
		Message: "activation drift", Retry: "run yard update",
	}
}

func releaseIDForConfigApply(value releasetransition.ReleaseID) *releasetransition.ReleaseID {
	return &value
}

func TestConfigApplyRepairGateRequiresFreshCompletedActivationShape(t *testing.T) {
	target := releasetransition.ReleaseID("1.2.3-aaaaaaaaaaaa")
	valid := releasetransition.Outcome{
		Status: releasetransition.StatusMigrationRequired,
		Code:   releasetransition.CodeTransitionRequired,
		Active: target, Target: target, Retry: "run yard update",
	}
	if !configApplyRepairGateShape(valid) {
		t.Fatal("fresh same-release activation repair was rejected")
	}
	transaction := releasetransition.TransactionID("tx-0123456789abcdef")
	for name, mutate := range map[string]func(*releasetransition.Outcome){
		"ready": func(value *releasetransition.Outcome) {
			value.Status, value.Code, value.ReachedGoal =
				releasetransition.StatusReady, releasetransition.CodeReady, true
		},
		"unfinished":     func(value *releasetransition.Outcome) { value.Transaction = &transaction },
		"foreign target": func(value *releasetransition.Outcome) { value.Target = "1.2.4-bbbbbbbbbbbb" },
		"blocked": func(value *releasetransition.Outcome) {
			value.Status, value.Code = releasetransition.StatusOperatorActionRequired,
				releasetransition.CodeJournalInvalid
		},
	} {
		t.Run(name, func(t *testing.T) {
			outcome := valid
			mutate(&outcome)
			if configApplyRepairGateShape(outcome) {
				t.Fatalf("unsafe gate was admitted: %#v", outcome)
			}
		})
	}
}

func TestConfigApplyRepairRequiresCompletedMatchingJournal(t *testing.T) {
	target := releasetransition.ReleaseID("1.2.3-aaaaaaaaaaaa")
	outcome := releasetransition.Outcome{Active: target, Target: target}
	journal := releasetransition.JournalRecord{
		Checkpoint: releasetransition.JournalComplete,
		Goal: releasetransition.Goal{
			Target: target, Direction: releasetransition.DirectionActivateTarget,
		},
	}
	if !configApplyRepairJournalMatches(journal, outcome) {
		t.Fatal("completed matching journal was rejected")
	}
	journal.Checkpoint = releasetransition.JournalReconciling
	if configApplyRepairJournalMatches(journal, outcome) {
		t.Fatal("unfinished journal was admitted")
	}
	journal.Checkpoint = releasetransition.JournalComplete
	journal.Goal.Target = "1.2.4-bbbbbbbbbbbb"
	if configApplyRepairJournalMatches(journal, outcome) {
		t.Fatal("foreign completed journal was admitted")
	}
}

func TestConfigApplyRepairReleaseIdentityComparison(t *testing.T) {
	release := releasetransition.ReleaseID("1.2.3-aaaaaaaaaaaa")
	same := release
	other := releasetransition.ReleaseID("1.2.4-bbbbbbbbbbbb")
	if !sameConfigApplyReleaseID(nil, nil) || !sameConfigApplyReleaseID(&release, &same) ||
		sameConfigApplyReleaseID(nil, &release) || sameConfigApplyReleaseID(&release, &other) {
		t.Fatal("release identity equality accepted an ambiguous previous link")
	}
}

func TestConfigApplyRepairFingerprintBindsDesiredScopeOnly(t *testing.T) {
	facts := configApplyRepairFacts{
		Journal: releasetransition.Fingerprint(strings.Repeat("a", 64)), Target: "1.2.3-aaaaaaaaaaaa",
		Direction: releasetransition.DirectionActivateTarget,
		OtherActivations: []configApplyActivationFact{{
			ID: "host-power", Actual: releasetransition.Fingerprint(strings.Repeat("b", 64)),
			Desired: releasetransition.Fingerprint(strings.Repeat("b", 64)), Converged: true,
		}},
		ConfigTargets: []configApplyTargetFact{{
			Name: "default", DesiredFingerprint: strings.Repeat("c", 64),
		}},
	}
	first, err := configApplyFactsFingerprint(facts)
	if err != nil {
		t.Fatal(err)
	}
	second, err := configApplyFactsFingerprint(facts)
	if err != nil || first != second || len(first) != 64 {
		t.Fatalf("stable facts fingerprints = %q and %q, err=%v", first, second, err)
	}
	facts.ConfigTargets[0].DesiredFingerprint = strings.Repeat("d", 64)
	changed, err := configApplyFactsFingerprint(facts)
	if err != nil || changed == first {
		t.Fatalf("desired scope change was not bound: first=%q changed=%q err=%v", first, changed, err)
	}
}

func TestConfigApplyRepairAllowsOnlyPreviouslyAdmittedDriftToRemain(t *testing.T) {
	if !configApplyDriftSubset(nil, []string{"default"}) ||
		!configApplyDriftSubset([]string{"default"}, []string{"default", "named"}) {
		t.Fatal("converged or remaining admitted drift was rejected")
	}
	if configApplyDriftSubset([]string{"named"}, []string{"default"}) ||
		configApplyDriftSubset([]string{"default", "named"}, []string{"default"}) {
		t.Fatal("new all-local drift was admitted")
	}
}

func TestNormalizedCompletedConfigApplyPairDoesNotSwitchLinks(t *testing.T) {
	from := releasetransition.ReleaseID("1.2.2-bbbbbbbbbbbb")
	target := releasetransition.ReleaseID("1.2.3-aaaaaaaaaaaa")
	got := normalizedCompletedConfigApplyPair(releasetransition.ReleasePair{
		From: from, Target: target,
	})
	if got.From != target || got.Target != target || got.Previous == nil || *got.Previous != from {
		t.Fatalf("normalized completed pair = %#v", got)
	}
}

func TestConfigApplyActivationObservationRequiresTypedFixedPoint(t *testing.T) {
	digest := releasetransition.Fingerprint(strings.Repeat("a", 64))
	if !validConfigApplyActivationObservation(releasetransition.V2ActivationObservation{
		Actual: digest, Desired: digest, Converged: true,
	}) {
		t.Fatal("valid activation observation was rejected")
	}
	if validConfigApplyActivationObservation(releasetransition.V2ActivationObservation{
		Actual: "short", Desired: digest, Converged: true,
	}) {
		t.Fatal("malformed activation fingerprint was admitted")
	}
}

func TestCompletedConfigRepairKeepsRustDriftDistinctFromCleanup(t *testing.T) {
	transaction := releasetransition.TransactionID("tx-" + strings.Repeat("a", 32))
	target := releasetransition.ReleaseID("1.2.3-current")
	baseJournal := releasetransition.JournalRecord{Checkpoint: releasetransition.JournalComplete, Transaction: transaction, Goal: releasetransition.Goal{Target: target, Direction: releasetransition.DirectionActivateTarget}}
	base := releasetransition.Outcome{Status: releasetransition.StatusRecovering, Code: releasetransition.CodeRecoveryPending, Active: target, Target: target, Transaction: &transaction, Retry: "run yard migrate"}
	for _, scenario := range []string{"drift", "wrong transaction", "incomplete", "wrong target", "wrong direction", "rollback profile init", "rollback wrong transaction", "cleanup ticket", "different observed outcome", "reached goal"} {
		t.Run(scenario, func(t *testing.T) {
			journal, outcome := baseJournal, base
			observed := outcome
			inspection := &releasetransition.Inspection{Outcome: &observed}
			switch scenario {
			case "wrong transaction":
				wrong := releasetransition.TransactionID("tx-" + strings.Repeat("b", 32))
				outcome.Transaction = &wrong
				observed = outcome
			case "incomplete":
				journal.Checkpoint = releasetransition.JournalReconciling
			case "wrong target":
				outcome.Target = "1.2.3-other"
				observed = outcome
			case "wrong direction":
				journal.Goal.Direction = releasetransition.DirectionActivatePrevious
			case "rollback profile init":
				journal.Goal.Direction = releasetransition.DirectionActivatePrevious
			case "rollback wrong transaction":
				journal.Goal.Direction = releasetransition.DirectionActivatePrevious
				wrong := releasetransition.TransactionID("tx-" + strings.Repeat("b", 32))
				outcome.Transaction = &wrong
				observed = outcome
			case "cleanup ticket":
				inspection.Resume = &transaction
			case "different observed outcome":
				observed.Retry = "different original operation"
			case "reached goal":
				outcome.ReachedGoal = true
				observed = outcome
			}
			if got := configApplyRepairRustInspectionMatches(journal, outcome, inspection, strings.HasPrefix(scenario, "rollback")); got != (scenario == "drift" || scenario == "rollback profile init") {
				t.Fatalf("completed repair admission=%t", got)
			}
		})
	}
	historical := base
	historical.Status = releasetransition.StatusMigrationRequired
	historical.Code = releasetransition.CodeTransitionRequired
	historical.Transaction = nil
	if !configApplyRepairGateShape(historical) || !configApplyRepairJournalMatches(baseJournal, historical) {
		t.Fatal("historical completed drift contract changed")
	}
	if configApplyRepairRustInspectionMatches(baseJournal, historical, &releasetransition.Inspection{Outcome: &historical}, false) {
		t.Fatal("historical outcome acquired Rust continuation authority")
	}
}

// The physical component fixture changes its observed files only after the
// ordinary init pipeline reaches its approved finalization stage.
type rollbackSourceInitPlatform struct {
	*completedSourcePlatformFixture
	ready    string
	physical *selectionOnlyIntegrationExecutor
}

func (fixture *rollbackSourceInitPlatform) ApplyStage(ctx context.Context, stage ports.ReconcileStageID) error {
	if err := fixture.initPlatformFixture.ApplyStage(ctx, stage); err != nil {
		return err
	}
	if stage == ports.ReconcileStageProfileRuntimes {
		fixture.physical.staleFiles = false
		return os.WriteFile(fixture.ready, []byte("ready\n"), 0600)
	}
	return nil
}

func TestRustCompletedRollbackAdmitsProfileInitThroughCLI(t *testing.T) {
	if os.Getenv("SUBYARD_RUST_SOURCE_QUERY_TOOLING_ROOT") == "" {
		t.Skip("explicit experimental Rust tooling supplied by delivery gate")
	}
	program, ready, home, _ := completedRustSourceAuthoringRuntimeFixture(t, false, nil, releasetransition.DirectionActivatePrevious)
	journalPath := filepath.Join(home, "release-transition/v2/journal.json")
	raw := mustReadConfigFixture(t, journalPath)
	journal, err := releasetransition.ParseJournal(raw)
	if err != nil || journal.Checkpoint != releasetransition.JournalComplete || journal.Goal.Direction != releasetransition.DirectionActivatePrevious {
		t.Fatal("actual Rust rollback did not publish COMPLETE", err)
	}
	reader := releaseruntime.NewSourceInspector(program.options.RepositoryRoot, program.options.Stderr)
	defer reader.Close()
	proof, err := reader.InspectSourceAdmission(context.Background(), program.env["YARD_RUNTIME_ROOT"], home)
	if err != nil || proof.Metadata.Journal == nil || proof.Metadata.Journal.Transaction != journal.Transaction {
		t.Fatal("actual rollback source proof unavailable", err)
	}
	request, err := json.Marshal(releasetransition.ProcessRequest{SchemaVersion: 1, Mode: releasetransition.ProcessInspect,
		RuntimeRoot: program.env["YARD_RUNTIME_ROOT"], ConfigHome: home, Yard: "default", Target: journal.Goal.Target,
		Direction: journal.Goal.Direction, ArtifactDigest: journal.ArtifactDigest, RegistryDigest: journal.RegistryDigest})
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(filepath.Join(program.options.RepositoryRoot, "bin/yard-engine"), "_release-transition")
	command.Env, command.Stdin = program.options.Environment, bytes.NewReader(request)
	var responseBytes, responseErrors bytes.Buffer
	command.Stdout, command.Stderr = &responseBytes, &responseErrors
	if err := command.Run(); err != nil {
		t.Fatalf("actual rollback frozen inspection failed: %v %s", err, responseErrors.String())
	}
	var response releasetransition.ProcessResponse
	if err := json.Unmarshal(responseBytes.Bytes(), &response); err != nil {
		t.Fatal("actual rollback frozen response rejected", err)
	}
	if response.Inspection == nil || response.Inspection.Resume != nil || response.Inspection.Outcome == nil || response.Inspection.Outcome.Transaction == nil || *response.Inspection.Outcome.Transaction != journal.Transaction || releasetransition.ValidateProcessInspection(journal.Goal, *response.Inspection) != nil {
		t.Fatal("actual rollback inspection did not preserve the original no-Resume transaction")
	}
	gate, err := program.inspectMutationGate(context.Background(), "default")
	if err != nil || gate == nil || gate.Transaction == nil || *gate.Transaction != journal.Transaction || !configApplyRepairRustGateShape(*gate) {
		t.Fatalf("actual rollback drift gate missing: %#v %v", gate, err)
	}
	loaded, err := program.loadSelectionRepair("default")
	if err != nil {
		t.Fatal(err)
	}
	incus := program.options.Incus.(*testkit.Incus)
	instance := incus.Instances[loaded.Context.IncusProject+"/"+loaded.Context.YardInstanceName]
	instance.Config = map[string]string{"volatile.uuid": "source-authoring-default", "user.subyard.managed": "true"}
	incus.Instances[loaded.Context.IncusProject+"/"+loaded.Context.YardInstanceName] = instance
	incus.Reconcile = ports.ReconcileState{InstanceFound: true, Instance: instance}
	physical := &selectionOnlyIntegrationExecutor{source: loaded.Environment["AGENT_codex_RULES"], staleFiles: true}
	platform := &rollbackSourceInitPlatform{completedSourcePlatformFixture: program.options.InitPlatform.(*completedSourcePlatformFixture), ready: ready, physical: physical}
	for stage := range platform.converged {
		platform.converged[stage] = true
	}
	// Materialized files are stale, so the owning refresh stage must apply.
	platform.converged[ports.ReconcileStageProfileRuntimes] = false
	platform.hooksApplicable = false
	options := program.options
	options.InitPlatform, options.Executor = platform, physical
	options.Arguments = []string{"init", "--yes"}
	var output rustOwnerOutput
	options.Stdout, options.Stderr = &output, &output
	history := activationRepairHistory(t, options.RepositoryRoot, home)
	repair, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if code := repair.Run(context.Background()); code != 0 {
		t.Fatalf("profile init after actual Rust rollback: %d %s", code, output.String())
	}
	permit := repair.profileInitRepair
	if permit == nil || !permit.profileInit || permit.cleanupID != "" || permit.gate.Transaction == nil || *permit.gate.Transaction != journal.Transaction || permit.sourceBinding != proof.Binding || !bytes.Equal(permit.journal.Payload, raw) {
		t.Fatal("ordinary init did not consume the exact completed rollback repair proof")
	}
	if len(platform.applied) == 0 || physical.staleFiles || !bytes.Equal(raw, mustReadConfigFixture(t, journalPath)) || !reflect.DeepEqual(history, activationRepairHistory(t, options.RepositoryRoot, home)) {
		t.Fatalf("profile init physical/history result: applied=%d stale=%t journalSame=%t historySame=%t", len(platform.applied), physical.staleFiles, bytes.Equal(raw, mustReadConfigFixture(t, journalPath)), reflect.DeepEqual(history, activationRepairHistory(t, options.RepositoryRoot, home)))
	}
}

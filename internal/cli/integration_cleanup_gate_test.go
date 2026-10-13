package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/testkit"
)

type cleanupRecoveryConflict string

func (conflict cleanupRecoveryConflict) Error() string {
	return "synthetic unverified integration ownership"
}
func (conflict cleanupRecoveryConflict) IntegrationCleanupTarget() string { return string(conflict) }

type cleanupRecoveryRuntime struct {
	*integrationCleanupFixture
	conflict string
	err      error
}

func (runtime *cleanupRecoveryRuntime) IntegrationPlan(context.Context) (reconcileruntime.IntegrationPlan, error) {
	if runtime.err != nil {
		return reconcileruntime.IntegrationPlan{}, runtime.err
	}
	if runtime.conflict == "" {
		return reconcileruntime.IntegrationPlan{}, nil
	}
	return reconcileruntime.IntegrationPlan{}, cleanupRecoveryConflict(runtime.conflict)
}

func cleanupRecoveryCLIFixture(t *testing.T, arguments []string, scenarios ...string) (*CLI, *cleanupRecoveryRuntime, string, *bytes.Buffer) {
	t.Helper()
	program, readyMarker, home := completedSelectionAuthoringFixture(t)
	root := program.options.RepositoryRoot
	agents := filepath.Join(root, "config/agents.env")
	testkit.WriteFile(t, agents, append(mustReadConfigFixture(t, agents), []byte("\nAGENT_widget_COMMAND=synthetic-widget\nAGENT_widget_CLEANUP=${SUBYARD_CONFIG_DIR}/widget-cleanup.sh\n")...), 0o600)
	testkit.WriteFile(t, filepath.Join(root, "config/widget-cleanup.sh"), []byte("#!/bin/sh\nexit 99\n"), 0o600)
	writeProfileTestFile(t, filepath.Join(home, "yards/default/config.env"), []byte("# persistent deselection\nCODING_TOOL_INTEGRATIONS=''\n"), 0o600)
	journalPath := filepath.Join(home, "release-transition/v2/journal.json")
	journal, err := releasetransition.ParseJournal(mustReadConfigFixture(t, journalPath))
	if err != nil {
		t.Fatal(err)
	}
	scenario := ""
	if len(scenarios) != 0 {
		scenario = scenarios[0]
	}
	if scenario == "unfinished" {
		journal.Checkpoint = releasetransition.JournalReconciling
	}
	response := releasetransition.ProcessResponse{SchemaVersion: 1, ActivationReconciliationOwned: true,
		Inspection: &releasetransition.Inspection{Plan: releasetransition.PlanToken("plan-v1-" + strings.Repeat("a", 64)),
			Assessment: domain.ActionAssessment{Action: "release.transition.v2", Effect: domain.ActionMutation, Changed: true,
				Impacts: []domain.ActionImpact{domain.ImpactLocalMetadata, domain.ImpactPersistentData, domain.ImpactYardRuntime}, Recovery: domain.RecoveryReversible, Consequences: []string{"inspect the explicit bounded cleanup plan"}},
			Blockers: []releasetransition.Blocker{{Code: releasetransition.CodeDependencyUnavailable, Resource: "activation.materialized-config", Message: "synthetic ownership conflict", Retry: "inspect integration cleanup"}},
			Outcome: &releasetransition.Outcome{Status: releasetransition.StatusOperatorActionRequired, Active: journal.Goal.Target,
				Previous: releaseIDForConfigApply("release-a"), Target: journal.Goal.Target, Code: releasetransition.CodeDependencyUnavailable,
				Message: "synthetic ownership conflict", Retry: "inspect integration cleanup", Transaction: &journal.Transaction}}}
	if scenario == "nonmaterialized" {
		response.Inspection.Blockers[0].Resource = "activation.route-consumers"
	}
	if scenario == "multiple-blockers" {
		response.Inspection.Blockers = append(response.Inspection.Blockers, releasetransition.Blocker{Code: releasetransition.CodeDependencyUnavailable, Resource: "activation.other", Message: "another contract is unavailable", Retry: "inspect the other contract"})
	}
	if scenario == "missing-handler" {
		testkit.WriteFile(t, agents, []byte(strings.ReplaceAll(string(mustReadConfigFixture(t, agents)), "AGENT_widget_CLEANUP=${SUBYARD_CONFIG_DIR}/widget-cleanup.sh\n", "")), 0o600)
	}
	if scenario == "required" {
		testkit.WriteFile(t, agents, append(mustReadConfigFixture(t, agents), []byte("AGENT_claude_DEPENDS=widget\n")...), 0o600)
	}
	payload, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	readyInspection := *response.Inspection
	readyOutcome := *readyInspection.Outcome
	readyOutcome.Status, readyOutcome.Code, readyOutcome.ReachedGoal, readyOutcome.Retry = releasetransition.StatusReady, releasetransition.CodeReady, true, ""
	readyInspection.Outcome, readyInspection.Blockers = &readyOutcome, nil
	readyInspection.Assessment.Changed, readyInspection.Assessment.Consequences = false, nil
	readyPayload, err := json.Marshal(releasetransition.ProcessResponse{SchemaVersion: 1, ActivationReconciliationOwned: true, Inspection: &readyInspection})
	if err != nil {
		t.Fatal(err)
	}
	engine := fmt.Sprintf("#!/bin/sh\ncase \"${1:-}\" in\n --version) printf 'yard-engine 1.2.3\\n' ;;\n _release-transition) cat >/dev/null; if [ -e %q ]; then printf '%%s\\n' %q; else printf '%%s\\n' %q; fi ;;\n *) exit 64 ;;\nesac\n", readyMarker, readyPayload, payload)
	testkit.WriteFile(t, filepath.Join(root, "bin/yard-engine"), []byte(engine), 0o700)
	manifest := ""
	for _, tree := range []string{"bin", "config", "scripts"} {
		if err := filepath.WalkDir(filepath.Join(root, tree), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			manifest += fmt.Sprintf("%x  ./%s\n", sha256.Sum256(mustReadConfigFixture(t, path)), filepath.ToSlash(relative))
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	testkit.WriteFile(t, filepath.Join(root, "runtime-files.sha256"), []byte(manifest), 0o600)
	journal.ArtifactDigest = releasetransition.Fingerprint(fmt.Sprintf("%x", sha256.Sum256([]byte(manifest))))
	journalPayload, err := releasetransition.MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, journalPath, journalPayload, 0o600)
	cleanup := &cleanupRecoveryRuntime{integrationCleanupFixture: &integrationCleanupFixture{integrationRuntimeFixture: &integrationRuntimeFixture{},
		plan: reconcileruntime.IntegrationCleanupPlan{Fingerprint: strings.Repeat("b", 64), Changed: true, Steps: []string{"Archive only the captured synthetic service; preserve user data"}}}, conflict: "widget"}
	cleanup.afterApply = func() {
		cleanup.conflict = ""
		cleanup.plan.Changed = false
		testkit.WriteFile(t, readyMarker, []byte("ready\n"), 0o600)
	}
	var output bytes.Buffer
	options := program.options
	options.Arguments, options.Stdout, options.Stderr = arguments, &output, &output
	options.IntegrationRuntime = func(config.Loaded) IntegrationRuntime { return cleanup }
	program, err = New(options)
	if err != nil {
		t.Fatal(err)
	}
	return program, cleanup, home, &output
}

func TestIntegrationCleanupRecoveryRepeatUsesOrdinaryReadyBoundary(t *testing.T) {
	program, runtime, home, output := cleanupRecoveryCLIFixture(t, []string{"integration", "cleanup", "widget", "--yes"})
	before := mustReadConfigFixture(t, filepath.Join(home, "release-transition/v2/journal.json"))
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("cleanup failed: %d %s", code, output)
	}
	output.Reset()
	repeat, err := New(program.options)
	if err != nil {
		t.Fatal(err)
	}
	if code := repeat.Run(context.Background()); code != 0 || repeat.integrationCleanupRepair != nil || runtime.plan.Changed {
		t.Fatalf("already retired repeat failed or reused repair authority: %d %s", code, output)
	}
	if !bytes.Equal(before, mustReadConfigFixture(t, filepath.Join(home, "release-transition/v2/journal.json"))) {
		t.Fatal("repeat changed history")
	}
}

func TestIntegrationCleanupRecoveryRemainsLocalCLIOnly(t *testing.T) {
	program, runtime, _, _ := cleanupRecoveryCLIFixture(t, nil)
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]*preparedCommand)}
	_, err = handler.Handle(context.Background(), rpc.Call{Method: "operation.plan", OperationID: "cleanup-recovery-rpc", Params: json.RawMessage(`{"command":"integration","arguments":["cleanup","widget"],"exact":true}`)}, nil)
	if fault, ok := err.(*rpc.Error); !ok || fault.Code != string(releasetransition.CodeDependencyUnavailable) || runtime.applied != 0 || program.integrationCleanupRepair != nil {
		t.Fatalf("RPC acquired local cleanup recovery authority: %v", err)
	}
}

func TestIntegrationCleanupRecoveryThroughFullCLI(t *testing.T) {
	for _, scenario := range []string{"check", "apply", "decline"} {
		t.Run(scenario, func(t *testing.T) {
			arguments := []string{"integration", "cleanup", "widget"}
			if scenario == "check" {
				arguments = append(arguments, "--check")
			}
			if scenario == "apply" {
				arguments = append(arguments, "--yes")
			}
			program, runtime, home, output := cleanupRecoveryCLIFixture(t, arguments)
			journalPath := filepath.Join(home, "release-transition/v2/journal.json")
			before := mustReadConfigFixture(t, journalPath)
			selectionPath := filepath.Join(home, "yards/default/config.env")
			selection := mustReadConfigFixture(t, selectionPath)
			code := program.Run(context.Background())
			if scenario == "decline" {
				if code == 0 || runtime.applied != 0 {
					t.Fatalf("declined cleanup applied: exit%d calls%d %s", code, runtime.applied, output)
				}
			} else if code != 0 || runtime.planned == 0 || runtime.applied != map[string]int{"check": 0, "apply": 1}[scenario] {
				t.Fatalf("owning cleanup recovery blocked: exit%d planned%d applied%d %s", code, runtime.planned, runtime.applied, output)
			}
			if scenario == "check" {
				if !strings.Contains(output.String(), "Archive only the captured synthetic service") {
					t.Fatalf("owning cleanup preview lost consequences: %s", output)
				}
			} else if !strings.Contains(output.String(), "integration.cleanup") || !strings.Contains(output.String(), "configured selection and unrelated artifacts retained") || !strings.Contains(output.String(), "integration remains deselected") {
				t.Fatalf("typed cleanup target, retention or guards not reviewed: %s", output)
			}
			if !bytes.Equal(before, mustReadConfigFixture(t, journalPath)) || !bytes.Equal(selection, mustReadConfigFixture(t, selectionPath)) {
				t.Fatal("cleanup rewrote history or selection")
			}
			if runtime.integrationRuntimeFixture.applied != 0 || len(program.options.InitPlatform.(*initPlatformFixture).applied) != 0 || program.options.InitPlatform.(*initPlatformFixture).projectHooks != 0 {
				t.Fatal("cleanup invoked ordinary integration or activation hooks")
			}
		})
	}
}

func TestIntegrationCleanupRecoveryRejectsUnsafeFullCLIAdmission(t *testing.T) {
	for _, scenario := range []string{"nonmaterialized", "multiple-blockers", "unfinished", "missing-handler", "generic-conflict", "wrong-id", "selected", "required", "override", "stopped", "missing", "other-activation", "corrupt", "foreign-links"} {
		t.Run(scenario, func(t *testing.T) {
			program, runtime, home, output := cleanupRecoveryCLIFixture(t, []string{"integration", "cleanup", "widget", "--yes"}, scenario)
			selectionPath := filepath.Join(home, "yards/default/config.env")
			journalPath := filepath.Join(home, "release-transition/v2/journal.json")
			switch scenario {
			case "generic-conflict":
				runtime.err = errors.New("untyped native ownership conflict")
			case "wrong-id":
				runtime.conflict = "another"
			case "selected":
				testkit.WriteFile(t, selectionPath, []byte("CODING_TOOL_INTEGRATIONS=widget\n"), 0o600)
			case "required":
				// The synthetic widget remains selected transitively in persistent configuration.
				testkit.WriteFile(t, selectionPath, []byte("CODING_TOOL_INTEGRATIONS=claude\n"), 0o600)
			case "override":
				program.baseEnv["CODING_TOOL_INTEGRATIONS"] = ""
			case "stopped", "missing":
				incus := program.options.Incus.(*testkit.Incus)
				if scenario == "missing" {
					delete(incus.Instances, "subyard/yard")
				} else {
					instance := incus.Instances["subyard/yard"]
					instance.Status = "Stopped"
					incus.Instances["subyard/yard"] = instance
				}
			case "other-activation":
				program.options.InitPlatform = &sampleRepairPlatformFixture{initPlatformFixture: newInitPlatformFixture(), observation: &ports.RuntimeObservation{State: ports.RuntimeStateStale, Actual: strings.Repeat("a", 64), Desired: strings.Repeat("b", 64)}}
			case "corrupt":
				testkit.WriteFile(t, journalPath, []byte("{broken\n"), 0o600)
			case "foreign-links":
				path := filepath.Join(program.env["YARD_RUNTIME_ROOT"], "current")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("releases/release-a", path); err != nil {
					t.Fatal(err)
				}
			}
			before := mustReadConfigFixture(t, journalPath)
			selection := mustReadConfigFixture(t, selectionPath)
			if code := program.Run(context.Background()); code == 0 || runtime.applied != 0 {
				t.Fatalf("unsafe cleanup admitted: exit%d applied%d %s", code, runtime.applied, output)
			}
			if !bytes.Equal(before, mustReadConfigFixture(t, journalPath)) || !bytes.Equal(selection, mustReadConfigFixture(t, selectionPath)) {
				t.Fatal("rejected cleanup changed selection or history")
			}
		})
	}
}

func TestIntegrationCleanupRecoveryRevalidatesAfterConsent(t *testing.T) {
	for _, scenario := range []string{"history", "source", "source-mode", "native-plan", "selection"} {
		t.Run(scenario, func(t *testing.T) {
			program, runtime, home, output := cleanupRecoveryCLIFixture(t, []string{"integration", "cleanup", "widget"})
			program.options.Prompt = &callbackPrompt{callback: func() {
				switch scenario {
				case "history":
					path := filepath.Join(home, "release-transition/v2/journal.json")
					testkit.WriteFile(t, path, append(mustReadConfigFixture(t, path), '\n'), 0o600)
				case "source":
					testkit.WriteFile(t, filepath.Join(program.options.RepositoryRoot, "config/widget-cleanup.sh"), []byte("#!/bin/sh\n# changed sealed handler\n"), 0o600)
				case "source-mode":
					if err := os.Chmod(filepath.Join(program.options.RepositoryRoot, "config/widget-cleanup.sh"), 0o666); err != nil {
						t.Fatal(err)
					}
				case "native-plan":
					runtime.plan.Fingerprint = strings.Repeat("c", 64)
				case "selection":
					testkit.WriteFile(t, filepath.Join(home, "yards/default/config.env"), []byte("CODING_TOOL_INTEGRATIONS=widget\n"), 0o600)
				}
			}}
			if code := program.Run(context.Background()); code == 0 || runtime.applied != 0 {
				t.Fatalf("changed consent state admitted: exit%d calls%d %s", code, runtime.applied, output)
			}
			if len(program.options.Prompt.(*callbackPrompt).requests) != 1 {
				t.Fatalf("did not exercise confirmation boundary: %s", output)
			}
		})
	}
}

type cleanupOtherYardRuntime struct {
	*integrationRuntimeFixture
	planned int
	err     error
}

func (runtime *cleanupOtherYardRuntime) IntegrationPlan(context.Context) (reconcileruntime.IntegrationPlan, error) {
	runtime.planned++
	return runtime.plan, runtime.err
}

func TestIntegrationCleanupRecoveryNamedYardRequiresOtherYardsCurrent(t *testing.T) {
	for _, scenario := range []string{"named-cleanup", "other-ownership-conflict", "other-integration-drift"} {
		t.Run(scenario, func(t *testing.T) {
			program, selected, home, output := cleanupRecoveryCLIFixture(t, []string{"-Y", "named", "integration", "cleanup", "widget", "--yes"})
			named := filepath.Join(home, "yards/named/config.env")
			writeProfileTestFile(t, named, []byte("# named persistent empty selection\nSSH_PORT=2255\nCODING_TOOL_INTEGRATIONS=''\n"), 0o600)
			other := &cleanupOtherYardRuntime{integrationRuntimeFixture: &integrationRuntimeFixture{
				plan: reconcileruntime.IntegrationPlan{Fingerprint: "other-current"}}}
			switch scenario {
			case "other-ownership-conflict":
				other.err = cleanupRecoveryConflict("widget")
			case "other-integration-drift":
				other.plan.Changed = true
			}
			program.options.IntegrationRuntime = func(loaded config.Loaded) IntegrationRuntime {
				if loaded.Context.YardName == "named" {
					return selected
				}
				if loaded.Context.YardName != "default" {
					t.Fatalf("unexpected cleanup yard %q", loaded.Context.YardName)
				}
				return other
			}
			incus := program.options.Incus.(*testkit.Incus)
			incus.Instances["subyard-named/yard-named"] = ports.InstanceInfo{Project: "subyard-named", Name: "yard-named", Status: "Running"}
			// Observe actual desired assets as current in both contexts, so a
			// negative can only come from the other yard's integration contract.
			incus.ExecSteps = nil
			operation := program.releaseActivationOperation()
			loaded, err := operation.resolveReleaseTransitionContext("default", home)
			if err != nil {
				t.Fatal(err)
			}
			targets, err := operation.localConfigTargets(loaded, true)
			if err != nil || len(targets) != 2 {
				t.Fatalf("two-yard fixture targets: %d %v", len(targets), err)
			}
			for range 6 {
				for _, target := range targets {
					appendHashSteps(t, incus, target.Loaded)
				}
			}
			paths := []string{named, filepath.Join(home, "yards/default/config.env"), filepath.Join(home, "release-transition/v2/journal.json"), filepath.Join(home, "release-transition/v2/ledger.json"), filepath.Join(home, "release-transition/v2/history-checkpoint.json"), filepath.Join(program.options.RepositoryRoot, "runtime-files.sha256")}
			before := make(map[string][]byte)
			for _, path := range paths {
				before[path] = mustReadConfigFixture(t, path)
			}
			currentPath := filepath.Join(program.env["YARD_RUNTIME_ROOT"], "current")
			previousPath := filepath.Join(program.env["YARD_RUNTIME_ROOT"], "previous")
			current, err := os.Readlink(currentPath)
			if err != nil {
				t.Fatal(err)
			}
			previous, err := os.Readlink(previousPath)
			if err != nil {
				t.Fatal(err)
			}
			code := program.Run(context.Background())
			if scenario == "named-cleanup" {
				if code != 0 || selected.applied != 1 || !strings.Contains(output.String(), "subyard-named/yard-named:integration widget") {
					t.Fatalf("named cleanup failed or targeted another yard: %d %s", code, output)
				}
			} else if code == 0 || selected.applied != 0 {
				t.Fatalf("other-yard integration state admitted cleanup: %d %s", code, output)
			}
			if other.planned == 0 || other.applied != 0 || selected.integrationRuntimeFixture.applied != 0 {
				t.Fatal("other-yard contract was skipped or ordinary integration reconciliation ran")
			}
			for _, path := range paths {
				if !bytes.Equal(before[path], mustReadConfigFixture(t, path)) {
					t.Fatalf("cleanup changed source or history: %s", filepath.Base(path))
				}
			}
			if got, err := os.Readlink(currentPath); err != nil || got != current {
				t.Fatal("cleanup switched current release")
			}
			if got, err := os.Readlink(previousPath); err != nil || got != previous {
				t.Fatal("cleanup switched previous release")
			}
			platform := program.options.InitPlatform.(*initPlatformFixture)
			if len(platform.applied) != 0 || platform.projectHooks != 0 {
				t.Fatal("cleanup applied release activation or hooks")
			}
		})
	}
}

type nativeCleanupRecoveryExecutor struct {
	retired     bool
	applied     int
	fingerprint string
}

func (executor *nativeCleanupRecoveryExecutor) Exec(_ context.Context, _, _ string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	if len(request.Command) < 5 || request.Command[2] != "-s" {
		return ports.InstanceExecResult{}, nil
	}
	if request.Command[4] == "apply" {
		executor.retired = true
		executor.applied++
	}
	payload, err := json.Marshal(struct {
		Fingerprint string   `json:"fingerprint"`
		Changed     bool     `json:"changed"`
		Steps       []string `json:"steps"`
	}{executor.fingerprint, !executor.retired, []string{"Archive only the captured synthetic native service"}})
	return ports.InstanceExecResult{Stdout: payload}, err
}

type nativeCleanupRecoveryObservation struct {
	plan               reconcileruntime.IntegrationCleanupPlan
	context            domain.Context
	operationIDPresent bool
}

type nativeCleanupRecoveryRuntime struct {
	reconcileruntime.Runtime
	observations *[]nativeCleanupRecoveryObservation
}

func (runtime nativeCleanupRecoveryRuntime) IntegrationPlan(context.Context) (reconcileruntime.IntegrationPlan, error) {
	return reconcileruntime.IntegrationPlan{}, cleanupRecoveryConflict("widget")
}

func (runtime nativeCleanupRecoveryRuntime) IntegrationCleanupPlan(ctx context.Context, id string) (reconcileruntime.IntegrationCleanupPlan, error) {
	plan, err := runtime.Runtime.IntegrationCleanupPlan(ctx, id)
	if err == nil {
		*runtime.observations = append(*runtime.observations, nativeCleanupRecoveryObservation{plan: plan, context: runtime.Yard, operationIDPresent: environmentValue(runtime.Environment, "SUBYARD_OPERATION_ID") != ""})
	}
	return plan, err
}

func nativeCleanupRecoveryCLIFixture(t *testing.T, arguments []string) (*CLI, *nativeCleanupRecoveryExecutor, *[]nativeCleanupRecoveryObservation, string, *bytes.Buffer) {
	t.Helper()
	program, _, home, output := cleanupRecoveryCLIFixture(t, arguments)
	incus := program.options.Incus.(*testkit.Incus)
	incus.Reconcile = ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{Project: "subyard", Name: "yard", Status: "Running"}}
	executor := &nativeCleanupRecoveryExecutor{fingerprint: strings.Repeat("a", 64)}
	var observations []nativeCleanupRecoveryObservation
	program.options.IntegrationRuntime = func(loaded config.Loaded) IntegrationRuntime {
		environment := structuredCommandContext(loaded)
		environment["SUBYARD_DISPATCHER_PATH"] = program.options.DispatcherPath
		environment["SUBYARD_POWER_ENGINE_SOURCE"] = program.options.DispatcherPath
		return nativeCleanupRecoveryRuntime{Runtime: reconcileruntime.Runtime{RepositoryRoot: program.options.RepositoryRoot, Yard: loaded.Context,
			Environment: environmentList(program.env, environment), Incus: incus, Executor: executor}, observations: &observations}
	}
	return program, executor, &observations, home, output
}

func nativeCleanupRecoveryInvariants(t *testing.T, program *CLI, home string) func() {
	t.Helper()
	paths := []string{filepath.Join(home, "release-transition/v2/journal.json"), filepath.Join(home, "release-transition/v2/ledger.json"), filepath.Join(home, "release-transition/v2/history-checkpoint.json"), filepath.Join(home, "yards/default/config.env"), filepath.Join(program.options.RepositoryRoot, "runtime-files.sha256")}
	before := map[string][]byte{}
	for _, path := range paths {
		before[path] = mustReadConfigFixture(t, path)
	}
	links := map[string]string{}
	for _, name := range []string{"current", "previous"} {
		path := filepath.Join(program.env["YARD_RUNTIME_ROOT"], name)
		value, err := os.Readlink(path)
		if err != nil {
			t.Fatal(err)
		}
		links[path] = value
	}
	return func() {
		for _, path := range paths {
			if !bytes.Equal(before[path], mustReadConfigFixture(t, path)) {
				t.Fatalf("native cleanup changed source/history: %s", filepath.Base(path))
			}
		}
		for path, value := range links {
			if got, err := os.Readlink(path); err != nil || got != value {
				t.Fatal("native cleanup changed release links")
			}
		}
		platform := program.options.InitPlatform.(*initPlatformFixture)
		if len(platform.applied) != 0 || platform.projectHooks != 0 {
			t.Fatal("native cleanup invoked activation/hooks")
		}
	}
}

func TestIntegrationCleanupRecoveryUsesConsistentNativeObservationContext(t *testing.T) {
	for _, confirmed := range []bool{true, false} {
		t.Run(fmt.Sprintf("confirmed=%t", confirmed), func(t *testing.T) {
			arguments := []string{"integration", "cleanup", "widget"}
			if confirmed {
				arguments = append(arguments, "--yes")
			}
			program, executor, recorded, home, output := nativeCleanupRecoveryCLIFixture(t, arguments)
			if !confirmed {
				program.options.Prompt = &testkit.Prompt{Answers: []bool{false}}
			}
			invariants := nativeCleanupRecoveryInvariants(t, program, home)
			code := program.Run(context.Background())
			observations := *recorded
			if len(observations) < 2 {
				t.Fatal("native capture and ordinary preparation were not both observed")
			}
			if !observations[0].operationIDPresent || !observations[1].operationIDPresent {
				t.Fatal("native capture and ordinary preparation must both bind the controller operation")
			}
			if observations[0].plan.Fingerprint != observations[1].plan.Fingerprint || !reflect.DeepEqual(observations[0].context, observations[1].context) || observations[0].plan.StateBinding() != observations[1].plan.StateBinding() {
				t.Fatal("unchanged native cleanup scope differed between gate and preparation")
			}
			if confirmed {
				if code != 0 || executor.applied != 1 {
					t.Fatalf("confirmed native cleanup failed: %d %s", code, output)
				}
			} else if code == 0 || executor.applied != 0 || len(program.options.Prompt.(*testkit.Prompt).Requests) != 1 {
				t.Fatalf("declined native cleanup was not consent-bound: %d %s", code, output)
			}
			invariants()
		})
	}
}

func TestIntegrationCleanupRecoveryRevalidatesFullNativeScopeAfterConsent(t *testing.T) {
	for _, scenario := range []string{"native-facts", "source", "target", "controller-binding"} {
		t.Run(scenario, func(t *testing.T) {
			program, executor, _, home, output := nativeCleanupRecoveryCLIFixture(t, []string{"integration", "cleanup", "widget"})
			invariants := nativeCleanupRecoveryInvariants(t, program, home)
			program.options.Prompt = &callbackPrompt{callback: func() {
				switch scenario {
				case "native-facts":
					executor.fingerprint = strings.Repeat("b", 64)
				case "source":
					testkit.WriteFile(t, filepath.Join(program.options.RepositoryRoot, "config/widget-cleanup.sh"), []byte("#!/bin/sh\n# changed native source\n"), 0o600)
				case "target":
					program.options.Incus.(*testkit.Incus).Reconcile.Instance.Config = map[string]string{"volatile.uuid": "replacement"}
				case "controller-binding":
					program.env["SUBYARD_OPERATION_ID"] = newOperationID()
				}
			}}
			if code := program.Run(context.Background()); code == 0 || executor.applied != 0 || len(program.options.Prompt.(*callbackPrompt).requests) != 1 {
				t.Fatalf("changed full native scope admitted without fresh consent: %d %s", code, output)
			}
			invariants()
		})
	}
}

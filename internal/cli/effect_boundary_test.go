package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
)

// lockOrderTrace records acquires of the three ranked locks and any acquire
// of a lower rank while a higher one is held.
type lockOrderTrace struct {
	mu         sync.Mutex
	held       []int
	acquired   []int
	violations []string
}

func traceLockOrder(t *testing.T) *lockOrderTrace {
	t.Helper()
	trace := &lockOrderTrace{}
	record := func(rank int) func() {
		trace.mu.Lock()
		defer trace.mu.Unlock()
		for _, held := range trace.held {
			if held > rank {
				trace.violations = append(trace.violations, strings.Join([]string{lockRankName(rank), "after", lockRankName(held)}, " "))
			}
		}
		trace.held = append(trace.held, rank)
		trace.acquired = append(trace.acquired, rank)
		return func() {
			trace.mu.Lock()
			defer trace.mu.Unlock()
			if index := slices.Index(trace.held, rank); index >= 0 {
				trace.held = slices.Delete(trace.held, index, index+1)
			}
		}
	}
	previousCLI, previousRelease := lockTrace, releasetransition.LockTrace
	lockTrace = record
	releasetransition.LockTrace = func() func() { return record(lockRankReleaseUpdate) }
	t.Cleanup(func() { lockTrace, releasetransition.LockTrace = previousCLI, previousRelease })
	return trace
}

func lockRankName(rank int) string {
	return map[int]string{lockRankOwnerBarrier: "owner barrier", lockRankReleaseUpdate: "release update lock", lockRankYard: "yard lock"}[rank]
}

// degradedScenario is a completed release whose named yard failed observation:
// the gate is yard-scoped, default is open and named admits only repair.
func degradedScenario(t *testing.T) yardFailureScenario {
	t.Helper()
	scenario := newYardFailureScenario(t, true, true, "default", "named")
	inspect := scenario.replayReleaseEngine(t)
	if outcome := scenario.converge(t); outcome.Status != releasetransition.StatusReady {
		t.Fatalf("baseline: %#v", outcome)
	}
	observeFailing(t, scenario, "named")
	recordReadiness(t, scenario, inspect)
	return scenario
}

// runWith runs one command with a prompt and returns its exit and output.
func runWith(t *testing.T, scenario yardFailureScenario, prompt confirmingPrompt, arguments ...string) (int, string) {
	t.Helper()
	program, calls, stdout, stderr := scenario.fixture.program(t, arguments, prompt, func(nestedCall, io.Writer) int { return 0 })
	code := program.Run(context.Background())
	output := stdout.String() + stderr.String()
	if len(*calls) != 0 {
		output += "\nnested: " + strings.Join((*calls)[0].arguments, " ")
	}
	return code, output
}

type boundaryRow struct {
	arguments []string
	// effect reports the fake-visible effects of a run.
	effect func(yardFailureScenario) int
	// exception names why the generic boundary run does not apply.
	exception string
}

func powerEffects(scenario yardFailureScenario) int {
	incus := scenario.fixture.cli.options.Incus.(*testkit.Incus)
	return len(incus.PowerUpdates) + len(incus.ConfigUpdates)
}

// Every gated mutating command has a boundary row. Flipping the release
// history during consent refuses a row before any effect; a positive run
// reaches its effect.
func TestEffectBoundaryTable(t *testing.T) {
	rows := map[string]boundaryRow{
		"stop":           {arguments: []string{"-Y", "named", "stop"}, effect: powerEffects},
		"start":          {exception: "boundary in preparedCommand.Execute, as stop; the host-free fixture has no Incus network policy adapter, which start needs before its effect"},
		"teardown":       {exception: "boundary in preparedCommand.Execute; the host-free fixture has no Incus network policy adapter, so teardown stops at its own prepare"},
		"init":           {exception: "boundary in preparedCommand.Execute; the fixture's failing observation stops init at its own prepare; runs in TestAdmittedInitAndConvergeNeverDeadlock"},
		"config":         {exception: "boundary in preparedCommand.Execute; config apply runs in TestConfigApplyAdmittedInProcessAndChildOnReadyPath"},
		"svc":            {exception: "legacy resource ingress: passes the boundary before its yard lock (resource_commands.go), TestLegacyResourceIngressPassesBoundaryBeforeYardLock"},
		"shell":          {exception: "no consent step: gate admission is the boundary"},
		"_project-state": {exception: "no consent step: gate admission is the boundary"},
		"sync":           {exception: "session and wire transport: checked at start only"},
		"code":           {exception: "session and wire transport: checked at start only"},
		"codex":          {exception: "session and wire transport: checked at start only"},
		"remote":         {exception: "session and wire transport: checked at start only"},
		"rpc":            {exception: "session and wire transport: each operation passes its own boundary"},
	}
	// Commands whose effects run through preparedCommand.Execute or an
	// orchestrator made by operationPlanner share the one boundary; their rows
	// assert that the hook is installed (TestOrchestratorCarriesTheBoundary).
	for _, name := range []string{"network", "cleanup", "keys", "ssh-agent", "provision", "service", "test-vms", "bind", "clone", "export", "remove", "up", "down", "host", "update", "migrate", "integration", "profile", "_authorize", "_state", "_migrate", "_release-transition", "_vm-start-policy", "_keys-exchange", "_keys-auto-sync", "_keys-init"} {
		rows[name] = boundaryRow{exception: "shared boundary: preparedCommand.Execute or the operationPlanner orchestrator hook"}
	}
	scenario := degradedScenario(t)
	program, _, _, _ := scenario.fixture.program(t, nil, nil, func(nestedCall, io.Writer) int { return 0 })
	for _, definition := range program.manifest.Commands() {
		if definition.Effect != command.EffectMutate {
			continue
		}
		if _, ok := rows[definition.Name]; !ok {
			t.Errorf("gated mutating command %q has no boundary row", definition.Name)
		}
	}
	journal := mustReadConfigFixture(t, scenario.fixture.journalPath)
	parsed, err := releasetransition.ParseJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Transaction = "tx-eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	flipped, err := releasetransition.MarshalJournal(parsed)
	if err != nil {
		t.Fatal(err)
	}
	for name, row := range rows {
		if row.exception != "" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			before := row.effect(scenario)
			code, output := runWith(t, scenario, confirmingPrompt{during: func() { testkit.WriteFile(t, scenario.fixture.journalPath, flipped, 0o600) }}, row.arguments...)
			testkit.WriteFile(t, scenario.fixture.journalPath, journal, 0o600)
			if code == 0 || !strings.Contains(output, "the release changed after confirmation") || row.effect(scenario) != before {
				t.Fatalf("flipped history: code=%d effects=%d->%d\n%s", code, before, row.effect(scenario), output)
			}
			code, output = runWith(t, scenario, confirmingPrompt{during: func() {}}, row.arguments...)
			if strings.Contains(output, "the release changed after confirmation") || row.effect(scenario) == before {
				t.Fatalf("positive run did not reach its effect: code=%d\n%s", code, output)
			}
		})
	}
}

// Every orchestrator the CLI plans with carries the effect boundary, so an
// adapter run without a prepared command still passes it.
func TestOrchestratorCarriesTheBoundary(t *testing.T) {
	scenario := degradedScenario(t)
	program, _, _, _ := scenario.fixture.program(t, nil, nil, func(nestedCall, io.Writer) int { return 0 })
	orchestrator := program.operationPlanner("boundary")
	if orchestrator.BeforeEffects == nil {
		t.Fatal("operationPlanner lost the effect boundary hook")
	}
	program.admittedHistory = &releaseHistoryToken{journal: "stale"}
	if err := orchestrator.BeforeEffects(context.Background()); err == nil || !strings.Contains(err.Error(), "changed after confirmation") {
		t.Fatalf("hook did not run the boundary: %v", err)
	}
}

// The ranked locks are only ever taken in order for the gate's flows.
func TestLockOrderOfGatedFlows(t *testing.T) {
	scenario := degradedScenario(t)
	for _, arguments := range [][]string{
		{"-Y", "named", "stop", "--yes"},
		{"-Y", "named", "init", "--yes"},
		{"-Y", "named", "teardown", "--yes"},
		{"-Y", "default", "config", "apply", "--yes"},
		{"-Y", "default", "config", "apply", "--all-local", "--yes"},
		{"-Y", "default", "config", "repair-alias", "--yes"},
		{"migrate", "--yes"},
	} {
		t.Run(strings.Join(arguments, "_"), func(t *testing.T) {
			trace := traceLockOrder(t)
			code, output := runWith(t, scenario, confirmingPrompt{during: func() {}}, arguments...)
			t.Logf("code=%d acquired=%v\n%s", code, trace.acquired, output)
			if len(trace.violations) != 0 {
				t.Fatalf("lock order violated: %v", trace.violations)
			}
		})
	}
	// The bounded stop over an unfinished activation.
	unfinished := newYardFailureScenario(t, false, false, "default", "named")
	inspect := unfinished.replayReleaseEngine(t)
	unfinished.runtime.failing(t, "named", true)
	unfinished.converge(t)
	recordReadiness(t, unfinished, inspect)
	trace := traceLockOrder(t)
	code, output := runWith(t, unfinished, confirmingPrompt{during: func() {}}, "-Y", "named", "stop", "--yes")
	if len(trace.violations) != 0 || !slices.Contains(trace.acquired, lockRankOwnerBarrier) {
		t.Fatalf("bounded stop: code=%d acquired=%v violations=%v\n%s", code, trace.acquired, trace.violations, output)
	}
}

// A migrate-like party holds the owner barrier and then the release update
// lock. An admitted repair run through program.Run finishes in both orders.
func TestAdmittedInitAndConvergeNeverDeadlock(t *testing.T) {
	for _, order := range []string{"migrate-first", "repair-first"} {
		t.Run(order, func(t *testing.T) {
			scenario := degradedScenario(t)
			program, _, _, _ := scenario.fixture.program(t, nil, nil, func(nestedCall, io.Writer) int { return 0 })
			loaded, err := program.loadContext("named")
			if err != nil {
				t.Fatal(err)
			}
			store, err := releasetransition.NewPOSIXV2Store(scenario.request.ConfigHome)
			if err != nil {
				t.Fatal(err)
			}
			migrate := func(holding chan<- struct{}) error {
				_, release, err := uninstall.OwnerLock(context.Background(), loaded.Context.Paths.DataHome, nil)
				if err != nil {
					return err
				}
				defer release()
				unlock, err := store.Lock()
				if err != nil {
					return err
				}
				if holding != nil {
					close(holding)
				}
				time.Sleep(200 * time.Millisecond)
				unlock()
				return nil
			}
			migrated := make(chan error, 1)
			repaired := make(chan string, 2)
			repair := func() {
				_, output := runWith(t, scenario, confirmingPrompt{during: func() {}}, "-Y", "named", "init", "--yes")
				repaired <- output
				_, output = runWith(t, scenario, confirmingPrompt{during: func() {}}, "-Y", "named", "stop", "--yes")
				repaired <- output
			}
			if order == "migrate-first" {
				holding := make(chan struct{})
				go func() { migrated <- migrate(holding) }()
				<-holding
				go repair()
			} else {
				go repair()
				go func() { migrated <- migrate(nil) }()
			}
			deadline := time.After(60 * time.Second)
			for range 2 {
				select {
				case output := <-repaired:
					if strings.Contains(output, mutationGateRefusalPrefix) {
						t.Fatalf("repair refused: %s", output)
					}
				case <-deadline:
					t.Fatal("admitted repair and converge deadlocked")
				}
			}
			select {
			case err := <-migrated:
				if err != nil {
					t.Fatal(err)
				}
			case <-deadline:
				t.Fatal("converge never finished")
			}
		})
	}
}

// With no injected applier: config apply admitted behind a non-ready gate
// spawns no child; on a ready release the dispatcher child inherits the owner
// barrier descriptor and can take the update lock without waiting.
func TestConfigApplyAdmittedInProcessAndChildOnReadyPath(t *testing.T) {
	stub := func(t *testing.T, scenario yardFailureScenario) string {
		root := t.TempDir()
		marker := filepath.Join(root, "child")
		script := filepath.Join(root, "yard")
		testkit.WriteFile(t, script, []byte("#!/bin/sh\nlock=$(find \""+scenario.request.ConfigHome+"\" -name update.lock | head -1)\nflock -n \"$lock\" true && took=yes || took=no\nprintf 'fd=%s took=%s args=%s\\n' \"${SUBYARD_OWNER_LOCK_FD:-}\" \"$took\" \"$*\" >> '"+marker+"'\n"), 0o700)
		return marker
	}
	for _, degraded := range []bool{true, false} {
		t.Run(map[bool]string{true: "admitted", false: "ready"}[degraded], func(t *testing.T) {
			scenario := newYardFailureScenario(t, true, true, "default", "named")
			inspect := scenario.replayReleaseEngine(t)
			scenario.converge(t)
			if degraded {
				observeFailing(t, scenario, "named")
			}
			recordReadiness(t, scenario, inspect)
			marker := stub(t, scenario)
			scenario.fixture.cli.options.Config = nil
			scenario.fixture.cli.options.DispatcherPath = filepath.Join(filepath.Dir(marker), "yard")
			code, output := runWith(t, scenario, confirmingPrompt{during: func() {}}, "-Y", "default", "config", "apply", "--yes")
			child, _ := os.ReadFile(marker)
			t.Logf("code=%d child=%q\n%s", code, child, output)
			if degraded && len(child) != 0 {
				t.Fatalf("admitted config apply spawned a child: %s", child)
			}
			if !degraded && len(child) != 0 && (!bytes.Contains(child, []byte("fd=")) || bytes.Contains(child, []byte("fd= ")) || !bytes.Contains(child, []byte("took=yes"))) {
				t.Fatalf("ready-path child lacked the barrier or waited: %s", child)
			}
		})
	}
}

// Legacy resource ingress takes its yard lock before its adapter runs, so it
// passes the effect boundary first. Its host-free fixture cannot be admitted
// behind a yard-scoped gate, so the order is pinned at the source.
func TestLegacyResourceIngressPassesBoundaryBeforeYardLock(t *testing.T) {
	source := string(mustReadConfigFixture(t, filepath.Join(repositoryRoot(t), "internal/cli/resource_commands.go")))
	boundary := strings.Index(source, "if err := cli.beginEffects(ctx); err != nil {")
	lock := strings.Index(source, "unlock, lockErr := lockIntegrationYard(ctx, loaded)")
	if boundary < 0 || lock < 0 || boundary > lock {
		t.Fatalf("legacy ingress locks its yard before the effect boundary: boundary=%d lock=%d", boundary, lock)
	}
}

// For each failure kind in yard A, yard B's observed drift survives: B admits
// only its repair commands. The core guest runtime shares this owner's code.
func TestObservationMatrixKeepsOtherYardsDrift(t *testing.T) {
	for _, kind := range []string{"observe-error", "invalid-config", "unreachable-agent"} {
		t.Run(kind, func(t *testing.T) {
			scenario := newYardFailureScenario(t, true, true, "default", "named")
			inspect := scenario.replayReleaseEngine(t)
			scenario.converge(t)
			testkit.WriteFile(t, scenario.runtime.state("default"), []byte(strings.Repeat("d", 64)), 0o600)
			switch kind {
			case "observe-error":
				observeFailing(t, scenario, "named")
			case "invalid-config":
				path := filepath.Join(scenario.request.ConfigHome, "yards", "named", "config.env")
				testkit.WriteFile(t, path, append(mustReadConfigFixture(t, path), []byte("SSH_PORT=99999\n")...), 0o600)
			case "unreachable-agent":
				observeFailing(t, scenario, "named")
				scenario.fixture.cli.options.Executor = lostAgentExecutor{scenario.fixture.cli.options.Executor, scenario.fixture.instanceName(t, "named")}
			}
			recordReadiness(t, scenario, inspect)
			program, _, _, _ := scenario.fixture.program(t, nil, nil, func(nestedCall, io.Writer) int { return 0 })
			outcome, inspection, err := program.inspectMutationGateInspection(context.Background(), "default")
			if err != nil {
				t.Fatal(err)
			}
			gate, ok := program.yardScopedGate(outcome, inspection)
			if !ok || !gate.state("default").drift || !gate.state("named").repairOnly() {
				t.Fatalf("%s: gate ok=%t yards=%v", kind, ok, gate.yards)
			}
		})
	}
}

// A materialized-config failure in one yard carries the drift it observed in
// the others; without attribution of the failure itself it attributes nothing.
func TestObservedYardsErrorCarriesOtherYardsDrift(t *testing.T) {
	drift := []releasetransition.V2YardAttribution{{Yard: "default", Kind: releasetransition.YardFindingDrift}}
	attributed := observedYardsError{materializedConfigActivationError{errors.New("guest read"), "named", "materialized config", "config status", "inspect", "/home"}, "/home", drift}
	findings := attributed.ActivationYardFindings()
	if len(findings) != 2 || findings[0] != (releasetransition.V2YardAttribution{Yard: "named", Kind: releasetransition.YardFindingFailed}) || findings[1] != drift[0] {
		t.Fatalf("findings: %v", findings)
	}
	if unattributed := (observedYardsError{errors.New("host"), "/home", drift}).ActivationYardFindings(); unattributed != nil {
		t.Fatalf("unattributed failure attributed: %v", unattributed)
	}
}

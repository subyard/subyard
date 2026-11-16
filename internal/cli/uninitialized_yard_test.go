package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

// observeFailing makes the sample handler's observe exit 2 in one yard, as on a
// running guest that a failed init left unprovisioned.
func observeFailing(t *testing.T, scenario yardFailureScenario, yard string, failing bool) {
	t.Helper()
	handler := filepath.Join(scenario.fixture.cli.options.RepositoryRoot, "config/profiles/sample/runtime.sh")
	marker := filepath.Join(scenario.runtime.root, "observe-fail-")
	payload := string(mustReadConfigFixture(t, handler))
	guard := "[ ! -e '" + marker + "'\"$yard\" ] || [ \"$1\" != observe ] || exit 2\n"
	if !strings.Contains(payload, guard) {
		payload = strings.Replace(payload, "yard=${SUBYARD_CONFIG_YARD_DIR##*/}\n", "yard=${SUBYARD_CONFIG_YARD_DIR##*/}\n"+guard, 1)
		testkit.WriteFile(t, handler, []byte(payload), 0o700)
	}
	if failing {
		testkit.WriteFile(t, marker+yard, nil, 0o600)
	} else if err := os.Remove(marker + yard); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// markYard writes managed power metadata on a yard's fixture instance.
func markYard(t *testing.T, fixture pendingInitFixture, yard, initialized string) {
	t.Helper()
	incus := fixture.cli.options.Incus.(*testkit.Incus)
	for key, instance := range incus.Instances {
		if instance.Name == fixture.instanceName(t, yard) {
			instance.Config = map[string]string{"user.subyard.managed": "true", "user.subyard.name": yard, "user.subyard.initialized": initialized, "user.subyard.disk_tmp": "v1"}
			incus.Instances[key] = instance
			return
		}
	}
	t.Fatalf("yard %s has no fixture instance", yard)
}

// recordReadiness writes a fresh readiness inspection for the replay engine.
func recordReadiness(t *testing.T, scenario yardFailureScenario, inspect string) releasetransition.ProcessResponse {
	t.Helper()
	readiness := scenario.invoke(t, releasetransition.ProcessInspect, nil)
	payload, err := json.Marshal(readiness)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, inspect, payload, 0o600)
	return readiness
}

// gateRefused runs one command and reports whether the release gate refused it.
func gateRefused(t *testing.T, scenario yardFailureScenario, arguments ...string) bool {
	t.Helper()
	integrations := &integrationRuntimeFixture{plan: reconcileruntime.IntegrationPlan{Fingerprint: strings.Repeat("c", 64), Scope: strings.Repeat("c", 64)}}
	scenario.fixture.cli.options.IntegrationRuntime = func(config.Loaded) IntegrationRuntime { return integrations }
	program, _, stdout, stderr := scenario.fixture.program(t, arguments, nil, func(nestedCall, io.Writer) int { return 0 })
	code := program.Run(context.Background())
	t.Logf("yard %s => %d\n%s%s", strings.Join(arguments, " "), code, stdout, stderr)
	return strings.Contains(stderr.String(), `"code":"dependency-unavailable"`)
}

// A failed first init leaves a running guest that is not provisioned. That
// yard is left to its own init: its failing observation neither blocks the
// release nor closes any yard's commands, and it reports one warning.
func TestUninitializedYardIsLeftToItsInit(t *testing.T) {
	scenario := newYardFailureScenario(t, true, true, "default", "named")
	inspect := scenario.replayReleaseEngine(t)
	if outcome := scenario.converge(t); outcome.Status != releasetransition.StatusReady {
		t.Fatalf("baseline: %#v", outcome)
	}
	markYard(t, scenario.fixture, "default", "true")
	markYard(t, scenario.fixture, "named", "false")
	observeFailing(t, scenario, "named", true)
	readiness := recordReadiness(t, scenario, inspect)
	outcome := readiness.Inspection.Outcome
	if outcome.Status != releasetransition.StatusReady || len(readiness.Inspection.Blockers) != 0 ||
		strings.Count(strings.Join(outcome.Warnings, "\n"), "not initialized") != 1 ||
		!strings.Contains(strings.Join(outcome.Warnings, "\n"), uninitializedYardWarning("named")) {
		payload, _ := json.Marshal(readiness)
		t.Fatalf("readiness with an uninitialized yard: %s", payload)
	}
	for _, arguments := range [][]string{
		{"-Y", "default", "stop"},
		{"-Y", "default", "init", "--yes"},
		{"-Y", "named", "stop"},
		{"-Y", "named", "init", "--yes"},
		{"-Y", "named", "teardown"},
	} {
		if gateRefused(t, scenario, arguments...) {
			t.Fatalf("yard %s was refused by the release gate", strings.Join(arguments, " "))
		}
	}

	// Once the yard's own init finishes, release work handles it normally: a
	// real failure of the initialized yard blocks again.
	markYard(t, scenario.fixture, "named", "true")
	readiness = recordReadiness(t, scenario, inspect)
	if outcome := readiness.Inspection.Outcome; outcome.Status == releasetransition.StatusReady ||
		strings.Contains(strings.Join(outcome.Warnings, "\n"), "not initialized") {
		payload, _ := json.Marshal(readiness)
		t.Fatalf("an initialized yard's failure was hidden: %s", payload)
	}
	if !gateRefused(t, scenario, "-Y", "default", "init", "--reset", "--yes") {
		t.Fatal("an initialized yard's failure left the release gate open")
	}
	observeFailing(t, scenario, "named", false)
	if outcome := scenario.converge(t); outcome.Status != releasetransition.StatusReady {
		t.Fatalf("converge after the yard's init: %#v", outcome)
	}
}

// Release repair never runs a hook in a yard whose init did not finish, even
// when its runtime is stale and its apply would fail, and the journal
// completes. After that yard's init, the next repair applies it normally.
func TestReleaseRepairSkipsUninitializedYard(t *testing.T) {
	scenario := newYardFailureScenario(t, false, false, "default", "named")
	markYard(t, scenario.fixture, "default", "true")
	markYard(t, scenario.fixture, "named", "false")
	scenario.runtime.failing(t, "named", true)
	outcome := scenario.converge(t)
	if outcome.Status != releasetransition.StatusReady ||
		!strings.Contains(strings.Join(outcome.Warnings, "\n"), uninitializedYardWarning("named")) {
		t.Fatalf("repair beside an uninitialized yard: %#v", outcome)
	}
	if applies := scenario.runtime.applies(t); len(applies) != 1 || applies[0] != "default" {
		t.Fatalf("hook applies: %v", applies)
	}
	if journal := scenario.journal(t); journal.Checkpoint != releasetransition.JournalComplete {
		t.Fatalf("journal: %s", journal.Checkpoint)
	}
	markYard(t, scenario.fixture, "named", "true")
	scenario.runtime.failing(t, "named", false)
	if inspection := scenario.invoke(t, releasetransition.ProcessInspect, nil); inspection.Inspection.Outcome.Status == releasetransition.StatusReady {
		t.Fatal("the initialized yard's stale runtime was not observed")
	}
	if outcome := scenario.converge(t); outcome.Status != releasetransition.StatusReady {
		t.Fatalf("repair after the yard's init: %#v", outcome)
	}
	if applies := scenario.runtime.applies(t); len(applies) != 2 || applies[1] != "named" {
		t.Fatalf("hook applies after init: %v", applies)
	}
}

// A yard left to its init stays left out for the rest of the engine process,
// in every yard-scoped owner, while host key tools never are.
func TestUninitializedYardDecisionOnlyNarrows(t *testing.T) {
	scenario := newYardFailureScenario(t, false, false, "default", "named")
	markYard(t, scenario.fixture, "named", "false")
	program, err := New(scenario.fixture.cli.options)
	if err != nil {
		t.Fatal(err)
	}
	operation := program.releaseActivationOperation(scenario.request)
	loaded, err := operation.resolveReleaseTransitionContext(context.Background(), "named", scenario.request.ConfigHome)
	if err != nil {
		t.Fatal(err)
	}
	target := configTarget{Name: "named", Loaded: loaded}
	// Materialized configuration leaves the running uninitialized yard like a
	// stopped one, without reading its guest.
	if assessment, err := operation.assessConfigTarget(context.Background(), target, true); err != nil || assessment.State != "stopped" || assessment.Changed {
		t.Fatalf("materialized config of an uninitialized yard: %#v err=%v", assessment, err)
	}
	if left, err := operation.releaseYardUninitialized(context.Background(), target); err != nil || !left {
		t.Fatalf("uninitialized yard: left=%t err=%v", left, err)
	}
	markYard(t, scenario.fixture, "named", "true")
	if left, err := operation.releaseYardUninitialized(context.Background(), target); err != nil || !left {
		t.Fatalf("decision widened within the process: left=%t err=%v", left, err)
	}
	keyTools := &profileRuntimeActivationReconciler{cli: operation, request: scenario.request, id: "key-tools-runtime"}
	if kept, uninitialized, failures := keyTools.splitUninitialized(context.Background(), operation, []configTarget{target}); len(kept) != 1 || len(uninitialized) != 0 || len(failures) != 0 {
		t.Fatalf("key tools deferred a yard: kept=%d uninitialized=%v failures=%v", len(kept), uninitialized, failures)
	}
	fresh, err := New(scenario.fixture.cli.options)
	if err != nil {
		t.Fatal(err)
	}
	if left, err := fresh.releaseActivationOperation(scenario.request).releaseYardUninitialized(context.Background(), target); err != nil || left {
		t.Fatalf("a fresh process kept an initialized yard out: left=%t err=%v", left, err)
	}
}

// Start of a yard that has never finished its first init refuses before any
// power change and names init; a yard whose re-init failed after a finished
// init, and a yard from a release before the completion mark, still start.
func TestStartRefusesNeverInitializedYardBeforePower(t *testing.T) {
	for _, test := range []struct {
		name, initialized, ever string
		refused                 bool
	}{
		{name: "never initialized", initialized: "false", refused: true},
		{name: "re-init failed", initialized: "false", ever: "true"},
		{name: "upgraded initialized", initialized: "true"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newConfigApplyRepairFixture(t, false)
			options := fixture.cli.options
			native := options.Incus.(*testkit.Incus)
			key := "subyard/yard"
			instance := native.Instances[key]
			instance.Status = "Stopped"
			instance.Config = map[string]string{
				"user.subyard.desired_power": "stopped", "user.subyard.managed": "true",
				"user.subyard.name": "default", "user.subyard.bridge": "incusbr0",
				"user.subyard.initialized": test.initialized, "boot.autostart": "false",
			}
			if test.ever != "" {
				instance.Config["user.subyard.ever_initialized"] = test.ever
			}
			native.Instances[key] = instance
			runner := &testkit.ScriptedAdapter{}
			options.AdapterRunner = runner
			program, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			definition, _ := program.manifest.Lookup("start")
			prepared := &preparedCommand{CLI: program, Definition: definition, Loaded: loaded, Arguments: []string{"--yes"}}
			err = prepared.prepareLifecycle(context.Background(), nil)
			if !test.refused {
				if err != nil {
					t.Fatalf("start refused: %v", err)
				}
				return
			}
			if err == nil || err.Error() != "yard default has not completed its first init; run yard -Y default init" {
				t.Fatalf("start of a never-initialized yard: %v", err)
			}
			after := native.Instances[key]
			if len(runner.Requests) != 0 || len(native.PowerUpdates) != 0 || after.Status != "Stopped" ||
				after.Config["user.subyard.desired_power"] != "stopped" || after.Config["user.subyard.initialized"] != "false" {
				t.Fatalf("refused start changed power: requests=%v updates=%v instance=%#v", runner.Requests, native.PowerUpdates, after)
			}
		})
	}
}

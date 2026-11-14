package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/shellquote"
	"github.com/Subyard/Subyard/internal/testkit"
)

// yardFailureRuntime is a sample profile runtime with per-yard native state.
// A yard with a fail marker leaves a partial actual and exits non-zero; with
// typed set it reports exactly one yard-local record.
type yardFailureRuntime struct {
	root, calls string
}

func (runtime yardFailureRuntime) state(yard string) string {
	return filepath.Join(runtime.root, "state-"+yard)
}
func (runtime yardFailureRuntime) fail(yard string) string {
	return filepath.Join(runtime.root, "fail-"+yard)
}

func (runtime yardFailureRuntime) failing(t *testing.T, yard string, failing bool) {
	t.Helper()
	if failing {
		testkit.WriteFile(t, runtime.fail(yard), nil, 0o600)
	} else if err := os.Remove(runtime.fail(yard)); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func (runtime yardFailureRuntime) applies(t *testing.T) []string {
	t.Helper()
	payload, err := os.ReadFile(runtime.calls)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var result []string
	for _, line := range strings.Split(strings.TrimSpace(string(payload)), "\n") {
		if fields := strings.Fields(line); len(fields) > 1 && fields[1] == "apply" {
			result = append(result, fields[0])
		}
	}
	return result
}

func installYardFailureRuntime(t *testing.T, root string, declared, typed bool) yardFailureRuntime {
	t.Helper()
	runtime := yardFailureRuntime{root: root, calls: filepath.Join(root, "sample-calls")}
	declaration := `{"schema_version":1,"runtime":{"activation_id":"sample-runtime","handler":"runtime.sh"}}`
	if declared {
		declaration = `{"schema_version":1,"runtime":{"activation_id":"sample-runtime","handler":"runtime.sh","failure_scope":"yard"}}`
	}
	record := ":"
	if typed {
		record = `printf '%s\n' "subyard-profile-runtime-diagnostic-v1 $SUBYARD_PROFILE_RUNTIME_DIAGNOSTIC_NONCE apply yard-local" >&2`
	}
	testkit.WriteFile(t, filepath.Join(root, "config/profiles/sample/profile.json"), []byte(declaration), 0o600)
	testkit.WriteFile(t, filepath.Join(root, "config/profiles/sample/runtime.sh"), []byte(fmt.Sprintf(`#!/bin/sh
set -eu
yard=${SUBYARD_CONFIG_YARD_DIR##*/}
printf '%%s %%s\n' "$yard" "$*" >> '%[1]s'
case " ${ENVIRONMENT_PROFILES:-} " in
*" sample "*) ;;
*) printf '%%s\n' '{"state":"absent","actual":"","desired":""}'; exit 0 ;;
esac
state='%[2]s'-$yard
[ -f "$state" ] || printf '%%s' %[3]s > "$state"
actual=$(cat "$state")
case "$1" in
observe)
  if [ "$actual" = %[4]s ]; then printf '{"state":"current","actual":"%%s","desired":"%%s"}\n' "$actual" "$actual"
  else printf '{"state":"stale","actual":"%%s","desired":"%%s"}\n' "$actual" %[4]s; fi ;;
apply)
  [ "$3" = "$actual" ] && [ "$4" = %[4]s ] || exit 17
  if [ -e '%[5]s'-"$yard" ]; then printf '%%s' %[6]s > "$state"; %[7]s; exit 99; fi
  printf '%%s' "$4" > "$state"
  printf '{"state":"current","actual":"%%s","desired":"%%s"}\n' "$4" "$4" ;;
*) exit 18 ;;
esac
`, runtime.calls, filepath.Join(root, "state"), strings.Repeat("a", 64), strings.Repeat("b", 64), filepath.Join(root, "fail"), strings.Repeat("d", 64), record)), 0o700)
	return runtime
}

type yardFailureScenario struct {
	fixture pendingInitFixture
	runtime yardFailureRuntime
	request releasetransition.ProcessRequest
	// owners are further activation owners of every process, after the runtime.
	owners []releasetransition.V2ActivationReconciler
}

// newYardFailureScenario selects the sample runtime in the named yards' installed
// and desired configuration. With ordinaryUpdate the unfinished journal is the
// activation of an update whose source differs from its target.
func newYardFailureScenario(t *testing.T, declared, typed bool, selected ...string) yardFailureScenario {
	t.Helper()
	yards := map[string]string{"default": ""}
	named := slices.Contains(selected, "named")
	if named {
		yards["named"] = ""
	}
	for _, yard := range selected {
		yards[yard] = "ENVIRONMENT_PROFILES=sample\n"
	}
	fixture := newPendingInitFixture(t, named, yards)
	runtime := installYardFailureRuntime(t, fixture.cli.options.RepositoryRoot, declared, typed)
	base := fixture.cli.options.Executor.(installedSettingsExecutor)
	base.guest = map[string]string{"ENVIRONMENT_PROFILES": "", "DEV_USER": "dev", "DEV_UID": "1000", "ALLOWS_CODING_TOOLS": "true"}
	base.byInstance = map[string]installedSettingsExecutor{}
	for _, yard := range selected {
		installed := base
		installed.byInstance = nil
		installed.profiles = []string{"sample"}
		installed.guest = map[string]string{"ENVIRONMENT_PROFILES": "sample", "DEV_USER": "dev", "DEV_UID": "1000", "ALLOWS_CODING_TOOLS": "true"}
		base.byInstance[fixture.instanceName(t, yard)] = installed
	}
	fixture.cli.options.Executor = base
	request, _ := repairRequest(t, fixture)
	request.SchemaVersion, request.Yard = releasetransition.ProcessProtocolSchemaV1, "default"
	request.InheritedSettingIDs = fixture.cli.releaseTransitionInheritedSettingIDs()
	return yardFailureScenario{fixture: fixture, runtime: runtime, request: request}
}

// invoke runs one native owner process for the release transition request.
func (scenario yardFailureScenario) invoke(t *testing.T, mode releasetransition.ProcessMode, execution *releasetransition.Execution) releasetransition.ProcessResponse {
	t.Helper()
	program, err := New(scenario.fixture.cli.options)
	if err != nil {
		t.Fatal(err)
	}
	request := scenario.request
	request.Mode, request.Execution = mode, execution
	response, err := executeReleaseTransitionRequest(context.Background(), program.options.RepositoryRoot, request,
		func(_ releasetransition.PlanToken, grant releasetransition.Authorization) bool { return grant != "" },
		append([]releasetransition.V2ActivationReconciler{program.profileActivationReconciler(request, "sample-runtime")}, scenario.owners...),
		releaseTransitionOwnerFixture{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func (scenario yardFailureScenario) converge(t *testing.T) releasetransition.Outcome {
	t.Helper()
	inspection := scenario.invoke(t, releasetransition.ProcessInspect, nil)
	if inspection.Inspection == nil || len(inspection.Inspection.Blockers) != 0 {
		payload, _ := json.Marshal(inspection)
		t.Fatalf("repair inspection: %s", payload)
	}
	converged := scenario.invoke(t, releasetransition.ProcessConverge, &releasetransition.Execution{Plan: inspection.Inspection.Plan, Authorization: "fixture-confirmed-repair"})
	if converged.Outcome == nil {
		t.Fatal("converge returned no outcome")
	}
	return *converged.Outcome
}

func (scenario yardFailureScenario) journal(t *testing.T) releasetransition.JournalRecord {
	t.Helper()
	_, journal := repairRequest(t, scenario.fixture)
	return journal
}

func requirePendingRuntimeOutcome(t *testing.T, outcome releasetransition.Outcome, yards ...string) {
	t.Helper()
	// Each yard is its own blocker; the outcome names all and continues with the first.
	if outcome.Status != releasetransition.StatusOperatorActionRequired || outcome.Code != releasetransition.CodeDependencyUnavailable ||
		outcome.Retry != shellquote.Command([]string{"yard", "-Y", yards[0], "init"}) {
		t.Fatalf("pending outcome: %#v", outcome)
	}
	for _, yard := range yards {
		if !strings.Contains(outcome.Message, "yard "+yard+": profile sample runtime failed (diagnose: yard -Y "+yard+" profile status sample); "+
			"profile not applied; fix it and run yard -Y "+yard+" init, or run yard -Y "+yard+" profile disable sample then init; the release is ready") {
			t.Fatalf("pending message omits yard %s: %s", yard, outcome.Message)
		}
	}
}

// A declared runtime's typed failure in an active release repair completes the
// journal and leaves the yard pending its init; readiness of a fresh process
// reports the same pending item without repeating the hook.
func TestDeclaredRuntimeYardFailureCompletesActiveReleaseRepair(t *testing.T) {
	scenario := newYardFailureScenario(t, true, true, "default")
	scenario.runtime.failing(t, "default", true)
	outcome := scenario.converge(t)
	requirePendingRuntimeOutcome(t, outcome, "default")
	if journal := scenario.journal(t); journal.Checkpoint != releasetransition.JournalComplete || journal.Releases.From != journal.Releases.Target {
		t.Fatalf("repair journal: %s %s -> %s", journal.Checkpoint, journal.Releases.From, journal.Releases.Target)
	}
	if applies := scenario.runtime.applies(t); !slices.Equal(applies, []string{"default"}) {
		t.Fatalf("hook applies: %v", applies)
	}
	readiness := scenario.invoke(t, releasetransition.ProcessInspect, nil)
	if readiness.Inspection == nil || readiness.Inspection.Resume != nil || len(readiness.Inspection.Blockers) != 1 ||
		readiness.Inspection.Blockers[0].Resource != "activation.sample-runtime.pending-configuration" ||
		readiness.Inspection.Blockers[0].Retry != shellquote.Command([]string{"yard", "-Y", "default", "init"}) {
		payload, _ := json.Marshal(readiness)
		t.Fatalf("fresh readiness: %s", payload)
	}
	if !pendingIntegrationConfigurationShape(readiness.Inspection.Outcome, readiness.Inspection) {
		t.Fatal("readiness is not the pending configuration shape")
	}
	if applies := scenario.runtime.applies(t); len(applies) != 1 {
		t.Fatalf("readiness repeated the hook: %v", applies)
	}
	// After the cause is fixed, the yard's init repeats the apply from the
	// partial state and the release becomes ready.
	scenario.runtime.failing(t, "default", false)
	program, err := New(scenario.fixture.cli.options)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareRuntimeOwnerInit(t, program).ApplyStage(context.Background(), ports.ReconcileStageProfileRuntimes); err != nil {
		t.Fatalf("init after the fix: %v", err)
	}
	ready := scenario.invoke(t, releasetransition.ProcessInspect, nil)
	if ready.Inspection == nil || ready.Inspection.Outcome.Status != releasetransition.StatusReady || len(ready.Inspection.Blockers) != 0 {
		payload, _ := json.Marshal(ready)
		t.Fatalf("readiness after init: %s", payload)
	}
	if applies := scenario.runtime.applies(t); !slices.Equal(applies, []string{"default", "default"}) {
		t.Fatalf("hook applies: %v", applies)
	}
	// The converged init retires the record (as yard init does after its
	// stages). The same partial state later is ordinary drift, not the old
	// pending item, and repair applies it again.
	if err := removeYardActivationPendingRecords(scenario.request.ConfigHome, "default", []string{"sample-runtime"}); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, scenario.runtime.state("default"), []byte(strings.Repeat("d", 64)), 0o600)
	drift := scenario.invoke(t, releasetransition.ProcessInspect, nil)
	if drift.Inspection == nil || len(drift.Inspection.Blockers) != 0 || drift.Inspection.Outcome.Status == releasetransition.StatusReady {
		payload, _ := json.Marshal(drift)
		t.Fatalf("resurrected pending item: %s", payload)
	}
	if outcome := scenario.converge(t); outcome.Status != releasetransition.StatusReady {
		t.Fatalf("repair of ordinary drift: %#v", outcome)
	}
	if applies := scenario.runtime.applies(t); !slices.Equal(applies, []string{"default", "default", "default"}) {
		t.Fatalf("repair skipped the apply: %v", applies)
	}
}

// A record that no longer matches is retired when the repair applies again,
// and a converged owner retires its record.
func TestRuntimeOwnerRetiresStalePendingRecords(t *testing.T) {
	scenario := newYardFailureScenario(t, true, true, "default")
	scenario.runtime.failing(t, "default", true)
	requirePendingRuntimeOutcome(t, scenario.converge(t), "default")
	path, err := activationPendingRecordPath(scenario.request.ConfigHome, "default", "sample-runtime")
	if err != nil {
		t.Fatal(err)
	}
	// The native state changes after the failure: the record no longer matches.
	testkit.WriteFile(t, scenario.runtime.state("default"), []byte(strings.Repeat("e", 64)), 0o600)
	scenario.runtime.failing(t, "default", false)
	if outcome := scenario.converge(t); outcome.Status != releasetransition.StatusReady {
		t.Fatalf("repair after changed state: %#v", outcome)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stale record was kept: %v", err)
	}
}

// Undeclared owners and untyped failures keep blocking recovery.
func TestUndeclaredOrUntypedRuntimeFailureKeepsRecovery(t *testing.T) {
	for _, test := range []struct {
		name            string
		declared, typed bool
	}{{"undeclared", false, true}, {"untyped", true, false}} {
		t.Run(test.name, func(t *testing.T) {
			scenario := newYardFailureScenario(t, test.declared, test.typed, "default")
			scenario.runtime.failing(t, "default", true)
			outcome := scenario.converge(t)
			if outcome.ReachedGoal || strings.Contains(outcome.Message, "profile not applied") || scenario.journal(t).Checkpoint != releasetransition.JournalReconciling {
				t.Fatalf("blocking failure was deferred: %#v", outcome)
			}
			records, _ := filepath.Glob(filepath.Join(scenario.request.ConfigHome, "activation-pending", "v1", "*", "*.json"))
			if len(records) != 0 {
				t.Fatalf("blocking failure recorded pending configuration: %v", records)
			}
		})
	}
}

// Every typed yard failure is collected in stable order; the loop does not
// stop at the first yard.
func TestRuntimeYardFailuresAreCollectedForEveryYard(t *testing.T) {
	scenario := newYardFailureScenario(t, true, true, "default", "named")
	scenario.runtime.failing(t, "default", true)
	scenario.runtime.failing(t, "named", true)
	requirePendingRuntimeOutcome(t, scenario.converge(t), "default", "named")
	if applies := scenario.runtime.applies(t); !slices.Equal(applies, []string{"default", "named"}) {
		t.Fatalf("hook applies: %v", applies)
	}
	readiness := scenario.invoke(t, releasetransition.ProcessInspect, nil)
	if readiness.Inspection == nil || len(readiness.Inspection.Blockers) != 2 {
		payload, _ := json.Marshal(readiness)
		t.Fatalf("readiness: %s", payload)
	}
	for index, yard := range []string{"default", "named"} {
		blocker := readiness.Inspection.Blockers[index]
		if blocker.Resource != "activation.sample-runtime.pending-configuration" || blocker.Retry != shellquote.Command([]string{"yard", "-Y", yard, "init"}) {
			t.Fatalf("blocker %d: %#v", index, blocker)
		}
	}
}

// publishOrdinaryUpdateActivation leaves the unfinished activation of an
// ordinary update: a blocking failure publishes the journal with the owner's
// authorized scope, whose source then names the update's previous release.
func (scenario yardFailureScenario) publishOrdinaryUpdateActivation(t *testing.T) {
	t.Helper()
	root := scenario.fixture.cli.options.RepositoryRoot
	declaration := filepath.Join(root, "config/profiles/sample/profile.json")
	declared := mustReadConfigFixture(t, declaration)
	testkit.WriteFile(t, declaration, []byte(`{"schema_version":1,"runtime":{"activation_id":"sample-runtime","handler":"runtime.sh"}}`), 0o600)
	if outcome := scenario.converge(t); outcome.ReachedGoal {
		t.Fatalf("fixture activation did not stop: %#v", outcome)
	}
	testkit.WriteFile(t, declaration, declared, 0o600)
	journal := scenario.journal(t)
	if journal.Checkpoint != releasetransition.JournalReconciling {
		t.Fatalf("no unfinished activation: %s", journal.Checkpoint)
	}
	journal.Releases.From, journal.Releases.Previous = "release-a", nil
	payload, err := releasetransition.MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, scenario.fixture.journalPath, payload, 0o600)
	for _, state := range []string{"default", "named"} {
		if err := os.Remove(scenario.runtime.state(state)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	if err := os.Remove(scenario.runtime.calls); err != nil {
		t.Fatal(err)
	}
}

// The activation of an ordinary update completes when one yard's declared
// runtime fails with its typed cause; only that yard waits for its init.
func TestDeclaredRuntimeYardFailureCompletesOrdinaryUpdateActivation(t *testing.T) {
	scenario := newYardFailureScenario(t, true, true, "named")
	scenario.runtime.failing(t, "named", true)
	scenario.publishOrdinaryUpdateActivation(t)
	resume := scenario.invoke(t, releasetransition.ProcessInspect, nil)
	if resume.Inspection == nil || resume.Inspection.Resume == nil || len(resume.Inspection.Blockers) != 0 {
		payload, _ := json.Marshal(resume)
		t.Fatalf("ordinary update resume: %s", payload)
	}
	converged := scenario.invoke(t, releasetransition.ProcessConverge, &releasetransition.Execution{Plan: resume.Inspection.Plan})
	requirePendingRuntimeOutcome(t, *converged.Outcome, "named")
	journal := scenario.journal(t)
	if journal.Checkpoint != releasetransition.JournalComplete || journal.Releases.From == journal.Releases.Target {
		t.Fatalf("update journal: %s %s -> %s", journal.Checkpoint, journal.Releases.From, journal.Releases.Target)
	}
	readiness := scenario.invoke(t, releasetransition.ProcessInspect, nil)
	if readiness.Inspection == nil || len(readiness.Inspection.Blockers) != 1 || !pendingIntegrationConfigurationShape(readiness.Inspection.Outcome, readiness.Inspection) {
		payload, _ := json.Marshal(readiness)
		t.Fatalf("fresh readiness: %s", payload)
	}
	if applies := scenario.runtime.applies(t); !slices.Equal(applies, []string{"named"}) {
		t.Fatalf("hook applies: %v", applies)
	}
}

// replayReleaseEngine installs a native engine that answers inspection from a
// file the test records from the in-process owner, and adds the public
// configuration and profile commands to the fixture registry.
func (scenario *yardFailureScenario) replayReleaseEngine(t *testing.T) string {
	t.Helper()
	root := scenario.fixture.cli.options.RepositoryRoot
	inspect := filepath.Join(root, "engine-inspect.json")
	writePendingRepairEngine(t, &scenario.fixture, []byte(fmt.Sprintf(`#!/bin/sh
if [ "$1" = --version ]; then printf 'yard-engine 1.2.3\n'; exit; fi
[ "$1" = _release-transition ] || exit 64
cat >/dev/null
case "$request" in *) cat '%s' ;; esac
`, inspect)))
	registry := filepath.Join(root, "config", "commands.registry")
	shipped := string(mustReadConfigFixture(t, filepath.Join(repositoryRoot(t), "config", "commands.registry")))
	extra := ""
	for _, line := range strings.Split(shipped, "\n") {
		if strings.HasPrefix(line, "config|") || strings.HasPrefix(line, "profile|") {
			extra += line + "\n"
		}
	}
	testkit.WriteFile(t, registry, append(mustReadConfigFixture(t, registry), extra...), 0o600)
	request, _ := repairRequest(t, scenario.fixture)
	scenario.request.ArtifactDigest = request.ArtifactDigest
	return inspect
}

// Completed readiness with a pending yard reports the same item to every
// reader, leaves unrelated commands to their own guards and admits that
// yard's init.
func TestPendingRuntimeConfigurationReadersAndAdmission(t *testing.T) {
	scenario := newYardFailureScenario(t, true, true, "default")
	inspect := scenario.replayReleaseEngine(t)
	scenario.runtime.failing(t, "default", true)
	requirePendingRuntimeOutcome(t, scenario.converge(t), "default")
	readiness := scenario.invoke(t, releasetransition.ProcessInspect, nil)
	payload, err := json.Marshal(readiness)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, inspect, payload, 0o600)
	// Integration plans are outside this owner; they are current.
	integrations := &integrationRuntimeFixture{plan: reconcileruntime.IntegrationPlan{Fingerprint: strings.Repeat("c", 64), Scope: strings.Repeat("c", 64)}}
	scenario.fixture.cli.options.IntegrationRuntime = func(config.Loaded) IntegrationRuntime { return integrations }
	gateRefusal := `"code":"dependency-unavailable"`
	for _, test := range []struct {
		arguments []string
		gated     bool
		output    bool
	}{
		{arguments: []string{"migrate", "--check"}, output: true},
		{arguments: []string{"-Y", "default", "config", "status"}, output: true},
		{arguments: []string{"-Y", "default", "status"}, output: true},
		{arguments: []string{"-Y", "default", "stop"}},
		{arguments: []string{"-Y", "default", "teardown"}},
		{arguments: []string{"-Y", "default", "init", "--yes"}},
		{arguments: []string{"-Y", "default", "init", "--reset", "--yes"}, gated: true},
	} {
		program, _, stdout, stderr := scenario.fixture.program(t, test.arguments, nil, func(nestedCall, io.Writer) int { return 0 })
		code := program.Run(context.Background())
		output := stdout.String() + stderr.String()
		t.Logf("yard %s => %d\n%s", strings.Join(test.arguments, " "), code, output)
		if strings.Contains(stderr.String(), gateRefusal) != test.gated {
			t.Fatalf("yard %s gate refusal=%t want %t: %s", strings.Join(test.arguments, " "), !test.gated, test.gated, output)
		}
		if test.output && !strings.Contains(output, "yard default: profile sample runtime failed") {
			t.Fatalf("yard %s omits the pending item: %s", strings.Join(test.arguments, " "), output)
		}
	}
}

// An unfinished repair of the active release applies the installed selection;
// a source reduction cannot release it and is refused before any write.
func TestReduceSelectionRefusesUnfinishedActiveReleaseRepair(t *testing.T) {
	scenario := newYardFailureScenario(t, false, false, "default")
	scenario.replayReleaseEngine(t)
	scenario.runtime.failing(t, "default", true)
	if outcome := scenario.converge(t); outcome.ReachedGoal || scenario.journal(t).Checkpoint != releasetransition.JournalReconciling {
		t.Fatalf("fixture repair did not stop: %#v", outcome)
	}
	path := filepath.Join(scenario.request.ConfigHome, "yards", "default", "config.env")
	before := mustReadConfigFixture(t, path)
	program, _, stdout, stderr := scenario.fixture.program(t, []string{"-Y", "default", "config", "reduce-selection", "ENVIRONMENT_PROFILES", "", "--scope", "yard", "--local", "--yes"}, nil, func(nestedCall, io.Writer) int { return 0 })
	if code := program.Run(context.Background()); code == 0 {
		t.Fatalf("reduction admitted: %s %s", stdout, stderr)
	}
	if !strings.Contains(stderr.String(), `applies the installed ENVIRONMENT_PROFILES="sample" that yard init recorded`) ||
		!strings.Contains(stderr.String(), "run yard migrate") || !strings.Contains(stderr.String(), "yard -Y default init") {
		t.Fatalf("refusal does not name the installed selection and exits: %s", stderr)
	}
	if !bytes.Equal(before, mustReadConfigFixture(t, path)) {
		t.Fatal("refused reduction wrote the source")
	}
}

// Pending items of different owners and yards never refuse each other's init:
// a yard's init is admitted when one pending blocker names it.
func TestPendingInitGateAdmitsEachNamedYardAcrossOwners(t *testing.T) {
	blocker := func(resource, yard string) string {
		retry := strings.ReplaceAll(shellquote.Command([]string{"yard", "-Y", yard, "init"}), "'", `'\''`)
		return fmt.Sprintf(`,{"code":"dependency-unavailable","resource":%q,"message":"yard %s pending","retry":"%s"}`, resource, yard, retry)
	}
	for _, test := range []struct {
		name, extra     string
		admitted, other []string
	}{
		{name: "two runtime owners", extra: blocker("activation.other-runtime.pending-configuration", "named"), admitted: []string{"default", "named"}},
		// Materialized configuration pending keeps its own rule: every blocker
		// must name the init, so it refuses a runtime item's init too.
		{name: "materialized and runtime", extra: `,{"code":"dependency-unavailable","resource":"activation.materialized-config.pending-configuration","message":"yard named: move the unowned file aside","retry":"incus exec yard-named -- namei -l /home/dev/.codex"}`,
			other: []string{"default", "named"}},
		// An undeclared owner's blocker keeps the materialized rule as well.
		{name: "undeclared owner", extra: blocker("activation.unknown-runtime.pending-configuration", "named"), other: []string{"default", "named"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := pendingInitGateFixtureFor(t, true, "default", "activation.sample-runtime.pending-configuration", test.extra)
			declareYardScopedRuntimes(t, &fixture)
			for _, yard := range test.admitted {
				if !fixture.cli.pendingInitRepairGate(context.Background(), yard, fixture.outcome) {
					t.Fatalf("init of %s refused", yard)
				}
			}
			for _, yard := range test.other {
				if fixture.cli.pendingInitRepairGate(context.Background(), yard, fixture.outcome) {
					t.Fatalf("init of %s admitted without a pending item", yard)
				}
			}
		})
	}
	// A yard without a pending item of its own needs no repair permit: its init
	// is admitted ordinarily while the other yards stay pending.
	fixture := pendingInitGateFixtureFor(t, true, "default", "activation.sample-runtime.pending-configuration", blocker("activation.other-runtime.pending-configuration", "named"))
	declareYardScopedRuntimes(t, &fixture)
	outcome, inspection, err := fixture.cli.inspectMutationGateInspection(context.Background(), "third")
	if err != nil || fixture.cli.pendingInitRepairGate(context.Background(), "third", fixture.outcome) || !fixture.cli.otherYardsPendingOnly(outcome, inspection, "third") {
		t.Fatalf("third yard admission: %v", err)
	}
	// Any blocker that is not pending configuration keeps every init gated.
	fixture = pendingInitGateFixtureFor(t, true, "default", "activation.sample-runtime.pending-configuration",
		`,{"code":"dependency-unavailable","resource":"activation.other-runtime","message":"untyped","retry":"run yard update --check"}`)
	declareYardScopedRuntimes(t, &fixture)
	if fixture.cli.pendingInitRepairGate(context.Background(), "default", fixture.outcome) {
		t.Fatal("a release blocker admitted init")
	}
}

// declareYardScopedRuntimes declares the sample runtime and a second runtime
// owner with the yard failure scope and reloads the fixture CLI.
func declareYardScopedRuntimes(t *testing.T, fixture *configApplyRepairFixture) {
	t.Helper()
	root := fixture.cli.options.RepositoryRoot
	for name, id := range map[string]string{"sample": "sample-runtime", "other": "other-runtime"} {
		if err := os.MkdirAll(filepath.Join(root, "config/profiles", name), 0o700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(root, "config/profiles", name, "profile.json"),
			[]byte(`{"schema_version":1,"runtime":{"activation_id":"`+id+`","handler":"runtime.sh","failure_scope":"yard"}}`), 0o600)
		testkit.WriteFile(t, filepath.Join(root, "config/profiles", name, "runtime.sh"),
			[]byte("#!/bin/sh\nprintf '%s\\n' '{\"state\":\"absent\",\"actual\":\"\",\"desired\":\"\"}'\n"), 0o700)
	}
	program, err := New(fixture.cli.options)
	if err != nil {
		t.Fatal(err)
	}
	fixture.cli = program
}

// Teardown retires the removed yard's records and only that yard's.
func TestTeardownRetiresRemovedYardPendingRecords(t *testing.T) {
	home := filepath.Join(testkit.TempDir(t), "config")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, yard := range []string{"named", "default"} {
		if err := writeActivationPendingRecord(home, activationPendingRecord{Yard: yard, Project: "p", Instance: "i", Owner: "sample-runtime",
			Actual: strings.Repeat("d", 64), Desired: strings.Repeat("b", 64), Inputs: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	scenario := newYardFailureScenario(t, true, true, "default")
	loaded := config.Loaded{Context: domain.Context{YardName: "named", AccessKind: domain.AccessLocal, Paths: domain.RuntimePaths{ConfigHome: home}}}
	program, err := New(scenario.fixture.cli.options)
	if err != nil {
		t.Fatal(err)
	}
	if err := program.retireTornDownActivationPending(loaded); err != nil {
		t.Fatal(err)
	}
	for yard, kept := range map[string]bool{"named": false, "default": true} {
		path, _ := activationPendingRecordPath(home, yard, "sample-runtime")
		if _, err := os.Stat(path); (err == nil) != kept {
			t.Fatalf("yard %s record kept=%v: %v", yard, !kept, err)
		}
	}
}

// A typed failure on one yard and a blocking failure on another keep the release
// recovering; the recorded failure is then not presented as release-ready
// pending configuration with an init step the recovering release refuses.
func TestMixedTypedAndBlockingFailuresDoNotClaimReady(t *testing.T) {
	scenario := newYardFailureScenario(t, true, true, "default", "named")
	handler := filepath.Join(scenario.fixture.cli.options.RepositoryRoot, "config/profiles/sample/runtime.sh")
	payload := string(mustReadConfigFixture(t, handler))
	patched := strings.Replace(payload, "printf '%s\\n' \"subyard-profile-runtime-diagnostic-v1",
		"[ \"$yard\" = default ] && printf '%s\\n' \"subyard-profile-runtime-diagnostic-v1", 1)
	if patched == payload {
		t.Fatal("fixture handler was not patched")
	}
	testkit.WriteFile(t, handler, []byte(patched), 0o700)
	scenario.runtime.failing(t, "default", true)
	scenario.runtime.failing(t, "named", true)
	outcome := scenario.converge(t)
	if outcome.ReachedGoal || scenario.journal(t).Checkpoint == releasetransition.JournalComplete {
		t.Fatalf("blocking failure completed the journal: %#v", outcome)
	}
	if strings.Contains(outcome.Message, "release ready") || strings.Contains(outcome.Message, "init") || strings.Contains(outcome.Retry, "init") {
		t.Fatalf("mixed outcome: %#v", outcome)
	}
}

// materializedCurrentExecutor reports every yard's materialized assets as
// current, so only the runtime owner differs between yards.
type materializedCurrentExecutor struct {
	installedSettingsExecutor
	hashes map[string]string
}

func (fixture materializedCurrentExecutor) Exec(ctx context.Context, project, instance string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	if len(request.Command) == 3 && request.Command[0] == "sha256sum" {
		if hash, ok := fixture.hashes[request.Command[2]]; ok {
			return ports.InstanceExecResult{Stdout: []byte(hash + "  " + request.Command[2] + "\n")}, nil
		}
	}
	return fixture.installedSettingsExecutor.Exec(ctx, project, instance, request)
}

// With two yards pending, the init of one yard succeeds and leaves the other
// yard's pending item for its own init.
func TestInitOfOnePendingYardSucceedsWhileAnotherStaysPending(t *testing.T) {
	scenario := newYardFailureScenario(t, true, true, "default", "named")
	root := scenario.fixture.cli.options.RepositoryRoot
	before, after, marker := filepath.Join(root, "inspect-before.json"), filepath.Join(root, "inspect-after.json"), filepath.Join(root, "default-initialized")
	writePendingRepairEngine(t, &scenario.fixture, []byte(fmt.Sprintf(`#!/bin/sh
if [ "$1" = --version ]; then printf 'yard-engine 1.2.3\n'; exit; fi
[ "$1" = _release-transition ] || exit 64
cat >/dev/null
if [ -e '%s' ]; then cat '%s'; else cat '%s'; fi
`, marker, after, before)))
	request, _ := repairRequest(t, scenario.fixture)
	scenario.request.ArtifactDigest = request.ArtifactDigest
	scenario.runtime.failing(t, "default", true)
	scenario.runtime.failing(t, "named", true)
	requirePendingRuntimeOutcome(t, scenario.converge(t), "default", "named")
	snapshot := func(path string) {
		payload, err := json.Marshal(scenario.invoke(t, releasetransition.ProcessInspect, nil))
		if err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, payload, 0o600)
	}
	snapshot(before)
	// Readiness once default's init converged: only named stays pending.
	partial := mustReadConfigFixture(t, scenario.runtime.state("default"))
	testkit.WriteFile(t, scenario.runtime.state("default"), []byte(strings.Repeat("b", 64)), 0o600)
	snapshot(after)
	testkit.WriteFile(t, scenario.runtime.state("default"), partial, 0o600)

	base := scenario.fixture.cli.options.Executor.(installedSettingsExecutor)
	hashes := map[string]string{}
	for _, yard := range []string{"default", "named"} {
		loaded, err := scenario.fixture.cli.resolveReleaseTransitionContext(context.Background(), yard, scenario.request.ConfigHome)
		if err != nil {
			t.Fatal(err)
		}
		assets, err := effectiveConfigAssets(loaded)
		if err != nil {
			t.Fatal(err)
		}
		for _, asset := range assets {
			hash, err := config.HashRegularFile(asset.Source)
			if err != nil {
				t.Fatal(err)
			}
			hashes[asset.Destination] = hash
		}
	}
	scenario.fixture.cli.options.Executor = materializedCurrentExecutor{installedSettingsExecutor: base, hashes: hashes}
	integrations := &integrationRuntimeFixture{plan: reconcileruntime.IntegrationPlan{Fingerprint: strings.Repeat("c", 64), Scope: strings.Repeat("c", 64)}}
	scenario.fixture.cli.options.IntegrationRuntime = func(config.Loaded) IntegrationRuntime { return integrations }
	stale := ports.RuntimeObservation{State: ports.RuntimeStateStale, Actual: strings.Repeat("d", 64), Desired: strings.Repeat("b", 64)}
	platform := &sampleRepairPlatformFixture{initPlatformFixture: newInitPlatformFixture(), observation: &stale}
	for stage := range platform.converged {
		platform.converged[stage] = true
	}
	platform.converged[ports.ReconcileStageProfileRuntimes] = false
	platform.converged[ports.ReconcileStageFinalize] = true
	platform.readyOnProfileRuntime = marker
	scenario.fixture.cli.options.InitPlatform = platform
	writeCLIFile(t, filepath.Join(scenario.request.ConfigHome, "host-id"), "owner-a\n", 0o600)

	program, _, stdout, stderr := scenario.fixture.program(t, []string{"-Y", "default", "init", "--yes"}, nil, func(nestedCall, io.Writer) int { return 0 })
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("init of default: code=%d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if !slices.Contains(platform.applied, ports.ReconcileStageProfileRuntimes) {
		t.Fatalf("init did not apply the runtime: %v", platform.applied)
	}
	if !strings.Contains(stdout.String(), "Still pending: yard named: profile sample runtime failed") {
		t.Fatalf("init did not name the other pending yard: %s", stdout)
	}
	for yard, kept := range map[string]bool{"default": false, "named": true} {
		path, _ := activationPendingRecordPath(scenario.request.ConfigHome, yard, "sample-runtime")
		if _, err := os.Stat(path); (err == nil) != kept {
			t.Fatalf("yard %s record kept=%v: %v", yard, !kept, err)
		}
	}
}

// otherOwnerFixture is a second activation owner that drifts independently.
type otherOwnerFixture struct {
	current    *bool
	reconciled *int
}

func (otherOwnerFixture) ID() string { return "other-owner" }
func (owner otherOwnerFixture) Observe(context.Context, releasetransition.ReleasePair, releasetransition.ReleaseLinks) (releasetransition.V2ActivationObservation, error) {
	desired := releasetransition.Fingerprint(strings.Repeat("c", 64))
	if *owner.current {
		return releasetransition.V2ActivationObservation{Actual: desired, Desired: desired, Converged: true}, nil
	}
	return releasetransition.V2ActivationObservation{Actual: releasetransition.Fingerprint(strings.Repeat("e", 64)), Desired: desired}, nil
}
func (owner otherOwnerFixture) Reconcile(context.Context, releasetransition.ReleaseLinks) error {
	*owner.reconciled++
	*owner.current = true
	return nil
}

// A pending yard of one owner does not wedge another owner's drift: with that
// drift the release needs repair, so migrate plans and repairs it, and the
// pending item is reported again once only it remains.
func TestPendingYardDoesNotBlockAnotherOwnersRepair(t *testing.T) {
	current, reconciled := true, 0
	scenario := newYardFailureScenario(t, true, true, "default")
	scenario.owners = []releasetransition.V2ActivationReconciler{otherOwnerFixture{current: &current, reconciled: &reconciled}}
	scenario.runtime.failing(t, "default", true)
	requirePendingRuntimeOutcome(t, scenario.converge(t), "default")
	// The other owner drifts later, for example after an admitted start.
	current = false
	inspection := scenario.invoke(t, releasetransition.ProcessInspect, nil)
	if inspection.Inspection == nil || len(inspection.Inspection.Blockers) != 0 || !inspection.Inspection.Assessment.Changed {
		payload, _ := json.Marshal(inspection)
		t.Fatalf("migrate cannot plan the other owner's repair: %s", payload)
	}
	converged := scenario.invoke(t, releasetransition.ProcessConverge, &releasetransition.Execution{Plan: inspection.Inspection.Plan, Authorization: "fixture-confirmed-repair"})
	if reconciled != 2 || !current || converged.Outcome == nil {
		t.Fatalf("other owner was not repaired: %d %#v", reconciled, converged)
	}
	requirePendingRuntimeOutcome(t, *converged.Outcome, "default")
	if applies := scenario.runtime.applies(t); len(applies) != 1 {
		t.Fatalf("repair repeated the recorded failure: %v", applies)
	}
}

// Materialized configuration pending keeps the gate of this release's base:
// it does not open ordinary commands or teardown; a declared runtime's
// recorded failure does.
func TestOnlyDeclaredRuntimePendingOpensOrdinaryCommands(t *testing.T) {
	for _, test := range []struct {
		resource string
		gated    bool
	}{
		{resource: "activation.materialized-config.pending-configuration", gated: true},
		{resource: "activation.sample-runtime.pending-configuration", gated: false},
	} {
		t.Run(test.resource, func(t *testing.T) {
			fixture := pendingInitGateFixture(t, false, test.resource, "")
			declareYardScopedRuntimes(t, &fixture)
			for _, arguments := range [][]string{{"-Y", "default", "stop"}, {"-Y", "default", "teardown"}} {
				var stdout, stderr bytes.Buffer
				options := fixture.cli.options
				options.Arguments, options.Stdout, options.Stderr = arguments, &stdout, &stderr
				program, err := New(options)
				if err != nil {
					t.Fatal(err)
				}
				program.Run(context.Background())
				if refused := strings.Contains(stderr.String(), `"code":"dependency-unavailable"`); refused != test.gated {
					t.Fatalf("yard %s gate refusal=%t want %t: %s%s", strings.Join(arguments, " "), refused, test.gated, stdout.String(), stderr.String())
				}
			}
		})
	}
}

// While another yard is pending, configuration commands on a yard without a
// pending item reach their own guards; a materialized item keeps them gated.
func TestConfigurationCommandsOnUnpendingYardWhileAnotherIsPending(t *testing.T) {
	for _, test := range []struct {
		resource string
		gated    bool
	}{
		{resource: "activation.sample-runtime.pending-configuration"},
		{resource: "activation.materialized-config.pending-configuration", gated: true},
	} {
		t.Run(test.resource, func(t *testing.T) {
			fixture := pendingInitGateFixtureFor(t, true, "named", test.resource, "")
			registry := filepath.Join(fixture.cli.options.RepositoryRoot, "config", "commands.registry")
			extra := ""
			for _, line := range strings.Split(string(mustReadConfigFixture(t, filepath.Join(repositoryRoot(t), "config", "commands.registry"))), "\n") {
				if strings.HasPrefix(line, "config|") || strings.HasPrefix(line, "profile|") {
					extra += line + "\n"
				}
			}
			testkit.WriteFile(t, registry, append(mustReadConfigFixture(t, registry), extra...), 0o600)
			declareYardScopedRuntimes(t, &fixture)
			for _, arguments := range [][]string{
				{"-Y", "default", "init", "--yes"},
				{"-Y", "default", "config", "set", "LIMITS_MEMORY", "4GiB", "--scope", "yard", "--local", "--yes"},
				{"-Y", "default", "profile", "enable", "sample", "--yes"},
			} {
				var stdout, stderr bytes.Buffer
				options := fixture.cli.options
				options.Arguments, options.Stdout, options.Stderr = arguments, &stdout, &stderr
				program, err := New(options)
				if err != nil {
					t.Fatal(err)
				}
				code := program.Run(context.Background())
				t.Logf("yard %s => %d %s%s", strings.Join(arguments, " "), code, stdout.String(), stderr.String())
				if refused := strings.Contains(stderr.String(), `"code":"dependency-unavailable"`); refused != test.gated {
					t.Fatalf("yard %s gate refusal=%t want %t: %s%s", strings.Join(arguments, " "), refused, test.gated, stdout.String(), stderr.String())
				}
			}
		})
	}
}

// A record binds the whole profile package, not only the declared wrapper: a
// changed resource the handler executes makes the old record stale.
func TestPendingRecordBindsTheProfilePackage(t *testing.T) {
	scenario := newYardFailureScenario(t, true, true, "default")
	resource := filepath.Join(scenario.fixture.cli.options.RepositoryRoot, "config/profiles/sample/resources/handler.sh")
	if err := os.MkdirAll(filepath.Dir(resource), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, resource, []byte("#!/bin/sh\nexit 0\n"), 0o700)
	scenario.runtime.failing(t, "default", true)
	requirePendingRuntimeOutcome(t, scenario.converge(t), "default")
	testkit.WriteFile(t, resource, []byte("#!/bin/sh\nexit 1\n"), 0o700)
	readiness := scenario.invoke(t, releasetransition.ProcessInspect, nil)
	if readiness.Inspection == nil || len(readiness.Inspection.Blockers) != 0 || readiness.Inspection.Outcome.Status == releasetransition.StatusReady {
		payload, _ := json.Marshal(readiness)
		t.Fatalf("record survived a changed package resource: %s", payload)
	}
}

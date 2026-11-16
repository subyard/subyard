package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/testkit"
)

// observeFailing makes the sample handler's observe exit 2 in one yard, as on a
// running guest that a failed init left unprovisioned.
func observeFailing(t *testing.T, scenario yardFailureScenario, yard string) {
	t.Helper()
	handler := filepath.Join(scenario.fixture.cli.options.RepositoryRoot, "config/profiles/sample/runtime.sh")
	payload := string(mustReadConfigFixture(t, handler))
	marker := filepath.Join(scenario.runtime.root, "observe-fail-")
	payload = strings.Replace(payload, "yard=${SUBYARD_CONFIG_YARD_DIR##*/}\n",
		"yard=${SUBYARD_CONFIG_YARD_DIR##*/}\n[ ! -e '"+marker+"'\"$yard\" ] || exit 2\n", 1)
	testkit.WriteFile(t, handler, []byte(payload), 0o700)
	testkit.WriteFile(t, marker+yard, nil, 0o600)
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

// mutationGateRefusalMarker opens every release gate refusal on stderr.
const mutationGateRefusalMarker = mutationGateRefusalPrefix

// gateRefused runs one command and reports whether the release gate refused it.
func gateRefused(t *testing.T, scenario yardFailureScenario, arguments ...string) bool {
	t.Helper()
	integrations := &integrationRuntimeFixture{plan: reconcileruntime.IntegrationPlan{Fingerprint: strings.Repeat("c", 64), Scope: strings.Repeat("c", 64)}}
	scenario.fixture.cli.options.IntegrationRuntime = func(config.Loaded) IntegrationRuntime { return integrations }
	program, _, stdout, stderr := scenario.fixture.program(t, arguments, nil, func(nestedCall, io.Writer) int { return 0 })
	code := program.Run(context.Background())
	t.Logf("yard %s => %d\n%s%s", strings.Join(arguments, " "), code, stdout, stderr)
	refused := strings.Contains(stderr.String(), mutationGateRefusalMarker)
	// An admitted command reached its own handler: it planned, or its own
	// preparation answered.
	if output := stdout.String() + stderr.String(); !refused && !strings.Contains(output, "Operation plan:") && !strings.Contains(output, "yard: prepare ") && !strings.Contains(output, "config inspection:") {
		t.Fatalf("yard %s was admitted but never reached its handler: %s", strings.Join(arguments, " "), output)
	}
	return refused
}

// A running yard whose init never finished is left to that init: its failing
// observation neither blocks the release nor closes any yard's commands.
func TestUninitializedYardIsLeftToItsInit(t *testing.T) {
	scenario := newYardFailureScenario(t, true, true, "default", "named")
	inspect := scenario.replayReleaseEngine(t)
	if outcome := scenario.converge(t); outcome.Status != releasetransition.StatusReady {
		t.Fatalf("baseline: %#v", outcome)
	}
	markYard(t, scenario.fixture, "named", "false")
	observeFailing(t, scenario, "named")
	readiness := recordReadiness(t, scenario, inspect)
	outcome := readiness.Inspection.Outcome
	if outcome.Status != releasetransition.StatusReady || len(readiness.Inspection.Blockers) != 0 ||
		!strings.Contains(strings.Join(outcome.Warnings, "\n"), uninitializedYardWarning("named")) {
		payload, _ := json.Marshal(readiness)
		t.Fatalf("readiness with an uninitialized yard: %s", payload)
	}
	for _, arguments := range [][]string{
		{"-Y", "default", "stop"},
		{"-Y", "named", "stop"},
		{"-Y", "named", "init", "--yes"},
		{"-Y", "named", "teardown", "--yes"},
	} {
		if gateRefused(t, scenario, arguments...) {
			t.Fatalf("yard %s was refused by the release gate", strings.Join(arguments, " "))
		}
	}
}

// Release repair never runs a hook in a yard whose init did not finish, even
// when its runtime is stale and would fail, and the journal completes.
func TestReleaseRepairSkipsUninitializedYard(t *testing.T) {
	scenario := newYardFailureScenario(t, false, false, "default", "named")
	markYard(t, scenario.fixture, "named", "false")
	scenario.runtime.failing(t, "named", true)
	outcome := scenario.converge(t)
	if outcome.Status != releasetransition.StatusReady {
		t.Fatalf("repair beside an uninitialized yard: %#v", outcome)
	}
	if applies := scenario.runtime.applies(t); len(applies) != 1 || applies[0] != "default" {
		t.Fatalf("hook applies: %v", applies)
	}
	if journal := scenario.journal(t); journal.Checkpoint != releasetransition.JournalComplete {
		t.Fatalf("journal: %s", journal.Checkpoint)
	}
}

// gateCase is one command and whether the release gate must refuse it.
type gateCase struct {
	arguments []string
	refused   bool
}

func requireGate(t *testing.T, scenario yardFailureScenario, cases []gateCase) {
	t.Helper()
	for _, test := range cases {
		if refused := gateRefused(t, scenario, test.arguments...); refused != test.refused {
			t.Fatalf("yard %s gate refusal=%t, want %t", strings.Join(test.arguments, " "), refused, test.refused)
		}
	}
}

// One initialized yard's failed observation closes only that yard, which
// admits its repair commands; every other yard stays open.
func TestYardObservationFailureClosesOnlyThatYard(t *testing.T) {
	scenario := newYardFailureScenario(t, true, true, "default", "named")
	inspect := scenario.replayReleaseEngine(t)
	if outcome := scenario.converge(t); outcome.Status != releasetransition.StatusReady {
		t.Fatalf("baseline: %#v", outcome)
	}
	observeFailing(t, scenario, "named")
	readiness := recordReadiness(t, scenario, inspect)
	if readiness.Inspection.Outcome.Status != releasetransition.StatusOperatorActionRequired {
		t.Fatalf("readiness: %#v", readiness.Inspection.Outcome)
	}
	requireGate(t, scenario, []gateCase{
		{arguments: []string{"-Y", "default", "stop"}},
		{arguments: []string{"-Y", "default", "start"}},
		{arguments: []string{"-Y", "named", "stop"}},
		{arguments: []string{"-Y", "named", "init", "--yes"}},
		{arguments: []string{"-Y", "named", "init", "--reset", "--yes"}},
		{arguments: []string{"-Y", "named", "teardown", "--yes"}},
		{arguments: []string{"-Y", "named", "provision", "--yes"}, refused: true},
	})
}

// A yard started after an update with it stopped is drift of that yard only.
func TestStartedDeferredYardDriftClosesOnlyThatYard(t *testing.T) {
	for _, pendingDefault := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "default-pending"}[pendingDefault], func(t *testing.T) {
			scenario := newYardFailureScenario(t, true, true, "default", "named")
			inspect := scenario.replayReleaseEngine(t)
			scenario.fixture.setPower(t, "named", "STOPPED")
			if pendingDefault {
				scenario.runtime.failing(t, "default", true)
			}
			scenario.converge(t)
			scenario.fixture.setPower(t, "named", "RUNNING")
			readiness := recordReadiness(t, scenario, inspect)
			// The process wire presents a fresh repair plan of completed history as
			// recovering; the runtime restores migration-required for the gate.
			if readiness.Inspection.Outcome.Status == releasetransition.StatusReady {
				t.Fatalf("readiness: %#v", readiness.Inspection.Outcome)
			}
			requireGate(t, scenario, []gateCase{
				{arguments: []string{"-Y", "default", "stop"}},
				{arguments: []string{"-Y", "named", "stop"}},
				{arguments: []string{"-Y", "named", "init", "--yes"}},
				{arguments: []string{"-Y", "named", "provision", "--yes"}, refused: true},
			})
		})
	}
}

// An invalid setting of a stopped yard is that yard's finding, reported once.
func TestInvalidYardConfigurationClosesOnlyThatYard(t *testing.T) {
	scenario := newYardFailureScenario(t, true, true, "default", "named")
	inspect := scenario.replayReleaseEngine(t)
	if outcome := scenario.converge(t); outcome.Status != releasetransition.StatusReady {
		t.Fatalf("baseline: %#v", outcome)
	}
	scenario.fixture.setPower(t, "named", "STOPPED")
	path := filepath.Join(scenario.request.ConfigHome, "yards", "named", "config.env")
	testkit.WriteFile(t, path, append(mustReadConfigFixture(t, path), []byte("SSH_PORT=99999\n")...), 0o600)
	recordReadiness(t, scenario, inspect)
	requireGate(t, scenario, []gateCase{
		{arguments: []string{"-Y", "default", "stop"}},
		{arguments: []string{"-Y", "default", "init", "--yes"}},
		{arguments: []string{"-Y", "named", "config", "unset", "SSH_PORT", "--scope", "yard", "--local", "--yes"}},
		{arguments: []string{"-Y", "named", "provision", "--yes"}, refused: true},
	})
}

// The default yard's own invalid configuration underlies every yard: each
// admits only its repair commands, so the setting can still be corrected.
func TestInvalidDefaultConfigurationAdmitsRepairEverywhere(t *testing.T) {
	scenario := newYardFailureScenario(t, true, true, "default", "named")
	inspect := scenario.replayReleaseEngine(t)
	if outcome := scenario.converge(t); outcome.Status != releasetransition.StatusReady {
		t.Fatalf("baseline: %#v", outcome)
	}
	path := filepath.Join(scenario.request.ConfigHome, "yards", "default", "config.env")
	testkit.WriteFile(t, path, append(mustReadConfigFixture(t, path), []byte("SSH_PORT=99999\n")...), 0o600)
	recordReadiness(t, scenario, inspect)
	requireGate(t, scenario, []gateCase{
		{arguments: []string{"-Y", "default", "config", "unset", "SSH_PORT", "--scope", "yard", "--local", "--yes"}},
		{arguments: []string{"-Y", "named", "stop"}},
		{arguments: []string{"-Y", "default", "provision", "--yes"}, refused: true},
		{arguments: []string{"-Y", "named", "provision", "--yes"}, refused: true},
	})
}

// decodeGateRefusal reads the text refusal back into the outcome it renders.
func decodeGateRefusal(text []byte) (mutationGateOutcome, error) {
	var outcome mutationGateOutcome
	lines := strings.Split(strings.TrimSpace(string(text)), "\n")
	head, found := strings.CutPrefix(lines[0], mutationGateRefusalPrefix+" release ")
	if !found {
		return outcome, fmt.Errorf("not a gate refusal: %q", text)
	}
	head, _, _ = strings.Cut(head, "; this command is not run")
	state, identity, _ := strings.Cut(head, ": active ")
	status, code, _ := strings.Cut(state, " (")
	outcome.Status, outcome.Code = releasetransition.PublicStatus(status), releasetransition.OutcomeCode(strings.TrimSuffix(code, ")"))
	active, rest, _ := strings.Cut(identity, ", target ")
	target, transaction, hasTransaction := strings.Cut(rest, ", transaction ")
	outcome.Active, outcome.Target = releasetransition.ReleaseID(active), releasetransition.ReleaseID(target)
	if hasTransaction {
		id := releasetransition.TransactionID(transaction)
		outcome.Transaction = &id
	}
	for _, line := range lines[1:] {
		if cause, ok := strings.CutPrefix(line, "yard: cause: "); ok {
			outcome.Message = cause
		}
		if next, ok := strings.CutPrefix(line, "yard: next: "); ok {
			outcome.Action = next
		}
	}
	return outcome, nil
}

// lostAgentExecutor fails every command in one instance the way a guest agent
// that does not answer does: an exec transport error, not a guest exit status.
type lostAgentExecutor struct {
	ports.InstanceExecutor
	instance string
}

func (executor lostAgentExecutor) Exec(ctx context.Context, project, instance string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	if instance == executor.instance {
		return ports.InstanceExecResult{}, errors.New("websocket: guest agent is not responding")
	}
	return executor.InstanceExecutor.Exec(ctx, project, instance, request)
}

// The core probe tells a lost guest agent from a failed handler: the finding
// keeps the handler's diagnostic, and its next step retries migrate before a
// forced stop and start.
func TestLostGuestAgentIsThatYardsFinding(t *testing.T) {
	scenario := newYardFailureScenario(t, true, true, "default", "named")
	inspect := scenario.replayReleaseEngine(t)
	if outcome := scenario.converge(t); outcome.Status != releasetransition.StatusReady {
		t.Fatalf("baseline: %#v", outcome)
	}
	observeFailing(t, scenario, "named")
	scenario.fixture.cli.options.Executor = lostAgentExecutor{scenario.fixture.cli.options.Executor, scenario.fixture.instanceName(t, "named")}
	readiness := recordReadiness(t, scenario, inspect)
	if len(readiness.Inspection.Blockers) != 1 || !strings.Contains(readiness.Inspection.Blockers[0].Message, "guest agent does not answer") ||
		!strings.Contains(readiness.Inspection.Blockers[0].Message, "stop --force") {
		payload, _ := json.Marshal(readiness.Inspection.Blockers)
		t.Fatalf("lost agent finding: %s", payload)
	}
	program, _, _, _ := scenario.fixture.program(t, nil, nil, func(nestedCall, io.Writer) int { return 0 })
	outcome, inspection, err := program.inspectMutationGateInspection(context.Background(), "named")
	if err != nil {
		t.Fatal(err)
	}
	gate, ok := program.yardScopedGate(outcome, inspection)
	if !ok || !gate.state("named").unreachable || gate.state("default").repairOnly() ||
		!strings.HasPrefix(yardNextStep("named", gate.state("named")), "yard migrate;") {
		t.Fatalf("lost agent gate: ok=%t %#v", ok, gate)
	}
}

// Status states the release once and each finding under its own yard.
func TestStatusReportsYardFindingsUnderTheirYard(t *testing.T) {
	scenario := newYardFailureScenario(t, true, true, "default", "named")
	inspect := scenario.replayReleaseEngine(t)
	scenario.converge(t)
	observeFailing(t, scenario, "named")
	recordReadiness(t, scenario, inspect)
	program, _, stdout, _ := scenario.fixture.program(t, []string{"status"}, nil, func(nestedCall, io.Writer) int { return 0 })
	program.Run(context.Background())
	output := stdout.String()
	if strings.Count(output, "release  active and complete; yard named needs attention") != 1 ||
		strings.Count(output, "attention yard named: profile sample-runtime observation failed") != 1 || strings.Contains(output, "NOT OK") {
		t.Fatalf("status: %s", output)
	}
}

// While an activation of the active release is unfinished, stopping one local
// yard is the only new exit; teardown and other lifecycle commands stay gated.
// (Init keeps its own embedded resume of the unfinished transition.)
func TestUnfinishedActivationAdmitsOnlyYardStop(t *testing.T) {
	scenario := newYardFailureScenario(t, false, false, "default", "named")
	inspect := scenario.replayReleaseEngine(t)
	scenario.runtime.failing(t, "named", true)
	if outcome := scenario.converge(t); outcome.ReachedGoal {
		t.Fatalf("fixture activation did not stop: %#v", outcome)
	}
	if journal := scenario.journal(t); journal.Checkpoint != releasetransition.JournalReconciling {
		t.Fatalf("journal: %s", journal.Checkpoint)
	}
	recordReadiness(t, scenario, inspect)
	requireGate(t, scenario, []gateCase{
		{arguments: []string{"-Y", "named", "stop"}},
		{arguments: []string{"-Y", "named", "stop", "--force"}},
		{arguments: []string{"-Y", "named", "teardown", "--yes"}, refused: true},
		{arguments: []string{"-Y", "default", "start"}, refused: true},
	})
}

// One yard's failed observation never hides another yard's observed drift:
// both yards are limited to their repair commands.
func TestFailedYardDoesNotHideAnotherYardsDrift(t *testing.T) {
	scenario := newYardFailureScenario(t, true, true, "default", "named")
	inspect := scenario.replayReleaseEngine(t)
	if outcome := scenario.converge(t); outcome.Status != releasetransition.StatusReady {
		t.Fatalf("baseline: %#v", outcome)
	}
	testkit.WriteFile(t, scenario.runtime.state("default"), []byte(strings.Repeat("d", 64)), 0o600)
	observeFailing(t, scenario, "named")
	recordReadiness(t, scenario, inspect)
	program, _, _, _ := scenario.fixture.program(t, nil, nil, func(nestedCall, io.Writer) int { return 0 })
	outcome, inspection, err := program.inspectMutationGateInspection(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	gate, ok := program.yardScopedGate(outcome, inspection)
	if !ok || !gate.state("named").failed || !gate.state("default").drift {
		t.Fatalf("gate: ok=%t %#v", ok, gate.yards)
	}
	requireGate(t, scenario, []gateCase{
		{arguments: []string{"-Y", "default", "provision", "--yes"}, refused: true},
		{arguments: []string{"-Y", "named", "provision", "--yes"}, refused: true},
	})
}

// confirmingPrompt runs a change while the operator confirms, as another
// process would between the plan and its consent.
type confirmingPrompt struct{ during func() }

func (prompt confirmingPrompt) Confirm(context.Context, domain.ConfirmationRequest) (bool, error) {
	prompt.during()
	return true, nil
}

// A repair admitted behind the gate rechecks the release after consent and
// before any effect: a transition that started meanwhile refuses it.
func TestAdmittedRepairRechecksReleaseAfterConsent(t *testing.T) {
	scenario := newYardFailureScenario(t, true, true, "default", "named")
	inspect := scenario.replayReleaseEngine(t)
	scenario.converge(t)
	observeFailing(t, scenario, "named")
	readiness := recordReadiness(t, scenario, inspect)
	changed := readiness
	inspection := *changed.Inspection
	outcome := *inspection.Outcome
	outcome.Status, outcome.Code = releasetransition.StatusRecovering, releasetransition.CodeRecoveryPending
	inspection.Outcome, inspection.Blockers = &outcome, nil
	changed.Inspection = &inspection
	payload, err := json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	prompt := confirmingPrompt{during: func() { testkit.WriteFile(t, inspect, payload, 0o600) }}
	integrations := &integrationRuntimeFixture{plan: reconcileruntime.IntegrationPlan{Fingerprint: strings.Repeat("c", 64), Scope: strings.Repeat("c", 64)}}
	scenario.fixture.cli.options.IntegrationRuntime = func(config.Loaded) IntegrationRuntime { return integrations }
	program, _, stdout, stderr := scenario.fixture.program(t, []string{"-Y", "named", "stop"}, prompt, func(nestedCall, io.Writer) int { return 0 })
	code := program.Run(context.Background())
	incus := scenario.fixture.cli.options.Incus.(*testkit.Incus)
	if code == 0 || !strings.Contains(stderr.String(), "the release changed after confirmation") || len(incus.PowerUpdates) != 0 {
		t.Fatalf("stop ran over a changed release: code=%d updates=%v\n%s%s", code, incus.PowerUpdates, stdout, stderr)
	}
}

// A declared shutdown verb is a repair command of a yard limited to repair,
// for the direct CLI and RPC alike; other verbs are not.
func TestShutdownVerbIsARepairCommandForCLIAndRPC(t *testing.T) {
	scenario := newYardFailureScenario(t, true, true, "default", "named")
	program, _, _, _ := scenario.fixture.program(t, nil, nil, func(nestedCall, io.Writer) int { return 0 })
	gate := yardGate{yards: map[string]*yardGateState{"named": {failed: true}}, order: []string{"named"}}
	definition := resource.Definition{Command: "relay", Shutdown: "down"}
	if !program.yardGateAdmits(gate, "named", "", []string{"down"}, definition, true) ||
		program.yardGateAdmits(gate, "named", "", []string{"up"}, definition, true) {
		t.Fatal("shutdown verb classification")
	}
	request := program.rpcGateRequest("named", command.Definition{Name: "stop", Handler: "@lifecycle"}, "stop", nil)
	if !request.stop || request.profileResource {
		t.Fatalf("RPC stop request: %#v", request)
	}
}

// While an activation is unfinished and a guest agent is lost, its yard can
// still be stopped, and the activation then completes without it.
func TestLostAgentDuringUnfinishedActivationAdmitsStopAndResumes(t *testing.T) {
	scenario := newYardFailureScenario(t, false, false, "default", "named")
	inspect := scenario.replayReleaseEngine(t)
	scenario.runtime.failing(t, "named", true)
	if outcome := scenario.converge(t); outcome.ReachedGoal {
		t.Fatalf("fixture activation did not stop: %#v", outcome)
	}
	observeFailing(t, scenario, "named")
	base := scenario.fixture.cli.options.Executor
	scenario.fixture.cli.options.Executor = lostAgentExecutor{base, scenario.fixture.instanceName(t, "named")}
	readiness := recordReadiness(t, scenario, inspect)
	if readiness.Inspection.Outcome.Status != releasetransition.StatusOperatorActionRequired || readiness.Inspection.Resume == nil {
		t.Fatalf("lost agent over an unfinished activation: %#v", readiness.Inspection.Outcome)
	}
	requireGate(t, scenario, []gateCase{
		{arguments: []string{"-Y", "named", "stop", "--force"}},
		{arguments: []string{"-Y", "named", "teardown", "--yes"}, refused: true},
	})
	// The admitted stop leaves the yard stopped; the activation resumes to ready.
	scenario.fixture.setPower(t, "named", "STOPPED")
	scenario.fixture.cli.options.Executor = base
	resume := scenario.invoke(t, releasetransition.ProcessInspect, nil)
	if resume.Inspection == nil || resume.Inspection.Resume == nil || len(resume.Inspection.Blockers) != 0 {
		payload, _ := json.Marshal(resume)
		t.Fatalf("resume inspection: %s", payload)
	}
	converged := scenario.invoke(t, releasetransition.ProcessConverge, &releasetransition.Execution{Plan: resume.Inspection.Plan})
	if converged.Outcome == nil || converged.Outcome.Status != releasetransition.StatusReady {
		t.Fatalf("resume after the stop: %#v", converged.Outcome)
	}
}

// An aggregate apply skips and reports yards whose own contract refuses it.
func TestAggregateApplySkipsPendingAndInvalidYards(t *testing.T) {
	scenario := newYardFailureScenario(t, true, true, "default", "named")
	program, _, _, stderr := scenario.fixture.program(t, nil, nil, func(nestedCall, io.Writer) int { return 0 })
	gate := yardGate{yards: map[string]*yardGateState{"named": {pending: true}}, order: []string{"named"}}
	program.gateAdmitted = &gateAdmission{gate: &gate}
	targets := []configTarget{{Name: "default"}, {Name: "named"}}
	if kept := program.skipGatedYards(targets, "default", true); len(kept) != 1 || kept[0].Name != "default" ||
		!strings.Contains(stderr.String(), "skipped yard named") {
		t.Fatalf("pending yard kept: %v %s", kept, stderr)
	}
	path := filepath.Join(scenario.request.ConfigHome, "yards", "named", "config.env")
	testkit.WriteFile(t, path, append(mustReadConfigFixture(t, path), []byte("SSH_PORT=99999\n")...), 0o600)
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	targets, loadErr := program.localConfigTargets(loaded, true)
	kept, err := program.skipInvalidYards(targets, loadErr, loaded, true)
	if loadErr == nil || err != nil || len(kept) == 0 || !strings.Contains(stderr.String(), "skipped yard named: its configuration is invalid") {
		t.Fatalf("invalid yard aborted the aggregate: %v %v %s", loadErr, err, stderr)
	}
}

// A host-scope setting from an open yard reaches a yard whose pending contract
// refuses configuration, so it waits for that yard's init.
func TestHostSettingWaitsForAnotherYardsPendingInit(t *testing.T) {
	fixture := pendingInitGateFixtureFor(t, true, "named", "activation.sample-runtime.pending-configuration", "")
	registry := filepath.Join(fixture.cli.options.RepositoryRoot, "config", "commands.registry")
	extra := ""
	for _, line := range strings.Split(string(mustReadConfigFixture(t, filepath.Join(repositoryRoot(t), "config", "commands.registry"))), "\n") {
		if strings.HasPrefix(line, "config|") {
			extra += line + "\n"
		}
	}
	testkit.WriteFile(t, registry, append(mustReadConfigFixture(t, registry), extra...), 0o600)
	declareYardScopedRuntimes(t, &fixture)
	var stdout, stderr bytes.Buffer
	options := fixture.cli.options
	options.Arguments, options.Stdout, options.Stderr = []string{"-Y", "default", "config", "set", "LIMITS_MEMORY", "4GiB", "--scope", "host", "--local", "--yes"}, &stdout, &stderr
	program, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code == 0 || !strings.Contains(stderr.String(), "a host or shared setting reaches yard named") ||
		!strings.Contains(stderr.String(), "next: yard -Y named init") && !strings.Contains(stderr.String(), "next: 'yard' '-Y' 'named' 'init'") {
		t.Fatalf("host setting beside a pending yard: code=%d %s%s", code, stdout.String(), stderr.String())
	}
}

// Gate admission and its effect boundary never take the release update lock,
// which Converge takes inside the owner barrier: an admitted repair finishes
// while a converge holds it, in either order, so neither can deadlock.
func TestAdmittedRepairNeverWaitsForTheReleaseUpdateLock(t *testing.T) {
	for _, order := range []string{"converge-first", "repair-first"} {
		t.Run(order, func(t *testing.T) {
			scenario := newYardFailureScenario(t, true, true, "default", "named")
			inspect := scenario.replayReleaseEngine(t)
			scenario.converge(t)
			observeFailing(t, scenario, "named")
			recordReadiness(t, scenario, inspect)
			store, err := releasetransition.NewPOSIXV2Store(scenario.request.ConfigHome)
			if err != nil {
				t.Fatal(err)
			}
			var release func()
			if order == "converge-first" {
				if release, err = store.Lock(); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan bool, 1)
			go func() { done <- gateRefused(t, scenario, "-Y", "named", "stop") }()
			select {
			case refused := <-done:
				if refused {
					t.Fatal("stop of the failed yard was refused")
				}
			case <-time.After(30 * time.Second):
				t.Fatal("admitted repair waited for the release update lock")
			}
			if release != nil {
				release()
			} else if unlock, err := store.Lock(); err != nil {
				t.Fatal(err)
			} else {
				unlock()
			}
		})
	}
}

// The effect boundary compares the release history the admission saw: a
// migrate that rewrote the journal while consent was pending refuses the
// command before any effect, on a ready release too.
func TestEffectBoundaryRefusesChangedHistory(t *testing.T) {
	if !historyCheckOnReadyRelease {
		t.Skip("history check limited to degraded admissions")
	}
	scenario := newYardFailureScenario(t, true, true, "default", "named")
	inspect := scenario.replayReleaseEngine(t)
	scenario.converge(t)
	recordReadiness(t, scenario, inspect)
	journal, err := releasetransition.ParseJournal(mustReadConfigFixture(t, scenario.fixture.journalPath))
	if err != nil {
		t.Fatal(err)
	}
	journal.Transaction = "tx-ffffffffffffffffffffffffffffffff"
	rewritten, err := releasetransition.MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	prompt := confirmingPrompt{during: func() { testkit.WriteFile(t, scenario.fixture.journalPath, rewritten, 0o600) }}
	program, _, stdout, stderr := scenario.fixture.program(t, []string{"-Y", "default", "stop"}, prompt, func(nestedCall, io.Writer) int { return 0 })
	code := program.Run(context.Background())
	incus := scenario.fixture.cli.options.Incus.(*testkit.Incus)
	if code == 0 || !strings.Contains(stderr.String(), "the release changed after confirmation") || len(incus.PowerUpdates) != 0 {
		t.Fatalf("stop ran over a changed history: code=%d updates=%v\n%s%s", code, incus.PowerUpdates, stdout, stderr)
	}
}

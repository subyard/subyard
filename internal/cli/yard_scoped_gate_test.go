package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
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
	return strings.Contains(stderr.String(), mutationGateRefusalMarker)
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

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

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/testkit"
)

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
	observeFailing(t, scenario, "named", true)
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
	observeFailing(t, scenario, "named", true)
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
		yardNextStep("named", gate.state("named")).command != "yard migrate" {
		t.Fatalf("lost agent gate: ok=%t %#v", ok, gate)
	}
}

// Status states the release once and each finding under its own yard.
func TestStatusReportsYardFindingsUnderTheirYard(t *testing.T) {
	scenario := newYardFailureScenario(t, true, true, "default", "named")
	inspect := scenario.replayReleaseEngine(t)
	scenario.converge(t)
	observeFailing(t, scenario, "named", true)
	recordReadiness(t, scenario, inspect)
	program, _, stdout, _ := scenario.fixture.program(t, []string{"status"}, nil, func(nestedCall, io.Writer) int { return 0 })
	program.Run(context.Background())
	output := stdout.String()
	if strings.Count(output, "release  active and complete; yard named needs attention") != 1 ||
		strings.Count(output, "attention yard named: profile sample-runtime observation failed") != 1 || strings.Contains(output, "NOT OK") {
		t.Fatalf("status: %s", output)
	}
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
	observeFailing(t, scenario, "named", true)
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
	// The fixture CLI is no published runtime; bind a stand-in installed source.
	program.sourceBinding = func(context.Context, releasetransition.JournalRecord) (string, string, error) {
		return "source", "tooling", nil
	}
	code := program.Run(context.Background())
	incus := scenario.fixture.cli.options.Incus.(*testkit.Incus)
	if code == 0 || !strings.Contains(stderr.String(), "the release changed after confirmation") || len(incus.PowerUpdates) != 0 {
		t.Fatalf("stop ran over a changed release: code=%d updates=%v\n%s%s", code, incus.PowerUpdates, stdout, stderr)
	}
}

// A declared shutdown verb is, before prepare, a candidate repair of a yard
// limited to repair, for the direct CLI and RPC alike; other verbs are not.
// Its prepared plan then decides whether it is that repair.
func TestShutdownVerbIsARepairCommandForCLIAndRPC(t *testing.T) {
	scenario := newYardFailureScenario(t, true, true, "default", "named")
	program, _, _, _ := scenario.fixture.program(t, nil, nil, func(nestedCall, io.Writer) int { return 0 })
	gate := yardGate{yards: map[string]*yardGateState{"named": {failed: true}}, order: []string{"named"}}
	definition := resource.Definition{Command: "relay", Shutdown: "down"}
	admits := func(verb string) bool {
		verdict := gate.verdict("named", program.provisionalSubject(gateRequest{yard: "named", arguments: []string{verb}, definition: definition, profileResource: true}))
		return verdict.err() == nil
	}
	if !admits("down") || admits("up") {
		t.Fatal("the declared shutdown is not the candidate repair, or another verb is")
	}
	request := program.rpcGateRequest("named", command.Definition{Name: "stop", Handler: "@lifecycle"}, nil)
	if !program.boundedStopRequest(request) || request.profileResource {
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
	observeFailing(t, scenario, "named", true)
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
	prepared := &preparedCommand{CLI: program, gateRecord: &gateAdmission{kind: admissionOpen, request: gateRequest{yard: "default"}, gate: &gate}}
	prepared.setGateSubject(gateSubject{declared: true, member: memberConfig, acts: []yardAction{{yard: "default", optional: true}, {yard: "named", optional: true}}})
	targets := []configTarget{{Name: "default"}, {Name: "named"}}
	if kept := prepared.gateSkipped(targets, true); len(kept) != 1 || kept[0].Name != "default" ||
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
	if code := program.Run(context.Background()); code == 0 || !strings.Contains(stderr.String(), "writes the desired configuration of yard named, whose pending configuration refuses it") ||
		!strings.Contains(stderr.String(), "next: yard -Y named init") && !strings.Contains(stderr.String(), "next: 'yard' '-Y' 'named' 'init'") {
		t.Fatalf("host setting beside a pending yard: code=%d %s%s", code, stdout.String(), stderr.String())
	}
}

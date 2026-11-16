package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/shellquote"
	"github.com/Subyard/Subyard/internal/testkit"
)

// convergedYardExecutor answers both integration views of the listed instances
// as current, accepts their settings capture, and answers the materialized
// configuration of stale instances as changed; every other read is the
// pending-retirement fixture's. It records each guest command by instance.
type convergedYardExecutor struct {
	*pendingRetirementExecutor
	converged, stale map[string]bool
	calls            map[string][]string
}

func (fixture convergedYardExecutor) Exec(ctx context.Context, project, instance string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	command := request.Command
	fixture.calls[instance] = append(fixture.calls[instance], strings.Join(command, " "))
	if fixture.converged[instance] && len(command) == 5 && command[0] == "python3" && strings.Contains(command[3], "Version 1 records observed ownership only") {
		switch command[4] {
		case "observe-applied", "observe":
			payload, _ := json.Marshal(map[string]any{"fingerprint": strings.Repeat("a", 64), "changed": false})
			return ports.InstanceExecResult{Stdout: payload}, nil
		case "capture-settings":
			return ports.InstanceExecResult{Stdout: []byte("{}")}, nil
		}
	}
	if fixture.stale[instance] && len(command) == 3 && command[0] == "sha256sum" && command[2] == fixture.destination {
		return ports.InstanceExecResult{Stdout: []byte(strings.Repeat("0", 64) + "  " + fixture.destination + "\n")}, nil
	}
	return fixture.pendingRetirementExecutor.Exec(ctx, project, instance, request)
}

// pendingDriftFixture completes the release for the default and named yards. Its
// installed engine answers every inspection with the actual inspection of the
// real materialized-config owner and records each converge request. Each pending
// yard's desired view deselects codex, so its pending init releases owned
// configuration in place; each converged yard is current. Every other yard's
// installed integration view has drift.
type pendingDriftFixture struct {
	pendingInitFixture
	inspection releasetransition.Inspection
	converges  string
	response   string
	request    releasetransition.ProcessRequest
	executor   convergedYardExecutor
}

func newPendingDriftFixture(t *testing.T, pending, converged []string) pendingDriftFixture {
	t.Helper()
	yards := map[string]string{}
	for _, yard := range pending {
		yards[yard] = "CODING_TOOL_INTEGRATIONS=\n"
	}
	fixture := newPendingInitFixture(t, true, yards)
	root := fixture.cli.options.RepositoryRoot
	response := filepath.Join(root, "pending-drift-inspection.json")
	converges := filepath.Join(root, "pending-drift-converge")
	target := "1.2.3-aaaaaaaaaaaa"
	writePendingRepairEngine(t, &fixture, []byte(fmt.Sprintf(`#!/bin/sh
case "${1:-}" in
--version) printf 'yard-engine 1.2.3\n' ;;
_release-transition)
  request="$(cat)"
  case "$request" in
    *'"mode":"converge"'*)
      printf converge >> %q
      printf '%%s\n' '{"schemaVersion":1,"activationReconciliationOwned":true,"outcome":{"status":"ready","reachedGoal":true,"active":"%s","previous":"release-a","target":"%s","code":"ready","message":"verified","transaction":"tx-0123456789abcdef"}}' ;;
    *) cat %q ;;
  esac ;;
*) exit 64 ;;
esac
`, converges, target, target, response)))
	installed := fixture.cli.options.Executor.(installedSettingsExecutor)
	retirement := &pendingRetirementExecutor{installedSettingsExecutor: installed, desiredByInstance: map[string]string{}}
	executor := convergedYardExecutor{pendingRetirementExecutor: retirement, converged: map[string]bool{}, stale: map[string]bool{}, calls: map[string][]string{}}
	for _, yard := range pending {
		retirement.desiredByInstance[fixture.instanceName(t, yard)] = "release"
	}
	for _, yard := range converged {
		executor.converged[fixture.instanceName(t, yard)] = true
	}
	fixture.cli.options.Executor = executor
	fixture.cli.options.InitPlatform = nil
	request, _ := repairRequest(t, fixture)
	request.SchemaVersion, request.Mode, request.Yard = releasetransition.ProcessProtocolSchemaV1, releasetransition.ProcessInspect, "default"
	request.InheritedSettingIDs = fixture.cli.releaseTransitionInheritedSettingIDs()
	owner := fixture.cli.activationReconcilers(request)[len(fixture.cli.profileRuntimeIDs())].(*materializedConfigActivationReconciler)
	if !owner.cli.activation.repair {
		t.Fatal("fixture does not inspect completed history in the installed view")
	}
	// The installed view still selects codex; its materialized rules are current.
	loaded, err := owner.cli.resolveReleaseTransitionContext(context.Background(), "default", request.ConfigHome)
	if err != nil {
		t.Fatal(err)
	}
	assets, err := effectiveConfigAssets(loaded)
	if err != nil || len(assets) != 1 {
		t.Fatalf("assets=%v err=%v", assets, err)
	}
	retirement.destination = assets[0].Destination
	if retirement.hash, err = config.HashRegularFile(assets[0].Source); err != nil {
		t.Fatal(err)
	}
	fake := fixture.cli.options.Incus.(*testkit.Incus)
	fake.Reconcile = ports.ReconcileState{InstanceFound: true, Instance: fake.Instances[loaded.Context.IncusProject+"/"+loaded.Context.YardInstanceName]}
	drift := pendingDriftFixture{pendingInitFixture: fixture, converges: converges, response: response, request: request, executor: executor}
	drift.refresh(t, owner)
	return drift
}

// refresh has the installed engine answer with a fresh actual inspection of the
// owner, or of a fresh materialized-config owner when owner is nil.
func (fixture *pendingDriftFixture) refresh(t *testing.T, owner releasetransition.V2ActivationReconciler) {
	t.Helper()
	if owner == nil {
		owner = fixture.owner()
	}
	inspected := fixture.execute(t, nil, owner)
	if inspected.Inspection == nil || inspected.Inspection.Outcome == nil {
		t.Fatalf("real owner inspection=%#v", inspected)
	}
	payload, err := json.Marshal(inspected)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, fixture.response, append(payload, '\n'), 0o600)
	fixture.inspection = *inspected.Inspection
}

// execute runs the actual transition engine in process with owner, or with the
// complete activation owner set of a fresh engine process when owner is nil: an
// inspection without execution, otherwise a converge of the authorized plan.
func (fixture pendingDriftFixture) execute(t *testing.T, execution *releasetransition.Execution, owner releasetransition.V2ActivationReconciler) releasetransition.ProcessResponse {
	t.Helper()
	request := fixture.request
	if request.Execution = execution; execution != nil {
		request.Mode = releasetransition.ProcessConverge
	}
	owners := fixture.cli.activationReconcilers(request)
	if owner != nil {
		owners = []releasetransition.V2ActivationReconciler{owner}
	}
	response, err := executeReleaseTransitionRequest(context.Background(), fixture.cli.options.RepositoryRoot, request,
		func(_ releasetransition.PlanToken, grant releasetransition.Authorization) bool { return grant != "" },
		owners, releaseTransitionOwnerFixture{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

// owner returns a fresh materialized-config owner, as a new engine process has.
func (fixture pendingDriftFixture) owner() *materializedConfigActivationReconciler {
	return fixture.cli.activationReconcilers(fixture.request)[len(fixture.cli.profileRuntimeIDs())].(*materializedConfigActivationReconciler)
}

// The next step of drift beside a pending yard is release repair. After it, the
// pending yard's own init is admitted, and once that yard's configuration is
// applied, the release is ready.
func TestPendingYardBesideDriftReachesReadiness(t *testing.T) {
	fixture := newPendingDriftFixture(t, []string{"default"}, nil)
	pending, repaired := fixture.instanceName(t, "default"), fixture.instanceName(t, "named")
	if outcome, err := fixture.pendingInitFixture.cli.inspectMutationGate(context.Background(), "default"); err != nil || outcome == nil || publicMutationGateOutcome(*outcome).Action != "run yard migrate" {
		t.Fatalf("next step beside drift: %#v %v", outcome, err)
	}
	if code, stderr := fixture.run(t, "migrate", "--yes"); !bytes.Contains(mustReadConfigFixture(t, fixture.converges), []byte("converge")) {
		t.Fatalf("yard migrate did not repair: code=%d stderr=%s", code, stderr)
	}
	// The repair converged the other yard; the pending yard waits for its init.
	fixture.executor.converged[repaired] = true
	fixture.refresh(t, nil)
	if refused, err := fixture.rpcGateRefusal(t, "default", `{"command":"start","arguments":[]}`); refused {
		t.Fatalf("ordinary command refused after repair: %v", err)
	}
	if code, stderr := fixture.run(t, "-Y", "default", "init", "--yes"); code != 0 && gateRefusal(stderr) {
		t.Fatalf("pending yard init refused after repair: %s", stderr)
	}
	// That init applied the yard's desired configuration.
	fixture.executor.desiredByInstance[pending] = ""
	fixture.executor.converged[pending] = true
	fixture.refresh(t, nil)
	if outcome, err := fixture.pendingInitFixture.cli.inspectMutationGate(context.Background(), "default"); err != nil || outcome != nil {
		t.Fatalf("release is not ready after the pending yard's init: %#v %v", outcome, err)
	}
}

// A prospective source publication does not defer a pending yard: its
// observation keeps the typed refusal, so the engine reports the blocker and its
// next step before it prepares any source activation.
func TestSourceActivationKeepsPendingYardRefusal(t *testing.T) {
	fixture := newPendingDriftFixture(t, []string{"default"}, []string{"named"})
	owner := fixture.owner()
	releases := releasetransition.ReleasePair{From: fixture.request.Target, Target: fixture.request.Target}
	_, err := owner.ObserveProspective(context.Background(), releases, releasetransition.ReleaseLinks{}, preparedActivationSnapshotFixture{home: fixture.request.ConfigHome})
	var release pendingInitOwnershipRelease
	if !errors.As(err, &release) || release.yard != "default" || owner.pendingDeferral().deferred("default") {
		t.Fatalf("prospective observation deferred the pending yard: %v", err)
	}
	if _, err := owner.Observe(context.Background(), releases, releasetransition.ReleaseLinks{}); err != nil || !owner.pendingDeferral().deferred("default") {
		t.Fatalf("release repair did not defer the pending yard: %v", err)
	}
}

// gateRefusal reports whether payload is the public mutation-gate outcome a
// refused command writes, rather than the command's own result or failure.
func gateRefusal(payload []byte) bool {
	if bytes.Contains(payload, []byte(mutationGateRefusalPrefix)) {
		return true // the CLI's text refusal
	}
	var refused mutationGateOutcome
	return json.Unmarshal(bytes.TrimSpace(payload), &refused) == nil && refused.Status != ""
}

func (fixture pendingDriftFixture) run(t *testing.T, arguments ...string) (int, []byte) {
	t.Helper()
	program, _, _, stderr := fixture.program(t, arguments, nil, func(nestedCall, io.Writer) int { return 0 })
	return program.Run(context.Background()), stderr.Bytes()
}

// rpcGateRefusal plans one command through the session RPC of yard and reports
// whether the mutation gate refused it.
func (fixture pendingDriftFixture) rpcGateRefusal(t *testing.T, yard, params string) (bool, error) {
	t.Helper()
	program, _, _, _ := fixture.program(t, nil, nil, func(nestedCall, io.Writer) int { return 0 })
	loaded, err := program.loadContext(yard)
	if err != nil {
		t.Fatal(err)
	}
	handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]*preparedCommand)}
	defer handler.closePlans()
	_, err = handler.Handle(context.Background(), rpc.Call{Method: "operation.plan", OperationID: "pending-drift",
		Params: json.RawMessage(params)}, nil)
	var fault *rpc.Error
	return errors.As(err, &fault) && gateRefusal([]byte(fault.Message)), err
}

// A command is admitted the same way by the direct CLI and by RPC, by the
// selected yard alone: another yard's unrepaired drift never closes it, and a
// drifted yard still starts, as start is one of its repair commands.
func TestOrdinaryCommandAdmissionDuringPendingConfiguration(t *testing.T) {
	for _, scenario := range []struct {
		name               string
		pending, converged []string
		admitted           bool
	}{
		{"pending-only", []string{"default"}, []string{"named"}, true},
		{"pending-beside-drift", []string{"default"}, nil, true},
		{"drift-only", nil, []string{"default"}, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newPendingDriftFixture(t, scenario.pending, scenario.converged)
			t.Run("RPC", func(t *testing.T) {
				refused, err := fixture.rpcGateRefusal(t, "default", `{"command":"start","arguments":[]}`)
				if refused == scenario.admitted {
					t.Fatalf("RPC start admitted=%t, want %t: %v; inspection=%+v", !refused, scenario.admitted, err, fixture.inspection.Blockers)
				}
			})
			t.Run("CLI", func(t *testing.T) {
				code, stderr := fixture.run(t, "start")
				if refused := code != 0 && gateRefusal(stderr); refused == scenario.admitted {
					t.Fatalf("CLI start admitted=%t, want %t: code=%d stderr=%s", !refused, scenario.admitted, code, stderr)
				}
			})
		})
	}
}

// Pending configuration of one yard never leaves the release without a next
// step: release repair skips that yard and repairs the others, and each pending
// yard's own init, direct or by RPC, stays admitted while other yards only wait
// for their inits.
func TestPendingConfigurationLeavesNoReleaseRepairDeadEnd(t *testing.T) {
	t.Run("pending-beside-drift", func(t *testing.T) {
		fixture := newPendingDriftFixture(t, []string{"default"}, nil)
		// The pending yard's init does not wait for the repair of another yard.
		if code, stderr := fixture.run(t, "-Y", "default", "init", "--yes"); code != 0 && gateRefusal(stderr) {
			t.Fatalf("init refused beside another yard's drift: code=%d stderr=%s", code, stderr)
		}
		if refused, err := fixture.rpcGateRefusal(t, "default", `{"command":"init","arguments":[]}`); refused {
			t.Fatalf("RPC init refused beside another yard's drift: %v", err)
		}
		code, stderr := fixture.run(t, "migrate", "--yes")
		if _, err := os.Stat(fixture.converges); err != nil {
			t.Fatalf("yard migrate did not repair the drifted named yard: code=%d stderr=%s", code, stderr)
		}
	})
	for _, scenario := range []struct {
		name               string
		pending, converged []string
	}{
		{"pending-only", []string{"default"}, []string{"named"}},
		{"both-pending", []string{"default", "named"}, nil},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newPendingDriftFixture(t, scenario.pending, scenario.converged)
			for _, yard := range scenario.pending {
				code, stderr := fixture.run(t, "-Y", yard, "init", "--yes")
				if code != 0 && gateRefusal(stderr) {
					t.Errorf("pending yard %s init refused by the release gate: %s", yard, stderr)
				}
				if refused, err := fixture.rpcGateRefusal(t, yard, `{"command":"init","arguments":[]}`); refused {
					t.Errorf("pending yard %s init refused by the RPC release gate: %v", yard, err)
				}
			}
		})
	}
}

// Release repair defers the pending yard as a whole for every activation owner,
// and keeps it in Desired like a stopped yard. The actual engine and the complete
// owner set refresh the other yard's stale materialized configuration; the
// fixture interrupts that refresh, and a fresh engine resumes it. The pending
// finding stays, is resolved, or first appears before the resume: the resume
// never sees a stale observation scope, and no owner runs a guest command in a
// deferred yard beyond the reads of an inspection.
func TestReleaseRepairNeverEntersTheDeferredYardIncludingResume(t *testing.T) {
	for _, scenario := range []string{"stays-pending", "resolved-before-resume", "pending-before-resume"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newPendingDriftFixture(t, []string{"default"}, []string{"named"})
			pending, repaired := fixture.instanceName(t, "default"), fixture.instanceName(t, "named")
			fixture.executor.stale[pending], fixture.executor.stale[repaired] = true, true
			// A profile runtime owner, observed and reconciled before the materialized
			// configuration, is stale in every yard until its handler applies it there.
			applied := filepath.Join(fixture.cli.options.RepositoryRoot, "sample-applied")
			stale, current := `{"state":"stale","actual":"`+strings.Repeat("a", 64)+`","desired":"`+strings.Repeat("b", 64)+`"}`, `{"state":"current","actual":"`+strings.Repeat("b", 64)+`","desired":"`+strings.Repeat("b", 64)+`"}`
			writeCLIFile(t, filepath.Join(fixture.cli.options.RepositoryRoot, "config/profiles/sample/runtime.sh"), fmt.Sprintf(`#!/bin/sh
if [ "$1" = apply ]; then printf '%%s\n' "$SUBYARD_YARD" >> %[1]q; fi
if grep -qx "$SUBYARD_YARD" %[1]q 2>/dev/null; then printf '%%s\n' '%[3]s'; else printf '%%s\n' '%[2]s'; fi
`, applied, stale, current), 0o700)
			// Commands an inspection runs only read guest state.
			reads := map[string]bool{}
			for _, deferring := range []bool{true, false} {
				clear(fixture.executor.calls)
				fixture.executor.desiredByInstance[pending] = map[bool]string{true: "release", false: ""}[deferring]
				fixture.execute(t, nil, nil)
				for _, call := range fixture.executor.calls[pending] {
					reads[call] = true
				}
			}
			fixture.executor.desiredByInstance[pending] = "release"
			if scenario == "pending-before-resume" {
				fixture.executor.desiredByInstance[pending] = ""
			}
			clear(fixture.executor.calls)
			inspection := fixture.execute(t, nil, nil).Inspection
			if inspection == nil || inspection.Outcome.Status == releasetransition.StatusOperatorActionRequired || len(inspection.Blockers) != 0 {
				t.Fatalf("repair inspection=%+v", inspection)
			}
			// The fixture refuses the first guest write it is not answering: the
			// refresh preparation of the repaired yard, or an undeferred yard's
			// integration apply.
			refreshed := "yard named: cannot apply materialized config"
			if scenario == "pending-before-resume" {
				refreshed = "yard default: cannot reconcile integrations"
			}
			first := fixture.execute(t, &releasetransition.Execution{Plan: inspection.Plan, Authorization: "fixture-grant"}, nil)
			if first.Outcome == nil || first.Outcome.Status != releasetransition.StatusRecovering || !strings.HasPrefix(first.Outcome.Message, refreshed) {
				t.Fatalf("repair outcome=%+v", first.Outcome)
			}
			switch scenario {
			case "resolved-before-resume":
				fixture.executor.desiredByInstance[pending] = ""
			case "pending-before-resume":
				fixture.executor.desiredByInstance[pending] = "release"
				clear(fixture.executor.calls)
				// The pending yard's runtime is stale again when the finding appears.
				if err := os.Remove(applied); err != nil {
					t.Fatal(err)
				}
			}
			resume := fixture.execute(t, nil, nil)
			if resume.Inspection == nil || resume.Inspection.Resume == nil || resume.Inspection.Outcome.Code == releasetransition.CodePlanStale {
				t.Fatalf("interrupted repair is not resumable: %+v", resume.Inspection)
			}
			calls := len(fixture.executor.calls[repaired])
			resumed := fixture.execute(t, &releasetransition.Execution{Plan: resume.Inspection.Plan, Authorization: "fixture-grant"}, nil)
			if resumed.Outcome == nil || resumed.Outcome.Code == releasetransition.CodePlanStale || len(fixture.executor.calls[repaired]) <= calls {
				t.Fatalf("resume did not repair again: %+v", resumed.Outcome)
			}
			if scenario == "resolved-before-resume" {
				return
			}
			for _, call := range fixture.executor.calls[pending] {
				if !reads[call] {
					t.Fatalf("release repair wrote into the deferred yard: %.300q", call)
				}
			}
			if yards, err := os.ReadFile(applied); err != nil || string(yards) != "named\n" {
				t.Fatalf("profile runtime applied to %q, want only the repaired yard: %v", yards, err)
			}
		})
	}
}

// Host key tools are one installation that every yard only addresses: release
// repair repairs them although every yard is deferred, so the pending yards'
// inits find them current. Yard-scoped owners leave the deferred yards.
func TestReleaseRepairKeepsHostKeyToolsBesideDeferredYards(t *testing.T) {
	fixture := newConfigApplyRepairFixture(t, true)
	platform := &installedActivationFixture{initPlatformFixture: newInitPlatformFixture(), state: ports.RuntimeObservation{State: ports.RuntimeStateStale, Actual: strings.Repeat("a", 64), Desired: strings.Repeat("b", 64)}}
	fixture.cli.options.InitPlatform = platform
	journal, err := releasetransition.ParseJournal(mustReadConfigFixture(t, fixture.journalPath))
	if err != nil {
		t.Fatal(err)
	}
	request := releasetransition.ProcessRequest{Mode: releasetransition.ProcessInspect, RuntimeRoot: fixture.cli.env["YARD_RUNTIME_ROOT"], ConfigHome: fixture.cli.env["SUBYARD_CONFIG_HOME"],
		Target: journal.Goal.Target, Direction: journal.Goal.Direction, ArtifactDigest: journal.ArtifactDigest, RegistryDigest: journal.RegistryDigest}
	operation := fixture.cli.releaseActivationOperation(request)
	if !operation.activation.repair {
		t.Fatal("fixture does not repair the active release")
	}
	operation.activation.pending.record(map[string]bool{"default": true, "named": true})
	for _, id := range []string{"key-tools-runtime", "core-guest-runtime"} {
		owner := operation.profileActivationReconciler(request, id).(*profileRuntimeActivationReconciler)
		_, targets, err := owner.targets(context.Background())
		if err != nil || len(targets) != map[string]int{"key-tools-runtime": 2, "core-guest-runtime": 0}[id] {
			t.Fatalf("%s targets=%d err=%v", id, len(targets), err)
		}
	}
	owner := operation.profileActivationReconciler(request, "key-tools-runtime").(*profileRuntimeActivationReconciler)
	before, err := owner.Observe(context.Background(), journal.Releases, releasetransition.ReleaseLinks{})
	if err != nil || before.Converged {
		t.Fatalf("stale host key tools hidden by deferred yards: %#v %v", before, err)
	}
	if err := owner.Reconcile(context.Background(), releasetransition.ReleaseLinks{}); err != nil || platform.applied != 1 {
		t.Fatalf("release repair skipped host key tools: %v applied=%d", err, platform.applied)
	}
	if after, err := owner.Observe(context.Background(), journal.Releases, releasetransition.ReleaseLinks{}); err != nil || !after.Converged {
		t.Fatalf("host key tools did not converge: %#v %v", after, err)
	}
}

// One gate inspection observes the materialized configuration once, although
// yard-scoped owners observed before it need its deferral decision.
func TestReleaseInspectionObservesMaterializedConfigurationOnce(t *testing.T) {
	for _, pending := range [][]string{nil, {"default"}} {
		t.Run(fmt.Sprint(len(pending)), func(t *testing.T) {
			fixture := newPendingDriftFixture(t, pending, []string{"named"})
			reads := func(owner releasetransition.V2ActivationReconciler) int {
				clear(fixture.executor.calls)
				fixture.execute(t, nil, owner)
				count := 0
				for _, calls := range fixture.executor.calls {
					for _, call := range calls {
						if strings.HasPrefix(call, "sha256sum ") {
							count++
						}
					}
				}
				return count
			}
			if alone, all := reads(fixture.owner()), reads(nil); alone == 0 || all != alone {
				t.Fatalf("materialized reads with all owners=%d, alone=%d", all, alone)
			}
		})
	}
}

// A config apply repair of a stale yard beside a pending yard restores the
// release readiness it was admitted for. The pending yard's item is returned
// for the boundary to report, as an init of another yard returns it.
func TestConfigApplyRepairLeavesOtherYardsPendingConfiguration(t *testing.T) {
	fixture := newPendingDriftFixture(t, []string{"default"}, []string{"named"})
	repaired := fixture.instanceName(t, "named")
	fixture.executor.stale[repaired] = true
	fixture.refresh(t, nil)
	outcome, err := fixture.cli.inspectMutationGate(context.Background(), "named")
	if err != nil || outcome == nil {
		t.Fatalf("gate beside drift: %#v %v", outcome, err)
	}
	permit, err := fixture.cli.prepareConfigApplyRepair(context.Background(), "named", false, *outcome)
	if err != nil || permit == nil {
		t.Fatalf("config apply repair of the drifted yard was not admitted: %v", err)
	}
	// The apply refreshed the stale yard; the pending yard waits for its init.
	fixture.executor.stale[repaired] = false
	fixture.refresh(t, nil)
	pending, err := fixture.cli.finishConfigApplyRepair(context.Background(), permit)
	if err != nil || len(pending) != 1 || pending[0].Retry != shellquote.Command([]string{"yard", "-Y", "default", "init"}) {
		t.Fatalf("config apply beside a pending yard: pending=%#v err=%v", pending, err)
	}
	var output bytes.Buffer
	reportStillPending(&output, pending)
	if !strings.HasPrefix(output.String(), "Still pending: yard default: ") || !strings.HasSuffix(output.String(), "; next: 'yard' '-Y' 'default' 'init'\n") {
		t.Fatalf("report=%q", output.String())
	}
}

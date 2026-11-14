package releasetransition

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// pendingFindingActivation models a yard-scoped owner with durable records. A
// typed failure is recorded and Reconcile succeeds; while the recorded facts
// still hold, the owner observes itself converged with pending findings. A
// drift of those facts makes it unconverged.
type pendingFindingActivation struct {
	id         string
	findings   []V2PendingConfiguration
	untyped    bool
	recorded   bool
	resolved   bool
	reconciled *[]string
	// driftAfter makes the observation drift at this observation after the failure.
	driftAfter, observedAfter int
}

func (owner *pendingFindingActivation) ID() string { return owner.id }
func (owner *pendingFindingActivation) Observe(context.Context, ReleasePair, ReleaseLinks) (V2ActivationObservation, error) {
	if owner.resolved || owner.findings == nil && owner.recorded {
		return V2ActivationObservation{Actual: digestC, Desired: digestC, Converged: true}, nil
	}
	if !owner.recorded {
		return V2ActivationObservation{Actual: digestB, Desired: digestC}, nil
	}
	owner.observedAfter++
	if owner.driftAfter != 0 && owner.observedAfter >= owner.driftAfter {
		return V2ActivationObservation{Actual: digestA, Desired: digestC}, nil
	}
	return V2ActivationObservation{Actual: digestC, Desired: digestC, Converged: true, Pending: owner.findings}, nil
}
func (owner *pendingFindingActivation) Reconcile(context.Context, ReleaseLinks) error {
	*owner.reconciled = append(*owner.reconciled, owner.id)
	if owner.untyped {
		return errors.New("hook failed")
	}
	owner.recorded = true
	return nil
}

func yardFinding(yard string) V2PendingConfiguration {
	return V2PendingConfiguration{Message: "yard " + yard + ": profile runtime failed; profile not applied", Retry: "yard -Y " + yard + " init"}
}

func readDeferredJournal(t *testing.T, transition *V2Transition) JournalRecord {
	t.Helper()
	snapshot, err := transition.store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	journal, err := ParseJournal(snapshot.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return journal
}

func convergeFresh(t *testing.T, transition *V2Transition) Outcome {
	t.Helper()
	ctx := context.Background()
	inspection, err := transition.Inspect(ctx, Goal{Target: "release-a", Direction: DirectionActivateTarget})
	if err != nil || len(inspection.Blockers) != 0 {
		t.Fatal(inspection, err)
	}
	outcome, err := transition.Converge(ctx, Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
	if err != nil {
		t.Fatal(err)
	}
	return outcome
}

func TestPendingFindingsCompleteRepairAndBlockOnlyCompletedReadiness(t *testing.T) {
	for _, scenario := range []string{"pending", "untyped"} {
		t.Run(scenario, func(t *testing.T) {
			transition, _, _ := v2TransitionFixture(t, nil)
			reconciled := []string{}
			owner := &pendingFindingActivation{id: "hook-runtime", findings: []V2PendingConfiguration{yardFinding("named")}, untyped: scenario == "untyped", reconciled: &reconciled}
			transition.options.Reconcilers = []V2ActivationReconciler{owner, &pendingFindingActivation{id: "later-owner", reconciled: &reconciled}}
			outcome := convergeFresh(t, transition)
			journal := readDeferredJournal(t, transition)
			if scenario == "untyped" {
				if outcome.Status == StatusReady || journal.Checkpoint == JournalComplete || !reflect.DeepEqual(reconciled, []string{"hook-runtime"}) {
					t.Fatalf("untyped failure did not keep recovery: %#v %s %v", outcome, journal.Checkpoint, reconciled)
				}
				return
			}
			if journal.Checkpoint != JournalComplete || !reflect.DeepEqual(reconciled, []string{"hook-runtime", "later-owner"}) {
				t.Fatalf("repair did not complete: %s %v", journal.Checkpoint, reconciled)
			}
			if outcome.Status != StatusOperatorActionRequired || outcome.Code != CodeDependencyUnavailable ||
				outcome.Message != yardFinding("named").Message+"; the release is ready" || outcome.Retry != "yard -Y named init" {
				t.Fatalf("pending configuration outcome: %#v", outcome)
			}
			goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
			readiness, err := transition.Inspect(context.Background(), goal)
			if err != nil || readiness.Resume != nil || len(readiness.Blockers) != 1 ||
				readiness.Blockers[0].Resource != "activation.hook-runtime.pending-configuration" || !reflect.DeepEqual(*readiness.Outcome, outcome) {
				t.Fatalf("completed readiness: %#v %v", readiness, err)
			}
			if err := ValidateProcessInspection(goal, readiness); err != nil {
				t.Fatal(err)
			}
			// The ready proof refuses pending findings; once resolved the release is ready.
			owner.resolved = true
			ready, err := transition.Inspect(context.Background(), goal)
			if err != nil || ready.Outcome.Status != StatusReady || len(ready.Blockers) != 0 {
				t.Fatalf("resolved owner kept the release pending: %#v %v", ready, err)
			}
		})
	}
}

// An interrupted repair is unfinished history: its pending findings are
// warnings, so the resume has no blockers and completes.
func TestResumedRepairWithPendingFindingsCompletes(t *testing.T) {
	interrupt := true
	transition, _, _ := v2TransitionFixture(t, func(point string) error {
		if point == "before-journal-complete" && interrupt {
			interrupt = false
			return errors.New("fixture interruption")
		}
		return nil
	})
	reconciled := []string{}
	transition.options.Reconcilers = []V2ActivationReconciler{&pendingFindingActivation{id: "hook-runtime", findings: []V2PendingConfiguration{yardFinding("named")}, reconciled: &reconciled}}
	if outcome := convergeFresh(t, transition); outcome.ReachedGoal || interrupt {
		t.Fatalf("fixture interruption: %#v", outcome)
	}
	if journal := readDeferredJournal(t, transition); journal.Checkpoint != JournalReconciling {
		t.Fatalf("interrupted journal: %s", journal.Checkpoint)
	}
	ctx := context.Background()
	resume, err := transition.Inspect(ctx, Goal{Target: "release-a", Direction: DirectionActivateTarget})
	if err != nil || resume.Resume == nil || len(resume.Blockers) != 0 || !strings.Contains(strings.Join(resume.Outcome.Warnings, "\n"), "waits for its yard init") {
		t.Fatalf("resume: %#v %v", resume, err)
	}
	outcome, err := transition.Converge(ctx, Execution{Plan: resume.Plan})
	if err != nil || outcome.Status != StatusOperatorActionRequired || outcome.Retry != "yard -Y named init" {
		t.Fatalf("resume: %#v %v", outcome, err)
	}
	if journal := readDeferredJournal(t, transition); journal.Checkpoint != JournalComplete {
		t.Fatalf("resumed journal: %s", journal.Checkpoint)
	}
}

// A recorded failure whose facts drift before the first observation after it,
// or before the aggregate verification, is ordinary unconverged state.
func TestDriftOfRecordedFactsKeepsRecovery(t *testing.T) {
	for _, driftAfter := range []int{1, 2} {
		t.Run(fmt.Sprintf("drift-after=%d", driftAfter), func(t *testing.T) {
			transition, _, _ := v2TransitionFixture(t, nil)
			reconciled := []string{}
			transition.options.Reconcilers = []V2ActivationReconciler{&pendingFindingActivation{id: "hook-runtime",
				findings: []V2PendingConfiguration{yardFinding("named")}, reconciled: &reconciled, driftAfter: driftAfter}}
			outcome := convergeFresh(t, transition)
			if outcome.Status != StatusRecovering || readDeferredJournal(t, transition).Checkpoint == JournalComplete {
				t.Fatalf("drifted facts completed the journal: %#v", outcome)
			}
		})
	}
}

// Another owner's drift after completion is release work: the pending finding
// is a warning, so the repair plans, runs and reports the pending item again.
func TestPendingFindingDoesNotBlockAnotherOwnersRepair(t *testing.T) {
	transition, _, _ := v2TransitionFixture(t, nil)
	reconciled := []string{}
	other := &pendingFindingActivation{id: "other-owner", reconciled: &reconciled}
	transition.options.Reconcilers = []V2ActivationReconciler{
		&pendingFindingActivation{id: "hook-runtime", findings: []V2PendingConfiguration{yardFinding("named")}, reconciled: &reconciled}, other,
	}
	convergeFresh(t, transition)
	other.recorded = false
	ctx := context.Background()
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(ctx, goal)
	if err != nil || len(inspection.Blockers) != 0 || !inspection.Assessment.Changed ||
		!strings.Contains(strings.Join(inspection.Outcome.Warnings, "\n"), "yard named: profile runtime failed") {
		t.Fatalf("other owner's repair cannot be planned: %#v %v", inspection, err)
	}
	outcome, err := transition.Converge(ctx, Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
	if err != nil || outcome.Retry != "yard -Y named init" || !other.recorded {
		t.Fatalf("repair: %#v %v", outcome, err)
	}
}

// Many yards with long names: every yard keeps its own blocker and init step,
// and the converge result stays within the public bound while naming the rest.
func TestManyPendingYardsKeepTheirOwnInitSteps(t *testing.T) {
	transition, _, _ := v2TransitionFixture(t, nil)
	var findings []V2PendingConfiguration
	for index := range 30 {
		findings = append(findings, yardFinding(fmt.Sprintf("a-rather-long-descriptive-yard-name-%02d", index)))
	}
	reconciled := []string{}
	transition.options.Reconcilers = []V2ActivationReconciler{&pendingFindingActivation{id: "hook-runtime", findings: findings, reconciled: &reconciled}}
	outcome := convergeFresh(t, transition)
	if outcome.Status != StatusOperatorActionRequired || outcome.Retry != "yard -Y a-rather-long-descriptive-yard-name-00 init" ||
		len(outcome.Message) > maxDiagnosticText || !strings.Contains(outcome.Message, "more pending; full details: yard migrate --check") {
		t.Fatalf("converge: %#v", outcome)
	}
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	readiness, err := transition.Inspect(context.Background(), goal)
	if err != nil || len(readiness.Blockers) != len(findings) {
		t.Fatalf("readiness: %#v %v", readiness, err)
	}
	for index, blocker := range readiness.Blockers {
		if blocker.Resource != "activation.hook-runtime.pending-configuration" || blocker.Retry != findings[index].Retry {
			t.Fatalf("blocker %d: %#v", index, blocker)
		}
	}
	if err := ValidateProcessInspection(goal, readiness); err != nil {
		t.Fatal(err)
	}
}

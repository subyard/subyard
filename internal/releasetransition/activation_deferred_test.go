package releasetransition

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
)

// deferringActivation models a yard-scoped owner with a durable pending record.
// Its typed failure is reported by Reconcile; while the journal is unfinished
// it observes its ordinary state, and completed readiness reports the pending
// configuration. A broken owner cannot observe itself after the failure.
type deferringActivation struct {
	id         string
	store      *POSIXV2Store
	failure    error
	broken     bool
	recorded   bool
	converged  bool
	reconciled *[]string
}

func (owner *deferringActivation) ID() string { return owner.id }
func (owner *deferringActivation) Observe(context.Context, ReleasePair, ReleaseLinks) (V2ActivationObservation, error) {
	if owner.converged {
		return V2ActivationObservation{Actual: digestC, Desired: digestC, Converged: true}, nil
	}
	if owner.recorded && owner.broken {
		return V2ActivationObservation{}, errors.New("owner state cannot be observed")
	}
	if owner.recorded {
		snapshot, err := owner.store.ReadCurrentJournal()
		if err != nil {
			return V2ActivationObservation{}, err
		}
		journal, err := ParseJournal(snapshot.Payload)
		if err != nil {
			return V2ActivationObservation{}, err
		}
		if journal.Checkpoint == JournalComplete {
			return V2ActivationObservation{}, owner.failure
		}
	}
	return V2ActivationObservation{Actual: digestB, Desired: digestC}, nil
}
func (owner *deferringActivation) Reconcile(context.Context, ReleaseLinks) error {
	*owner.reconciled = append(*owner.reconciled, owner.id)
	if owner.failure == nil {
		owner.converged = true
		return nil
	}
	owner.recorded = true
	return owner.failure
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

func TestPendingConfigurationReconcileFailureCompletesRepair(t *testing.T) {
	for _, scenario := range []string{"deferred", "unobservable", "untyped"} {
		t.Run(scenario, func(t *testing.T) {
			transition, _, _ := v2TransitionFixture(t, nil)
			reconciled := []string{}
			failure := error(pendingConfigurationFinding("yard named: hook failed"))
			if scenario == "untyped" {
				failure = errors.New("hook failed")
			}
			deferring := &deferringActivation{id: "hook-runtime", store: transition.store, failure: failure, broken: scenario == "unobservable", reconciled: &reconciled}
			transition.options.Reconcilers = []V2ActivationReconciler{
				deferring,
				&deferringActivation{id: "later-owner", store: transition.store, reconciled: &reconciled},
			}
			ctx := context.Background()
			goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
			inspection, err := transition.Inspect(ctx, goal)
			if err != nil || len(inspection.Blockers) != 0 {
				t.Fatal(inspection, err)
			}
			outcome, err := transition.Converge(ctx, Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
			if err != nil {
				t.Fatal(err)
			}
			journal := readDeferredJournal(t, transition)
			if scenario != "deferred" {
				if outcome.Status == StatusReady || journal.Checkpoint == JournalComplete || !reflect.DeepEqual(reconciled, []string{"hook-runtime"}) {
					t.Fatalf("untyped or unobservable failure did not keep recovery: %#v %s %v", outcome, journal.Checkpoint, reconciled)
				}
				return
			}
			// Every other owner converges and the journal completes; the outcome
			// is the completed readiness of the deferred owner.
			if journal.Checkpoint != JournalComplete || !reflect.DeepEqual(reconciled, []string{"hook-runtime", "later-owner"}) {
				t.Fatalf("repair did not complete: %s %v", journal.Checkpoint, reconciled)
			}
			if outcome.Status != StatusOperatorActionRequired || outcome.Code != CodeDependencyUnavailable ||
				outcome.Message != "yard named: hook failed" || outcome.Retry != "inspect yard named: hook failed" {
				t.Fatalf("pending configuration outcome: %#v", outcome)
			}
			readiness, err := transition.Inspect(ctx, goal)
			if err != nil || readiness.Resume != nil || len(readiness.Blockers) != 1 ||
				readiness.Blockers[0].Resource != "activation.hook-runtime.pending-configuration" || !reflect.DeepEqual(*readiness.Outcome, outcome) {
				t.Fatalf("completed readiness: %#v %v", readiness, err)
			}
			// Once the owner converges, for example after yard init, the release is ready.
			deferring.converged = true
			ready, err := transition.Inspect(ctx, goal)
			if err != nil || ready.Outcome.Status != StatusReady || len(ready.Blockers) != 0 {
				t.Fatalf("converged owner kept the release pending: %#v %v", ready, err)
			}
		})
	}
}

// An interrupted repair resumes with the recorded owner still observable as
// itself, so the authorized scope is unchanged; the journal then completes.
func TestResumedRepairDefersRecordedPendingConfiguration(t *testing.T) {
	interrupt := true
	transition, _, _ := v2TransitionFixture(t, func(point string) error {
		if point == "before-journal-complete" && interrupt {
			interrupt = false
			return errors.New("fixture interruption")
		}
		return nil
	})
	reconciled := []string{}
	deferring := &deferringActivation{id: "hook-runtime", store: transition.store, failure: pendingConfigurationFinding("yard named: hook failed"), reconciled: &reconciled}
	transition.options.Reconcilers = []V2ActivationReconciler{deferring, &deferringActivation{id: "later-owner", store: transition.store, reconciled: &reconciled}}
	ctx := context.Background()
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(ctx, goal)
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := transition.Converge(ctx, Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)}); err != nil || outcome.ReachedGoal || interrupt {
		t.Fatalf("fixture interruption: %#v %v", outcome, err)
	}
	if journal := readDeferredJournal(t, transition); journal.Checkpoint != JournalReconciling {
		t.Fatalf("interrupted journal: %s", journal.Checkpoint)
	}
	resume, err := transition.Inspect(ctx, goal)
	if err != nil || resume.Resume == nil || len(resume.Blockers) != 0 {
		t.Fatalf("no resumable repair: %#v %v", resume, err)
	}
	outcome, err := transition.Converge(ctx, Execution{Plan: resume.Plan})
	if err != nil || outcome.Status != StatusOperatorActionRequired || outcome.Code != CodeDependencyUnavailable {
		t.Fatalf("resume: %#v %v", outcome, err)
	}
	if journal := readDeferredJournal(t, transition); journal.Checkpoint != JournalComplete {
		t.Fatalf("resumed journal: %s", journal.Checkpoint)
	}
}

type yardPendingFinding string

func (yard yardPendingFinding) Error() string                   { return "yard " + string(yard) + " pending" }
func (yardPendingFinding) ActivationPendingConfiguration() bool { return true }
func (yard yardPendingFinding) ActivationDiagnostic() (string, string) {
	return "yard " + string(yard) + ": profile runtime failed; release ready, profile not applied", "yard -Y " + string(yard) + " init"
}

// Many yards with long names: every yard keeps its own blocker and init step,
// and the outcome stays within the public bound while naming the rest.
func TestManyPendingYardsKeepTheirOwnInitSteps(t *testing.T) {
	transition, _, _ := v2TransitionFixture(t, nil)
	var findings []error
	for index := range 30 {
		findings = append(findings, yardPendingFinding(fmt.Sprintf("a-rather-long-descriptive-yard-name-%02d", index)))
	}
	reconciled := []string{}
	transition.options.Reconcilers = []V2ActivationReconciler{
		&deferringActivation{id: "hook-runtime", store: transition.store, failure: domain.CollectRefusals(findings...), reconciled: &reconciled},
	}
	ctx := context.Background()
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(ctx, goal)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := transition.Converge(ctx, Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
	if err != nil || outcome.Status != StatusOperatorActionRequired || outcome.Retry != "yard -Y a-rather-long-descriptive-yard-name-00 init" ||
		len(outcome.Message) > maxDiagnosticText || !strings.Contains(outcome.Message, "more pending; full details: yard migrate --check") {
		t.Fatalf("converge: %#v %v", outcome, err)
	}
	readiness, err := transition.Inspect(ctx, goal)
	if err != nil || len(readiness.Blockers) != len(findings) {
		t.Fatalf("readiness: %#v %v", readiness, err)
	}
	for index, blocker := range readiness.Blockers {
		yard := fmt.Sprintf("a-rather-long-descriptive-yard-name-%02d", index)
		if blocker.Resource != "activation.hook-runtime.pending-configuration" || blocker.Retry != "yard -Y "+yard+" init" {
			t.Fatalf("blocker %d: %#v", index, blocker)
		}
	}
	if readiness.Outcome.Retry != readiness.Blockers[0].Retry || readiness.Outcome.Message != readiness.Blockers[0].Message {
		t.Fatalf("readiness outcome: %#v", readiness.Outcome)
	}
	if err := ValidateProcessInspection(goal, readiness); err != nil {
		t.Fatal(err)
	}
}

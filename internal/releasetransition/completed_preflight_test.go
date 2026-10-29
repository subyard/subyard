package releasetransition

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
)

func TestCompletedV2ConvergePreservesLockedBlockerWarnings(t *testing.T) {
	for _, caller := range []string{"fresh", "cached", "resume"} {
		t.Run(caller, func(t *testing.T) {
			transition, store := completedSourceMetadataFixture(t)
			ctx := context.Background()
			inspection, err := transition.Inspect(ctx, Goal{Target: "release-a", Direction: DirectionActivateTarget})
			if err != nil {
				t.Fatal(err)
			}
			plan := inspection.Plan
			if caller == "resume" {
				snapshot, err := store.ReadCurrentJournal()
				if err != nil {
					t.Fatal(err)
				}
				journal, err := ParseJournal(snapshot.Payload)
				if err != nil {
					t.Fatal(err)
				}
				plan = journal.ResumePlan
			}
			transition.options.Reconcilers = []V2ActivationReconciler{
				v2ObservationErrorReconciler{id: "test-runtime", err: errors.New("private native detail")},
				&v2TestReconciler{id: "warnings", converged: true, warnings: []string{"z warning", "a warning", "z warning"}},
			}
			transition.options.VerifyAuthorization = func(PlanToken, Authorization) bool {
				t.Fatal("completed observation blocker reached authorization")
				return false
			}
			if caller == "fresh" {
				transition, err = NewV2Transition(transition.options)
				if err != nil {
					t.Fatal(err)
				}
			}
			before := snapshotStoreV2Tree(t, store.configHome)
			outcome, err := transition.Converge(ctx, Execution{Plan: plan})
			if err != nil || outcome.Code != CodeDependencyUnavailable || outcome.Status != StatusOperatorActionRequired ||
				!slices.Equal(outcome.Warnings, []string{"a warning", "z warning"}) {
				t.Fatalf("completed blocker: %+v, err=%v", outcome, err)
			}
			if !bytes.Equal(before, snapshotStoreV2Tree(t, store.configHome)) {
				t.Fatal("completed blocker changed protected state")
			}
		})
	}
}

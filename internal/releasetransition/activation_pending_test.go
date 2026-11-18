package releasetransition

import (
	"context"
	"slices"
	"testing"
)

func TestActivationReconcilesOwnersOnceInOrder(t *testing.T) {
	var calls []string
	transition, _, _ := v2TransitionFixture(t, func(checkpoint string) error {
		if slices.Contains([]string{"before-reconciler-0", "before-reconciler-1", "after-reconciler-0", "after-reconciler-1"}, checkpoint) {
			calls = append(calls, checkpoint)
		}
		return nil
	})
	transition.options.Reconcilers = []V2ActivationReconciler{
		&v2TestReconciler{id: "profile-runtime"},
		&v2TestReconciler{id: "materialized-config"},
	}
	ctx := context.Background()
	inspection, err := transition.Inspect(ctx, Goal{Target: "release-a", Direction: DirectionActivateTarget})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := transition.Converge(ctx, Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
	if err != nil || outcome.Status != StatusReady || !slices.Equal(calls, []string{
		"before-reconciler-0", "after-reconciler-0", "before-reconciler-1", "after-reconciler-1",
	}) {
		t.Fatalf("activation order = %v, outcome = %#v, err = %v", calls, outcome, err)
	}
}

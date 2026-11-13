package releasetransition

import (
	"context"
	"errors"
	"slices"
	"testing"
)

type plannedActivationFixture struct {
	v2TestReconciler
	pending bool
}

func (owner *plannedActivationFixture) Observe(ctx context.Context, releases ReleasePair, links ReleaseLinks) (V2ActivationObservation, error) {
	observed, err := owner.v2TestReconciler.Observe(ctx, releases, links)
	if owner.pending {
		observed.Planned = []string{"conditional metadata registration; native runtime retained"}
	}
	return observed, err
}

func (owner *plannedActivationFixture) Reconcile(ctx context.Context, links ReleaseLinks) error {
	owner.pending = false
	return owner.v2TestReconciler.Reconcile(ctx, links)
}

func TestPlannedActivationSurvivesResumeWithoutChangingReadiness(t *testing.T) {
	ctx := context.Background()
	interrupted := errors.New("interrupted after metadata registration")
	transition, _, _ := v2TransitionFixture(t, func(point string) error {
		if point == "after-reconciler-0" {
			return interrupted
		}
		return nil
	})
	owner := &plannedActivationFixture{v2TestReconciler: v2TestReconciler{id: "fixture-runtime", converged: true}, pending: true}
	transition.options.Reconcilers = []V2ActivationReconciler{owner}
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(ctx, goal)
	if err != nil || !inspection.Assessment.Changed || !slices.Contains(inspection.Assessment.Consequences, "conditional metadata registration; native runtime retained") {
		t.Fatalf("converged owner lost planned consequence: %+v %v", inspection, err)
	}
	outcome, err := transition.Converge(ctx, Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
	if err != nil && !errors.Is(err, interrupted) || outcome.Status == StatusReady || owner.pending {
		t.Fatalf("interruption did not follow registration: %+v %v", outcome, err)
	}
	options := transition.options
	options.fault = nil
	resumed, err := NewV2Transition(options)
	if err != nil {
		t.Fatal(err)
	}
	inspection, err = resumed.Inspect(ctx, goal)
	if err != nil || inspection.Resume == nil || slices.Contains(inspection.Assessment.Consequences, "conditional metadata registration; native runtime retained") {
		t.Fatalf("changed planned text prevented resume: %+v %v", inspection, err)
	}
	outcome, err = resumed.Converge(ctx, Execution{Plan: inspection.Plan})
	if err != nil || outcome.Status != StatusReady {
		t.Fatalf("resume without new authorization: %+v %v", outcome, err)
	}
	// Even a pending metadata condition alone cannot turn a ready repeat into work.
	owner.pending = true
	inspection, err = resumed.Inspect(ctx, goal)
	if err != nil || inspection.Assessment.Changed || len(inspection.Assessment.Consequences) != 0 || inspection.Outcome.Status != StatusReady {
		t.Fatalf("metadata changed readiness: %+v %v", inspection, err)
	}
}

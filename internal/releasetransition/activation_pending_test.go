package releasetransition

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

type crowdedPendingActivation struct{ pendingInputActivation }

func (owner *crowdedPendingActivation) Observe(ctx context.Context, pair ReleasePair, links ReleaseLinks) (V2ActivationObservation, error) {
	state, err := owner.pendingInputActivation.Observe(ctx, pair, links)
	for len(state.Diagnostics) < 129 {
		state.Diagnostics = append(state.Diagnostics, state.Diagnostics[0])
	}
	return state, err
}

func TestPendingDiagnosticCapacityReturnsBoundedRefusal(t *testing.T) {
	transition, _, _ := v2TransitionFixture(t, nil)
	input, calls := false, []string{}
	transition.options.Reconcilers = []V2ActivationReconciler{&crowdedPendingActivation{pendingInputActivation{id: "consumer", input: &input, calls: &calls}}}
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil || inspection.Outcome == nil || inspection.Outcome.Status != StatusOperatorActionRequired || !strings.Contains(inspection.Outcome.Message, "diagnostic capacity") || inspection.Outcome.Retry != "run yard config status --all-local" || len(calls) != 0 {
		t.Fatal("diagnostic limit escaped as an internal error", inspection, err)
	}
	if err := ValidateProcessInspection(goal, inspection); err != nil {
		t.Fatal(err)
	}
}

type pendingInputActivation struct {
	id                    string
	input                 *bool
	current, leavePending bool
	calls                 *[]string
}

func (owner *pendingInputActivation) ID() string { return owner.id }
func (owner *pendingInputActivation) Observe(context.Context, ReleasePair, ReleaseLinks) (V2ActivationObservation, error) {
	pending := owner.id == "consumer" && !*owner.input
	actual := digestB
	if owner.current && !pending {
		actual = digestC
	}
	observation := V2ActivationObservation{Actual: actual, Desired: digestC, Converged: owner.current && !pending, PendingInputs: pending}
	if pending {
		observation.Diagnostics = []RedactedDecision{{Resource: "sample.input.invalid", Scope: "yard.default", Decision: DecisionPreserve, Result: "changed -> current"}}
	}
	return observation, nil
}
func (owner *pendingInputActivation) Reconcile(context.Context, ReleaseLinks) error {
	*owner.calls = append(*owner.calls, owner.id)
	if owner.id == "consumer" && !*owner.input {
		return pendingInputRepairError{}
	}
	if owner.id == "config" && !owner.leavePending {
		*owner.input = true
	}
	owner.current = true
	return nil
}

type pendingInputRepairError struct{}

func (pendingInputRepairError) Error() string { return "input repair pending" }
func (pendingInputRepairError) ActivationDiagnostic() (string, string) {
	return "sample.input.invalid: changed -> current", "run yard -Y default config apply"
}

func TestActivationPendingInputRunsApprovedRepairBeforeConsumer(t *testing.T) {
	for _, leavePending := range []bool{false, true} {
		t.Run(map[bool]string{false: "converges", true: "actionable-refusal"}[leavePending], func(t *testing.T) {
			transition, _, _ := v2TransitionFixture(t, nil)
			input, calls := false, []string{}
			transition.options.Reconcilers = []V2ActivationReconciler{
				&pendingInputActivation{id: "consumer", input: &input, calls: &calls},
				&pendingInputActivation{id: "config", input: &input, calls: &calls, leavePending: leavePending},
			}
			ctx := context.Background()
			inspection, err := transition.Inspect(ctx, Goal{Target: "release-a", Direction: DirectionActivateTarget})
			if err != nil || inspection.Outcome.Status == StatusReady || len(inspection.Blockers) != 0 || len(calls) != 0 {
				t.Fatal(inspection, calls, err)
			}
			if err := ValidateProcessInspection(Goal{Target: "release-a", Direction: DirectionActivateTarget}, inspection); err != nil {
				t.Fatal("pending diagnostics cannot cross the installed owner protocol", err)
			}
			found := false
			for _, diagnostic := range inspection.Decisions {
				if diagnostic.Resource == "sample.input.invalid" && diagnostic.Result == "changed -> current" {
					found = true
				}
			}
			if !found {
				t.Fatal("pending input diagnostic absent from readiness", inspection)
			}
			for _, consequence := range inspection.Assessment.Consequences {
				if strings.Contains(consequence, "sample.input.invalid") {
					t.Fatal("expected drift became a consequence", consequence)
				}
			}
			outcome, err := transition.Converge(ctx, Execution{Plan: inspection.Plan})
			if err != nil || outcome.Code != CodeConfirmationRequired || len(calls) != 0 {
				t.Fatal("pending bypassed consent", outcome, calls, err)
			}
			outcome, err = transition.Converge(ctx, Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
			if err != nil || !reflect.DeepEqual(calls, []string{"config", "consumer"}) {
				t.Fatal("repair order", outcome, calls, err)
			}
			if leavePending {
				if outcome.Status == StatusReady || outcome.Retry != "run yard -Y default config apply" {
					t.Fatal("pending input was hidden or looped", outcome)
				}
			} else {
				if outcome.Status != StatusReady {
					t.Fatal("repair did not converge", outcome)
				}
				input = false
				transition.options.Reconcilers[0].(*pendingInputActivation).current = false
				inspection, err = transition.Inspect(ctx, Goal{Target: "release-a", Direction: DirectionActivateTarget})
				if err != nil || !strings.Contains(inspection.Outcome.Message, "sample.input.invalid: changed -> current") {
					t.Fatal("completed-history readiness hid pending facts", inspection, err)
				}
			}
		})
	}
}

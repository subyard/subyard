package domain

import (
	"errors"
	"testing"
)

func TestOperationStepsPermitOnlyAuthorizedResolution(t *testing.T) {
	approved := []OperationStep{
		{ID: "base", Target: "owner/yard", Observed: "absent", Desired: "ready", Decision: StepApply, Verify: "ready", Consequence: "Prepare yard"},
		{ID: "profile", Target: "owner/yard/profile", Observed: "unknown", Desired: "installed", Decision: StepConditional, DependsOn: []string{"base"}, Preconditions: []string{"owned profile"}, Verify: "installed", Consequence: "Install profile"},
		{ID: "retained", Target: "owner/yard/data", Observed: "current", Desired: "current", Decision: StepSkip, Verify: "retained"},
	}
	for _, name := range []string{"unchanged", "partial-convergence", "conditional-apply", "scope", "desired", "observation", "guard", "verify", "skip-expansion", "new-step"} {
		t.Run(name, func(t *testing.T) {
			fresh := CloneOperationSteps(approved)
			wantStale := true
			switch name {
			case "unchanged":
				wantStale = false
			case "partial-convergence":
				fresh[0].Observed, fresh[0].Decision = "ready", StepSkip
				wantStale = false
			case "conditional-apply":
				fresh[1].Observed, fresh[1].Decision = "absent", StepApply
				wantStale = false
			case "scope":
				fresh[1].Target = "other/yard/profile"
			case "desired":
				fresh[1].Desired = "different"
			case "observation":
				fresh[0].Observed = "foreign"
			case "guard":
				fresh[1].Preconditions[0] = "different ownership"
			case "verify":
				fresh[1].Verify = "different postcondition"
			case "skip-expansion":
				fresh[2].Decision = StepApply
			case "new-step":
				fresh = append(fresh, OperationStep{ID: "extra", Target: "owner/other", Observed: "absent", Desired: "ready", Decision: StepApply, Verify: "ready"})
			}
			err := CheckOperationSteps(approved, fresh)
			if wantStale && !errors.Is(err, ErrPlanStale) || !wantStale && err != nil {
				t.Fatalf("resolution=%s err=%v", name, err)
			}
			if approved[1].Preconditions[0] != "owned profile" {
				t.Fatal("execution projection mutated approved preconditions")
			}
		})
	}
}

func TestOperationStepsRejectInvalidMetadata(t *testing.T) {
	valid := OperationStep{ID: "step", Target: "yard", Observed: "unknown", Desired: "ready", Decision: StepConditional, Verify: "ready"}
	for _, name := range []string{"id", "duplicate", "decision", "control", "invalid-utf8", "missing-target", "future-dependency", "duplicate-dependency"} {
		t.Run(name, func(t *testing.T) {
			steps := []OperationStep{valid}
			switch name {
			case "id":
				steps[0].ID = "../unsafe"
			case "duplicate":
				steps = append(steps, valid)
			case "decision":
				steps[0].Decision = "maybe"
			case "control":
				steps[0].Desired = "ready\nunsafe"
			case "invalid-utf8":
				steps[0].Observed = string([]byte{0xff})
			case "missing-target":
				steps[0].Target = ""
			case "future-dependency":
				steps[0].DependsOn = []string{"later"}
			case "duplicate-dependency":
				other := valid
				other.ID, other.DependsOn = "later", []string{"step", "step"}
				steps = append(steps, other)
			}
			if err := ValidateOperationSteps(steps); !errors.Is(err, ErrActionPolicyInvalid) {
				t.Fatalf("invalid metadata accepted: %v", err)
			}
		})
	}
}

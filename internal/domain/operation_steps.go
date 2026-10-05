package domain

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

type StepDecision string

const (
	StepApply       StepDecision = "apply"
	StepSkip        StepDecision = "skip"
	StepConditional StepDecision = "conditional"
)

// OperationStep describes an authorized effect. Its owning runner retains the
// executable checks and guards; this projection never contains executable code
// or protected payloads. Conditional observations do not widen Target or Desired.
type OperationStep struct {
	ID            string       `json:"id"`
	Target        string       `json:"target"`
	Observed      string       `json:"observed"`
	Desired       string       `json:"desired"`
	Decision      StepDecision `json:"decision"`
	Preconditions []string     `json:"preconditions,omitempty"`
	DependsOn     []string     `json:"dependsOn,omitempty"`
	Verify        string       `json:"verify"`
	Consequence   string       `json:"consequence,omitempty"`
}

func ValidateOperationSteps(steps []OperationStep) error {
	if len(steps) > 256 {
		return fmt.Errorf("%w: too many operation steps", ErrActionPolicyInvalid)
	}
	seen := make(map[string]bool, len(steps))
	for _, step := range steps {
		if !SafeID(step.ID) || seen[step.ID] {
			return fmt.Errorf("%w: invalid or duplicate operation step ID", ErrActionPolicyInvalid)
		}
		if step.Decision != StepApply && step.Decision != StepSkip && step.Decision != StepConditional {
			return fmt.Errorf("%w: invalid operation step decision", ErrActionPolicyInvalid)
		}
		for _, value := range []string{step.Target, step.Observed, step.Desired, step.Verify} {
			if !operationStepText(value) {
				return fmt.Errorf("%w: invalid operation step fact", ErrActionPolicyInvalid)
			}
		}
		if step.Consequence != "" && !operationStepText(step.Consequence) {
			return fmt.Errorf("%w: invalid operation step consequence", ErrActionPolicyInvalid)
		}
		if len(step.Preconditions) > 64 || len(step.DependsOn) > 64 {
			return fmt.Errorf("%w: too many operation step guards", ErrActionPolicyInvalid)
		}
		for _, guard := range step.Preconditions {
			if !operationStepText(guard) {
				return fmt.Errorf("%w: invalid operation step precondition", ErrActionPolicyInvalid)
			}
		}
		dependencies := make(map[string]bool, len(step.DependsOn))
		for _, dependency := range step.DependsOn {
			if !seen[dependency] || dependencies[dependency] {
				return fmt.Errorf("%w: operation step dependency must precede its step", ErrActionPolicyInvalid)
			}
			dependencies[dependency] = true
		}
		seen[step.ID] = true
	}
	return nil
}

func operationStepText(value string) bool {
	return value != "" && len(value) <= 512 && utf8.ValidString(value) &&
		strings.TrimSpace(value) == value && !strings.ContainsFunc(value, unicode.IsControl)
}

func CloneOperationSteps(steps []OperationStep) []OperationStep {
	copy := slices.Clone(steps)
	for index := range copy {
		copy[index].Preconditions = slices.Clone(copy[index].Preconditions)
		copy[index].DependsOn = slices.Clone(copy[index].DependsOn)
	}
	return copy
}

func EqualOperationSteps(left, right []OperationStep) bool {
	return slices.EqualFunc(left, right, func(a, b OperationStep) bool {
		return a.ID == b.ID && a.Target == b.Target && a.Observed == b.Observed && a.Desired == b.Desired &&
			a.Decision == b.Decision && a.Verify == b.Verify && a.Consequence == b.Consequence &&
			slices.Equal(a.Preconditions, b.Preconditions) && slices.Equal(a.DependsOn, b.DependsOn)
	})
}

// CheckOperationSteps accepts only the original ordered effect set or proven
// convergence to its desired state. Native ownership and permission guards must
// still run before mutation, including when an effect resolves to skip.
func CheckOperationSteps(approved, fresh []OperationStep) error {
	if err := ValidateOperationSteps(approved); err != nil {
		return err
	}
	if err := ValidateOperationSteps(fresh); err != nil {
		return err
	}
	if len(approved) != len(fresh) {
		return fmt.Errorf("%w: operation step set changed", ErrPlanStale)
	}
	for index, before := range approved {
		after := fresh[index]
		if before.ID != after.ID || before.Target != after.Target || before.Desired != after.Desired ||
			before.Verify != after.Verify || before.Consequence != after.Consequence ||
			!slices.Equal(before.Preconditions, after.Preconditions) || !slices.Equal(before.DependsOn, after.DependsOn) {
			return fmt.Errorf("%w: operation step %q scope changed", ErrPlanStale, before.ID)
		}
		if after.Decision == StepSkip {
			continue
		}
		if before.Decision == StepSkip || (before.Decision == StepApply &&
			(after.Decision != StepApply || before.Observed != after.Observed)) {
			return fmt.Errorf("%w: operation step %q requires new work", ErrPlanStale, before.ID)
		}
	}
	return nil
}

func OperationStepConsequences(steps []OperationStep) []string {
	var consequences []string
	for _, step := range steps {
		if step.Decision == StepSkip || step.Consequence == "" {
			continue
		}
		consequences = append(consequences, step.Consequence)
	}
	return consequences
}

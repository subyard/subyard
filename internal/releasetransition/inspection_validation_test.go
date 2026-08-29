package releasetransition

import (
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
)

func TestInspectionRejectsAssessmentOutsideTheCanonicalV2Policy(t *testing.T) {
	goal := Goal{Target: "release-b", Direction: DirectionActivateTarget}
	base := Inspection{
		Plan: PlanToken("plan-v1-" + strings.Repeat("a", 64)),
		Assessment: domain.ActionAssessment{
			Action: v2ActionID, Effect: domain.ActionMutation, Changed: true,
			Impacts: []domain.ActionImpact{
				domain.ImpactLocalMetadata, domain.ImpactPersistentData, domain.ImpactYardRuntime,
			},
			Recovery: domain.RecoveryReversible,
			Consequences: []string{
				"apply the exact typed migration and release activation plan",
			},
		},
		Outcome: &Outcome{
			Status: StatusMigrationRequired, Active: "release-a", Target: "release-b",
			Code: CodeTransitionRequired, Message: "the release transition has not started",
			Retry: "run yard update",
		},
	}
	for _, test := range []struct {
		name   string
		mutate func(*domain.ActionAssessment)
	}{
		{
			name: "omitted impacts",
			mutate: func(assessment *domain.ActionAssessment) {
				assessment.Impacts = []domain.ActionImpact{domain.ImpactLocalMetadata}
			},
		},
		{
			name: "injected impacts",
			mutate: func(assessment *domain.ActionAssessment) {
				assessment.Impacts = []domain.ActionImpact{
					domain.ImpactHostOS, domain.ImpactLocalMetadata,
					domain.ImpactPersistentData, domain.ImpactYardRuntime,
				}
			},
		},
		{
			name: "missing changed consequence",
			mutate: func(assessment *domain.ActionAssessment) {
				assessment.Consequences = nil
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			inspection := base
			inspection.Assessment = base.Assessment.Clone()
			test.mutate(&inspection.Assessment)
			if err := inspection.ValidateOutcome(goal); err == nil {
				t.Fatal("inspection accepted an assessment outside the canonical v2 action policy")
			}
		})
	}
}

func TestConvergenceOutcomeMustMatchTheInspectedGoalAndIdentity(t *testing.T) {
	goal := Goal{Target: "release-b", Direction: DirectionActivateTarget}
	inspection := Inspection{
		Plan: PlanToken("plan-v1-" + strings.Repeat("a", 64)),
		Assessment: domain.ActionAssessment{
			Action: v2ActionID, Effect: domain.ActionMutation, Changed: true,
			Impacts: []domain.ActionImpact{
				domain.ImpactLocalMetadata, domain.ImpactPersistentData, domain.ImpactYardRuntime,
			},
			Recovery: domain.RecoveryReversible,
			Consequences: []string{
				"apply the exact typed migration and release activation plan",
			},
		},
		Outcome: &Outcome{
			Status: StatusMigrationRequired, Active: "release-a", Target: "release-b",
			Code: CodeTransitionRequired, Message: "the release transition has not started",
			Retry: "run yard update",
		},
	}
	transaction := TransactionID("tx-0123456789abcdef")
	previous := ReleaseID("release-a")
	valid := Outcome{
		Status: StatusReady, ReachedGoal: true, Active: "release-b", Previous: &previous,
		Target: "release-b", Code: CodeReady, Message: "verified", Transaction: &transaction,
	}
	if err := valid.ValidateConvergence(goal, inspection); err != nil {
		t.Fatalf("valid convergence outcome: %v", err)
	}

	for _, test := range []struct {
		name   string
		mutate func(*Outcome)
	}{
		{"active is not the target", func(outcome *Outcome) { outcome.Active = "release-a" }},
		{"previous is not the inspected active release", func(outcome *Outcome) {
			other := ReleaseID("release-z")
			outcome.Previous = &other
		}},
		{"changed transition has no transaction", func(outcome *Outcome) { outcome.Transaction = nil }},
		{"target differs from the goal", func(outcome *Outcome) { outcome.Target = "release-c" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			outcome := valid
			test.mutate(&outcome)
			if err := outcome.ValidateConvergence(goal, inspection); err == nil {
				t.Fatal("inconsistent convergence outcome was accepted")
			}
		})
	}
}

func TestConvergenceAllowsUnknownLinksOnlyForPostMutationRecoveryAmbiguity(t *testing.T) {
	goal, inspection := convergenceValidationFixture()
	transaction := TransactionID("tx-0123456789abcdef")
	unknown := Outcome{
		Status: StatusOperatorActionRequired, Target: goal.Target,
		Code: CodeRecoveryAmbiguous, Message: "release facts cannot be observed",
		Retry: "run yard update --check", Transaction: &transaction,
	}
	if err := unknown.ValidateConvergence(goal, inspection); err != nil {
		t.Fatalf("valid unknown-link recovery outcome: %v", err)
	}

	invalid := unknown
	invalid.Code = CodePlanStale
	if err := invalid.ValidateConvergence(goal, inspection); err == nil {
		t.Fatal("non-recovery outcome accepted an unknown active release")
	}
	previous := ReleaseID("release-a")
	invalid = unknown
	invalid.Previous = &previous
	if err := invalid.ValidateConvergence(goal, inspection); err == nil {
		t.Fatal("unknown-link outcome accepted a previous release")
	}
	invalid = unknown
	invalid.Transaction = nil
	if err := invalid.ValidateConvergence(goal, inspection); err == nil {
		t.Fatal("unknown-link outcome accepted without a durable transaction")
	}
}

func TestConvergenceRecoveryMustMatchInspectedResumeIdentity(t *testing.T) {
	goal, inspection := convergenceValidationFixture()
	transaction := TransactionID("tx-0123456789abcdef")
	inspection.Resume = &transaction
	inspection.Plan = PlanToken("resume-v1-" + strings.Repeat("b", 64))
	inspection.Outcome = &Outcome{
		Status: StatusRecovering, Active: "release-a", Target: goal.Target,
		Code: CodeRecoveryPending, Message: "the transition can resume",
		Retry: "run yard update", Transaction: &transaction,
	}

	foreign := TransactionID("tx-fedcba9876543210")
	outcome := Outcome{
		Status: StatusRecovering, Active: "release-a", Target: goal.Target,
		Code: CodeRecoveryPending, Message: "the transition can resume",
		Retry: "run yard update", Transaction: &foreign,
	}
	if err := outcome.ValidateConvergence(goal, inspection); err == nil {
		t.Fatal("recovering convergence accepted a foreign transaction")
	}

	outcome.Transaction = &transaction
	outcome.Active = "release-z"
	if err := outcome.ValidateConvergence(goal, inspection); err == nil {
		t.Fatal("recovering convergence accepted impossible release links")
	}
}

func TestConvergenceAcceptsDurableIntermediateModuleOutcomes(t *testing.T) {
	goal, inspection := convergenceValidationFixture()
	transaction := TransactionID("tx-0123456789abcdef")
	stagedTarget := goal.Target
	for _, test := range []struct {
		name     string
		code     OutcomeCode
		previous *ReleaseID
	}{
		{name: "interrupted staged links", code: CodeVerificationFailed, previous: &stagedTarget},
		{name: "retryable dependency", code: CodeDependencyUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			outcome := Outcome{
				Status: StatusRecovering, Active: "release-a", Previous: test.previous,
				Target: goal.Target, Code: test.code, Message: "the transition can resume",
				Retry: "run yard update", Transaction: &transaction,
			}
			if err := outcome.ValidateConvergence(goal, inspection); err != nil {
				t.Fatalf("valid intermediate convergence outcome: %v", err)
			}
		})
	}
}

func convergenceValidationFixture() (Goal, Inspection) {
	goal := Goal{Target: "release-b", Direction: DirectionActivateTarget}
	return goal, Inspection{
		Plan: PlanToken("plan-v1-" + strings.Repeat("a", 64)),
		Assessment: domain.ActionAssessment{
			Action: v2ActionID, Effect: domain.ActionMutation, Changed: true,
			Impacts: []domain.ActionImpact{
				domain.ImpactLocalMetadata, domain.ImpactPersistentData, domain.ImpactYardRuntime,
			},
			Recovery: domain.RecoveryReversible,
			Consequences: []string{
				"apply the exact typed migration and release activation plan",
			},
		},
		Outcome: &Outcome{
			Status: StatusMigrationRequired, Active: "release-a", Target: "release-b",
			Code: CodeTransitionRequired, Message: "the release transition has not started",
			Retry: "run yard update",
		},
	}
}

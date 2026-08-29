// Package releasetransition defines the fact-based contract for durable release
// activation and one-time state migration.
package releasetransition

import (
	"context"

	"github.com/Subyard/Subyard/internal/domain"
)

type ReleaseTransition interface {
	Inspect(context.Context, Goal) (Inspection, error)
	Converge(context.Context, Execution) (Outcome, error)
}

type ReleaseID string
type Direction string
type PublicStatus string
type OutcomeCode string
type TransactionID string
type PlanToken string
type Authorization string
type Decision string
type Fingerprint string

const (
	DirectionActivateTarget   Direction = "activate-target"
	DirectionActivatePrevious Direction = "activate-previous"
)

const (
	StatusReady                  PublicStatus = "ready"
	StatusMigrationRequired      PublicStatus = "migration-required"
	StatusRecovering             PublicStatus = "recovering"
	StatusOperatorActionRequired PublicStatus = "operator-action-required"
)

const (
	CodeReady                 OutcomeCode = "ready"
	CodeTransitionRequired    OutcomeCode = "transition-required"
	CodeRecoveryPending       OutcomeCode = "recovery-pending"
	CodeRegistryInvalid       OutcomeCode = "registry-invalid"
	CodeUnsupportedEpoch      OutcomeCode = "unsupported-epoch"
	CodeUnsupportedKind       OutcomeCode = "unsupported-kind"
	CodeConfirmationRequired  OutcomeCode = "confirmation-required"
	CodeOperationDeclined     OutcomeCode = "operation-declined"
	CodePlanStale             OutcomeCode = "plan-stale"
	CodeMigrationStale        OutcomeCode = "migration-stale"
	CodeResourceConflict      OutcomeCode = "resource-conflict"
	CodePreconditionBlocked   OutcomeCode = "precondition-blocked"
	CodeDependencyUnavailable OutcomeCode = "dependency-unavailable"
	CodeVerificationFailed    OutcomeCode = "verification-failed"
	CodeActivationAmbiguous   OutcomeCode = "activation-ambiguous"
	CodeRecoveryAmbiguous     OutcomeCode = "recovery-ambiguous"
	CodeJournalInvalid        OutcomeCode = "journal-invalid"
	CodeRollbackIncompatible  OutcomeCode = "rollback-incompatible"
	CodeRollbackExpired       OutcomeCode = "rollback-expired"
)

const (
	DecisionPreserve     Decision = "preserve"
	DecisionTransform    Decision = "transform"
	DecisionCanonicalize Decision = "canonicalize"
	DecisionReset        Decision = "reset"
	DecisionQuarantine   Decision = "quarantine"
	DecisionBlock        Decision = "block"
)

type Goal struct {
	Target    ReleaseID `json:"target"`
	Direction Direction `json:"direction"`
}

type RedactedDecision struct {
	Resource string   `json:"resource"`
	Scope    string   `json:"scope,omitempty"`
	Decision Decision `json:"decision"`
	Result   string   `json:"result,omitempty"`
}

type Blocker struct {
	Code     OutcomeCode `json:"code"`
	Resource string      `json:"resource,omitempty"`
	Message  string      `json:"message"`
	Retry    string      `json:"retry"`
}

type Inspection struct {
	Plan       PlanToken               `json:"plan"`
	Assessment domain.ActionAssessment `json:"assessment"`
	Decisions  []RedactedDecision      `json:"decisions,omitempty"`
	Blockers   []Blocker               `json:"blockers,omitempty"`
	Resume     *TransactionID          `json:"resume,omitempty"`
	Outcome    *Outcome                `json:"outcome,omitempty"`
}

type Execution struct {
	Plan          PlanToken     `json:"plan"`
	Authorization Authorization `json:"authorization"`
}

type Outcome struct {
	Status      PublicStatus `json:"status"`
	ReachedGoal bool         `json:"reachedGoal"`
	// Active is empty only when a post-mutation observation failed and the
	// operator-action-required outcome must represent the link as unknown.
	Active      ReleaseID      `json:"active"`
	Previous    *ReleaseID     `json:"previous,omitempty"`
	Target      ReleaseID      `json:"target"`
	Code        OutcomeCode    `json:"code"`
	Message     string         `json:"message"`
	Retry       string         `json:"retry,omitempty"`
	Transaction *TransactionID `json:"transaction,omitempty"`
	Warnings    []string       `json:"warnings,omitempty"`
}

func (goal Goal) Validate() error {
	if err := validateReleaseID(goal.Target, "target release"); err != nil {
		return err
	}
	if !validDirection(goal.Direction) {
		return invalid("unknown release direction %q", goal.Direction)
	}
	return nil
}

func validDirection(value Direction) bool {
	return value == DirectionActivateTarget || value == DirectionActivatePrevious
}

func validOutcomeCode(value OutcomeCode) bool {
	switch value {
	case CodeReady, CodeTransitionRequired, CodeRecoveryPending, CodeRegistryInvalid,
		CodeUnsupportedEpoch, CodeUnsupportedKind, CodeConfirmationRequired,
		CodeOperationDeclined, CodePlanStale, CodeMigrationStale, CodeResourceConflict,
		CodePreconditionBlocked, CodeDependencyUnavailable, CodeVerificationFailed,
		CodeActivationAmbiguous, CodeRecoveryAmbiguous, CodeJournalInvalid,
		CodeRollbackIncompatible, CodeRollbackExpired:
		return true
	default:
		return false
	}
}

func validDecision(value Decision) bool {
	switch value {
	case DecisionPreserve, DecisionTransform, DecisionCanonicalize, DecisionReset,
		DecisionQuarantine, DecisionBlock:
		return true
	default:
		return false
	}
}

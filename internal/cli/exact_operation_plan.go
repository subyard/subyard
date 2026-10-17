package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
)

const exactPlanLifetime = 5 * time.Minute

// The additive capability leaves legacy callers unchanged. Exact clients must
// retain this envelope and execute its digest over the same negotiated session.
type exactOperationPlan struct {
	Schema     int                  `json:"schema"`
	StepSchema int                  `json:"stepSchema,omitempty"`
	Plan       domain.OperationPlan `json:"plan"`
	Digest     string               `json:"digest"`
	ExpiresAt  time.Time            `json:"expiresAt"`
}

func exactOperationDigest(prepared *preparedCommand, plan exactOperationPlan) string {
	return operationStateDigest(struct {
		Schema     int
		StepSchema int
		Plan       domain.OperationPlan
		ExpiresAt  time.Time
		Context    domain.Context
		Arguments  []string
		State      string
	}{plan.Schema, plan.StepSchema, plan.Plan, plan.ExpiresAt, prepared.Loaded.Context, prepared.Arguments, prepared.exactState})
}

func bindExactOperationPlan(prepared *preparedCommand, now time.Time) exactOperationPlan {
	plan := exactOperationPlan{Schema: 1, Plan: prepared.Plan.Clone(), ExpiresAt: now.Add(exactPlanLifetime)}
	if prepared.stepsComplete {
		plan.StepSchema = 1
	}
	plan.Digest = exactOperationDigest(prepared, plan)
	return plan
}

func validateExactOperationPlan(plan exactOperationPlan, prepared *preparedCommand, digest string, now time.Time) error {
	if !now.Before(plan.ExpiresAt) {
		return fmt.Errorf("%w: operation plan expired", domain.ErrPlanStale)
	}
	if plan.Schema != 1 || plan.StepSchema < 0 || plan.StepSchema > 1 || (plan.StepSchema == 1 && (!prepared.stepsComplete || domain.ValidateOperationSteps(plan.Plan.Steps) != nil || len(plan.Plan.Steps) == 0)) || digest == "" || digest != plan.Digest || digest != exactOperationDigest(prepared, plan) || operationStateDigest(plan.Plan) != operationStateDigest(prepared.Plan) {
		return errors.New("operation plan digest does not match the stored plan")
	}
	return nil
}

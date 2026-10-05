package reconcileruntime

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
)

func cloneIntegrationConfig(config map[string]string) map[string]string {
	copy := maps.Clone(config)
	for _, key := range []string{"user.subyard.ai_observer_proxy", "user.subyard.ai_observer_provision", "user.subyard.ccusage_version"} {
		delete(copy, key)
	}
	return copy
}

func (plan IntegrationPlan) StateBinding() string {
	payload, _ := json.Marshal(struct {
		Plan  IntegrationPlan
		Scope string
		Facts []integrationFact
	}{plan, plan.Scope, plan.facts})
	return fmt.Sprintf("%x", sha256.Sum256(payload))
}

// Actual ownership is checked by the native inventory and structured writers.
// Only movement toward captured desired facts can relax a changed fingerprint.
func CheckIntegrationPlan(approved, fresh IntegrationPlan) error {
	if approved.Fingerprint == fresh.Fingerprint && approved.Changed == fresh.Changed {
		return nil
	}
	if approved.Scope == "" || fresh.Scope != approved.Scope || len(approved.facts) == 0 || !approved.Changed {
		return fmt.Errorf("%w: integration scope or runtime changed", domain.ErrPlanStale)
	}
	before := make(map[string]integrationFact, len(approved.facts))
	for _, fact := range approved.facts {
		before[fact.Key] = fact
	}
	for _, fact := range fresh.facts {
		old, exists := before[fact.Key]
		if !exists || old.Desired != fact.Desired || (!fact.AtDesired && (old.AtDesired || old.Observed != fact.Observed)) {
			return fmt.Errorf("%w: integration target requires unapproved work", domain.ErrPlanStale)
		}
		delete(before, fact.Key)
	}
	for _, fact := range before {
		if fact.Desired != "absent" {
			return fmt.Errorf("%w: integration target set changed", domain.ErrPlanStale)
		}
	}
	return nil
}

func (plan IntegrationPlan) OperationSteps(target string) []domain.OperationStep {
	decision, observed := domain.StepSkip, "converged"
	if plan.Changed {
		decision, observed = domain.StepApply, "captured native ownership, readiness and inventory"
	}
	steps := []domain.OperationStep{{ID: "integration.runtime", Target: target, Observed: observed,
		Desired: "approved selected integration artifacts, packages, routes, managed startup and installed project hooks", Decision: decision,
		Preconditions: []string{"selected artifact destinations and desired source identities are unchanged", "native ownership and protected path checks pass", "each captured converged fact remains converged; other facts retain their baseline or reach desired state"},
		Verify:        "native artifact inventory, structured configuration, package checks, routes and project hooks all return converged", Consequence: "reconcile only the approved selected integration artifacts, packages, routes and installed project hooks"}}
	for index, fact := range plan.facts {
		decision, observed := domain.StepApply, "captured pending native fact"
		if fact.AtDesired {
			decision, observed = domain.StepSkip, "converged"
		}
		desired := "captured desired native state"
		if fact.Desired == "absent" {
			desired = "owned artifact absent; unrelated artifacts retained"
		}
		label := fact.Key
		if strings.HasPrefix(label, "check:") {
			label = fmt.Sprintf("readiness check %d", index)
		}
		steps = append(steps, domain.OperationStep{ID: fmt.Sprintf("integration.fact.%d", index), Target: target + ":" + label,
			Observed: observed, Desired: desired, Decision: decision,
			Preconditions: []string{"native target ownership and captured desired identity are unchanged", "a converged target cannot require new work"},
			Verify:        "native observation proves this captured fact reached its original desired state", Consequence: "converge " + label})
	}
	return steps
}

func (plan IntegrationCleanupPlan) StateBinding() string {
	payload, _ := json.Marshal(struct {
		Fingerprint, Hook, Scope string
		Script                   []byte
	}{plan.Fingerprint, plan.hookState, plan.scope, plan.script})
	return fmt.Sprintf("%x", sha256.Sum256(payload))
}

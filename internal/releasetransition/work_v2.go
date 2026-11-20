package releasetransition

import (
	"reflect"
	"slices"

	"github.com/Subyard/Subyard/internal/migrationhistory"
)

// Only generic source ingress, live ledger-only migrations and initial baseline
// publication carry work. Frozen historical migrations cannot construct an
// executable recipe.
type v2Work struct {
	intent    PlannerStepIntent
	migration MigrationDefinitionV2
	recipe    *ingressWorkRecipe
	ledger    *ledgerWorkRecipe
	baseline  []byte
}
type ingressWorkRecipe struct {
	operation V2IngressOperation
	binding   V2IngressBinding
}
type ledgerWorkRecipe struct {
	edge migrationhistory.Edge
	from authorityLedgerRevision
	to   []byte
}

// Journal edges bind the validated authoritative ledger view. A pinned legacy
// reader projection has a separate type and cannot supply CAS work authority.
type authorityLedgerRevision struct{ snapshot ProtectedSnapshot }
type compatibilityProjectionRevision struct{ snapshot ProtectedSnapshot }

func (work v2Work) kind() v2WorkKind {
	if work.baseline != nil {
		return v2BaselineWork
	}
	if work.ledger != nil {
		return v2LedgerWork
	}
	if work.recipe != nil {
		return v2IngressWork
	}
	return ""
}
func (work v2Work) ingressRecipe() *ingressWorkRecipe { return work.recipe }
func (work v2Work) ledgerRecipe() *ledgerWorkRecipe   { return work.ledger }
func ledgerMigrationWork(intent PlannerStepIntent, migration MigrationDefinitionV2, edge migrationhistory.Edge, from authorityLedgerRevision, to []byte) v2Work {
	from.snapshot.Payload = slices.Clone(from.snapshot.Payload)
	migration.DependsOn = slices.Clone(migration.DependsOn)
	return v2Work{intent: intent, migration: migration, ledger: &ledgerWorkRecipe{edge: edge, from: from, to: slices.Clone(to)}}
}
func ingressMigrationWork(intent PlannerStepIntent, operation V2IngressOperation, binding V2IngressBinding) v2Work {
	binding.Steps = slices.Clone(binding.Steps)
	binding.Releases.Previous = cloneReleaseID(binding.Releases.Previous)
	return v2Work{intent: intent, recipe: &ingressWorkRecipe{operation: operation, binding: binding}}
}
func (transition *V2Transition) validateWorkRecipe(work v2Work, journal JournalRecord, step JournalStep) error {
	if transition == nil {
		return invalid("migration work has no transition owner")
	}
	expected := PlannerStepIntent{ID: step.ID, Migration: step.Migration, Resource: step.Resource, Decision: step.Decision, Expected: step.Expected, Desired: step.Desired}
	if !validStepCheckpoint(step.Checkpoint) {
		return invalid("migration work has an unknown evidence phase")
	}
	if err := validatePlannerIntents([]PlannerStepIntent{expected}); err != nil {
		return err
	}
	if err := validateStepEvidence(journal, step); err != nil {
		return err
	}
	if work.kind() == v2BaselineWork {
		return transition.validateBaselineWork(work, journal, expected)
	}
	if work.kind() == v2LedgerWork {
		if work.intent != expected {
			return invalid("migration work recipe is absent or differs from its journal intent")
		}
		return transition.validateLedgerWork(work, step)
	}
	if work.kind() != v2IngressWork || work.intent != expected {
		return invalid("migration work recipe is absent or differs from its journal intent")
	}
	kind, known := ingressOperationKindForIntent(expected)
	if !known || kind != work.recipe.operation.Kind || transition.options.Ingress == nil || ingressIntent(work.recipe.operation) != expected {
		return invalid("ingress recipe differs from its journal intent")
	}
	return nil
}
func nilV2Adapter(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return v.IsNil()
	}
	return false
}
func historicalProjectionLedger(registry RegistryV2, projection compatibilityProjectionRevision) (LedgerV2, error) {
	if !projection.snapshot.Exists || !validProtectedSnapshot(projection.snapshot) {
		return LedgerV2{}, invalid("historical compatibility projection is unavailable")
	}
	ledger, _, err := ParseLedgerV2(projection.snapshot.Payload, registry)
	return ledger, err
}

// Reconstruct the complete work set before authorization publication. Per-step
// validation repeats immediately before effects and rereads recorded results.
func (transition *V2Transition) validateObservationWork(observation v2Observation) error {
	if len(observation.work) != len(observation.intents) {
		return invalid("migration work reconstruction is incomplete")
	}
	seen := map[string]bool{}
	for _, work := range observation.work {
		if seen[work.intent.ID] {
			return invalid("duplicate reconstructed migration work")
		}
		seen[work.intent.ID] = true
		matched := false
		for _, intent := range observation.intents {
			if intent == work.intent {
				matched = true
				break
			}
		}
		if !matched {
			return invalid("reconstructed work has no matching intent")
		}
		intent := work.intent
		step := JournalStep{ID: intent.ID, Migration: intent.Migration, Resource: intent.Resource, Decision: intent.Decision, Expected: intent.Expected, Desired: intent.Desired, Checkpoint: StepIntent}
		journal := JournalRecord{}
		if observation.journal != nil && observation.journal.Checkpoint != JournalComplete && observation.replacement == nil {
			journal = *observation.journal
			for _, candidate := range journal.Steps {
				if candidate.ID == step.ID {
					step = candidate
					break
				}
			}
		}
		if err := transition.validateWorkRecipe(work, journal, step); err != nil {
			return err
		}
	}
	return nil
}

// boundWorkStep is the checked recipe/phase intersection consumed by effects.
// Frozen JournalStep DTOs and an arbitrary work recipe cannot call that boundary.
type boundWorkStep struct {
	owner       *V2Transition
	transaction TransactionID
	index       int
	checkpoint  StepCheckpoint
	work        v2Work
}

func (transition *V2Transition) bindWorkStep(journal JournalRecord, index int, work v2Work) (boundWorkStep, error) {
	if transition == nil {
		return boundWorkStep{}, invalid("migration work has no transition owner")
	}
	if index < 0 || index >= len(journal.Steps) {
		return boundWorkStep{}, invalid("migration work has no journal step")
	}
	if err := transition.validateWorkRecipe(work, journal, journal.Steps[index]); err != nil {
		return boundWorkStep{}, err
	}
	return boundWorkStep{owner: transition, transaction: journal.Transaction, index: index, checkpoint: journal.Steps[index].Checkpoint, work: work}, nil
}

// validateBoundWorkStep rechecks an effect-bound recipe, including physical
// freshness. It is distinct from pure record validation.
func (bound boundWorkStep) validateBoundWorkStep(transition *V2Transition, journal *JournalRecord) error {
	if bound.owner == nil || bound.owner != transition || journal == nil || bound.transaction != journal.Transaction || bound.index < 0 || bound.index >= len(journal.Steps) || bound.checkpoint != journal.Steps[bound.index].Checkpoint {
		return invalid("zero, foreign or stale bound work step")
	}
	return transition.validateWorkRecipe(bound.work, *journal, journal.Steps[bound.index])
}

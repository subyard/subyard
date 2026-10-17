package releasetransition

import (
	"reflect"
	"slices"

	"github.com/Subyard/Subyard/internal/migrationhistory"
)

// Work has one closed recipe variant. A recorded settings result is explicitly
// observation-only; it cannot supply a missing mutation recipe.
type v2Work struct {
	intent    PlannerStepIntent
	migration MigrationDefinitionV2
	recipe    v2WorkRecipe
}
type v2WorkRecipe interface{ migrationWorkRecipe() }
type settingsWorkRecipe struct{ file settingsV2FilePlan }
type recordedSettingsWork struct{ intent PlannerStepIntent }
type ownerWorkRecipe struct{ plan ownerRegistrationV2Plan }
type ingressWorkRecipe struct {
	operation V2IngressOperation
	binding   V2IngressBinding
}
type ledgerWorkRecipe struct {
	edge migrationhistory.Edge
	from authorityLedgerRevision
	to   []byte
}

func (*settingsWorkRecipe) migrationWorkRecipe()   {}
func (*recordedSettingsWork) migrationWorkRecipe() {}
func (*ownerWorkRecipe) migrationWorkRecipe()      {}
func (*ingressWorkRecipe) migrationWorkRecipe()    {}
func (*ledgerWorkRecipe) migrationWorkRecipe()     {}

// Journal edges bind the validated authoritative ledger view. A pinned legacy
// reader projection has a separate type and cannot supply CAS work authority.
type authorityLedgerRevision struct{ snapshot ProtectedSnapshot }
type compatibilityProjectionRevision struct{ snapshot ProtectedSnapshot }

func (work v2Work) kind() v2WorkKind {
	switch recipe := work.recipe.(type) {
	case *settingsWorkRecipe:
		if recipe != nil {
			return v2SettingsWork
		}
	case *recordedSettingsWork:
		if recipe != nil {
			return v2SettingsWork
		}
	case *ownerWorkRecipe:
		if recipe != nil {
			return v2OwnerWork
		}
	case *ingressWorkRecipe:
		if recipe != nil {
			return v2IngressWork
		}
	case *ledgerWorkRecipe:
		if recipe != nil {
			return v2LedgerWork
		}
	}
	return ""
}
func (work v2Work) settingsFile() *settingsV2FilePlan {
	if recipe, ok := work.recipe.(*settingsWorkRecipe); ok && recipe != nil {
		return &recipe.file
	}
	return nil
}
func (work v2Work) ownerPlan() *ownerRegistrationV2Plan {
	if recipe, ok := work.recipe.(*ownerWorkRecipe); ok && recipe != nil {
		return &recipe.plan
	}
	return nil
}
func (work v2Work) ingressRecipe() *ingressWorkRecipe {
	recipe, _ := work.recipe.(*ingressWorkRecipe)
	return recipe
}
func (work v2Work) ledgerRecipe() *ledgerWorkRecipe {
	recipe, _ := work.recipe.(*ledgerWorkRecipe)
	return recipe
}
func cloneWorkMigration(migration MigrationDefinitionV2) MigrationDefinitionV2 {
	migration.DependsOn = slices.Clone(migration.DependsOn)
	return migration
}
func settingsMigrationWork(intent PlannerStepIntent, migration MigrationDefinitionV2, file settingsV2FilePlan) v2Work {
	file.Desired = slices.Clone(file.Desired)
	file.Before = slices.Clone(file.Before)
	file.Expected.Content = slices.Clone(file.Expected.Content)
	return v2Work{intent: intent, migration: cloneWorkMigration(migration), recipe: &settingsWorkRecipe{file: file}}
}
func ownerMigrationWork(intent PlannerStepIntent, migration MigrationDefinitionV2, plan ownerRegistrationV2Plan) v2Work {
	return v2Work{intent: intent, migration: cloneWorkMigration(migration), recipe: &ownerWorkRecipe{plan: plan}}
}
func ingressMigrationWork(intent PlannerStepIntent, operation V2IngressOperation, binding V2IngressBinding) v2Work {
	binding.Steps = slices.Clone(binding.Steps)
	binding.Releases.Previous = cloneReleaseID(binding.Releases.Previous)
	return v2Work{intent: intent, recipe: &ingressWorkRecipe{operation: operation, binding: binding}}
}
func ledgerMigrationWork(intent PlannerStepIntent, migration MigrationDefinitionV2, edge migrationhistory.Edge, from authorityLedgerRevision, to []byte) v2Work {
	from.snapshot.Payload = slices.Clone(from.snapshot.Payload)
	return v2Work{intent: intent, migration: cloneWorkMigration(migration), recipe: &ledgerWorkRecipe{edge: edge, from: from, to: slices.Clone(to)}}
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
	if work.kind() == "" || work.intent != expected {
		return invalid("migration work recipe is absent, typed nil or differs from its journal intent")
	}
	switch recipe := work.recipe.(type) {
	case *ledgerWorkRecipe:
		return transition.validateLedgerWork(work, step)
	case *settingsWorkRecipe:
		if !settingsWorkMigration(work.migration) || recipe.file.LocalSSHPort != (work.migration.Kind == localSSHPortMigration) || recipe.file.validate() != nil || settingsIntent(work.migration, recipe.file) != expected {
			return invalid("settings recipe differs from its journal intent")
		}
	case *recordedSettingsWork:
		if !settingsWorkMigration(work.migration) || recipe.intent != expected || step.Checkpoint == StepIntent {
			return invalid("recorded settings work has no verified reconstruction")
		}
		_, _, actual, err := transition.observeJournalSettingsWork(journal, step, work.migration)
		if err != nil || actual != step.Desired {
			return invalid("recorded settings work cannot provide a mutation recipe")
		}
	case *ownerWorkRecipe:
		if work.migration.Kind != "test-yard-owner-v1-to-v2" || work.migration.Domain != "owner-registration" || step.ID != work.migration.ID+"."+ownerRegistrationResource || step.Decision != DecisionCanonicalize || transition.options.OwnerRegistration == nil || validateOwnerRegistrationObservation(recipe.plan.Before) != nil || recipe.plan.ExpectedFingerprint != step.Expected || recipe.plan.DesiredFingerprint != step.Desired || step.Resource != ownerRegistrationResource {
			return invalid("owner recipe differs from its journal intent")
		}
	case *ingressWorkRecipe:
		kind, known := ingressOperationKindForIntent(expected)
		if !known || kind != recipe.operation.Kind || transition.options.Ingress == nil || ingressIntent(recipe.operation) != expected {
			return invalid("ingress recipe differs from its journal intent")
		}
		return nil
	default:
		return invalid("unknown migration work recipe")
	}
	definition, known := transition.migration(work.migration.ID)
	if !known || !sameMigrationDefinition(definition, work.migration) {
		return invalid("migration work recipe has a foreign definition")
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

func settingsWorkMigration(migration MigrationDefinitionV2) bool {
	return migration.Domain == "settings" && (migration.Kind == localSSHPortMigration || migration.Kind == "test-vms-settings-v1-to-v2")
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

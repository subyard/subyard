package releasetransition

import (
	"context"

	"github.com/Subyard/Subyard/internal/migrationhistory"
)

type rollbackRewindRecipe struct {
	edge       migrationhistory.ReverseEdge
	projection Fingerprint
}

func (*rollbackRewindRecipe) migrationWorkRecipe() {}

type rollbackVerifyRecipe struct{}

func (*rollbackVerifyRecipe) migrationWorkRecipe() {}

// rewindOnlyWork recreates the identical work from the sealed origin, even if
// a prior process published the checkpoint but died before recording it.
func (transition *V2Transition) rewindOnlyWork(edge migrationhistory.ReverseEdge, projection Fingerprint) ([]v2Work, error) {
	migration, ok := transition.migration(edge.Migration().ID)
	definition, compiled := compiledMigration(migration)
	if !ok || !compiled || !sameMigrationDefinition(migration, definition.migration) || definition.rollback.class != rollbackRewindOnly {
		return nil, invalid("rollback edge has no compiled rewind-only verifier")
	}
	owner, err := transition.registry.historyRegistry()
	if err != nil {
		return nil, err
	}
	if err := owner.CheckReverseEdgeBinding(edge); err != nil {
		return nil, err
	}
	_, before, err := MarshalLedgerV2(ledgerFromAuthority(edge.Before()), transition.registry)
	if err != nil {
		return nil, err
	}
	_, after, err := MarshalLedgerV2(ledgerFromAuthority(edge.After()), transition.registry)
	if err != nil {
		return nil, err
	}
	predicate := fingerprintPayload([]byte("rewind-only:" + migration.ID))
	return []v2Work{
		{intent: PlannerStepIntent{ID: migration.ID + ".verify-rollback", Migration: migration.ID, Resource: "rollback.compatibility", Decision: DecisionPreserve, Expected: predicate, Desired: predicate}, migration: migration, recipe: &rollbackVerifyRecipe{}},
		{intent: PlannerStepIntent{ID: migration.ID + ".rewind-ledger", Migration: migration.ID, Resource: "ledger." + migration.Domain, Decision: DecisionTransform, Expected: before, Desired: after}, migration: migration, recipe: &rollbackRewindRecipe{edge: edge, projection: projection}},
	}, nil
}

func (transition *V2Transition) publishRollbackEdge(snapshot ProtectedSnapshot, work v2Work, recipe *rollbackRewindRecipe) error {
	state, err := transition.store.ReadMigrationLedger(transition.registry, transition.registryDigest)
	if err != nil {
		return err
	}
	if !state.Checkpointed || state.ProjectionSnapshot.Fingerprint != recipe.projection {
		return ErrProtectedStoreStale
	}
	proof, err := transition.store.prepareCheckpointRewind(state, transition.registry, transition.registryDigest, transition.registry, transition.registryDigest, recipe.edge, snapshot, work.intent.ID)
	if err != nil {
		return err
	}
	return transition.store.rewindMigrationCheckpoint(proof)
}

func (transition *V2Transition) observeRollbackWork(ctx context.Context, work v2Work) (Fingerprint, error) {
	switch recipe := work.recipe.(type) {
	case *rollbackVerifyRecipe:
		definition, _ := compiledMigration(work.migration)
		if err := definition.rollback.verify(transition, ctx); err != nil {
			return "", err
		}
		return work.intent.Desired, nil
	case *rollbackRewindRecipe:
		state, err := transition.store.ReadMigrationLedger(transition.registry, transition.registryDigest)
		if err != nil {
			return "", err
		}
		if !state.Checkpointed || state.ProjectionSnapshot.Fingerprint != recipe.projection {
			return "", ErrProtectedStoreStale
		}
		_, pin, err := MarshalLedgerV2(state.Ledger, transition.registry)
		return pin, err
	default:
		return "", invalid("unknown rollback work recipe")
	}
}

func (transition *V2Transition) validateRollbackWork(journal JournalRecord, work v2Work) error {
	if journal.RegistryDigest != transition.registryDigest || journal.Goal.Direction != DirectionActivatePrevious || journal.Checkpoint != JournalMigrating || !HasRollbackSourceOwner(journal) {
		return invalid("rollback work has no source-owned migrating journal")
	}
	migration, known := transition.migration(work.migration.ID)
	definition, compiled := compiledMigration(migration)
	if !known || !compiled || !sameMigrationDefinition(migration, definition.migration) || !sameMigrationDefinition(migration, work.migration) || definition.rollback.class != rollbackRewindOnly {
		return invalid("rollback work has no exact compiled verifier")
	}
	if recipe, ok := work.recipe.(*rollbackRewindRecipe); ok {
		expected, err := transition.rewindOnlyWork(recipe.edge, recipe.projection)
		if err != nil {
			return err
		}
		if expected[1].intent != work.intent {
			return invalid("rollback ledger work differs from its reverse edge")
		}
	}
	return nil
}

// convergeRollbackRewind runs the next reverse edge with the transition lock
// held. Abort calls it only when that edge has already entered an uncertain
// phase. Ordinary execution can also call it for a new edge.
func (transition *V2Transition) convergeRollbackRewind(ctx context.Context, origin rollbackHistoryOrigin, journal *JournalRecord, selected *ProtectedSnapshot, links ReleaseLinks, uncertainOnly bool) (Outcome, error) {
	if err := origin.Owner.CheckJournal(*journal); err != nil {
		return Outcome{}, err
	}
	if origin.Owner.ConfigHome != transition.store.configHome || origin.Owner.SourceRegistry != transition.registryDigest || journal.Checkpoint != JournalMigrating {
		return Outcome{}, invalid("rollback recovery belongs to another source")
	}
	payload, err := MarshalJournal(*journal)
	if err != nil {
		return Outcome{}, err
	}
	current, err := transition.store.ReadCurrentJournal()
	if err != nil {
		return Outcome{}, err
	}
	if !sameProtectedSnapshot(current, *selected) || !sameProtectedSnapshot(current, protectedSnapshotFromPayload(payload)) {
		return Outcome{}, ErrProtectedStoreStale
	}
	payload, err = origin.marshal()
	if err != nil {
		return Outcome{}, err
	}
	record, err := transition.store.ReadRecovery(journal.Transaction, rollbackSourcePlanStep)
	if err != nil {
		return Outcome{}, err
	}
	if !sameProtectedSnapshot(record, protectedSnapshotFromPayload(payload)) {
		return Outcome{}, ErrProtectedStoreStale
	}
	bound := false
	for _, step := range journal.Steps {
		if step.ID != rollbackSourcePlanStep {
			continue
		}
		if step.Resource != "rollback.source-plan" || step.Expected != record.Fingerprint || step.Desired != record.Fingerprint || step.Checkpoint != StepVerified || step.Evidence == nil || step.Evidence.Recovery != record.Fingerprint {
			return Outcome{}, invalid("rollback history origin differs from the verified plan")
		}
		if err := transition.store.checkRewindStepEvidence(*journal, step); err != nil {
			return Outcome{}, err
		}
		bound = true
	}
	if !bound {
		return Outcome{}, invalid("rollback history origin is not bound to the journal")
	}
	source, target, initial, err := origin.inspect()
	if err != nil {
		return Outcome{}, err
	}
	progress, err := inspectRollbackHistoryProgress(source, target, initial.Ledger, *journal)
	if err != nil {
		return Outcome{}, err
	}
	if len(progress.remaining) == 0 || uncertainOnly && !progress.needsResolution {
		return Outcome{}, nil
	}
	work, err := transition.rewindOnlyWork(progress.remaining[0], origin.Projection.Fingerprint)
	if err != nil {
		return Outcome{}, err
	}
	actual, err := transition.observeRollbackWork(ctx, work[1])
	if err != nil {
		return Outcome{}, err
	}
	if actual != work[1].intent.Expected && actual != work[1].intent.Desired {
		return Outcome{}, ErrProtectedStoreStale
	}
	for _, item := range work {
		index := -1
		for i, step := range journal.Steps {
			if step.ID == item.intent.ID {
				index = i
				break
			}
		}
		bound, err := transition.bindWorkStep(*journal, index, item)
		if err != nil {
			return Outcome{}, err
		}
		step := journal.Steps[index]
		if step.Checkpoint == StepVerified {
			if err := transition.store.checkRewindStepEvidence(*journal, step); err != nil {
				return Outcome{}, err
			}
			if _, err := transition.observeRollbackWork(ctx, item); err != nil {
				return Outcome{}, err
			}
			continue
		}
		if item.kind() == v2RollbackRewindWork && actual == item.intent.Desired && step.Checkpoint == StepIntent {
			return Outcome{}, ErrProtectedStoreStale
		}
		outcome, err := transition.convergeStep(ctx, journal, selected, bound, links)
		if err != nil || outcome.Code != "" {
			return outcome, err
		}
	}
	return Outcome{}, nil
}

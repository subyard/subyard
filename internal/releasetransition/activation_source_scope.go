package releasetransition

import (
	"context"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
)

// V2SourceStableActivationReconciler can assess release-wide activation when
// prepared source work changes only the ledger or pins existing effective SSH
// ports. The returned function restores its historical scope.
type V2SourceStableActivationReconciler interface {
	UseSourceStableActivationScope() func()
}

func (transition *V2Transition) sourceWorkStable(observation v2Observation) bool {
	if transition.options.Direction != DirectionActivateTarget ||
		transition.options.SourceIngress != nil || len(observation.blockers) != 0 {
		return false
	}
	if journal := observation.journal; journal != nil &&
		(journal.Checkpoint != JournalComplete ||
			(transition.options.MigrationCheckpoint && observation.migrationLedgerSnapshot.RequiresCheckpointPublication())) {
		// Terminal checkpoint publication still uses the source scope that was
		// authorized before these migrations changed configuration/registration.
		if journal.SourceIngress != nil {
			return false
		}
		for _, step := range journal.Steps {
			if !strings.HasPrefix(step.Resource, "ledger.") && !transition.localSSHPortScopeStable(PlannerStepIntent{
				ID: step.ID, Migration: step.Migration, Resource: step.Resource,
				Decision: step.Decision, Expected: step.Expected, Desired: step.Desired,
			}) {
				return false
			}
		}
	}
	for _, work := range observation.work {
		if work.kind == v2LedgerWork {
			continue
		}
		if work.kind != v2SettingsWork || work.migration.Kind != localSSHPortMigration ||
			!transition.localSSHPortScopeStable(work.intent) {
			return false
		}
		if work.file != nil {
			if !work.file.LocalSSHPort || work.file.validate() != nil || settingsIntent(work.migration, *work.file) != work.intent {
				return false
			}
		} else if observation.journal == nil || !slices.ContainsFunc(observation.journal.Steps, func(step JournalStep) bool {
			return step.ID == work.intent.ID && step.Migration == work.intent.Migration && step.Resource == work.intent.Resource &&
				step.Decision == work.intent.Decision && step.Expected == work.intent.Expected && step.Desired == work.intent.Desired
		}) {
			// Completed CAS work may no longer have a fresh file plan. Its exact
			// durable step was already checked by protected recovery observation.
			return false
		}
	}
	return true
}

func (transition *V2Transition) localSSHPortScopeStable(intent PlannerStepIntent) bool {
	migration, known := transition.migration(intent.Migration)
	return known && migration.Kind == localSSHPortMigration && migration.Domain == "settings" &&
		migration.FromEpoch == 2 && migration.ToEpoch == 3 && transition.catalog.Supports(localSSHPortMigration, "settings") &&
		domain.SafeName(intent.Resource) && intent.ID == migration.ID+"."+intent.Resource &&
		intent.Decision == DecisionCanonicalize && validFingerprint(intent.Expected) && validFingerprint(intent.Desired) &&
		intent.Expected != intent.Desired
}

func (transition *V2Transition) prepareSourceStableActivationScope() []func() {
	var restore []func()
	for _, reconciler := range transition.options.Reconcilers {
		if scoped, ok := reconciler.(V2SourceStableActivationReconciler); ok {
			if undo := scoped.UseSourceStableActivationScope(); undo != nil {
				restore = append(restore, undo)
			}
		}
	}
	return restore
}

func (transition *V2Transition) observeActivationScope(ctx context.Context, observation *v2Observation) error {
	stable := transition.sourceWorkStable(*observation)
	journal := observation.journal
	resume := journal != nil && (journal.Checkpoint != JournalComplete ||
		(transition.options.MigrationCheckpoint && observation.migrationLedgerSnapshot.RequiresCheckpointPublication()))
	if stable && !resume {
		// Include every affected target in the original assessment and grant.
		transition.prepareSourceStableActivationScope()
	}
	base := *observation
	if err := transition.observeActivation(ctx, observation); err != nil {
		return err
	}
	checkpointSource := observation.ledgerSnapshot.Fingerprint
	if journal != nil {
		baseScope, scopeErr := transition.bindObservationScope(observation.activationScope)
		oldScope := scopeErr == nil && journal.ObservationScope == baseScope
		// Bound activation-scope replay does not make a completed unmarked
		// journal authorize checkpoint conversion against its old ledger.
		if journal.Checkpoint != JournalComplete || !oldScope {
			for _, step := range journal.Steps {
				if strings.HasPrefix(step.Resource, "ledger.") {
					checkpointSource = step.Expected
					break
				}
			}
		}
	}
	if observation.checkpointAuthority {
		checkpointSource = ""
		hasLedgerWork := false
		hasCurrentLedgerWork := false
		for _, work := range observation.work {
			if work.kind == v2LedgerWork {
				hasLedgerWork = true
				hasCurrentLedgerWork = true
			}
		}
		// Completed ledger steps retain their historical readiness scope, but
		// cannot become ledger work in a fresh activation-only repair. That
		// repair must bind the checkpoint its zero-step journal will resume against.
		if journal != nil && (journal.Checkpoint != JournalComplete || observation.activationFixed) {
			for _, step := range journal.Steps {
				hasLedgerWork = hasLedgerWork || strings.HasPrefix(step.Resource, "ledger.")
			}
		}
		if !hasLedgerWork {
			checkpointSource = observation.checkpointSnapshot.Fingerprint
		} else if transition.options.MigrationCheckpoint && journal != nil &&
			journal.Checkpoint == JournalComplete &&
			!hasCurrentLedgerWork &&
			ledgerCheckpointHasSuffix(observation.migrationLedgerSnapshot.Checkpoint) {
			// A fresh compaction journal following a completed legacy transaction
			// binds the unchanged checkpoint authority. A journal whose marked
			// scope was created for its own ledger work keeps that scope stable.
			markedWithoutSource, scopeErr := transition.bindObservationScopeWithCheckpoint(
				observation.activationScope, true,
			)
			if scopeErr != nil {
				return scopeErr
			}
			if journal.ObservationScope != markedWithoutSource {
				checkpointSource = observation.checkpointSnapshot.Fingerprint
			}
		}
	}
	scope, consent, err := transition.currentObservationScope(
		observation.activationScope, journal,
		transition.options.MigrationCheckpoint,
		checkpointSource,
	)
	if err != nil {
		return err
	}
	observation.observationScope = scope
	observation.checkpointConsent = consent
	if err := transition.prepareSourceActivationScope(ctx, observation, checkpointSource); err != nil {
		return err
	}
	scope = observation.observationScope
	if !stable || !resume || journal.ObservationScope == scope {
		return nil
	}
	// Old journals can authorize selected-yard scope even with ledger-only
	// steps. Try the release-wide scope only when the recorded binding requires
	// it; neither an old grant nor new desired inputs may expand authorization.
	restore := transition.prepareSourceStableActivationScope()
	if len(restore) == 0 {
		return nil
	}
	alternate := base
	alternate.decisions = slices.Clone(base.decisions)
	alternate.blockers = slices.Clone(base.blockers)
	alternate.observations = slices.Clone(base.observations)
	err = transition.observeActivation(ctx, &alternate)
	if err == nil {
		alternate.observationScope, alternate.checkpointConsent, err = transition.currentObservationScope(
			alternate.activationScope, journal,
			transition.options.MigrationCheckpoint,
			checkpointSource,
		)
	}
	if err == nil && alternate.observationScope == journal.ObservationScope {
		*observation = alternate
		return nil
	}
	for index := len(restore) - 1; index >= 0; index-- {
		restore[index]()
	}
	return nil
}

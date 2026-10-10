package releasetransition

import (
	"context"
	"slices"
	"strings"
)

// V2SourceStableActivationReconciler can assess release-wide activation when
// the prepared source work changes only the ledger, not configuration or
// registration. The returned function restores its historical scope.
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
			if !strings.HasPrefix(step.Resource, "ledger.") {
				return false
			}
		}
	}
	for _, work := range observation.work {
		if work.kind != v2LedgerWork {
			return false
		}
	}
	return true
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
	resume := journal != nil && journal.Checkpoint != JournalComplete
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
		if resume || !oldScope {
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
		if journal != nil {
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

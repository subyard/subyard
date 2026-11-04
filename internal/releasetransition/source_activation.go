package releasetransition

import (
	"context"
	"slices"
)

// V2SourceActivationView contains the post-settings view and the exact prepared
// owner before-state. Owners project their own registration/backend changes;
// these facts grant no permission and never replace native observations.
type V2SourceActivationView struct {
	Settings          V2SettingsSnapshotView
	OwnerRegistration *OwnerRegistrationObservation
	// These are already journal-bound pending work identities, not drift grants.
	RegisteredYards map[string]Fingerprint
	GuestDiskTmp    map[string]Fingerprint
}

type V2SourceActivationPlan struct {
	Desired      Fingerprint
	Consequences []string
}

// V2SourceActivationPlanner assesses the desired activation after prepared source
// work, without target writes. Native Actual, Converged and mutation guards remain
// authoritative. Planning is repeated from bound source facts after restart.
type V2SourceActivationPlanner interface {
	PrepareSourceActivation(context.Context, ReleasePair, ReleaseLinks, V2SourceActivationView) (V2SourceActivationPlan, error)
}

func (transition *V2Transition) retainSourceActivationView(observation *v2Observation, settings V2SettingsSnapshotView) {
	if !pendingSourceActivationWork(*observation) {
		return
	}
	if settings == nil {
		settings = posixV2SettingsSnapshotView{configHome: transition.options.ConfigHome}
	}
	view := &V2SourceActivationView{Settings: settings, RegisteredYards: map[string]Fingerprint{}, GuestDiskTmp: map[string]Fingerprint{}}
	for _, work := range observation.work {
		verified := observation.journal != nil && slices.ContainsFunc(observation.journal.Steps, func(step JournalStep) bool { return step.ID == work.intent.ID && step.Checkpoint == StepVerified })
		if work.kind() == v2NativeWork && nativeMigration(work.migration) && !verified {
			desired := view.RegisteredYards
			if work.migration.Kind == guestDiskTmpMigration {
				desired = view.GuestDiskTmp
			}
			if before, exists := desired[work.intent.Resource]; exists && before != work.intent.Desired {
				desired[work.intent.Resource] = "" // Ambiguous work supplies no proof.
			} else {
				desired[work.intent.Resource] = work.intent.Desired
			}
		}
		if work.ownerPlan() != nil {
			before := work.ownerPlan().Before
			view.OwnerRegistration = &before
		}
	}
	observation.sourceActivationView = view
}

func pendingSourceActivationWork(observation v2Observation) bool {
	journal := observation.journal
	if journal != nil && journal.Checkpoint == JournalComplete {
		if journal.Goal == observation.goal {
			return false
		}
		// Foreign completed work is history, not verification of this fresh plan.
		journal = nil
	}
	for _, work := range observation.work {
		if work.kind() != v2SettingsWork && work.kind() != v2OwnerWork && work.kind() != v2IngressWork && work.kind() != v2NativeWork {
			continue
		}
		verified := false
		if journal != nil {
			for _, step := range journal.Steps {
				if step.ID == work.intent.ID {
					verified = step.Checkpoint == StepVerified
					break
				}
			}
		}
		if !verified {
			return true
		}
	}
	return false
}

func (transition *V2Transition) prepareSourceActivationScope(ctx context.Context, observation *v2Observation, checkpointSource Fingerprint) error {
	if observation.sourceActivationView == nil || len(observation.blockers) != 0 {
		return nil
	}
	journal := observation.journal
	if journal != nil && journal.Checkpoint == JournalComplete && journal.Goal != observation.goal {
		// A new goal binds its own prospective scope; the old grant stays history.
		journal = nil
	}
	activation := slices.Clone(observation.activationScope)
	var consequences []string
	prepared := false
	for index, reconciler := range transition.options.Reconcilers {
		planner, ok := reconciler.(V2SourceActivationPlanner)
		if !ok {
			continue
		}
		plan, err := planner.PrepareSourceActivation(ctx, transition.options.Releases, observation.links, *observation.sourceActivationView)
		if err != nil {
			return err
		}
		if err := validateFingerprint(plan.Desired, "prospective activation desired fingerprint"); err != nil {
			return err
		}
		if len(plan.Consequences) > MaxPlanItems {
			return invalid("prospective activation consequences exceed their bound")
		}
		for _, consequence := range plan.Consequences {
			if err := validateText(consequence, "prospective activation consequence", maxDiagnosticText, true); err != nil {
				return err
			}
		}
		activation[index].Desired = plan.Desired
		consequences = append(consequences, plan.Consequences...)
		prepared = true
	}
	if !prepared {
		return nil
	}
	scope, consent, err := transition.currentObservationScope(activation, journal, transition.options.MigrationCheckpoint, checkpointSource)
	if err != nil {
		return err
	}
	if journal != nil && scope != journal.ObservationScope {
		// New desired inputs cannot reconstruct or expand an old grant.
		return nil
	}
	observation.activationScope = activation
	observation.observationScope, observation.checkpointConsent = scope, consent
	observation.activationConsequences = append(observation.activationConsequences, consequences...)
	observation.sourceActivationPrepared = true
	observation.sourceActivationCheckpointSource = checkpointSource
	return nil
}

func sourceActivationStaleOutcome(links ReleaseLinks, journal JournalRecord) Outcome {
	return v2OperatorOutcome(links, journal.Goal.Target, transactionIDPointer(journal.Transaction),
		CodePlanStale, "native activation desired state differs from the prepared source plan", "run yard update --check")
}

func (transition *V2Transition) verifyPreparedSourceActivation(ctx context.Context, prepared v2Observation, journal JournalRecord) (Outcome, error) {
	native := v2Observation{links: prepared.links}
	if err := transition.observeActivation(ctx, &native); err != nil {
		return Outcome{}, err
	}
	if len(native.blockers) != 0 {
		outcome, err := transition.reducePostMutationFailure(ctx, journal, native.activationObservationFailure)
		return withActivationWarnings(outcome, native.activationWarnings), err
	}
	if !slices.Equal(native.activationScope, prepared.activationScope) {
		return withActivationWarnings(sourceActivationStaleOutcome(prepared.links, journal), native.activationWarnings), nil
	}
	scope, _, err := transition.currentObservationScope(native.activationScope, &journal, transition.options.MigrationCheckpoint, prepared.sourceActivationCheckpointSource)
	if err != nil {
		return Outcome{}, err
	}
	if scope != journal.ObservationScope {
		return withActivationWarnings(sourceActivationStaleOutcome(prepared.links, journal), native.activationWarnings), nil
	}
	return Outcome{}, nil
}

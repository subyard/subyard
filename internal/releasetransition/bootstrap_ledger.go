package releasetransition

import (
	"bytes"
	"context"
)

const baselineLedgerStep = "initialize-ledger-v2.initialize"
const baselineLedgerMigration = "initialize-ledger-v2"

// This is a ledger publication, not a retired registry migration. Its exact
// intent fits the existing V2 journal/evidence grammar, including older codecs.
func baselineLedgerIntent(registry RegistryV2) (PlannerStepIntent, []byte, error) {
	payload, digest, err := MarshalLedgerV2(BaselineLedgerV2(registry), registry)
	return PlannerStepIntent{ID: baselineLedgerStep, Migration: baselineLedgerMigration, Resource: "ledger.bootstrap", Decision: DecisionTransform, Expected: absentProtectedSnapshot().Fingerprint, Desired: digest}, payload, err
}

func bootstrapReleasePair(pair ReleasePair) bool {
	return pair.From == pair.Target && pair.Previous == nil
}

// Only the initial self-activation can authorize baseline publication. A prior
// journal, ledger/checkpoint or active predecessor is never reclassified fresh.
func (transition *V2Transition) initialLedgerBootstrap(observation v2Observation) bool {
	return len(transition.registry.RetiredPrefixes) != 0 && len(transition.registry.Migrations) == 0 &&
		observation.goal.Direction == DirectionActivateTarget && transition.options.Direction == DirectionActivateTarget && transition.options.SourceIngress == nil &&
		observation.replacement == nil && transition.options.Replacement == nil &&
		bootstrapReleasePair(transition.options.Releases) && observation.links.Active == observation.goal.Target && observation.links.Previous == nil &&
		observation.journal == nil && !observation.journalSnapshot.Exists &&
		!observation.migrationLedgerSnapshot.AuthoritySnapshot.Exists && !observation.ledgerSnapshot.Exists && !observation.checkpointSnapshot.Exists
}

func (transition *V2Transition) appendBaselineWork(observation *v2Observation, step *JournalStep) error {
	intent, payload, err := baselineLedgerIntent(transition.registry)
	if err != nil {
		return err
	}
	if step != nil {
		recorded := PlannerStepIntent{ID: step.ID, Migration: step.Migration, Resource: step.Resource, Decision: step.Decision, Expected: step.Expected, Desired: step.Desired}
		if recorded != intent || !bootstrapReleasePair(observation.journal.Releases) || observation.journal.Goal.Direction != DirectionActivateTarget || observation.journal.SourceIngress != nil {
			return invalid("baseline ledger step differs from initial bootstrap intent")
		}
	}
	actual := observation.migrationLedgerSnapshot.ProjectionSnapshot
	if actual.Fingerprint != intent.Expected && actual.Fingerprint != intent.Desired {
		return invalid("bootstrap ledger changed outside its authorized step")
	}
	observation.intents = append(observation.intents, intent)
	observation.work = append(observation.work, v2Work{intent: intent, baseline: payload})
	observation.decisions = append(observation.decisions, RedactedDecision{Resource: intent.Resource, Scope: "migration-history", Decision: intent.Decision, Result: "initialize-ledger"})
	observation.observations = append(observation.observations, ResourceObservation{Resource: intent.Resource, Class: "ledger-baseline-v2", Fingerprint: actual.Fingerprint})
	return nil
}

func (transition *V2Transition) validateBaselineWork(work v2Work, journal JournalRecord, expected PlannerStepIntent) error {
	intent, payload, err := baselineLedgerIntent(transition.registry)
	if err != nil {
		return err
	}
	if len(transition.registry.RetiredPrefixes) == 0 || len(transition.registry.Migrations) != 0 || expected != intent || !bytes.Equal(payload, work.baseline) || work.intent != intent || !bootstrapReleasePair(transition.options.Releases) {
		return invalid("baseline ledger recipe differs from its canonical intent")
	}
	if journal.Transaction != "" && (!bootstrapReleasePair(journal.Releases) || journal.Goal.Direction != DirectionActivateTarget || journal.SourceIngress != nil) {
		return invalid("baseline ledger recipe has a prior release")
	}
	return nil
}

func (transition *V2Transition) applyBaselineLedger(ctx context.Context, work v2Work) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	checkpoint, err := transition.store.ReadMigrationCheckpoint()
	if err != nil {
		return err
	}
	if checkpoint.Exists {
		return invalid("baseline ledger cannot overwrite checkpoint authority")
	}
	if err := transition.inject("before-baseline-ledger"); err != nil {
		return err
	}
	if err := transition.store.CompareAndSwapLedger(absentProtectedSnapshot(), work.baseline); err != nil {
		return err
	}
	return transition.inject("after-baseline-ledger")
}

// Terminal evidence is reader data; it never authorizes another baseline write.
func validateTerminalBaseline(journal JournalRecord, step JournalStep, registry RegistryV2) error {
	intent, _, err := baselineLedgerIntent(registry)
	if err != nil {
		return err
	}
	got := PlannerStepIntent{ID: step.ID, Migration: step.Migration, Resource: step.Resource, Decision: step.Decision, Expected: step.Expected, Desired: step.Desired}
	if got != intent || step.Checkpoint != StepVerified || step.Evidence == nil || step.Evidence.Recovery != "" || !bootstrapReleasePair(journal.Releases) || journal.SourceIngress != nil || (journal.Goal != (Goal{}) && journal.Goal != (Goal{Target: journal.Releases.Target, Direction: DirectionActivateTarget})) {
		return invalid("terminal baseline ledger evidence differs from initial bootstrap")
	}
	return nil
}

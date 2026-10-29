package releasetransition

import (
	"slices"

	"github.com/Subyard/Subyard/internal/migrationhistory"
)

// runtimeReadyProof is minted from checked migration completion, exact active
// links and activation fixed-point observation. Journaled completion also binds
// validated durable journal facts. It belongs to this transition and transaction,
// never the wire DTO.
type runtimeReadyProof struct {
	owner                          *V2Transition
	transaction                    TransactionID
	registryDigest, artifactDigest Fingerprint
	journalFingerprint             Fingerprint
	registry                       migrationhistory.Registry
	completion                     migrationhistory.Completion
	outcome                        Outcome
}

func (proof runtimeReadyProof) validateRuntimeReadyProof(transition *V2Transition, transaction TransactionID) error {
	if proof.owner == nil || proof.owner != transition || proof.transaction != transaction || proof.registryDigest != transition.registryDigest || proof.artifactDigest != transition.options.ArtifactDigest || proof.outcome.Status != StatusReady || !proof.outcome.ReachedGoal {
		return invalid("zero, foreign or stale runtime readiness proof")
	}
	if err := proof.registry.CheckCompletion(proof.completion); err != nil {
		return invalid("runtime readiness has no validated migration completion: %v", err)
	}
	return nil
}
func cloneReadyOutcome(outcome Outcome) Outcome {
	outcome.Warnings = slices.Clone(outcome.Warnings)
	outcome.Previous = cloneReleaseID(outcome.Previous)
	if outcome.Transaction != nil {
		transaction := *outcome.Transaction
		outcome.Transaction = &transaction
	}
	return outcome
}

// A no-op assessment has no transaction to clean up. It still crosses the
// runtime-readiness consumer after validated history and activation observation.
func (transition *V2Transition) observedReadyProof(observation v2Observation) (runtimeReadyProof, error) {
	if transition == nil {
		return runtimeReadyProof{}, invalid("readiness observation has no transition owner")
	}
	if len(observation.blockers) != 0 || !transition.fixedPoint(observation) {
		return runtimeReadyProof{}, invalid("observed release is not at its runtime fixed point")
	}
	owner, authority, err := transition.registry.historyAuthority(observation.ledger)
	if err != nil {
		return runtimeReadyProof{}, err
	}
	completion, err := owner.Complete(authority)
	if err != nil {
		return runtimeReadyProof{}, invalid("%v", err)
	}
	outcome := withActivationWarnings(readyOutcome(Outcome{Active: observation.links.Active, Previous: cloneReleaseID(observation.links.Previous), Target: observation.goal.Target}), observation.activationWarnings)
	return runtimeReadyProof{owner: transition, registryDigest: transition.registryDigest, artifactDigest: transition.options.ArtifactDigest, registry: owner, completion: completion, outcome: cloneReadyOutcome(outcome)}, nil
}

// Completed history needs the same readiness proof plus its exact journal
// binding. The caller observes under the transition lock before cleanup.
func (transition *V2Transition) observedJournalReadyProof(observation v2Observation) (runtimeReadyProof, error) {
	if observation.checkpointConversion || !transition.completedJournalMatches(observation) {
		return runtimeReadyProof{}, invalid("readiness observation does not match completed journal history")
	}
	proof, err := transition.observedReadyProof(observation)
	if err != nil {
		return runtimeReadyProof{}, err
	}
	payload, err := MarshalJournal(*observation.journal)
	if err != nil {
		return runtimeReadyProof{}, err
	}
	proof.transaction = observation.journal.Transaction
	proof.journalFingerprint = fingerprintPayload(payload)
	proof.outcome = cloneReadyOutcome(transition.inspectionOutcome(observation))
	if err := proof.validateJournal(transition, *observation.journal); err != nil {
		return runtimeReadyProof{}, err
	}
	return proof, nil
}

func (transition *V2Transition) readyResult(proof runtimeReadyProof) (Outcome, error) {
	if err := proof.validateRuntimeReadyProof(transition, proof.transaction); err != nil {
		return Outcome{}, err
	}
	return cloneReadyOutcome(proof.outcome), nil
}

func (proof runtimeReadyProof) validateJournal(transition *V2Transition, journal JournalRecord) error {
	if err := proof.validateRuntimeReadyProof(transition, journal.Transaction); err != nil {
		return err
	}
	payload, err := MarshalJournal(journal)
	if err != nil {
		return err
	}
	if proof.journalFingerprint == "" || proof.journalFingerprint != fingerprintPayload(payload) {
		return invalid("runtime readiness proof belongs to different journal facts")
	}
	return nil
}

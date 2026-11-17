package releasetransition

import (
	"bytes"
	"encoding/json"
	"slices"
)

const rollbackNativeCheckpointStep = "rollback-native-checkpoint-v1"

// Finalization changes the checkpoint's native reader shape, never its source
// ledger cut. In particular, an S-only domain already at baseline needs no
// reverse edge but must still disappear before R reads its own checkpoint.
func nativeRollbackCheckpointPayload(expected MigrationLedgerSnapshot, source RegistryV2, sourceDigest Fingerprint, target RegistryV2, targetDigest Fingerprint) ([]byte, error) {
	if !expected.Checkpointed {
		return nil, invalid("native rollback finalization requires a checkpoint")
	}
	parsed, err := ParseMigrationLedger(expected.ProjectionSnapshot, expected.AuthoritySnapshot, source, sourceDigest)
	if err != nil {
		return nil, err
	}
	if !sameLedgerV2(parsed.Ledger, expected.Ledger) {
		return nil, invalid("native rollback input differs from its authoritative cut")
	}
	payload, err := rollbackCheckpointForLedger(parsed, parsed.Ledger, source, sourceDigest, target, targetDigest)
	if err != nil {
		return nil, err
	}
	result := protectedSnapshotFromPayload(payload)
	native, err := ParseMigrationLedger(parsed.ProjectionSnapshot, result, target, targetDigest)
	if err != nil {
		return nil, err
	}
	pending, err := target.PendingPath(native.Ledger)
	if err != nil || len(pending) != 0 {
		return nil, invalid("native rollback finalization has pending target migrations")
	}
	retained, err := ParseMigrationLedger(parsed.ProjectionSnapshot, result, source, sourceDigest)
	if err != nil {
		return nil, err
	}
	if !sameLedgerV2(parsed.Ledger, retained.Ledger) {
		return nil, invalid("native rollback finalization changes the source ledger cut")
	}
	return payload, nil
}

// Uses the same proof consumer and CAS/fsync path as checked edge rewind.
// Publication is admitted only after all earlier work is durably verified.
func (store *POSIXV2Store) prepareNativeRollbackCheckpoint(expected MigrationLedgerSnapshot, source RegistryV2, sourceDigest Fingerprint, target RegistryV2, targetDigest Fingerprint, selected ProtectedSnapshot) (checkpointRewindProof, error) {
	if store == nil || !validProtectedSnapshot(selected) || !selected.Exists {
		return checkpointRewindProof{}, invalid("native rollback finalization has no selected journal")
	}
	journal, err := ParseJournal(selected.Payload)
	if err != nil {
		return checkpointRewindProof{}, err
	}
	if journal.SourceIngress != nil || IsRecoveryTransaction(journal.Transaction) || journal.Checkpoint != JournalMigrating || journal.Goal.Direction != DirectionActivatePrevious || journal.RegistryDigest != sourceDigest {
		return checkpointRewindProof{}, invalid("native rollback finalization is not owned by S's migrating journal")
	}
	payload, err := nativeRollbackCheckpointPayload(expected, source, sourceDigest, target, targetDigest)
	if err != nil {
		return checkpointRewindProof{}, err
	}
	assessed, found := false, false
	for _, step := range journal.Steps {
		if step.ID == rollbackNativeCheckpointStep {
			if !assessed || step.Migration != rollbackNativeCheckpointStep || step.Resource != "ledger.native-target" || step.Decision != DecisionTransform || step.Checkpoint != StepEvidence || step.Evidence == nil || step.Evidence.Recovery != "" || step.Desired != fingerprintPayload(payload) {
				return checkpointRewindProof{}, invalid("native rollback checkpoint row does not bind verified work and target history")
			}
			if step.Expected != expected.AuthoritySnapshot.Fingerprint && !bytes.Equal(expected.AuthoritySnapshot.Payload, payload) {
				return checkpointRewindProof{}, ErrProtectedStoreStale
			}
			if err := store.checkRewindStepEvidence(journal, step); err != nil {
				return checkpointRewindProof{}, err
			}
			found = true
			break
		}
		if step.Checkpoint != StepVerified {
			return checkpointRewindProof{}, invalid("native rollback checkpoint has unfinished predecessor work")
		}
		if err := store.checkRewindStepEvidence(journal, step); err != nil {
			return checkpointRewindProof{}, err
		}
		if step.ID == RollbackTargetActivationStep && step.Migration == RollbackTargetActivationStep && step.Resource == "activation.rollback-target-plan" && step.Decision == DecisionPreserve && step.Expected == step.Desired {
			assessed = true
		}
	}
	if !found {
		return checkpointRewindProof{}, invalid("native rollback checkpoint row is missing")
	}
	current, err := store.ReadCurrentJournal()
	if err != nil {
		return checkpointRewindProof{}, err
	}
	if !sameProtectedSnapshot(current, selected) {
		return checkpointRewindProof{}, ErrProtectedStoreStale
	}
	sourcePayload, err := json.Marshal(source)
	if err != nil {
		return checkpointRewindProof{}, err
	}
	selected.Payload = slices.Clone(selected.Payload)
	authority, projection := expected.AuthoritySnapshot, expected.ProjectionSnapshot
	authority.Payload, projection.Payload = slices.Clone(authority.Payload), slices.Clone(projection.Payload)
	return checkpointRewindProof{store: store, journal: selected, authority: authority, projection: projection, payload: payload, sourcePayload: sourcePayload, sourceDigest: sourceDigest}, nil
}

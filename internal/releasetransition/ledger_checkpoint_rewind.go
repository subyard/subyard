package releasetransition

import (
	"bytes"
	"encoding/json"
	"slices"

	"github.com/Subyard/Subyard/internal/migrationhistory"
)

// checkpointRewindProof binds verified reverse work to one journal and exact
// history cut. Only the owning transition may request this storage operation.
// It carries no authorization for resource effects or runtime activation.
type checkpointRewindProof struct {
	store                 *POSIXV2Store
	journal               ProtectedSnapshot
	authority, projection ProtectedSnapshot
	payload               []byte
	sourcePayload         []byte
	sourceDigest          Fingerprint
}

func rewindCheckpointPayload(expected MigrationLedgerSnapshot, source RegistryV2, sourceDigest Fingerprint, target RegistryV2, targetDigest Fingerprint, edge migrationhistory.ReverseEdge) ([]byte, error) {
	if !expected.Checkpointed {
		return nil, invalid("migration rewind requires an existing checkpoint")
	}
	parsed, err := ParseMigrationLedger(expected.ProjectionSnapshot, expected.AuthoritySnapshot, source, sourceDigest)
	if err != nil {
		return nil, err
	}
	if !parsed.Checkpointed || !sameLedgerV2(parsed.Ledger, expected.Ledger) {
		return nil, invalid("rewind input is not the authenticated checkpoint view")
	}
	expected = parsed
	owner, err := source.historyRegistry()
	if err != nil {
		return nil, err
	}
	if err := owner.CheckReverseEdgeBinding(edge); err != nil {
		return nil, invalid("%v", err)
	}
	before, after := ledgerFromAuthority(edge.Before()), ledgerFromAuthority(edge.After())
	if !sameLedgerV2(expected.Ledger, before) && !sameLedgerV2(expected.Ledger, after) {
		return nil, ErrProtectedStoreStale
	}
	if err := expected.Checkpoint.Validate(source, sourceDigest, expected.ProjectionSnapshot); err != nil {
		return nil, err
	}
	return rollbackCheckpointForLedger(expected, after, source, sourceDigest, target, targetDigest)
}

// The caller supplies either a checked reverse edge's result or the unchanged
// authenticated source ledger. This helper itself grants no storage authority.
func rollbackCheckpointForLedger(expected MigrationLedgerSnapshot, after LedgerV2, source RegistryV2, sourceDigest Fingerprint, target RegistryV2, targetDigest Fingerprint) ([]byte, error) {
	checkpoint, err := newLedgerCheckpointV1(after, source, sourceDigest, expected.ProjectionSnapshot, expected.Checkpoint.LegacyProjection.Epochs)
	if err != nil {
		return nil, err
	}
	for name, state := range after.Domains {
		if _, exists := target.MinimumEpochs[name]; !exists {
			if state.Epoch != 1 || source.MinimumEpochs[name] != 1 || len(state.Applied) != 0 || source.RetiredPrefixes[name].Digest != "" {
				return nil, invalid("rewind cannot omit applied or retired domain %q", name)
			}
			delete(checkpoint.Domains, name)
			continue
		}
		sourcePrefix, err := source.MigrationPrefixDigest(name, state.Epoch)
		if err != nil {
			return nil, err
		}
		targetPrefix, err := target.MigrationPrefixDigest(name, state.Epoch)
		if err != nil || sourcePrefix != targetPrefix {
			return nil, invalid("rewind target has a different migration prefix for domain %q", name)
		}
	}
	// Both readers must accept the exact same checkpoint and frozen projection.
	// In particular, this rejects a target below the projection's epoch floor.
	targetView, err := checkpoint.LedgerView(target)
	if err != nil {
		return nil, err
	}
	if targetDigest != sourceDigest {
		pending, err := target.PendingPath(targetView)
		if err != nil || len(pending) != 0 {
			return nil, invalid("rewind does not complete the target's native migration history")
		}
	}
	payload, _, err := MarshalLedgerCheckpointV1(checkpoint, target, targetDigest, expected.ProjectionSnapshot)
	if err != nil {
		return nil, err
	}
	if _, _, err := ParseLedgerCheckpointV1(payload, source, sourceDigest, expected.ProjectionSnapshot); err != nil {
		return nil, err
	}
	return payload, nil
}

// prepareCheckpointRewind is called after reverse resource verification and
// before the ordinary ledger row advances from captured to applied evidence.
// The journal still owns the operation; no second history selector is created.
func (store *POSIXV2Store) prepareCheckpointRewind(expected MigrationLedgerSnapshot, source RegistryV2, sourceDigest Fingerprint, target RegistryV2, targetDigest Fingerprint, edge migrationhistory.ReverseEdge, journalSnapshot ProtectedSnapshot, ledgerStepID string) (checkpointRewindProof, error) {
	if store == nil || !validProtectedSnapshot(journalSnapshot) || !journalSnapshot.Exists {
		return checkpointRewindProof{}, invalid("rewind has no selected journal")
	}
	journal, err := ParseJournal(journalSnapshot.Payload)
	if err != nil {
		return checkpointRewindProof{}, err
	}
	if journal.Goal.Direction != DirectionActivatePrevious || journal.Checkpoint != JournalMigrating || journal.RegistryDigest != sourceDigest {
		return checkpointRewindProof{}, invalid("rewind is not owned by the migrating rollback journal")
	}
	migration := edge.Migration()
	_, beforePin, err := MarshalLedgerV2(ledgerFromAuthority(edge.Before()), source)
	if err != nil {
		return checkpointRewindProof{}, err
	}
	_, afterPin, err := MarshalLedgerV2(ledgerFromAuthority(edge.After()), source)
	if err != nil {
		return checkpointRewindProof{}, err
	}
	verifiedResource := false
	found := false
	for _, step := range journal.Steps {
		if step.ID == ledgerStepID {
			if step.ID != migration.ID+".rewind-ledger" || step.Migration != migration.ID || step.Resource != "ledger."+migration.Domain || step.Decision != DecisionTransform || step.Checkpoint != StepEvidence || step.Expected != beforePin || step.Desired != afterPin || !verifiedResource || step.Evidence == nil || step.Evidence.Recovery != "" {
				return checkpointRewindProof{}, invalid("rewind ledger row does not bind verified reverse work")
			}
			if err := store.checkRewindStepEvidence(journal, step); err != nil {
				return checkpointRewindProof{}, err
			}
			found = true
			break
		}
		if step.Checkpoint != StepVerified {
			return checkpointRewindProof{}, invalid("rewind has unfinished predecessor work")
		}
		if err := store.checkRewindStepEvidence(journal, step); err != nil {
			return checkpointRewindProof{}, err
		}
		if step.Migration == migration.ID && step.Resource != "ledger."+migration.Domain {
			verifiedResource = true
		}
	}
	if !found {
		return checkpointRewindProof{}, invalid("rewind ledger row is missing")
	}
	payload, err := rewindCheckpointPayload(expected, source, sourceDigest, target, targetDigest, edge)
	if err != nil {
		return checkpointRewindProof{}, err
	}
	current, err := store.ReadCurrentJournal()
	if err != nil {
		return checkpointRewindProof{}, err
	}
	if !sameProtectedSnapshot(current, journalSnapshot) {
		return checkpointRewindProof{}, ErrProtectedStoreStale
	}
	sourcePayload, err := json.Marshal(source)
	if err != nil {
		return checkpointRewindProof{}, err
	}
	journalSnapshot.Payload = slices.Clone(journalSnapshot.Payload)
	authority, projection := expected.AuthoritySnapshot, expected.ProjectionSnapshot
	authority.Payload, projection.Payload = slices.Clone(authority.Payload), slices.Clone(projection.Payload)
	return checkpointRewindProof{store: store, journal: journalSnapshot, authority: authority, projection: projection, payload: payload, sourcePayload: sourcePayload, sourceDigest: sourceDigest}, nil
}

func (store *POSIXV2Store) checkRewindStepEvidence(journal JournalRecord, step JournalStep) error {
	if err := validateStepEvidence(journal, step); err != nil {
		return err
	}
	phases := []EvidenceCheckpoint{EvidenceCaptured}
	if step.Checkpoint == StepVerified {
		phases = append(phases, EvidenceApplied, EvidenceVerified)
	}
	for _, phase := range phases {
		snapshot, err := store.ReadCheckpointEvidence(journal.Transaction, step.ID, phase)
		if err != nil {
			return err
		}
		if !snapshot.Exists {
			return invalid("rewind verification evidence is missing")
		}
		evidence, err := ParseEvidence(snapshot.Payload)
		if err != nil {
			return err
		}
		canonical, err := MarshalEvidence(evidence)
		if err != nil {
			return err
		}
		redacted := evidence
		redacted.Recovery = ""
		if !bytes.Equal(canonical, snapshot.Payload) || !postActivationEvidenceMatches(journal, step, phase, redacted) || evidence.Recovery != step.Evidence.Recovery {
			return invalid("rewind verification evidence differs from its journal")
		}
		if phase == phases[len(phases)-1] && !evidenceRecordsEqual(evidence, *step.Evidence) {
			return invalid("rewind embedded evidence differs from its protected record")
		}
	}
	return nil
}

// The caller holds the existing transition lock. Exact result comparison makes
// a retry after publication safe without relaxing the ordinary forward writer.
func (store *POSIXV2Store) rewindMigrationCheckpoint(proof checkpointRewindProof) error {
	if store == nil || proof.store != store || len(proof.payload) == 0 {
		return invalid("zero or foreign checkpoint rewind proof")
	}
	journal, err := store.ReadCurrentJournal()
	if err != nil {
		return err
	}
	if !sameProtectedSnapshot(journal, proof.journal) {
		return ErrProtectedStoreStale
	}
	var source RegistryV2
	if err := decodeBoundedRecord(proof.sourcePayload, MaxRegistryV2Bytes, &source); err != nil {
		return err
	}
	current, err := store.ReadMigrationLedger(source, proof.sourceDigest)
	if err != nil {
		return err
	}
	if !sameProtectedSnapshot(current.ProjectionSnapshot, proof.projection) {
		return ErrProtectedStoreStale
	}
	if bytes.Equal(current.AuthoritySnapshot.Payload, proof.payload) {
		return nil
	}
	if !current.Checkpointed || !sameProtectedSnapshot(current.AuthoritySnapshot, proof.authority) {
		return ErrProtectedStoreStale
	}
	return store.compareAndSwap([]string{"release-transition", "v2"}, migrationLedgerCheckpointName, current.AuthoritySnapshot, proof.payload)
}

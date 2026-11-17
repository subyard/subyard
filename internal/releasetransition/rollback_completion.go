package releasetransition

import "bytes"

// CompletedRollback is read-only provenance plus checked native target history.
// It neither authorizes resumption of S nor establishes R's activation readiness.
type CompletedRollback struct {
	Journal JournalRecord
	Ledger  MigrationLedgerSnapshot
}

// InspectSealedCompletedRollback keeps the historical codec path for unmarked
// journals and requires protected closure for source-owned completion.
func (store *POSIXV2Store) InspectSealedCompletedRollback(registryPayload []byte, artifact Fingerprint) (*CompletedRollback, error) {
	snapshot, err := store.ReadCurrentJournal()
	if err != nil || !snapshot.Exists {
		return nil, err
	}
	journal, err := ParseJournal(snapshot.Payload)
	if err != nil {
		return nil, err
	}
	if journal.Checkpoint != JournalComplete || journal.Goal.Direction != DirectionActivatePrevious {
		return nil, nil
	}
	closure, err := store.readRecord(rollbackClosureParts, string(journal.Transaction)+".json")
	if err != nil {
		return nil, err
	}
	if !closure.Exists {
		return nil, checkMissingRollbackClosure(journal)
	}
	registry, digest, err := ParseRegistryV2(registryPayload, BuiltinCapabilityCatalog())
	if err != nil {
		return nil, err
	}
	return store.InspectCompletedRollback(registry, digest, artifact)
}

// InspectCompletedRollback recognizes only an exactly closed foreign rollback.
// Callers must also observe current links and activation.
func (store *POSIXV2Store) InspectCompletedRollback(registry RegistryV2, digest, artifact Fingerprint) (*CompletedRollback, error) {
	if err := registry.Validate(BuiltinCapabilityCatalog()); err != nil {
		return nil, err
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil || !snapshot.Exists {
		return nil, err
	}
	journal, err := ParseJournal(snapshot.Payload)
	if err != nil {
		return nil, err
	}
	if journal.Checkpoint != JournalComplete || journal.Goal.Direction != DirectionActivatePrevious {
		return nil, nil
	}
	closure, err := store.readRollbackClosure(journal.Transaction)
	if err != nil {
		return nil, err
	}
	if closure == nil {
		return nil, checkMissingRollbackClosure(journal)
	}
	if closure.Retirement != nil || !bytes.Equal(closure.RollbackJournal, snapshot.Payload) || closure.Completion.TargetRegistry != digest || closure.Completion.TargetArtifact != artifact || closure.ActivationRequest.ConfigHome != store.configHome {
		return nil, invalid("completed rollback does not bind this target and protected home")
	}
	state, err := store.ReadMigrationLedger(registry, digest)
	if err != nil {
		return nil, err
	}
	_, history, err := MarshalLedgerV2(state.Ledger, registry)
	if err != nil {
		return nil, err
	}
	if !state.Checkpointed || state.AuthoritySnapshot.Fingerprint != closure.Completion.Checkpoint || state.ProjectionSnapshot.Fingerprint != closure.Completion.Projection || history != closure.Completion.NativeHistory {
		return nil, invalid("completed rollback differs from the target's native history")
	}
	pending, err := registry.PendingPath(state.Ledger)
	if err != nil || len(pending) != 0 {
		return nil, invalid("completed rollback has pending target migrations")
	}
	return &CompletedRollback{Journal: journal, Ledger: state}, nil
}

func checkMissingRollbackClosure(journal JournalRecord) error {
	if HasRollbackSourceOwner(journal) {
		return invalid("completed source-owned rollback has no protected closure")
	}
	return nil
}

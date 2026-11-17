package releasetransition

import (
	"bytes"
	"encoding/json"
)

// The origin is ordinary rollback-time recovery data. It binds the starting
// cut to the immutable source-plan row; it cannot select history authority.
type rollbackHistoryOrigin struct {
	SchemaVersion  int                        `json:"schemaVersion"`
	Owner          RollbackSourceOwnerBinding `json:"owner"`
	SourceRegistry []byte                     `json:"sourceRegistry"`
	TargetRegistry []byte                     `json:"targetRegistry"`
	Projection     ProtectedSnapshot          `json:"projection"`
	Checkpoint     ProtectedSnapshot          `json:"checkpoint"`
}

func (origin rollbackHistoryOrigin) inspect() (RegistryV2, RegistryV2, MigrationLedgerSnapshot, error) {
	if origin.SchemaVersion != 1 {
		return RegistryV2{}, RegistryV2{}, MigrationLedgerSnapshot{}, invalid("unsupported rollback history origin")
	}
	if _, err := origin.Owner.Fingerprint(); err != nil {
		return RegistryV2{}, RegistryV2{}, MigrationLedgerSnapshot{}, err
	}
	source, sourceDigest, err := ParseRegistryV2Provenance(origin.SourceRegistry)
	if err != nil {
		return RegistryV2{}, RegistryV2{}, MigrationLedgerSnapshot{}, err
	}
	target, targetDigest, err := ParseRegistryV2Provenance(origin.TargetRegistry)
	if err != nil {
		return RegistryV2{}, RegistryV2{}, MigrationLedgerSnapshot{}, err
	}
	if sourceDigest != origin.Owner.SourceRegistry || targetDigest != origin.Owner.TargetRegistry {
		return RegistryV2{}, RegistryV2{}, MigrationLedgerSnapshot{}, invalid("rollback origin differs from its sealed registries")
	}
	initial, err := ParseMigrationLedger(origin.Projection, origin.Checkpoint, source, sourceDigest)
	if err != nil {
		return RegistryV2{}, RegistryV2{}, MigrationLedgerSnapshot{}, err
	}
	if !initial.Checkpointed {
		return RegistryV2{}, RegistryV2{}, MigrationLedgerSnapshot{}, invalid("rollback origin requires checkpoint authority")
	}
	if _, err := rollbackTargetEpochs(source, target); err != nil {
		return RegistryV2{}, RegistryV2{}, MigrationLedgerSnapshot{}, err
	}
	return source, target, initial, nil
}

func (origin rollbackHistoryOrigin) marshal() ([]byte, error) {
	if _, _, _, err := origin.inspect(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(origin)
	if err != nil {
		return nil, err
	}
	if len(payload) > MaxProtectedRecordBytes {
		return nil, invalid("rollback origin exceeds its recovery bound")
	}
	return append(payload, '\n'), nil
}

// A cut proof admits only archival of the selected journal. It grants no
// inverse, activation, link-switch, or journal-removal authority. The abort
// controller must separately assess and verify those operations.
type rollbackAbortCut struct {
	store    *POSIXV2Store
	journal  ProtectedSnapshot
	history  MigrationLedgerSnapshot
	origin   ProtectedSnapshot
	registry RegistryV2
	digest   Fingerprint
}

func (store *POSIXV2Store) prepareRollbackAbortCut(selected ProtectedSnapshot) (rollbackAbortCut, error) {
	if store == nil || !validProtectedSnapshot(selected) || !selected.Exists {
		return rollbackAbortCut{}, invalid("rollback abort has no selected journal")
	}
	journal, err := ParseJournal(selected.Payload)
	if err != nil {
		return rollbackAbortCut{}, err
	}
	if journal.Checkpoint == JournalComplete {
		return rollbackAbortCut{}, invalid("completed rollback requires an ordinary forward update")
	}
	originSnapshot, err := store.ReadRecovery(journal.Transaction, rollbackSourcePlanStep)
	if err != nil {
		return rollbackAbortCut{}, err
	}
	if !originSnapshot.Exists {
		return rollbackAbortCut{}, invalid("rollback abort has no bound starting history")
	}
	var origin rollbackHistoryOrigin
	if err := decodeBoundedRecord(originSnapshot.Payload, MaxProtectedRecordBytes, &origin); err != nil {
		return rollbackAbortCut{}, err
	}
	canonical, err := origin.marshal()
	if err != nil {
		return rollbackAbortCut{}, err
	}
	if !bytes.Equal(canonical, originSnapshot.Payload) || origin.Owner.ConfigHome != store.configHome {
		return rollbackAbortCut{}, invalid("rollback origin is noncanonical or belongs to another root")
	}
	if err := origin.Owner.CheckJournal(journal); err != nil {
		return rollbackAbortCut{}, err
	}
	bound := false
	for _, step := range journal.Steps {
		if step.ID == rollbackSourcePlanStep {
			if step.Resource != "rollback.source-plan" || step.Expected != originSnapshot.Fingerprint || step.Desired != originSnapshot.Fingerprint || step.Checkpoint != StepVerified || step.Evidence == nil || step.Evidence.Recovery != originSnapshot.Fingerprint {
				return rollbackAbortCut{}, invalid("rollback origin differs from its verified source-plan row")
			}
			bound = true
		}
		if step.Checkpoint == StepVerified {
			if err := store.checkRewindStepEvidence(journal, step); err != nil {
				return rollbackAbortCut{}, err
			}
			if step.Evidence.Recovery != "" {
				recovery, err := store.ReadRecovery(journal.Transaction, step.ID)
				if err != nil {
					return rollbackAbortCut{}, err
				}
				if !recovery.Exists || recovery.Fingerprint != step.Evidence.Recovery {
					return rollbackAbortCut{}, invalid("rollback abort recovery differs from its verified evidence")
				}
			}
		}
	}
	if !bound {
		return rollbackAbortCut{}, invalid("rollback starting history has no immutable plan binding")
	}
	source, target, initial, err := origin.inspect()
	if err != nil {
		return rollbackAbortCut{}, err
	}
	progress, err := inspectRollbackHistoryProgress(source, target, initial.Ledger, journal)
	if err != nil {
		return rollbackAbortCut{}, err
	}
	if progress.needsResolution {
		return rollbackAbortCut{}, invalid("rollback abort must resolve the uncertain inverse through its ledger boundary")
	}
	current, err := store.ReadMigrationLedger(source, origin.Owner.SourceRegistry)
	if err != nil {
		return rollbackAbortCut{}, err
	}
	if !current.Checkpointed || !sameProtectedSnapshot(current.ProjectionSnapshot, initial.ProjectionSnapshot) || !sameLedgerV2(current.Ledger, progress.ledger) {
		return rollbackAbortCut{}, invalid("rollback abort checkpoint differs from its verified ledger cut")
	}
	after, err := store.ReadCurrentJournal()
	if err != nil {
		return rollbackAbortCut{}, err
	}
	if !sameProtectedSnapshot(selected, after) {
		return rollbackAbortCut{}, ErrProtectedStoreStale
	}
	return rollbackAbortCut{store: store, journal: selected, history: current, origin: originSnapshot, registry: source, digest: origin.Owner.SourceRegistry}, nil
}

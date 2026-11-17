package releasetransition

import (
	"bytes"
	"errors"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

type nativeCheckpointFixture struct {
	store          *POSIXV2Store
	source, target RegistryV2
	state          MigrationLedgerSnapshot
	journal        ProtectedSnapshot
}

func newNativeCheckpointFixture(t *testing.T) nativeCheckpointFixture {
	t.Helper()
	source, target := rewindRegistries()
	source.MinimumEpochs["new-domain"], source.CurrentEpochs["new-domain"] = 1, 2
	source.Migrations = append(source.Migrations, MigrationDefinitionV2{ID: "new-domain-v2", Domain: "new-domain", FromEpoch: 1, ToEpoch: 2, Kind: "future-domain-v2", DependsOn: []string{"settings-v3"}})
	projection, _, err := MarshalLedgerV2(completeRewindLedger(target), target)
	if err != nil {
		t.Fatal(err)
	}
	ledger := BaselineLedgerV2(source)
	ledger.Domains["settings"] = completeRewindLedger(target).Domains["settings"]
	checkpoint, err := newLedgerCheckpointV1(ledger, source, digestA, protectedSnapshotFromPayload(projection), target.CurrentEpochs)
	if err != nil {
		t.Fatal(err)
	}
	payload, _, err := MarshalLedgerCheckpointV1(checkpoint, source, digestA, protectedSnapshotFromPayload(projection))
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewPOSIXV2Store(testkit.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapLedger(absentProtectedSnapshot(), projection); err != nil {
		t.Fatal(err)
	}
	if err := store.createImmutable([]string{"release-transition", "v2"}, migrationLedgerCheckpointName, payload); err != nil {
		t.Fatal(err)
	}
	state, err := store.ReadMigrationLedger(source, digestA)
	if err != nil {
		t.Fatal(err)
	}
	native, err := nativeRollbackCheckpointPayload(state, source, digestA, target, digestB)
	if err != nil {
		t.Fatal(err)
	}
	journal := validJournal(ReleasePair{From: "release-s", Target: "release-r"})
	journal.Goal.Direction, journal.RegistryDigest, journal.Checkpoint = DirectionActivatePrevious, digestA, JournalMigrating
	journal.Steps = []JournalStep{
		{ID: RollbackTargetActivationStep, Migration: RollbackTargetActivationStep, Resource: "activation.rollback-target-plan", Decision: DecisionPreserve, Expected: digestC, Desired: digestC, Checkpoint: StepVerified},
		{ID: rollbackNativeCheckpointStep, Migration: rollbackNativeCheckpointStep, Resource: "ledger.native-target", Decision: DecisionTransform, Expected: state.AuthoritySnapshot.Fingerprint, Desired: fingerprintPayload(native), Checkpoint: StepEvidence},
	}
	for index := range journal.Steps {
		step := &journal.Steps[index]
		phases := []EvidenceCheckpoint{EvidenceCaptured}
		if step.Checkpoint == StepVerified {
			phases = append(phases, EvidenceApplied, EvidenceVerified)
		}
		for _, phase := range phases {
			observed := step.Desired
			if phase == EvidenceCaptured {
				observed = step.Expected
			}
			evidence := evidenceFor(journal, *step, phase, observed, "")
			payload, err := MarshalEvidence(evidence)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.CreateCheckpointEvidence(journal.Transaction, step.ID, phase, payload); err != nil {
				t.Fatal(err)
			}
			step.Evidence = &evidence
		}
	}
	journal.IntentDigest = bindJournalIntent(journal.AuthorizationPlan, journal.ResumePlan, journal.ObservationScope, journal.Steps)
	payload, err = MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapCurrentJournal(absentProtectedSnapshot(), payload); err != nil {
		t.Fatal(err)
	}
	selected, err := store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	return nativeCheckpointFixture{store, source, target, state, selected}
}

func (f nativeCheckpointFixture) proof() (checkpointRewindProof, error) {
	return f.store.prepareNativeRollbackCheckpoint(f.state, f.source, digestA, f.target, digestB, f.journal)
}

func TestNativeRollbackFinalizesUnappliedDomainsWithoutChangingEpochs(t *testing.T) {
	f := newNativeCheckpointFixture(t)
	if _, err := ParseMigrationLedger(f.state.ProjectionSnapshot, f.state.AuthoritySnapshot, f.target, digestB); err == nil {
		t.Fatal("fixture checkpoint is already native to R")
	}
	proof, err := f.proof()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.rewindMigrationCheckpoint(proof); err != nil {
		t.Fatal(err)
	}
	native, err := f.store.ReadMigrationLedger(f.target, digestB)
	if err != nil || !sameLedgerV2(native.Ledger, completeRewindLedger(f.target)) || !bytes.Equal(native.ProjectionSnapshot.Payload, f.state.ProjectionSnapshot.Payload) {
		t.Fatalf("native target history = %+v, %v", native, err)
	}
	retained, err := f.store.ReadMigrationLedger(f.source, digestA)
	if err != nil || !sameLedgerV2(retained.Ledger, f.state.Ledger) {
		t.Fatalf("finalization changed source epochs: %+v, %v", retained, err)
	}
	pending, err := f.source.PendingPath(retained.Ledger)
	if err != nil || len(pending) != 2 {
		t.Fatalf("unapplied source registrations disappeared: %+v, %v", pending, err)
	}
}

func TestNativeRollbackFinalizationCannotPerformARewind(t *testing.T) {
	source, target := rewindRegistries()
	state := rewindSnapshot(t, source, target)
	if _, err := nativeRollbackCheckpointPayload(state, source, digestA, target, digestB); err == nil {
		t.Fatal("unverified applied history was lowered by finalization")
	}
	state.Checkpointed = false
	if _, err := nativeRollbackCheckpointPayload(state, source, digestA, target, digestB); err == nil {
		t.Fatal("missing checkpoint was treated as a rollback horizon")
	}
}

func TestNativeRollbackFinalizationResumesAcrossPublicationCuts(t *testing.T) {
	for _, point := range []string{"after-pending-fsync", "after-publish-before-dir-fsync"} {
		t.Run(point, func(t *testing.T) {
			f := newNativeCheckpointFixture(t)
			proof, err := f.proof()
			if err != nil {
				t.Fatal(err)
			}
			interrupted := errors.New("interrupted native finalization")
			f.store.fault = func(at string) error {
				if at == point {
					return interrupted
				}
				return nil
			}
			if err := f.store.rewindMigrationCheckpoint(proof); !errors.Is(err, interrupted) {
				t.Fatalf("publication was not interrupted: %v", err)
			}
			f.store, err = NewPOSIXV2Store(f.store.configHome)
			if err != nil {
				t.Fatal(err)
			}
			f.state, err = f.store.ReadMigrationLedger(f.source, digestA)
			if err != nil {
				t.Fatal(err)
			}
			proof, err = f.proof()
			if err != nil {
				t.Fatal(err)
			}
			if err := f.store.rewindMigrationCheckpoint(proof); err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.ReadMigrationLedger(f.target, digestB); err != nil {
				t.Fatal(err)
			}
		})
	}
}

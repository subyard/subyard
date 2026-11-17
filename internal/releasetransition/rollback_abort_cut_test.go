package releasetransition

import (
	"encoding/json"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func abortCutFixture(t *testing.T, closed int, partial string) (*POSIXV2Store, ProtectedSnapshot) {
	t.Helper()
	source, target, initial, journal := rollbackCutFixture(t, closed, partial)
	store, err := NewPOSIXV2Store(testkit.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	sourcePayload, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	targetPayload, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	sourceDigest, targetDigest := fingerprintPayload(sourcePayload), fingerprintPayload(targetPayload)
	journal.RegistryDigest, journal.ArtifactDigest = sourceDigest, digestB
	projectionPayload, _, err := MarshalLedgerV2(completeRewindLedger(target), target)
	if err != nil {
		t.Fatal(err)
	}
	projection := protectedSnapshotFromPayload(projectionPayload)
	checkpoint, err := newLedgerCheckpointV1(initial, source, sourceDigest, projection, target.CurrentEpochs)
	if err != nil {
		t.Fatal(err)
	}
	checkpointPayload, _, err := MarshalLedgerCheckpointV1(checkpoint, source, sourceDigest, projection)
	if err != nil {
		t.Fatal(err)
	}
	owner := RollbackSourceOwnerBinding{RuntimeRoot: store.configHome + "/runtime", ConfigHome: store.configHome, Releases: journal.Releases, SourceArtifact: digestA, SourceRegistry: sourceDigest, TargetArtifact: digestB, TargetRegistry: targetDigest}
	origin := rollbackHistoryOrigin{SchemaVersion: 1, Owner: owner, SourceRegistry: sourcePayload, TargetRegistry: targetPayload, Projection: projection, Checkpoint: protectedSnapshotFromPayload(checkpointPayload)}
	originPayload, err := origin.marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRecovery(journal.Transaction, rollbackSourcePlanStep, originPayload); err != nil {
		t.Fatal(err)
	}
	originPin := fingerprintPayload(originPayload)
	ownerPin, err := owner.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	journal.Steps = append([]JournalStep{
		{ID: RollbackSourceOwnerStep, Migration: RollbackSourceOwnerStep, Resource: "rollback.source-owner", Decision: DecisionPreserve, Expected: ownerPin, Desired: ownerPin, Checkpoint: StepVerified},
		{ID: rollbackSourcePlanStep, Migration: rollbackSourcePlanStep, Resource: "rollback.source-plan", Decision: DecisionPreserve, Expected: originPin, Desired: originPin, Checkpoint: StepVerified},
	}, journal.Steps...)
	for index := range journal.Steps {
		step := &journal.Steps[index]
		if step.Checkpoint == StepIntent {
			continue
		}
		recovery := Fingerprint("")
		if step.ID == rollbackSourcePlanStep {
			recovery = originPin
		}
		phases := []EvidenceCheckpoint{EvidenceCaptured}
		if step.Checkpoint == StepApplied || step.Checkpoint == StepVerified {
			phases = append(phases, EvidenceApplied)
		}
		if step.Checkpoint == StepVerified {
			phases = append(phases, EvidenceVerified)
		}
		for _, phase := range phases {
			observed := step.Desired
			if phase == EvidenceCaptured {
				observed = step.Expected
			}
			evidence := evidenceFor(journal, *step, phase, observed, recovery)
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
	journalPayload, err := MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapCurrentJournal(absentProtectedSnapshot(), journalPayload); err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapLedger(absentProtectedSnapshot(), projectionPayload); err != nil {
		t.Fatal(err)
	}
	cut := initial
	cut.Domains["settings"] = DomainLedgerV2{Epoch: 3 - closed, Applied: nonNilIDs(source.appliedPrefix("settings", 3-closed))}
	checkpoint, err = newLedgerCheckpointV1(cut, source, sourceDigest, projection, target.CurrentEpochs)
	if err != nil {
		t.Fatal(err)
	}
	checkpointPayload, _, err = MarshalLedgerCheckpointV1(checkpoint, source, sourceDigest, projection)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.createImmutable([]string{"release-transition", "v2"}, migrationLedgerCheckpointName, checkpointPayload); err != nil {
		t.Fatal(err)
	}
	selected, err := store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	return store, selected
}

func TestRollbackAbortCutBindsDurableOriginEvidenceAndCurrentHistory(t *testing.T) {
	for closed := 0; closed <= 2; closed++ {
		store, selected := abortCutFixture(t, closed, "")
		proof, err := store.prepareRollbackAbortCut(selected)
		if err != nil {
			t.Fatal(err)
		}
		if proof.store != store || proof.history.Ledger.Domains["settings"].Epoch != 3-closed || !sameProtectedSnapshot(proof.journal, selected) {
			t.Fatalf("wrong abort cut: %+v", proof)
		}
		fresh, err := NewPOSIXV2Store(store.configHome)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fresh.prepareRollbackAbortCut(selected); err != nil {
			t.Fatalf("fresh source process: %v", err)
		}
	}
}

func TestRollbackAbortCutPreservesUncertainOrChangedHistory(t *testing.T) {
	for _, partial := range []string{"captured", "effect", "verified resource", "uncertain ledger"} {
		store, selected := abortCutFixture(t, 1, partial)
		before, err := store.ReadMigrationCheckpoint()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.prepareRollbackAbortCut(selected); err == nil {
			t.Fatalf("uncertain %s supplied an abort cut", partial)
		}
		after, err := store.ReadMigrationCheckpoint()
		if err != nil || !sameProtectedSnapshot(before, after) {
			t.Fatal("failed abort inspection changed history")
		}
	}
	for _, change := range []string{"origin", "evidence", "checkpoint", "journal"} {
		t.Run(change, func(t *testing.T) {
			store, selected := abortCutFixture(t, 1, "")
			switch change {
			case "origin":
				testkit.WriteFile(t, store.configHome+"/release-transition/v2/transactions/tx-001/recovery/"+rollbackSourcePlanStep+".json", []byte("{}\n"), 0o600)
			case "evidence":
				testkit.WriteFile(t, store.configHome+"/release-transition/v2/transactions/tx-001/evidence/settings-v3.reverse.verified.json", []byte("{}\n"), 0o600)
			case "checkpoint":
				journal, err := ParseJournal(selected.Payload)
				if err != nil {
					t.Fatal(err)
				}
				value, err := store.ReadRecovery(journal.Transaction, rollbackSourcePlanStep)
				if err != nil {
					t.Fatal(err)
				}
				var origin rollbackHistoryOrigin
				if err := json.Unmarshal(value.Payload, &origin); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, store.configHome+"/release-transition/v2/"+migrationLedgerCheckpointName, origin.Checkpoint.Payload, 0o600)
			case "journal":
				testkit.WriteFile(t, store.configHome+"/release-transition/v2/journal.json", []byte("{}\n"), 0o600)
			}
			if _, err := store.prepareRollbackAbortCut(selected); err == nil {
				t.Fatal("changed abort evidence was accepted")
			}
		})
	}
}

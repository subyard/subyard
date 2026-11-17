package releasetransition

import (
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func completedRollbackFixture(t *testing.T) (*POSIXV2Store, RegistryV2, rollbackClosureV1) {
	t.Helper()
	store, closure := rollbackClosureFixture(t)
	registry, digest, err := ParseRegistryV2(closure.TargetRegistry, BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	projection, _, err := MarshalLedgerV2(*closure.ActivationRequest.NativeHistory, registry)
	if err != nil {
		t.Fatal(err)
	}
	absent, err := store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapLedger(absent, projection); err != nil {
		t.Fatal(err)
	}
	state, err := store.ReadMigrationLedger(registry, digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConvertMigrationLedger(state, registry, digest, registry, digest); err != nil {
		t.Fatal(err)
	}
	state, err = store.ReadMigrationLedger(registry, digest)
	if err != nil {
		t.Fatal(err)
	}
	closure.ActivationRequest.Projection = state.ProjectionSnapshot.Fingerprint
	owner := &V2Transition{registry: registry}
	closure.ActivationPlan.Token, err = owner.rollbackActivationPlan(closure.ActivationRequest, closure.ActivationPlan.Assessment, closure.ActivationPlan.Scopes)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := ParseJournal(closure.RollbackJournal)
	if err != nil {
		t.Fatal(err)
	}
	step := &journal.Steps[0]
	step.Expected = fingerprintPayload([]byte(closure.ActivationPlan.Token))
	step.Desired = step.Expected
	for _, phase := range []EvidenceCheckpoint{EvidenceCaptured, EvidenceApplied, EvidenceVerified} {
		evidence := evidenceFor(journal, *step, phase, step.Desired, "")
		payload, err := MarshalEvidence(evidence)
		if err != nil {
			t.Fatal(err)
		}
		name, err := checkpointEvidenceName(journal.Transaction, step.ID, phase)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.createImmutable([]string{"release-transition", "v2", "transactions", string(journal.Transaction), "evidence"}, name, payload); err != nil {
			t.Fatal(err)
		}
		if phase == EvidenceVerified {
			step.Evidence = &evidence
		}
	}
	journal.IntentDigest = bindJournalIntent(journal.AuthorizationPlan, journal.ResumePlan, journal.ObservationScope, journal.Steps)
	closure.RollbackJournal, err = MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	closure.Transaction, closure.Releases = journal.Transaction, journal.Releases
	closure.Completion.Intent = journal.IntentDigest
	closure.Completion.Plan = closure.ActivationPlan.Token
	closure.Completion.Checkpoint = state.AuthoritySnapshot.Fingerprint
	closure.Completion.Projection = state.ProjectionSnapshot.Fingerprint
	closure.Artifacts, err = store.rollbackArtifactInventory(journal.Transaction)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := marshalRollbackClosure(closure)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.createImmutable(rollbackClosureParts, string(journal.Transaction)+".json", payload); err != nil {
		t.Fatal(err)
	}
	selected, err := store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapCurrentJournal(selected, closure.RollbackJournal); err != nil {
		t.Fatal(err)
	}
	return store, registry, closure
}

func TestCompletedRollbackReadsNativeHistoryWithoutSourceHandlers(t *testing.T) {
	store, registry, closure := completedRollbackFixture(t)
	proof, err := store.InspectCompletedRollback(registry, closure.Completion.TargetRegistry, closure.Completion.TargetArtifact)
	if err != nil || proof == nil || proof.Journal.Transaction != closure.Transaction {
		t.Fatalf("completed rollback = %+v, %v", proof, err)
	}
	if _, _, err := ParseRegistryV2(closure.SourceRegistry, BuiltinCapabilityCatalog()); err == nil {
		t.Fatal("test does not exercise an unknown source kind")
	}
}

func TestCompletedRollbackRequiresExactClosureAndNativeSnapshots(t *testing.T) {
	for _, change := range []string{"target-registry", "target-artifact", "projection", "checkpoint", "artifact-evidence", "missing-closure"} {
		t.Run(change, func(t *testing.T) {
			store, registry, closure := completedRollbackFixture(t)
			digest, artifact := closure.Completion.TargetRegistry, closure.Completion.TargetArtifact
			switch change {
			case "target-registry":
				digest = digestD
			case "target-artifact":
				artifact = digestD
			case "projection":
				testkit.WriteFile(t, filepath.Join(store.configHome, "release-transition", "v2", "ledger.json"), []byte("{}\n"), 0o600)
			case "checkpoint":
				testkit.WriteFile(t, filepath.Join(store.configHome, "release-transition", "v2", migrationLedgerCheckpointName), []byte("{}\n"), 0o600)
			case "artifact-evidence":
				testkit.WriteFile(t, filepath.Join(store.configHome, "release-transition", "v2", "transactions", string(closure.Transaction), "evidence", RollbackTargetActivationStep+".verified.json"), []byte("{}\n"), 0o600)
			case "missing-closure":
				// Absence conveys no foreign-history admission. The ordinary
				// strict reader must remain responsible for this case.
				store.configHome = testkit.TempDir(t)
				selected, err := store.ReadCurrentJournal()
				if err != nil {
					t.Fatal(err)
				}
				if err := store.CompareAndSwapCurrentJournal(selected, closure.RollbackJournal); err != nil {
					t.Fatal(err)
				}
			}
			proof, err := store.InspectCompletedRollback(registry, digest, artifact)
			if proof != nil || err == nil && change != "missing-closure" {
				t.Fatalf("changed completion was accepted: %+v, %v", proof, err)
			}
		})
	}
}

func TestCompletedRollbackSourceMetadataRequiresClosureForEveryForeignArtifactSet(t *testing.T) {
	store, registry, closure := completedRollbackFixture(t)
	if _, err := store.InspectSourceMetadata(registry, closure.Completion.TargetRegistry); err == nil {
		t.Fatal("unclosed foreign forward artifacts were admitted")
	}
	forward := closure
	forward.Transaction = "tx-001"
	forward.Releases = ReleasePair{From: "older", Target: "source"}
	var err error
	forward.Artifacts, err = store.rollbackArtifactInventory(forward.Transaction)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := marshalRollbackClosure(forward)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.createImmutable(rollbackClosureParts, string(forward.Transaction)+".json", payload); err != nil {
		t.Fatal(err)
	}
	proof, err := store.InspectSourceMetadata(registry, closure.Completion.TargetRegistry)
	if err != nil || proof.Journal == nil || proof.Journal.RegistryDigest == closure.Completion.TargetRegistry || !sameLedgerV2(proof.Ledger.Ledger, *closure.ActivationRequest.NativeHistory) {
		t.Fatalf("native source metadata = %+v, %v", proof, err)
	}
	testkit.WriteFile(t, filepath.Join(store.configHome, "reconcile", "native-identities", string(forward.Transaction), "future-migration.object.json"), []byte("changed native identity"), 0o600)
	if _, err := store.InspectSourceMetadata(registry, closure.Completion.TargetRegistry); err == nil {
		t.Fatal("changed native identity was admitted")
	}
}

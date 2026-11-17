package releasetransition

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestRollbackRetentionRejectsChangedSuccessorBeforeUnlink(t *testing.T) {
	for _, field := range []string{"selected journal", "successor closure"} {
		t.Run(field, func(t *testing.T) {
			store, registry, previous := rollbackRetentionFixture(t)
			current := nextRollbackRetentionCycle(t, store, previous, "tx-later")
			selected, err := store.ReadCurrentJournal()
			if err != nil {
				t.Fatal(err)
			}
			state, err := store.ReadMigrationLedger(registry, current.Completion.TargetRegistry)
			if err != nil {
				t.Fatal(err)
			}
			endpoints := ReleaseLinks{Active: "target", Previous: releaseIDPointer("source")}
			if err := store.prepareRollbackRetirement(previous.Transaction, selected, state, endpoints, &current); err != nil {
				t.Fatal(err)
			}
			intent, _, err := store.readRollbackRetirement(previous.Transaction)
			if err != nil {
				t.Fatal(err)
			}
			if field == "selected journal" {
				intent.SelectedJournal = digestA
			} else {
				intent.Superseding = digestA
			}
			payload, err := json.Marshal(intent)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(store.configHome, "reconcile", "rollback", "v1", "closures", rollbackRetirementPrefix+string(previous.Transaction)+".json")
			testkit.WriteFile(t, path, append(payload, '\n'), 0o600)
			if _, err := store.removeRetiringRollbackArtifacts(previous.Transaction, endpoints); err == nil || !strings.Contains(err.Error(), field+" changed") {
				t.Fatalf("changed proof: %v", err)
			}
			if err := store.checkRollbackArtifactInventory(previous.Transaction, previous.Artifacts); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRollbackRetentionSyncsParentsOfAlreadyRemovedRoots(t *testing.T) {
	store, registry, original := rollbackRetentionFixture(t)
	previous := nextRollbackRetentionCycle(t, store, original, "tx-with-native-root")
	current := nextRollbackRetentionCycle(t, store, previous, "tx-new-root")
	selected, err := store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.ReadMigrationLedger(registry, current.Completion.TargetRegistry)
	if err != nil {
		t.Fatal(err)
	}
	endpoints := ReleaseLinks{Active: "target", Previous: releaseIDPointer("source")}
	if err := store.prepareRollbackRetirement(previous.Transaction, selected, state, endpoints, &current); err != nil {
		t.Fatal(err)
	}
	stop := errors.New("crash after unlink")
	for _, root := range []string{"transaction", "native-identities"} {
		store.fault = func(point string) error {
			if point != "after-rollback-directory-unlink" {
				return nil
			}
			parts, name, _, err := rollbackArtifactLocation(previous.Transaction, root+"/")
			if err != nil {
				return err
			}
			path := filepath.Join(append([]string{store.configHome}, append(parts, name)...)...)
			if _, err := os.Stat(path); os.IsNotExist(err) {
				return stop
			}
			return nil
		}
		// Removal visits transaction before native-identities in reverse lexical order.
		if _, err := store.removeRetiringRollbackArtifacts(previous.Transaction, endpoints); !errors.Is(err, stop) {
			t.Fatalf("remove %s: %v", root, err)
		}
	}
	for _, at := range []int{1, 2} {
		syncs := 0
		store.fault = func(point string) error {
			if point == "before-rollback-artifact-root-parent-fsync" {
				syncs++
				if syncs == at {
					return stop
				}
			}
			return nil
		}
		if _, err := store.removeRetiringRollbackArtifacts(previous.Transaction, endpoints); !errors.Is(err, stop) {
			t.Fatalf("sync %d: %v", at, err)
		}
		if _, receipt, err := store.readRollbackRetirement(previous.Transaction); err != nil || !receipt.Exists {
			t.Fatalf("proof discarded before sync %d: %v", at, err)
		}
	}
	store.fault = nil
	if _, err := store.removeRetiringRollbackArtifacts(previous.Transaction, endpoints); err != nil {
		t.Fatal(err)
	}
}

func rollbackRetentionFixture(t *testing.T) (*POSIXV2Store, RegistryV2, rollbackClosureV1) {
	t.Helper()
	store, registry, closure := completedRollbackFixture(t)
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
	return store, registry, closure
}

func nextRollbackRetentionCycle(t *testing.T, store *POSIXV2Store, previous rollbackClosureV1, transaction TransactionID) rollbackClosureV1 {
	t.Helper()
	closure := previous
	closure.Retirement = nil
	journal, err := ParseJournal(previous.RollbackJournal)
	if err != nil {
		t.Fatal(err)
	}
	journal.Transaction = transaction
	journal.RegistryDigest = closure.ActivationRequest.SourceRegistry
	journal.AuthorizationPlan = PlanToken("plan-v1-" + string(fingerprintPayload([]byte(transaction))))
	target, _, err := ParseRegistryV2Provenance(closure.TargetRegistry)
	if err != nil {
		t.Fatal(err)
	}
	owner := &V2Transition{registry: target}
	closure.ActivationPlan.Token, err = owner.rollbackActivationPlan(closure.ActivationRequest, closure.ActivationPlan.Assessment, closure.ActivationPlan.Scopes)
	if err != nil {
		t.Fatal(err)
	}
	closure.Completion.Plan = closure.ActivationPlan.Token
	for index := range journal.Steps {
		step := &journal.Steps[index]
		if step.ID == RollbackSourceOwnerStep {
			binding := rollbackRequestSourceOwner(closure.ActivationRequest)
			step.Expected, err = binding.Fingerprint()
			if err != nil {
				t.Fatal(err)
			}
			step.Desired = step.Expected
		}
		if step.ID == RollbackTargetActivationStep {
			step.Expected = fingerprintPayload([]byte(closure.ActivationPlan.Token))
			step.Desired = step.Expected
		}
		for _, phase := range []EvidenceCheckpoint{EvidenceCaptured, EvidenceApplied, EvidenceVerified} {
			observed := step.Desired
			if phase == EvidenceCaptured {
				observed = step.Expected
			}
			evidence := evidenceFor(journal, *step, phase, observed, "")
			payload, err := MarshalEvidence(evidence)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.CreateCheckpointEvidence(transaction, step.ID, phase, payload); err != nil {
				t.Fatal(err)
			}
			if phase == EvidenceVerified {
				step.Evidence = &evidence
			}
		}
	}
	if err := store.CreateNativeIdentity(transaction, journal.Steps[0].ID, []byte("native identity\n")); err != nil {
		t.Fatal(err)
	}
	journal.IntentDigest = bindJournalIntent(journal.AuthorizationPlan, journal.ResumePlan, journal.ObservationScope, journal.Steps)
	closure.RollbackJournal, err = MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	closure.Transaction = transaction
	closure.Completion.Transaction = transaction
	closure.Completion.Intent = journal.IntentDigest
	closure.Artifacts, err = store.rollbackArtifactInventory(transaction)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := marshalRollbackClosure(closure)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.createImmutable(rollbackClosureParts, string(transaction)+".json", payload); err != nil {
		t.Fatal(err)
	}
	selected, err := store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapCurrentJournal(selected, closure.RollbackJournal); err != nil {
		t.Fatal(err)
	}
	return closure
}

func TestRollbackRetentionMakesRenamedIntentDurableBeforeRetryUnlinks(t *testing.T) {
	store, registry, previous := rollbackRetentionFixture(t)
	current := nextRollbackRetentionCycle(t, store, previous, "tx-new-cycle")
	interrupted := errors.New("stopped after rename before directory fsync")
	store.fault = func(point string) error {
		if point == "after-publish-before-dir-fsync" {
			return interrupted
		}
		return nil
	}
	if err := cleanupRollbackCycle(store, registry, current); !errors.Is(err, interrupted) {
		t.Fatalf("retirement publication = %v", err)
	}
	fresh, err := NewPOSIXV2Store(store.configHome)
	if err != nil {
		t.Fatal(err)
	}
	blockedFsync := errors.New("closure directory fsync unavailable")
	fresh.fault = func(point string) error {
		if point == "before-rollback-retirement-dir-fsync" {
			return blockedFsync
		}
		return nil
	}
	if err := cleanupRollbackCycle(fresh, registry, current); !errors.Is(err, blockedFsync) {
		t.Fatalf("retirement retry = %v", err)
	}
	if err := fresh.checkRollbackArtifactInventory(previous.Transaction, previous.Artifacts); err != nil {
		t.Fatalf("retry touched artifacts before the directory fsync: %v", err)
	}
	fresh.fault = nil
	if err := cleanupRollbackCycle(fresh, registry, current); err != nil {
		t.Fatal(err)
	}
	if closed, err := fresh.readRollbackClosure(previous.Transaction); err != nil || closed != nil {
		t.Fatalf("old cycle remains after a durable retry: %+v, %v", closed, err)
	}
}

func TestRollbackRetentionBindsTwoLargeCyclesWithoutCopyingThem(t *testing.T) {
	store, registry, template := rollbackRetentionFixture(t)
	template.SourceRegistry = append(bytes.Clone(template.SourceRegistry), bytes.Repeat([]byte(" "), 430<<10)...)
	template.ActivationRequest.SourceRegistry = fingerprintPayload(template.SourceRegistry)
	previous := nextRollbackRetentionCycle(t, store, template, "tx-large-old")
	current := nextRollbackRetentionCycle(t, store, previous, "tx-large-new")
	oldBytes, err := marshalRollbackClosure(previous)
	if err != nil {
		t.Fatal(err)
	}
	newBytes, err := marshalRollbackClosure(current)
	if err != nil {
		t.Fatal(err)
	}
	if len(oldBytes)+len(newBytes) <= MaxProtectedRecordBytes {
		t.Fatal("fixture does not exceed the combined record limit")
	}
	interrupted := errors.New("stop at durable retirement")
	store.fault = func(point string) error {
		if point == "after-rollback-retirement-publication" {
			return interrupted
		}
		return nil
	}
	if err := cleanupRollbackCycle(store, registry, current); !errors.Is(err, interrupted) {
		t.Fatalf("large old cycle could not publish retirement: %v", err)
	}
	retired, err := store.readRollbackClosure(previous.Transaction)
	if err != nil || retired == nil || retired.Retirement == nil {
		t.Fatalf("large cycle retirement = %+v, %v", retired, err)
	}
	receipt := retired.Retirement
	if receipt.Original != fingerprintPayload(oldBytes) || receipt.Superseding != fingerprintPayload(newBytes) || receipt.SelectedJournal != fingerprintPayload(current.RollbackJournal) {
		t.Fatal("compact retirement lost its closure or successor binding")
	}
	_, intent, err := store.readRollbackRetirement(previous.Transaction)
	if err != nil || len(intent.Payload) == 0 || len(intent.Payload) > maxRollbackRetirementBytes {
		t.Fatalf("retirement record is not compact: %d bytes, %v", len(intent.Payload), err)
	}
	store.fault = nil
	if err := cleanupRollbackCycle(store, registry, current); err != nil {
		t.Fatal(err)
	}
	if closed, err := store.readRollbackClosure(previous.Transaction); err != nil || closed != nil {
		t.Fatalf("old large cycle remains: %+v, %v", closed, err)
	}
}

func TestRollbackRetentionFinishesAfterDeletingOriginalClosure(t *testing.T) {
	store, registry, previous := rollbackRetentionFixture(t)
	current := nextRollbackRetentionCycle(t, store, previous, "tx-new-cycle")
	interrupted := errors.New("stopped after closure removal")
	store.fault = func(point string) error {
		if point == "after-rollback-closure-retirement" {
			return interrupted
		}
		return nil
	}
	if err := cleanupRollbackCycle(store, registry, current); !errors.Is(err, interrupted) {
		t.Fatalf("closure removal = %v", err)
	}
	fresh, err := NewPOSIXV2Store(store.configHome)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.InspectSourceMetadata(registry, current.Completion.TargetRegistry); err != nil {
		t.Fatalf("remaining retirement intent blocks source metadata: %v", err)
	}
	if err := cleanupRollbackCycle(fresh, registry, current); err != nil {
		t.Fatal(err)
	}
	entries, _, err := fresh.readDirectoryEntries(rollbackClosureParts, 2)
	if err != nil || len(entries) != 1 || entries[0].Name() != string(current.Transaction)+".json" {
		t.Fatalf("cleanup retained old closure or retirement records: %v, %v", entries, err)
	}
}

func TestRollbackRetentionDoesNotSupersedeADifferentSealedPair(t *testing.T) {
	store, registry, previous := rollbackRetentionFixture(t)
	other := previous
	other.SourceArtifact = digestA
	other.ActivationRequest.SourceArtifact = digestA
	current := nextRollbackRetentionCycle(t, store, other, "tx-other-pair")
	if err := cleanupRollbackCycle(store, registry, current); err != nil {
		t.Fatal(err)
	}
	for _, transaction := range []TransactionID{previous.Transaction, "tx-001"} {
		closure, err := store.readRollbackClosure(transaction)
		if err != nil || closure == nil || closure.Retirement != nil {
			t.Fatalf("different sealed pair retired %s: %+v, %v", transaction, closure, err)
		}
	}
}

func TestRollbackRetentionCollectsUnreferencedPairsWithoutSupersession(t *testing.T) {
	store, registry, previous := rollbackRetentionFixture(t)
	writeStoreCurrentJournal(t, store, "tx-unrelated", PlanToken("plan-v1-"+string(digestA)))
	if err := store.cleanupClosedMigrationTransactions("tx-unrelated", registry, previous.Completion.TargetRegistry, ReleaseLinks{Active: "release-b", Previous: releaseIDPointer("release-a")}); err != nil {
		t.Fatal(err)
	}
	entries, _, err := store.readDirectoryEntries(rollbackClosureParts, 1)
	if err != nil || len(entries) != 0 {
		t.Fatalf("unreferenced closed pair remained: %v, %v", entries, err)
	}
}

func TestRollbackRetentionPreservesLiveRecoveryReferences(t *testing.T) {
	store, registry, previous := rollbackRetentionFixture(t)
	current := nextRollbackRetentionCycle(t, store, previous, "tx-new-cycle")
	receipt := recoveryReceiptFixture(t)
	receipt.Predecessor.Transaction = previous.Transaction
	payload, err := MarshalJournal(receipt.Predecessor)
	if err != nil {
		t.Fatal(err)
	}
	receipt.Replacement = ActivationOnlyRecoveryRequest{Transaction: previous.Transaction, Fingerprint: fingerprintPayload(payload)}
	payload, err = MarshalRecoveryReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.createImmutable(recoveryReceiptParts, string(receipt.Successor.Transaction)+".json", payload); err != nil {
		t.Fatal(err)
	}
	if err := cleanupRollbackCycle(store, registry, current); err != nil {
		t.Fatal(err)
	}
	closure, err := store.readRollbackClosure(previous.Transaction)
	if err != nil || closure == nil || closure.Retirement != nil {
		t.Fatalf("live recovery predecessor was retired: %+v, %v", closure, err)
	}
}

func cleanupRollbackCycle(store *POSIXV2Store, registry RegistryV2, closure rollbackClosureV1) error {
	return store.cleanupClosedMigrationTransactions(closure.Transaction, registry, closure.Completion.TargetRegistry, ReleaseLinks{Active: "target", Previous: releaseIDPointer("source")})
}

func TestRollbackRetentionSupersedesRepeatedEndpointCyclesBeyondGraphHorizon(t *testing.T) {
	store, registry, closure := rollbackRetentionFixture(t)
	for cycle := 0; cycle <= maxTransactionGraphEntries; cycle++ {
		closure = nextRollbackRetentionCycle(t, store, closure, TransactionID(fmt.Sprintf("tx-cycle-%03d", cycle)))
		if err := cleanupRollbackCycle(store, registry, closure); err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
		entries, _, err := store.readDirectoryEntries([]string{"release-transition", "v2", "transactions"}, 2)
		if err != nil || len(entries) != 1 || entries[0].Name() != string(closure.Transaction) {
			t.Fatalf("cycle %d retained stale transaction directories: %v, %v", cycle, entries, err)
		}
		entries, _, err = store.readDirectoryEntries(rollbackClosureParts, 2)
		if err != nil || len(entries) != 1 {
			t.Fatalf("cycle %d retained stale closure records: %v, %v", cycle, entries, err)
		}
		entries, _, err = store.readDirectoryEntries([]string{"reconcile", "native-identities"}, 2)
		if err != nil || len(entries) != 1 || entries[0].Name() != string(closure.Transaction) {
			t.Fatalf("cycle %d retained native identity siblings: %v, %v", cycle, entries, err)
		}
	}
}

func TestRollbackRetentionResumesEveryArtifactRemovalCut(t *testing.T) {
	for cut := 0; cut <= 9; cut++ {
		t.Run(fmt.Sprint(cut), func(t *testing.T) {
			store, registry, previous := rollbackRetentionFixture(t)
			current := nextRollbackRetentionCycle(t, store, previous, "tx-new-cycle")
			count := 0
			failure := errors.New("simulated cleanup interruption")
			store.fault = func(point string) error {
				if cut == 0 && point == "after-rollback-retirement-publication" {
					return failure
				}
				if point == "after-rollback-artifact-retirement" {
					count++
					if count == cut {
						return failure
					}
				}
				return nil
			}
			if err := cleanupRollbackCycle(store, registry, current); !errors.Is(err, failure) {
				t.Fatalf("cut %d did not interrupt cleanup: %v", cut, err)
			}
			fresh, err := NewPOSIXV2Store(store.configHome)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fresh.InspectSourceMetadata(registry, current.Completion.TargetRegistry); err != nil {
				t.Fatalf("closed partial retirement blocks source metadata: %v", err)
			}
			if err := cleanupRollbackCycle(fresh, registry, current); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(store.configHome, "release-transition", "v2", "transactions", string(previous.Transaction))); !os.IsNotExist(err) {
				t.Fatalf("old rollback artifacts remain: %v", err)
			}
		})
	}
}

func TestRollbackRetentionPreservesPinnedProjectionAndChangedBytes(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		t.Run(fmt.Sprint(pinned), func(t *testing.T) {
			store, registry, previous := rollbackRetentionFixture(t)
			if pinned {
				previous.ProjectionProducer = previous.Completion.Projection
				payload, err := marshalRollbackClosure(previous)
				if err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(store.configHome, "reconcile", "rollback", "v1", "closures", string(previous.Transaction)+".json"), payload, 0o600)
			}
			current := nextRollbackRetentionCycle(t, store, previous, "tx-new-cycle")
			// A producer pin belongs to the original transaction, not its successor.
			if pinned {
				current.ProjectionProducer = ""
				payload, err := marshalRollbackClosure(current)
				if err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(store.configHome, "reconcile", "rollback", "v1", "closures", string(current.Transaction)+".json"), payload, 0o600)
				if err := cleanupRollbackCycle(store, registry, current); err != nil {
					t.Fatal(err)
				}
				if closure, err := store.readRollbackClosure(previous.Transaction); err != nil || closure == nil || closure.Retirement != nil {
					t.Fatalf("projection producer was retired: %+v, %v", closure, err)
				}
				return
			}
			store.fault = func(point string) error {
				if point == "after-rollback-retirement-publication" {
					return errors.New("stop before deletion")
				}
				return nil
			}
			if err := cleanupRollbackCycle(store, registry, current); err == nil {
				t.Fatal("cleanup did not stop before deletion")
			}
			testkit.WriteFile(t, filepath.Join(store.configHome, "reconcile", "native-identities", "tx-001", "future-migration.object.json"), []byte("changed"), 0o600)
			fresh, err := NewPOSIXV2Store(store.configHome)
			if err != nil {
				t.Fatal(err)
			}
			if err := cleanupRollbackCycle(fresh, registry, current); err == nil {
				t.Fatal("changed retiring bytes were deleted")
			}
		})
	}
}

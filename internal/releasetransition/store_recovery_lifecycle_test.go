package releasetransition

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
)

func lifecycleReceiptFixture(t *testing.T) RecoveryReceiptV1 {
	t.Helper()
	receipt := recoveryReceiptFixture(t)
	receipt.SchemaVersion, receipt.Contract = RecoveryReceiptSchemaV2, ActivationOnlyRecoveryContractV2
	receipt.Generation = 1
	receipt.Successor.Transaction = lifecycleTransaction(t, receipt.Replacement, receipt.Generation)
	receipt.BasePlan = receipt.Successor.AuthorizationPlan
	if err := receipt.Validate(); err != nil {
		t.Fatal(err)
	}
	return receipt
}

func lifecycleTransaction(t *testing.T, request ActivationOnlyRecoveryRequest, generation uint64) TransactionID {
	t.Helper()
	transaction, err := RecoveryLifecycleTransaction(request, generation)
	if err != nil {
		t.Fatal(err)
	}
	return transaction
}

func lifecycleStoreFixture(t *testing.T) (*POSIXV2Store, RecoveryReceiptV1) {
	t.Helper()
	store, err := NewPOSIXV2Store(protectedConfigHome(t))
	if err != nil {
		t.Fatal(err)
	}
	receipt := lifecycleReceiptFixture(t)
	payload, err := MarshalJournal(receipt.Predecessor)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapCurrentJournal(absentProtectedSnapshot(), payload); err != nil {
		t.Fatal(err)
	}
	createLifecycleReceipt(t, store, receipt)
	return store, receipt
}

func createLifecycleReceipt(t *testing.T, store *POSIXV2Store, receipt RecoveryReceiptV1) {
	t.Helper()
	payload, err := MarshalRecoveryReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRecoveryReceipt(receipt.Successor.Transaction, payload); err != nil {
		t.Fatal(err)
	}
}

func lifecycleNewPlan(t *testing.T, store *POSIXV2Store, request ActivationOnlyRecoveryRequest) (PlanToken, PlanToken, Fingerprint) {
	t.Helper()
	base := PlanToken("plan-v1-" + strings.Repeat("b", 64))
	reservation, err := store.RecoveryReservation(request, base)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BindRecoveryLifecyclePlan(base, reservation)
	if err != nil {
		t.Fatal(err)
	}
	return base, plan, reservation
}

func TestRecoveryLifecycleCancellationResumesEveryDurableBoundary(t *testing.T) {
	for _, point := range []string{"before-recovery-cancellation", "after-pending-fsync", "before-publish", "after-publish-before-dir-fsync", "after-recovery-cancellation", "after-recovery-lifecycle-unlink"} {
		t.Run(point, func(t *testing.T) {
			store, receipt := lifecycleStoreFixture(t)
			unlock, err := store.Lock()
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			before, err := store.ReadCurrentJournal()
			if err != nil {
				t.Fatal(err)
			}
			base, plan, reservation := lifecycleNewPlan(t, store, receipt.Replacement)
			if reservation != fingerprintPayload(mustRecoveryReceiptPayload(receipt)) {
				t.Fatal("fresh plan did not bind the exact cancelled receipt")
			}
			injected := errors.New("interrupted lifecycle storage")
			store.fault = func(actual string) error {
				if point == actual {
					return injected
				}
				return nil
			}
			proposed := TransactionID(RecoveryTransactionPrefixV2 + "fresh")
			if _, err := store.ResolveLifecycleRecoveryTransaction(proposed, receipt.Replacement, base, plan, digestC); !errors.Is(err, injected) {
				t.Fatalf("fault %s was not observed: %v", point, err)
			}
			store.fault = nil
			again, err := store.RecoveryReservation(receipt.Replacement, base)
			if err != nil || again != reservation {
				t.Fatalf("reservation changed across restart: got=%s err=%v", again, err)
			}
			transaction, err := store.ResolveLifecycleRecoveryTransaction(proposed, receipt.Replacement, base, plan, digestB)
			if err != nil || transaction != lifecycleTransaction(t, receipt.Replacement, 2) {
				t.Fatalf("restart cancellation: transaction=%s err=%v", transaction, err)
			}
			terminal, present, err := store.readLifecycleRecord(receipt.Successor.Transaction)
			if err != nil || !present || terminal.Cancellation == nil || !sameRecoveryReceipt(terminal.Receipt, receipt) {
				t.Fatalf("cancellation evidence lost: present=%t err=%v", present, err)
			}
			active, err := store.ReadRecoveryReceipt(receipt.Successor.Transaction)
			if err != nil || active.Exists {
				t.Fatalf("cancelled reservation remains active: exists=%t err=%v", active.Exists, err)
			}
			after, err := store.ReadCurrentJournal()
			if err != nil || !bytes.Equal(before.Payload, after.Payload) {
				t.Fatalf("cancellation changed current journal: %v", err)
			}
			next := receipt
			next.BasePlan, next.Reservation = base, reservation
			next.Generation = 2
			next.Successor.Transaction, next.Successor.AuthorizationPlan = transaction, plan
			next.Successor.IntentDigest = bindJournalIntent(plan, next.Successor.ResumePlan, next.Successor.ObservationScope, next.Successor.Steps)
			createLifecycleReceipt(t, store, next)
			if got, err := store.RecoveryReservation(receipt.Replacement, base); err != nil || got != reservation {
				t.Fatalf("own new receipt changed authorization binding: got=%s err=%v", got, err)
			}
			if err := store.CreateRecoveryReceipt(receipt.Successor.Transaction, mustRecoveryReceiptPayload(receipt)); err == nil {
				t.Fatal("cancelled successor was republished")
			}
		})
	}
}

func TestRecoveryLifecycleCancellationInvalidatesOnlyExactPendingCAS(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprintf("foreign=%t", foreign), func(t *testing.T) {
			store, receipt := lifecycleStoreFixture(t)
			base, plan, _ := lifecycleNewPlan(t, store, receipt.Replacement)
			pending := receipt.Successor
			if foreign {
				pending.Transaction = "tx-foreign"
			}
			payload, err := MarshalJournal(pending)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(store.configHome, "release-transition", "v2", ".journal.json.pending")
			testkit.WriteFile(t, path, payload, 0o600)
			_, err = store.ResolveLifecycleRecoveryTransaction(RecoveryTransactionPrefixV2+"fresh", receipt.Replacement, base, plan, digestC)
			if foreign {
				if err == nil {
					t.Fatal("foreign pending CAS was cancelled")
				}
				got, readErr := os.ReadFile(path)
				if readErr != nil || !bytes.Equal(got, payload) {
					t.Fatal("failed cancellation changed foreign pending evidence")
				}
			} else if err != nil {
				t.Fatal(err)
			} else if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
				t.Fatal("cancelled successor still has a prepared CAS")
			}
		})
	}
}

func TestRecoveryLifecycleRejectsCrossFamilyReservationsAndSelectedCancellation(t *testing.T) {
	t.Run("legacy reservation", func(t *testing.T) {
		store, err := NewPOSIXV2Store(protectedConfigHome(t))
		if err != nil {
			t.Fatal(err)
		}
		receipt := recoveryReceiptFixture(t)
		createLifecycleReceipt(t, store, receipt)
		if _, err := store.RecoveryReservation(receipt.Replacement, receipt.Successor.AuthorizationPlan); err == nil {
			t.Fatal("lifecycle admission bypassed a legacy reservation")
		}
		if _, err := store.ResolveRecoveryTransaction(RecoveryTransactionPrefixV1+"retry", receipt.Replacement, receipt.Successor.AuthorizationPlan); err != nil {
			t.Fatalf("legacy exact retry changed: %v", err)
		}
	})
	t.Run("lifecycle reservation", func(t *testing.T) {
		store, receipt := lifecycleStoreFixture(t)
		if _, err := store.ResolveRecoveryTransaction(RecoveryTransactionPrefixV1+"other", receipt.Replacement, receipt.Successor.AuthorizationPlan); err == nil {
			t.Fatal("legacy writer bypassed a lifecycle reservation")
		}
		before, err := store.ReadCurrentJournal()
		if err != nil {
			t.Fatal(err)
		}
		payload, err := MarshalJournal(receipt.Successor)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.CompareAndSwapCurrentJournal(before, payload); err != nil {
			t.Fatal(err)
		}
		base, plan, _ := lifecycleNewPlan(t, store, receipt.Replacement)
		if _, err := store.ResolveLifecycleRecoveryTransaction(RecoveryTransactionPrefixV2+"other", receipt.Replacement, base, plan, digestC); err == nil {
			t.Fatal("selected successor was cancelled")
		}
		if _, err := store.ValidatedCurrentRecovery(receipt.Successor); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRecoveryLifecycleExactResumeIgnoresUnrelatedHistoricalCorruption(t *testing.T) {
	store, receipt := lifecycleStoreFixture(t)
	parent, _, err := store.openParent(recoveryArchiveParts, true)
	if err != nil {
		t.Fatal(err)
	}
	unix.Close(parent)
	path := filepath.Join(store.configHome, "release-transition", "recovery", "v2", "archive", "unrelated.json")
	testkit.WriteFile(t, path, []byte("malformed history\n"), 0o600)
	if _, err := store.ValidatedCurrentRecovery(receipt.Successor); err != nil {
		t.Fatalf("unrelated history prevented exact resume: %v", err)
	}
	current := receipt.Successor
	current.Checkpoint = JournalComplete
	before, err := store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := MarshalJournal(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapCurrentJournal(before, payload); err != nil {
		t.Fatal(err)
	}
	if err := store.CleanupRecovery(current.Transaction); err == nil {
		t.Fatal("cleanup accepted malformed historical evidence")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("failed cleanup discarded malformed history")
	}
}

func TestRecoveryLifecycleCleanupRequiresTerminalProofAndPreservesReferences(t *testing.T) {
	store, receipt := lifecycleStoreFixture(t)
	current := receipt.Predecessor
	current.Transaction, current.Checkpoint = "tx-next", JournalComplete
	before, err := store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := MarshalJournal(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapCurrentJournal(before, payload); err != nil {
		t.Fatal(err)
	}
	if err := store.CleanupRecovery(current.Transaction); err != nil {
		t.Fatal(err)
	}
	if snapshot, err := store.ReadRecoveryReceipt(receipt.Successor.Transaction); err != nil || !snapshot.Exists {
		t.Fatal("unreferenced initial receipt was treated as completed")
	}
	completed := receipt.Successor
	completed.Checkpoint = JournalComplete
	terminal := RecoveryLifecycleRecord{SchemaVersion: RecoveryLifecycleSchemaV1, Receipt: receipt, Completed: &completed}
	if err := store.createLifecycleRecord(terminal); err != nil {
		t.Fatal(err)
	}
	parts := []string{"release-transition", "v2", "transactions", "tx-reference"}
	archive := SupersededJournalRecord{SchemaVersion: SupersededJournalSchemaV1,
		AuthorizationPlan: current.AuthorizationPlan,
		Replacement:       JournalReplacement{Transaction: completed.Transaction, Fingerprint: fingerprintPayload(mustJournalPayload(t, completed)), Reason: JournalReplacementPostActivationScopeV0111, SourceVersion: "1.0.0"}, Journal: completed}
	archivePayload, err := MarshalSupersededJournal(archive)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(append([]string{store.configHome}, append(parts, ".superseded-journal.json.pending")...)...)
	parent, _, err := store.openParent(parts, true)
	if err != nil {
		t.Fatal(err)
	}
	unix.Close(parent)
	testkit.WriteFile(t, path, archivePayload, 0o600)
	if err := store.CleanupRecovery(current.Transaction); err != nil {
		t.Fatal(err)
	}
	if snapshot, err := store.ReadRecoveryReceipt(receipt.Successor.Transaction); err != nil || !snapshot.Exists {
		t.Fatal("pending ordinary archive lost referenced recovery receipt")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := store.CleanupRecovery(current.Transaction); err != nil {
		t.Fatal(err)
	}
	if snapshot, err := store.ReadRecoveryReceipt(receipt.Successor.Transaction); err != nil || snapshot.Exists {
		t.Fatal("unreachable receipt with terminal proof was not retired")
	}
}

func mustJournalPayload(t *testing.T, journal JournalRecord) []byte {
	t.Helper()
	payload, err := MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestRecoveryLifecycleHistoryStaysBoundedBeyondLegacyCeiling(t *testing.T) {
	store, err := NewPOSIXV2Store(protectedConfigHome(t))
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < maxTransactionGraphEntries+2; index++ {
		receipt := lifecycleReceiptFixture(t)
		receipt.Predecessor.Transaction = TransactionID(fmt.Sprintf("tx-source-%03d", index))
		receipt.Replacement.Transaction = receipt.Predecessor.Transaction
		before := mustJournalPayload(t, receipt.Predecessor)
		receipt.Replacement.Fingerprint = fingerprintPayload(before)
		receipt.Successor.Transaction = lifecycleTransaction(t, receipt.Replacement, receipt.Generation)
		selected, err := store.ReadCurrentJournal()
		if err != nil {
			t.Fatal(err)
		}
		if err := store.CompareAndSwapCurrentJournal(selected, before); err != nil {
			t.Fatal(err)
		}
		createLifecycleReceipt(t, store, receipt)
		selected, err = store.ReadCurrentJournal()
		if err != nil {
			t.Fatal(err)
		}
		completed := receipt.Successor
		completed.Checkpoint = JournalComplete
		if err := store.CompareAndSwapCurrentJournal(selected, mustJournalPayload(t, completed)); err != nil {
			t.Fatal(err)
		}
		if err := store.CleanupRecovery(completed.Transaction); err != nil {
			t.Fatalf("cycle %d: %v", index, err)
		}
	}
	active, err := store.readRecoveryReceiptsAt(recoveryLifecycleParts)
	if err != nil || len(active) != 1 {
		t.Fatalf("active receipt history: count=%d err=%v", len(active), err)
	}
	archive, err := store.readLifecycleArchive()
	if err != nil || len(archive) > 32 {
		t.Fatalf("terminal archive history: count=%d err=%v", len(archive), err)
	}
}

func TestRecoveryLifecycleCleanupMakesRoomForCompletionAtArchiveCapacity(t *testing.T) {
	store, receipt := lifecycleStoreFixture(t)
	for index := 0; index < maxRecoveryLifecycleEntries; index++ {
		history := lifecycleReceiptFixture(t)
		history.Predecessor.Transaction = TransactionID(fmt.Sprintf("tx-history-source-%03d", index))
		history.Replacement.Transaction = history.Predecessor.Transaction
		history.Replacement.Fingerprint = fingerprintPayload(mustJournalPayload(t, history.Predecessor))
		history.Successor.Transaction = lifecycleTransaction(t, history.Replacement, history.Generation)
		completed := history.Successor
		completed.Checkpoint = JournalComplete
		payload, err := MarshalRecoveryLifecycleRecord(RecoveryLifecycleRecord{
			SchemaVersion: RecoveryLifecycleSchemaV1, Receipt: history, Completed: &completed,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.createImmutable(recoveryArchiveParts, string(history.Successor.Transaction)+".json", payload); err != nil {
			t.Fatal(err)
		}
	}
	completed := receipt.Successor
	completed.Checkpoint = JournalComplete
	selected, err := store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapCurrentJournal(selected, mustJournalPayload(t, completed)); err != nil {
		t.Fatal(err)
	}
	if err := store.CleanupRecovery(completed.Transaction); err != nil {
		t.Fatalf("eligible full archive prevented completion capture: %v", err)
	}
	archive, err := store.readLifecycleArchive()
	if err != nil || len(archive) != maxRecoveryLifecycleEntries-32+1 {
		t.Fatalf("bounded capacity cleanup: count=%d err=%v", len(archive), err)
	}
	witness, present, err := store.readLifecycleRecord(completed.Transaction)
	if err != nil || !present || witness.Completed == nil || witness.Completed.Transaction != completed.Transaction {
		t.Fatalf("current completion witness was not captured: present=%t err=%v", present, err)
	}
	active, err := store.ReadRecoveryReceipt(completed.Transaction)
	if err != nil || !active.Exists {
		t.Fatal("capacity cleanup deleted the selected current receipt")
	}
}

func TestRecoveryLifecycleReadOnlyBindsChangedInputsAfterCancellationArchive(t *testing.T) {
	store, receipt := lifecycleStoreFixture(t)
	base, plan, reservation := lifecycleNewPlan(t, store, receipt.Replacement)
	injected := errors.New("stop after cancellation publication")
	store.fault = func(point string) error {
		if point == "after-recovery-cancellation" {
			return injected
		}
		return nil
	}
	if _, err := store.ResolveLifecycleRecoveryTransaction(RecoveryTransactionPrefixV2+"next", receipt.Replacement, base, plan, digestC); !errors.Is(err, injected) {
		t.Fatal(err)
	}
	store.fault = nil
	before, err := store.ReadRecoveryReceipt(receipt.Successor.Transaction)
	if err != nil {
		t.Fatal(err)
	}
	third := PlanToken("plan-v1-" + strings.Repeat("e", 64))
	if got, err := store.RecoveryReservation(receipt.Replacement, third); err != nil || got != reservation {
		t.Fatalf("changed inputs lost durable cancellation binding: got=%s err=%v", got, err)
	}
	after, err := store.ReadRecoveryReceipt(receipt.Successor.Transaction)
	if err != nil || !bytes.Equal(before.Payload, after.Payload) {
		t.Fatal("read-only assessment mutated the live reservation")
	}
	if got, err := store.RecoveryReservation(receipt.Replacement, base); err != nil || got != reservation {
		t.Fatalf("exact cancellation continuation was lost: got=%s err=%v", got, err)
	}
}

func TestRecoveryLifecycleJournalOverwriteCapturesCompletionOrRetainsEvidence(t *testing.T) {
	for _, interrupt := range []bool{false, true} {
		t.Run(fmt.Sprintf("interrupt=%t", interrupt), func(t *testing.T) {
			store, receipt := lifecycleStoreFixture(t)
			completed := receipt.Successor
			completed.Checkpoint = JournalComplete
			selected, err := store.ReadCurrentJournal()
			if err != nil {
				t.Fatal(err)
			}
			if err := store.CompareAndSwapCurrentJournal(selected, mustJournalPayload(t, completed)); err != nil {
				t.Fatal(err)
			}
			selected, err = store.ReadCurrentJournal()
			if err != nil {
				t.Fatal(err)
			}
			if interrupt {
				store.fault = func(point string) error {
					if point == "before-recovery-completion" {
						return errors.New("terminal evidence capture interrupted")
					}
					return nil
				}
			}
			later := receipt.Predecessor
			later.Transaction, later.Checkpoint = "tx-later", JournalComplete
			payload := mustJournalPayload(t, later)
			if err := store.CompareAndSwapCurrentJournal(selected, payload); err != nil {
				t.Fatalf("completion capture prevented otherwise valid journal CAS: %v", err)
			}
			store.fault = nil
			current, err := store.ReadCurrentJournal()
			if err != nil || !bytes.Equal(current.Payload, payload) {
				t.Fatal("journal overwrite did not publish exact new selection")
			}
			terminal, present, err := store.readLifecycleRecord(completed.Transaction)
			if err != nil {
				t.Fatal(err)
			}
			if interrupt {
				if present || !store.retirementCaptureFailed {
					t.Fatal("completion failure did not retain conservative cleanup state")
				}
				if err := store.CleanupRecovery(later.Transaction); err == nil {
					t.Fatal("completion evidence loss did not surface as pending cleanup")
				}
				active, err := store.ReadRecoveryReceipt(completed.Transaction)
				if err != nil || !active.Exists {
					t.Fatal("completion capture failure discarded the old live receipt")
				}
			} else if !present || terminal.Completed == nil || !bytes.Equal(mustJournalPayload(t, *terminal.Completed), selected.Payload) {
				t.Fatal("journal overwrite did not preserve exact selected completion witness")
			}
		})
	}
}

func TestRecoveryLifecyclePreCASCompactionBeyondNamespaceCapacity(t *testing.T) {
	store, receipt := lifecycleStoreFixture(t)
	before, err := store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	first := receipt
	for generation := uint64(2); generation <= maxRecoveryLifecycleEntries+2; generation++ {
		base := PlanToken("plan-v1-" + strings.Repeat("b", 64))
		if generation%2 != 0 {
			base = PlanToken("plan-v1-" + strings.Repeat("e", 64))
		}
		reservation, err := store.RecoveryReservation(receipt.Replacement, base)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := BindRecoveryLifecyclePlan(base, reservation)
		if err != nil {
			t.Fatal(err)
		}
		transaction, err := store.ResolveLifecycleRecoveryTransaction("recovery-v2-proposed", receipt.Replacement, base, plan, digestC)
		if err != nil {
			t.Fatalf("generation %d admission: %v", generation, err)
		}
		receipt.BasePlan, receipt.Reservation, receipt.Generation = base, reservation, generation
		receipt.Successor.Transaction, receipt.Successor.AuthorizationPlan = transaction, plan
		receipt.Successor.IntentDigest = bindJournalIntent(plan, receipt.Successor.ResumePlan, receipt.Successor.ObservationScope, receipt.Successor.Steps)
		createLifecycleReceipt(t, store, receipt)
	}
	after, err := store.ReadCurrentJournal()
	if err != nil || !sameProtectedSnapshot(before, after) {
		t.Fatal("pre-CAS cancellation compaction changed original journal")
	}
	archive, err := store.readLifecycleArchive()
	if err != nil || len(archive) != 1 {
		t.Fatalf("pre-CAS history accumulated: count=%d err=%v", len(archive), err)
	}
	if _, exists := archive[first.Successor.Transaction]; exists {
		t.Fatal("first cancellation was not compacted")
	}
	if err := store.CreateRecoveryReceipt(first.Successor.Transaction, mustRecoveryReceiptPayload(first)); err == nil {
		t.Fatal("compacted cancelled generation was republished")
	}
}

func TestRecoveryLifecycleRetiresOnlyCompletedUnreferencedLegacy(t *testing.T) {
	for _, test := range []struct {
		name    string
		witness bool
		pin     string
	}{
		{"unproven", false, ""},
		{"closed", true, ""},
		{"pending-predecessor", true, "pending"},
		{"ordinary-history", true, "history"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, err := NewPOSIXV2Store(protectedConfigHome(t))
			if err != nil {
				t.Fatal(err)
			}
			receipt := recoveryReceiptFixture(t)
			createLifecycleReceipt(t, store, receipt)
			completed := receipt.Successor
			completed.Checkpoint = JournalComplete
			if test.witness {
				if err := store.createLifecycleRecord(RecoveryLifecycleRecord{SchemaVersion: RecoveryLifecycleSchemaV1, Receipt: receipt, Completed: &completed}); err != nil {
					t.Fatal(err)
				}
			}
			current := receipt.Predecessor
			current.Transaction, current.Checkpoint = "tx-new-selected", JournalComplete
			if err := store.CompareAndSwapCurrentJournal(absentProtectedSnapshot(), mustJournalPayload(t, current)); err != nil {
				t.Fatal(err)
			}
			if test.pin == "pending" {
				testkit.WriteFile(t, filepath.Join(store.configHome, "release-transition", "v2", ".journal.json.pending"), mustJournalPayload(t, receipt.Predecessor), 0o600)
			}
			if test.pin == "history" {
				archive := SupersededJournalRecord{SchemaVersion: SupersededJournalSchemaV1, AuthorizationPlan: current.AuthorizationPlan,
					Replacement: JournalReplacement{Transaction: completed.Transaction, Fingerprint: fingerprintPayload(mustJournalPayload(t, completed)), Reason: JournalReplacementPostActivationScopeV0111, SourceVersion: "1.0.0"}, Journal: completed}
				payload, err := MarshalSupersededJournal(archive)
				if err != nil {
					t.Fatal(err)
				}
				parts := []string{"release-transition", "v2", "transactions", "tx-rollback-reference"}
				if err := store.createImmutable(parts, "superseded-journal.json", payload); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.CleanupRecovery(current.Transaction); err != nil {
				t.Fatal(err)
			}
			live, err := store.ReadRecoveryReceipt(receipt.Successor.Transaction)
			wantExists := !test.witness || test.pin != ""
			if err != nil || live.Exists != wantExists {
				t.Fatalf("legacy evidence retirement: exists=%t want=%t err=%v", live.Exists, wantExists, err)
			}
		})
	}
}

func TestRecoveryLifecycleNamespaceCountsPairsAndRejectsAggregateBytes(t *testing.T) {
	store, receipt := lifecycleStoreFixture(t)
	payload := mustRecoveryReceiptPayload(receipt)
	if err := store.createImmutable(recoveryLifecycleParts, "."+string(receipt.Successor.Transaction)+".json.pending", payload); err != nil {
		t.Fatal(err)
	}
	if records, err := store.readRecoveryReceiptsAt(recoveryLifecycleParts); err != nil || len(records) != 1 {
		t.Fatalf("pending copy counted twice: %d %v", len(records), err)
	}
	parent, _, err := store.openParent(recoveryArchiveParts, true)
	if err != nil {
		t.Fatal(err)
	}
	unix.Close(parent)
	for index := 0; index <= maxRecoveryLifecycleBytes/MaxProtectedRecordBytes; index++ {
		path := filepath.Join(append([]string{store.configHome}, append(append([]string(nil), recoveryArchiveParts...), fmt.Sprintf("tx-byte-%03d.json", index))...)...)
		testkit.WriteFile(t, path, []byte("{}\n"), 0o600)
		if err := os.Truncate(path, MaxProtectedRecordBytes); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := store.recoveryEntries(recoveryArchiveParts); err == nil || !strings.Contains(err.Error(), "aggregate byte limit") {
		t.Fatalf("aggregate byte budget was not enforced before parsing: %v", err)
	}
}

func TestRecoveryLifecycleRejectsRewoundCompletedSuccessor(t *testing.T) {
	store, receipt := lifecycleStoreFixture(t)
	completed := receipt.Successor
	completed.Checkpoint = JournalComplete
	if err := store.createLifecycleRecord(RecoveryLifecycleRecord{SchemaVersion: RecoveryLifecycleSchemaV1, Receipt: receipt, Completed: &completed}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ValidatedCurrentRecovery(receipt.Successor); err == nil {
		t.Fatal("completed terminal witness accepted a rewound current checkpoint")
	}
}

func TestRecoveryLifecycleGenerationOverflowPreservesEvidence(t *testing.T) {
	store, err := NewPOSIXV2Store(protectedConfigHome(t))
	if err != nil {
		t.Fatal(err)
	}
	prior := lifecycleReceiptFixture(t)
	prior.Generation = ^uint64(0) - 1
	prior.Successor.Transaction = lifecycleTransaction(t, prior.Replacement, prior.Generation)
	base := PlanToken("plan-v1-" + strings.Repeat("b", 64))
	reservation := fingerprintPayload(mustRecoveryReceiptPayload(prior))
	plan, err := BindRecoveryLifecyclePlan(base, reservation)
	if err != nil {
		t.Fatal(err)
	}
	terminal := RecoveryLifecycleRecord{SchemaVersion: RecoveryLifecycleSchemaV1, Receipt: prior,
		Cancellation: &RecoveryCancellation{BasePlan: base, Plan: plan, AuthorizationDigest: digestC}}
	frontier := RecoveryFrontier{SchemaVersion: RecoveryLifecycleSchemaV1, Replacement: prior.Replacement, Generation: ^uint64(0), Cancellation: &terminal}
	payload, err := MarshalRecoveryFrontier(frontier)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.createImmutable(recoveryFrontierParts, string(prior.Replacement.Transaction)+".json", payload); err != nil {
		t.Fatal(err)
	}
	receipt := prior
	receipt.Generation = ^uint64(0)
	receipt.BasePlan, receipt.Reservation = base, reservation
	receipt.Successor.Transaction = lifecycleTransaction(t, receipt.Replacement, receipt.Generation)
	receipt.Successor.AuthorizationPlan = plan
	receipt.Successor.IntentDigest = bindJournalIntent(plan, receipt.Successor.ResumePlan, receipt.Successor.ObservationScope, receipt.Successor.Steps)
	if err := store.createImmutable(recoveryLifecycleParts, string(receipt.Successor.Transaction)+".json", mustRecoveryReceiptPayload(receipt)); err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapCurrentJournal(absentProtectedSnapshot(), mustJournalPayload(t, receipt.Predecessor)); err != nil {
		t.Fatal(err)
	}
	nextBase := PlanToken("plan-v1-" + strings.Repeat("e", 64))
	if _, err := store.RecoveryReservation(receipt.Replacement, nextBase); err == nil || !strings.Contains(err.Error(), "generation exhausted") {
		t.Fatalf("read-only assessment allowed counter overflow: %v", err)
	}
	nextReservation := fingerprintPayload(mustRecoveryReceiptPayload(receipt))
	nextPlan, err := BindRecoveryLifecyclePlan(nextBase, nextReservation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveLifecycleRecoveryTransaction("recovery-v2-next", receipt.Replacement, nextBase, nextPlan, digestC); err == nil || !strings.Contains(err.Error(), "generation exhausted") {
		t.Fatalf("counter wrapped instead of refusing safely: %v", err)
	}
	if current, err := store.ReadRecoveryReceipt(receipt.Successor.Transaction); err != nil || !bytes.Equal(current.Payload, mustRecoveryReceiptPayload(receipt)) {
		t.Fatal("overflow refusal changed live evidence")
	}
	if _, present, err := store.readLifecycleRecord(receipt.Successor.Transaction); err != nil || present {
		t.Fatal("overflow refusal published cancellation")
	}
}

func TestRecoveryLifecycleLegacyCapacityAdmitsAfterProvenRetirement(t *testing.T) {
	store, err := NewPOSIXV2Store(protectedConfigHome(t))
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < maxTransactionGraphEntries; index++ {
		receipt := recoveryReceiptFixture(t)
		receipt.Predecessor.Transaction = TransactionID(fmt.Sprintf("tx-legacy-source-%03d", index))
		receipt.Replacement.Transaction = receipt.Predecessor.Transaction
		receipt.Replacement.Fingerprint = fingerprintPayload(mustJournalPayload(t, receipt.Predecessor))
		receipt.Successor.Transaction = TransactionID(fmt.Sprintf("recovery-v1-history-%03d", index))
		if err := store.createImmutable(recoveryReceiptParts, string(receipt.Successor.Transaction)+".json", mustRecoveryReceiptPayload(receipt)); err != nil {
			t.Fatal(err)
		}
		completed := receipt.Successor
		completed.Checkpoint = JournalComplete
		payload, err := MarshalRecoveryLifecycleRecord(RecoveryLifecycleRecord{SchemaVersion: RecoveryLifecycleSchemaV1, Receipt: receipt, Completed: &completed})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.createImmutable(recoveryArchiveParts, string(receipt.Successor.Transaction)+".json", payload); err != nil {
			t.Fatal(err)
		}
	}
	request := recoveryReceiptFixture(t).Replacement
	transaction, err := store.ResolveRecoveryTransaction("recovery-v1-next", request, PlanToken("plan-v1-"+strings.Repeat("c", 64)))
	if err != nil || transaction != "recovery-v1-next" {
		t.Fatalf("closed legacy backlog prevented new admission: %s %v", transaction, err)
	}
	live, err := store.readRecoveryReceipts()
	if err != nil || len(live) != maxTransactionGraphEntries-32 {
		t.Fatalf("legacy retirement exceeded/missed bounded batch: count=%d err=%v", len(live), err)
	}
}

func TestRecoveryLifecycleCompletedPredecessorClosesUnselectedReservations(t *testing.T) {
	for _, modern := range []bool{false, true} {
		t.Run(fmt.Sprintf("modern=%t", modern), func(t *testing.T) {
			store, err := NewPOSIXV2Store(protectedConfigHome(t))
			if err != nil {
				t.Fatal(err)
			}
			receipt := recoveryReceiptFixture(t)
			if modern {
				receipt = lifecycleReceiptFixture(t)
			}
			if err := store.CompareAndSwapCurrentJournal(absentProtectedSnapshot(), mustJournalPayload(t, receipt.Predecessor)); err != nil {
				t.Fatal(err)
			}
			createLifecycleReceipt(t, store, receipt)
			sourceCompleted := receipt.Predecessor
			sourceCompleted.Checkpoint = JournalComplete
			selected, err := store.ReadCurrentJournal()
			if err != nil {
				t.Fatal(err)
			}
			if err := store.CompareAndSwapCurrentJournal(selected, mustJournalPayload(t, sourceCompleted)); err != nil {
				t.Fatal(err)
			}
			if err := store.CleanupRecovery(sourceCompleted.Transaction); err != nil {
				t.Fatal(err)
			}
			terminal, present, err := store.readLifecycleRecord(receipt.Successor.Transaction)
			if err != nil || !present || terminal.PredecessorCompleted == nil || terminal.Completed != nil || terminal.Cancellation != nil {
				t.Fatalf("source completion was not captured: %#v %v", terminal, err)
			}
			if !bytes.Equal(mustJournalPayload(t, *terminal.PredecessorCompleted), mustJournalPayload(t, sourceCompleted)) {
				t.Fatal("closure witness guessed original journal fields")
			}
			if live, err := store.ReadRecoveryReceipt(receipt.Successor.Transaction); err != nil || !live.Exists {
				t.Fatal("selected predecessor pin was ignored")
			}
			if _, err := store.ValidatedCurrentRecovery(receipt.Successor); err == nil {
				t.Fatal("closed original journal permitted a stale successor selection")
			}
			selected, err = store.ReadCurrentJournal()
			if err != nil {
				t.Fatal(err)
			}
			later := sourceCompleted
			later.Transaction = "tx-later-ordinary"
			if err := store.CompareAndSwapCurrentJournal(selected, mustJournalPayload(t, later)); err != nil {
				t.Fatal(err)
			}
			if err := store.CleanupRecovery(later.Transaction); err != nil {
				t.Fatal(err)
			}
			if live, err := store.ReadRecoveryReceipt(receipt.Successor.Transaction); err != nil || live.Exists {
				t.Fatal("closed obsolete unselected reservation was retained forever")
			}
			if modern {
				if _, present, err := store.recoveryFrontier(receipt.Replacement); err != nil || present {
					t.Fatal("closed obsolete lifecycle frontier was retained forever")
				}
			}
		})
	}
}

func TestRecoveryLifecyclePredecessorClosureCaptureAtOverwriteAndFault(t *testing.T) {
	for _, interrupt := range []bool{false, true} {
		t.Run(fmt.Sprintf("interrupt=%t", interrupt), func(t *testing.T) {
			store, receipt := lifecycleStoreFixture(t)
			completed := receipt.Predecessor
			completed.Checkpoint = JournalComplete
			selected, err := store.ReadCurrentJournal()
			if err != nil {
				t.Fatal(err)
			}
			if err := store.CompareAndSwapCurrentJournal(selected, mustJournalPayload(t, completed)); err != nil {
				t.Fatal(err)
			}
			if interrupt {
				store.fault = func(point string) error {
					if point == "before-recovery-completion" {
						return errors.New("closure capture interrupted")
					}
					return nil
				}
			}
			selected, err = store.ReadCurrentJournal()
			if err != nil {
				t.Fatal(err)
			}
			later := completed
			later.Transaction = "tx-later-ordinary"
			if err := store.CompareAndSwapCurrentJournal(selected, mustJournalPayload(t, later)); err != nil {
				t.Fatalf("witness capture prevented otherwise valid CAS: %v", err)
			}
			store.fault = nil
			terminal, present, err := store.readLifecycleRecord(receipt.Successor.Transaction)
			if err != nil {
				t.Fatal(err)
			}
			if interrupt {
				if present || !store.retirementCaptureFailed {
					t.Fatal("failed closure capture lacked conservative warning state")
				}
				if err := store.CleanupRecovery(later.Transaction); err == nil {
					t.Fatal("closure capture loss was reported as successful cleanup")
				}
				if live, err := store.ReadRecoveryReceipt(receipt.Successor.Transaction); err != nil || !live.Exists {
					t.Fatal("capture fault discarded unproven reservation")
				}
			} else if !present || terminal.PredecessorCompleted == nil || !bytes.Equal(mustJournalPayload(t, *terminal.PredecessorCompleted), selected.Payload) {
				t.Fatal("overwrite lost exact completed predecessor witness")
			}
		})
	}
}

func TestRecoveryLifecyclePredecessorClosureRejectsUnknownReferences(t *testing.T) {
	store, receipt := lifecycleStoreFixture(t)
	completed := receipt.Predecessor
	completed.Checkpoint = JournalComplete
	selected, err := store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapCurrentJournal(selected, mustJournalPayload(t, completed)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.configHome, "release-transition", "v2", ".journal.json.pending")
	testkit.WriteFile(t, path, []byte("unknown pending journal\n"), 0o600)
	if err := store.RecordCompletedRecovery(completed); err == nil {
		t.Fatal("unknown pending journal permitted closure capture")
	}
	if _, present, err := store.readLifecycleRecord(receipt.Successor.Transaction); err != nil || present {
		t.Fatal("invalid references gained a closure witness")
	}
}

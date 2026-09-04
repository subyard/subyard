package releasetransition

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func readCheckpointEvidence(
	store *POSIXV2Store,
	transaction TransactionID,
	step string,
	checkpoint EvidenceCheckpoint,
) (ProtectedSnapshot, error) {
	return store.ReadCheckpointEvidence(transaction, step, checkpoint)
}

func TestPOSIXV2StoreReadOnlyPathsCreateNothing(t *testing.T) {
	configHome := filepath.Join(t.TempDir(), "config")
	store, err := NewPOSIXV2Store(configHome)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := readCheckpointEvidence(store, "tx-a", "settings-v2", EvidenceCaptured)
	if err != nil {
		t.Fatal(err)
	}
	if ledger.Exists || evidence.Exists {
		t.Fatal("missing records were reported as present")
	}
	if _, err := os.Lstat(configHome); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only store created config root: %v", err)
	}
}

func TestPOSIXV2StoreCASPersistsProtectedLedgerAndRejectsStaleSnapshot(t *testing.T) {
	configHome := protectedConfigHome(t)
	store, err := NewPOSIXV2Store(configHome)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	first := []byte(`{"schemaVersion":2,"domains":{}}`)
	if err := store.CompareAndSwapLedger(missing, first); err != nil {
		t.Fatal(err)
	}
	stored, err := store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Exists || !bytes.Equal(stored.Payload, first) || stored.Fingerprint != fingerprintPayload(first) {
		t.Fatalf("stored ledger = %#v", stored)
	}
	assertProtectedFile(t, filepath.Join(configHome, "release-transition", "v2", "ledger.json"))
	assertProtectedDirectory(t, filepath.Join(configHome, "release-transition"))
	assertProtectedDirectory(t, filepath.Join(configHome, "release-transition", "v2"))
	if err := store.CompareAndSwapLedger(missing, []byte(`{"schemaVersion":2}`)); !errors.Is(err, ErrProtectedStoreStale) {
		t.Fatalf("stale CAS error = %v", err)
	}
	second := []byte(`{"schemaVersion":2,"domains":{"settings":{"epoch":1,"applied":[]}}}`)
	if err := store.CompareAndSwapLedger(stored, second); err != nil {
		t.Fatal(err)
	}
	updated, err := store.ReadLedger()
	if err != nil || !bytes.Equal(updated.Payload, second) {
		t.Fatalf("updated ledger = %#v, err=%v", updated, err)
	}
}

func TestPOSIXV2StoreCurrentJournalUsesProtectedCAS(t *testing.T) {
	store, err := NewPOSIXV2Store(protectedConfigHome(t))
	if err != nil {
		t.Fatal(err)
	}
	missing, err := store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	first := []byte(`{"schemaVersion":2,"transaction":"tx-a"}`)
	if err := store.CompareAndSwapCurrentJournal(missing, first); err != nil {
		t.Fatal(err)
	}
	current, err := store.ReadCurrentJournal()
	if err != nil || !bytes.Equal(current.Payload, first) {
		t.Fatalf("journal = %#v, err=%v", current, err)
	}
	second := []byte(`{"schemaVersion":2,"transaction":"tx-b"}`)
	if err := store.CompareAndSwapCurrentJournal(current, second); err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapCurrentJournal(current, first); !errors.Is(err, ErrProtectedStoreStale) {
		t.Fatalf("stale journal CAS error = %v", err)
	}
}

func TestPOSIXV2StoreCheckpointEvidenceIsWriteOnceAndIdempotentForExactBytes(t *testing.T) {
	configHome := protectedConfigHome(t)
	store, err := NewPOSIXV2Store(configHome)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"schemaVersion":1,"step":"settings-v2"}`)
	if err := store.CreateCheckpointEvidence("tx-a", "settings-v2", EvidenceCaptured, payload); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateCheckpointEvidence("tx-a", "settings-v2", EvidenceCaptured, payload); err != nil {
		t.Fatalf("exact evidence retry: %v", err)
	}
	if err := store.CreateCheckpointEvidence(
		"tx-a", "settings-v2", EvidenceCaptured, []byte(`{"different":true}`),
	); !errors.Is(err, ErrProtectedStoreExists) {
		t.Fatalf("foreign evidence error = %v", err)
	}
	stored, err := readCheckpointEvidence(store, "tx-a", "settings-v2", EvidenceCaptured)
	if err != nil || !bytes.Equal(stored.Payload, payload) {
		t.Fatalf("stored evidence=%#v err=%v", stored, err)
	}
	assertProtectedFile(t, filepath.Join(
		configHome, "release-transition", "v2", "transactions", "tx-a", "evidence",
		"settings-v2.captured.json",
	))
}

func TestPOSIXV2StoreSupersededJournalIsWriteOnceAndIdempotentForExactBytes(t *testing.T) {
	store, err := NewPOSIXV2Store(protectedConfigHome(t))
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"schemaVersion":1,"authorizationPlan":"plan-v1-source"}`)
	if err := store.CreateSupersededJournal("tx-new", payload); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSupersededJournal("tx-new", payload); err != nil {
		t.Fatalf("exact archive retry: %v", err)
	}
	if err := store.CreateSupersededJournal("tx-new", []byte(`{"different":true}`)); !errors.Is(err, ErrProtectedStoreExists) {
		t.Fatalf("foreign archive error = %v", err)
	}
	stored, err := store.ReadSupersededJournal("tx-new")
	if err != nil || !bytes.Equal(stored.Payload, payload) {
		t.Fatalf("stored archive=%#v err=%v", stored, err)
	}
	assertProtectedFile(t, filepath.Join(
		store.configHome, "release-transition", "v2", "transactions", "tx-new",
		"superseded-journal.json",
	))
}

func TestPOSIXV2StoreReplacementGraphAdmissionRejectsUnsafeProposedEdgeWithoutMutation(t *testing.T) {
	authorization := PlanToken("plan-v1-" + strings.Repeat("c", 64))
	tests := []struct {
		name     string
		proposed TransactionID
		setup    func(*testing.T, *POSIXV2Store)
	}{
		{name: "source already has predecessor", proposed: "tx-proposed", setup: func(t *testing.T, store *POSIXV2Store) {
			createStoreTransaction(t, store, "tx-older")
			createStoreSupersession(t, store, "tx-source", "tx-older", authorization)
		}},
		{name: "foreign successor already points to source", proposed: "tx-proposed", setup: func(t *testing.T, store *POSIXV2Store) {
			foreignAuthorization := PlanToken("plan-v1-" + strings.Repeat("d", 64))
			createStoreSupersession(t, store, "tx-foreign", "tx-source", foreignAuthorization)
		}},
		{name: "allocation exceeds graph bound", proposed: "tx-proposed", setup: func(t *testing.T, store *POSIXV2Store) {
			for index := 0; index < maxTransactionGraphEntries-1; index++ {
				createStoreTransaction(t, store, TransactionID(fmt.Sprintf("tx-bound-%03d", index)))
			}
		}},
		{name: "proposed transaction collides", proposed: "tx-proposed", setup: func(t *testing.T, store *POSIXV2Store) {
			createStoreTransaction(t, store, "tx-proposed")
		}},
		{name: "proposed transaction is source", proposed: "tx-source"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configHome := protectedConfigHome(t)
			store, err := NewPOSIXV2Store(configHome)
			if err != nil {
				t.Fatal(err)
			}
			createStoreTransaction(t, store, "tx-source")
			if test.setup != nil {
				test.setup(t, store)
			}
			replacement := storeSupersessionRecord(t, "tx-source", authorization).Replacement
			before := snapshotStoreV2Tree(t, configHome)
			if _, err := store.ResolveSupersededJournalTransaction(
				test.proposed, replacement, authorization,
			); err == nil {
				t.Fatal("unsafe proposed predecessor edge was accepted")
			}
			after := snapshotStoreV2Tree(t, configHome)
			if !bytes.Equal(after, before) {
				t.Fatalf("failed admission changed the protected tree\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

func TestPOSIXV2StoreReplacementGraphAdmissionRejectsPreActivationReplacement(t *testing.T) {
	store, err := NewPOSIXV2Store(protectedConfigHome(t))
	if err != nil {
		t.Fatal(err)
	}
	authorization := PlanToken("plan-v1-" + strings.Repeat("c", 64))
	createStoreTransaction(t, store, "tx-source")
	replacement := storeSupersessionRecord(t, "tx-source", authorization).Replacement
	replacement.Reason = JournalReplacementPreActivationPlanStale
	replacement.SourceVersion = ""
	before := snapshotStoreV2Tree(t, store.configHome)
	if _, err := store.ResolveSupersededJournalTransaction(
		"tx-proposed", replacement, authorization,
	); err == nil {
		t.Fatal("pre-activation replacement was admitted to the archive graph")
	}
	after := snapshotStoreV2Tree(t, store.configHome)
	if !bytes.Equal(after, before) {
		t.Fatalf("rejected pre-activation replacement changed the protected tree\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestPOSIXV2StoreReplacementGraphAdmissionReusesExactArchive(t *testing.T) {
	store, err := NewPOSIXV2Store(protectedConfigHome(t))
	if err != nil {
		t.Fatal(err)
	}
	authorization := PlanToken("plan-v1-" + strings.Repeat("c", 64))
	createStoreTransaction(t, store, "tx-source")
	createStoreSupersession(t, store, "tx-existing", "tx-source", authorization)
	replacement := storeSupersessionRecord(t, "tx-source", authorization).Replacement
	resolved, err := store.ResolveSupersededJournalTransaction(
		"tx-proposed", replacement, authorization,
	)
	if err != nil || resolved != "tx-existing" {
		t.Fatalf("exact archive resolution = %q, err=%v", resolved, err)
	}
}

func TestPOSIXV2StoreKeepsCheckpointEvidenceAndRecoverySeparate(t *testing.T) {
	store, err := NewPOSIXV2Store(protectedConfigHome(t))
	if err != nil {
		t.Fatal(err)
	}
	for checkpoint, payload := range map[EvidenceCheckpoint][]byte{
		EvidenceCaptured: []byte(`{"checkpoint":"captured"}`),
		EvidenceApplied:  []byte(`{"checkpoint":"applied"}`),
		EvidenceVerified: []byte(`{"checkpoint":"verified"}`),
	} {
		if err := store.CreateCheckpointEvidence("tx-a", "settings-v2", checkpoint, payload); err != nil {
			t.Fatal(err)
		}
		actual, err := readCheckpointEvidence(store, "tx-a", "settings-v2", checkpoint)
		if err != nil || !bytes.Equal(actual.Payload, payload) {
			t.Fatalf("checkpoint %s evidence = %#v, err=%v", checkpoint, actual, err)
		}
	}
	recovery := []byte(`{"schemaVersion":1,"before":[]}`)
	if err := store.CreateRecovery("tx-a", "settings-v2", recovery); err != nil {
		t.Fatal(err)
	}
	actual, err := store.ReadRecovery("tx-a", "settings-v2")
	if err != nil || !bytes.Equal(actual.Payload, recovery) {
		t.Fatalf("recovery = %#v, err=%v", actual, err)
	}
}

func TestPOSIXV2StoreCleanupRetainsCurrentRecoveryTransaction(t *testing.T) {
	configHome := protectedConfigHome(t)
	store, err := NewPOSIXV2Store(configHome)
	if err != nil {
		t.Fatal(err)
	}
	for _, transaction := range []TransactionID{"tx-old", "tx-current"} {
		if err := store.CreateCheckpointEvidence(
			transaction, "settings-v2", EvidenceVerified, []byte(`{"checkpoint":"verified"}`),
		); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.CleanupTransactions("tx-current"); err != nil {
		t.Fatal(err)
	}
	old, err := readCheckpointEvidence(store, "tx-old", "settings-v2", EvidenceVerified)
	if err != nil || old.Exists {
		t.Fatalf("old evidence = %#v, err=%v", old, err)
	}
	current, err := readCheckpointEvidence(store, "tx-current", "settings-v2", EvidenceVerified)
	if err != nil || !current.Exists {
		t.Fatalf("current evidence = %#v, err=%v", current, err)
	}
}

func TestPOSIXV2StoreCleanupRetainsArchivedSourceAndRemovesOnlyUnrelatedTransactions(t *testing.T) {
	configHome := protectedConfigHome(t)
	store, err := NewPOSIXV2Store(configHome)
	if err != nil {
		t.Fatal(err)
	}
	authorization := PlanToken("plan-v1-" + strings.Repeat("c", 64))
	writeStoreCurrentJournal(t, store, "tx-current", authorization)
	createStoreSupersession(t, store, "tx-current", "tx-source", authorization)
	for _, transaction := range []TransactionID{"tx-source", "tx-unrelated"} {
		if err := store.CreateCheckpointEvidence(
			transaction, "ledger-step", EvidenceVerified, []byte("evidence\n"),
		); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.CleanupTransactions("tx-current"); err != nil {
		t.Fatal(err)
	}
	archive, err := store.ReadSupersededJournal("tx-current")
	if err != nil || !archive.Exists {
		t.Fatalf("current archive = %#v, err=%v", archive, err)
	}
	source, err := readCheckpointEvidence(store, "tx-source", "ledger-step", EvidenceVerified)
	if err != nil || !source.Exists {
		t.Fatalf("referenced source = %#v, err=%v", source, err)
	}
	unrelated, err := readCheckpointEvidence(store, "tx-unrelated", "ledger-step", EvidenceVerified)
	if err != nil || unrelated.Exists {
		t.Fatalf("unrelated transaction = %#v, err=%v", unrelated, err)
	}
}

func TestPOSIXV2StoreCleanupRejectsInvalidPredecessorGraphBeforeDeletion(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, *POSIXV2Store, PlanToken)
	}{
		{name: "malformed archive", setup: func(t *testing.T, store *POSIXV2Store, _ PlanToken) {
			if err := store.CreateSupersededJournal("tx-current", []byte("{\n")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "cycle", setup: func(t *testing.T, store *POSIXV2Store, authorization PlanToken) {
			createStoreSupersession(t, store, "tx-current", "tx-source", authorization)
			createStoreSupersession(t, store, "tx-source", "tx-current", authorization)
		}},
		{name: "nested predecessor", setup: func(t *testing.T, store *POSIXV2Store, authorization PlanToken) {
			createStoreSupersession(t, store, "tx-current", "tx-source", authorization)
			createStoreSupersession(t, store, "tx-source", "tx-older", authorization)
			if err := store.CreateCheckpointEvidence(
				"tx-older", "ledger-step", EvidenceVerified, []byte("older\n"),
			); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "unrelated nested predecessor", setup: func(t *testing.T, store *POSIXV2Store, authorization PlanToken) {
			createStoreTransaction(t, store, "tx-older")
			createStoreSupersession(t, store, "tx-unrelated", "tx-source", authorization)
			createStoreSupersession(t, store, "tx-source", "tx-older", authorization)
		}},
		{name: "unrelated missing predecessor", setup: func(t *testing.T, store *POSIXV2Store, authorization PlanToken) {
			createStoreSupersession(t, store, "tx-orphan", "tx-missing", authorization)
		}},
		{name: "shared predecessor", setup: func(t *testing.T, store *POSIXV2Store, authorization PlanToken) {
			createStoreSupersession(t, store, "tx-current", "tx-source", authorization)
			createStoreSupersession(t, store, "tx-other", "tx-source", authorization)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configHome := protectedConfigHome(t)
			store, err := NewPOSIXV2Store(configHome)
			if err != nil {
				t.Fatal(err)
			}
			authorization := PlanToken("plan-v1-" + strings.Repeat("c", 64))
			writeStoreCurrentJournal(t, store, "tx-current", authorization)
			for _, transaction := range []TransactionID{"tx-source", "tx-unrelated"} {
				if err := store.CreateCheckpointEvidence(
					transaction, "ledger-step", EvidenceVerified, []byte("evidence\n"),
				); err != nil {
					t.Fatal(err)
				}
			}
			test.setup(t, store, authorization)
			before := snapshotStoreV2Tree(t, configHome)
			if err := store.CleanupTransactions("tx-current"); err == nil {
				t.Fatal("invalid predecessor graph was accepted")
			}
			after := snapshotStoreV2Tree(t, configHome)
			if !bytes.Equal(after, before) {
				t.Fatalf("failed cleanup deleted before validating graph\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

func TestPOSIXV2StoreCleanupKeepsWholeComponentAtDeletionBound(t *testing.T) {
	configHome := protectedConfigHome(t)
	store, err := NewPOSIXV2Store(configHome)
	if err != nil {
		t.Fatal(err)
	}
	authorization := PlanToken("plan-v1-" + strings.Repeat("c", 64))
	writeStoreCurrentJournal(t, store, "tx-current", authorization)
	for index := 0; index < 31; index++ {
		createStoreTransaction(t, store, TransactionID(fmt.Sprintf("tx-a-%02d", index)))
	}
	createStoreTransaction(t, store, "tx-b-predecessor")
	createStoreSupersession(t, store, "tx-c-successor", "tx-b-predecessor", authorization)

	if err := store.CleanupTransactions("tx-current"); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 31; index++ {
		transaction := TransactionID(fmt.Sprintf("tx-a-%02d", index))
		if _, err := os.Stat(filepath.Join(
			configHome, "release-transition", "v2", "transactions", string(transaction),
		)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("singleton %q was not removed: %v", transaction, err)
		}
	}
	for _, transaction := range []TransactionID{"tx-b-predecessor", "tx-c-successor"} {
		if _, err := os.Stat(filepath.Join(
			configHome, "release-transition", "v2", "transactions", string(transaction),
		)); err != nil {
			t.Fatalf("component member %q was split at cleanup bound: %v", transaction, err)
		}
	}
	current, err := store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	journal, err := ParseJournal(current.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.inspectTransactionGraph("tx-current", &journal); err != nil {
		t.Fatalf("bounded cleanup left an invalid graph: %v", err)
	}
}

func writeStoreCurrentJournal(
	t *testing.T,
	store *POSIXV2Store,
	transaction TransactionID,
	authorization PlanToken,
) {
	t.Helper()
	journal := validJournal(ReleasePair{From: "release-a", Target: "release-b"})
	journal.Transaction = transaction
	journal.AuthorizationPlan = authorization
	journal.Checkpoint = JournalComplete
	journal.Steps = nil
	journal.IntentDigest = bindJournalIntent(
		journal.AuthorizationPlan, journal.ResumePlan, journal.ObservationScope, journal.Steps,
	)
	payload, err := MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapCurrentJournal(missing, payload); err != nil {
		t.Fatal(err)
	}
}

func createStoreSupersession(
	t *testing.T,
	store *POSIXV2Store,
	successor TransactionID,
	predecessor TransactionID,
	authorization PlanToken,
) {
	t.Helper()
	record := storeSupersessionRecord(t, predecessor, authorization)
	payload, err := MarshalSupersededJournal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSupersededJournal(successor, payload); err != nil {
		t.Fatal(err)
	}
}

func storeSupersessionRecord(
	t *testing.T,
	predecessor TransactionID,
	authorization PlanToken,
) SupersededJournalRecord {
	t.Helper()
	journal := validJournal(ReleasePair{
		From: v2PostActivationPreviousRelease, Target: v2PostActivationSourceRelease,
	})
	journal.Transaction = predecessor
	journal.Checkpoint = JournalReconciling
	journal.Steps = nil
	journal.IntentDigest = bindJournalIntent(
		journal.AuthorizationPlan, journal.ResumePlan, journal.ObservationScope, journal.Steps,
	)
	journalPayload, err := MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	return SupersededJournalRecord{
		SchemaVersion: SupersededJournalSchemaV1, AuthorizationPlan: authorization,
		Replacement: JournalReplacement{
			Transaction: predecessor, Fingerprint: fingerprintPayload(journalPayload),
			Reason: JournalReplacementPostActivationScopeV0111, SourceVersion: "0.11.1",
		},
		Journal: journal,
	}
}

func createStoreTransaction(t *testing.T, store *POSIXV2Store, transaction TransactionID) {
	t.Helper()
	if err := store.CreateCheckpointEvidence(
		transaction, "ledger-step", EvidenceVerified, []byte("evidence\n"),
	); err != nil {
		t.Fatal(err)
	}
}

func snapshotStoreV2Tree(t *testing.T, configHome string) []byte {
	t.Helper()
	root := filepath.Join(configHome, "release-transition", "v2")
	var snapshot bytes.Buffer
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		snapshot.WriteString(relative)
		snapshot.WriteByte('\t')
		snapshot.WriteString(info.Mode().String())
		snapshot.WriteByte('\n')
		if info.Mode().IsRegular() {
			payload, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			snapshot.Write(payload)
			snapshot.WriteByte('\n')
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return snapshot.Bytes()
}

func TestPOSIXV2StoreRecoversDeterministicPendingCreate(t *testing.T) {
	configHome := protectedConfigHome(t)
	store, err := NewPOSIXV2Store(configHome)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"schemaVersion":1,"step":"settings-v2"}`)
	injected := errors.New("injected crash after pending fsync")
	store.fault = func(point string) error {
		if point == "after-pending-fsync" {
			return injected
		}
		return nil
	}
	if err := store.CreateCheckpointEvidence(
		"tx-a", "settings-v2", EvidenceCaptured, payload,
	); !errors.Is(err, injected) {
		t.Fatalf("injected create error = %v", err)
	}
	store.fault = nil
	if err := store.CreateCheckpointEvidence(
		"tx-a", "settings-v2", EvidenceCaptured, payload,
	); err != nil {
		t.Fatalf("resume pending create: %v", err)
	}
	path := filepath.Join(configHome, "release-transition", "v2", "transactions", "tx-a", "evidence")
	assertProtectedFile(t, filepath.Join(path, "settings-v2.captured.json"))
	if _, err := os.Lstat(filepath.Join(path, ".settings-v2.captured.json.pending")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending evidence residue = %v", err)
	}
}

func TestPOSIXV2StoreLedgerCASConvergesAcrossPublicationFaults(t *testing.T) {
	for _, point := range []string{"after-pending-fsync", "after-publish-before-dir-fsync"} {
		t.Run(point, func(t *testing.T) {
			store, err := NewPOSIXV2Store(protectedConfigHome(t))
			if err != nil {
				t.Fatal(err)
			}
			expected, err := store.ReadLedger()
			if err != nil {
				t.Fatal(err)
			}
			payload := []byte(`{"schemaVersion":2,"domains":{}}`)
			injected := errors.New("injected ledger publication fault")
			store.fault = func(actual string) error {
				if actual == point {
					return injected
				}
				return nil
			}
			if err := store.CompareAndSwapLedger(expected, payload); !errors.Is(err, injected) {
				t.Fatalf("fault error = %v", err)
			}
			store.fault = nil
			current, err := store.ReadLedger()
			if err != nil {
				t.Fatal(err)
			}
			if point == "after-pending-fsync" {
				if current.Exists {
					t.Fatalf("pre-publication ledger = %#v", current)
				}
				if err := store.CompareAndSwapLedger(expected, payload); err != nil {
					t.Fatalf("resume pending ledger: %v", err)
				}
			} else {
				if !bytes.Equal(current.Payload, payload) {
					t.Fatalf("post-publication ledger = %#v", current)
				}
				if err := store.CompareAndSwapLedger(expected, payload); !errors.Is(err, ErrProtectedStoreStale) {
					t.Fatalf("post-publication retry error = %v", err)
				}
			}
		})
	}
}

func TestPOSIXV2StorePinsParentDirectoryAcrossSymlinkSwap(t *testing.T) {
	configHome := protectedConfigHome(t)
	store, err := NewPOSIXV2Store(configHome)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	store.fault = func(point string) error {
		if point != "before-publish" {
			return nil
		}
		root := filepath.Join(configHome, "release-transition", "v2")
		if err := os.Rename(root, root+"-held"); err != nil {
			return err
		}
		return os.Symlink(outside, root)
	}
	if err := store.CompareAndSwapLedger(missing, []byte(`{"schemaVersion":2,"domains":{}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "ledger.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("store escaped through swapped parent: %v", err)
	}
	assertProtectedFile(t, filepath.Join(configHome, "release-transition", "v2-held", "ledger.json"))
}

func TestPOSIXV2StoreRejectsUnsafeFilesAndParents(t *testing.T) {
	t.Run("symlink ledger", func(t *testing.T) {
		configHome := protectedConfigHome(t)
		root := filepath.Join(configHome, "release-transition", "v2")
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(configHome, "target")
		if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(root, "ledger.json")); err != nil {
			t.Fatal(err)
		}
		store, _ := NewPOSIXV2Store(configHome)
		if _, err := store.ReadLedger(); err == nil {
			t.Fatal("symlink ledger was accepted")
		}
	})

	t.Run("hard linked ledger", func(t *testing.T) {
		configHome := protectedConfigHome(t)
		root := filepath.Join(configHome, "release-transition", "v2")
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		ledger := filepath.Join(root, "ledger.json")
		if err := os.WriteFile(ledger, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(ledger, filepath.Join(root, "alias")); err != nil {
			t.Fatal(err)
		}
		store, _ := NewPOSIXV2Store(configHome)
		if _, err := store.ReadLedger(); err == nil {
			t.Fatal("hard-linked ledger was accepted")
		}
	})

	t.Run("unsafe parent", func(t *testing.T) {
		configHome := protectedConfigHome(t)
		root := filepath.Join(configHome, "release-transition")
		if err := os.Mkdir(root, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(root, 0o777); err != nil {
			t.Fatal(err)
		}
		store, _ := NewPOSIXV2Store(configHome)
		if _, err := store.ReadLedger(); err == nil {
			t.Fatal("unsafe parent was accepted")
		}
	})
}

func TestPOSIXV2StoreRejectsUnboundedPayloadAndUnsafeIdentifiers(t *testing.T) {
	store, err := NewPOSIXV2Store(protectedConfigHome(t))
	if err != nil {
		t.Fatal(err)
	}
	missing, err := store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapLedger(missing, bytes.Repeat([]byte{'x'}, MaxProtectedRecordBytes+1)); err == nil {
		t.Fatal("unbounded ledger was accepted")
	}
	if err := store.CreateCheckpointEvidence("../tx", "step", EvidenceCaptured, []byte("{}")); err == nil {
		t.Fatal("unsafe transaction ID was accepted")
	}
	if _, err := readCheckpointEvidence(store, "tx", "../step", EvidenceCaptured); err == nil {
		t.Fatal("unsafe step ID was accepted")
	}
	if _, err := NewPOSIXV2Store("relative/config"); err == nil {
		t.Fatal("relative config home was accepted")
	}
}

func TestPOSIXV2StoreUsesSharedMigrationUpdateLock(t *testing.T) {
	configHome := protectedConfigHome(t)
	store, err := NewPOSIXV2Store(configHome)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := store.Lock()
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	path := filepath.Join(configHome, "migrations", "update.lock")
	assertProtectedFile(t, path)
}

func protectedConfigHome(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertProtectedFile(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 ||
		stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 {
		t.Fatalf("unsafe protected file %s: mode=%v stat=%#v", path, info.Mode(), stat)
	}
}

func assertProtectedDirectory(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm() != 0o700 || stat.Uid != uint32(os.Getuid()) {
		t.Fatalf("unsafe protected directory %s: mode=%v stat=%#v", path, info.Mode(), stat)
	}
}

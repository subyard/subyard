package releasetransition

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func readCheckpointEvidence(
	store *POSIXV2Store,
	transaction TransactionID,
	step string,
	checkpoint EvidenceCheckpoint,
) (ProtectedSnapshot, error) {
	name, err := checkpointEvidenceName(transaction, step, checkpoint)
	if err != nil {
		return ProtectedSnapshot{}, err
	}
	return store.readRecord(
		[]string{"release-transition", "v2", "transactions", string(transaction), "evidence"},
		name,
	)
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

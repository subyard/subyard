package releasetransition

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestMigrationHistoryCleanupRetainsClosedEvidenceWithoutPromotingAuthority(t *testing.T) {
	for _, checkpointed := range []bool{false, true} {
		name := "legacy"
		if checkpointed {
			name = "checkpoint"
		}
		t.Run(name, func(t *testing.T) {
			transition, store := completedSourceMetadataFixture(t)
			if checkpointed {
				state, err := store.ReadMigrationLedger(transition.registry, transition.registryDigest)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.ConvertMigrationLedger(state, transition.registry, transition.registryDigest, transition.registry, transition.registryDigest); err != nil {
					t.Fatal(err)
				}
			}
			// The old journal is replaced normally. Its supported evidence triples and
			// typed recovery are retention evidence even without a historical anchor.
			writeStoreCurrentJournal(t, store, "tx-new", PlanToken("plan-v1-"+string(digestA)))
			before := snapshotStoreV2Tree(t, store.configHome)
			if err := store.cleanupClosedMigrationTransactions("tx-new", transition.registry, transition.registryDigest, ReleaseLinks{Active: "release-a"}); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, snapshotStoreV2Tree(t, store.configHome)) {
				t.Fatal("cleanup removed or rewrote closed old artifacts")
			}
			// The current transaction's own evidence is independently retained too.
			old, err := store.sourceRetainedTerminalEvidence("tx-test-001", transition.registry)
			if err != nil || len(old.Steps) == 0 {
				t.Fatalf("old closure lost: %v", err)
			}
		})
	}
}

func TestMigrationHistoryCleanupPreservesUnknownIncompleteAndConflictingBytes(t *testing.T) {
	for _, mutant := range []string{"unknown", "incomplete", "conflicting"} {
		t.Run(mutant, func(t *testing.T) {
			transition, store := completedSourceMetadataFixture(t)
			writeStoreCurrentJournal(t, store, "tx-new", PlanToken("plan-v1-"+string(digestA)))
			root := filepath.Join(store.configHome, "release-transition", "v2", "transactions", "tx-test-001", "evidence")
			files, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			switch mutant {
			case "unknown":
				testkit.WriteFile(t, filepath.Join(root, "unknown.json"), []byte("unknown\n"), 0o600)
			case "incomplete":
				if err := os.Remove(filepath.Join(root, files[0].Name())); err != nil {
					t.Fatal(err)
				}
			case "conflicting":
				path := filepath.Join(root, files[0].Name())
				payload, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, append(payload, '\n'), 0o600)
			}
			before := snapshotStoreV2Tree(t, store.configHome)
			if err := store.cleanupClosedMigrationTransactions("tx-new", transition.registry, transition.registryDigest, ReleaseLinks{Active: "release-a"}); err == nil {
				t.Fatal("unsupported closure did not leave cleanup pending")
			}
			if !bytes.Equal(before, snapshotStoreV2Tree(t, store.configHome)) {
				t.Fatal("cleanup deleted unsupported bytes")
			}
		})
	}
}

func TestMigrationHistoryCleanupCollectsOnlyUnreferencedClosedArtifacts(t *testing.T) {
	for _, tc := range []struct {
		name       string
		checkpoint bool
		endpoints  ReleaseLinks
		retained   bool
	}{
		{"current-runtime", false, ReleaseLinks{Active: "release-a"}, true},
		{"previous-runtime", false, ReleaseLinks{Active: "release-b", Previous: releaseIDPointer("release-a")}, true},
		{"closed-reference-free", false, ReleaseLinks{Active: "release-b", Previous: releaseIDPointer("release-c")}, false},
		{"pinned-projection", true, ReleaseLinks{Active: "release-b", Previous: releaseIDPointer("release-c")}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transition, store := completedSourceMetadataFixture(t)
			if tc.checkpoint {
				state, err := store.ReadMigrationLedger(transition.registry, transition.registryDigest)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.ConvertMigrationLedger(state, transition.registry, transition.registryDigest, transition.registry, transition.registryDigest); err != nil {
					t.Fatal(err)
				}
			}
			writeStoreCurrentJournal(t, store, "tx-new", PlanToken("plan-v1-"+string(digestA)))
			old := filepath.Join(store.configHome, "release-transition", "v2", "transactions", "tx-test-001")
			before := snapshotStoreV2Tree(t, store.configHome)
			if err := store.cleanupClosedMigrationTransactions("tx-new", transition.registry, transition.registryDigest, tc.endpoints); err != nil {
				t.Fatal(err)
			}
			_, err := os.Stat(old)
			if tc.retained {
				if err != nil || !bytes.Equal(before, snapshotStoreV2Tree(t, store.configHome)) {
					t.Fatalf("referenced history changed: %v", err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatalf("closed reference-free history not collected: %v", err)
			}
		})
	}
}

func TestMigrationHistoryCleanupRetainsLiveRecoveryReference(t *testing.T) {
	transition, store := completedSourceMetadataFixture(t)
	writeStoreCurrentJournal(t, store, "tx-new", PlanToken("plan-v1-"+string(digestA)))
	receipt := recoveryReceiptFixture(t)
	receipt.Predecessor.Transaction = "tx-test-001"
	payload, err := MarshalJournal(receipt.Predecessor)
	if err != nil {
		t.Fatal(err)
	}
	receipt.Replacement = ActivationOnlyRecoveryRequest{Transaction: "tx-test-001", Fingerprint: fingerprintPayload(payload)}
	payload, err = MarshalRecoveryReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.createImmutable(recoveryReceiptParts, string(receipt.Successor.Transaction)+".json", payload); err != nil {
		t.Fatal(err)
	}
	before := snapshotStoreV2Tree(t, store.configHome)
	if err := store.cleanupClosedMigrationTransactions("tx-new", transition.registry, transition.registryDigest, ReleaseLinks{Active: "release-b"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, snapshotStoreV2Tree(t, store.configHome)) {
		t.Fatal("live recovery reference lost its predecessor")
	}
}

func TestMigrationHistoryCleanupDoesNotRetainIntermediateProjectionPin(t *testing.T) {
	transition, store := completedSourceMetadataFixture(t)
	state, err := store.ReadMigrationLedger(transition.registry, transition.registryDigest)
	if err != nil {
		t.Fatal(err)
	}
	baseline := BaselineLedgerV2(transition.registry)
	path, err := transition.registry.PendingPath(baseline)
	if err != nil {
		t.Fatal(err)
	}
	intermediate, err := baseline.Advance(transition.registry, path[0])
	if err != nil {
		t.Fatal(err)
	}
	payload, _, err := MarshalLedgerV2(intermediate, transition.registry)
	if err != nil {
		t.Fatal(err)
	}
	projection := protectedSnapshotFromPayload(payload)
	epochs, err := decodeLegacyLedgerEpochs(payload)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := newLedgerCheckpointV1(state.Ledger, transition.registry, transition.registryDigest, projection, epochs)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapLedger(state.ProjectionSnapshot, payload); err != nil {
		t.Fatal(err)
	}
	payload, _, err = MarshalLedgerCheckpointV1(checkpoint, transition.registry, transition.registryDigest, projection)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.compareAndSwap([]string{"release-transition", "v2"}, migrationLedgerCheckpointName, absentProtectedSnapshot(), payload); err != nil {
		t.Fatal(err)
	}
	writeStoreCurrentJournal(t, store, "tx-new", PlanToken("plan-v1-"+string(digestA)))
	if err := store.cleanupClosedMigrationTransactions("tx-new", transition.registry, transition.registryDigest, ReleaseLinks{Active: "release-b"}); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(store.configHome, "release-transition", "v2", "transactions", "tx-test-001")
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("intermediate pin retained unrelated closed history: %v", err)
	}
}

package releasetransition

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func rollbackArtifactFixture(t *testing.T) (*POSIXV2Store, TransactionID, []rollbackArtifact) {
	t.Helper()
	home := testkit.TempDir(t)
	store, err := NewPOSIXV2Store(home)
	if err != nil {
		t.Fatal(err)
	}
	journal := validJournal(ReleasePair{From: "source", Target: "target"})
	step := JournalStep{ID: "future-migration.object", Migration: "future-migration", Resource: "object", Decision: DecisionTransform, Expected: digestA, Desired: digestB}
	for _, checkpoint := range []EvidenceCheckpoint{EvidenceCaptured, EvidenceApplied, EvidenceVerified} {
		observed := step.Desired
		if checkpoint == EvidenceCaptured {
			observed = step.Expected
		}
		evidence := evidenceFor(journal, step, checkpoint, observed, "")
		payload, err := MarshalEvidence(evidence)
		if err != nil {
			t.Fatal(err)
		}
		name, err := checkpointEvidenceName(journal.Transaction, step.ID, checkpoint)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.createImmutable([]string{"release-transition", "v2", "transactions", string(journal.Transaction), "evidence"}, name, payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.CreateRecovery(journal.Transaction, step.ID, []byte("future typed recovery\n")); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateNativeIdentity(journal.Transaction, step.ID, []byte("future native identity\n")); err != nil {
		t.Fatal(err)
	}
	inventory, err := store.rollbackArtifactInventory(journal.Transaction)
	if err != nil {
		t.Fatal(err)
	}
	return store, journal.Transaction, inventory
}

func TestRollbackArtifactInventoryIncludesNativeSiblingsWithoutLoadingKinds(t *testing.T) {
	store, transaction, inventory := rollbackArtifactFixture(t)
	if len(inventory) != 9 {
		t.Fatalf("inventory = %+v", inventory)
	}
	if !slices.ContainsFunc(inventory, func(artifact rollbackArtifact) bool {
		return artifact.Path == "native-identities/future-migration.object.json"
	}) {
		t.Fatal("native identity sibling is missing")
	}
	if err := store.checkRollbackArtifactInventory(transaction, inventory); err != nil {
		t.Fatal(err)
	}
	fresh, err := NewPOSIXV2Store(store.configHome)
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.checkRollbackArtifactInventory(transaction, inventory); err != nil {
		t.Fatal(err)
	}
}

func TestRollbackArtifactInventoryRefusesUnclosedOrChangedBytes(t *testing.T) {
	for _, change := range []string{"missing", "changed", "extra", "native-changed", "unsafe-mode", "symlink", "pending", "unknown-directory", "foreign-evidence", "empty-directory"} {
		t.Run(change, func(t *testing.T) {
			store, transaction, inventory := rollbackArtifactFixture(t)
			root := filepath.Join(store.configHome, "release-transition", "v2", "transactions", string(transaction))
			name := filepath.Join(root, "recovery", "future-migration.object.json")
			switch change {
			case "missing":
				if err := os.Remove(name); err != nil {
					t.Fatal(err)
				}
			case "changed":
				testkit.WriteFile(t, name, []byte("changed"), 0o600)
			case "extra":
				testkit.WriteFile(t, filepath.Join(root, "recovery", "extra.json"), []byte("extra"), 0o600)
			case "native-changed":
				testkit.WriteFile(t, filepath.Join(store.configHome, "reconcile", "native-identities", string(transaction), "future-migration.object.json"), []byte("changed"), 0o600)
			case "unsafe-mode":
				if err := os.Chmod(name, 0o644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(name); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("/dev/null", name); err != nil {
					t.Fatal(err)
				}
			case "pending":
				testkit.WriteFile(t, filepath.Join(root, "recovery", ".future-migration.object.json.pending"), []byte("pending"), 0o600)
			case "unknown-directory":
				if err := os.Mkdir(filepath.Join(root, "unknown"), 0o700); err != nil {
					t.Fatal(err)
				}
			case "foreign-evidence":
				path := filepath.Join(root, "evidence", "future-migration.object.verified.json")
				payload, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				evidence, err := ParseEvidence(payload)
				if err != nil {
					t.Fatal(err)
				}
				evidence.Transaction = "foreign-transaction"
				payload, err = MarshalEvidence(evidence)
				if err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, payload, 0o600)
			case "empty-directory":
				// Removing an empty namespace is a different inventory, too.
				path := filepath.Join(root, "recovery")
				if err := os.Remove(name); err != nil {
					t.Fatal(err)
				}
				inventory, _ = store.rollbackArtifactInventory(transaction)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.checkRollbackArtifactInventory(transaction, inventory); err == nil {
				t.Fatal("changed or unsafe artifact set was accepted")
			}
		})
	}
}

package releasetransition

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Subyard/Subyard/internal/testkit"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These canonical receipts were captured from the pre-retirement test writer.
// Readers are seeded directly: no current engine creates a historical replacement.
func migrationOwnerReaderFixture(t *testing.T, previous bool) (*V2Transition, MigrationOwnerReceiptV1, *ReleaseLinks) {
	t.Helper()
	name := "migration-owner-receipt.json"
	if previous {
		name = "migration-owner-previous-receipt.json"
	}
	payload, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := ParseMigrationOwnerReceipt(payload)
	if err != nil {
		t.Fatal(err)
	}
	links := receipt.Links
	candidate, _, _ := v2TransitionFixtureWithReleases(t, nil, receipt.Successor.Releases, links)
	candidate.options.RegistryPayload = receipt.Registry
	candidate.registry, candidate.registryDigest, err = ParseRegistryV2(receipt.Registry, candidate.catalog)
	if err != nil {
		t.Fatal(err)
	}
	candidate.options.MigrationCheckpoint = true
	candidate.options.ArtifactDigest = receipt.Successor.ArtifactDigest
	candidate.options.ObserveLinks = func(context.Context) (ReleaseLinks, error) { return links, nil }
	seedOwnerFile(t, candidate, []string{"release-transition", "v2"}, "ledger.json", receipt.ProjectionPayload)
	seedOwnerFile(t, candidate, []string{"release-transition", "v2"}, migrationLedgerCheckpointName, receipt.CheckpointPayload)
	seedOwnerFile(t, candidate, []string{"release-transition", "v2"}, "journal.json", mustJournalPayload(t, receipt.Predecessor))
	for _, step := range receipt.Predecessor.Steps {
		for _, checkpoint := range []EvidenceCheckpoint{EvidenceCaptured, EvidenceApplied, EvidenceVerified} {
			observed := step.Desired
			if checkpoint == EvidenceCaptured {
				observed = step.Expected
			}
			evidence := evidenceFor(receipt.Predecessor, step, checkpoint, observed, "")
			payload, err := MarshalEvidence(evidence)
			if err != nil {
				t.Fatal(err)
			}
			if err := candidate.store.CreateCheckpointEvidence(receipt.Predecessor.Transaction, step.ID, checkpoint, payload); err != nil {
				t.Fatal(err)
			}
		}
	}
	return candidate, receipt, &links
}

func seedOwnerFile(t *testing.T, candidate *V2Transition, parts []string, name string, payload []byte) {
	t.Helper()
	root := candidate.options.ConfigHome
	for _, part := range parts {
		root = filepath.Join(root, part)
		if err := os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	testkit.WriteFile(t, filepath.Join(root, name), payload, 0600)
}

func seedMigrationOwnerReceipt(t *testing.T, candidate *V2Transition, receipt MigrationOwnerReceiptV1) {
	t.Helper()
	payload, err := MarshalMigrationOwnerReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	seedOwnerFile(t, candidate, migrationOwnerReceiptParts, string(receipt.Successor.Transaction)+".json", payload)
}

func seedMigrationOwnerTerminal(t *testing.T, candidate *V2Transition, receipt MigrationOwnerReceiptV1) JournalRecord {
	t.Helper()
	terminal := receipt.Successor
	terminal.Checkpoint = JournalComplete
	seedOwnerFile(t, candidate, []string{"release-transition", "v2"}, "journal.json", mustJournalPayload(t, terminal))
	return terminal
}

func TestMigrationOwnerLegacyReceiptStaging(t *testing.T) {
	for _, size := range []int{0, 18, -1, -2} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			c, r, links := migrationOwnerReaderFixture(t, false)
			payload, err := MarshalMigrationOwnerReceipt(r)
			if err != nil {
				t.Fatal(err)
			}
			canonical := size == -2
			if canonical {
				size = len(payload)
			} else if size < 0 {
				size = len(payload) / 2
			}
			name := "." + string(r.Successor.Transaction) + ".json.pending"
			seedOwnerFile(t, c, migrationOwnerReceiptParts, name, payload[:size])
			before := snapshotStoreV2Tree(t, c.options.ConfigHome)
			receipts, err := c.store.migrationOwnerReceipts()
			if canonical {
				if err != nil || len(receipts) != 1 {
					t.Fatalf("canonical receipt: %v %v", receipts, err)
				}
			} else if err == nil {
				t.Fatal("legacy partial staging became receipt authority")
			}
			if !bytes.Equal(before, snapshotStoreV2Tree(t, c.options.ConfigHome)) || links.Active != "release-b" {
				t.Fatal("receipt reader changed protected bytes")
			}
			if canonical {
				if err := c.store.createOwnerImmutable(migrationOwnerReceiptParts, string(r.Successor.Transaction)+".json", payload); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(filepath.Join(c.options.ConfigHome, filepath.Join(migrationOwnerReceiptParts...), name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("canonical staging not retired: %v", err)
				}
			}
		})
	}
}

func TestMigrationOwnerReceiptOwnsNestedInputs(t *testing.T) {
	_, receipt, _ := migrationOwnerReaderFixture(t, true)
	receipt.RetainedTransactions = []TransactionID{"tx-retained-one", "tx-retained-two"}
	before, _ := json.Marshal(receipt)
	clone := receipt.cloneMigrationOwnerReceipt()
	clone.Registry[0] ^= 1
	clone.SourceRegistry[0] ^= 1
	clone.ProjectionPayload[0] ^= 1
	clone.CheckpointPayload[0] ^= 1
	clone.PreviousSource.Release = "changed"
	clone.RetainedTransactions[0] = "tx-changed"
	clone.Predecessor.Steps[0].Evidence.Observed = digestC
	*clone.Predecessor.Releases.Previous = "changed"
	*clone.Links.Previous = "changed"
	after, _ := json.Marshal(receipt)
	if !bytes.Equal(before, after) {
		t.Fatal("receipt clone aliases protected payload, graph or journal fields")
	}
}

func TestMigrationOwnerReplacementTerminalMetadataRetainsTypedClosure(t *testing.T) {
	c, r, _ := migrationOwnerReaderFixture(t, false)
	seedMigrationOwnerReceipt(t, c, r)
	terminal := seedMigrationOwnerTerminal(t, c, r)
	proof, err := c.store.InspectSourceMetadata(c.registry, c.registryDigest)
	if err != nil || proof.Journal == nil || proof.Journal.Checkpoint != JournalComplete {
		t.Fatalf("terminal metadata: %+v %v", proof, err)
	}
	if err := c.store.recordCompletedMigrationOwner(terminal); err != nil {
		t.Fatal(err)
	}
	closure, err := c.store.migrationOwnerCompletion(r, nil)
	if err != nil || closure.Checkpoint != JournalComplete {
		t.Fatalf("durable terminal provenance: %+v %v", closure, err)
	}
}

func TestMigrationOwnerReplacementBindsRetainedTerminalHistory(t *testing.T) {
	for _, target := range []ReleaseID{"foreign-release", "release-a", "release-previous"} {
		t.Run(string(target), func(t *testing.T) {
			candidate, receipt, _ := migrationOwnerReaderFixture(t, true)
			original, _, err := ParseRegistryV2(receipt.SourceRegistry, BuiltinCapabilityCatalog())
			if err != nil {
				t.Fatal(err)
			}
			ledger := FrozenHistoryBaselineV2(original)
			path, err := original.PendingPath(ledger)
			if err != nil {
				t.Fatal(err)
			}
			_, expected, err := MarshalLedgerV2(ledger, original)
			if err != nil {
				t.Fatal(err)
			}
			journal := JournalRecord{Transaction: "retained-terminal", Releases: ReleasePair{From: target, Target: target}}
			for _, migration := range path {
				ledger, err = ledger.Advance(original, migration)
				if err != nil {
					t.Fatal(err)
				}
				_, desired, err := MarshalLedgerV2(ledger, original)
				if err != nil {
					t.Fatal(err)
				}
				step := JournalStep{ID: migration.ID + ".ledger", Migration: migration.ID, Resource: "ledger." + migration.Domain, Decision: DecisionTransform, Expected: expected, Desired: desired, Checkpoint: StepVerified}
				for _, checkpoint := range []EvidenceCheckpoint{EvidenceCaptured, EvidenceApplied, EvidenceVerified} {
					observed := desired
					if checkpoint == EvidenceCaptured {
						observed = expected
					}
					evidence := evidenceFor(journal, step, checkpoint, observed, "")
					payload, err := MarshalEvidence(evidence)
					if err != nil {
						t.Fatal(err)
					}
					if err := candidate.store.CreateCheckpointEvidence(journal.Transaction, step.ID, checkpoint, payload); err != nil {
						t.Fatal(err)
					}
				}
				expected = desired
			}
			receipt.RetainedTransactions = []TransactionID{journal.Transaction}
			err = candidate.store.migrationOwnerRetainedHistory(receipt)
			if target == "foreign-release" {
				if err == nil || !strings.Contains(err.Error(), "foreign target") {
					t.Fatalf("foreign retained history admitted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			seedMigrationOwnerReceipt(t, candidate, receipt)
			terminal := seedMigrationOwnerTerminal(t, candidate, receipt)
			if _, err := os.Stat(filepath.Join(candidate.options.ConfigHome, "release-transition", "v2", "transactions", string(journal.Transaction))); err != nil {
				t.Fatalf("cleanup orphaned retained history proof: %v", err)
			}
			receipts, err := candidate.store.migrationOwnerReceipts()
			if err != nil {
				t.Fatal(err)
			}
			receipt = receipts[terminal.Transaction]
			if len(receipt.RetainedTransactions) != 1 || receipt.PreviousSource == nil {
				t.Fatal("receipt omitted retained source bindings")
			}
			if err := candidate.store.migrationOwnerRetainedHistory(receipt); err != nil {
				t.Fatal(err)
			}
			beforeCleanup := snapshotStoreV2Tree(t, candidate.options.ConfigHome)
			if err := candidate.store.cleanupClosedMigrationTransactions(terminal.Transaction, candidate.registry, candidate.registryDigest, ReleaseLinks{Active: "release-unrelated", Previous: releaseIDPointer("release-other")}); err != nil {
				t.Fatalf("receipt graph roots must precede closed-history classification: %v", err)
			}
			if !bytes.Equal(beforeCleanup, snapshotStoreV2Tree(t, candidate.options.ConfigHome)) {
				t.Fatal("later updater cleanup removed receipt-bound predecessor or retained history")
			}
			receipt.PreviousSource.Registry = digestB
			if err := receipt.Validate(); err == nil {
				t.Fatal("receipt accepted previous registry drift")
			}
		})
	}
}

package releasetransition

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestLocalSSHMigrationCatalogPreservesKnownRetainedHistoryAndBlocksUnknown(t *testing.T) {
	transition, store := completedSourceMetadataFixture(t)
	snapshot, err := store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	journal, err := ParseJournal(snapshot.Payload)
	if err != nil {
		t.Fatal(err)
	}
	journal.Transaction = "tx-known-catalog-activation"
	journal.Checkpoint = JournalReconciling
	journal.Steps = []JournalStep{}
	journal.CatalogDigest = PreviousBuiltinCapabilityCatalog().Digest()
	journal.IntentDigest = bindJournalIntent(journal.AuthorizationPlan, journal.ResumePlan, journal.ObservationScope, journal.Steps)
	payload, err := MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapCurrentJournal(snapshot, payload); err != nil {
		t.Fatal(err)
	}
	current, err := NewV2Transition(transition.options)
	if err != nil {
		t.Fatal(err)
	}
	if current.catalog.Digest() != journal.CatalogDigest {
		t.Fatal("original frozen catalog changed")
	}
	before := snapshotStoreV2Tree(t, store.configHome)
	if _, err := store.InspectSourceMetadata(current.registry, current.registryDigest); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, snapshotStoreV2Tree(t, store.configHome)) {
		t.Fatal("reader rewrote history")
	}
	if _, known := SupportedBuiltinCatalog(digestA); known {
		t.Fatal("unknown catalog admitted")
	}
}

func TestLocalSSHMigrationHasNoLegacyRollbackCompatibility(t *testing.T) {
	registryPayload, err := os.ReadFile(filepath.Join("..", "..", "config/release-transition.json"))
	registryPayload = localSSHHistoricalRegistry(t, registryPayload)
	if err != nil {
		t.Fatal(err)
	}
	for _, applied := range []bool{false, true} {
		fixture := newV2RollbackBoundaryFixtureWithLedger(t, "0.18.6", "", registryPayload, "absent")
		if applied {
			registry := fixture.transition.registry
			ledger := FrozenHistoryBaselineV2(registry)
			for _, migration := range registry.Migrations {
				ledger, err = ledger.Advance(registry, migration)
				if err != nil {
					t.Fatal(err)
				}
			}
			payload, _, err := MarshalLedgerV2(ledger, registry)
			if err != nil {
				t.Fatal(err)
			}
			before, err := fixture.transition.store.ReadLedger()
			if err != nil {
				t.Fatal(err)
			}
			if err := fixture.transition.store.CompareAndSwapLedger(before, payload); err != nil {
				t.Fatal(err)
			}
		}
		inspection, err := fixture.transition.Inspect(context.Background(), Goal{Target: "release-a", Direction: DirectionActivatePrevious})
		if err != nil || inspection.Outcome == nil || inspection.Outcome.Code != CodeRollbackIncompatible || inspection.Assessment.Changed {
			t.Fatalf("legacy rollback admitted (applied=%t): %#v %v", applied, inspection, err)
		}
		if *fixture.authorizes != 0 || *fixture.activations != 0 {
			t.Fatal("incompatible rollback authorized or activated")
		}
	}
}

func localSSHHistoricalRegistry(t *testing.T, payload []byte) []byte {
	t.Helper()
	var registry RegistryV2
	if err := json.Unmarshal(historyV028Payload, &registry); err != nil {
		t.Fatal(err)
	}
	registry.CurrentEpochs["owner-registration"], registry.CurrentEpochs["power-metadata"] = 2, 2
	registry.Migrations = slices.DeleteFunc(registry.Migrations, nativeMigration)
	encoded, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	// Preserve the exact published four-kind registry, including its inline
	// SSH dependency array. Owner-replacement authority pins these bytes.
	encoded = bytes.Replace(encoded, []byte("\"dependsOn\": [\n        \"canonicalize-test-vms-settings-v2\",\n        \"canonicalize-test-yard-owner-v2\"\n      ]"), []byte("\"dependsOn\": [\"canonicalize-test-vms-settings-v2\", \"canonicalize-test-yard-owner-v2\"]"), 1)
	return append(encoded, '\n')
}

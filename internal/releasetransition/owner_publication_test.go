package releasetransition

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestOwnerPublicationOrderPreservesCompletedHistoricalOwnerPrefix(t *testing.T) {
	payload, err := os.ReadFile(filepath.Join("..", "..", "config", "release-transition.json"))
	payload = localSSHHistoricalRegistry(t, payload)
	if err != nil {
		t.Fatal(err)
	}
	registry, digest, err := ParseRegistryV2(payload, BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	// The former cross-domain order completed owner epoch 2 while power stayed
	// at epoch 1. Its exact domain histories and compacted prefixes remain valid.
	historical := registry
	historical.CurrentEpochs = cloneEpochs(registry.CurrentEpochs)
	historical.CurrentEpochs["settings"] = 2
	historical.Migrations = nil
	for _, domain := range []string{"settings", "owner-registration", "power-metadata"} {
		for _, migration := range registry.Migrations {
			if migration.Domain == domain && migration.Kind != localSSHPortMigration {
				historical.Migrations = append(historical.Migrations, migration)
			}
		}
	}
	historicalPayload, err := json.Marshal(historical)
	if err != nil {
		t.Fatal(err)
	}
	historical, historicalDigest, err := ParseRegistryV2(historicalPayload, BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	ledger := FrozenHistoryBaselineV2(historical)
	for _, domain := range []string{"settings", "owner-registration"} {
		for _, migration := range historical.Migrations {
			if migration.Domain == domain {
				ledger, err = ledger.Advance(historical, migration)
				if err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	projectionPayload, _, err := MarshalLedgerV2(ledger, historical)
	if err != nil {
		t.Fatalf("historical owner epoch 2 / power epoch 1: %v", err)
	}
	projection := protectedSnapshotFromPayload(projectionPayload)
	epochs := map[string]int{}
	for domain, state := range ledger.Domains {
		epochs[domain] = state.Epoch
	}
	checkpoint, err := newLedgerCheckpointV1(ledger, historical, historicalDigest, projection, epochs)
	if err != nil {
		t.Fatal(err)
	}
	checkpointPayload, _, err := MarshalLedgerCheckpointV1(checkpoint, historical, historicalDigest, projection)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := ParseMigrationLedger(projection, protectedSnapshotFromPayload(checkpointPayload), registry, digest)
	if err != nil {
		t.Fatalf("compacted historical owner prefix: %v", err)
	}
	pending, err := registry.PendingPath(retained.Ledger)
	if err != nil || len(pending) != 2 || pending[0].Domain != "power-metadata" || pending[1].Kind != localSSHPortMigration {
		t.Fatalf("historical remaining work: %+v %v", pending, err)
	}
}

func TestOwnerPublicationBoundaryRefusesEpochOneRollbackBeforeMutation(t *testing.T) {
	payload, err := os.ReadFile(filepath.Join("..", "..", "config", "release-transition.json"))
	payload = localSSHHistoricalRegistry(t, payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"absent", "baseline", "partial", ""} {
		t.Run(state, func(t *testing.T) {
			fixture := newV2RollbackBoundaryFixtureWithLedger(t, "0.18.1", "", payload, state)
			inspection, err := fixture.transition.Inspect(context.Background(), Goal{Target: "release-a", Direction: DirectionActivatePrevious})
			if err != nil || inspection.Outcome == nil || inspection.Outcome.Code != CodeRollbackIncompatible || inspection.Assessment.Changed {
				t.Fatalf("unsafe rollback: %+v %v", inspection, err)
			}
			fixture.assertUnchanged(t, true)
		})
	}
}

// Releases before the NetworkManager device guard boundary carry the registry without it. Their
// registry cannot parse a ledger past the boundary, which is the condition that makes rollback
// refuse before assessment; a ledger before the boundary still parses.
func TestNetworkManagerDeviceGuardLedgerIsUnreadableBelowTheBoundary(t *testing.T) {
	current, err := os.ReadFile(filepath.Join("..", "..", "config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	currentRegistry, _, err := ParseRegistryV2(current, BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	var released RegistryV2
	if err := json.Unmarshal(current, &released); err != nil {
		t.Fatal(err)
	}
	released.CurrentEpochs["power-metadata"] = 3
	released.Migrations = slices.DeleteFunc(released.Migrations, func(m MigrationDefinitionV2) bool { return m.Kind == nmDeviceGuardMigration })
	releasedPayload, err := json.Marshal(released)
	if err != nil {
		t.Fatal(err)
	}
	releasedRegistry, _, err := ParseRegistryV2(releasedPayload, registeredYardsCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	ledger := FreshBootstrapLedgerV2(currentRegistry)
	for _, migration := range currentRegistry.Migrations {
		if migration.Kind == nmDeviceGuardMigration {
			before, _, err := MarshalLedgerV2(ledger, currentRegistry)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := ParseLedgerV2(before, releasedRegistry); err != nil {
				t.Fatalf("a ledger before the boundary no longer parses for the released registry: %v", err)
			}
		}
		if ledger, err = ledger.Advance(currentRegistry, migration); err != nil {
			t.Fatal(err)
		}
	}
	after, _, err := MarshalLedgerV2(ledger, currentRegistry)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ParseLedgerV2(after, releasedRegistry); err == nil {
		t.Fatal("the released registry accepted a ledger past the NetworkManager device guard boundary")
	}
}

package releasetransition

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Legacy owner commits install power helpers and initialize the replacement
// owner. Those publication writes require the already persisted reader epoch.
type powerReaderOwnerRegistration struct {
	*v2TestOwnerRegistration
	transition *V2Transition
}

func (owner *powerReaderOwnerRegistration) Commit(ctx context.Context, before OwnerRegistrationObservation) error {
	state, err := owner.transition.store.ReadMigrationLedger(owner.transition.registry, owner.transition.registryDigest)
	if err != nil {
		return err
	}
	if state.Ledger.Domains["power-metadata"].Epoch != 2 {
		return errors.New("legacy owner publication preceded its protected reader epoch")
	}
	return owner.v2TestOwnerRegistration.Commit(ctx, before)
}

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
	ledger := BaselineLedgerV2(historical)
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

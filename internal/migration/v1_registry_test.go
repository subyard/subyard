package migration

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
)

func TestRegistryRejectsNonContiguousAndUnsafeDefinitions(t *testing.T) {
	registry := Registry{
		SchemaVersion: 1,
		MinimumLayout: 1,
		CurrentLayout: 2,
		Migrations: []Definition{{
			ID:             "unsafe",
			FromLayout:     1,
			ToLayout:       2,
			Resources:      []string{"unsafe-resource"},
			FinalizePolicy: "remove-source-after-active-verify",
			RollbackPolicy: "restore-recovery-before-runtime-swap",
			Moves: []Move{{
				Scope: "config-home", Source: "../escape",
				Destination: "current/config.env", Consumer: "assignments",
			}},
		}},
	}
	if err := registry.Validate(); err == nil {
		t.Fatal("registry accepted an escaping source")
	}
	registry.Migrations[0].Moves[0].Source = "legacy/config.env"
	registry.Migrations[0].ToLayout = 3
	if err := registry.Validate(); err == nil {
		t.Fatal("registry accepted a skipped layout")
	}

	registry.Migrations[0] = Definition{
		ID:             "typed",
		FromLayout:     1,
		ToLayout:       2,
		Resources:      []string{"typed-resource"},
		FinalizePolicy: orderedFinalizePolicy,
		RollbackPolicy: orderedRollbackPolicy,
		Operations: []Operation{{
			ID: "arbitrary", Kind: "shell-command",
		}},
	}
	if err := registry.Validate(); err == nil {
		t.Fatal("registry accepted an arbitrary operation kind")
	}
	registry.Migrations[0].Operations[0].Kind = OperationKindTestYardOwnerV1
	registry.Migrations[0].Moves = []Move{{
		Scope: "config-home", Source: "legacy/config.env",
		Destination: "current/config.env", Consumer: "assignments",
	}}
	if err := registry.Validate(); err != nil {
		t.Fatalf("registry rejected ordered moves plus typed operations: %v", err)
	}
}

func TestPublishedV1RegistryRetainsTheExactCompatibilityHistory(t *testing.T) {
	registry, err := LoadRegistry(filepath.Join("..", "..", "config", "migrations.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.Migrations) != 4 || registry.CurrentLayout != 5 {
		t.Fatalf("published registry layout=%d migrations=%d", registry.CurrentLayout, len(registry.Migrations))
	}
	operations := registry.Migrations[0].Operations
	if len(operations) != 2 || operations[0].Kind != OperationKindTestYardOwnerV1 ||
		operations[1].Kind != OperationKindTestYardRouteConsumersV1 {
		t.Fatalf("published operation order = %#v", operations)
	}
	broker := registry.Migrations[1]
	if broker.FromLayout != 2 || broker.ToLayout != 3 || len(broker.Operations) != 1 ||
		broker.Operations[0].Kind != OperationKindTestVMBrokerRuntimeV1 {
		t.Fatalf("published broker migration = %#v", broker)
	}
	power := registry.Migrations[2]
	if power.FromLayout != 3 || power.ToLayout != 4 || len(power.Operations) != 1 ||
		power.Operations[0].Kind != OperationKindPowerReconcilerRuntimeV1 {
		t.Fatalf("published power reconciler migration = %#v", power)
	}
	compatibility := registry.Migrations[3]
	if compatibility.ID != "repair-power-reconciler-systemd-compat" ||
		compatibility.FromLayout != 4 || compatibility.ToLayout != 5 ||
		!slices.Equal(compatibility.Resources, []string{"power-reconciler-runtime"}) ||
		len(compatibility.Operations) != 1 ||
		compatibility.Operations[0].ID != "power-reconciler-systemd-compat" ||
		compatibility.Operations[0].Kind != OperationKindPowerReconcilerSystemdCompatV1 {
		t.Fatalf("published power reconciler compatibility migration = %#v", compatibility)
	}
}

func TestSourceUpgradeFixtureExtendsExactPublishedV1Registry(t *testing.T) {
	shipped, err := LoadRegistry(filepath.Join("..", "..", "config", "migrations.json"))
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := LoadRegistry(filepath.Join(
		"..", "..", "tests", "fixtures", "migrations", "layout-6-production-prefix.json",
	))
	if err != nil {
		t.Fatal(err)
	}
	if fixture.CurrentLayout != shipped.CurrentLayout+1 ||
		len(fixture.Migrations) != len(shipped.Migrations)+1 {
		t.Fatalf("source-upgrade fixture layout=%d migrations=%d", fixture.CurrentLayout, len(fixture.Migrations))
	}
	shippedPrefix, err := json.Marshal(shipped.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	fixturePrefix, err := json.Marshal(fixture.Migrations[:len(shipped.Migrations)])
	if err != nil {
		t.Fatal(err)
	}
	if string(fixturePrefix) != string(shippedPrefix) {
		t.Fatal("source-upgrade fixture rewrote the published migration prefix")
	}
	synthetic := fixture.Migrations[len(shipped.Migrations)]
	if synthetic.ID != "move-legacy-assignments" || synthetic.FromLayout != shipped.CurrentLayout ||
		synthetic.ToLayout != fixture.CurrentLayout {
		t.Fatalf("source-upgrade synthetic migration = %#v", synthetic)
	}
}

package ownerinventory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
)

func TestLegacyDiscoveryPersistsOnlyUntrustedCacheAndFallsBackOffline(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	inventory := domain.OwnerInventory{
		Schema:     domain.OwnerInventorySchema,
		HostID:     "owner-remote",
		ObservedAt: now,
		Yards: []domain.OwnerYard{{
			Name: "default", Kind: "container",
			Instance: "yard", State: "RUNNING", SSHPort: 2222, DevUser: "dev",
			Projects: []domain.OwnerProject{{
				ProjectID: "project-id", Name: "project", Mode: "sync", Target: "yard",
			}},
		}},
	}
	service := LegacyService{
		Store: Connections{Root: root}, Clock: fixedClock{now: now},
		Fetch: func(context.Context, string) (domain.OwnerInventory, error) {
			return inventory, nil
		},
	}
	connection := Connection{
		Destination: "dev@remote.example", LegacyNames: []string{"peer"},
		Yards: map[string]YardRoute{"default": {SSHHost: "yard-peer"}},
	}
	first := service.Read(context.Background(), connection, true)
	if first.Err != nil || first.Stale || first.Inventory.HostID != inventory.HostID {
		t.Fatalf("legacy discovery failed: %#v", first)
	}
	records, err := service.Store.List()
	if err != nil || len(records) != 1 || records[0].HostID != inventory.HostID || records[0].Trust != nil {
		t.Fatalf("legacy discovery persisted trusted or incomplete state: %#v, %v", records, err)
	}

	service.Fetch = func(context.Context, string) (domain.OwnerInventory, error) {
		return domain.OwnerInventory{}, errors.New("owner offline")
	}
	second := service.Read(context.Background(), records[0], true)
	if second.Err == nil || !second.Stale || second.Inventory.HostID != inventory.HostID ||
		len(second.Inventory.Yards) != 1 || len(second.Inventory.Yards[0].Projects) != 1 {
		t.Fatalf("offline legacy fallback lost its explicit stale snapshot: %#v", second)
	}
}

func TestLegacyRefreshRejectsHostIDChangeWithoutManagedTrust(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	store := Connections{Root: root}
	connection := Connection{HostID: "owner-old", Destination: "dev@remote.example"}
	if err := store.Write(connection); err != nil {
		t.Fatal(err)
	}
	if err := (Cache{Root: root}).Write(Snapshot{
		FetchedAt: now, Inventory: domain.OwnerInventory{
			Schema: domain.OwnerInventorySchema, HostID: "owner-old", ObservedAt: now,
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := LegacyService{
		Store: store, Clock: fixedClock{now: now.Add(time.Minute)},
		Fetch: func(context.Context, string) (domain.OwnerInventory, error) {
			return domain.OwnerInventory{
				Schema: domain.OwnerInventorySchema, HostID: "owner-new", ObservedAt: now,
			}, nil
		},
	}
	result := service.Read(context.Background(), connection, true)
	if result.Err == nil || !errors.Is(result.Err, ErrIntegrity) || !result.Stale ||
		result.Inventory.HostID != "owner-old" {
		t.Fatalf("untrusted legacy refresh adopted HostID change: %#v", result)
	}
}

func TestLegacyDiscoveryCanBeUpgradedToManagedTrust(t *testing.T) {
	root := t.TempDir()
	store := Connections{Root: root}
	now := time.Now().UTC()
	inventory := domain.OwnerInventory{
		Schema: domain.OwnerInventorySchema, HostID: "owner-a", ObservedAt: now,
	}
	legacy := Connection{
		Destination: "owner-alias", LegacyNames: []string{"old-yard"},
		Yards: map[string]YardRoute{"default": {SSHHost: "yard-old"}},
	}
	result := (LegacyService{
		Store: store,
		Fetch: func(context.Context, string) (domain.OwnerInventory, error) {
			return inventory, nil
		},
	}).Read(context.Background(), legacy, true)
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	routing := store.routingPath("owner-a")
	if err := os.MkdirAll(routing, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(routing, "project"), []byte("kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	trust := testSSHHostTrust(t, "owner-alias")
	plan, err := store.PrepareRegistration(Connection{
		HostID: "owner-a", Destination: "owner-alias", Trust: &trust,
	}, Snapshot{FetchedAt: now.Add(time.Second), Inventory: inventory})
	if err != nil {
		t.Fatalf("prepare managed upgrade: %v", err)
	}
	if err := store.ApplyRegistration(plan); err != nil {
		t.Fatalf("apply managed upgrade: %v", err)
	}
	records, err := store.List()
	if err != nil || len(records) != 1 || records[0].Trust == nil ||
		records[0].Trust.Fingerprint != trust.Fingerprint ||
		len(records[0].LegacyNames) != 1 || records[0].LegacyNames[0] != "old-yard" ||
		records[0].Yards["default"].SSHHost != "yard-old" {
		t.Fatalf("managed upgrade = %#v, %v", records, err)
	}
	if payload, err := os.ReadFile(filepath.Join(routing, "project")); err != nil || string(payload) != "kept\n" {
		t.Fatalf("managed upgrade lost routing state: %q, %v", payload, err)
	}
}

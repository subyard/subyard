package ownerinventory

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRepairAndRemovalJournalsRejectOversizedFiles(t *testing.T) {
	for _, test := range []struct {
		name string
		path func(Connections) string
	}{
		{name: "repair", path: Connections.connectionRepairPath},
		{name: "removal", path: Connections.removalPath},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := Connections{Root: t.TempDir()}
			payload := bytes.Repeat([]byte{'x'}, 8*1024*1024+1)
			if err := os.WriteFile(test.path(store), payload, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := store.Recover(); err == nil || !strings.Contains(err.Error(), "too large") {
				t.Fatalf("Recover oversized %s journal = %v", test.name, err)
			}
		})
	}
}

func writeJournalFixture(t *testing.T, path string, value any) {
	t.Helper()
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeOwnerFile(path, append(payload, '\n')); err != nil {
		t.Fatal(err)
	}
}

func TestRegistrationJournalRecoveryIsIdempotent(t *testing.T) {
	root := t.TempDir()
	store := Connections{Root: root}
	trust := testSSHHostTrust(t, "owner.example")
	connection := Connection{HostID: "owner-a", Destination: "owner-alias", Trust: &trust}
	snapshot := Snapshot{FetchedAt: time.Now(), Inventory: fixtureInventory("owner-a", time.Now())}
	writeJournalFixture(t, store.registrationPath(), registrationJournal{SchemaVersion: registrationSchema, Connection: connection, Snapshot: snapshot})
	for range 2 {
		if err := store.Recover(); err != nil {
			t.Fatal(err)
		}
	}
	if records, err := store.List(); err != nil || len(records) != 1 {
		t.Fatalf("registration recovery = %#v, %v", records, err)
	}
	if _, err := (Cache{Root: root}).Read("owner-a"); err != nil {
		t.Fatal(err)
	}
}

func TestRegistrationRecoveryCompletesConnectionOnlyMixedState(t *testing.T) {
	root := t.TempDir()
	store := Connections{Root: root}
	trust := testSSHHostTrust(t, "owner.example")
	connection := Connection{HostID: "owner-a", Destination: "owner-alias", Trust: &trust}
	snapshot := Snapshot{FetchedAt: time.Now(), Inventory: fixtureInventory("owner-a", time.Now())}
	writeJournalFixture(t, store.registrationPath(), registrationJournal{SchemaVersion: registrationSchema, Connection: connection, Snapshot: snapshot})
	if err := store.writeConnectionFile(connection); err != nil {
		t.Fatal(err)
	}
	if err := store.Recover(); err != nil {
		t.Fatal(err)
	}
	if _, err := (Cache{Root: root}).Read("owner-a"); err != nil {
		t.Fatalf("cache was not completed: %v", err)
	}
}

func TestLegacyRegistrationJournalRecoveryCompletesConnectionOnlyMixedState(t *testing.T) {
	root := t.TempDir()
	store := Connections{Root: root}
	connection := Connection{HostID: "owner-a", Destination: "owner-alias"}
	snapshot := Snapshot{FetchedAt: time.Now(), Inventory: fixtureInventory("owner-a", time.Now())}
	writeJournalFixture(t, store.registrationPath(), registrationJournal{SchemaVersion: registrationSchema, Connection: connection, Snapshot: snapshot})
	if err := store.writeConnectionFile(connection); err != nil {
		t.Fatal(err)
	}
	if err := store.Recover(); err != nil {
		t.Fatal(err)
	}
	if _, err := (Cache{Root: root}).Read("owner-a"); err != nil {
		t.Fatalf("legacy cache was not completed: %v", err)
	}
}

func TestHostIDMigrationJournalRecoveryIsIdempotent(t *testing.T) {
	root := t.TempDir()
	store := Connections{Root: root}
	trust := testSSHHostTrust(t, "owner.example")
	oldConnection := Connection{HostID: "owner-a", Destination: "owner-alias", Trust: &trust}
	if err := store.Write(oldConnection); err != nil {
		t.Fatal(err)
	}
	oldRouting := store.routingPath("owner-a")
	if err := os.MkdirAll(oldRouting, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldRouting, "state"), []byte("kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	newConnection := oldConnection
	newConnection.HostID = "owner-b"
	snapshot := Snapshot{FetchedAt: time.Now(), Inventory: fixtureInventory("owner-b", time.Now())}
	writeJournalFixture(t, store.hostIDAdoptionPath(), hostIDAdoptionJournal{SchemaVersion: hostIDAdoptionSchema, OldConnection: oldConnection, NewConnection: newConnection, Snapshot: &snapshot, RoutingWasPresent: true})
	for range 2 {
		if err := store.Recover(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(store.routingPath("owner-b"), "state")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(store.routingPath("owner-a")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old routing survived: %v", err)
	}
}

func TestHostIDMigrationRecoveryCompletesBothSidesOfRoutingCommitPoint(t *testing.T) {
	for _, routingAlreadyMoved := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-routing-rename", true: "after-routing-rename"}[routingAlreadyMoved], func(t *testing.T) {
			root := t.TempDir()
			store := Connections{Root: root}
			trust := testSSHHostTrust(t, "owner.example")
			oldConnection := Connection{HostID: "owner-a", Destination: "owner-alias", Trust: &trust}
			if err := store.Write(oldConnection); err != nil {
				t.Fatal(err)
			}
			newConnection := oldConnection
			newConnection.HostID = "owner-b"
			snapshot := Snapshot{FetchedAt: time.Now(), Inventory: fixtureInventory("owner-b", time.Now())}
			journal := hostIDAdoptionJournal{SchemaVersion: hostIDAdoptionSchema, OldConnection: oldConnection, NewConnection: newConnection, Snapshot: &snapshot, RoutingWasPresent: true}
			writeJournalFixture(t, store.hostIDAdoptionPath(), journal)
			if err := store.writeConnectionFile(newConnection); err != nil {
				t.Fatal(err)
			}
			if err := (Cache{Root: root}).Write(snapshot); err != nil {
				t.Fatal(err)
			}
			oldRouting, newRouting := store.routingPath("owner-a"), store.routingPath("owner-b")
			if err := os.MkdirAll(oldRouting, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(oldRouting, "state"), []byte("kept\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if routingAlreadyMoved {
				if err := os.Rename(oldRouting, newRouting); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Recover(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(newRouting, "state")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRepairAndRemovalJournalRecoveryAreIdempotent(t *testing.T) {
	root := t.TempDir()
	store := Connections{Root: root}
	oldTrust := testSSHHostTrust(t, "owner.example")
	newTrust := testSSHHostTrust(t, "owner.example")
	oldConnection := Connection{HostID: "owner-a", Destination: "owner-alias", Trust: &oldTrust}
	if err := store.Write(oldConnection); err != nil {
		t.Fatal(err)
	}
	newConnection := oldConnection
	newConnection.Trust = &newTrust
	snapshot := Snapshot{FetchedAt: time.Now(), Inventory: fixtureInventory("owner-a", time.Now())}
	writeJournalFixture(t, store.connectionRepairPath(), connectionRepairJournal{SchemaVersion: connectionRepairSchema, OldConnection: oldConnection, NewConnection: newConnection, Snapshot: snapshot})
	for range 2 {
		if err := store.Recover(); err != nil {
			t.Fatal(err)
		}
	}
	writeJournalFixture(t, store.removalPath(), removalJournal{SchemaVersion: removalSchema, HostID: "owner-a"})
	for range 2 {
		if err := store.Recover(); err != nil {
			t.Fatal(err)
		}
	}
	if records, err := store.List(); err != nil || len(records) != 0 {
		t.Fatalf("removal recovery = %#v, %v", records, err)
	}
}

func TestRepairRecoveryCompletesConnectionOnlyMixedState(t *testing.T) {
	root := t.TempDir()
	store := Connections{Root: root}
	oldTrust, newTrust := testSSHHostTrust(t, "owner.example"), testSSHHostTrust(t, "owner.example")
	oldConnection := Connection{HostID: "owner-a", Destination: "owner-alias", Trust: &oldTrust}
	if err := store.Write(oldConnection); err != nil {
		t.Fatal(err)
	}
	newConnection := oldConnection
	newConnection.Trust = &newTrust
	snapshot := Snapshot{FetchedAt: time.Now(), Inventory: fixtureInventory("owner-a", time.Now())}
	journal := connectionRepairJournal{SchemaVersion: connectionRepairSchema, OldConnection: oldConnection, NewConnection: newConnection, Snapshot: snapshot}
	writeJournalFixture(t, store.connectionRepairPath(), journal)
	if err := store.writeConnectionFile(newConnection); err != nil {
		t.Fatal(err)
	}
	if err := store.Recover(); err != nil {
		t.Fatal(err)
	}
	cached, err := (Cache{Root: root}).Read("owner-a")
	if err != nil || !cached.FetchedAt.Equal(snapshot.FetchedAt) {
		t.Fatalf("repair cache not completed: %#v, %v", cached, err)
	}
}

func TestRemovalRecoveryCompletesEveryDeleteBoundary(t *testing.T) {
	for _, removedCount := range []int{0, 1, 2} {
		t.Run(map[int]string{0: "journal-only", 1: "connection-deleted", 2: "connection-and-cache-deleted"}[removedCount], func(t *testing.T) {
			root := t.TempDir()
			store := Connections{Root: root}
			connection := Connection{HostID: "owner-a", Destination: "owner.example"}
			if err := store.Write(connection); err != nil {
				t.Fatal(err)
			}
			snapshot := Snapshot{FetchedAt: time.Now(), Inventory: fixtureInventory("owner-a", time.Now())}
			if err := (Cache{Root: root}).Write(snapshot); err != nil {
				t.Fatal(err)
			}
			routing := store.routingPath("owner-a")
			if err := os.MkdirAll(routing, 0o700); err != nil {
				t.Fatal(err)
			}
			writeJournalFixture(t, store.removalPath(), removalJournal{SchemaVersion: removalSchema, HostID: "owner-a"})
			if removedCount >= 1 {
				if err := os.Remove(filepath.Join(root, "connections", "owner-a.json")); err != nil {
					t.Fatal(err)
				}
			}
			if removedCount >= 2 {
				if err := os.Remove((Cache{Root: root}).path("owner-a")); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Recover(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(routing); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("routing survived: %v", err)
			}
		})
	}
}

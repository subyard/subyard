package ownerinventory

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var errInjectedOwnerIO = errors.New("injected owner inventory I/O failure")

func failOwnerBoundaryOnce(name string) func(string) error {
	fired := false
	return func(candidate string) error {
		if !fired && candidate == name {
			fired = true
			return errInjectedOwnerIO
		}
		return nil
	}
}

func TestRegistrationRecoversFromEveryDurableIOBoundary(t *testing.T) {
	for _, boundary := range []string{
		ownerIOJournalWrite,
		ownerIOConnectionWrite,
		ownerIOCacheWrite,
		ownerIODirectorySync,
		ownerIOJournalCleanup,
	} {
		t.Run(boundary, func(t *testing.T) {
			root := t.TempDir()
			trust := testSSHHostTrust(t, "owner.example")
			connection := Connection{HostID: "owner-a", Destination: "owner-alias", Trust: &trust}
			snapshot := Snapshot{FetchedAt: time.Now(), Inventory: fixtureInventory("owner-a", time.Now())}
			store := Connections{Root: root, failIO: failOwnerBoundaryOnce(boundary)}

			err := store.Register(connection, snapshot)
			if !errors.Is(err, errInjectedOwnerIO) {
				t.Fatalf("Register error = %v, want injected failure at %s", err, boundary)
			}
			store.failIO = nil
			if boundary == ownerIOJournalWrite {
				if records, listErr := store.List(); listErr != nil || len(records) != 0 {
					t.Fatalf("pre-journal failure changed old state: %#v, %v", records, listErr)
				}
				return
			}
			if err := store.Recover(); err != nil {
				t.Fatal(err)
			}
			assertOwnerStateIsNew(t, store, "owner-a")
		})
	}
}

func TestHostIDAdoptionRecoversAcrossRoutingRename(t *testing.T) {
	root := t.TempDir()
	trust := testSSHHostTrust(t, "owner.example")
	oldConnection := Connection{HostID: "owner-a", Destination: "owner-alias", Trust: &trust}
	store := Connections{Root: root}
	if err := store.Write(oldConnection); err != nil {
		t.Fatal(err)
	}
	if err := (Cache{Root: root}).Write(Snapshot{FetchedAt: time.Now(), Inventory: fixtureInventory("owner-a", time.Now())}); err != nil {
		t.Fatal(err)
	}
	oldRouting := store.routingPath("owner-a")
	if err := os.MkdirAll(oldRouting, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldRouting, "state"), []byte("kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store.failIO = failOwnerBoundaryOnce(ownerIORoutingRename)
	snapshot := Snapshot{FetchedAt: time.Now(), Inventory: fixtureInventory("owner-b", time.Now())}
	err := store.AdoptHostID(IdentityProof{
		ExpectedHostID: "owner-a", ObservedHostID: "owner-b", Destination: "owner-alias",
		TrustFingerprint: trust.Fingerprint,
	}, snapshot)
	if !errors.Is(err, errInjectedOwnerIO) {
		t.Fatalf("AdoptHostID error = %v, want routing failure", err)
	}
	store.failIO = nil
	if err := store.Recover(); err != nil {
		t.Fatal(err)
	}
	assertOwnerStateIsNew(t, store, "owner-b")
	if _, err := os.Stat(filepath.Join(store.routingPath("owner-b"), "state")); err != nil {
		t.Fatalf("routing state was not preserved: %v", err)
	}
	if _, err := os.Lstat(store.routingPath("owner-a")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old routing survived: %v", err)
	}
}

func TestHostIDAdoptionRecoversFromEveryDurableIOBoundary(t *testing.T) {
	for _, boundary := range []string{
		ownerIOJournalWrite, ownerIOConnectionWrite, ownerIOCacheWrite, ownerIORoutingRename,
		ownerIODirectorySync, ownerIOJournalCleanup,
	} {
		t.Run(boundary, func(t *testing.T) {
			store, trust := adoptionFailureFixture(t)
			store.failIO = failOwnerBoundaryOnce(boundary)
			snapshot := Snapshot{FetchedAt: time.Now(), Inventory: fixtureInventory("owner-b", time.Now())}
			err := store.AdoptHostID(IdentityProof{
				ExpectedHostID: "owner-a", ObservedHostID: "owner-b", Destination: "owner-alias",
				TrustFingerprint: trust.Fingerprint,
			}, snapshot)
			if !errors.Is(err, errInjectedOwnerIO) {
				t.Fatalf("AdoptHostID error = %v, want injected failure at %s", err, boundary)
			}
			store.failIO = nil
			if boundary == ownerIOJournalWrite {
				assertOwnerStateIsNew(t, store, "owner-a")
				return
			}
			if err := store.Recover(); err != nil {
				t.Fatal(err)
			}
			assertOwnerStateIsNew(t, store, "owner-b")
			if _, err := os.Stat(filepath.Join(store.routingPath("owner-b"), "state")); err != nil {
				t.Fatalf("routing state was not preserved: %v", err)
			}
		})
	}
}

func adoptionFailureFixture(t *testing.T) (Connections, SSHHostTrust) {
	t.Helper()
	root := t.TempDir()
	trust := testSSHHostTrust(t, "owner.example")
	store := Connections{Root: root}
	connection := Connection{HostID: "owner-a", Destination: "owner-alias", Trust: &trust}
	if err := store.Write(connection); err != nil {
		t.Fatal(err)
	}
	if err := store.cache().Write(Snapshot{FetchedAt: time.Now(), Inventory: fixtureInventory("owner-a", time.Now())}); err != nil {
		t.Fatal(err)
	}
	routing := store.routingPath("owner-a")
	if err := os.MkdirAll(routing, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(routing, "state"), []byte("kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return store, trust
}

func TestRepairRecoversFromEveryDurableIOBoundary(t *testing.T) {
	for _, boundary := range []string{
		ownerIOJournalWrite, ownerIOConnectionWrite, ownerIOCacheWrite,
		ownerIODirectorySync, ownerIOJournalCleanup,
	} {
		t.Run(boundary, func(t *testing.T) {
			root := t.TempDir()
			oldTrust, newTrust := testSSHHostTrust(t, "owner.example"), testSSHHostTrust(t, "owner.example")
			store := Connections{Root: root}
			old := Connection{HostID: "owner-a", Destination: "owner-alias", Trust: &oldTrust}
			if err := store.Write(old); err != nil {
				t.Fatal(err)
			}
			snapshot := Snapshot{FetchedAt: time.Now(), Inventory: fixtureInventory("owner-a", time.Now())}
			plan, err := store.PrepareRepair("owner-a", newTrust, snapshot)
			if err != nil {
				t.Fatal(err)
			}
			store.failIO = failOwnerBoundaryOnce(boundary)
			err = store.ApplyRepair(plan)
			if !errors.Is(err, errInjectedOwnerIO) {
				t.Fatalf("ApplyRepair error = %v, want injected failure at %s", err, boundary)
			}
			store.failIO = nil
			if boundary == ownerIOJournalWrite {
				records, listErr := store.List()
				if listErr != nil || len(records) != 1 || records[0].Trust.Fingerprint != oldTrust.Fingerprint {
					t.Fatalf("pre-journal repair changed old state: %#v, %v", records, listErr)
				}
				return
			}
			if err := store.Recover(); err != nil {
				t.Fatal(err)
			}
			records, err := store.List()
			if err != nil || len(records) != 1 || records[0].Trust.Fingerprint != newTrust.Fingerprint {
				t.Fatalf("repair did not converge to whole new state: %#v, %v", records, err)
			}
		})
	}
}

func TestCombinedKeyAndHostIDRepairRecoversFromEveryDurableIOBoundary(t *testing.T) {
	for _, boundary := range []string{
		ownerIOJournalWrite, ownerIOConnectionWrite, ownerIOCacheWrite, ownerIORoutingRename,
		ownerIODirectorySync, ownerIOJournalCleanup,
	} {
		t.Run(boundary, func(t *testing.T) {
			store, oldTrust := adoptionFailureFixture(t)
			newTrust := testSSHHostTrust(t, "owner.example")
			snapshot := Snapshot{FetchedAt: time.Now(), Inventory: fixtureInventory("owner-b", time.Now())}
			plan, err := store.PrepareRepair("owner-a", newTrust, snapshot)
			if err != nil {
				t.Fatal(err)
			}
			store.failIO = failOwnerBoundaryOnce(boundary)
			err = store.ApplyRepair(plan)
			if !errors.Is(err, errInjectedOwnerIO) {
				t.Fatalf("ApplyRepair error = %v, want injected failure at %s", err, boundary)
			}
			store.failIO = nil
			if boundary == ownerIOJournalWrite {
				records, listErr := store.List()
				if listErr != nil || len(records) != 1 || records[0].HostID != "owner-a" ||
					records[0].Trust.Fingerprint != oldTrust.Fingerprint {
					t.Fatalf("pre-journal combined repair changed old state: %#v, %v", records, listErr)
				}
				return
			}
			if err := store.Recover(); err != nil {
				t.Fatal(err)
			}
			records, err := store.List()
			if err != nil || len(records) != 1 || records[0].HostID != "owner-b" ||
				records[0].Trust.Fingerprint != newTrust.Fingerprint {
				t.Fatalf("combined repair did not converge to whole new state: %#v, %v", records, err)
			}
			if _, err := os.Stat(filepath.Join(store.routingPath("owner-b"), "state")); err != nil {
				t.Fatalf("combined repair lost routing: %v", err)
			}
			if _, err := os.Lstat(store.routingPath("owner-a")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("combined repair retained old routing: %v", err)
			}
		})
	}
}

func TestRemovalRecoversFromEveryDurableIOBoundary(t *testing.T) {
	for _, boundary := range []string{
		ownerIOJournalWrite, ownerIOStateDelete, ownerIODirectorySync, ownerIOJournalCleanup,
	} {
		t.Run(boundary, func(t *testing.T) {
			root := t.TempDir()
			store := Connections{Root: root}
			connection := Connection{HostID: "owner-a", Destination: "owner-alias"}
			if err := store.Write(connection); err != nil {
				t.Fatal(err)
			}
			snapshot := Snapshot{FetchedAt: time.Now(), Inventory: fixtureInventory("owner-a", time.Now())}
			if err := store.cache().Write(snapshot); err != nil {
				t.Fatal(err)
			}
			plan, err := store.PrepareRemoval(connection, snapshot)
			if err != nil {
				t.Fatal(err)
			}
			store.failIO = failOwnerBoundaryOnce(boundary)
			_, err = store.ApplyRemoval(plan)
			if !errors.Is(err, errInjectedOwnerIO) {
				t.Fatalf("ApplyRemoval error = %v, want injected failure at %s", err, boundary)
			}
			store.failIO = nil
			if boundary == ownerIOJournalWrite {
				assertOwnerStateIsNew(t, store, "owner-a")
				return
			}
			if err := store.Recover(); err != nil {
				t.Fatal(err)
			}
			if records, listErr := store.List(); listErr != nil || len(records) != 0 {
				t.Fatalf("removal did not converge to whole new state: %#v, %v", records, listErr)
			}
			if _, err := store.cache().Read("owner-a"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("removed cache survived: %v", err)
			}
		})
	}
}

func assertOwnerStateIsNew(t *testing.T, store Connections, hostID string) {
	t.Helper()
	records, err := store.List()
	if err != nil || len(records) != 1 || records[0].HostID != hostID {
		t.Fatalf("connections are not whole new state: %#v, %v", records, err)
	}
	if _, err := (Cache{Root: store.Root}).Read(hostID); err != nil {
		t.Fatalf("cache is not whole new state: %v", err)
	}
}

package ownerinventory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/testkit/interrupt"
)

// ownerSweep cuts one owner-state operation at every failIO call and recovers with Recover.
//
// Fail mode is crash-faithful here because the error paths only release locks. Known gap:
// writeOwnerFile, writeConnectionFile and Cache.Write remove their temporary files in a defer,
// and their hooks fire before the temporary file exists, so no cut leaves the stale
// .owner-state-*, .connection-* or .inventory-* file that a real crash would.
func ownerSweep(t *testing.T, minPoints int, setup func(t *testing.T) (Connections, func(Connections) error),
	completed func(t *testing.T, store Connections), require ...string,
) {
	t.Helper()
	type state struct {
		store Connections
		apply func(Connections) error
	}
	interrupt.Sweep(t, interrupt.Case[state]{
		Setup: func(t *testing.T) state {
			store, apply := setup(t)
			return state{store, apply}
		},
		Run: func(t *testing.T, s state, in *interrupt.Injector) interrupt.Outcome {
			store := s.store
			store.failIO = in.Hook("owner")
			return interrupt.Outcome{Err: s.apply(store)}
		},
		Resume: func(t *testing.T, s state) interrupt.Outcome {
			return interrupt.Outcome{Err: s.store.Recover()}
		},
		// Lock files are created empty by acquiring a lock and never carry state.
		Signature: func(t *testing.T, s state) any { return interrupt.Tree(t, s.store.Root, ".lock", "mutations") },
		Completed: func(t *testing.T, s state, o interrupt.Outcome) {
			if o.Err != nil {
				t.Fatal(o.Err)
			}
			completed(t, s.store)
		},
		Interrupted: func(t *testing.T, _ state, p interrupt.Point, o interrupt.Outcome) {
			if !errors.Is(o.Err, interrupt.ErrInjected) {
				t.Fatalf("error = %v, want the injected failure at %s", o.Err, p)
			}
		},
		Resumed: func(t *testing.T, _ state, p interrupt.Point, o interrupt.Outcome) {
			if o.Err != nil {
				t.Fatalf("Recover after %s: %v", p, o.Err)
			}
		},
		Repeated: func(t *testing.T, _ state, p interrupt.Point, o interrupt.Outcome) {
			if o.Err != nil {
				t.Fatalf("second Recover after %s: %v", p, o.Err)
			}
		},
		Convergence: interrupt.BeforeOrAfter,
		MinPoints:   minPoints,
		Require:     require,
	})
}

func TestRegistrationRecoversFromEveryDurableIOBoundary(t *testing.T) {
	now, trust := time.Now(), testSSHHostTrust(t, "owner.example")
	ownerSweep(t, 8, func(t *testing.T) (Connections, func(Connections) error) {
		connection := Connection{HostID: "owner-a", Destination: "owner-alias", Trust: &trust}
		snapshot := Snapshot{FetchedAt: now, Inventory: fixtureInventory("owner-a", now)}
		return Connections{Root: testkit.TempDir(t)}, func(store Connections) error { return store.Register(connection, snapshot) }
	}, func(t *testing.T, store Connections) {
		assertOwnerStateIsNew(t, store, "owner-a")
	}, ownerIOJournalWrite, ownerIOConnectionWrite, ownerIOCacheWrite, ownerIODirectorySync, ownerIOJournalCleanup)
}

func TestHostIDAdoptionRecoversFromEveryDurableIOBoundary(t *testing.T) {
	now, trust := time.Now(), testSSHHostTrust(t, "owner.example")
	ownerSweep(t, 13, func(t *testing.T) (Connections, func(Connections) error) {
		store := adoptionFailureFixture(t, trust, now)
		snapshot := Snapshot{FetchedAt: now, Inventory: fixtureInventory("owner-b", now)}
		return store, func(store Connections) error {
			return store.AdoptHostID(IdentityProof{
				ExpectedHostID: "owner-a", ObservedHostID: "owner-b", Destination: "owner-alias",
				TrustFingerprint: trust.Fingerprint,
			}, snapshot)
		}
	}, func(t *testing.T, store Connections) {
		assertOwnerStateIsNew(t, store, "owner-b")
		assertRoutingMoved(t, store)
	}, ownerIOJournalWrite, ownerIOConnectionWrite, ownerIOCacheWrite, ownerIORoutingRename,
		ownerIODirectorySync, ownerIOJournalCleanup)
}

func TestRepairRecoversFromEveryDurableIOBoundary(t *testing.T) {
	now := time.Now()
	oldTrust, newTrust := testSSHHostTrust(t, "owner.example"), testSSHHostTrust(t, "owner.example")
	snapshot := Snapshot{FetchedAt: now, Inventory: fixtureInventory("owner-a", now)}
	ownerSweep(t, 8, func(t *testing.T) (Connections, func(Connections) error) {
		store := Connections{Root: testkit.TempDir(t)}
		if err := store.Write(Connection{HostID: "owner-a", Destination: "owner-alias", Trust: &oldTrust}); err != nil {
			t.Fatal(err)
		}
		plan, err := store.PrepareRepair("owner-a", newTrust, snapshot)
		if err != nil {
			t.Fatal(err)
		}
		return store, func(store Connections) error { return store.ApplyRepair(plan) }
	}, func(t *testing.T, store Connections) {
		records, err := store.List()
		if err != nil || len(records) != 1 || records[0].Trust.Fingerprint != newTrust.Fingerprint {
			t.Fatalf("repair did not converge to whole new state: %#v, %v", records, err)
		}
		cached, err := store.cache().Read("owner-a")
		if err != nil || !cached.FetchedAt.Equal(snapshot.FetchedAt) {
			t.Fatalf("repair did not publish the inventory snapshot: fetchedAt=%v err=%v", cached.FetchedAt, err)
		}
	}, ownerIOJournalWrite, ownerIOConnectionWrite, ownerIOCacheWrite, ownerIODirectorySync, ownerIOJournalCleanup)
}

func TestCombinedKeyAndHostIDRepairRecoversFromEveryDurableIOBoundary(t *testing.T) {
	now := time.Now()
	oldTrust, newTrust := testSSHHostTrust(t, "owner.example"), testSSHHostTrust(t, "owner.example")
	snapshot := Snapshot{FetchedAt: now, Inventory: fixtureInventory("owner-b", now)}
	ownerSweep(t, 13, func(t *testing.T) (Connections, func(Connections) error) {
		store := adoptionFailureFixture(t, oldTrust, now)
		plan, err := store.PrepareRepair("owner-a", newTrust, snapshot)
		if err != nil {
			t.Fatal(err)
		}
		return store, func(store Connections) error { return store.ApplyRepair(plan) }
	}, func(t *testing.T, store Connections) {
		records, err := store.List()
		if err != nil || len(records) != 1 || records[0].HostID != "owner-b" ||
			records[0].Trust.Fingerprint != newTrust.Fingerprint {
			t.Fatalf("combined repair did not converge to whole new state: %#v, %v", records, err)
		}
		assertRoutingMoved(t, store)
	}, ownerIOJournalWrite, ownerIOConnectionWrite, ownerIOCacheWrite, ownerIORoutingRename,
		ownerIODirectorySync, ownerIOJournalCleanup)
}

func TestRemovalRecoversFromEveryDurableIOBoundary(t *testing.T) {
	now := time.Now()
	snapshot := Snapshot{FetchedAt: now, Inventory: fixtureInventory("owner-a", now)}
	ownerSweep(t, 8, func(t *testing.T) (Connections, func(Connections) error) {
		store := Connections{Root: testkit.TempDir(t)}
		connection := Connection{HostID: "owner-a", Destination: "owner-alias"}
		if err := store.Write(connection); err != nil {
			t.Fatal(err)
		}
		if err := store.cache().Write(snapshot); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(store.routingPath("owner-a"), 0o700); err != nil {
			t.Fatal(err)
		}
		plan, err := store.PrepareRemoval(connection, snapshot)
		if err != nil {
			t.Fatal(err)
		}
		return store, func(store Connections) error {
			_, err := store.ApplyRemoval(context.Background(), plan, func(context.Context, Connection) (Snapshot, error) {
				return snapshot, nil
			})
			return err
		}
	}, func(t *testing.T, store Connections) {
		if records, err := store.List(); err != nil || len(records) != 0 {
			t.Fatalf("removal did not converge to whole new state: %#v, %v", records, err)
		}
		if _, err := store.cache().Read("owner-a"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("removed cache survived: %v", err)
		}
		if _, err := os.Lstat(store.routingPath("owner-a")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("routing survived: %v", err)
		}
	}, ownerIOJournalWrite, ownerIOStateDelete, ownerIODirectorySync, ownerIOJournalCleanup)
}

func adoptionFailureFixture(t *testing.T, trust SSHHostTrust, now time.Time) Connections {
	t.Helper()
	store := Connections{Root: testkit.TempDir(t)}
	if err := store.Write(Connection{HostID: "owner-a", Destination: "owner-alias", Trust: &trust}); err != nil {
		t.Fatal(err)
	}
	if err := store.cache().Write(Snapshot{FetchedAt: now, Inventory: fixtureInventory("owner-a", now)}); err != nil {
		t.Fatal(err)
	}
	routing := store.routingPath("owner-a")
	if err := os.MkdirAll(routing, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(routing, "state"), []byte("kept\n"), 0o600)
	return store
}

func assertRoutingMoved(t *testing.T, store Connections) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(store.routingPath("owner-b"), "state")); err != nil {
		t.Fatalf("routing state was not preserved: %v", err)
	}
	if _, err := os.Lstat(store.routingPath("owner-a")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old routing survived: %v", err)
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

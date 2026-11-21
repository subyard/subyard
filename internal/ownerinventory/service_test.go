package ownerinventory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

type fixedClock struct{ now time.Time }

func (clock fixedClock) Now() time.Time { return clock.now }

func fixtureInventory(hostID string, observed time.Time, projects ...string) domain.OwnerInventory {
	result := domain.OwnerInventory{
		Schema: domain.OwnerInventorySchema, HostID: hostID, ObservedAt: observed,
		Yards: []domain.OwnerYard{{
			Name: "default", Kind: "container", Instance: "subyard", State: "RUNNING",
			SSHPort: 2222, DevUser: "dev",
		}},
	}
	for _, name := range projects {
		result.Yards[0].Projects = append(result.Yards[0].Projects, domain.OwnerProject{
			ProjectID: name + "-id", Name: name, Mode: "sync", Target: "yard",
		})
	}
	return result
}

func TestFreshCacheDoesNotFetch(t *testing.T) {
	now := time.Unix(1000, 0)
	cache := Cache{Root: testkit.TempDir(t)}
	inventory := fixtureInventory("owner-a", now, "one")
	if err := cache.Write(Snapshot{FetchedAt: now, Inventory: inventory}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	result := (Service{
		Cache: cache, Clock: fixedClock{now: now.Add(10 * time.Second)},
		Fetch: func(context.Context, string) (domain.OwnerInventory, error) {
			calls++
			return domain.OwnerInventory{}, errors.New("unexpected")
		},
	}).Read(context.Background(), "owner-a", false)
	if result.Err != nil || result.Stale || calls != 0 || len(result.Inventory.Yards[0].Projects) != 1 {
		t.Fatalf("fresh cache read drifted: result=%#v calls=%d", result, calls)
	}
}

func TestForceRefreshBypassesFreshCache(t *testing.T) {
	now := time.Unix(1500, 0)
	cache := Cache{Root: testkit.TempDir(t)}
	if err := cache.Write(Snapshot{
		FetchedAt: now, Inventory: fixtureInventory("owner-a", now, "old"),
	}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	result := (Service{
		Cache: cache, Clock: fixedClock{now: now},
		Fetch: func(context.Context, string) (domain.OwnerInventory, error) {
			calls++
			return fixtureInventory("owner-a", now, "new"), nil
		},
	}).Read(context.Background(), "owner-a", true)
	if result.Err != nil || calls != 1 ||
		result.Inventory.Yards[0].Projects[0].Name != "new" {
		t.Fatalf("force refresh drifted: result=%#v calls=%d", result, calls)
	}
}

func TestStaleCacheRefreshReplacesDeletedProjects(t *testing.T) {
	now := time.Unix(2000, 0)
	cache := Cache{Root: testkit.TempDir(t)}
	if err := cache.Write(Snapshot{
		FetchedAt: now.Add(-time.Minute), Inventory: fixtureInventory("owner-a", now, "ghost"),
	}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	replacement := fixtureInventory("owner-a", now)
	result := (Service{
		Cache: cache, Clock: fixedClock{now: now},
		Fetch: func(_ context.Context, expected string) (domain.OwnerInventory, error) {
			calls++
			if expected != "owner-a" {
				t.Fatalf("unexpected expected HostID %q", expected)
			}
			return replacement, nil
		},
	}).Read(context.Background(), "owner-a", false)
	if result.Err != nil || result.Stale || calls != 1 ||
		len(result.Inventory.Yards[0].Projects) != 0 {
		t.Fatalf("stale refresh drifted: result=%#v calls=%d", result, calls)
	}
	persisted, err := cache.Read("owner-a")
	if err != nil || len(persisted.Inventory.Yards[0].Projects) != 0 {
		t.Fatalf("replacement was not atomic/complete: snapshot=%#v err=%v", persisted, err)
	}
}

func TestRefreshFailureReturnsExplicitStaleSnapshot(t *testing.T) {
	now := time.Unix(3000, 0)
	cache := Cache{Root: testkit.TempDir(t)}
	if err := cache.Write(Snapshot{
		FetchedAt: now.Add(-time.Minute), Inventory: fixtureInventory("owner-a", now, "kept"),
	}); err != nil {
		t.Fatal(err)
	}
	result := (Service{
		Cache: cache, Clock: fixedClock{now: now},
		Fetch: func(context.Context, string) (domain.OwnerInventory, error) {
			return domain.OwnerInventory{}, errors.New("offline")
		},
	}).Read(context.Background(), "owner-a", false)
	if result.Err == nil || !result.Stale || len(result.Inventory.Yards[0].Projects) != 1 {
		t.Fatalf("stale fallback drifted: %#v", result)
	}
}

func TestMissingSnapshotRefreshFailureIsUnavailable(t *testing.T) {
	result := (Service{
		Cache: Cache{Root: testkit.TempDir(t)}, Clock: fixedClock{now: time.Unix(3500, 0)},
		Fetch: func(context.Context, string) (domain.OwnerInventory, error) {
			return domain.OwnerInventory{}, errors.New("offline")
		},
	}).Read(context.Background(), "owner-a", false)
	if result.Err == nil || result.Stale || result.Inventory.HostID != "" {
		t.Fatalf("missing snapshot was not unavailable: %#v", result)
	}
}

func TestCacheUsesPrivateAtomicFile(t *testing.T) {
	root := testkit.TempDir(t)
	cache := Cache{Root: root}
	now := time.Unix(4000, 0)
	if err := cache.Write(Snapshot{
		FetchedAt: now, Inventory: fixtureInventory("owner-a", now),
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "owners", "owner-a.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("cache mode = %o, want 0600", info.Mode().Perm())
	}
}

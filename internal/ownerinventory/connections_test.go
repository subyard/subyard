package ownerinventory

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"golang.org/x/crypto/ssh"
)

func testSSHHostTrust(t *testing.T, host string) SSHHostTrust {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	line := host + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	trust, err := NewSSHHostTrust(line)
	if err != nil {
		t.Fatal(err)
	}
	return trust
}

func TestSSHHostTrustValidatesConcreteKeyAndFingerprint(t *testing.T) {
	trust := testSSHHostTrust(t, "owner.example")
	if !strings.HasPrefix(trust.Fingerprint, "SHA256:") {
		t.Fatalf("unexpected fingerprint %q", trust.Fingerprint)
	}
	if err := trust.Validate(); err != nil {
		t.Fatalf("valid trust rejected: %v", err)
	}
	tampered := trust
	tampered.Fingerprint = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if err := tampered.Validate(); err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("tampered fingerprint accepted: %v", err)
	}
	if _, err := (Connection{HostID: "owner-a", Destination: "owner.example"}).RequireTrust(); err == nil || !strings.Contains(err.Error(), "SSH host trust") {
		t.Fatalf("legacy connection proved continuity without trust: %v", err)
	}
}

func TestReadSSHHostTrustRequiresExactlyOneCapturedKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "candidate.known_hosts")
	trust := testSSHHostTrust(t, "owner.example")
	if err := os.WriteFile(path, []byte(trust.KnownHostsLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	read, err := ReadSSHHostTrust(path)
	if err != nil || read.Fingerprint != trust.Fingerprint {
		t.Fatalf("captured trust = %#v, %v", read, err)
	}
	if err := os.WriteFile(path, []byte(trust.KnownHostsLine+"\n"+trust.KnownHostsLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSSHHostTrust(path); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("multiple captured keys accepted: %v", err)
	}
}

func TestConnectionsCollapseAliasesAndRejectIdentityCollisions(t *testing.T) {
	store := Connections{Root: t.TempDir()}
	if err := store.Write(Connection{
		HostID: "owner-a", Destination: "dev@owner.example",
		LegacyNames: []string{"two", "one", "one"},
		Yards: map[string]YardRoute{
			"default": {SSHHost: "yard-one"},
			"inner":   {SSHHost: "yard-two"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	records, err := store.List()
	if err != nil || len(records) != 1 ||
		strings.Join(records[0].LegacyNames, ",") != "one,two" ||
		records[0].Yards["inner"].SSHHost != "yard-two" {
		t.Fatalf("connection migration drifted: records=%#v err=%v", records, err)
	}
	if err := store.Write(Connection{
		HostID: "owner-a", Destination: "dev@other.example",
	}); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("HostID collision was accepted: %v", err)
	}
	if err := store.Write(Connection{
		HostID: "owner-b", Destination: "dev@owner.example",
	}); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("destination collision was accepted: %v", err)
	}
}

func TestConnectionsWriteRejectsManagedTrustDowngradeOrReplacement(t *testing.T) {
	store := Connections{Root: t.TempDir()}
	trust := testSSHHostTrust(t, "owner.example")
	connection := Connection{
		HostID: "owner-a", Destination: "owner-alias", Trust: &trust,
	}
	if err := store.Write(connection); err != nil {
		t.Fatal(err)
	}
	withoutTrust := connection
	withoutTrust.Trust = nil
	if err := store.Write(withoutTrust); err == nil || !strings.Contains(err.Error(), "repair") {
		t.Fatalf("managed trust downgrade was accepted: %v", err)
	}
	replacement := testSSHHostTrust(t, "owner.example")
	withNewTrust := connection
	withNewTrust.Trust = &replacement
	if err := store.Write(withNewTrust); err == nil || !strings.Contains(err.Error(), "repair") {
		t.Fatalf("managed trust replacement bypassed repair: %v", err)
	}
}

func TestConnectionsRemoveRejectsProjectReferencesAndDeletesControllerState(t *testing.T) {
	root := t.TempDir()
	store := Connections{Root: root}
	connection := Connection{HostID: "owner-a", Destination: "dev@owner.example"}
	if err := store.Write(connection); err != nil {
		t.Fatal(err)
	}
	cache := Cache{Root: root}
	withProject := fixtureInventory("owner-a", time.Now(), "project-one")
	if err := cache.Write(Snapshot{FetchedAt: time.Now(), Inventory: withProject}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Remove("owner-a"); err == nil || !strings.Contains(err.Error(), "project reference") {
		t.Fatalf("remove accepted a referenced owner: %v", err)
	}
	if records, err := store.List(); err != nil || len(records) != 1 {
		t.Fatalf("failed removal mutated registration: records=%#v err=%v", records, err)
	}
	empty := fixtureInventory("owner-a", time.Now())
	if err := cache.Write(Snapshot{FetchedAt: time.Now(), Inventory: empty}); err != nil {
		t.Fatal(err)
	}
	routing := filepath.Join(root, "routing", "owner-a", "default", "projects")
	if err := os.MkdirAll(routing, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(routing, "stale.json"), []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Remove("owner-a"); err == nil || !strings.Contains(err.Error(), "routing state") {
		t.Fatalf("remove accepted project routing state: %v", err)
	}
	if err := os.Remove(filepath.Join(routing, "stale.json")); err != nil {
		t.Fatal(err)
	}
	removed, err := store.Remove("owner-a")
	if err != nil || removed.HostID != "owner-a" {
		t.Fatalf("remove = %#v, %v", removed, err)
	}
	if records, err := store.List(); err != nil || len(records) != 0 {
		t.Fatalf("registration remains: records=%#v err=%v", records, err)
	}
	if _, err := cache.Read("owner-a"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache remains after removal: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "routing", "owner-a")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("routing state remains after removal: %v", err)
	}
}

func TestConnectionsRemoveFailsClosedWithoutFreshAuthoritativeSnapshot(t *testing.T) {
	root := t.TempDir()
	store := Connections{Root: root}
	connection := Connection{HostID: "owner-a", Destination: "dev@owner.example"}
	if err := store.Write(connection); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Remove("owner-a"); err == nil || !strings.Contains(err.Error(), "no authoritative") {
		t.Fatalf("remove without snapshot did not fail closed: %v", err)
	}
	if err := (Cache{Root: root}).Write(Snapshot{
		FetchedAt: time.Now().Add(-Freshness - time.Second),
		Inventory: fixtureInventory("owner-a", time.Now()),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Remove("owner-a"); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("remove with stale snapshot did not fail closed: %v", err)
	}
}

func TestRemovalPlanRejectsConnectionChangedAfterConfirmation(t *testing.T) {
	root := t.TempDir()
	store := Connections{Root: root}
	connection := Connection{HostID: "owner-a", Destination: "owner.example"}
	if err := store.Write(connection); err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{FetchedAt: time.Now(), Inventory: fixtureInventory("owner-a", time.Now())}
	plan, err := store.PrepareRemoval(connection, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	changed := connection
	changed.Yards = map[string]YardRoute{"default": {SSHHost: "yard-owner"}}
	if err := store.Write(changed); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyRemoval(plan); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("stale removal plan deleted a changed connection: %v", err)
	}
	if records, err := store.List(); err != nil || len(records) != 1 || len(records[0].Yards) != 1 {
		t.Fatalf("stale removal mutated current registration: %#v, %v", records, err)
	}
}

func TestRemovalPlanRequiresCurrentSnapshot(t *testing.T) {
	now := time.Unix(1_000, 0).UTC()
	for _, test := range []struct {
		name      string
		fetchedAt time.Time
	}{
		{name: "stale", fetchedAt: now.Add(-Freshness - time.Second)},
		{name: "future-clock-skew", fetchedAt: now.Add(Freshness + time.Second)},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			store := Connections{Root: root, now: func() time.Time { return now }}
			connection := Connection{HostID: "owner-a", Destination: "owner.example"}
			if err := store.Write(connection); err != nil {
				t.Fatal(err)
			}
			snapshot := Snapshot{
				FetchedAt: test.fetchedAt,
				Inventory: fixtureInventory("owner-a", now),
			}
			if _, err := store.PrepareRemoval(connection, snapshot); err == nil ||
				!strings.Contains(err.Error(), "stale") {
				t.Fatalf("PrepareRemoval accepted %s snapshot: %v", test.name, err)
			}
			if records, err := store.List(); err != nil || len(records) != 1 {
				t.Fatalf("rejected removal mutated registration: %#v, %v", records, err)
			}
		})
	}
}

func TestRemovalPlanExpiresBeforeApply(t *testing.T) {
	now := time.Unix(1_000, 0).UTC()
	root := t.TempDir()
	store := Connections{Root: root, now: func() time.Time { return now }}
	connection := Connection{HostID: "owner-a", Destination: "owner.example"}
	if err := store.Write(connection); err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{FetchedAt: now, Inventory: fixtureInventory("owner-a", now)}
	plan, err := store.PrepareRemoval(connection, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(Freshness + time.Second)
	if _, err := store.ApplyRemoval(plan); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("ApplyRemoval accepted expired snapshot: %v", err)
	}
	if records, err := store.List(); err != nil || len(records) != 1 {
		t.Fatalf("expired removal mutated registration: %#v, %v", records, err)
	}
}

func TestConnectionsRegisterPersistsConnectionTrustAndInitialSnapshotTogether(t *testing.T) {
	root := t.TempDir()
	store := Connections{Root: root}
	trust := testSSHHostTrust(t, "owner.example")
	connection := Connection{
		HostID: "owner-a", Destination: "owner-alias", Trust: &trust,
	}
	snapshot := Snapshot{
		FetchedAt: time.Unix(123, 0).UTC(),
		Inventory: fixtureInventory("owner-a", time.Unix(122, 0).UTC()),
	}
	if err := store.Register(connection, snapshot); err != nil {
		t.Fatal(err)
	}
	records, err := store.List()
	if err != nil || len(records) != 1 || records[0].Trust == nil ||
		records[0].Trust.Fingerprint != trust.Fingerprint {
		t.Fatalf("registered connection = %#v, %v", records, err)
	}
	cached, err := (Cache{Root: root}).Read("owner-a")
	if err != nil || !cached.FetchedAt.Equal(snapshot.FetchedAt) {
		t.Fatalf("initial snapshot = %#v, %v", cached, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "registration.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed registration journal remains: %v", err)
	}
}

func TestConnectionsAdoptHostIDMigratesConnectionAndCacheWithoutAlias(t *testing.T) {
	root := t.TempDir()
	store := Connections{Root: root}
	connection := Connection{
		HostID: "owner-a", Destination: "dev@owner.example",
		Trust: func() *SSHHostTrust { trust := testSSHHostTrust(t, "owner.example"); return &trust }(),
		Yards: map[string]YardRoute{"default": {SSHHost: "yard-demo"}},
	}
	if err := store.Write(connection); err != nil {
		t.Fatal(err)
	}
	cache := Cache{Root: root}
	inventory := fixtureInventory("owner-a", time.Now())
	if err := cache.Write(Snapshot{FetchedAt: time.Now(), Inventory: inventory}); err != nil {
		t.Fatal(err)
	}
	oldRouting := filepath.Join(root, "routing", "owner-a", "default", "projects")
	if err := os.MkdirAll(oldRouting, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldRouting, "project.json"), []byte("project-state\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	proof := IdentityProof{
		ExpectedHostID: "owner-a", ObservedHostID: "owner-b",
		Destination: "dev@owner.example", TrustFingerprint: connection.Trust.Fingerprint,
	}
	fetchedAt := time.Unix(456, 0).UTC()
	if err := store.AdoptHostID(proof, Snapshot{
		FetchedAt: fetchedAt, Inventory: inventoryWithHostID(inventory, "owner-b"),
	}); err != nil {
		t.Fatal(err)
	}
	records, err := store.List()
	if err != nil || len(records) != 1 || records[0].HostID != "owner-b" ||
		records[0].Destination != connection.Destination {
		t.Fatalf("adopted connections = %#v err=%v", records, err)
	}
	if _, err := cache.Read("owner-a"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old cache remains readable: %v", err)
	}
	updated, err := cache.Read("owner-b")
	if err != nil || updated.Inventory.HostID != "owner-b" || !updated.FetchedAt.Equal(fetchedAt) {
		t.Fatalf("new cache = %#v err=%v", updated, err)
	}
	newProject := filepath.Join(root, "routing", "owner-b", "default", "projects", "project.json")
	if payload, err := os.ReadFile(newProject); err != nil || string(payload) != "project-state\n" {
		t.Fatalf("routing/project state was not migrated: payload=%q err=%v", payload, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "routing", "owner-a")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old HostID routing scope remains: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "connections", "owner-a.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old HostID alias remains: %v", err)
	}
}

func TestConnectionsAdoptHostIDFailsClosedOnProofAndCollision(t *testing.T) {
	for _, test := range []struct {
		name  string
		proof IdentityProof
		other bool
	}{
		{
			name: "missing trust proof", proof: IdentityProof{
				ExpectedHostID: "owner-a", ObservedHostID: "owner-b",
				Destination: "dev@owner.example",
			},
		},
		{
			name: "different destination", proof: IdentityProof{
				ExpectedHostID: "owner-a", ObservedHostID: "owner-b",
				Destination: "dev@attacker.example", TrustFingerprint: "SHA256:wrong",
			},
		},
		{
			name: "new HostID collision", other: true, proof: IdentityProof{
				ExpectedHostID: "owner-a", ObservedHostID: "owner-b",
				Destination: "dev@owner.example", TrustFingerprint: "SHA256:wrong",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			store := Connections{Root: root}
			trust := testSSHHostTrust(t, "owner.example")
			if err := store.Write(Connection{
				HostID: "owner-a", Destination: "dev@owner.example", Trust: &trust,
			}); err != nil {
				t.Fatal(err)
			}
			if test.other {
				if err := store.Write(Connection{HostID: "owner-b", Destination: "dev@other.example"}); err != nil {
					t.Fatal(err)
				}
			}
			inventory := fixtureInventory("owner-b", time.Now())
			if err := store.AdoptHostID(test.proof, Snapshot{
				FetchedAt: time.Now(), Inventory: inventory,
			}); err == nil {
				t.Fatal("unsafe HostID adoption succeeded")
			}
			records, err := store.List()
			if err != nil || records[0].HostID != "owner-a" {
				t.Fatalf("failed adoption mutated registry: %#v err=%v", records, err)
			}
		})
	}
}

func TestConnectionsPreparedRepairAcceptsChangedKeyAndHostID(t *testing.T) {
	root := t.TempDir()
	store := Connections{Root: root}
	oldTrust := testSSHHostTrust(t, "owner.example")
	newTrust := testSSHHostTrust(t, "owner.example")
	connection := Connection{
		HostID: "owner-a", Destination: "owner-alias", Trust: &oldTrust,
	}
	if err := store.Write(connection); err != nil {
		t.Fatal(err)
	}
	oldInventory := fixtureInventory("owner-a", time.Unix(100, 0).UTC())
	if err := (Cache{Root: root}).Write(Snapshot{
		FetchedAt: time.Unix(101, 0).UTC(), Inventory: oldInventory,
	}); err != nil {
		t.Fatal(err)
	}
	routing := filepath.Join(root, "routing", "owner-a", "default", "projects")
	if err := os.MkdirAll(routing, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(routing, "one.json"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{
		FetchedAt: time.Unix(200, 0).UTC(),
		Inventory: inventoryWithHostID(oldInventory, "owner-b"),
	}
	plan, err := store.PrepareRepair("owner-a", newTrust, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if plan.OldHostID != "owner-a" || plan.NewHostID != "owner-b" ||
		plan.OldFingerprint != oldTrust.Fingerprint || plan.NewFingerprint != newTrust.Fingerprint {
		t.Fatalf("repair assessment is not exact: %#v", plan)
	}
	if err := store.ApplyRepair(plan); err != nil {
		t.Fatal(err)
	}
	records, err := store.List()
	if err != nil || len(records) != 1 || records[0].HostID != "owner-b" ||
		records[0].Trust == nil || records[0].Trust.Fingerprint != newTrust.Fingerprint {
		t.Fatalf("repaired connection = %#v, %v", records, err)
	}
	if _, err := os.Stat(filepath.Join(root, "routing", "owner-b", "default", "projects", "one.json")); err != nil {
		t.Fatalf("repair did not migrate project routing: %v", err)
	}
}

func TestConnectionsAdoptHostIDRejectsOrphanTargetCacheBeforeMutation(t *testing.T) {
	root := t.TempDir()
	store := Connections{Root: root}
	trust := testSSHHostTrust(t, "owner.example")
	connection := Connection{
		HostID: "owner-a", Destination: "owner-alias", Trust: &trust,
	}
	if err := store.Write(connection); err != nil {
		t.Fatal(err)
	}
	cache := Cache{Root: root}
	oldSnapshot := Snapshot{
		FetchedAt: time.Unix(100, 0).UTC(),
		Inventory: fixtureInventory("owner-a", time.Unix(99, 0).UTC()),
	}
	if err := cache.Write(oldSnapshot); err != nil {
		t.Fatal(err)
	}
	orphan := Snapshot{
		FetchedAt: time.Unix(50, 0).UTC(),
		Inventory: fixtureInventory("owner-b", time.Unix(49, 0).UTC()),
	}
	if err := cache.Write(orphan); err != nil {
		t.Fatal(err)
	}
	err := store.AdoptHostID(IdentityProof{
		ExpectedHostID: "owner-a", ObservedHostID: "owner-b", Destination: "owner-alias",
		TrustFingerprint: trust.Fingerprint,
	}, Snapshot{
		FetchedAt: time.Unix(200, 0).UTC(),
		Inventory: fixtureInventory("owner-b", time.Unix(199, 0).UTC()),
	})
	if err == nil || !strings.Contains(err.Error(), "cache") {
		t.Fatalf("orphan target cache was overwritten: %v", err)
	}
	records, listErr := store.List()
	if listErr != nil || len(records) != 1 || records[0].HostID != "owner-a" {
		t.Fatalf("failed preflight mutated connection: %#v, %v", records, listErr)
	}
	remaining, readErr := cache.Read("owner-b")
	if readErr != nil || !remaining.FetchedAt.Equal(orphan.FetchedAt) {
		t.Fatalf("orphan target cache changed: %#v, %v", remaining, readErr)
	}
}

func inventoryWithHostID(inventory domain.OwnerInventory, hostID string) domain.OwnerInventory {
	inventory.HostID = hostID
	return inventory
}

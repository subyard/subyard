package testvmsruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLeaseStoreConfiguredCapacityMatrix(t *testing.T) {
	for _, count := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("N=%d", count), func(t *testing.T) {
			store := LeaseStore{
				Path: filepath.Join(t.TempDir(), "leases.json"), SlotCount: count,
			}
			var grants []LeaseGrant
			for index := 0; index < count; index++ {
				slotID := fmt.Sprintf("slot-%03d", index+1)
				grant, err := store.AcquireSlot(
					fmt.Sprintf("client-%d", index), "SHA256:key", "", "", slotID,
				)
				if err != nil {
					t.Fatal(err)
				}
				grants = append(grants, grant)
			}
			if _, err := store.AcquireSlot(
				"overflow", "SHA256:key", "", "", fmt.Sprintf("slot-%03d", count+1),
			); !errors.Is(err, ErrInvalidSlot) {
				t.Fatalf("out-of-pool exact acquire error = %v", err)
			}
			seen := map[string]bool{}
			for _, grant := range grants {
				seen[grant.SlotID] = true
			}
			if len(seen) != count {
				t.Fatalf("distinct winners = %d, want %d", len(seen), count)
			}
		})
	}
}

func TestLeaseStoreAcquireSlotDoesNotFallback(t *testing.T) {
	store := LeaseStore{Path: filepath.Join(t.TempDir(), "leases.json"), SlotCount: 2}
	grant, err := store.AcquireSlot(
		"exact", "SHA256:key", "checkout", "tests", "slot-002",
	)
	if err != nil {
		t.Fatal(err)
	}
	if grant.SlotID != "slot-002" {
		t.Fatalf("exact acquire returned %s", grant.SlotID)
	}
	if grant.ResourceGeneration == 0 {
		t.Fatal("exact acquire omitted the resource generation")
	}
	before, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	if before.Slots[0].State != SlotAvailable ||
		before.Slots[1].State != SlotProvisioning {
		t.Fatalf("unexpected exact acquire state: %#v", before.Slots)
	}
	if grant.ResourceGeneration != before.Slots[1].ResourceGeneration {
		t.Fatalf("grant generation=%d slot generation=%d",
			grant.ResourceGeneration, before.Slots[1].ResourceGeneration)
	}
	if _, err := store.AcquireSlot(
		"second", "SHA256:key", "", "", "slot-002",
	); !errors.Is(err, ErrLeaseBusy) {
		t.Fatalf("occupied exact acquire error = %v", err)
	}
	after, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	if after.Slots[0] != before.Slots[0] {
		t.Fatalf("failed exact acquire mutated neighbor: before=%#v after=%#v",
			before.Slots[0], after.Slots[0])
	}
	if err := store.Quarantine(grant, errors.New("fixture failure")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireSlot(
		"third", "SHA256:key", "", "", "slot-002",
	); !errors.Is(err, ErrLeaseBusy) {
		t.Fatalf("quarantined exact acquire error = %v", err)
	}
	neighbor, err := store.AcquireSlot(
		"neighbor", "SHA256:key", "", "", "slot-001",
	)
	if err != nil {
		t.Fatal(err)
	}
	if neighbor.SlotID != "slot-001" {
		t.Fatalf("neighbor exact acquire returned %s", neighbor.SlotID)
	}
	if _, err := store.AcquireSlot(
		"invalid", "SHA256:key", "", "", "slot-2",
	); err == nil || errors.Is(err, ErrLeaseBusy) {
		t.Fatalf("non-canonical slot error = %v", err)
	}
}

func TestLeaseStoreExactBusyOwnerTracksHeldLeaseLifecycle(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	store := LeaseStore{
		Path: filepath.Join(t.TempDir(), "leases.json"), SlotCount: 1,
		Now: func() time.Time { return now },
	}
	grant, err := store.AcquireV2Slot(
		"owner", "SHA256:controller", "test-yard", "Subyard-2", "run-a", "unit-tests",
		"slot-001",
	)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	heldExpires, err := store.MarkHeld(grant)
	if err != nil {
		t.Fatal(err)
	}
	assertBusyOwner := func(wantExpires time.Time) {
		t.Helper()
		_, acquireErr := store.AcquireV2Slot(
			"contender", "SHA256:other", "test-yard", "Subyard-3", "run-b", "other-tests",
			"slot-001",
		)
		var unavailable *SlotUnavailableError
		if !errors.As(acquireErr, &unavailable) {
			t.Fatalf("exact held acquire error = %v", acquireErr)
		}
		if unavailable.State != SlotHeld || unavailable.Reason != "busy" ||
			unavailable.Owner == nil {
			t.Fatalf("exact held result = %#v", unavailable)
		}
		owner := unavailable.Owner
		if owner.DisplayLabel != "Subyard-2#run-a" || owner.Yard != "test-yard" ||
			owner.Project != "Subyard-2" || owner.Run != "run-a" ||
			owner.Purpose != "unit-tests" || !owner.AcquiredAt.Equal(grant.ExpiresAt.Add(-ProvisioningTTL)) ||
			!owner.ExpiresAt.Equal(wantExpires) {
			t.Fatalf("exact held owner = %#v, want expiry %s", owner, wantExpires)
		}
	}
	assertBusyOwner(heldExpires)

	now = now.Add(time.Minute)
	renewedExpires, err := store.Renew(grant)
	if err != nil {
		t.Fatal(err)
	}
	assertBusyOwner(renewedExpires)

	now = renewedExpires.Add(time.Second)
	_, err = store.AcquireV2Slot(
		"late", "SHA256:late", "test-yard", "Subyard-3", "run-c", "late-tests", "slot-001",
	)
	var expired *SlotUnavailableError
	if !errors.As(err, &expired) || expired.State != SlotDraining ||
		expired.Reason != "draining" || expired.Owner != nil {
		t.Fatalf("expired held result = %#v, error=%v", expired, err)
	}
	if err := store.FinishDrain("slot-001", nil); err != nil {
		t.Fatal(err)
	}
	pool, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	slot := pool.Slots[0]
	if slot.State != SlotAvailable || slot.DisplayLabel != "" || slot.Yard != "" ||
		slot.Project != "" || slot.Run != "" || slot.Purpose != "" ||
		!slot.AcquiredAt.IsZero() || !slot.ExpiresAt.IsZero() {
		t.Fatalf("released slot retained owner attribution: %#v", slot)
	}
}

func TestLeaseStoreInspectIsReadOnlyAndExpiresOnlyInMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "leases.json")
	now := time.Unix(1_000, 0).UTC()
	pool := LeasePool{
		SchemaVersion: LeaseSchemaVersion, ResourceType: "agent-e2e", ResourceID: "test-vms",
		Slots: []LeaseSlot{{
			SlotID: "slot-001", State: SlotHeld,
			ExpiresAt: now.Add(-time.Second),
		}},
	}
	payload, err := json.Marshal(pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	store := LeaseStore{Path: path, SlotCount: 1, Now: func() time.Time { return now }}
	inspected, err := store.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if len(inspected.Slots) != 1 || inspected.Slots[0].State != SlotDraining {
		t.Fatalf("inspect state=%#v, want expired slot draining", inspected.Slots)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("read-only inspection rewrote lease state")
	}
	if _, err := os.Stat(path + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only inspection created lock: %v", err)
	}
}

func TestLeaseStoreInspectRejectsNoncanonicalSlotInventory(t *testing.T) {
	for _, test := range []struct {
		name  string
		count int
		slots []LeaseSlot
	}{
		{
			name: "duplicate", count: 2,
			slots: []LeaseSlot{{SlotID: "slot-001", State: SlotAvailable}, {SlotID: "slot-001", State: SlotAvailable}},
		},
		{
			name: "noncanonical id", count: 1,
			slots: []LeaseSlot{{SlotID: "slot-1", State: SlotAvailable}},
		},
		{
			name: "unconfigured id", count: 1,
			slots: []LeaseSlot{{SlotID: "slot-002", State: SlotAvailable}},
		},
		{
			name: "unknown state", count: 1,
			slots: []LeaseSlot{{SlotID: "slot-001", State: SlotState("unknown")}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "leases.json")
			payload, err := json.Marshal(LeasePool{
				SchemaVersion: LeaseSchemaVersion, ResourceType: "agent-e2e",
				ResourceID: "test-vms", Slots: test.slots,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := (LeaseStore{Path: path, SlotCount: test.count}).Inspect(); err == nil {
				t.Fatalf("Inspect accepted slots=%#v", test.slots)
			}
		})
	}
}

func TestLeaseStoreConcurrentCapacityAndFencing(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	store := LeaseStore{
		Path: filepath.Join(t.TempDir(), "leases.json"), SlotCount: 2,
		Now: func() time.Time { return now },
	}
	var wait sync.WaitGroup
	results := make(chan LeaseGrant, 3)
	failures := make(chan error, 3)
	requestedSlots := []string{"slot-001", "slot-002", "slot-001"}
	for index, slotID := range requestedSlots {
		wait.Add(1)
		go func(index int, slotID string) {
			defer wait.Done()
			grant, err := store.AcquireSlot(
				string(rune('a'+index)), "SHA256:controller", "checkout", "test", slotID,
			)
			if err != nil {
				failures <- err
			} else {
				results <- grant
			}
		}(index, slotID)
	}
	wait.Wait()
	close(results)
	close(failures)
	var grants []LeaseGrant
	for grant := range results {
		grants = append(grants, grant)
	}
	if len(grants) != 2 || len(failures) != 1 {
		t.Fatalf("winners=%d losers=%d", len(grants), len(failures))
	}
	if grants[0].SlotID == grants[1].SlotID {
		t.Fatal("concurrent leases received the same slot")
	}
	if _, err := store.MarkHeld(grants[0]); err != nil {
		t.Fatal(err)
	}
	stale := grants[0]
	stale.Capability = "wrong"
	if _, err := store.Renew(stale); err == nil {
		t.Fatal("wrong capability renewed a lease")
	}
	if err := store.BeginDrain(grants[0]); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishDrain(grants[0].SlotID, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishDrain(grants[0].SlotID, nil); err != nil {
		t.Fatalf("replayed successful fencing was not idempotent: %v", err)
	}
	next, err := store.AcquireSlot(
		"next", "SHA256:controller", "", "", grants[0].SlotID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if next.LeaseEpoch <= grants[0].LeaseEpoch {
		t.Fatal("lease epoch did not fence the previous holder")
	}
	if _, err := store.Renew(grants[0]); err == nil {
		t.Fatal("released lease became valid again")
	}
}

func TestLeaseStoreExpiryShrinkAndQuarantine(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "leases.json")
	store := LeaseStore{Path: path, SlotCount: 3, Now: func() time.Time { return now }}
	grant, err := store.AcquireSlot("client", "SHA256:key", "", "", "slot-001")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(LeaseTTL)
	pool, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	if pool.Slots[0].State != SlotProvisioning {
		t.Fatalf("live provisioning state=%s", pool.Slots[0].State)
	}
	now = now.Add(ProvisioningTTL - LeaseTTL)
	pool, err = store.Status()
	if err != nil {
		t.Fatal(err)
	}
	if pool.Slots[0].State != SlotDraining {
		t.Fatalf("expired state=%s", pool.Slots[0].State)
	}
	if pool.Slots[0].FailureReason != provisioningExpiredReason {
		t.Fatalf("expired provisioning reason=%q", pool.Slots[0].FailureReason)
	}
	shrunk := LeaseStore{Path: path, SlotCount: 1, Now: store.Now}
	retiring, err := shrunk.PrepareResize()
	if err != nil || len(retiring) != 2 {
		t.Fatalf("prepare available higher slots: retiring=%v err=%v", retiring, err)
	}
	if err := shrunk.CommitResize(); err != nil {
		t.Fatalf("commit available higher slots: %v", err)
	}
	if err := shrunk.FinishDrain(grant.SlotID, errors.New("stop failed")); err != nil {
		t.Fatal(err)
	}
	pool, err = shrunk.Status()
	if err != nil {
		t.Fatal(err)
	}
	if pool.Slots[0].State != SlotQuarantined || pool.Slots[0].FailureReason == "" {
		t.Fatalf("quarantine not recorded: %#v", pool.Slots[0])
	}
}

func TestLeaseStoreHeldDeadlineStartsAfterProvisioning(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	store := LeaseStore{
		Path:      filepath.Join(t.TempDir(), "leases.json"),
		SlotCount: 1,
		Now:       func() time.Time { return now },
	}
	grant, err := store.AcquireSlot("client", "SHA256:key", "", "", "slot-001")
	if err != nil {
		t.Fatal(err)
	}
	// A cold pair provisions two VMs serially and can exceed 60 minutes when
	// image pulls and package installation share a constrained P0 host.
	if want := now.Add(90 * time.Minute); !grant.ExpiresAt.Equal(want) {
		t.Fatalf("provisioning expiry=%s, want %s", grant.ExpiresAt, want)
	}
	now = now.Add(LeaseTTL)
	expires, err := store.MarkHeld(grant)
	if err != nil {
		t.Fatal(err)
	}
	if want := now.Add(LeaseTTL); !expires.Equal(want) {
		t.Fatalf("held expiry=%s, want %s", expires, want)
	}
	now = now.Add(LeaseTTL)
	pool, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	if pool.Slots[0].State != SlotDraining ||
		pool.Slots[0].FailureReason != heartbeatExpiredReason {
		t.Fatalf("expired held slot=%#v", pool.Slots[0])
	}
}

func TestProvisioningDeadlineContainsColdToolchainBudget(t *testing.T) {
	want := time.Duration(provisionedGuestCount)*guestToolchainTimeout +
		provisioningSafetyMargin
	if ProvisioningTTL != want {
		t.Fatalf("provisioning TTL=%s, cold pair budget with margin=%s",
			ProvisioningTTL, want)
	}
	if guestToolchainTimeout != 30*time.Minute ||
		provisioningSafetyMargin != 30*time.Minute ||
		ProvisioningTTL != 90*time.Minute {
		t.Fatalf("unexpected deadline contract: guest=%s margin=%s lease=%s",
			guestToolchainTimeout, provisioningSafetyMargin, ProvisioningTTL)
	}
}

func TestLeaseStoreBlockedShrinkDoesNotMutateState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "leases.json")
	store := LeaseStore{Path: path, SlotCount: 3}
	for index := 0; index < 3; index++ {
		if _, err := store.AcquireSlot(
			fmt.Sprintf("client-%d", index), "SHA256:key", "", "",
			fmt.Sprintf("slot-%03d", index+1),
		); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	shrunk := LeaseStore{Path: path, SlotCount: 1}
	if _, _, err := shrunk.ResizePlan(); err == nil ||
		!strings.Contains(err.Error(), "slot-002 is provisioning") {
		t.Fatalf("blocked shrink error = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("blocked shrink changed lease state")
	}
}

func TestLeaseStoreRedactsCapability(t *testing.T) {
	store := LeaseStore{Path: filepath.Join(t.TempDir(), "leases.json"), SlotCount: 1}
	grant, err := store.AcquireSlot(
		"client", "SHA256:key", "label", "purpose", "slot-001",
	)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	if pool.Slots[0].CapabilityHash == "" ||
		pool.Slots[0].CapabilityHash == grant.Capability {
		t.Fatal("capability was not stored in verifier-only form")
	}
}

func TestLeaseStoreCorruptStateRecoveryKeepsOtherSlotsQuarantined(t *testing.T) {
	path := filepath.Join(t.TempDir(), "leases.json")
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := LeaseStore{Path: path, SlotCount: 2}
	if _, err := store.Status(); !errors.Is(err, ErrCorruptLeaseState) {
		t.Fatalf("status error = %v", err)
	}
	if err := store.BeginRecovery("slot-001"); err != nil {
		t.Fatal(err)
	}
	pool, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	if pool.Slots[0].State != SlotQuarantined ||
		pool.Slots[1].State != SlotQuarantined {
		t.Fatalf("recovery pool = %#v", pool.Slots)
	}
	backups, err := filepath.Glob(path + ".corrupt-*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("corrupt-state backups = %v, %v", backups, err)
	}
}

func TestLeaseStoreCombinedLabelPublishesOnlyCurrentAttribution(t *testing.T) {
	store := LeaseStore{Path: filepath.Join(t.TempDir(), "leases.json"), SlotCount: 1}
	grant, err := store.AcquireSlot(
		"client", "SHA256:key", "Subyard/Subyard@checkout-a#run-a", "unit-tests",
		"slot-001",
	)
	if err != nil {
		t.Fatal(err)
	}
	if grant.Context == nil ||
		grant.Context.Project != "Subyard/Subyard" ||
		grant.Context.Checkout != "checkout-a" ||
		grant.Context.Run != "run-a" ||
		grant.Context.Purpose != "unit-tests" {
		t.Fatalf("grant context = %#v", grant.Context)
	}
	pool, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	slot := pool.Slots[0]
	if slot.Project != grant.Context.Project ||
		slot.Checkout != grant.Context.Checkout ||
		slot.Run != grant.Context.Run ||
		slot.Purpose != grant.Context.Purpose {
		t.Fatalf("slot attribution = %#v", slot)
	}
	if err := store.BeginDrain(grant); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishDrain(grant.SlotID, nil); err != nil {
		t.Fatal(err)
	}
	pool, err = store.Status()
	if err != nil {
		t.Fatal(err)
	}
	slot = pool.Slots[0]
	if slot.Project != "" || slot.Checkout != "" ||
		slot.Run != "" || slot.Purpose != "" {
		t.Fatalf("released slot retained attribution: %#v", slot)
	}
}

func TestLeaseStoreV2PublishesCanonicalProjectWithoutCheckout(t *testing.T) {
	store := LeaseStore{Path: filepath.Join(t.TempDir(), "leases.json"), SlotCount: 1}
	grant, err := store.AcquireV2Slot(
		"client", "SHA256:key", "default", "Subyard-2", "run-a", "unit-tests",
		"slot-001",
	)
	if err != nil {
		t.Fatal(err)
	}
	if grant.Context == nil || grant.Context.SchemaVersion != 2 ||
		grant.Context.Yard != "default" || grant.Context.Project != "Subyard-2" ||
		grant.Context.Checkout != "" || grant.Context.Run != "run-a" {
		t.Fatalf("v2 grant context = %#v", grant.Context)
	}
	pool, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	slot := pool.Slots[0]
	if slot.Yard != "default" || slot.Project != "Subyard-2" ||
		slot.Checkout != "" || slot.DisplayLabel != "Subyard-2#run-a" {
		t.Fatalf("v2 slot = %#v", slot)
	}
	if _, err := store.MarkHeld(grant); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Renew(grant); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseStoreRejectsUnsafeAttributionBeforeMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "leases.json")
	store := LeaseStore{Path: path, SlotCount: 1}
	if _, err := store.AcquireSlot(
		"client", "SHA256:key", "/home/dev/private@checkout#run", "tests", "slot-001",
	); err == nil {
		t.Fatal("unsafe attribution was accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("invalid attribution mutated state: %v", err)
	}
}

func TestLeaseStoreLoadsAdditiveSchemaAndPreservesAttribution(t *testing.T) {
	path := filepath.Join(t.TempDir(), "leases.json")
	payload := `{
	  "schema_version": 1,
	  "resource_type": "agent-e2e",
	  "resource_id": "test-vms",
	  "slots": [{
	    "slot_id": "slot-001",
	    "resource_generation": 7,
	    "lease_epoch": 11,
	    "state": "quarantined",
	    "project": "Subyard/Subyard",
	    "checkout": "checkout-a",
	    "run": "run-a",
	    "purpose": "migration"
	  }]
	}`
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	store := LeaseStore{Path: path, SlotCount: 1}
	pool, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	slot := pool.Slots[0]
	if pool.SchemaVersion != LeaseSchemaVersion ||
		slot.ResourceGeneration != 7 ||
		slot.LeaseEpoch != 11 ||
		slot.Project != "Subyard/Subyard" ||
		slot.Checkout != "checkout-a" ||
		slot.Run != "run-a" ||
		slot.Purpose != "migration" {
		t.Fatalf("upgraded lease state = %#v", pool)
	}
}

func TestLeaseRecoveryJournalSurvivesPreviousProducerRewrite(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "leases.json")
	store := LeaseStore{
		Path:      path,
		SlotCount: 1,
		Now:       func() time.Time { return now },
	}
	grant, err := store.AcquireSlot(
		"client", "SHA256:key", "", "rollback", "slot-001",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Quarantine(grant, errors.New("stop timeout")); err != nil {
		t.Fatal(err)
	}
	failureID := "00000000000000000001-0123456789abcdef"
	incidentID := "00000000000000000002-fedcba9876543210"
	if err := store.SetQuarantineIncident(grant.SlotID, failureID, incidentID); err != nil {
		t.Fatal(err)
	}
	if _, started, err := store.BeginScheduledRecovery(grant.SlotID, false); err != nil ||
		!started {
		t.Fatalf("begin recovery = %v, %v", started, err)
	}
	if _, err := store.FinishRecovery(
		grant.SlotID,
		errors.New("capacity unavailable"),
		"",
		"",
	); err != nil {
		t.Fatal(err)
	}

	// The immediate previous producer accepts schema 1 but rewrites only fields
	// known to that release. Its status/heartbeat write must not erase the
	// current producer's recovery schedule.
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var previous map[string]any
	if err := json.Unmarshal(payload, &previous); err != nil {
		t.Fatal(err)
	}
	slots := previous["slots"].([]any)
	slot := slots[0].(map[string]any)
	for _, name := range []string{
		"last_failure_event_id",
		"incident_id",
		"recovery_attempt",
		"next_recovery_at",
		"recovery_started_at",
	} {
		delete(slot, name)
	}
	previousPayload, err := json.MarshalIndent(previous, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(previousPayload, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	pool, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	recovered := pool.Slots[0]
	if recovered.State != SlotQuarantined ||
		recovered.LastFailureEventID != failureID ||
		recovered.IncidentID != incidentID ||
		recovered.RecoveryAttempt != 1 ||
		!recovered.NextRecoveryAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("rollback recovery metadata = %#v", recovered)
	}
}

func TestLeaseStoreRecoveryScheduleGenerationAndFencing(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	store := LeaseStore{
		Path:      filepath.Join(t.TempDir(), "leases.json"),
		SlotCount: 1,
		Now:       func() time.Time { return now },
	}
	grant, err := store.AcquireSlot(
		"client",
		"SHA256:key",
		"Subyard/Subyard@checkout-a#run-a",
		"recovery",
		"slot-001",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Quarantine(grant, errors.New("stop timeout")); err != nil {
		t.Fatal(err)
	}
	failureID := "00000000000000000001-0123456789abcdef"
	incidentID := "00000000000000000002-fedcba9876543210"
	if err := store.SetQuarantineIncident(
		grant.SlotID,
		failureID,
		incidentID,
	); err != nil {
		t.Fatal(err)
	}
	first, started, err := store.BeginScheduledRecovery(grant.SlotID, false)
	if err != nil || !started || first.RecoveryAttempt != 1 ||
		first.State != SlotRecovering {
		t.Fatalf("first recovery = %#v, %v, %v", first, started, err)
	}
	failed, err := store.FinishRecovery(
		grant.SlotID,
		errors.New("capacity offline"),
		"",
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	if failed.State != SlotQuarantined ||
		!failed.NextRecoveryAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("first retry = %#v", failed)
	}
	now = now.Add(30 * time.Second)
	_, started, err = store.BeginScheduledRecovery(grant.SlotID, false)
	if err != nil || started {
		t.Fatalf("early retry = %v, %v", started, err)
	}
	now = now.Add(30 * time.Second)
	second, started, err := store.BeginScheduledRecovery(grant.SlotID, false)
	if err != nil || !started || second.RecoveryAttempt != 2 {
		t.Fatalf("second recovery = %#v, %v, %v", second, started, err)
	}
	available, err := store.FinishRecovery(grant.SlotID, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if available.State != SlotAvailable ||
		available.ResourceGeneration != 2 ||
		available.LastFailureEventID != failureID ||
		available.IncidentID != incidentID ||
		available.Project != "" ||
		available.Checkout != "" ||
		available.Run != "" ||
		available.Purpose != "" {
		t.Fatalf("recovered slot = %#v", available)
	}
	if _, err := store.Renew(grant); err == nil {
		t.Fatal("pre-rebuild capability renewed after resource generation changed")
	}
}

func TestInterruptedRecoveryReturnsToImmediateQuarantine(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	store := LeaseStore{
		Path:      filepath.Join(t.TempDir(), "leases.json"),
		SlotCount: 1,
		Now:       func() time.Time { return now },
	}
	grant, err := store.AcquireSlot(
		"client", "SHA256:key", "", "crash-retry", "slot-001",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Quarantine(grant, errors.New("fixture quarantine")); err != nil {
		t.Fatal(err)
	}
	startedSlot, started, err := store.BeginScheduledRecovery(grant.SlotID, false)
	if err != nil || !started || !dueForRecovery(startedSlot, now) {
		t.Fatalf("started recovery = %#v, %v, %v", startedSlot, started, err)
	}
	if err := store.InterruptRecovery(
		grant.SlotID,
		errors.New("previous recovery was interrupted before completion"),
	); err != nil {
		t.Fatal(err)
	}
	pool, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	slot := pool.Slots[0]
	if slot.State != SlotQuarantined ||
		!slot.NextRecoveryAt.Equal(now) ||
		!slot.RecoveryStartedAt.IsZero() ||
		slot.RecoveryAttempt != 1 ||
		!dueForRecovery(slot, now) {
		t.Fatalf("interrupted recovery = %#v", slot)
	}
}

func TestLeaseStoreRecoveryBackoffNeverBecomesTerminal(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	store := LeaseStore{
		Path:      filepath.Join(t.TempDir(), "leases.json"),
		SlotCount: 1,
		Now:       func() time.Time { return now },
	}
	grant, err := store.AcquireSlot(
		"client", "SHA256:key", "", "repeated-recovery", "slot-001",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Quarantine(grant, errors.New("fixture quarantine")); err != nil {
		t.Fatal(err)
	}
	wantDelays := []time.Duration{
		time.Minute,
		5 * time.Minute,
		15 * time.Minute,
		time.Hour,
		time.Hour,
	}
	for index, wantDelay := range wantDelays {
		started, ok, beginErr := store.BeginScheduledRecovery(grant.SlotID, false)
		if beginErr != nil || !ok || started.RecoveryAttempt != uint64(index+1) {
			t.Fatalf("attempt %d start = %#v, %v, %v", index+1, started, ok, beginErr)
		}
		failed, finishErr := store.FinishRecovery(
			grant.SlotID,
			fmt.Errorf("fixture failure %d", index+1),
			"",
			"",
		)
		if finishErr != nil {
			t.Fatal(finishErr)
		}
		if failed.State != SlotQuarantined ||
			!failed.NextRecoveryAt.Equal(now.Add(wantDelay)) {
			t.Fatalf("attempt %d schedule = %#v", index+1, failed)
		}
		now = failed.NextRecoveryAt
	}
	started, ok, err := store.BeginScheduledRecovery(grant.SlotID, false)
	if err != nil || !ok || started.RecoveryAttempt != 6 {
		t.Fatalf("unbounded retry = %#v, %v, %v", started, ok, err)
	}
	available, err := store.FinishRecovery(grant.SlotID, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if available.State != SlotAvailable || available.ResourceGeneration != 2 {
		t.Fatalf("eventual recovery = %#v", available)
	}
}

func TestLeaseStoreExpectedDrainFencesOnlyTheSnapshottedLease(t *testing.T) {
	store := LeaseStore{Path: filepath.Join(t.TempDir(), "leases.json"), SlotCount: 1}
	original, err := store.AcquireSlot(
		"original", "SHA256:key", "", "stale-drain", "slot-001",
	)
	if err != nil {
		t.Fatal(err)
	}
	expected := LeaseIdentity{
		SlotID: original.SlotID, ResourceGeneration: original.ResourceGeneration,
		LeaseEpoch: original.LeaseEpoch,
	}
	if err := store.BeginDrainSlot(original.SlotID, "fixture replacement"); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishDrain(original.SlotID, nil); err != nil {
		t.Fatal(err)
	}
	replacement, err := store.AcquireSlot(
		"replacement", "SHA256:key", "", "stale-drain", original.SlotID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if started, err := store.BeginExpectedDrain(expected, "operator revoke"); !errors.Is(err, ErrLeaseTargetStale) || started {
		t.Fatalf("stale drain = %v, %v", started, err)
	}
	pool, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	if pool.Slots[0].State != SlotProvisioning ||
		pool.Slots[0].LeaseEpoch != replacement.LeaseEpoch {
		t.Fatalf("replacement was fenced: %#v", pool.Slots[0])
	}
}

func TestLeaseStoreExpectedDrainStartsMatchingTargetAndSkipsCompletedTarget(t *testing.T) {
	store := LeaseStore{Path: filepath.Join(t.TempDir(), "leases.json"), SlotCount: 1}
	grant, err := store.AcquireSlot(
		"client", "SHA256:key", "", "exact-drain", "slot-001",
	)
	if err != nil {
		t.Fatal(err)
	}
	expected := LeaseIdentity{
		SlotID: grant.SlotID, ResourceGeneration: grant.ResourceGeneration,
		LeaseEpoch: grant.LeaseEpoch,
	}
	started, err := store.BeginExpectedDrain(expected, "operator revoke")
	if err != nil || !started {
		t.Fatalf("matching drain = %v, %v", started, err)
	}
	if err := store.FinishDrain(grant.SlotID, nil); err != nil {
		t.Fatal(err)
	}
	started, err = store.BeginExpectedDrain(expected, "operator revoke retry")
	if err != nil || started {
		t.Fatalf("completed drain retry = %v, %v", started, err)
	}
}

func TestLeaseStoreExpectedRecoveryRejectsReplacementPair(t *testing.T) {
	store := LeaseStore{Path: filepath.Join(t.TempDir(), "leases.json"), SlotCount: 1}
	grant, err := store.AcquireSlot(
		"client", "SHA256:key", "", "stale-recovery", "slot-001",
	)
	if err != nil {
		t.Fatal(err)
	}
	expected := LeaseIdentity{
		SlotID: grant.SlotID, ResourceGeneration: grant.ResourceGeneration,
		LeaseEpoch: grant.LeaseEpoch,
	}
	if err := store.Quarantine(grant, errors.New("fixture quarantine")); err != nil {
		t.Fatal(err)
	}
	if _, started, err := store.BeginScheduledRecovery(grant.SlotID, true); err != nil || !started {
		t.Fatalf("begin fixture recovery = %v, %v", started, err)
	}
	if _, err := store.FinishRecovery(grant.SlotID, nil, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, started, err := store.BeginExpectedRecovery(expected); !errors.Is(err, ErrLeaseTargetStale) || started {
		t.Fatalf("stale recovery = %v, %v", started, err)
	}
	pool, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	if pool.Slots[0].State != SlotAvailable ||
		pool.Slots[0].ResourceGeneration == expected.ResourceGeneration {
		t.Fatalf("replacement pair was mutated: %#v", pool.Slots[0])
	}
}

func TestLeaseStoreExpectedRecoveryStartsMatchingTargetAndDoesNotRestartIt(t *testing.T) {
	store := LeaseStore{Path: filepath.Join(t.TempDir(), "leases.json"), SlotCount: 1}
	grant, err := store.AcquireSlot(
		"client", "SHA256:key", "", "exact-recovery", "slot-001",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Quarantine(grant, errors.New("fixture quarantine")); err != nil {
		t.Fatal(err)
	}
	expected := LeaseIdentity{
		SlotID: grant.SlotID, ResourceGeneration: grant.ResourceGeneration,
		LeaseEpoch: grant.LeaseEpoch,
	}
	startedSlot, started, err := store.BeginExpectedRecovery(expected)
	if err != nil || !started || startedSlot.State != SlotRecovering ||
		startedSlot.RecoveryAttempt != 1 {
		t.Fatalf("matching recovery = %#v, %v, %v", startedSlot, started, err)
	}
	retrySlot, started, err := store.BeginExpectedRecovery(expected)
	if err != nil || started || retrySlot.State != SlotRecovering ||
		retrySlot.RecoveryAttempt != 1 {
		t.Fatalf("in-flight recovery retry = %#v, %v, %v", retrySlot, started, err)
	}
}

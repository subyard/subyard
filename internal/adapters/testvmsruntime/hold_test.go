package testvmsruntime

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
)

// quarantineCycle leases slot 1, fails its release and lets the reaper recover it.
func (f *brokerPoolFixture) quarantineCycle() LeaseSlot {
	f.t.Helper()
	f.failRelease(f.acquire(1, "loop"))
	brokerMust(f.t, f.rt.ReapExpired(f.ctx, f.store))
	return f.slot(1)
}

func (f *brokerPoolFixture) holdRecords() []JournalRecord {
	f.t.Helper()
	records, _ := f.journal("errors-v1 - slot-001 200")
	var held []JournalRecord
	for _, record := range records {
		if !record.HoldUntil.IsZero() || record.Kind == "slot.hold_ended" {
			held = append(held, record)
		}
	}
	return held
}

// Five counted quarantines within an hour hold the cleaned slot for 2 h,
// doubling per consecutive hold up to 24 h; the hold expires by itself or the
// operator's recovery lifts it, and 24 quiet hours reset the level.
func TestLoopProtectionHoldsARepeatedlyQuarantinedSlot(t *testing.T) {
	f := newBrokerPoolFixture(t, 1)
	for cycle := 1; cycle < holdThreshold; cycle++ {
		if slot := f.quarantineCycle(); slot.State != SlotAvailable || !slot.HoldUntil.IsZero() {
			t.Fatalf("cycle %d: %+v", cycle, slot)
		}
		f.now = f.now.Add(10 * time.Minute)
	}
	start := f.now
	held := f.quarantineCycle()
	if held.State != SlotQuarantined || held.HoldLevel != 1 || !held.HoldUntil.Equal(start.Add(2*time.Hour)) ||
		!held.NextRecoveryAt.Equal(held.HoldUntil) || len(held.RecentQuarantines) != 0 ||
		!held.HoldResetAt.Equal(held.HoldUntil.Add(24*time.Hour)) {
		t.Fatalf("fifth quarantine within an hour: %+v", held)
	}
	records := f.holdRecords()
	if kinds := journalKinds(records); !slices.Equal(kinds, []string{"slot.quarantined"}) ||
		records[0].HoldLevel != 1 || !records[0].HoldUntil.Equal(held.HoldUntil) {
		t.Fatalf("hold journal = %+v", records)
	}
	if result := f.requestAcquire(1, "during-hold", fixturePublicKey(t)); result.Status != "error" ||
		result.State != SlotQuarantined {
		t.Fatalf("acquire during hold = %#v", result)
	}
	var status facadeResponse
	f.request("status", &status)
	if got := status.Pool.Slots[0]; !got.HoldUntil.Equal(held.HoldUntil) || got.HoldLevel != 1 {
		t.Fatalf("status does not show the hold: %+v", got)
	}
	// The reaper leaves the cleaned slot alone until the hold ends.
	f.now = held.HoldUntil.Add(-time.Second)
	f.incus.calls = nil
	brokerMust(t, f.rt.ReapExpired(f.ctx, f.store))
	if slot := f.slot(1); slot.State != SlotQuarantined || slot.ResourceGeneration != held.ResourceGeneration {
		t.Fatalf("hold ended early: %+v", slot)
	}
	f.now = held.HoldUntil
	brokerMust(t, f.rt.ReapExpired(f.ctx, f.store))
	if slot := f.slot(1); slot.State != SlotAvailable || !slot.HoldUntil.IsZero() || slot.HoldLevel != 1 {
		t.Fatalf("expired hold: %+v", slot)
	}
	if records := f.holdRecords(); records[len(records)-1].Kind != "slot.hold_ended" ||
		records[len(records)-1].HoldReason != "expired" || records[len(records)-1].Outcome != "" {
		t.Fatalf("expiry journal = %+v", records)
	}

	// The next hold within 24 h doubles; the operator lifts it and keeps the level.
	for range holdThreshold {
		f.quarantineCycle()
		f.now = f.now.Add(time.Minute)
	}
	second := f.slot(1)
	if second.HoldLevel != 2 || second.HoldUntil.Sub(second.HoldStartedAt) != 4*time.Hour {
		t.Fatalf("second hold: %+v", second)
	}
	brokerMust(t, f.rt.RecoverExpectedSlot(f.ctx, f.store, LeaseIdentity{
		SlotID: second.SlotID, ResourceGeneration: second.ResourceGeneration, LeaseEpoch: second.LeaseEpoch,
	}))
	if slot := f.slot(1); slot.State != SlotAvailable || !slot.HoldUntil.IsZero() || slot.HoldLevel != 2 {
		t.Fatalf("lifted hold: %+v", slot)
	}
	if records := f.holdRecords(); records[len(records)-1].HoldReason != "manual" {
		t.Fatalf("manual lift journal = %+v", records)
	}

	// 24 quiet hours after the hold reset the level.
	f.now = second.HoldResetAt
	for range holdThreshold {
		f.quarantineCycle()
		f.now = f.now.Add(time.Minute)
	}
	if third := f.slot(1); third.HoldLevel != 1 || third.HoldUntil.Sub(third.HoldStartedAt) != 2*time.Hour {
		t.Fatalf("reset hold: %+v", third)
	}
}

func TestLoopProtectionCountingRules(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	t.Run("sliding window", func(t *testing.T) {
		var slot LeaseSlot
		for index := range holdThreshold - 1 {
			countQuarantine(&slot, now.Add(time.Duration(index)*20*time.Minute))
		}
		// The first quarantine left the window, so the fifth does not hold.
		countQuarantine(&slot, now.Add(61*time.Minute))
		if slot.HoldLevel != 0 || len(slot.RecentQuarantines) != holdThreshold-1 {
			t.Fatalf("%+v", slot)
		}
	})
	t.Run("doubling stops at the cap", func(t *testing.T) {
		var got []time.Duration
		for level := uint64(1); level <= 7; level++ {
			got = append(got, holdDuration(level))
		}
		want := []time.Duration{2 * time.Hour, 4 * time.Hour, 8 * time.Hour, 16 * time.Hour,
			24 * time.Hour, 24 * time.Hour, 24 * time.Hour}
		if !slices.Equal(got, want) {
			t.Fatalf("durations = %v", got)
		}
	})
	t.Run("clock moved back", func(t *testing.T) {
		slot := LeaseSlot{RecentQuarantines: []time.Time{now.Add(48 * time.Hour)}}
		countQuarantine(&slot, now)
		if len(slot.RecentQuarantines) != 2 || !slot.RecentQuarantines[0].Equal(now) {
			t.Fatalf("future quarantine was not clamped: %+v", slot)
		}
		slot = LeaseSlot{State: SlotQuarantined, HoldUntil: now.Add(30 * 24 * time.Hour), NextRecoveryAt: now.Add(30 * 24 * time.Hour)}
		if until := activeHoldUntil(slot, now); !until.Equal(now.Add(holdCap)) {
			t.Fatalf("hold until = %s", until)
		}
		if !dueForRecovery(slot, now) {
			t.Fatal("a far future recovery time defers recovery beyond the cap")
		}
	})
	t.Run("recovery failures are not counted", func(t *testing.T) {
		store := LeaseStore{Path: filepath.Join(testkit.TempDir(t), "leases.json"), SlotCount: 1, Now: func() time.Time { return now }}
		grant, err := store.AcquireSlot("client", "SHA256:client", "label", "purpose", "slot-001")
		brokerMust(t, err)
		brokerMust(t, store.Quarantine(grant, errors.New("fixture")))
		for range holdThreshold {
			_, started, err := store.BeginScheduledRecovery("slot-001", true)
			if err != nil || !started {
				t.Fatalf("begin recovery: %v %v", started, err)
			}
			_, err = store.FinishRecovery("slot-001", errors.New("still broken"), "", "")
			brokerMust(t, err)
		}
		slot, _ := storeSlot(store, "slot-001")
		if len(slot.RecentQuarantines) != 1 || slot.HoldLevel != 0 {
			t.Fatalf("%+v", slot)
		}
	})
}

// Hold state survives a pool file written by a broker that does not know it.
func TestLoopProtectionHoldSurvivesInTheRecoveryJournal(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	store := LeaseStore{Path: filepath.Join(testkit.TempDir(t), "leases.json"), SlotCount: 1, Now: func() time.Time { return now }}
	grant, err := store.AcquireSlot("client", "SHA256:client", "label", "purpose", "slot-001")
	brokerMust(t, err)
	brokerMust(t, store.Quarantine(grant, errors.New("fixture")))
	pool, err := store.Status()
	brokerMust(t, err)
	pool.Slots[0].HoldLevel, pool.Slots[0].HoldUntil = 3, now.Add(8*time.Hour)
	brokerMust(t, store.writePool(pool))
	pool.Slots[0].HoldLevel, pool.Slots[0].HoldUntil, pool.Slots[0].RecentQuarantines = 0, time.Time{}, nil
	brokerMust(t, writeJSONAtomic(store.Path, pool))
	slot, err := storeSlot(store, "slot-001")
	brokerMust(t, err)
	if slot.HoldLevel != 3 || !slot.HoldUntil.Equal(now.Add(8*time.Hour)) || len(slot.RecentQuarantines) != 1 {
		t.Fatalf("journaled hold lost: %+v", slot)
	}
}

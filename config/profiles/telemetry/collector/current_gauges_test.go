package collector

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestCurrentGaugesRequireLiveObjectAndSource(t *testing.T) {
	for _, test := range []struct {
		name, state                                                                                                       string
		final, stopped, offline, recovered, foreignSource, oldGeneration, missingGauge, mismatch, tiedReceipt, oldReceipt bool
		live                                                                                                              bool
	}{
		{name: "running", state: "held", live: true}, {name: "draining_expired_lease", state: "draining", live: true},
		{name: "logical_release", state: "released", live: true}, {name: "final_sample", state: "held", final: true},
		{name: "available", state: "available"}, {name: "confirmed_stop", state: "draining", stopped: true},
		{name: "ended_object", state: "ended"}, {name: "offline", state: "held", offline: true},
		{name: "recovered", state: "held", offline: true, recovered: true, live: true},
		{name: "offline_receipt_tie", state: "held", offline: true, tiedReceipt: true},
		{name: "recovered_receipt_tie", state: "held", offline: true, recovered: true, tiedReceipt: true, live: true},
		{name: "late_old_gap_receipt", state: "held", offline: true, oldReceipt: true, live: true},
		{name: "foreign_source_gap", state: "held", offline: true, foreignSource: true, live: true},
		{name: "old_generation_gap", state: "held", offline: true, oldGeneration: true, live: true},
		{name: "generation_mismatch", state: "held", mismatch: true}, {name: "missing_gauge", state: "held", missingGauge: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := fixtureStore(t)
			ctx := context.Background()
			now := time.Now().UTC()
			at := func(seconds int) string {
				return now.Add(time.Duration(seconds) * time.Second).Format(time.RFC3339Nano)
			}
			object := map[string]any{"object_id": "object", "object_kind": "vm", "state": test.state, "owner_id": "owner", "resource_generation": 2, "lease_epoch": 3, "acquired_at": at(-10), "expires_at": at(-5)}
			if test.stopped {
				object["stopped_at"] = at(-1)
			}
			point := func(cpu int) map[string]any {
				return map[string]any{"object_id": "object", "resource_generation": 2, "lease_epoch": 3, "boot_id": "boot", "counter_generation": "counter", "monotonic_ns": cpu, "cpu_ns": cpu, "memory_bytes": 100, "memory_peak_bytes": 200, "rss_bytes": 80, "pss_bytes": 70, "swap_bytes": 10}
			}
			last := point(200)
			last["final"] = test.final
			if test.mismatch {
				last["resource_generation"] = 1
			}
			if test.missingGauge {
				delete(last, "memory_bytes")
			}
			firstRecord, lastRecord := record("sample", point(100)), record("sample", last)
			firstRecord.SourceTime = at(-3)
			lastRecord.SourceTime = at(-2)
			edge := map[string]any{"object_id": "object", "agent_run_id": "run", "quality": "explicit", "start_time": at(-10)}
			if test.state == "released" {
				edge["end_time"] = at(-1)
			}
			input := []Record{record("run", map[string]any{"agent_run_id": "run", "state": "main_exited"}), record("allocation", object), record("association", edge), firstRecord, lastRecord}
			if err := s.Ingest(ctx, input); err != nil {
				t.Fatal(err)
			}
			if test.offline {
				gap := record("gap", map[string]any{"object_id": "object", "owner_id": "owner", "reason": "source_offline"})
				gap.SourceTime = "2000-01-01T00:00:00Z" // Manager and owner clocks must not be compared.
				if test.foreignSource {
					gap.SourceID = "other-observer"
				}
				if test.oldGeneration {
					gap.Body["resource_generation"] = 1
				}
				if err := s.Ingest(ctx, []Record{gap}); err != nil {
					t.Fatal(err)
				}
				if test.oldReceipt {
					if _, err := s.db.Exec("UPDATE records SET received_time=? WHERE event_id=?", at(-10), gap.EventID); err != nil {
						t.Fatal(err)
					}
				}
				if test.recovered {
					newer := record("sample", point(300))
					newer.SourceTime = at(-1)
					if err := s.Ingest(ctx, []Record{newer}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if test.tiedReceipt {
				if _, err := s.db.Exec("UPDATE records SET received_time=?", at(0)); err != nil {
					t.Fatal(err)
				}
			}
			result, err := ShowRun(ctx, s.db, "run")
			if err != nil {
				t.Fatal(err)
			}
			entry := result["selected_nonoverlapping"].([]any)[0].(map[string]any)
			if entry["latest_sample"] == nil || fmt.Sprint(entry["kernel_peak_memory_bytes"]) != "200" || fmt.Sprint(entry["sampled_peak_memory_bytes"]) != "100" {
				t.Fatal("historical measurements lost")
			}
			if test.live {
				if fmt.Sprint(entry["current_memory_bytes"]) != "100" || entry["stale"] != false {
					t.Fatal("live responsibility lost current observation")
				}
			} else if entry["current_memory_bytes"] != nil {
				t.Fatal("historical gauge reported as live")
			}
			if !test.live && !test.missingGauge {
				for _, key := range []string{"current_rss_bytes", "current_pss_bytes", "current_swap_bytes"} {
					if entry[key] != nil {
						t.Fatalf("historical %s reported as live", key)
					}
				}
			}
			if test.missingGauge && (fmt.Sprint(entry["current_rss_bytes"]) != "80" || entry["stale"] != false) {
				t.Fatal("one missing gauge invalidated other fresh observations")
			}
			gauge := result["totals"].(map[string]any)["broker_actual"].(map[string]any)["current_memory_bytes"].(map[string]any)
			if !test.live && (gauge["known_sum"] != nil || gauge["aggregation"] == "single_fresh_object") {
				t.Fatal("unavailable gauge aggregated as fresh")
			}
			if !test.mismatch && fmt.Sprint(entry["cpu_ns_delta"]) == "<nil>" {
				t.Fatal("historical counter interval lost")
			}
		})
	}
}

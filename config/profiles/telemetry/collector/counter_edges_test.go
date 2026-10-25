package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

func TestCounterDomainChangesNeverInventDeltas(t *testing.T) {
	for _, change := range []string{"counter_reset", "boot_id", "generation", "counter_generation", "cpu_ns"} {
		t.Run(change, func(t *testing.T) {
			first := map[string]any{"boot_id": "boot", "generation": "scope", "counter_generation": "counter", "monotonic_ns": json.Number("1"), "cpu_ns": json.Number("100")}
			last := map[string]any{"boot_id": "boot", "generation": "scope", "counter_generation": "counter", "monotonic_ns": json.Number("2"), "cpu_ns": json.Number("150")}
			switch change {
			case "counter_reset":
				last[change] = true
			case "cpu_ns":
				last[change] = json.Number("50")
			default:
				last[change] = "replacement"
			}
			entry := map[string]any{"object": map[string]any{"object_kind": "scope"}}
			summarizeObject(entry, []any{first, last}, []map[string]any{{"start_time": "2026-10-24T00:00:00Z"}})
			if entry["cpu_ns_delta"] != nil {
				t.Fatalf("incompatible counter fabricated delta: %+v", entry)
			}
			if change != "cpu_ns" && entry["counter_coverage"] != "unknown_reset_or_disjoint_interval" {
				t.Fatalf("counter domain change not explicit: %+v", entry)
			}
		})
	}
}

func TestLateOldGenerationDoesNotContaminateReplacement(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	start := "2026-10-24T00:00:00Z"
	records := []Record{record("run", map[string]any{"agent_run_id": "run"})}
	for _, generation := range []int{2, 1} {
		id := fmt.Sprintf("owner:slot:g%d", generation)
		state := "held"
		if generation == 1 {
			state = "available"
		}
		r := record("allocation", map[string]any{"object_id": id, "object_kind": "vm", "owner_id": "owner", "slot_id": "slot", "resource_generation": generation, "state": state})
		r.SourceTime = start
		records = append(records, r, record("association", map[string]any{"object_id": id, "agent_run_id": "run", "quality": "explicit", "start_time": start}))
		for i := 1; i <= 2; i++ {
			r := record("sample", map[string]any{"object_id": id, "boot_id": "boot", "counter_generation": id, "monotonic_ns": i, "cpu_ns": i * generation * 100})
			r.SourceTime = fmt.Sprintf("2026-10-24T00:00:0%dZ", i)
			records = append(records, r)
		}
	}
	if err := s.Ingest(ctx, records); err != nil {
		t.Fatal(err)
	}
	var exported bytes.Buffer
	if _, err := Export(ctx, s.db, &exported, 0, 100); err != nil {
		t.Fatal(err)
	}
	destination := fixtureStore(t)
	for i := 0; i < 2; i++ {
		if err := destination.Replay(ctx, bytes.NewReader(exported.Bytes())); err != nil {
			t.Fatal(err)
		}
	}
	for _, store := range []*Store{s, destination} {
		result, err := ShowRun(ctx, store.db, "run")
		if err != nil {
			t.Fatal(err)
		}
		entries := result["selected_nonoverlapping"].([]any)
		if len(entries) != 2 {
			t.Fatal("resource generations merged")
		}
		for _, value := range entries {
			entry := value.(map[string]any)
			object := entry["object"].(map[string]any)
			if object["object_id"] == "owner:slot:g2" && (object["state"] != "held" || fmt.Sprint(entry["cpu_ns_delta"]) != "200" || len(entry["samples"].([]any)) != 2) {
				t.Fatalf("late old generation contaminated replacement: %+v", entry)
			}
		}
	}
}

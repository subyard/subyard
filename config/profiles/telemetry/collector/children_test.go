package collector

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestRunIncludingChildrenSelectsObjectsOnce(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	start, end := "2026-10-24T01:00:00Z", "2026-10-24T01:00:10Z"
	records := []Record{
		record("run", map[string]any{"agent_run_id": "parent"}),
		record("run", map[string]any{"agent_run_id": "child", "parent_run_id": "parent"}),
		record("run", map[string]any{"agent_run_id": "grandchild", "parent_run_id": "child"}),
		record("run", map[string]any{"agent_run_id": "sibling", "parent_run_id": "parent"}),
		record("run", map[string]any{"agent_run_id": "unrelated"}),
		record("session", map[string]any{"agent_run_id": "child", "session_id": "native-child", "source": "native-otlp-attribute/registered-resource-attribute"}),
		record("gap", map[string]any{"agent_run_id": "grandchild", "reason": "final_tail_missing", "quality": "unknown"}),
	}
	addObject := func(kind, id, parent, accounting string, runs []string, cpu int) {
		body := map[string]any{"object_id": id, "object_kind": kind, "parent_object_id": parent, "accounting": accounting}
		recordKind := "scope"
		if kind == "vm" {
			recordKind = "allocation"
			body["acquired_at"], body["stopped_at"], body["allocated_cpu"] = start, end, 2
		}
		records = append(records, record(recordKind, body))
		for _, run := range runs {
			records = append(records, record("association", map[string]any{"association_id": id + ":" + run, "object_id": id, "agent_run_id": run, "quality": "explicit", "source": "fixture-explicit", "start_time": start, "end_time": end}))
		}
		for i, at := range []string{start, end} {
			r := record("sample", map[string]any{"object_id": id, "cpu_ns": i * cpu, "monotonic_ns": i*10000000000 + 1, "boot_id": "boot", "counter_generation": id + ":generation", "memory_bytes": 100})
			r.SourceTime = at
			records = append(records, r)
		}
	}
	addObject("scope", "parent-scope", "", "inclusive", []string{"parent"}, 100)
	addObject("scope", "child-scope", "parent-scope", "inclusive", []string{"child"}, 60)
	addObject("scope", "child-direct", "child-scope", "direct", []string{"child"}, 20)
	addObject("vm", "child-vm", "", "qemu-process-and-threads", []string{"grandchild"}, 40)
	addObject("scope", "shared-backend", "", "inclusive", []string{"child", "sibling"}, 50)
	addObject("vm", "unrelated-vm", "", "qemu-process-and-threads", []string{"unrelated"}, 999)
	if err := s.Ingest(ctx, records); err != nil {
		t.Fatal(err)
	}
	db, err := OpenReadOnly(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	direct, err := ShowRun(ctx, db, "parent")
	if err != nil {
		t.Fatal(err)
	}
	inclusive, err := ShowRunIncludingChildren(ctx, db, "parent")
	if err != nil {
		t.Fatal(err)
	}
	if direct["attribution_mode"] != "direct_run" || len(direct["included_runs"].([]string)) != 1 || len(direct["selected_nonoverlapping"].([]any)) != 1 {
		t.Fatalf("direct view changed: %+v", direct)
	}
	if inclusive["hierarchy_source"] != "run.parent_run_id" || inclusive["attribution_mode"] != "including_child_runs" || fmt.Sprint(inclusive["included_runs"]) != "[child grandchild parent sibling]" {
		t.Fatalf("explicit hierarchy evidence missing: %+v", inclusive)
	}
	selected := inclusive["selected_nonoverlapping"].([]any)
	if len(selected) != 2 || len(inclusive["excluded_overlapping_detail"].([]any)) != 2 || len(inclusive["shared"].([]any)) != 1 {
		t.Fatalf("object overlap/shared selection: %+v", inclusive)
	}
	for _, value := range selected {
		entry := value.(map[string]any)
		refs := entry["sample_refs"].([]any)
		if len(refs) != 2 || refs[0].(map[string]any)["origin_id"] != "producer:fixture" || refs[0].(map[string]any)["event_id"] == "" {
			t.Fatalf("sample identity lost: %+v", entry)
		}
		for _, edge := range entry["associations"].([]map[string]any) {
			if edge["agent_run_id"] == "" || edge["source"] != "fixture-explicit" || edge["quality"] != "explicit" {
				t.Fatalf("association provenance lost: %+v", edge)
			}
		}
	}
	shared := inclusive["shared"].([]any)[0].(map[string]any)
	if shared["object"].(map[string]any)["object_id"] != "shared-backend" || len(shared["samples"].([]any)) != 2 || len(shared["associations"].([]map[string]any)) != 2 || shared["quality"] != "shared" {
		t.Fatalf("shared backend duplicated or reattributed: %+v", shared)
	}
	metric := func(result map[string]any, category, key string) any {
		return result["totals"].(map[string]any)[category].(map[string]any)[key].(map[string]any)["known_sum"]
	}
	if fmt.Sprint(metric(inclusive, "local_inclusive", "cpu_ns_delta")) != "100" || fmt.Sprint(metric(inclusive, "broker_actual", "cpu_ns_delta")) != "40" || fmt.Sprint(metric(inclusive, "broker_actual", "allocated_vcpu_seconds")) != "20" {
		t.Fatalf("incorrect inclusive totals: %+v", inclusive["totals"])
	}
	if metric(direct, "broker_actual", "cpu_ns_delta") != nil || len(inclusive["sessions"].([]map[string]any)) != 1 || len(inclusive["gaps"].([]any)) != 1 {
		t.Fatal("direct view or descendant coverage evidence incorrect")
	}
}

func TestRunHierarchyCycleAndWindow(t *testing.T) {
	for _, cycle := range []bool{true, false} {
		t.Run(fmt.Sprintf("cycle=%t", cycle), func(t *testing.T) {
			s := fixtureStore(t)
			records := []Record{record("run", map[string]any{"agent_run_id": "root"})}
			want := "window exceeded"
			if cycle {
				records[0].Body["parent_run_id"] = "child"
				records = append(records, record("run", map[string]any{"agent_run_id": "child", "parent_run_id": "root"}))
				want = "hierarchy cycle"
			} else {
				for i := 0; i < 1000; i++ {
					records = append(records, record("run", map[string]any{"agent_run_id": fmt.Sprintf("child-%04d", i), "parent_run_id": "root"}))
				}
			}
			if err := s.Ingest(context.Background(), records); err != nil {
				t.Fatal(err)
			}
			if _, err := ShowRunIncludingChildren(context.Background(), s.db, "root"); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("expected finite %s diagnostic, got %v", want, err)
			}
		})
	}
}

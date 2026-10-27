package collector

import (
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"
)

var deltaMetrics = []string{"cpu_ns", "io_read_bytes", "io_write_bytes", "io_read_ops", "io_write_ops"}
var gaugeMetrics = map[string]string{"memory_bytes": "current_memory_bytes", "memory_peak_bytes": "kernel_peak_memory_bytes", "rss_bytes": "current_rss_bytes", "rss_peak_bytes": "kernel_peak_rss_bytes", "pss_bytes": "current_pss_bytes", "swap_bytes": "current_swap_bytes"}

func sameMeasuredGeneration(expected, observed map[string]any) bool {
	for _, key := range []string{"generation", "resource_generation", "lease_epoch", "boot_id", "counter_generation"} {
		if value := expected[key]; value != nil && fmt.Sprint(value) != "" && fmt.Sprint(value) != fmt.Sprint(observed[key]) {
			return false
		}
	}
	return true
}

func liveSample(object, sample map[string]any) bool {
	if sample["final"] == true || stringValue(object, "stopped_at") != "" || !sameMeasuredGeneration(object, sample) {
		return false
	}
	switch stringValue(object, "state") {
	case "available", "stopped", "ended", "deleted":
		return false
	}
	return true
}

func summarizeObject(entry map[string]any, samples []any, edges []map[string]any) {
	for _, key := range deltaMetrics {
		entry[key+"_delta"] = nil
	}
	for _, key := range gaugeMetrics {
		entry[key] = nil
	}
	entry["sampled_peak_memory_bytes"] = nil
	entry["counter_coverage"] = "insufficient_samples"
	if len(samples) > 0 {
		last := samples[len(samples)-1].(map[string]any)
		if !liveSample(entry["object"].(map[string]any), last) {
			entry["stale"] = true
		}
		for source, key := range gaugeMetrics {
			if entry["stale"] != true || !strings.HasPrefix(key, "current_") {
				entry[key] = last[source]
			}
		}
		var peak *big.Int
		for _, sample := range samples {
			if n, ok := integerValue(sample.(map[string]any)["memory_bytes"]); ok && (peak == nil || n.Cmp(peak) > 0) {
				peak = n
			}
		}
		if peak != nil {
			entry["sampled_peak_memory_bytes"] = json.Number(peak.String())
		}
	}
	if len(samples) > 1 {
		first, last := samples[0].(map[string]any), samples[len(samples)-1].(map[string]any)
		same := stringValue(first, "boot_id") != "" && (stringValue(first, "generation") != "" || stringValue(first, "counter_generation") != "") && len(edges) == 1
		for _, sample := range samples {
			point := sample.(map[string]any)
			for _, key := range []string{"host_id", "boot_id", "generation", "counter_generation"} {
				same = same && stringValue(first, key) == stringValue(point, key)
			}
			reset, _ := point["counter_reset"].(bool)
			same = same && !reset
		}
		start, m1 := integerValue(first["monotonic_ns"])
		end, m2 := integerValue(last["monotonic_ns"])
		same = same && m1 && m2 && end.Cmp(start) > 0
		if same {
			entry["counter_coverage"] = "observed_interval"
			entry["observed_elapsed_ns"] = json.Number(new(big.Int).Sub(end, start).String())
			for _, key := range deltaMetrics {
				a, aok := integerValue(first[key])
				b, bok := integerValue(last[key])
				if aok && bok && b.Cmp(a) >= 0 {
					entry[key+"_delta"] = json.Number(new(big.Int).Sub(b, a).String())
				}
			}
		} else {
			entry["counter_coverage"] = "unknown_reset_or_disjoint_interval"
		}
	}
	object := entry["object"].(map[string]any)
	if kind := stringValue(object, "object_kind"); kind == "vm" || kind == "android" {
		holdingSummary(entry, object, edges)
	}
}
func holdingSummary(entry, object map[string]any, edges []map[string]any) {
	for _, key := range []string{"allocated_holding_seconds", "allocated_vcpu_seconds", "reserved_vcpu_seconds", "reserved_memory_byte_seconds", "requested_vcpu_seconds"} {
		entry[key] = nil
	}
	entry["holding_clock"] = "source_utc"
	entry["holding_coverage"] = "missing_boundary"
	start, err := time.Parse(time.RFC3339Nano, stringValue(object, "acquired_at"))
	if err != nil {
		return
	}
	end := time.Now().UTC()
	coverage := "open_estimate"
	if stopped := stringValue(object, "stopped_at"); stopped != "" {
		end, err = time.Parse(time.RFC3339Nano, stopped)
		if err != nil {
			return
		}
		coverage = "reported_stop"
	}
	if end.Before(start) {
		entry["holding_coverage"] = "invalid_clock_interval"
		return
	}
	type interval struct{ a, b time.Time }
	windows := []interval{}
	for _, edge := range edges {
		a, e := time.Parse(time.RFC3339Nano, stringValue(edge, "start_time"))
		if e != nil {
			continue
		}
		b := end
		if text := stringValue(edge, "end_time"); text != "" {
			b, e = time.Parse(time.RFC3339Nano, text)
			if e != nil {
				continue
			}
		}
		if a.Before(start) {
			a = start
		}
		if b.After(end) {
			b = end
		}
		if !b.Before(a) {
			windows = append(windows, interval{a, b})
		}
	}
	if len(windows) == 0 {
		return
	}
	sort.Slice(windows, func(i, j int) bool { return windows[i].a.Before(windows[j].a) })
	merged := []interval{windows[0]}
	for _, window := range windows[1:] {
		last := &merged[len(merged)-1]
		if !window.a.After(last.b) {
			if window.b.After(last.b) {
				last.b = window.b
			}
		} else {
			merged = append(merged, window)
		}
	}
	duration := new(big.Int)
	for _, window := range merged {
		duration.Add(duration, big.NewInt(window.b.Sub(window.a).Nanoseconds()))
	}
	seconds := new(big.Rat).SetFrac(duration, big.NewInt(1000000000))
	entry["allocated_holding_seconds"] = json.Number(seconds.FloatString(9))
	entry["holding_coverage"] = coverage
	for source, target := range map[string]string{"allocated_cpu": "allocated_vcpu_seconds", "reserved_cpu": "reserved_vcpu_seconds", "reserved_memory_bytes": "reserved_memory_byte_seconds", "requested_cpu": "requested_vcpu_seconds"} {
		if value := object[source]; value != nil {
			amount, ok := new(big.Rat).SetString(fmt.Sprint(value))
			if ok && amount.Sign() >= 0 {
				entry[target] = json.Number(new(big.Rat).Mul(amount, seconds).FloatString(9))
			}
		}
	}
}
func runTotals(selected, detail, shared, unknown []any) map[string]any {
	categories := map[string][]map[string]any{"local_inclusive": {}, "local_direct": {}, "broker_actual": {}, "process_detail": {}, "shared": {}, "unknown": {}}
	classify := func(value any, allowInclusive bool) {
		entry := value.(map[string]any)
		object := entry["object"].(map[string]any)
		kind := stringValue(object, "object_kind")
		category := "unknown"
		switch {
		case kind == "vm" || kind == "android":
			category = "broker_actual"
		case kind == "process":
			category = "process_detail"
		case stringValue(object, "accounting") == "direct":
			category = "local_direct"
		case allowInclusive && stringValue(object, "accounting") == "inclusive":
			category = "local_inclusive"
		}
		categories[category] = append(categories[category], entry)
	}
	for _, entry := range selected {
		classify(entry, true)
	}
	for _, entry := range detail {
		object := entry.(map[string]any)["object"].(map[string]any)
		if stringValue(object, "accounting") == "direct" {
			classify(entry, false)
		}
	}
	for _, entry := range shared {
		categories["shared"] = append(categories["shared"], entry.(map[string]any))
	}
	for _, entry := range unknown {
		categories["unknown"] = append(categories["unknown"], entry.(map[string]any))
	}
	metrics := []string{"cpu_ns_delta", "io_read_bytes_delta", "io_write_bytes_delta", "io_read_ops_delta", "io_write_ops_delta", "current_memory_bytes", "kernel_peak_memory_bytes", "sampled_peak_memory_bytes", "current_rss_bytes", "kernel_peak_rss_bytes", "current_pss_bytes", "current_swap_bytes", "allocated_holding_seconds", "allocated_vcpu_seconds", "reserved_vcpu_seconds", "reserved_memory_byte_seconds", "requested_vcpu_seconds"}
	totals := map[string]any{"semantics": "Separate views: never add local_direct or process_detail to local_inclusive. Shared measurements are not exclusive run charges. Counter deltas cover observed intervals; reservation time is separate from actual use."}
	for category, entries := range categories {
		values := map[string]any{"objects": len(entries), "exclusive_attribution": category != "shared" && category != "unknown"}
		staleObjects := 0
		for _, entry := range entries {
			stale, _ := entry["stale"].(bool)
			if stale {
				staleObjects++
			}
		}
		values["stale_objects"] = staleObjects
		for _, key := range metrics {
			sum := new(big.Rat)
			known := 0
			for _, entry := range entries {
				if value := entry[key]; value != nil {
					n, ok := new(big.Rat).SetString(fmt.Sprint(value))
					if ok {
						sum.Add(sum, n)
						known++
					}
				}
			}
			var value any
			if known > 0 {
				if sum.IsInt() {
					value = json.Number(sum.Num().String())
				} else {
					value = json.Number(sum.FloatString(9))
				}
			}
			aggregation := "observed_interval_sum"
			if strings.Contains(key, "peak_") {
				value = nil
				aggregation = "per_object_only"
			} else if strings.HasPrefix(key, "current_") {
				if len(entries) != 1 || staleObjects > 0 || known == 0 {
					value = nil
					aggregation = "no_common_fresh_snapshot"
				} else {
					aggregation = "single_fresh_object"
				}
			}
			values[key] = map[string]any{"known_sum": value, "observed_objects": known, "missing_objects": len(entries) - known, "aggregation": aggregation}
		}
		totals[category] = values
	}
	return totals
}

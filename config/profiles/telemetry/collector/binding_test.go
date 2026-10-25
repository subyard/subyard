package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestVerifiedResourceContextsAcrossSignals(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	if err := s.Ingest(ctx, []Record{record("run", map[string]any{"agent_run_id": "run-a"}), record("run", map[string]any{"agent_run_id": "run-b"})}); err != nil {
		t.Fatal(err)
	}
	consumers, err := makeConsumers(s, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	logs := plogotlp.NewExportRequest()
	if err = logs.UnmarshalJSON([]byte(`{"resourceLogs":[{"resource":{"attributes":[{"key":"subyard.agent_run.id","value":{"stringValue":"run-a"}}]},"scopeLogs":[{"logRecords":[{"attributes":[{"key":"session.id","value":{"stringValue":"session-a-logs"}}]}]}]},{"resource":{"attributes":[{"key":"subyard.agent_run.id","value":{"stringValue":"run-b"}}]},"scopeLogs":[{"logRecords":[{"attributes":[{"key":"session.id","value":{"stringValue":"session-b-logs"}}]}]}]}]}`)); err != nil {
		t.Fatal(err)
	}
	if err = consumers.logs.ConsumeLogs(ctx, logs.Logs()); err != nil {
		t.Fatal(err)
	}
	metrics := pmetricotlp.NewExportRequest()
	if err = metrics.UnmarshalJSON([]byte(`{"resourceMetrics":[{"resource":{"attributes":[{"key":"subyard.agent_run.id","value":{"stringValue":"run-a"}}]},"scopeMetrics":[{"metrics":[{"name":"metric","gauge":{"dataPoints":[{"asInt":"1","attributes":[{"key":"session.id","value":{"stringValue":"session-a-metrics"}}]}]}}]}]},{"resource":{"attributes":[{"key":"subyard.agent_run.id","value":{"stringValue":"run-b"}}]},"scopeMetrics":[{"metrics":[{"name":"metric","gauge":{"dataPoints":[{"asInt":"1","attributes":[{"key":"session.id","value":{"stringValue":"session-b-metrics"}}]}]}}]}]}]}`)); err != nil {
		t.Fatal(err)
	}
	if err = consumers.metrics.ConsumeMetrics(ctx, metrics.Metrics()); err != nil {
		t.Fatal(err)
	}
	traces := ptraceotlp.NewExportRequest()
	if err = traces.UnmarshalJSON([]byte(`{"resourceSpans":[{"resource":{"attributes":[{"key":"subyard.agent_run.id","value":{"stringValue":"run-a"}}]},"scopeSpans":[{"spans":[{"traceId":"01000000000000000000000000000000","spanId":"0100000000000000","attributes":[{"key":"session.id","value":{"stringValue":"session-a-traces"}}]}]}]},{"resource":{"attributes":[{"key":"subyard.agent_run.id","value":{"stringValue":"run-b"}}]},"scopeSpans":[{"spans":[{"traceId":"02000000000000000000000000000000","spanId":"0200000000000000","attributes":[{"key":"session.id","value":{"stringValue":"session-b-traces"}}]}]}]}]}`)); err != nil {
		t.Fatal(err)
	}
	if err = consumers.traces.ConsumeTraces(ctx, traces.Traces()); err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.Query("SELECT agent_run_id,session_id,body FROM relations ORDER BY session_id")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for rows.Next() {
		var run, session, body string
		if err = rows.Scan(&run, &session, &body); err != nil {
			t.Fatal(err)
		}
		wanted := "run-a"
		if session[8] == 'b' {
			wanted = "run-b"
		}
		if run != wanted {
			t.Fatalf("cross-resource session binding: %s -> %s", run, session)
		}
		parsed, err := decodeBody([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if parsed["source"] != "native-otlp-attribute/registered-resource-attribute" {
			t.Fatalf("binding provenance missing: %s", body)
		}
		count++
	}
	rows.Close()
	if count != 6 {
		t.Fatalf("missing session edges: %d", count)
	}
	var before, after int
	s.db.QueryRow("SELECT count(*) FROM records").Scan(&before)
	unknown := plogotlp.NewExportRequest()
	raw, _ := logs.MarshalProto()
	unknown.UnmarshalProto(raw)
	unknown.Logs().ResourceLogs().At(1).Resource().Attributes().PutStr("subyard.agent_run.id", "not-registered")
	if err = consumers.logs.ConsumeLogs(ctx, unknown.Logs()); status.Code(err) != codes.Unavailable {
		t.Fatalf("pending resource context must retry: %v", err)
	}
	s.db.QueryRow("SELECT count(*) FROM records").Scan(&after)
	if before != after {
		t.Fatal("mixed valid/pending request partially committed")
	}
	conflict := plogotlp.NewExportRequest()
	conflict.UnmarshalProto(raw)
	headerContext := context.WithValue(ctx, runContextKey{}, "run-a")
	if err = consumers.logs.ConsumeLogs(headerContext, conflict.Logs()); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("conflicting header/resource accepted: %v", err)
	}
}

func TestAggregateNeverSumsIndependentPeaksOrStaleGauges(t *testing.T) {
	object := func(id string, peak, current, cpu int) any {
		return map[string]any{"object": map[string]any{"object_id": id, "object_kind": "vm"}, "kernel_peak_memory_bytes": json.Number(fmt.Sprint(peak)), "sampled_peak_memory_bytes": json.Number(fmt.Sprint(peak)), "kernel_peak_rss_bytes": json.Number(fmt.Sprint(peak)), "current_memory_bytes": json.Number(fmt.Sprint(current)), "cpu_ns_delta": json.Number(fmt.Sprint(cpu)), "last_seen": "different-source-times", "stale": false}
	}
	result := runTotals([]any{object("a", 100, 10, 5), object("b", 200, 20, 7)}, nil, nil, nil)["broker_actual"].(map[string]any)
	for _, key := range []string{"kernel_peak_memory_bytes", "sampled_peak_memory_bytes", "kernel_peak_rss_bytes", "current_memory_bytes"} {
		if result[key].(map[string]any)["known_sum"] != nil {
			t.Fatalf("fabricated simultaneous %s: %+v", key, result[key])
		}
	}
	if fmt.Sprint(result["cpu_ns_delta"].(map[string]any)["known_sum"]) != "12" {
		t.Fatal("independent counter deltas lost")
	}
	single := object("a", 100, 10, 5).(map[string]any)
	single["stale"] = true
	result = runTotals([]any{single}, nil, nil, nil)["broker_actual"].(map[string]any)
	if result["current_memory_bytes"].(map[string]any)["known_sum"] != nil {
		t.Fatal("stale gauge represented as current")
	}
}

func TestRetentionKeepsHeldOfflineResponsibility(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	records := []Record{record("run", map[string]any{"agent_run_id": "old-run", "state": "main_exited"}), record("allocation", map[string]any{"object_id": "held-vm", "object_kind": "vm", "state": "draining", "acquired_at": "2020-01-01T00:00:00Z", "expires_at": "2020-01-01T00:00:01Z", "reserved_memory_bytes": json.Number("1024")}), record("association", map[string]any{"object_id": "held-vm", "agent_run_id": "old-run", "quality": "explicit", "start_time": "2020-01-01T00:00:00Z"}), record("gap", map[string]any{"object_id": "held-vm", "agent_run_id": "old-run", "reason": "source_offline", "quality": "unknown"}), record("sample", map[string]any{"object_id": "held-vm", "cpu_ns": 1, "memory_bytes": 2})}
	for i := range records {
		records[i].SourceTime = "2020-01-01T00:00:02Z"
	}
	if err := s.Ingest(ctx, records); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE records SET received_time='2020-01-01T00:00:03.000000000Z'"); err != nil {
		t.Fatal(err)
	}
	if err := s.prune(); err != nil {
		t.Fatal(err)
	}
	var samples int
	s.db.QueryRow("SELECT count(*) FROM samples").Scan(&samples)
	if samples != 0 {
		t.Fatal("old high-volume samples not pruned")
	}
	result, err := ShowRun(ctx, s.db, "old-run")
	if err != nil {
		t.Fatal(err)
	}
	if len(result["selected_nonoverlapping"].([]any)) != 1 || len(result["gaps"].([]any)) != 1 {
		t.Fatalf("held/offline responsibility disappeared: %+v", result)
	}
	object := result["selected_nonoverlapping"].([]any)[0].(map[string]any)
	if object["latest_sample"] != nil || object["stale"] != true || object["cpu_ns_delta"] != nil {
		t.Fatal("retained responsibility fabricated current counters")
	}
}

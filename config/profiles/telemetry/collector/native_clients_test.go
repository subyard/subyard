package collector

import (
	"bytes"
	"context"
	"maps"
	"os"
	"slices"
	"testing"

	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
)

// The fixtures follow the emitters of the installed clients: Claude Code
// 2.1.327 (rl() and aje() in the bundle) and Codex 0.170.0
// (codex-rs/otel log_event! call sites). Every value that must be dropped
// contains "canary"; every retained value is free of it.
func TestNativeClientFieldsSurviveSanitizer(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	consumers, err := makeConsumers(s, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"claude-code-2.1.327", "codex-0.170.0"} {
		logs := plogotlp.NewExportRequest()
		if err = logs.UnmarshalJSON(readFixture(t, name+"-logs.json")); err != nil {
			t.Fatal(err)
		}
		if err = consumers.logs.ConsumeLogs(ctx, logs.Logs()); err != nil {
			t.Fatal(err)
		}
		metrics := pmetricotlp.NewExportRequest()
		if err = metrics.UnmarshalJSON(readFixture(t, name+"-metrics.json")); err != nil {
			t.Fatal(err)
		}
		if err = consumers.metrics.ConsumeMetrics(ctx, metrics.Metrics()); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.db.Query("SELECT encoding,coalesce(body,''),coalesce(protobuf,'') FROM records")
	if err != nil {
		t.Fatal(err)
	}
	events := map[string]map[string]any{}
	var points []map[string]any
	var services []string
	for rows.Next() {
		var encoding string
		var body, protobuf []byte
		if err = rows.Scan(&encoding, &body, &protobuf); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(body, []byte("canary")) || bytes.Contains(protobuf, []byte("canary")) {
			t.Fatalf("private client value survived in %s record", encoding)
		}
		switch encoding {
		case "otlp/logs":
			r := plogotlp.NewExportRequest()
			if err = r.UnmarshalProto(protobuf); err != nil {
				t.Fatal(err)
			}
			for _, rl := range r.Logs().ResourceLogs().All() {
				service, _ := rl.Resource().Attributes().Get("service.name")
				services = append(services, service.Str())
				for _, sl := range rl.ScopeLogs().All() {
					for _, record := range sl.LogRecords().All() {
						attrs := record.Attributes().AsRaw()
						name, _ := attrs["event.name"].(string)
						if name == "" {
							t.Fatalf("event.name dropped: %v", attrs)
						}
						if name == "tool_result" && attrs["tool_name"] == "Read" {
							name = "tool_result/failed"
						}
						events[name] = attrs
					}
				}
			}
		case "otlp/metrics":
			r := pmetricotlp.NewExportRequest()
			if err = r.UnmarshalProto(protobuf); err != nil {
				t.Fatal(err)
			}
			for _, rm := range r.Metrics().ResourceMetrics().All() {
				for _, sm := range rm.ScopeMetrics().All() {
					for _, metric := range sm.Metrics().All() {
						for _, point := range metric.Sum().DataPoints().All() {
							attrs := point.Attributes().AsRaw()
							attrs["metric"], attrs["unit"] = metric.Name(), metric.Unit()
							points = append(points, attrs)
						}
					}
				}
			}
		}
	}
	rows.Close()
	want := map[string]map[string]any{
		"user_prompt": {"session.id": "0b9f7d4e-1111-4222-8333-944455556666", "app.version": "2.1.327", "prompt_length": int64(24)},
		"api_request": {"model": "claude-opus-5-5", "input_tokens": int64(1200), "output_tokens": int64(345),
			"cache_read_tokens": int64(800), "cache_creation_tokens": int64(64), "cache_creation_5m_tokens": int64(64),
			"cache_creation_1h_tokens": int64(0), "cost_usd": 0.0421, "cost_usd_micros": int64(42100),
			"duration_ms": float64(2300), "ttft_ms": float64(410)},
		"tool_decision":      {"tool_name": "Bash"},
		"tool_result":        {"tool_name": "Bash", "success": true, "duration_ms": float64(12)},
		"tool_result/failed": {"tool_name": "Read", "success": false, "duration_ms": float64(3)},
		"codex.conversation_starts": {"conversation.id": "019a0000-1111-7222-8333-944455556666", "app.version": "0.170.0",
			"model": "gpt-5.5", "slug": "gpt-5.5"},
		"codex.user_prompt": {"prompt_length": int64(24)},
		"codex.api_request": {"duration_ms": float64(812), "model": "gpt-5.5"},
		"codex.sse_event": {"input_token_count": int64(1500), "output_token_count": int64(220), "cached_token_count": int64(1024),
			"cache_write_token_count": int64(0), "reasoning_token_count": int64(64), "tool_token_count": int64(1720), "ttft_ms": float64(390)},
		"codex.tool_decision": {"tool_name": "exec_command", "tool_namespace": "functions"},
		"codex.tool_result":   {"tool_name": "exec_command", "tool_namespace": "functions", "success": true, "duration_ms": float64(12)},
	}
	for name, fields := range want {
		got, ok := events[name]
		if !ok {
			t.Errorf("event %s dropped", name)
			continue
		}
		for field, value := range fields {
			if got[field] != value {
				t.Errorf("%s %s = %#v, want %#v", name, field, got[field], value)
			}
		}
	}
	if len(events) != len(want) {
		t.Errorf("unexpected events: %v", slices.Sorted(maps.Keys(events)))
	}
	for _, expected := range []map[string]any{
		{"metric": "claude_code.token.usage", "unit": "tokens", "type": "input", "model": "claude-opus-5-5", "session.id": "0b9f7d4e-1111-4222-8333-944455556666"},
		{"metric": "claude_code.cost.usage", "unit": "USD", "model": "claude-opus-5-5"},
		{"metric": "codex.turn.token_usage", "unit": "tokens", "token_type": "output", "model": "gpt-5.5"},
		{"metric": "codex.tool.call", "success": true},
	} {
		if !slices.ContainsFunc(points, func(point map[string]any) bool {
			for field, value := range expected {
				if point[field] != value {
					return false
				}
			}
			return true
		}) {
			t.Errorf("metric point lost fields: %v in %v", expected, points)
		}
	}
	// Codex app-server reports its client name as service.name.
	slices.Sort(services)
	if !slices.Equal(slices.Compact(services), []string{"claude-code", "subyard-fixture"}) {
		t.Errorf("client service names lost: %v", services)
	}
	if len(points) != 8 {
		t.Errorf("metric points dropped: %d", len(points))
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A sensitive value found in one record must not remove the same value from
// a sibling record, while resource values still apply to every record below.
func TestSensitiveValuesAreScopedToTheirRecord(t *testing.T) {
	r := plogotlp.NewExportRequest()
	if err := r.UnmarshalJSON([]byte(`{"resourceLogs":[{"resource":{"attributes":[{"key":"user.email","value":{"stringValue":"resource-private"}},{"key":"service.name","value":{"stringValue":"client@example.com"}}]},"scopeLogs":[{"logRecords":[
		{"attributes":[{"key":"event.name","value":{"stringValue":"api_request"}},{"key":"model","value":{"stringValue":"claude-opus-5-5"}},{"key":"tool_name","value":{"stringValue":"resource-private"}}]},
		{"attributes":[{"key":"event.name","value":{"stringValue":"codex.tool_result"}},{"key":"output","value":{"stringValue":"claude-opus-5-5"}},{"key":"model","value":{"stringValue":"claude-opus-5-5"}}]}]}]}]}`)); err != nil {
		t.Fatal(err)
	}
	raw, _ := r.MarshalProto()
	e := Envelope{Encoding: "otlp/logs", Protobuf: raw}
	if _, err := sanitizeOTLP(&e, DefaultPolicy(), valueFilter{}); err != nil {
		t.Fatal(err)
	}
	clean := plogotlp.NewExportRequest()
	if err := clean.UnmarshalProto(e.Protobuf); err != nil {
		t.Fatal(err)
	}
	if clean.Logs().ResourceLogs().At(0).Resource().Attributes().Len() != 0 {
		t.Fatal("unsafe client service name survived the value check")
	}
	records := clean.Logs().ResourceLogs().At(0).ScopeLogs().At(0).LogRecords()
	first, second := records.At(0).Attributes().AsRaw(), records.At(1).Attributes().AsRaw()
	if first["model"] != "claude-opus-5-5" || first["tool_name"] != nil {
		t.Fatalf("record scope or resource scope wrong: %v", first)
	}
	if second["model"] != nil || second["output"] != nil {
		t.Fatalf("value from the record's own sensitive field survived: %v", second)
	}
}

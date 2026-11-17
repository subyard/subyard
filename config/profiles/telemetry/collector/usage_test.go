package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

func TestModelUsageAcrossManagedRunsExportAndRetention(t *testing.T) {
	ctx := context.Background()
	dir := testkit.TempDir(t)
	legacy, err := Open(dir, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.db.Exec(`DROP VIEW model_usage; DROP VIEW model_tool_results;
		CREATE VIEW model_tool_results AS SELECT json_extract(body,'$.request_id') AS request_id
		FROM records WHERE kind='tool_result';`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Ingest(ctx, []Record{
		record("run", map[string]any{"agent_run_id": "claude", "task_id": "task"}),
		record("run", map[string]any{"agent_run_id": "codex-start", "task_id": "task", "parent_run_id": "claude"}),
		record("run", map[string]any{"agent_run_id": "codex-resume", "task_id": "task", "parent_run_id": "claude"}),
		record("gap", map[string]any{"reason": "historical"}),
	}); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var historical int
	if err := s.db.QueryRow("SELECT count(*) FROM records WHERE kind='gap' AND json_extract(body,'$.reason')='historical'").Scan(&historical); err != nil || historical != 1 {
		t.Fatalf("historical record count: %d, %v", historical, err)
	}
	logs := plog.NewLogs()
	add := func(run string, stamp uint64, attributes map[string]any) {
		resource := logs.ResourceLogs().AppendEmpty()
		if run != "" {
			resource.Resource().Attributes().PutStr("subyard.agent_run.id", run)
		}
		event := resource.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
		if attributes["event.name"] == "codex.sse_event" {
			event.SetObservedTimestamp(pcommon.Timestamp(stamp))
		} else {
			event.SetTimestamp(pcommon.Timestamp(stamp))
		}
		event.Body().SetStr("fixture-private-output")
		if err := event.Attributes().FromRaw(attributes); err != nil {
			t.Fatal(err)
		}
	}
	const validTimestamp = "2026-10-01T02:03:04.123+02:00"
	add("claude", 100, map[string]any{"event.name": "api_request", "request_id": "req-main", "session.id": "claude-thread",
		"event.timestamp": validTimestamp, "prompt.id": "prompt-1", "model": "claude-fixture", "input_tokens": 10, "cache_read_tokens": 4, "cache_creation_tokens": 0, "output_tokens": 6})
	add("claude", 110, map[string]any{"event.name": "api_request", "request_id": "req-worker", "session.id": "claude-thread",
		"prompt.id": "prompt-1", "agent_id": "worker", "parent_agent_id": "parent-worker", "input_tokens": 5})
	add("claude", 120, map[string]any{"event.name": "tool_result", "request_id": "req-main", "tool_use_id": "tool-1",
		"tool_result_size_bytes": 64000, "tool_result": "fixture-private-output"})
	for i, run := range []string{"codex-start", "codex-resume", ""} {
		add(run, uint64(200+i), map[string]any{"event.name": "codex.sse_event", "event.kind": "response.completed",
			"conversation.id": "codex-thread", "turn.id": "turn-1", "input_token_count": "20", "cached_token_count": 12,
			"output_token_count": "3", "reasoning_token_count": 2, "tool_token_count": "23",
			"cache_write_token_count": "fixture-private-counter", "event.timestamp": "fixture-private-timestamp"})
	}
	consumers, err := makeConsumers(s, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := consumers.logs.ConsumeLogs(ctx, logs); err != nil {
			t.Fatal(err)
		}
	}
	var rawExport bytes.Buffer
	if _, err := Export(ctx, s.db, &rawExport, 0, 1000); err != nil {
		t.Fatal(err)
	}
	sawCounter, sawTimestamp := false, false
	for line := range bytes.SplitSeq(bytes.TrimSpace(rawExport.Bytes()), []byte("\n")) {
		var entry Envelope
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Kind != "otlp/logs" {
			continue
		}
		if bytes.Contains(entry.Protobuf, []byte("fixture-private-counter")) {
			t.Fatal("nonnumeric token counter survived in exported OTLP")
		}
		request := plogotlp.NewExportRequest()
		if err := request.UnmarshalProto(entry.Protobuf); err != nil {
			t.Fatal(err)
		}
		for _, resource := range request.Logs().ResourceLogs().All() {
			for _, scope := range resource.ScopeLogs().All() {
				for _, event := range scope.LogRecords().All() {
					if stamp, ok := event.Attributes().Get("event.timestamp"); ok {
						if stamp.Str() != validTimestamp {
							t.Fatal("invalid event.timestamp survived in exported OTLP")
						}
						sawTimestamp = true
					}
					value, ok := event.Attributes().Get("input_token_count")
					sawCounter = sawCounter || (ok && value.Str() == "20")
				}
			}
		}
	}
	if !sawCounter {
		t.Fatal("numeric string token counter missing from exported OTLP")
	}
	if !sawTimestamp {
		t.Fatal("valid RFC3339 timestamp missing from exported OTLP")
	}
	trace := ptrace.NewTraces()
	spanEvent := trace.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty().Events().AppendEmpty()
	if err := spanEvent.Attributes().FromRaw(map[string]any{"event.name": "codex.sse_event", "event.kind": "response.completed", "input_token_count": "20"}); err != nil {
		t.Fatal(err)
	}
	if err := consumers.traces.ConsumeTraces(ctx, trace); err != nil {
		t.Fatal(err)
	}
	for _, trigger := range []string{"poll_unchanged", "worker_result"} {
		if err := s.Ingest(ctx, []Record{record("model_usage", map[string]any{
			"client": "claude", "request_identity": "provider", "request_id": "req-" + trigger, "agent_run_id": "claude",
			"trigger": trigger, "tool_output_bytes": 0, "input_tokens": 2, "transcript": "fixture-private-output",
		})}); err != nil {
			t.Fatal(err)
		}
	}
	// A second observation of the same provider request can supply a missing field.
	if err := s.Ingest(ctx, []Record{
		record("model_usage", map[string]any{
			"client": "claude", "request_identity": "provider", "request_id": "req-main", "trigger": "new_prompt", "input_tokens": 10,
		}),
		record("tool_result", map[string]any{
			"client": "claude", "request_identity": "native_log", "request_id": "req-main", "tool_use_id": "tool-1", "tool_result_bytes": 42,
		}),
	}); err != nil {
		t.Fatal(err)
	}

	check := func(store *Store) {
		t.Helper()
		db, err := OpenReadOnly(store.dir)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var nativeSize int
		if err := db.QueryRow("SELECT tool_result_bytes FROM model_tool_results WHERE request_identity='native_log' AND request_id='req-main' AND tool_use_id='tool-1'").Scan(&nativeSize); err != nil || nativeSize != 42 {
			t.Fatalf("native-log tool size: %d, %v", nativeSize, err)
		}
		rows, err := Query(ctx, db, `SELECT u.agent_run_id, u.session_id, u.turn_id, u.native_agent_id, u.parent_native_agent_id,
			json_extract(r.body,'$.task_id') task_id,json_extract(r.body,'$.parent_run_id') parent_run_id,
			u.input_tokens,u.cache_read_tokens,u.cache_write_tokens,u.reasoning_tokens,u.tool_output_bytes,u.trigger,t.tool_result_bytes
			FROM model_usage u LEFT JOIN runs r USING(agent_run_id)
			LEFT JOIN model_tool_results t USING(client,request_identity,request_id)
			ORDER BY u.source_time`)
		if err != nil || len(rows) != 7 {
			t.Fatalf("request rows: %v, %v", rows, err)
		}
		if rows[0]["cache_write_tokens"] != int64(0) || rows[0]["reasoning_tokens"] != nil || rows[0]["tool_result_bytes"] != int64(64000) ||
			rows[0]["tool_output_bytes"] != nil || rows[0]["trigger"] != "new_prompt" ||
			rows[1]["native_agent_id"] != "worker" || rows[1]["parent_native_agent_id"] != "parent-worker" || rows[1]["turn_id"] != "prompt-1" ||
			rows[2]["agent_run_id"] != "codex-start" || rows[3]["agent_run_id"] != "codex-resume" ||
			rows[2]["session_id"] != rows[3]["session_id"] || rows[3]["parent_run_id"] != "claude" || rows[3]["task_id"] != "task" ||
			rows[3]["reasoning_tokens"] != int64(2) || rows[4]["task_id"] != nil || rows[4]["agent_run_id"] != nil ||
			rows[5]["trigger"] != "poll_unchanged" || rows[5]["tool_output_bytes"] != int64(0) || rows[6]["trigger"] != "worker_result" {
			t.Fatalf("unexpected request attribution: %+v", rows)
		}
		var count, total int
		if err := db.QueryRow("SELECT count(*),sum(input_tokens) FROM model_usage").Scan(&count, &total); err != nil || count != 7 || total != 79 {
			t.Fatalf("request totals: %d, %d, %v", count, total, err)
		}
	}
	check(s)
	if _, err := s.db.Exec("UPDATE records SET received_time='2000-01-01T00:00:00.000000000Z'"); err != nil {
		t.Fatal(err)
	}
	if err := s.prune(); err != nil {
		t.Fatal(err)
	}
	var exported bytes.Buffer
	if _, err := Export(ctx, s.db, &exported, 0, 1000); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(exported.String(), "fixture-private-output") || strings.Contains(exported.String(), "otlp/") {
		t.Fatal("old OTLP or private content survived")
	}
	replayed := fixtureStore(t)
	for range 2 {
		if err := replayed.Replay(ctx, bytes.NewReader(exported.Bytes())); err != nil {
			t.Fatal(err)
		}
	}
	check(replayed)
}

func TestUsageProducerAndOutboxRejectInvalidRecords(t *testing.T) {
	s := fixtureStore(t)
	root := testkit.TempDir(t)
	cmd := exec.Command("python3", "-B", "-c", `
from pathlib import Path
import sys
from records import emit

root = Path(sys.argv[1])
body = dict(client='codex', request_identity='provider', request_id='request', input_tokens=7)
for invalid in (dict(input_tokens='private message here'), dict(trigger='private conversation')):
    assert not emit(root, 'model_usage', dict(body, **invalid), event_id='rejected')
assert not (root/'outbox/rejected.json').exists()
assert emit(root, 'model_usage', body, event_id='valid')
`, root)
	cmd.Dir = ".."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("emit fixture: %v\n%s", err, out)
	}
	outbox := filepath.Join(root, "outbox")
	// Older producers could leave malformed JSON or invalid usage bodies behind.
	testkit.WriteFile(t, filepath.Join(outbox, "bad-json.json"), []byte("{"), 0600)
	r := record("model_usage", map[string]any{
		"client": "codex", "request_identity": "provider", "request_id": "rejected", "input_tokens": "private message here",
	})
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(outbox, "bad-usage.json"), data, 0600)
	if err := s.IngestOutbox(context.Background(), outbox); err != nil {
		t.Fatal(err)
	}
	if discarded := s.outboxDiscarded.Load(); discarded != 2 {
		t.Fatalf("discarded %d records, want 2", discarded)
	}
	var count, tokens int
	if err := s.db.QueryRow("SELECT count(*), coalesce(sum(input_tokens), 0) FROM model_usage").Scan(&count, &tokens); err != nil || count != 1 || tokens != 7 {
		t.Fatalf("outbox delivery: %d rows, %d tokens, error %v", count, tokens, err)
	}
	if entries, err := os.ReadDir(outbox); err != nil || len(entries) != 1 || entries[0].Name() != ".lock" {
		t.Fatalf("outbox retained processed records: %v, %v", entries, err)
	}
}

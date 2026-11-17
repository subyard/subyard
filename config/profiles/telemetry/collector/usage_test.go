package collector

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

func TestModelUsageAcrossManagedRunsExportAndRetention(t *testing.T) {
	ctx := context.Background()
	dir := testkit.TempDir(t)
	legacy, err := Open(dir, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.db.Exec("DROP VIEW model_usage; DROP VIEW model_tool_results"); err != nil {
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
	if err := s.Ingest(ctx, []Record{
		record("run", map[string]any{"agent_run_id": "claude", "task_id": "task"}),
		record("run", map[string]any{"agent_run_id": "codex-start", "task_id": "task", "parent_run_id": "claude"}),
		record("run", map[string]any{"agent_run_id": "codex-resume", "task_id": "task", "parent_run_id": "claude"}),
	}); err != nil {
		t.Fatal(err)
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
	add("claude", 100, map[string]any{"event.name": "api_request", "request_id": "req-main", "session.id": "claude-thread",
		"prompt.id": "prompt-1", "model": "claude-fixture", "input_tokens": 10, "cache_read_tokens": 4, "cache_creation_tokens": 0, "output_tokens": 6})
	add("claude", 110, map[string]any{"event.name": "api_request", "request_id": "req-worker", "session.id": "claude-thread",
		"prompt.id": "prompt-1", "agent_id": "worker", "parent_agent_id": "parent-worker", "input_tokens": 5})
	add("claude", 120, map[string]any{"event.name": "tool_result", "request_id": "req-main", "tool_use_id": "tool-1",
		"tool_result_size_bytes": 64000, "tool_result": "fixture-private-output"})
	for i, run := range []string{"codex-start", "codex-resume", ""} {
		add(run, uint64(200+i), map[string]any{"event.name": "codex.sse_event", "event.kind": "response.completed",
			"conversation.id": "codex-thread", "turn.id": "turn-1", "input_token_count": "20", "cached_token_count": 12,
			"output_token_count": "3", "reasoning_token_count": 2, "tool_token_count": "23"})
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
	if err := s.Ingest(ctx, []Record{record("model_usage", map[string]any{
		"client": "claude", "request_identity": "provider", "request_id": "req-main", "trigger": "new_prompt", "input_tokens": 10,
	})}); err != nil {
		t.Fatal(err)
	}

	check := func(store *Store) {
		t.Helper()
		db, err := OpenReadOnly(store.dir)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		rows, err := Query(ctx, db, `SELECT u.agent_run_id, u.session_id, u.turn_id, u.native_agent_id, u.parent_native_agent_id,
			json_extract(r.body,'$.task_id') task_id,json_extract(r.body,'$.parent_run_id') parent_run_id,
			u.input_tokens,u.cache_read_tokens,u.cache_write_tokens,u.reasoning_tokens,u.tool_output_bytes,u.trigger,t.tool_result_bytes
			FROM model_usage u LEFT JOIN runs r USING(agent_run_id)
			LEFT JOIN model_tool_results t ON t.client=u.client AND t.request_id=u.request_id
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

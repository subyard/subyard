package collector

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
)

// usageTotals is the task report shape: coordinator by session, native
// subagents by agent ID, workers through their parent-session binding.
const usageTotals = `SELECT client||'/'||coalesce(native_agent_id,'main') AS who,count(*) AS requests,
	coalesce(sum(input_tokens),0)+coalesce(sum(input_token_count),0) AS input,
	coalesce(sum(cache_read_tokens),0)+coalesce(sum(cached_token_count),0) AS cached
	FROM model_requests GROUP BY 1 ORDER BY 1`

func usageRows(t *testing.T, s *Store, sql string) []map[string]any {
	t.Helper()
	rows, err := Query(context.Background(), s.db, sql)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func usageSummary(t *testing.T, s *Store) string {
	t.Helper()
	var out []string
	for _, row := range usageRows(t, s, usageTotals) {
		out = append(out, stringValue(row, "who")+":"+numberText(row["requests"])+":"+numberText(row["input"])+":"+numberText(row["cached"]))
	}
	return strings.Join(out, " ")
}

func TestUsageFromNativeLogs(t *testing.T) {
	s := fixtureStore(t)
	captureNativeFixtures(t, s)
	const want = "claude/a1b2c3d4fixture:1:90:0 claude/main:1:1200:800 codex/main:1:1500:1024"
	if got := usageSummary(t, s); got != want {
		t.Fatalf("model request totals = %q, want %q", got, want)
	}
	// Codex counts cached input inside input; tool_token_count is the total.
	codex := usageRows(t, s, "SELECT * FROM model_requests WHERE client='codex'")[0]
	if codex["request_identity"] != "native_log" || codex["tool_token_count"] != int64(1720) || codex["cache_write_token_count"] != int64(0) {
		t.Fatalf("codex request: %v", codex)
	}
	// A reported zero stays zero; an absent counter stays unknown.
	sub := usageRows(t, s, "SELECT * FROM model_requests WHERE native_agent_id IS NOT NULL")[0]
	if sub["cache_read_tokens"] != int64(0) || sub["cache_creation_5m_tokens"] != nil || sub["request_id"] != "req_011CVfixture0002" || sub["request_identity"] != "provider" {
		t.Fatalf("subagent request: %v", sub)
	}
	tools := usageRows(t, s, "SELECT client,tool_use_id,tool_result_size_bytes,output_length FROM tool_outputs ORDER BY client,tool_use_id")
	if len(tools) != 3 || tools[1]["tool_result_size_bytes"] != int64(512) || tools[2]["output_length"] != int64(64) {
		t.Fatalf("tool outputs: %v", tools)
	}
	causes := usageRows(t, s, "SELECT group_concat(cause,' ') AS causes FROM (SELECT cause FROM model_turn_triggers ORDER BY client,source_time)")
	if causes[0]["causes"] != "new_prompt unknown new_prompt" {
		t.Fatalf("turn causes: %v", causes)
	}
	bindings := usageRows(t, s, "SELECT group_concat(json_extract(body,'$.relation'),' ') AS kinds FROM (SELECT body FROM records WHERE kind='relation' AND source_id='"+usageSource+"' ORDER BY event_id)")
	if kinds := stringValue(bindings[0], "kinds"); !strings.Contains(kinds, "subagent") || !strings.Contains(kinds, "child_thread") {
		t.Fatalf("bindings: %v", kinds)
	}
	for _, row := range usageRows(t, s, "SELECT body FROM records WHERE source_id='"+usageSource+"'") {
		if strings.Contains(stringValue(row, "body"), "canary") {
			t.Fatalf("private value in derived row: %v", row)
		}
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != storeVersion {
		t.Fatalf("store version changed: %d (%v)", version, err)
	}
}

// A re-delivered export and a double replay add raw rows at most, never usage.
func TestUsageRedeliveryAndReplayAddNothing(t *testing.T) {
	ctx := context.Background()
	s := fixtureStore(t)
	captureNativeFixtures(t, s)
	want := usageSummary(t, s)
	captureNativeFixtures(t, s)
	if got := usageSummary(t, s); got != want {
		t.Fatalf("re-delivery changed totals: %q, want %q", got, want)
	}
	var export bytes.Buffer
	if _, err := Export(ctx, s.db, &export, 0, 10000); err != nil {
		t.Fatal(err)
	}
	copy := fixtureStore(t)
	for range 2 {
		if err := copy.Replay(ctx, bytes.NewReader(export.Bytes())); err != nil {
			t.Fatal(err)
		}
	}
	if got := usageSummary(t, copy); got != want {
		t.Fatalf("replay changed totals: %q, want %q", got, want)
	}
}

// A worker that inherited its coordinator's session reports it as a
// resource attribute; the collector records one durable binding.
func TestParentSessionBinding(t *testing.T) {
	s := fixtureStore(t)
	logs := plogotlp.NewExportRequest()
	if err := logs.UnmarshalJSON([]byte(`{"resourceLogs":[{"resource":{"attributes":[{"key":"subyard.parent_session.id","value":{"stringValue":"coordinator-session"}}]},
		"scopeLogs":[{"logRecords":[{"observedTimeUnixNano":"1792800000000000001","attributes":[{"key":"event.name","value":{"stringValue":"codex.sse_event"}},
		{"key":"event.kind","value":{"stringValue":"response.completed"}},{"key":"conversation.id","value":{"stringValue":"worker-thread"}},{"key":"input_token_count","value":{"stringValue":"5"}}]}]}]}]}`)); err != nil {
		t.Fatal(err)
	}
	consumers, err := makeConsumers(s, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = consumers.logs.ConsumeLogs(context.Background(), logs.Logs()); err != nil {
			t.Fatal(err)
		}
	}
	rows := usageRows(t, s, `SELECT r.session_id AS worker,json_extract(r.body,'$.parent_session_id') AS parent,sum(m.input_token_count) AS input
		FROM relations r JOIN model_requests m ON m.session_id=r.session_id WHERE json_extract(r.body,'$.relation')='parent_session' GROUP BY 1,2`)
	if len(rows) != 1 || rows[0]["parent"] != "coordinator-session" || rows[0]["input"] != int64(5) {
		t.Fatalf("parent binding: %v", rows)
	}
}

func TestRawOTLPCompressionAndLegacyRows(t *testing.T) {
	ctx := context.Background()
	s := fixtureStore(t)
	captureNativeFixtures(t, s)
	legacy := []byte{0x0a, 0x00}
	if _, err := s.db.Exec(`INSERT INTO records(origin_id,event_id,received_time,source_id,source_version,source_time,kind,encoding,policy_version,redactions,protobuf)
		VALUES(?,'legacy-row',?,'native-otlp',?,'',"otlp/logs","otlp/logs",?,0,?)`, s.origin, spoolTime(), nativeSourceVersion, PolicyVersion, legacy); err != nil {
		t.Fatal(err)
	}
	var stored, raw int64
	if err := s.db.QueryRow("SELECT count(*) FROM records WHERE kind LIKE 'otlp/%' AND substr(protobuf,1,4)=?", zstdMagic).Scan(&stored); err != nil || stored != 4 {
		t.Fatalf("compressed rows: %d (%v)", stored, err)
	}
	var export bytes.Buffer
	if _, err := Export(ctx, s.db, &export, 0, 10000); err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(export.String()) {
		e, err := replayEnvelope([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		if e.Encoding == "otlp/logs" {
			r := plogotlp.NewExportRequest()
			if e.EventID != "legacy-row" && r.UnmarshalProto(e.Protobuf) != nil {
				t.Fatal("exported raw OTLP is not plain protobuf")
			}
			if e.EventID == "legacy-row" && bytes.Equal(e.Protobuf, legacy) {
				raw++
			}
		}
	}
	if raw != 1 {
		t.Fatal("uncompressed legacy row not readable")
	}
}

// Without raw capacity usage is still recorded, status names the cleanup,
// and a manual prune keeps usage unless asked.
func TestCapacityKeepsUsageAndPruneKeepsUsage(t *testing.T) {
	ctx := context.Background()
	s := fixtureStore(t)
	s.limits.DiskBytes = 1
	logs := plogotlp.NewExportRequest()
	if err := logs.UnmarshalJSON(readFixture(t, "claude-code-2.1.327-logs.json")); err != nil {
		t.Fatal(err)
	}
	consumers, err := makeConsumers(s, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if err = consumers.logs.ConsumeLogs(ctx, logs.Logs()); err == nil {
		t.Fatal("raw OTLP accepted without capacity")
	}
	count := func(where string) (n int) {
		if err := s.db.QueryRow("SELECT count(*) FROM records WHERE " + where).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count("kind LIKE 'otlp/%'") != 0 || count("kind='usage'") != 5 {
		t.Fatal("capacity refusal did not keep only usage")
	}
	stats, err := Stats(ctx, s.db)
	if err != nil || !strings.Contains(strings.Join(stats["warnings"].([]string), ""), "subyard-telemetry prune") {
		t.Fatalf("status names no cleanup: %v (%v)", stats["warnings"], err)
	}
	s.limits.DiskBytes = DefaultDiskBytes
	if err = consumers.logs.ConsumeLogs(ctx, logs.Logs()); err != nil {
		t.Fatal(err)
	}
	if stats, _ = Stats(ctx, s.db); stats["warnings"] != nil || count("kind LIKE 'otlp/%'") != 1 {
		t.Fatalf("raw capacity did not resume: %v", stats)
	}
	if _, err = s.db.Exec("UPDATE records SET received_time='2000-01-01T00:00:00.000000000Z'"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Prune(time.Now().Add(-60*24*time.Hour), false); err != nil {
		t.Fatal(err)
	}
	if count("kind LIKE 'otlp/%' OR kind='session'") != 0 || count("source_id='"+usageSource+"'") != 6 {
		t.Fatal("prune removed usage or kept raw OTLP")
	}
	if _, err = s.Prune(time.Now().Add(-60*24*time.Hour), true); err != nil || count("source_id='"+usageSource+"'") != 0 {
		t.Fatalf("explicit usage prune kept usage (%v)", err)
	}
}

// A store that filled the former one-third database share accepts data once
// the larger budget is in effect.
func TestFullStoreResumesWithLargerBudget(t *testing.T) {
	ctx := context.Background()
	s := fixtureStore(t)
	if _, err := s.db.Exec("PRAGMA max_page_count=1"); err != nil {
		t.Fatal(err)
	}
	var batch []Record
	for range 2000 {
		batch = append(batch, record("gap", map[string]any{"reason": "source_offline"}))
	}
	if err := s.Ingest(ctx, batch); !errors.Is(err, ErrStorage) {
		t.Fatalf("full store accepted data: %v", err)
	}
	dir := s.dir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Ingest(ctx, []Record{record("gap", map[string]any{"reason": "source_offline"})}); err != nil {
		t.Fatal(err)
	}
}

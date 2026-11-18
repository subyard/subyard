package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
)

func TestPrivateFilenameNeverReachesDurableTelemetry(t *testing.T) {
	const filename = "Acquisition-Target.pdf"
	const producer = `
import sys
sys.path.insert(0, '..')
import records
root, filename = sys.argv[1:]
body = dict.fromkeys(records.BODY_RULES, filename)
body.update({filename: filename, 'purpose': filename})
assert records.emit(root, 'allocation', body, filename, filename, filename)
`
	s := fixtureStore(t)
	ctx := context.Background()
	cmd := exec.Command("python3", "-B", "-c", producer, s.dir, filename)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("producer: %v\n%s", err, output)
	}
	outbox := filepath.Join(s.dir, "outbox")
	files, err := filepath.Glob(filepath.Join(outbox, "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("outbox files=%d: %v", len(files), err)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil || bytes.Contains(raw, []byte(filename)) || strings.Contains(files[0], filename) {
		t.Fatalf("the private filename reached the outbox: %v", err)
	}
	var spooled Record
	if err = json.Unmarshal(raw, &spooled); err != nil || spooled.EventID != s.key.id(filename) || spooled.SourceID != spooled.EventID ||
		spooled.Body["object_id"] != spooled.EventID {
		t.Fatalf("producer identities did not use the collector key: %v", err)
	}
	if err = s.IngestOutbox(ctx, outbox); err != nil {
		t.Fatal(err)
	}
	// A live update uses the same pseudonym as the Python producer. Register
	// the raw legacy run ID so it can also exercise verified OTLP correlation.
	update := record("allocation", map[string]any{"object_id": filename, "state": "available", "purpose": filename, "resource_kind": filename})
	update.SourceID, update.SourceVersion = filename, filename
	run := record("run", map[string]any{"agent_run_id": filename, "agent": "codex", "agent_version": filename})
	if err = s.Ingest(ctx, []Record{update, run}); err != nil {
		t.Fatal(err)
	}

	policy := DefaultPolicy()
	policy.Attributes = append(policy.Attributes, filename)
	policy.SpanNames = append(policy.SpanNames, filename)
	policy.EventNames = append(policy.EventNames, filename)
	consumers, err := makeConsumers(s, policy)
	if err != nil {
		t.Fatal(err)
	}
	privateAttributes := func(attributes pcommon.Map) {
		for name := range attributeRules {
			attributes.PutStr(name, filename)
		}
		attributes.PutStr(filename, filename)
		attributes.PutInt("gen_ai.usage.output_tokens", 7)
		attributes.PutInt("gen_ai.usage.reasoning.output_tokens", -1)
	}
	privateScope := func(scope pcommon.InstrumentationScope) {
		scope.SetName(filename)
		scope.SetVersion(filename)
		privateAttributes(scope.Attributes())
	}

	logs := plogotlp.NewExportRequest()
	resourceLogs := logs.Logs().ResourceLogs().AppendEmpty()
	resourceLogs.SetSchemaUrl(filename)
	privateAttributes(resourceLogs.Resource().Attributes())
	scopeLogs := resourceLogs.ScopeLogs().AppendEmpty()
	scopeLogs.SetSchemaUrl(filename)
	privateScope(scopeLogs.Scope())
	log := scopeLogs.LogRecords().AppendEmpty()
	log.SetEventName(filename)
	log.SetSeverityText(filename)
	log.SetSeverityNumber(plog.SeverityNumberWarn)
	log.Body().SetStr(filename)
	privateAttributes(log.Attributes())
	if err = consumers.logs.ConsumeLogs(ctx, logs.Logs()); err != nil {
		t.Fatal(err)
	}

	metrics := pmetricotlp.NewExportRequest()
	resourceMetrics := metrics.Metrics().ResourceMetrics().AppendEmpty()
	resourceMetrics.SetSchemaUrl(filename)
	privateAttributes(resourceMetrics.Resource().Attributes())
	scopeMetrics := resourceMetrics.ScopeMetrics().AppendEmpty()
	scopeMetrics.SetSchemaUrl(filename)
	privateScope(scopeMetrics.Scope())
	metric := scopeMetrics.Metrics().AppendEmpty()
	metric.SetName(filename)
	metric.SetUnit(filename)
	metric.SetDescription(filename)
	privateAttributes(metric.Metadata())
	point := metric.SetEmptyGauge().DataPoints().AppendEmpty()
	point.SetIntValue(11)
	privateAttributes(point.Attributes())
	exemplar := point.Exemplars().AppendEmpty()
	exemplar.SetIntValue(13)
	privateAttributes(exemplar.FilteredAttributes())
	known := scopeMetrics.Metrics().AppendEmpty()
	known.SetName("claude_code.token.usage")
	known.SetUnit("tokens")
	known.SetEmptyGauge().DataPoints().AppendEmpty().SetIntValue(17)
	if err = consumers.metrics.ConsumeMetrics(ctx, metrics.Metrics()); err != nil {
		t.Fatal(err)
	}

	traces := ptraceotlp.NewExportRequest()
	resourceSpans := traces.Traces().ResourceSpans().AppendEmpty()
	resourceSpans.SetSchemaUrl(filename)
	privateAttributes(resourceSpans.Resource().Attributes())
	scopeSpans := resourceSpans.ScopeSpans().AppendEmpty()
	scopeSpans.SetSchemaUrl(filename)
	privateScope(scopeSpans.Scope())
	span := scopeSpans.Spans().AppendEmpty()
	span.SetName(filename)
	span.TraceState().FromRaw(filename)
	span.Status().SetMessage(filename)
	privateAttributes(span.Attributes())
	event := span.Events().AppendEmpty()
	event.SetName(filename)
	privateAttributes(event.Attributes())
	link := span.Links().AppendEmpty()
	link.TraceState().FromRaw(filename)
	privateAttributes(link.Attributes())
	if err = consumers.traces.ConsumeTraces(ctx, traces.Traces()); err != nil {
		t.Fatal(err)
	}

	// Legacy exported OTLP is sanitized independently of the capture claim,
	// and arbitrary strings in its envelope cannot become local provenance.
	legacyProto, err := logs.MarshalProto()
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(Envelope{Schema: ExportSchema, OriginID: filename, EventID: filename,
		SourceID: filename, SourceVersion: filename, SourceTime: filename, ReceivedTime: filename,
		Kind: "otlp/logs", Encoding: "otlp/logs", PolicyVersion: PolicyVersion, ImportPolicyVersion: filename,
		Protobuf: legacyProto})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Replay(ctx, bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}

	for _, signal := range []struct {
		kind string
		req  otlpRequest
	}{
		{"otlp/logs", plogotlp.NewExportRequest()},
		{"otlp/metrics", pmetricotlp.NewExportRequest()},
		{"otlp/traces", ptraceotlp.NewExportRequest()},
	} {
		var stored []byte
		if err = s.db.QueryRow("SELECT protobuf FROM records WHERE kind=? ORDER BY sequence DESC LIMIT 1", signal.kind).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if err = signal.req.UnmarshalProto(stored); err != nil {
			t.Fatal(err)
		}
		payload, err := signal.req.MarshalJSON()
		if err != nil || bytes.Contains(payload, []byte(filename)) {
			t.Fatalf("the private filename reached decoded %s: %v", signal.kind, err)
		}
		switch request := signal.req.(type) {
		case plogotlp.ExportRequest:
			log := request.Logs().ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0)
			if log.SeverityNumber() != plog.SeverityNumberWarn || log.SeverityText() != "" {
				t.Fatal("log severity schema failed")
			}
			assertPrivateFixtureCounters(t, log.Attributes())
		case pmetricotlp.ExportRequest:
			m := request.Metrics().ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics()
			point := m.At(0).Gauge().DataPoints().At(0)
			if point.IntValue() != 11 || m.At(1).Name() != "claude_code.token.usage" || m.At(1).Unit() != "tokens" {
				t.Fatal("metric data or known vocabulary was lost")
			}
			assertPrivateFixtureCounters(t, point.Attributes())
		}
	}
	var matches int
	if err = s.db.QueryRow("SELECT count(*) FROM relations WHERE agent_run_id=? AND session_id=?", spooled.EventID, spooled.EventID).Scan(&matches); err != nil || matches != 3 {
		t.Fatalf("sanitized relation joins=%d: %v", matches, err)
	}
	var allocation string
	if err = s.db.QueryRow("SELECT json_extract(body,'$.state') FROM allocations WHERE object_id=?", spooled.EventID).Scan(&allocation); err != nil || allocation != "available" {
		t.Fatalf("Python and Go allocation IDs did not join: %v", err)
	}
	for _, name := range []string{"telemetry.sqlite", "telemetry.sqlite-wal"} {
		content, err := os.ReadFile(filepath.Join(s.dir, name))
		if err != nil || bytes.Contains(content, []byte(filename)) {
			t.Fatalf("the private filename reached %s: %v", name, err)
		}
	}
	var exported bytes.Buffer
	if _, err = Export(ctx, s.db, &exported, 0, 100); err != nil || bytes.Contains(exported.Bytes(), []byte(filename)) {
		t.Fatalf("the private filename reached the re-export: %v", err)
	}
}

func assertPrivateFixtureCounters(t *testing.T, attrs pcommon.Map) {
	t.Helper()
	for _, key := range []string{"gen_ai.usage.input_tokens", "gen_ai.usage.reasoning.output_tokens"} {
		if _, exists := attrs.Get(key); exists {
			t.Errorf("invalid counter %s persisted", key)
		}
	}
	counter, exists := attrs.Get("gen_ai.usage.output_tokens")
	if !exists || counter.Type() != pcommon.ValueTypeInt || counter.Int() != 7 {
		t.Fatal("valid intValue counter lost")
	}
}

func TestMergedProjectionDropsLegacyPrivateMetadata(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	const at = "2026-01-01T01:00:00.000000000Z"
	const private = "Acquisition-Target.pdf"
	const legacy = `{"purpose":"` + private + `","unknown":"` + private + `","reserved_cpu":"7","state":"held"}`
	run, object, association := newID(), newID(), newID()
	for _, seed := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO runs VALUES(?,?,?)", []any{run, legacy, at}},
		{"INSERT INTO objects VALUES(?,?,?,?,?,?)", []any{object, "allocation", private, private, legacy, at}},
		{"INSERT INTO allocations VALUES(?,?,?)", []any{object, legacy, at}},
		{"INSERT INTO associations VALUES(?,?,?,?,?,?,?,?)", []any{association, object, private, "explicit", at, private, false, legacy}},
	} {
		if _, err := s.db.Exec(seed.sql, seed.args...); err != nil {
			t.Fatal(err)
		}
	}
	records := []Record{
		record("run", map[string]any{"agent_run_id": run, "state": "main_exited"}),
		record("allocation", map[string]any{"object_id": object, "state": "available"}),
		record("association", map[string]any{"association_id": association, "object_id": object, "quality": "measured"}),
	}
	if err := s.Ingest(ctx, records); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"SELECT body FROM runs", "SELECT body FROM allocations",
		"SELECT body || parent_object_id || accounting FROM objects",
		"SELECT body || agent_run_id || end_time FROM associations",
	} {
		var row string
		if err := s.db.QueryRow(query).Scan(&row); err != nil || strings.Contains(row, private) || !strings.Contains(row, `"reserved_cpu":7`) {
			t.Errorf("unsafe or incomplete projection %s: %s (%v)", query, row, err)
		}
	}
}

func TestLegacyCounterAndIdentityDelivery(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	source := strings.Repeat("s", 128) + "/z"
	const at = "2026-01-01T01:00:00Z"
	// These use the prior v1 envelopes with a previously valid source ID and
	// a sample whose uint counter was serialized as a decimal string.
	const oldBody = `{"object_id":"scope/path","cpu_ns":"7"}`
	old := fmt.Sprintf(`{"schema":"subyard.telemetry.export/v1","origin_id":"producer:%[1]s","event_id":"old-event","source_id":"%[1]s","source_version":"1","source_time":"%[2]s","received_time":"%[2]s","kind":"sample","encoding":"json","policy_version":"subyard.telemetry.capture/v1","body":%[3]s}`, source, at, oldBody)
	following := fmt.Sprintf(`{"schema":"subyard.telemetry.export/v1","origin_id":"producer:%[1]s","event_id":"following","source_id":"%[1]s","source_version":"1","source_time":"%[2]s","kind":"scope","encoding":"json","policy_version":"subyard.telemetry.capture/v1","body":{"object_id":"scope/path"}}`, source, at)
	for range 2 {
		if err := s.Replay(ctx, strings.NewReader(old+"\n"+following)); err != nil {
			t.Fatal(err)
		}
	}
	outbox := testkit.TempDir(t)
	for _, entry := range []struct{ file, record string }{
		{"00.json", fmt.Sprintf(`{"schema_version":1,"source_id":"%s","event_id":"old-event","source_version":"1","source_time":"%s","kind":"sample","body":%s}`, source, at, oldBody)},
		{"01.json", fmt.Sprintf(`{"schema":"subyard.telemetry.record/v1","source_id":"%s","event_id":"new-event","source_version":"1","source_time":"%s","kind":"sample","body":{"object_id":"%s","cpu_ns":8}}`, source, at, s.key.id("scope/path"))},
	} {
		testkit.WriteFile(t, filepath.Join(outbox, entry.file), []byte(entry.record), 0600)
	}
	if err := s.IngestOutbox(ctx, outbox); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(outbox)
	if err != nil || len(files) != 0 {
		t.Fatalf("legacy outbox did not drain: remaining=%d, err=%v", len(files), err)
	}
	var count, total int
	if err = s.db.QueryRow(`SELECT count(*),sum(json_extract(samples.body,'$.cpu_ns')) FROM samples
		JOIN objects USING(object_id) JOIN records USING(origin_id,event_id)
		WHERE origin_id=? AND source_id=? AND json_type(samples.body,'$.cpu_ns')='integer'`,
		"producer:"+s.key.id(source), s.key.id(source)).Scan(&count, &total); err != nil || count != 2 || total != 15 {
		t.Fatalf("legacy joins, counters or retries failed: count=%d total=%d err=%v", count, total, err)
	}
}

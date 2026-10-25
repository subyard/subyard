package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
)

func fixtureStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(testkit.TempDir(t), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func record(kind string, body map[string]any) Record {
	return Record{Schema: RecordSchema, SourceID: "fixture", EventID: newID(), SourceVersion: "fixture/1", SourceTime: time.Now().UTC().Format(time.RFC3339Nano), Kind: kind, Body: body}
}
func TestStoreAtomicReplayAndIdenticalEvents(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	r := record("run", map[string]any{"agent_run_id": "run-one", "state": "running", "argv": "fixture-private"})
	if err := s.Ingest(ctx, []Record{r}); err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest(ctx, []Record{r}); err != nil {
		t.Fatal(err)
	}
	r.EventID = newID()
	if err := s.Ingest(ctx, []Record{r}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM records").Scan(&count); err != nil || count != 2 {
		t.Fatalf("identical legitimate records: %d %v", count, err)
	}
	valid := record("run", map[string]any{"agent_run_id": "rollback"})
	invalid := record("sample", map[string]any{"cpu_ns": 1})
	if err := s.Ingest(ctx, []Record{valid, invalid}); err == nil {
		t.Fatal("partial invalid batch committed")
	}
	if s.RegisteredRun(ctx, "rollback") {
		t.Fatal("request was not atomic")
	}
	var exported bytes.Buffer
	if _, err := Export(ctx, s.db, &exported, 0, 100); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(exported.String(), "fixture-private") {
		t.Fatal("private field persisted")
	}
	destination := fixtureStore(t)
	for i := 0; i < 2; i++ {
		if err := destination.Replay(ctx, bytes.NewReader(exported.Bytes())); err != nil {
			t.Fatal(err)
		}
	}
	if err := destination.db.QueryRow("SELECT count(*) FROM records").Scan(&count); err != nil || count != 2 {
		t.Fatalf("replay identity: %d %v", count, err)
	}
}
func TestStorageFailureAndRestart(t *testing.T) {
	dir := testkit.TempDir(t)
	s, err := Open(dir, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err = s.Ingest(ctx, []Record{record("run", map[string]any{"agent_run_id": "durable"})}); err != nil {
		t.Fatal(err)
	}
	origin := s.origin
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.origin != origin || !s.RegisteredRun(ctx, "durable") {
		t.Fatal("acknowledged data missing after restart")
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		st, err := os.Stat(filepath.Join(dir, "telemetry.sqlite") + suffix)
		if err == nil && st.Mode().Perm() != 0600 {
			t.Fatalf("unsafe SQLite mode %s %o", suffix, st.Mode().Perm())
		}
	}
	if _, err = s.db.Exec("PRAGMA query_only=ON"); err != nil {
		t.Fatal(err)
	}
	err = s.Ingest(ctx, []Record{record("run", map[string]any{"agent_run_id": "uncommitted"})})
	if !errors.Is(err, ErrStorage) {
		t.Fatalf("storage failure was success: %v", err)
	}
}
func TestBoundsAndFutureSchema(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	s.limits.RequestBytes = 100
	if err := s.Ingest(ctx, []Record{record("run", map[string]any{"agent_run_id": "bounded"})}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("request bound: %v", err)
	}
	s.limits.RequestBytes = 2 << 20
	s.limits.DiskBytes = 1
	if err := s.Ingest(ctx, []Record{record("run", map[string]any{"agent_run_id": "disk-bounded"})}); err == nil {
		t.Fatal("disk bound acknowledged")
	}
	dir := testkit.TempDir(t)
	other, err := Open(dir, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = other.db.Exec("PRAGMA user_version=3"); err != nil {
		t.Fatal(err)
	}
	other.Close()
	if future, err := Open(dir, DefaultLimits()); err == nil {
		future.Close()
		t.Fatal("future schema modified")
	}
}
func TestOTLPSanitizationPreservesMetricKinds(t *testing.T) {
	data := []byte(`{"resourceMetrics":[{"resource":{"attributes":[{"key":"Authorization","value":{"stringValue":"fixture-private"}}]},"scopeMetrics":[{"scope":{"name":"fixture","attributes":[{"key":"secret","value":{"stringValue":"fixture-private"}}]},"metrics":[{"name":"gauge","gauge":{"dataPoints":[{"timeUnixNano":"1792800000000000001","asInt":"9223372036854775806","attributes":[{"key":"secret","value":{"stringValue":"fixture-private"}}]}]}},{"name":"sum","sum":{"aggregationTemporality":2,"isMonotonic":true,"dataPoints":[{"startTimeUnixNano":"1792800000000000000","timeUnixNano":"1792800000000000001","asInt":"42"}]}},{"name":"histogram","histogram":{"aggregationTemporality":1,"dataPoints":[{"timeUnixNano":"1792800000000000001","count":"2","sum":5,"max":4,"bucketCounts":["1","1"],"explicitBounds":[2],"exemplars":[{"timeUnixNano":"1792800000000000001","asDouble":4,"traceId":"01000000000000000000000000000000","spanId":"0100000000000000","filteredAttributes":[{"key":"secret","value":{"stringValue":"fixture-private"}}]}]}]}},{"name":"exponential","exponentialHistogram":{"aggregationTemporality":1,"dataPoints":[{"timeUnixNano":"1792800000000000001","count":"2","scale":-1,"zeroCount":"1","positive":{"offset":-2,"bucketCounts":["1"]},"negative":{"offset":0,"bucketCounts":[]}}]}},{"name":"summary","summary":{"dataPoints":[{"timeUnixNano":"1792800000000000001","count":"2","sum":5,"quantileValues":[{"quantile":0.5,"value":2}]}]}}]}]}]}`)
	r := pmetricotlp.NewExportRequest()
	if err := r.UnmarshalJSON(data); err != nil {
		t.Fatal(err)
	}
	proto, err := r.MarshalProto()
	if err != nil {
		t.Fatal(err)
	}
	e := Envelope{Encoding: "otlp/metrics", Protobuf: proto}
	if err = SanitizeEnvelope(&e, DefaultPolicy()); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(e.Protobuf, []byte("fixture-private")) {
		t.Fatal("nested metric secret survived")
	}
	clean := pmetricotlp.NewExportRequest()
	if err = clean.UnmarshalProto(e.Protobuf); err != nil {
		t.Fatal(err)
	}
	metrics := clean.Metrics().ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics()
	if metrics.Len() != 5 || clean.Metrics().DataPointCount() != 5 {
		t.Fatal("metric kinds dropped")
	}
	if metrics.At(0).Gauge().DataPoints().At(0).IntValue() != 9223372036854775806 {
		t.Fatal("integer precision lost")
	}
	h := metrics.At(2).Histogram().DataPoints().At(0)
	if h.HasMin() || !h.HasMax() || h.Max() != 4 || h.BucketCounts().At(1) != 1 || h.Exemplars().At(0).TraceID().IsEmpty() {
		t.Fatal("histogram optional/bucket/exemplar semantics lost")
	}
	if metrics.At(1).Sum().DataPoints().At(0).StartTimestamp() != 1792800000000000000 || !metrics.At(1).Sum().IsMonotonic() {
		t.Fatal("start time/reset semantics lost")
	}
	if metrics.At(3).ExponentialHistogram().DataPoints().At(0).Scale() != -1 {
		t.Fatal("exponential histogram changed")
	}
}
func TestTraceLinksAndPolicy(t *testing.T) {
	r := ptraceotlp.NewExportRequest()
	err := r.UnmarshalJSON([]byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"01000000000000000000000000000000","spanId":"0100000000000000","parentSpanId":"0200000000000000","name":"safe.operation","startTimeUnixNano":"1","endTimeUnixNano":"3","status":{"code":2,"message":"fixture-private"},"links":[{"traceId":"03000000000000000000000000000000","spanId":"0400000000000000","attributes":[{"key":"secret","value":{"stringValue":"fixture-private"}}]}],"events":[{"name":"safe.event","timeUnixNano":"2","attributes":[{"key":"tool.arguments","value":{"stringValue":"fixture-private"}}]}]}]}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := r.MarshalProto()
	e := Envelope{Encoding: "otlp/traces", Protobuf: raw}
	if err = SanitizeEnvelope(&e, DefaultPolicy()); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(e.Protobuf, []byte("fixture-private")) {
		t.Fatal("trace secret survived")
	}
	clean := ptraceotlp.NewExportRequest()
	clean.UnmarshalProto(e.Protobuf)
	s := clean.Traces().ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0)
	if s.Links().Len() != 1 || s.Events().Len() != 1 || s.ParentSpanID().IsEmpty() || s.EndTimestamp() != 3 {
		t.Fatal("trace structure lost")
	}
}

func TestOTLPNumericSpecialValuesSurvivePolicy(t *testing.T) {
	r := pmetricotlp.NewExportRequest()
	err := r.UnmarshalJSON([]byte(`{"resourceMetrics":[{"scopeMetrics":[{"metrics":[{"name":"gauge","gauge":{"dataPoints":[{"asDouble":"NaN","attributes":[{"key":"numeric.value","value":{"doubleValue":"NaN"}}]}]}},{"name":"histogram","histogram":{"dataPoints":[{"count":"1","sum":"Infinity","min":"-Infinity","max":"Infinity","explicitBounds":["Infinity"],"bucketCounts":["1","0"]}]}},{"name":"exponential","exponentialHistogram":{"dataPoints":[{"count":"1","zeroThreshold":"NaN"}]}},{"name":"summary","summary":{"dataPoints":[{"count":"1","sum":1,"quantileValues":[{"quantile":0.5,"value":"NaN"}]}]}}]}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := r.MarshalProto()
	e := Envelope{Encoding: "otlp/metrics", Protobuf: raw}
	if err = SanitizeEnvelope(&e, DefaultPolicy()); err != nil {
		t.Fatal(err)
	}
	clean := pmetricotlp.NewExportRequest()
	if err = clean.UnmarshalProto(e.Protobuf); err != nil {
		t.Fatal(err)
	}
	metrics := clean.Metrics().ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics()
	gauge := metrics.At(0).Gauge().DataPoints().At(0)
	attribute, ok := gauge.Attributes().Get("numeric.value")
	if !math.IsNaN(gauge.DoubleValue()) || !ok || !math.IsNaN(attribute.Double()) {
		t.Fatal("numeric special attribute or gauge changed")
	}
	histogram := metrics.At(1).Histogram().DataPoints().At(0)
	if !math.IsInf(histogram.ExplicitBounds().At(0), 1) || !math.IsInf(histogram.Sum(), 1) || !math.IsInf(histogram.Min(), -1) || !math.IsInf(histogram.Max(), 1) {
		t.Fatal("histogram infinities changed")
	}
	if !math.IsNaN(metrics.At(2).ExponentialHistogram().DataPoints().At(0).ZeroThreshold()) || !math.IsNaN(metrics.At(3).Summary().DataPoints().At(0).QuantileValues().At(0).Value()) {
		t.Fatal("exponential histogram or summary NaN changed")
	}
}
func TestNonoverlappingAttribution(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	rs := []Record{record("run", map[string]any{"agent_run_id": "r"}), record("scope", map[string]any{"object_id": "parent", "accounting": "inclusive"}), record("scope", map[string]any{"object_id": "child", "parent_object_id": "parent", "accounting": "inclusive"}), record("scope", map[string]any{"object_id": "shared", "accounting": "direct"})}
	for _, id := range []string{"parent", "child", "shared"} {
		rs = append(rs, record("association", map[string]any{"object_id": id, "agent_run_id": "r", "quality": "explicit", "start_time": "2026-01-01T00:00:00Z", "shared": id == "shared"}))
	}
	if err := s.Ingest(ctx, rs); err != nil {
		t.Fatal(err)
	}
	result, err := ShowRun(ctx, s.db, "r")
	if err != nil {
		t.Fatal(err)
	}
	if len(result["selected_nonoverlapping"].([]any)) != 1 || len(result["shared"].([]any)) != 1 || len(result["excluded_overlapping_detail"].([]any)) != 1 {
		t.Fatalf("incorrect object accounting: %+v", result)
	}
}
func TestNativeHTTPCommitAuthAndRegistration(t *testing.T) {
	s := fixtureStore(t)
	token := strings.Repeat("a", 32)
	authFile := filepath.Join(s.dir, "auth")
	testkit.WriteFile(t, authFile, []byte(token), 0600)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, s, Options{Listen: address, AuthFile: authFile}) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	client := &http.Client{Timeout: time.Second}
	post := func(signal string, body []byte, bearer, run string) int {
		req, _ := http.NewRequest("POST", "http://"+address+"/v1/"+signal, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/x-protobuf")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if run != "" {
			req.Header.Set("x-subyard-agent-run-id", run)
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	var code int
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		code = post("logs", nil, "", "")
		if code != 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code != 401 {
		t.Fatalf("authentication status %d", code)
	}
	if code = post("logs", nil, token, "pending"); code != 503 {
		t.Fatalf("pending registration status %d", code)
	}
	if err = s.Ingest(ctx, []Record{record("run", map[string]any{"agent_run_id": "pending"})}); err != nil {
		t.Fatal(err)
	}
	r := plogotlp.NewExportRequest()
	logs := r.Logs().ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	logs.Body().SetStr("fixture-private")
	logs.Attributes().PutStr("secret", "fixture-private")
	logs.Attributes().PutStr("session.id", "native-session")
	raw, _ := r.MarshalProto()
	if code = post("logs", raw, token, "pending"); code != 200 {
		t.Fatalf("OTLP status %d", code)
	}
	var stored []byte
	if err = s.db.QueryRow("SELECT protobuf FROM records WHERE kind='otlp/logs'").Scan(&stored); err != nil {
		t.Fatal("ack before commit", err)
	}
	if bytes.Contains(stored, []byte("fixture-private")) {
		t.Fatal("private bytes persisted")
	}
	clean := plogotlp.NewExportRequest()
	clean.UnmarshalProto(stored)
	id, ok := clean.Logs().ResourceLogs().At(0).Resource().Attributes().Get("subyard.agent_run.id")
	if !ok || id.Str() != "pending" {
		t.Fatal("explicit run context missing")
	}
	var session string
	if err = s.db.QueryRow("SELECT session_id FROM relations WHERE agent_run_id='pending'").Scan(&session); err != nil || session != "native-session" {
		t.Fatalf("native session relation missing: %q %v", session, err)
	}
	resourceRequest := plogotlp.NewExportRequest()
	resource := resourceRequest.Logs().ResourceLogs().AppendEmpty()
	resource.Resource().Attributes().PutStr("subyard.agent_run.id", "pending")
	resource.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty().Attributes().PutStr("session.id", "resource-native-session")
	resourceBytes, _ := resourceRequest.MarshalProto()
	if code = post("logs", resourceBytes, token, ""); code != 200 {
		t.Fatalf("resource-based run context HTTP %d", code)
	}
	if err = s.db.QueryRow("SELECT session_id FROM relations WHERE agent_run_id='pending' AND session_id='resource-native-session'").Scan(&session); err != nil {
		t.Fatal("resource native session missing", err)
	}
	resource.Resource().Attributes().PutStr("subyard.agent_run.id", "not-yet-registered")
	resourceBytes, _ = resourceRequest.MarshalProto()
	if code = post("logs", resourceBytes, token, ""); code != 503 {
		t.Fatalf("pending resource run HTTP %d", code)
	}
	if code = post("logs", resourceBytes, token, "pending"); code != 400 {
		t.Fatalf("conflicting header/resource HTTP %d", code)
	}
	metrics := pmetricotlp.NewExportRequest()
	metrics.Metrics().ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty().SetEmptyGauge().DataPoints().AppendEmpty().SetIntValue(1)
	metricBytes, _ := metrics.MarshalProto()
	if code = post("metrics", metricBytes, token, "pending"); code != 200 {
		t.Fatalf("metrics HTTP status %d", code)
	}
	traces := ptraceotlp.NewExportRequest()
	span := traces.Traces().ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	span.SetTraceID(pcommon.TraceID{1})
	span.SetSpanID(pcommon.SpanID{1})
	traceBytes, _ := traces.MarshalProto()
	if code = post("traces", traceBytes, token, "pending"); code != 200 {
		t.Fatalf("traces HTTP status %d", code)
	}
	db, err := OpenReadOnly(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = Query(ctx, db, "DELETE FROM records"); err == nil {
		t.Fatal("query API allowed mutation")
	}
	var output bytes.Buffer
	if _, err = Export(ctx, db, &output, 0, 100); err != nil {
		t.Fatal(err)
	}
	var exported Envelope
	if err = json.NewDecoder(&output).Decode(&exported); err != nil {
		t.Fatal(err)
	}
}

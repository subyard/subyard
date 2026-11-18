package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
)

func TestCustomCapturePolicyAndFingerprint(t *testing.T) {
	p := DefaultPolicy()
	p.Attributes = append(p.Attributes, "custom.safe")
	p.SpanNames, p.EventNames = []string{"codex.turn", "fixture.operation"}, []string{"codex.api_request"}
	path := filepath.Join(testkit.TempDir(t), "policy.json")
	data, _ := json.Marshal(p)
	testkit.WriteFile(t, path, data, 0600)
	loaded, err := LoadPolicy(path)
	if err != nil || policyFingerprint(loaded) != policyFingerprint(p) {
		t.Fatalf("policy load/fingerprint changed: %v", err)
	}
	changed := loaded
	changed.EventNames = []string{"different.event"}
	if policyFingerprint(changed) == policyFingerprint(p) {
		t.Fatal("different capture policy retained fingerprint")
	}
	r := ptraceotlp.NewExportRequest()
	if err := r.UnmarshalJSON([]byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[{"name":"codex.turn","attributes":[{"key":"gen_ai.usage.input_tokens","value":{"intValue":"7"}},{"key":"custom.safe","value":{"stringValue":"safe-metadata"}},{"key":"Authorization","value":{"stringValue":"PRIVATE"}}],"events":[{"name":"codex.api_request"}]}]}]}]}`)); err != nil {
		t.Fatal(err)
	}
	raw, _ := r.MarshalProto()
	e := Envelope{Encoding: "otlp/traces", Protobuf: raw}
	if err := (identityKey{}).sanitizeOTLP(&e, p); err != nil {
		t.Fatal(err)
	}
	clean := ptraceotlp.NewExportRequest()
	if err := clean.UnmarshalProto(e.Protobuf); err != nil {
		t.Fatal(err)
	}
	span := clean.Traces().ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0)
	if span.Name() != "codex.turn" || span.Events().At(0).Name() != "codex.api_request" || span.Attributes().Len() != 1 || bytes.Contains(e.Protobuf, []byte("PRIVATE")) || e.PolicyVersion != policyFingerprint(p) {
		t.Fatal("custom safe capture or mandatory sensitive-key exclusion failed")
	}
}

func TestReplayPreservesCapturePolicyAndAppliesTrustedImportPolicy(t *testing.T) {
	ctx := context.Background()
	for _, custom := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "custom"}[custom], func(t *testing.T) {
			s := fixtureStore(t)
			p := DefaultPolicy()
			if custom {
				p.Attributes = append(p.Attributes, "custom.safe")
				p.SpanNames = []string{"custom.operation"}
			}
			r := ptraceotlp.NewExportRequest()
			span := r.Traces().ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty()
			span.SetName("custom.operation")
			span.Attributes().PutStr("custom.safe", "safe-metadata")
			raw, _ := r.MarshalProto()
			e := Envelope{Kind: "otlp/traces", Encoding: "otlp/traces", SourceID: "native-otlp", SourceVersion: "1", Protobuf: raw}
			// A historical custom policy could authorize arbitrary text.
			e.PolicyVersion = policyFingerprint(p)
			if !custom {
				if err := s.key.sanitizeOTLP(&e, p); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.submit(ctx, []Envelope{e}); err != nil {
				t.Fatal(err)
			}
			var exported bytes.Buffer
			if _, err := Export(ctx, s.db, &exported, 0, 10); err != nil {
				t.Fatal(err)
			}
			var original Envelope
			if err := json.Unmarshal(exported.Bytes(), &original); err != nil {
				t.Fatal(err)
			}
			// Import metadata is untrusted input and cannot authorize capture.
			original.ImportPolicyVersion = "PRIVATE-untrusted-import-policy"
			input, _ := json.Marshal(original)
			destination := fixtureStore(t)
			for i := 0; i < 2; i++ {
				if err := destination.Replay(ctx, bytes.NewReader(input)); err != nil {
					t.Fatal(err)
				}
			}
			var capture, applied string
			var stored []byte
			var count int
			if err := destination.db.QueryRow("SELECT count(*),policy_version,import_policy_version,protobuf FROM records").Scan(&count, &capture, &applied, &stored); err != nil {
				t.Fatal(err)
			}
			if count != 1 || capture != policyFingerprint(p) || applied != policyFingerprint(DefaultPolicy()) || bytes.Contains(stored, []byte("safe-metadata")) || bytes.Contains(stored, []byte("custom.operation")) {
				t.Fatalf("capture claim/import enforcement: count=%d capture=%s applied=%s", count, capture, applied)
			}
			var imported bytes.Buffer
			if _, err := Export(ctx, destination.db, &imported, 0, 10); err != nil {
				t.Fatal(err)
			}
			var roundTrip Envelope
			if err := json.Unmarshal(imported.Bytes(), &roundTrip); err != nil {
				t.Fatal(err)
			}
			if roundTrip.OriginID != original.OriginID || roundTrip.EventID != original.EventID || roundTrip.PolicyVersion != original.PolicyVersion || roundTrip.ImportPolicyVersion != applied || bytes.Contains(imported.Bytes(), []byte("PRIVATE")) {
				t.Fatal("export lost provenance/identity or trusted imported policy text")
			}
			if err := destination.Replay(ctx, bytes.NewReader(imported.Bytes())); err != nil {
				t.Fatal("re-export replay not idempotent", err)
			}
			if err := s.Replay(ctx, bytes.NewReader(exported.Bytes())); err != nil {
				t.Fatal("local export replay not idempotent", err)
			}
			for _, captureClaim := range []string{"PRIVATE-invalid-capture", PolicyVersion + ":" + strings.Repeat("a", 64)} {
				collision := roundTrip
				collision.PolicyVersion = captureClaim
				data, _ := json.Marshal(collision)
				if err := destination.Replay(ctx, bytes.NewReader(data)); err == nil {
					t.Fatalf("invalid/conflicting capture claim accepted: %s", captureClaim)
				}
			}
		})
	}
}

func TestCapturePolicyV1MigrationAndLegacyExport(t *testing.T) {
	dir := testkit.TempDir(t)
	s, err := Open(dir, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest(context.Background(), []Record{record("run", map[string]any{"agent_run_id": "legacy"})}); err != nil {
		t.Fatal(err)
	}
	// Reconstruct the historical v1 records layout while preserving a real row.
	if _, err := s.db.Exec("ALTER TABLE records DROP COLUMN import_policy_version; PRAGMA user_version=1;"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	db, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	var exported bytes.Buffer
	_, err = Export(context.Background(), db, &exported, 0, 10)
	var version int
	db.QueryRow("PRAGMA user_version").Scan(&version)
	db.Close()
	if err != nil || version != 1 || bytes.Contains(exported.Bytes(), []byte("import_policy_version")) {
		t.Fatalf("legacy read-only export changed schema: version=%d err=%v", version, err)
	}
	s, err = Open(dir, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var capture, applied string
	if err := s.db.QueryRow("SELECT policy_version,import_policy_version FROM records").Scan(&capture, &applied); err != nil || capture != PolicyVersion || applied != "" {
		t.Fatalf("migration altered original policy claim: %q %q %v", capture, applied, err)
	}
	if err := s.Replay(context.Background(), bytes.NewReader(exported.Bytes())); err != nil {
		t.Fatal("legacy JSONL replay failed", err)
	}
	s.db.QueryRow("PRAGMA user_version").Scan(&version)
	if version != 2 || !s.RegisteredRun(context.Background(), "legacy") {
		t.Fatal("migration lost lifecycle data or schema version")
	}
}

func TestReplaySanitizesSourceMetadataWithoutBlockingDelivery(t *testing.T) {
	ctx := context.Background()
	store := fixtureStore(t)
	r := record("gap", map[string]any{"reason": "/home/fixture/private-notes", "status": "fixture@example.invalid", "observed_time": "not a timestamp"})
	r.SourceID = strings.Repeat("s", 129)
	r.SourceVersion = "private version text"
	if err := store.Ingest(ctx, []Record{r}); err != nil {
		t.Fatal(err)
	}
	raw := ptraceotlp.NewExportRequest()
	raw.Traces().ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	protobuf, err := raw.MarshalProto()
	if err != nil {
		t.Fatal(err)
	}
	var input bytes.Buffer
	encoder := json.NewEncoder(&input)
	for _, sourceTime := range []string{"private time text", "2026-01-01T01:00:00+01:00", ""} {
		if err := encoder.Encode(Envelope{
			Schema: ExportSchema, OriginID: "fixture", EventID: newID(),
			SourceID: "fixture", SourceVersion: "1", SourceTime: sourceTime,
			Kind: "otlp/traces", Encoding: "otlp/traces", Protobuf: protobuf,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Replay(ctx, &input); err != nil {
		t.Fatal(err)
	}
	var exported bytes.Buffer
	if _, err := Export(ctx, store.db, &exported, 0, 10); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(exported.Bytes(), []byte("private")) || bytes.Contains(exported.Bytes(), []byte("not a timestamp")) {
		t.Fatal("unsafe source or body metadata survived")
	}
	decoder := json.NewDecoder(bytes.NewReader(exported.Bytes()))
	var producer Envelope
	if err := decoder.Decode(&producer); err != nil || producer.SourceVersion != "" || producer.Redactions != 4 || string(producer.Body) != "{}" {
		t.Fatalf("producer version and body were not removed: redactions=%d, err=%v", producer.Redactions, err)
	}
	for i, want := range []string{"", "2026-01-01T00:00:00.000000000Z", ""} {
		var envelope Envelope
		if err := decoder.Decode(&envelope); err != nil || envelope.SourceTime != want || envelope.SourceVersion != "1" || (envelope.Redactions == 1) != (i == 0) {
			t.Fatalf("raw source metadata mismatch at row %d: redactions=%d, err=%v", i, envelope.Redactions, err)
		}
	}
	destination := fixtureStore(t)
	// Populate retained rows with bytes accepted before the policy tightened.
	producer.SourceID = r.SourceID
	producer.OriginID = "producer:" + r.SourceID
	producer.SourceVersion = r.SourceVersion
	producer.Body, err = json.Marshal(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	legacyStore := fixtureStore(t)
	if err := legacyStore.submit(ctx, []Envelope{producer}); err != nil {
		t.Fatal(err)
	}
	if err := legacyStore.Ingest(ctx, []Record{r}); err != nil {
		t.Fatal("legacy producer retry failed", err)
	}
	var legacy bytes.Buffer
	if _, err := Export(ctx, legacyStore.db, &legacy, 0, 10); err != nil {
		t.Fatal(err)
	}
	for _, target := range []*Store{legacyStore, store} {
		if err := target.Replay(ctx, bytes.NewReader(legacy.Bytes())); err != nil {
			t.Fatal("legacy replay failed", err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := destination.Replay(ctx, bytes.NewReader(exported.Bytes())); err != nil {
			t.Fatal("export replay failed", err)
		}
	}
	var count int
	if err := destination.db.QueryRow("SELECT count(*) FROM records").Scan(&count); err != nil || count != 4 {
		t.Fatalf("replay count=%d, err=%v", count, err)
	}
}

func TestReplayLegacyOutOfRangeTimestamps(t *testing.T) {
	ctx := context.Background()
	store := fixtureStore(t)
	r := record("gap", map[string]any{"reason": "source_offline"})
	legacyTime, err := time.Parse(time.RFC3339Nano, "9999-12-31T23:59:59-01:00")
	if err != nil {
		t.Fatal(err)
	}
	legacy := Envelope{OriginID: "producer:" + r.SourceID, EventID: r.EventID,
		SourceID: r.SourceID, SourceVersion: r.SourceVersion, SourceTime: legacyTime.UTC().Format(timestampLayout),
		ReceivedTime: legacyTime.UTC().Format(timestampLayout),
		Kind:         r.Kind, Encoding: "json", PolicyVersion: PolicyVersion, Body: json.RawMessage(`{"reason":"source_offline"}`)}
	if err := store.submit(ctx, []Envelope{legacy}); err != nil {
		t.Fatal(err)
	}
	r.EventID = newID()
	if err := store.Ingest(ctx, []Record{r}); err != nil {
		t.Fatal(err)
	}
	var exported bytes.Buffer
	if _, err := Export(ctx, store.db, &exported, 0, 10); err != nil {
		t.Fatal(err)
	}
	destination := fixtureStore(t)
	for _, target := range []*Store{destination, store} {
		if err := target.Replay(ctx, bytes.NewReader(exported.Bytes())); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	var sourceTime, receivedTime string
	var redactions int
	if err := destination.db.QueryRow("SELECT source_time, received_time, redactions FROM records WHERE event_id=?", legacy.EventID).Scan(&sourceTime, &receivedTime, &redactions); err != nil || sourceTime != unknownSourceTime || receivedTime != unknownSourceTime || redactions != 2 {
		t.Fatalf("source time=%s, receive time=%s, redactions=%d, err=%v", sourceTime, receivedTime, redactions, err)
	}
	if err := destination.db.QueryRow("SELECT count(*) FROM records").Scan(&count); err != nil || count != 2 {
		t.Fatalf("replayed count=%d, err=%v", count, err)
	}
}

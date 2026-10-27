package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
)

func TestCustomCapturePolicyAndFingerprint(t *testing.T) {
	p := DefaultPolicy()
	p.Attributes = append(p.Attributes, "custom.safe")
	p.SpanNames, p.EventNames = []string{"fixture.operation"}, []string{"fixture.event"}
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
	for _, key := range []string{"Authorization", "api_key", "tool.arguments", "input.messages", "lease.capability", "secret", "argv", "http.request.header"} {
		bad := p
		bad.Attributes = []string{key}
		data, _ = json.Marshal(bad)
		testkit.WriteFile(t, path, data, 0600)
		if _, err := LoadPolicy(path); err == nil {
			t.Errorf("sensitive policy key accepted: %s", key)
		}
	}
	r := ptraceotlp.NewExportRequest()
	if err := r.UnmarshalJSON([]byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[{"name":"fixture.operation","attributes":[{"key":"custom.safe","value":{"stringValue":"safe-metadata"}},{"key":"Authorization","value":{"stringValue":"PRIVATE"}}],"events":[{"name":"fixture.event"}]}]}]}]}`)); err != nil {
		t.Fatal(err)
	}
	raw, _ := r.MarshalProto()
	e := Envelope{Encoding: "otlp/traces", Protobuf: raw}
	if err := SanitizeEnvelope(&e, p); err != nil {
		t.Fatal(err)
	}
	clean := ptraceotlp.NewExportRequest()
	if err := clean.UnmarshalProto(e.Protobuf); err != nil {
		t.Fatal(err)
	}
	span := clean.Traces().ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0)
	if span.Name() != "fixture.operation" || span.Events().At(0).Name() != "fixture.event" || span.Attributes().Len() != 1 || bytes.Contains(e.Protobuf, []byte("PRIVATE")) || e.PolicyVersion != policyFingerprint(p) {
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
			e := Envelope{Kind: "otlp/traces", Encoding: "otlp/traces", SourceID: "fixture", SourceVersion: "fixture/1", Protobuf: raw}
			if err := SanitizeEnvelope(&e, p); err != nil {
				t.Fatal(err)
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
			if !custom {
				if err := s.Replay(ctx, bytes.NewReader(exported.Bytes())); err != nil {
					t.Fatal("local default export replay not idempotent", err)
				}
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

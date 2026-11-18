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
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
)

func TestReplayLegacyRowCanGrowPastLiveRequestLimit(t *testing.T) {
	ctx := context.Background()
	store := fixtureStore(t)
	logs := plogotlp.NewExportRequest()
	records := logs.Logs().ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords()
	records.EnsureCapacity(30000)
	for range 30000 {
		records.AppendEmpty().Attributes().PutStr("session.id", "s")
	}
	raw, err := logs.MarshalProto()
	if err != nil || int64(len(raw)) >= store.limits.RequestBytes {
		t.Fatalf("legacy fixture exceeds live request limit: %d (%v)", len(raw), err)
	}
	var stream bytes.Buffer
	encoder := json.NewEncoder(&stream)
	if err := encoder.Encode(Envelope{Schema: ExportSchema, OriginID: "legacy", EventID: "large-signal",
		SourceID: "native-otlp", SourceVersion: nativeSourceVersion, Encoding: "otlp/logs", Kind: "otlp/logs",
		PolicyVersion: PolicyVersion, Protobuf: raw}); err != nil {
		t.Fatal(err)
	}
	stream.WriteString(`{"schema":"subyard.telemetry.export/v1","origin_id":"producer:fixture","source_id":"fixture","event_id":"following","source_version":"1","kind":"gap","encoding":"json","body":{}}`)
	if err := store.Replay(ctx, &stream); err != nil {
		t.Fatal(err)
	}
	var count, size int64
	if err := store.db.QueryRow("SELECT count(*),max(length(protobuf)) FROM records").Scan(&count, &size); err != nil || count != 2 || size <= store.limits.RequestBytes {
		t.Fatalf("replay did not retain the expanded record and its successor: %d %d (%v)", count, size, err)
	}
	stream.Reset()
	if _, err := Export(ctx, store.db, &stream, 0, 100); err != nil {
		t.Fatal(err)
	}
	if err := fixtureStore(t).Replay(ctx, &stream); err != nil {
		t.Fatal("expanded row is not portable", err)
	}
}

func TestNarrowPolicyControlsLiveAndReplayedSessionRelations(t *testing.T) {
	ctx := context.Background()
	source := fixtureStore(t)
	const run = "fixture-run"
	if err := source.Ingest(ctx, []Record{record("run", map[string]any{"agent_run_id": run})}); err != nil {
		t.Fatal(err)
	}
	p := Policy{Schema: PolicyVersion, Attributes: []string{"session.id"}}
	data := func() plogotlp.ExportRequest {
		logs := plogotlp.NewExportRequest()
		resource := logs.Logs().ResourceLogs().AppendEmpty()
		resource.Resource().Attributes().PutStr("subyard.agent_run.id", run)
		signal := resource.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
		signal.Attributes().PutStr("session.id", "kept")
		signal.Attributes().PutStr("thread.id", "excluded")
		return logs
	}
	logs, _, _, err := source.Consumers(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := logs.ConsumeLogs(ctx, data().Logs()); err != nil {
		t.Fatal(err)
	}
	var found, attached int
	if err := source.db.QueryRow("SELECT count(*),sum(agent_run_id<>'') FROM relations").Scan(&found, &attached); err != nil || found != 1 || attached != 0 {
		t.Fatalf("live relation bypassed the policy: count=%d attached=%d (%v)", found, attached, err)
	}
	cutoff := int64(3) // Registered run, narrow OTLP, narrow relation.
	logs, _, _, err = source.Consumers(DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if err := logs.ConsumeLogs(ctx, data().Logs()); err != nil {
		t.Fatal(err)
	}
	relation := record("relation", map[string]any{
		"session_id": "mixed", "session_attributes": []string{"session.id", "thread.id"},
		"agent_run_id": run, "relation": "native_session", "source": "native-otlp-attribute/authenticated-header",
	})
	if err := source.Ingest(ctx, []Record{relation}); err != nil {
		t.Fatal(err)
	}
	var exported bytes.Buffer
	if _, err := Export(ctx, source.db, &exported, cutoff, 100); err != nil {
		t.Fatal(err)
	}
	config, _ := json.Marshal(p)
	temp := testkit.TempDir(t)
	policyFile, input := filepath.Join(temp, "policy.json"), filepath.Join(temp, "input.jsonl")
	testkit.WriteFile(t, policyFile, config, 0600)
	testkit.WriteFile(t, input, exported.Bytes(), 0600)
	dest := testkit.TempDir(t)
	if err := RunCLI([]string{"replay", "--data-dir", dest, "--input", input, "--policy-file", policyFile}, nil); err != nil {
		t.Fatal(err)
	}
	db, err := OpenReadOnly(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow("SELECT count(*),sum(agent_run_id<>'') FROM relations").Scan(&found, &attached); err != nil || found != 2 || attached != 0 {
		t.Fatalf("replayed relation bypassed the selected policy: count=%d attached=%d (%v)", found, attached, err)
	}
	var raw []byte
	var applied string
	if err := db.QueryRow("SELECT protobuf,import_policy_version FROM records WHERE encoding='otlp/logs'").Scan(&raw, &applied); err != nil || applied != policyFingerprint(p) {
		t.Fatalf("replay did not apply the selected policy: %s (%v)", applied, err)
	}
	var redactions int
	var body string
	if err := db.QueryRow("SELECT import_policy_version,redactions,body FROM records WHERE kind='session' ORDER BY redactions DESC LIMIT 1").
		Scan(&applied, &redactions, &body); err != nil || applied != policyFingerprint(p) || redactions != 4 ||
		strings.Contains(body, "thread.id") || strings.Contains(body, "agent_run_id") || strings.Contains(body, "authenticated-header") {
		t.Fatalf("JSON import policy/redactions: %s %d %s (%v)", applied, redactions, body, err)
	}
	replayed := plogotlp.NewExportRequest()
	if err := replayed.UnmarshalProto(raw); err != nil {
		t.Fatal(err)
	}
	content, err := replayed.MarshalJSON()
	if err != nil || bytes.Contains(content, []byte("thread.id")) || bytes.Contains(content, []byte("subyard.agent_run.id")) || !bytes.Contains(content, []byte("session.id")) {
		t.Fatalf("replay payload escaped policy: %v", err)
	}
	privateSchema := strings.Repeat("ab", 32)
	p.Schema = privateSchema
	if _, _, _, err := source.Consumers(p); err == nil || strings.Contains(policyFingerprint(p), privateSchema) {
		t.Fatal("programmatic policy admitted a caller-supplied schema")
	}
}

func TestPythonOutboxUsesSelectedCapturePolicy(t *testing.T) {
	store := fixtureStore(t)
	outbox := filepath.Join(store.dir, "outbox")
	if err := privateDir(outbox); err != nil {
		t.Fatal(err)
	}
	p := Policy{Schema: PolicyVersion, Attributes: []string{"session.id"}}
	if err := store.publishCapturePolicy(outbox, p); err != nil {
		t.Fatal(err)
	}
	const producer = `
from pathlib import Path
import sys
sys.path.insert(0, '..')
import records
root = Path(sys.argv[1])
relation = dict(session_id='synthetic-session', agent_run_id='synthetic-run',
                relation='native_session', source='native-otlp-attribute/authenticated-header')
assert records.emit(root, 'relation', dict(relation, session_attributes=['thread.id']), event_id='excluded')
assert not list((root/'outbox').glob('*.json'))
for _ in range(2):
    assert records.emit(root, 'relation', dict(relation, session_attributes=['thread.id', 'session.id']), event_id='mixed')
path, = (root/'outbox').glob('*.json')
record = records.read_outbox(path)
assert record['kind'] == 'session'
assert record['body'] == dict(session_id=records.opaque_id(records.identity_key(root), 'synthetic-session'),
                             session_attributes=['session.id'], source='native-otlp-attribute')
assert record['redactions'] == 5  # Four fields or entries, plus the unknown source time.
`
	if output, err := exec.Command("python3", "-B", "-c", producer, store.dir).CombinedOutput(); err != nil {
		t.Fatalf("producer: %v\n%s", err, output)
	}
	if err := store.ingestOutbox(context.Background(), outbox, p); err != nil {
		t.Fatal(err)
	}
	var redactions int
	var policy string
	if err := store.db.QueryRow("SELECT redactions,policy_version FROM records WHERE kind='session'").Scan(&redactions, &policy); err != nil ||
		redactions != 5 || policy != policyFingerprint(p) {
		t.Fatalf("outbox drain lost policy/redactions: %s %d (%v)", policy, redactions, err)
	}
	if entries, err := os.ReadDir(outbox); err != nil || len(entries) != 1 || entries[0].Name() != ".lock" {
		t.Fatalf("outbox did not drain: %v (%v)", entries, err)
	}
}

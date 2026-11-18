package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
key = records.identity_key(root)
record['event_id'] = records.opaque_id(key, 'previous-format')
del record['policy_version']
records.atomic_json(root/'outbox'/(record['event_id']+'.json'), records.signed_record(key, record), outbox=True)
`
	if output, err := exec.Command("python3", "-B", "-c", producer, store.dir).CombinedOutput(); err != nil {
		t.Fatalf("producer: %v\n%s", err, output)
	}
	// Restarting the collector with a broader policy must not relabel capture.
	defaultPolicy := DefaultPolicy()
	if err := store.publishCapturePolicy(outbox, defaultPolicy); err != nil {
		t.Fatal(err)
	}
	if err := store.ingestOutbox(context.Background(), outbox, defaultPolicy); err != nil {
		t.Fatal(err)
	}
	for event, want := range map[string]string{"mixed": policyFingerprint(p), "previous-format": PolicyVersion} {
		var redactions int
		var policy string
		if err := store.db.QueryRow("SELECT redactions,policy_version FROM records WHERE kind='session' AND event_id=?",
			store.key.id(event)).Scan(&redactions, &policy); err != nil || redactions != 5 || policy != want {
			t.Fatalf("outbox drain lost policy/redactions for %s: %s %d (%v)", event, policy, redactions, err)
		}
	}
	if entries, err := os.ReadDir(outbox); err != nil || len(entries) != 1 || entries[0].Name() != ".lock" {
		t.Fatalf("outbox did not drain: %v (%v)", entries, err)
	}
}

func TestPreStartupOutboxRelationDrainsAfterPolicyChange(t *testing.T) {
	ctx := context.Background()
	const producer = `
from pathlib import Path
import sys
sys.path.insert(0, '..')
import records
root = Path(sys.argv[1])
assert records.emit(root, 'relation', dict(session_id='fixture-session', agent_run_id='fixture-run',
    relation='native_session', source='native-otlp-attribute/authenticated-header',
    session_attributes=['session.id', 'thread.id']), event_id='first-relation')
`
	wide := DefaultPolicy()
	narrow := Policy{Schema: PolicyVersion, Attributes: []string{"session.id"}}
	for _, change := range []struct {
		name, kind    string
		before, after Policy
	}{
		{"tighten", "relation", wide, narrow},
		{"widen", "session", narrow, wide},
	} {
		t.Run(change.name, func(t *testing.T) {
			dir := testkit.TempDir(t)
			if output, err := exec.Command("python3", "-B", "-c", producer, dir).CombinedOutput(); err != nil {
				t.Fatalf("producer: %v\n%s", err, output)
			}
			store, err := Open(dir, DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { store.Close() })
			outbox := filepath.Join(dir, "outbox")
			path := filepath.Join(outbox, store.key.id("first-relation")+".json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			signed, err := store.key.decodeRecord(data)
			want := policyFingerprint(DefaultPolicy())
			if err != nil || !signed.authenticated || signed.Kind != "relation" || signed.capturePolicy != want {
				t.Fatalf("pre-startup capture policy = %q, want %q (%v)", signed.capturePolicy, want, err)
			}
			// Commit as the original drain would, leaving the file for redelivery.
			if err = store.ingest(ctx, []Record{signed}, change.before); err != nil {
				t.Fatal(err)
			}
			if err = store.Close(); err != nil {
				t.Fatal(err)
			}
			restarted, err := Open(dir, DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			if err := restarted.publishCapturePolicy(outbox, change.after); err != nil {
				t.Fatal(err)
			}
			if err := restarted.ingestOutbox(ctx, outbox, change.after); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("committed relation still in the outbox: %v", err)
			}
			var count int
			var kind, policy string
			if err := restarted.db.QueryRow("SELECT count(*),min(kind),min(policy_version) FROM records").
				Scan(&count, &kind, &policy); err != nil || count != 1 || kind != change.kind || policy != want {
				t.Fatalf("retry changed the committed record: %d %s %s (%v)", count, kind, policy, err)
			}
			signed.Body["session_id"] = restarted.key.id("different-session")
			if err := restarted.ingest(ctx, []Record{signed}, change.after); !errors.Is(err, errEventCollision) {
				t.Fatalf("different session must still collide: %v", err)
			}
		})
	}
}

func TestSessionPolicyAcrossProducerIngestAndReplay(t *testing.T) {
	ctx := context.Background()
	p := Policy{Schema: PolicyVersion, Attributes: []string{"session.id"}}
	input := []Record{
		record("run", map[string]any{"agent_run_id": "test-run", "session_id": "excluded",
			"session_attributes": []string{"thread.id"}, "unknown_fields": []string{"excluded"}}),
		record("relation", map[string]any{"agent_run_id": "test-run", "native_session_id": "excluded",
			"session_attributes": []string{"thread.id"}}),
		record("relation", map[string]any{"agent_run_id": "test-run", "native_session_id": "allowed",
			"session_attributes": []string{"thread.id", "session.id"}}),
		record("run", map[string]any{"agent_run_id": "other-run", "native_session_id": "allowed",
			"session_attributes": []string{"session.id"}}),
	}
	for _, transport := range []string{"producer", "ingest", "replay"} {
		t.Run(transport, func(t *testing.T) {
			store := fixtureStore(t)
			switch transport {
			case "producer":
				outbox := filepath.Join(store.dir, "outbox")
				if err := store.publishCapturePolicy(outbox, p); err != nil {
					t.Fatal(err)
				}
				const producer = `
import json, pathlib, sys
sys.path.insert(0, '..')
import records
root = pathlib.Path(sys.argv[1])
for row in json.load(sys.stdin):
    assert records.emit(root, row['kind'], row['body'], source_id=row['source_id'], event_id=row['event_id'])
`
				raw, err := json.Marshal(input)
				if err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command("python3", "-B", "-c", producer, store.dir)
				cmd.Stdin = bytes.NewReader(raw)
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("producer: %v\n%s", err, output)
				}
				// Drain under the default policy to prove filtering happened before publication.
				if err := store.IngestOutbox(ctx, outbox); err != nil {
					t.Fatal(err)
				}
			case "ingest":
				if err := store.ingest(ctx, input, p); err != nil {
					t.Fatal(err)
				}
			case "replay":
				var stream bytes.Buffer
				for _, r := range input {
					body, _ := json.Marshal(r.Body)
					e := Envelope{Schema: ExportSchema, OriginID: "producer:" + r.SourceID,
						SourceID: r.SourceID, SourceVersion: r.SourceVersion, EventID: r.EventID,
						Kind: r.Kind, Encoding: "json", Body: body}
					if err := json.NewEncoder(&stream).Encode(e); err != nil {
						t.Fatal(err)
					}
				}
				if err := store.replay(ctx, &stream, p); err != nil {
					t.Fatal(err)
				}
			}
			expected := map[string]struct{ kind, body string }{
				store.key.id(input[0].EventID): {"run", `{"agent_run_id":"` + store.key.id("test-run") + `"}`},
				store.key.id(input[2].EventID): {"session", `{"session_attributes":["session.id"],"session_id":"` +
					store.key.id("allowed") + `","source":"native-otlp-attribute"}`},
				store.key.id(input[3].EventID): {"run", `{"agent_run_id":"` + store.key.id("other-run") + `"}`},
			}
			rows, err := store.db.Query("SELECT event_id,kind,body FROM records")
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			for rows.Next() {
				var event, kind, body string
				if err := rows.Scan(&event, &kind, &body); err != nil {
					t.Fatal(err)
				}
				if want := expected[event]; want.kind != kind || want.body != body {
					t.Errorf("unexpected %s record %s", kind, body)
				}
				delete(expected, event)
			}
			if err := rows.Err(); err != nil || len(expected) != 0 {
				t.Fatalf("missing policy-filtered records: %v (%v)", expected, err)
			}
		})
	}
}

func TestReplayRestoresPrunedSampleToItsOriginalRun(t *testing.T) {
	ctx := context.Background()
	store := fixtureStore(t)
	const run, object = "fixture-run", "fixture-scope"
	scope := record("scope", map[string]any{"object_id": object, "agent_run_id": run, "quality": "explicit"})
	sample := record("sample", map[string]any{"object_id": object, "cpu_ns": 10})
	if err := store.Ingest(ctx, []Record{record("run", map[string]any{"agent_run_id": run}), scope, sample}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("UPDATE records SET received_time='2000-01-01T00:00:00.000000000Z' WHERE kind='sample'"); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReadOnly(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var exported bytes.Buffer
	if _, err := Export(ctx, reader, &exported, 0, 10); err != nil {
		t.Fatal(err)
	}
	if err := store.prune(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRow("SELECT count(*) FROM samples").Scan(&count); err != nil || count != 0 {
		t.Fatalf("sample was not pruned: %d (%v)", count, err)
	}
	// An altered export must go through the destination's keyed mapping,
	// even when a case-insensitive decoder would see the original body.
	lines := bytes.Split(bytes.TrimSpace(exported.Bytes()), []byte("\n"))
	forgedBody := `"body":{"cpu_ns":90,"object_id":"` + store.key.id(object) + `"},"BODY":`
	altered := bytes.Replace(lines[len(lines)-1], []byte(`"body":`), []byte(forgedBody), 1)
	if err := store.Replay(ctx, bytes.NewReader(altered)); err != nil {
		t.Fatal(err)
	}
	if err := store.Replay(ctx, &exported); err != nil {
		t.Fatal(err)
	}
	query := `SELECT count(*),sum(sample_values.cpu_ns) FROM sample_values
		JOIN objects USING(object_id) JOIN associations USING(object_id)
		JOIN runs USING(agent_run_id) WHERE runs.agent_run_id=?`
	var cpu int
	if err := store.db.QueryRow(query, store.key.id(run)).Scan(&count, &cpu); err != nil || count != 1 || cpu != 10 {
		t.Fatalf("restored sample failed to join its original run: count=%d cpu=%d (%v)", count, cpu, err)
	}
}

func TestLegacyAssociationIntervalsStaySeparate(t *testing.T) {
	ctx := context.Background()
	rows := []struct{ kind, event, body string }{
		{"allocation", "object", `{"object_id":"object"}`},
		{"association", "start-a", `{"association_id":"interval A","object_id":"object","agent_run_id":"run","source":"validated-grant","start_time":"2026-01-01T00:00:00Z"}`},
		{"association", "end-a", `{"association_id":"interval A","object_id":"object","agent_run_id":"run","source":"validated-grant","end_time":"2026-01-01T01:00:00Z"}`},
		{"association", "start-b", `{"association_id":"interval B","object_id":"object","agent_run_id":"run","source":"validated-grant","start_time":"2026-01-01T02:00:00Z"}`},
		{"association", "start-c", `{"object_id":"object","agent_run_id":"run","source":"private source C","start_time":"2026-01-01T03:00:00Z"}`},
		{"association", "end-c", `{"object_id":"object","agent_run_id":"run","source":"private source C","end_time":"2026-01-01T04:00:00Z"}`},
		{"association", "start-d", `{"object_id":"object","agent_run_id":"run","source":"private source D","start_time":"2026-01-01T05:00:00Z"}`},
	}
	for _, input := range []string{"archive", "unsigned outbox"} {
		t.Run(input, func(t *testing.T) {
			destination := fixtureStore(t)
			if input == "archive" {
				archive := fixtureStore(t)
				for _, row := range rows {
					seedRetained(t, archive, Envelope{OriginID: "archive", SourceID: "legacy", SourceVersion: "1",
						EventID: row.event, Kind: row.kind, Encoding: "json", Body: json.RawMessage(row.body)})
				}
				if _, err := archive.db.Exec("PRAGMA user_version=2"); err != nil {
					t.Fatal(err)
				}
				var exported bytes.Buffer
				if _, err := Export(ctx, archive.db, &exported, 0, 10); err != nil {
					t.Fatal(err)
				}
				for range 2 {
					if err := destination.Replay(ctx, bytes.NewReader(exported.Bytes())); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				outbox := filepath.Join(destination.dir, "outbox")
				for i, row := range rows {
					body, err := decodeBody([]byte(row.body))
					if err != nil {
						t.Fatal(err)
					}
					data, err := json.Marshal(Record{SchemaVersion: 1, SourceID: "legacy",
						EventID: row.event, Kind: row.kind, Body: body})
					if err != nil {
						t.Fatal(err)
					}
					testkit.WriteFile(t, filepath.Join(outbox, strconv.Itoa(i)+".json"), data, 0600)
				}
				if err := destination.IngestOutbox(ctx, outbox); err != nil {
					t.Fatal(err)
				}
			}
			var count, closed int
			var history string
			if err := destination.db.QueryRow(`SELECT count(*),sum(end_time<>''),group_concat(associations.body)
					FROM associations JOIN objects USING(object_id) WHERE agent_run_id=?`, destination.key.id("run")).
				Scan(&count, &closed, &history); err != nil || count != 4 || closed != 2 ||
				strings.Contains(history, "interval") || strings.Contains(history, "private source") {
				t.Fatalf("legacy intervals were merged or exposed: count=%d closed=%d (%v)", count, closed, err)
			}
		})
	}
}

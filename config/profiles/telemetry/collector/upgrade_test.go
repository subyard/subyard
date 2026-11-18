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

func appendPython(t *testing.T, store *Store, input []Record) {
	t.Helper()
	const producer = `
import json, pathlib, sys
sys.path.insert(0, '..')
import records
root = pathlib.Path(sys.argv[1])
for row in json.load(sys.stdin):
    assert records.emit(root, row['kind'], row['body'], source_id=row['source_id'],
                        source_time=row['source_time'], event_id=row['event_id'])
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
}

func TestPythonSpoolKeepsCaptureAcrossPolicyChanges(t *testing.T) {
	p := Policy{Schema: PolicyVersion, Attributes: []string{"session.id"}}
	wide := DefaultPolicy()
	for _, change := range []struct{ before, after Policy }{{p, wide}, {wide, p}} {
		t.Run(policyFingerprint(change.before), func(t *testing.T) {
			ctx := context.Background()
			store := fixtureStore(t)
			hexID := strings.Repeat("a", 64)
			digestID := "h1:" + hexID
			input := record("relation", map[string]any{"session_id": hexID, "agent_run_id": digestID,
				"relation": "native_session", "source": "native-otlp-attribute/authenticated-header",
				"session_attributes": []string{"session.id", "thread.id"}})
			if err := store.publishCapturePolicy(change.before); err != nil {
				t.Fatal(err)
			}
			appendPython(t, store, []Record{input})
			files, err := filepath.Glob(filepath.Join(store.dir, "spool", "*.open"))
			if err != nil || len(files) != 1 {
				t.Fatalf("spool: %v (%v)", files, err)
			}
			raw, err := os.ReadFile(files[0])
			if err != nil || bytes.Contains(raw, []byte(hexID)) || strings.Contains(files[0], hexID) {
				t.Fatalf("caller ID reached spool: %v", err)
			}
			if err := store.publishCapturePolicy(change.after); err != nil {
				t.Fatal(err)
			}
			if err := store.drainSpool(ctx, change.after); err != nil {
				t.Fatal(err)
			}
			appendPython(t, store, []Record{input}) // Same stable input after a policy change.
			if err := store.drainSpool(ctx, change.after); err != nil {
				t.Fatal(err)
			}
			var count int
			var kind, claim, body string
			if err := store.db.QueryRow("SELECT count(*),min(kind),min(policy_version),min(body) FROM records").
				Scan(&count, &kind, &claim, &body); err != nil || count != 1 || kind != "session" ||
				claim != policyFingerprint(change.before) || strings.Contains(body, "agent_run_id") ||
				strings.Contains(body, "thread.id") || !strings.Contains(body, store.key.id(hexID)) {
				t.Fatalf("capture was changed by retry: count=%d kind=%q claim=%q (%v)", count, kind, claim, err)
			}
			var export bytes.Buffer
			if _, err := Export(ctx, store.db, &export, 0, 10); err != nil {
				t.Fatal(err)
			}
			replay := fixtureStore(t)
			for range 2 {
				if err := replay.Replay(ctx, bytes.NewReader(export.Bytes())); err != nil {
					t.Fatal(err)
				}
			}
			if err := replay.db.QueryRow("SELECT count(*),min(body) FROM records").Scan(&count, &body); err != nil ||
				count != 1 || strings.Contains(body, "agent_run_id") || strings.Contains(body, "thread.id") {
				t.Fatalf("replay reconstructed missing fields: %d %q (%v)", count, body, err)
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
				if err := store.publishCapturePolicy(p); err != nil {
					t.Fatal(err)
				}
				appendPython(t, store, input)
				if err := store.drainSpool(ctx, DefaultPolicy()); err != nil {
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
				outbox := legacyOutboxFixture(t, destination)
				for _, row := range rows {
					body, err := decodeBody([]byte(row.body))
					if err != nil {
						t.Fatal(err)
					}
					data, err := json.Marshal(Record{SchemaVersion: 1, SourceID: "legacy",
						EventID: row.event, Kind: row.kind, Body: body})
					if err != nil {
						t.Fatal(err)
					}
					testkit.WriteFile(t, filepath.Join(outbox, row.event+".json"), data, 0600)
				}
				if err := importLegacyFixture(t, destination, DefaultPolicy()); err != nil {
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

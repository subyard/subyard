package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
)

func TestReplayEmptyOTLPExport(t *testing.T) {
	ctx := context.Background()
	source, destination := fixtureStore(t), fixtureStore(t)
	const input = `{"schema":"subyard.telemetry.export/v1","origin_id":"fixture","event_id":"empty",
		"source_id":"native-otlp","source_version":"1","kind":"otlp/logs","encoding":"otlp/logs","protobuf":""}`
	line := strings.ReplaceAll(input, "\n", "")
	if err := source.Replay(ctx, strings.NewReader(line)); err != nil {
		t.Fatal(err)
	}
	for _, row := range []string{line, strings.Replace(line, `,"protobuf":""`, "", 1)} {
		if err := source.Replay(ctx, strings.NewReader(row)); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	if _, err := Export(ctx, source.db, &output, 0, 1); err != nil {
		t.Fatal(err)
	}
	if err := destination.Replay(ctx, &output); err != nil {
		t.Fatal(err)
	}
	var size int
	if err := destination.db.QueryRow("SELECT coalesce(length(protobuf),0) FROM records WHERE kind='otlp/logs'").Scan(&size); err != nil || size != 0 {
		t.Fatalf("round-tripped empty OTLP message: bytes=%d (%v)", size, err)
	}
}

func TestReplayFirstAcceptedPolicyWins(t *testing.T) {
	ctx := context.Background()
	store := fixtureStore(t)
	logs := plogotlp.NewExportRequest()
	entry := logs.Logs().ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	entry.Attributes().PutStr("session.id", "fixture-session")
	consumer, _, _, err := store.Consumers(DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if err = consumer.ConsumeLogs(ctx, logs.Logs()); err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.Exec("UPDATE records SET received_time=?", unknownSourceTime); err != nil {
		t.Fatal(err)
	}
	var local bytes.Buffer
	if _, err = Export(ctx, store.db, &local, 0, 1); err != nil {
		t.Fatal(err)
	}
	var original Envelope
	if err = json.Unmarshal(local.Bytes(), &original); err != nil || !bytes.Contains(original.Protobuf, []byte("session.id")) {
		t.Fatalf("fixture did not retain the optional attribute (%v)", err)
	}
	if err = store.prune(); err != nil {
		t.Fatal(err)
	}
	narrow := DefaultPolicy()
	narrow.Attributes = nil
	if err = store.replay(ctx, bytes.NewReader(local.Bytes()), narrow); err != nil {
		t.Fatal(err)
	}
	if err = store.Replay(ctx, &local); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err = store.db.QueryRow("SELECT protobuf FROM records WHERE kind='otlp/logs'").Scan(&raw); err != nil ||
		bytes.Contains(raw, []byte("session.id")) {
		t.Fatalf("retry altered the restored payload (%v)", err)
	}
}

func TestReplayUsesQueueBoundForLargeRows(t *testing.T) {
	ctx := context.Background()
	store := fixtureStore(t)
	logs := plogotlp.NewExportRequest()
	records := logs.Logs().ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords()
	records.EnsureCapacity(300)
	for range 300 {
		records.AppendEmpty().Attributes().PutStr("session.id", "s")
	}
	raw, err := logs.MarshalProto()
	if err != nil {
		t.Fatal(err)
	}
	store.limits.RequestBytes = 1024
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
		t.Fatalf("replay did not retain the large record and its successor: %d %d (%v)", count, size, err)
	}
	stream.Reset()
	if _, err := Export(ctx, store.db, &stream, 0, 100); err != nil {
		t.Fatal(err)
	}
	if err := fixtureStore(t).Replay(ctx, &stream); err != nil {
		t.Fatal("large row is not portable", err)
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
	if err := RunCLI([]string{"replay", "--data-dir", dest, "--input", input}, nil); err != nil {
		t.Fatal("unchanged export after widening the policy", err)
	}
	changed := bytes.ReplaceAll(exported.Bytes(), []byte(`"mixed"`), []byte(`"changed"`))
	if bytes.Equal(changed, exported.Bytes()) {
		t.Fatal("fixture did not change the exported session ID")
	}
	testkit.WriteFile(t, input, changed, 0600)
	if err := RunCLI([]string{"replay", "--data-dir", dest, "--input", input}, nil); err != nil {
		t.Fatal("modified duplicate stopped replay", err)
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
		strings.Contains(body, "thread.id") || strings.Contains(body, "agent_run_id") || strings.Contains(body, "authenticated-header") || !strings.Contains(body, `"mixed"`) {
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
	cmd := pythonCollectorCommand(t, producer, store.dir)
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
			const runID = "fixture-run"
			input := record("relation", map[string]any{"session_id": hexID, "agent_run_id": runID,
				"relation": "native_session", "source": "native-otlp-attribute/authenticated-header",
				"session_attributes": []string{"session.id", "thread.id"}})
			if err := store.publishCapturePolicy(context.Background(), change.before); err != nil {
				t.Fatal(err)
			}
			appendPython(t, store, []Record{input})
			files, err := filepath.Glob(filepath.Join(store.dir, "spool", "*.open"))
			if err != nil || len(files) != 1 {
				t.Fatalf("spool: %v (%v)", files, err)
			}
			raw, err := os.ReadFile(files[0])
			if err != nil || !bytes.Contains(raw, []byte(hexID)) || strings.Contains(files[0], runID) {
				t.Fatalf("raw session or generated spool name incorrect: %v", err)
			}
			wantStream := fixtureSpoolStream(runID)
			if policyFingerprint(change.before) == policyFingerprint(p) {
				if bytes.Contains(raw, []byte(runID)) {
					t.Fatal("filtered run binding reached spool contents")
				}
				wantStream = "broker"
			}
			if filepath.Base(files[0]) != wantStream+".open" || !bytes.HasPrefix(raw, []byte(spoolVersion+" "+wantStream+" ")) {
				t.Fatal("spool routing disagrees with capture policy")
			}
			if err := store.publishCapturePolicy(context.Background(), change.after); err != nil {
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
				claim != policyFingerprint(change.after) || strings.Contains(body, "agent_run_id") ||
				strings.Contains(body, "thread.id") || !strings.Contains(body, hexID) {
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
			if err := store.publishCapturePolicy(context.Background(), p); err != nil {
				t.Fatal(err)
			}
			const producer = `
import sys
sys.path.insert(0, '..')
import records
records.MAX_RECORDS = 0
assert not records.emit(sys.argv[1], 'relation', {'agent_run_id':sys.argv[2],
    'session_id':'session', 'session_attributes':['session.id']})
`
			if output, err := pythonCollectorCommand(t, producer, store.dir, runID).CombinedOutput(); err != nil {
				t.Fatalf("force producer admission failure: %v\n%s", err, output)
			}
			if err := store.drainSpool(ctx, p); err != nil {
				t.Fatal(err)
			}
			var run string
			if err := store.db.QueryRow("SELECT agent_run_id FROM gaps WHERE json_extract(body,'$.reason')='producer_spool_capacity'").Scan(&run); err != nil || run != "" {
				t.Fatalf("loss marker retained filtered run binding: %v", err)
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
				if err := store.publishCapturePolicy(context.Background(), p); err != nil {
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
				input[0].EventID: {"run", `{"agent_run_id":"` + "test-run" + `"}`},
				input[2].EventID: {"session", `{"session_attributes":["session.id"],"session_id":"` +
					"allowed" + `","source":"native-otlp-attribute"}`},
				input[3].EventID: {"run", `{"agent_run_id":"` + "other-run" + `"}`},
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
			retry := input[1]
			if transport == "replay" {
				retry.OriginID = "producer:" + retry.SourceID
			}
			if err := store.Ingest(ctx, []Record{retry}); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := store.db.QueryRow("SELECT count(*) FROM records WHERE event_id=?", retry.EventID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("wider policy resurrected a filtered event: %d (%v)", count, err)
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
	if err := store.Replay(ctx, bytes.NewReader(exported.Bytes())); err != nil {
		t.Fatal(err)
	}
	changed := bytes.ReplaceAll(exported.Bytes(), []byte(`"cpu_ns":10`), []byte(`"cpu_ns":90`))
	if err := store.Replay(ctx, bytes.NewReader(changed)); err != nil {
		t.Fatal(err)
	}
	query := `SELECT count(*),sum(sample_values.cpu_ns) FROM sample_values
		JOIN objects USING(origin_id,object_id) JOIN associations USING(origin_id,object_id)
		JOIN runs USING(origin_id,agent_run_id) WHERE runs.agent_run_id=?`
	var cpu int
	if err := store.db.QueryRow(query, run).Scan(&count, &cpu); err != nil || count != 1 || cpu != 10 {
		t.Fatalf("restored sample failed to join its original run: count=%d cpu=%d (%v)", count, cpu, err)
	}
}

func TestForeignSampleStillDeduplicatesAfterLocalRestore(t *testing.T) {
	ctx := context.Background()
	source, destination := fixtureStore(t), fixtureStore(t)
	sample := record("sample", map[string]any{"object_id": "scope", "cpu_ns": 10})
	if err := source.Ingest(ctx, []Record{sample, record("gap", map[string]any{"reason": "source_offline"})}); err != nil {
		t.Fatal(err)
	}
	backup := testkit.TempDir(t)
	if _, err := source.db.Exec("VACUUM INTO ?", filepath.Join(backup, databaseName)); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReadOnly(backup)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var foreign, local bytes.Buffer
	if _, err := Export(ctx, reader, &foreign, 0, 10); err != nil {
		t.Fatal(err)
	}
	first, following, _ := bytes.Cut(foreign.Bytes(), []byte("\n"))
	for range 2 {
		if err := destination.Replay(ctx, bytes.NewReader(first)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := destination.db.Exec("UPDATE records SET received_time=?", unknownSourceTime); err != nil {
		t.Fatal(err)
	}
	if _, err := Export(ctx, destination.db, &local, 0, 1); err != nil {
		t.Fatal(err)
	}
	if err := destination.prune(); err != nil {
		t.Fatal(err)
	}
	if err := destination.Replay(ctx, &local); err != nil {
		t.Fatal(err)
	}
	changed := bytes.Replace(first, []byte(`"cpu_ns":10`), []byte(`"cpu_ns":20`), 1)
	if bytes.Equal(changed, first) {
		t.Fatal("fixture did not change the sample")
	}
	if err := destination.Replay(ctx, bytes.NewReader(bytes.Join([][]byte{changed, following}, []byte("\n")))); err != nil {
		t.Fatal(err)
	}
	if err := destination.Replay(ctx, &foreign); err != nil {
		t.Fatal(err)
	}
	var samples, cpu, gaps int
	if err := destination.db.QueryRow("SELECT (SELECT count(*) FROM samples),(SELECT sum(cpu_ns) FROM sample_values),(SELECT count(*) FROM gaps)").
		Scan(&samples, &cpu, &gaps); err != nil || samples != 1 || cpu != 10 || gaps != 1 {
		t.Fatalf("restored samples=%d, cpu=%d, subsequent gaps=%d (%v)", samples, cpu, gaps, err)
	}
}

func TestShowRunScopesReplayedMeasurementsByOrigin(t *testing.T) {
	ctx := context.Background()
	source, destination := fixtureStore(t), fixtureStore(t)
	for store, cpu := range map[*Store]int{source: 10, destination: 20} {
		if err := store.Ingest(ctx, []Record{
			record("run", map[string]any{"agent_run_id": "same-run"}),
			record("scope", map[string]any{"object_id": "same-object", "agent_run_id": "same-run", "quality": "explicit"}),
			record("sample", map[string]any{"object_id": "same-object", "cpu_ns": cpu}),
		}); err != nil {
			t.Fatal(err)
		}
	}
	var exported bytes.Buffer
	if _, err := Export(ctx, source.db, &exported, 0, 10); err != nil {
		t.Fatal(err)
	}
	if err := destination.Replay(ctx, &exported); err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct{ origin, cpu string }{{"", "20"}, {source.OriginID(), "10"}} {
		report, err := showRunOrigin(ctx, destination.db, "same-run", want.origin, false)
		if err != nil {
			t.Fatal(err)
		}
		objects := report["selected_nonoverlapping"].([]any)
		if len(objects) != 1 {
			t.Fatalf("origin %q has %d objects", want.origin, len(objects))
		}
		samples := objects[0].(map[string]any)["samples"].([]any)
		if len(samples) != 1 || samples[0].(map[string]any)["cpu_ns"] != json.Number(want.cpu) {
			t.Fatalf("origin %q selected the wrong samples: %v", want.origin, samples)
		}
	}
}

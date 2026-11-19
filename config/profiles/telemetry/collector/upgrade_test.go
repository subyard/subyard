package collector

import (
	"bytes"
	"context"
	"encoding/json"
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
	var count int64
	var stored []byte
	if err := store.db.QueryRow("SELECT count(*),max(protobuf) FROM records").Scan(&count, &stored); err != nil {
		t.Fatal(err)
	}
	if stored, err = decompressOTLP(stored); err != nil || count != 2 || int64(len(stored)) <= store.limits.RequestBytes {
		t.Fatalf("replay did not retain the large record and its successor: %d %d (%v)", count, len(stored), err)
	}
	stream.Reset()
	if _, err := Export(ctx, store.db, &stream, 0, 100); err != nil {
		t.Fatal(err)
	}
	if err := fixtureStore(t).Replay(ctx, &stream); err != nil {
		t.Fatal("large row is not portable", err)
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
	if err := prune(store); err != nil {
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
	if err := prune(destination); err != nil {
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

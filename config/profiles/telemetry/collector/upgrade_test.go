package collector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
)

func TestProducerRetryAndAttributionAfterUpgrade(t *testing.T) {
	const run = "01234567-abcd-4321-88aa-0123456789ab"
	const object = "scope/path"
	const source = "resource-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const at = "2026-01-01T00:00:00.000000000Z"
	const end = "2026-01-01T00:00:07.000000000Z"
	ctx := context.Background()
	for _, encodedByOldProducer := range []bool{false, true} {
		t.Run(fmt.Sprint(encodedByOldProducer), func(t *testing.T) {
			dir := testkit.TempDir(t)
			old, err := Open(dir, DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			legacyObject, legacySample := object, "sample/first"
			if encodedByOldProducer {
				legacyObject, legacySample = old.key.legacy(object), old.key.legacy(legacySample)
			}
			origin := "producer:" + source
			edge := origin + ":" + legacyObject + ":" + run + ":managed_scope_membership"
			if encodedByOldProducer {
				edge = fmt.Sprintf("association:%x", sha256.Sum256([]byte(strings.Join(
					[]string{origin, legacyObject, run, "managed_scope_membership"}, "\x00"))))
			}
			runBody := fmt.Sprintf(`{"agent_run_id":%q}`, run)
			objectBody := fmt.Sprintf(`{"object_id":%q,"object_kind":"process","accounting":"direct","historical_note":"retained"}`, legacyObject)
			sampleBody := fmt.Sprintf(`{"object_id":%q,"cpu_ns":"+0000000000000000000000007"}`, legacyObject)
			edgeBody := fmt.Sprintf(`{"object_id":%q,"agent_run_id":%q,"source":"managed_scope_membership","start_time":%q,"quality":"explicit"}`, legacyObject, run, at)
			for _, item := range []struct{ kind, event, body string }{
				{"run", "run-start", runBody}, {"process", "declare", objectBody},
				{"sample", legacySample, sampleBody}, {"association", "open-edge", edgeBody},
			} {
				seedRetained(t, old, Envelope{OriginID: origin, EventID: item.event, SourceID: source, SourceVersion: "1",
					SourceTime: at, Kind: item.kind, Encoding: "json", PolicyVersion: PolicyVersion, Body: json.RawMessage(item.body)})
			}
			for _, row := range []struct {
				statement string
				args      []any
			}{
				{"INSERT INTO runs VALUES(?,?,?)", []any{run, runBody, at}},
				{"INSERT INTO objects VALUES(?,?,NULL,?,?,?)", []any{legacyObject, "process", "direct", objectBody, at}},
				{"INSERT INTO samples VALUES(?,?,?,?,?)", []any{origin, legacySample, legacyObject, at, sampleBody}},
				{"INSERT INTO associations VALUES(?,?,?,?,?,'',0,?)", []any{edge, legacyObject, run, "explicit", at, edgeBody}},
				{"PRAGMA user_version=2", nil},
			} {
				if _, err := old.db.Exec(row.statement, row.args...); err != nil {
					t.Fatal(err)
				}
			}
			if err := old.Close(); err != nil {
				t.Fatal(err)
			}
			store, err := Open(dir, DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()

			const producer = `
import sys
sys.path.insert(0, '..')
import records
root, source, run, obj, end = sys.argv[1:]
assert records.emit(root, 'sample', dict(object_id=obj, cpu_ns=7), source_id=source, event_id='sample/first')
assert records.emit(root, 'sample', dict(object_id=obj, cpu_ns=12), source_id=source, event_id='sample/next', source_time=end)
assert records.emit(root, 'association', dict(object_id=obj, agent_run_id=run, source='managed_scope_membership',
    end_time=end, quality='explicit'), source_id=source, event_id='close-edge', source_time=end)
`
			cmd := exec.Command("python3", "-B", "-c", producer, dir, source, run, object, end)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("upgraded producer: %v\n%s", err, output)
			}
			outbox := filepath.Join(dir, "outbox")
			if err := store.IngestOutbox(ctx, outbox); err != nil {
				t.Fatal(err)
			}
			if files, err := os.ReadDir(outbox); err != nil || len(files) != 1 || files[0].Name() != ".lock" {
				t.Fatalf("retry was not acknowledged: %v (%v)", files, err)
			}
			for _, target := range []*Store{store, fixtureStore(t)} {
				id := store.key.id(run)
				if target != store {
					var stream bytes.Buffer
					if _, err := Export(ctx, store.db, &stream, 0, 100); err != nil {
						t.Fatal(err)
					}
					if !bytes.Contains(stream.Bytes(), []byte(`"historical_note":"retained"`)) {
						t.Fatal("migration rewrote unrelated historical metadata")
					}
					if err := target.Replay(ctx, &stream); err != nil {
						t.Fatal(err)
					}
					id = target.key.id(id)
				}
				var count, total int
				var from, to string
				err := target.db.QueryRow(`SELECT count(*),sum(CAST(json_extract(samples.body,'$.cpu_ns') AS INTEGER)),
				       min(associations.start_time),max(associations.end_time)
				       FROM samples JOIN objects USING(object_id) JOIN associations USING(object_id)
				       WHERE associations.agent_run_id=?`, id).Scan(&count, &total, &from, &to)
				if err != nil || count != 2 || total != 19 || from != at || to != end {
					t.Fatalf("history split or retry duplicated: count=%d cpu=%d from=%s to=%s err=%v", count, total, from, to, err)
				}
			}
		})
	}
}

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
	if err := db.QueryRow("SELECT count(*),sum(agent_run_id<>'') FROM relations").Scan(&found, &attached); err != nil || found != 1 || attached != 0 {
		t.Fatalf("replayed relation bypassed the selected policy: count=%d attached=%d (%v)", found, attached, err)
	}
	var raw []byte
	var applied string
	if err := db.QueryRow("SELECT protobuf,import_policy_version FROM records WHERE encoding='otlp/logs'").Scan(&raw, &applied); err != nil || applied != policyFingerprint(p) {
		t.Fatalf("replay did not apply the selected policy: %s (%v)", applied, err)
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

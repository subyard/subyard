package collector

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
)

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

// Every ingest path applies the selected capture policy before storage, and
// the first accepted row of an (origin_id, event_id) pair wins over a later
// delivery under another policy.
func TestCapturePolicyAcrossIngestPaths(t *testing.T) {
	ctx := context.Background()
	narrow := Policy{Schema: PolicyVersion, Attributes: []string{"session.id"}}
	wide := DefaultPolicy()
	const run = "fixture-run"
	session := strings.Repeat("a", 64) // A hex session ID stays a raw identifier.
	input := []Record{
		record("run", map[string]any{"agent_run_id": run, "session_id": "excluded", "session_attributes": []string{"thread.id"}}),
		record("relation", map[string]any{"agent_run_id": run, "session_id": session, "relation": "native_session",
			"source": "native-otlp-attribute/authenticated-header", "session_attributes": []string{"session.id", "thread.id"}}),
		record("relation", map[string]any{"agent_run_id": run, "native_session_id": "thread-only", "session_attributes": []string{"thread.id"}}),
	}
	otlp := func() plogotlp.ExportRequest {
		logs := plogotlp.NewExportRequest()
		resource := logs.Logs().ResourceLogs().AppendEmpty()
		resource.Resource().Attributes().PutStr("subyard.agent_run.id", run)
		signal := resource.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
		signal.Attributes().PutStr("session.id", "otlp-session")
		signal.Attributes().PutStr("thread.id", "otlp-thread")
		return logs
	}
	live := func(t *testing.T, s *Store, p Policy, jsonOnly bool) {
		if err := s.ingest(ctx, input, p); err != nil {
			t.Fatal(err)
		}
		if jsonOnly {
			return // Native OTLP has no upstream event ID, so only JSON retries dedupe.
		}
		logs, _, _, err := s.Consumers(p)
		if err != nil {
			t.Fatal(err)
		}
		if err = logs.ConsumeLogs(ctx, otlp().Logs()); err != nil {
			t.Fatal(err)
		}
	}
	type delivery func(t *testing.T, p Policy, retry bool) *sql.DB
	paths := map[string]func(t *testing.T) delivery{
		"live": func(t *testing.T) delivery {
			s := fixtureStore(t)
			return func(t *testing.T, p Policy, retry bool) *sql.DB {
				live(t, s, p, retry)
				return s.db
			}
		},
		"python spool": func(t *testing.T) delivery {
			s := fixtureStore(t)
			return func(t *testing.T, p Policy, retry bool) *sql.DB {
				if err := s.publishCapturePolicy(ctx, p); err != nil {
					t.Fatal(err)
				}
				appendPython(t, s, input)
				files, err := filepath.Glob(filepath.Join(s.dir, "spool", "*.open"))
				streams := map[string][]byte{}
				for _, file := range files {
					if streams[filepath.Base(file)], err = os.ReadFile(file); err != nil {
						t.Fatal(err)
					}
				}
				broker, routed := streams["broker.open"], streams[fixtureSpoolStream(run)+".open"]
				narrowed := policyFingerprint(p) == policyFingerprint(narrow)
				// Generated stream names; a filtered run binding cannot route a frame.
				if !retry && (err != nil || len(routed) == 0 || len(streams) != map[bool]int{true: 2, false: 1}[narrowed] ||
					!bytes.Contains(append(broker, routed...), []byte(session)) || bytes.Contains(broker, []byte(run))) {
					t.Fatalf("spool routing disagrees with capture policy: %v (%v)", files, err)
				}
				if err := s.drainSpool(ctx, wide); err != nil {
					t.Fatal(err)
				}
				return s.db
			}
		},
		"replay": func(t *testing.T) delivery {
			source := fixtureStore(t)
			live(t, source, wide, false)
			var exported bytes.Buffer
			if _, err := Export(ctx, source.db, &exported, 0, 100); err != nil {
				t.Fatal(err)
			}
			// Import metadata is untrusted input and cannot authorize capture.
			untrusted := bytes.ReplaceAll(exported.Bytes(), []byte(`"schema":`), []byte(`"import_policy_version":"PRIVATE-untrusted","schema":`))
			temp, dest := testkit.TempDir(t), testkit.TempDir(t)
			var db *sql.DB
			return func(t *testing.T, p Policy, retry bool) *sql.DB {
				config, _ := json.Marshal(p)
				policyFile, file := filepath.Join(temp, "policy.json"), filepath.Join(temp, "input.jsonl")
				testkit.WriteFile(t, policyFile, config, 0600)
				data := untrusted
				if retry { // A changed duplicate is skipped, not an error.
					data = bytes.ReplaceAll(untrusted, []byte(session), []byte(strings.Repeat("b", 64)))
				}
				testkit.WriteFile(t, file, data, 0600)
				if err := RunCLI([]string{"replay", "--data-dir", dest, "--input", file, "--policy-file", policyFile}, nil); err != nil {
					t.Fatal(err)
				}
				if db == nil {
					var err error
					if db, err = OpenReadOnly(dest); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { db.Close() })
				}
				var imported, identities int
				if err := db.QueryRow(`SELECT sum(import_policy_version=?),
					count(DISTINCT origin_id||'/'||event_id) FROM records`, policyFingerprint(p)).Scan(&imported, &identities); err != nil {
					t.Fatal(err)
				}
				// Each replayed row keeps its identity and the capture claim it was exported with.
				claims := map[string]string{}
				for i, rows := range [][]byte{dump(t, source.db), dump(t, db)} {
					for _, line := range strings.Split(strings.TrimSpace(string(rows)), "\n") {
						if f := strings.Fields(line); i == 0 {
							claims[f[0]+"/"+f[1]] = f[3]
						} else if claims[f[0]+"/"+f[1]] != f[3] {
							t.Fatalf("replay changed identity or capture claim: %s %s", f[0], f[1])
						}
					}
				}
				var original int
				source.db.QueryRow("SELECT count(*) FROM records").Scan(&original)
				// Narrow import leaves wholly filtered events as pair receipts only.
				complete := policyFingerprint(p) == policyFingerprint(wide)
				if !retry && (imported != identities || (identities == original) != complete || bytes.Contains(dump(t, db), []byte("PRIVATE"))) {
					t.Fatalf("replay lost identities or trusted import metadata: imported=%d identities=%d source=%d", imported, identities, original)
				}
				return db
			}
		},
	}
	for name, path := range paths {
		for _, order := range [][2]Policy{{narrow, wide}, {wide, narrow}} {
			first := map[bool]string{true: "narrow", false: "wide"}[policyFingerprint(order[0]) == policyFingerprint(narrow)]
			t.Run(name+"/"+first, func(t *testing.T) {
				deliver := path(t)
				db := deliver(t, order[0], false)
				bodies := string(dump(t, db))
				var attached int
				db.QueryRow("SELECT count(*) FROM relations WHERE agent_run_id<>''").Scan(&attached)
				if !strings.Contains(bodies, session) || !strings.Contains(bodies, "session.id") {
					t.Fatal("allowed raw session fields lost")
				}
				if first == "narrow" && (attached != 0 || strings.Contains(bodies, "thread") || strings.Contains(bodies, "excluded") ||
					strings.Contains(bodies, "authenticated-header") || strings.Contains(bodies, "subyard.agent_run.id")) {
					t.Fatalf("narrow capture leaked excluded fields: attached=%d %s", attached, bodies)
				}
				if first == "wide" && (attached == 0 || !strings.Contains(bodies, "thread.id")) {
					t.Fatalf("wide capture dropped allowed fields: attached=%d %s", attached, bodies)
				}
				before := dump(t, db)
				if deliver(t, order[1], true); !bytes.Equal(dump(t, db), before) {
					t.Fatal("a later delivery under another policy replaced the first accepted rows")
				}
			})
		}
	}
	t.Run("python spool/narrow loss marker", func(t *testing.T) {
		s := fixtureStore(t)
		if err := s.publishCapturePolicy(ctx, narrow); err != nil {
			t.Fatal(err)
		}
		const producer = `
import sys
sys.path.insert(0, '..')
import records
records.MAX_RECORDS = 0
assert not records.emit(sys.argv[1], 'relation', {'agent_run_id':sys.argv[2], 'session_id':'session', 'session_attributes':['session.id']})
`
		if output, err := pythonCollectorCommand(t, producer, s.dir, run).CombinedOutput(); err != nil {
			t.Fatalf("force producer admission failure: %v\n%s", err, output)
		}
		if err := s.drainSpool(ctx, narrow); err != nil {
			t.Fatal(err)
		}
		var bound string
		if err := s.db.QueryRow("SELECT agent_run_id FROM gaps WHERE json_extract(body,'$.reason')='producer_spool_capacity'").Scan(&bound); err != nil || bound != "" {
			t.Fatalf("loss marker retained filtered run binding: %q (%v)", bound, err)
		}
	})
	private := narrow
	private.Schema = strings.Repeat("ab", 32)
	if _, _, _, err := fixtureStore(t).Consumers(private); err == nil || strings.Contains(policyFingerprint(private), private.Schema) {
		t.Fatal("programmatic policy admitted a caller-supplied schema")
	}
}

// dump returns one line per stored row in a stable order; OTLP keys stay readable.
func dump(t *testing.T, db *sql.DB) []byte {
	t.Helper()
	rows, err := db.Query(`SELECT origin_id||' '||event_id||' '||kind||' '||policy_version||' '||import_policy_version||' '||
		coalesce(body,'')||replace(CAST(coalesce(protobuf,'') AS TEXT),char(10),' ') FROM records ORDER BY origin_id,event_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out bytes.Buffer
	for rows.Next() {
		var row []byte
		if err = rows.Scan(&row); err != nil {
			t.Fatal(err)
		}
		out.Write(append(row, '\n'))
	}
	return out.Bytes()
}

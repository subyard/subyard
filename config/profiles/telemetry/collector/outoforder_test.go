package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestUnorderedLifecycleProjection(t *testing.T) {
	ctx := context.Background()
	initialTime := "2026-10-01T01:00:00Z"
	finalTime := "2026-10-01T02:00:00Z"
	makeRecord := func(kind, at string, body map[string]any) Record {
		r := record(kind, body)
		r.SourceTime = at
		return r
	}
	initial := []Record{
		makeRecord("run", initialTime, map[string]any{"agent_run_id": "unordered", "state": "starting", "agent": "other", "agent_version": "1.2.3", "project_id": "project", "session_id": "session", "parent_run_id": "parent"}),
		makeRecord("allocation", initialTime, map[string]any{"object_id": "vm", "state": "held", "parent_object_id": "owner", "accounting": "qemu-process-and-threads", "allocated_cpu": 4, "allocated_memory_bytes": 4096, "allocated_disk_bytes": 8192, "acquired_at": initialTime}),
		makeRecord("process", initialTime, map[string]any{"object_id": "process", "state": "running", "parent_object_id": "old-parent", "accounting": "inclusive", "pid": 123}),
	}
	final := []Record{
		makeRecord("run", finalTime, map[string]any{"agent_run_id": "unordered", "state": "resources_complete", "exit_code": 7}),
		makeRecord("allocation", finalTime, map[string]any{"object_id": "vm", "state": "ended", "stopped_at": finalTime}),
		makeRecord("process", finalTime, map[string]any{"object_id": "process", "state": "ended", "parent_object_id": "new-parent", "accounting": "direct"}),
	}
	for _, reverse := range []bool{false, true} {
		for _, transport := range []string{"ingest", "spool", "replay"} {
			t.Run(fmt.Sprintf("%s/final_first=%t", transport, reverse), func(t *testing.T) {
				s := fixtureStore(t)
				records := append(append([]Record{}, initial...), final...)
				if reverse {
					records = append(append([]Record{}, final...), initial...)
				}
				switch transport {
				case "ingest":
					if err := s.Ingest(ctx, records); err != nil {
						t.Fatal(err)
					}
				case "spool":
					p := DefaultPolicy()
					if err := s.publishCapturePolicy(p); err != nil {
						t.Fatal(err)
					}
					appendPython(t, s, records)
					if err := s.drainSpool(ctx, p); err != nil {
						t.Fatal(err)
					}
				case "replay":
					var stream bytes.Buffer
					for _, r := range records {
						body, _ := json.Marshal(r.Body)
						e := Envelope{Schema: ExportSchema, OriginID: "producer:" + fixtureSourceID, EventID: r.EventID, SourceID: r.SourceID, SourceVersion: r.SourceVersion, SourceTime: r.SourceTime, Kind: r.Kind, Encoding: "json", Body: body}
						if err := json.NewEncoder(&stream).Encode(e); err != nil {
							t.Fatal(err)
						}
					}
					for i := 0; i < 2; i++ {
						if err := s.Replay(ctx, bytes.NewReader(stream.Bytes())); err != nil {
							t.Fatal(err)
						}
					}
				}
				check := func(query, id string, expected map[string]string) {
					t.Helper()
					var raw, at string
					if err := s.db.QueryRow(query, s.key.id(id)).Scan(&raw, &at); err != nil {
						t.Fatal(err)
					}
					body, err := decodeBody([]byte(raw))
					if err != nil {
						t.Fatal(err)
					}
					for k, want := range expected {
						if fmt.Sprint(body[k]) != want {
							t.Errorf("%s: %s=%v, want %s", query, k, body[k], want)
						}
					}
					if at != "2026-10-01T02:00:00.000000000Z" {
						t.Errorf("source time rolled back: %s", at)
					}
				}
				check("SELECT body,source_time FROM runs WHERE agent_run_id=?", "unordered", map[string]string{"state": "resources_complete", "agent": "other", "agent_version": "1.2.3", "project_id": s.key.id("project"), "session_id": s.key.id("session"), "parent_run_id": s.key.id("parent"), "exit_code": "7"})
				allocation := map[string]string{"state": "ended", "allocated_cpu": "4", "allocated_memory_bytes": "4096", "allocated_disk_bytes": "8192", "acquired_at": "2026-10-01T01:00:00.000000000Z", "stopped_at": "2026-10-01T02:00:00.000000000Z"}
				check("SELECT body,source_time FROM allocations WHERE object_id=?", "vm", allocation)
				check("SELECT body,source_time FROM objects WHERE object_id=?", "vm", allocation)
				check("SELECT body,source_time FROM objects WHERE object_id=?", "process", map[string]string{"state": "ended", "pid": "123", "parent_object_id": s.key.id("new-parent"), "accounting": "direct"})
				for object, want := range map[string][2]string{"vm": {"owner", "qemu-process-and-threads"}, "process": {"new-parent", "direct"}} {
					var parent, accounting string
					if err := s.db.QueryRow("SELECT parent_object_id,accounting FROM objects WHERE object_id=?", s.key.id(object)).Scan(&parent, &accounting); err != nil || parent != s.key.id(want[0]) || accounting != want[1] {
						t.Errorf("typed %s: parent=%q accounting=%q err=%v", object, parent, accounting, err)
					}
				}
				var count int
				if err := s.db.QueryRow("SELECT count(*) FROM records").Scan(&count); err != nil || count != 6 {
					t.Fatalf("raw identities changed: %d %v", count, err)
				}
			})
		}
	}
}

func TestLegacyTimestampRetry(t *testing.T) {
	for _, original := range []string{"malformed legacy timestamp", "2026-01-01T00:00:00Z"} {
		t.Run(original, func(t *testing.T) {
			ctx := context.Background()
			store := fixtureStore(t)
			r := record("gap", map[string]any{"reason": "source_offline"})
			for _, at := range []string{original, unknownSourceTime} {
				r.SourceTime = at
				if err := store.Ingest(ctx, []Record{r}); err != nil {
					t.Fatal(err)
				}
			}
			var count int
			var at string
			want := canonicalTimestamp(original)
			if want == "" {
				want = unknownSourceTime
			}
			if err := store.db.QueryRow("SELECT count(*), min(source_time) FROM records").Scan(&count, &at); err != nil || count != 1 || at != want {
				t.Fatalf("outbox retry count=%d, time=%s, err=%v", count, at, err)
			}
			r.Body["reason"] = "final_tail_missing"
			if err := store.Ingest(ctx, []Record{r}); !errors.Is(err, errEventCollision) {
				t.Fatalf("retry with changed body should collide: %v", err)
			}
		})
	}
}

func TestSpoolRecoveryAcrossCommitAndUnlink(t *testing.T) {
	for _, unlinked := range []bool{false, true} {
		t.Run(fmt.Sprintf("unlinked=%t", unlinked), func(t *testing.T) {
			ctx := context.Background()
			s := fixtureStore(t)
			p := DefaultPolicy()
			if err := s.publishCapturePolicy(p); err != nil {
				t.Fatal(err)
			}
			first := record("gap", map[string]any{"reason": "source_offline"})
			appendPython(t, s, []Record{first})
			dir := filepath.Join(s.dir, "spool")
			path := filepath.Join(dir, "broker.open")
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.WriteString("{malformed}\n{torn"); err != nil {
				t.Fatal(err)
			}
			f.Close()
			if _, err = s.db.Exec(`CREATE TRIGGER interrupted BEFORE UPDATE ON spool_offsets
				BEGIN SELECT raise(ABORT,'before spool commit'); END`); err != nil {
				t.Fatal(err)
			}
			if err := s.drainSpool(ctx, p); !errors.Is(err, ErrStorage) {
				t.Fatalf("interrupted drain: %v", err)
			}
			assertCount := func(db *Store, count int) {
				t.Helper()
				var n int
				if err := db.db.QueryRow("SELECT count(*) FROM records").Scan(&n); err != nil || n != count {
					t.Fatalf("records = %d, want %d (%v)", n, count, err)
				}
			}
			assertCount(s, 0)
			if _, err := s.db.Exec("DROP TRIGGER interrupted"); err != nil {
				t.Fatal(err)
			}
			if err := s.drainSpool(ctx, p); err != nil {
				t.Fatal(err)
			}
			assertCount(s, 2) // Complete frames drain even while the tail is open.
			conflict := first
			conflict.Body = map[string]any{"reason": "final_tail_missing"}
			appendPython(t, s, []Record{first, conflict, record("gap", map[string]any{"reason": "source_offline"})})
			sealed, err := filepath.Glob(filepath.Join(dir, "*.sealed"))
			if err != nil || len(sealed) != 1 {
				t.Fatalf("torn segment did not rotate: %v (%v)", sealed, err)
			}
			file, err := os.Open(sealed[0])
			if err != nil {
				t.Fatal(err)
			}
			id := strings.TrimSuffix(filepath.Base(sealed[0]), ".sealed")
			err = s.drainSegment(ctx, file, id, true, p)
			file.Close()
			if err != nil {
				t.Fatal(err)
			}
			assertCount(s, 3)
			if unlinked {
				if err = os.Remove(sealed[0]); err != nil {
					t.Fatal(err)
				}
			}
			foreign := filepath.Join(dir, s.key.id("foreign")+".open")
			testkit.WriteFile(t, foreign, []byte("subyard-spool/99 fixture\n"), 0600)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			restarted, err := Open(s.dir, DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			if err = restarted.drainSpool(ctx, p); !errors.Is(err, errInvalidRecord) {
				t.Fatalf("unknown version was not reported: %v", err)
			}
			assertCount(restarted, 5)
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(foreign) {
				t.Fatalf("only foreign spool evidence should remain: %v (%v)", entries, err)
			}
			var checkpoints int
			if err := restarted.db.QueryRow("SELECT count(*) FROM spool_offsets").Scan(&checkpoints); err != nil || checkpoints != 0 {
				t.Fatalf("retired offsets remain: %d (%v)", checkpoints, err)
			}
			if unlinked {
				const producer = `
import sys
sys.path.insert(0, '..')
import records
records.MAX_RECORDS = 0
for _ in range(2):
    assert not records.emit(sys.argv[1], 'run', {'agent_run_id':'quota-lost'})
`
				if output, err := exec.Command("python3", "-B", "-c", producer, restarted.dir).CombinedOutput(); err != nil {
					t.Fatalf("write reserved loss marker: %v\n%s", err, output)
				}
				for range 2 {
					if err := restarted.drainSpool(ctx, p); !errors.Is(err, errInvalidRecord) {
						t.Fatalf("marker drain next to foreign stream: %v", err)
					}
				}
				var count, missing int
				if err := restarted.db.QueryRow(`SELECT count(*),sum(json_extract(body,'$.missing')) FROM gaps
					WHERE agent_run_id=? AND json_extract(body,'$.reason')='producer_spool_capacity'`, restarted.key.id("quota-lost")).
					Scan(&count, &missing); err != nil || count != 1 || missing != 2 {
					t.Fatalf("marker-only recovery: gaps=%d losses=%d (%v)", count, missing, err)
				}
			}
		})
	}
}

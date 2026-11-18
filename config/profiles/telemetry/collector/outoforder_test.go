package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
		for _, transport := range []string{"ingest", "outbox", "replay"} {
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
				case "outbox":
					dir := testkit.TempDir(t)
					for i, r := range records {
						path := filepath.Join(dir, fmt.Sprintf("%03d.json", i))
						if i == 0 {
							path = filepath.Join(dir, ".legacy.json")
						}
						data, err := json.Marshal(r)
						if err != nil {
							t.Fatal(err)
						}
						testkit.WriteFile(t, path, data, 0600)
					}
					testkit.WriteFile(t, filepath.Join(dir, ".in-progress.tmp"), nil, 0600)
					if err := s.IngestOutbox(ctx, dir); err != nil {
						t.Fatal(err)
					}
					if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 || entries[0].Name() != ".in-progress.tmp" {
						t.Fatalf("only the temporary file should remain: %v %v", entries, err)
					}
				case "replay":
					var stream bytes.Buffer
					for _, r := range records {
						body, _ := json.Marshal(r.Body)
						e := Envelope{Schema: ExportSchema, OriginID: "producer:fixture", EventID: r.EventID, SourceID: r.SourceID, SourceVersion: r.SourceVersion, SourceTime: r.SourceTime, Kind: r.Kind, Encoding: "json", Body: body}
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
				check := func(query string, expected map[string]string) {
					t.Helper()
					var raw, at string
					if err := s.db.QueryRow(query).Scan(&raw, &at); err != nil {
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
				check("SELECT body,source_time FROM runs WHERE agent_run_id='unordered'", map[string]string{"state": "resources_complete", "agent": "other", "agent_version": "1.2.3", "project_id": "project", "session_id": "session", "parent_run_id": "parent", "exit_code": "7"})
				allocation := map[string]string{"state": "ended", "allocated_cpu": "4", "allocated_memory_bytes": "4096", "allocated_disk_bytes": "8192", "acquired_at": "2026-10-01T01:00:00.000000000Z", "stopped_at": "2026-10-01T02:00:00.000000000Z"}
				check("SELECT body,source_time FROM allocations WHERE object_id='vm'", allocation)
				check("SELECT body,source_time FROM objects WHERE object_id='vm'", allocation)
				check("SELECT body,source_time FROM objects WHERE object_id='process'", map[string]string{"state": "ended", "pid": "123", "parent_object_id": "new-parent", "accounting": "direct"})
				for object, want := range map[string][2]string{"vm": {"owner", "qemu-process-and-threads"}, "process": {"new-parent", "direct"}} {
					var parent, accounting string
					if err := s.db.QueryRow("SELECT parent_object_id,accounting FROM objects WHERE object_id=?", object).Scan(&parent, &accounting); err != nil || parent != want[0] || accounting != want[1] {
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

func TestOutboxLegacyTimestampRetry(t *testing.T) {
	for _, original := range []string{"malformed legacy timestamp", "2026-01-01T00:00:00Z"} {
		t.Run(original, func(t *testing.T) {
			ctx := context.Background()
			store := fixtureStore(t)
			dir := testkit.TempDir(t)
			path := filepath.Join(dir, ".event.json")
			r := record("gap", map[string]any{"reason": "source_offline"})
			for _, at := range []string{original, unknownSourceTime} {
				r.SourceTime = at
				data, err := json.Marshal(r)
				if err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, data, 0600)
				if err := store.IngestOutbox(ctx, dir); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("outbox record remains after drain: %v", err)
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

func TestOutboxContinuesPastInvalidRecords(t *testing.T) {
	ctx := context.Background()
	store := fixtureStore(t)
	dir := testkit.TempDir(t)
	bad := record("sample", map[string]any{"object_id": "legacy", "monotonic_ns": -1})
	collision := record("gap", map[string]any{"reason": "source_offline"})
	if err := store.Ingest(ctx, []Record{collision}); err != nil {
		t.Fatal(err)
	}
	collision.Body["reason"] = "final_tail_missing"
	for name, r := range map[string]Record{
		".collision.json": collision,
		".invalid.json":   bad,
		"good.json":       record("gap", map[string]any{"reason": "source_offline"}),
	} {
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(dir, name), data, 0600)
	}
	unsafe, err := json.Marshal(record("gap", map[string]any{"reason": "source_offline"}))
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(dir, ".bad-mode.json"), unsafe, 0644)
	if err := os.Mkdir(filepath.Join(dir, ".directory.json"), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(dir, ".large.json"), make([]byte, store.limits.RequestBytes+1), 0600)
	testkit.WriteFile(t, filepath.Join(dir, ".malformed.json"), []byte("{"), 0600)
	if err := store.IngestOutbox(ctx, dir); !errors.Is(err, errInvalidRecord) {
		t.Fatalf("expected invalid record diagnostic, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "good.json")); !os.IsNotExist(err) {
		t.Fatalf("valid file did not drain: %v", err)
	}
	for _, name := range []string{".bad-mode.json", ".collision.json", ".directory.json", ".invalid.json", ".large.json", ".malformed.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("refused file %s was not retained: %v", name, err)
		}
	}
}

func TestOutboxBoundsInvalidFileAttemptsAndResumes(t *testing.T) {
	ctx := context.Background()
	store := fixtureStore(t)
	dir := testkit.TempDir(t)
	for i := range 257 {
		testkit.WriteFile(t, filepath.Join(dir, fmt.Sprintf("%03d.json", i)), []byte("{"), 0600)
	}
	data, err := json.Marshal(record("gap", map[string]any{"reason": "source_offline"}))
	if err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(dir, "good.json")
	testkit.WriteFile(t, good, data, 0600)
	if err := store.IngestOutbox(ctx, dir); !errors.Is(err, errInvalidRecord) {
		t.Fatalf("expected invalid record diagnostic, got %v", err)
	}
	if _, err := os.Stat(good); err != nil {
		t.Fatalf("first poll exceeded its budget: %v", err)
	}
	if err := store.IngestOutbox(ctx, dir); !errors.Is(err, errInvalidRecord) {
		t.Fatalf("expected remaining invalid record diagnostic, got %v", err)
	}
	if _, err := os.Stat(good); !os.IsNotExist(err) {
		t.Fatalf("second poll did not drain valid file: %v", err)
	}
}

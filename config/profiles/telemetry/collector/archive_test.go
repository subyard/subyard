package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestArchiveUpgrade(t *testing.T) {
	ctx := context.Background()
	for _, version := range []int{1, 2, 3, 4} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			s := fixtureStore(t)
			const runID = "42424242-4242-4242-8242-424242424242"
			legacy := runID
			if version == 4 {
				legacy = "h1:" + strings.Repeat("ab", 32)
			}
			body, _ := json.Marshal(map[string]any{"agent_run_id": legacy, "historical_note": "retained", "state": "resources_observing"})
			seedRetained(t, s, Envelope{OriginID: "producer:old-source", EventID: "old-run", SourceID: "old-source",
				SourceVersion: "1", Kind: "run", Encoding: "json", PolicyVersion: PolicyVersion, Body: body})
			if _, err := s.db.Exec(`DROP TABLE runs;
				CREATE TABLE runs(agent_run_id TEXT PRIMARY KEY,body TEXT NOT NULL,source_time TEXT NOT NULL);
				DROP TABLE objects;
				CREATE TABLE objects(object_id TEXT PRIMARY KEY,kind TEXT NOT NULL,parent_object_id TEXT,accounting TEXT,body TEXT NOT NULL,source_time TEXT NOT NULL);
				DROP TABLE associations;
				CREATE TABLE associations(association_id TEXT PRIMARY KEY,object_id TEXT NOT NULL,agent_run_id TEXT,quality TEXT,start_time TEXT,end_time TEXT,shared INTEGER NOT NULL,body TEXT NOT NULL);
				DROP TABLE allocations;
				CREATE TABLE allocations(object_id TEXT PRIMARY KEY,body TEXT NOT NULL,source_time TEXT NOT NULL);
				DELETE FROM metadata WHERE key='store_generation_id'`); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec("INSERT INTO runs VALUES(?,?,?)", legacy, string(body), unknownSourceTime); err != nil {
				t.Fatal(err)
			}
			if version == 1 {
				if _, err := s.db.Exec("ALTER TABLE records DROP COLUMN import_policy_version"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version=%d", version)); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(s.dir, databaseName)
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if version == 3 {
				// Keep the last committed update in the legacy WAL.
				const writer = `
import os, sqlite3, sys
db = sqlite3.connect(sys.argv[1])
assert db.execute('PRAGMA journal_mode=WAL').fetchone() == ('wal',)
db.execute("UPDATE records SET event_id='wal-event'")
db.commit()
os._exit(0)
`
				if err := exec.Command("python3", "-B", "-c", writer, path).Run(); err != nil {
					t.Fatal(err)
				}
			}
			runs := filepath.Join(s.dir, "runs")
			if err := privateDir(runs); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(runs, 0700) })
			boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
			if err != nil {
				t.Fatal(err)
			}
			manifest := fmt.Sprintf(`{"agent_run_id":%q,"boot_id":%q,"pid":2147483647}`, runID, strings.TrimSpace(string(boot)))
			testkit.WriteFile(t, filepath.Join(runs, runID+".json"), []byte(manifest), 0600)
			if err := os.Chmod(runs, 0500); err != nil {
				t.Fatal(err)
			}
			outbox := filepath.Join(s.dir, "outbox")
			if err := privateDir(outbox); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(outbox, 0700) })
			gap := []byte(`{"schema_version":1,"source_id":"old-source","event_id":".legacy","source_version":"1","kind":"gap","body":{"reason":"source_offline"}}`)
			gapPath := filepath.Join(outbox, ".legacy.json")
			testkit.WriteFile(t, gapPath, gap, 0600)
			if err := os.Chmod(outbox, os.ModeSticky|0500); err != nil {
				t.Fatal(err)
			}
			st := info.Sys().(*syscall.Stat_t)
			c := cutover{Device: st.Dev, Inode: st.Ino, Archive: "archive", Nonce: newID(), Started: spoolTime()}
			data, _ := json.Marshal(c)
			testkit.WriteFile(t, filepath.Join(s.dir, cutoverFile), data, 0600)
			if version <= 2 {
				// Interrupt after committing historical evidence, with the old inode
				// still at the active path in v1 and already staged in v2.
				if err := c.checkpointArchive(path); err != nil {
					t.Fatal(err)
				}
			}
			if version == 2 {
				stage := filepath.Join(s.dir, pendingArchive+"-"+c.Nonce)
				if err := privateDir(stage); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path, filepath.Join(stage, databaseName)); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				active, err := Open(s.dir, DefaultLimits())
				if err != nil {
					t.Fatal(err)
				}
				if err := active.prune(); err != nil {
					t.Fatal(err)
				}
				stats, err := Stats(ctx, active.db)
				if err != nil || stats["records"] != int64(0) || active.RegisteredRun(ctx, runID) || active.origin == s.origin {
					t.Fatalf("active cutover rows: %v (%v)", stats, err)
				}
				var offsets int
				if err := active.db.QueryRow("SELECT count(*) FROM spool_offsets").Scan(&offsets); err != nil || offsets != 0 {
					t.Fatalf("fresh store has spool checkpoints: %d (%v)", offsets, err)
				}
				if err := active.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if pending, err := os.ReadFile(gapPath); err != nil || !bytes.Equal(pending, gap) {
				t.Fatalf("retired outbox payload changed: %v", err)
			}
			archive := filepath.Join(s.dir, "archive")
			if sealed, err := os.Stat(filepath.Join(archive, databaseName)); err != nil ||
				sealed.Mode().Perm() != 0400 || !os.SameFile(info, sealed) {
				t.Fatalf("archive was not moved and sealed: %v (%v)", sealed, err)
			}
			if active, err := Open(archive, DefaultLimits()); err == nil {
				active.Close()
				t.Fatal("archive opened for writing")
			}
			db, err := OpenReadOnly(archive)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			stats, err := Stats(ctx, db)
			if err != nil || stats["schema_version"] != version || stats["store_generation_id"] != s.OriginID() {
				t.Fatalf("historical store metadata: %v (%v)", stats, err)
			}
			report, err := ShowRun(ctx, db, legacy)
			if err != nil {
				t.Fatal(err)
			}
			gaps := report["gaps"].([]any)
			if len(gaps) != 1 || gaps[0].(map[string]any)["missing"] != "final_tail" {
				t.Fatalf("historical final gap: %v", gaps)
			}
			var exported bytes.Buffer
			cursor, err := Export(ctx, db, &exported, 0, 10)
			if err != nil || cursor != 2 || !bytes.Contains(exported.Bytes(), []byte(`"historical_note":"retained"`)) ||
				(version == 3 && !bytes.Contains(exported.Bytes(), []byte("wal-event"))) {
				t.Fatalf("archive export: cursor=%d (%v)", cursor, err)
			}
			replay := fixtureStore(t)
			for range 2 {
				if err = replay.Replay(ctx, bytes.NewReader(exported.Bytes())); err != nil {
					t.Fatal(err)
				}
			}
			replayed, err := ShowRun(ctx, replay.db, legacy)
			if err != nil {
				t.Fatal(err)
			}
			replayedGaps := replayed["gaps"].([]any)
			if len(replayedGaps) != 1 || replayedGaps[0].(map[string]any)["reason"] != "interrupted_by_upgrade" {
				t.Fatalf("historical final gap after repeated replay: %v", replayedGaps)
			}
		})
	}
}

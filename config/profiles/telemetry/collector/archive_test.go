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

func legacyOutboxFixture(t *testing.T, s *Store) string {
	t.Helper()
	path := filepath.Join(s.dir, "outbox")
	if err := privateDir(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0700) })
	return path
}

func importLegacyFixture(t *testing.T, s *Store, p Policy) error {
	t.Helper()
	if err := os.Chmod(filepath.Join(s.dir, "outbox"), os.ModeSticky|0500); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	c := cutover{Started: spoolTime()}
	if err := c.importLegacy(tx, s.dir, s.key, p); err != nil {
		return err
	}
	return tx.Commit()
}

func TestArchiveUpgrade(t *testing.T) {
	ctx := context.Background()
	for _, version := range []int{1, 2, 3, 4} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			s := fixtureStore(t)
			const runID = "42424242-4242-4242-8242-424242424242"
			legacy := runID
			if version == 4 {
				legacy = s.key.id(legacy)
			}
			body, _ := json.Marshal(map[string]any{"agent_run_id": legacy, "historical_note": "retained"})
			seedRetained(t, s, Envelope{OriginID: "producer:old-source", EventID: "old-run", SourceID: "old-source",
				SourceVersion: "1", Kind: "run", Encoding: "json", PolicyVersion: PolicyVersion, Body: body})
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
			outbox := legacyOutboxFixture(t, s)
			gap := []byte(`{"schema_version":1,"source_id":"old-source","event_id":".legacy","source_version":"1","kind":"gap","body":{"reason":"source_offline"}}`)
			testkit.WriteFile(t, filepath.Join(outbox, ".legacy.json"), gap, 0600)
			testkit.WriteFile(t, filepath.Join(outbox, ".foreign.json"), gap, 0600)
			unsupported := strings.NewReplacer(`"schema_version":1`, `"schema_version":2`, `".legacy"`, `".unsupported"`).Replace(string(gap))
			testkit.WriteFile(t, filepath.Join(outbox, ".unsupported.json"), []byte(unsupported), 0600)
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
				if err := c.checkpointArchive(s.dir, path); err != nil {
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
				if err != nil || stats["records"] != int64(2) || !active.RegisteredRun(ctx, runID) || active.origin == s.origin {
					t.Fatalf("active cutover rows: %v (%v)", stats, err)
				}
				var offsets int
				if err := active.db.QueryRow("SELECT count(*) FROM spool_offsets").Scan(&offsets); err != nil || offsets != 0 {
					t.Fatalf("legacy import left checkpoints: %d (%v)", offsets, err)
				}
				if err := active.Close(); err != nil {
					t.Fatal(err)
				}
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
			var count int
			if err := replay.db.QueryRow(`SELECT count(*) FROM gaps WHERE json_extract(body,'$.reason')='interrupted_by_upgrade'`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("historical gap after repeated replay: %d (%v)", count, err)
			}
		})
	}
}

func TestEstablishedStoreRefusesMissingAndMismatchedKeys(t *testing.T) {
	for _, missing := range []bool{true, false} {
		t.Run(fmt.Sprint(missing), func(t *testing.T) {
			dir := testkit.TempDir(t)
			s, err := Open(dir, DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Ingest(context.Background(), []Record{record("gap", map[string]any{})}); err != nil {
				t.Fatal(err)
			}
			if err := s.publishCapturePolicy(DefaultPolicy()); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, identityKeyFile)
			want := "does not match"
			if missing {
				want = "is missing"
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else {
				replacement := bytes.Clone(s.key)
				replacement[0] ^= 1
				testkit.WriteFile(t, path, replacement, 0600)
			}
			if opened, err := Open(dir, DefaultLimits()); err == nil {
				opened.Close()
				t.Fatal("store accepted an incorrect key")
			} else if !strings.Contains(err.Error(), want) {
				t.Fatal(err)
			}
			const producer = `
import sys
sys.path.insert(0, '..')
import records
from pathlib import Path
root = Path(sys.argv[1])
assert not records.emit(root, 'gap', {}, source_id='fixture', event_id='wrong-key')
assert not list((root/'spool').iterdir())
`
			if output, err := exec.Command("python3", "-B", "-c", producer, dir, want).CombinedOutput(); err != nil {
				t.Fatalf("producer check: %v\n%s", err, output)
			}
			if missing {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("missing key was recreated: %v", err)
				}
			}
			testkit.WriteFile(t, path, s.key, 0600)
			restored, err := Open(dir, DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			if stats, err := Stats(context.Background(), restored.db); err != nil || stats["records"] != int64(1) || restored.OriginID() != s.OriginID() {
				t.Fatalf("restored store changed: %v (%v)", stats, err)
			}
		})
	}
}

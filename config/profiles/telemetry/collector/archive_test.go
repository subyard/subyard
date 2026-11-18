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
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestArchiveUpgrade(t *testing.T) {
	ctx := context.Background()
	for _, version := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			dir := testkit.TempDir(t)
			old, err := Open(dir, DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			oldOrigin := old.OriginID()
			seedRetained(t, old, Envelope{OriginID: "producer:old-source", EventID: "old-run", SourceID: "old-source",
				SourceVersion: "1", Kind: "run", Encoding: "json", PolicyVersion: PolicyVersion,
				Body: json.RawMessage(`{"agent_run_id":"legacy","historical_note":"retained"}`)})
			if _, err = old.db.Exec(`INSERT INTO runs VALUES('legacy','{"agent_run_id":"legacy"}','')`); err != nil {
				t.Fatal(err)
			}
			if version == 1 {
				if _, err = old.db.Exec("ALTER TABLE records DROP COLUMN import_policy_version"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = old.db.Exec(fmt.Sprintf("PRAGMA user_version=%d", version)); err != nil {
				t.Fatal(err)
			}
			if err = old.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, databaseName)
			oldFile, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(dir, "archive")
			archivedDB := filepath.Join(archive, databaseName)
			switch version {
			case 2:
				// Simulate interruption immediately after the archive rename.
				if err = privateDir(archive); err != nil {
					t.Fatal(err)
				}
				if err = os.Rename(path, archivedDB); err != nil {
					t.Fatal(err)
				}
			case 3:
				// Leave a committed row exclusively in the legacy writer's WAL.
				code := `
import os, sqlite3, sys
db = sqlite3.connect(sys.argv[1])
assert db.execute('PRAGMA journal_mode=WAL').fetchone() == ('wal',)
db.execute("UPDATE records SET event_id='wal-event'")
db.commit()
os._exit(0)
`
				if output, err := exec.Command("python3", "-B", "-c", code, path).CombinedOutput(); err != nil {
					t.Fatalf("legacy writer: %v\n%s", err, output)
				}
				if stat, err := os.Stat(path + "-wal"); err != nil || stat.Size() == 0 {
					t.Fatalf("missing legacy WAL: %v", err)
				}
			}
			var activeOrigin string
			for range 2 {
				active, err := Open(dir, DefaultLimits())
				if err != nil {
					t.Fatal(err)
				}
				stats, err := Stats(ctx, active.db)
				if closeErr := active.Close(); closeErr != nil {
					t.Fatal(closeErr)
				}
				if err != nil || stats["records"] != int64(0) || active.OriginID() == oldOrigin ||
					(activeOrigin != "" && active.OriginID() != activeOrigin) {
					t.Fatalf("active store was not initialized once with a new origin: %v (%v)", stats, err)
				}
				activeOrigin = active.OriginID()
			}
			if stat, err := os.Stat(archivedDB); err != nil || stat.Mode().Perm() != 0400 || !os.SameFile(oldFile, stat) {
				t.Fatalf("original database was not moved and sealed: %v", err)
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
			rows, err := Query(ctx, db, "SELECT agent_run_id FROM runs")
			if err != nil || len(rows) != 1 || rows[0]["agent_run_id"] != "legacy" {
				t.Fatalf("archive query changed legacy rows: %v (%v)", rows, err)
			}
			var archivedVersion int
			if err := db.QueryRow("PRAGMA user_version").Scan(&archivedVersion); err != nil || archivedVersion != version {
				t.Fatalf("archive schema=%d (%v)", archivedVersion, err)
			}
			var exported bytes.Buffer
			cursor, err := Export(ctx, db, &exported, 0, 10)
			var row Envelope
			decodeErr := json.Unmarshal(exported.Bytes(), &row)
			event := "old-run"
			if version == 3 {
				event = "wal-event"
			}
			if err != nil || cursor != 1 || decodeErr != nil || row.EventID != event ||
				row.OriginID != "producer:old-source" || row.ImportPolicyVersion != "" ||
				!bytes.Contains(row.Body, []byte(`"historical_note":"retained"`)) {
				t.Fatalf("archive export changed legacy data: %s (%v, %v)", exported.Bytes(), err, decodeErr)
			}
			if err = fixtureStore(t).Replay(ctx, &exported); err != nil {
				t.Fatal("archive export is not replayable", err)
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
root, message = Path(sys.argv[1]), sys.argv[2]
assert not records.emit(root, 'gap', {}, source_id='fixture', event_id='wrong-key')
assert not (root/'outbox').exists()
try:
    records.identity_key(root)
except ValueError as error:
    assert message in str(error)
else:
    raise AssertionError('producer accepted incorrect key')
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

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
	"go.opentelemetry.io/collector/pdata/pcommon"
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
			outbox := filepath.Join(dir, "outbox")
			if err = os.Chmod(outbox, 0700); err != nil {
				t.Fatal(err)
			}
			runs := filepath.Join(dir, "runs")
			if err = privateDir(runs); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(runs, 0700) })
			boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
			if err != nil {
				t.Fatal(err)
			}
			process, err := os.ReadFile("/proc/self/stat")
			if err != nil {
				t.Fatal(err)
			}
			start := strings.Fields(string(process)[strings.LastIndex(string(process), ") ")+2:])[19]
			for run, ticks := range map[string]string{"legacy": start, "stale": start + "0"} {
				manifest := fmt.Sprintf(`{"agent_run_id":%q,"boot_id":%q,"pid":%d,"start_ticks":%s}`, run, strings.TrimSpace(string(boot)), os.Getpid(), ticks)
				testkit.WriteFile(t, filepath.Join(runs, run+".json"), []byte(manifest), 0600)
			}
			switch version {
			case 1:
				// Token validation happens after opening the standalone writer.
				// Even a failed serve must already have fenced the old producers.
				if err = RunCLI([]string{"serve", "--data-dir", dir, "--auth-file", filepath.Join(dir, "missing")}, nil); !os.IsNotExist(err) {
					t.Fatalf("serve should fail for the missing token: %v", err)
				}
			case 2:
				if err = privateDir(archive); err != nil {
					t.Fatal(err)
				}
				code := `import os, signal, sys; os.rename(*sys.argv[1:]); os.kill(os.getpid(), signal.SIGKILL)`
				if err = exec.Command("python3", "-B", "-c", code, path, filepath.Join(dir, pendingArchive)).Run(); err == nil ||
					err.(*exec.ExitError).Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
					t.Fatalf("archive staging was not interrupted: %v", err)
				}
				if err = publishPendingArchive(dir); err != nil {
					t.Fatal(err)
				}
				// Try a writer before the original data directory is reopened.
				if writer, err := Open(archive, DefaultLimits()); err == nil {
					writer.Close()
					t.Fatal("published archive opened for writing")
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
				input := filepath.Join(dir, "empty.jsonl")
				testkit.WriteFile(t, input, nil, 0600)
				if err = RunCLI([]string{"replay", "--data-dir", dir, "--input", input}, nil); err != nil {
					t.Fatal(err)
				}
			}
			if version != 2 {
				if st, err := os.Stat(outbox); err != nil || st.Mode() != os.ModeDir|os.ModeSticky|0700 {
					t.Fatalf("standalone writer did not fence the legacy outbox: %v (%v)", st, err)
				}
			}
			var activeOrigin string
			for range 2 {
				active, err := Open(dir, DefaultLimits())
				if err != nil {
					t.Fatal(err)
				}
				if err = active.prune(); err != nil {
					t.Fatal(err)
				}
				if err = active.resourceContext(context.WithValue(ctx, runContextKey{}, "legacy"), pcommon.NewMap()); err != nil {
					t.Fatalf("native OTLP rejected the live legacy registration: %v", err)
				}
				if active.RegisteredRun(ctx, "stale") {
					t.Fatal("stale registration carried into the active store")
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
			if version != 2 {
				report, err := ShowRun(ctx, db, "legacy")
				if err != nil {
					t.Fatal(err)
				}
				gaps := report["gaps"].([]any)
				if report["run"].(map[string]any)["interrupted_by_upgrade"] != true || len(gaps) != 1 ||
					gaps[0].(map[string]any)["missing"] != "final_tail" {
					t.Fatalf("missing interruption coverage: %v", report)
				}
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

func TestArchiveRestoredLegacyStoreAlongsideExistingArchive(t *testing.T) {
	dir := testkit.TempDir(t)
	ctx := context.Background()
	open := func() *Store {
		t.Helper()
		s, err := Open(dir, DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	legacy := open()
	for _, event := range []string{"first-history", "second-history"} {
		seedRetained(t, legacy, Envelope{OriginID: "legacy", EventID: event, SourceID: "legacy",
			SourceVersion: "1", Kind: "gap", Encoding: "json", Body: json.RawMessage(`{}`)})
		if _, err := legacy.db.Exec("PRAGMA user_version=2"); err != nil {
			t.Fatal(err)
		}
		if err := legacy.Close(); err != nil {
			t.Fatal(err)
		}
		legacy = open()
		stats, err := Stats(ctx, legacy.db)
		if err != nil || stats["records"] != int64(0) {
			t.Fatalf("expected an empty active store after %s: %v (%v)", event, stats, err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopening also completes a seal interrupted on a numbered archive.
	if err := os.Chmod(filepath.Join(dir, "archive-2", databaseName), 0600); err != nil {
		t.Fatal(err)
	}
	active := open()
	defer active.Close()
	for archive, want := range map[string]string{"archive": "first-history", "archive-2": "second-history"} {
		path := filepath.Join(dir, archive)
		info, err := os.Stat(filepath.Join(path, databaseName))
		if err != nil || info.Mode().Perm() != 0400 {
			t.Fatalf("%s was not sealed: %v", archive, err)
		}
		db, err := OpenReadOnly(path)
		if err != nil {
			t.Fatal(err)
		}
		var event string
		err = db.QueryRow("SELECT event_id FROM records").Scan(&event)
		db.Close()
		if err != nil || event != want {
			t.Fatalf("%s has event %q, want %q (%v)", archive, event, want, err)
		}
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
assert not list((root/'outbox').glob('*.json'))
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

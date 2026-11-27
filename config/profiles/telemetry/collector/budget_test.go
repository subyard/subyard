package collector

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/safefs"
	"github.com/Subyard/Subyard/internal/testkit/interrupt"
)

// waitBudgetPrune waits until no budget prune runs.
func waitBudgetPrune(t *testing.T, s *Store) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		s.pruneMu.Lock()
		running := s.budgetPruning
		s.pruneMu.Unlock()
		if !running {
			return
		}
	}
	t.Fatal("budget prune did not finish")
}

// rawBatch stores count raw OTLP rows of 4 KiB received age ago.
func rawBatch(t *testing.T, s *Store, label string, age time.Duration, count int) {
	t.Helper()
	rawRows(t, s, label, age, count, 4096)
}

func rawRows(t *testing.T, s *Store, label string, age time.Duration, count, size int) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	received := time.Now().Add(-age).UTC().Format(timestampLayout)
	payload := make([]byte, size)
	for i := range count {
		rand.Read(payload)
		if _, err = tx.Exec(`INSERT INTO records(origin_id,event_id,received_time,source_id,source_version,kind,encoding,policy_version,redactions,protobuf)
			VALUES(?,?,?,?,'1','otlp/logs','protobuf',?,0,?)`, s.origin, fmt.Sprintf("%s-%d", label, i), received, fixtureSourceID, PolicyVersion, payload); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func countRows(t *testing.T, s *Store, where string, args ...any) (n int) {
	t.Helper()
	if err := s.db.QueryRow("SELECT count(*) FROM records WHERE "+where, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// smallBatches makes budget prune batches small enough to observe their order.
func smallBatches(t *testing.T) {
	rows := budgetBatchRows
	budgetBatchRows = 100
	t.Cleanup(func() { budgetBatchRows = rows })
}

func usedNow(t *testing.T, s *Store) int64 {
	t.Helper()
	used, err := s.usedBytes()
	if err != nil {
		t.Fatal(err)
	}
	return used
}

// orphans counts sample and relation projections whose record is gone.
func orphans(t *testing.T, s *Store) (n int) {
	t.Helper()
	if err := s.db.QueryRow(`SELECT (SELECT count(*) FROM samples p WHERE NOT EXISTS(SELECT 1 FROM records r WHERE r.origin_id=p.origin_id AND r.event_id=p.event_id))
		+(SELECT count(*) FROM relations p WHERE NOT EXISTS(SELECT 1 FROM records r WHERE r.origin_id=p.origin_id AND r.event_id=p.event_id))`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Above the budget the oldest raw records go first and records younger than
// MinRetention stay even while the store is still over budget; usage,
// lifecycle and resource records stay.
func TestBudgetPruneRemovesOldestRawAndKeepsRecent(t *testing.T) {
	ctx := context.Background()
	smallBatches(t)
	s := fixtureStore(t)
	captureNativeFixtures(t, s)
	if err := s.Ingest(ctx, []Record{record("run", map[string]any{"agent_run_id": "budget"}),
		record("scope", map[string]any{"object_id": "scope-1"}), record("sample", map[string]any{"object_id": "scope-1", "cpu_ns": 1})}); err != nil {
		t.Fatal(err)
	}
	// The native fixtures, their correlations and every other record are the oldest.
	if _, err := s.db.Exec("UPDATE records SET received_time='2000-01-01T00:00:00.000000000Z'"); err != nil {
		t.Fatal(err)
	}
	fixtureRaw := "received_time<'2001' AND (kind='sample' OR kind LIKE 'otlp/%' OR source_id='collector:otlp-correlation')"
	if countRows(t, s, fixtureRaw+" AND source_id='collector:otlp-correlation'") == 0 || countRows(t, s, fixtureRaw+" AND kind='sample'") == 0 {
		t.Fatal("fixture has no old correlation or sample")
	}
	rawBatch(t, s, "oldest", 60*24*time.Hour, 1000)
	rawBatch(t, s, "older", 40*24*time.Hour, 1000)
	rawBatch(t, s, "recent", 29*24*time.Hour, 200)
	kept := countRows(t, s, "kind NOT LIKE 'otlp/%' AND source_id!='collector:otlp-correlation' AND kind!='sample'")
	floor := time.Now().Add(-MinRetention)
	// Below the budget nothing is pruned.
	if over, err := s.pruneToBudget(floor); err != nil || over || countRows(t, s, "event_id LIKE 'oldest-%'") != 1000 {
		t.Fatalf("prune below budget: over=%v err=%v", over, err)
	}
	s.limits.DiskBytes = usedNow(t, s) - 1<<20
	if over, err := s.pruneToBudget(floor); err != nil || over {
		t.Fatalf("first budget prune: over=%v err=%v", over, err)
	}
	if oldest := countRows(t, s, "event_id LIKE 'oldest-%'"); countRows(t, s, fixtureRaw) != 0 || oldest == 0 || oldest == 1000 ||
		countRows(t, s, "event_id LIKE 'older-%'") != 1000 || usedNow(t, s) > s.limits.DiskBytes {
		t.Fatalf("budget prune did not remove the oldest raw records first, just enough: %d oldest left", oldest)
	}
	if orphans(t, s) != 0 {
		t.Fatal("budget prune left sample or relation projections")
	}
	if _, err := os.Lstat(filepath.Join(s.dir, OverBudgetMarker)); !os.IsNotExist(err) {
		t.Fatalf("over-budget marker within budget: %v", err)
	}
	s.limits.DiskBytes = 1 << 20
	if over, err := s.pruneToBudget(floor); err != nil || !over {
		t.Fatalf("second budget prune: over=%v err=%v", over, err)
	}
	if countRows(t, s, "event_id LIKE 'oldest-%' OR event_id LIKE 'older-%'") != 0 || countRows(t, s, "event_id LIKE 'recent-%'") != 200 {
		t.Fatal("budget prune removed records younger than the retention floor")
	}
	if countRows(t, s, "kind NOT LIKE 'otlp/%' AND source_id!='collector:otlp-correlation' AND kind!='sample'") != kept || countRows(t, s, "source_id=?", usageSource) == 0 {
		t.Fatal("budget prune removed usage, lifecycle or resource records")
	}
	if info, err := os.Lstat(filepath.Join(s.dir, OverBudgetMarker)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("over-budget marker: %v %v", info, err)
	}
	// Raising the budget clears the warning at the next commit.
	s.limits.DiskBytes = DefaultDiskBytes
	if err := s.Ingest(ctx, []Record{record("gap", map[string]any{"reason": "source_offline"})}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(s.dir, OverBudgetMarker)); !os.IsNotExist(err) {
		t.Fatalf("over-budget marker after raising the budget: %v", err)
	}
}

// A commit above the budget starts one background prune; ingestion goes on,
// and a store held over budget by recent records is not rescanned at once.
func TestCommitAboveBudgetPrunesInBackground(t *testing.T) {
	ctx := context.Background()
	s := fixtureStore(t)
	rawBatch(t, s, "oldest", 60*24*time.Hour, 300)
	rawBatch(t, s, "recent", time.Hour, 300)
	s.limits.DiskBytes = 1 << 20
	if err := s.Ingest(ctx, []Record{record("run", map[string]any{"agent_run_id": "trigger"})}); err != nil {
		t.Fatalf("ingestion refused above budget: %v", err)
	}
	waitBudgetPrune(t, s)
	if countRows(t, s, "event_id LIKE 'oldest-%'") != 0 || countRows(t, s, "event_id LIKE 'recent-%'") != 300 {
		t.Fatal("background budget prune did not remove only old raw records")
	}
	s.pruneMu.Lock()
	retry := s.budgetRetryAt
	s.pruneMu.Unlock()
	if !retry.After(time.Now()) {
		t.Fatal("store still over budget is retried at once")
	}
	rawBatch(t, s, "late", 60*24*time.Hour, 10)
	if err := s.Ingest(ctx, []Record{record("run", map[string]any{"agent_run_id": "again"})}); err != nil {
		t.Fatal(err)
	}
	waitBudgetPrune(t, s)
	if countRows(t, s, "event_id LIKE 'late-%'") != 10 {
		t.Fatal("budget prune ran again before its retry delay")
	}
}

// pinReader holds a read snapshot from another connection, as the stats,
// query and export commands do, so no checkpoint can reset the WAL.
func pinReader(t *testing.T, s *Store) {
	t.Helper()
	db, err := OpenReadOnly(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err = tx.QueryRow("SELECT count(*) FROM records").Scan(&n); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback(); db.Close() })
}

// A reader that blocks WAL checkpoints changes neither how much the budget
// prune removes nor its verdict.
func TestBudgetPruneIgnoresSnapshotReader(t *testing.T) {
	ctx := context.Background()
	smallBatches(t)
	s := fixtureStore(t)
	// Small rows: each deleting batch rewrites more WAL than it frees pages.
	// The reader's snapshot ends inside the WAL, so the WAL cannot restart.
	if _, err := s.db.Exec("PRAGMA wal_autocheckpoint=0"); err != nil {
		t.Fatal(err)
	}
	rawRows(t, s, "oldest", 60*24*time.Hour, 20000, 64)
	pinReader(t, s)
	if _, err := s.db.Exec("PRAGMA wal_autocheckpoint=128"); err != nil {
		t.Fatal(err)
	}
	s.limits.DiskBytes = usedNow(t, s) - 64<<10
	if over, err := s.pruneToBudget(time.Now().Add(-MinRetention)); err != nil || over {
		t.Fatalf("budget prune with a reader: over=%v err=%v", over, err)
	}
	if left := countRows(t, s, "event_id LIKE 'oldest-%'"); left < 19000 {
		t.Fatalf("a reader made the budget prune remove %d records", 20000-left)
	}
	if _, err := os.Lstat(filepath.Join(s.dir, OverBudgetMarker)); !os.IsNotExist(err) {
		t.Fatalf("false over-budget verdict: %v", err)
	}
	// The WAL the reader keeps is outside the budget: a commit within it
	// starts no prune.
	rawBatch(t, s, "late", 60*24*time.Hour, 10)
	s.limits.DiskBytes = usedNow(t, s) + 1<<20
	if err := s.Ingest(ctx, []Record{record("run", map[string]any{"agent_run_id": "within"})}); err != nil {
		t.Fatal(err)
	}
	waitBudgetPrune(t, s)
	if countRows(t, s, "event_id LIKE 'late-%'") != 10 {
		t.Fatal("a store within its budget was pruned")
	}
	// Recent records above the budget still give the warning.
	rawBatch(t, s, "recent", time.Hour, 300)
	s.limits.DiskBytes = 1 << 20
	if _, err := s.db.Exec("DELETE FROM records WHERE event_id LIKE 'oldest-%' OR event_id LIKE 'late-%'"); err != nil {
		t.Fatal(err)
	}
	if over, err := s.pruneToBudget(time.Now().Add(-MinRetention)); err != nil || !over {
		t.Fatalf("recent store above budget with a reader: over=%v err=%v", over, err)
	}
	if _, err := os.Lstat(filepath.Join(s.dir, OverBudgetMarker)); err != nil {
		t.Fatalf("over-budget verdict suppressed by a reader: %v", err)
	}
}

// budgetSweepState is the fixture of one interrupted budget prune.
type budgetSweepState struct{ store *Store }

// A budget prune cut after any committed batch, before its verdict or inside
// the over-budget marker publication converges on retry: SQLite commits each
// batch whole, the error path returns without cleanup, and a crash keeps only
// .telemetry-* staging files, which nothing reads.
func TestInterruptedBudgetPruneConverges(t *testing.T) {
	smallBatches(t)
	interrupt.Sweep(t, interrupt.Case[budgetSweepState]{
		Setup: func(t *testing.T) budgetSweepState {
			s := fixtureStore(t)
			rawBatch(t, s, "oldest", 60*24*time.Hour, 150)
			rawBatch(t, s, "recent", time.Hour, 300)
			s.limits.DiskBytes = 1 << 20
			return budgetSweepState{s}
		},
		Run: func(t *testing.T, st budgetSweepState, in *interrupt.Injector) interrupt.Outcome {
			budgetPruneFault, fileFault = in.Hook("budget"), in.Safefs()
			defer func() { budgetPruneFault, fileFault = nil, nil }()
			over, err := st.store.pruneToBudget(time.Now().Add(-MinRetention))
			return interrupt.Outcome{Err: err, Output: fmt.Sprint(over)}
		},
		Signature: func(t *testing.T, st budgetSweepState) any {
			_, err := os.Lstat(filepath.Join(st.store.dir, OverBudgetMarker))
			return [3]any{countRows(t, st.store, "event_id LIKE 'oldest-%'"), countRows(t, st.store, "event_id LIKE 'recent-%'"),
				fmt.Sprint(err == nil, interrupt.Tree(t, st.store.dir, "telemetry.sqlite*", ".telemetry-*", OverBudgetMarker))}
		},
		Completed: func(t *testing.T, st budgetSweepState, o interrupt.Outcome) {
			if o.Err != nil || o.Output != "true" || countRows(t, st.store, "event_id LIKE 'oldest-%'") != 0 || countRows(t, st.store, "event_id LIKE 'recent-%'") != 300 {
				t.Fatalf("uncut budget prune: %+v", o)
			}
			if _, err := os.Lstat(filepath.Join(st.store.dir, OverBudgetMarker)); err != nil {
				t.Fatal(err)
			}
		},
		Interrupted: func(t *testing.T, _ budgetSweepState, p interrupt.Point, o interrupt.Outcome) {
			// The marker publication is best effort: a cut inside it is retried by
			// the next prune, so the verdict itself still returns.
			if p.Seam == "budget" && !errors.Is(o.Err, interrupt.ErrInjected) {
				t.Fatalf("error = %v, want the injected cut at %s", o.Err, p)
			}
		},
		Resumed: func(t *testing.T, _ budgetSweepState, p interrupt.Point, o interrupt.Outcome) {
			if o.Err != nil || o.Output != "true" {
				t.Fatalf("budget prune after %s: %+v", p, o)
			}
		},
		Mode:      interrupt.Crash,
		MinPoints: 4,
		Require:   []string{"batch", "verdict", string(safefs.AfterStage), string(safefs.BeforePublish)},
	})
}

// Close during a running budget prune waits for it; a reopened store resumes.
func TestCloseWaitsForBudgetPrune(t *testing.T) {
	smallBatches(t)
	s := fixtureStore(t)
	rawBatch(t, s, "oldest", 60*24*time.Hour, 1000)
	rawBatch(t, s, "recent", time.Hour, 300)
	s.limits.DiskBytes = 1 << 20
	started := make(chan struct{})
	budgetPruneFault = func(string) error {
		select {
		case <-started:
		default:
			close(started)
		}
		for !s.isClosed() {
			time.Sleep(time.Millisecond)
		}
		return nil
	}
	t.Cleanup(func() { budgetPruneFault = nil })
	s.startBudgetPrune()
	<-started
	dir := s.dir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	budgetPruneFault = nil
	limits := DefaultLimits()
	limits.DiskBytes = 1 << 20
	reopened, err := Open(dir, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if countRows(t, reopened, "event_id LIKE 'oldest-%'") == 0 {
		t.Fatal("Close did not stop the budget prune between batches")
	}
	if over, err := reopened.pruneToBudget(time.Now().Add(-MinRetention)); err != nil || !over {
		t.Fatalf("resumed prune: over=%v err=%v", over, err)
	}
	if countRows(t, reopened, "event_id LIKE 'oldest-%'") != 0 || countRows(t, reopened, "event_id LIKE 'recent-%'") != 300 {
		t.Fatal("resumed prune did not finish")
	}
}

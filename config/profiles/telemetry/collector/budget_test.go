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
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	received := time.Now().Add(-age).UTC().Format(timestampLayout)
	payload := make([]byte, 4096)
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

func pageBytes(t *testing.T, s *Store) (n int64) {
	t.Helper()
	if err := s.db.QueryRow("SELECT (page_count-freelist_count)*page_size FROM pragma_page_count(),pragma_freelist_count(),pragma_page_size()").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Above the budget the oldest raw records go first and records younger than
// MinRetention stay even while the store is still over budget; usage,
// lifecycle and resource records stay.
func TestBudgetPruneRemovesOldestRawAndKeepsRecent(t *testing.T) {
	ctx := context.Background()
	s := fixtureStore(t)
	captureNativeFixtures(t, s)
	if err := s.Ingest(ctx, []Record{record("run", map[string]any{"agent_run_id": "budget"})}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE records SET received_time='2000-01-01T00:00:00.000000000Z' WHERE kind NOT LIKE 'otlp/%' AND source_id!='collector:otlp-correlation'"); err != nil {
		t.Fatal(err)
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
	s.limits.DiskBytes = pageBytes(t, s) - 1<<20
	if over, err := s.pruneToBudget(floor); err != nil || over {
		t.Fatalf("first budget prune: over=%v err=%v", over, err)
	}
	if countRows(t, s, "event_id LIKE 'oldest-%'") != 0 || countRows(t, s, "event_id LIKE 'older-%'") != 1000 {
		t.Fatal("budget prune did not remove the oldest raw records first")
	}
	if _, err := os.Lstat(filepath.Join(s.dir, OverBudgetMarker)); !os.IsNotExist(err) {
		t.Fatalf("over-budget marker within budget: %v", err)
	}
	s.limits.DiskBytes = 1 << 20
	if over, err := s.pruneToBudget(floor); err != nil || !over {
		t.Fatalf("second budget prune: over=%v err=%v", over, err)
	}
	if countRows(t, s, "event_id LIKE 'older-%'") != 0 || countRows(t, s, "event_id LIKE 'recent-%'") != 200 {
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

// An interrupted budget prune leaves whole batches removed and every other
// record intact, and the next run finishes the work.
func TestInterruptedBudgetPruneResumes(t *testing.T) {
	s := fixtureStore(t)
	rawBatch(t, s, "oldest", 60*24*time.Hour, 1000)
	rawBatch(t, s, "older", 40*24*time.Hour, 1000)
	rawBatch(t, s, "recent", time.Hour, 300)
	s.limits.DiskBytes = 1 << 20
	interrupted := errors.New("interrupted")
	budgetPruneFault = func() error { return interrupted }
	t.Cleanup(func() { budgetPruneFault = nil })
	if _, err := s.pruneToBudget(time.Now().Add(-MinRetention)); !errors.Is(err, interrupted) {
		t.Fatalf("fault did not interrupt the prune: %v", err)
	}
	if countRows(t, s, "event_id LIKE 'oldest-%'") != 0 || countRows(t, s, "event_id LIKE 'older-%'") != 1000 || countRows(t, s, "event_id LIKE 'recent-%'") != 300 {
		t.Fatal("interrupted prune left a partial batch")
	}
	if _, err := os.Lstat(filepath.Join(s.dir, OverBudgetMarker)); !os.IsNotExist(err) {
		t.Fatalf("interrupted prune published a verdict: %v", err)
	}
	// Close during a running budget prune waits for it; a reopened store resumes.
	budgetPruneFault = func() error { time.Sleep(50 * time.Millisecond); return nil }
	s.startBudgetPrune()
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
	if over, err := reopened.pruneToBudget(time.Now().Add(-MinRetention)); err != nil || !over {
		t.Fatalf("resumed prune: over=%v err=%v", over, err)
	}
	if countRows(t, reopened, "event_id LIKE 'older-%'") != 0 || countRows(t, reopened, "event_id LIKE 'recent-%'") != 300 {
		t.Fatal("resumed prune did not finish")
	}
}

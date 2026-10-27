package collector

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestCommitSurvivesKilledWriter(t *testing.T) {
	if dir := os.Getenv("SUBYARD_TELEMETRY_CRASH_FIXTURE"); dir != "" {
		s, err := Open(dir, DefaultLimits())
		if err != nil {
			panic(err)
		}
		if err = s.Ingest(context.Background(), []Record{record("run", map[string]any{"agent_run_id": "acknowledged"})}); err != nil {
			panic(err)
		}
		fmt.Println("committed")
		select {}
	}
	dir := testkit.TempDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCommitSurvivesKilledWriter$")
	cmd.Env = append(os.Environ(), "SUBYARD_TELEMETRY_CRASH_FIXTURE="+dir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var diagnostics bytes.Buffer
	cmd.Stderr = &diagnostics
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "committed" {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatal("writer failed before acknowledgment", diagnostics.String())
	}
	cmd.Process.Kill()
	cmd.Wait()
	s, err := Open(dir, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !s.RegisteredRun(context.Background(), "acknowledged") {
		t.Fatal("acknowledged transaction missing after SIGKILL")
	}
}

func TestReadOnlyAuthorizerAndDeadline(t *testing.T) {
	s := fixtureStore(t)
	if err := s.Ingest(context.Background(), []Record{record("run", map[string]any{"agent_run_id": "read"})}); err != nil {
		t.Fatal(err)
	}
	db, err := OpenReadOnly(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	target := filepath.Join(s.dir, "forbidden.sqlite")
	for _, query := range []string{"ATTACH DATABASE '" + target + "' AS extra", "PRAGMA query_only=OFF", "DELETE FROM records", "CREATE TEMP TABLE bypass(x)", "VACUUM INTO '" + target + "'"} {
		if _, err = Query(context.Background(), db, query); err == nil {
			t.Fatalf("read-only query allowed %s", query)
		}
	}
	if _, err = os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("read-only query created external database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err = Query(ctx, db, "WITH RECURSIVE numbers(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM numbers) SELECT sum(x) FROM numbers"); err == nil {
		t.Fatal("unbounded recursive query ignored deadline")
	}
	missing := filepath.Join(testkit.TempDir(t), "not-created")
	empty, err := OpenReadOnly(missing)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Stats(context.Background(), empty)
	empty.Close()
	if err == nil {
		t.Fatal("missing database read succeeded")
	}
	if _, err = os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("read-only open created data directory")
	}
}

func TestPrecisionTemporalClosureAndCollision(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	r := record("run", map[string]any{"agent_run_id": "r", "agent": "fixture"})
	scope := record("scope", map[string]any{"object_id": "obj", "agent_run_id": "r", "quality": "explicit", "accounting": "inclusive"})
	scope.SourceTime = "2026-01-01T00:00:00Z"
	a := record("sample", map[string]any{"object_id": "obj", "cpu_ns": json.Number("9223372036854775805"), "monotonic_ns": json.Number("10000000000000000000"), "boot_id": "boot", "generation": "g"})
	a.SourceTime = "2026-01-01T00:00:01Z"
	b := a
	b.EventID = newID()
	b.SourceTime = "2026-01-01T00:00:02Z"
	b.Body = map[string]any{"object_id": "obj", "cpu_ns": json.Number("9223372036854775806"), "monotonic_ns": json.Number("10000000000000000001"), "boot_id": "boot", "generation": "g"}
	if err := s.Ingest(ctx, []Record{r, scope, a, b}); err != nil {
		t.Fatal(err)
	}
	update := record("run", map[string]any{"agent_run_id": "r", "state": "main_exited"})
	if err := s.Ingest(ctx, []Record{update}); err != nil {
		t.Fatal(err)
	}
	result, err := ShowRun(ctx, s.db, "r")
	if err != nil {
		t.Fatal(err)
	}
	selected := result["selected_nonoverlapping"].([]any)
	if len(selected) != 1 {
		t.Fatalf("selection %+v", result)
	}
	entry := selected[0].(map[string]any)
	if fmt.Sprint(entry["cpu_ns_delta"]) != "1" {
		t.Fatalf("precision lost: %+v", entry)
	}
	if result["run"].(map[string]any)["agent"] != "fixture" {
		t.Fatal("lifecycle update erased run metadata")
	}
	var exported bytes.Buffer
	if _, err = Export(ctx, s.db, &exported, 0, 100); err != nil {
		t.Fatal(err)
	}
	destination := fixtureStore(t)
	if err = destination.Replay(ctx, bytes.NewReader(exported.Bytes())); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(exported.String(), "9223372036854775806") {
		t.Fatal("counter precision lost in export")
	}
	r.Body = map[string]any{"agent_run_id": "different"}
	if err = s.Ingest(ctx, []Record{r}); err == nil {
		t.Fatal("conflicting replay identity silently discarded")
	}
	start := record("association", map[string]any{"object_id": "proc", "agent_run_id": "r", "source": "membership", "start_time": "2026-01-01T00:00:00Z", "quality": "explicit"})
	end := record("association", map[string]any{"object_id": "proc", "agent_run_id": "r", "source": "membership", "end_time": "2026-01-01T00:00:03Z", "quality": "explicit"})
	if err = s.Ingest(ctx, []Record{end, start}); err != nil {
		t.Fatal(err)
	}
	var count int
	var from, to string
	if err = s.db.QueryRow("SELECT count(*),start_time,end_time FROM associations WHERE object_id='proc'").Scan(&count, &from, &to); err != nil || count != 1 || from == "" || to == "" {
		t.Fatalf("end/start closure %d %q %q %v", count, from, to, err)
	}
}

func TestQueueAdmissionIncludesInFlightBatch(t *testing.T) {
	limits := DefaultLimits()
	limits.QueueRequests = 1
	s, err := Open(testkit.TempDir(t), limits)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	transaction, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- s.Ingest(context.Background(), []Record{record("run", map[string]any{"agent_run_id": "blocked"})})
	}()
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		s.mu.Lock()
		pending := s.pendingRequests
		s.mu.Unlock()
		if pending == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err = s.Ingest(context.Background(), []Record{record("run", map[string]any{"agent_run_id": "overflow"})}); err != ErrCapacity {
		transaction.Rollback()
		t.Fatalf("in-flight request bypassed count bound: %v", err)
	}
	transaction.Rollback()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReplayReportsTruncatedAndOversizedTail(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	if err := s.Ingest(ctx, []Record{record("run", map[string]any{"agent_run_id": "stream"})}); err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	if _, err := Export(ctx, s.db, &data, 0, 100); err != nil {
		t.Fatal(err)
	}
	destination := fixtureStore(t)
	tail := append(append([]byte{}, data.Bytes()...), []byte("{\"schema\":")...)
	if err := destination.Replay(ctx, bytes.NewReader(tail)); err == nil {
		t.Fatal("truncated JSONL tail reported success")
	}
	oversized := strings.NewReader(strings.Repeat("x", int(destination.limits.RequestBytes*2+65537)))
	if err := destination.Replay(ctx, oversized); err == nil {
		t.Fatal("oversized JSONL record reported success")
	}
}

func TestReservationSummaryDoesNotTreatExpiryAsStop(t *testing.T) {
	entry := map[string]any{}
	body := map[string]any{"acquired_at": "2026-01-01T00:00:00Z", "expires_at": "2026-01-01T00:00:01Z", "stopped_at": "2026-01-01T00:00:03Z", "reserved_cpu": nil, "requested_cpu": json.Number("2"), "reserved_memory_bytes": json.Number("1024")}
	edges := []map[string]any{{"start_time": "2026-01-01T00:00:00Z", "end_time": "2026-01-01T00:00:03Z"}}
	holdingSummary(entry, body, edges)
	if fmt.Sprint(entry["allocated_holding_seconds"]) != "3.000000000" || fmt.Sprint(entry["reserved_memory_byte_seconds"]) != "3072.000000000" || entry["reserved_vcpu_seconds"] != nil || fmt.Sprint(entry["requested_vcpu_seconds"]) != "6.000000000" {
		t.Fatalf("invalid allocation accounting: %+v", entry)
	}
}

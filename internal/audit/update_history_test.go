package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUpdateHistoryPersistsStructuredAttemptWithPrivateModes(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	history := UpdateHistory{Home: home, Now: func() time.Time { return now }}
	recorder, err := history.Begin(UpdateRecord{
		OperationID: "op-update-one", Direction: "activate",
		SourceRelease: "1.0.0-aaaaaaaaaaaa", SourceVersion: "1.0.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if err := recorder.Advance("refresh", "1.1.0-bbbbbbbbbbbb", "1.1.0"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if err := recorder.Finish("refresh", "success", "", "1.1.0-bbbbbbbbbbbb", "1.1.0"); err != nil {
		t.Fatal(err)
	}
	records, err := history.Read(10)
	if err != nil || len(records) != 1 {
		t.Fatalf("records=%#v err=%v", records, err)
	}
	record := records[0]
	if record.Status != "success" || record.Phase != "result" || record.Direction != "activate" ||
		record.SourceVersion != "1.0.0" || record.TargetVersion != "1.1.0" || record.FinishedAt.IsZero() ||
		len(record.Events) != 5 || record.Events[0].Phase != "prepare" || record.Events[0].Status != "success" ||
		record.Events[2].Phase != "refresh" || record.Events[3].Phase != "refresh" {
		t.Fatalf("record=%#v", record)
	}
	directory := filepath.Join(home, "logs", "updates")
	info, err := os.Stat(directory)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode=%v err=%v", info.Mode().Perm(), err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
	info, err = entries[0].Info()
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("record mode=%v err=%v", info.Mode().Perm(), err)
	}
}

func TestUpdateHistoryRetainsNewestThirtyAttempts(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	history := UpdateHistory{Home: home, Now: func() time.Time { return now }}
	for index := 0; index < 32; index++ {
		id := fmt.Sprintf("op-update-%02d", index)
		recorder, err := history.Begin(UpdateRecord{OperationID: id, Direction: "activate"})
		if err != nil {
			t.Fatal(err)
		}
		if err := recorder.Finish("execute", "failure", "execution_failed", "", ""); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Second)
	}
	records, err := history.Read(100)
	if err != nil || len(records) != 30 {
		t.Fatalf("retained=%d err=%v", len(records), err)
	}
	if records[0].OperationID != "op-update-31" || records[29].OperationID != "op-update-02" {
		t.Fatalf("retention order=%s..%s", records[0].OperationID, records[29].OperationID)
	}
}

func TestUpdateHistoryKeepsRepeatedOperationIDAsSeparateAttempts(t *testing.T) {
	history := UpdateHistory{Home: t.TempDir()}
	for range 2 {
		recorder, err := history.Begin(UpdateRecord{OperationID: "op-repeated", Direction: "activate"})
		if err != nil {
			t.Fatal(err)
		}
		if err := recorder.Finish("execute", "failure", "execution_failed", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	records, err := history.Read(10)
	if err != nil || len(records) != 2 || records[0].AttemptID == records[1].AttemptID {
		t.Fatalf("repeated operation attempts=%#v err=%v", records, err)
	}
}

func TestUpdateHistoryReopensBegunAttemptAsUnfinished(t *testing.T) {
	history := UpdateHistory{Home: t.TempDir()}
	if _, err := history.Begin(UpdateRecord{OperationID: "op-unfinished", Direction: "activate"}); err != nil {
		t.Fatal(err)
	}
	records, err := history.Read(10)
	if err != nil || len(records) != 1 || records[0].Status != "unfinished" ||
		len(records[0].Events) != 2 || records[0].Events[1].Status != "running" {
		t.Fatalf("unfinished records=%#v err=%v", records, err)
	}
}

func TestUpdateHistoryPreservesRefreshFailureWhenPruningFails(t *testing.T) {
	home := t.TempDir()
	history := UpdateHistory{Home: home}
	recorder, err := history.Begin(UpdateRecord{OperationID: "op-prune-failure", Direction: "activate"})
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(home, "logs", "updates")
	if err := os.WriteFile(filepath.Join(directory, "corrupt.json"), []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Advance("refresh", "release-target", ""); err == nil {
		t.Fatal("expected pruning failure")
	}
	if err := recorder.Finish("refresh", "failure", "refresh_failed", "", ""); err == nil {
		t.Fatal("expected terminal write pruning failure")
	}
	payload, err := os.ReadFile(filepath.Join(directory, recorder.record.AttemptID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var durable UpdateRecord
	if err := json.Unmarshal(payload, &durable); err != nil {
		t.Fatal(err)
	}
	if durable.Status != "failure" || durable.ErrorCode != "refresh_failed" ||
		len(durable.Events) < 4 || durable.Events[len(durable.Events)-2].Phase != "refresh" {
		t.Fatalf("durable refresh failure=%#v", durable)
	}
}

func TestAuditRotationRetainsFiveGenerationsAndViewerReadsAcrossThem(t *testing.T) {
	home := t.TempDir()
	for index := 0; index < 7; index++ {
		if err := WriteInvocation(Invocation{
			Home: home, Command: "status", Arguments: []string{fmt.Sprintf("%02d", index)}, Maximum: 1,
			Now: time.Date(2026, 9, 12, 12, 0, index, 0, time.UTC), PID: index + 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	lines, err := ReadAuditLines(home, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 6 || !strings.Contains(lines[0], "01") || !strings.Contains(lines[5], "06") {
		t.Fatalf("retained audit lines=%q", lines)
	}
	if _, err := os.Stat(filepath.Join(home, "logs", "yard.log.5")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "logs", "yard.log.6")); !os.IsNotExist(err) {
		t.Fatalf("unexpected sixth rotation: %v", err)
	}
}

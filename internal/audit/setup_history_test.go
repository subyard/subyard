package audit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestSetupHistoryKeepsUnfinishedThenTerminalOutcome(t *testing.T) {
	home := testkit.TempDir(t)
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	history := SetupHistory{Home: home, Now: func() time.Time { return now }}
	recorder, err := history.Begin("op-setup-one", "default")
	if err != nil {
		t.Fatal(err)
	}
	// A process killed here leaves the attempt unfinished, not absent.
	records, err := history.Read(10)
	if err != nil || len(records) != 1 || records[0].Status != "unfinished" || records[0].Phase != "execute" ||
		!records[0].FinishedAt.IsZero() {
		t.Fatalf("records=%#v err=%v", records, err)
	}
	now = now.Add(time.Second)
	if err := recorder.Finish(SetupOutcome{Status: "failure", ErrorCode: "execution_failed", Incomplete: true,
		Refusals: []SetupRefusal{{Source: "config", Code: "profile-requires-host-access", Names: []string{"android", "ALLOWS_HOST_ACCESS"}}},
	}); err != nil {
		t.Fatal(err)
	}
	records, err = history.Read(10)
	if err != nil || len(records) != 1 {
		t.Fatalf("records=%#v err=%v", records, err)
	}
	record := records[0]
	if record.Status != "failure" || record.ErrorCode != "execution_failed" || !record.Incomplete ||
		len(record.Refusals) != 1 || strings.Join(record.Refusals[0].Names, ",") != "android,ALLOWS_HOST_ACCESS" ||
		!record.FinishedAt.Equal(now) || record.Yard != "default" {
		t.Fatalf("record=%#v", record)
	}
	entries, err := os.ReadDir(filepath.Join(home, "logs", "setup"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
	if info, err := entries[0].Info(); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("record mode err=%v", err)
	}
	if err := history.Terminal("op-setup-two", "work", "confirmation", SetupOutcome{Status: "declined", ErrorCode: "operator_declined"}); err != nil {
		t.Fatal(err)
	}
	if records, err = history.Read(1); err != nil || len(records) != 1 || records[0].Yard != "work" || records[0].Phase != "confirmation" {
		t.Fatalf("newest=%#v err=%v", records, err)
	}
}

func TestSetupHistoryKeepsOnlyValueFreeRefusals(t *testing.T) {
	home := testkit.TempDir(t)
	history := SetupHistory{Home: home}
	refusals := []SetupRefusal{
		{Source: "config", Code: "unknown-yard"},
		{Source: "config", Code: "bad", Names: []string{"value with spaces /secret"}},
		{Source: "stderr", Code: "anything"},
		{Source: "config", Code: "Bad Code"},
	}
	for index := 0; index < MaxSetupRefusals+2; index++ {
		refusals = append(refusals, SetupRefusal{Source: "config", Code: fmt.Sprintf("code-%d", index)})
	}
	if err := history.Terminal("op-setup", "default", "prepare", SetupOutcome{Status: "failure", ErrorCode: "preparation_failed", Refusals: refusals}); err != nil {
		t.Fatal(err)
	}
	records, err := history.Read(1)
	if err != nil || len(records) != 1 {
		t.Fatalf("records=%#v err=%v", records, err)
	}
	record := records[0]
	if !record.Incomplete || len(record.Refusals) != MaxSetupRefusals || record.OmittedRefusals != 3 ||
		record.Refusals[0].Code != "unknown-yard" || record.Refusals[1].Code != "code-0" {
		t.Fatalf("record=%#v", record)
	}
	for _, refused := range []error{
		history.Terminal("op-setup", "default", "prepare", SetupOutcome{Status: "failure", ErrorCode: "free text"}),
		history.Terminal("op-setup", "Not A Yard", "prepare", SetupOutcome{Status: "failure"}),
		history.Terminal("op-setup", "default", "execute", SetupOutcome{Status: "success"}),
		history.Terminal("op setup", "default", "prepare", SetupOutcome{Status: "failure"}),
	} {
		if refused == nil {
			t.Fatal("an invalid setup attempt was recorded")
		}
	}
	recorder, err := history.Begin("op-setup-three", "default")
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Finish(SetupOutcome{Status: "success", ErrorCode: "execution_failed"}); err == nil {
		t.Fatal("a success with failure facts was recorded")
	}
}

func TestSetupHistoryRetainsNewestThirtyAttempts(t *testing.T) {
	home := testkit.TempDir(t)
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	history := SetupHistory{Home: home, Now: func() time.Time { return now }}
	for index := 0; index < SetupHistoryRetention+3; index++ {
		now = now.Add(time.Second)
		if err := history.Terminal(fmt.Sprintf("op-%02d", index), "default", "prepare", SetupOutcome{Status: "failure"}); err != nil {
			t.Fatal(err)
		}
	}
	records, err := history.Read(100)
	if err != nil || len(records) != SetupHistoryRetention || records[0].OperationID != "op-32" ||
		records[len(records)-1].OperationID != "op-03" {
		t.Fatalf("records=%d err=%v", len(records), err)
	}
}

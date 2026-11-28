package hosterrors

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/audit"
	"github.com/Subyard/Subyard/internal/testkit"
)

const secret = "s3cr3t-value"

func appendRPC(t *testing.T, home string, records ...audit.RPCError) {
	t.Helper()
	for _, record := range records {
		if err := (audit.RPCErrorLog{Home: home}).Append(context.Background(), record); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSnapshotCarriesOnlyFactsOfTheYard(t *testing.T) {
	home := testkit.TempDir(t)
	at := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	appendRPC(t, home,
		audit.RPCError{At: at, Yard: "work", Request: "req-" + secret, Operation: "op-" + secret,
			Method: "operation.plan", Code: "plan_failed", Error: "token=" + secret + " at /home/owner/path"},
		audit.RPCError{At: at.Add(time.Second), Yard: "work", Request: "r2", Method: "operation.plan", Code: "plan_failed", Error: secret},
		audit.RPCError{At: at.Add(2 * time.Second), Yard: "other", Request: "r3", Method: "owner.inventory", Code: "owner_inventory_failed", Error: "other"},
		audit.RPCError{At: at.Add(3 * time.Second), Request: "r4", Method: "x." + secret, Code: secret, Error: secret},
	)
	history := audit.SetupHistory{Home: home, Now: func() time.Time { return at }}
	if err := history.Terminal("op-"+secret, "work", "prepare", audit.SetupOutcome{
		Status: "failure", ErrorCode: "preparation_failed", Incomplete: true,
		Refusals: []audit.SetupRefusal{{Source: "config", Code: "profile-requires-host-access", Names: []string{"ALLOWS_HOST_ACCESS"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := history.Terminal("op-other", "other", "prepare", audit.SetupOutcome{Status: "failure"}); err != nil {
		t.Fatal(err)
	}
	update := audit.UpdateHistory{Home: home, Now: func() time.Time { return at }}
	if err := update.Terminal(audit.UpdateRecord{OperationID: "op-" + secret, Direction: "activate",
		SourceVersion: "0.32.0", TargetVersion: "0.33.0"}, "prepare", "failure", "preparation_failed"); err != nil {
		t.Fatal(err)
	}

	snapshot := Build(home, "work", at.Add(time.Minute))
	payload, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{secret, "/home/owner", "other", "req-", "op-"} {
		if strings.Contains(string(payload), leaked) {
			t.Fatalf("snapshot leaks %q: %s", leaked, payload)
		}
	}
	if len(snapshot.RPC) != 2 || snapshot.RPC[0].Method != "unknown" || snapshot.RPC[0].Code != "unknown" ||
		snapshot.RPC[1].Count != 2 || snapshot.RPC[1].Method != "operation.plan" ||
		snapshot.RPC[1].At != "2026-11-28T10:00:00Z" || snapshot.RPC[1].Last != "2026-11-28T10:00:01Z" {
		t.Fatalf("rpc=%#v", snapshot.RPC)
	}
	if len(snapshot.Setup) != 1 || snapshot.Setup[0].Code != "preparation_failed" || !snapshot.Setup[0].Incomplete ||
		snapshot.Setup[0].Refusals[0].Names[0] != "ALLOWS_HOST_ACCESS" {
		t.Fatalf("setup=%#v", snapshot.Setup)
	}
	if len(snapshot.Updates) != 1 || snapshot.Updates[0].Phase != "prepare" || snapshot.Updates[0].Status != "failure" ||
		snapshot.Updates[0].Source != "0.32.0" || snapshot.Updates[0].Target != "0.33.0" {
		t.Fatalf("updates=%#v", snapshot.Updates)
	}
	if snapshot.GeneratedAt != "2026-11-28T10:01:00.000000000Z" || snapshot.Yard != "work" || len(snapshot.Unavailable) != 0 {
		t.Fatalf("snapshot=%#v", snapshot)
	}
}

func TestSnapshotBoundsSectionsAndNamesUnreadableSources(t *testing.T) {
	home := testkit.TempDir(t)
	at := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	methods := []string{"operation.plan", "owner.inventory"}
	for index := 0; index < SectionEntries+5; index++ {
		appendRPC(t, home, audit.RPCError{At: at.Add(time.Duration(index) * time.Second), Yard: "default",
			Request: "r", Method: methods[index%2], Code: "plan_failed", Error: "e"})
	}
	refusals := []audit.SetupRefusal{}
	for index := 0; index < 20; index++ {
		refusals = append(refusals, audit.SetupRefusal{Source: "config", Code: fmt.Sprintf("code-%d", index), Names: []string{"SETTING"}})
	}
	setup := audit.SetupHistory{Home: home, Now: func() time.Time { return at }}
	for index := 0; index < SectionEntries+2; index++ {
		if err := setup.Terminal(fmt.Sprintf("op-%d", index), "default", "prepare",
			audit.SetupOutcome{Status: "failure", Refusals: refusals}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(home, "logs", "updates"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(home, "logs", "updates", "attempt-broken.json"), []byte("{"), 0o600)

	snapshot := Build(home, "default", at)
	if len(snapshot.RPC) != SectionEntries || snapshot.Omitted.RPC != 5 || snapshot.RPC[0].Method != "operation.plan" ||
		len(snapshot.Setup) != SectionEntries || snapshot.Omitted.Setup != 2 ||
		len(snapshot.Setup[0].Refusals) != SnapshotRefusals || snapshot.Setup[0].OmittedRefusals != 12 ||
		strings.Join(snapshot.Unavailable, ",") != "updates" || len(snapshot.Updates) != 0 {
		t.Fatalf("snapshot=%#v", snapshot)
	}
	payload, err := json.Marshal(snapshot)
	if err != nil || len(payload) > MaxBytes {
		t.Fatalf("payload=%d err=%v", len(payload), err)
	}
	empty := Build(testkit.TempDir(t), "default", at)
	if empty.RPC == nil || empty.Updates == nil || empty.Setup == nil || empty.Unavailable == nil || len(empty.Unavailable) != 0 {
		t.Fatalf("empty=%#v", empty)
	}
}

func TestSnapshotFitDropsOldestEntries(t *testing.T) {
	names := make([]string, 16)
	for index := range names {
		names[index] = strings.Repeat("N", 120)
	}
	snapshot := Snapshot{RPC: []RPCEntry{}, Updates: []UpdateEntry{}, Unavailable: []string{}}
	for index := 0; index < SectionEntries; index++ {
		entry := SetupEntry{Attempt: fmt.Sprintf("attempt-%d", index), Refusals: []Refusal{}}
		for range SnapshotRefusals {
			entry.Refusals = append(entry.Refusals, Refusal{Source: "config", Code: "code", Names: names})
		}
		snapshot.Setup = append(snapshot.Setup, entry)
	}
	snapshot.fit()
	payload, _ := json.Marshal(snapshot)
	if len(payload) > MaxBytes || snapshot.Omitted.Setup == 0 || snapshot.Setup[0].Attempt != "attempt-0" {
		t.Fatalf("payload=%d omitted=%d", len(payload), snapshot.Omitted.Setup)
	}
}

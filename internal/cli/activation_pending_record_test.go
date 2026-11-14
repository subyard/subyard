package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestActivationPendingRecordBindsExactFacts(t *testing.T) {
	home := filepath.Join(testkit.TempDir(t), "config")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	record := activationPendingRecord{Yard: "named", Project: "subyard-named", Instance: "yard-named", Owner: "sample-runtime",
		Actual: strings.Repeat("d", 64), Desired: strings.Repeat("b", 64), Inputs: "inputs-1", Message: "yard named: sample-runtime failed"}
	if _, found, err := pendingActivation(home, record); err != nil || found {
		t.Fatalf("absent record matched: %v %v", found, err)
	}
	if err := writeActivationPendingRecord(home, record); err != nil {
		t.Fatal(err)
	}
	path, _ := activationPendingRecordPath(home, "named", "sample-runtime")
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("record mode: %v %v", info, err)
	}
	stored, found, err := pendingActivation(home, record)
	if err != nil || !found || stored.Message != record.Message {
		t.Fatalf("exact facts did not match: %#v %v %v", stored, found, err)
	}
	for name, change := range map[string]func(*activationPendingRecord){
		"actual":   func(r *activationPendingRecord) { r.Actual = strings.Repeat("e", 64) },
		"desired":  func(r *activationPendingRecord) { r.Desired = strings.Repeat("c", 64) },
		"inputs":   func(r *activationPendingRecord) { r.Inputs = "inputs-2" },
		"instance": func(r *activationPendingRecord) { r.Instance = "yard-other" },
		"project":  func(r *activationPendingRecord) { r.Project = "subyard-other" },
	} {
		current := record
		change(&current)
		if _, found, err := pendingActivation(home, current); err != nil || found {
			t.Fatalf("%s change still matched: %v", name, err)
		}
	}
	if err := removeActivationPendingRecord(home, "named", "sample-runtime"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := pendingActivation(home, record); err != nil || found {
		t.Fatalf("removed record matched: %v %v", found, err)
	}
	for _, payload := range []string{"{broken\n", `{"schemaVersion":1,"yard":"named","owner":"sample-runtime","extra":1}`, `{"schemaVersion":1,"yard":"other","project":"p","instance":"i","owner":"sample-runtime","actual":"a","desired":"b","inputs":"x","message":""}`} {
		testkit.WriteFile(t, path, []byte(payload), 0o600)
		if _, _, err := pendingActivation(home, record); err == nil {
			t.Fatalf("invalid record accepted: %s", payload)
		}
	}
	for _, invalid := range []activationPendingRecord{
		{Yard: "../x", Project: "p", Instance: "i", Owner: "o", Actual: "a", Desired: "b", Inputs: "x"},
		{Yard: "named", Project: "p", Instance: "i", Owner: "o", Actual: "a", Desired: "a", Inputs: "x"},
		{Yard: "named", Project: "p", Instance: "i", Owner: "o", Actual: "a", Desired: "b", Inputs: "x", Message: "line\nbreak"},
	} {
		if err := writeActivationPendingRecord(home, invalid); err == nil {
			t.Fatalf("invalid record written: %#v", invalid)
		}
	}
}

// A crashed persistent write leaves the writer's own temporary file beside the
// records; retirement removes it, and refuses only foreign entries.
func TestYardRecordRetirementIgnoresOwnWriterDebris(t *testing.T) {
	home := filepath.Join(testkit.TempDir(t), "config")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	record := activationPendingRecord{Yard: "named", Project: "p", Instance: "i", Owner: "sample-runtime",
		Actual: strings.Repeat("d", 64), Desired: strings.Repeat("b", 64), Inputs: "x"}
	if err := writeActivationPendingRecord(home, record); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(home, "activation-pending", "v1", "named")
	testkit.WriteFile(t, filepath.Join(directory, ".sample-runtime.json.tmp-123456"), []byte("partial"), 0o600)
	if err := removeYardActivationPendingRecords(home, "named"); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(directory); err != nil || len(entries) != 0 {
		t.Fatalf("records or debris kept: %v %v", entries, err)
	}
	testkit.WriteFile(t, filepath.Join(directory, "foreign.txt"), []byte("x"), 0o600)
	if err := removeYardActivationPendingRecords(home, "named"); err == nil {
		t.Fatal("a foreign entry was accepted")
	}
}

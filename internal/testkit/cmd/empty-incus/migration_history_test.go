package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestHistorySkillObservationRequiresExactSourcesAndSettings(t *testing.T) {
	root := testkit.TempDir(t)
	control := filepath.Join(root, "control")
	files := map[string][]byte{"config/skills/probe.py": []byte("exact probe"), "config/skills/deliver.py": []byte("exact helper")}
	hashes := map[string]string{}
	for name, payload := range files {
		hashes[name] = fmt.Sprintf("%x", sha256.Sum256(payload))
	}
	respond := historyResponder(historyControl{SkillFilesSHA256: hashes}, control)
	for _, change := range []string{"valid", "bytes", "missing", "extra", "user", "profiles", "coding", "program"} {
		t.Run(change, func(t *testing.T) {
			input := map[string]any{"files": map[string][]byte{}, "user": "dev", "profiles": "", "coding": true}
			values := input["files"].(map[string][]byte)
			for name, payload := range files {
				values[name] = payload
			}
			program := string(files["config/skills/probe.py"])
			switch change {
			case "bytes":
				values["config/skills/deliver.py"] = []byte("changed")
			case "missing":
				delete(values, "config/skills/deliver.py")
			case "extra":
				values["unknown"] = []byte("unknown")
			case "user":
				input["user"] = "other"
			case "profiles":
				input["profiles"] = "other"
			case "coding":
				input["coding"] = false
			case "program":
				program = "unknown"
			}
			payload, _ := json.Marshal(input)
			call := testkit.IncusServerExecCall{Project: "subyard", Name: "yard", Command: []string{"python3", "-B", "-c", program}, Stdin: payload}
			step := respond(call)
			code := step.ExitCode
			if step.AfterInput != nil {
				code = step.AfterInput(call)
			}
			if change == "valid" {
				if code != 0 || string(step.Stdout) != "skills-current\n" {
					t.Fatalf("valid skill observation: %+v code=%d", step, code)
				}
			} else if code != 99 {
				t.Fatalf("changed skill observation accepted: %d", code)
			}
		})
	}
}

func TestHistoryResponderFailsOnlyExactPostMigrationApply(t *testing.T) {
	root := testkit.TempDir(t)
	source := filepath.Join(root, "source")
	guest := filepath.Join(root, "guest")
	journal := filepath.Join(root, "journal")
	control := filepath.Join(root, "control")
	testkit.WriteFile(t, source, []byte("desired\n"), 0o600)
	testkit.WriteFile(t, guest, []byte("prior\n"), 0o600)
	testkit.WriteFile(t, journal, []byte(`{"checkpoint":"reconciling","steps":[{"migration":"first","checkpoint":"verified"},{"migration":"second","checkpoint":"verified"}]}`), 0o600)
	if err := os.Symlink("releases/candidate", filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	value := historyControl{Journal: journal, Runtime: root, Target: "candidate", Source: source, Guest: guest, Destination: "/home/dev/.subyard-fixture/config.txt", Migrations: []string{"first", "second"}, FailOnce: true}
	respond := historyResponder(value, control)
	call := testkit.IncusServerExecCall{Project: "subyard", Name: "yard", Command: []string{"sh", "-eu", "-c", configWriteScript, "subyard", value.Destination, "1000"}, Stdin: []byte("desired\n")}
	verified, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{
		`{"checkpoint":"activating","steps":[{"migration":"first","checkpoint":"verified"},{"migration":"second","checkpoint":"verified"}]}`,
		`{"checkpoint":"reconciling","steps":[{"migration":"first","checkpoint":"verified"},{"migration":"second","checkpoint":"applied"}]}`,
	} {
		testkit.WriteFile(t, journal, []byte(invalid), 0o600)
		if respond(call).AfterInput(call) != 99 {
			t.Fatal("apply accepted before exact verified reconciling boundary")
		}
		if _, err := os.Stat(control + ".failed"); !os.IsNotExist(err) {
			t.Fatal("refused apply consumed the controlled failure")
		}
		payload, _ := os.ReadFile(guest)
		if string(payload) != "prior\n" {
			t.Fatal("refused apply changed guest bytes")
		}
	}
	testkit.WriteFile(t, journal, verified, 0o600)
	if err := os.Remove(control + ".unexpected"); err != nil {
		t.Fatal(err)
	}
	step := respond(call)
	if step.AfterInput == nil || step.AfterInput(call) != 37 {
		t.Fatal("exact reconciling apply did not fail once")
	}
	before, _ := os.ReadFile(guest)
	if string(before) != "prior\n" {
		t.Fatal("failed apply changed simulated guest bytes")
	}
	if respond(call).AfterInput(call) != 0 {
		t.Fatal("ordinary retry did not succeed")
	}
	after, _ := os.ReadFile(guest)
	if !bytes.Equal(after, call.Stdin) {
		t.Fatal("retry did not apply exact captured stdin")
	}
	call.Stdin = []byte("foreign\n")
	if respond(call).AfterInput(call) != 99 {
		t.Fatal("foreign input accepted")
	}
	final, _ := os.ReadFile(guest)
	if !bytes.Equal(final, after) {
		t.Fatal("foreign input changed guest bytes")
	}
	call.Command = []string{"sh", "-c", "unknown"}
	if respond(call).ExitCode != 99 {
		t.Fatal("unknown guest effect accepted")
	}
}

func TestHistoryDriftStartsOnlyAfterVerifiedCandidateActivation(t *testing.T) {
	root := testkit.TempDir(t)
	guest, drift, journal, control := filepath.Join(root, "guest"), filepath.Join(root, "drift"), filepath.Join(root, "journal"), filepath.Join(root, "control")
	testkit.WriteFile(t, guest, []byte("desired\n"), 0o600)
	testkit.WriteFile(t, drift, []byte("prior\n"), 0o600)
	testkit.WriteFile(t, journal, []byte(`{"checkpoint":"complete"}`), 0o600)
	if err := os.Symlink("releases/prior", filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	value := historyControl{Journal: journal, Runtime: root, Target: "candidate", Guest: guest, InitialDrift: drift, Destination: "/home/dev/.subyard-fixture/config.txt", Migrations: []string{"one"}}
	respond := historyResponder(value, control)
	call := testkit.IncusServerExecCall{Project: "subyard", Name: "yard", Command: []string{"sha256sum", "--", value.Destination}}
	if respond(call).ExitCode != 0 {
		t.Fatal("predecessor observation failed")
	}
	before, _ := os.ReadFile(guest)
	if string(before) != "desired\n" {
		t.Fatal("fixture made predecessor unready")
	}
	if err := os.Remove(filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/candidate", filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, journal, []byte(`{"checkpoint":"reconciling","steps":[{"migration":"one","checkpoint":"applied"}]}`), 0o600)
	respond(call)
	before, _ = os.ReadFile(guest)
	if string(before) != "desired\n" {
		t.Fatal("drift armed before verified ledger")
	}
	testkit.WriteFile(t, journal, []byte(`{"checkpoint":"reconciling","steps":[{"migration":"one","checkpoint":"verified"}]}`), 0o600)
	if respond(call).ExitCode != 0 {
		t.Fatal("verified candidate observation failed")
	}
	after, _ := os.ReadFile(guest)
	if string(after) != "prior\n" {
		t.Fatal("fixture did not expose known post-migration drift")
	}
	if _, err := os.Stat(control + ".failed"); !os.IsNotExist(err) {
		t.Fatal("read observation consumed apply failure")
	}
	testkit.WriteFile(t, guest, []byte("desired\n"), 0o600)
	respond(call)
	after, _ = os.ReadFile(guest)
	if string(after) != "desired\n" {
		t.Fatal("fixture reintroduced drift after successful repair")
	}
}

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
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
	respond := historyResponder(historyControl{Skills: &historySkills{Files: hashes, ProgramSHA256: hashes["config/skills/probe.py"], User: "dev", Profiles: "", Coding: true}}, control)
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

func TestHistoryProfileRecorderOnlyAcceptsExactAbsentFactsRequest(t *testing.T) {
	root := testkit.TempDir(t)
	program := "synthetic exact recorder"
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(program)))
	respond := historyResponder(historyControl{ProfileRecorderSHA256: hash, ProfileNames: []string{"sample"}}, filepath.Join(root, "control"))
	for _, scenario := range []string{"valid", "program", "yard", "profile", "selected", "unknown", "trailing"} {
		t.Run(scenario, func(t *testing.T) {
			command := []string{"python3", "-B", "-c", program}
			input := map[string]any{"yard": map[string]string{"name": "default", "project": "subyard", "instance": "yard"}, "profile": "sample", "selected": false}
			switch scenario {
			case "program":
				command[3] = "different recorder"
			case "yard":
				input["yard"] = map[string]string{"name": "other", "project": "subyard", "instance": "yard"}
			case "profile":
				input["profile"] = "foreign"
			case "selected":
				input["selected"] = true
			case "unknown":
				input["extra"] = "ignored"
			}
			payload, _ := json.Marshal(input)
			if scenario == "trailing" {
				payload = append(payload, []byte(" {}")...)
			}
			call := testkit.IncusServerExecCall{Project: "subyard", Name: "yard", Command: command, Stdin: payload}
			step := respond(call)
			code := step.ExitCode
			if step.AfterInput != nil {
				code = step.AfterInput(call)
			}
			if scenario == "valid" && code != 10 || scenario != "valid" && code != 99 {
				t.Fatalf("scenario %s: exit=%d", scenario, code)
			}
		})
	}
}

func TestHistoryGuestFactsObservationsRequireExactAbsentFactsRequest(t *testing.T) {
	root := testkit.TempDir(t)
	program := "synthetic exact profile observation"
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(program)))
	respond := historyResponder(historyControl{ProfileSelectionSHA256: hash, GuestFactsVersion: "1.0.0-test"}, filepath.Join(root, "control"))
	for _, scenario := range []string{"valid", "program", "yard", "profiles", "version", "unknown", "trailing"} {
		t.Run(scenario, func(t *testing.T) {
			command := []string{"python3", "-B", "-c", program}
			input := map[string]any{"version": "1.0.0-test", "yard": map[string]string{"name": "default", "project": "subyard", "instance": "yard"}, "selected_profiles": []string{}}
			switch scenario {
			case "program":
				command[3] = "different"
			case "yard":
				input["yard"] = map[string]string{"name": "other", "project": "subyard", "instance": "yard"}
			case "profiles":
				input["selected_profiles"] = []string{"sample"}
			case "version":
				input["version"] = "other"
			case "unknown":
				input["extra"] = true
			}
			payload, _ := json.Marshal(input)
			if scenario == "trailing" {
				payload = append(payload, []byte(" {}")...)
			}
			call := testkit.IncusServerExecCall{Project: "subyard", Name: "yard", Command: command, Stdin: payload}
			step := respond(call)
			code := step.ExitCode
			if step.AfterInput != nil {
				code = step.AfterInput(call)
			}
			if scenario == "valid" && code != 10 || scenario != "valid" && code != 99 {
				t.Fatalf("%s: exit=%d", scenario, code)
			}
		})
	}
}

func TestHistorySettingsCaptureRequiresVerifiedBoundaryAndClosedEvidence(t *testing.T) {
	root := testkit.TempDir(t)
	source, journal := filepath.Join(root, "source"), filepath.Join(root, "journal")
	testkit.WriteFile(t, source, []byte("desired"), 0o600)
	testkit.WriteFile(t, journal, []byte(`{"checkpoint":"reconciling","steps":[{"migration":"first","checkpoint":"verified"}]}`), 0o600)
	if err := os.Symlink("releases/candidate", filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	program := "exact inventory program"
	control := historyControl{Source: source, Journal: journal, Runtime: root, Target: "candidate", Destination: "/home/dev/.subyard-fixture/config.txt", Migrations: []string{"first"}, InventorySHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(program)))}
	respond := historyResponder(control, filepath.Join(root, "control"))
	for _, scenario := range []string{"valid", "valid-files", "wrong-file-path", "wrong-file-digest", "foreign-file", "extra-file-field", "unknown-field", "unknown-diagnostic", "wrong-expected", "bad-key", "unverified"} {
		t.Run(scenario, func(t *testing.T) {
			input := map[string]any{"home": "/home/dev", "uid": 1000, "developer": "dev", "legacy": []string{}, "adopt": false, "expected": strings.Repeat("a", 64), "preview_skill_cutover": false, "applied_settings": map[string]string{}, "diagnostics": map[string]any{"key": strings.Repeat("a", 64), "digests": map[string]string{}}, "entries": []map[string]string{
				{"id": "fixture", "kind": "file", "path": control.Destination, "digest": fmt.Sprintf("%x", sha256.Sum256([]byte("desired")))},
				{"id": "fixture", "kind": "package"}, {"id": "_projects", "kind": "file", "path": "/usr/local/libexec/subyard/projects-changed"}, {"id": "_projects", "kind": "file", "path": "/etc/subyard/agent-project-hooks"},
			}}
			valid := scenario == "valid" || scenario == "valid-files"
			if scenario == "valid-files" || strings.Contains(scenario, "file") {
				key := bytes.Repeat([]byte{0xaa}, 32)
				file := map[string]string{"path": source, "digest": config.ConfigurationInputDigest("AGENT_fixture_CONFIG", config.SettingFile, key, []byte("desired"))}
				files := map[string]any{"AGENT_fixture_CONFIG": file}
				switch scenario {
				case "wrong-file-path":
					file["path"] = "/foreign"
				case "wrong-file-digest":
					file["digest"] = strings.Repeat("b", 64)
				case "foreign-file":
					files = map[string]any{"AGENT_other_CONFIG": file}
				case "extra-file-field":
					file["unexpected"] = "value"
				}
				input["diagnostics"].(map[string]any)["files"] = files
			}
			switch scenario {
			case "unknown-field":
				input["unexpected"] = true
			case "unknown-diagnostic":
				input["diagnostics"].(map[string]any)["extra"] = true
			case "wrong-expected":
				input["expected"] = strings.Repeat("b", 64)
			case "bad-key":
				input["diagnostics"] = map[string]any{"key": "bad", "digests": map[string]string{}}
			case "unverified":
				testkit.WriteFile(t, journal, []byte(`{"checkpoint":"activating","steps":[]}`), 0o600)
				defer testkit.WriteFile(t, journal, []byte(`{"checkpoint":"reconciling","steps":[{"migration":"first","checkpoint":"verified"}]}`), 0o600)
			}
			payload, _ := json.Marshal(input)
			call := testkit.IncusServerExecCall{Project: "subyard", Name: "yard", Command: []string{"python3", "-B", "-c", program, "capture-settings"}, Stdin: payload}
			step := respond(call)
			code := step.ExitCode
			if step.AfterInput != nil {
				code = step.AfterInput(call)
			}
			if valid && code != 0 || !valid && code != 99 {
				t.Fatalf("%s: exit=%d", scenario, code)
			}
			if valid {
				readPayload, _ := json.Marshal(map[string]any{"home": "/home/dev", "uid": 1000, "developer": "dev", "entries": nil, "expected": "", "legacy": []string{}, "adopt": false, "preview_skill_cutover": false})
				read := testkit.IncusServerExecCall{Project: "subyard", Name: "yard", Command: []string{"python3", "-B", "-c", program, "settings"}, Stdin: readPayload}
				observed := respond(read)
				var value struct {
					AppliedSettings map[string]string `json:"applied_settings"`
					Diagnostics     *struct {
						Key string `json:"key"`
					} `json:"diagnostics"`
				}
				if observed.AfterInput == nil || observed.AfterInput(read) != 0 || json.Unmarshal(observed.Stdout, &value) != nil || value.AppliedSettings == nil || value.Diagnostics == nil || value.Diagnostics.Key != strings.Repeat("a", 64) {
					t.Fatalf("captured baseline not readable: %s", observed.Stdout)
				}
				effects, _ := os.ReadFile(filepath.Join(root, "control.effects.jsonl"))
				delete(input, "applied_settings")
				delete(input, "diagnostics")
				input["expected"] = ""
				observationPayload, _ := json.Marshal(input)
				observation := testkit.IncusServerExecCall{Project: "subyard", Name: "yard", Command: []string{"python3", "-B", "-c", program, "observe-applied"}, Stdin: observationPayload}
				step := respond(observation)
				if step.AfterInput == nil || step.AfterInput(observation) != 0 {
					t.Fatal("applied observation refused")
				}
				after, _ := os.ReadFile(filepath.Join(root, "control.effects.jsonl"))
				if !bytes.Equal(effects, after) {
					t.Fatal("applied observation produced an effect")
				}
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

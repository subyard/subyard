package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/testkit"
)

type historyControl struct {
	DiskTmpCheckSHA256 string   `json:"diskTmpCheckSHA256"`
	Journal            string   `json:"journal"`
	Runtime            string   `json:"runtime"`
	Target             string   `json:"target"`
	Source             string   `json:"source"`
	Guest              string   `json:"guest"`
	InitialDrift       string   `json:"initialDrift"`
	Destination        string   `json:"destination"`
	Migrations         []string `json:"migrations"`
	FailOnce           bool     `json:"failOnce"`
	InventorySHA256    string   `json:"inventoryProgramSHA256"`
	guestContract
}

// persistContract is the exact persist reader and the mappings the candidate
// derives for the fixture's selection, deselected links included. The fixture
// guest holds no data at any of them.
type persistContract struct {
	PersistSHA256   string                            `json:"persistProgramSHA256"`
	PersistMappings []reconcileruntime.PersistMapping `json:"persistMappings"`
}

type historySkills struct {
	Files         map[string]string `json:"files"`
	ProgramSHA256 string            `json:"programSHA256"`
	User          string            `json:"user"`
	Profiles      string            `json:"profiles"`
	Coding        bool              `json:"coding"`
}

var historyMu sync.Mutex

//go:embed config-write.sh
var configWriteScript string

func historyResponder(control historyControl, path string) func(testkit.IncusServerExecCall) testkit.IncusServerExecStep {
	return func(call testkit.IncusServerExecCall) testkit.IncusServerExecStep {
		reject := func() testkit.IncusServerExecStep {
			payload, _ := json.Marshal(call)
			_ = os.WriteFile(path+".unexpected", payload, 0o600)
			return testkit.IncusServerExecStep{ExitCode: 99, Stderr: []byte("unsupported migration history fixture command\n")}
		}
		if call.Project != "subyard" || call.Name != "yard" {
			return reject()
		}
		command := call.Command
		if slices.Equal(command, []string{"sh", "-s", "--", "check"}) {
			return diskTmpCheckResponse(control.DiskTmpCheckSHA256)
		}
		programSHA := ""
		if len(command) > 3 {
			programSHA = fmt.Sprintf("%x", sha256.Sum256([]byte(command[3])))
		}
		if step, ok := control.guestContract.respond(command, reject); ok {
			return step
		}
		if len(command) == 4 && command[0] == "test" && command[1] == "!" && command[2] == "-e" && command[3] == "/var/lib/subyard/integrations/inventory.json" {
			return testkit.IncusServerExecStep{ExitCode: 1}
		}
		if len(command) == 11 && command[0] == "sh" && command[1] == "-eu" && command[2] == "-c" && command[4] == "subyard" &&
			(programSHA == "a01443d0ba01dfecb656b59ab5c0e32e84c9a57445354d1832c37e2b63ea1851" || programSHA == "61f1199f9be495a3427b873b7a677f10bc663908324357062657d7aa7890be48") &&
			(command[5] == "1a99d38f52be1c6aead189ccdbd2f16bd24be92e3110c9915ee6624e7e0b1ddf" || command[5] == "bfcc059c108b8bc9af4b06b01beba490357d72ef02b697b49a796386478423e0" || command[5] == "1abec2e90dd14f1a2e860eb35bba95e97a50ee7958020c9578a394b02ca70d53") &&
			command[6] == "01ba4719c80b6fe911b091a7c05124b64eeece964e09c058ef8f9805daca546b" &&
			command[7] == "/usr/local/libexec/subyard/projects-changed" && command[8] == "/etc/subyard/agent-project-hooks" &&
			command[9] == "/usr/local/libexec/subyard/projects-changed.d" && command[10] == "0:0" {
			return testkit.IncusServerExecStep{}
		}

		if len(command) == 7 && command[0] == "sh" && command[1] == "-eu" && command[2] == "-c" && programSHA == "8195718501349e512581cbebc2b569cce3f86ab129873b6b5f78def1a99a9a2f" && command[4] == "subyard" && command[5] == "observe" && command[6] == "true" {
			return testkit.IncusServerExecStep{Stdout: []byte("changed=0\n")}
		}
		if len(command) == 8 && command[0] == "sh" && command[1] == "-eu" && command[2] == "-c" && (programSHA == "a320d966cbd1e55825c506d7de86331caab144c3b18b5acc442191decd8531de" || programSHA == "fb927909c321199bd8846421bc0e29fc2b62c8642fd0aa2d24c84323839037f2" || programSHA == "4ba7a0856a03cb4fd293c9aa217be900b3f1e1df433b466cc5de94bd35ef6a26") && command[4] == "subyard" && command[5] == "observe" && command[6] == "fixture" && command[7] == "true" {
			return testkit.IncusServerExecStep{Stdout: []byte("changed=0\n")}
		}
		if len(command) == 5 && command[0] == "sh" && command[1] == "-c" && command[2] == `command -v "$1" >/dev/null` && command[3] == "subyard" && command[4] == "fixture" {
			return testkit.IncusServerExecStep{}
		}
		if len(command) == 6 && command[0] == "sh" && command[1] == "-eu" && command[2] == "-c" && command[3] == `command -v python3 >/dev/null; id "$1" >/dev/null; test -d /srv/agents` && command[4] == "subyard" && command[5] == "dev" {
			return testkit.IncusServerExecStep{}
		}
		if len(command) == 3 && command[0] == "sha256sum" && command[1] == "--" && command[2] == control.Destination {
			if control.InitialDrift != "" {
				historyMu.Lock()
				if _, err := os.Stat(path + ".drift"); os.IsNotExist(err) && historyApplyBoundary(control) {
					drift, err := os.ReadFile(control.InitialDrift)
					if err == nil {
						err = os.WriteFile(control.Guest, drift, 0o600)
					}
					if err == nil {
						err = os.WriteFile(path+".drift", []byte("armed at verified activation boundary\n"), 0o600)
					}
					if err != nil {
						historyMu.Unlock()
						return reject()
					}
				}
				historyMu.Unlock()
			}
			payload, err := os.ReadFile(control.Guest)
			if err != nil {
				return testkit.IncusServerExecStep{ExitCode: 1}
			}
			return testkit.IncusServerExecStep{Stdout: []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(payload), control.Destination))}
		}
		if inventoryReadMode(command, control.InventorySHA256) != "" {
			stored, err := storedInventory(path+".inventory.json", `{"installed":true,"applied_settings":null,"diagnostics":null,"selected":["fixture"]}`)
			if err != nil {
				return reject()
			}
			return inventoryReadStep(stored, reject)
		}
		if len(command) == 5 && command[0] == "python3" && command[1] == "-B" && command[2] == "-c" && (programSHA == control.InventorySHA256 || programSHA == "2193bc75e21ea70024008739bb74004f92a2d3ffc5894de7ca017f2d4291be2e") && (command[4] == "configs-prepare" || command[4] == "configs-commit" || command[4] == "observe" || (command[4] == "capture-settings" || command[4] == "observe-applied") && programSHA == control.InventorySHA256) {
			output := []byte(nil)
			if command[4] == "observe" || command[4] == "observe-applied" {
				output = []byte(`{"fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","changed":false,"facts":[]}`)
			}
			return testkit.IncusServerExecStep{Stdout: output, AfterInput: func(actual testkit.IncusServerExecCall) int {
				var input settingsCapture
				source, err := os.ReadFile(control.Source)
				if err != nil || json.Unmarshal(actual.Stdin, &input) != nil || input.Home != "/home/dev" || input.Developer != "dev" || input.UID != 1000 {
					reject()
					return 99
				}
				wanted := 1
				if command[4] == "observe" || command[4] == "observe-applied" || command[4] == "capture-settings" {
					wanted = 4
				}
				if len(input.Entries) != wanted {
					reject()
					return 99
				}
				for _, entry := range input.Entries {
					valid := entry.ID == "fixture" && entry.Kind == "file" && entry.Path == control.Destination && entry.Digest == fmt.Sprintf("%x", sha256.Sum256(source))
					if command[4] == "observe" || command[4] == "observe-applied" || command[4] == "capture-settings" {
						valid = valid || entry.ID == "fixture" && entry.Kind == "package" && entry.Path == "" || entry.ID == "_projects" && entry.Kind == "file" && (entry.Path == "/usr/local/libexec/subyard/projects-changed" || entry.Path == "/etc/subyard/agent-project-hooks")
					}
					if !valid {
						reject()
						return 99
					}
				}
				if command[4] == "capture-settings" {
					captured, ok := settingsCaptureRequest(actual.Stdin)
					if !ok || len(captured.Diagnostics.Files) > 1 || !historyApplyBoundary(control) {
						reject()
						return 99
					}
					for name, encoded := range captured.Diagnostics.Files {
						var file map[string]string
						key, err := hex.DecodeString(captured.Diagnostics.Key)
						if err != nil || name != "AGENT_fixture_CONFIG" || json.Unmarshal(encoded, &file) != nil || len(file) != 2 || file["path"] != control.Source || file["digest"] != config.ConfigurationInputDigest(name, config.SettingFile, key, source) {
							reject()
							return 99
						}
					}
					if storeInventory(path+".inventory.json", captured.inventory("fixture")) != nil {
						return 99
					}
				}
				if command[4] != "observe" && command[4] != "observe-applied" {
					historyMu.Lock()
					defer historyMu.Unlock()
					line, _ := json.Marshal(map[string]any{"inventory": command[4], "stdinSHA256": fmt.Sprintf("%x", sha256.Sum256(actual.Stdin)), "exit": 0})
					file, err := os.OpenFile(path+".effects.jsonl", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
					if err != nil {
						return 99
					}
					_, err = file.Write(append(line, '\n'))
					_ = file.Close()
					if err != nil {
						return 99
					}
				}
				return 0
			}}
		}
		if len(command) == 8 && command[0] == "sh" && command[1] == "-eu" && command[2] == "-c" && command[3] == configWriteScript && command[4] == "subyard" && command[5] == control.Destination && command[6] == "1000" {
			expected, err := os.ReadFile(control.Source)
			if err != nil {
				return reject()
			}
			return testkit.IncusServerExecStep{Stderr: []byte("controlled materialized config apply fixture\n"), AfterInput: func(actual testkit.IncusServerExecCall) int {
				historyMu.Lock()
				defer historyMu.Unlock()
				if !bytes.Equal(actual.Stdin, expected) || command[7] != fmt.Sprintf("%x", sha256.Sum256(actual.Stdin)) {
					reject()
					return 99
				}
				if !historyApplyBoundary(control) {
					reject()
					return 99
				}
				payload, err := os.ReadFile(control.Journal)
				if err != nil {
					reject()
					return 99
				}

				event := map[string]any{"command": actual.Command, "stdinSHA256": fmt.Sprintf("%x", sha256.Sum256(actual.Stdin)), "transaction": json.RawMessage(payload)}
				code := 0
				if _, err := os.Stat(path + ".failed"); control.FailOnce && os.IsNotExist(err) {
					_ = os.WriteFile(path+".failed", []byte("failed once\n"), 0o600)
					code = 37
				} else if os.WriteFile(control.Guest, actual.Stdin, 0o600) != nil {
					code = 99
				}
				event["exit"] = code
				line, _ := json.Marshal(event)
				file, err := os.OpenFile(path+".effects.jsonl", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
				if err == nil {
					_, _ = file.Write(append(line, '\n'))
					_ = file.Close()
				}
				return code
			}}
		}
		return reject()
	}
}

// guestContract is the exact read-only guest programs a fixture guest without
// guest facts or persist data answers, shared by every owned fixture.
type guestContract struct {
	ProfileRecorderSHA256  string         `json:"profileRecorderProgramSHA256"`
	ProfileSelectionSHA256 string         `json:"profileSelectionProgramSHA256"`
	GuestIdentitySHA256    string         `json:"guestIdentityProgramSHA256"`
	GuestFactsVersion      string         `json:"guestFactsVersion"`
	ProfileNames           []string       `json:"profileNames"`
	Skills                 *historySkills `json:"skills,omitempty"`
	persistContract
}

func (contract guestContract) respond(command []string, reject func() testkit.IncusServerExecStep) (testkit.IncusServerExecStep, bool) {
	programSHA := ""
	if len(command) > 3 {
		programSHA = fmt.Sprintf("%x", sha256.Sum256([]byte(command[3])))
	}
	if contract.PersistSHA256 != "" && len(command) == 5 && slices.Equal(command[:3], []string{"python3", "-B", "-c"}) &&
		programSHA == contract.PersistSHA256 && command[4] == "observe" {
		return contract.persistObservation(reject), true
	}
	// This fixture has no guest facts. The exact recorder must therefore
	// return its missing-facts exit without executing a guest-side writer.
	if contract.ProfileRecorderSHA256 != "" && len(command) == 4 && command[0] == "python3" && command[1] == "-B" && command[2] == "-c" && programSHA == contract.ProfileRecorderSHA256 {
		return testkit.IncusServerExecStep{AfterInput: func(actual testkit.IncusServerExecCall) int {
			var input struct {
				Yard     struct{ Name, Project, Instance string } `json:"yard"`
				Profile  string                                   `json:"profile"`
				Selected *bool                                    `json:"selected"`
			}
			decoder := json.NewDecoder(bytes.NewReader(actual.Stdin))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF || input.Yard.Name != "default" || input.Yard.Project != "subyard" || input.Yard.Instance != "yard" || input.Selected == nil || *input.Selected || !slices.Contains(contract.ProfileNames, input.Profile) {
				reject()
				return 99
			}
			return 10
		}}, true
	}
	if len(command) == 4 && command[0] == "python3" && command[1] == "-B" && command[2] == "-c" && programSHA != "" && (programSHA == contract.ProfileSelectionSHA256 || programSHA == contract.GuestIdentitySHA256) {
		return testkit.IncusServerExecStep{AfterInput: func(actual testkit.IncusServerExecCall) int {
			var input struct {
				Version          string                                   `json:"version"`
				Yard             struct{ Name, Project, Instance string } `json:"yard"`
				SelectedProfiles []string                                 `json:"selected_profiles"`
			}
			decoder := json.NewDecoder(bytes.NewReader(actual.Stdin))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF || input.Version != contract.GuestFactsVersion || input.Yard.Name != "default" || input.Yard.Project != "subyard" || input.Yard.Instance != "yard" || input.SelectedProfiles == nil || len(input.SelectedProfiles) != 0 {
				reject()
				return 99
			}
			return 10
		}}, true
	}
	if contract.Skills != nil && len(command) == 4 && command[0] == "python3" && command[1] == "-B" && command[2] == "-c" && programSHA == contract.Skills.ProgramSHA256 {
		return testkit.IncusServerExecStep{Stdout: []byte("skills-current\n"), AfterInput: func(actual testkit.IncusServerExecCall) int {
			var input struct {
				Files    map[string][]byte `json:"files"`
				User     *string           `json:"user"`
				Profiles *string           `json:"profiles"`
				Coding   *bool             `json:"coding"`
			}
			decoder := json.NewDecoder(bytes.NewReader(actual.Stdin))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&input) != nil || input.User == nil || *input.User != contract.Skills.User || input.Profiles == nil || *input.Profiles != contract.Skills.Profiles || input.Coding == nil || *input.Coding != contract.Skills.Coding || len(input.Files) != len(contract.Skills.Files) {
				reject()
				return 99
			}
			if decoder.Decode(new(any)) != io.EOF {
				reject()
				return 99
			}
			for name, digest := range contract.Skills.Files {
				payload, ok := input.Files[name]
				if !ok || fmt.Sprintf("%x", sha256.Sum256(payload)) != digest {
					reject()
					return 99
				}
			}
			return 0
		}}, true
	}
	return testkit.IncusServerExecStep{}, false
}

// inventoryReadStep answers only a read-only settings or selection request.
func inventoryReadStep(output []byte, reject func() testkit.IncusServerExecStep) testkit.IncusServerExecStep {
	return testkit.IncusServerExecStep{Stdout: output, AfterInput: func(actual testkit.IncusServerExecCall) int {
		var input settingsCapture
		var fields map[string]json.RawMessage
		if json.Unmarshal(actual.Stdin, &input) != nil || json.Unmarshal(actual.Stdin, &fields) != nil || len(fields) != 8+len(historyCandidateReadFields(fields)) || input.Home != "/home/dev" || input.Developer != "dev" || input.UID != 1000 || len(input.Entries) != 0 || input.Expected != "" || input.Legacy == nil || input.Adopt == nil || *input.Adopt || input.Preview == nil {
			reject()
			return 99
		}
		for _, key := range []string{"home", "uid", "developer", "entries", "legacy", "adopt", "expected", "preview_skill_cutover"} {
			if _, ok := fields[key]; !ok {
				reject()
				return 99
			}
		}
		return 0
	}}
}

// inventoryReadMode returns the mode of an exact settings or selection read of
// the reader with the given program digest, or "" for any other command.
func inventoryReadMode(command []string, reader string) string {
	if len(command) == 5 && slices.Equal(command[:3], []string{"python3", "-B", "-c"}) && (command[4] == "settings" || command[4] == "selection") &&
		reader != "" && fmt.Sprintf("%x", sha256.Sum256([]byte(command[3]))) == reader {
		return command[4]
	}
	return ""
}

// storedInventory returns the inventory a settings capture stored at file, or
// fallback before any capture or without a store.
func storedInventory(file, fallback string) ([]byte, error) {
	if file == "" {
		return []byte(fallback), nil
	}
	historyMu.Lock()
	defer historyMu.Unlock()
	stored, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return []byte(fallback), nil
	}
	return stored, err
}

func storeInventory(file string, payload []byte) error {
	historyMu.Lock()
	defer historyMu.Unlock()
	return os.WriteFile(file, payload, 0o600)
}

// settingsCapture is a candidate's applied-settings capture request; its
// entries and diagnostic files stay for the owning fixture to check.
type settingsCapture struct {
	Home, Developer string
	UID             int
	Expected        string   `json:"expected"`
	Legacy          []string `json:"legacy"`
	Adopt           *bool    `json:"adopt"`
	Preview         *bool    `json:"preview_skill_cutover"`
	Entries         []struct{ ID, Kind, Path, Digest string }
	AppliedSettings map[string]string `json:"applied_settings"`
	Diagnostics     struct {
		Key     string                     `json:"key"`
		Digests map[string]string          `json:"digests"`
		Files   map[string]json.RawMessage `json:"files,omitempty"`
	} `json:"diagnostics"`
}

// settingsCaptureRequest validates shared capture fields; each fixture checks its artifacts.
func settingsCaptureRequest(payload []byte) (settingsCapture, bool) {
	var input settingsCapture
	var fields, diagnostics map[string]json.RawMessage
	if json.Unmarshal(payload, &input) != nil || json.Unmarshal(payload, &fields) != nil || len(fields) != 10+len(historyCandidateReadFields(fields)) ||
		input.Home != "/home/dev" || input.Developer != "dev" || input.UID != 1000 || input.Expected != strings.Repeat("a", 64) ||
		input.Legacy == nil || input.Adopt == nil || *input.Adopt || input.Preview == nil ||
		input.AppliedSettings == nil || (config.Catalog{}).ValidateIntegrationInitSettings(input.AppliedSettings) != nil ||
		json.Unmarshal(fields["diagnostics"], &diagnostics) != nil || len(diagnostics) < 2 || len(diagnostics) > 3 || input.Diagnostics.Digests == nil ||
		!(config.Catalog{}).ValidIntegrationDiagnosticEvidence(input.Diagnostics.Key, input.Diagnostics.Digests) {
		return settingsCapture{}, false
	}
	for _, key := range []string{"home", "uid", "developer", "entries", "legacy", "adopt", "expected", "preview_skill_cutover", "applied_settings", "diagnostics"} {
		if _, ok := fields[key]; !ok {
			return settingsCapture{}, false
		}
	}
	for key := range diagnostics {
		if key != "key" && key != "digests" && key != "files" {
			return settingsCapture{}, false
		}
	}
	return input, true
}

// inventory is what a settings read returns after this capture.
func (capture settingsCapture) inventory(selected ...string) []byte {
	stored, _ := json.Marshal(map[string]any{"installed": true, "applied_settings": capture.AppliedSettings, "diagnostics": capture.Diagnostics, "selected": append([]string{}, selected...)})
	return stored
}

// historyCandidateReadFields accepts empty persist evidence and retained pending intent.
func historyCandidateReadFields(fields map[string]json.RawMessage) []string {
	present := []string{}
	if value, ok := fields["persist"]; ok && (string(value) == "null" || string(value) == "{}") {
		present = append(present, "persist")
	}
	if value, ok := fields["retain_pending_intent"]; ok && string(value) == "true" {
		present = append(present, "retain_pending_intent")
	}
	return present
}

// persistObservation answers the exact persist reader with what the real
// program reports for declared links whose parent directory is absent: one
// entry per mapping without operations. Any other request is rejected.
func (control persistContract) persistObservation(reject func() testkit.IncusServerExecStep) testkit.IncusServerExecStep {
	entries := make([]map[string]any, 0, len(control.PersistMappings))
	for _, mapping := range control.PersistMappings {
		entries = append(entries, map[string]any{"integration": mapping.Integration, "source": mapping.Source, "target": mapping.Target,
			"selected": mapping.Selected, "ops": []string{}, "id": "", "state": "none", "persist": "", "files": 0, "bytes": 0,
			"existing": []string{}, "existing_count": 0, "identity": nil, "target_identity": nil, "retirable": false})
	}
	output, _ := json.Marshal(map[string]any{"fingerprint": strings.Repeat("b", 64), "entries": entries})
	return testkit.IncusServerExecStep{Stdout: output, AfterInput: func(actual testkit.IncusServerExecCall) int {
		var input struct {
			Yard      string                            `json:"yard"`
			UID       int                               `json:"uid"`
			Developer string                            `json:"developer"`
			Mappings  []reconcileruntime.PersistMapping `json:"mappings"`
			Start     *bool                             `json:"start"`
		}
		var fields map[string]json.RawMessage
		decoder := json.NewDecoder(bytes.NewReader(actual.Stdin))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF || json.Unmarshal(actual.Stdin, &fields) != nil || len(fields) != 5 ||
			input.Yard != "default" || input.UID != 1000 || input.Developer != "dev" || input.Start == nil || len(control.PersistMappings) == 0 || !slices.Equal(input.Mappings, control.PersistMappings) {
			reject()
			return 99
		}
		var mappings []map[string]json.RawMessage
		if json.Unmarshal(fields["mappings"], &mappings) != nil {
			reject()
			return 99
		}
		for _, mapping := range mappings {
			if len(mapping) != 4 {
				reject()
				return 99
			}
		}
		return 0
	}}
}

// The initial known drift is armed only after the candidate's migration work;
// it cannot make a released predecessor unready before ordinary update starts.
func historyApplyBoundary(control historyControl) bool {
	var journal struct {
		Checkpoint string
		Steps      []struct{ Migration, Checkpoint string }
	}
	payload, err := os.ReadFile(control.Journal)
	if err != nil || json.Unmarshal(payload, &journal) != nil || journal.Checkpoint != "reconciling" {
		return false
	}
	active, err := os.Readlink(control.Runtime + "/current")
	if err != nil || active != "releases/"+control.Target {
		return false
	}
	for _, id := range control.Migrations {
		found := false
		for _, step := range journal.Steps {
			if step.Migration == id && step.Checkpoint == "verified" {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

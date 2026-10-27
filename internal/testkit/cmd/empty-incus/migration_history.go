package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/Subyard/Subyard/internal/testkit"
)

type historyControl struct {
	Journal         string         `json:"journal"`
	Runtime         string         `json:"runtime"`
	Target          string         `json:"target"`
	Source          string         `json:"source"`
	Guest           string         `json:"guest"`
	InitialDrift    string         `json:"initialDrift"`
	Destination     string         `json:"destination"`
	Migrations      []string       `json:"migrations"`
	FailOnce        bool           `json:"failOnce"`
	InventorySHA256 string         `json:"inventoryProgramSHA256"`
	Skills          *historySkills `json:"skills,omitempty"`
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
		programSHA := ""
		if len(command) > 3 {
			programSHA = fmt.Sprintf("%x", sha256.Sum256([]byte(command[3])))
		}
		if control.Skills != nil && len(command) == 4 && command[0] == "python3" && command[1] == "-B" && command[2] == "-c" && programSHA == control.Skills.ProgramSHA256 {
			return testkit.IncusServerExecStep{Stdout: []byte("skills-current\n"), AfterInput: func(actual testkit.IncusServerExecCall) int {
				var input struct {
					Files    map[string][]byte `json:"files"`
					User     *string           `json:"user"`
					Profiles *string           `json:"profiles"`
					Coding   *bool             `json:"coding"`
				}
				decoder := json.NewDecoder(bytes.NewReader(actual.Stdin))
				decoder.DisallowUnknownFields()
				if decoder.Decode(&input) != nil || input.User == nil || *input.User != control.Skills.User || input.Profiles == nil || *input.Profiles != control.Skills.Profiles || input.Coding == nil || *input.Coding != control.Skills.Coding || len(input.Files) != len(control.Skills.Files) {
					reject()
					return 99
				}
				if decoder.Decode(new(any)) != io.EOF {
					reject()
					return 99
				}
				for name, digest := range control.Skills.Files {
					payload, ok := input.Files[name]
					if !ok || fmt.Sprintf("%x", sha256.Sum256(payload)) != digest {
						reject()
						return 99
					}
				}
				return 0
			}}
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
		if len(command) == 5 && command[0] == "python3" && command[1] == "-B" && command[2] == "-c" && (programSHA == control.InventorySHA256 || programSHA == "2193bc75e21ea70024008739bb74004f92a2d3ffc5894de7ca017f2d4291be2e") && (command[4] == "configs-prepare" || command[4] == "configs-commit" || command[4] == "observe") {
			output := []byte(nil)
			if command[4] == "observe" {
				output = []byte(`{"fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","changed":false,"facts":[]}`)
			}
			return testkit.IncusServerExecStep{Stdout: output, AfterInput: func(actual testkit.IncusServerExecCall) int {
				var input struct {
					Home, Developer string
					UID             int
					Entries         []struct{ ID, Kind, Path, Digest string }
				}
				source, err := os.ReadFile(control.Source)
				if err != nil || json.Unmarshal(actual.Stdin, &input) != nil || input.Home != "/home/dev" || input.Developer != "dev" || input.UID != 1000 {
					reject()
					return 99
				}
				wanted := 1
				if command[4] == "observe" {
					wanted = 4
				}
				if len(input.Entries) != wanted {
					reject()
					return 99
				}
				for _, entry := range input.Entries {
					valid := entry.ID == "fixture" && entry.Kind == "file" && entry.Path == control.Destination && entry.Digest == fmt.Sprintf("%x", sha256.Sum256(source))
					if command[4] == "observe" {
						valid = valid || entry.ID == "fixture" && entry.Kind == "package" && entry.Path == "" || entry.ID == "_projects" && entry.Kind == "file" && (entry.Path == "/usr/local/libexec/subyard/projects-changed" || entry.Path == "/etc/subyard/agent-project-hooks")
					}
					if !valid {
						reject()
						return 99
					}
				}
				if command[4] != "observe" {
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
		if len(command) == 7 && command[0] == "sh" && command[1] == "-eu" && command[2] == "-c" && command[3] == configWriteScript && command[4] == "subyard" && command[5] == control.Destination && command[6] == "1000" {
			expected, err := os.ReadFile(control.Source)
			if err != nil {
				return reject()
			}
			return testkit.IncusServerExecStep{Stderr: []byte("controlled materialized config apply fixture\n"), AfterInput: func(actual testkit.IncusServerExecCall) int {
				historyMu.Lock()
				defer historyMu.Unlock()
				if !bytes.Equal(actual.Stdin, expected) {
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

package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/configmaterial"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestConfigObservationBindsInstalledFilesAndRejectsForeignRequests(t *testing.T) {
	root := testkit.TempDir(t)
	repository, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	c := configObservationControl{Repository: repository, Home: root, Config: filepath.Join(root, "config"), Data: filepath.Join(root, "data"), InventoryProgram: filepath.Join(repository, "internal/adapters/reconcileruntime/integration_inventory.py")}
	if err := os.Mkdir(c.Config, 0700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "source.toml")
	testkit.WriteFile(t, source, []byte("fixture = true\n"), 0600)
	testkit.WriteFile(t, filepath.Join(c.Config, "config.env"), []byte("ENVIRONMENT_PROFILES=\nCODING_TOOL_INTEGRATIONS=codex\nAGENT_codex_CONFIG="+source+"\n"), 0600)
	raw, err := os.ReadFile(c.InventoryProgram)
	if err != nil {
		t.Fatal(err)
	}
	program := strings.NewReplacer("@STATE_ROOT@", "/var/lib/subyard/integrations", "@STATE_UID@", "0").Replace(string(raw))
	c.InventorySHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(program)))
	expected, err := c.request()
	if err != nil {
		t.Fatal(err)
	}
	var request configObservationRequest
	if json.Unmarshal(expected, &request) != nil || len(request.Entries) != 2 || request.Entries[0].Digest != fmt.Sprintf("%x", sha256.Sum256([]byte("fixture = true\n"))) || request.Entries[0].Path != "/home/dev/.codex/config.toml" {
		t.Fatal("fixture did not independently bind its selected source")
	}
	call := testkit.IncusServerExecCall{Project: "subyard", Name: "yard", Command: []string{"python3", "-B", "-c", program, "configs-observe"}, Stdin: expected}
	release := make(chan struct{})
	dispatcher, err := os.ReadFile(filepath.Join(repository, "config/projects-changed.sh"))
	if err != nil {
		t.Fatal(err)
	}
	projects := testkit.IncusServerExecCall{Project: "subyard", Name: "yard", Command: []string{"bash", "-s", "--", "--observe"}, User: 1000, Group: 1000, Stdin: dispatcher}
	projectsStep := configObservationStep(c, projects, release)
	if projectsStep.AfterInput == nil || projectsStep.AfterInput(projects) != 0 || projectsStep.Release != release {
		t.Fatal("exact projects observation lost the existing guarded responder")
	}
	projects.Stdin = append(slices.Clone(dispatcher), '\n')
	if projectsStep.AfterInput(projects) != 99 {
		t.Fatal("modified projects dispatcher was admitted")
	}
	step := configObservationStep(c, call, release)
	var states []historyConfigState
	if step.AfterInput == nil || step.AfterInput(call) != 0 || step.Release != release || json.Unmarshal(step.Stdout, &states) != nil || !slices.Equal(states, make([]historyConfigState, 2)) {
		t.Fatal("exact read-only request did not observe the two absent fixture files")
	}
	testkit.WriteFile(t, source, []byte("fixture = false\n"), 0600)
	if step.AfterInput(call) != 99 {
		t.Fatal("source changed after capture was admitted")
	}
	testkit.WriteFile(t, source, []byte("fixture = true\n"), 0600)
	for _, change := range []string{"program", "program-binding", "write", "project", "instance", "user", "group", "environment", "malformed", "unknown-field", "home", "uid", "id", "path", "digest", "order", "count", "source"} {
		t.Run(change, func(t *testing.T) {
			foreign := call
			foreign.Command = slices.Clone(call.Command)
			binding := c
			input := request
			input.Entries = slices.Clone(request.Entries)
			switch change {
			case "program":
				foreign.Command[3] += "\n"
			case "program-binding":
				binding.InventorySHA256 = strings.Repeat("0", 64)
			case "write":
				foreign.Command[4] = "configs-write"
			case "project":
				foreign.Project = "foreign"
			case "instance":
				foreign.Name = "foreign"
			case "user":
				foreign.User = 1000
			case "group":
				foreign.Group = 1000
			case "environment":
				foreign.Environment = map[string]string{"FOREIGN": "1"}
			case "malformed":
				foreign.Stdin = []byte("{")
			case "unknown-field":
				foreign.Stdin = append([]byte(`{"foreign":true,`), expected[1:]...)
			case "home":
				input.Home = "/home/foreign"
			case "uid":
				input.UID = 2000
			case "id":
				input.Entries[0].ID = "foreign"
			case "path":
				input.Entries[0].Path = "/home/dev/foreign"
			case "digest":
				input.Entries[0].Digest = strings.Repeat("0", 64)
			case "order":
				slices.Reverse(input.Entries)
			case "count":
				input.Entries = input.Entries[:1]
			case "source":
				testkit.WriteFile(t, source, []byte("fixture = false\n"), 0600)
				defer testkit.WriteFile(t, source, []byte("fixture = true\n"), 0600)
			}
			if foreign.Stdin == nil || string(foreign.Stdin) == string(expected) {
				foreign.Stdin, _ = json.Marshal(input)
			}
			rejected := configObservationStep(binding, foreign, release)
			code := rejected.ExitCode
			if rejected.AfterInput != nil {
				code = rejected.AfterInput(foreign)
			}
			if code != 99 {
				t.Fatal("foreign observation admitted")
			}
		})
	}
	t.Run("structured-absence", func(t *testing.T) {
		_, payloads, err := c.description()
		if err != nil {
			t.Fatal(err)
		}
		file := request.Entries[0]
		raw, err := configmaterial.Request(file.Format, configmaterial.ModeObserve, "dev", file.Path, 1000, payloads[file.Path])
		if err != nil {
			t.Fatal(err)
		}
		capture, _ := configmaterial.WithAfterOwnerObservation(raw, nil)
		call := testkit.IncusServerExecCall{Project: "subyard", Name: "yard", Command: capture.Command, Stdin: capture.Stdin}
		step := configObservationStep(c, call, release)
		observed, err := configmaterial.ParseObservation(step.Stdout)
		if err != nil || step.AfterInput == nil || step.AfterInput(call) != 0 || step.Release != release || !observed.State.Valid() || observed.Destination == nil || *observed.Destination != (configmaterial.DestinationState{}) {
			t.Fatal("declared absent structured file lost protected state", err)
		}
		retained, _ := configmaterial.WithAfterOwnerObservation(raw, &configmaterial.ApplyExpectedState{State: observed.State, Fingerprint: observed.Fingerprint})
		current := call
		current.Command = retained.Command
		if step := configObservationStep(c, current, release); step.AfterInput == nil || step.AfterInput(current) != 0 {
			t.Fatal("original absent structured state did not match")
		}
		for _, change := range []string{"program", "write", "path", "payload", "identity", "expected", "extra", "source"} {
			t.Run(change, func(t *testing.T) {
				foreign := call
				foreign.Command = slices.Clone(call.Command)
				switch change {
				case "program":
					foreign.Command[3] += "\n"
				case "write":
					foreign.Command[4] = configmaterial.ModeApply
				case "path":
					foreign.Command[6] = "/home/dev/foreign"
				case "payload":
					foreign.Stdin = []byte("fixture = false\n")
				case "identity":
					foreign.User = 1000
				case "expected":
					foreign.Command[10] = `{"capture":false}`
				case "extra":
					foreign.Command = append(foreign.Command, "extra")
				case "source":
					testkit.WriteFile(t, source, []byte("fixture = false\n"), 0600)
					defer testkit.WriteFile(t, source, []byte("fixture = true\n"), 0600)
				}
				rejected := configObservationStep(c, foreign, release)
				code := rejected.ExitCode
				if rejected.AfterInput != nil {
					code = rejected.AfterInput(foreign)
				}
				if code != 99 {
					t.Fatal("foreign structured request admitted")
				}
			})
		}
	})
}

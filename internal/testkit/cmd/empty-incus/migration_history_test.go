package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/incusclient"
	"github.com/Subyard/Subyard/internal/ports"
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
	// Exercise the actual websocket stdin path, not the initial exec metadata.
	dispatcher, err := os.ReadFile("../../../../config/projects-changed.sh")
	if err != nil {
		t.Fatal(err)
	}
	value.ProjectHookProgram = filepath.Join(root, "projects-changed.sh")
	value.ProjectHookSHA256 = fmt.Sprintf("%x", sha256.Sum256(dispatcher))
	testkit.WriteFile(t, value.ProjectHookProgram, dispatcher, 0o600)
	server, err := testkit.NewIncusServer(testkit.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	server.SetExtensions("container_exec_user_group_cwd")
	server.SetExecResponder(historyResponder(value, control))
	client := incusclient.New(server.SocketPath)
	request := ports.InstanceExecRequest{Command: []string{"bash", "-s", "--", "--observe"}, User: 1000, Group: 1000, Stdin: dispatcher}
	t.Run("project-hooks-observe", func(t *testing.T) {
		result, err := client.Exec(t.Context(), "subyard", "yard", request)
		if err != nil || result.ExitCode != 0 {
			t.Fatalf("exact bound project-hook observation failed: %v, exit=%d", err, result.ExitCode)
		}
		var observation struct {
			Projects, Wiring string
			Hooks, Roots     []string
			Facts, Resolved  map[string]string
		}
		if json.Unmarshal(result.Stdout, &observation) != nil || observation.Projects != fmt.Sprintf("%x", sha256.Sum256(nil)) || observation.Wiring == "" || observation.Wiring == observation.Projects || len(observation.Hooks) != 0 || len(observation.Roots) != 0 || len(observation.Facts) != 0 || len(observation.Resolved) != 0 {
			t.Fatal("observation lost the declared empty workspace and installed dispatcher wiring")
		}
	})
	for _, name := range []string{"script", "user", "group", "environment", "argv", "project", "instance", "bound-source"} {
		t.Run("project-hooks-refuse-"+name, func(t *testing.T) {
			foreign := request
			project, instance := "subyard", "yard"
			switch name {
			case "script":
				foreign.Stdin = append(append([]byte(nil), dispatcher...), '\n')
			case "user":
				foreign.User = 0
			case "group":
				foreign.Group = 0
			case "environment":
				foreign.Environment = map[string]string{"FOREIGN": "1"}
			case "argv":
				foreign.Command = []string{"bash", "-s", "--", "--other"}
			case "project":
				project = "foreign"
			case "instance":
				instance = "foreign"
			case "bound-source":
				testkit.WriteFile(t, value.ProjectHookProgram, []byte("foreign\n"), 0o600)
			}
			result, err := client.Exec(t.Context(), project, instance, foreign)
			if err == nil || result.ExitCode != 99 {
				t.Fatalf("foreign project-hook probe did not refuse: %v, exit=%d", err, result.ExitCode)
			}
			testkit.WriteFile(t, value.ProjectHookProgram, dispatcher, 0o600)
			payload, _ := os.ReadFile(guest)
			if string(payload) != "prior\n" {
				t.Fatal("observation changed guest bytes")
			}
			for _, suffix := range []string{".failed", ".effects.jsonl"} {
				if _, err := os.Stat(control + suffix); !os.IsNotExist(err) {
					t.Fatal("observation consumed apply failure or wrote effects")
				}
			}
		})
	}
	if err := os.Remove(control + ".unexpected"); err != nil {
		t.Fatal(err)
	}
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

	// The same source-owned fixture now accepts the real current-file Python
	// request, without weakening the historical shell branch above.
	program, err := os.ReadFile("../../../adapters/reconcileruntime/integration_inventory.py")
	if err != nil {
		t.Fatal(err)
	}
	python := strings.NewReplacer("@STATE_ROOT@", "/var/lib/subyard/integrations", "@STATE_UID@", "0").Replace(string(program))
	value.InventorySHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(python)))
	respond = historyResponder(value, control)
	testkit.WriteFile(t, guest, []byte("prior\n"), 0600)
	if err := os.Remove(control + ".failed"); err != nil {
		t.Fatal(err)
	}
	priorState, err := historyConfigSnapshot(value)
	if err != nil {
		t.Fatal(err)
	}
	beforeJSON, _ := json.Marshal(priorState)
	desiredDigest := fmt.Sprintf("%x", sha256.Sum256([]byte("desired\n")))
	observe := testkit.IncusServerExecCall{Project: "subyard", Name: "yard", Command: []string{"python3", "-B", "-c", python, "configs-observe"}, Stdin: []byte(fmt.Sprintf(`{"home":"/home/dev","uid":1000,"entries":[{"id":"fixture","kind":"file","path":%q,"digest":%q}]}`, value.Destination, desiredDigest))}
	step = respond(observe)
	var observed []historyConfigState
	if step.AfterInput == nil || step.AfterInput(observe) != 0 || json.Unmarshal(step.Stdout, &observed) != nil || len(observed) != 1 || observed[0] != priorState {
		t.Fatal("ordered original current states changed")
	}
	write := observe
	write.Command = append([]string(nil), observe.Command...)
	write.Command[4] = "configs-write"
	write.Stdin = []byte(fmt.Sprintf(`{"home":"/home/dev","uid":1000,"entries":[{"id":"fixture","kind":"file","path":%q,"digest":%q,"content":%q}],"before":%s}`, value.Destination, desiredDigest, base64.StdEncoding.EncodeToString([]byte("desired\n")), beforeJSON))
	effectsBefore, _ := os.ReadFile(control + ".effects.jsonl")
	for _, scenario := range []string{"program", "foreign-input", "stale-before", "premature"} {
		t.Run("current-cas-refuses-"+scenario, func(t *testing.T) {
			foreign := write
			foreign.Command = append([]string(nil), write.Command...)
			foreign.Stdin = bytes.Clone(write.Stdin)
			switch scenario {
			case "program":
				foreign.Command[3] += "\n"
			case "foreign-input":
				foreign.Stdin = bytes.Replace(foreign.Stdin, []byte(`"home":"/home/dev"`), []byte(`"home":"/home/dev","foreign":true`), 1)
			case "stale-before":
				foreign.Stdin = bytes.Replace(foreign.Stdin, []byte(priorState.Digest), []byte(strings.Repeat("f", 64)), 1)
			case "premature":
				testkit.WriteFile(t, journal, []byte(`{"checkpoint":"complete"}`), 0600)
			}
			rejected := respond(foreign)
			code := rejected.ExitCode
			if rejected.AfterInput != nil {
				code = rejected.AfterInput(foreign)
			}
			if code != 99 {
				t.Fatal("foreign current CAS admitted", code)
			}
			if _, err := os.Stat(control + ".failed"); !os.IsNotExist(err) {
				t.Fatal("refusal consumed controlled failure")
			}
			body, _ := os.ReadFile(guest)
			events, _ := os.ReadFile(control + ".effects.jsonl")
			if string(body) != "prior\n" || !bytes.Equal(events, effectsBefore) {
				t.Fatal("refusal changed bytes or effect provenance")
			}
			testkit.WriteFile(t, journal, verified, 0600)
		})
	}
	step = respond(write)
	if step.AfterInput == nil || step.AfterInput(write) != 37 {
		t.Fatal("exact source-pinned current CAS did not fail once")
	}
	body, _ := os.ReadFile(guest)
	if string(body) != "prior\n" {
		t.Fatal("failed CAS changed original bytes")
	}
	if respond(write).AfterInput(write) != 0 {
		t.Fatal("current CAS retry failed")
	}
	body, _ = os.ReadFile(guest)
	if string(body) != "desired\n" {
		t.Fatal("current CAS lost exact original payload")
	}
	events, _ := os.ReadFile(control + ".effects.jsonl")
	lines := bytes.Split(bytes.TrimSpace(events[len(effectsBefore):]), []byte("\n"))
	if len(lines) != 2 {
		t.Fatal("missing exact failure/success provenance")
	}
	for i, line := range lines {
		var event struct {
			Command     []string           `json:"command"`
			Input       string             `json:"stdinSHA256"`
			Payload     string             `json:"payloadSHA256"`
			Before      historyConfigState `json:"before"`
			Transaction json.RawMessage    `json:"transaction"`
			Exit        int                `json:"exit"`
		}
		if json.Unmarshal(line, &event) != nil || !slices.Equal(event.Command, write.Command) || event.Input != fmt.Sprintf("%x", sha256.Sum256(write.Stdin)) || event.Payload != desiredDigest || event.Before != priorState || !bytes.Equal(event.Transaction, verified) || event.Exit != []int{37, 0}[i] {
			t.Fatal("current CAS event lost exact command/input/original transaction", string(line))
		}
	}
	if respond(write).AfterInput(write) != 0 {
		t.Fatal("approved desired state was not a no-op")
	}
	afterEvents, _ := os.ReadFile(control + ".effects.jsonl")
	if !bytes.Equal(events, afterEvents) {
		t.Fatal("desired-state CAS replayed effects")
	}
}

func TestHistoryObservationsPreserveInitialDrift(t *testing.T) {
	root := testkit.TempDir(t)
	source, guest, journal, control := filepath.Join(root, "source"), filepath.Join(root, "guest"), filepath.Join(root, "journal"), filepath.Join(root, "control")
	testkit.WriteFile(t, source, []byte("desired\n"), 0600)
	testkit.WriteFile(t, guest, []byte("prior\n"), 0600)
	testkit.WriteFile(t, journal, []byte(`{"checkpoint":"complete"}`), 0600)
	if err := os.Symlink("releases/prior", filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	program, err := os.ReadFile("../../../adapters/reconcileruntime/integration_inventory.py")
	if err != nil {
		t.Fatal(err)
	}
	python := strings.NewReplacer("@STATE_ROOT@", "/var/lib/subyard/integrations", "@STATE_UID@", "0").Replace(string(program))
	value := historyControl{Journal: journal, Runtime: root, Target: "candidate", Source: source, PriorSource: guest, Guest: guest, Destination: "/home/dev/.subyard-fixture/config.txt", Migrations: []string{"one"}, InventorySHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(python)))}
	respond := historyResponder(value, control)
	server, err := testkit.NewIncusServer(testkit.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	server.SetExecResponder(respond)
	client := incusclient.New(server.SocketPath)
	prior := historyConfigState{true, fmt.Sprintf("%x", sha256.Sum256([]byte("prior\n"))), 1000, 1000, 0644}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("desired\n")))
	input := []byte(fmt.Sprintf(`{"home":"/home/dev","uid":1000,"entries":[{"id":"fixture","kind":"file","path":%q,"digest":%q}]}`, value.Destination, digest))
	for _, swapped := range []bool{false, true} {
		entries := []map[string]string{{"id": "fixture", "kind": "file", "path": value.Destination, "digest": digest}, {"id": "fixture", "kind": "package", "digest": "fixture"}, {"id": "_projects", "kind": "file", "path": "/usr/local/libexec/subyard/projects-changed", "digest": "dispatcher"}, {"id": "_projects", "kind": "file", "path": "/etc/subyard/agent-project-hooks", "digest": "hooks"}}
		if swapped {
			entries[0]["digest"] = prior.Digest
		}
		body, _ := json.Marshal(map[string]any{"home": "/home/dev", "uid": 1000, "developer": "dev", "entries": entries})
		result, err := client.Exec(t.Context(), "subyard", "yard", ports.InstanceExecRequest{Command: []string{"python3", "-B", "-c", python, "observe"}, Stdin: body})
		if !swapped && err != nil || swapped && (err == nil || result.ExitCode != 99) {
			t.Fatal("inventory source routing changed", swapped, err)
		}
	}
	for _, phase := range []string{"before", "after"} {
		t.Run(phase, func(t *testing.T) {
			if phase == "after" {
				testkit.WriteFile(t, journal, []byte(`{"checkpoint":"reconciling","steps":[{"migration":"one","checkpoint":"verified"}]}`), 0600)
				if err := os.Remove(filepath.Join(root, "current")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("releases/candidate", filepath.Join(root, "current")); err != nil {
					t.Fatal(err)
				}
			}
			hash, err := client.Exec(t.Context(), "subyard", "yard", ports.InstanceExecRequest{Command: []string{"sha256sum", "--", value.Destination}})
			if err != nil || string(hash.Stdout) != prior.Digest+"  "+value.Destination+"\n" {
				t.Fatal("hash observation changed", err)
			}
			request := ports.InstanceExecRequest{Command: []string{"python3", "-B", "-c", python, "configs-observe"}, Stdin: input}
			result, err := client.Exec(t.Context(), "subyard", "yard", request)
			if err != nil {
				t.Fatal(err)
			}
			var observed []historyConfigState
			if json.Unmarshal(result.Stdout, &observed) != nil || len(observed) != 1 || observed[0] != prior {
				t.Fatal("current observation differs from original state")
			}
			actual, err := historyConfigSnapshot(value)
			if err != nil || actual != prior {
				t.Fatal("observation changed physical fixture", err)
			}
			for _, suffix := range []string{".failed", ".effects.jsonl", ".drift"} {
				if _, err := os.Stat(control + suffix); !os.IsNotExist(err) {
					t.Fatal("observation wrote fixture state", suffix)
				}
			}
		})
	}
}

// Exercise the Python observation producer through the real SDK adapter, rather
// than supplying a second hand-written inventory to the server.
func TestHistoryObservationPreservesInstanceIdentity(t *testing.T) {
	root := testkit.TempDir(t)
	server, err := testkit.NewIncusServer(root)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	control := filepath.Join(root, "observation.json")
	testkit.WriteFile(t, control, []byte("{}"), 0600)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); observeFixture(ctx, server, control) }()
	defer func() { cancel(); <-done }()
	client := incusclient.New(server.SocketPath)
	var identity string
	for _, status := range []string{"Running", "Stopped", "absent", "Running"} {
		command := exec.CommandContext(t.Context(), "python3", "-B", "-c", `import pathlib,runpy,sys
runpy.run_path(sys.argv[1])["set_fixture_observation"](pathlib.Path(sys.argv[2]), None if sys.argv[3] == "absent" else sys.argv[3])`, "../../../../dev/verify-release-upgrades.py", control, status)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("observation producer: %v\n%s", err, output)
		}
		instance, err := client.Instance(t.Context(), "subyard", "yard")
		if status == "absent" {
			if !errors.Is(err, ports.ErrInstanceNotFound) {
				t.Fatal("absent inventory did not preserve absence", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		uuid := instance.Config["volatile.uuid"]
		if uuid == "" || instance.LocalConfig["volatile.uuid"] != uuid || (identity != "" && identity != uuid) {
			t.Fatal("present inventory lost its stable local and expanded instance identity")
		}
		if instance.Status != status {
			t.Fatal("observation changed the requested power state")
		}
		identity = uuid
	}
}

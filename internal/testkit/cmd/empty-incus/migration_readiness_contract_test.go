package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/incusclient"
	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestHistoryReadinessContract(t *testing.T) {
	root := testkit.TempDir(t)
	files := map[string][]byte{"config/skills/core/sample/SKILL.md": []byte("generic core skill\n")}
	for _, name := range []string{"scripts/refresh-agent-skills.sh", "config/skills/package.py", "config/skills/refresh.sh", "config/skills/deliver.py", "config/skills/probe.py", "config/skills/migrate-preview.py"} {
		payload, err := os.ReadFile(filepath.Join("../../../..", name))
		if err != nil {
			t.Fatal(err)
		}
		files[name] = payload
	}
	expected := &historySkills{Files: map[string]string{}, User: "dev", Profiles: "", Coding: true}
	for name, payload := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, payload, 0o600)
		expected.Files[name] = fmt.Sprintf("%x", sha256.Sum256(payload))
	}
	expected.ProgramSHA256 = expected.Files["config/skills/probe.py"]
	control := filepath.Join(root, "control")
	value := historyControl{Skills: expected, Source: filepath.Join(root, "source"), Guest: filepath.Join(root, "guest"), Destination: "/home/dev/.subyard-fixture/config.txt"}
	testkit.WriteFile(t, value.Source, []byte("desired\n"), 0o600)
	testkit.WriteFile(t, value.Guest, []byte("prior\n"), 0o600)
	server, err := testkit.NewIncusServer(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	server.SetExtensions("projects", "instances", "instance_all_projects", "storage", "network", "container_exec_user_group_cwd")
	server.SetInventory(map[string]map[string]any{"subyard": {"name": "subyard", "config": map[string]string{}}}, map[string]map[string]any{"subyard/yard": {"name": "yard", "status": "Running", "type": "container", "config": map[string]string{}, "devices": map[string]any{}}})
	respond := historyResponder(value, control)
	server.SetExecResponder(respond)
	client := incusclient.New(server.SocketPath)
	runtime := reconcileruntime.Runtime{RepositoryRoot: root, Incus: client, Executor: client, Integrations: []profile.IntegrationDefinition{}, Yard: domain.Context{IncusProject: "subyard", YardInstanceName: "yard", DevUser: "dev", DevUID: 1000}, Environment: []string{"ENVIRONMENT_PROFILES=", "ALLOWS_CODING_TOOLS=true", "CODING_TOOL_INTEGRATIONS="}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for range 2 {
		ready, err := runtime.ConfigsConverged(ctx)
		if err != nil || !ready {
			t.Fatalf("readiness=%v err=%v", ready, err)
		}
	}
	calls := server.ExecCalls()
	if len(calls) != 2 {
		t.Fatalf("skills branch was not actually probed twice: %d calls", len(calls))
	}
	if len(calls[0].Command) != 4 || calls[0].Command[3] != string(files["config/skills/probe.py"]) {
		t.Fatal("actual producer omitted captured probe")
	}
	for _, suffix := range []string{".unexpected", ".effects.jsonl", ".failed", ".drift"} {
		if _, err := os.Stat(control + suffix); !os.IsNotExist(err) {
			t.Fatalf("readiness created %s: %v", suffix, err)
		}
	}
	// The same public readiness boundary must observe real config drift and
	// reach the skills probe only after the allowed fixture repair.
	runtime.Integrations = []profile.IntegrationDefinition{{SchemaVersion: 2, Name: "fixture", Artifacts: []profile.IntegrationArtifact{{ID: "instructions", Kind: "instructions", SourceSetting: "FIXTURE_SOURCE", Destination: ".subyard-fixture/config.txt", Format: "raw"}}}}
	packagePath := filepath.Join(root, "config/agents/fixture")
	if err := os.MkdirAll(packagePath, 0o700); err != nil {
		t.Fatal(err)
	}
	declaration, err := json.Marshal(runtime.Integrations[0])
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(packagePath, "integration.json"), declaration, 0o600)
	runtime.Integrations, err = profile.LoadIntegrations(root)
	if err != nil {
		t.Fatal(err)
	}
	runtime.Environment = []string{"ENVIRONMENT_PROFILES=", "ALLOWS_CODING_TOOLS=true", "CODING_TOOL_INTEGRATIONS=fixture", "FIXTURE_SOURCE=" + value.Source}
	for _, payload := range []string{"prior\n", "desired\n", "changed source\n"} {
		if payload == "changed source\n" {
			testkit.WriteFile(t, value.Source, []byte(payload), 0o600)
		} else {
			testkit.WriteFile(t, value.Guest, []byte(payload), 0o600)
		}
		ready, err := runtime.ConfigsConverged(ctx)
		if err != nil || ready != (payload == "desired\n") {
			t.Fatalf("config %q readiness=%v err=%v", payload, ready, err)
		}
	}
	guest, _ := os.ReadFile(value.Guest)
	if string(guest) != "desired\n" {
		t.Fatal("readiness mutated guest")
	}
	if len(server.ExecCalls()) != 6 {
		t.Fatalf("config readiness did not reach the skills probe after repair: %d calls", len(server.ExecCalls()))
	}
	for _, suffix := range []string{".unexpected", ".effects.jsonl", ".failed", ".drift"} {
		if _, err := os.Stat(control + suffix); !os.IsNotExist(err) {
			t.Fatalf("config readiness created %s: %v", suffix, err)
		}
	}
	// Mutants exercise the strict consumer independently of the real emitter.
	for _, mutation := range []string{"bytes", "missing", "extra", "settings", "program"} {
		t.Run(mutation, func(t *testing.T) {
			call := calls[0]
			call.Command = append([]string(nil), call.Command...)
			var input map[string]any
			if err := json.Unmarshal(call.Stdin, &input); err != nil {
				t.Fatal(err)
			}
			sources := input["files"].(map[string]any)
			switch mutation {
			case "bytes":
				sources["config/skills/deliver.py"] = "Zm9yZWlnbg=="
			case "missing":
				delete(sources, "config/skills/package.py")
			case "extra":
				sources["foreign"] = ""
			case "settings":
				input["profiles"] = "foreign"
			case "program":
				call.Command[3] += "\n# foreign"
			}
			call.Stdin, _ = json.Marshal(input)
			step := respond(call)
			code := step.ExitCode
			if step.AfterInput != nil {
				code = step.AfterInput(call)
			}
			if code != 99 {
				t.Fatalf("mutant accepted: %d", code)
			}
		})
	}
	guest, _ = os.ReadFile(value.Guest)
	if !bytes.Equal(guest, []byte("desired\n")) {
		t.Fatal("negative probes changed guest")
	}
}

package reconcileruntime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

// Map guest filesystem arguments to a real temporary filesystem and execute the actual probe.
type projectHooksFileExecutor struct{ root string }

func (fixture projectHooksFileExecutor) Exec(ctx context.Context, _, _ string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	command := append([]string(nil), request.Command...)
	for _, index := range []int{7, 8, 9} {
		command[index] = filepath.Join(fixture.root, command[index])
	}
	command[10] = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	output, err := exec.CommandContext(ctx, command[0], command[1:]...).CombinedOutput()
	result := ports.InstanceExecResult{Stdout: output}
	if err != nil {
		result.ExitCode = 1
	}
	return result, err
}

func TestProjectHooksProbeUsesExactDispatcherAndSelectedList(t *testing.T) {
	source, err := os.ReadFile("../../../config/projects-changed.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"ready", "dispatcher missing", "dispatcher stale", "list stale", "selection changed", "mode wrong"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			dispatcher := filepath.Join(root, "usr/local/libexec/subyard/projects-changed")
			hooks := filepath.Join(root, "etc/subyard/agent-project-hooks")
			for _, directory := range []string{dispatcher + ".d", filepath.Dir(hooks)} {
				if err := os.MkdirAll(directory, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			testkit.WriteFile(t, dispatcher, source, 0o755)
			testkit.WriteFile(t, hooks, []byte("/usr/local/bin/selected-hook\n"), 0o644)
			runtime := Runtime{RepositoryRoot: "../../..", Executor: projectHooksFileExecutor{root}, Environment: []string{
				"CODING_TOOL_INTEGRATIONS=selected", "AGENT_selected_PROJECTS_CHANGED=/usr/local/bin/selected-hook",
				"AGENT_disabled_PROJECTS_CHANGED=/usr/local/bin/not-selected",
			}}
			switch scenario {
			case "dispatcher missing":
				err = os.Remove(dispatcher)
			case "dispatcher stale":
				err = os.WriteFile(dispatcher, []byte("#!/bin/sh\nexit 0\n"), 0o755)
			case "list stale":
				err = os.WriteFile(hooks, []byte("/usr/local/bin/other-hook\n"), 0o644)
			case "selection changed":
				runtime.Environment[0] = "CODING_TOOL_INTEGRATIONS=disabled"
			case "mode wrong":
				err = os.Chmod(dispatcher, 0o644)
			}
			if err != nil {
				t.Fatal(err)
			}
			ready, err := runtime.projectHooksConverged(context.Background())
			if err != nil || ready != (scenario == "ready") {
				t.Fatalf("ready=%v error=%v", ready, err)
			}
		})
	}
}

func TestInitProjectHooksUseDevAndSkipIntentionallyStoppedYard(t *testing.T) {
	for _, status := range []string{"running", "stopped", "unexpectedly stopped"} {
		t.Run(status, func(t *testing.T) {
			instance := ports.InstanceInfo{Status: status, Config: map[string]string{
				"user.subyard.managed": "true", "user.subyard.initialized": "true", "user.subyard.desired_power": "stopped",
			}}
			if status == "unexpectedly stopped" {
				instance.Status, instance.Config["user.subyard.desired_power"] = "stopped", "running"
			}
			incus := &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true, Instance: instance}, ExecSteps: []testkit.IncusExecStep{{}}}
			runtime := Runtime{Incus: incus, Executor: incus, Yard: domain.Context{
				IncusProject: "test-project", YardInstanceName: "test-yard", DevUser: "developer", DevUID: 1001,
			}}
			err := runtime.RunProjectHooks(context.Background())
			if (err != nil) != (status == "unexpectedly stopped") {
				t.Fatalf("error=%v", err)
			}
			if status != "running" {
				if len(incus.ExecCalls) != 0 {
					t.Fatal("stopped yard received guest execution")
				}
				return
			}
			if len(incus.ExecCalls) != 1 {
				t.Fatalf("hook attempts=%d", len(incus.ExecCalls))
			}
			call := incus.ExecCalls[0]
			if call.Project != "test-project" || call.Name != "test-yard" || call.Request.User != 1001 ||
				call.Request.Group != 1001 || call.Request.Environment["HOME"] != "/home/developer" {
				t.Fatalf("wrong hook execution identity: %#v", call)
			}
		})
	}
}

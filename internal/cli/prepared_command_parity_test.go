package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/state"
	"github.com/Subyard/Subyard/internal/testkit"
)

type preparationParityProbe struct {
	output   []byte
	exitCode int
}

func (probe preparationParityProbe) Execute(
	_ context.Context,
	_ domain.Context,
	request ports.InstanceExecRequest,
) (ports.InstanceExecResult, error) {
	if len(request.Command) > 0 && request.Command[0] == "docker" {
		return ports.InstanceExecResult{Stdout: []byte("present"), ExitCode: 0}, nil
	}
	if probe.exitCode == 0 && len(probe.output) == 0 {
		return ports.InstanceExecResult{Stdout: []byte("missing"), ExitCode: 0}, nil
	}
	return ports.InstanceExecResult{Stdout: probe.output, ExitCode: probe.exitCode}, nil
}

func (preparationParityProbe) Stream(
	context.Context,
	domain.Context,
	ports.InstanceExecRequest,
	io.Reader,
) (ports.InstanceExecResult, error) {
	return ports.InstanceExecResult{}, errors.New("unexpected preparation stream")
}

type preparationParityCase struct {
	name      string
	command   string
	arguments []string
	action    domain.ActionID
	changed   bool
	target    domain.ExecutionTarget
	setup     func(*testing.T, string, []string, string) ([]string, *testkit.Incus, ports.YardExecutor, ports.InitPlatform)
	noPrompt  bool
}

func TestDirectAndRPCPreparationParity(t *testing.T) {
	cases := []preparationParityCase{
		{
			name: "forced stop", command: "stop", arguments: []string{"--force"},
			action: "yard.stop-force", changed: true, target: domain.TargetLocalOwner,
			setup: preparationRunningYard,
		},
		{
			name: "soft remove", command: "remove", arguments: []string{"--soft", "Demo"},
			action: "project.remove-soft", changed: true, target: domain.TargetLocalOwner,
			setup: preparationProject,
		},
		{
			name: "rebuild project environment", command: "up", arguments: []string{"--rebuild", "Demo"},
			action: "project.environment.rebuild", changed: true, target: domain.TargetLocalOwner,
			setup: preparationProject,
		},
		{
			name: "teardown keeping data", command: "teardown", arguments: []string{"--keep-data"},
			action: "yard.teardown.keep-data", changed: true, target: domain.TargetLocalOwner,
			setup: preparationRunningYard,
		},
		{
			name: "revoke test VM slot", command: "test-vms", arguments: []string{"revoke", "--slot", "1"},
			action: "test-vms.revoke", changed: true, target: domain.TargetLocalOwner,
			setup: preparationHeldTestVMSlot,
		},
		{
			name: "converged init", command: "init", action: "yard.init.reconcile",
			changed: false, target: domain.TargetLocalOwner, setup: preparationConvergedInit, noPrompt: true,
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			root, environment, stateDirectory := preparationParityFixture(t)
			environment, incus, projectData, initPlatform := test.setup(t, root, environment, stateDirectory)
			clock := testkit.NewManualClock(time.Unix(100, 0))
			prompt := &testkit.Prompt{Answers: []bool{false}}
			runner := &testkit.ScriptedAdapter{}
			var directStderr bytes.Buffer
			direct, err := New(Options{
				RepositoryRoot: root, Program: "yard",
				Arguments:   append([]string{test.command}, test.arguments...),
				Environment: append(environment, "SUBYARD_OPERATION_ID=direct-parity"),
				WorkingDir:  root, Prompt: prompt, AdapterRunner: runner, Clock: clock,
				Incus: incus, ProjectData: projectData, InitPlatform: initPlatform,
				Stdout: io.Discard, Stderr: &directStderr,
			})
			if err != nil {
				t.Fatal(err)
			}
			code := direct.Run(context.Background())
			if test.noPrompt {
				if code != 0 || len(prompt.Requests) != 0 || len(runner.Requests) != 0 {
					t.Fatalf("direct no-op: code=%d prompts=%#v adapters=%#v stderr=%q",
						code, prompt.Requests, runner.Requests, directStderr.String())
				}
			} else if code != 1 || len(prompt.Requests) != 1 || len(runner.Requests) != 0 {
				t.Fatalf("direct decline: code=%d prompts=%#v adapters=%#v stderr=%q",
					code, prompt.Requests, runner.Requests, directStderr.String())
			}

			rpcProgram, err := New(Options{
				RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
				Clock: clock, Incus: incus, ProjectData: projectData, InitPlatform: initPlatform,
				AdapterRunner: &testkit.ScriptedAdapter{}, Stdout: io.Discard, Stderr: io.Discard,
			})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := rpcProgram.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			handler := &rpcHandler{cli: rpcProgram, loaded: loaded}
			params, err := json.Marshal(map[string]any{"command": test.command, "arguments": test.arguments})
			if err != nil {
				t.Fatal(err)
			}
			result, err := handler.Handle(context.Background(), rpc.Call{
				Method: "operation.plan", OperationID: "rpc-parity", Params: params,
			}, nil)
			if err != nil {
				t.Fatalf("RPC plan failed: %v", err)
			}
			plan := result.(domain.OperationPlan)
			if plan.Assessment == nil || plan.Assessment.Action != test.action ||
				plan.Assessment.Changed != test.changed || plan.Target != test.target {
				t.Fatalf("RPC facts: plan=%#v", plan)
			}
			if test.noPrompt {
				if plan.ConfirmationRequest != nil {
					t.Fatalf("no-op RPC requested confirmation: %#v", plan.ConfirmationRequest)
				}
			} else if plan.ConfirmationRequest == nil ||
				!reflect.DeepEqual(prompt.Requests[0], *plan.ConfirmationRequest) {
				t.Fatalf("confirmation drift: direct=%#v RPC=%#v", prompt.Requests, plan.ConfirmationRequest)
			}

			handler.plansMu.Lock()
			stored, ok := handler.plans[plan.OperationID]
			handler.plansMu.Unlock()
			if !ok || !reflect.DeepEqual(stored.Arguments, test.arguments) {
				t.Fatalf("stored canonical arguments=%#v, present=%t, want=%#v",
					stored.Arguments, ok, test.arguments)
			}
			handler.closePlans()
		})
	}
}

func TestCoreManifestPreparationSupportBaseline(t *testing.T) {
	root, environment, _ := preparationParityFixture(t)
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"init", "start", "provision", "test-vms", "stop", "teardown", "sync", "bind",
		"clone", "code", "export", "remove", "up", "down", "info", "remote",
	} {
		definition, ok := program.manifest.Lookup(name)
		if !ok || !baselinePreparedSupport(definition) {
			t.Errorf("core command %q is not supported: definition=%#v", name, definition)
		}
	}
	if definition, ok := program.manifest.Lookup("svc"); !ok || baselinePreparedSupport(definition) {
		t.Fatalf("profile resources entered the core resolver: %#v", definition)
	}

	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	handler := &rpcHandler{cli: program, loaded: loaded}
	for _, test := range []struct {
		command string
		code    string
	}{
		{command: "info", code: "command_not_mutating"},
		{command: "remote", code: "command_not_found"},
	} {
		params, _ := json.Marshal(map[string]any{"command": test.command, "arguments": []string{}})
		_, err := handler.Handle(context.Background(), rpc.Call{
			Method: "operation.plan", OperationID: "negative-" + test.command, Params: params,
		}, nil)
		fault, ok := err.(*rpc.Error)
		if !ok || fault.Code != test.code {
			t.Errorf("RPC %s error=%#v, want %s", test.command, err, test.code)
		}
	}
}

func preparationParityFixture(t *testing.T) (string, []string, string) {
	t.Helper()
	root, environment, stateDirectory := nativeFixture(t)
	registry, err := os.ReadFile(filepath.Join(repositoryRoot(t), "config", "commands.registry"))
	if err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(root, "config", "commands.registry"), string(registry), 0o600)
	return root, environment, stateDirectory
}

func preparationRunningYard(
	_ *testing.T, _ string, environment []string, _ string,
) ([]string, *testkit.Incus, ports.YardExecutor, ports.InitPlatform) {
	incus := lifecycleIncus()
	instance := incus.Instances["subyard/yard"]
	instance.Status = "Running"
	incus.Instances["subyard/yard"] = instance
	incus.Reconcile = ports.ReconcileState{InstanceFound: true}
	return environment, incus, preparationParityProbe{}, nil
}

func preparationProject(
	t *testing.T, root string, environment []string, stateDirectory string,
) ([]string, *testkit.Incus, ports.YardExecutor, ports.InitPlatform) {
	t.Helper()
	record := projectRemovalRecord(domain.ProjectSync)
	record.Target = "demo"
	profileDirectory := filepath.Join(root, "config", "profiles", "demo")
	if err := os.MkdirAll(profileDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(profileDirectory, "profile.conf"), "PROFILE_NAME=demo\nPROJECT_ENV_BASE_IMAGE=debian:13\n", 0o600)
	store, err := state.NewFileStore(stateDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	incus := lifecycleIncus()
	instance := incus.Instances["subyard/yard"]
	instance.Status = "Running"
	incus.Instances["subyard/yard"] = instance
	return environment, incus, preparationParityProbe{}, nil
}

func preparationHeldTestVMSlot(
	_ *testing.T, _ string, environment []string, _ string,
) ([]string, *testkit.Incus, ports.YardExecutor, ports.InitPlatform) {
	environment = append(environment, "NESTED_E2E_VMS=1")
	incus := lifecycleIncus()
	instance := incus.Instances["subyard/yard"]
	instance.Status = "Running"
	incus.Instances["subyard/yard"] = instance
	probe := preparationParityProbe{output: []byte(`{"schema_version":1,"status":"ok","pool":{"schema_version":1,"resource_type":"agent-e2e","resource_id":"test-vms","slots":[{"slot_id":"slot-001","resource_generation":7,"lease_epoch":3,"state":"held"}]}}`)}
	return environment, incus, probe, nil
}

func preparationConvergedInit(
	t *testing.T, _ string, environment []string, stateDirectory string,
) ([]string, *testkit.Incus, ports.YardExecutor, ports.InitPlatform) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(stateDirectory), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(filepath.Dir(stateDirectory), "host-id"), "yard\n", 0o600)
	platform := newInitPlatformFixture()
	for stage := range platform.converged {
		platform.converged[stage] = true
	}
	return environment, lifecycleIncus(), preparationParityProbe{exitCode: 1}, platform
}

func baselinePreparedSupport(definition command.Definition) bool {
	behavior, err := resolveCoreCommand(definition)
	return err == nil && behavior.prepare != nil
}

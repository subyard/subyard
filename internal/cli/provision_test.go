package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestProvisionSelectionUsesYardThenProjectProfiles(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	writeProvisionProfile(t, root, "android")
	writeProvisionProfile(t, root, "hermes")
	writeProvisionProfile(t, root, "openclaw")
	writeProvisionProfile(t, root, "subyard-dev")
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	loaded.Environment["YARD_PROFILES"] = "openclaw android"
	execution, err := program.prepareProvisionExecution(loaded, nil, &projectExecution{
		Environment: map[string]string{"SUBYARD_PROJECT_PROFILES": "android subyard-dev"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(execution.profiles, []string{"openclaw", "android", "subyard-dev"}) {
		t.Fatalf("profiles=%v", execution.profiles)
	}
	loaded.Environment["YARD_PROFILES"] = "hermes"
	execution, err = program.prepareProvisionExecution(loaded, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(execution.profiles, []string{"hermes"}) {
		t.Fatalf("Hermes no-argument profiles=%v", execution.profiles)
	}
}

func TestProvisionRejectsHookWithoutCheckProtocol(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	directory := filepath.Join(root, "config", "profiles", "legacy")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(directory, "profile.conf"), "PROFILE_NAME=legacy\n", 0o600)
	writeCLIFile(t, filepath.Join(directory, "provision.sh"), "true\n", 0o700)
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := program.prepareProvisionExecution(loaded, []string{"legacy"}, nil); err == nil {
		t.Fatal("legacy provision hook without check protocol was accepted")
	}
}

func TestProvisionAssessmentChecksRunningProfilesReadOnly(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	writeProvisionProfile(t, root, "android")
	writeProvisionProfile(t, root, "openclaw")
	incus := lifecycleIncus()
	instance := incus.Instances["subyard/yard"]
	instance.Status = "Running"
	incus.Instances["subyard/yard"] = instance
	runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{
		{Result: domain.AdapterResult{Schema: 1, OperationID: "provision-assessment", Status: "ok"}, Stderr: "converged\n"},
		{Result: domain.AdapterResult{Schema: 1, OperationID: "provision-assessment", Status: "ok"}, Stderr: "changed\n"},
	}}
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: append(environment,
			"SUBYARD_OPERATION_ID=provision-assessment"),
		WorkingDir: root, Incus: incus, AdapterRunner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	loaded.Environment["YARD_PROFILES"] = "android openclaw"
	execution, err := program.prepareProvisionExecution(loaded, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	definition, ok := program.manifest.Lookup("provision")
	if !ok {
		t.Fatal("provision definition is missing")
	}
	action, delta, typed, err := program.assessStructuredAction(
		context.Background(), loaded, definition, nil, nil, nil, execution, nil,
	)
	if err != nil || !typed || action != "yard.provision" || !delta.Changed ||
		!slices.Equal(execution.changedProfiles, []string{"openclaw"}) {
		t.Fatalf("action=%q delta=%#v typed=%t changed=%v err=%v",
			action, delta, typed, execution.changedProfiles, err)
	}
	if len(runner.Requests) != 2 || runner.Requests[0].Action != "profile-check" ||
		!slices.Equal(runner.Requests[0].Arguments, []string{"--check", "android"}) {
		t.Fatalf("checks=%#v", runner.Requests)
	}
}

func TestProvisionCLIAndRPCUseNativeRunner(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	writeProvisionProfile(t, root, "subyard-dev")
	for _, rpcMode := range []bool{false, true} {
		incus := lifecycleIncus()
		instance := incus.Instances["subyard/yard"]
		instance.Status = "Running"
		incus.Instances["subyard/yard"] = instance
		clock := testkit.NewManualClock(time.Unix(100, 0))
		operationID := "provision-cli"
		if rpcMode {
			operationID = "provision-rpc"
		}
		okResult := domain.AdapterResult{Schema: 1, OperationID: operationID, Status: "ok"}
		runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{
			{Result: okResult, Stderr: "changed\n"},
			{Result: okResult, Stderr: "changed\n"},
			{Result: okResult, Stderr: "changed\n"},
			{Result: okResult},
			{Result: okResult, Stderr: "converged\n"},
		}}
		options := Options{
			RepositoryRoot: root, Program: "yard", Environment: environment,
			WorkingDir: root, Incus: incus, AdapterRunner: runner, Clock: clock,
		}
		if !rpcMode {
			options.Arguments = []string{"provision", "--yes"}
			options.Environment = append(slices.Clone(environment), "SUBYARD_OPERATION_ID="+operationID)
			program, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code != 0 {
				t.Fatalf("CLI provision failed with %d", code)
			}
		} else {
			program, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]rpcPlannedOperation)}
			if _, err := handler.Handle(context.Background(), rpc.Call{
				ID: "plan", OperationID: operationID, Method: "operation.plan",
				Params: json.RawMessage(`{"command":"provision","arguments":[]}`),
			}, func(string, any) (uint64, error) { return 1, nil }); err != nil {
				t.Fatal(err)
			}
			if _, err := handler.Handle(context.Background(), rpc.Call{
				ID: "execute", OperationID: operationID, Method: "operation.execute",
				Params: json.RawMessage(`{"confirmed":true}`),
			}, func(string, any) (uint64, error) { return 1, nil }); err != nil {
				t.Fatal(err)
			}
		}
		if len(runner.Requests) != 5 || runner.Requests[3].Action != "profile" ||
			!slices.Equal(runner.Requests[3].Arguments, []string{"subyard-dev"}) {
			t.Fatalf("rpc=%v physical=%v", rpcMode, runner.Requests)
		}
		for _, index := range []int{0, 1, 2, 4} {
			if runner.Requests[index].Action != "profile-check" ||
				!slices.Equal(runner.Requests[index].Arguments, []string{"--check", "subyard-dev"}) {
				t.Fatalf("rpc=%v check[%d]=%#v", rpcMode, index, runner.Requests[index])
			}
		}
	}
}

func TestProvisionNoOpSkipsPromptAndApply(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	writeProvisionProfile(t, root, "subyard-dev")
	incus := lifecycleIncus()
	instance := incus.Instances["subyard/yard"]
	instance.Status = "Running"
	incus.Instances["subyard/yard"] = instance
	runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{
		Result: domain.AdapterResult{Schema: 1, OperationID: "provision-noop", Status: "ok"},
		Stderr: "converged\n",
	}}}
	prompt := &testkit.Prompt{}
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"provision", "subyard-dev"},
		Environment: append(environment, "SUBYARD_OPERATION_ID=provision-noop"),
		WorkingDir:  root, Incus: incus, AdapterRunner: runner, Prompt: prompt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("provision no-op failed with %d", code)
	}
	if len(prompt.Requests) != 0 || len(runner.Requests) != 1 || runner.Requests[0].Action != "profile-check" {
		t.Fatalf("no-op prompted or applied: prompts=%#v requests=%#v", prompt.Requests, runner.Requests)
	}
}

func writeProvisionProfile(t *testing.T, root, name string) {
	t.Helper()
	directory := filepath.Join(root, "config", "profiles", name)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(directory, "profile.conf"), "PROFILE_NAME="+name+"\n", 0o600)
	writeCLIFile(t, filepath.Join(directory, "provision.sh"), "#!/usr/bin/env bash\n# subyard-provision-check-v1\ntrue\n", 0o700)
}

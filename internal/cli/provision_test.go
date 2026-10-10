package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

func TestProvisionSelectionUsesYardThenProjectProfiles(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	writeProvisionProfile(t, root, "sample-a")
	writeProvisionProfile(t, root, "sample-h")
	writeProvisionProfile(t, root, "sample-o")
	writeProvisionProfile(t, root, "sample-s")
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	loaded.Environment["ENVIRONMENT_PROFILES"] = "sample-o sample-a"
	execution, err := program.prepareProvisionExecution(loaded, nil, &projectExecution{
		Environment: map[string]string{"SUBYARD_PROJECT_PROFILES": "sample-a sample-s"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(execution.profiles, []string{"sample-o", "sample-a", "sample-s"}) {
		t.Fatalf("profiles=%v", execution.profiles)
	}
	loaded.Environment["ENVIRONMENT_PROFILES"] = "sample-h"
	execution, err = program.prepareProvisionExecution(loaded, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(execution.profiles, []string{"sample-h"}) {
		t.Fatalf("SampleService no-argument profiles=%v", execution.profiles)
	}
}

func TestDedicatedProvisionRequiresOptIn(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	writeProvisionProfile(t, root, "service")
	writeCLIFile(t, filepath.Join(root, "config/profiles/service/profile.conf"), "PROFILE_NAME=service\nPROVISION_SCOPE=dedicated\n", 0o600)
	writeProvisionProfile(t, root, "sample-a")
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	for _, selection := range []string{"", "sample-a", "service"} {
		loaded.Environment["ENVIRONMENT_PROFILES"] = selection
		execution, err := program.prepareProvisionExecution(loaded, nil, nil)
		if err != nil || slices.Contains(execution.profiles, "service") {
			t.Fatalf("ordinary yard selected dedicated service: %v, %v", execution, err)
		}
	}
	if _, err := program.prepareProvisionExecution(loaded, []string{"service"}, nil); err == nil {
		t.Fatal("explicit hook bypassed the dedicated-yard boundary")
	}
	loaded.Environment["EXCLUSIVE_ENVIRONMENT_PROFILE"] = "service"
	loaded.Environment["ENVIRONMENT_PROFILES"] = "service"
	execution, err := program.prepareProvisionExecution(loaded, nil, nil)
	if err != nil || !slices.Equal(execution.profiles, []string{"service"}) {
		t.Fatalf("dedicated service selection failed: %v, %v", execution, err)
	}
}

func TestDeclaredProvisionSelectionPreservesLegacyProfiles(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	writeProvisionProfile(t, root, "selected")
	writeProvisionProfile(t, root, "legacy")
	testkit.WriteFile(t, filepath.Join(root, "config", "profiles", "selected", "profile.json"), []byte(`{"schema_version":1,"selected_provision_only":true,"default_yards":["default"],"disabled_when":{"DISABLE_SERVICE":"1"}}`), 0o644)
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	empty, selected := "", "selected legacy"
	for _, scenario := range []struct {
		yard      string
		selection *string
		disabled  bool
		selected  bool
	}{
		{yard: "default", selected: true}, {yard: "other", selected: false},
		{yard: "default", selection: &empty, selected: false},
		{yard: "other", selection: &selected, selected: true},
		{yard: "default", disabled: true, selected: false},
		{yard: "other", selection: &selected, disabled: true, selected: false},
	} {
		loaded.Context.YardName = scenario.yard
		delete(loaded.Environment, "ENVIRONMENT_PROFILES")
		delete(loaded.Environment, "DISABLE_SERVICE")
		if scenario.disabled {
			loaded.Environment["DISABLE_SERVICE"] = "1"
		}
		if scenario.selection != nil {
			loaded.Environment["ENVIRONMENT_PROFILES"] = *scenario.selection
		}
		execution, err := program.prepareProvisionExecution(loaded, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(execution.profiles, "selected") != scenario.selected || !slices.Contains(execution.profiles, "legacy") {
			t.Fatalf("unexpected profile selection for %s: %v", scenario.yard, execution.profiles)
		}
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
	writeProvisionProfile(t, root, "sample-a")
	writeProvisionProfile(t, root, "sample-o")
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
	loaded.Environment["YARD_PROFILES"] = "sample-a sample-o"
	execution, err := program.prepareProvisionExecution(loaded, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	definition, ok := program.manifest.Lookup("provision")
	if !ok {
		t.Fatal("provision definition is missing")
	}
	if err := program.observeProvisionExecution(context.Background(), loaded, definition, execution); err != nil {
		t.Fatal(err)
	}
	action, delta, err := execution.actionPlan(definition, loaded.Context)
	if err != nil || action != "yard.provision" || !delta.Changed ||
		!slices.Equal(execution.changedProfiles, []string{"sample-o"}) {
		t.Fatalf("action=%q delta=%#v changed=%v err=%v",
			action, delta, execution.changedProfiles, err)
	}
	if len(runner.Requests) != 2 || runner.Requests[0].Action != "profile-check" ||
		!slices.Equal(runner.Requests[0].Arguments, []string{"--check", "sample-a"}) {
		t.Fatalf("checks=%#v", runner.Requests)
	}
}

func TestProvisionCLIAndRPCUseNativeRunner(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	writeProvisionProfile(t, root, "sample-s")
	dispatcher := filepath.Join(root, "selected-engine")
	writeCLIFile(t, dispatcher, "#!/bin/sh\nexit 0\n", 0o700)
	environment = append(environment, "YARD_KIND=vm", "VM_CPU_WEIGHT=100", "SUBYARD_DISPATCHER_PATH=/ambient/engine")
	for _, scenario := range []struct{ rpcMode, stopped bool }{{false, true}, {true, true}, {false, false}, {true, false}} {
		rpcMode := scenario.rpcMode
		incus := lifecycleIncus()
		instance := incus.Instances["subyard/yard"]
		instance.Status = "Running"
		if scenario.stopped {
			instance.Status = "Stopped"
		}
		instance.Type = domain.YardVM
		instance.Config["user.subyard.vm_cpu_weight"] = "1000"
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
		if scenario.stopped {
			runner.Steps = []testkit.AdapterStep{
				{Result: okResult},
				{Result: okResult, Stderr: "changed\n"},
				{Result: okResult},
				{Result: okResult, Stderr: "converged\n"},
				{Result: okResult},
			}
			powerScriptedIncus(runner, incus)
		}
		options := Options{
			RepositoryRoot: root, Program: "yard", Environment: environment,
			WorkingDir: root, Incus: incus, AdapterRunner: runner, Clock: clock,
			DispatcherPath: dispatcher,
			NetworkPolicy:  &yardnetwork.Service{Host: &networkCLIHost{}, Lock: testNetworkPolicyLock{}},
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
			handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]*preparedCommand)}
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
		profileIndex := 3
		checkIndexes := []int{0, 1, 2, 4}
		if scenario.stopped {
			profileIndex, checkIndexes = 2, []int{1, 3}
			if len(runner.Requests) != 5 || runner.Requests[0].Action != "start" ||
				runner.Requests[4].Action != "stop" || incus.Instances["subyard/yard"].Status != "Stopped" {
				t.Fatalf("temporary startup did not restore power: rpc=%v state=%s requests=%d", rpcMode,
					incus.Instances["subyard/yard"].Status, len(runner.Requests))
			}
		}
		if len(runner.Requests) != 5 || runner.Requests[profileIndex].Action != "profile" ||
			!slices.Equal(runner.Requests[profileIndex].Arguments, []string{"sample-s"}) {
			t.Fatalf("rpc=%v physical=%v", rpcMode, runner.Requests)
		}
		for _, request := range runner.Requests {
			if request.Action != "profile-check" && (request.Context["SUBYARD_DISPATCHER_PATH"] != dispatcher ||
				request.Context["VM_CPU_WEIGHT"] != "1000") {
				t.Fatalf("provision lost selected scheduler: rpc=%v action=%s", rpcMode, request.Action)
			}
		}
		for _, index := range checkIndexes {
			if runner.Requests[index].Action != "profile-check" ||
				!slices.Equal(runner.Requests[index].Arguments, []string{"--check", "sample-s"}) {
				t.Fatalf("rpc=%v check[%d]=%#v", rpcMode, index, runner.Requests[index])
			}
		}
	}
}

func TestProvisionCPUAuthorizationPrecedesTemporaryStart(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	writeProvisionProfile(t, root, "sample-s")
	commands := testkit.TempDir(t)
	writeCLIFile(t, filepath.Join(commands, "systemctl"), "#!/bin/sh\nprintf 'inactive\\n'\nexit 3\n", 0o700)
	writeCLIFile(t, filepath.Join(commands, "sudo"), "#!/bin/sh\nexit 1\n", 0o700)
	t.Setenv("PATH", commands)
	incus := lifecycleIncus()
	incus.Instances["subyard/yard"].Config["user.subyard.vm_cpu_weight"] = "1000"
	program, err := New(Options{RepositoryRoot: root, WorkingDir: root, Incus: incus,
		Environment: append(environment, "YARD_KIND=vm", "PATH="+commands)})
	if err != nil {
		t.Fatal(err)
	}
	program.effectiveUID = func() int { return 1000 }
	program.operatorTerminal = func() bool { return false }
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	execution, err := program.prepareProvisionExecution(loaded, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	definition, _ := program.manifest.Lookup("provision")
	if err := program.observeProvisionExecution(context.Background(), loaded, definition, execution); err != nil {
		t.Fatal(err)
	}
	runner := &testkit.ScriptedAdapter{}
	_, err = program.executeProvision(context.Background(), &application.Orchestrator{Runner: runner}, loaded,
		domain.OperationPlan{OperationID: "provision-auth"}, execution, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "sudo authorization is required") || len(runner.Requests) != 0 ||
		incus.Instances["subyard/yard"].Status != "Stopped" {
		t.Fatalf("CPU authorization did not precede startup: err=%v requests=%d state=%s", err,
			len(runner.Requests), incus.Instances["subyard/yard"].Status)
	}
}

func TestProvisionNoOpSkipsPromptAndApply(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	platform := convergedProvisionInit(t, root)
	writeCLIFile(t, filepath.Join(root, "config", "host.env"), "ENVIRONMENT_PROFILES=sample-s\n", 0o600)
	writeProvisionProfile(t, root, "sample-s")
	incus := lifecycleIncus()
	instance := incus.Instances["subyard/yard"]
	instance.Status = "Running"
	incus.Instances["subyard/yard"] = instance
	runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{
		Result: domain.AdapterResult{Schema: 1, OperationID: "provision-noop", Status: "ok"},
		Stderr: "converged\n",
	}, {
		Result: domain.AdapterResult{Schema: 1, OperationID: "provision-noop", Status: "ok"},
		Stderr: "converged\n",
	}}}
	prompt := &testkit.Prompt{}
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"provision", "sample-s"},
		Environment: append(environment, "SUBYARD_OPERATION_ID=provision-noop"),
		WorkingDir:  root, Incus: incus, AdapterRunner: runner, Prompt: prompt, InitPlatform: platform,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("provision no-op failed with %d", code)
	}
	if len(prompt.Requests) != 0 || len(runner.Requests) != 2 || runner.Requests[0].Action != "profile-check" || runner.Requests[1].Action != "profile-check" {
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

func convergedProvisionInit(t *testing.T, root string) *initPlatformFixture {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(root, "state", "host-id"), "5034c950-74d0-46c4-9428-b7835e602109\n", 0o600)
	platform := newInitPlatformFixture()
	for stage := range platform.converged {
		platform.converged[stage] = true
	}
	return platform
}

func TestProvisionApprovalRetainsFirstObservedProfileDecisions(t *testing.T) {
	execution := &provisionExecution{profiles: []string{"sample-a", "sample-b"}, changedProfiles: []string{"sample-b"}}
	execution.captureApproved(false)
	execution.changedProfiles = []string{"sample-a"}
	execution.requiresPowerCycle = true
	execution.captureApproved(true)
	if len(execution.approvedProfiles) != 2 || !execution.approvedProfiles[0].Converged || execution.approvedProfiles[1].Converged || execution.approvedProfiles[0].Conditional || execution.approvedPowerCycle {
		t.Fatalf("live observation broadened approval: profiles=%+v power=%t", execution.approvedProfiles, execution.approvedPowerCycle)
	}
}

func TestProvisionPendingPrerequisiteCapturesConditionalProfiles(t *testing.T) {
	execution := &provisionExecution{profiles: []string{"sample-a"}}
	execution.captureApproved(true)
	if !execution.approvedProfiles[0].Conditional || execution.approvedProfiles[0].Converged || !execution.approvedPowerCycle {
		t.Fatalf("conditional approval=%+v power=%t", execution.approvedProfiles, execution.approvedPowerCycle)
	}
}

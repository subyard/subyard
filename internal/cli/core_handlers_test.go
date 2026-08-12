package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/testvmsruntime"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestTestVMsUsesTypedWorkerInvocation(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	environment = append(environment, "NESTED_E2E_VMS=1", "SUBYARD_OPERATION_ID=test-vms-revoke")
	incus := lifecycleIncus()
	instance := incus.Instances["subyard/yard"]
	instance.Status = "Running"
	incus.Instances["subyard/yard"] = instance
	runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{
		Schema: 1, OperationID: "test-vms-revoke", Status: "ok",
	}}}}
	prompt := &testkit.Prompt{Answers: []bool{true}}
	probe := &testVMStatusProbe{output: []byte(`{"schema_version":1,"status":"ok","pool":{"schema_version":1,"resource_type":"agent-e2e","resource_id":"test-vms","slots":[{"slot_id":"slot-001","resource_generation":1,"state":"available"},{"slot_id":"slot-002","resource_generation":7,"lease_epoch":3,"state":"held"}]}}`)}
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"test-vms", "revoke", "--slot", "2"},
		Environment: environment, WorkingDir: root, Incus: incus, ProjectData: probe, AdapterRunner: runner,
		Prompt: prompt, Clock: testkit.NewManualClock(time.Unix(100, 0)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("test-vms failed with %d", code)
	}
	if len(runner.Requests) != 1 || runner.Requests[0].Adapter != "test-vms" ||
		runner.Requests[0].Action != "revoke" ||
		!slices.Equal(runner.Requests[0].Arguments, []string{
			"revoke-slot-2",
			"--expect-resource-generation", "7",
			"--expect-lease-epoch", "3",
			"--yes",
		}) {
		t.Fatalf("requests=%#v", runner.Requests)
	}
	if len(prompt.Requests) != 1 ||
		prompt.Requests[0].Summary != "Revoke test VM lease slot" ||
		prompt.Requests[0].Default != domain.ConfirmationDefaultYes {
		t.Fatalf("confirmation requests=%#v", prompt.Requests)
	}
}

func TestTestVMsMapsWorkerStaleIdentityToPlanStale(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	environment = append(environment, "NESTED_E2E_VMS=1", "SUBYARD_OPERATION_ID=test-vms-stale")
	incus := lifecycleIncus()
	instance := incus.Instances["subyard/yard"]
	instance.Status = "Running"
	incus.Instances["subyard/yard"] = instance
	workerErr := exec.Command("sh", "-c", "exit 75").Run()
	runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Err: workerErr}}}
	probe := &testVMStatusProbe{output: []byte(`{"schema_version":1,"status":"ok","pool":{"schema_version":1,"resource_type":"agent-e2e","resource_id":"test-vms","slots":[{"slot_id":"slot-001","resource_generation":1,"state":"available"},{"slot_id":"slot-002","resource_generation":7,"lease_epoch":3,"state":"held"}]}}`)}
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"test-vms", "revoke", "--slot", "2"},
		Environment: environment, WorkingDir: root, Incus: incus, ProjectData: probe,
		AdapterRunner: runner, Prompt: &testkit.Prompt{Answers: []bool{true}}, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 ||
		!strings.Contains(stderr.String(), domain.ErrPlanStale.Error()) {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}

func TestRemoteTestVMsForwardsConfirmedLeaseIdentity(t *testing.T) {
	root, environment, stateDirectory := nativeFixture(t)
	configHome := filepath.Dir(stateDirectory)
	if err := os.MkdirAll(filepath.Join(configHome, "yards", "remote"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(configHome, "yards", "remote", "config.env"),
		"ACCESS_KIND=remote\nREMOTE_DEST=owner.example\nREMOTE_YARD=inner\n"+
			"SSH_PORT=4444\nNESTED_E2E_VMS=1\n", 0o600)
	fakeBin := filepath.Join(root, "fake-bin")
	if err := os.MkdirAll(fakeBin, 0o700); err != nil {
		t.Fatal(err)
	}
	sshLog := filepath.Join(root, "remote-test-vms-ssh.log")
	writeCLIFile(t, filepath.Join(fakeBin, "ssh"), `#!/bin/sh
printf '%s\n' "$@" >"$SUBYARD_TEST_SSH_LOG"
`, 0o700)
	t.Setenv("PATH", fakeBin+":"+os.Getenv("PATH"))
	environment = append(environment,
		"PATH="+os.Getenv("PATH"),
		"SUBYARD_TEST_SSH_LOG="+sshLog,
		"SUBYARD_OPERATION_ID=remote-test-vms-revoke",
	)
	probe := &testVMStatusProbe{output: []byte(`{"schema_version":1,"status":"ok","pool":{"schema_version":1,"resource_type":"agent-e2e","resource_id":"test-vms","slots":[{"slot_id":"slot-001","resource_generation":1,"state":"available"},{"slot_id":"slot-002","resource_generation":7,"lease_epoch":3,"state":"held"}]}}`)}
	prompt := &testkit.Prompt{Answers: []bool{true}}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"-Y", "remote", "test-vms", "revoke", "--slot", "2"},
		Environment: environment, WorkingDir: root, ProjectData: probe,
		Prompt: prompt, Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("remote test-vms: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	forwarded, err := os.ReadFile(sshLog)
	if err != nil {
		t.Fatal(err)
	}
	command := string(forwarded)
	for _, expected := range []string{
		"owner.example", "test-vms", "revoke", "--slot", "2",
		"--expect-resource-generation", "7", "--expect-lease-epoch", "3", "--yes",
	} {
		if !strings.Contains(command, expected) {
			t.Fatalf("remote forwarding omitted %q:\n%s", expected, command)
		}
	}
	if len(prompt.Requests) != 1 {
		t.Fatalf("confirmation requests=%#v", prompt.Requests)
	}
}

func TestTestVMsRejectsForwardedReplacementIdentity(t *testing.T) {
	loaded := config.Loaded{Context: domain.Context{
		NestedE2EVMs: true, AccessKind: domain.AccessLocal, YardKind: domain.YardContainer,
		IncusProject: "subyard", YardInstanceName: "yard",
	}}
	probe := &testVMStatusProbe{output: []byte(`{"schema_version":1,"status":"ok","pool":{"schema_version":1,"resource_type":"agent-e2e","resource_id":"test-vms","slots":[{"slot_id":"slot-001","resource_generation":1,"state":"available"},{"slot_id":"slot-002","resource_generation":7,"lease_epoch":4,"state":"held"}]}}`)}
	program := &CLI{options: Options{
		Incus: &testkit.Incus{Instances: map[string]ports.InstanceInfo{
			"subyard/yard": {Name: "yard", Project: "subyard", Status: "Running"},
		}},
		ProjectData: probe,
	}}
	_, err := program.prepareTestVMExecution(context.Background(), loaded, []string{
		"revoke", "--slot", "2",
		"--expect-resource-generation", "7", "--expect-lease-epoch", "3", "--yes",
	})
	if !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("replacement identity error = %v", err)
	}
}

func TestRemoteTestVMsForwardsAvailableSlotSnapshot(t *testing.T) {
	loaded := config.Loaded{Context: domain.Context{
		NestedE2EVMs: true, AccessKind: domain.AccessRemote, YardKind: domain.YardContainer,
	}}
	probe := &testVMStatusProbe{output: []byte(`{"schema_version":1,"status":"ok","pool":{"schema_version":1,"resource_type":"agent-e2e","resource_id":"test-vms","slots":[{"slot_id":"slot-001","resource_generation":7,"lease_epoch":0,"state":"available"}]}}`)}
	program := &CLI{options: Options{ProjectData: probe}}
	execution, err := program.prepareTestVMExecution(
		context.Background(), loaded, []string{"revoke", "--slot", "1"},
	)
	if err != nil {
		t.Fatal(err)
	}
	forwarded, err := execution.remoteArguments([]string{"revoke", "--slot", "1"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(forwarded, []string{
		"revoke", "--slot", "1",
		"--expect-resource-generation", "7", "--expect-lease-epoch", "0",
	}) {
		t.Fatalf("forwarded = %#v", forwarded)
	}
}

func TestTestVMsRejectsAvailableSlotClaimedAfterRemoteNoOp(t *testing.T) {
	loaded := config.Loaded{Context: domain.Context{
		NestedE2EVMs: true, AccessKind: domain.AccessLocal, YardKind: domain.YardContainer,
		IncusProject: "subyard", YardInstanceName: "yard",
	}}
	probe := &testVMStatusProbe{output: []byte(`{"schema_version":1,"status":"ok","pool":{"schema_version":1,"resource_type":"agent-e2e","resource_id":"test-vms","slots":[{"slot_id":"slot-001","resource_generation":7,"lease_epoch":1,"state":"held"}]}}`)}
	program := &CLI{options: Options{
		Incus: &testkit.Incus{Instances: map[string]ports.InstanceInfo{
			"subyard/yard": {Name: "yard", Project: "subyard", Status: "Running"},
		}},
		ProjectData: probe,
	}}
	_, err := program.prepareTestVMExecution(context.Background(), loaded, []string{
		"revoke", "--slot", "1",
		"--expect-resource-generation", "7", "--expect-lease-epoch", "0", "--yes",
	})
	if !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("available replacement identity error = %v", err)
	}
}

func TestTestVMStatusIsReadOnly(t *testing.T) {
	loaded := config.Loaded{Context: domain.Context{
		NestedE2EVMs: true, YardKind: domain.YardContainer,
		IncusProject: "subyard", YardInstanceName: "yard",
	}}
	program := &CLI{options: Options{Incus: &testkit.Incus{Instances: map[string]ports.InstanceInfo{
		"subyard/yard": {Status: "Running"},
	}}}}
	execution, err := program.prepareTestVMExecution(
		context.Background(), loaded, []string{"status"},
	)
	if err != nil {
		t.Fatal(err)
	}
	action, delta, err := execution.actionPlan()
	if err != nil || action != "test-vms.status" || delta.Changed {
		t.Fatalf("action=%q delta=%#v err=%v", action, delta, err)
	}
}

func TestTestVMStatusExecutesReadOnlyWorkerWithoutConfirmation(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	environment = append(environment, "NESTED_E2E_VMS=1", "SUBYARD_OPERATION_ID=test-vms-status")
	incus := lifecycleIncus()
	instance := incus.Instances["subyard/yard"]
	instance.Status = "Running"
	incus.Instances["subyard/yard"] = instance
	runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{
		Schema: 1, OperationID: "test-vms-status", Status: "ok",
	}}}}
	prompt := &testkit.Prompt{}
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"test-vms", "status"},
		Environment: environment, WorkingDir: root, Incus: incus, AdapterRunner: runner,
		Prompt: prompt, Clock: testkit.NewManualClock(time.Unix(100, 0)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("test-vms status failed with %d", code)
	}
	if len(prompt.Requests) != 0 {
		t.Fatalf("read-only status prompted: %#v", prompt.Requests)
	}
	if len(runner.Requests) != 1 || runner.Requests[0].Adapter != "test-vms" ||
		runner.Requests[0].Action != "status" ||
		!slices.Equal(runner.Requests[0].Arguments, []string{"status"}) {
		t.Fatalf("status requests=%#v", runner.Requests)
	}
}

func TestTestVMsRejectsStoppedYardBeforePrompt(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	environment = append(environment, "NESTED_E2E_VMS=1")
	prompt := &testkit.Prompt{}
	runner := &testkit.ScriptedAdapter{}
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"test-vms", "recover", "--slot", "2"},
		Environment: environment, WorkingDir: root, Incus: lifecycleIncus(), AdapterRunner: runner,
		Prompt: prompt, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 2 || !strings.Contains(stderr.String(), "must be running") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if len(prompt.Requests) != 0 || len(runner.Requests) != 0 {
		t.Fatalf("stopped-yard preflight prompted or applied: prompts=%#v requests=%#v", prompt.Requests, runner.Requests)
	}
}

func TestTestVMsDeclineLeavesWorkerUntouched(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	environment = append(environment, "NESTED_E2E_VMS=1", "SUBYARD_OPERATION_ID=test-vms-decline")
	incus := lifecycleIncus()
	instance := incus.Instances["subyard/yard"]
	instance.Status = "Running"
	incus.Instances["subyard/yard"] = instance
	prompt := &testkit.Prompt{Answers: []bool{false}}
	runner := &testkit.ScriptedAdapter{}
	probe := &testVMStatusProbe{output: []byte(`{"schema_version":1,"status":"ok","pool":{"schema_version":1,"resource_type":"agent-e2e","resource_id":"test-vms","slots":[{"slot_id":"slot-001","resource_generation":1,"state":"available"},{"slot_id":"slot-002","resource_generation":7,"lease_epoch":3,"state":"held"}]}}`)}
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"test-vms", "revoke", "--slot", "2"},
		Environment: environment, WorkingDir: root, Incus: incus, ProjectData: probe, AdapterRunner: runner,
		Prompt: prompt, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 || !strings.Contains(stderr.String(), "operation declined") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if len(prompt.Requests) != 1 || prompt.Requests[0].Default != domain.ConfirmationDefaultYes ||
		len(runner.Requests) != 0 || len(incus.PowerUpdates) != 0 {
		t.Fatalf("decline mutated state: prompts=%#v requests=%#v power=%#v", prompt.Requests, runner.Requests, incus.PowerUpdates)
	}
}

func TestTestVMRevokeAvailableSlotIsNoOpBeforeConfirmation(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	environment = append(environment, "NESTED_E2E_VMS=1", "SUBYARD_OPERATION_ID=test-vms-noop")
	incus := lifecycleIncus()
	instance := incus.Instances["subyard/yard"]
	instance.Status = "Running"
	incus.Instances["subyard/yard"] = instance
	probe := &testVMStatusProbe{output: []byte(`{"schema_version":1,"status":"ok","pool":{"schema_version":1,"resource_type":"agent-e2e","resource_id":"test-vms","slots":[{"slot_id":"slot-001","state":"available"},{"slot_id":"slot-002","state":"available"}]}}`)}
	prompt := &testkit.Prompt{}
	runner := &testkit.ScriptedAdapter{}
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"test-vms", "revoke", "--slot", "2"},
		Environment: environment, WorkingDir: root, Incus: incus, ProjectData: probe,
		AdapterRunner: runner, Prompt: prompt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("test-vms returned %d", code)
	}
	if len(prompt.Requests) != 0 || len(runner.Requests) != 0 {
		t.Fatalf("available revoke was not a no-op: prompts=%#v requests=%#v", prompt.Requests, runner.Requests)
	}
	if len(probe.requests) != 1 || !slices.Equal(probe.requests[0].Command,
		[]string{"/usr/local/libexec/subyard/test-vms-inner", "_test-vms-worker", "status"}) {
		t.Fatalf("status probe=%#v", probe.requests)
	}
}

func TestTestVMRecoverAvailableSlotIsNoOpBeforeConfirmation(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	environment = append(environment, "NESTED_E2E_VMS=1", "SUBYARD_OPERATION_ID=test-vms-recover-noop")
	incus := lifecycleIncus()
	instance := incus.Instances["subyard/yard"]
	instance.Status = "Running"
	incus.Instances["subyard/yard"] = instance
	probe := &testVMStatusProbe{output: []byte(`{"schema_version":1,"status":"ok","pool":{"schema_version":1,"resource_type":"agent-e2e","resource_id":"test-vms","slots":[{"slot_id":"slot-001","state":"available"},{"slot_id":"slot-002","state":"available"}]}}`)}
	prompt := &testkit.Prompt{}
	runner := &testkit.ScriptedAdapter{}
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"test-vms", "recover", "--slot", "2"},
		Environment: environment, WorkingDir: root, Incus: incus, ProjectData: probe,
		AdapterRunner: runner, Prompt: prompt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("test-vms returned %d", code)
	}
	if len(prompt.Requests) != 0 || len(runner.Requests) != 0 {
		t.Fatalf("available recover was not a no-op: prompts=%#v requests=%#v", prompt.Requests, runner.Requests)
	}
}

func TestTestVMRecoverInProgressIsNoOpBeforeConfirmation(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	environment = append(environment, "NESTED_E2E_VMS=1", "SUBYARD_OPERATION_ID=test-vms-recovering-noop")
	incus := lifecycleIncus()
	instance := incus.Instances["subyard/yard"]
	instance.Status = "Running"
	incus.Instances["subyard/yard"] = instance
	probe := &testVMStatusProbe{output: []byte(`{"schema_version":1,"status":"ok","pool":{"schema_version":1,"resource_type":"agent-e2e","resource_id":"test-vms","slots":[{"slot_id":"slot-001","resource_generation":1,"state":"available"},{"slot_id":"slot-002","resource_generation":4,"lease_epoch":8,"state":"recovering"}]}}`)}
	prompt := &testkit.Prompt{}
	runner := &testkit.ScriptedAdapter{}
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"test-vms", "recover", "--slot", "2"},
		Environment: environment, WorkingDir: root, Incus: incus, ProjectData: probe,
		AdapterRunner: runner, Prompt: prompt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("test-vms returned %d", code)
	}
	if len(prompt.Requests) != 0 || len(runner.Requests) != 0 {
		t.Fatalf("recovering slot was not a no-op: prompts=%#v requests=%#v", prompt.Requests, runner.Requests)
	}
}

func TestTestVMPreflightRejectsNoncanonicalSlotInventory(t *testing.T) {
	for _, test := range []struct {
		name  string
		slots string
	}{
		{
			name:  "duplicate",
			slots: `[{"slot_id":"slot-001","state":"held"},{"slot_id":"slot-001","state":"available"}]`,
		},
		{
			name:  "noncanonical id",
			slots: `[{"slot_id":"slot-001","state":"held"},{"slot_id":"slot-2","state":"available"}]`,
		},
		{
			name:  "unknown state",
			slots: `[{"slot_id":"slot-001","state":"held"},{"slot_id":"slot-002","state":"unknown"}]`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			environment = append(environment, "NESTED_E2E_VMS=1")
			incus := lifecycleIncus()
			instance := incus.Instances["subyard/yard"]
			instance.Status = "Running"
			incus.Instances["subyard/yard"] = instance
			probe := &testVMStatusProbe{output: []byte(
				`{"schema_version":1,"status":"ok","pool":{"schema_version":1,"resource_type":"agent-e2e","resource_id":"test-vms","slots":` + test.slots + `}}`,
			)}
			prompt := &testkit.Prompt{}
			runner := &testkit.ScriptedAdapter{}
			var stderr bytes.Buffer
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard",
				Arguments:   []string{"test-vms", "revoke", "--slot", "1"},
				Environment: environment, WorkingDir: root, Incus: incus, ProjectData: probe,
				AdapterRunner: runner, Prompt: prompt, Stderr: &stderr,
			})
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code != 2 ||
				!strings.Contains(stderr.String(), "invalid status response") {
				t.Fatalf("code=%d stderr=%q", code, stderr.String())
			}
			if len(prompt.Requests) != 0 || len(runner.Requests) != 0 {
				t.Fatalf("invalid inventory reached prompt/apply: prompts=%#v requests=%#v",
					prompt.Requests, runner.Requests)
			}
		})
	}
}

type testVMStatusProbe struct {
	requests []ports.InstanceExecRequest
	output   []byte
	err      error
}

func (probe *testVMStatusProbe) Execute(
	_ context.Context,
	_ domain.Context,
	request ports.InstanceExecRequest,
) (ports.InstanceExecResult, error) {
	probe.requests = append(probe.requests, request)
	if probe.err != nil {
		return ports.InstanceExecResult{}, probe.err
	}
	return ports.InstanceExecResult{Stdout: probe.output}, nil
}

func (probe *testVMStatusProbe) Stream(
	context.Context,
	domain.Context,
	ports.InstanceExecRequest,
	io.Reader,
) (ports.InstanceExecResult, error) {
	return ports.InstanceExecResult{}, errors.New("unexpected stream")
}

func TestTestVMLogsReadsHostWideLogWithoutYardBrokerContext(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	dataHome := environmentValue(environment, "SUBYARD_HOME")
	recorder := testvmsruntime.EventRecorder{
		StateDir: filepath.Join(root, "broker"),
		Source:   "test-yard",
		Now:      func() time.Time { return time.Unix(100, 0).UTC() },
	}
	if _, err := recorder.Record(testvmsruntime.BrokerEvent{
		Kind:               "recovery.available",
		SlotID:             "slot-002",
		ResourceGeneration: 2,
	}); err != nil {
		t.Fatal(err)
	}
	batch, err := recorder.Export()
	if err != nil {
		t.Fatal(err)
	}
	if err := (&testvmsruntime.HostSink{
		DataHome:    dataHome,
		Now:         func() time.Time { return time.Unix(100, 0).UTC() },
		OperatorGID: -1,
	}).Ingest(batch); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root,
		Program:        "yard",
		Arguments:      []string{"test-vms", "logs", "--slot", "2", "-n", "1"},
		Environment:    environment,
		WorkingDir:     root,
		Stdout:         &stdout,
		Stderr:         &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("logs failed: code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"kind":"recovery.available"`) ||
		!strings.Contains(stdout.String(), `"slot_id":"slot-002"`) {
		t.Fatalf("host-wide log output = %q", stdout.String())
	}
}

func environmentValue(environment []string, name string) string {
	prefix := name + "="
	for _, assignment := range environment {
		if strings.HasPrefix(assignment, prefix) {
			return strings.TrimPrefix(assignment, prefix)
		}
	}
	return ""
}

func TestTeardownRejectsUnknownInputAndPublishesMode(t *testing.T) {
	if _, err := prepareTeardownExecution([]string{"keepdata"}); err == nil {
		t.Fatal("unsafe teardown argument was accepted")
	}
	root, environment, _ := nativeFixture(t)
	runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{
		Schema: 1, OperationID: "teardown-test", Status: "ok",
	}}}}
	prompt := &testkit.Prompt{Answers: []bool{true}}
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"teardown", "--keep-data"},
		Environment: append(environment, "SUBYARD_OPERATION_ID=teardown-test"), WorkingDir: root,
		AdapterRunner: runner, Prompt: prompt, Clock: testkit.NewManualClock(time.Unix(100, 0)),
		Stderr: &stderr, Incus: &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("teardown failed: code=%d stderr=%q", code, stderr.String())
	}
	if len(runner.Requests) != 1 || runner.Requests[0].Adapter != "teardown" ||
		runner.Requests[0].Context["SUBYARD_TEARDOWN_KEEP_DATA"] != "1" ||
		runner.Requests[0].Context["SUBYARD_TEARDOWN_KEEP_SHARED"] != "0" {
		t.Fatalf("requests=%#v", runner.Requests)
	}
}

func TestLifecycleExecutionBuildsTypedStopDelta(t *testing.T) {
	for _, test := range []struct {
		name      string
		execution lifecycleExecution
		action    domain.ActionID
		changed   bool
	}{
		{name: "stopped", execution: lifecycleExecution{action: "stop"}, action: "yard.stop"},
		{name: "running", execution: lifecycleExecution{action: "stop", changed: true}, action: "yard.stop", changed: true},
		{name: "forced", execution: lifecycleExecution{action: "stop", force: true, changed: true}, action: "yard.stop-force", changed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			action, delta, err := test.execution.actionPlan(command.Definition{Name: "stop"}, domain.Context{
				IncusProject: "subyard", YardInstanceName: "yard",
			})
			if err != nil || action != test.action || delta.Changed != test.changed {
				t.Fatalf("action=%q delta=%#v err=%v", action, delta, err)
			}
			if test.changed && len(delta.Consequences) == 0 {
				t.Fatal("changed lifecycle action has no consequences")
			}
		})
	}
}

func TestObserveLifecycleExecutionDetectsStoppedNoOp(t *testing.T) {
	for _, test := range []struct {
		status  string
		changed bool
	}{
		{status: "Stopped"},
		{status: "Running", changed: true},
	} {
		t.Run(test.status, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			incus := lifecycleIncus()
			instance := incus.Instances["subyard/yard"]
			instance.Status = test.status
			incus.Instances["subyard/yard"] = instance
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard", Environment: environment,
				WorkingDir: root, Incus: incus,
			})
			if err != nil {
				t.Fatal(err)
			}
			execution := &lifecycleExecution{action: "stop"}
			if err := program.observeLifecycleExecution(context.Background(), domain.Context{
				IncusProject: "subyard", YardInstanceName: "yard",
			}, execution); err != nil {
				t.Fatal(err)
			}
			if execution.changed != test.changed {
				t.Fatalf("changed=%t, want %t", execution.changed, test.changed)
			}
		})
	}
}

func TestTeardownExecutionBuildsTypedActionDelta(t *testing.T) {
	for _, test := range []struct {
		name      string
		execution teardownExecution
		action    domain.ActionID
		changed   bool
	}{
		{name: "absent", execution: teardownExecution{}, action: "yard.teardown.purge"},
		{name: "keep data", execution: teardownExecution{keepData: true, changed: true}, action: "yard.teardown.keep-data", changed: true},
		{name: "purge", execution: teardownExecution{changed: true}, action: "yard.teardown.purge", changed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			action, delta, err := test.execution.actionPlan(command.Definition{Name: "teardown"}, domain.Context{
				IncusProject: "subyard", YardInstanceName: "yard",
			})
			if err != nil || action != test.action || delta.Changed != test.changed {
				t.Fatalf("action=%q delta=%#v err=%v", action, delta, err)
			}
			if test.changed && len(delta.Consequences) == 0 {
				t.Fatal("changed teardown action has no consequences")
			}
		})
	}
}

func TestObserveTeardownExecutionDetectsOwnedTarget(t *testing.T) {
	for _, test := range []struct {
		name      string
		reconcile ports.ReconcileState
		changed   bool
	}{
		{name: "absent"},
		{name: "instance", reconcile: ports.ReconcileState{InstanceFound: true}, changed: true},
		{name: "persistent volume", reconcile: ports.ReconcileState{VolumeFound: true}, changed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			incus := lifecycleIncus()
			incus.Reconcile = test.reconcile
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard", Environment: environment,
				WorkingDir: root, Incus: incus,
			})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			execution := &teardownExecution{}
			if err := program.observeTeardownExecution(context.Background(), loaded, execution); err != nil {
				t.Fatal(err)
			}
			if execution.changed != test.changed {
				t.Fatalf("changed=%t, want %t", execution.changed, test.changed)
			}
		})
	}
}

func TestTeardownKeepsSharedIncusForAnotherRegisteredLocalYard(t *testing.T) {
	root, environment, stateDirectory := nativeFixture(t)
	yardDirectory := filepath.Join(filepath.Dir(stateDirectory), "yards", "other")
	if err := os.MkdirAll(yardDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(yardDirectory, "config.env"), "SSH_PORT=2223\n", 0o600)
	runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{
		Schema: 1, OperationID: "teardown-shared-test", Status: "ok",
	}}}}
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"teardown", "--yes"},
		Environment: append(environment, "SUBYARD_OPERATION_ID=teardown-shared-test"), WorkingDir: root,
		AdapterRunner: runner, Prompt: &testkit.Prompt{},
		Incus: &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true}},
		Clock: testkit.NewManualClock(time.Unix(100, 0)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("teardown failed with %d", code)
	}
	if len(runner.Requests) != 1 ||
		runner.Requests[0].Context["SUBYARD_TEARDOWN_KEEP_SHARED"] != "1" {
		t.Fatalf("shared Incus lifecycle was not preserved: %#v", runner.Requests)
	}
}

func testDefinition(name string) command.Definition {
	return command.Definition{
		Name: name, Effect: command.EffectMutate,
		Confirmation: command.ConfirmationDynamic, Remote: command.RemoteForward,
	}
}

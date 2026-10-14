package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/adapters/testvmsruntime"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestTestVMStatusFormatsPublicOutput(t *testing.T) {
	const payload = `{"status":"ok","pool":{"slots":[{"slot_id":"slot-001","resource_generation":9007199254740993,"state":"available"}]}}`
	for _, tc := range []struct {
		name    string
		json    bool
		failure bool
	}{
		{name: "formatted"},
		{name: "compact", json: true},
		{name: "diagnostic", failure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			environment = append(environment, "NESTED_E2E_VMS=1")
			incus := lifecycleIncus()
			instance := incus.Instances["subyard/yard"]
			instance.Status = "Running"
			incus.Instances["subyard/yard"] = instance
			output := payload
			ending := ""
			if tc.failure {
				output = "broker unavailable"
				ending = "exit 1\n"
			}
			path := filepath.Join(root, "scripts", "e2e-lab", "invoke.sh")
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			}
			writeCLIFile(t, path, "#!/bin/sh\n[ \"$*\" = status ] || exit 2\nprintf '%s\\n' '"+output+"'\n"+ending, 0755)
			arguments := []string{"test-vms", "status"}
			if tc.json {
				arguments = append(arguments, "--json")
			}
			var stdout, stderr bytes.Buffer
			program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: arguments,
				Environment: environment, WorkingDir: root, Incus: incus, Stdout: &stdout, Stderr: &stderr})
			if err != nil {
				t.Fatal(err)
			}
			code := program.Run(context.Background())
			if tc.failure {
				if code == 0 || !strings.Contains(stdout.String()+stderr.String(), output) {
					t.Fatalf("lost failure: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
				}
				return
			}
			if code != 0 || stderr.Len() != 0 {
				t.Fatalf("status: code=%d stderr=%q", code, stderr.String())
			}
			var compact bytes.Buffer
			if err := json.Compact(&compact, stdout.Bytes()); err != nil || compact.String() != payload {
				t.Fatalf("status data changed: %q (%v)", stdout.String(), err)
			}
			if tc.json && stdout.String() != payload+"\n" || !tc.json && !strings.Contains(stdout.String(), "\n  \"pool\": {\n    \"slots\": [") {
				t.Fatalf("wrong formatting: %q", stdout.String())
			}
		})
	}
}

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
	runner.Steps[0].Apply = func(domain.AdapterRequest) {
		probe.output = bytes.Replace(probe.output, []byte(`"state":"held"`), []byte(`"state":"available"`), 1)
	}
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

func TestTestVMRecoverVerifiesCompletedRecoveryIdentity(t *testing.T) {
	for _, tc := range []struct {
		name       string
		generation uint64
		mutate     func(*testvmsruntime.LeaseSlot)
		wantOK     bool
	}{
		{name: "completed recovery", wantOK: true},
		{name: "unchanged generation", mutate: func(slot *testvmsruntime.LeaseSlot) { slot.ResourceGeneration-- }},
		{name: "replacement generation", mutate: func(slot *testvmsruntime.LeaseSlot) { slot.ResourceGeneration++ }},
		{name: "replacement epoch", mutate: func(slot *testvmsruntime.LeaseSlot) { slot.LeaseEpoch++ }},
		{name: "lease still present", mutate: func(slot *testvmsruntime.LeaseSlot) { slot.LeaseID = "retained-lease" }},
		{name: "still quarantined", mutate: func(slot *testvmsruntime.LeaseSlot) { slot.State = testvmsruntime.SlotQuarantined }},
		{name: "still recovering", mutate: func(slot *testvmsruntime.LeaseSlot) { slot.State = testvmsruntime.SlotRecovering }},
		{name: "generation overflow", generation: ^uint64(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			environment = append(environment, "NESTED_E2E_VMS=1", "SUBYARD_OPERATION_ID=test-vms-recover")
			incus := lifecycleIncus()
			instance := incus.Instances["subyard/yard"]
			instance.Status = "Running"
			incus.Instances["subyard/yard"] = instance
			generation := tc.generation
			if generation == 0 {
				generation = 337
			}
			expected := testvmsruntime.LeaseIdentity{SlotID: "slot-002", ResourceGeneration: generation, LeaseEpoch: 330}
			pool := testvmsruntime.LeasePool{
				SchemaVersion: testvmsruntime.LeaseSchemaVersion, ResourceType: "agent-e2e", ResourceID: "test-vms",
				Slots: []testvmsruntime.LeaseSlot{
					{SlotID: "slot-001", ResourceGeneration: 1, State: testvmsruntime.SlotAvailable},
					{SlotID: expected.SlotID, ResourceGeneration: expected.ResourceGeneration, LeaseEpoch: expected.LeaseEpoch,
						State: testvmsruntime.SlotQuarantined, LeaseID: "quarantined-lease"},
				},
			}
			store := testvmsruntime.LeaseStore{Path: filepath.Join(root, "recovery-leases.json"), SlotCount: 2}
			payload, err := json.Marshal(pool)
			if err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, store.Path, payload, 0o600)
			probe := &testVMStatusProbe{}
			publish := func() {
				payload, err := json.Marshal(struct {
					SchemaVersion int                      `json:"schema_version"`
					Status        string                   `json:"status"`
					Pool          testvmsruntime.LeasePool `json:"pool"`
				}{SchemaVersion: 1, Status: "ok", Pool: pool})
				if err != nil {
					t.Fatal(err)
				}
				probe.output = payload
			}
			publish()
			runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{
				Schema: 1, OperationID: "test-vms-recover", Status: "ok",
			}}}}
			runner.Steps[0].Apply = func(domain.AdapterRequest) {
				if _, started, err := store.BeginExpectedRecovery(expected); err != nil || !started {
					t.Fatalf("begin recovery: started=%v error=%v", started, err)
				}
				finished, err := store.FinishRecovery(expected.SlotID, nil, "", "")
				if err != nil {
					t.Fatal(err)
				}
				if tc.mutate != nil {
					tc.mutate(&finished)
				}
				pool.Slots[1] = finished
				publish()
			}
			var stderr bytes.Buffer
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard", Arguments: []string{"test-vms", "recover", "--slot", "2"},
				Environment: environment, WorkingDir: root, Incus: incus, ProjectData: probe, AdapterRunner: runner,
				Prompt: &testkit.Prompt{Answers: []bool{true}}, Stderr: &stderr,
			})
			if err != nil {
				t.Fatal(err)
			}
			code := program.Run(context.Background())
			if tc.wantOK {
				if code != 0 {
					t.Fatalf("completed recovery returned %d: %s", code, stderr.String())
				}
			} else if code != 1 || !strings.Contains(stderr.String(), "test VM physical cleanup did not publish the approved available slot") {
				t.Fatalf("unproved recovery: code=%d stderr=%q", code, stderr.String())
			}
			if len(runner.Requests) != 1 || runner.Requests[0].Action != "recover" ||
				!slices.Equal(runner.Requests[0].Arguments, []string{
					"recover-slot-2", "--expect-resource-generation", strconv.FormatUint(expected.ResourceGeneration, 10),
					"--expect-lease-epoch", "330", "--yes",
				}) {
				t.Fatalf("approved target identity was not forwarded: %#v", runner.Requests)
			}
		})
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
	sshLog := writeStructuredOwnerFixture(t, fakeBin, "test-vms", []string{"revoke", "--slot", "2"}, []domain.OperationStep{{ID: "lease.revoke", Target: "inner/test-vms/slot-002", Observed: "held generation 7 epoch 3", Desired: "release generation 7 epoch 3", Decision: domain.StepApply, Verify: "native lease identity is revoked", Consequence: "revoke slot 2 generation 7 epoch 3"}}, "revoke slot 2 generation 7 epoch 3")

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
		"operation.plan", "stepSchema", "operation.execute", strings.Repeat("a", 64),
	} {
		if !strings.Contains(command, expected) {
			t.Fatalf("remote forwarding omitted %q:\n%s", expected, command)
		}
	}
	if len(prompt.Requests) != 1 || !strings.Contains(strings.Join(prompt.Requests[0].Consequences, " "), "generation 7 epoch 3") {
		t.Fatalf("confirmation lost owner lease identity: %#v", prompt.Requests)
	}
	if len(probe.requests) != 0 {
		t.Fatalf("controller observed owner-native lease state: %#v", probe.requests)
	}
	if strings.Count(command, "session ") != 1 || strings.Count(command, `"method": "operation.execute"`) != 1 {
		t.Fatalf("lease action did not use one exact owner session: %s", command)
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

func TestTestVMStatusInvocationBypassesMutationGate(t *testing.T) {
	for _, arguments := range [][]string{{"status"}, {"--yes", "status"}, {"-y", "status"}} {
		if !testVMStatusInvocation(arguments) {
			t.Errorf("test-vms %v was not classified as read-only", arguments)
		}
	}
	for _, arguments := range [][]string{nil, {"recover"}, {"--yes", "revoke"}} {
		if testVMStatusInvocation(arguments) {
			t.Errorf("test-vms %v was classified as read-only", arguments)
		}
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
	if len(probe.requests) != 2 || !slices.Equal(probe.requests[0].Command,
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
	probe := &testVMStatusProbe{output: []byte(`{"schema_version":1,"status":"ok","pool":{"schema_version":2,"resource_type":"agent-e2e","resource_id":"test-vms","slots":[{"slot_id":"slot-001","state":"available"},{"slot_id":"slot-002","state":"available"}]}}`)}
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
	probe := &testVMStatusProbe{output: []byte(`{"schema_version":1,"status":"ok","pool":{"schema_version":2,"resource_type":"agent-e2e","resource_id":"test-vms","slots":[{"slot_id":"slot-001","resource_generation":1,"state":"available"},{"slot_id":"slot-002","resource_generation":4,"lease_epoch":8,"state":"recovering"}]}}`)}
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
	afterPath   string
	afterOutput []byte
	requests    []ports.InstanceExecRequest
	output      []byte
	err         error
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
	if probe.afterPath != "" {
		if _, err := os.Stat(probe.afterPath); err == nil {
			return ports.InstanceExecResult{Stdout: probe.afterOutput}, nil
		}
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
	dispatcher := filepath.Join(root, ".build", "yard")
	runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{
		Schema: 1, OperationID: "teardown-test", Status: "ok",
	}}}}
	incus := lifecycleIncus()
	incus.Reconcile.InstanceFound = true
	runner.Steps[0].Apply = func(domain.AdapterRequest) {
		incus.Reconcile.InstanceFound = false
		incus.Instances = map[string]ports.InstanceInfo{}
	}
	prompt := &testkit.Prompt{Answers: []bool{true}}
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", DispatcherPath: dispatcher, Arguments: []string{"teardown", "--keep-data"},
		Environment: append(environment, "SUBYARD_OPERATION_ID=teardown-test", "SUBYARD_DISPATCHER_PATH=/ambient/engine"), WorkingDir: root,
		AdapterRunner: runner, Prompt: prompt, Clock: testkit.NewManualClock(time.Unix(100, 0)),
		Stderr: &stderr, Incus: incus,
		NetworkPolicy: allowTestNetworkPolicy(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("teardown failed: code=%d stderr=%q", code, stderr.String())
	}
	if len(runner.Requests) != 1 || runner.Requests[0].Adapter != "teardown" ||
		runner.Requests[0].Context["SUBYARD_DISPATCHER_PATH"] != dispatcher ||
		runner.Requests[0].Context["SUBYARD_TEARDOWN_KEEP_DATA"] != "1" ||
		runner.Requests[0].Context["SUBYARD_TEARDOWN_KEEP_SHARED"] != "0" {
		t.Fatalf("requests=%#v", runner.Requests)
	}
	var artifacts []ports.TeardownArtifact
	if err := json.Unmarshal([]byte(runner.Requests[0].Context["SUBYARD_TEARDOWN_ARTIFACTS"]), &artifacts); err != nil || len(artifacts) == 0 {
		t.Fatalf("physical teardown lost approved artifact scope: count=%d err=%v", len(artifacts), err)
	}
	if artifacts[1].Path != filepath.Join(root, "home", ".ssh", "subyard.config") || artifacts[2].Path != filepath.Join(root, "data", "space.cache") {
		t.Fatalf("default-yard artifacts differ from physical cleanup paths: %#v", artifacts)
	}
}

type recordedNativeIntegrationRuntime struct {
	reconcileruntime.Runtime
	unavailable *bool
}

func (runtime recordedNativeIntegrationRuntime) IntegrationPlan(ctx context.Context) (reconcileruntime.IntegrationPlan, error) {
	plan, err := runtime.Runtime.IntegrationPlan(ctx)
	*runtime.unavailable = err != nil
	return plan, err
}

func TestCompletedTeardownUsesFreshConsentAndProtectedHistory(t *testing.T) {
	for _, scenario := range []string{
		"purge", "keep-data", "decline", "reset-config", "corrupt history", "unfinished history", "foreign history",
		"unrelated target", "global current", "global drift", "unknown runtime", "temporary host storage", "persistent host storage",
		"source after consent", "history after consent", "runtime after consent", "integration after consent",
		"absent inventory", "stopped inventory", "paused inventory", "unknown inventory", "inventory after consent",
	} {
		t.Run(scenario, func(t *testing.T) {
			var assets map[string]string
			var powerEnvironment []string
			if scenario == "global drift" || scenario == "global current" {
				powerRoot := testkit.TempDir(t)
				unit, reconciler := filepath.Join(powerRoot, "unit"), filepath.Join(powerRoot, "reconciler")
				testkit.WriteFile(t, unit, []byte("installed power fixture\n"), 0o600)
				assets = map[string]string{"config/host.env": "SUBYARD_POWER_UNIT_PATH=" + unit + "\nSUBYARD_POWER_RECONCILER_PATH=" + reconciler + "\n"}
				powerEnvironment = []string{"SUBYARD_POWER_UNIT_PATH=" + unit, "SUBYARD_POWER_RECONCILER_PATH=" + reconciler}
			}
			program, _, home := completedSourceAuthoringFixture(t, false, assets)
			options := program.options
			// Exercise persisted source authority; the storage pair isolates a temporary host override.
			options.Environment = append(environmentList(program.freshMigrationEnvironment(program.baseEnv, options.RepositoryRoot), nil), powerEnvironment...)
			options.Arguments = []string{"teardown"}
			if scenario == "keep-data" {
				options.Arguments = append(options.Arguments, "--keep-data")
			} else if scenario == "reset-config" {
				options.Arguments = append(options.Arguments, "--reset-config")
			}
			hostSource := filepath.Join(home, "config.env")
			var hostSettings []byte
			if scenario == "temporary host storage" || scenario == "persistent host storage" {
				storage := filepath.Join(testkit.TempDir(t), "incus-storage")
				options.Environment = append(options.Environment, "STORAGE_PATH="+storage)
				if scenario == "persistent host storage" {
					hostSettings = []byte("STORAGE_PATH=" + storage + "\n")
					testkit.WriteFile(t, hostSource, hostSettings, 0o600)
				}
			}
			source := filepath.Join(home, "yards/default/config.env")
			writeProfileTestFile(t, source, []byte("# retain owner settings\n"), 0o600)
			before := mustReadConfigFixture(t, source)
			loaded, err := program.loadSelectionRepair("default")
			if err != nil {
				t.Fatal(err)
			}
			incus := options.Incus.(*testkit.Incus)
			incus.Reconcile.InstanceFound = true
			platform := &sampleRepairPlatformFixture{initPlatformFixture: newInitPlatformFixture()}
			for stage := range platform.converged {
				platform.converged[stage] = true
			}
			platform.converged[ports.ReconcileStageProfileRuntimes] = false
			if scenario == "global drift" {
				platform.converged[ports.ReconcileStagePower] = false
			} else if scenario == "unknown runtime" {
				platform.observation = &ports.RuntimeObservation{State: "unknown"}
			} else if scenario == "unrelated target" {
				writeProfileTestFile(t, filepath.Join(home, "yards/neighbor/config.env"), []byte("SSH_PORT=2223\n"), 0o600)
			}
			integrations := &integrationRuntimeFixture{plan: reconcileruntime.IntegrationPlan{
				Fingerprint: operationStateDigest("current integration fixture"), Scope: operationStateDigest("integration scope"),
			}}
			inventoryCase := strings.Contains(scenario, "inventory")
			var inventoryContext domain.Context
			var inventorySource string
			var inventorySettings []byte
			var nativeInventoryUnavailable bool
			if inventoryCase {
				inventorySource = filepath.Join(home, "yards/inventory/config.env")
				writeProfileTestFile(t, inventorySource, []byte("SSH_PORT=2223\n"), 0o600)
				inventorySettings = mustReadConfigFixture(t, inventorySource)
				inventory, err := program.resolveReleaseTransitionContext("inventory", home)
				if err != nil {
					t.Fatal(err)
				}
				inventoryContext = inventory.Context
				status := map[string]string{"stopped inventory": "Stopped", "paused inventory": "Paused", "unknown inventory": "unknown"}[scenario]
				if status != "" {
					incus.Instances[inventoryContext.IncusProject+"/"+inventoryContext.YardInstanceName] = ports.InstanceInfo{Status: status}
				}
				// Isolate integration drift on the selected target from the registered inventory target.
				platform.converged[ports.ReconcileStageProfileRuntimes] = true
				integrations.plan.Changed = true
			}
			owned := filepath.Join(loaded.Context.Paths.OperatorHome, ".ssh", "subyard.config")
			writeProfileTestFile(t, owned, []byte("owned yard SSH snippet\n"), 0o600)
			journalPath := filepath.Join(home, "release-transition/v2/journal.json")
			if scenario == "corrupt history" {
				testkit.WriteFile(t, journalPath, []byte("{invalid\n"), 0o600)
			} else if scenario == "unfinished history" || scenario == "foreign history" {
				journal, err := releasetransition.ParseJournal(mustReadConfigFixture(t, journalPath))
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "unfinished history" {
					journal.Checkpoint = releasetransition.JournalReconciling
				} else {
					journal.ArtifactDigest = releasetransition.Fingerprint(operationStateDigest("foreign release artifact"))
				}
				payload, err := releasetransition.MarshalJournal(journal)
				if err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, journalPath, payload, 0o600)
			}
			history := operationStateDigest(activationRepairHistory(t, options.RepositoryRoot, home))
			checkpointPath := filepath.Join(home, "release-transition/v2/history-checkpoint.json")
			checkpoint := mustReadConfigFixture(t, checkpointPath)
			runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{Schema: 1, Status: "ok"}, Apply: func(request domain.AdapterRequest) {
				if request.Adapter != "teardown" {
					t.Fatal("completed teardown dispatched another adapter")
				}
				incus.Reconcile.InstanceFound = false
				delete(incus.Instances, loaded.Context.IncusProject+"/"+loaded.Context.YardInstanceName)
				if err := os.Remove(owned); err != nil {
					t.Fatal(err)
				}
			}}}}
			prompt := &callbackPrompt{callback: func() {
				if len(runner.Requests) != 0 || !bytes.Equal(before, mustReadConfigFixture(t, source)) {
					t.Fatal("teardown changed state before consent")
				}
				switch scenario {
				case "source after consent":
					testkit.WriteFile(t, source, append(before, '#', '\n'), 0o600)
					before = mustReadConfigFixture(t, source)
				case "history after consent":
					testkit.WriteFile(t, journalPath, append(mustReadConfigFixture(t, journalPath), '\n'), 0o600)
					history = operationStateDigest(activationRepairHistory(t, options.RepositoryRoot, home))
				case "runtime after consent":
					platform.observation = &ports.RuntimeObservation{State: ports.RuntimeStateAbsent}
				case "integration after consent":
					integrations.plan.Fingerprint = operationStateDigest("replaced integration instance")
				case "inventory after consent":
					incus.Instances[inventoryContext.IncusProject+"/"+inventoryContext.YardInstanceName] = ports.InstanceInfo{Status: "Stopped"}
				}
			}}
			decline := &testkit.Prompt{Answers: []bool{false}}
			options.AdapterRunner, options.Prompt = runner, prompt
			if scenario == "decline" {
				options.Prompt = decline
			}
			options.InitPlatform, options.NetworkPolicy = platform, allowTestNetworkPolicy()
			options.IntegrationRuntime = func(loaded config.Loaded) IntegrationRuntime {
				if !inventoryCase || loaded.Context.YardName != "inventory" {
					return integrations
				}
				instance, err := incus.Instance(context.Background(), loaded.Context.IncusProject, loaded.Context.YardInstanceName)
				if err != nil && !errors.Is(err, ports.ErrInstanceNotFound) {
					t.Fatal(err)
				}
				// Use the actual native precondition, which refuses absent and stopped targets.
				return recordedNativeIntegrationRuntime{Runtime: reconcileruntime.Runtime{
					Yard: loaded.Context, Incus: &testkit.Incus{Reconcile: ports.ReconcileState{
						InstanceFound: err == nil, Instance: instance,
					}},
				}, unavailable: &nativeInventoryUnavailable}
			}
			var output bytes.Buffer
			options.Stdout, options.Stderr = &output, &output
			code := runAliasRepairFixture(t, options)
			approved := scenario == "purge" || scenario == "keep-data" || scenario == "global current" || scenario == "persistent host storage" ||
				scenario == "absent inventory" || scenario == "stopped inventory"
			if (code == 0) != approved {
				t.Fatalf("completed teardown: code=%d approved=%t native_inventory_plan_unavailable=%t output=%s", code, approved, nativeInventoryUnavailable, output.String())
			}
			prompts := len(prompt.requests) + len(decline.Requests)
			wantPrompt := approved || scenario == "decline" || strings.Contains(scenario, "after consent")
			if (prompts == 1) != wantPrompt || prompts > 1 || (len(runner.Requests) == 1) != approved {
				t.Fatalf("fresh teardown authority: prompts=%d native=%d output=%s", prompts, len(runner.Requests), output.String())
			}
			if approved {
				expectedDefault, keep := domain.ConfirmationDefaultNo, "0"
				if scenario == "keep-data" {
					expectedDefault, keep = domain.ConfirmationDefaultYes, "1"
				}
				if prompt.requests[0].Default != expectedDefault || runner.Requests[0].Context["SUBYARD_TEARDOWN_KEEP_DATA"] != keep {
					t.Fatal("teardown changed ordinary consent or retained-data policy")
				}
			} else if _, err := os.Stat(owned); err != nil || !incus.Reconcile.InstanceFound {
				t.Fatal("refused teardown removed owned state")
			}
			info, err := os.Stat(source)
			if err != nil || info.Mode().Perm() != 0o600 || !bytes.Equal(before, mustReadConfigFixture(t, source)) ||
				history != operationStateDigest(activationRepairHistory(t, options.RepositoryRoot, home)) ||
				!bytes.Equal(checkpoint, mustReadConfigFixture(t, checkpointPath)) {
				t.Fatal("teardown changed settings, protected release history or links")
			}
			if len(platform.applied) != 0 || platform.projectHooks != 0 || integrations.applied != 0 {
				t.Fatal("teardown repaired activation instead of removing its approved target")
			}
			if scenario == "persistent host storage" {
				info, err := os.Stat(hostSource)
				if err != nil || info.Mode().Perm() != 0o600 || !bytes.Equal(hostSettings, mustReadConfigFixture(t, hostSource)) {
					t.Fatal("teardown changed the persisted host storage source")
				}
			} else if scenario == "temporary host storage" {
				if _, err := os.Stat(hostSource); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("refused teardown persisted temporary host storage")
				}
			}
			if inventoryCase {
				info, err := os.Stat(inventorySource)
				if err != nil || info.Mode().Perm() != 0o600 || !bytes.Equal(inventorySettings, mustReadConfigFixture(t, inventorySource)) {
					t.Fatal("teardown changed the unselected inventory declaration")
				}
				if scenario == "stopped inventory" && incus.Instances[inventoryContext.IncusProject+"/"+inventoryContext.YardInstanceName].Status != "Stopped" {
					t.Fatal("teardown removed the unselected stopped target")
				}
			}
		})
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

func TestLifecycleStartFailsClosedWithoutNetworkPolicyAdapter(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	incus := lifecycleIncus()
	runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{
		Schema: 1, OperationID: "start-without-network-policy", Status: "ok",
	}}}}
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
		Incus: incus, AdapterRunner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	execution := &lifecycleExecution{action: "start"}
	yard := domain.Context{YardName: "default", IncusProject: "subyard", YardInstanceName: "yard", IncusBridge: "incusbr0"}
	if err := program.observeLifecycleExecution(context.Background(), yard, execution); err != nil {
		t.Fatal(err)
	}
	result, err := program.executeLifecycle(
		context.Background(),
		program.operationOrchestrator("start-without-network-policy", config.Loaded{Context: domain.Context{
			YardName: "default", IncusProject: "subyard", YardInstanceName: "yard", IncusBridge: "incusbr0",
		}}, nil, nil),
		domain.Context{YardName: "default", IncusProject: "subyard", YardInstanceName: "yard", IncusBridge: "incusbr0"},
		domain.OperationPlan{OperationID: "start-without-network-policy", Confirmed: true},
		execution, io.Discard,
	)
	if err == nil || !strings.Contains(err.Error(), "network policy adapter is unavailable") ||
		result.Status != "" || len(runner.Requests) != 0 {
		t.Fatalf("start result=%#v requests=%#v err=%v", result, runner.Requests, err)
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
				WorkingDir: root, Incus: incus, NetworkPolicy: allowTestNetworkPolicy(),
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
	incus := lifecycleIncus()
	incus.Reconcile.InstanceFound = true
	runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{
		Schema: 1, OperationID: "teardown-shared-test", Status: "ok",
	}}}}
	runner.Steps[0].Apply = func(domain.AdapterRequest) {
		incus.Reconcile.InstanceFound = false
		incus.Instances = map[string]ports.InstanceInfo{}
	}
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"teardown", "--yes"},
		Environment: append(environment, "SUBYARD_OPERATION_ID=teardown-shared-test"), WorkingDir: root,
		AdapterRunner: runner, Prompt: &testkit.Prompt{},
		Incus: incus,
		Clock: testkit.NewManualClock(time.Unix(100, 0)), NetworkPolicy: allowTestNetworkPolicy(),
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

func TestLegacyVMRetirementRequiresDefaultNoAndExactIdentity(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	environment = append(environment, "NESTED_E2E_VMS=1", "SUBYARD_OPERATION_ID=retire-legacy")
	incus := lifecycleIncus()
	instance := incus.Instances["subyard/yard"]
	instance.Status = "Running"
	incus.Instances["subyard/yard"] = instance
	probe := &testVMStatusProbe{output: []byte(`{"schema_version":1,"status":"ok","pool":{"schema_version":2,"resource_type":"agent-e2e","resource_id":"test-vms","slots":[{"slot_id":"slot-001","resource_generation":7,"lease_epoch":0,"state":"available","legacy_retained":true}]}}`)}
	probe.afterPath = filepath.Join(root, "retirement-arguments")
	probe.afterOutput = bytes.Replace(probe.output, []byte(`"legacy_retained":true`), []byte(`"legacy_retained":false`), 1)
	prompt := &testkit.Prompt{Answers: []bool{true}}
	if err := os.MkdirAll(filepath.Join(root, "scripts/e2e-lab"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(root, "scripts/e2e-lab/invoke.sh"), "#!/bin/sh\nprintf '%s\\n' \"$@\" > retirement-arguments\n", 0o700)
	var stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"test-vms", "retire-legacy", "--slot", "1"},
		Environment: environment, WorkingDir: root, Incus: incus, ProjectData: probe, Prompt: prompt, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("retirement returned %d: %s", code, stderr.String())
	}
	if len(prompt.Requests) != 1 || prompt.Requests[0].Default != domain.ConfirmationDefaultNo {
		t.Fatalf("retirement prompt: %#v", prompt.Requests)
	}
	arguments, err := os.ReadFile(filepath.Join(root, "retirement-arguments"))
	if err != nil || string(arguments) != "retire-legacy-slot-1\n--expect-resource-generation\n7\n--expect-lease-epoch\n0\n--yes\n" {
		t.Fatalf("retirement arguments: %q, error: %v", arguments, err)
	}
}

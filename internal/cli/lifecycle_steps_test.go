package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestStopCapturedNoOpRejectsNewWork(t *testing.T) {
	program, loaded, _, _ := credentialCLIFixture(t, strings.NewReader(""), &testkit.Prompt{}, false)
	incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName: {Name: loaded.Context.YardInstanceName, Project: loaded.Context.IncusProject, Status: "Stopped"}}}
	program.options.Incus = incus
	execution, err := prepareLifecycleExecution(command.Definition{Name: "stop", Handler: "@lifecycle"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := program.observeLifecycleExecution(context.Background(), loaded.Context, execution); err != nil {
		t.Fatal(err)
	}
	if execution.changed || execution.steps(loaded.Context)[0].Decision != domain.StepSkip {
		t.Fatal("stopped instance did not project skip")
	}
	instance := incus.Instances[loaded.Context.IncusProject+"/"+loaded.Context.YardInstanceName]
	instance.Status = "Running"
	incus.Instances[loaded.Context.IncusProject+"/"+loaded.Context.YardInstanceName] = instance
	if err := program.observeLifecycleExecution(context.Background(), loaded.Context, execution); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("stop expanded from skip: %v", err)
	}
}

func TestTeardownSnapshotRejectsNewResourceAndArtifactChanges(t *testing.T) {
	resource := ports.TeardownResource{Kind: "instance", Name: "approved", Binding: strings.Repeat("a", 64)}
	approved := &teardownSnapshot{Resources: []ports.TeardownResource{resource}, Artifacts: []teardownArtifact{{"owned", "before"}}}
	converged := &teardownSnapshot{Artifacts: []teardownArtifact{{"owned", ""}}}
	if err := checkTeardownSnapshot(approved, converged); err != nil {
		t.Fatal(err)
	}
	newTarget := &teardownSnapshot{Resources: []ports.TeardownResource{resource, {Kind: "instance", Name: "new", Binding: strings.Repeat("b", 64)}}, Artifacts: approved.Artifacts}
	if err := checkTeardownSnapshot(approved, newTarget); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("new resource accepted: %v", err)
	}
	altered := &teardownSnapshot{Resources: approved.Resources, Artifacts: []teardownArtifact{{"owned", "after"}}}
	if err := checkTeardownSnapshot(approved, altered); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("artifact drift accepted: %v", err)
	}
}

func TestTestVMNoOpRejectsNewAllocationBeforeWorker(t *testing.T) {
	loaded := config.Loaded{Context: domain.Context{NestedE2EVMs: true, AccessKind: domain.AccessLocal, YardKind: domain.YardContainer, IncusProject: "subyard", YardInstanceName: "yard"}}
	available := `{"schema_version":1,"status":"ok","pool":{"schema_version":2,"resource_type":"agent-e2e","resource_id":"test-vms","slots":[{"slot_id":"slot-001","resource_generation":7,"lease_epoch":0,"state":"available"}]}}`
	probe := &testVMStatusProbe{output: []byte(available)}
	program := &CLI{options: Options{Incus: &testkit.Incus{Instances: map[string]ports.InstanceInfo{"subyard/yard": {Name: "yard", Project: "subyard", Status: "Running"}}}, ProjectData: probe}}
	execution, err := program.prepareTestVMExecution(context.Background(), loaded, []string{"revoke", "--slot", "1"})
	if err != nil {
		t.Fatal(err)
	}
	if !execution.noOp || execution.steps()[0].Decision != domain.StepSkip {
		t.Fatal("available slot did not project skip")
	}
	probe.output = []byte(strings.Replace(strings.Replace(available, `"state":"available"`, `"state":"held"`, 1), `"lease_epoch":0`, `"lease_epoch":1`, 1))
	_, err = program.executeTestVMs(context.Background(), nil, loaded, domain.OperationPlan{OperationID: "no-op-slot"}, execution, io.Discard)
	if !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("new allocation accepted by no-op: %v", err)
	}
}

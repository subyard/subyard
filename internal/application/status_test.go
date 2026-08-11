package application

import (
	"context"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

type statusFacts struct{}

func (statusFacts) ReadStatusFacts(
	context.Context,
	domain.Context,
	bool,
) (domain.StatusFacts, error) {
	return domain.StatusFacts{}, nil
}

type blockingStatusExecutor struct{ calls int }

func (executor *blockingStatusExecutor) Exec(
	ctx context.Context,
	_, _ string,
	_ ports.InstanceExecRequest,
) (ports.InstanceExecResult, error) {
	executor.calls++
	<-ctx.Done()
	return ports.InstanceExecResult{}, ctx.Err()
}

func TestStatusCombinesServiceAndVSCodeProbe(t *testing.T) {
	yard := domain.Context{
		IncusProject: "subyard", YardInstanceName: "yard", DevUser: "dev",
	}
	incus := &testkit.Incus{
		Instances: map[string]ports.InstanceInfo{"subyard/yard": {
			Status: "Running", Config: map[string]string{}, Devices: map[string]map[string]string{},
		}},
		ExecSteps: []testkit.IncusExecStep{
			{Result: ports.InstanceExecResult{Stdout: []byte("10.0.0.2\n")}},
			{Result: ports.InstanceExecResult{
				Stdout: []byte("services=active/inactive\nvscode=key=yes server=yes git-id=no\n"),
			}},
		},
	}
	status, err := (StatusService{
		Incus: incus, Executor: incus, Store: testkit.NewMemoryState(), Facts: statusFacts{},
	}).Read(context.Background(), yard)
	if err != nil {
		t.Fatal(err)
	}
	if status.IP != "10.0.0.2" || status.Services != "active/inactive" ||
		status.VSCode != "key=yes server=yes git-id=no" || len(incus.ExecCalls) != 2 {
		t.Fatalf("combined optional probes changed: status=%#v calls=%#v", status, incus.ExecCalls)
	}
}

func TestStatusBoundsEachOptionalProbe(t *testing.T) {
	yard := domain.Context{IncusProject: "subyard", YardInstanceName: "yard"}
	incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{"subyard/yard": {
		Status: "Running", Config: map[string]string{}, Devices: map[string]map[string]string{},
	}}}
	executor := &blockingStatusExecutor{}
	started := time.Now()
	status, err := (StatusService{
		Incus: incus, Executor: executor, Store: testkit.NewMemoryState(), Facts: statusFacts{},
		ProbeTimeout: 20 * time.Millisecond,
	}).Read(context.Background(), yard)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("optional probes exceeded their deadlines: %s", elapsed)
	}
	if executor.calls != 2 || status.IP != "?" || status.Services != "?" || status.VSCode != "?" {
		t.Fatalf("optional timeout did not degrade fields: status=%#v calls=%d", status, executor.calls)
	}
}

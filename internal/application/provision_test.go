package application

import (
	"context"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

type provisionFixture struct {
	instance  ports.InstanceInfo
	updates   []map[string]string
	actions   []string
	arguments [][]string
	profiles  []string
	checks    []string
	checkRuns map[string][]string
	fail      string
}

func (fixture *provisionFixture) Instance(context.Context, string, string) (ports.InstanceInfo, error) {
	return fixture.instance, nil
}

func (fixture *provisionFixture) Server(context.Context) (ports.ServerInfo, error) {
	return ports.ServerInfo{}, nil
}

func (fixture *provisionFixture) ReconcileState(context.Context, string, string, string, string, string) (ports.ReconcileState, error) {
	return ports.ReconcileState{}, nil
}

func (fixture *provisionFixture) Events(context.Context, []string) (<-chan domain.OperationEvent, <-chan error) {
	return nil, nil
}

func (fixture *provisionFixture) SetInstanceConfig(_ context.Context, _, _ string, values map[string]string) error {
	fixture.updates = append(fixture.updates, values)
	return nil
}

func (fixture *provisionFixture) Run(_ context.Context, request domain.AdapterRequest, _ io.Reader) (domain.AdapterResult, string, error) {
	if request.Action == "profile-check" {
		name := request.Arguments[len(request.Arguments)-1]
		fixture.checks = append(fixture.checks, name)
		if fixture.checkRuns == nil {
			state := "changed"
			if slices.Contains(fixture.profiles, name) {
				state = "converged"
			}
			return domain.AdapterResult{Schema: 1, OperationID: request.OperationID, Status: "ok"}, state + "\n", nil
		}
		states := fixture.checkRuns[name]
		if len(states) == 0 {
			return domain.AdapterResult{}, "", errors.New("check fixture exhausted")
		}
		fixture.checkRuns[name] = states[1:]
		return domain.AdapterResult{Schema: 1, OperationID: request.OperationID, Status: "ok"}, states[0] + "\n", nil
	}
	if request.Action == "profile" {
		name := request.Arguments[0]
		fixture.profiles = append(fixture.profiles, name)
		if name == fixture.fail {
			return domain.AdapterResult{}, "", errors.New("hook failed")
		}
		return domain.AdapterResult{Schema: 1, OperationID: request.OperationID, Status: "ok"}, "", nil
	}
	fixture.actions = append(fixture.actions, request.Action)
	fixture.arguments = append(fixture.arguments, append([]string(nil), request.Arguments...))
	if request.Action == "start" {
		fixture.instance.Status = "Running"
	}
	if request.Action == "stop" {
		fixture.instance.Status = "Stopped"
	}
	return domain.AdapterResult{Schema: 1, OperationID: request.OperationID, Status: "ok"}, "", nil
}

func TestProvisionAppliesOnlyChangedProfilesAndVerifiesThem(t *testing.T) {
	fixture := &provisionFixture{
		instance: managedProvisionInstance("Running", PowerRunning),
		checkRuns: map[string][]string{
			"android":  {"converged"},
			"openclaw": {"changed", "converged"},
		},
	}
	runner := provisionRunnerFixture(fixture, "android", "openclaw")
	if _, _, err := runner.Run(context.Background(), provisionRequest(), nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fixture.profiles, []string{"openclaw"}) ||
		!slices.Equal(fixture.checks, []string{"android", "openclaw", "openclaw"}) {
		t.Fatalf("profiles=%v checks=%v", fixture.profiles, fixture.checks)
	}
}

func TestProvisionRestoresTemporarilyStartedYard(t *testing.T) {
	fixture := &provisionFixture{instance: managedProvisionInstance("Stopped", PowerStopped)}
	runner := provisionRunnerFixture(fixture, "android", "openclaw")
	result, _, err := runner.Run(context.Background(), provisionRequest(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "ok" || !slices.Equal(fixture.actions, []string{"start", "stop"}) {
		t.Fatalf("result=%+v actions=%v", result, fixture.actions)
	}
	if !slices.Equal(fixture.arguments[0], []string{"start", "--reconcile"}) ||
		!slices.Equal(fixture.arguments[1], []string{"stop", "--reconcile"}) {
		t.Fatalf("lifecycle arguments=%v", fixture.arguments)
	}
	if !slices.Equal(fixture.profiles, []string{"android", "openclaw"}) {
		t.Fatalf("profiles=%v", fixture.profiles)
	}
}

func TestProvisionRestoresPowerAfterHookFailure(t *testing.T) {
	fixture := &provisionFixture{instance: managedProvisionInstance("Stopped", PowerStopped), fail: "openclaw"}
	runner := provisionRunnerFixture(fixture, "android", "openclaw")
	if _, _, err := runner.Run(context.Background(), provisionRequest(), nil); err == nil {
		t.Fatal("expected hook failure")
	}
	if !slices.Equal(fixture.actions, []string{"start", "stop"}) {
		t.Fatalf("actions=%v", fixture.actions)
	}
}

func TestProvisionKeepsDesiredRunningYardStarted(t *testing.T) {
	fixture := &provisionFixture{instance: managedProvisionInstance("Stopped", PowerRunning)}
	runner := provisionRunnerFixture(fixture, "subyard-dev")
	if _, _, err := runner.Run(context.Background(), provisionRequest(), nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fixture.actions, []string{"start"}) {
		t.Fatalf("actions=%v", fixture.actions)
	}
}

func provisionRunnerFixture(fixture *provisionFixture, names ...string) ProvisionRunner {
	yard := domain.Context{YardName: "test", IncusProject: "subyard-test", YardInstanceName: "yard-test"}
	return ProvisionRunner{
		Power: PowerService{Instances: fixture, Config: fixture}, Physical: fixture,
		Yard: yard, Profiles: names,
	}
}

func managedProvisionInstance(status, desired string) ports.InstanceInfo {
	return ports.InstanceInfo{Status: status, Config: map[string]string{
		"user.subyard.managed": "true", "user.subyard.desired_power": desired,
		"user.subyard.initialized": "true", "user.subyard.name": "test",
		"user.subyard.bridge": "incusbr0", "boot.autostart": "false",
	}}
}

func provisionRequest() domain.AdapterRequest {
	return domain.AdapterRequest{Schema: 1, OperationID: "provision-test", Adapter: "provision", Action: "apply"}
}

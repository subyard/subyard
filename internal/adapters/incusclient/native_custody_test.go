package incusclient

import (
	"context"
	"errors"
	"github.com/Subyard/Subyard/internal/ports"
	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
	"math"
	"strings"
	"testing"
)

type custodyServer struct{ incus.InstanceServer }

func (custodyServer) GetConnectionInfo() (*incus.ConnectionInfo, error) {
	return &incus.ConnectionInfo{Protocol: "unix", SocketPath: "/fixture/incus.socket"}, nil
}

type custodyOperation struct {
	incus.Operation
	value            api.Operation
	refreshes, waits int
	refreshErr       error
	waitChange       func(*api.Operation)
}

func (o *custodyOperation) Get() api.Operation { return o.value }
func (o *custodyOperation) Wait() error {
	o.waits++
	if o.waitChange != nil {
		o.waitChange(&o.value)
	}
	return nil
}
func (o *custodyOperation) WaitContext(context.Context) error { return o.Wait() }
func (o *custodyOperation) Refresh() error {
	o.refreshes++
	if o.refreshErr != nil {
		return o.refreshErr
	}
	o.value.StatusCode = api.Success
	return nil
}
func (o *custodyOperation) Cancel() error { return nil }

type custodyObserver struct {
	accepted, terminal []ports.NativeOperation
	uncertain          bool
}

func (o *custodyObserver) Uncertain()                                    { o.uncertain = true }
func (o *custodyObserver) Before(ports.NativeOperation) error            { return nil }
func (o *custodyObserver) CommandStarted(string, string) (uint64, error) { return 1, nil }
func (o *custodyObserver) CommandTerminal(uint64) error                  { return nil }

func (o *custodyObserver) Accepted(v ports.NativeOperation) error {
	o.accepted = append(o.accepted, v)
	return nil
}
func (o *custodyObserver) Terminal(v ports.NativeOperation) error {
	o.terminal = append(o.terminal, v)
	return nil
}
func TestNativeCustodyRequiresExplicitFinalStatusAfterNilWait(t *testing.T) {
	observer := &custodyObserver{}
	operation := &custodyOperation{value: api.Operation{ID: "operation-1", StatusCode: api.Running, Resources: map[string][]string{"instances": {"/1.0/instances/yard"}}}}
	ctx := ports.WithNativeOperationObserver(context.Background(), observer)
	if err := waitNative(ctx, custodyServer{}, operation, "instance-state", "project", "yard"); err != nil {
		t.Fatal(err)
	}
	if operation.refreshes != 1 || operation.waits != 1 || len(observer.accepted) != 1 || len(observer.terminal) != 1 {
		t.Fatalf("missing accepted/final witness: %#v %#v", operation, observer)
	}
	before, after := observer.accepted[0], observer.terminal[0]
	if before.ID != "operation-1" || before.Project != "project" || before.Resource != "yard" || before.Status != 0 || after.Status != int(api.Success) || before.Server == "" {
		t.Fatalf("unbound operation witness: %#v %#v", before, after)
	}
}
func TestNativeCustodyRefreshFailureDoesNotInventTerminal(t *testing.T) {
	observer := &custodyObserver{}
	operation := &custodyOperation{value: api.Operation{ID: "operation-1", StatusCode: api.Running, Resources: map[string][]string{"instances": {"/1.0/instances/yard"}}}, refreshErr: errors.New("lost exact operation")}
	if err := waitNative(ports.WithNativeOperationObserver(context.Background(), observer), custodyServer{}, operation, "instance-state", "project", "yard"); err == nil {
		t.Fatal("lost operation accepted as terminal")
	}
	if len(observer.accepted) != 1 || len(observer.terminal) != 0 {
		t.Fatalf("unproved terminal witness: %#v", observer)
	}
}
func TestOrdinaryWaitKeepsExistingBehaviorWithoutCustodyObserver(t *testing.T) {
	operation := &custodyOperation{value: api.Operation{ID: "operation-1", StatusCode: api.Running}}
	if err := waitNative(context.Background(), custodyServer{}, operation, "instance-state", "project", "yard"); err != nil {
		t.Fatal(err)
	}
	if operation.waits != 1 || operation.refreshes != 0 {
		t.Fatalf("ordinary caller changed: %#v", operation)
	}
}

func TestNativeCustodyValidatesAlreadyFinalIdentity(t *testing.T) {
	for _, changed := range []string{"id", "resource", "project"} {
		t.Run(changed, func(t *testing.T) {
			observer := &custodyObserver{}
			operation := &custodyOperation{value: api.Operation{ID: "operation-1", StatusCode: api.Success, Resources: map[string][]string{"instances": {"/1.0/instances/yard"}}}}
			operation.waitChange = func(value *api.Operation) {
				switch changed {
				case "id":
					value.ID = "operation-2"
				case "resource":
					value.Resources["instances"][0] = "/1.0/instances/other"
				case "project":
					value.Resources["instances"][0] = "/1.0/instances/yard?project=other"
				}
			}
			if err := waitNative(ports.WithNativeOperationObserver(context.Background(), observer), custodyServer{}, operation, "instance-state", "project", "yard"); err == nil {
				t.Fatal("changed final identity accepted")
			}
			if len(observer.accepted) != 1 || len(observer.terminal) != 0 || !observer.uncertain {
				t.Fatalf("changed identity did not poison invocation: %#v", observer)
			}
		})
	}
}

func TestNativeCustodyRecordsKnownFailureWithoutCallingItSuccess(t *testing.T) {
	for _, status := range []api.StatusCode{api.Failure, api.Cancelled} {
		observer := &custodyObserver{}
		operation := &custodyOperation{value: api.Operation{ID: "operation-1", StatusCode: status, Resources: map[string][]string{"instances": {"/1.0/instances/yard"}}}}
		err := waitNative(ports.WithNativeOperationObserver(context.Background(), observer), custodyServer{}, operation, "instance-state", "project", "yard")
		var failure *ports.NativeOperationFailure
		if !errors.As(err, &failure) || len(observer.accepted) != 1 || len(observer.terminal) != 1 || observer.terminal[0].Status != int(status) || observer.uncertain {
			t.Fatalf("final failure was not distinguished from unknown/success: %v %#v", err, observer)
		}
	}
}

func TestNativeCustodyMissingUUIDStaysUnknown(t *testing.T) {
	observer := &custodyObserver{}
	operation := &custodyOperation{value: api.Operation{StatusCode: api.Success, Resources: map[string][]string{"instances": {"/1.0/instances/yard"}}}}
	err := waitNative(ports.WithNativeOperationObserver(context.Background(), observer), custodyServer{}, operation, "instance-state", "project", "yard")
	var failure *ports.NativeOperationFailure
	if err == nil || errors.As(err, &failure) || len(observer.accepted) != 0 || len(observer.terminal) != 0 || !observer.uncertain {
		t.Fatalf("unbound request acquired a terminal proof: %v %#v", err, observer)
	}
}

func TestNativeExecCustodyRequiresExitBeforeTerminal(t *testing.T) {
	for _, value := range []any{nil, "0", 0.5, -1, 256} {
		observer := &custodyObserver{}
		operation := &custodyOperation{value: api.Operation{ID: "operation-1", StatusCode: api.Success, Resources: map[string][]string{"instances": {"/1.0/instances/yard"}}, Metadata: map[string]any{"return": value}}}
		err := waitNative(ports.WithNativeOperationObserver(context.Background(), observer), custodyServer{}, operation, "instance-exec", "project", "yard")
		if err == nil || len(observer.terminal) != 0 || !observer.uncertain {
			t.Fatalf("exit metadata %v sealed terminal: %v %#v", value, err, observer)
		}
	}
	for _, value := range []any{float64(0), float64(7)} {
		observer := &custodyObserver{}
		operation := &custodyOperation{value: api.Operation{ID: "operation-1", StatusCode: api.Success, Resources: map[string][]string{"instances": {"/1.0/instances/yard"}}, Metadata: map[string]any{"return": value}}}
		if err := waitNative(ports.WithNativeOperationObserver(context.Background(), observer), custodyServer{}, operation, "instance-exec", "project", "yard"); err != nil || len(observer.terminal) != 1 || observer.terminal[0].Status != 200 {
			t.Fatalf("valid remote exec exit changed status: %v %#v", err, observer)
		}
	}
}

func TestCompletedNativeExecRechecksExactDrainedResult(t *testing.T) {
	for _, change := range []string{"none", "id", "project", "resource", "server", "status", "terminal-failure", "missing", "string", "fractional", "overflow", "changed-exit", "zero", "canceled"} {
		t.Run(change, func(t *testing.T) {
			observer := &custodyObserver{}
			ctx, cancel := context.WithCancel(ports.WithNativeSynchronousConfigCAS(ports.WithNativeOperationObserver(context.Background(), observer)))
			defer cancel()
			operation := &custodyOperation{value: api.Operation{ID: "operation-1", StatusCode: api.Success, Resources: map[string][]string{"instances": {"/1.0/instances/yard?project=project"}}, Metadata: map[string]any{"return": float64(7)}}}
			var checked nativeWaitResult
			if err := waitNativeChecked(ctx, custodyServer{}, operation, "instance-exec", "project", "yard", &checked); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "id":
				operation.value.ID = "operation-2"
			case "project":
				operation.value.Resources["instances"][0] = "/1.0/instances/yard?project=other"
			case "resource":
				operation.value.Resources["instances"][0] = "/1.0/instances/other?project=project"
			case "server":
				checked.operation.Server = strings.Repeat("a", 64)
			case "status":
				operation.value.StatusCode = api.Running
			case "terminal-failure":
				operation.value.StatusCode = api.Failure
			case "missing":
				delete(operation.value.Metadata, "return")
			case "string":
				operation.value.Metadata["return"] = "7"
			case "fractional":
				operation.value.Metadata["return"] = 7.5
			case "overflow":
				operation.value.Metadata["return"] = math.MaxFloat64
			case "changed-exit":
				operation.value.Metadata["return"] = float64(8)
			case "zero":
				operation.value.Metadata["return"] = float64(0)
			case "canceled":
				cancel()
			}
			code, err := completedNativeExec(ctx, custodyServer{}, operation, checked)
			var failure *ports.NativeOperationFailure
			if change == "none" {
				if !errors.As(err, &failure) || code != 7 || failure.ExecExitCode == nil || *failure.ExecExitCode != 7 || failure.Operation != observer.terminal[0] || observer.uncertain {
					t.Fatal("exact completed failure lost", err)
				}
			} else if err == nil || errors.As(err, &failure) || !observer.uncertain {
				t.Fatal("changed SDK result acquired failure custody", change, err)
			}
		})
	}
}

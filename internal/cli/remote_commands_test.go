package cli

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/testkit"
)

type remoteControlStub struct {
	applied int
	records []domain.RemoteRecord
}

func (control *remoteControlStub) Lookup(context.Context, string) (domain.RemoteRecord, bool, error) {
	return domain.RemoteRecord{}, false, nil
}
func (control *remoteControlStub) List(context.Context) ([]domain.RemoteRecord, error) {
	return control.records, nil
}
func (control *remoteControlStub) ProbeOwner(context.Context, domain.RemoteSpec) (domain.RemoteInfo, error) {
	return domain.RemoteInfo{State: "RUNNING", SSHPort: 2222, DevUser: "dev"}, nil
}
func (control *remoteControlStub) ScanYardKeys(context.Context, domain.RemoteSpec, int) ([]domain.RemoteKey, error) {
	return []domain.RemoteKey{{Material: "ssh-ed25519 fixture", Fingerprint: "SHA256:new"}}, nil
}
func (control *remoteControlStub) RecordedYardKeys(context.Context, string) ([]domain.RemoteKey, error) {
	return nil, nil
}
func (control *remoteControlStub) Apply(_ context.Context, prepared domain.RemotePrepared) (domain.RemoteResult, error) {
	control.applied++
	return domain.RemoteResult{Message: string(prepared.Action)}, nil
}

func TestRPCRemotePlanContainsPreparedFingerprintBeforeApply(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	control := &remoteControlStub{}
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, RemoteControl: control,
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]rpcPlannedOperation)}
	params, _ := json.Marshal(map[string]any{
		"command": "remote", "arguments": []string{"add", "demo", "owner"},
	})
	result, err := handler.Handle(context.Background(), rpc.Call{
		Method: "operation.plan", OperationID: "operation-remote", Params: params,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan := result.(domain.OperationPlan)
	if control.applied != 0 || !strings.Contains(strings.Join(plan.Consequences, " "), "SHA256:new") {
		t.Fatalf("remote prepare mutated state or omitted fingerprint: %#v", plan)
	}
	execute, _ := json.Marshal(map[string]bool{"confirmed": true})
	if _, err := handler.Handle(context.Background(), rpc.Call{
		Method: "operation.execute", OperationID: plan.OperationID, Params: execute,
	}, func(string, any) (uint64, error) { return 1, nil }); err != nil {
		t.Fatal(err)
	}
	if control.applied != 1 {
		t.Fatalf("confirmed plan applied %d times", control.applied)
	}
}

func TestDirectAndRPCRemoteAddUseEquivalentOwnerActionPlans(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	directControl := &remoteControlStub{}
	directPrompt := &testkit.Prompt{Answers: []bool{true}}
	direct, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"remote", "add", "demo", "owner"},
		Environment: append(environment, "SUBYARD_OPERATION_ID=operation-remote-direct"), RemoteControl: directControl,
		Prompt: directPrompt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := direct.Run(context.Background()); code != 0 || directControl.applied != 1 || len(directPrompt.Requests) != 1 {
		t.Fatalf("direct code=%d applies=%d requests=%#v", code, directControl.applied, directPrompt.Requests)
	}

	rpcControl := &remoteControlStub{}
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, RemoteControl: rpcControl,
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]rpcPlannedOperation)}
	params, _ := json.Marshal(map[string]any{
		"command": "remote", "arguments": []string{"add", "demo", "owner"},
	})
	result, err := handler.Handle(context.Background(), rpc.Call{
		Method: "operation.plan", OperationID: "operation-remote-rpc", Params: params,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan := result.(domain.OperationPlan)
	if rpcControl.applied != 0 || plan.Assessment == nil || plan.Assessment.Action != "remote.add" ||
		plan.Confirmation != domain.ConfirmationPromptDefaultYes || plan.ConfirmationRequest == nil ||
		!reflect.DeepEqual(*plan.ConfirmationRequest, directPrompt.Requests[0]) {
		t.Fatalf("direct request=%#v rpc plan=%#v applies=%d", directPrompt.Requests[0], plan, rpcControl.applied)
	}
	execute, _ := json.Marshal(map[string]bool{"confirmed": true})
	if _, err := handler.Handle(context.Background(), rpc.Call{
		Method: "operation.execute", OperationID: plan.OperationID, Params: execute,
	}, func(string, any) (uint64, error) { return 1, nil }); err != nil || rpcControl.applied != 1 {
		t.Fatalf("execute err=%v applies=%d", err, rpcControl.applied)
	}
}

func TestRPCOperationPlanMapsActionPolicyInvalid(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, RemoteControl: &remoteControlStub{},
	})
	if err != nil {
		t.Fatal(err)
	}
	program.coreActions, err = domain.NewActionRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]rpcPlannedOperation)}
	params, _ := json.Marshal(map[string]any{
		"command": "remote", "arguments": []string{"add", "demo", "owner"},
	})
	_, err = handler.Handle(context.Background(), rpc.Call{
		Method: "operation.plan", OperationID: "operation-policy-plan", Params: params,
	}, nil)
	rpcErr, ok := err.(*rpc.Error)
	if !ok || rpcErr.Code != domain.ActionPolicyInvalid {
		t.Fatalf("operation.plan err=%#v", err)
	}
}

func TestRPCOperationExecuteMapsActionPolicyInvalid(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	control := &remoteControlStub{}
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, RemoteControl: control,
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]rpcPlannedOperation)}
	params, _ := json.Marshal(map[string]any{
		"command": "remote", "arguments": []string{"add", "demo", "owner"},
	})
	result, err := handler.Handle(context.Background(), rpc.Call{
		Method: "operation.plan", OperationID: "operation-policy-execute", Params: params,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan := result.(domain.OperationPlan)
	handler.plansMu.Lock()
	planned := handler.plans[plan.OperationID]
	planned.Plan.Confirmed = true
	handler.plans[plan.OperationID] = planned
	handler.plansMu.Unlock()
	execute, _ := json.Marshal(map[string]bool{"confirmed": true})
	_, err = handler.Handle(context.Background(), rpc.Call{
		Method: "operation.execute", OperationID: plan.OperationID, Params: execute,
	}, func(string, any) (uint64, error) { return 1, nil })
	rpcErr, ok := err.(*rpc.Error)
	if !ok || rpcErr.Code != domain.ActionPolicyInvalid || control.applied != 0 {
		t.Fatalf("operation.execute err=%#v applies=%d", err, control.applied)
	}
}

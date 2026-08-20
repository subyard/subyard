package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerinventory"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestRPCConcurrentKeysPrepareKeepsSessionEnvironmentStable(t *testing.T) {
	program, loaded, _, _ := credentialCLIFixture(t, strings.NewReader(""), nil, false)
	handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]rpcPlannedOperation)}
	const requests = 32
	ready := make(chan struct{}, requests)
	release := make(chan struct{})
	gated := rpc.HandlerFunc(func(ctx context.Context, call rpc.Call, emit rpc.Emit) (any, error) {
		ready <- struct{}{}
		<-release
		return handler.Handle(ctx, call, emit)
	})
	client, server := net.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- (rpc.Session{Handler: gated, Buffer: requests + 1, DrainOnEOF: true}).Serve(
			context.Background(), server, server,
		)
	}()
	codec := rpc.NewCodec(client, client)
	if err := codec.Write(rpc.Request{
		Version: rpc.ProtocolVersion, Type: "request", ID: "negotiate", Method: "rpc.negotiate",
	}); err != nil {
		t.Fatal(err)
	}
	if response, err := codec.ReadResponse(); err != nil || response.Error != nil {
		t.Fatalf("negotiate response=%#v err=%v", response, err)
	}
	for index := range requests {
		id := fmt.Sprintf("prepare-%d", index)
		if err := codec.Write(rpc.Request{
			Version: rpc.ProtocolVersion, Type: "request", ID: id, OperationID: id,
			Method: "keys.prepare", Params: json.RawMessage(`{"arguments":[]}`),
		}); err != nil {
			t.Fatal(err)
		}
	}
	for range requests {
		select {
		case <-ready:
		case <-time.After(time.Second):
			t.Fatal("RPC preparations did not reach the concurrency barrier")
		}
	}
	close(release)
	seen := make(map[string]bool, requests)
	for range requests {
		response, err := codec.ReadResponse()
		if err != nil || response.Error != nil {
			t.Fatalf("prepare response=%#v err=%v", response, err)
		}
		seen[response.OperationID] = true
	}
	if len(seen) != requests || program.env["SUBYARD_OPERATION_ID"] != "credential-operation" {
		t.Fatalf("concurrent preparations changed session state: ids=%d operation=%q",
			len(seen), program.env["SUBYARD_OPERATION_ID"])
	}
	_ = client.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("RPC session did not stop after EOF")
	}
}

func TestRPCOperationCopiesMutableMaps(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	program.inventoryRoutes["existing-route"] = config.Loaded{}
	program.discoveredOwners["existing-owner"] = ownerinventory.Connection{}

	operation := program.rpcOperation("operation-copy")
	delete(operation.inventoryRoutes, "existing-route")
	delete(operation.discoveredOwners, "existing-owner")
	operation.inventoryRoutes["operation-route"] = config.Loaded{}
	operation.discoveredOwners["operation-owner"] = ownerinventory.Connection{}

	if program.env["SUBYARD_OPERATION_ID"] != "" || operation.env["SUBYARD_OPERATION_ID"] != "operation-copy" {
		t.Fatalf("operation environment was not isolated: session=%q operation=%q",
			program.env["SUBYARD_OPERATION_ID"], operation.env["SUBYARD_OPERATION_ID"])
	}
	if _, ok := program.inventoryRoutes["existing-route"]; !ok || len(program.inventoryRoutes) != 1 {
		t.Fatalf("operation changed session inventory routes: %#v", program.inventoryRoutes)
	}
	if _, ok := program.discoveredOwners["existing-owner"]; !ok || len(program.discoveredOwners) != 1 {
		t.Fatalf("operation changed session discovered owners: %#v", program.discoveredOwners)
	}
}

func TestRPCExecuteUsesEnvironmentCapturedDuringPlan(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	operationID := "operation-context"
	runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{
		Schema: 1, OperationID: operationID, Status: "ok",
	}}}}
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
		AdapterRunner: runner, Incus: lifecycleIncus(),
	})
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
		Params: json.RawMessage(`{"command":"start","arguments":[]}`),
	}, func(string, any) (uint64, error) { return 1, nil }); err != nil {
		t.Fatal(err)
	}

	program.env["SUBYARD_SUDO_PREAUTHORIZED"] = "1"
	if _, err := handler.Handle(context.Background(), rpc.Call{
		ID: "execute", OperationID: operationID, Method: "operation.execute",
		Params: json.RawMessage(`{"confirmed":true}`),
	}, func(string, any) (uint64, error) { return 1, nil }); err != nil {
		t.Fatal(err)
	}
	if len(runner.Requests) != 1 || runner.Requests[0].Context["SUBYARD_SUDO_PREAUTHORIZED"] != "" {
		t.Fatalf("execute observed environment changed after plan: %#v", runner.Requests)
	}
}

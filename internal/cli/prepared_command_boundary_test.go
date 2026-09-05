package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/rpc"
)

func TestPreparedCommandFailureStillAuditsInvocation(t *testing.T) {
	root, environment, _ := preparationParityFixture(t)
	home := filepath.Join(root, "audit")
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", WorkingDir: root,
		Environment: append(environment, "SUBYARD_NO_AUDIT=", "SUBYARD_HOME="+home),
		Arguments:   []string{"stop", "--invalid"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 2 {
		t.Fatalf("invalid stop exit = %d", code)
	}
	data, err := os.ReadFile(filepath.Join(home, "logs", "yard.log"))
	if err != nil || strings.Count(string(data), " -- stop --invalid\n") != 1 {
		t.Fatalf("failed preparation invocation audit = %q, err=%v", data, err)
	}
}

func TestUnknownCoreHandlerFailsStartup(t *testing.T) {
	root, environment, _ := preparationParityFixture(t)
	path := filepath.Join(root, "config", "commands.registry")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, path, strings.Replace(string(data), "start||@lifecycle|", "start||@unregistered|", 1), 0o600)
	if _, err := New(Options{RepositoryRoot: root, Environment: environment}); err == nil || !strings.Contains(err.Error(), "@unregistered") {
		t.Fatalf("unknown handler startup error = %v", err)
	}
}

func TestRPCPlanLimitAndDuplicatePreserveOwnedPlans(t *testing.T) {
	root, environment, _ := preparationParityFixture(t)
	program, err := New(Options{RepositoryRoot: root, Environment: environment, WorkingDir: root})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	handler := &rpcHandler{cli: program, loaded: loaded}
	defer handler.closePlans()
	plan := func(id string) error {
		_, err := handler.Handle(context.Background(), rpc.Call{
			Method: "operation.plan", OperationID: id,
			Params: json.RawMessage(`{"command":"start"}`),
		}, nil)
		return err
	}
	for index := range 64 {
		if err := plan(fmt.Sprintf("plan-%d", index)); err != nil {
			t.Fatal(err)
		}
	}
	first := handler.plans["plan-0"]
	for _, test := range []struct{ id, code string }{{"plan-0", "duplicate_plan"}, {"overflow", "too_many_plans"}} {
		err := plan(test.id)
		if rpcErr, ok := err.(*rpc.Error); !ok || rpcErr.Code != test.code {
			t.Fatalf("%s error = %v", test.id, err)
		}
	}
	if len(handler.plans) != 64 || handler.plans["plan-0"] != first || first.closed {
		t.Fatal("rejected plan changed ownership of an existing plan")
	}
	handler.closePlans()
	if len(handler.plans) != 0 || !first.closed {
		t.Fatal("session cleanup retained a plan")
	}
}

func TestRPCDisconnectClosesPreparedResourcesWithoutExecution(t *testing.T) {
	root, environment, _ := preparationParityFixture(t)
	program, err := New(Options{RepositoryRoot: root, Environment: environment, WorkingDir: root})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	handler := &rpcHandler{cli: program, loaded: loaded}
	closed := make(chan struct{})
	gated := rpc.HandlerFunc(func(ctx context.Context, call rpc.Call, emit rpc.Emit) (any, error) {
		result, err := handler.Handle(ctx, call, emit)
		if err == nil && call.Method == "operation.plan" {
			handler.plans[call.OperationID].closeResource = func() error { close(closed); return nil }
		}
		return result, err
	})
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	done := make(chan error, 1)
	go func() {
		err := (rpc.Session{Handler: gated}).Serve(context.Background(), server, server)
		handler.closePlans()
		done <- err
	}()
	codec := rpc.NewCodec(client, client)
	for _, request := range []rpc.Request{
		{Version: rpc.ProtocolVersion, Type: "request", ID: "negotiate", Method: "rpc.negotiate"},
		{Version: rpc.ProtocolVersion, Type: "request", ID: "plan", OperationID: "disconnect", Method: "operation.plan", Params: json.RawMessage(`{"command":"start"}`)},
	} {
		if err := codec.Write(request); err != nil {
			t.Fatal(err)
		}
		if response, err := codec.ReadResponse(); err != nil || response.Error != nil {
			t.Fatalf("response=%#v err=%v", response, err)
		}
	}
	select {
	case <-closed:
		t.Fatal("resource closed before disconnect")
	default:
	}
	_ = client.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session did not finish after disconnect")
	}
	select {
	case <-closed:
	default:
		t.Fatal("disconnect leaked the prepared resource")
	}
}

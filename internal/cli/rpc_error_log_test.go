package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/audit"
	"github.com/Subyard/Subyard/internal/rpc"
)

// The owner keeps the full text of each failed request, which the session
// sends only as its bounded error, and `yard logs --rpc` shows the newest.
func TestOwnerRPCFailuresAreKeptAndShownByLogsRPC(t *testing.T) {
	fixture := newFixture(t)
	home := environmentValue(fixture.Environment, "SUBYARD_HOME")
	failures := map[string]error{
		"operation.plan":    fmt.Errorf("prepare start: %w", errors.New("profile catalog at /owner/path is invalid\n\x1b[31m")),
		"owner.inventory":   rpc.RefusalError(rpc.CodePlanFailed, "inventory refused", rpc.ErrorData{}),
		"operation.execute": context.Canceled,
	}
	recorder := rpcErrorRecorder{log: audit.RPCErrorLog{Home: home}, handler: rpc.HandlerFunc(
		func(_ context.Context, call rpc.Call, _ rpc.Emit) (any, error) {
			return nil, failures[call.Method]
		})}
	for index, method := range []string{"operation.plan", "owner.inventory", "operation.execute", "context.get"} {
		id := fmt.Sprintf("veranda-%d", index+1)
		_, err := recorder.Handle(context.Background(), rpc.Call{ID: id, OperationID: id, Method: method}, nil)
		if !errors.Is(err, failures[method]) {
			t.Fatalf("%s: the recorder changed the error: %v", method, err)
		}
	}
	var stdout, stderr bytes.Buffer
	program := fixture.New(Options{
		Arguments:   []string{"logs", "--rpc", "-n", "5"},
		Environment: append(fixture.Environment, "SUBYARD_NO_AUDIT=1"),
		Stdout:      &stdout, Stderr: &stderr,
	})
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 ||
		!strings.HasSuffix(lines[0], " request=veranda-1 method=operation.plan code=internal -- prepare start: profile catalog at /owner/path is invalid\\n\\x1b[31m") ||
		!strings.HasSuffix(lines[1], " request=veranda-2 method=owner.inventory code=plan_failed -- plan_failed: inventory refused") {
		t.Fatalf("logs --rpc = %q", stdout.String())
	}
}

func TestRPCStdioSessionRecordsAFailedRequest(t *testing.T) {
	fixture := newFixture(t)
	client, server := net.Pipe()
	defer client.Close()
	if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	program := fixture.New(Options{
		Program: "yard", Arguments: []string{"rpc", "--stdio"}, Environment: fixture.Environment,
		WorkingDir: fixture.Root, Stdin: server, Stdout: server, Stderr: &stderr,
	})
	done := make(chan int, 1)
	go func() {
		done <- program.Run(context.Background())
		_ = server.Close()
	}()
	codec := rpc.NewCodec(client, client)
	for _, request := range []rpc.Request{
		{Version: 1, Type: "request", ID: "negotiate", Method: "rpc.negotiate"},
		{Version: 1, Type: "request", ID: "veranda-2", OperationID: "op-plan", Method: "operation.plan",
			Params: json.RawMessage(`{"command":"no-such-command","arguments":[]}`)},
	} {
		if err := codec.Write(request); err != nil {
			t.Fatal(err)
		}
		if _, err := codec.ReadResponse(); err != nil {
			t.Fatal(err)
		}
	}
	_ = client.Close()
	<-done
	records, err := (audit.RPCErrorLog{Home: environmentValue(fixture.Environment, "SUBYARD_HOME")}).Read(5)
	if err != nil || len(records) != 1 || records[0].Request != "veranda-2" || records[0].Operation != "op-plan" ||
		records[0].Method != "operation.plan" || records[0].Code == "" || records[0].Error == "" {
		t.Fatalf("records = %+v err=%v stderr=%q", records, err, stderr.String())
	}
}

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/audit"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/uninstall"
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
	recorder, closeRecorder := startRPCErrorRecorder(rpc.HandlerFunc(
		func(_ context.Context, call rpc.Call, _ rpc.Emit) (any, error) {
			return nil, failures[call.Method]
		}), audit.RPCErrorLog{Home: home})
	for index, method := range []string{"operation.plan", "owner.inventory", "operation.execute", "context.get"} {
		id := fmt.Sprintf("veranda-%d", index+1)
		_, err := recorder.Handle(context.Background(), rpc.Call{ID: id, OperationID: id, Method: method}, nil)
		if !errors.Is(err, failures[method]) {
			t.Fatalf("%s: the recorder changed the error: %v", method, err)
		}
	}
	closeRecorder()
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

// Run returns only after the session's queued records are written.
func TestRPCStdioSessionRecordsAFailedRequest(t *testing.T) {
	fixture := newFixture(t)
	if err := os.Mkdir(environmentValue(fixture.Environment, "SUBYARD_HOME"), 0o700); err != nil {
		t.Fatal(err)
	}
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

// failingRecorder records through the session admission of a fixture CLI, so
// its writes wait for the owner barrier like a real session's.
func failingRecorder(t *testing.T, failure error) (*CLI, string, rpcErrorRecorder, func()) {
	t.Helper()
	fixture := newFixture(t)
	home := environmentValue(fixture.Environment, "SUBYARD_HOME")
	// The session log writes only into a data home that existed at its start.
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	cli := fixture.New(Options{Environment: fixture.Environment})
	recorder, closeRecorder := startRPCErrorRecorder(rpc.HandlerFunc(
		func(_ context.Context, call rpc.Call, _ rpc.Emit) (any, error) {
			_ = cli.rpcOperation(call.OperationID)
			return nil, failure
		}), audit.RPCErrorLog{Home: home, BeforeWrite: cli.sessionLogAdmission(home)})
	return cli, home, recorder, closeRecorder
}

func holdOwnerBarrier(t *testing.T, home string) func() {
	t.Helper()
	_, release, err := uninstall.OwnerLock(context.Background(), home, nil)
	if err != nil {
		t.Fatal(err)
	}
	return release
}

// A held owner barrier never delays a failed response; the queued records
// follow in order once it is released, and the rest are dropped.
func TestRPCErrorRecordsWaitForTheOwnerBarrierAfterTheResponse(t *testing.T) {
	failure := errors.New("plan failed")
	_, home, recorder, closeRecorder := failingRecorder(t, failure)
	release := holdOwnerBarrier(t, home)
	started := time.Now()
	for index := 1; index <= 100; index++ {
		id := fmt.Sprintf("veranda-%d", index)
		if _, err := recorder.Handle(context.Background(), rpc.Call{ID: id, OperationID: id, Method: "operation.plan"}, nil); !errors.Is(err, failure) {
			t.Fatalf("error = %v", err)
		}
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("100 failed responses took %s", elapsed)
	}
	log := audit.RPCErrorLog{Home: home}
	if records, err := log.Read(200); err != nil || len(records) != 0 {
		t.Fatalf("records under the barrier = %+v err=%v", records, err)
	}
	release()
	// The writer is still waiting for the barrier; let it take it before the
	// session ends, as an idle session would.
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if records, _ := log.Read(1); len(records) > 0 {
			break
		}
	}
	closeRecorder()
	records, err := log.Read(200)
	if err != nil || len(records) < rpcErrorBacklog || len(records) > rpcErrorBacklog+1 {
		t.Fatalf("records = %d err=%v", len(records), err)
	}
	for index, record := range records {
		if record.Request != fmt.Sprintf("veranda-%d", index+1) {
			t.Fatalf("record %d = %+v", index, record)
		}
	}
}

func TestClosingTheRecorderUnderAHeldOwnerBarrierReturnsPromptly(t *testing.T) {
	failure := errors.New("plan failed")
	_, home, recorder, closeRecorder := failingRecorder(t, failure)
	release := holdOwnerBarrier(t, home)
	defer release()
	if _, err := recorder.Handle(context.Background(), rpc.Call{ID: "veranda-1", Method: "operation.plan"}, nil); !errors.Is(err, failure) {
		t.Fatalf("error = %v", err)
	}
	closed := make(chan struct{})
	go func() {
		closeRecorder()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("closing waited for the owner barrier")
	}
	if records, err := (audit.RPCErrorLog{Home: home}).Read(5); err != nil || len(records) != 0 {
		t.Fatalf("records = %+v err=%v", records, err)
	}
}

// Concurrent failing requests copy the session CLI while records are written.
func TestConcurrentFailedRequestsShareNoWriterState(t *testing.T) {
	failure := errors.New("plan failed")
	_, home, recorder, closeRecorder := failingRecorder(t, failure)
	var requests sync.WaitGroup
	for index := 1; index <= 32; index++ {
		requests.Go(func() {
			id := fmt.Sprintf("veranda-%d", index)
			_, _ = recorder.Handle(context.Background(), rpc.Call{ID: id, OperationID: id, Method: "operation.plan"}, nil)
		})
	}
	requests.Wait()
	closeRecorder()
	if records, err := (audit.RPCErrorLog{Home: home}).Read(64); err != nil || len(records) != 32 {
		t.Fatalf("records = %d err=%v", len(records), err)
	}
}

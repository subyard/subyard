package incusclient

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/contracttest"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/lxc/incus/v6/shared/api"
)

func TestNormalizeErrorClassifiesTemporaryIncusFailure(t *testing.T) {
	cause := api.StatusErrorf(http.StatusServiceUnavailable, "storage pool unavailable")
	temporary := normalizeError("start instance", cause)
	if !errors.Is(temporary, ports.ErrIncusUnavailable) {
		t.Fatalf("HTTP 503 was not classified as temporary: %v", temporary)
	}
	if !errors.Is(temporary, cause) {
		t.Fatalf("HTTP 503 cause was not preserved: %v", temporary)
	}

	permanent := normalizeError("start instance", errors.New("invalid instance configuration"))
	if errors.Is(permanent, ports.ErrIncusUnavailable) {
		t.Fatalf("permanent error was classified as temporary: %v", permanent)
	}
}

func TestOfficialClientMapsServerAndInstance(t *testing.T) {
	server, err := testkit.NewIncusServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	server.SetInstance("subyard", "yard", map[string]any{
		"name": "yard", "project": "subyard", "type": "container", "status": "Running",
		"expanded_config":  map[string]string{"security.nesting": "true"},
		"expanded_devices": map[string]map[string]string{"root": {"type": "disk", "path": "/"}},
	})
	contracttest.IncusRead(t, New(server.SocketPath, "projects"))
}

func TestOfficialClientListsAndChangesInstancePower(t *testing.T) {
	server, err := testkit.NewIncusServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	server.SetInstance("subyard-a", "yard-a", map[string]any{
		"name": "yard-a", "project": "subyard-a", "type": "container", "status": "Stopped",
	})
	server.SetInstance("subyard-b", "yard-b", map[string]any{
		"name": "yard-b", "project": "subyard-b", "type": "virtual-machine", "status": "Running",
	})
	client := New(server.SocketPath, "projects")
	instances, err := client.ListInstances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 2 {
		t.Fatalf("unexpected inventory: %#v", instances)
	}
	if err := client.SetInstancePower(context.Background(), "subyard-a", "yard-a", "start", false); err != nil {
		t.Fatal(err)
	}
	calls := server.PowerCalls()
	if len(calls) != 1 || calls[0].Project != "subyard-a" || calls[0].Name != "yard-a" ||
		calls[0].Action != "start" || calls[0].Force {
		t.Fatalf("unexpected power call: %#v", calls)
	}
}

func TestRequiredExtensionFailsClosed(t *testing.T) {
	server, err := testkit.NewIncusServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	server.SetExtensions()
	client := New(server.SocketPath, "projects")
	if _, err := client.Server(context.Background()); err == nil {
		t.Fatal("missing required extension was accepted")
	}
	if _, err := client.Instance(context.Background(), "subyard", "yard"); err == nil {
		t.Fatal("instance call bypassed required extension validation")
	}
	if _, err := client.Exec(context.Background(), "subyard", "yard", ports.InstanceExecRequest{
		Command: []string{"true"},
	}); err == nil {
		t.Fatal("exec call bypassed required extension validation")
	}
	events, errorsOut := client.Events(context.Background(), []string{"lifecycle"})
	if _, ok := <-events; ok {
		t.Fatal("event stream remained open without required extension")
	}
	if err := <-errorsOut; err == nil {
		t.Fatal("event stream did not report missing required extension")
	}
}

func TestOfficialClientMapsEventsAndDisconnect(t *testing.T) {
	server, err := testkit.NewIncusServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, errorsOut := New(server.SocketPath).Events(ctx, []string{"lifecycle"})
	waitContext, stopWaiting := context.WithTimeout(context.Background(), time.Second)
	defer stopWaiting()
	if err := server.WaitForEventClient(waitContext); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(server.EventQuery(), "all-projects=true") {
		t.Fatalf("event stream is not subscribed across projects: %q", server.EventQuery())
	}
	if err := server.Emit(map[string]any{
		"type": "lifecycle", "timestamp": time.Unix(100, 0).UTC(),
		"metadata": map[string]any{"id": "operation-1", "action": "instance-started", "apiToken": "must-not-escape"},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if event.OperationID != "operation-1" || event.Kind != "lifecycle" || event.Sequence != 1 ||
			event.Data["action"] != "instance-started" {
			t.Fatalf("unexpected event: %#v", event)
		}
		if _, leaked := event.Data["apiToken"]; leaked {
			t.Fatalf("secret-like Incus metadata escaped event projection: %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("Incus event was not delivered")
	}
	server.DisconnectEvents()
	select {
	case disconnect := <-errorsOut:
		if disconnect == nil {
			t.Fatal("event disconnect returned no error")
		}
	case <-time.After(time.Second):
		t.Fatal("Incus event disconnect was not reported")
	}
}

func TestOfficialClientExecUsesAsyncWebsocketsAndFlushesOutput(t *testing.T) {
	server, err := testkit.NewIncusServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	server.QueueExec(testkit.IncusServerExecStep{
		Stdout: []byte("output\n"), Stderr: []byte("diagnostic\n"),
	})
	result, err := New(server.SocketPath).Exec(context.Background(), "subyard", "yard", ports.InstanceExecRequest{
		Command:     []string{"sh", "-c", "read value; printf '%s' \"$value\""},
		Environment: map[string]string{"FIXTURE": "yes"}, Stdin: []byte("input\n"), User: 1000, Group: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitContext, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.WaitForExecInput(waitContext, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(result.Stdout, []byte("output\n")) ||
		!bytes.Equal(result.Stderr, []byte("diagnostic\n")) || result.ExitCode != 0 {
		t.Fatalf("unexpected exec result: %#v", result)
	}
	calls := server.ExecCalls()
	if len(calls) != 1 || calls[0].Project != "subyard" || calls[0].Name != "yard" ||
		calls[0].Environment["FIXTURE"] != "yes" || !bytes.Equal(calls[0].Stdin, []byte("input\n")) ||
		calls[0].User != 1000 || calls[0].Group != 1000 {
		t.Fatalf("structured exec request changed: %#v", calls)
	}
}

func TestOfficialClientExecMapsStartOperationExitAndCancellation(t *testing.T) {
	server, err := testkit.NewIncusServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	server.QueueExec(
		testkit.IncusServerExecStep{StartError: "command rejected"},
		testkit.IncusServerExecStep{OperationError: "runtime disconnected"},
		testkit.IncusServerExecStep{ExitCode: 17, Stderr: []byte("failed\n")},
	)
	client := New(server.SocketPath)
	request := ports.InstanceExecRequest{Command: []string{"false"}}
	if _, err := client.Exec(context.Background(), "subyard", "yard", request); err == nil ||
		!strings.Contains(err.Error(), "command rejected") {
		t.Fatalf("start error was not normalized: %v", err)
	}
	if _, err := client.Exec(context.Background(), "subyard", "yard", request); err == nil ||
		!strings.Contains(err.Error(), "runtime disconnected") {
		t.Fatalf("operation error was not normalized: %v", err)
	}
	result, err := client.Exec(context.Background(), "subyard", "yard", request)
	if err == nil || !strings.Contains(err.Error(), "status 17") || result.ExitCode != 17 ||
		!bytes.Equal(result.Stderr, []byte("failed\n")) {
		t.Fatalf("exit status was not preserved: result=%#v err=%v", result, err)
	}

	release := make(chan struct{})
	server.QueueExec(testkit.IncusServerExecStep{Release: release})
	ctx, cancel := context.WithCancelCause(context.Background())
	finished := make(chan error, 1)
	go func() {
		_, callErr := client.Exec(ctx, "subyard", "yard", request)
		finished <- callErr
	}()
	waitContext, stopWaiting := context.WithTimeout(context.Background(), time.Second)
	defer stopWaiting()
	if err := server.WaitForExecCount(waitContext, 3); err != nil {
		t.Fatal(err)
	}
	cancel(errors.New("test cancellation"))
	select {
	case err := <-finished:
		if err == nil || !strings.Contains(err.Error(), "test cancellation") {
			t.Fatalf("context cancellation was not preserved: %v", err)
		}
	case <-waitContext.Done():
		t.Fatal("cancelled exec did not stop")
	}
}

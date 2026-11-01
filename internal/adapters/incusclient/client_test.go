package incusclient

import (
	"bytes"
	"context"
	"errors"
	"github.com/gorilla/websocket"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/contracttest"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/yardnetwork"
	"github.com/lxc/incus/v6/shared/api"
)

func TestLocalInstallationAbsentPreservesUnavailableState(t *testing.T) {
	for _, scenario := range []string{"cold", "installed", "retained", "symlink", "custom socket", "socket environment", "relative directory"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			directory := filepath.Join(root, "incus-state")
			t.Setenv("PATH", root)
			t.Setenv("INCUS_DIR", directory)
			t.Setenv("INCUS_SOCKET", "")
			client := New("")
			var err error
			switch scenario {
			case "installed":
				err = os.WriteFile(filepath.Join(root, "incus"), []byte("#!/bin/sh\n"), 0o700)
			case "retained":
				err = os.Mkdir(directory, 0o700)
			case "symlink":
				err = os.Symlink(filepath.Join(root, "missing"), directory)
			case "custom socket":
				client = New(filepath.Join(root, "missing.socket"))
			case "socket environment":
				t.Setenv("INCUS_SOCKET", filepath.Join(root, "missing.socket"))
			case "relative directory":
				t.Setenv("INCUS_DIR", "relative-incus-state")
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := client.LocalInstallationAbsent(); got != (scenario == "cold") {
				t.Fatalf("LocalInstallationAbsent() = %v", got)
			}
		})
	}
}

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

func TestOfficialClientClassifiesAsyncPowerErrors(t *testing.T) {
	for _, scenario := range []struct {
		message   string
		temporary bool
	}{
		{`Storage pool "default" unavailable on this server`, true},
		{`Storage pool "yard-data" unavailable on this server`, true},
		{`Storage pool "default" not found`, false},
		{`Failed to mount storage pool "default": permission denied`, false},
		{`invalid instance configuration`, false},
		{`Storage pool "" unavailable on this server`, false},
	} {
		t.Run(scenario.message, func(t *testing.T) {
			server, err := testkit.NewIncusServer(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = server.Close() })
			server.SetInstance("subyard", "yard", map[string]any{
				"name": "yard", "project": "subyard", "type": "container", "status": "Stopped",
			})
			server.QueuePowerErrors(scenario.message, scenario.message)
			client := New(server.SocketPath)
			for caller, err := range map[string]error{
				"boot": client.SetInstancePower(context.Background(), "subyard", "yard", "start", false),
				"network": client.NetworkPower(context.Background(), yardnetwork.Yard{
					Project: "subyard", Instance: "yard",
				}, "start"),
			} {
				if err == nil || !strings.Contains(err.Error(), scenario.message) {
					t.Fatalf("%s operation error was lost: %v", caller, err)
				}
				if got := errors.Is(err, ports.ErrIncusUnavailable); got != scenario.temporary {
					t.Fatalf("%s temporary = %v, want %v: %v", caller, got, scenario.temporary, err)
				}
			}
		})
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

func TestReconcileStateReadsDefaultProfileConfiguration(t *testing.T) {
	fake := newNetworkIncusServer(t)
	fake.projects["project-a"] = fakeProject{Config: map[string]string{"restricted": "true"}}
	fake.profiles["project-a/default"] = fakeProfile{Config: map[string]string{"raw.lxc": "lxc.apparmor.profile=unconfined", "limits.cpu": "4"}, Devices: map[string]map[string]string{"root": {"type": "disk", "path": "/", "pool": "default"}}}
	client := New(fake.socket, "projects")
	observed, err := client.ReconcileState(context.Background(), "project-a", "absent-instance", "default", "srv", "incusbr0")
	if err != nil {
		t.Fatal(err)
	}
	if !observed.ProfileFound || observed.ProfileConfig["raw.lxc"] != "lxc.apparmor.profile=unconfined" || observed.ProfileConfig["limits.cpu"] != "4" {
		t.Fatal("native profile configuration omitted", observed.ProfileConfig)
	}
}

func TestFixtureExecResponderChoosesExitFromExactCapturedInput(t *testing.T) {
	server, err := testkit.NewIncusServer(testkit.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	server.SetExecResponder(func(call testkit.IncusServerExecCall) testkit.IncusServerExecStep {
		if call.Project != "subyard" || call.Name != "yard" || len(call.Command) != 1 || call.Command[0] != "fixture" {
			return testkit.IncusServerExecStep{ExitCode: 99}
		}
		return testkit.IncusServerExecStep{AfterInput: func(actual testkit.IncusServerExecCall) int {
			if bytes.Equal(actual.Stdin, []byte("expected\n")) {
				return 37
			}
			return 99
		}}
	})
	for _, value := range []struct {
		input string
		exit  int
	}{{"expected\n", 37}, {"foreign\n", 99}} {
		result, err := New(server.SocketPath).Exec(context.Background(), "subyard", "yard", ports.InstanceExecRequest{Command: []string{"fixture"}, Stdin: []byte(value.input)})
		if err == nil || result.ExitCode != value.exit {
			t.Fatalf("captured-input response: %+v %v", result, err)
		}
	}
}

func TestNativeExecRequiresExplicitIntegralReturnAfterStreamsDrain(t *testing.T) {
	for _, mode := range []string{"zero", "nonzero", "negative", "missing", "string", "fractional", "failure-missing", "ordinary-missing", "ordinary-nonzero", "stream-nonzero", "unscoped-nonzero", "remote-failure"} {
		t.Run(mode, func(t *testing.T) {
			server, err := testkit.NewIncusServer(testkit.TempDir(t))
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			metadata := map[string]any{}
			switch mode {
			case "zero", "remote-failure":
				metadata["return"] = 0
			case "nonzero", "ordinary-nonzero", "stream-nonzero", "unscoped-nonzero":
				metadata["return"] = 7
			case "negative":
				metadata["return"] = -1
			case "string":
				metadata["return"] = "0"
			case "fractional":
				metadata["return"] = 0.5
			}
			step := testkit.IncusServerExecStep{Stdout: []byte("complete output"), ReturnMetadata: metadata}
			if mode == "failure-missing" || mode == "remote-failure" {
				step.OperationError = "remote failed"
			}
			server.QueueExec(step)
			observer := &custodyObserver{}
			ctx := context.Background()
			if !strings.HasPrefix(mode, "ordinary-") {
				ctx = ports.WithNativeOperationObserver(ctx, observer)
			}
			if mode != "unscoped-nonzero" {
				ctx = ports.WithNativeSynchronousConfigCAS(ctx)
			}
			client := New(server.SocketPath)
			request := ports.InstanceExecRequest{Command: []string{"fixture"}}
			var result ports.InstanceExecResult
			if mode == "stream-nonzero" {
				result, err = client.StreamExec(ctx, "subyard", "yard", request, bytes.NewReader(nil))
			} else {
				result, err = client.Exec(ctx, "subyard", "yard", request)
			}
			var failure *ports.NativeOperationFailure
			witnessed := errors.As(err, &failure) && failure.ExecExitCode != nil
			if witnessed != (mode == "nonzero" || mode == "negative") {
				t.Fatal("completed failure custody scope changed", mode, err)
			}
			if witnessed && (failure.Operation != observer.terminal[0] || failure.Operation.Status != 200 || *failure.ExecExitCode != result.ExitCode || result.ExitCode == 0) {
				t.Fatal("command exit lost exact real SDK200 tuple", failure, result.ExitCode)
			}
			if string(result.Stdout) != "complete output" {
				t.Fatal("streams did not drain", result)
			}
			invalid := mode == "missing" || mode == "string" || mode == "fractional" || mode == "failure-missing"
			if !strings.HasPrefix(mode, "ordinary-") {
				if len(observer.accepted) != 1 {
					t.Fatal("missing exact SDK acceptance")
				}
				final, queryErr := client.NativeOperationState(context.Background(), observer.accepted[0])
				if invalid {
					if len(observer.terminal) != 0 || queryErr == nil {
						t.Fatal("invalid exec exit sealed or queried terminal", queryErr, observer.terminal)
					}
				} else {
					status := 200
					if mode == "remote-failure" {
						status = 400
					}
					if queryErr != nil || len(observer.terminal) != 1 || final != observer.terminal[0] || final.Status != status {
						t.Fatal("valid exec exit lost real remote status", queryErr, observer.terminal)
					}
					if mode == "remote-failure" && (!errors.As(err, &failure) || failure.Operation.Status != 400 || failure.ExecExitCode != nil) {
						t.Fatal("existing real remote400 failure changed", err)
					}
				}
			}
			if mode == "zero" || mode == "ordinary-missing" {
				if err != nil || result.ExitCode != 0 || observer.uncertain {
					t.Fatal(result, err, observer.uncertain)
				}
				return
			}
			if err == nil {
				t.Fatal("missing/failed native return became success")
			}
			if observer.uncertain != invalid {
				t.Fatalf("unknown=%v err=%v", observer.uncertain, err)
			}
			if mode == "negative" && result.ExitCode != -1 {
				t.Fatal("signed exit lost")
			}
		})
	}
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), math.MaxFloat64} {
		if _, valid := explicitNativeExecExit(map[string]any{"return": value}); valid {
			t.Fatal("invalid numeric return accepted", value)
		}
	}
}

// The official SDK operation finishes with return7 while output websockets are
// held open. Only the owning test release drains them after bounded refusal.
func TestNativeCompletedExecRequiresSDKStreamDrain(t *testing.T) {
	fixture, err := testkit.NewIncusServer(testkit.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.Close()
	fixture.QueueExec(testkit.IncusServerExecStep{ExitCode: 7})
	release := make(chan struct{})
	blocked := make(chan struct{})
	var once sync.Once
	var handlers sync.WaitGroup
	listener, err := net.Listen("unix", filepath.Join(testkit.TempDir(t), "exec.socket"))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/websocket") && (strings.HasSuffix(r.URL.Query().Get("secret"), "-stdout") || strings.HasSuffix(r.URL.Query().Get("secret"), "-stderr")) {
			handlers.Add(1)
			defer handlers.Done()
			conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			once.Do(func() { close(blocked) })
			<-release
			return
		}
		fixture.ServeHTTP(w, r)
	})}
	go server.Serve(listener)
	defer func() { close(release); server.Close(); listener.Close(); handlers.Wait() }()
	observer := &custodyObserver{}
	ctx, cancel := context.WithTimeout(ports.WithNativeSynchronousConfigCAS(ports.WithNativeOperationObserver(context.Background(), observer)), 300*time.Millisecond)
	defer cancel()
	_, err = New(listener.Addr().String()).Exec(ctx, "project", "yard", ports.InstanceExecRequest{Command: []string{"fixture"}})
	var failure *ports.NativeOperationFailure
	if err == nil || errors.As(err, &failure) || !observer.uncertain || len(observer.terminal) != 1 || observer.terminal[0].Status != 200 {
		t.Fatal("undrained streams became completed command failure", err, observer)
	}
	select {
	case <-blocked:
	default:
		t.Fatal("withheld SDK stream was not reached")
	}
}

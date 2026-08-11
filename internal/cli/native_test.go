package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/shelladapter"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/state"
	"github.com/Subyard/Subyard/internal/testkit"
)

type statusFactsStub struct{ value domain.StatusFacts }

type projectActionObservationProbe struct {
	execute func(ports.InstanceExecRequest) (ports.InstanceExecResult, error)
	stream  func(ports.InstanceExecRequest, io.Reader) (ports.InstanceExecResult, error)
}

type projectActionArchive string

func (archive projectActionArchive) Open(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(string(archive))), nil
}

func (probe projectActionObservationProbe) Execute(
	_ context.Context,
	_ domain.Context,
	request ports.InstanceExecRequest,
) (ports.InstanceExecResult, error) {
	if probe.execute == nil {
		return ports.InstanceExecResult{}, nil
	}
	return probe.execute(request)
}

func (probe projectActionObservationProbe) Stream(
	_ context.Context,
	_ domain.Context,
	request ports.InstanceExecRequest,
	input io.Reader,
) (ports.InstanceExecResult, error) {
	if probe.stream == nil {
		return ports.InstanceExecResult{}, nil
	}
	return probe.stream(request, input)
}

func (stub statusFactsStub) ReadStatusFacts(context.Context, domain.Context, bool) (domain.StatusFacts, error) {
	return stub.value, nil
}

type spaceIncusStub struct {
	*testkit.Incus
	instanceErrors map[string]error
}

type spaceAdvancingExecutor struct {
	clock  *testkit.ManualClock
	result ports.InstanceExecResult
}

func (executor spaceAdvancingExecutor) Exec(
	context.Context,
	string,
	string,
	ports.InstanceExecRequest,
) (ports.InstanceExecResult, error) {
	executor.clock.Advance(5 * time.Second)
	return executor.result, nil
}

func (stub spaceIncusStub) Instance(
	ctx context.Context,
	project, name string,
) (ports.InstanceInfo, error) {
	if err := stub.instanceErrors[project+"/"+name]; err != nil {
		return ports.InstanceInfo{}, err
	}
	return stub.Incus.Instance(ctx, project, name)
}

func TestNativeCodeAndShellDoNotStartCredentialAutoSync(t *testing.T) {
	root, environment, stateDirectory := nativeFixture(t)
	store, err := state.NewFileStore(stateDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), domain.ProjectRecord{
		Schema: 1, ProjectID: "demo-12345678", Name: "Demo", HostPath: "/host/Demo",
		YardPath: "/srv/workspaces/demo-12345678/src", Mode: domain.ProjectSync,
		SSHHost: "yard", Target: "yard",
	}); err != nil {
		t.Fatal(err)
	}
	configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	if err := os.MkdirAll(filepath.Join(configHome, "keys"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(configHome, "host-id"), "owner-a\n", 0o600)
	writeCLIFile(t, filepath.Join(configHome, "keys", "identity.json"), "{}\n", 0o600)
	dispatchLog := filepath.Join(root, "credential-dispatch.log")
	dispatcher := filepath.Join(root, "dispatcher")
	writeCLIFile(t, dispatcher, `#!/bin/sh
printf '%s\n' "$*" >> "$DISPATCH_LOG"
exit 97
`, 0o700)
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	incusLog := filepath.Join(root, "incus.log")
	writeCLIFile(t, filepath.Join(bin, "incus"), `#!/bin/sh
case "$1" in
  list) printf 'RUNNING\n' ;;
  exec) printf '%s\n' "$*" > "$INCUS_LOG" ;;
esac
`, 0o700)
	codeLog := filepath.Join(root, "code.log")
	writeCLIFile(t, filepath.Join(bin, "code"), `#!/bin/sh
printf '%s\0' "$@" > "$CODE_LOG"
`, 0o700)
	path := bin + string(os.PathListSeparator) + os.Getenv("PATH")
	t.Setenv("PATH", path)
	environment = append(environment,
		"PATH="+path,
		"CODE_LOG="+codeLog,
		"DISPATCH_LOG="+dispatchLog,
		"INCUS_LOG="+incusLog,
	)

	incus := &testkit.Incus{
		Instances: map[string]ports.InstanceInfo{"subyard/yard": {
			Status: "Running", Devices: map[string]map[string]string{"ssh": {"type": "proxy"}},
		}},
	}
	codePrompt := &testkit.Prompt{}
	var codeStderr bytes.Buffer
	codeProgram, err := New(Options{
		RepositoryRoot: root, DispatcherPath: dispatcher, Program: "yard",
		Arguments: []string{"code", "Demo"}, Environment: environment, WorkingDir: root,
		Stdin: strings.NewReader(""), Incus: incus, Executor: incus,
		Prompt: codePrompt, Stderr: &codeStderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := codeProgram.Run(context.Background()); code != 0 {
		t.Fatalf("code failed: code=%d stderr=%q", code, codeStderr.String())
	}
	wantWorkspace := filepath.Join(
		configHome, "workspaces", "eWFyZA.demo-12345678", "Demo.code-workspace",
	)
	codeArguments, readErr := os.ReadFile(codeLog)
	if readErr != nil || string(codeArguments) != wantWorkspace+"\x00" ||
		len(incus.ExecCalls) != 0 || len(codePrompt.Seen) != 0 ||
		strings.Contains(codeStderr.String(), "Proceed?") {
		t.Fatalf("code launch drifted: arguments=%q readErr=%v exec=%#v", codeArguments, readErr, incus.ExecCalls)
	}
	workspacePayload, err := os.ReadFile(wantWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	var workspace struct {
		RemoteAuthority string            `json:"remoteAuthority"`
		Settings        map[string]string `json:"settings"`
	}
	if err := json.Unmarshal(workspacePayload, &workspace); err != nil ||
		workspace.RemoteAuthority != "ssh-remote+yard" ||
		workspace.Settings["window.title"] != "${rootNameShort} — Yard SSH: owner-a/default" ||
		strings.Contains(workspace.Settings["window.title"], "yard-") {
		t.Fatalf("code workspace identity drifted: workspace=%#v err=%v", workspace, err)
	}

	var shellStderr bytes.Buffer
	shellProgram, err := New(Options{
		RepositoryRoot: root, DispatcherPath: dispatcher, Program: "yard",
		Arguments: []string{"shell", "--", "true"}, Environment: environment, WorkingDir: root,
		Stderr: &shellStderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := shellProgram.Run(context.Background()); code != 0 {
		t.Fatalf("shell failed: code=%d stderr=%q", code, shellStderr.String())
	}
	if payload, err := os.ReadFile(incusLog); err != nil ||
		!strings.Contains(string(payload), "exec yard --project subyard") {
		t.Fatalf("shell launch drifted: log=%q err=%v", payload, err)
	}
	if payload, err := os.ReadFile(dispatchLog); err == nil {
		t.Fatalf("session launch dispatched credential sync: %q", payload)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if output := codeStderr.String() + shellStderr.String(); strings.Contains(output, "credential sync") {
		t.Fatalf("session launch emitted a credential warning: %q", output)
	}
}

func TestRPCResyncReturnsFullSnapshotAndContinuesMonotonicSessionEvents(t *testing.T) {
	root, environment, stateDirectory := nativeFixture(t)
	store, err := state.NewFileStore(stateDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), domain.ProjectRecord{
		Schema: 1, ProjectID: "demo-12345678", Name: "Demo", HostPath: "/host/Demo",
		YardPath: "/srv/workspaces/demo-12345678/src", Mode: domain.ProjectSync, SSHHost: "yard",
	}); err != nil {
		t.Fatal(err)
	}
	fakeIncus := &testkit.Incus{
		ServerInfo: ports.ServerInfo{Environment: "incus", Version: "6.23", APIExtensions: []string{"projects"}},
		Instances: map[string]ports.InstanceInfo{"subyard/yard": {
			Name: "yard", Project: "subyard", Type: domain.InstanceContainer, Status: "Stopped",
			Config: map[string]string{}, Devices: map[string]map[string]string{},
		}},
	}
	metadata := domain.CredentialMetadata{
		SchemaVersion: 1, CredentialID: "cred-0123456789abcdef0123456789abcdef",
		RevisionID: "actor-a-000000000001-aaaaaaaa", Label: "fixture", Kind: "token", Zone: "fixture",
		Scope: "staging", Consumer: "staging-env", State: "active",
		RecipientActors: []string{"actor-a"}, Syncable: true, ActorID: "actor-a",
		ActorCounter: 1, Timestamp: time.Unix(100, 0).UTC(),
	}
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
		Incus: fakeIncus, Executor: fakeIncus,
		StatusFacts: statusFactsStub{value: domain.StatusFacts{Security: "static-only", Space: "unknown"}},
		Credentials: &testkit.CredentialMetadataReader{Metadata: []domain.CredentialMetadata{metadata}},
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	handler := &rpcHandler{cli: program, loaded: loaded}
	var snapshotEvent string
	var snapshotEventRevision uint64
	result, err := handler.Handle(context.Background(), rpc.Call{Method: "system.resync"}, func(event string, _ any) (uint64, error) {
		snapshotEvent, snapshotEventRevision = event, 7
		return snapshotEventRevision, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, ok := result.(rpcSnapshot)
	if !ok || snapshot.Revision == 0 || len(snapshot.Projects.Projects) != 1 ||
		len(snapshot.Credentials) != 1 || len(snapshot.CredentialStatus.Credentials) != 1 ||
		snapshot.Status.ProjectCount != 1 {
		t.Fatalf("unexpected typed snapshot: %#v", result)
	}
	if snapshotEvent != "snapshot.ready" || snapshotEventRevision != snapshot.Revision {
		t.Fatalf("snapshot event is not correlated: event=%q revision=%d", snapshotEvent, snapshotEventRevision)
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"payload", "privateKey", "password"} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("snapshot contains secret-bearing field %q: %s", forbidden, payload)
		}
	}

	client, server := net.Pipe()
	sessionDone := make(chan error, 1)
	go func() {
		sessionDone <- (rpc.Session{Handler: handler}).Serve(context.Background(), server, server)
	}()
	codec := rpc.NewCodec(client, client)
	write := func(request rpc.Request) {
		t.Helper()
		if err := codec.Write(request); err != nil {
			t.Fatal(err)
		}
	}
	read := func() rpc.Response {
		t.Helper()
		response, err := codec.ReadResponse()
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	write(rpc.Request{Version: 1, Type: "request", ID: "negotiate", Method: "rpc.negotiate"})
	if response := read(); response.Error != nil {
		t.Fatalf("RPC negotiation failed: %#v", response)
	}
	write(rpc.Request{Version: 1, Type: "request", ID: "resync", Method: "system.resync"})
	resyncEvent := read()
	resyncResponse := read()
	snapshotResult, ok := resyncResponse.Result.(map[string]any)
	if resyncEvent.Type != "event" || resyncEvent.Event != "snapshot.ready" ||
		resyncEvent.Sequence != 1 || resyncEvent.Revision != 1 || resyncResponse.Error != nil || !ok {
		t.Fatalf("unexpected resync exchange: event=%#v response=%#v", resyncEvent, resyncResponse)
	}
	for _, field := range []string{
		"revision", "context", "commands", "projects", "status", "credentials", "credentialStatus",
	} {
		if _, present := snapshotResult[field]; !present {
			t.Fatalf("full resync snapshot omitted %q: %#v", field, snapshotResult)
		}
	}
	if revision, ok := snapshotResult["revision"].(float64); !ok || revision != 1 {
		t.Fatalf("snapshot revision is not bound to snapshot.ready: %#v", snapshotResult["revision"])
	}
	write(rpc.Request{Version: 1, Type: "request", ID: "ping", Method: "system.ping"})
	started := read()
	finished := read()
	pingResponse := read()
	if started.Event != "operation.started" || started.Sequence != 2 || started.Revision != 2 ||
		finished.Event != "operation.finished" || finished.Sequence != 3 || finished.Revision != 3 ||
		pingResponse.Error != nil {
		t.Fatalf("session event stream did not continue monotonically: %#v %#v %#v",
			started, finished, pingResponse)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-sessionDone; err != nil {
		t.Fatal(err)
	}

	if _, err := handler.Handle(context.Background(), rpc.Call{
		Method: "project.list", Params: json.RawMessage(`{"unknown":true}`),
	}, func(string, any) (uint64, error) { return 1, nil }); err == nil {
		t.Fatal("unknown RPC params were accepted")
	}
	route, err := handler.Handle(context.Background(), rpc.Call{
		ID: "request-route", OperationID: "operation-route", Method: "operation.route",
		Params: json.RawMessage(`{"command":"status"}`),
	}, func(string, any) (uint64, error) { return 1, nil })
	if err != nil || route.(map[string]any)["target"] != domain.TargetLocalOwner ||
		route.(map[string]any)["operationId"] != "operation-route" {
		t.Fatalf("typed operation route failed: %#v %v", route, err)
	}
}

func TestRPCIncusEventsAreTypedAndBoundToCancellation(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	events := make(chan domain.OperationEvent, 1)
	errorsOut := make(chan error)
	events <- domain.OperationEvent{
		OperationID: "incus-op", Sequence: 1, Revision: 7, Kind: "lifecycle",
		Data: map[string]any{"action": "instance-started"},
	}
	close(events)
	close(errorsOut)
	fakeIncus := &testkit.Incus{EventsOut: events, ErrorsOut: errorsOut}
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
		Incus: fakeIncus, Executor: fakeIncus,
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	var emitted domain.OperationEvent
	result, err := (&rpcHandler{cli: program, loaded: loaded}).Handle(
		context.Background(), rpc.Call{ID: "events", Method: "incus.events"},
		func(event string, data any) (uint64, error) {
			if event != "incus.lifecycle" {
				t.Fatalf("unexpected event envelope: %s", event)
			}
			emitted = data.(domain.OperationEvent)
			return 1, nil
		},
	)
	if err != nil || emitted.OperationID != "incus-op" || result.(map[string]any)["closed"] != true {
		t.Fatalf("typed Incus event stream failed: event=%#v result=%#v err=%v", emitted, result, err)
	}
}

func TestStructuredStartSharesPlanAndAdapterAcrossCLIAndRPC(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	clock := testkit.NewManualClock(time.Unix(100, 0))
	cliRunner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{
		Schema: 1, OperationID: "operation-cli", Status: "ok",
	}}}}
	prompt := &testkit.Prompt{}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"start"},
		Environment: append(environment, "SUBYARD_OPERATION_ID=operation-cli"),
		WorkingDir:  root,
		Stdout:      &stdout, Stderr: &stderr, AdapterRunner: cliRunner, Prompt: prompt, Clock: clock,
		Incus: lifecycleIncus(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("structured CLI start failed: code=%d stderr=%q", code, stderr.String())
	}
	if len(prompt.Seen) != 0 || strings.Contains(stderr.String(), "Proceed?") || len(cliRunner.Requests) != 1 ||
		cliRunner.Requests[0].Adapter != "lifecycle" || cliRunner.Requests[0].Action != "start" ||
		!slices.Equal(cliRunner.Requests[0].Arguments, []string{"start"}) ||
		cliRunner.Requests[0].Context["SUBYARD_CONFIG_LOADED"] != "1" ||
		cliRunner.Requests[0].Context["SUBYARD_SUDO_PREAUTHORIZED"] != "" {
		t.Fatalf("CLI bypassed the structured operation: prompt=%#v requests=%#v", prompt.Seen, cliRunner.Requests)
	}

	rpcRunner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{
		Schema: 1, OperationID: "operation-rpc", Status: "ok",
	}}}}
	program, err = New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
		Stderr: &stderr, AdapterRunner: rpcRunner, Clock: clock, Incus: lifecycleIncus(),
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]rpcPlannedOperation)}
	planResult, err := handler.Handle(context.Background(), rpc.Call{
		ID: "plan", OperationID: "operation-rpc", Method: "operation.plan",
		Params: json.RawMessage(`{"command":"start","arguments":[]}`),
	}, func(string, any) (uint64, error) { return 1, nil })
	if err != nil {
		t.Fatal(err)
	}
	plan := planResult.(domain.OperationPlan)
	if !plan.Confirmed || plan.Confirmation != domain.ConfirmationNever ||
		plan.Effect != domain.CommandMutate || len(plan.Consequences) != 3 {
		t.Fatalf("RPC returned an invalid plan: %#v", plan)
	}
	if _, err := handler.Handle(context.Background(), rpc.Call{
		ID: "execute-refused", OperationID: "operation-rpc", Method: "operation.execute",
		Params: json.RawMessage(`{"confirmed":false}`),
	}, func(string, any) (uint64, error) { return 1, nil }); err == nil || err.(*rpc.Error).Code != "confirmation_required" {
		t.Fatalf("RPC accepted an unconfirmed mutation: %v", err)
	}
	events := make([]string, 0, 2)
	result, err := handler.Handle(context.Background(), rpc.Call{
		ID: "execute", OperationID: "operation-rpc", Method: "operation.execute",
		Params: json.RawMessage(`{"confirmed":true}`),
	}, func(event string, _ any) (uint64, error) {
		events = append(events, event)
		return uint64(len(events)), nil
	})
	if err != nil || result.(map[string]any)["result"].(domain.AdapterResult).Status != "ok" ||
		len(events) != 2 || events[0] != "operation.started" || events[1] != "operation.finished" ||
		len(rpcRunner.Requests) != 1 {
		t.Fatalf("RPC execution bypassed orchestration: result=%#v events=%#v requests=%#v err=%v",
			result, events, rpcRunner.Requests, err)
	}
	if _, err := handler.Handle(context.Background(), rpc.Call{
		ID: "execute-replay", OperationID: "operation-rpc", Method: "operation.execute",
		Params: json.RawMessage(`{"confirmed":true}`),
	}, func(string, any) (uint64, error) { return 1, nil }); err == nil || err.(*rpc.Error).Code != "plan_not_found" {
		t.Fatalf("RPC replayed an already executed plan: %v", err)
	}
}

func TestUpdateUsesThePreparedReleaseAcrossRPCPlanAndExecute(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	assets := filepath.Join(root, "release")
	cache := filepath.Join(root, "cache")
	runtimeRoot := filepath.Join(root, "runtime")
	capture := filepath.Join(root, "installer.args")
	if err := os.MkdirAll(assets, 0o700); err != nil {
		t.Fatal(err)
	}
	name := "subyard-1.2.3-linux-" + runtime.GOARCH + ".tar.gz"
	for _, suffix := range []string{"", ".sha256", ".manifest.json", ".provenance.json"} {
		writeCLIFile(t, filepath.Join(assets, name+suffix), "fixture", 0o600)
	}
	writeCLIFile(t, filepath.Join(root, "scripts", "install-runtime-release.sh"),
		"#!/bin/sh\nset -eu\nprintf '%s\\n' \"$@\" > \"$RELEASE_CAPTURE\"\n", 0o700)
	environment = append(environment,
		"YARD_RELEASE_BASE_URL=file://"+assets,
		"YARD_RELEASE_CACHE="+cache,
		"RELEASE_CAPTURE="+capture,
	)
	configApplier := &recordingConfigApplier{}
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment,
		Clock: testkit.NewManualClock(time.Unix(100, 0)), Config: configApplier,
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
		"command": "update", "arguments": []string{
			"--version", "1.2.3", "--runtime-root", runtimeRoot,
		},
	})
	result, err := handler.Handle(context.Background(), rpc.Call{
		Method: "operation.plan", OperationID: "operation-update", Params: params,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan := result.(domain.OperationPlan)
	if plan.Effect != domain.CommandMutate || plan.Confirmation != domain.ConfirmationPromptDefaultYes ||
		plan.Assessment == nil || plan.Assessment.Action != "update.activate" ||
		plan.ConfirmationRequest == nil || plan.ConfirmationRequest.Default != domain.ConfirmationDefaultYes ||
		!strings.Contains(strings.Join(plan.Consequences, " "), "1.2.3") {
		t.Fatalf("release plan lost prepared evidence: %#v", plan)
	}
	execute, _ := json.Marshal(map[string]bool{"confirmed": true})
	events := make([]string, 0, 2)
	if _, err := handler.Handle(context.Background(), rpc.Call{
		Method: "operation.execute", OperationID: plan.OperationID, Params: execute,
	}, func(event string, _ any) (uint64, error) {
		events = append(events, event)
		return uint64(len(events)), nil
	}); err != nil {
		t.Fatal(err)
	}
	arguments, err := os.ReadFile(capture)
	if err != nil || slices.Contains(strings.Fields(string(arguments)), "--check") ||
		!slices.Equal(events, []string{"operation.started", "operation.finished"}) ||
		!slices.Equal(configApplier.yards, []string{"default"}) {
		t.Fatalf("release RPC bypassed its prepared operation: args=%q events=%q configs=%q err=%v",
			arguments, events, configApplier.yards, err)
	}
}

func TestUpdateTypedConfirmationSeparatesCheckActivationAndRollbackPreflight(t *testing.T) {
	t.Run("check is bounded and never prompts or activates", func(t *testing.T) {
		root, environment, runtimeRoot := updateReleaseFixture(t)
		capture := filepath.Join(root, "check-installer.args")
		writeCLIFile(t, filepath.Join(root, "scripts", "install-runtime-release.sh"),
			"#!/bin/sh\nset -eu\nprintf '%s\\n' \"$@\" > \"$CHECK_CAPTURE\"\n", 0o700)
		environment = append(environment, "CHECK_CAPTURE="+capture)
		prompt := &testkit.Prompt{}
		configs := &recordingConfigApplier{}
		var stderr bytes.Buffer
		program, err := New(Options{
			RepositoryRoot: root, Program: "yard",
			Arguments:   []string{"update", "--check", "--version", "1.2.3", "--runtime-root", runtimeRoot},
			Environment: environment, WorkingDir: root, Prompt: prompt, Config: configs,
			Stdout: &bytes.Buffer{}, Stderr: &stderr,
		})
		if err != nil {
			t.Fatal(err)
		}
		if code := program.Run(context.Background()); code != 0 {
			t.Fatalf("code=%d stderr=%q", code, stderr.String())
		}
		if len(prompt.Requests) != 0 || len(configs.yards) != 0 {
			t.Fatalf("check prompted or refreshed: prompt=%#v configs=%#v", prompt.Requests, configs.yards)
		}
		arguments, err := os.ReadFile(capture)
		if err != nil || !slices.Contains(strings.Fields(string(arguments)), "--check") {
			t.Fatalf("check installer args=%q err=%v", arguments, err)
		}
		if _, err := os.Lstat(filepath.Join(runtimeRoot, "current")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("check activated runtime: %v", err)
		}
	})

	t.Run("declined activation does not download install or refresh", func(t *testing.T) {
		root, environment, runtimeRoot := updateReleaseFixture(t)
		prompt := &testkit.Prompt{Answers: []bool{false}}
		configs := &recordingConfigApplier{}
		var stderr bytes.Buffer
		program, err := New(Options{
			RepositoryRoot: root, Program: "yard",
			Arguments:   []string{"update", "--version", "1.2.3", "--runtime-root", runtimeRoot},
			Environment: environment, WorkingDir: root, Prompt: prompt, Config: configs,
			Stdout: &bytes.Buffer{}, Stderr: &stderr,
		})
		if err != nil {
			t.Fatal(err)
		}
		if code := program.Run(context.Background()); code != 1 || len(prompt.Requests) != 1 ||
			prompt.Requests[0].Default != domain.ConfirmationDefaultYes {
			t.Fatalf("code=%d prompt=%#v stderr=%q", code, prompt.Requests, stderr.String())
		}
		cache := environmentValue(environment, "YARD_RELEASE_CACHE")
		if _, err := os.Stat(cache); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("decline wrote release cache: %v", err)
		}
		if _, err := os.Stat(runtimeRoot); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("decline wrote runtime root: %v", err)
		}
		if len(configs.yards) != 0 {
			t.Fatalf("decline refreshed configs: %#v", configs.yards)
		}
	})

	for _, assumeYes := range []bool{false, true} {
		t.Run(fmt.Sprintf("rollback precondition yes=%v", assumeYes), func(t *testing.T) {
			root, environment, _ := updateReleaseFixture(t)
			arguments := []string{"update", "--rollback", "--runtime-root", filepath.Join(root, "missing-runtime")}
			if assumeYes {
				arguments = append([]string{"update", "--yes"}, arguments[1:]...)
			}
			prompt := &testkit.Prompt{}
			var stderr bytes.Buffer
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard", Arguments: arguments,
				Environment: environment, WorkingDir: root, Prompt: prompt, Stderr: &stderr,
			})
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code != 1 ||
				!strings.Contains(stderr.String(), "previous") || len(prompt.Requests) != 0 {
				t.Fatalf("code=%d prompt=%#v stderr=%q", code, prompt.Requests, stderr.String())
			}
		})
	}
}

func TestUpdateRefreshesConfigsWithActivatedRuntimeLauncher(t *testing.T) {
	for _, test := range []struct {
		name                                                string
		arguments                                           func(string) []string
		defaultRoot, sameVersion, rollback, inheritedEngine bool
	}{
		{
			name: "default-root update", arguments: func(string) []string {
				return []string{"update", "--yes", "--version", "1.2.3"}
			}, defaultRoot: true,
		},
		{
			name: "explicit-root update", arguments: func(root string) []string {
				return []string{"update", "--yes", "--version", "1.2.3", "--runtime-root", root}
			},
		},
		{
			name: "explicit-root same-version", arguments: func(root string) []string {
				return []string{"update", "--yes", "--version", "1.2.3", "--runtime-root", root}
			}, sameVersion: true,
		},
		{
			name: "explicit-root rollback", arguments: func(root string) []string {
				return []string{"update", "--yes", "--runtime-root", root, "--rollback"}
			}, rollback: true,
		},
		{
			name: "explicit-root ignores inherited engine", arguments: func(root string) []string {
				return []string{"update", "--yes", "--version", "1.2.3", "--runtime-root", root}
			}, inheritedEngine: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, environment, runtimeRoot := updateReleaseFixture(t)
			if test.defaultRoot {
				runtimeRoot = filepath.Join(root, "data", "runtime")
			}
			if test.sameVersion {
				prepareCLIReleaseLinks(t, runtimeRoot, false)
			}
			if test.rollback {
				prepareCLIReleaseLinks(t, runtimeRoot, true)
			}
			oldDispatcher := filepath.Join(root, "old-dispatcher")
			oldLog := filepath.Join(root, "old-dispatcher.log")
			activeLog := filepath.Join(root, "active-launcher.log")
			writeCLIFile(t, oldDispatcher, "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$OLD_DISPATCHER_LOG\"\nexit 97\n", 0o700)
			environment = append(environment, "OLD_DISPATCHER_LOG="+oldLog, "ACTIVE_CAPTURE="+activeLog)
			if test.inheritedEngine {
				environment = append(environment, "YARD_ENGINE_PATH="+oldDispatcher)
			}

			program, err := New(Options{
				RepositoryRoot: root, DispatcherPath: oldDispatcher, Program: "yard",
				Arguments: test.arguments(runtimeRoot), Environment: environment, WorkingDir: root,
			})
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code != 0 {
				t.Fatalf("update failed: code=%d", code)
			}
			activeArguments, err := os.ReadFile(activeLog)
			if err != nil || string(activeArguments) != "config apply --yes\n" {
				t.Fatalf("active launcher arguments = %q, err=%v", activeArguments, err)
			}
			if payload, err := os.ReadFile(oldLog); err == nil {
				t.Fatalf("old dispatcher refreshed configs: %q", payload)
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
		})
	}
}

func prepareCLIReleaseLinks(t *testing.T, root string, withPrevious bool) {
	t.Helper()
	names := []string{"current-a"}
	if withPrevious {
		names = append(names, "previous-b")
	}
	for _, name := range names {
		engine := filepath.Join(root, "releases", name, "bin", "yard-engine")
		if err := os.MkdirAll(filepath.Dir(engine), 0o700); err != nil {
			t.Fatal(err)
		}
		writeCLIFile(t, engine, "#!/bin/sh\nprintf 'yard 1.2.3\\n'\n", 0o700)
	}
	if err := os.Symlink("releases/current-a", filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	if withPrevious {
		if err := os.Symlink("releases/previous-b", filepath.Join(root, "previous")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUpdateRefreshesAfterOperationFinishedRecordError(t *testing.T) {
	root, environment, runtimeRoot := updateReleaseFixture(t)
	oldDispatcher := filepath.Join(root, "old-dispatcher")
	oldLog := filepath.Join(root, "old-dispatcher.log")
	activeLog := filepath.Join(root, "active-launcher.log")
	writeCLIFile(t, oldDispatcher, "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$OLD_DISPATCHER_LOG\"\nexit 97\n", 0o700)
	environment = append(environment, "OLD_DISPATCHER_LOG="+oldLog, "ACTIVE_CAPTURE="+activeLog)
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, DispatcherPath: oldDispatcher, Program: "yard",
		Arguments:   []string{"update", "--yes", "--version", "1.2.3", "--runtime-root", runtimeRoot},
		Environment: environment, WorkingDir: root, Stderr: &stderr,
		Audit: &finishedOperationAudit{err: errors.New("operation finished sentinel")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 ||
		!strings.Contains(stderr.String(), "operation finished sentinel") {
		t.Fatalf("update recording failure = code=%d stderr=%q", code, stderr.String())
	}
	activeArguments, err := os.ReadFile(activeLog)
	if err != nil || string(activeArguments) != "config apply --yes\n" {
		t.Fatalf("active launcher did not refresh after record failure: %q, err=%v", activeArguments, err)
	}
	if payload, err := os.ReadFile(oldLog); err == nil {
		t.Fatalf("old dispatcher refreshed configs: %q", payload)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestUpdateJoinsRefreshAndOperationFinishedErrors(t *testing.T) {
	root, environment, runtimeRoot := updateReleaseFixture(t)
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"update", "--yes", "--version", "1.2.3", "--runtime-root", runtimeRoot},
		Environment: environment, WorkingDir: root, Stderr: &stderr,
		Audit:  &finishedOperationAudit{err: errors.New("operation finished sentinel")},
		Config: failingConfigApplier{err: errors.New("config applier sentinel")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 ||
		!strings.Contains(stderr.String(), "operation finished sentinel") ||
		!strings.Contains(stderr.String(), "config applier sentinel") {
		t.Fatalf("update combined failure = code=%d stderr=%q", code, stderr.String())
	}
}

func TestUpdateReportsActivatedRuntimeWhenConfigRefreshFails(t *testing.T) {
	for _, test := range []struct {
		name, yard, retry string
	}{
		{name: "default", yard: "default", retry: "yard config apply"},
		{name: "named", yard: "named", retry: "yard -Y named config apply"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, environment, runtimeRoot := updateReleaseFixture(t)
			if test.yard != "default" {
				path := filepath.Join(
					environmentValue(environment, "SUBYARD_CONFIG_HOME"), "yards", test.yard, "config.env",
				)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				writeCLIFile(t, path, "", 0o600)
			}
			var stderr bytes.Buffer
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard",
				Arguments:   []string{"-Y", test.yard, "update", "--yes", "--version", "1.2.3", "--runtime-root", runtimeRoot},
				Environment: append(environment, "PRIVATE_CONFIG_VALUE=must-not-appear"), WorkingDir: root,
				Config: failingConfigApplier{err: errors.New("config applier sentinel")}, Stderr: &stderr,
			})
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code != 1 ||
				!strings.Contains(stderr.String(), "runtime activation completed") ||
				!strings.Contains(stderr.String(), test.retry) ||
				strings.Contains(stderr.String(), "must-not-appear") {
				t.Fatalf("update refresh failure = code=%d stderr=%q", code, stderr.String())
			}
		})
	}
}

func TestUpdateRefreshAcceptsReadableConfigAndRejectsWritableConfigAfterActivation(t *testing.T) {
	for _, test := range []struct {
		name     string
		mode     os.FileMode
		wantCode int
		wantErr  string
	}{
		{name: "group readable", mode: 0o640},
		{name: "group writable", mode: 0o660, wantCode: 1, wantErr: "group/world writable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, environment, runtimeRoot := updateReleaseFixture(t)
			configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
			yardDirectory := filepath.Join(configHome, "yards", "hermes")
			if err := os.MkdirAll(yardDirectory, 0o755); err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(yardDirectory, "config.env")
			writeCLIFile(t, configPath, "SSH_PORT=2224\n", test.mode)
			if err := os.Chmod(configPath, test.mode); err != nil {
				t.Fatal(err)
			}

			var stderr bytes.Buffer
			applier := &validatingConfigApplier{configHome: configHome}
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard",
				Arguments: []string{
					"-Y", "hermes", "update", "--yes", "--version", "1.2.3",
					"--runtime-root", runtimeRoot,
				},
				Environment: environment, WorkingDir: root, Config: applier, Stderr: &stderr,
			})
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code != test.wantCode ||
				(test.wantErr != "" && !strings.Contains(stderr.String(), test.wantErr)) {
				t.Fatalf("update mode %04o: code=%d stderr=%q", test.mode, code, stderr.String())
			}
			if _, err := os.Lstat(filepath.Join(runtimeRoot, "current", "bin", "yard")); err != nil {
				t.Fatalf("runtime was not activated before refresh: %v", err)
			}
			if test.wantCode == 0 && !slices.Equal(applier.yards, []string{"hermes"}) {
				t.Fatalf("refreshed yards = %#v", applier.yards)
			}
		})
	}
}

type validatingConfigApplier struct {
	configHome string
	yards      []string
}

func (applier *validatingConfigApplier) ApplyConfig(_ context.Context, yard string) error {
	if err := validateManagedConfigTree(applier.configHome); err != nil {
		return err
	}
	applier.yards = append(applier.yards, yard)
	return nil
}

type failingConfigApplier struct{ err error }

func (applier failingConfigApplier) ApplyConfig(context.Context, string) error { return applier.err }

type finishedOperationAudit struct {
	err    error
	events []domain.OperationEvent
}

func (sink *finishedOperationAudit) WriteAudit(_ context.Context, event domain.OperationEvent) error {
	sink.events = append(sink.events, event)
	if event.Kind == "operation.finished" {
		return sink.err
	}
	return nil
}

func updateReleaseFixture(t *testing.T) (string, []string, string) {
	t.Helper()
	root, environment, _ := nativeFixture(t)
	assets := filepath.Join(root, "release")
	cache := filepath.Join(root, "cache")
	runtimeRoot := filepath.Join(root, "runtime")
	if err := os.MkdirAll(assets, 0o700); err != nil {
		t.Fatal(err)
	}
	name := "subyard-1.2.3-linux-" + runtime.GOARCH + ".tar.gz"
	for _, suffix := range []string{"", ".sha256", ".manifest.json", ".provenance.json"} {
		writeCLIFile(t, filepath.Join(assets, name+suffix), "fixture", 0o600)
	}
	writeCLIFile(t, filepath.Join(root, "scripts", "install-runtime-release.sh"), `#!/bin/sh
set -eu
while [ "$#" -gt 0 ]; do
	case "$1" in
	--runtime-root) root="$2"; shift 2 ;;
	*) shift ;;
	esac
done
mkdir -p "$root/current/bin"
printf '%s\n' '#!/bin/sh' 'if [ -n "${YARD_ENGINE_PATH:-}" ]; then exec "$YARD_ENGINE_PATH" "$@"; fi' 'printf "%s\\n" "$*" >> "$ACTIVE_CAPTURE"' > "$root/current/bin/yard"
chmod 700 "$root/current/bin/yard"
`, 0o700)
	return root, append(environment,
		"YARD_RELEASE_BASE_URL=file://"+assets,
		"YARD_RELEASE_CACHE="+cache,
	), runtimeRoot
}

func TestNetworkManagerPrivilegesAuthorizeBeforeBoundedAdapter(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "sudo.log")
	writeCLIFile(t, filepath.Join(bin, "systemctl"), "#!/bin/sh\nprintf 'active\\n'\n", 0o700)
	writeCLIFile(t, filepath.Join(bin, "sudo"), `#!/bin/sh
printf '%s\n' "$*" >> "$SUDO_LOG"
if [ "$*" = "-n true" ]; then
	[ -f "$SUDO_LOG.auth" ] || exit "${SUDO_NONINTERACTIVE_RC:-0}"
	exit 0
fi
if [ "$*" = "-v" ]; then
	: > "$SUDO_LOG.auth"
fi
`, 0o700)
	t.Setenv("PATH", bin)
	environment = append(environment, "PATH="+bin, "SUDO_LOG="+logPath, "SUDO_NONINTERACTIVE_RC=1")
	var diagnostics bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
		Stdin: strings.NewReader("operator input\n"), Stdout: &diagnostics, Stderr: &diagnostics,
	})
	if err != nil {
		t.Fatal(err)
	}
	program.operatorTerminal = func() bool { return true }
	if err := program.prepareNetworkManagerPrivileges(
		context.Background(), &diagnostics, 1000, "start",
	); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "-n true\n-v\n-n true\n" ||
		!strings.Contains(diagnostics.String(), "authorizing") {
		t.Fatalf("sudo authorization was not explicit: log=%q diagnostics=%q", payload, diagnostics.String())
	}

	writeCLIFile(t, filepath.Join(bin, "systemctl"), "#!/bin/sh\nprintf 'inactive\\n'\nexit 3\n", 0o700)
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	if err := program.prepareNetworkManagerPrivileges(
		context.Background(), &diagnostics, 1000, "start",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inactive NetworkManager invoked sudo: %v", err)
	}
}

func TestSudoPreauthorizationIsProcessLocal(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	program, err := New(Options{
		RepositoryRoot: root,
		Environment:    append(environment, "SUBYARD_SUDO_PREAUTHORIZED=1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if program.env["SUBYARD_SUDO_PREAUTHORIZED"] != "" ||
		program.baseEnv["SUBYARD_SUDO_PREAUTHORIZED"] != "" {
		t.Fatal("CLI inherited sudo preauthorization from another process")
	}
}

func TestRootStepPrivilegesAuthorizeBeforeBoundedAdapter(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "sudo.log")
	writeCLIFile(t, filepath.Join(bin, "sudo"), `#!/bin/sh
printf '%s\n' "$*" >> "$SUDO_LOG"
if [ "$*" = "-n true" ]; then
	[ -f "$SUDO_LOG.auth" ] || exit "${SUDO_NONINTERACTIVE_RC:-0}"
	exit 0
fi
if [ "$*" = "-v" ]; then
	: > "$SUDO_LOG.auth"
fi
IFS= read -r input
printf 'input=%s\n' "$input" >> "$SUDO_LOG"
`, 0o700)
	t.Setenv("PATH", bin)
	environment = append(environment, "PATH="+bin, "SUDO_LOG="+logPath, "SUDO_NONINTERACTIVE_RC=1")
	var diagnostics bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
		Stdin: strings.NewReader("operator-terminal-input\n"), Stdout: &diagnostics, Stderr: &diagnostics,
	})
	if err != nil {
		t.Fatal(err)
	}
	program.operatorTerminal = func() bool { return true }
	if err := program.prepareSudoPrivileges(
		context.Background(), &diagnostics, 1000, "teardown",
	); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "-n true\n-v\ninput=operator-terminal-input\n-n true\n" ||
		!strings.Contains(diagnostics.String(), "authorizing root steps for teardown") {
		t.Fatalf("root-step sudo authorization did not retain operator stdio: log=%q diagnostics=%q",
			payload, diagnostics.String())
	}
	if program.env["SUBYARD_SUDO_PREAUTHORIZED"] != "1" {
		t.Fatal("successful sudo authorization did not record its preauthorized context")
	}

	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	noninteractiveEnvironment := append([]string(nil), environment...)
	for index, value := range noninteractiveEnvironment {
		if value == "SUDO_NONINTERACTIVE_RC=1" {
			noninteractiveEnvironment[index] = "SUDO_NONINTERACTIVE_RC=0"
		}
	}
	noninteractive, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: noninteractiveEnvironment, WorkingDir: root,
		Stdin: strings.NewReader("must-not-be-read\n"), Stdout: &diagnostics, Stderr: &diagnostics,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := noninteractive.prepareSudoPrivileges(
		context.Background(), &diagnostics, 1000, "teardown",
	); err != nil {
		t.Fatal(err)
	}
	payload, err = os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "-n true\n" {
		t.Fatalf("passwordless sudo unexpectedly fell back to terminal authorization: %q", payload)
	}
	if noninteractive.env["SUBYARD_SUDO_PREAUTHORIZED"] != "1" {
		t.Fatal("passwordless sudo did not record its preauthorized context")
	}

	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	if err := program.prepareSudoPrivileges(
		context.Background(), &diagnostics, 0, "teardown",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("root execution invoked sudo: %v", err)
	}

	if err := os.Remove(logPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.Remove(logPath + ".auth"); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	blocked, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
		Stdin: strings.NewReader("must-not-be-read\n"), Stdout: &diagnostics, Stderr: &diagnostics,
	})
	if err != nil {
		t.Fatal(err)
	}
	blocked.operatorTerminal = func() bool { return false }
	if err := blocked.prepareSudoPrivileges(
		context.Background(), &diagnostics, 1000, "teardown",
	); err == nil || !strings.Contains(err.Error(), "operator terminal") {
		t.Fatalf("no-TTY sudo did not fail with an actionable diagnostic: %v", err)
	}
	payload, err = os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "-n true\n" {
		t.Fatalf("no-TTY sudo attempted an interactive authorization: %q", payload)
	}
	if blocked.env["SUBYARD_SUDO_PREAUTHORIZED"] == "1" {
		t.Fatal("failed sudo authorization recorded a false preauthorized context")
	}
}

func TestMigrationRootStepsAuthorizeThroughControllingTerminal(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "sudo.log")
	writeCLIFile(t, filepath.Join(bin, "sudo"), `#!/bin/sh
printf '%s\n' "$*" >> "$SUDO_LOG"
if [ "$*" = "-n true" ]; then
	[ -f "$SUDO_LOG.auth" ] || exit 1
	exit 0
fi
if [ "$*" = "-v" ]; then
	IFS= read -r input
	printf 'input=%s\n' "$input" >> "$SUDO_LOG"
	: > "$SUDO_LOG.auth"
fi
`, 0o700)
	terminalPath := filepath.Join(root, "terminal")
	writeCLIFile(t, terminalPath, "migration-terminal-input\n", 0o600)
	t.Setenv("PATH", bin)
	environment = append(
		environment,
		"PATH="+bin,
		"SUDO_LOG="+logPath,
		"SUBYARD_INTERNAL_MIGRATION_CHILD=1",
	)
	var diagnostics bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
		Stdin: strings.NewReader("closed migration stdin"), Stdout: &diagnostics, Stderr: &diagnostics,
	})
	if err != nil {
		t.Fatal(err)
	}
	program.operatorTerminal = func() bool { return false }
	program.openTerminal = func() (*os.File, error) {
		return os.OpenFile(terminalPath, os.O_RDWR, 0)
	}
	if err := program.prepareSudoPrivileges(
		context.Background(), &diagnostics, 1000, "teardown",
	); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) !=
		"-n true\n-v\ninput=migration-terminal-input\n-n true\n" {
		t.Fatalf("migration sudo did not use the controlling terminal: %q", payload)
	}
}

func TestStructuredMutationSharesTypedAdapterAcrossCLIAndRPC(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	clock := testkit.NewManualClock(time.Unix(100, 0))
	cliIncus := lifecycleIncus()
	cliInstance := cliIncus.Instances["subyard/yard"]
	cliInstance.Status = "Running"
	cliIncus.Instances["subyard/yard"] = cliInstance
	cliRunner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{
		Schema: 1, OperationID: "operation-stop-cli", Status: "ok",
	}}}}
	prompt := &testkit.Prompt{Answers: []bool{true}}
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"stop", "--force"},
		Environment: append(environment, "SUBYARD_OPERATION_ID=operation-stop-cli"), WorkingDir: root,
		Stderr: &stderr, AdapterRunner: cliRunner, Prompt: prompt, Clock: clock,
		Incus: cliIncus,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("structured CLI stop failed: code=%d stderr=%q", code, stderr.String())
	}
	if len(cliRunner.Requests) != 1 || cliRunner.Requests[0].Adapter != "lifecycle" ||
		cliRunner.Requests[0].Action != "stop" ||
		!slices.Equal(cliRunner.Requests[0].Arguments, []string{"stop", "--force"}) {
		t.Fatalf("CLI mutation bypassed the typed command adapter: %#v", cliRunner.Requests)
	}

	rpcRunner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{
		Schema: 1, OperationID: "operation-stop-rpc", Status: "ok",
	}}}}
	rpcIncus := lifecycleIncus()
	rpcInstance := rpcIncus.Instances["subyard/yard"]
	rpcInstance.Status = "Running"
	rpcIncus.Instances["subyard/yard"] = rpcInstance
	program, err = New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
		Stderr: &stderr, AdapterRunner: rpcRunner, Clock: clock, Incus: rpcIncus,
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]rpcPlannedOperation)}
	planResult, err := handler.Handle(context.Background(), rpc.Call{
		ID: "plan-stop", OperationID: "operation-stop-rpc", Method: "operation.plan",
		Params: json.RawMessage(`{"command":"stop","arguments":["--force"]}`),
	}, func(string, any) (uint64, error) { return 1, nil })
	if err != nil || planResult.(domain.OperationPlan).Command != "stop" {
		t.Fatalf("typed stop plan failed: %#v %v", planResult, err)
	}
	if _, err := handler.Handle(context.Background(), rpc.Call{
		ID: "execute-stop", OperationID: "operation-stop-rpc", Method: "operation.execute",
		Params: json.RawMessage(`{"confirmed":true}`),
	}, func(string, any) (uint64, error) { return 1, nil }); err != nil {
		t.Fatal(err)
	}
	if len(rpcRunner.Requests) != 1 || rpcRunner.Requests[0].Action != "stop" ||
		!slices.Equal(rpcRunner.Requests[0].Arguments, []string{"stop", "--force"}) {
		t.Fatalf("RPC mutation bypassed the typed command adapter: %#v", rpcRunner.Requests)
	}
}

func TestStructuredStoppedYardIsNoOpWithoutPromptOrAdapter(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	runner := &testkit.ScriptedAdapter{}
	prompt := &testkit.Prompt{}
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"stop"},
		Environment: append(environment, "SUBYARD_OPERATION_ID=operation-stop-noop"),
		WorkingDir:  root, Stderr: &stderr, AdapterRunner: runner, Prompt: prompt,
		Incus: lifecycleIncus(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("stopped-yard no-op failed: code=%d stderr=%q", code, stderr.String())
	}
	if len(prompt.Requests) != 0 || len(runner.Requests) != 0 {
		t.Fatalf("no-op prompted or applied: prompts=%#v requests=%#v", prompt.Requests, runner.Requests)
	}
}

func TestStructuredActionRechecksNoOpAfterConsent(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	incus := lifecycleIncus()
	instance := incus.Instances["subyard/yard"]
	instance.Status = "Running"
	incus.Instances["subyard/yard"] = instance
	runner := &testkit.ScriptedAdapter{}
	prompt := &callbackPrompt{callback: func() {
		current := incus.Instances["subyard/yard"]
		current.Status = "Stopped"
		incus.Instances["subyard/yard"] = current
	}}
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"stop"},
		Environment: append(environment, "SUBYARD_OPERATION_ID=operation-stop-recheck"),
		WorkingDir:  root, Stderr: &stderr, AdapterRunner: runner, Prompt: prompt, Incus: incus,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("post-consent no-op failed: code=%d stderr=%q", code, stderr.String())
	}
	if len(runner.Requests) != 0 {
		t.Fatalf("post-consent no-op was applied: %#v", runner.Requests)
	}
}

func TestStructuredStartAutomationSkipsOnlyTheLocalPrompt(t *testing.T) {
	for _, test := range []struct {
		name        string
		arguments   []string
		environment []string
	}{
		{name: "command option", arguments: []string{"start", "--yes"}},
		{name: "global option", arguments: []string{"--yes", "start"}},
		{name: "automation environment", arguments: []string{"start"}, environment: []string{"ASSUME_YES=1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			environment = append(environment, test.environment...)
			runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{
				Schema: 1, OperationID: "operation-automation", Status: "ok",
			}}}}
			prompt := &testkit.Prompt{}
			var stderr bytes.Buffer
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard", Arguments: test.arguments,
				Environment: append(environment, "SUBYARD_OPERATION_ID=operation-automation"),
				WorkingDir:  root, Stderr: &stderr, AdapterRunner: runner, Prompt: prompt,
				Incus: lifecycleIncus(),
			})
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code != 0 {
				t.Fatalf("automated start failed: code=%d stderr=%q", code, stderr.String())
			}
			if len(prompt.Seen) != 0 || len(runner.Requests) != 1 {
				t.Fatalf("automation did not bypass exactly the local prompt: prompt=%#v requests=%#v",
					prompt.Seen, runner.Requests)
			}
		})
	}
}

func TestProductionStartAdapterUsesGuardedShellHandler(t *testing.T) {
	fixtureRoot, environment, _ := nativeFixture(t)
	program, err := New(Options{
		RepositoryRoot: fixtureRoot, Program: "yard", Environment: environment, WorkingDir: fixtureRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	root := repositoryRoot(t)
	temporary := t.TempDir()
	bin := filepath.Join(temporary, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(temporary, "state")
	logPath := filepath.Join(temporary, "incus.log")
	writeCLIFile(t, statePath, "STOPPED\n", 0o600)
	writeCLIFile(t, logPath, "", 0o600)
	writeCLIFile(t, filepath.Join(bin, "incus"), fmt.Sprintf(`#!/bin/sh
set -eu
state=%q
log=%q
case "${1:-}" in
  info) exit 0 ;;
  list) cat "$state" ;;
  start) printf 'RUNNING\n' > "$state"; printf 'start\n' >> "$log" ;;
  exec) exit 0 ;;
  *) exit 90 ;;
esac
`, statePath, logPath), 0o700)
	writeCLIFile(t, filepath.Join(bin, "systemctl"), `#!/bin/sh
case "$*" in
  'is-active NetworkManager') printf 'inactive\n'; exit 3 ;;
esac
exit 90
`, 0o700)
	writeCLIFile(t, filepath.Join(bin, "ip"), "#!/bin/sh\nexit 0\n", 0o700)
	contextValues := structuredAdapterContext(loaded.Context)
	if contextValues["YARD_VERSION"] != Version {
		t.Fatalf("structured adapter context lost engine version: %#v", contextValues)
	}
	remoteContext := loaded.Context
	remoteContext.YardType = domain.YardRemote
	remoteContext.RemoteDest = "dev@owner.example"
	remoteContext.RemoteYard = "named"
	remoteValues := structuredAdapterContext(remoteContext)
	if remoteValues["REMOTE_DEST"] != remoteContext.RemoteDest || remoteValues["REMOTE_YARD"] != remoteContext.RemoteYard {
		t.Fatalf("structured adapter context lost remote route: %#v", remoteValues)
	}
	commandValues := structuredCommandContext(config.Loaded{Context: loaded.Context, Environment: map[string]string{
		"CCUSAGE_PROVISION":                "/config/agents/ccusage/provision.sh",
		"E2E_VM_SLOT_COUNT":                "2",
		"AGENT_codex_CONFIG":               "/config/agents/codex/config.toml",
		"HOST_OPENCODE_AGENTS_MD":          "/home/operator/.config/opencode/AGENTS.md",
		"YARD_RUNTIME_ROOT":                "/opt/subyard/runtime",
		"SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE": "1",
		"SUBYARD_CONFIG_SECRETS_DIR":       "/home/operator/.config/subyard/secrets",
		"AGENT_codex_TOKEN":                "must-not-cross",
		"AWS_SECRET_ACCESS_KEY":            "must-not-cross",
		"UNRELATED_AMBIENT_VALUE":          "must-not-cross",
	}})
	for name, expected := range map[string]string{
		"CCUSAGE_PROVISION":                "/config/agents/ccusage/provision.sh",
		"E2E_VM_SLOT_COUNT":                "2",
		"AGENT_codex_CONFIG":               "/config/agents/codex/config.toml",
		"HOST_OPENCODE_AGENTS_MD":          "/home/operator/.config/opencode/AGENTS.md",
		"YARD_RUNTIME_ROOT":                "/opt/subyard/runtime",
		"SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE": "1",
	} {
		if commandValues[name] != expected {
			t.Fatalf("structured command context lost %s: %#v", name, commandValues)
		}
	}
	for _, name := range []string{
		"SUBYARD_CONFIG_SECRETS_DIR", "AGENT_codex_TOKEN",
		"AWS_SECRET_ACCESS_KEY", "UNRELATED_AMBIENT_VALUE",
	} {
		if _, ok := commandValues[name]; ok {
			t.Fatalf("structured command context leaked %s", name)
		}
	}
	contextKeys := make(map[string]struct{}, len(contextValues))
	for key := range contextValues {
		contextKeys[key] = struct{}{}
	}
	physical := shelladapter.Runner{
		RepositoryRoot: root,
		Actions: map[string]map[string]shelladapter.Action{"lifecycle": {
			"start": {
				Path: filepath.Join(root, "scripts", "lifecycle-guard.sh"), Direct: true,
			},
		}},
		ContextKeys: contextKeys, Path: bin + ":/usr/sbin:/usr/bin:/sbin:/bin", Timeout: time.Second,
	}
	power := lifecycleIncus()
	runner := application.LifecycleRunner{
		Power:    application.PowerService{Instances: power, Config: power},
		Physical: physical, Yard: loaded.Context,
	}
	request := domain.AdapterRequest{
		Schema: 1, OperationID: "operation-production", Adapter: "lifecycle", Action: "start",
		Context: contextValues,
	}
	result, diagnostics, err := runner.Run(context.Background(), request, nil)
	if err != nil || result.Status != "ok" || !strings.Contains(diagnostics, "starting yard") {
		t.Fatalf("production adapter failed: result=%#v diagnostics=%q err=%v", result, diagnostics, err)
	}
	payload, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), "start\n") ||
		power.Instances["subyard/yard"].Config["user.subyard.desired_power"] != "running" {
		t.Fatalf("guarded start did not commit metadata: log=%s state=%#v", payload, power.Instances)
	}
}

func TestStructuredStartRunsOverFramedRPCSession(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{
		Schema: 1, OperationID: "operation-wire", Status: "ok",
	}}}}
	client, server := net.Pipe()
	defer client.Close()
	if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"rpc", "--stdio"},
		Environment: environment, WorkingDir: root, Stdin: server, Stdout: server, Stderr: &stderr,
		AdapterRunner: runner, Clock: testkit.NewManualClock(time.Unix(100, 0)),
		Incus: lifecycleIncus(),
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		done <- program.Run(context.Background())
		_ = server.Close()
	}()
	codec := rpc.NewCodec(client, client)
	if err := codec.Write(rpc.Request{Version: 1, Type: "request", ID: "negotiate", Method: "rpc.negotiate"}); err != nil {
		t.Fatal(err)
	}
	negotiated, err := codec.ReadResponse()
	if err != nil || negotiated.Error != nil {
		t.Fatalf("RPC negotiation failed: response=%#v err=%v", negotiated, err)
	}
	if err := codec.Write(rpc.Request{
		Version: 1, Type: "request", ID: "plan", OperationID: "operation-wire", Method: "operation.plan",
		Params: json.RawMessage(`{"command":"start","arguments":[]}`),
	}); err != nil {
		t.Fatal(err)
	}
	planned, err := codec.ReadResponse()
	if err != nil || planned.Error != nil || planned.OperationID != "operation-wire" {
		t.Fatalf("RPC plan failed: response=%#v err=%v", planned, err)
	}
	if err := codec.Write(rpc.Request{
		Version: 1, Type: "request", ID: "execute", OperationID: "operation-wire", Method: "operation.execute",
		Params: json.RawMessage(`{"confirmed":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	seenEvents := make([]string, 0, 2)
	var executed rpc.Response
	for executed.ID == "" {
		response, err := codec.ReadResponse()
		if err != nil {
			t.Fatal(err)
		}
		if response.Type == "event" {
			seenEvents = append(seenEvents, response.Event)
		} else {
			executed = response
		}
	}
	if executed.Error != nil || executed.OperationID != "operation-wire" ||
		len(seenEvents) != 2 || seenEvents[0] != "operation.started" || seenEvents[1] != "operation.finished" ||
		len(runner.Requests) != 1 {
		t.Fatalf("framed execution failed: response=%#v events=%#v requests=%#v stderr=%q",
			executed, seenEvents, runner.Requests, stderr.String())
	}
	_ = client.Close()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("RPC server exited %d: %s", code, stderr.String())
		}
	case <-time.After(time.Second):
		t.Fatal("RPC server did not stop after EOF")
	}
}

type projectObserverStub struct{ value domain.ProjectObservation }

func (stub projectObserverStub) Observe(
	context.Context,
	domain.Context,
	[]domain.ProjectRecord,
	bool,
) (domain.ProjectObservation, error) {
	return stub.value, nil
}

func TestNativeOwnerInfoUsesTypedContextAndLiveInventory(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	fakeIncus := &testkit.Incus{
		ServerInfo: ports.ServerInfo{Environment: "incus", Version: "6.23"},
		Instances:  map[string]ports.InstanceInfo{"subyard/yard": {Status: "Running"}},
	}
	observation := domain.ProjectObservation{Reached: true, Live: []domain.ProjectRecord{
		{ProjectID: "demo-12345678"}, {ProjectID: "demo-12345678"},
	}}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"_info"}, Environment: environment,
		WorkingDir: root, Stdout: &stdout, Stderr: &stderr, Incus: fakeIncus,
		ProjectObserver: projectObserverStub{value: observation},
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("_info failed: code=%d stderr=%q", code, stderr.String())
	}
	var info ownerInfo
	if err := json.Unmarshal(stdout.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.Name != "default" || info.State != "RUNNING" || info.SSHPort != 2222 ||
		info.Projects == nil || *info.Projects != 1 {
		t.Fatalf("unexpected owner info: %#v", info)
	}
}

func TestNativeAuthorizeValidatesAndWritesOneControllerKey(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	fakeIncus := &testkit.Incus{
		Instances: map[string]ports.InstanceInfo{"subyard/yard": {Status: "Running"}},
		ExecSteps: []testkit.IncusExecStep{{Result: ports.InstanceExecResult{
			Stdout: []byte("added"), ExitCode: 0,
		}}},
	}
	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA controller"
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"_authorize"}, Environment: environment,
		WorkingDir: root, Stdin: strings.NewReader(key + "\n"), Stderr: &stderr,
		Incus: fakeIncus, Executor: fakeIncus,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("_authorize failed: code=%d stderr=%q", code, stderr.String())
	}
	if len(fakeIncus.ExecCalls) != 1 || fakeIncus.ExecCalls[0].Request.Environment["PUBKEY"] != key ||
		fakeIncus.ExecCalls[0].Request.Environment["DEV_USER"] != "dev" {
		t.Fatalf("unexpected authorize request: %#v", fakeIncus.ExecCalls)
	}
}

func TestNativeLogArgumentsAreBounded(t *testing.T) {
	arguments, help, err := parseLogArguments([]string{"-n", "25", "docker"})
	if err != nil || help || !slices.Equal(arguments,
		[]string{"journalctl", "-n", "25", "-u", "docker", "--no-pager"}) {
		t.Fatalf("unexpected log arguments: %#v help=%v err=%v", arguments, help, err)
	}
	if _, _, err := parseLogArguments([]string{"-n", "0"}); err == nil {
		t.Fatal("logs accepted an invalid line count")
	}
}

func TestNativeStatusUsesTypedPortsAndRendersParityFields(t *testing.T) {
	root, environment, stateDirectory := nativeFixture(t)
	store, err := state.NewFileStore(stateDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), domain.ProjectRecord{
		Schema: 1, ProjectID: "demo-12345678", Name: "Demo", HostPath: "/host/Demo",
		YardPath: "/srv/workspaces/demo-12345678/src", Mode: domain.ProjectSync, SSHHost: "yard",
	}); err != nil {
		t.Fatal(err)
	}
	fakeIncus := &testkit.Incus{
		ServerInfo: ports.ServerInfo{Environment: "incus", Version: "6.23"},
		Instances: map[string]ports.InstanceInfo{"subyard/yard": {
			Name: "yard", Project: "subyard", Type: domain.InstanceContainer, Status: "Running",
			Config: map[string]string{
				"user.subyard.desired_power": "running", "user.subyard.initialized": "true",
				"boot.autostart": "false",
			},
			Devices: map[string]map[string]string{"ssh": {"type": "proxy"}, "host-demo": {"type": "disk"}},
		}},
		ExecSteps: []testkit.IncusExecStep{
			{Result: ports.InstanceExecResult{Stdout: []byte("10.0.0.2\n")}},
			{Result: ports.InstanceExecResult{Stdout: []byte(
				"services=active/active\nvscode=key=yes server=yes git-id=yes\n",
			)}},
		},
	}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"-Y", "default", "status"}, Environment: environment,
		WorkingDir: root, Stdout: &stdout, Stderr: &stderr, Incus: fakeIncus, Executor: fakeIncus,
		StatusFacts: statusFactsStub{value: domain.StatusFacts{
			Shared:   []domain.SharedResourceStatus{{Profile: "android", Name: "emulator", State: "up", Hint: "yard emu down"}},
			Security: "static-only", Space: "1G  (in-yard rootfs, 1s ago)",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("status failed: code=%d stderr=%q", code, stderr.String())
	}
	for _, expected := range []string{
		"yard  RUNNING", "desired  running", "ip       10.0.0.2", "host-demo",
		"services ssh/docker = active/active", "vscode   key=yes server=yes git-id=yes",
		"projects 1", "android   emulator", "security static-only", "space    1G",
	} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("status omitted %q:\n%s", expected, stdout.String())
		}
	}
}

func TestSpaceListsEveryLocalYardFromCache(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	dataHome := environmentValue(environment, "SUBYARD_HOME")
	if err := os.MkdirAll(filepath.Join(configHome, "yards"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataHome, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(configHome, "yards", "demo.env"), "SSH_PORT=3333\n", 0o600)
	writeCLIFile(t, filepath.Join(dataHome, "space.cache"), "1G 90\n", 0o600)
	writeCLIFile(t, filepath.Join(dataHome, "space-demo.cache"), "2G 80\n", 0o600)
	incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
		"subyard/yard":           {Status: "Running"},
		"subyard-demo/yard-demo": {Status: "Stopped"},
	}}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"space"},
		Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr,
		Incus: incus, Executor: incus, Clock: testkit.NewManualClock(time.Unix(100, 0)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("space failed: code=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	want := fmt.Sprintf("%-16s %-12s %8s %s\n", "YARD", "STATE", "USED", "MEASURED") +
		fmt.Sprintf("%-16s %-12s %8s %s\n", "default", "RUNNING", "1G", "10s ago") +
		fmt.Sprintf("%-16s %-12s %8s %s\n", "demo", "STOPPED", "2G", "20s ago")
	if stdout.String() != want || len(incus.ExecCalls) != 0 {
		t.Fatalf("space output = %q, want %q; exec=%#v", stdout.String(), want, incus.ExecCalls)
	}
}

func TestSpaceRefreshesOnlyRunningYards(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	dataHome := environmentValue(environment, "SUBYARD_HOME")
	if err := os.MkdirAll(filepath.Join(configHome, "yards"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataHome, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(configHome, "yards", "demo.env"), "SSH_PORT=3333\n", 0o600)
	writeCLIFile(t, filepath.Join(configHome, "yards", "new.env"), "SSH_PORT=4444\n", 0o600)
	writeCLIFile(t, filepath.Join(dataHome, "space.cache"), "1G 90\n", 0o600)
	writeCLIFile(t, filepath.Join(dataHome, "space-demo.cache"), "2G 80\n", 0o600)
	incus := &testkit.Incus{
		Instances: map[string]ports.InstanceInfo{
			"subyard/yard":           {Status: "Running"},
			"subyard-demo/yard-demo": {Status: "Stopped"},
		},
		ExecSteps: []testkit.IncusExecStep{{
			Result: ports.InstanceExecResult{Stdout: []byte("3G\n")},
		}},
	}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"space", "--refresh"},
		Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr,
		Incus: incus, Executor: incus, Clock: testkit.NewManualClock(time.Unix(100, 0)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("space refresh failed: code=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	for _, expected := range []string{
		fmt.Sprintf("%-16s %-12s %8s %s\n", "default", "RUNNING", "3G", "0s ago"),
		fmt.Sprintf("%-16s %-12s %8s %s\n", "demo", "STOPPED", "2G", "20s ago"),
		fmt.Sprintf("%-16s %-12s %8s %s\n", "new", "NOT_CREATED", "—", "never"),
	} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("space refresh omitted %q: %q", expected, stdout.String())
		}
	}
	if len(incus.ExecCalls) != 1 || incus.ExecCalls[0].Project != "subyard" ||
		incus.ExecCalls[0].Name != "yard" {
		t.Fatalf("space refreshed unexpected yards: %#v", incus.ExecCalls)
	}
}

func TestSpaceRefreshTimestampsEachCompletedMeasurement(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	dataHome := environmentValue(environment, "SUBYARD_HOME")
	if err := os.MkdirAll(dataHome, 0o700); err != nil {
		t.Fatal(err)
	}
	clock := testkit.NewManualClock(time.Unix(100, 0))
	incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
		"subyard/yard": {Status: "Running"},
	}}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"space", "--refresh"},
		Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr,
		Incus: incus,
		Executor: spaceAdvancingExecutor{
			clock: clock, result: ports.InstanceExecResult{Stdout: []byte("3G\n")},
		},
		Clock: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("space refresh failed: code=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	contents, readErr := os.ReadFile(filepath.Join(dataHome, "space.cache"))
	if readErr != nil || string(contents) != "3G 105\n" || !strings.Contains(stdout.String(), "3G 0s ago") {
		t.Fatalf("completion timestamp: cache=%q err=%v output=%q", contents, readErr, stdout.String())
	}
}

func TestSpaceRefreshContinuesAfterFailureAndPreservesCache(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	dataHome := environmentValue(environment, "SUBYARD_HOME")
	if err := os.MkdirAll(filepath.Join(configHome, "yards"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataHome, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(configHome, "yards", "demo.env"), "SSH_PORT=3333\n", 0o600)
	defaultCache := filepath.Join(dataHome, "space.cache")
	writeCLIFile(t, defaultCache, "1G 90\n", 0o600)
	writeCLIFile(t, filepath.Join(dataHome, "space-demo.cache"), "2G 80\n", 0o600)
	incus := &testkit.Incus{
		Instances: map[string]ports.InstanceInfo{
			"subyard/yard":           {Status: "Running"},
			"subyard-demo/yard-demo": {Status: "Running"},
		},
		ExecSteps: []testkit.IncusExecStep{
			{Err: errors.New("measurement failed")},
			{Result: ports.InstanceExecResult{Stdout: []byte("4G\n")}},
		},
	}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"space", "--refresh"},
		Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr,
		Incus: incus, Executor: incus, Clock: testkit.NewManualClock(time.Unix(100, 0)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 {
		t.Fatalf("space refresh code=%d, want 1; stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	for _, expected := range []string{
		fmt.Sprintf("%-16s %-12s %8s %s\n", "default", "RUNNING", "1G", "10s ago"),
		fmt.Sprintf("%-16s %-12s %8s %s\n", "demo", "RUNNING", "4G", "0s ago"),
	} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("space refresh omitted %q: %q", expected, stdout.String())
		}
	}
	if !strings.Contains(stderr.String(), "space: default: refresh:") || len(incus.ExecCalls) != 2 {
		t.Fatalf("space refresh did not report and continue: stderr=%q exec=%#v", stderr.String(), incus.ExecCalls)
	}
	contents, readErr := os.ReadFile(defaultCache)
	if readErr != nil || string(contents) != "1G 90\n" {
		t.Fatalf("failed refresh changed cache: contents=%q err=%v", contents, readErr)
	}
}

func TestSpaceRefreshReportsStateFailureAndContinues(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	dataHome := environmentValue(environment, "SUBYARD_HOME")
	if err := os.MkdirAll(filepath.Join(configHome, "yards"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataHome, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(configHome, "yards", "demo.env"), "SSH_PORT=3333\n", 0o600)
	writeCLIFile(t, filepath.Join(dataHome, "space.cache"), "1G 90\n", 0o600)
	writeCLIFile(t, filepath.Join(dataHome, "space-demo.cache"), "2G 80\n", 0o600)
	executor := &testkit.Incus{
		Instances: map[string]ports.InstanceInfo{
			"subyard-demo/yard-demo": {Status: "Running"},
		},
		ExecSteps: []testkit.IncusExecStep{{
			Result: ports.InstanceExecResult{Stdout: []byte("4G\n")},
		}},
	}
	incus := spaceIncusStub{
		Incus: executor,
		instanceErrors: map[string]error{
			"subyard/yard": errors.New("state unavailable"),
		},
	}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"space", "--refresh"},
		Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr,
		Incus: incus, Executor: executor, Clock: testkit.NewManualClock(time.Unix(100, 0)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 {
		t.Fatalf("space refresh code=%d, want 1; stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	for _, expected := range []string{
		fmt.Sprintf("%-16s %-12s %8s %s\n", "default", "UNKNOWN", "1G", "10s ago"),
		fmt.Sprintf("%-16s %-12s %8s %s\n", "demo", "RUNNING", "4G", "0s ago"),
	} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("space refresh omitted %q: %q", expected, stdout.String())
		}
	}
	if !strings.Contains(stderr.String(), "space: default: state: state unavailable") ||
		len(executor.ExecCalls) != 1 || executor.ExecCalls[0].Name != "yard-demo" {
		t.Fatalf("space refresh did not report and continue: stderr=%q exec=%#v", stderr.String(), executor.ExecCalls)
	}
}

func TestSpaceHonorsExplicitYardSelectors(t *testing.T) {
	for _, arguments := range [][]string{{"-Y", "demo", "space"}, {"@demo", "space"}} {
		t.Run(strings.Join(arguments, " "), func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
			dataHome := environmentValue(environment, "SUBYARD_HOME")
			if err := os.MkdirAll(filepath.Join(configHome, "yards"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(dataHome, 0o700); err != nil {
				t.Fatal(err)
			}
			writeCLIFile(t, filepath.Join(configHome, "yards", "demo.env"), "SSH_PORT=3333\n", 0o600)
			writeCLIFile(t, filepath.Join(dataHome, "space.cache"), "1G 90\n", 0o600)
			writeCLIFile(t, filepath.Join(dataHome, "space-demo.cache"), "2G 80\n", 0o600)
			incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
				"subyard/yard":           {Status: "Running"},
				"subyard-demo/yard-demo": {Status: "Stopped"},
			}}
			var stdout, stderr bytes.Buffer
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard", Arguments: arguments,
				Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr,
				Incus: incus, Executor: incus, Clock: testkit.NewManualClock(time.Unix(100, 0)),
			})
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code != 0 {
				t.Fatalf("selected space failed: code=%d stderr=%q", code, stderr.String())
			}
			want := fmt.Sprintf("%-16s %-12s %8s %s\n", "YARD", "STATE", "USED", "MEASURED") +
				fmt.Sprintf("%-16s %-12s %8s %s\n", "demo", "STOPPED", "2G", "20s ago")
			if stdout.String() != want {
				t.Fatalf("selected space output = %q, want %q", stdout.String(), want)
			}
		})
	}
}

func TestStatusRoutesSummaryAndExplicitYardSelectors(t *testing.T) {
	tests := []struct {
		name        string
		arguments   []string
		environment []string
		summary     bool
		instance    string
	}{
		{name: "implicit default", arguments: []string{"status"}, summary: true},
		{
			name: "inherited default", arguments: []string{"status"},
			environment: []string{"SUBYARD_YARD=default"}, summary: true,
		},
		{
			name: "explicit default", arguments: []string{"-Y", "default", "status"},
			instance: "yard",
		},
		{
			name: "explicit marker default", arguments: []string{"status"},
			environment: []string{"SUBYARD_YARD=default", "SUBYARD_YARD_EXPLICIT=1"},
			instance:    "yard",
		},
		{
			name: "explicit named", arguments: []string{"-Y", "demo", "status"},
			instance: "yard-demo",
		},
		{
			name: "at named", arguments: []string{"@demo", "status"},
			instance: "yard-demo",
		},
		{
			name: "canonical selector", arguments: []string{"-Y", "owner-a/demo", "status"},
			instance: "yard-demo",
		},
		{
			name: "at canonical selector", arguments: []string{"@owner-a/demo", "status"},
			instance: "yard-demo",
		},
		{
			name: "inherited named", arguments: []string{"status"},
			environment: []string{"SUBYARD_YARD=demo"}, instance: "yard-demo",
		},
		{
			name: "all overrides selector", arguments: []string{"-Y", "demo", "status", "--all"},
			summary: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
			if err := os.MkdirAll(filepath.Join(configHome, "yards"), 0o700); err != nil {
				t.Fatal(err)
			}
			writeCLIFile(t, filepath.Join(configHome, "yards", "demo.env"), "SSH_PORT=3333\n", 0o600)
			environment = append(environment, "SUBYARD_HOST_ID=owner-a")
			environment = append(environment, test.environment...)
			incus := &testkit.Incus{
				Instances: map[string]ports.InstanceInfo{
					"subyard/yard": {
						Name: "yard", Project: "subyard", Type: domain.InstanceContainer,
						Status: "Running", Config: map[string]string{}, Devices: map[string]map[string]string{},
					},
					"subyard-demo/yard-demo": {
						Name: "yard-demo", Project: "subyard-demo", Type: domain.InstanceContainer,
						Status: "Running", Config: map[string]string{}, Devices: map[string]map[string]string{},
					},
				},
				ExecSteps: make([]testkit.IncusExecStep, 8),
			}
			var stdout, stderr bytes.Buffer
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard", Arguments: test.arguments,
				Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr,
				Incus: incus, Executor: incus,
				StatusFacts: statusFactsStub{value: domain.StatusFacts{Security: "live", Space: "unknown"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code != 0 {
				t.Fatalf("status failed: code=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
			}
			if test.summary {
				if !strings.Contains(stdout.String(), "owner-a/default") ||
					!strings.Contains(stdout.String(), "owner-a/demo") ||
					len(incus.ExecCalls) != 0 {
					t.Fatalf("status did not use inventory summary:\n%s\nexec=%#v", stdout.String(), incus.ExecCalls)
				}
				return
			}
			if strings.Contains(stdout.String(), "owner-a/") || len(incus.ExecCalls) == 0 {
				t.Fatalf("status did not use detailed path:\n%s\nexec=%#v", stdout.String(), incus.ExecCalls)
			}
			for _, call := range incus.ExecCalls {
				if call.Name != test.instance {
					t.Fatalf("status probed %q, want %q: %#v", call.Name, test.instance, incus.ExecCalls)
				}
			}
		})
	}
}

func TestNativeLiveListDoesNotImportL1Metadata(t *testing.T) {
	root, environment, stateDirectory := nativeFixture(t)
	observation := domain.ProjectObservation{
		Reached: true,
		Live: []domain.ProjectRecord{{
			Schema: 1, ProjectID: "live-12345678", Name: "Live", Mode: domain.ProjectSync,
			YardPath: "/srv/workspaces/live-12345678/src", SSHHost: "yard", Target: "openclaw",
		}},
		Presence: map[string]domain.ProjectPresence{"live-12345678": domain.ProjectPresent},
		Boxes:    map[string]domain.ProjectBoxState{"live-12345678": domain.ProjectBoxNone},
	}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"list", "--live"},
		Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr,
		ProjectObserver: projectObserverStub{value: observation},
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("list failed: code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No projects in the selected owner inventory.") {
		t.Fatalf("unexpected list output:\n%s", stdout.String())
	}
	store, err := state.NewFileStore(stateDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), "live-12345678"); err == nil {
		t.Fatalf("controller imported L1 metadata into owner registry: err=%v", err)
	}
}

func TestNativeListRepairsLegacyProjectPermissions(t *testing.T) {
	root, environment, stateDirectory := nativeFixture(t)
	store, err := state.NewFileStore(stateDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), domain.ProjectRecord{
		Schema: 1, ProjectID: "legacy-12345678", Name: "Legacy", HostPath: "/host/Legacy",
		YardPath: "/srv/workspaces/legacy-12345678/src", Mode: domain.ProjectSync, SSHHost: "yard",
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(stateDirectory, "legacy-12345678.json")
	if err := os.Chmod(path, 0o664); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"list"}, Environment: environment,
		WorkingDir: root, Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("list failed: code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Legacy") {
		t.Fatalf("legacy project missing from list:\n%s", stdout.String())
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("legacy state mode = %o, want 600", info.Mode().Perm())
	}
}

func TestNativeListBoundsOnlyOwnerDisplay(t *testing.T) {
	root, environment, stateDirectory := nativeFixture(t)
	store, err := state.NewFileStore(stateDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), domain.ProjectRecord{
		Schema: 1, ProjectID: "demo-12345678", Name: "Demo", HostPath: "/host/Demo",
		YardPath: "/srv/workspaces/demo-12345678/src", Mode: domain.ProjectSync, SSHHost: "yard",
		Target: "yard",
	}); err != nil {
		t.Fatal(err)
	}
	const hostID = "5034c950-74d0-46c4-9428-b7835e602109"
	writeCLIFile(t, filepath.Join(filepath.Dir(stateDirectory), "host-id"), hostID+"\n", 0o600)
	fakeIncus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{}}

	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"list"}, Environment: environment,
		WorkingDir: root, Stdout: &stdout, Stderr: &stderr, Incus: fakeIncus,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("list failed: code=%d stderr=%q", code, stderr.String())
	}
	want := "" +
		"NAME                     MODE   TARGET     OWNER                YARD\n" +
		"Demo                     sync   yard       5034c950-74d0-46c... default\n"
	if stdout.String() != want {
		t.Fatalf("bounded list output drifted:\ngot:\n%swant:\n%s", stdout.String(), want)
	}

	stdout.Reset()
	stderr.Reset()
	program, err = New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments: []string{"list", "--complete-projects"}, Environment: environment,
		WorkingDir: root, Stdout: &stdout, Stderr: &stderr, Incus: fakeIncus,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("project completion failed: code=%d stderr=%q", code, stderr.String())
	}
	if want := "Demo\nDemo/" + hostID + "\n"; stdout.String() != want {
		t.Fatalf("completion changed owner identity: got %q, want %q", stdout.String(), want)
	}
}

func TestProjectAdaptersReceiveGoResolvedSnapshotAndGoCommitsState(t *testing.T) {
	root, environment, stateDirectory := nativeFixture(t)
	projectPath := filepath.Join(root, "Demo")
	if err := os.MkdirAll(projectPath, 0o700); err != nil {
		t.Fatal(err)
	}
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	execution, err := program.prepareProjectImport(
		context.Background(), loaded, "sync", []string{projectPath, "--target", "yard"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if execution.Commit != projectCommitPut || execution.Environment["SUBYARD_PROJECT_SNAPSHOT"] != "1" ||
		execution.Environment["SUBYARD_PROJECT_HOST_PATH"] != projectPath ||
		execution.Environment["SUBYARD_PROJECT_YARD_PATH"] != state.YardPath(execution.Record.ProjectID) ||
		execution.Record.ProjectID != "Demo" || execution.Record.Name != "Demo" ||
		execution.Record.IdentityVersion != 2 || execution.Reservation != nil {
		t.Fatalf("unexpected project adapter snapshot: %#v", execution)
	}
	store, err := state.NewFileStore(stateDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), execution.Record.ProjectID); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("project state was published before the adapter succeeded: %v", err)
	}
	if err := program.reserveProjectExecution(context.Background(), execution); err != nil {
		t.Fatal(err)
	}
	if execution.Reservation == nil || execution.OperationID != execution.Reservation.OperationID {
		t.Fatalf("post-consent reservation was not acquired: %#v", execution.Reservation)
	}
	if err := program.commitProjectExecution(context.Background(), execution); err != nil {
		t.Fatal(err)
	}
	record, err := store.Get(context.Background(), execution.Record.ProjectID)
	if err != nil || record.HostPath != projectPath || record.Target != "yard" {
		t.Fatalf("Go did not publish the adapter result atomically: %#v %v", record, err)
	}
	secondPath := filepath.Join(root, "other", "Demo")
	if err := os.MkdirAll(secondPath, 0o700); err != nil {
		t.Fatal(err)
	}
	program.env["SUBYARD_OPERATION_ID"] = "op-second"
	second, err := program.prepareProjectImport(
		context.Background(), loaded, "sync", []string{secondPath, "--target", "yard"},
	)
	if err != nil || second.Record.ProjectID != "Demo-2" ||
		second.Record.YardPath != state.YardPath("Demo-2") ||
		second.OperationID != "op-second" {
		t.Fatalf("second basename admission = %#v, %v", second, err)
	}
	program.abortProjectExecution(context.Background(), second)
	program.env["SUBYARD_OPERATION_ID"] = "op-explicit"
	if _, err := program.prepareProjectImport(
		context.Background(), loaded, "sync",
		[]string{secondPath, "--name", "Demo", "--target", "yard"},
	); err == nil {
		t.Fatal("explicit colliding project name was accepted")
	}
}

func TestProjectPreparationDoesNotPublishAdmissionBeforeConsent(t *testing.T) {
	root, environment, stateDirectory := nativeFixture(t)
	projectPath := filepath.Join(root, "Preview")
	if err := os.MkdirAll(projectPath, 0o700); err != nil {
		t.Fatal(err)
	}
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	execution, err := program.prepareProjectImport(
		context.Background(), loaded, "sync", []string{projectPath},
	)
	if err != nil {
		t.Fatal(err)
	}
	if execution.Reservation != nil {
		t.Fatalf("prepare published reservation: %#v", execution.Reservation)
	}
	if _, err := os.Lstat(filepath.Join(stateDirectory, ".reservations")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prepare created reservation directory: %v", err)
	}
}

func TestProjectExecutionBuildsTypedActionVariants(t *testing.T) {
	record := domain.ProjectRecord{
		Schema: 1, IdentityVersion: 2, ProjectID: "Demo", Name: "Demo",
		HostPath: "/host/Demo", SourceKey: state.SourceKey("/host/Demo"),
		YardPath: state.YardPath("Demo"), Mode: domain.ProjectSync,
		SSHHost: "yard", Target: "yard",
	}
	for _, test := range []struct {
		name      string
		command   string
		execution projectExecution
		action    domain.ActionID
		changed   bool
	}{
		{name: "sync no-op", command: "sync", execution: projectExecution{Record: record}, action: "project.sync"},
		{name: "sync changed", command: "sync", execution: projectExecution{Record: record, ActionChanged: true}, action: "project.sync", changed: true},
		{name: "bind", command: "bind", execution: projectExecution{Record: record, ActionChanged: true}, action: "project.bind", changed: true},
		{name: "clone", command: "clone", execution: projectExecution{Record: record, ActionChanged: true}, action: "project.clone", changed: true},
		{name: "export", command: "export", execution: projectExecution{Record: record, ActionChanged: true}, action: "project.export-patch", changed: true},
		{name: "up", command: "up", execution: projectExecution{Record: record, ActionChanged: true, Environment: map[string]string{}}, action: "project.environment.up", changed: true},
		{name: "rebuild", command: "up", execution: projectExecution{Record: record, ActionChanged: true, Environment: map[string]string{"SUBYARD_PROJECT_REBUILD": "1"}}, action: "project.environment.rebuild", changed: true},
		{name: "down", command: "down", execution: projectExecution{Record: record, ActionChanged: true}, action: "project.environment.down", changed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			action, delta, err := test.execution.actionPlan(test.command)
			if err != nil || action != test.action || delta.Changed != test.changed {
				t.Fatalf("action=%q delta=%#v err=%v", action, delta, err)
			}
			if test.changed && len(delta.Consequences) == 0 {
				t.Fatal("changed project action has no consequences")
			}
		})
	}
}

func TestExistingProjectExportCarriesOperationIDIntoAssessment(t *testing.T) {
	root, environment, stateDirectory := nativeFixture(t)
	record := domain.ProjectRecord{
		Schema: 1, IdentityVersion: 2, ProjectID: "Demo", Name: "Demo",
		HostPath: filepath.Join(root, "Demo"), SourceKey: state.SourceKey(filepath.Join(root, "Demo")),
		YardPath: state.YardPath("Demo"), Mode: domain.ProjectSync,
		SSHHost: "yard", Target: "yard",
	}
	store, err := state.NewFileStore(stateDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	probe := projectActionObservationProbe{stream: func(
		ports.InstanceExecRequest, io.Reader,
	) (ports.InstanceExecResult, error) {
		return ports.InstanceExecResult{ExitCode: 0}, nil
	}}
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment,
		WorkingDir: root, ProjectData: probe, ProjectArchive: projectActionArchive("archive"),
	})
	if err != nil {
		t.Fatal(err)
	}
	program.env["SUBYARD_OPERATION_ID"] = "operation-export"
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	execution, err := program.prepareExistingProject(
		context.Background(), loaded, "export", []string{"Demo"}, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := program.observeProjectAction(context.Background(), "export", execution); err != nil {
		t.Fatalf("ordinary export assessment rejected the CLI operation ID: %v", err)
	}
	if execution.OperationID != "operation-export" {
		t.Fatalf("operation ID=%q", execution.OperationID)
	}
}

func TestObserveProjectBindDetectsConvergenceAndConflicts(t *testing.T) {
	record := domain.ProjectRecord{
		Schema: 1, IdentityVersion: 2, ProjectID: "Demo", Name: "Demo",
		HostPath: "/host/Demo", SourceKey: state.SourceKey("/host/Demo"),
		YardPath: state.YardPath("Demo"), Mode: domain.ProjectBind,
		SSHHost: "yard", Target: "yard",
	}
	loaded := config.Loaded{Context: domain.Context{
		YardType: domain.YardLocal, IncusProject: "subyard", InstanceName: "yard",
	}}
	desired := map[string]string{
		"type": "disk", "source": record.HostPath, "path": record.YardPath, "shift": "true",
	}
	for _, test := range []struct {
		name      string
		device    map[string]string
		changed   bool
		wantError bool
	}{
		{name: "missing", changed: true},
		{name: "converged", device: desired},
		{name: "conflict", device: map[string]string{
			"type": "disk", "source": "/host/Other", "path": record.YardPath, "shift": "true",
		}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			devices := map[string]map[string]string{}
			if test.device != nil {
				devices[state.WorkspaceDeviceFor(record)] = test.device
			}
			incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
				"subyard/yard": {Name: "yard", Project: "subyard", LocalDevices: devices},
			}}
			program := &CLI{options: Options{
				Incus: incus,
				ProjectData: projectActionObservationProbe{execute: func(ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
					return ports.InstanceExecResult{Stdout: []byte("match"), ExitCode: 0}, nil
				}},
			}}
			execution := &projectExecution{Loaded: loaded, Record: record}
			err := program.observeProjectAction(context.Background(), "bind", execution)
			if (err != nil) != test.wantError || execution.ActionChanged != test.changed {
				t.Fatalf("changed=%t err=%v", execution.ActionChanged, err)
			}
		})
	}
}

func TestAssessStructuredActionUsesTypedCoreAssessment(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	incus := lifecycleIncus()
	instance := incus.Instances["subyard/yard"]
	instance.Status = "Running"
	incus.Instances["subyard/yard"] = instance
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
		Incus: incus,
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	definition, ok := program.manifest.Lookup("stop")
	if !ok {
		t.Fatal("stop definition is missing")
	}
	lifecycle, err := prepareLifecycleExecution(definition, []string{"--force"})
	if err != nil {
		t.Fatal(err)
	}
	action, delta, typed, err := program.assessStructuredAction(
		context.Background(), loaded, definition, nil, nil, lifecycle, nil, nil,
	)
	if err != nil || !typed || action != "yard.stop-force" || !delta.Changed {
		t.Fatalf("action=%q delta=%#v typed=%t err=%v", action, delta, typed, err)
	}
}

func TestObserveProjectActionDetectsSyncAndEnvironmentNoOps(t *testing.T) {
	record := domain.ProjectRecord{
		Schema: 1, IdentityVersion: 2, ProjectID: "Demo", Name: "Demo",
		HostPath: "/host/Demo", SourceKey: state.SourceKey("/host/Demo"),
		YardPath: state.YardPath("Demo"), Mode: domain.ProjectSync,
		SSHHost: "yard", Target: "yard",
	}
	t.Run("new sync", func(t *testing.T) {
		execution := &projectExecution{Record: record}
		program := &CLI{}
		if err := program.observeProjectAction(context.Background(), "sync", execution); err != nil {
			t.Fatal(err)
		}
		if !execution.ActionChanged {
			t.Fatal("new sync was assessed as a no-op")
		}
	})
	t.Run("converged sync", func(t *testing.T) {
		root, environment, _ := nativeFixture(t)
		incus := lifecycleIncus()
		probe := projectActionObservationProbe{
			execute: func(request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
				if slices.Contains(request.Command, filepath.Join(filepath.Dir(record.YardPath), ".subyard-meta.json")) {
					return ports.InstanceExecResult{Stdout: []byte("match"), ExitCode: 0}, nil
				}
				return ports.InstanceExecResult{Stdout: []byte("present"), ExitCode: 0}, nil
			},
			stream: func(request ports.InstanceExecRequest, _ io.Reader) (ports.InstanceExecResult, error) {
				if len(request.Command) == 0 || request.Command[0] != "tar" {
					t.Fatalf("sync comparison=%#v", request.Command)
				}
				return ports.InstanceExecResult{ExitCode: 0}, nil
			},
		}
		program, err := New(Options{
			RepositoryRoot: root, Program: "yard", Environment: environment,
			WorkingDir: root, Incus: incus, ProjectData: probe, ProjectArchive: projectActionArchive("archive"),
		})
		if err != nil {
			t.Fatal(err)
		}
		existing := record
		execution := &projectExecution{Loaded: config.Loaded{Context: domain.Context{YardType: domain.YardLocal}}, Record: record, PreviewExisting: &existing}
		if err := program.observeProjectAction(context.Background(), "sync", execution); err != nil {
			t.Fatal(err)
		}
		if execution.ActionChanged {
			t.Fatal("converged sync was assessed as changed")
		}
	})
	t.Run("sync metadata drift", func(t *testing.T) {
		root, environment, _ := nativeFixture(t)
		probe := projectActionObservationProbe{
			execute: func(request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
				if slices.Contains(request.Command, record.YardPath) {
					return ports.InstanceExecResult{Stdout: []byte("present"), ExitCode: 0}, nil
				}
				return ports.InstanceExecResult{Stdout: []byte("different"), ExitCode: 0}, nil
			},
			stream: func(ports.InstanceExecRequest, io.Reader) (ports.InstanceExecResult, error) {
				return ports.InstanceExecResult{ExitCode: 0}, nil
			},
		}
		program, err := New(Options{
			RepositoryRoot: root, Program: "yard", Environment: environment,
			WorkingDir: root, Incus: lifecycleIncus(), ProjectData: probe,
			ProjectArchive: projectActionArchive("archive"),
		})
		if err != nil {
			t.Fatal(err)
		}
		existing := record
		execution := &projectExecution{
			Loaded: config.Loaded{Context: domain.Context{YardType: domain.YardLocal, YardName: "default"}},
			Record: record, PreviewExisting: &existing,
		}
		if err := program.observeProjectAction(context.Background(), "sync", execution); err != nil {
			t.Fatal(err)
		}
		if !execution.ActionChanged {
			t.Fatal("metadata drift was assessed as a no-op")
		}
	})
	for _, test := range []struct {
		name     string
		exitCode int
		changed  bool
	}{
		{name: "export no-op", exitCode: 0},
		{name: "export diff", exitCode: 1, changed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			probe := projectActionObservationProbe{stream: func(
				request ports.InstanceExecRequest, _ io.Reader,
			) (ports.InstanceExecResult, error) {
				if len(request.Command) == 0 || request.Command[0] != "sh" {
					t.Fatalf("export assessment=%#v", request.Command)
				}
				result := ports.InstanceExecResult{ExitCode: test.exitCode}
				if test.exitCode == 1 {
					return result, errors.New("diff")
				}
				return result, nil
			}}
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard", Environment: environment,
				WorkingDir: root, Incus: lifecycleIncus(), ProjectData: probe,
				ProjectArchive: projectActionArchive("archive"),
			})
			if err != nil {
				t.Fatal(err)
			}
			execution := &projectExecution{
				Loaded: config.Loaded{Context: domain.Context{YardType: domain.YardLocal}},
				Record: record, OperationID: "operation-export-assessment",
			}
			if err := program.observeProjectAction(context.Background(), "export", execution); err != nil {
				t.Fatal(err)
			}
			if execution.ActionChanged != test.changed {
				t.Fatalf("changed=%t", execution.ActionChanged)
			}
		})
	}
	t.Run("unowned environment", func(t *testing.T) {
		root, environment, _ := nativeFixture(t)
		probe := projectActionObservationProbe{execute: func(ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
			return ports.InstanceExecResult{Stdout: []byte("running\t\t\t"), ExitCode: 0}, nil
		}}
		program, err := New(Options{
			RepositoryRoot: root, Program: "yard", Environment: environment,
			WorkingDir: root, Incus: lifecycleIncus(), ProjectData: probe,
		})
		if err != nil {
			t.Fatal(err)
		}
		environmentRecord := record
		environmentRecord.Target = "fixture"
		execution := &projectExecution{
			Loaded: config.Loaded{Context: domain.Context{YardType: domain.YardLocal}},
			Record: environmentRecord, Environment: map[string]string{},
		}
		err = program.observeProjectAction(context.Background(), "up", execution)
		if err == nil || !strings.Contains(err.Error(), "not owned") {
			t.Fatalf("unowned environment error=%v", err)
		}
	})
	t.Run("environment manifest drift", func(t *testing.T) {
		root, environment, _ := nativeFixture(t)
		probe := projectActionObservationProbe{execute: func(request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
			if slices.Contains(request.Command, "/srv/env-meta/Demo/profile.json") {
				return ports.InstanceExecResult{Stdout: []byte("different"), ExitCode: 0}, nil
			}
			return ports.InstanceExecResult{Stdout: []byte("running\t1\tDemo\tfixture"), ExitCode: 0}, nil
		}}
		program, err := New(Options{
			RepositoryRoot: root, Program: "yard", Environment: environment,
			WorkingDir: root, Incus: lifecycleIncus(), ProjectData: probe,
		})
		if err != nil {
			t.Fatal(err)
		}
		environmentRecord := record
		environmentRecord.Target = "fixture"
		execution := &projectExecution{
			Loaded: config.Loaded{Context: domain.Context{YardType: domain.YardLocal}},
			Record: environmentRecord, Environment: map[string]string{},
		}
		err = program.observeProjectAction(context.Background(), "up", execution)
		if err == nil || !strings.Contains(err.Error(), "--rebuild") {
			t.Fatalf("manifest drift error=%v", err)
		}
	})
	for _, test := range []struct {
		name      string
		command   string
		state     string
		rebuild   bool
		changed   bool
		wantError bool
	}{
		{name: "up running", command: "up", state: "running"},
		{name: "up stopped", command: "up", state: "stopped", changed: true},
		{name: "rebuild running", command: "up", state: "running", rebuild: true, changed: true},
		{name: "down running", command: "down", state: "running", changed: true},
		{name: "down stopped", command: "down", state: "stopped"},
		{name: "down missing", command: "down", state: "missing", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			environmentRecord := record
			environmentRecord.Target = "fixture"
			root, environment, _ := nativeFixture(t)
			probe := projectActionObservationProbe{execute: func(request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
				if slices.Contains(request.Command, "/srv/env-meta/Demo/profile.json") {
					return ports.InstanceExecResult{Stdout: []byte("match"), ExitCode: 0}, nil
				}
				observation := test.state
				if test.state != "missing" {
					observation += "\t1\tDemo\tfixture"
				}
				return ports.InstanceExecResult{Stdout: []byte(observation), ExitCode: 0}, nil
			}}
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard", Environment: environment,
				WorkingDir: root, Incus: lifecycleIncus(), ProjectData: probe,
			})
			if err != nil {
				t.Fatal(err)
			}
			execution := &projectExecution{
				Loaded: config.Loaded{Context: domain.Context{YardType: domain.YardLocal}},
				Record: environmentRecord, Environment: map[string]string{},
			}
			if test.rebuild {
				execution.Environment["SUBYARD_PROJECT_REBUILD"] = "1"
			}
			err = program.observeProjectAction(context.Background(), test.command, execution)
			if (err != nil) != test.wantError || execution.ActionChanged != test.changed {
				t.Fatalf("changed=%t err=%v", execution.ActionChanged, err)
			}
		})
	}
}

func TestProjectReservationRejectsIdentityDriftAfterConsent(t *testing.T) {
	root, environment, stateDirectory := nativeFixture(t)
	projectPath := filepath.Join(root, "sources", "Demo")
	if err := os.MkdirAll(projectPath, 0o700); err != nil {
		t.Fatal(err)
	}
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	execution, err := program.prepareProjectImport(
		context.Background(), loaded, "sync", []string{projectPath},
	)
	if err != nil || execution.Record.ProjectID != "Demo" {
		t.Fatalf("preview=%#v err=%v", execution, err)
	}
	store, err := state.NewFileStore(stateDirectory)
	if err != nil {
		t.Fatal(err)
	}
	competing, err := store.Admit(
		context.Background(), "competing-operation", "/other/Demo",
		domain.ProjectSync, "Demo", false,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer store.AbortAdmission(context.Background(), competing.Reservation.OperationID)
	if err := program.reserveProjectExecution(context.Background(), execution); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("identity drift was accepted: %v", err)
	}
	if execution.Reservation != nil {
		t.Fatalf("stale reservation was retained: %#v", execution.Reservation)
	}
}

func TestOwnerProjectPreviewIsReadOnly(t *testing.T) {
	stateDirectory := t.TempDir()
	if err := os.Chmod(stateDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := state.NewFileStore(stateDirectory)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	program := &CLI{options: Options{Stdout: &stdout, Stderr: &stderr, Program: "yard"}}
	code := program.runOwnerProjectState(
		context.Background(), state.Service{Store: store}, domain.Context{SSHHost: "yard"},
		[]string{"preview", "/host/Demo", "sync", "Demo", "0"},
	)
	if code != 0 {
		t.Fatalf("preview failed: code=%d stderr=%q", code, stderr.String())
	}
	var response struct {
		ProjectID string                `json:"projectId"`
		Name      string                `json:"name"`
		Existing  *domain.ProjectRecord `json:"existing"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.ProjectID != "Demo" || response.Name != "Demo" || response.Existing != nil {
		t.Fatalf("preview=%#v", response)
	}
	if _, err := os.Lstat(filepath.Join(stateDirectory, ".reservations")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preview published a reservation: %v", err)
	}
}

func TestRemoteProjectAdmissionUsesOwnerReadOnlyPreview(t *testing.T) {
	fakeBin := t.TempDir()
	writeCLIFile(t, filepath.Join(fakeBin, "ssh"), `#!/bin/sh
printf '%s\n' '{"projectId":"Demo-2","name":"Demo-2","existing":null}'
`, 0o700)
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	program := &CLI{
		options: Options{WorkingDir: t.TempDir(), Stderr: io.Discard},
		env:     map[string]string{"PATH": fakeBin, "SUBYARD_OPERATION_ID": "preview-operation"},
	}
	loaded := config.Loaded{Context: domain.Context{
		YardType: domain.YardRemote, RemoteDest: "dev@owner.example", SSHHost: "yard",
	}}
	admission, err := program.previewProjectAdmission(
		context.Background(), loaded, nil, "/host/Demo", domain.ProjectSync, "Demo", false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if admission.ProjectID != "Demo-2" || admission.Name != "Demo-2" ||
		admission.Existing != nil || admission.Reservation != nil {
		t.Fatalf("remote preview=%#v", admission)
	}
}

func TestRemoteProjectReservationRejectsPreviewDriftAfterConsent(t *testing.T) {
	fakeBin := t.TempDir()
	writeCLIFile(t, filepath.Join(fakeBin, "ssh"), `#!/bin/sh
printf '%s\n' '{"projectId":"Demo-2","name":"Demo-2","reserved":true,"existing":null}'
`, 0o700)
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	program := &CLI{
		options: Options{WorkingDir: t.TempDir(), Stderr: io.Discard},
		env:     map[string]string{"PATH": fakeBin, "SUBYARD_OPERATION_ID": "reserve-operation"},
	}
	execution := &projectExecution{
		Loaded: config.Loaded{Context: domain.Context{
			YardType: domain.YardRemote, RemoteDest: "dev@owner.example", SSHHost: "yard",
		}},
		Commit: projectCommitPut, OperationID: "reserve-operation", RequestedName: "Demo",
		Record: domain.ProjectRecord{
			Schema: 1, IdentityVersion: 2, ProjectID: "Demo", Name: "Demo",
			HostPath: "/host/Demo", SourceKey: state.SourceKey("/host/Demo"),
			YardPath: state.YardPath("Demo"), Mode: domain.ProjectSync, SSHHost: "yard", Target: "yard",
		},
	}
	if err := program.reserveProjectExecution(context.Background(), execution); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("remote identity drift was accepted: %v", err)
	}
	if execution.RemoteReserved {
		t.Fatal("stale remote reservation was retained")
	}
}

func TestProjectParsersRejectEmptyExplicitName(t *testing.T) {
	if _, _, _, _, err := parseProjectImportArguments([]string{"--name="}); err == nil {
		t.Fatal("import accepted an empty explicit project name")
	}
	if _, _, _, _, _, err := parseProjectCloneArguments(
		[]string{"https://example.invalid/demo.git", "--name="},
	); err == nil {
		t.Fatal("clone accepted an empty explicit project name")
	}
}

func TestProjectImportAndCloneAllocateSameBasenameInEitherOrder(t *testing.T) {
	for _, first := range []string{"bind", "clone"} {
		t.Run(first+"-first", func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			projectPath := filepath.Join(root, "sources", "Demo")
			if err := os.MkdirAll(projectPath, 0o700); err != nil {
				t.Fatal(err)
			}
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard",
				Environment: environment, WorkingDir: root,
			})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			program.env["SUBYARD_OPERATION_ID"] = "op-first"
			var firstRun *projectExecution
			if first == "bind" {
				firstRun, err = program.prepareProjectImport(
					context.Background(), loaded, "bind", []string{projectPath},
				)
			} else {
				firstRun, err = program.prepareProjectClone(
					context.Background(), loaded,
					[]string{"https://example.invalid/Demo.git"},
				)
			}
			if err != nil || firstRun.Record.ProjectID != "Demo" {
				t.Fatalf("first admission = %#v, %v", firstRun, err)
			}
			if err := program.commitProjectExecution(context.Background(), firstRun); err != nil {
				t.Fatal(err)
			}

			program.env["SUBYARD_OPERATION_ID"] = "op-second"
			var secondRun *projectExecution
			if first == "bind" {
				secondRun, err = program.prepareProjectClone(
					context.Background(), loaded,
					[]string{"https://example.invalid/Demo.git"},
				)
			} else {
				secondRun, err = program.prepareProjectImport(
					context.Background(), loaded, "bind", []string{projectPath},
				)
			}
			if err != nil || secondRun.Record.ProjectID != "Demo-2" {
				t.Fatalf("second admission = %#v, %v", secondRun, err)
			}
			if err := program.commitProjectExecution(context.Background(), secondRun); err != nil {
				t.Fatal(err)
			}

			thirdPath := filepath.Join(root, "third", "Demo")
			if err := os.MkdirAll(thirdPath, 0o700); err != nil {
				t.Fatal(err)
			}
			program.env["SUBYARD_OPERATION_ID"] = "op-third"
			thirdRun, err := program.prepareProjectImport(
				context.Background(), loaded, "sync", []string{thirdPath},
			)
			if err != nil || thirdRun.Record.ProjectID != "Demo-3" {
				t.Fatalf("third admission = %#v, %v", thirdRun, err)
			}
			program.abortProjectExecution(context.Background(), thirdRun)

			program.env["SUBYARD_OPERATION_ID"] = "op-repeat"
			if _, err := program.prepareProjectClone(
				context.Background(), loaded,
				[]string{"https://example.invalid/Demo.git"},
			); err == nil || !strings.Contains(err.Error(), "already in the yard") {
				t.Fatalf("repeat clone = %v", err)
			}
		})
	}
}

func TestBindAcceptsExplicitPathAndPlansExposure(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	projectPath := filepath.Join(root, "home", ".ssh")
	if err := os.MkdirAll(projectPath, 0o700); err != nil {
		t.Fatal(err)
	}
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	execution, err := program.prepareProjectImport(
		context.Background(), loaded, "bind", []string{projectPath, "--target", "yard"},
	)
	if err != nil {
		t.Fatal(err)
	}
	consequences := strings.Join(application.ProjectConsequences(
		"bind", execution.Record, false,
	), " ")
	if execution.Record.HostPath != projectPath || !strings.Contains(consequences, "expose "+projectPath) {
		t.Fatalf("explicit bind path or safety plan missing: %#v %q", execution.Record, consequences)
	}
}

func TestProjectSelectionRoutesAcrossYardsBeforeAdapter(t *testing.T) {
	root, environment, stateDirectory := nativeFixture(t)
	configHome := ""
	for _, pair := range environment {
		if strings.HasPrefix(pair, "SUBYARD_CONFIG_HOME=") {
			configHome = strings.TrimPrefix(pair, "SUBYARD_CONFIG_HOME=")
		}
	}
	if configHome == "" {
		t.Fatal("fixture has no config home")
	}
	yardRegistry := filepath.Join(configHome, "yards")
	if err := os.MkdirAll(yardRegistry, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(configHome, "host-id"), "owner-a\n", 0o600)
	writeCLIFile(t, filepath.Join(yardRegistry, "other.env"), "SSH_PORT=2233\n", 0o600)
	otherState := filepath.Join(yardRegistry, "other", "projects")
	store, err := state.NewFileStore(otherState)
	if err != nil {
		t.Fatal(err)
	}
	record := domain.ProjectRecord{
		Schema: 1, ProjectID: "demo-12345678", Name: "Demo", HostPath: "/host/Demo",
		YardPath: state.YardPath("demo-12345678"), Mode: domain.ProjectSync,
		SSHHost: "yard-other", Target: "yard",
	}
	if err := store.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	execution, err := program.prepareExistingProject(
		context.Background(), loaded, "code", []string{"Demo"}, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if execution.Loaded.Context.YardName != "other" ||
		execution.YardIdentity != "owner-a/other" ||
		execution.Environment["SUBYARD_PROJECT_ID"] != record.ProjectID ||
		execution.Environment["SUBYARD_PROJECT_SSH_HOST"] != "yard-other" {
		t.Fatalf("project was not routed before adapter launch: %#v", execution)
	}
	if selected, err := program.routeProjectSource(
		context.Background(), loaded.Context, "/host/Unknown", "",
	); err != nil || selected != "default" {
		t.Fatalf("zero source matches routed to %q: %v", selected, err)
	}
	if selected, err := program.routeProjectSource(
		context.Background(), loaded.Context, record.HostPath, "",
	); err != nil || selected != "other" {
		t.Fatalf("one source match routed to %q: %v", selected, err)
	}
	defaultStore, err := state.NewFileStore(stateDirectory)
	if err != nil {
		t.Fatal(err)
	}
	defaultRecord := record
	defaultRecord.ProjectID = "default-demo"
	defaultRecord.Name = "DefaultDemo"
	defaultRecord.YardPath = state.YardPath(defaultRecord.ProjectID)
	defaultRecord.SSHHost = "yard"
	if err := defaultStore.Put(context.Background(), defaultRecord); err != nil {
		t.Fatal(err)
	}
	if _, err := program.routeProjectSource(
		context.Background(), loaded.Context, record.HostPath, "",
	); err == nil || !strings.Contains(err.Error(), "default other") {
		t.Fatalf("multiple source matches were not rejected: %v", err)
	}
	if selected, err := program.routeProjectSource(
		context.Background(), loaded.Context, record.HostPath, "other",
	); err != nil || selected != "other" {
		t.Fatalf("explicit source route = %q, %v", selected, err)
	}
}

func TestProjectSelectorsDoNotLetBareNamesBeShadowedByHostPaths(t *testing.T) {
	root, environment, stateDirectory := nativeFixture(t)
	collidingPath := filepath.Join(root, "Subyard")
	if err := os.MkdirAll(collidingPath, 0o700); err != nil {
		t.Fatal(err)
	}
	pathID := "LegacySource"
	store, err := state.NewFileStore(stateDirectory)
	if err != nil {
		t.Fatal(err)
	}
	registered := domain.ProjectRecord{
		Schema: 1, ProjectID: "Subyard-d2dfd1fb", Name: "Subyard",
		HostPath: "https://github.com/example/Subyard.git",
		YardPath: state.YardPath("Subyard-d2dfd1fb"), Mode: domain.ProjectGit,
		SSHHost: "yard", Target: "yard",
	}
	if err := store.Put(context.Background(), registered); err != nil {
		t.Fatal(err)
	}
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}

	for _, selector := range []string{"Subyard", registered.ProjectID, "default/Subyard"} {
		local, resolveErr := program.resolveLocalProject(
			context.Background(), loaded.Context, store, selector,
		)
		if resolveErr != nil || local.Record.ProjectID != registered.ProjectID {
			t.Fatalf("local selector %q resolved to %#v: %v", selector, local, resolveErr)
		}
		global, resolveErr := program.resolveGlobalProject(context.Background(), loaded.Context, selector)
		if resolveErr != nil || global.Record.ProjectID != registered.ProjectID {
			t.Fatalf("global selector %q resolved to %#v: %v", selector, global, resolveErr)
		}
	}

	for _, command := range []string{"shell", "code", "up", "down", "info", "remove"} {
		execution, prepareErr := program.prepareExistingProject(
			context.Background(), loaded, command, []string{"Subyard"}, false,
		)
		if prepareErr != nil || execution.Record.ProjectID != registered.ProjectID {
			t.Fatalf("%s selected %#v: %v", command, execution, prepareErr)
		}
	}

	for _, selector := range []string{"./Subyard", collidingPath} {
		if _, resolveErr := program.resolveLocalProject(
			context.Background(), loaded.Context, store, selector,
		); resolveErr == nil || !strings.Contains(resolveErr.Error(), selector) ||
			strings.Contains(resolveErr.Error(), pathID) {
			t.Fatalf("local explicit path %q returned unexpected error: %v", selector, resolveErr)
		}
		if _, resolveErr := program.resolveGlobalProject(
			context.Background(), loaded.Context, selector,
		); resolveErr == nil || !strings.Contains(resolveErr.Error(), selector) ||
			strings.Contains(resolveErr.Error(), pathID) {
			t.Fatalf("global explicit path %q returned unexpected error: %v", selector, resolveErr)
		}
	}

	pathRecord := registered
	pathRecord.ProjectID = pathID
	pathRecord.Name = "LegacySource"
	pathRecord.HostPath = collidingPath
	pathRecord.YardPath = state.YardPath(pathID)
	pathRecord.Mode = domain.ProjectSync
	if err := store.Put(context.Background(), pathRecord); err != nil {
		t.Fatal(err)
	}
	for _, selector := range []string{"./Subyard", collidingPath} {
		local, resolveErr := program.resolveLocalProject(
			context.Background(), loaded.Context, store, selector,
		)
		if resolveErr != nil || local.Record.ProjectID != pathID {
			t.Fatalf("local explicit path %q resolved to %#v: %v", selector, local, resolveErr)
		}
		global, resolveErr := program.resolveGlobalProject(context.Background(), loaded.Context, selector)
		if resolveErr != nil || global.Record.ProjectID != pathID {
			t.Fatalf("global explicit path %q resolved to %#v: %v", selector, global, resolveErr)
		}
	}
}

func TestExplicitProjectPathSyntax(t *testing.T) {
	for _, test := range []struct {
		selector string
		want     bool
	}{
		{selector: "Subyard"},
		{selector: "Subyard-d2dfd1fb"},
		{selector: "default/Subyard"},
		{selector: "." + string(filepath.Separator) + "Subyard", want: true},
		{selector: ".." + string(filepath.Separator) + "Subyard", want: true},
		{selector: ".", want: true},
		{selector: "..", want: true},
		{selector: filepath.Join(string(filepath.Separator), "srv", "Subyard"), want: true},
	} {
		if got := isExplicitProjectPath(test.selector); got != test.want {
			t.Fatalf("isExplicitProjectPath(%q) = %t, want %t", test.selector, got, test.want)
		}
	}
}

func TestParseShellArguments(t *testing.T) {
	root, selector, command, help, err := parseShellArguments(
		[]string{"--root", "Demo", "--", "sh", "-lc", "pwd"},
	)
	if err != nil || !root || help || selector != "Demo" ||
		!slices.Equal(command, []string{"sh", "-lc", "pwd"}) {
		t.Fatalf("unexpected shell parse: root=%v selector=%q command=%q help=%v err=%v",
			root, selector, command, help, err)
	}
	if _, _, _, _, err := parseShellArguments([]string{"one", "two"}); err == nil {
		t.Fatal("multiple project selectors were accepted")
	}
}

func TestUsageAndShellExecArgumentsPreserveTypedBoundaries(t *testing.T) {
	yard := domain.Context{
		InstanceName: "yard", IncusProject: "subyard", DevUser: "dev", DevUID: 1000,
	}
	usageInput := []string{"daily", "--json", "space arg", "", "$(touch should-not-run)"}
	filtered, help := parseUsageArguments(append([]string{"--yes"}, usageInput...))
	if help || !slices.Equal(filtered, usageInput) {
		t.Fatalf("usage arguments changed during parsing: %#v help=%v", filtered, help)
	}
	if filtered, help := parseUsageArguments([]string{"daily", "--help", "ignored"}); !help || filtered != nil {
		t.Fatalf("usage help was not terminal: %#v help=%v", filtered, help)
	}
	usage := usageExecArguments(yard, usageInput)
	wantPrefix := []string{
		"exec", "yard", "--project", "subyard",
		"--user", "1000", "--group", "1000",
		"--cwd", "/home/dev", "--env", "HOME=/home/dev", "--env", "USER=dev",
		"--", "/usr/local/bin/ccusage",
	}
	if len(usage) != len(wantPrefix)+len(usageInput) ||
		!slices.Equal(usage[:len(wantPrefix)], wantPrefix) ||
		!slices.Equal(usage[len(wantPrefix):], usageInput) {
		t.Fatalf("usage exec boundary drifted: %#v", usage)
	}

	devShell := shellExecArguments(yard, false, "/srv/workspaces/demo/src", []string{"sh", "-lc", "pwd"})
	for _, expected := range [][]string{
		{"--user", "1000"}, {"--group", "1000"}, {"--env", "HOME=/home/dev"},
		{"--cwd", "/srv/workspaces/demo/src"}, {"--", "sh", "-lc", "pwd"},
	} {
		if !containsSequence(devShell, expected) {
			t.Fatalf("dev shell omitted %#v: %#v", expected, devShell)
		}
	}
	rootShell := shellExecArguments(yard, true, "/home/dev", nil)
	for _, expected := range [][]string{
		{"--user", "0"}, {"--group", "0"}, {"--env", "HOME=/root"},
		{"--cwd", "/home/dev"}, {"-t", "--", "bash", "-l"},
	} {
		if !containsSequence(rootShell, expected) {
			t.Fatalf("root shell omitted %#v: %#v", expected, rootShell)
		}
	}
}

func containsSequence(values, sequence []string) bool {
	if len(sequence) == 0 {
		return true
	}
	for index := 0; index+len(sequence) <= len(values); index++ {
		if slices.Equal(values[index:index+len(sequence)], sequence) {
			return true
		}
	}
	return false
}

func nativeFixture(t *testing.T) (string, []string, string) {
	t.Helper()
	root := t.TempDir()
	for _, directory := range []string{"config", "scripts"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	manifest := strings.Join([]string{
		"init||@init||forward|mutate|dynamic|public|lifecycle|simple|init|init|--configs --reset --profile --yes --help|",
		"start||@lifecycle||forward|mutate|never|public|lifecycle|simple|start|start|--yes --help|",
		"stop||@lifecycle||forward|mutate|dynamic|public|lifecycle|simple|stop|stop|--force --yes --help|",
		"provision||@provision||forward|mutate|dynamic|public|lifecycle|profiles|provision [profile]|provision|-l --list --yes --help|",
		"test-vms||@test-vms||forward|mutate|dynamic|public|lifecycle|simple|test-vms <command>|test-vms|--slot -n -f --yes --help|logs status revoke recover",
		"teardown||@teardown||forward|mutate|dynamic|public|lifecycle|teardown|teardown|teardown|--keep-data --yes --help|",
		"status||@status||forward|read|never|public|lifecycle|status|status|status|--all --help|",
		"space||@space||local|read|never|public|lifecycle|simple|space|space|--refresh --help|",
		"logs||@logs||forward|read|never|public|lifecycle|simple|logs|logs|-f -n --yes --help|",
		"usage||@usage||forward|read|never|public|lifecycle|simple|usage|usage|--help|",
		"shell||@shell||forward|mutate|never|public|lifecycle|project-shell|shell|shell|--root --yes --help|",
		"clone||@project||local|mutate|dynamic|public|projects|clone|clone <url>|clone|--target --yes --help|",
		"code||@project||local|mutate|never|public|projects|project|code [project]|code|--yes --help|",
		"remove||@project||local|mutate|dynamic|public|projects|remove|remove [project]|remove|--soft --yes --help|",
		"yards||@yards||local|read|never|public|lifecycle|simple|yards|yards|--help|",
		"remote||@remote||local|mutate|dynamic|public|remote|remote|remote|remote|--yard --yes --help|add repair-key remove list",
		"update||@update||local|mutate|dynamic|public|lifecycle|simple|update|update|--check --version --offline --rollback --force --yes --help|",
		"list||@list||local|read|never|public|projects|simple|list|list|--live --help|",
		"_info||@info||local|read|never|hidden|internal|none|_info|info||",
		"_authorize||@authorize||forward|mutate|dynamic|hidden|internal|none|_authorize|authorize||",
		"rpc||@rpc||local|mutate|dynamic|hidden|internal|none|rpc --stdio|rpc|--stdio|",
		"_state||@state||local|mutate|dynamic|hidden|internal|none|_state|state||",
	}, "\n") + "\n"
	writeCLIFile(t, filepath.Join(root, "config", "commands.registry"), manifest, 0o600)
	for _, name := range []string{"incus.project.env", "subyard.env", "host.env", "agents.env", "ports.env"} {
		writeCLIFile(t, filepath.Join(root, "config", name), "", 0o600)
	}
	home := filepath.Join(root, "home")
	configHome := filepath.Join(root, "state")
	stateDirectory := filepath.Join(configHome, "projects")
	hostBase := filepath.Join(root, "host")
	environment := []string{
		"HOME=" + home, "SUBYARD_OPERATOR_HOME=" + home, "SUBYARD_CONFIG_HOME=" + configHome,
		"SUBYARD_HOME=" + filepath.Join(root, "data"),
		"STORAGE_PATH=" + filepath.Join(root, "data", "storage"),
		"HOST_BASE=" + hostBase, "RESTRICTED_DISK_PATHS=" + hostBase,
		"SHIFT_MODE=shift", "FORWARD_SSH_AGENT=0", "DEV_SUDO=0", "DEV_UID=1000",
		"DEV_USER=dev", "SSH_PORT=2222", "SUBYARD_NO_AUDIT=1",
	}
	return root, environment, stateDirectory
}

func lifecycleIncus() *testkit.Incus {
	return &testkit.Incus{Instances: map[string]ports.InstanceInfo{"subyard/yard": {
		Name: "yard", Project: "subyard", Status: "Stopped", Config: map[string]string{
			"user.subyard.managed": "true", "user.subyard.initialized": "true",
			"user.subyard.desired_power": "stopped", "user.subyard.name": "default",
			"user.subyard.bridge": "incusbr0", "boot.autostart": "false",
		},
	}}}
}

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/adapters/shelladapter"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
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
			Name: "yard", Project: "subyard", Type: domain.YardContainer, Status: "Stopped",
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
	handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]*preparedCommand)}
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
	writeCLIUpdateInstaller(t, root)
	prepareCLIOldRelease(t, runtimeRoot)
	environment = append(environment,
		"YARD_RELEASE_BASE_URL=file://"+assets,
		"YARD_RELEASE_CACHE="+cache,
		"RELEASE_CAPTURE="+capture,
		"ACTIVE_CAPTURE="+filepath.Join(root, "active-launcher.log"),
		"MIGRATION_FINALIZE_CAPTURE="+filepath.Join(root, "migration-finalize.log"),
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
	handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]*preparedCommand)}
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
	if err != nil || !slices.Contains(strings.Fields(string(arguments)), "--publish-only") ||
		!slices.Equal(events, []string{"operation.started", "operation.finished"}) ||
		!slices.Equal(configApplier.yards, []string{"default"}) {
		t.Fatalf("release RPC bypassed its prepared operation: args=%q events=%q configs=%q err=%v",
			arguments, events, configApplier.yards, err)
	}
}

func TestReleaseRuntimeConfigCarriesExactRepositoryRoot(t *testing.T) {
	repositoryRoot := filepath.Join(t.TempDir(), "runtime", "releases", "0.11.2-candidate")
	program := &CLI{options: Options{
		RepositoryRoot: repositoryRoot,
		Stdout:         &bytes.Buffer{},
		Stderr:         &bytes.Buffer{},
	}}
	config := program.releaseRuntimeConfig(map[string]string{"HOME": t.TempDir()})
	if config.RepositoryRoot != repositoryRoot {
		t.Fatalf("release runtime repository root = %q, want %q", config.RepositoryRoot, repositoryRoot)
	}
}

func TestV0111RecoveryPreparedActionUsesOneDefaultYesPrompt(t *testing.T) {
	const recoveryConsequence = "supersede the verified v0.11.1 recovery journal with the standalone candidate plan"
	countRecoveryConsequence := func(consequences []string) int {
		count := 0
		for _, consequence := range consequences {
			if consequence == recoveryConsequence {
				count++
			}
		}
		return count
	}
	for _, assumeYes := range []bool{false, true} {
		t.Run(fmt.Sprintf("assume yes=%v", assumeYes), func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			prompt := &testkit.Prompt{Answers: []bool{true}}
			program, err := New(Options{
				RepositoryRoot: root, Environment: environment, Prompt: prompt,
				Clock: testkit.NewManualClock(time.Unix(100, 0)),
			})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			prepared := releaseruntime.Prepared{
				Action: "update.activate", Changed: true,
				Consequences: []string{
					recoveryConsequence,
					"apply the exact typed migration and release activation plan",
				},
			}
			plan, err := program.operationOrchestrator(
				"operation-v0111-recovery", loaded, nil, nil,
			).PlanAction(
				context.Background(), loaded.Context, "update", domain.RemoteOnController,
				prepared.Action, domain.ActionDelta{
					Changed: prepared.Changed, Consequences: prepared.Consequences,
				}, assumeYes,
			)
			if err != nil || !plan.Confirmed || countRecoveryConsequence(plan.Consequences) != 1 {
				t.Fatalf("recovery plan=%#v err=%v", plan, err)
			}
			if assumeYes {
				if len(prompt.Requests) != 0 {
					t.Fatalf("--yes prompted: %#v", prompt.Requests)
				}
				return
			}
			if len(prompt.Requests) != 1 ||
				prompt.Requests[0].Default != domain.ConfirmationDefaultYes ||
				countRecoveryConsequence(prompt.Requests[0].Consequences) != 1 {
				t.Fatalf("interactive recovery prompt=%#v", prompt.Requests)
			}
		})
	}
}

func TestUpdateTypedConfirmationSeparatesCheckActivationAndRollbackPreflight(t *testing.T) {
	t.Run("check is bounded and never prompts or activates", func(t *testing.T) {
		root, environment, runtimeRoot := updateReleaseFixture(t)
		capture := filepath.Join(root, "check-installer.args")
		environment = append(environment, "RELEASE_CAPTURE="+capture)
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
		if err != nil || !slices.Contains(strings.Fields(string(arguments)), "--publish-only") {
			t.Fatalf("check installer args=%q err=%v", arguments, err)
		}
		if target, err := os.Readlink(filepath.Join(runtimeRoot, "current")); err != nil ||
			target != "releases/release-old" {
			t.Fatalf("check changed active runtime: target=%q err=%v", target, err)
		}
	})

	t.Run("declined activation leaves published candidate inactive", func(t *testing.T) {
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
		if target, err := os.Readlink(filepath.Join(runtimeRoot, "current")); err != nil ||
			target != "releases/release-old" {
			t.Fatalf("decline changed active runtime: target=%q err=%v", target, err)
		}
		if _, err := os.Stat(filepath.Join(runtimeRoot, "releases", "1.2.3-f16d05ec6b29")); err != nil {
			t.Fatalf("decline lost the safely published inspected candidate: %v", err)
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

func TestUpdateCheckReturnsZeroForBlockedV2Inspection(t *testing.T) {
	root, environment, runtimeRoot := updateReleaseFixture(t)
	environment = append(environment, "UPDATE_BLOCK_INSPECTION=1")
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments: []string{
			"update", "--check", "--version", "1.2.3", "--runtime-root", runtimeRoot,
		},
		Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 || stderr.Len() != 0 {
		t.Fatalf("blocked update check: code=%d stdout=%q stderr=%q",
			code, stdout.String(), stderr.String())
	}
	var output struct {
		Outcome *releasetransition.Outcome `json:"outcome"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &output); err != nil ||
		output.Outcome == nil ||
		output.Outcome.Status != releasetransition.StatusOperatorActionRequired ||
		output.Outcome.Code != releasetransition.CodeMigrationStale ||
		output.Outcome.Active != "release-old" ||
		output.Outcome.Target != "1.2.3-f16d05ec6b29" ||
		output.Outcome.Transaction == nil ||
		output.Outcome.Retry != "run yard update --check" {
		t.Fatalf("blocked update check outcome=%#v stdout=%q err=%v",
			output.Outcome, stdout.String(), err)
	}
}

func TestRollbackDoesNotExecuteRetainedEngineBeforeVerification(t *testing.T) {
	root, environment, runtimeRoot := updateReleaseFixture(t)
	prepareCLIReleaseLinks(t, runtimeRoot, true)
	marker := filepath.Join(root, "unverified-retained-engine-ran")
	engine := filepath.Join(runtimeRoot, "releases", "previous-b", "bin", "yard-engine")
	writeCLIFile(t, engine, fmt.Sprintf("#!/bin/sh\n: > %q\nprintf 'yard-engine 1.2.3\\n'\n", marker), 0o700)
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root,
		Program:        "yard",
		Arguments:      []string{"update", "--yes", "--runtime-root", runtimeRoot, "--rollback"},
		Environment:    environment,
		WorkingDir:     root,
		Stderr:         &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 {
		t.Fatalf("tampered rollback exit=%d stderr=%q", code, stderr.String())
	}
	if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback executed an unverified retained engine: %v", err)
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
				prepareCLIOldRelease(t, runtimeRoot)
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

			var stderr bytes.Buffer
			program, err := New(Options{
				RepositoryRoot: root, DispatcherPath: oldDispatcher, Program: "yard",
				Arguments: test.arguments(runtimeRoot), Environment: environment, WorkingDir: root,
				Stderr: &stderr,
			})
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code != 0 {
				t.Fatalf("update failed: code=%d stderr=%q", code, stderr.String())
			}
			if finalized, finalizeErr := os.ReadFile(filepath.Join(root, "migration-finalize.log")); !errors.Is(finalizeErr, os.ErrNotExist) {
				t.Fatalf("update invoked legacy migration finalization: payload=%q err=%v", finalized, finalizeErr)
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
	for _, name := range []string{"current", "previous"} {
		if err := os.Remove(filepath.Join(root, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	current := "1.2.3-f16d05ec6b29"
	if withPrevious {
		current = "current-a"
	}
	names := []string{current}
	if withPrevious {
		names = append(names, "previous-b")
	}
	for _, name := range names {
		releaseRoot := filepath.Join(root, "releases", name)
		bin := filepath.Join(releaseRoot, "bin")
		if err := os.MkdirAll(bin, 0o700); err != nil {
			t.Fatal(err)
		}
		writeCLIFile(t, filepath.Join(bin, "yard-engine"), `#!/bin/sh
set -eu
case "${1:-}" in
  --version) printf 'yard-engine 1.2.3\n' ;;
  _migrate) [ "${2:-}" = check ]; printf '%s\n' '{"targetLayout":1,"changed":false}' ;;
  _release-transition)
    request=$(cat)
    mode=$(printf '%s' "$request" | jq -r .mode)
	    runtime_root=$(printf '%s' "$request" | jq -r .runtimeRoot)
	    target_release=$(printf '%s' "$request" | jq -r .target)
	    if [ "$mode" = inspect ]; then
	      active=$(readlink "$runtime_root/current")
	      active_release=${active#releases/}
	      printf '{"schemaVersion":1,"inspection":{"plan":"plan-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible","consequences":["activate retained runtime"]},"outcome":{"status":"migration-required","reachedGoal":false,"active":"%s","target":"%s","code":"transition-required","message":"the retained release transition has not started","retry":"run yard update"}}}\n' "$active_release" "$target_release"
	      exit 0
	    fi
	    old=$(readlink "$runtime_root/current")
	    old_release=${old#releases/}
	    if [ "$old_release" = "$target_release" ]; then
	      printf '{"schemaVersion":1,"outcome":{"status":"ready","reachedGoal":true,"active":"%s","target":"%s","code":"ready","message":"verified","transaction":"tx-0123456789abcdef"}}\n' "$target_release" "$target_release"
	    else
	      rm -f "$runtime_root/current" "$runtime_root/previous"
	      ln -s "releases/$target_release" "$runtime_root/current"
	      ln -s "$old" "$runtime_root/previous"
	      printf '{"schemaVersion":1,"outcome":{"status":"ready","reachedGoal":true,"active":"%s","previous":"%s","target":"%s","code":"ready","message":"verified","transaction":"tx-0123456789abcdef"}}\n' "$target_release" "$old_release" "$target_release"
	    fi
    ;;
  *) exit 64 ;;
esac
`, 0o700)
		writeCLIFile(t, filepath.Join(bin, "yard"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$ACTIVE_CAPTURE\"\n", 0o700)
		registry := []byte("{}\n")
		if err := os.MkdirAll(filepath.Join(releaseRoot, "config"), 0o700); err != nil {
			t.Fatal(err)
		}
		writeCLIFile(t, filepath.Join(releaseRoot, "config", "release-transition.json"),
			string(registry), 0o600)
		var manifest strings.Builder
		for _, relative := range []string{
			"bin/yard", "bin/yard-engine", "config/release-transition.json",
		} {
			payload, err := os.ReadFile(filepath.Join(releaseRoot, relative))
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(payload)
			fmt.Fprintf(&manifest, "%x  ./%s\n", digest, relative)
		}
		writeCLIFile(t, filepath.Join(root, "releases", name, "runtime-files.sha256"), manifest.String(), 0o600)
	}
	if err := os.Symlink(filepath.Join("releases", current), filepath.Join(root, "current")); err != nil {
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
	writeCLIUpdateInstaller(t, root)
	prepareCLIOldRelease(t, runtimeRoot)
	return root, append(environment,
		"YARD_RELEASE_BASE_URL=file://"+assets,
		"YARD_RELEASE_CACHE="+cache,
		"MIGRATION_FINALIZE_CAPTURE="+filepath.Join(root, "migration-finalize.log"),
	), runtimeRoot
}

func prepareCLIOldRelease(t *testing.T, runtimeRoot string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(runtimeRoot, "releases", "release-old"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/release-old", filepath.Join(runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
}

func writeCLIUpdateInstaller(t *testing.T, repositoryRoot string) {
	t.Helper()
	script := strings.ReplaceAll(`#!/bin/sh
set -eu
if [ -n "${RELEASE_CAPTURE:-}" ]; then
	printf '%s\n' "$@" > "$RELEASE_CAPTURE"
fi
rollback=false
publish_only=false
while [ "$#" -gt 0 ]; do
	case "$1" in
	--runtime-root) root="$2"; shift 2 ;;
	--rollback) rollback=true; shift ;;
	--publish-only) publish_only=true; shift ;;
	*) shift ;;
	esac
done
if [ "$rollback" = true ]; then
	current=$(readlink "$root/current")
	previous=$(readlink "$root/previous")
	rm -f "$root/current" "$root/previous"
	ln -s "$previous" "$root/current"
	ln -s "$current" "$root/previous"
	exit 0
fi
target='__RELEASE_TARGET__'
destination="$root/$target"
mkdir -p "$destination/bin" "$destination/config"
chmod 700 "$destination" "$destination/bin" "$destination/config"
cat > "$destination/bin/yard-engine" <<'ENGINE'
#!/bin/sh
set -eu
case "${1:-}" in
  --version) printf 'yard-engine 1.2.3\n' ;;
  _migrate) [ "${2:-}" = check ]; printf '%s\n' '{"targetLayout":1,"changed":false}' ;;
  _release-transition)
    request=$(cat)
    mode=$(printf '%s' "$request" | jq -r .mode)
    runtime_root=$(printf '%s' "$request" | jq -r .runtimeRoot)
    target_release=$(printf '%s' "$request" | jq -r .target)
    if [ "$mode" = inspect ]; then
      if [ "${UPDATE_BLOCK_INSPECTION:-}" = 1 ]; then
        printf '%s\n' '{"schemaVersion":1,"inspection":{"plan":"plan-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible","consequences":["inspect blocked candidate"]},"blockers":[{"code":"migration-stale","resource":"yard.fixture","message":"the candidate resource changed","retry":"run yard update --check"}],"outcome":{"status":"operator-action-required","reachedGoal":false,"active":"release-old","target":"1.2.3-f16d05ec6b29","code":"migration-stale","message":"the candidate resource changed","retry":"run yard update --check","transaction":"tx-0123456789abcdef"}}}'
        exit 0
      fi
	      active=$(readlink "$runtime_root/current")
	      active_release=${active#releases/}
	      printf '{"schemaVersion":1,"inspection":{"plan":"plan-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible","consequences":["activate verified runtime 1.2.3"]},"outcome":{"status":"migration-required","reachedGoal":false,"active":"%s","target":"%s","code":"transition-required","message":"the inspected release transition has not started","retry":"run yard update"}}}\n' "$active_release" "$target_release"
      exit 0
    fi
	    old=''
	    if [ -L "$runtime_root/current" ]; then old=$(readlink "$runtime_root/current"); fi
	    old_release=${old#releases/}
    if [ -n "$old" ] && [ "$old" != "releases/$target_release" ]; then
      rm -f "$runtime_root/previous"
      ln -s "$old" "$runtime_root/previous"
    fi
    rm -f "$runtime_root/current"
    ln -s "releases/$target_release" "$runtime_root/current"
	    if [ "$old_release" = "$target_release" ]; then
	      printf '{"schemaVersion":1,"outcome":{"status":"ready","reachedGoal":true,"active":"%s","target":"%s","code":"ready","message":"verified","transaction":"tx-0123456789abcdef"}}\n' "$target_release" "$target_release"
	    else
	      printf '{"schemaVersion":1,"outcome":{"status":"ready","reachedGoal":true,"active":"%s","previous":"%s","target":"%s","code":"ready","message":"verified","transaction":"tx-0123456789abcdef"}}\n' "$target_release" "$old_release" "$target_release"
	    fi
    ;;
  *) exit 64 ;;
esac
ENGINE
printf '%s\n' '#!/bin/sh' 'if [ -n "${YARD_ENGINE_PATH:-}" ]; then exec "$YARD_ENGINE_PATH" "$@"; fi' 'printf "%s\\n" "$*" >> "$ACTIVE_CAPTURE"' > "$destination/bin/yard"
chmod 700 "$destination/bin/yard-engine" "$destination/bin/yard"
printf '{}\n' > "$destination/config/release-transition.json"
(cd "$destination" && sha256sum ./bin/yard ./bin/yard-engine ./config/release-transition.json > runtime-files.sha256)
[ "$publish_only" = true ] || exit 64
printf '%s\n' "$target"
`, "__RELEASE_TARGET__", "releases/1.2.3-f16d05ec6b29")
	writeCLIFile(t, filepath.Join(repositoryRoot, "scripts", "install-runtime-release.sh"), script, 0o700)
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
	handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]*preparedCommand)}
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
	remoteContext.AccessKind = domain.AccessRemote
	remoteContext.OwnerEndpoint = "dev@owner.example"
	remoteContext.OwnerYardName = "named"
	remoteValues := structuredAdapterContext(remoteContext)
	if remoteValues["OWNER_ENDPOINT"] != remoteContext.OwnerEndpoint || remoteValues["OWNER_YARD_NAME"] != remoteContext.OwnerYardName {
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
	if info.YardName != "default" || info.State != "RUNNING" || info.SSHPort != 2222 ||
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
			Name: "yard", Project: "subyard", Type: domain.YardContainer, Status: "Running",
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
			Profiles: []string{"android", "orca"},
			Agents: []domain.AgentStatus{
				{Name: "codex", State: "enabled"},
				{Name: "aiobserver", State: "up", URL: "http://127.0.0.1:18080/"},
			},
			Shared: []domain.SharedResourceStatus{{
				Profile: "android", Name: "emulator", State: "up", Hint: "yard emu down",
			}},
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
		"projects 1", "profiles android orca", "codex", "enabled", "aiobserver", "up",
		"http://127.0.0.1:18080/", "android   emulator", "security static-only", "space    1G",
	} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("status omitted %q:\n%s", expected, stdout.String())
		}
	}
}

func TestAIObserverStatusRendersWorkingLocalAndRemoteTunnelHints(t *testing.T) {
	tests := []struct {
		name          string
		yard          domain.Context
		agent         domain.AgentStatus
		ownerEndpoint string
		want          []string
	}{
		{
			name: "local container direct route",
			yard: domain.Context{YardKind: domain.YardContainer, SSHHost: "yard"},
			agent: domain.AgentStatus{
				Name: "aiobserver", State: "up", DashboardPort: 18080,
				URL: "http://127.0.0.1:18080/",
			},
			want: []string{"http://127.0.0.1:18080/"},
		},
		{
			name:  "local vm guest tunnel",
			yard:  domain.Context{YardKind: domain.YardVM, SSHHost: "yard-demo"},
			agent: domain.AgentStatus{Name: "aiobserver", State: "up", DashboardPort: 18080},
			want: []string{
				"http://127.0.0.1:18080/",
				"'ssh' '-N' '-o' 'ExitOnForwardFailure=yes' '-L' '18080:127.0.0.1:8080' 'yard-demo'",
			},
		},
		{
			name: "remote container owner tunnel",
			yard: domain.Context{YardKind: domain.YardContainer, SSHHost: "yard"},
			agent: domain.AgentStatus{
				Name: "aiobserver", State: "up", DashboardPort: 18080,
				URL: "http://127.0.0.1:18080/",
			},
			ownerEndpoint: "operator@owner.example",
			want: []string{
				"http://127.0.0.1:18080/",
				"'ssh' '-N' '-o' 'ExitOnForwardFailure=yes' '-L' '18080:127.0.0.1:18080' 'operator@owner.example'",
			},
		},
		{
			name:          "remote vm nested tunnel",
			yard:          domain.Context{YardKind: domain.YardVM, SSHHost: "yard-demo"},
			agent:         domain.AgentStatus{Name: "aiobserver", State: "up", DashboardPort: 18080},
			ownerEndpoint: "operator@owner.example",
			want: []string{
				"http://127.0.0.1:18080/",
				"'ssh' '-T' '-o' 'ExitOnForwardFailure=yes' '-L' '18080:127.0.0.1:18080' 'operator@owner.example'",
				"yard-demo",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			program := &CLI{options: Options{Stdout: &output}}
			program.printAgentStatus(test.yard, test.agent, test.ownerEndpoint)
			for _, want := range test.want {
				if !strings.Contains(output.String(), want) {
					t.Fatalf("agent status omitted %q: %s", want, output.String())
				}
			}
		})
	}
}

func TestAIObserverRemoteVMTunnelRunsNestedSSHThroughOwner(t *testing.T) {
	got := aiObserverTunnelArguments(
		domain.Context{YardKind: domain.YardVM, SSHHost: "yard-demo"},
		domain.AgentStatus{Name: "aiobserver", State: "up", DashboardPort: 18080},
		"operator@owner.example",
	)
	want := []string{
		"ssh", "-T", "-o", "ExitOnForwardFailure=yes",
		"-L", "18080:127.0.0.1:18080", "operator@owner.example",
		"'ssh' '-N' '-o' 'ExitOnForwardFailure=yes' '-L' '18080:127.0.0.1:8080' 'yard-demo'",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("remote VM tunnel arguments = %#v, want %#v", got, want)
	}
}

func TestRemoteStatusFactsTunnelLoopbackResourceDashboard(t *testing.T) {
	var output bytes.Buffer
	program := &CLI{options: Options{Stdout: &output}}
	status := domain.YardStatus{Facts: domain.StatusFacts{Shared: []domain.SharedResourceStatus{{
		Profile: "demo", Name: "dashboard", State: "up", URL: "http://localhost:19119/ui",
	}}}}
	program.printRemoteStatusFacts(status, "operator@owner.example")
	want := "open http://127.0.0.1:19119/ui after: 'ssh' '-N' '-o' 'ExitOnForwardFailure=yes' '-L' '19119:localhost:19119' 'operator@owner.example'"
	if !strings.Contains(output.String(), want) {
		t.Fatalf("remote loopback dashboard was not rendered through owner tunnel:\n%s", output.String())
	}
	if strings.Contains(output.String(), "'-p' '2222'") {
		t.Fatalf("REMOTE_SSH_PORT must not be used for the owner transport:\n%s", output.String())
	}
}

func TestRemoteStatusFactsPreserveProfilesAgentsAndResources(t *testing.T) {
	var output bytes.Buffer
	program := &CLI{options: Options{Stdout: &output}}
	status := domain.YardStatus{
		Context: domain.Context{YardKind: domain.YardContainer, SSHHost: "yard"},
		Facts: domain.StatusFacts{
			Profiles: []string{"hermes"},
			Agents: []domain.AgentStatus{{
				Name: "aiobserver", State: "up", DashboardPort: 18080,
				URL: "http://127.0.0.1:18080/",
			}},
			Shared: []domain.SharedResourceStatus{{
				Profile: "hermes", Name: "dashboard", State: "up",
				URL: "http://owner.tailnet.ts.net:19119/",
			}},
		},
	}
	program.printRemoteStatusFacts(status, "operator@owner.example")
	for _, want := range []string{
		"profiles hermes", "aiobserver", "operator@owner.example", "hermes", "dashboard",
		"http://owner.tailnet.ts.net:19119/",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("remote facts omitted %q: %s", want, output.String())
		}
	}
}

func TestReadQueriesDoNotFinalizeInvalidPendingReleaseMigration(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	sum := sha256.Sum256([]byte(Version))
	journalPath := filepath.Join(
		configHome,
		"migrations",
		"transactions",
		fmt.Sprintf("%x", sum[:16]),
		"transaction.json",
	)
	if err := os.MkdirAll(filepath.Dir(journalPath), 0o700); err != nil {
		t.Fatal(err)
	}
	journal := []byte("{not-json\n")
	writeCLIFile(t, journalPath, string(journal), 0o600)

	for _, test := range []struct {
		name      string
		arguments []string
		want      string
	}{
		{name: "version", arguments: []string{"--version"}, want: "yard " + Version + "\n"},
		{name: "status", arguments: []string{"status"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
				"subyard/yard": {
					Name: "yard", Project: "subyard", Type: domain.YardContainer,
					Status: "Running", Config: map[string]string{}, Devices: map[string]map[string]string{},
				},
			}}
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard", Arguments: test.arguments,
				Environment: append(environment, "SUBYARD_HOST_ID=owner-a"), WorkingDir: root,
				Stdout: &stdout, Stderr: &stderr, Incus: incus, Executor: incus,
				StatusFacts: statusFactsStub{value: domain.StatusFacts{Security: "live", Space: "unknown"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code != 0 {
				t.Fatalf("read query failed: code=%d stderr=%q", code, stderr.String())
			}
			if test.want != "" && stdout.String() != test.want {
				t.Fatalf("read query output = %q, want %q", stdout.String(), test.want)
			}
			got, err := os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, journal) {
				t.Fatalf("read query changed pending migration journal: got %q, want %q", got, journal)
			}
		})
	}
}

func TestReadOnlyCoreInvocationsDoNotRecoverPendingOwnerState(t *testing.T) {
	for _, recovery := range []struct {
		name string
		path func([]string) string
	}{
		{
			name: "owner inventory",
			path: func(environment []string) string {
				return filepath.Join(
					environmentValue(environment, "SUBYARD_HOME"),
					"owner-inventory",
					"registration.json",
				)
			},
		},
		{
			name: "HostID rename",
			path: func(environment []string) string {
				return configsync.HostIDRenameTransactionPath(
					environmentValue(environment, "SUBYARD_CONFIG_HOME"),
				)
			},
		},
	} {
		for _, invocation := range []struct {
			name      string
			arguments []string
		}{
			{name: "global version", arguments: []string{"--version"}},
			{name: "read command", arguments: []string{"status"}},
			{name: "project list", arguments: []string{"list"}},
			{name: "yard list", arguments: []string{"yards"}},
			{name: "command help", arguments: []string{"update", "--help"}},
		} {
			t.Run(recovery.name+"/"+invocation.name, func(t *testing.T) {
				root, environment, _ := nativeFixture(t)
				environment = append(environment, "SUBYARD_HOST_ID=owner-a")
				journalPath := recovery.path(environment)
				journal := []byte("{invalid-recovery\n")
				if err := os.MkdirAll(filepath.Dir(journalPath), 0o700); err != nil {
					t.Fatal(err)
				}
				writeCLIFile(t, journalPath, string(journal), 0o600)

				var stdout, stderr bytes.Buffer
				incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
					"subyard/yard": {
						Name: "yard", Project: "subyard", Type: domain.YardContainer,
						Status: "Running", Config: map[string]string{}, Devices: map[string]map[string]string{},
					},
				}}
				program, err := New(Options{
					RepositoryRoot: root, Program: "yard", Arguments: invocation.arguments,
					Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr,
					Incus: incus, Executor: incus,
					StatusFacts: statusFactsStub{value: domain.StatusFacts{Security: "live", Space: "unknown"}},
				})
				if err != nil {
					t.Fatal(err)
				}
				if code := program.Run(context.Background()); code != 0 {
					t.Fatalf("read-only invocation failed: code=%d stderr=%q", code, stderr.String())
				}
				got, err := os.ReadFile(journalPath)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, journal) {
					t.Fatalf("read-only invocation changed recovery journal: got %q, want %q", got, journal)
				}
			})
		}
	}
}

func TestMutationStillRequiresOwnerInventoryRecovery(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	journalPath := filepath.Join(
		environmentValue(environment, "SUBYARD_HOME"),
		"owner-inventory",
		"registration.json",
	)
	if err := os.MkdirAll(filepath.Dir(journalPath), 0o700); err != nil {
		t.Fatal(err)
	}
	journal := []byte("{invalid-recovery\n")
	writeCLIFile(t, journalPath, string(journal), 0o600)

	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"start"},
		Environment: environment, WorkingDir: root, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 ||
		!strings.Contains(stderr.String(), "recover owner inventory transaction") {
		t.Fatalf("mutation bypassed recovery gate: code=%d stderr=%q", code, stderr.String())
	}
	got, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, journal) {
		t.Fatalf("failed recovery changed invalid journal: got %q, want %q", got, journal)
	}
}

func installUnfinishedMutationGateFixture(
	t *testing.T, root string, environment []string, runtimeRoot string,
) string {
	t.Helper()
	registry := `{
  "schemaVersion": 1,
  "minimumLayout": 1,
  "currentLayout": 2,
  "migrations": [{
    "id": "move-fixture-settings",
    "fromLayout": 1,
    "toLayout": 2,
    "resources": ["fixture-settings"],
    "finalizePolicy": "remove-source-after-active-verify",
    "rollbackPolicy": "restore-recovery-before-runtime-swap",
    "moves": [{
      "scope": "config-home",
      "source": "legacy/settings.env",
      "destination": "current/settings.env",
      "consumer": "assignments"
    }]
  }]
}`
	writeCLIFile(t, filepath.Join(root, "config", "migrations.json"), registry, 0o600)
	if err := os.MkdirAll(runtimeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"current": "releases/0.0.9", "previous": "releases/0.0.8",
	} {
		if err := os.MkdirAll(filepath.Join(runtimeRoot, target), 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(runtimeRoot, name)
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	}
	transaction := map[string]any{
		"schemaVersion": 1,
		"fromLayout":    1,
		"toLayout":      2,
		"toRelease":     Version,
		"phase":         "preparing",
		"migrations":    []string{"move-fixture-settings"},
		"entries": []map[string]any{{
			"migrationId": "move-fixture-settings",
			"scope":       "config-home",
			"source":      "legacy/settings.env",
			"destination": "current/settings.env",
			"consumer":    "assignments",
			"recovery":    "recovery/0000",
		}},
	}
	payload, err := json.Marshal(transaction)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(Version))
	transactionID := fmt.Sprintf("%x", sum[:16])
	transactionPath := filepath.Join(
		environmentValue(environment, "SUBYARD_CONFIG_HOME"),
		"migrations", "transactions", transactionID, "transaction.json",
	)
	if err := os.MkdirAll(filepath.Dir(transactionPath), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, transactionPath, string(payload), 0o600)
	return transactionID
}

func TestMutationGatePrecedesOwnerRecoveryAndReturnsStructuredOutcome(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	runtimeRoot := filepath.Join(root, "runtime")
	environment = append(environment, "YARD_RUNTIME_ROOT="+runtimeRoot)
	installUnfinishedMutationGateFixture(t, root, environment, runtimeRoot)

	ownerJournalPath := filepath.Join(
		environmentValue(environment, "SUBYARD_HOME"), "owner-inventory", "registration.json",
	)
	if err := os.MkdirAll(filepath.Dir(ownerJournalPath), 0o700); err != nil {
		t.Fatal(err)
	}
	ownerJournal := []byte("{invalid-owner-recovery\n")
	writeCLIFile(t, ownerJournalPath, string(ownerJournal), 0o640)
	beforeInfo, err := os.Stat(ownerJournalPath)
	if err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"start"},
		Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 {
		t.Fatalf("gated mutation exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var outcome struct {
		Status      string  `json:"status"`
		Code        string  `json:"code"`
		Active      string  `json:"active"`
		Previous    *string `json:"previous"`
		Target      string  `json:"target"`
		Transaction *string `json:"transaction"`
		Action      string  `json:"action"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(stderr.Bytes()), &outcome); err != nil {
		t.Fatalf("mutation gate output is not structured JSON: %q: %v", stderr.String(), err)
	}
	if outcome.Status != "recovering" || outcome.Code != "recovery-pending" ||
		outcome.Active != "0.0.9" || outcome.Previous == nil || *outcome.Previous != "0.0.8" ||
		outcome.Target != Version || outcome.Transaction == nil ||
		!strings.HasPrefix(*outcome.Transaction, "v1-") ||
		outcome.Action != "run yard update" {
		t.Fatalf("mutation gate outcome=%#v", outcome)
	}
	got, err := os.ReadFile(ownerJournalPath)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(ownerJournalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, ownerJournal) || !os.SameFile(beforeInfo, afterInfo) ||
		afterInfo.Mode().Perm() != beforeInfo.Mode().Perm() {
		t.Fatalf("gate touched owner journal: bytes=%q same=%v before=%o after=%o",
			got, os.SameFile(beforeInfo, afterInfo), beforeInfo.Mode().Perm(), afterInfo.Mode().Perm())
	}
}

func installUnfinishedV2MutationGateFixture(
	t *testing.T,
	root string,
	environment []string,
	runtimeRoot string,
) (string, string) {
	t.Helper()
	target := "1.2.3-aaaaaaaaaaaa"
	registry := []byte(`{"schemaVersion":2,"minimumEpochs":{"settings":1},"currentEpochs":{"settings":1},"migrations":[]}`)
	writeCLIFile(t, filepath.Join(root, "config", "release-transition.json"), string(registry), 0o600)
	for _, release := range []string{"release-a", target} {
		if err := os.MkdirAll(filepath.Join(runtimeRoot, "releases", release, "bin"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("releases/release-a", filepath.Join(runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	links, err := releasetransition.NewRuntimeLinkStore(runtimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	if err := os.MkdirAll(configHome, 0o700); err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(root, "v2-candidate-inspections")
	engine := filepath.Join(runtimeRoot, "releases", target, "bin", "yard-engine")
	writeCLIFile(t, engine, `#!/bin/sh
case "${1:-}" in
--version) printf 'yard-engine 1.2.3\n' ;;
_release-transition)
request=$(cat)
config_home=$(printf '%s' "$request" | jq -r .configHome)
target_release=$(printf '%s' "$request" | jq -r .target)
journal="$config_home/release-transition/v2/journal.json"
resume_plan=$(jq -r .resumePlan "$journal")
transaction=$(jq -r .transaction "$journal")
printf called >> "$V2_GATE_CAPTURE"
printf '{"schemaVersion":1,"activationReconciliationOwned":false,"inspection":{"plan":"%s","assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible","consequences":["resume the transition"]},"resume":"%s","outcome":{"status":"recovering","reachedGoal":false,"active":"release-a","target":"%s","code":"recovery-pending","message":"the authorized release transition can resume from observed facts","retry":"run yard update","transaction":"%s"}}}\n' "$resume_plan" "$transaction" "$target_release" "$transaction"
;;
*) exit 64 ;;
esac
`, 0o700)
	payload, err := os.ReadFile(engine)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	if err := os.MkdirAll(
		filepath.Join(runtimeRoot, "releases", target, "config"), 0o700,
	); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t,
		filepath.Join(runtimeRoot, "releases", target, "config", "release-transition.json"),
		string(registry), 0o600,
	)
	registryDigest := sha256.Sum256(registry)
	manifest := fmt.Sprintf("%x  ./bin/yard-engine\n%x  ./config/release-transition.json\n",
		digest, registryDigest)
	writeCLIFile(t, filepath.Join(runtimeRoot, "releases", target, "runtime-files.sha256"),
		manifest, 0o600)
	manifestDigest := sha256.Sum256([]byte(manifest))
	transaction := releasetransition.TransactionID("tx-0123456789abcdef")
	transition, err := releasetransition.NewV2Transition(releasetransition.V2Options{
		ConfigHome: configHome,
		Releases: releasetransition.ReleasePair{
			From: "release-a", Target: releasetransition.ReleaseID(target),
		},
		ObserveLinks: func(context.Context) (releasetransition.ReleaseLinks, error) {
			return links.Observe()
		},
		ActivateLinks: func(
			context.Context,
			releasetransition.ReleasePair,
		) (releasetransition.ReleaseLinks, error) {
			observed, observeErr := links.Observe()
			if observeErr != nil {
				return releasetransition.ReleaseLinks{}, observeErr
			}
			return observed, errors.New("fixture activation interruption")
		},
		RegistryPayload:  registry,
		ArtifactDigest:   releasetransition.Fingerprint(fmt.Sprintf("%x", manifestDigest)),
		NewTransactionID: func() releasetransition.TransactionID { return transaction },
		VerifyAuthorization: func(
			releasetransition.PlanToken,
			releasetransition.Authorization,
		) bool {
			return true
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	goal := releasetransition.Goal{
		Target: releasetransition.ReleaseID(target), Direction: releasetransition.DirectionActivateTarget,
	}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := transition.Converge(context.Background(), releasetransition.Execution{
		Plan: inspection.Plan, Authorization: "fixture-authorization",
	})
	if err != nil || outcome.Status != releasetransition.StatusRecovering ||
		outcome.Transaction == nil || *outcome.Transaction != transaction {
		t.Fatalf("unfinished v2 fixture outcome = %#v, err=%v", outcome, err)
	}
	return filepath.Join(configHome, "release-transition", "v2", "journal.json"), capture
}

func TestV2MutationGateBlocksLifecycleWhileStatusRemainsReadOnly(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	runtimeRoot := filepath.Join(root, "runtime-v2")
	capture := filepath.Join(root, "v2-candidate-inspections")
	environment = append(environment,
		"YARD_RUNTIME_ROOT="+runtimeRoot,
		"V2_GATE_CAPTURE="+capture,
	)
	journalPath, _ := installUnfinishedV2MutationGateFixture(
		t, root, environment, runtimeRoot,
	)
	before, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Lstat(journalPath)
	if err != nil {
		t.Fatal(err)
	}

	runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{
		Schema: 1, OperationID: "unexpected-v2-mutation", Status: "ok",
	}}}}
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"start"},
		Environment: environment, WorkingDir: root, Stdout: &bytes.Buffer{}, Stderr: &stderr,
		AdapterRunner: runner, Incus: lifecycleIncus(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 || len(runner.Requests) != 0 {
		t.Fatalf("v2 gated mutation: code=%d requests=%#v stderr=%q",
			code, runner.Requests, stderr.String())
	}
	var gated struct {
		Status      string  `json:"status"`
		Code        string  `json:"code"`
		Active      string  `json:"active"`
		Target      string  `json:"target"`
		Transaction *string `json:"transaction"`
		Action      string  `json:"action"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(stderr.Bytes()), &gated); err != nil ||
		gated.Status != "recovering" || gated.Code != "recovery-pending" ||
		gated.Active != "release-a" || gated.Target != "1.2.3-aaaaaaaaaaaa" ||
		gated.Transaction == nil || *gated.Transaction != "tx-0123456789abcdef" ||
		gated.Action != "run yard update" {
		t.Fatalf("v2 gate outcome=%#v stderr=%q err=%v", gated, stderr.String(), err)
	}

	var statusStderr bytes.Buffer
	status, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"status"},
		Environment: environment, WorkingDir: root, Stdout: &bytes.Buffer{}, Stderr: &statusStderr,
		Incus: lifecycleIncus(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := status.Run(context.Background()); code != 0 {
		t.Fatalf("status during v2 recovery: code=%d stderr=%q", code, statusStderr.String())
	}
	testVMRunner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{
		Schema: 1, OperationID: "v2-read-only-test-vms-status", Status: "ok",
	}}}}
	testVMIncus := lifecycleIncus()
	testVMInstance := testVMIncus.Instances["subyard/yard"]
	testVMInstance.Status = "Running"
	testVMIncus.Instances["subyard/yard"] = testVMInstance
	var testVMStderr bytes.Buffer
	testVMStatus, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"test-vms", "status"},
		Environment: append(environment,
			"NESTED_E2E_VMS=1", "SUBYARD_OPERATION_ID=v2-read-only-test-vms-status",
		),
		WorkingDir: root, Stdout: &bytes.Buffer{}, Stderr: &testVMStderr,
		AdapterRunner: testVMRunner, Incus: testVMIncus,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := testVMStatus.Run(context.Background()); code != 0 || len(testVMRunner.Requests) != 1 {
		t.Fatalf("test-vms status during v2 recovery: code=%d requests=%#v stderr=%q",
			code, testVMRunner.Requests, testVMStderr.String())
	}
	after, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Lstat(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) || !os.SameFile(beforeInfo, afterInfo) ||
		afterInfo.Mode() != beforeInfo.Mode() {
		t.Fatal("mutation gate or read-only status changed the protected v2 journal")
	}
	if calls, err := os.ReadFile(capture); err != nil || string(calls) != "called" {
		t.Fatalf("candidate inspection calls=%q err=%v", calls, err)
	}
}

func TestV2MutationGateOwnsRecoveryWhileImportingV1Journal(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	runtimeRoot := filepath.Join(root, "runtime-v2-with-v1")
	capture := filepath.Join(root, "v2-with-v1-candidate-inspections")
	environment = append(environment,
		"YARD_RUNTIME_ROOT="+runtimeRoot,
		"V2_GATE_CAPTURE="+capture,
	)
	installUnfinishedV2MutationGateFixture(t, root, environment, runtimeRoot)
	installUnfinishedMutationGateFixture(t, root, environment, runtimeRoot)
	program, err := New(Options{
		RepositoryRoot: root,
		Program:        "yard",
		Environment:    environment,
		WorkingDir:     root,
		Stderr:         &bytes.Buffer{},
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := program.inspectMutationGate(context.Background(), "default")
	if err != nil || outcome == nil || outcome.Status != releasetransition.StatusRecovering ||
		outcome.Transaction == nil || *outcome.Transaction != "tx-0123456789abcdef" {
		t.Fatalf("v2 did not own related v1 recovery: outcome=%#v err=%v", outcome, err)
	}
}

func TestV2MutationGateRedactsCandidateVerificationPathsForCLIAndRPC(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	runtimeRoot := filepath.Join(root, "runtime-v2-redaction")
	environment = append(environment, "YARD_RUNTIME_ROOT="+runtimeRoot)
	installUnfinishedV2MutationGateFixture(t, root, environment, runtimeRoot)
	candidate := filepath.Join(runtimeRoot, "releases", "1.2.3-aaaaaaaaaaaa")
	relocated := filepath.Join(root, "private-candidate-location")
	if err := os.Rename(candidate, relocated); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(relocated, candidate); err != nil {
		t.Fatal(err)
	}
	configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")

	t.Run("CLI", func(t *testing.T) {
		var stderr bytes.Buffer
		program, err := New(Options{
			RepositoryRoot: root, Program: "yard", Arguments: []string{"start"},
			Environment: environment, WorkingDir: root,
			Stdout: &bytes.Buffer{}, Stderr: &stderr, Incus: lifecycleIncus(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if code := program.Run(context.Background()); code != 1 ||
			strings.Contains(stderr.String(), runtimeRoot) || strings.Contains(stderr.String(), configHome) {
			t.Fatalf("redacted CLI gate: code=%d stderr=%q", code, stderr.String())
		}
		var outcome mutationGateOutcome
		if err := json.Unmarshal(bytes.TrimSpace(stderr.Bytes()), &outcome); err != nil ||
			outcome.Status != releasetransition.StatusOperatorActionRequired ||
			outcome.Code != releasetransition.CodeDependencyUnavailable ||
			outcome.Active != "release-a" || outcome.Target != "1.2.3-aaaaaaaaaaaa" ||
			outcome.Transaction == nil || *outcome.Transaction != "tx-0123456789abcdef" ||
			outcome.Action != "restore the journal-selected release, then run yard update --check" {
			t.Fatalf("structured CLI gate: outcome=%#v stderr=%q err=%v",
				outcome, stderr.String(), err)
		}
	})

	t.Run("RPC", func(t *testing.T) {
		program, err := New(Options{
			RepositoryRoot: root, Program: "yard", Environment: environment,
			WorkingDir: root, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
		})
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := program.loadContext("default")
		if err != nil {
			t.Fatal(err)
		}
		handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]*preparedCommand)}
		params, err := json.Marshal(map[string]any{"command": "start", "arguments": []string{}})
		if err != nil {
			t.Fatal(err)
		}
		_, err = handler.Handle(context.Background(), rpc.Call{
			Method: "operation.plan", OperationID: "rpc-redacted-gate", Params: params,
		}, nil)
		fault, ok := err.(*rpc.Error)
		if !ok || fault.Code != "dependency-unavailable" ||
			strings.Contains(fault.Message, runtimeRoot) || strings.Contains(fault.Message, configHome) {
			t.Fatalf("redacted RPC gate error=%#v", err)
		}
		var outcome mutationGateOutcome
		if unmarshalErr := json.Unmarshal([]byte(fault.Message), &outcome); unmarshalErr != nil ||
			outcome.Status != releasetransition.StatusOperatorActionRequired ||
			outcome.Code != releasetransition.CodeDependencyUnavailable ||
			outcome.Active != "release-a" || outcome.Target != "1.2.3-aaaaaaaaaaaa" ||
			outcome.Transaction == nil || *outcome.Transaction != "tx-0123456789abcdef" ||
			outcome.Action != "restore the journal-selected release, then run yard update --check" {
			t.Fatalf("structured RPC gate: outcome=%#v error=%#v err=%v",
				outcome, fault, unmarshalErr)
		}
	})
}

func TestV2MutationGateReportsCorruptJournalAsStructuredOutcome(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	runtimeRoot := filepath.Join(root, "runtime-v2-corrupt-journal")
	environment = append(environment, "YARD_RUNTIME_ROOT="+runtimeRoot)
	journal, _ := installUnfinishedV2MutationGateFixture(t, root, environment, runtimeRoot)
	writeCLIFile(t, journal, `{`, 0o600)

	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"start"},
		Environment: environment, WorkingDir: root,
		Stdout: &bytes.Buffer{}, Stderr: &stderr, Incus: lifecycleIncus(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 {
		t.Fatalf("corrupt journal gate: code=%d stderr=%q", code, stderr.String())
	}
	var outcome mutationGateOutcome
	if err := json.Unmarshal(bytes.TrimSpace(stderr.Bytes()), &outcome); err != nil ||
		outcome.Status != releasetransition.StatusOperatorActionRequired ||
		outcome.Code != releasetransition.CodeJournalInvalid ||
		outcome.Active != "release-a" || outcome.Previous != nil || outcome.Target != "unknown" ||
		outcome.Transaction != nil ||
		outcome.Action != "restore protected release metadata from backup, then run yard update --check" {
		t.Fatalf("corrupt journal outcome=%#v stderr=%q err=%v", outcome, stderr.String(), err)
	}

	var stdout bytes.Buffer
	stderr.Reset()
	program, err = New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"update", "--check", "--runtime-root", runtimeRoot},
		Environment: environment, WorkingDir: root,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("corrupt journal update check: code=%d stdout=%q stderr=%q",
			code, stdout.String(), stderr.String())
	}
	var check struct {
		Outcome *releasetransition.Outcome `json:"outcome"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &check); err != nil ||
		check.Outcome == nil || check.Outcome.Code != releasetransition.CodeJournalInvalid ||
		check.Outcome.Status != releasetransition.StatusOperatorActionRequired ||
		check.Outcome.Active != "release-a" || check.Outcome.Target != "unknown" ||
		check.Outcome.Retry != "restore protected release metadata from backup, then run yard update --check" ||
		stderr.Len() != 0 || strings.Contains(stdout.String(), runtimeRoot) {
		t.Fatalf("corrupt journal update check outcome=%#v stdout=%q stderr=%q err=%v",
			check.Outcome, stdout.String(), stderr.String(), err)
	}
}

func TestRPCUpdateDifferentTargetIsGatedBeforePublication(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	runtimeRoot := filepath.Join(root, "runtime-v2-rpc-update")
	cache := filepath.Join(root, "rpc-update-cache")
	gateCapture := filepath.Join(root, "rpc-update-gate-inspection")
	publicationCapture := filepath.Join(root, "rpc-update-publication")
	environment = append(environment,
		"YARD_RUNTIME_ROOT="+runtimeRoot,
		"YARD_RELEASE_CACHE="+cache,
		"YARD_RELEASE_BASE_URL=file://"+filepath.Join(root, "missing-release-assets"),
		"V2_GATE_CAPTURE="+gateCapture,
	)
	installUnfinishedV2MutationGateFixture(t, root, environment, runtimeRoot)
	writeCLIFile(t, filepath.Join(root, "scripts", "install-runtime-release.sh"),
		"#!/bin/sh\n# --publish-only\nprintf called > \"$RPC_UPDATE_PUBLICATION_CAPTURE\"\nexit 91\n", 0o700)
	environment = append(environment, "RPC_UPDATE_PUBLICATION_CAPTURE="+publicationCapture)
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
		Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]*preparedCommand)}
	params, err := json.Marshal(map[string]any{
		"command":   "update",
		"arguments": []string{"--version", "9.9.9", "--runtime-root", runtimeRoot},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = handler.Handle(context.Background(), rpc.Call{
		Method: "operation.plan", OperationID: "rpc-different-update", Params: params,
	}, nil)
	fault, ok := err.(*rpc.Error)
	if !ok || fault.Code != "plan_failed" || !strings.Contains(fault.Message, "exact") {
		t.Fatalf("different-target RPC update error = %#v", err)
	}
	if _, err := os.Lstat(publicationCapture); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("different-target RPC update invoked publication: %v", err)
	}
	if _, err := os.Lstat(cache); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("different-target RPC update created release cache: %v", err)
	}
}

func TestV2MutationGateFailsClosedOnTamperedJournal(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	runtimeRoot := filepath.Join(root, "runtime-v2-tampered")
	environment = append(environment, "YARD_RUNTIME_ROOT="+runtimeRoot)
	configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	journalPath := filepath.Join(configHome, "release-transition", "v2", "journal.json")
	if err := os.MkdirAll(filepath.Dir(journalPath), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, journalPath, `{"schemaVersion":2,"checkpoint":"authorized"}`, 0o600)
	runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{
		Schema: 1, OperationID: "unexpected-tampered-mutation", Status: "ok",
	}}}}
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"start"},
		Environment: environment, WorkingDir: root, Stdout: &bytes.Buffer{}, Stderr: &stderr,
		AdapterRunner: runner, Incus: lifecycleIncus(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 || len(runner.Requests) != 0 ||
		strings.Contains(stderr.String(), `"status":"recovering"`) ||
		strings.Contains(stderr.String(), `"action":"run yard update"`) {
		t.Fatalf("tampered v2 gate: code=%d requests=%#v stderr=%q",
			code, runner.Requests, stderr.String())
	}
}

func TestMutationGateDoesNotHideTamperedV2BehindV1Recovery(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	runtimeRoot := filepath.Join(root, "runtime-combined-tamper")
	environment = append(environment, "YARD_RUNTIME_ROOT="+runtimeRoot)
	installUnfinishedMutationGateFixture(t, root, environment, runtimeRoot)
	configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	journalPath := filepath.Join(configHome, "release-transition", "v2", "journal.json")
	if err := os.MkdirAll(filepath.Dir(journalPath), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, journalPath, `{"schemaVersion":2,"checkpoint":"authorized"}`, 0o600)
	runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{
		Schema: 1, OperationID: "unexpected-combined-mutation", Status: "ok",
	}}}}
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"start"},
		Environment: environment, WorkingDir: root, Stdout: &bytes.Buffer{}, Stderr: &stderr,
		AdapterRunner: runner, Incus: lifecycleIncus(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 || len(runner.Requests) != 0 ||
		strings.Contains(stderr.String(), `"status":"recovering"`) ||
		strings.Contains(stderr.String(), `"action":"run yard update"`) {
		t.Fatalf("combined tampered gate: code=%d requests=%#v stderr=%q",
			code, runner.Requests, stderr.String())
	}
}

func TestRPCMutationGateBlocksPlanAndRechecksExecute(t *testing.T) {
	t.Run("plan before project preparation", func(t *testing.T) {
		root, environment, stateDirectory := nativeFixture(t)
		runtimeRoot := filepath.Join(root, "runtime")
		environment = append(environment, "YARD_RUNTIME_ROOT="+runtimeRoot)
		store, err := state.NewFileStore(stateDirectory)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Put(context.Background(), domain.ProjectRecord{
			Schema: 1, IdentityVersion: 2, ProjectID: "Demo", Name: "Demo",
			HostPath: "/host/Demo", SourceKey: state.SourceKey("/host/Demo"),
			YardPath: state.YardPath("Demo"), Mode: domain.ProjectSync, SSHHost: "yard", Target: "yard",
		}); err != nil {
			t.Fatal(err)
		}
		projectJournalPath := filepath.Join(stateDirectory, ".name-migration")
		projectJournal := []byte("{invalid-project-recovery\n")
		writeCLIFile(t, projectJournalPath, string(projectJournal), 0o600)
		installUnfinishedMutationGateFixture(t, root, environment, runtimeRoot)

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
		handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]*preparedCommand)}
		_, err = handler.Handle(context.Background(), rpc.Call{
			Method: "operation.plan", OperationID: "gated-project-plan",
			Params: json.RawMessage(`{"command":"code","arguments":["Demo"]}`),
		}, nil)
		fault, ok := err.(*rpc.Error)
		if !ok || fault.Code != "recovery-pending" {
			t.Fatalf("gated plan error=%#v", err)
		}
		got, readErr := os.ReadFile(projectJournalPath)
		if readErr != nil || !bytes.Equal(got, projectJournal) {
			t.Fatalf("gated plan reached project preparation: bytes=%q err=%v", got, readErr)
		}
	})

	t.Run("execute recheck", func(t *testing.T) {
		root, environment, _ := nativeFixture(t)
		runtimeRoot := filepath.Join(root, "runtime")
		environment = append(environment, "YARD_RUNTIME_ROOT="+runtimeRoot)
		runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{
			Schema: 1, OperationID: "gated-execute", Status: "ok",
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
		handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]*preparedCommand)}
		result, err := handler.Handle(context.Background(), rpc.Call{
			Method: "operation.plan", OperationID: "gated-execute",
			Params: json.RawMessage(`{"command":"start","arguments":[]}`),
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		plan := result.(domain.OperationPlan)
		installUnfinishedMutationGateFixture(t, root, environment, runtimeRoot)
		_, err = handler.Handle(context.Background(), rpc.Call{
			Method: "operation.execute", OperationID: plan.OperationID,
			Params: json.RawMessage(`{"confirmed":true}`),
		}, func(string, any) (uint64, error) { return 1, nil })
		fault, ok := err.(*rpc.Error)
		if !ok || fault.Code != "recovery-pending" || len(runner.Requests) != 0 {
			t.Fatalf("execute gate error=%#v requests=%#v", err, runner.Requests)
		}
	})
}

func TestCurrentEngineRejectsSupersededMutatingMigrationVerbs(t *testing.T) {
	program, err := New(Options{RepositoryRoot: repositoryRoot(t)})
	if err != nil {
		t.Fatal(err)
	}
	definition, ok := program.manifest.Lookup("_migrate")
	if !ok {
		t.Fatal("_migrate command is missing")
	}
	for _, verb := range []string{"apply", "finalize", "rollback", "cleanup"} {
		if slices.Contains(definition.Verbs, verb) {
			t.Errorf("_migrate manifest still advertises superseded verb %q", verb)
		}
		t.Run(verb, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			var stderr bytes.Buffer
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard",
				Environment: environment, WorkingDir: root,
				Stdout: &bytes.Buffer{}, Stderr: &stderr,
			})
			if err != nil {
				t.Fatal(err)
			}
			if code := program.runMigration(context.Background(), "default", []string{verb}); code != 2 ||
				!strings.Contains(stderr.String(), "superseded") {
				t.Fatalf("_migrate %s result: code=%d stderr=%q", verb, code, stderr.String())
			}
		})
	}
}

func TestMutatingUpdateIsExemptFromMutationGate(t *testing.T) {
	root, environment, runtimeRoot := updateReleaseFixture(t)
	environment = append(environment, "YARD_RUNTIME_ROOT="+runtimeRoot)
	installUnfinishedMutationGateFixture(t, root, environment, runtimeRoot)
	capture := filepath.Join(root, "update-exempt-called")
	writeCLIFile(t, filepath.Join(root, "scripts", "install-runtime-release.sh"),
		"#!/bin/sh\n# --publish-only\nprintf called > \"$UPDATE_EXEMPT_CAPTURE\"\nexit 17\n", 0o700)
	environment = append(environment, "UPDATE_EXEMPT_CAPTURE="+capture)
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"update", "--yes", "--version", "1.2.3", "--runtime-root", runtimeRoot},
		Environment: environment, WorkingDir: root, Stdout: &bytes.Buffer{}, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 {
		t.Fatalf("update failure exit=%d stderr=%q", code, stderr.String())
	}
	if payload, err := os.ReadFile(capture); err != nil || string(payload) != "called" {
		t.Fatalf("mutating update did not pass its gate exemption: payload=%q err=%v stderr=%q",
			payload, err, stderr.String())
	}
}

func TestMutationGateSkipsMissingMetadataButNotInternalChild(t *testing.T) {
	t.Run("operator home unavailable", func(t *testing.T) {
		root, _, _ := nativeFixture(t)
		program, err := New(Options{
			RepositoryRoot: root, Program: "yard", WorkingDir: root,
		})
		if err != nil {
			t.Fatal(err)
		}
		outcome, err := program.inspectMutationGate(context.Background(), "default")
		if err != nil || outcome != nil {
			t.Fatalf("operator-home-free mutation gate outcome=%#v err=%v", outcome, err)
		}
	})

	t.Run("missing metadata", func(t *testing.T) {
		root, environment, _ := nativeFixture(t)
		configHome := filepath.Join(root, "missing-config-home")
		environment = append(environment, "SUBYARD_CONFIG_HOME="+configHome)
		program, err := New(Options{
			RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
		})
		if err != nil {
			t.Fatal(err)
		}
		outcome, err := program.inspectMutationGate(context.Background(), "default")
		if err != nil || outcome != nil {
			t.Fatalf("missing mutation metadata outcome=%#v err=%v", outcome, err)
		}
		if _, err := os.Lstat(configHome); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("mutation gate created missing config home: %v", err)
		}
	})

	t.Run("internal migration child", func(t *testing.T) {
		root, environment, _ := nativeFixture(t)
		runtimeRoot := filepath.Join(root, "runtime")
		environment = append(environment,
			"YARD_RUNTIME_ROOT="+runtimeRoot,
			"SUBYARD_INTERNAL_MIGRATION_CHILD=1",
		)
		installUnfinishedMutationGateFixture(t, root, environment, runtimeRoot)
		program, err := New(Options{
			RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
		})
		if err != nil {
			t.Fatal(err)
		}
		outcome, err := program.inspectMutationGate(context.Background(), "default")
		if err != nil || outcome == nil || outcome.Status != releasetransition.StatusRecovering {
			t.Fatalf("caller-controlled child marker bypassed mutation gate: outcome=%#v err=%v",
				outcome, err)
		}
	})
}

func TestTrustedInProcessReleaseTransitionChildBypassesMutationGate(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	writeCLIFile(t, filepath.Join(root, "config", "subyard.env"), strings.Join([]string{
		"SHIFT_MODE=shift",
		"FORWARD_SSH_AGENT=0",
		"DEV_SUDO=0",
		"DEV_UID=1000",
		"DEV_USER=dev",
		"SSH_PORT=2222",
		"STORAGE_PATH=" + filepath.Join(root, "data", "storage"),
		"HOST_BASE=" + filepath.Join(root, "host"),
		"RESTRICTED_DISK_PATHS=" + filepath.Join(root, "host"),
	}, "\n")+"\n", 0o600)
	runtimeRoot := filepath.Join(root, "runtime")
	environment = append(environment,
		"YARD_RUNTIME_ROOT="+runtimeRoot,
		"SUBYARD_OPERATION_ID=trusted-release-child",
	)
	installUnfinishedMutationGateFixture(t, root, environment, runtimeRoot)
	runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{
		Schema: 1, OperationID: "trusted-release-child", Status: "ok",
	}}}}
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Environment: environment, WorkingDir: root, Stderr: &stderr,
		AdapterRunner: runner, Incus: lifecycleIncus(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := program.runReleaseTransitionYardCommand(
		context.Background(), "default", &stderr, "start",
	); err != nil || len(runner.Requests) != 1 {
		t.Fatalf("trusted release child: err=%v requests=%#v stderr=%q",
			err, runner.Requests, stderr.String())
	}
}

func TestReleaseTransitionChildUsesSanitizedCandidateContext(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	candidateConfig := filepath.Join(root, "config", "agents", "codex", "config.toml")
	previousConfig := filepath.Join(root, "previous-runtime", "config.toml")
	if err := os.MkdirAll(filepath.Dir(candidateConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(previousConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, candidateConfig, "candidate\n", 0o600)
	writeCLIFile(t, previousConfig, "previous\n", 0o600)
	writeCLIFile(t, filepath.Join(root, "config", "agents.env"),
		"AGENT_codex_CONFIG="+candidateConfig+"\n", 0o600)
	environment = append(environment, "AGENT_codex_CONFIG="+previousConfig)
	writeCLIFile(t, filepath.Join(root, "config", "subyard.env"), strings.Join([]string{
		"SHIFT_MODE=shift",
		"FORWARD_SSH_AGENT=0",
		"DEV_SUDO=0",
		"DEV_UID=1000",
		"DEV_USER=dev",
		"SSH_PORT=2222",
		"STORAGE_PATH=" + filepath.Join(root, "data", "storage"),
		"HOST_BASE=" + filepath.Join(root, "host"),
		"RESTRICTED_DISK_PATHS=" + filepath.Join(root, "host"),
	}, "\n")+"\n", 0o600)
	registryPath := filepath.Join(root, "config", "commands.registry")
	registry, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, registryPath, string(registry)+
		"probe-dispatch||probe-dispatch.sh||local|read|never|public|lifecycle|simple|probe-dispatch|probe dispatcher|--help|\n", 0o600)
	writeCLIFile(t, filepath.Join(root, "scripts", "probe-dispatch.sh"),
		"#!/bin/sh\nprintf '%s|%s\\n' \"$SUBYARD_DISPATCH_PATH\" \"$AGENT_codex_CONFIG\"\n", 0o700)
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	candidateDispatcher := filepath.Join(root, "bin", "yard-engine")
	writeCLIFile(t, candidateDispatcher, "#!/bin/sh\nexit 0\n", 0o700)
	program, err := New(Options{
		RepositoryRoot: root, DispatcherPath: filepath.Join(root, "removed-candidate"),
		Program: "yard", Environment: environment, WorkingDir: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := program.runReleaseTransitionYardCommand(
		context.Background(), "default", &output, "probe-dispatch",
	); err != nil {
		t.Fatal(err)
	}
	want := candidateDispatcher + "|" + candidateConfig
	if got := strings.TrimSpace(output.String()); got != want {
		t.Fatalf("release transition child context=%q, want %q", got, want)
	}
}

func TestCoreReadCommandsDoNotRecoverProjectStore(t *testing.T) {
	for _, test := range []struct {
		name      string
		arguments []string
	}{
		{name: "status summary", arguments: []string{"status"}},
		{name: "status detail", arguments: []string{"-Y", "default", "status"}},
		{name: "project list", arguments: []string{"list"}},
		{name: "yard list", arguments: []string{"yards"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, environment, stateDirectory := nativeFixture(t)
			environment = append(environment, "SUBYARD_HOST_ID=owner-a")
			if err := os.MkdirAll(stateDirectory, 0o700); err != nil {
				t.Fatal(err)
			}
			journalPath := filepath.Join(stateDirectory, ".name-migration")
			journal := []byte("{invalid-project-recovery\n")
			writeCLIFile(t, journalPath, string(journal), 0o600)
			incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
				"subyard/yard": {
					Name: "yard", Project: "subyard", Type: domain.YardContainer,
					Status: "Stopped", Config: map[string]string{}, Devices: map[string]map[string]string{},
				},
			}}
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
				t.Fatalf("read command recovered project state: code=%d stderr=%q", code, stderr.String())
			}
			got, err := os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, journal) {
				t.Fatalf("read command changed project recovery journal: got %q, want %q", got, journal)
			}
			if _, err := os.Lstat(filepath.Join(stateDirectory, ".lock")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("read command created project state lock: %v", err)
			}
		})
	}
}

func TestProjectInfoDoesNotRecoverOwnerOrProjectJournals(t *testing.T) {
	root, environment, projectDirectory := nativeFixture(t)
	environment = append(environment,
		"SUBYARD_HOST_ID=owner-a",
		"SUBYARD_OPERATION_ID=read-project-info",
	)
	registryPath := filepath.Join(root, "config", "commands.registry")
	registry, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	registry = append(registry,
		[]byte("info||@project-env||forward|read|never|public|project_env|project-env|info [project]|info|--yes --help|\n")...,
	)
	writeCLIFile(t, registryPath, string(registry), 0o600)

	projectStore, err := state.NewFileStore(projectDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if err := projectStore.Put(context.Background(), domain.ProjectRecord{
		Schema: 1, IdentityVersion: 2, ProjectID: "Remote", Name: "Remote",
		HostPath: "/host/Remote", SourceKey: state.SourceKey("/host/Remote"),
		YardPath: state.YardPath("Remote"), Mode: domain.ProjectSync,
		SSHHost: "yard", Target: "go",
	}); err != nil {
		t.Fatal(err)
	}
	ownerRoot := filepath.Join(environmentValue(environment, "SUBYARD_HOME"), "owner-inventory")
	if err := os.MkdirAll(ownerRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	ownerJournalPath := filepath.Join(ownerRoot, "registration.json")
	projectJournalPath := filepath.Join(projectDirectory, ".name-migration")
	ownerJournal := []byte("{invalid-owner-recovery\n")
	projectJournal := []byte("{invalid-project-recovery\n")
	writeCLIFile(t, ownerJournalPath, string(ownerJournal), 0o600)
	writeCLIFile(t, projectJournalPath, string(projectJournal), 0o600)

	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"info", "Remote"},
		Environment: environment, WorkingDir: root, Stdout: &bytes.Buffer{}, Stderr: &stderr,
		ProjectData: projectActionObservationProbe{execute: func(ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
			return ports.InstanceExecResult{ExitCode: 0, Stdout: []byte("{}\n")}, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("project info failed: code=%d stderr=%q", code, stderr.String())
	}
	for path, want := range map[string][]byte{
		ownerJournalPath: ownerJournal, projectJournalPath: projectJournal,
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("read-only info changed %s: got %q, want %q", path, got, want)
		}
	}
}

func TestRPCReadsDoNotRecoverProjectStore(t *testing.T) {
	for _, method := range []string{"project.list", "yard.status"} {
		t.Run(method, func(t *testing.T) {
			root, environment, stateDirectory := nativeFixture(t)
			environment = append(environment, "SUBYARD_HOST_ID=owner-a")
			if err := os.MkdirAll(stateDirectory, 0o700); err != nil {
				t.Fatal(err)
			}
			journalPath := filepath.Join(stateDirectory, ".name-migration")
			journal := []byte("{invalid-project-recovery\n")
			writeCLIFile(t, journalPath, string(journal), 0o600)
			incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
				"subyard/yard": {
					Name: "yard", Project: "subyard", Type: domain.YardContainer,
					Status: "Stopped", Config: map[string]string{}, Devices: map[string]map[string]string{},
				},
			}}
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard", Environment: environment,
				WorkingDir: root, Incus: incus, Executor: incus,
				StatusFacts: statusFactsStub{value: domain.StatusFacts{Security: "live", Space: "unknown"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]*preparedCommand)}
			if _, err := handler.Handle(context.Background(), rpc.Call{
				Method: method, OperationID: "read-project-state", Params: json.RawMessage(`{}`),
			}, nil); err != nil {
				t.Fatalf("RPC read recovered project state: %v", err)
			}
			got, err := os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, journal) {
				t.Fatalf("RPC read changed project recovery journal: got %q, want %q", got, journal)
			}
			if _, err := os.Lstat(filepath.Join(stateDirectory, ".lock")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("RPC read created project state lock: %v", err)
			}
		})
	}
}

func TestUpdateCheckDoesNotRecoverUnrelatedOwnerState(t *testing.T) {
	for _, recovery := range []struct {
		name string
		path func([]string) string
	}{
		{
			name: "owner inventory",
			path: func(environment []string) string {
				return filepath.Join(
					environmentValue(environment, "SUBYARD_HOME"),
					"owner-inventory",
					"registration.json",
				)
			},
		},
		{
			name: "HostID rename",
			path: func(environment []string) string {
				return configsync.HostIDRenameTransactionPath(
					environmentValue(environment, "SUBYARD_CONFIG_HOME"),
				)
			},
		},
	} {
		t.Run(recovery.name, func(t *testing.T) {
			root, environment, runtimeRoot := updateReleaseFixture(t)
			environment = append(environment, "SUBYARD_HOST_ID=owner-a")
			journalPath := recovery.path(environment)
			journal := []byte("{invalid-owner-recovery\n")
			if err := os.MkdirAll(filepath.Dir(journalPath), 0o700); err != nil {
				t.Fatal(err)
			}
			writeCLIFile(t, journalPath, string(journal), 0o600)

			var stderr bytes.Buffer
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard",
				Arguments: []string{
					"update", "--check", "--version", "1.2.3", "--runtime-root", runtimeRoot,
				},
				Environment: environment, WorkingDir: root, Stdout: &bytes.Buffer{}, Stderr: &stderr,
			})
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code != 0 {
				t.Fatalf("update check recovered owner state: code=%d stderr=%q", code, stderr.String())
			}
			got, err := os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, journal) {
				t.Fatalf("update check changed recovery journal: got %q, want %q", got, journal)
			}
		})
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
						Name: "yard", Project: "subyard", Type: domain.YardContainer,
						Status: "Running", Config: map[string]string{}, Devices: map[string]map[string]string{},
					},
					"subyard-demo/yard-demo": {
						Name: "yard-demo", Project: "subyard-demo", Type: domain.YardContainer,
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

func TestNativeListPreservesLegacyProjectPermissions(t *testing.T) {
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
	if info.Mode().Perm() != 0o664 {
		t.Fatalf("read-only list changed legacy state mode: got %o, want 664", info.Mode().Perm())
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
		context.Background(), loaded, "export", []string{"Demo"}, false, false,
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
		AccessKind: domain.AccessLocal, IncusProject: "subyard", YardInstanceName: "yard",
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
	if err := program.observeLifecycleExecution(context.Background(), loaded.Context, lifecycle); err != nil {
		t.Fatal(err)
	}
	action, delta, err := lifecycle.actionPlan(definition, loaded.Context)
	if err != nil || action != "yard.stop-force" || !delta.Changed {
		t.Fatalf("action=%q delta=%#v err=%v", action, delta, err)
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
		execution := &projectExecution{Loaded: config.Loaded{Context: domain.Context{AccessKind: domain.AccessLocal}}, Record: record, PreviewExisting: &existing}
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
			Loaded: config.Loaded{Context: domain.Context{AccessKind: domain.AccessLocal, YardName: "default"}},
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
				Loaded: config.Loaded{Context: domain.Context{AccessKind: domain.AccessLocal}},
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
			Loaded: config.Loaded{Context: domain.Context{AccessKind: domain.AccessLocal}},
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
			Loaded: config.Loaded{Context: domain.Context{AccessKind: domain.AccessLocal}},
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
				Loaded: config.Loaded{Context: domain.Context{AccessKind: domain.AccessLocal}},
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
		AccessKind: domain.AccessRemote, OwnerEndpoint: "dev@owner.example", SSHHost: "yard",
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
			AccessKind: domain.AccessRemote, OwnerEndpoint: "dev@owner.example", SSHHost: "yard",
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
		context.Background(), loaded, "code", []string{"Demo"}, false, false,
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
			context.Background(), loaded, command, []string{"Subyard"}, false, command == "info",
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
		YardInstanceName: "yard", IncusProject: "subyard", DevUser: "dev", DevUID: 1000,
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

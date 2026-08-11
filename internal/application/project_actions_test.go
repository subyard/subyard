package application

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

type projectDataStub struct {
	requests []ports.InstanceExecRequest
	failAt   int
	run      func(ports.InstanceExecRequest) (ports.InstanceExecResult, error)
}

type projectDevicesStub struct {
	ensured []string
	removed []string
}

func (stub *projectDevicesStub) EnsureDiskDevice(
	_ context.Context,
	_, _, device, _, _ string,
) (bool, error) {
	stub.ensured = append(stub.ensured, device)
	return true, nil
}

func (stub *projectDevicesStub) RemoveDevice(
	_ context.Context,
	_, _, device string,
) (bool, error) {
	stub.removed = append(stub.removed, device)
	return true, nil
}

func (stub *projectDataStub) Execute(
	_ context.Context,
	_ domain.Context,
	request ports.InstanceExecRequest,
) (ports.InstanceExecResult, error) {
	stub.requests = append(stub.requests, request)
	if stub.run != nil {
		return stub.run(request)
	}
	if len(stub.requests) == stub.failAt {
		return ports.InstanceExecResult{Stderr: []byte("fixture failure")}, errors.New("failed")
	}
	return ports.InstanceExecResult{}, nil
}

func (stub *projectDataStub) Stream(
	_ context.Context,
	_ domain.Context,
	request ports.InstanceExecRequest,
	input io.Reader,
) (ports.InstanceExecResult, error) {
	request.Stdin, _ = io.ReadAll(input)
	return stub.Execute(context.Background(), domain.Context{}, request)
}

type projectArchiveStub struct {
	payload string
}

type projectExportStub struct {
	patch []byte
	path  string
}

type vsCodeStub struct {
	calls [][]string
}

func (stub *vsCodeStub) Run(_ context.Context, arguments ...string) ([]byte, error) {
	stub.calls = append(stub.calls, append([]string(nil), arguments...))
	return nil, nil
}

func (stub *projectExportStub) Publish(_ context.Context, _ string, patch []byte) (string, error) {
	stub.patch = append([]byte(nil), patch...)
	return stub.path, nil
}

func (stub projectArchiveStub) Open(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(stub.payload)), nil
}

func TestProjectCloneOwnsSequenceAndMetadata(t *testing.T) {
	data := &projectDataStub{}
	runner := ProjectActionRunner{
		Data: data,
		Yard: domain.Context{
			YardName: "default", AccessKind: domain.AccessLocal, DevUser: "dev", DevUID: 1000,
		},
		Project: cloneRecord(),
	}
	result, _, err := runner.Run(context.Background(), domain.AdapterRequest{
		Schema: 1, OperationID: "operation-clone", Adapter: "project", Action: "clone",
	}, nil)
	if err != nil || result.Status != "ok" || len(data.requests) != 5 {
		t.Fatalf("clone failed: result=%#v calls=%#v err=%v", result, data.requests, err)
	}
	if got := data.requests[2].Command; len(got) != 5 || got[0] != "git" || got[2] != "--" ||
		got[3] != cloneRecord().HostPath || data.requests[2].User != 1000 {
		t.Fatalf("unsafe clone command: %#v", data.requests[2])
	}
	var metadata map[string]any
	if err := json.Unmarshal(data.requests[3].Stdin, &metadata); err != nil ||
		metadata["projectId"] != cloneRecord().ProjectID || metadata["target"] != "openclaw" ||
		metadata["yard"] != "default" {
		t.Fatalf("invalid portable metadata: %q err=%v", data.requests[3].Stdin, err)
	}
}

func TestProjectCloneCleansPartialWorkspace(t *testing.T) {
	data := &projectDataStub{failAt: 3}
	runner := ProjectActionRunner{
		Data:    data,
		Yard:    domain.Context{AccessKind: domain.AccessRemote, DevUser: "dev", DevUID: 1000},
		Project: cloneRecord(),
	}
	if _, _, err := runner.Run(context.Background(), domain.AdapterRequest{
		Schema: 1, OperationID: "operation-clone", Adapter: "project", Action: "clone",
	}, nil); err == nil {
		t.Fatal("clone failure was hidden")
	}
	if len(data.requests) != 4 || data.requests[3].Command[0] != "rm" {
		t.Fatalf("partial clone was not cleaned: %#v", data.requests)
	}
}

func TestProjectCloneRejectsUnownedExistingWorkspace(t *testing.T) {
	data := &projectDataStub{}
	data.run = func(request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
		if len(request.Command) != 0 && request.Command[0] == "sh" {
			return ports.InstanceExecResult{ExitCode: 1}, errors.New("workspace exists")
		}
		return ports.InstanceExecResult{}, nil
	}
	runner := ProjectActionRunner{
		Data:    data,
		Yard:    domain.Context{AccessKind: domain.AccessRemote, DevUser: "dev", DevUID: 1000},
		Project: cloneRecord(),
	}
	if _, _, err := runner.Run(context.Background(), domain.AdapterRequest{
		Schema: 1, OperationID: "operation-clone", Adapter: "project", Action: "clone",
	}, nil); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("unowned workspace was accepted: %v", err)
	}
	if len(data.requests) != 1 || data.requests[0].Command[0] != "sh" {
		t.Fatalf("existing workspace was modified: %#v", data.requests)
	}
}

func TestProjectMetadataIsCanonicalAndNewlineTerminated(t *testing.T) {
	record := cloneRecord()
	payload, err := ProjectMetadata(record, "default")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema":1,"yard":"default","projectId":"demo-12345678","name":"Demo","mode":"git","target":"openclaw","importedAt":"2026-07-22T00:00:00Z"}` + "\n"
	if string(payload) != want {
		t.Fatalf("metadata=%q", payload)
	}
}

func TestProjectRemoveCleansEnvironmentBeforeWorkspace(t *testing.T) {
	data := &projectDataStub{}
	record := cloneRecord()
	record.Mode = domain.ProjectSync
	runner := ProjectActionRunner{
		Data: data, Yard: domain.Context{AccessKind: domain.AccessRemote}, Project: record,
	}
	if _, _, err := runner.Run(context.Background(), domain.AdapterRequest{
		Schema: 1, OperationID: "operation-remove", Adapter: "project", Action: "remove",
	}, nil); err != nil {
		t.Fatal(err)
	}
	if len(data.requests) != 6 || data.requests[0].Command[0] != "docker" ||
		data.requests[4].Command[0] != "rm" || data.requests[4].Command[3] != "/srv/workspaces/demo-12345678" ||
		data.requests[5].Command[4] != projectHooksDispatcher {
		t.Fatalf("unexpected removal order: %#v", data.requests)
	}
}

func TestProjectRemoveFailureKeepsWorkspace(t *testing.T) {
	data := &projectDataStub{failAt: 1}
	record := cloneRecord()
	record.Mode = domain.ProjectSync
	runner := ProjectActionRunner{
		Data: data, Yard: domain.Context{AccessKind: domain.AccessRemote}, Project: record,
	}
	if _, _, err := runner.Run(context.Background(), domain.AdapterRequest{
		Schema: 1, OperationID: "operation-remove", Adapter: "project", Action: "remove",
	}, nil); err == nil {
		t.Fatal("environment cleanup failure was hidden")
	}
	if len(data.requests) != 1 {
		t.Fatalf("workspace changed after failed environment cleanup: %#v", data.requests)
	}
}

func TestProjectRemoveDetachesBindWithoutDeletingHostData(t *testing.T) {
	data, devices := &projectDataStub{}, &projectDevicesStub{}
	record := cloneRecord()
	record.Mode, record.Target = domain.ProjectBind, "yard"
	runner := ProjectActionRunner{
		Data: data, Devices: devices, Yard: domain.Context{AccessKind: domain.AccessLocal}, Project: record,
	}
	if _, _, err := runner.Run(context.Background(), domain.AdapterRequest{
		Schema: 1, OperationID: "operation-remove", Adapter: "project", Action: "remove",
	}, nil); err != nil {
		t.Fatal(err)
	}
	if len(data.requests) != 1 || len(devices.removed) != 1 || devices.removed[0] != "ws-demo-12345678" ||
		data.requests[0].Command[4] != projectHooksDispatcher {
		t.Fatalf("bind removal crossed the data boundary: calls=%#v devices=%#v", data.requests, devices.removed)
	}
}

func TestProjectSyncStreamsArchiveAndWritesMetadata(t *testing.T) {
	data := &projectDataStub{}
	record := cloneRecord()
	record.Mode, record.HostPath = domain.ProjectSync, "/host/demo"
	runner := ProjectActionRunner{
		Data: data, Archive: projectArchiveStub{payload: "archive"},
		Yard: domain.Context{AccessKind: domain.AccessRemote, DevUID: 1000}, Project: record,
	}
	if _, _, err := runner.Run(context.Background(), domain.AdapterRequest{
		Schema: 1, OperationID: "operation-sync", Adapter: "project", Action: "sync",
	}, nil); err != nil {
		t.Fatal(err)
	}
	if len(data.requests) != 4 || data.requests[1].Command[0] != "tar" ||
		string(data.requests[1].Stdin) != "archive" || data.requests[2].Command[0] != "tee" ||
		data.requests[3].Command[4] != projectHooksDispatcher {
		t.Fatalf("unexpected sync sequence: %#v", data.requests)
	}
}

func TestProjectBindUsesIncusDeviceAndWritesMetadata(t *testing.T) {
	data, devices := &projectDataStub{}, &projectDevicesStub{}
	record := cloneRecord()
	record.Mode, record.HostPath = domain.ProjectBind, "/host/demo"
	runner := ProjectActionRunner{
		Data: data, Devices: devices,
		Yard: domain.Context{AccessKind: domain.AccessLocal, DevUID: 1000}, Project: record,
	}
	if _, _, err := runner.Run(context.Background(), domain.AdapterRequest{
		Schema: 1, OperationID: "operation-bind", Adapter: "project", Action: "bind",
	}, nil); err != nil {
		t.Fatal(err)
	}
	if len(devices.ensured) != 1 || devices.ensured[0] != "ws-demo-12345678" ||
		len(data.requests) != 3 || data.requests[1].Command[0] != "tee" ||
		data.requests[2].Command[4] != projectHooksDispatcher {
		t.Fatalf("unexpected bind sequence: calls=%#v devices=%#v", data.requests, devices.ensured)
	}
}

func TestProjectHookFailureIsNonBlockingAndVisible(t *testing.T) {
	data := &projectDataStub{}
	data.run = func(request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
		if len(request.Command) == 5 && request.Command[4] == projectHooksDispatcher {
			return ports.InstanceExecResult{ExitCode: 1}, errors.New("hook failed")
		}
		return ports.InstanceExecResult{}, nil
	}
	runner := ProjectActionRunner{
		Data:    data,
		Yard:    domain.Context{AccessKind: domain.AccessRemote, DevUser: "dev", DevUID: 1000},
		Project: cloneRecord(),
	}
	result, message, err := runner.Run(context.Background(), domain.AdapterRequest{
		Schema: 1, OperationID: "operation-clone", Adapter: "project", Action: "clone",
	}, nil)
	if err != nil || result.Status != "ok" || !strings.Contains(message, "optional agent project hook failed") {
		t.Fatalf("hook changed project result: result=%#v message=%q err=%v", result, message, err)
	}
}

func TestProjectExportUsesDataPlaneAndPublishesPortablePatch(t *testing.T) {
	data := &projectDataStub{}
	data.run = func(request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
		if len(request.Command) > 0 && request.Command[0] == "diff" {
			return ports.InstanceExecResult{ExitCode: 1, Stdout: []byte(
				"diff -ruN /tmp/subyard-export-operation-export/a/file /srv/workspaces/demo-12345678/src/file\n" +
					"--- /tmp/subyard-export-operation-export/a/file\n+++ /srv/workspaces/demo-12345678/src/file\n@@ -1 +1,2 @@\n-old\n+new\n+/srv/workspaces/demo-12345678/src stays content\n",
			)}, errors.New("status 1")
		}
		return ports.InstanceExecResult{}, nil
	}
	record := cloneRecord()
	record.Mode, record.HostPath = domain.ProjectSync, "/host/demo"
	exports := &projectExportStub{path: "/data/exports/demo.patch"}
	runner := ProjectActionRunner{
		Data: data, Archive: projectArchiveStub{payload: "archive"}, Exports: exports,
		Yard: domain.Context{AccessKind: domain.AccessRemote}, Project: record,
	}
	result, message, err := runner.Run(context.Background(), domain.AdapterRequest{
		Schema: 1, OperationID: "operation-export", Adapter: "project", Action: "export",
	}, nil)
	if err != nil || result.Output["patch"] != exports.path {
		t.Fatalf("export failed: result=%#v message=%q err=%v", result, message, err)
	}
	if !strings.Contains(string(exports.patch), "--- a/file\n+++ b/file\n") ||
		!strings.Contains(string(exports.patch), "+/srv/workspaces/demo-12345678/src stays content\n") {
		t.Fatalf("patch is not portable: %q", exports.patch)
	}
	if len(data.requests) != 4 || data.requests[1].Command[0] != "tar" ||
		data.requests[2].Command[0] != "diff" || data.requests[3].Command[0] != "rm" {
		t.Fatalf("unexpected export sequence: %#v", data.requests)
	}
}

func TestProjectExportRejectsProjectsWithoutHostCopy(t *testing.T) {
	runner := ProjectActionRunner{Data: &projectDataStub{}, Project: cloneRecord()}
	if _, _, err := runner.Run(context.Background(), domain.AdapterRequest{
		Schema: 1, OperationID: "operation-export", Adapter: "project", Action: "export",
	}, nil); err == nil || !strings.Contains(err.Error(), "cannot be exported") {
		t.Fatalf("git export was accepted: %v", err)
	}
}

func TestProjectCodeWritesRemoteWorkspaceAndOpensDescriptor(t *testing.T) {
	data := &projectDataStub{}
	code := &vsCodeStub{}
	workspaceDirectory := t.TempDir()
	record := cloneRecord()
	record.Mode, record.HostPath, record.Target = domain.ProjectSync, "/host/demo", "yard"
	incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
		"subyard/yard": {Status: "Running", Devices: map[string]map[string]string{"ssh": {"type": "proxy"}}},
	}}
	runner := ProjectActionRunner{
		Data: data, Instances: incus, VSCode: code,
		WorkspaceDirectory: workspaceDirectory,
		Yard:               domain.Context{AccessKind: domain.AccessLocal, IncusProject: "subyard", YardInstanceName: "yard", DevUser: "dev", DevUID: 1000},
		Project:            record, Extensions: []string{"anthropic.claude-code"},
		YardIdentity: "owner/default",
	}
	_, message, err := runner.Run(context.Background(), domain.AdapterRequest{
		Schema: 1, OperationID: "operation-code", Adapter: "project", Action: "code",
	}, nil)
	if err != nil || !strings.Contains(message, "opened Demo") {
		t.Fatalf("code failed: message=%q err=%v", message, err)
	}
	if len(data.requests) != 0 {
		t.Fatalf("workspace descriptor was written inside the yard: %#v", data.requests)
	}
	workspacePath := filepath.Join(
		workspaceDirectory, "eWFyZA.demo-12345678", "Demo.code-workspace",
	)
	payload, readErr := os.ReadFile(workspacePath)
	if readErr != nil {
		t.Fatalf("read controller workspace: %v", readErr)
	}
	var workspace struct {
		RemoteAuthority string              `json:"remoteAuthority"`
		Folders         []map[string]string `json:"folders"`
		Settings        map[string]string   `json:"settings"`
		Extensions      struct {
			Recommendations []string `json:"recommendations"`
		} `json:"extensions"`
	}
	if err := json.Unmarshal(payload, &workspace); err != nil ||
		workspace.RemoteAuthority != "ssh-remote+yard" ||
		len(workspace.Folders) != 1 ||
		workspace.Folders[0]["name"] != "Demo" ||
		workspace.Folders[0]["uri"] != "vscode-remote://ssh-remote+yard"+record.YardPath ||
		workspace.Settings["window.title"] != "${rootNameShort} — Yard SSH: owner/default" ||
		!slices.Equal(workspace.Extensions.Recommendations, []string{"anthropic.claude-code"}) {
		t.Fatalf("invalid workspace: %#v err=%v", workspace, err)
	}
	if len(code.calls) != 1 || !slices.Equal(code.calls[0], []string{workspacePath}) {
		t.Fatalf("VS Code workspace was not opened: %#v", code.calls)
	}
}

func TestProjectCodeWorkspaceNamespaceSeparatesHostAndProjectIdentity(t *testing.T) {
	workspaceDirectory := t.TempDir()
	code := &vsCodeStub{}
	for _, identity := range []struct{ host, projectID string }{
		{host: "a-b", projectID: "c"},
		{host: "a", projectID: "b-c"},
	} {
		record := cloneRecord()
		record.SSHHost, record.ProjectID = identity.host, identity.projectID
		record.YardPath = "/srv/workspaces/" + identity.projectID + "/src"
		runner := ProjectActionRunner{
			Data: &projectDataStub{}, VSCode: code, WorkspaceDirectory: workspaceDirectory,
			Yard: domain.Context{AccessKind: domain.AccessRemote}, Project: record,
			YardIdentity: "owner/default",
		}
		if _, _, err := runner.Run(context.Background(), domain.AdapterRequest{
			Schema: 1, OperationID: "operation-code", Adapter: "project", Action: "code",
		}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(code.calls) != 2 || code.calls[0][0] == code.calls[1][0] {
		t.Fatalf("workspace identities collided: %#v", code.calls)
	}
}

func TestProjectCodeRequiresCanonicalYardIdentity(t *testing.T) {
	code := &vsCodeStub{}
	runner := ProjectActionRunner{
		Data: &projectDataStub{}, VSCode: code, WorkspaceDirectory: t.TempDir(),
		Yard: domain.Context{AccessKind: domain.AccessRemote}, Project: cloneRecord(),
	}
	_, _, err := runner.Run(context.Background(), domain.AdapterRequest{
		Schema: 1, OperationID: "operation-code", Adapter: "project", Action: "code",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "canonical yard identity") || len(code.calls) != 0 {
		t.Fatalf("code accepted a missing yard identity: calls=%#v err=%v", code.calls, err)
	}
}

func TestProjectCodeManualFallbackUsesPositionalControllerDescriptor(t *testing.T) {
	record := cloneRecord()
	workspaceDirectory := filepath.Join(t.TempDir(), "directory with spaces")
	runner := ProjectActionRunner{
		Data: &projectDataStub{}, WorkspaceDirectory: workspaceDirectory,
		Yard: domain.Context{AccessKind: domain.AccessRemote}, Project: record,
		YardIdentity: "owner/default",
	}
	_, message, err := runner.Run(context.Background(), domain.AdapterRequest{
		Schema: 1, OperationID: "operation-code", Adapter: "project", Action: "code",
	}, nil)
	if err != nil || !strings.Contains(message, "'code' '") ||
		!strings.Contains(message, "Demo.code-workspace'") ||
		strings.Contains(message, "--file-uri") || strings.Contains(message, "--folder-uri") {
		t.Fatalf("manual workspace launch drifted: message=%q err=%v", message, err)
	}
}

func cloneRecord() domain.ProjectRecord {
	return domain.ProjectRecord{
		Schema: 1, ProjectID: "demo-12345678", Name: "Demo",
		HostPath: "https://example.invalid/demo.git", YardPath: "/srv/workspaces/demo-12345678/src",
		Mode: domain.ProjectGit, SSHHost: "yard", Target: "openclaw", ImportedAt: "2026-07-22T00:00:00Z",
	}
}

package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerinventory"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/state"
	"github.com/Subyard/Subyard/internal/testkit"
)

type projectRemovalProbe struct {
	requests []ports.InstanceExecRequest
	err      error
	failAt   int
}

type projectRemovalRemoteControl struct {
	remoteControlStub
	records []domain.RemoteRecord
}

func (control *projectRemovalRemoteControl) List(context.Context) ([]domain.RemoteRecord, error) {
	return append([]domain.RemoteRecord(nil), control.records...), nil
}

func (probe *projectRemovalProbe) Execute(
	_ context.Context,
	_ domain.Context,
	request ports.InstanceExecRequest,
) (ports.InstanceExecResult, error) {
	probe.requests = append(probe.requests, request)
	if probe.err != nil && (probe.failAt == 0 || len(probe.requests) == probe.failAt) {
		return ports.InstanceExecResult{}, probe.err
	}
	return ports.InstanceExecResult{Stdout: []byte("present"), ExitCode: 0}, nil
}

func (probe *projectRemovalProbe) Stream(
	context.Context,
	domain.Context,
	ports.InstanceExecRequest,
	io.Reader,
) (ports.InstanceExecResult, error) {
	return ports.InstanceExecResult{}, errors.New("unexpected stream")
}

func TestProjectRemovalVariantsUseTypedDefaultsAndDeclineWithoutMutation(t *testing.T) {
	for _, test := range []struct {
		name        string
		mode        domain.ProjectMode
		arguments   []string
		device      bool
		summary     string
		default_    domain.ConfirmationDefault
		wantProbe   bool
		consequence string
	}{
		{
			name: "soft", mode: domain.ProjectSync, arguments: []string{"remove", "--soft", "Demo"},
			summary: "Remove project registration and keep workspace", default_: domain.ConfirmationDefaultYes,
			consequence: "keep the project workspace",
		},
		{
			name: "bind detach", mode: domain.ProjectBind, arguments: []string{"remove", "Demo"}, device: true,
			summary: "Detach bound project", default_: domain.ConfirmationDefaultYes,
			consequence: "without deleting the host directory",
		},
		{
			name: "hard workspace", mode: domain.ProjectSync, arguments: []string{"remove", "Demo"},
			summary: "Permanently remove project workspace", default_: domain.ConfirmationDefaultNo,
			wantProbe: true, consequence: "delete the project workspace",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, environment, stateDirectory := nativeFixture(t)
			record := projectRemovalRecord(test.mode)
			store, err := state.NewFileStore(stateDirectory)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Put(context.Background(), record); err != nil {
				t.Fatal(err)
			}
			hostMarker := filepath.Join(root, "host-source-marker")
			if err := os.WriteFile(hostMarker, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			record.HostPath = hostMarker
			if err := store.Put(context.Background(), record); err != nil {
				t.Fatal(err)
			}
			incus := lifecycleIncus()
			instance := incus.Instances["subyard/yard"]
			instance.Status = "Running"
			if test.device {
				instance.Devices = map[string]map[string]string{
					state.WorkspaceDeviceFor(record): {"type": "disk", "source": hostMarker},
				}
			}
			incus.Instances["subyard/yard"] = instance
			probe := &projectRemovalProbe{}
			prompt := &testkit.Prompt{Answers: []bool{false}}
			runner := &testkit.ScriptedAdapter{}
			var stderr bytes.Buffer
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard", Arguments: test.arguments,
				Environment: environment, WorkingDir: root, Incus: incus, ProjectData: probe,
				AdapterRunner: runner, Prompt: prompt, Stderr: &stderr,
			})
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code != 1 ||
				!strings.Contains(stderr.String(), "operation declined") {
				t.Fatalf("code=%d stderr=%q", code, stderr.String())
			}
			if len(prompt.Requests) != 1 || prompt.Requests[0].Summary != test.summary ||
				prompt.Requests[0].Default != test.default_ ||
				!strings.Contains(strings.Join(prompt.Requests[0].Consequences, "\n"), test.consequence) {
				t.Fatalf("confirmation requests=%#v", prompt.Requests)
			}
			if len(runner.Requests) != 0 || (len(probe.requests) != 0) != test.wantProbe {
				t.Fatalf("decline applied or preflight drifted: adapter=%#v probes=%#v", runner.Requests, probe.requests)
			}
			if _, err := store.Get(context.Background(), record.ProjectID); err != nil {
				t.Fatalf("decline removed registry: %v", err)
			}
			if value, err := os.ReadFile(hostMarker); err != nil || string(value) != "keep" {
				t.Fatalf("host source changed: value=%q err=%v", value, err)
			}
		})
	}
}

func TestProjectWorkspaceRemovalExplicitYesCommitsOnlyAfterPhysicalSuccess(t *testing.T) {
	root, environment, stateDirectory := nativeFixture(t)
	environment = append(environment, "SUBYARD_OPERATION_ID=remove-workspace")
	record := projectRemovalRecord(domain.ProjectSync)
	store, err := state.NewFileStore(stateDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	incus := lifecycleIncus()
	instance := incus.Instances["subyard/yard"]
	instance.Status = "Running"
	incus.Instances["subyard/yard"] = instance
	probe := &projectRemovalProbe{}
	prompt := &testkit.Prompt{}
	runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{
		Schema: 1, OperationID: "remove-workspace", Status: "ok",
	}}}}
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"remove", "--yes", "Demo"},
		Environment: environment, WorkingDir: root, Incus: incus, ProjectData: probe,
		AdapterRunner: runner, Prompt: prompt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("remove returned %d", code)
	}
	if len(prompt.Requests) != 0 || len(runner.Requests) != 0 || len(probe.requests) != 4 {
		t.Fatalf("explicit consent flow: prompts=%#v adapters=%#v probes=%#v", prompt.Requests, runner.Requests, probe.requests)
	}
	if _, err := store.Get(context.Background(), record.ProjectID); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("successful removal retained registry: %v", err)
	}
}

func TestProjectWorkspaceRemovalFailureRetainsRegistry(t *testing.T) {
	root, environment, stateDirectory := nativeFixture(t)
	environment = append(environment, "SUBYARD_OPERATION_ID=remove-failure")
	record := projectRemovalRecord(domain.ProjectSync)
	store, err := state.NewFileStore(stateDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	incus := lifecycleIncus()
	instance := incus.Instances["subyard/yard"]
	instance.Status = "Running"
	incus.Instances["subyard/yard"] = instance
	probe := &projectRemovalProbe{err: errors.New("physical failure"), failAt: 2}
	runner := &testkit.ScriptedAdapter{}
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"remove", "--yes", "Demo"},
		Environment: environment, WorkingDir: root, Incus: incus, ProjectData: probe,
		AdapterRunner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 {
		t.Fatalf("remove returned %d", code)
	}
	if _, err := store.Get(context.Background(), record.ProjectID); err != nil {
		t.Fatalf("failed physical removal deleted registry: %v", err)
	}
}

func TestProjectWorkspaceRemovalPreconditionFailsBeforePrompt(t *testing.T) {
	root, environment, stateDirectory := nativeFixture(t)
	record := projectRemovalRecord(domain.ProjectSync)
	store, err := state.NewFileStore(stateDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	prompt := &testkit.Prompt{}
	runner := &testkit.ScriptedAdapter{}
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"remove", "Demo"},
		Environment: environment, WorkingDir: root, Incus: lifecycleIncus(),
		ProjectData: &projectRemovalProbe{}, AdapterRunner: runner, Prompt: prompt, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 || !strings.Contains(stderr.String(), "must be running") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if len(prompt.Requests) != 0 || len(runner.Requests) != 0 {
		t.Fatalf("failed preflight prompted/applied: prompts=%#v adapters=%#v", prompt.Requests, runner.Requests)
	}
}

func TestProjectSoftRemovalWorksWhenYardInstanceIsMissing(t *testing.T) {
	root, environment, stateDirectory := nativeFixture(t)
	environment = append(environment, "SUBYARD_OPERATION_ID=remove-stale-registration")
	record := projectRemovalRecord(domain.ProjectSync)
	store, err := state.NewFileStore(stateDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	incus := lifecycleIncus()
	delete(incus.Instances, "subyard/yard")
	probe := &projectRemovalProbe{err: errors.New("data plane must not be used")}
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"remove", "--soft", "--yes", "Demo"},
		Environment: environment, WorkingDir: root, Incus: incus, ProjectData: probe,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("soft removal returned %d", code)
	}
	if _, err := store.Get(context.Background(), record.ProjectID); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("stale registration was retained: %v", err)
	}
}

func TestProjectRemovalDeclineDoesNotRefreshRemoteOwnerInventory(t *testing.T) {
	root, environment, stateDirectory := nativeFixture(t)
	record := projectRemovalRecord(domain.ProjectSync)
	store, err := state.NewFileStore(stateDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	ownerRoot := filepath.Join(environmentValue(environment, "SUBYARD_HOME"), "owner-inventory")
	if err := (ownerinventory.Connections{Root: ownerRoot}).Write(ownerinventory.Connection{
		HostID: "remote-owner", Destination: "dev@remote.example",
		Yards: map[string]ownerinventory.YardRoute{"default": {SSHHost: "yard-remote"}},
	}); err != nil {
		t.Fatal(err)
	}
	cache := ownerinventory.Cache{Root: ownerRoot}
	if err := cache.Write(ownerinventory.Snapshot{
		FetchedAt: time.Unix(1, 0).UTC(), Inventory: inventoryResult("remote-owner", "default", "Remote").inventory,
	}); err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(ownerRoot, "owners", "remote-owner.json")
	before, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	prompt := &testkit.Prompt{Answers: []bool{false}}
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"remove", "--soft", "Demo"},
		Environment: environment, WorkingDir: root, Incus: lifecycleIncus(), Prompt: prompt, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 || !strings.Contains(stderr.String(), "operation declined") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	after, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("declined project removal refreshed remote owner inventory")
	}
}

func TestProjectRemovalDeclineDoesNotRepairOrMigrateLegacyLocalState(t *testing.T) {
	root, environment, stateDirectory := nativeFixture(t)
	record := projectRemovalRecord(domain.ProjectSync)
	store, err := state.NewFileStore(stateDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(stateDirectory, record.ProjectID+".json")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	prompt := &testkit.Prompt{Answers: []bool{false}}
	var stderr bytes.Buffer
	incus := lifecycleIncus()
	instance := incus.Instances["subyard/yard"]
	instance.Status = "Running"
	incus.Instances["subyard/yard"] = instance
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"remove", "--soft", "Demo"},
		Environment: environment, WorkingDir: root, Incus: incus, Prompt: prompt, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 || !strings.Contains(stderr.String(), "operation declined") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || afterInfo.Mode().Perm() != beforeInfo.Mode().Perm() {
		t.Fatalf("declined removal repaired or migrated legacy state: before=%#o after=%#o", beforeInfo.Mode().Perm(), afterInfo.Mode().Perm())
	}
}

func TestProjectRemovalRejectsUnsafeStateDirectoryBeforePrompt(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*testing.T, string, domain.ProjectRecord)
	}{
		{
			name: "broad permissions",
			setup: func(t *testing.T, directory string, record domain.ProjectRecord) {
				t.Helper()
				store, err := state.NewFileStore(directory)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.Put(context.Background(), record); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(directory, 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlink",
			setup: func(t *testing.T, directory string, record domain.ProjectRecord) {
				t.Helper()
				target := filepath.Join(t.TempDir(), "projects")
				store, err := state.NewFileStore(target)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.Put(context.Background(), record); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(directory), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, directory); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, environment, stateDirectory := nativeFixture(t)
			test.setup(t, stateDirectory, projectRemovalRecord(domain.ProjectSync))
			prompt := &testkit.Prompt{}
			var stderr bytes.Buffer
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard", Arguments: []string{"remove", "--soft", "Demo"},
				Environment: environment, WorkingDir: root, Incus: lifecycleIncus(),
				Prompt: prompt, Stderr: &stderr,
			})
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code != 1 ||
				!strings.Contains(stderr.String(), "state") {
				t.Fatalf("code=%d stderr=%q", code, stderr.String())
			}
			if len(prompt.Requests) != 0 {
				t.Fatalf("unsafe state reached prompt: %#v", prompt.Requests)
			}
		})
	}
}

func TestCanonicalOwnerProjectRemovalDeclinePreservesRoutingMetadata(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	dataHome := environmentValue(environment, "SUBYARD_HOME")
	ownerRoot := filepath.Join(dataHome, "owner-inventory")
	connection := ownerinventory.Connection{
		HostID: "remote-owner", Destination: "dev@127.0.0.1",
		Yards: map[string]ownerinventory.YardRoute{"default": {SSHHost: "yard-legacy-route"}},
	}
	if err := (ownerinventory.Connections{Root: ownerRoot}).Write(connection); err != nil {
		t.Fatal(err)
	}
	observedAt := time.Unix(100, 0).UTC()
	remoteInventory := inventoryResult("remote-owner", "default", "Remote").inventory
	remoteInventory.ObservedAt = observedAt
	if err := (ownerinventory.Cache{Root: ownerRoot}).Write(ownerinventory.Snapshot{
		FetchedAt: observedAt, Inventory: remoteInventory,
	}); err != nil {
		t.Fatal(err)
	}
	routeState := filepath.Join(ownerRoot, "routing", "remote-owner", "default", "projects")
	routeStore, err := state.NewFileStore(routeState)
	if err != nil {
		t.Fatal(err)
	}
	record := projectRemovalRecord(domain.ProjectSync)
	record.ProjectID = "remote-id"
	record.Name = "Remote"
	record.HostPath = "/remote/Remote"
	record.YardPath = state.YardPath(record.ProjectID)
	record.SSHHost = "yard-legacy-route"
	if err := routeStore.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	projectPath := filepath.Join(routeState, record.ProjectID+".json")
	if err := os.Chmod(projectPath, 0o644); err != nil {
		t.Fatal(err)
	}
	connectionPath := filepath.Join(ownerRoot, "connections", "remote-owner.json")
	cachePath := filepath.Join(ownerRoot, "owners", "remote-owner.json")
	beforeConnection, err := os.ReadFile(connectionPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeCache, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	beforeProject, err := os.ReadFile(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeProjectInfo, err := os.Stat(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	prompt := &testkit.Prompt{Answers: []bool{false}}
	control := &projectRemovalRemoteControl{records: []domain.RemoteRecord{{
		Spec: domain.RemoteSpec{
			LegacyAlias: "legacy-route", OwnerEndpoint: connection.Destination, OwnerYardName: "default",
		},
		Remote: true,
	}}}
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"-Y", "remote-owner/default", "remove", "--soft", "Remote"},
		Environment: environment, WorkingDir: root, RemoteControl: control, Prompt: prompt,
		Stderr: &stderr, Clock: testkit.NewManualClock(observedAt),
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 || !strings.Contains(stderr.String(), "operation declined") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if len(prompt.Requests) != 1 {
		t.Fatalf("canonical removal prompts=%#v", prompt.Requests)
	}
	afterConnection, err := os.ReadFile(connectionPath)
	if err != nil {
		t.Fatal(err)
	}
	afterCache, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	afterProject, err := os.ReadFile(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	afterProjectInfo, err := os.Stat(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeConnection, afterConnection) || !bytes.Equal(beforeCache, afterCache) ||
		!bytes.Equal(beforeProject, afterProject) ||
		beforeProjectInfo.Mode().Perm() != afterProjectInfo.Mode().Perm() {
		t.Fatalf("canonical decline mutated routing metadata: project mode before=%#o after=%#o",
			beforeProjectInfo.Mode().Perm(), afterProjectInfo.Mode().Perm())
	}
}

func projectRemovalRecord(mode domain.ProjectMode) domain.ProjectRecord {
	return domain.ProjectRecord{
		Schema: 1, ProjectID: "demo-12345678", Name: "Demo", HostPath: "/host/Demo",
		YardPath: state.YardPath("demo-12345678"), Mode: mode,
		SSHHost: "yard", Target: "yard",
	}
}

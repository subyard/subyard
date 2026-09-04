package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/state"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestDirectPreparedCommandPreservesFailurePhaseDiagnostics(t *testing.T) {
	t.Run("plan", func(t *testing.T) {
		root, environment, _ := nativeFixture(t)
		incus := lifecycleIncus()
		instance := incus.Instances["subyard/yard"]
		instance.Status = "Running"
		incus.Instances["subyard/yard"] = instance
		var stderr bytes.Buffer
		program, err := New(Options{
			RepositoryRoot: root, Program: "yard",
			Arguments: []string{"stop", "--force", "--yes"},
			Environment: append(environment,
				"SUBYARD_OPERATION_ID=invalid operation id",
			),
			WorkingDir: root, Incus: incus, Stderr: &stderr,
		})
		if err != nil {
			t.Fatal(err)
		}
		if code := program.Run(context.Background()); code != 1 ||
			!strings.Contains(stderr.String(), "yard: plan stop: ID source returned an invalid operation ID") {
			t.Fatalf("plan failure diagnostic: code=%d stderr=%q", code, stderr.String())
		}
	})

	t.Run("update preparation", func(t *testing.T) {
		root, environment, _ := updateReleaseFixture(t)
		var stderr bytes.Buffer
		program, err := New(Options{
			RepositoryRoot: root, Program: "yard",
			Arguments: []string{
				"update", "--rollback", "--runtime-root", filepath.Join(root, "missing-runtime"),
			},
			Environment: environment, WorkingDir: root, Stderr: &stderr,
		})
		if err != nil {
			t.Fatal(err)
		}
		if code := program.Run(context.Background()); code != 1 ||
			!strings.Contains(stderr.String(), "yard: update:") ||
			strings.Contains(stderr.String(), "yard: prepare update:") {
			t.Fatalf("update preparation diagnostic: code=%d stderr=%q", code, stderr.String())
		}
	})
}

func TestPreparedCommandResolverContract(t *testing.T) {
	root := repositoryRoot(t)
	program, err := New(Options{
		RepositoryRoot: root,
		WorkingDir:     root,
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		family preparedCommandFamily
		direct bool
		rpc    bool
	}{
		{name: "init", family: preparedCommandInit, direct: true, rpc: true},
		{name: "start", family: preparedCommandLifecycle, direct: true, rpc: true},
		{name: "provision", family: preparedCommandProvision, direct: true, rpc: true},
		{name: "test-vms", family: preparedCommandTestVMs, direct: true, rpc: true},
		{name: "stop", family: preparedCommandLifecycle, direct: true, rpc: true},
		{name: "teardown", family: preparedCommandTeardown, direct: true, rpc: true},
		{name: "sync", family: preparedCommandProject, direct: true, rpc: true},
		{name: "bind", family: preparedCommandProject, direct: true, rpc: true},
		{name: "clone", family: preparedCommandProject, direct: true, rpc: true},
		{name: "code", family: preparedCommandProject, direct: true, rpc: true},
		{name: "export", family: preparedCommandProject, direct: true, rpc: true},
		{name: "remove", family: preparedCommandProject, direct: true, rpc: true},
		{name: "up", family: preparedCommandProjectEnvironment, direct: true, rpc: true},
		{name: "down", family: preparedCommandProjectEnvironment, direct: true, rpc: true},
		{name: "info", family: preparedCommandProjectEnvironment, direct: true, rpc: false},
		{name: "remote", family: preparedCommandRemote, direct: true, rpc: false},
		{name: "update", family: preparedCommandRelease, direct: true, rpc: true},
		{name: "keys", direct: false, rpc: false},
		{name: "svc", direct: false, rpc: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			definition, ok := program.manifest.Lookup(test.name)
			if !ok {
				t.Fatalf("command %q is missing from the manifest", test.name)
			}
			resolved := resolveCommandPreparation(definition)
			if resolved.Direct != test.direct {
				t.Fatalf("direct=%v, want %v", resolved.Direct, test.direct)
			}
			if !resolved.Direct {
				if resolved.RPC {
					t.Fatal("unsupported direct command became RPC-plannable")
				}
				return
			}
			if resolved.Family != test.family {
				t.Fatalf("family=%q, want %q", resolved.Family, test.family)
			}
			if resolved.RPC != test.rpc {
				t.Fatalf("rpc=%v, want %v", resolved.RPC, test.rpc)
			}
		})
	}
	for alias, canonical := range map[string]string{"setup": "init", "uninstall": "teardown"} {
		definition, ok := program.manifest.Lookup(alias)
		if !ok || definition.Name != canonical {
			t.Fatalf("alias %q resolved to %#v, want %q", alias, definition, canonical)
		}
	}
}

func TestPrepareCommandOwnsCanonicalStopPlan(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	incus := lifecycleIncus()
	instance := incus.Instances["subyard/yard"]
	instance.Status = "Running"
	incus.Instances["subyard/yard"] = instance
	program, err := New(Options{
		RepositoryRoot: root,
		Environment: append(environment,
			"SUBYARD_OPERATION_ID=prepared-stop",
		),
		WorkingDir: root,
		Incus:      incus,
		Clock:      testkit.NewManualClock(time.Unix(100, 0)),
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
		t.Fatal("stop is missing from the manifest")
	}
	arguments := []string{"--force"}
	prepared, err := program.prepareCommand(context.Background(), prepareCommandRequest{
		Loaded:     loaded,
		Definition: definition,
		Arguments:  arguments,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	arguments[0] = "changed-after-prepare"
	if !slices.Equal(prepared.Arguments, []string{"--force"}) ||
		prepared.Plan.OperationID != "prepared-stop" || prepared.Plan.Command != "stop" ||
		prepared.Plan.Assessment == nil || prepared.Plan.Assessment.Action != "yard.stop-force" ||
		!prepared.Plan.Assessment.Changed || prepared.Plan.Confirmed {
		t.Fatalf("prepared stop lost canonical plan inputs: %#v", prepared)
	}
}

func TestPrepareCommandDirectAndRPCPlanInputsMatch(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T) (*CLI, prepareCommandRequest)
	}{
		{name: "stop --force", setup: prepareStopParityFixture},
		{name: "remove --soft", setup: prepareRemoveParityFixture},
		{name: "up --rebuild", setup: prepareUpParityFixture},
		{name: "teardown --keep-data", setup: prepareTeardownParityFixture},
		{name: "test-vms revoke --slot 1", setup: prepareTestVMParityFixture},
		{name: "no-op init", setup: prepareInitParityFixture},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			program, request := test.setup(t)
			request.Direct = true
			direct, err := program.prepareCommand(context.Background(), request)
			if err != nil {
				t.Fatalf("prepare direct command: %v", err)
			}
			defer direct.Close()

			request.Direct = false
			rpc, err := program.prepareCommand(context.Background(), request)
			if err != nil {
				t.Fatalf("prepare RPC command: %v", err)
			}
			defer rpc.Close()

			if !slices.Equal(direct.Arguments, rpc.Arguments) {
				t.Fatalf("canonical arguments differ: direct=%v RPC=%v", direct.Arguments, rpc.Arguments)
			}
			if direct.Plan.Command != rpc.Plan.Command || direct.Plan.Target != rpc.Plan.Target ||
				direct.Plan.Effect != rpc.Plan.Effect || direct.Plan.Confirmation != rpc.Plan.Confirmation ||
				!slices.Equal(direct.Plan.Consequences, rpc.Plan.Consequences) ||
				!reflect.DeepEqual(direct.Plan.Assessment, rpc.Plan.Assessment) ||
				!reflect.DeepEqual(direct.Plan.ConfirmationRequest, rpc.Plan.ConfirmationRequest) {
				t.Fatalf("plan inputs differ:\ndirect=%#v\nRPC=%#v", direct.Plan, rpc.Plan)
			}
		})
	}
}

func preparedParityFixture(
	t *testing.T,
	name string,
	arguments []string,
	configure func(string, string, *[]string, *Options),
) (*CLI, prepareCommandRequest) {
	t.Helper()
	root, environment, stateDirectory := nativeFixture(t)
	options := Options{
		RepositoryRoot: root,
		Environment: append(environment,
			"SUBYARD_OPERATION_ID=prepared-parity",
		),
		WorkingDir: root,
		Clock:      testkit.NewManualClock(time.Unix(100, 0)),
	}
	if configure != nil {
		configure(root, stateDirectory, &options.Environment, &options)
	}
	program, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	definition, ok := program.manifest.Lookup(name)
	if !ok {
		t.Fatalf("command %q is missing from the fixture manifest", name)
	}
	return program, prepareCommandRequest{
		Loaded: loaded, Definition: definition, Arguments: arguments, ExplicitYard: true,
	}
}

func prepareStopParityFixture(t *testing.T) (*CLI, prepareCommandRequest) {
	return preparedParityFixture(t, "stop", []string{"--force"}, func(
		_ string, _ string, _ *[]string, options *Options,
	) {
		incus := lifecycleIncus()
		instance := incus.Instances["subyard/yard"]
		instance.Status = "Running"
		incus.Instances["subyard/yard"] = instance
		options.Incus = incus
	})
}

func prepareRemoveParityFixture(t *testing.T) (*CLI, prepareCommandRequest) {
	return preparedParityFixture(t, "remove", []string{"--soft"}, func(
		root string, stateDirectory string, _ *[]string, options *Options,
	) {
		projectPath := filepath.Join(root, "Demo")
		if err := os.MkdirAll(projectPath, 0o700); err != nil {
			t.Fatal(err)
		}
		record := projectRemovalRecord(domain.ProjectSync)
		record.HostPath = projectPath
		record.SourceKey = state.SourceKey(projectPath)
		store, err := state.NewFileStore(stateDirectory)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Put(context.Background(), record); err != nil {
			t.Fatal(err)
		}
		options.WorkingDir = projectPath
		options.Incus = lifecycleIncus()
	})
}

func prepareUpParityFixture(t *testing.T) (*CLI, prepareCommandRequest) {
	return preparedParityFixture(t, "up", []string{"--rebuild"}, func(
		root string, stateDirectory string, _ *[]string, options *Options,
	) {
		manifestPath := filepath.Join(root, "config", "commands.registry")
		manifest, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		writeCLIFile(t, manifestPath, string(manifest)+
			"up||@project-env||forward|mutate|dynamic|public|project_env|project-env-up|up [project]|up|--rebuild --yes --help|\n", 0o600)
		projectPath := filepath.Join(root, "Demo")
		if err := os.MkdirAll(projectPath, 0o700); err != nil {
			t.Fatal(err)
		}
		record := projectRemovalRecord(domain.ProjectSync)
		record.HostPath = projectPath
		record.SourceKey = state.SourceKey(projectPath)
		record.Target = "fixture"
		record.Profile = "fixture"
		store, err := state.NewFileStore(stateDirectory)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Put(context.Background(), record); err != nil {
			t.Fatal(err)
		}
		profileDirectory := filepath.Join(root, "config", "profiles", "fixture")
		if err := os.MkdirAll(profileDirectory, 0o700); err != nil {
			t.Fatal(err)
		}
		writeCLIFile(t, filepath.Join(profileDirectory, "profile.conf"),
			"PROFILE_NAME=fixture\nPROJECT_ENV_BASE_IMAGE=debian:13\n", 0o600)
		incus := lifecycleIncus()
		instance := incus.Instances["subyard/yard"]
		instance.Status = "Running"
		incus.Instances["subyard/yard"] = instance
		options.WorkingDir = projectPath
		options.Incus = incus
		options.ProjectData = &testVMStatusProbe{output: []byte("missing")}
	})
}

func prepareTeardownParityFixture(t *testing.T) (*CLI, prepareCommandRequest) {
	return preparedParityFixture(t, "teardown", []string{"--keep-data"}, func(
		_ string, _ string, _ *[]string, options *Options,
	) {
		incus := lifecycleIncus()
		incus.Reconcile.InstanceFound = true
		options.Incus = incus
	})
}

func prepareTestVMParityFixture(t *testing.T) (*CLI, prepareCommandRequest) {
	return preparedParityFixture(t, "test-vms", []string{"revoke", "--slot", "1"}, func(
		_ string, _ string, environment *[]string, options *Options,
	) {
		*environment = append(*environment, "NESTED_E2E_VMS=1")
		incus := lifecycleIncus()
		instance := incus.Instances["subyard/yard"]
		instance.Status = "Running"
		incus.Instances["subyard/yard"] = instance
		options.Incus = incus
		options.ProjectData = &testVMStatusProbe{output: []byte(
			`{"schema_version":1,"status":"ok","pool":{"schema_version":1,"resource_type":"agent-e2e","resource_id":"test-vms","slots":[{"slot_id":"slot-001","resource_generation":7,"lease_epoch":1,"state":"held"}]}}`,
		)}
	})
}

func prepareInitParityFixture(t *testing.T) (*CLI, prepareCommandRequest) {
	return preparedParityFixture(t, "init", nil, func(
		root string, _ string, _ *[]string, options *Options,
	) {
		configHome := filepath.Join(root, "state")
		if err := os.MkdirAll(configHome, 0o700); err != nil {
			t.Fatal(err)
		}
		writeCLIFile(t, filepath.Join(configHome, "host-id"), "owner-fixture\n", 0o600)
		platform := newInitPlatformFixture()
		for stage := range platform.converged {
			platform.converged[stage] = true
		}
		options.InitPlatform = platform
	})
}

func TestPreparedCommandExecutesItsCapturedStopPlan(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	incus := lifecycleIncus()
	instance := incus.Instances["subyard/yard"]
	instance.Status = "Running"
	incus.Instances["subyard/yard"] = instance
	runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{
		Result: domain.AdapterResult{Schema: 1, OperationID: "prepared-execute", Status: "ok"},
	}}}
	program, err := New(Options{
		RepositoryRoot: root,
		Environment: append(environment,
			"SUBYARD_OPERATION_ID=prepared-execute",
		),
		WorkingDir:    root,
		Incus:         incus,
		AdapterRunner: runner,
		Clock:         testkit.NewManualClock(time.Unix(100, 0)),
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	definition, _ := program.manifest.Lookup("stop")
	prepared, err := program.prepareCommand(context.Background(), prepareCommandRequest{
		Loaded: loaded, Definition: definition, Arguments: []string{"--force"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	orchestrator := prepared.CLI.operationOrchestrator(
		prepared.Plan.OperationID, prepared.Loaded, nil, &prepared.Definition,
	)
	prepared.Plan, err = orchestrator.Confirm(context.Background(), prepared.Plan, true)
	if err != nil {
		t.Fatal(err)
	}
	result, err := prepared.Execute(context.Background(), orchestrator, io.Discard)
	if err != nil || result.Status != "ok" || len(runner.Requests) != 1 ||
		runner.Requests[0].Adapter != "lifecycle" || runner.Requests[0].Action != "stop" {
		t.Fatalf("result=%#v requests=%#v err=%v", result, runner.Requests, err)
	}
}

func TestPreparedCommandClassifiesProjectCommitFailure(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	incus := lifecycleIncus()
	instance := incus.Instances["subyard/yard"]
	instance.Status = "Running"
	incus.Instances["subyard/yard"] = instance
	runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{
		Result: domain.AdapterResult{Schema: 1, OperationID: "prepared-commit", Status: "ok"},
	}}}
	program, err := New(Options{
		RepositoryRoot: root,
		Environment: append(environment,
			"SUBYARD_OPERATION_ID=prepared-commit",
		),
		WorkingDir: root, Incus: incus, AdapterRunner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	definition, _ := program.manifest.Lookup("stop")
	prepared, err := program.prepareCommand(context.Background(), prepareCommandRequest{
		Loaded: loaded, Definition: definition, Arguments: []string{"--force"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	store, err := state.NewFileStore(secureTempDirectory(t))
	if err != nil {
		t.Fatal(err)
	}
	prepared.Project = &projectExecution{
		Store: store, Commit: projectCommitDelete,
		Record: domain.ProjectRecord{ProjectID: "invalid project"},
	}
	orchestrator := prepared.CLI.operationOrchestrator(
		prepared.Plan.OperationID, prepared.Loaded, nil, &prepared.Definition,
	)
	prepared.Plan, err = orchestrator.Confirm(context.Background(), prepared.Plan, true)
	if err != nil {
		t.Fatal(err)
	}
	_, err = prepared.Execute(context.Background(), orchestrator, io.Discard)
	var commitErr *preparedCommandCommitError
	if !errors.As(err, &commitErr) || !strings.Contains(err.Error(), "invalid project ID") {
		t.Fatalf("project commit error was not classified: %v", err)
	}
}

func TestPreparedCommandCloseReleasesProjectReservationOnce(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := state.NewFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := store.Admit(
		context.Background(), "operation-one", "/work/demo",
		domain.ProjectSync, "demo", false,
	)
	if err != nil {
		t.Fatal(err)
	}
	prepared := &preparedCommand{
		CLI: &CLI{},
		Project: &projectExecution{
			Store:       store,
			Reservation: reservation.Reservation,
		},
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Admit(
		context.Background(), "operation-two", "/work/demo",
		domain.ProjectSync, "demo", false,
	); err != nil {
		t.Fatalf("reservation remained after close: %v", err)
	}
}

func TestRPCHandlerCloseReleasesEveryPreparedCommand(t *testing.T) {
	store, err := state.NewFileStore(secureTempDirectory(t))
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := store.Admit(
		context.Background(), "operation-rpc-one", "/work/rpc-demo",
		domain.ProjectSync, "rpc-demo", false,
	)
	if err != nil {
		t.Fatal(err)
	}
	release := &releaseExecution{runtime: releaseruntime.New(releaseruntime.Config{})}
	handler := &rpcHandler{plans: map[string]*preparedCommand{
		"operation-rpc-one": {
			CLI:     &CLI{},
			Release: release,
			Project: &projectExecution{
				Store:       store,
				Reservation: reservation.Reservation,
			},
		},
	}}
	handler.closePreparedCommands()
	if len(handler.plans) != 0 {
		t.Fatalf("closed handler retained %d plans", len(handler.plans))
	}
	if release.runtime != nil {
		t.Fatal("RPC disconnect retained the prepared release handle")
	}
	if _, err := store.Admit(
		context.Background(), "operation-rpc-two", "/work/rpc-demo",
		domain.ProjectSync, "rpc-demo", false,
	); err != nil {
		t.Fatalf("RPC disconnect retained project reservation: %v", err)
	}
}

func secureTempDirectory(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func TestPreparedCommandResolverRejectsUnknownHandler(t *testing.T) {
	definition := command.Definition{
		Name:       "future",
		Handler:    "future-handler.sh",
		Effect:     command.EffectMutate,
		Visibility: command.VisibilityPublic,
	}
	resolved := resolveCommandPreparation(definition)
	if resolved.Direct || resolved.Family != preparedCommandUnknown || resolved.RPC {
		t.Fatalf("unknown handler resolved as %#v", resolved)
	}
}

func TestPreparedCommandManifestCompletenessRejectsUnownedMutation(t *testing.T) {
	manifest, err := command.Parse(strings.NewReader(
		"future||future-handler.sh||local|mutate|dynamic|public|lifecycle|simple|future|future command||\n",
	))
	if err != nil {
		t.Fatal(err)
	}
	if err := validatePreparedCommandManifest(manifest); err == nil ||
		!strings.Contains(err.Error(), "future") {
		t.Fatalf("unowned mutating handler passed validation: %v", err)
	}
}

func TestRepositoryPreparedCommandManifestIsComplete(t *testing.T) {
	program, err := New(Options{RepositoryRoot: repositoryRoot(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err := validatePreparedCommandManifest(program.manifest); err != nil {
		t.Fatal(err)
	}
}

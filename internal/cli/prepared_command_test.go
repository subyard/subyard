package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
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
		name     string
		prepared bool
		rpc      bool
	}{
		{name: "init", prepared: true, rpc: true},
		{name: "start", prepared: true, rpc: true},
		{name: "provision", prepared: true, rpc: true},
		{name: "test-vms", prepared: true, rpc: true},
		{name: "stop", prepared: true, rpc: true},
		{name: "teardown", prepared: true, rpc: true},
		{name: "sync", prepared: true, rpc: true},
		{name: "bind", prepared: true, rpc: true},
		{name: "clone", prepared: true, rpc: true},
		{name: "code", prepared: true, rpc: true},
		{name: "export", prepared: true, rpc: true},
		{name: "remove", prepared: true, rpc: true},
		{name: "up", prepared: true, rpc: true},
		{name: "down", prepared: true, rpc: true},
		{name: "info", prepared: true, rpc: false},
		{name: "remote", prepared: true, rpc: false},
		{name: "update", prepared: true, rpc: true},
		{name: "keys", rpc: false},
		{name: "svc", rpc: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			definition, ok := program.manifest.Lookup(test.name)
			if !ok {
				t.Fatalf("command %q is missing from the manifest", test.name)
			}
			behavior, err := resolveCoreCommand(definition)
			if err != nil {
				t.Fatal(err)
			}
			if (behavior.prepare != nil) != test.prepared {
				t.Fatalf("prepared=%v, want %v (reason=%q)", behavior.prepare != nil, test.prepared, behavior.nonRPCReason)
			}
			rpcEligible := definition.Visibility == command.VisibilityPublic &&
				definition.Effect == command.EffectMutate && behavior.prepare != nil &&
				behavior.nonRPCReason == ""
			if rpcEligible != test.rpc {
				t.Fatalf("rpc=%v, want %v", rpcEligible, test.rpc)
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
	var commitErr *commandCommitError
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
			CLI:           &CLI{},
			closeResource: release.Close,
			Project: &projectExecution{
				Store:       store,
				Reservation: reservation.Reservation,
			},
		},
	}}
	handler.closePlans()
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
		Handler:    "@future",
		Effect:     command.EffectMutate,
		Visibility: command.VisibilityPublic,
	}
	if _, err := resolveCoreCommand(definition); err == nil {
		t.Fatal("unknown internal handler was accepted")
	}
}

func TestPreparedCommandManifestCompletenessRejectsUnownedMutation(t *testing.T) {
	manifest, err := command.Parse(strings.NewReader(
		"future||@future||local|mutate|dynamic|public|lifecycle|simple|future|future command||\n",
	))
	if err != nil {
		t.Fatal(err)
	}
	definition := manifest.Commands()[0]
	if _, err := resolveCoreCommand(definition); err == nil ||
		!strings.Contains(err.Error(), "@future") {
		t.Fatalf("unowned mutating handler passed validation: %v", err)
	}
}

func TestRepositoryPreparedCommandManifestIsComplete(t *testing.T) {
	program, err := New(Options{RepositoryRoot: repositoryRoot(t)})
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range program.manifest.Commands() {
		if _, err := resolveCoreCommand(definition); err != nil {
			t.Fatalf("command %q: %v", definition.Name, err)
		}
	}
}

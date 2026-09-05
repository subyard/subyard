package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/shelladapter"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/state"
)

func TestPreparedCommandRequiresConfirmationExecutesOnceAndClosesOnce(t *testing.T) {
	executions := 0
	closes := 0
	prepared := &preparedCommand{
		CLI: &CLI{},
		Plan: domain.OperationPlan{OperationID: "lifecycle", Assessment: &domain.ActionAssessment{
			Action: "yard.stop", Effect: domain.ActionMutation, Changed: true,
		}},
		execute: func(context.Context, *application.Orchestrator, io.Writer) (domain.AdapterResult, error) {
			executions++
			return domain.AdapterResult{Schema: shelladapter.ProtocolSchema, OperationID: "lifecycle", Status: "ok"}, nil
		},
		closeResource: func() error {
			closes++
			return nil
		},
	}

	if _, err := prepared.Execute(context.Background(), &application.Orchestrator{}, io.Discard); !errors.Is(err, domain.ErrConfirmationRequired) {
		t.Fatalf("unconfirmed execution error = %v", err)
	}
	if executions != 0 {
		t.Fatalf("unconfirmed command executed %d times", executions)
	}
	prepared.Plan.Confirmed = true
	result, err := prepared.Execute(context.Background(), &application.Orchestrator{}, io.Discard)
	if err != nil || result.Status != "ok" || executions != 1 {
		t.Fatalf("first execution: result=%#v executions=%d err=%v", result, executions, err)
	}
	if _, err := prepared.Execute(context.Background(), &application.Orchestrator{}, io.Discard); err == nil || executions != 1 {
		t.Fatalf("repeated execution: executions=%d err=%v", executions, err)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Close(); err != nil || closes != 1 {
		t.Fatalf("repeated close: closes=%d err=%v", closes, err)
	}
	if _, err := prepared.Execute(context.Background(), &application.Orchestrator{}, io.Discard); err == nil || executions != 1 {
		t.Fatalf("closed command executed: executions=%d err=%v", executions, err)
	}
}

func TestPreparedCommandClosePreservesResourceError(t *testing.T) {
	closeErr := errors.New("release cleanup failed")
	calls := 0
	prepared := &preparedCommand{closeResource: func() error {
		calls++
		return closeErr
	}}
	for range 2 {
		if err := prepared.Close(); !errors.Is(err, closeErr) {
			t.Fatalf("close error = %v, want %v", err, closeErr)
		}
	}
	if calls != 1 {
		t.Fatalf("resource closed %d times", calls)
	}
}

func TestPreparedProjectExecutionOwnsReservationAndCommitLifecycle(t *testing.T) {
	for _, test := range []struct {
		name    string
		runErr  error
		commits bool
	}{
		{name: "failure releases without commit", runErr: errors.New("physical failure")},
		{name: "success commits", commits: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, environment, stateDirectory := preparationParityFixture(t)
			projectPath := filepath.Join(root, "Project")
			if err := os.MkdirAll(projectPath, 0o700); err != nil {
				t.Fatal(err)
			}
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard", Environment: append(environment, "SUBYARD_OPERATION_ID=project-lifecycle"),
				WorkingDir: root, Incus: lifecycleIncus(), Stdout: io.Discard, Stderr: io.Discard,
			})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			definition, ok := program.manifest.Lookup("sync")
			if !ok {
				t.Fatal("sync command missing from manifest")
			}
			prepared, err := program.prepareCommand(context.Background(), prepareCommandRequest{
				Loaded: loaded, Definition: definition, Arguments: []string{projectPath}, ExplicitYard: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Close()
			runs := 0
			prepared.execute = func(context.Context, *application.Orchestrator, io.Writer) (domain.AdapterResult, error) {
				runs++
				return domain.AdapterResult{Schema: shelladapter.ProtocolSchema, OperationID: prepared.Plan.OperationID, Status: "ok"}, test.runErr
			}
			orchestrator := program.operationOrchestrator(prepared.Plan.OperationID, prepared.Loaded, nil, &prepared.Definition)
			prepared.Plan, err = orchestrator.Confirm(context.Background(), prepared.Plan, true)
			if err != nil {
				t.Fatal(err)
			}
			_, executeErr := prepared.Execute(context.Background(), orchestrator, io.Discard)
			if !errors.Is(executeErr, test.runErr) || runs != 1 {
				t.Fatalf("execute: runs=%d err=%v", runs, executeErr)
			}
			if err := prepared.Close(); err != nil {
				t.Fatal(err)
			}
			if prepared.Project.Reservation != nil {
				t.Fatalf("reservation retained after close: %#v", prepared.Project.Reservation)
			}
			store, err := state.NewFileStore(stateDirectory)
			if err != nil {
				t.Fatal(err)
			}
			record, getErr := store.Get(context.Background(), prepared.Project.Record.ProjectID)
			if test.commits {
				if getErr != nil || record.HostPath != projectPath {
					t.Fatalf("successful project state = %#v, err=%v", record, getErr)
				}
			} else if !errors.Is(getErr, state.ErrNotFound) {
				t.Fatalf("failed execution committed project state: %#v, err=%v", record, getErr)
			}
		})
	}
}

func TestCoreCommandResolverIsCompleteAndRejectsUnknownInternalHandler(t *testing.T) {
	root, environment, _ := preparationParityFixture(t)
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root})
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range program.manifest.Commands() {
		behavior, err := resolveCoreCommand(definition)
		if err != nil {
			t.Errorf("manifest command %q has no behavior: %v", definition.Name, err)
			continue
		}
		if behavior.prepare == nil && behavior.nonRPCReason == "" {
			t.Errorf("manifest command %q is neither preparable nor explicitly excluded", definition.Name)
		}
	}
	for _, handler := range []string{"", "@unknown"} {
		if _, err := resolveCoreCommand(command.Definition{Name: "unknown", Handler: handler}); err == nil {
			t.Errorf("unknown internal handler %q was accepted", handler)
		}
	}
}

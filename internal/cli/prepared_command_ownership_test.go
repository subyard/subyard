package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/shelladapter"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/state"
)

func TestPreparedCommandRequiresConfirmationExecutesOnceAndClosesOnce(t *testing.T) {
	executions := 0
	closes := 0
	prepared := &preparedCommand{lifecycle: &preparedLifecycle{},
		CLI:  &CLI{},
		Plan: domain.OperationPlan{OperationID: "lifecycle", Command: "fixture", Effect: domain.CommandMutate, Confirmation: domain.ConfirmationPromptDefaultYes, Target: domain.TargetLocalController},
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
	prepared.seal()
	if _, err := prepared.Confirm(context.Background(), &application.Orchestrator{}, true); err != nil {
		t.Fatal(err)
	}
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
	prepared := &preparedCommand{lifecycle: &preparedLifecycle{}, closeResource: func() error {
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
				ProjectData: projectActionObservationProbe{execute: func(ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
					return ports.InstanceExecResult{}, nil
				}},
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
			_, err = prepared.Confirm(context.Background(), orchestrator, true)
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

// Unrelated execution fixtures may bypass prompting; authority regressions use Confirm.
func authorizePreparedFixture(prepared *preparedCommand) {
	prepared.Plan.Confirmed = true
	prepared.seal()
	prepared.lifecycle.phase = commandAuthorized
}

func TestPreparedAuthorityRejectsForgedAndTamperedPreview(t *testing.T) {
	for _, when := range []string{"before confirmation", "after confirmation"} {
		t.Run(when, func(t *testing.T) {
			runs := 0
			prepared := authorityFixture(&runs)
			defer prepared.Close()
			orchestrator := &application.Orchestrator{}
			if when == "after confirmation" {
				if _, err := prepared.Confirm(context.Background(), orchestrator, true); err != nil {
					t.Fatal(err)
				}
			}
			prepared.Plan.Confirmed = true
			prepared.Plan.Consequences[0] = "expanded effect"
			if _, err := prepared.Execute(context.Background(), orchestrator, io.Discard); err == nil || runs != 0 {
				t.Fatalf("tampered preview executed: runs=%d err=%v", runs, err)
			}
			if _, err := prepared.Confirm(context.Background(), orchestrator, true); err == nil {
				t.Fatal("tampered preview granted approval")
			}
		})
	}
	var nilCommand *preparedCommand
	for _, prepared := range []*preparedCommand{nilCommand, {}} {
		if _, err := prepared.Execute(context.Background(), &application.Orchestrator{}, io.Discard); err == nil {
			t.Fatal("nil/zero command executed")
		}
		if err := prepared.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func authorityFixture(runs *int) *preparedCommand {
	prepared := &preparedCommand{lifecycle: &preparedLifecycle{}, CLI: &CLI{}, Plan: domain.OperationPlan{
		OperationID: "authority", Command: "fixture", Effect: domain.CommandMutate,
		Confirmation: domain.ConfirmationPromptDefaultYes, Target: domain.TargetLocalController,
		Consequences: []string{"bounded effect"},
	}}
	prepared.execute = func(context.Context, *application.Orchestrator, io.Writer) (domain.AdapterResult, error) {
		*runs++
		return domain.AdapterResult{Status: "ok"}, nil
	}
	prepared.seal()
	return prepared
}

func TestPreparedCopiesConsumeAndCloseTogether(t *testing.T) {
	runs, closes := 0, 0
	prepared := authorityFixture(&runs)
	prepared.closeResource = func() error { closes++; return nil }
	if _, err := prepared.Confirm(context.Background(), &application.Orchestrator{}, true); err != nil {
		t.Fatal(err)
	}
	alias := *prepared
	start := make(chan struct{})
	var wait sync.WaitGroup
	for _, handle := range []*preparedCommand{prepared, &alias} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, _ = handle.Execute(context.Background(), &application.Orchestrator{}, io.Discard)
			_ = handle.Close()
		}()
	}
	close(start)
	wait.Wait()
	if runs != 1 || closes != 1 {
		t.Fatalf("copies: runs=%d closes=%d", runs, closes)
	}
}

func TestPreparedAdmissionFailureConsumesApproval(t *testing.T) {
	runs := 0
	prepared := authorityFixture(&runs)
	prepared.Loaded.Context.AccessKind = domain.AccessLocal
	prepared.ownerAdmission.epoch = 1
	// An obsolete owner epoch cannot reach physical owner mutation.
	prepared.ownerAdmission.home = filepath.Join(t.TempDir(), "missing", "owner")
	prepared.seal()
	if _, err := prepared.Confirm(context.Background(), &application.Orchestrator{}, true); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := prepared.Execute(ctx, &application.Orchestrator{}, io.Discard); err == nil {
		t.Fatal("stale owner admission succeeded")
	}
	if _, err := prepared.Execute(context.Background(), &application.Orchestrator{}, io.Discard); err == nil || runs != 0 {
		t.Fatalf("admission retried: runs=%d err=%v", runs, err)
	}
	_ = prepared.Close()
}

func TestPreparedRemoteForwardingConsumesBeforePreparation(t *testing.T) {
	runs, forwards := 0, 0
	prepared := authorityFixture(&runs)
	prepared.CLI.options.Stdout, prepared.CLI.options.Stderr = io.Discard, io.Discard
	prepared.Plan.Target = domain.TargetRemoteOwner
	prepared.remoteArguments = func(arguments []string) ([]string, error) {
		forwards++
		if prepared.lifecycle.phase != commandConsumed {
			t.Fatal("forwarding started without consuming approval")
		}
		return nil, errors.New("fixture refuses before SSH")
	}
	prepared.seal()
	alias := *prepared
	if code := prepared.CLI.runPreparedCommand(context.Background(), prepared, true); code != 1 {
		t.Fatalf("forwarding code = %d", code)
	}
	alias.Plan = prepared.Plan.Clone()
	if _, err := alias.Execute(context.Background(), &application.Orchestrator{}, io.Discard); err == nil || runs != 0 || forwards != 1 {
		t.Fatalf("forwarding approval reused: err=%v runs=%d forwards=%d", err, runs, forwards)
	}
	_ = prepared.Close()
}

// Direct adapter-leaf fixtures use real approval without testing prompt rendering.
func confirmedAdapterFixture(t *testing.T, orchestrator *application.Orchestrator, plan domain.OperationPlan) domain.OperationPlan {
	t.Helper()
	plan.Confirmed = false
	if plan.Command == "" {
		plan.Command = "fixture"
	}
	if plan.Effect == "" {
		plan.Effect = domain.CommandMutate
	}
	if plan.Confirmation == "" {
		plan.Confirmation = domain.ConfirmationPromptDefaultYes
	}
	if plan.OperationID == "" {
		plan.OperationID = "fixture"
	}
	confirmed, err := orchestrator.Confirm(context.Background(), plan, true)
	if err != nil {
		t.Fatal(err)
	}
	return confirmed
}

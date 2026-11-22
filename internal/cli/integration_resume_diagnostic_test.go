package cli

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

type pendingInventoryExecutor struct {
	installedSettingsExecutor
	selectionOnlyIntegrationExecutor
	pending bool
}

func (e *pendingInventoryExecutor) Exec(ctx context.Context, project, instance string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	if len(request.Command) == 5 && request.Command[0] == "python3" && request.Command[3] == reconcileruntime.IntegrationInventoryReader() {
		switch request.Command[4] {
		case "settings", "selection":
			return e.installedSettingsExecutor.Exec(ctx, project, instance, request)
		case "observe", "observe-applied":
			body, _ := json.Marshal(map[string]any{"fingerprint": strings.Repeat("a", 64), "changed": false, "pending": e.pending})
			return ports.InstanceExecResult{Stdout: body}, nil
		default:
			return ports.InstanceExecResult{}, errors.New("unexpected inventory mutation")
		}
	}
	if len(request.Command) == 4 && strings.HasPrefix(request.Command[3], "__name__ = 'subyard_") {
		return e.installedSettingsExecutor.Exec(ctx, project, instance, request)
	}
	if len(request.Command) == 4 && request.Command[0] == "test" && request.Command[1] == "!" {
		return ports.InstanceExecResult{ExitCode: 1}, &ports.GuestExitError{Code: 1}
	}
	return e.selectionOnlyIntegrationExecutor.Exec(ctx, project, instance, request)
}

func TestCompletedReleaseObservationPreservesPendingInitTail(t *testing.T) {
	for _, complete := range []bool{false, true} {
		for _, mode := range []releasetransition.ProcessMode{releasetransition.ProcessInspect, releasetransition.ProcessConverge} {
			f := newPendingInitFixture(t, false, nil)
			request, journal := repairRequest(t, f)
			request.Mode = mode
			if !complete {
				journal.Checkpoint = releasetransition.JournalReconciling
				journal.Releases.From = journal.Releases.Target
				payload, err := releasetransition.MarshalJournal(journal)
				if err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, f.journalPath, payload, 0600)
			}
			ctx := context.Background()
			loaded, err := f.cli.resolveReleaseTransitionContext(ctx, "default", request.ConfigHome)
			if err != nil {
				t.Fatal(err)
			}
			e := &pendingInventoryExecutor{installedSettingsExecutor: f.cli.options.Executor.(installedSettingsExecutor),
				selectionOnlyIntegrationExecutor: selectionOnlyIntegrationExecutor{source: loaded.Environment["AGENT_codex_RULES"]}}
			options := f.cli.options
			options.Executor, options.InitPlatform = e, nil
			native := options.Incus.(*testkit.Incus)
			native.Reconcile = ports.ReconcileState{InstanceFound: true, Instance: native.Instances[loaded.Context.IncusProject+"/"+loaded.Context.YardInstanceName]}
			var before releasetransition.V2ActivationObservation
			for _, pending := range []bool{false, true} {
				e.pending = pending
				program := mustNew(t, options)
				actor := &materializedConfigActivationReconciler{cli: program.releaseActivationOperation(request), configHome: request.ConfigHome,
					goal: journal.Goal, artifactDigest: journal.ArtifactDigest, registryDigest: journal.RegistryDigest}
				observed, err := actor.Observe(ctx, journal.Releases, releasetransition.ReleaseLinks{})
				if err != nil {
					t.Fatal(err)
				}
				if !pending {
					before = observed
				} else if complete && !reflect.DeepEqual(before, observed) {
					t.Fatalf("completed release changed after failed init: before=%#v after=%#v", before, observed)
				} else if !complete && (observed.Converged || !strings.Contains(strings.Join(observed.Consequences, "\n"), "reconcile selected integrations")) {
					t.Fatalf("unfinished repair lost its pending apply tail: %#v", observed)
				}
			}
		}
	}
}

type resumeConflictCause struct{ error }

func (resumeConflictCause) ActivationConflict() bool { return true }
func (cause resumeConflictCause) Unwrap() error      { return cause.error }

type resumeTransientCause struct{}

func (resumeTransientCause) Error() string { return "private dependency error" }
func (resumeTransientCause) ActivationDiagnostic() (string, string) {
	return "native dependency unavailable", "run yard -Y default integration status"
}

func TestIntegrationApplyFailurePreservesPublicCauseAndExecutableRetry(t *testing.T) {
	for _, test := range []struct {
		cause          error
		message, retry string
	}{
		{resumeConflictCause{publicReconcileTestError{}}, "known integration ownership conflict", "run yard -Y default integration status"},
		{resumeTransientCause{}, "native dependency unavailable", "run yard migrate"},
		{independentOwnershipTestError("/home/dev/settings"), "ownership conflict at /home/dev/settings", "inspect /home/dev/settings"},
		{errors.New("private native stderr"), "yard sample: cannot reconcile integrations for release activation (stage=apply projected=false)", "run yard migrate"},
		{&application.ProjectHookFailure{Yard: "sample", Reason: "timed out", Hooks: []string{"/hooks/sample"}}, "yard sample: project hook /hooks/sample failed; yard sample: optional agent project hooks timed out", "run yard migrate"},
		{&application.ProjectHookFailure{Yard: "sample", Reason: "timed out"}, "yard sample: optional agent project hooks timed out; the hook pass failed or its state is undetermined, see the integration's status", "run yard migrate"},
	} {
		failure := &integrationActivationFailure{cause: test.cause, stage: "apply"}
		boundary := materializedConfigActivationError{cause: failure, yard: "sample", phase: "integrations", action: "reconcile", command: "integration status"}
		message, retry := boundary.ActivationDiagnostic()
		if message != test.message || retry != test.retry || strings.Contains(message, "private native stderr") {
			t.Fatalf("diagnostic = %q / %q", message, retry)
		}
	}
}

func TestIntegrationApplyStepRendersAtActivationBoundary(t *testing.T) {
	cliFixture := newFixture(t, withCheckoutConfig(), withWorkingDir(""))
	root := cliFixture.Root
	runtime := reconcileruntime.Runtime{RepositoryRoot: root, Executor: resumeConflictExecutor{"apply"},
		Incus:        &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{Status: "Running"}}},
		Yard:         domain.Context{YardName: "sample", DevUser: "dev", DevUID: 1000},
		Integrations: []profile.IntegrationDefinition{}}
	ctx := context.Background()
	plan, err := runtime.IntegrationPlan(ctx)
	if err != nil || !plan.Changed {
		t.Fatalf("fixture plan: changed=%t, %v", plan.Changed, err)
	}
	cause := runtime.ApplyIntegrations(ctx, plan)
	if cause == nil {
		t.Fatal("inventory apply was expected to fail")
	}
	boundary := materializedConfigActivationError{
		cause: &integrationActivationFailure{cause: cause, stage: "apply", projected: true},
		yard:  "sample", phase: "integrations", action: "reconcile", command: "integration status",
	}
	message, retry := boundary.ActivationDiagnostic()
	if message != "integration apply inventory update failed in yard sample" || retry != "run yard migrate" {
		t.Fatalf("activation diagnostic = %q / %q", message, retry)
	}
}

type resumeConflictExecutor struct{ kind string }

func (e resumeConflictExecutor) Exec(_ context.Context, _, _ string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	if strings.Contains(string(request.Stdin), `"mappings"`) && e.kind == "persist" {
		return ports.InstanceExecResult{ExitCode: 1, Stderr: []byte(`{"conflicts":[{"reason":"persist filesystem error","path":"","integration":"sample"}]}`)}, nil
	}
	if len(request.Command) >= 5 && request.Command[3] == reconcileruntime.IntegrationInventoryReader() {
		if e.kind == "apply" {
			if request.Command[4] == "apply" {
				return ports.InstanceExecResult{Stderr: []byte("private native stderr")}, errors.New("private native error")
			}
			return ports.InstanceExecResult{Stdout: []byte(`{"fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","changed":true}`)}, nil
		}
		return ports.InstanceExecResult{Stdout: []byte(`{"fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","changed":false}`)}, nil
	}
	if strings.Contains(strings.Join(request.Command, " "), "mode=$1") {
		if e.kind == "ccusage" {
			return ports.InstanceExecResult{ExitCode: 1, Stderr: []byte("subyard-integration-conflict:ccusage\n")}, nil
		}
		return ports.InstanceExecResult{Stdout: []byte("changed=0\n")}, nil
	}
	return ports.InstanceExecResult{}, nil
}

func TestIntegrationActivationConflictRetriesRemainInspection(t *testing.T) {
	for _, kind := range []string{"ccusage", "persist"} {
		t.Run(kind, func(t *testing.T) {
			cliFixture := newFixture(t, withCheckoutConfig(), withWorkingDir(""))
			root := cliFixture.Root
			runtime := reconcileruntime.Runtime{RepositoryRoot: root, Executor: resumeConflictExecutor{kind},
				Incus:        &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{Status: "Running"}}},
				Yard:         domain.Context{YardName: "sample", DevUser: "dev", DevUID: 1000},
				Integrations: []profile.IntegrationDefinition{{Name: "sample"}}}
			if kind == "ccusage" {
				runtime.Integrations = []profile.IntegrationDefinition{}
			}
			if kind == "persist" {
				runtime.Environment = []string{"AGENT_sample_PERSIST=.memory:/mnt/host/agent-sessions/sample/memory"}
			}
			_, cause := runtime.IntegrationPlan(context.Background())
			var diagnostic interface{ ActivationDiagnostic() (string, string) }
			if cause == nil || !errors.As(cause, &diagnostic) {
				t.Fatalf("missing native conflict: %v", cause)
			}
			wantMessage, wantRetry := diagnostic.ActivationDiagnostic()
			for _, wrapped := range []error{cause, &integrationActivationFailure{cause: cause, stage: "apply"}} {
				boundary := materializedConfigActivationError{cause: wrapped, yard: "sample", phase: "integrations", action: "reconcile", command: "integration status"}
				message, retry := boundary.ActivationDiagnostic()
				if message != wantMessage || retry != wantRetry || strings.Contains(retry, "migrate") {
					t.Fatalf("conflict retry changed: %q / %q", message, retry)
				}
			}
		})
	}
}

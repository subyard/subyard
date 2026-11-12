package cli

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
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
				program, err := New(options)
				if err != nil {
					t.Fatal(err)
				}
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
	} {
		failure := &integrationActivationFailure{cause: test.cause, stage: "apply"}
		boundary := materializedConfigActivationError{cause: failure, yard: "sample", phase: "integrations", action: "reconcile", command: "integration status"}
		message, retry := boundary.ActivationDiagnostic()
		if message != test.message || retry != test.retry || strings.Contains(message, "private native stderr") {
			t.Fatalf("diagnostic = %q / %q", message, retry)
		}
	}
}

type resumeConflictExecutor struct{ kind string }

func (e resumeConflictExecutor) Exec(_ context.Context, _, _ string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	if strings.Contains(string(request.Stdin), `"mappings"`) && e.kind == "persist" {
		return ports.InstanceExecResult{ExitCode: 1, Stderr: []byte(`{"conflicts":[{"reason":"persist filesystem error","path":"","integration":"sample"}]}`)}, nil
	}
	if len(request.Command) >= 4 && request.Command[3] == reconcileruntime.IntegrationInventoryReader() {
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
	for _, kind := range []string{"foreign-ownership", "ambiguous-ownership", "ccusage", "persist"} {
		t.Run(kind, func(t *testing.T) {
			root, _, _, _ := configCommandFixture(t)
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
			if strings.HasSuffix(kind, "ownership") {
				handlerRoot := testkit.TempDir(t)
				testkit.WriteFile(t, filepath.Join(handlerRoot, "integration.json"), []byte(`{"schema_version":1}`), 0644)
				testkit.WriteFile(t, filepath.Join(handlerRoot, "hook.sh"), []byte("#!/bin/sh\nprintf 'subyard-lifecycle-diagnostic-v1 inspect "+kind+"\\n' >&2\nexit 1\n"), 0755)
				runtime.Integrations[0].Root = handlerRoot
				runtime.Integrations[0].Lifecycle = &profile.IntegrationLifecycle{Handler: "hook.sh"}
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

package cli

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

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

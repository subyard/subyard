package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/shellquote"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestMaterializedConfigReconcileIdentifiesFailedYard(t *testing.T) {
	for _, test := range []struct {
		name           string
		cause          error
		message, retry string
	}{
		{"raw", errors.New("private transport detail"), "yard default: cannot reconcile integrations for release activation", "run yard -Y default integration status"},
		{"stale", domain.ErrPlanStale, "yard default: cannot reconcile integrations for release activation", "run yard -Y default integration status"},
		{"public", fmt.Errorf("private wrapper: %w", publicReconcileTestError{}), "known integration ownership conflict", "run yard -Y default integration status"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, _, configHome, environment := configCommandFixture(t)
			fake := &testkit.Incus{Err: test.cause}
			var stdout, stderr bytes.Buffer
			program, err := New(Options{RepositoryRoot: root, Environment: environment,
				Incus: fake, Executor: fake, Stdout: &stdout, Stderr: &stderr})
			if err != nil {
				t.Fatal(err)
			}
			reconciler := &materializedConfigActivationReconciler{
				cli: program, yard: "default", configHome: configHome,
				integrationPlans: map[string]reconcileruntime.IntegrationPlan{"default": {}},
			}
			err = reconciler.Reconcile(context.Background(), releasetransition.ReleaseLinks{})
			var diagnostic interface{ ActivationDiagnostic() (string, string) }
			if !errors.Is(err, test.cause) || !errors.As(err, &diagnostic) {
				t.Fatalf("integration error lost its cause or public context: %v", err)
			}
			message, retry := diagnostic.ActivationDiagnostic()
			if message != test.message || retry != test.retry {
				t.Fatalf("integration diagnostic = %q / %q", message, retry)
			}
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatal("reconciler printed private failure details")
			}
		})
	}
}

type publicReconcileTestError struct{}

func (publicReconcileTestError) Error() string { return "private cause" }
func (publicReconcileTestError) ActivationDiagnostic() (string, string) {
	return "known integration ownership conflict", "run yard -Y default integration status"
}

// Direct integration status retains the full public hint outside the frozen
// release protocol, including independently parseable cleanup command lines.
func TestIntegrationStatusRendersOwnedCleanupCommands(t *testing.T) {
	command := []string{"/tmp/candidate with 'quotes'/bin/yard-engine", "-Y", "example", "integration", "cleanup", "fixture"}
	apply := shellquote.Command(command)
	check := shellquote.Command(append(command, "--check"))
	detail := "known integration ownership conflict\n" + check + "\nThen run with confirmation:\n" + apply
	var stdout bytes.Buffer
	program := &CLI{options: Options{Stdout: &stdout}}
	program.printIntegrationStatus(integrationStatus{Yard: "example", ID: "fixture", Observed: "conflict", Detail: detail}, false)
	lines := strings.Split(stdout.String(), "\n")
	for _, wanted := range []string{check, apply} {
		count := 0
		for _, line := range lines {
			if line == wanted {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("activation lost separate cleanup argv %q: %q", wanted, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "private") || strings.Contains(stdout.String(), "--yes") {
		t.Fatalf("unsafe activation output: %q", stdout.String())
	}
}

func TestActivationProjectsNativeOwnershipExitError(t *testing.T) {
	root, _, _, _ := configCommandFixture(t)
	fake := &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{Status: "Running"}}, ExecSteps: []testkit.IncusExecStep{
		{},
		{Result: ports.InstanceExecResult{Stdout: []byte(`{"fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","changed":false}`)}},
		{},
		{Result: ports.InstanceExecResult{ExitCode: 1, Stderr: []byte("private guest detail\nsubyard-integration-conflict:ccusage\n")}, Err: errors.New("private guest exit error")},
	}}
	runtime := reconcileruntime.Runtime{RepositoryRoot: root, Incus: fake, Executor: fake, Integrations: []profile.IntegrationDefinition{}, Yard: domain.Context{YardName: "example"}}
	_, cause := runtime.IntegrationPlan(context.Background())
	if cause == nil {
		t.Fatal("native ownership conflict was accepted")
	}
	boundary := materializedConfigActivationError{cause: cause, yard: "example", phase: "integration plan", action: "inspect", command: "integration status"}
	message, retry := boundary.ActivationDiagnostic()
	var stdout bytes.Buffer
	program := &CLI{options: Options{Stdout: &stdout}}
	program.printUpdateResult(&releaseExecution{prepared: releaseruntime.Prepared{Action: "update.activate"}, outcome: &releasetransition.Outcome{Status: releasetransition.StatusOperatorActionRequired, Message: message, Retry: retry}}, false)
	if !strings.Contains(stdout.String(), "utility preserved") || strings.Contains(stdout.String(), "private") || strings.Contains(stdout.String(), "cannot inspect") {
		t.Fatalf("native public diagnostic was lost or unsafe: %q", stdout.String())
	}
}

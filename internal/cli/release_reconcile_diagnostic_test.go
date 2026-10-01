package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
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

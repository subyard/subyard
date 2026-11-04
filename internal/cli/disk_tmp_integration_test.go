package cli

import (
	"context"
	"maps"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

type integrationGuestCalls struct{ calls int }

func (executor *integrationGuestCalls) Exec(ctx context.Context, project, name string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	executor.calls++
	return (activationIntegrationExecutor{}).Exec(ctx, project, name, request)
}

// A captured running integration plan must not be replayed on a stopped yard
// even after it stops. Skipping leaves the saved target and scope unchanged.
func TestStoppedMaterializedIntegrationPlanIsNotReplayed(t *testing.T) {
	for _, scenario := range []string{"running", "stopped"} {
		for _, projected := range []bool{false, true} {
			name := scenario
			if projected {
				name += "/projected"
			} else {
				name += "/ordinary"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				root, _, _, environment := configCommandFixture(t)
				loaded := loadConfigCommandContext(t, root, environment, "default")
				loaded.Integrations = config.IntegrationSelection{Present: true, AllowsCodingTools: true}
				yard := loaded.Context
				values := map[string]string{"volatile.uuid": "uuid-original", "user.subyard.name": "default", "user.subyard.managed": "true"}
				native := &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{Project: yard.IncusProject, Name: yard.YardInstanceName, Status: "Running", Config: values, LocalConfig: maps.Clone(values)}}}
				executor := &integrationGuestCalls{}
				runtime := reconcileruntime.Runtime{RepositoryRoot: root, Incus: native, Executor: executor,
					Integrations: []profile.IntegrationDefinition{}, Environment: []string{}, Yard: loaded.Context}
				approved, err := runtime.IntegrationPlan(ctx)
				if err != nil {
					t.Fatal(err)
				}
				program, err := New(Options{RepositoryRoot: root, Environment: environment, InitPlatform: runtime, Incus: native, Executor: executor})
				if err != nil {
					t.Fatal(err)
				}
				owner := &materializedConfigActivationReconciler{projectedIntegrationPlans: projected,
					integrationPlans: map[string]reconcileruntime.IntegrationPlan{}}
				owner.integrationPlans["default"] = approved
				if scenario == "stopped" {
					native.Reconcile.Instance.Status = "Stopped"
				}
				native.Instances = map[string]ports.InstanceInfo{yard.IncusProject + "/" + yard.YardInstanceName: native.Reconcile.Instance}
				before := executor.calls
				err = owner.reconcileIntegrationTarget(ctx, program, configTarget{Name: "default", Loaded: loaded})
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "stopped" && executor.calls != before {
					t.Fatal("stopped yard replayed a running guest plan")
				}
				if scenario == "running" && executor.calls == before {
					t.Fatal("running control skipped its plan")
				}
				if owner.integrationPlans["default"].StateBinding() != approved.StateBinding() {
					t.Fatal("skipping changed the saved integration scope")
				}

			})
		}
	}
}

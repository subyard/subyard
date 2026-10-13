package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

func (prepared *preparedCommand) prepareIntegrationCleanup(ctx context.Context, loaded config.Loaded, request integrationRequest) error {
	cli := prepared.CLI
	if _, err := config.ResolveIntegrationSelection(loaded.Environment, []string{request.id}); err != nil {
		return err
	}
	if !loaded.Integrations.Present {
		return errors.New("cleanup requires an explicit persistent integration selection")
	}
	if slices.Contains(loaded.Integrations.Effective, request.id) {
		return fmt.Errorf("integration %s is still selected or required by another integration; disable it in persistent configuration before cleanup", request.id)
	}
	runtime, ok := cli.integrationRuntime(loaded).(IntegrationCleanupRuntime)
	if !ok {
		return errors.New("integration runtime does not support profile-owned cleanup")
	}
	plan, err := runtime.IntegrationCleanupPlan(ctx, request.id)
	if err != nil {
		return err
	}
	permit := cli.integrationCleanupRepair
	if permit != nil && (permit.yard != loaded.Context.YardName || permit.cleanupID != request.id || permit.cleanupPlanBinding != plan.StateBinding()) {
		return domain.ErrPlanStale
	}
	preview := func() {
		fmt.Fprintf(cli.options.Stdout, "Integration cleanup: %s (yard %s)\n", request.id, loaded.Context.YardName)
		if !plan.Changed {
			fmt.Fprintln(cli.options.Stdout, "  No cleanup needed.")
		}
		for _, step := range plan.Steps {
			fmt.Fprintln(cli.options.Stdout, "  "+step)
		}
	}
	if request.check {
		prepared.displayOnly = preview
		return nil
	}
	prepared.exactState = operationStateDigest(struct{ Runtime, Configuration string }{
		plan.StateBinding(), integrationConfigurationFingerprint(loaded),
	})
	if permit != nil {
		prepared.exactState = operationStateDigest([]string{prepared.exactState, permit.factsFingerprint})
	}
	prepared.steps = func() []domain.OperationStep {
		decision, observed := domain.StepSkip, "converged"
		if plan.Changed {
			decision, observed = domain.StepApply, "captured native cleanup ownership"
		}
		return []domain.OperationStep{{ID: "integration.cleanup", Target: loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName + ":integration " + request.id,
			Observed: observed, Desired: "profile-owned installation absent; configured selection and unrelated artifacts retained", Decision: decision,
			Preconditions: []string{"integration remains deselected", "captured cleanup handler and native target ownership remain valid"}, Verify: "native cleanup observation reports no remaining owned cleanup", Consequence: "remove only the captured profile-owned installation of " + request.id}}
	}
	prepared.preview = preview
	prepared.stepsComplete = true
	prepared.assess = func(context.Context) (domain.ActionID, domain.ActionDelta, error) {
		return "integration.cleanup", domain.ActionDelta{Changed: plan.Changed, Consequences: slices.Clone(plan.Steps)}, nil
	}
	prepared.executeNoOp = true
	prepared.execute = func(ctx context.Context, _ *application.Orchestrator, _ io.Writer) (domain.AdapterResult, error) {
		result := domain.AdapterResult{Schema: 1, OperationID: prepared.Plan.OperationID, Status: "ok"}
		if permit != nil {
			unlock, err := cli.lockConfigApplyRepair(ctx, permit)
			if err != nil {
				return result, err
			}
			defer unlock()
		}
		unlock, err := lockIntegrationYard(ctx, loaded)
		if err != nil {
			return result, err
		}
		defer unlock()
		if err := cli.requireRunningIntegrationYard(ctx, loaded); err != nil {
			return result, err
		}
		current, err := cli.persistentIntegrationContext(loaded)
		if err != nil {
			return result, err
		}
		if integrationConfigurationFingerprint(current) != integrationConfigurationFingerprint(loaded) {
			return result, fmt.Errorf("%w: integration settings changed before cleanup", domain.ErrPlanStale)
		}
		if err := runtime.ApplyIntegrationCleanup(ctx, request.id, plan); err != nil {
			return result, err
		}
		final, err := cli.persistentIntegrationContext(loaded)
		if err != nil {
			return result, err
		}
		if integrationConfigurationFingerprint(final) != integrationConfigurationFingerprint(loaded) {
			return result, fmt.Errorf("%w: integration settings changed during cleanup; inspect status", domain.ErrPlanStale)
		}
		if permit != nil {
			if err := cli.finishIntegrationCleanupRecovery(ctx, permit); err != nil {
				return result, err
			}
		}
		return result, nil
	}
	return nil
}

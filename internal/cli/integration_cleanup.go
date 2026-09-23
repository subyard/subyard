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
		plan.Fingerprint, integrationConfigurationFingerprint(loaded),
	})
	prepared.preview = preview
	prepared.assess = func(context.Context) (domain.ActionID, domain.ActionDelta, error) {
		return "integration.cleanup", domain.ActionDelta{Changed: plan.Changed, Consequences: slices.Clone(plan.Steps)}, nil
	}
	prepared.executeNoOp = true
	prepared.execute = func(ctx context.Context, _ *application.Orchestrator, _ io.Writer) (domain.AdapterResult, error) {
		result := domain.AdapterResult{Schema: 1, OperationID: prepared.Plan.OperationID, Status: "ok"}
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
		return result, nil
	}
	return nil
}

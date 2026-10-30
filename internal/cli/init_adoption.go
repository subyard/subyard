package cli

import (
	"context"
	"fmt"
	"maps"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

func (execution *initExecution) adopting() bool {
	return execution.legacyContinuation != nil && execution.legacyContinuation.adopt
}

func (continuation *initLegacyContinuation) applyAdoption(ctx context.Context, cli *CLI, execution *initExecution) error {
	runtime := execution.platform.(reconcileruntime.Runtime)
	writer, ok := runtime.Incus.(ports.ProjectAdopter)
	if !ok {
		return fmt.Errorf("native atomic project adoption is unavailable")
	}
	if err := continuation.check(ctx, cli, execution); err != nil {
		return err
	}
	if err := writer.AdoptProject(ctx, runtime.Yard.IncusProject, continuation.projectBefore, continuation.adoptionConfig); err != nil {
		return err
	}
	// Preserve only the protected grant already validated by assessment/check.
	// Fresh adoption must still prove an initialized native identity.
	if continuation.before.Exists {
		runtime.LegacyContinuation = &continuation.grant
	}
	_, _, metadata, current, err := runtime.InspectYardAdoption(ctx)
	if err != nil {
		return fmt.Errorf("verify yard adoption: %w", err)
	}
	for key, value := range metadata {
		if current[key] != value {
			return fmt.Errorf("%w: adoption publication is incomplete", domain.ErrPlanStale)
		}
	}
	if !maps.Equal(metadata, continuation.adoptionConfig) {
		return fmt.Errorf("%w: adopted identity changed", domain.ErrPlanStale)
	}
	if continuation.before.Exists {
		if err := config.RemovePersistentFileSnapshot(runtime.Yard.Paths.ConfigHome, continuation.path, continuation.before); err != nil {
			return fmt.Errorf("adoption verified; continuation retirement incomplete, rerun init --adopt: %w", err)
		}
	}
	continuation.published = true
	return nil
}

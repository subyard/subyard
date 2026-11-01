package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

// The owning fixture deselects an existing running dedicated VM. Reuse source,
// fresh-helper, image, OFD and observer custody from the startup fixture. This
// exercises only the original init resource component, not full init or Rust.
func TestOwnerOrphanPhysicalOriginal(t *testing.T) {
	runOwnerResourcePhysicalOriginal(t, true)
}

func runPhysicalOrphan(ctx context.Context, cli *CLI, loaded config.Loaded, runtime reconcileruntime.Runtime, input physicalStartupInput, observer *physicalStartupObserver) (bool, error) {
	execution, err := restoreOwnerInitRecord(input.Init, loaded, runtime)
	if err != nil {
		return false, err
	}
	runtime = execution.platform.(reconcileruntime.Runtime)
	sources, err := runtime.BindInstalledRuntimeContext(ctx, input.Sources)
	if err != nil {
		return false, err
	}
	retained, err := runtime.OwnerSourceRuntime(ctx, sources)
	if err != nil {
		return false, errors.Join(err, sources.Close())
	}
	physical, err := restoreOwnerInitResources(cli, execution, runtime, retained)
	if err != nil {
		return false, errors.Join(err, sources.FinishOwnerSources(err))
	}
	plan := execution.orphanIngress
	if plan == nil || len(plan.entries) == 0 {
		return false, errors.Join(domain.ErrPlanStale, sources.Close())
	}
	incus, _ := physical.statusPorts()
	closer, ok := incus.(originalOrphanIngressCloser)
	if !ok {
		return false, errors.Join(domain.ErrPlanStale, sources.Close())
	}
	entry := plan.entries[0]
	started := len(observer.started)
	// An otherwise exact SDK request with a substituted UUID must fail before
	// reserving any mutation. Actual replacement negatives are host-free tests.
	negative := closer.RemoveOwnedResourceIngressIf(ctx, plan.yard, entry.contract, plan.uuid+"-substituted", plan.nic, entry.device, entry.marker, true)
	uuidRejected := errors.Is(negative, domain.ErrPlanStale) && len(observer.started) == started
	if !uuidRejected {
		return false, errors.Join(errors.New("physical substituted UUID admitted"), negative, sources.Close())
	}
	err = plan.apply(ctx, physical, loaded, "original-orphan")
	return true, errors.Join(err, sources.FinishOwnerSources(err))
}

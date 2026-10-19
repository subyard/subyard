package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/Subyard/Subyard/internal/adapters/testvmsruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

type ownerLifecycleRecord struct {
	Action                     string
	Force, Changed, VMCPUReady bool
	Status, StartupScope       string
	Observed                   ownerTeardownState
	Steps                      []domain.OperationStep
}

func captureOwnerStart(ctx context.Context, operation *CLI, loaded config.Loaded) (*ownerLifecycleRecord, error) {
	definition, ok := operation.manifest.Lookup("start")
	if !ok {
		return nil, domain.ErrPlanStale
	}
	execution, err := prepareLifecycleExecution(definition, []string{"--yes"})
	if err != nil {
		return nil, err
	}
	if err := operation.observeLifecycleExecution(ctx, loaded.Context, execution); err != nil {
		return nil, err
	}
	scope, err := operation.startupScopeBinding(ctx, loaded)
	if err != nil {
		return nil, err
	}
	startup, err := operation.prepareResourceStartup(ctx, loaded)
	if err != nil {
		return nil, err
	}
	if startup != nil {
		return nil, errors.New("owner temporary start requires original startup resource capsule")
	}
	return &ownerLifecycleRecord{execution.action, execution.force, execution.changed, execution.vmCPUReady, execution.status, scope, describeTeardownState(ports.ReconcileState{Instance: *execution.observed}), domain.CloneOperationSteps(execution.approvedSteps)}, nil
}

func (record *ownerLifecycleRecord) restore(loaded config.Loaded) (*lifecycleExecution, error) {
	if record == nil || record.Action != "start" || record.Force || record.Observed.Instance.Name != loaded.Context.YardInstanceName || record.Observed.Instance.Project != loaded.Context.IncusProject || record.Observed.Instance.Config.restore()["volatile.uuid"] == "" {
		return nil, domain.ErrPlanStale
	}
	instance := record.Observed.restore().Instance
	return &lifecycleExecution{action: record.Action, force: record.Force, changed: record.Changed, vmCPUReady: record.VMCPUReady, status: record.Status, observed: &instance, approvedSteps: domain.CloneOperationSteps(record.Steps)}, nil
}

func (p *preparedOwnerChildren) runStart(ctx context.Context, yard string, output io.Writer) error {
	child, ok := p.inits[yard]
	if !ok || child.start == nil {
		return domain.ErrPlanStale
	}
	execution, err := child.start.restore(child.loaded)
	if err != nil {
		return err
	}
	scope, err := p.operation.startupScopeBinding(ctx, child.loaded)
	if err != nil || scope != child.start.StartupScope {
		return domain.ErrPlanStale
	}
	sources, err := p.restoreChildSources(ctx, yard)
	if err != nil {
		return err
	}
	runtime, err := child.runtime.OwnerSourceRuntime(ctx, sources)
	if err != nil {
		return errors.Join(err, sources.Close())
	}
	orchestrator, plan, err := ownerChildOrchestrator(ctx, p.operation, child.loaded, "start", runtime.RepositoryRoot)
	if err != nil {
		return errors.Join(err, sources.FinishOwnerSources(err))
	}
	result, err := p.operation.executeLifecycle(ctx, orchestrator, child.loaded.Context, plan, execution, output)
	if err == nil && result.Status != "ok" {
		err = errors.New("original owner temporary start did not complete")
	}
	return errors.Join(err, sources.FinishOwnerSources(err))
}

func (p *preparedOwnerChildren) runPower(ctx context.Context, yard string) error {
	child, ok := p.inits[yard]
	if !ok || child.power == nil {
		return domain.ErrPlanStale
	}
	prepared, err := child.runtime.RestoreOwnerHostPower(ctx, *child.power)
	if err != nil {
		return err
	}
	defer prepared.Close()
	sources, err := p.restoreChildSources(ctx, yard)
	if err != nil {
		return err
	}
	return child.runtime.ApplyOwnerHostPower(ctx, prepared, sources)
}

func (p *preparedOwnerChildren) readBrokerStatus(ctx context.Context, yard string, output io.Writer) error {
	child, ok := p.inits[yard]
	if !ok || output == nil {
		return domain.ErrPlanStale
	}
	pool, err := p.operation.probeTestVMPool(ports.WithoutNativeOperationObserver(ctx), child.loaded)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(struct {
		Pool *testvmsruntime.LeasePool `json:"pool"`
	}{pool})
}

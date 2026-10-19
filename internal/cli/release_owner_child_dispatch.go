package cli

import (
	"context"
	"errors"
	"io"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

// Only the already selected start/teardown wrappers use this component bridge.
// The Rust invocation owns consent; this local plan only admits its original
// fixed adapter request through the existing component execution boundary.
func ownerChildOrchestrator(ctx context.Context, operation *CLI, loaded config.Loaded, kind, sourceRoot string) (*application.Orchestrator, domain.OperationPlan, error) {
	if kind != "start" && kind != "teardown" {
		return nil, domain.OperationPlan{}, domain.ErrPlanStale
	}
	definition, ok := operation.manifest.Lookup(kind)
	if !ok || kind == "start" && definition.Handler != "@lifecycle" || kind == "teardown" && definition.Handler != "@teardown" {
		return nil, domain.OperationPlan{}, domain.ErrPlanStale
	}
	physical := *operation
	physical.options.RepositoryRoot = sourceRoot
	orchestrator := physical.operationOrchestrator("owner-registration", loaded, nil, &definition)
	plan, err := orchestrator.Confirm(ctx, domain.OperationPlan{OperationID: "owner-registration", Command: kind, Effect: domain.CommandMutate, Confirmation: domain.ConfirmationNever, Target: domain.TargetLocalOwner}, true)
	return orchestrator, plan, err
}

func (p *preparedOwnerChildren) runTeardown(ctx context.Context, yard string, output io.Writer) error {
	child, ok := p.teardowns[yard]
	init, sourceOK := p.inits[yard]
	if !ok || !sourceOK || child.execution == nil || len(init.sourceRaw) == 0 {
		return domain.ErrPlanStale
	}
	runtime := init.runtime
	sources, err := p.restoreChildSources(ctx, yard)
	if err != nil {
		return err
	}
	executionRuntime, err := runtime.OwnerSourceRuntime(ctx, sources)
	if err != nil {
		return errors.Join(err, sources.Close())
	}
	orchestrator, plan, err := ownerChildOrchestrator(ctx, p.operation, child.loaded, "teardown", executionRuntime.RepositoryRoot)
	if err != nil {
		return errors.Join(err, sources.FinishOwnerSources(err))
	}
	result, err := p.operation.executeTeardown(ctx, orchestrator, child.loaded, plan, child.execution, output)
	if err == nil && result.Status != "ok" {
		err = errors.New("original owner teardown did not complete")
	}
	err = errors.Join(err, sources.FinishOwnerSources(err))
	if err == nil {
		if p.removed == nil {
			p.removed = map[string]bool{}
		}
		p.removed[yard] = true
	}
	return err
}

func (p *preparedOwnerChildren) restoreChildSources(ctx context.Context, yard string) (*reconcileruntime.PreparedInstalledRuntime, error) {
	child, ok := p.inits[yard]
	if !ok || len(child.sourceRaw) == 0 {
		return nil, domain.ErrPlanStale
	}
	return child.runtime.BindInstalledRuntimeContext(ctx, child.sourceRaw)
}

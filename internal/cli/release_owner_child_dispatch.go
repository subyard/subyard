package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/uninstall"
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
	if physical.ownerMutationLock == nil {
		home := loaded.Context.Paths.DataHome
		held, err := uninstall.BorrowOwnerLock(home)
		if err != nil {
			return nil, domain.OperationPlan{}, err
		}
		if held != nil {
			defer held.Close()
			if err := uninstall.ValidateOwnerLockDescriptor(home, uint32(os.Getuid()), held); err != nil {
				return nil, domain.OperationPlan{}, err
			}
			binding, err := uninstall.Capture(uninstall.OwnerLockPath(home))
			if err != nil {
				return nil, domain.OperationPlan{}, err
			}
			// Audit shares the admitted native barrier; it must not reopen its flock.
			physical.ownerMutationLock = &binding
		}
	}
	orchestrator := physical.operationOrchestrator("owner-registration", loaded, nil, &definition)
	plan, err := orchestrator.Confirm(ctx, domain.OperationPlan{OperationID: "owner-registration", Command: kind, Effect: domain.CommandMutate, Confirmation: domain.ConfirmationNever, Target: domain.TargetLocalOwner}, true)
	return orchestrator, plan, err
}

func (p *preparedOwnerChildren) runTeardown(ctx context.Context, yard string) error {
	child, ok := p.teardowns[yard]
	init, sourceOK := p.inits[yard]
	if !ok || !sourceOK || child.execution == nil || init.sources == nil {
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
	operation := *p.operation
	// Capture the shell runner's existing redacted diagnostics instead of
	// streaming raw child output into the native helper's discarded stderr.
	operation.options.Stderr = nil
	orchestrator, plan, err := ownerChildOrchestrator(ctx, &operation, child.loaded, "teardown", executionRuntime.RepositoryRoot)
	if err != nil {
		return errors.Join(err, sources.FinishOwnerSources(err))
	}
	var diagnostics bytes.Buffer
	result, err := operation.executeTeardown(ctx, orchestrator, child.loaded, plan, child.execution, &diagnostics)
	if err != nil {
		if interaction := ports.NativeInteractionFrom(ctx); interaction != nil {
			var exit *exec.ExitError
			var errno syscall.Errno
			if errors.As(err, &exit) {
				fmt.Fprintf(interaction, "owner teardown adapter exit=%d\n", exit.ExitCode())
			} else if errors.As(err, &errno) {
				fmt.Fprintf(interaction, "owner teardown adapter: %s\n", errno)
			}
			_, _ = io.Copy(interaction, &diagnostics)
		}
	}
	if err == nil && result.Status != "ok" {
		err = errors.New("original owner teardown did not complete")
	}
	err = errors.Join(err, sources.FinishOwnerSources(err))
	return err
}

func (p *preparedOwnerChildren) restoreChildSources(ctx context.Context, yard string) (*reconcileruntime.PreparedInstalledRuntime, error) {
	child, ok := p.inits[yard]
	if !ok || child.sources == nil {
		return nil, domain.ErrPlanStale
	}
	return child.runtime.BindInstalledRuntimeContext(ctx, child.sources.Recipe())
}

package cli

import (
	"context"

	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

// assessStructuredAction is the single action-assessment entry point shared by
// direct CLI and RPC planning. A false typed result is valid only for commands
// with a concrete, non-dynamic manifest policy.
func (cli *CLI) assessStructuredAction(
	ctx context.Context,
	loaded config.Loaded,
	definition command.Definition,
	project *projectExecution,
	initRun *initExecution,
	lifecycleRun *lifecycleExecution,
	provisionRun *provisionExecution,
	teardownRun *teardownExecution,
) (domain.ActionID, domain.ActionDelta, bool, error) {
	if initRun != nil {
		action, delta, err := initRun.actionPlan()
		return action, delta, true, err
	}
	if lifecycleRun != nil && lifecycleRun.action == "stop" {
		if err := cli.observeLifecycleExecution(ctx, loaded.Context, lifecycleRun); err != nil {
			return "", domain.ActionDelta{}, true, err
		}
		action, delta, err := lifecycleRun.actionPlan(definition, loaded.Context)
		return action, delta, true, err
	}
	if provisionRun != nil {
		if err := cli.observeProvisionExecution(ctx, loaded, definition, provisionRun); err != nil {
			return "", domain.ActionDelta{}, true, err
		}
		action, delta, err := provisionRun.actionPlan(definition, loaded.Context)
		return action, delta, true, err
	}
	if teardownRun != nil {
		if err := cli.observeTeardownExecution(ctx, loaded, teardownRun); err != nil {
			return "", domain.ActionDelta{}, true, err
		}
		action, delta, err := teardownRun.actionPlan(definition, loaded.Context)
		return action, delta, true, err
	}
	if project == nil {
		return "", domain.ActionDelta{}, false, nil
	}
	if definition.Name == "remove" {
		if err := cli.prepareProjectRemoval(ctx, project); err != nil {
			return "", domain.ActionDelta{}, true, err
		}
		action, delta, err := project.removeActionPlan()
		return action, delta, true, err
	}
	switch definition.Name {
	case "sync", "bind", "clone", "export", "up", "down":
		if err := cli.observeProjectAction(ctx, definition.Name, project); err != nil {
			return "", domain.ActionDelta{}, true, err
		}
		action, delta, err := project.actionPlan(definition.Name)
		return action, delta, true, err
	default:
		return "", domain.ActionDelta{}, false, nil
	}
}

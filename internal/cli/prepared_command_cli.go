package cli

import (
	"context"
	"errors"
	"slices"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/domain"
)

func (cli *CLI) reportPreparationError(definition command.Definition, err error) int {
	behavior, _ := resolveCoreCommand(definition)
	code := 1
	prefix := "prepare " + definition.Name
	var failure *commandPreparationError
	if errors.As(err, &failure) {
		switch failure.phase {
		case "prepare":
			code = behavior.prepareExit
			if definition.Handler == "@update" {
				prefix = "update"
			}
		case "assessment":
			prefix += " action"
		case "plan":
			prefix = "plan " + definition.Name
		}
	}
	if errors.Is(err, domain.ErrPlanStale) {
		code = 1
	}
	cli.errorf("%s: %v", prefix, err)
	return code
}

func preparationRPCError(definition command.Definition, err error) error {
	code := "plan_failed"
	var failure *commandPreparationError
	if errors.As(err, &failure) {
		switch failure.phase {
		case "project":
			code = "invalid_params"
		case "prepare":
			behavior, _ := resolveCoreCommand(definition)
			code = behavior.prepareRPCCode
		}
	}
	return operationRPCError(code, err)
}

func (cli *CLI) runPreparedCommand(ctx context.Context, prepared *preparedCommand, assumeYes bool) int {
	if prepared.displayOnly != nil {
		prepared.displayOnly()
		return 0
	}
	if prepared.preview != nil {
		prepared.preview()
	}
	assumeYes = assumeYes || slices.Contains(prepared.Arguments, "--yes") || slices.Contains(prepared.Arguments, "-y")
	orchestrator := cli.operationOrchestrator(prepared.Plan.OperationID, prepared.Loaded, nil, &prepared.Definition)
	plan, err := orchestrator.Confirm(ctx, prepared.Plan, assumeYes)
	if err != nil {
		if errors.Is(err, application.ErrDeclined) {
			cli.errorf("operation declined")
		} else {
			cli.errorf("plan %s: %v", prepared.Definition.Name, err)
		}
		return 1
	}
	prepared.Plan = plan
	if plan.Target == domain.TargetRemoteOwner {
		arguments := slices.Clone(prepared.Arguments)
		if prepared.remoteArguments != nil {
			arguments, err = prepared.remoteArguments(arguments)
			if err != nil {
				cli.errorf("prepare remote %s: %v", prepared.Definition.Name, err)
				return 1
			}
		}
		if !slices.Contains(arguments, "--yes") && !slices.Contains(arguments, "-y") {
			arguments = append([]string{"--yes"}, arguments...)
		}
		return cli.forwardRemote(ctx, prepared.Loaded.Context, prepared.Definition.Name, arguments)
	}
	result, err := prepared.Execute(ctx, orchestrator, cli.options.Stdout)
	if err != nil {
		var commitErr *commandCommitError
		if errors.As(err, &commitErr) {
			cli.errorf("commit %s: %v", prepared.Definition.Name, err)
		} else {
			cli.errorf("%s: %v", prepared.Definition.Name, err)
		}
		return 1
	}
	if result.Status != "ok" {
		cli.errorf("%s adapter returned %s (%s)", prepared.Definition.Name, result.Status, result.ErrorCode)
		return 1
	}
	if prepared.printResult != nil {
		prepared.printResult(result)
	}
	return 0
}

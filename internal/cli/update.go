package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/adapters/shelladapter"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

type releaseAdapter struct{ prepared releaseruntime.Prepared }

type releaseExecution struct {
	prepared releaseruntime.Prepared
	yard     string
}

func (adapter releaseAdapter) Run(ctx context.Context, request domain.AdapterRequest, _ io.Reader) (domain.AdapterResult, string, error) {
	if request.Adapter != "release" || request.Action != "execute" {
		return domain.AdapterResult{}, "", errors.New("unsupported release adapter request")
	}
	if err := adapter.prepared.Execute(ctx); err != nil {
		return domain.AdapterResult{}, "", err
	}
	return domain.AdapterResult{Schema: shelladapter.ProtocolSchema, OperationID: request.OperationID, Status: "ok"}, "", nil
}

func (cli *CLI) runUpdate(ctx context.Context, loaded config.Loaded, definition command.Definition, arguments []string) int {
	execution, err := cli.prepareRelease(ctx, loaded, arguments)
	if err != nil {
		cli.errorf("update: %v", err)
		return 1
	}
	assumeYes := cli.env["ASSUME_YES"] == "1"
	for _, argument := range arguments {
		if argument == "-y" || argument == "--yes" {
			assumeYes = true
		}
	}
	orchestrator := cli.operationOrchestrator(cli.env["SUBYARD_OPERATION_ID"], loaded, nil, &definition)
	plan, err := orchestrator.PlanAction(
		ctx, loaded.Context, definition.Name, domain.RemotePolicy(definition.Remote),
		execution.prepared.Action, domain.ActionDelta{
			Changed: execution.prepared.Changed, Consequences: execution.prepared.Consequences,
		}, assumeYes,
	)
	if err != nil {
		if errors.Is(err, application.ErrDeclined) {
			cli.errorf("operation declined")
		} else {
			cli.errorf("plan update: %v", err)
		}
		return 1
	}
	result, err := cli.executeRelease(ctx, orchestrator, plan, execution)
	if err != nil {
		cli.errorf("update: %v", err)
		return 1
	}
	if result.Status != "ok" {
		cli.errorf("update returned %s", result.Status)
		return 1
	}
	return 0
}

func (cli *CLI) prepareRelease(ctx context.Context, loaded config.Loaded, arguments []string) (*releaseExecution, error) {
	runtime := releaseruntime.New(releaseruntime.Config{
		Environment: loaded.Environment,
		Installer:   filepath.Join(cli.options.RepositoryRoot, "scripts", "install-runtime-release.sh"),
		Stdout:      cli.options.Stdout,
		Stderr:      cli.options.Stderr,
	})
	prepared, err := runtime.Prepare(ctx, arguments)
	if err != nil {
		return nil, err
	}
	return &releaseExecution{prepared: prepared, yard: loaded.Context.YardName}, nil
}

func (execution *releaseExecution) prepareAction(
	orchestrator *application.Orchestrator,
	loaded config.Loaded,
	definition command.Definition,
) (domain.OperationPlan, error) {
	return orchestrator.PrepareAction(
		loaded.Context, definition.Name, domain.RemotePolicy(definition.Remote),
		execution.prepared.Action, domain.ActionDelta{
			Changed: execution.prepared.Changed, Consequences: execution.prepared.Consequences,
		},
	)
}

func (cli *CLI) executeRelease(ctx context.Context, orchestrator *application.Orchestrator,
	plan domain.OperationPlan, execution *releaseExecution) (domain.AdapterResult, error) {
	orchestrator.Runner = releaseAdapter{prepared: execution.prepared}
	result, _, runErr := orchestrator.RunAdapter(ctx, plan, domain.AdapterRequest{
		Schema: shelladapter.ProtocolSchema, OperationID: plan.OperationID, Adapter: "release", Action: "execute",
	}, nil)
	if result.Status == "ok" && execution.prepared.RefreshConfigs {
		applier := cli.options.Config
		if applier == nil {
			environment := maps.Clone(cli.baseEnv)
			delete(environment, "YARD_ENGINE_PATH")
			applier = dispatcherConfigApplier{
				path: execution.prepared.ActiveLauncher, environment: environment,
				stdout: cli.options.Stdout, stderr: cli.options.Stderr, applyDrift: true,
			}
		}
		if applyErr := applier.ApplyConfig(ctx, execution.yard); applyErr != nil {
			retry := "yard config apply"
			if execution.yard != "" && execution.yard != "default" {
				retry = "yard -Y " + execution.yard + " config apply"
			}
			refreshErr := fmt.Errorf("runtime activation completed, but refreshing materialized agent configuration failed: %w; retry with: %s", applyErr, retry)
			if runErr != nil {
				return result, errors.Join(runErr, refreshErr)
			}
			return result, refreshErr
		}
	}
	return result, runErr
}

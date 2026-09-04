package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"sort"

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
	runtime  *releaseruntime.Runtime
}

func (execution *releaseExecution) Close() error {
	if execution == nil || execution.runtime == nil {
		return nil
	}
	runtime := execution.runtime
	execution.runtime = nil
	return runtime.Close()
}

func (adapter releaseAdapter) Run(ctx context.Context, request domain.AdapterRequest, _ io.Reader) (domain.AdapterResult, string, error) {
	if request.Adapter != "release" || request.Action != "execute" {
		return domain.AdapterResult{}, "", errors.New("unsupported release adapter request")
	}
	if err := adapter.prepared.Execute(ctx); err != nil {
		return domain.AdapterResult{}, "", fmt.Errorf("execute prepared release: %w", err)
	}
	return domain.AdapterResult{Schema: shelladapter.ProtocolSchema, OperationID: request.OperationID, Status: "ok"}, "", nil
}

func (cli *CLI) prepareRelease(ctx context.Context, loaded config.Loaded, arguments []string) (*releaseExecution, error) {
	releaseEnvironment := maps.Clone(loaded.Environment)
	releaseEnvironment["SUBYARD_OPERATION_ID"] = cli.env["SUBYARD_OPERATION_ID"]
	runtime := releaseruntime.New(releaseruntime.Config{
		Environment: releaseEnvironment,
		Installer:   filepath.Join(cli.options.RepositoryRoot, "scripts", "install-runtime-release.sh"),
		Stdout:      cli.options.Stdout,
		Stderr:      cli.options.Stderr,
	})
	prepared, err := runtime.PrepareTransition(
		ctx, arguments, loaded.Environment["SUBYARD_CONFIG_HOME"],
		loaded.Context.YardName, cli.releaseTransitionInheritedSettingIDs(),
	)
	if err != nil {
		_ = runtime.Close()
		return nil, err
	}
	return &releaseExecution{
		prepared: prepared, yard: loaded.Context.YardName, runtime: runtime,
	}, nil
}

func (cli *CLI) releaseTransitionInheritedSettingIDs() []string {
	inheritedSettingIDs := make([]string, 0)
	for name := range cli.baseEnv {
		if _, setting := config.LookupSetting(name); setting {
			inheritedSettingIDs = append(inheritedSettingIDs, name)
		}
	}
	sort.Strings(inheritedSettingIDs)
	return inheritedSettingIDs
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

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"sort"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/adapters/shelladapter"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/audit"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

type releaseAdapter struct{ prepared releaseruntime.Prepared }

type releaseExecution struct {
	prepared     releaseruntime.Prepared
	yard         string
	runtime      *releaseruntime.Runtime
	history      *audit.UpdateRecorder
	phase        string
	failurePhase string
	failureCode  string
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

func (cli *CLI) runUpdate(ctx context.Context, loaded config.Loaded, definition command.Definition, arguments []string) int {
	prepared, err := cli.prepareCommand(ctx, prepareCommandRequest{Loaded: loaded, Definition: definition, Arguments: arguments})
	if err != nil {
		return cli.reportPreparationError(definition, err)
	}
	defer prepared.Close()
	return cli.runPreparedCommand(ctx, prepared, cli.env["ASSUME_YES"] == "1")
}

func (cli *CLI) prepareRelease(ctx context.Context, loaded config.Loaded, arguments []string) (*releaseExecution, error) {
	releaseEnvironment := maps.Clone(loaded.Environment)
	releaseEnvironment["SUBYARD_OPERATION_ID"] = cli.env["SUBYARD_OPERATION_ID"]
	runtime := releaseruntime.New(cli.releaseRuntimeConfig(releaseEnvironment))
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
		phase: "execute",
	}, nil
}

func (cli *CLI) releaseRuntimeConfig(environment map[string]string) releaseruntime.Config {
	return releaseruntime.Config{
		Environment:    environment,
		Installer:      filepath.Join(cli.options.RepositoryRoot, "scripts", "install-runtime-release.sh"),
		RepositoryRoot: cli.options.RepositoryRoot,
		Stdout:         cli.options.Stdout,
		Stderr:         cli.options.Stderr,
		Progress:       cli.updateProgress,
	}
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

func (cli *CLI) executeRelease(ctx context.Context, orchestrator *application.Orchestrator,
	plan domain.OperationPlan, execution *releaseExecution) (domain.AdapterResult, error) {
	orchestrator.Runner = releaseAdapter{prepared: execution.prepared}
	result, _, runErr := orchestrator.RunAdapter(ctx, plan, domain.AdapterRequest{
		Schema: shelladapter.ProtocolSchema, OperationID: plan.OperationID, Adapter: "release", Action: "execute",
	}, nil)
	executionFailed := runErr != nil || result.Status != "ok"
	if executionFailed {
		execution.failurePhase = "execute"
		execution.failureCode = "execution_failed"
	}
	if result.Status == "ok" && execution.prepared.RefreshConfigs {
		runErr = errors.Join(runErr, cli.refreshReleaseConfig(ctx, execution))
	}
	return result, runErr
}

func (cli *CLI) refreshReleaseConfig(ctx context.Context, execution *releaseExecution) error {
	execution.phase = "refresh"
	var refreshErr error
	if execution.history != nil {
		if historyErr := execution.history.Advance(
			"refresh", execution.prepared.TargetRelease, execution.prepared.TargetVersion,
		); historyErr != nil {
			execution.failurePhase = "refresh"
			execution.failureCode = "history_write_failed"
			refreshErr = fmt.Errorf("persist update history: %w", historyErr)
		}
	}
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
		execution.failurePhase = "refresh"
		execution.failureCode = "refresh_failed"
		retry := "yard config apply"
		if execution.yard != "" && execution.yard != "default" {
			retry = "yard -Y " + execution.yard + " config apply"
		}
		refreshErr = errors.Join(refreshErr, fmt.Errorf(
			"runtime activation completed, but refreshing materialized agent configuration failed: %w; retry with: %s",
			applyErr, retry,
		))
	}
	return refreshErr
}

func (cli *CLI) beginUpdateHistory(plan domain.OperationPlan, execution *releaseExecution) error {
	direction, ok := updateDirection(execution.prepared.Action)
	if !ok {
		return nil
	}
	recorder, err := cli.updateHistory().Begin(audit.UpdateRecord{
		OperationID: plan.OperationID, Direction: direction,
		SourceRelease: execution.prepared.SourceRelease, SourceVersion: execution.prepared.SourceVersion,
		TargetRelease: execution.prepared.TargetRelease, TargetVersion: execution.prepared.TargetVersion,
	})
	if err != nil {
		return err
	}
	execution.history = recorder
	execution.phase = "execute"
	return nil
}

func (cli *CLI) finishUpdateHistory(ctx context.Context, execution *releaseExecution, result domain.AdapterResult, runErr error) error {
	if execution == nil || execution.history == nil {
		return runErr
	}
	status, code := "success", ""
	terminalPhase := execution.phase
	executionFailed := runErr != nil || result.Status != "ok"
	if executionFailed {
		failurePhase := execution.failurePhase
		if failurePhase == "" {
			failurePhase = execution.phase
		}
		terminalPhase = failurePhase
		failureCode := execution.failureCode
		if failureCode == "" {
			failureCode = failurePhase + "_failed"
		}
		status, code = "failure", failureCode
	}
	if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) ||
		(executionFailed && ctx.Err() != nil) {
		status, code = "interrupted", "context_cancelled"
	}
	historyErr := execution.history.Finish(terminalPhase, status, code, "", "")
	if historyErr != nil {
		historyErr = fmt.Errorf("persist update history: %w", historyErr)
	}
	return errors.Join(runErr, historyErr)
}

func (cli *CLI) recordUpdateTerminal(arguments []string, operationID, phase, status, code string, execution *releaseExecution) error {
	direction := "activate"
	for _, argument := range arguments {
		if argument == "--rollback" {
			direction = "rollback"
			break
		}
	}
	sourceRelease, sourceVersion := "", ""
	targetRelease, targetVersion := "", ""
	if execution != nil {
		sourceRelease, sourceVersion = execution.prepared.SourceRelease, execution.prepared.SourceVersion
		targetRelease, targetVersion = execution.prepared.TargetRelease, execution.prepared.TargetVersion
	}
	return cli.updateHistory().Terminal(audit.UpdateRecord{
		OperationID: operationID, Direction: direction,
		SourceRelease: sourceRelease, SourceVersion: sourceVersion,
		TargetRelease: targetRelease, TargetVersion: targetVersion,
	}, phase, status, code)
}

func (cli *CLI) updateHistory() audit.UpdateHistory {
	home := cli.env["SUBYARD_HOME"]
	if home == "" {
		operatorHome := cli.env["SUBYARD_OPERATOR_HOME"]
		if operatorHome == "" {
			operatorHome = cli.env["HOME"]
		}
		if operatorHome != "" {
			home = filepath.Join(operatorHome, ".subyard")
		}
	}
	clock := cli.options.Clock
	return audit.UpdateHistory{Home: home, Now: func() time.Time {
		if clock != nil {
			return clock.Now()
		}
		return time.Now()
	}}
}

func updateDirection(action domain.ActionID) (string, bool) {
	switch action {
	case "update.activate":
		return "activate", true
	case "update.rollback":
		return "rollback", true
	default:
		return "", false
	}
}

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/audit"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/hosterrors"
	"github.com/Subyard/Subyard/internal/ports"
)

const (
	// hostErrorsProgram is the release's guest facts helper; it validates and
	// stores the snapshot as root.
	hostErrorsProgram = "config/skills/core/subyard-environment/facts.py"
	hostErrorsTimeout = 10 * time.Second
)

func (cli *CLI) setupHistory() audit.SetupHistory {
	home := cli.hostDataHome()
	clock := cli.options.Clock
	return audit.SetupHistory{Home: home, BeforeWrite: cli.auditWriteAdmission(home), Now: func() time.Time {
		if clock != nil {
			return clock.Now()
		}
		return time.Now()
	}}
}

// beginSetupHistory records an unfinished `yard init` execution, best
// effort: a history failure never changes the operation.
func (cli *CLI) beginSetupHistory(operationID, yard string) *audit.SetupRecorder {
	if cli.hostDataHome() == "" {
		return nil
	}
	recorder, err := cli.setupHistory().Begin(operationID, yard)
	if err != nil {
		return nil
	}
	return recorder
}

func (cli *CLI) finishSetupHistory(ctx context.Context, recorder *audit.SetupRecorder, result domain.AdapterResult, err error) {
	if recorder == nil {
		return
	}
	_ = recorder.Finish(setupOutcome(ctx, result, err))
}

// recordSetupTerminal records a `yard init` attempt that ended before
// execution, best effort.
func (cli *CLI) recordSetupTerminal(operationID, yard, phase, status, code string, cause error) {
	if cli.hostDataHome() == "" {
		return
	}
	outcome := audit.SetupOutcome{Status: status, ErrorCode: code}
	if cause != nil {
		outcome.Refusals, outcome.Incomplete = setupRefusals(cause)
	}
	_ = cli.setupHistory().Terminal(operationID, yard, phase, outcome)
}

func setupOutcome(ctx context.Context, result domain.AdapterResult, err error) audit.SetupOutcome {
	if err == nil && result.Status == "ok" {
		return audit.SetupOutcome{Status: "success"}
	}
	outcome := audit.SetupOutcome{Status: "failure", ErrorCode: "execution_failed"}
	var hookFailure *application.ProjectHookFailure
	switch {
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil:
		outcome.Status, outcome.ErrorCode = "interrupted", "context_cancelled"
	case errors.Is(err, domain.ErrPlanStale):
		outcome.ErrorCode = "plan_stale"
	case errors.As(err, &hookFailure):
		outcome.ErrorCode = "project_hook_failed"
	case err == nil && audit.ValidHistoryCode(result.ErrorCode):
		outcome.ErrorCode = result.ErrorCode
	}
	if err != nil {
		outcome.Refusals, outcome.Incomplete = setupRefusals(err)
	}
	return outcome
}

// setupRefusals keeps the typed configuration refusals of err as codes and
// names, and names each configuration source error by its setting alone
// (code source-invalid); any other cause marks the outcome incomplete.
func setupRefusals(err error) ([]audit.SetupRefusal, bool) {
	diagnostic := config.Diagnose("", err)
	refusals := make([]audit.SetupRefusal, 0, len(diagnostic.Typed))
	for _, typed := range diagnostic.Typed {
		refusals = append(refusals, audit.SetupRefusal{Source: "config", Code: typed.Code.Name(), Names: typed.Names})
	}
	seen := map[string]bool{}
	visitErrors(err, func(cause error) {
		source, ok := cause.(*config.SourceError)
		if !ok || seen[source.Key] {
			return
		}
		if _, valid := domain.ConfigSettingName(source.Key); valid {
			seen[source.Key] = true
			refusals = append(refusals, audit.SetupRefusal{Source: "config", Code: "source-invalid", Names: []string{source.Key}})
		}
	})
	return refusals, !diagnostic.Complete()
}

// visitErrors visits err and every error it wraps, depth first.
func visitErrors(err error, visit func(error)) {
	if err == nil {
		return
	}
	visit(err)
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		for _, cause := range wrapped.Unwrap() {
			visitErrors(cause, visit)
		}
	case interface{ Unwrap() error }:
		visitErrors(wrapped.Unwrap(), visit)
	}
}

// publishAfterCommand refreshes the host-errors snapshot after a command
// whose outcome it records: init and start for their yard, update and
// migrate for every local yard.
func (cli *CLI) publishAfterCommand(ctx context.Context, definition command.Definition, loaded config.Loaded, arguments []string) {
	if cli.releaseTransitionChild || commandHelpRequested(arguments) {
		return
	}
	switch {
	case definition.Handler == "@init" || definition.Handler == "@lifecycle" && definition.Name == "start":
		cli.publishHostErrors(ctx, loaded.Context)
	case definition.Handler == "@update" || definition.Handler == "@current-migration":
		for _, name := range cli.configSyncLocalYards(loaded) {
			if target, err := cli.loadInventoryLoaded(name, loaded); err == nil {
				cli.publishHostErrors(ctx, target.Context)
			}
		}
	}
}

// publishHostErrors writes the yard's value-free failure snapshot into the
// running yard through the release's guest facts helper.
func (cli *CLI) publishHostErrors(ctx context.Context, yard domain.Context) {
	cli.hostErrorsPublisher(yard)(ctx)
}

// hostErrorsPublisher binds the owner state a publication needs, so an RPC
// session's writer can publish without reading the CLI concurrently. Each
// publication is best effort and bounded: a stopped yard or failing Incus
// keeps the previous snapshot, and nothing here changes a command's output
// or exit code.
func (cli *CLI) hostErrorsPublisher(yard domain.Context) func(context.Context) {
	home := cli.hostDataHome()
	if home == "" || cli.coldNative || yard.AccessKind == domain.AccessRemote ||
		!domain.SafeName(yard.YardName) || yard.IncusProject == "" || yard.YardInstanceName == "" {
		return func(context.Context) {}
	}
	program, err := os.ReadFile(filepath.Join(cli.options.RepositoryRoot, hostErrorsProgram))
	if err != nil {
		return func(context.Context) {}
	}
	incusPort, executor := cli.statusPorts()
	clock := cli.options.Clock
	return func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hostErrorsTimeout)
		defer cancel()
		instance, err := incusPort.Instance(ctx, yard.IncusProject, yard.YardInstanceName)
		if err != nil || !strings.EqualFold(instance.Status, "running") {
			return
		}
		now := time.Now()
		if clock != nil {
			now = clock.Now()
		}
		payload, err := json.Marshal(hosterrors.Build(home, yard.YardName, now))
		if err != nil {
			return
		}
		_, _ = executor.Exec(ctx, yard.IncusProject, yard.YardInstanceName, ports.InstanceExecRequest{
			Command: []string{"python3", "-B", "-c", string(program), "host-errors-put"}, Stdin: payload,
		})
	}
}

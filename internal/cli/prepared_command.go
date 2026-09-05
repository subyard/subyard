package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/shelladapter"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

// coreCommandBehavior binds a handler family to its preparation and physical
// leaves. User-facing metadata continues to come from the command manifest.
type coreCommandBehavior struct {
	prepare        func(*preparedCommand, context.Context, *initBootstrap) error
	shellActions   func(string) map[string]map[string]shelladapter.Action
	nonRPCReason   string
	prepareExit    int
	prepareRPCCode string
}

func resolveCoreCommand(definition command.Definition) (coreCommandBehavior, error) {
	behavior := coreCommandBehavior{prepareExit: 2, prepareRPCCode: "invalid_params"}
	switch definition.Handler {
	case "@init":
		behavior.prepare = (*preparedCommand).prepareInit
		behavior.prepareExit, behavior.prepareRPCCode = 1, "plan_failed"
	case "@lifecycle":
		behavior.prepare = (*preparedCommand).prepareLifecycle
		behavior.shellActions = lifecycleShellActions
	case "@provision":
		behavior.prepare = (*preparedCommand).prepareProvision
		behavior.shellActions = func(root string) map[string]map[string]shelladapter.Action {
			actions := lifecycleShellActions(root)
			actions["provision"] = map[string]shelladapter.Action{
				"profile":       {Path: filepath.Join(root, "scripts/provision-profile.sh"), Direct: true, Timeout: 45 * time.Minute},
				"profile-check": {Path: filepath.Join(root, "scripts/provision-profile.sh"), Direct: true, Capture: true},
			}
			return actions
		}
	case "@test-vms":
		behavior.prepare = (*preparedCommand).prepareTestVMs
		behavior.shellActions = func(root string) map[string]map[string]shelladapter.Action {
			path := filepath.Join(root, "scripts/e2e-lab/invoke.sh")
			return map[string]map[string]shelladapter.Action{"test-vms": {
				"up": {Path: path, Direct: true}, "status": {Path: path, Direct: true},
				"down": {Path: path, Direct: true}, "revoke": {Path: path, Direct: true},
				"recover": {Path: path, Direct: true},
			}}
		}
	case "@teardown":
		behavior.prepare = (*preparedCommand).prepareTeardown
		behavior.shellActions = func(root string) map[string]map[string]shelladapter.Action {
			return map[string]map[string]shelladapter.Action{"teardown": {
				"apply": {Path: filepath.Join(root, "scripts/teardown-physical.sh"), Direct: true},
			}}
		}
	case "@project", "@project-env":
		behavior.prepare = (*preparedCommand).prepareProject
	case "@remote":
		behavior.prepare = (*preparedCommand).prepareRemote
		behavior.prepareExit = 1
	case "@update":
		behavior.prepare = (*preparedCommand).prepareUpdate
		behavior.prepareExit, behavior.prepareRPCCode = 1, "plan_failed"
	case "@keys":
		behavior.nonRPCReason = "protected credential transport"
	case "@shell":
		behavior.nonRPCReason = "interactive terminal session"
	case "@config", "@host":
		behavior.nonRPCReason = "dedicated configuration and registration workflow"
	case "@resource":
		behavior.nonRPCReason = "profile resource pipeline"
	case "@check", "@security", "@status", "@space", "@info", "@yards", "@logs", "@usage", "@list", "@help":
		behavior.nonRPCReason = "read-only query"
	case "@rpc", "@authorize", "@state", "@project-state", "@migrate", "@release-transition":
		behavior.nonRPCReason = "dedicated internal protocol"
	default:
		if !strings.HasPrefix(definition.Handler, "@") && definition.Handler != "" {
			behavior.nonRPCReason = "legacy physical script dispatch"
			break
		}
		return coreCommandBehavior{}, fmt.Errorf("unsupported core handler %q", definition.Handler)
	}
	return behavior, nil
}

func lifecycleShellActions(root string) map[string]map[string]shelladapter.Action {
	path := filepath.Join(root, "scripts", "lifecycle-guard.sh")
	return map[string]map[string]shelladapter.Action{"lifecycle": {
		"start": {Path: path, Direct: true}, "stop": {Path: path, Direct: true},
	}}
}

type prepareCommandRequest struct {
	Loaded       config.Loaded
	Definition   command.Definition
	Arguments    []string
	ExplicitYard bool
	ReadOnly     bool
	Bootstrap    *initBootstrap
	// OnResolved lets the direct boundary audit canonical inputs before assessment.
	OnResolved func(config.Loaded, []string)
}

type commandAssessment func(context.Context) (domain.ActionID, domain.ActionDelta, error)

// A prepared command owns one execution closure and its captured typed state.
// Project admission is shared lifecycle state, not a second execution variant.
type preparedCommand struct {
	CLI             *CLI
	Definition      command.Definition
	Arguments       []string
	Loaded          config.Loaded
	Plan            domain.OperationPlan
	Project         *projectExecution
	policy          domain.CommandPolicy
	assess          commandAssessment
	refresh         commandAssessment
	execute         func(context.Context, *application.Orchestrator, io.Writer) (domain.AdapterResult, error)
	closeResource   func() error
	preview         func()
	displayOnly     func()
	printResult     func(domain.AdapterResult)
	remoteArguments func([]string) ([]string, error)
	executeNoOp     bool
	closed          bool
	executed        bool
	closeOnce       sync.Once
	closeErr        error
}

type commandPreparationError struct {
	phase string
	err   error
}

func (failure *commandPreparationError) Error() string { return failure.err.Error() }
func (failure *commandPreparationError) Unwrap() error { return failure.err }

type commandCommitError struct{ err error }

func (failure *commandCommitError) Error() string { return failure.err.Error() }
func (failure *commandCommitError) Unwrap() error { return failure.err }

func (cli *CLI) prepareCommand(ctx context.Context, request prepareCommandRequest) (_ *preparedCommand, err error) {
	behavior, err := resolveCoreCommand(request.Definition)
	if err != nil {
		return nil, err
	}
	if behavior.prepare == nil {
		return nil, fmt.Errorf("command has no prepared execution: %s", behavior.nonRPCReason)
	}
	operationID := cli.ensureOperationID()
	prepared := &preparedCommand{CLI: cli, Definition: request.Definition,
		Arguments: slices.Clone(request.Arguments), Loaded: request.Loaded}
	defer func() {
		if err != nil {
			_ = prepared.Close()
		}
	}()
	prepared.Project, err = cli.prepareProjectExecution(ctx, prepared.Loaded, prepared.Definition,
		prepared.Arguments, request.ExplicitYard, request.ReadOnly)
	if err != nil {
		return nil, &commandPreparationError{phase: "project", err: err}
	}
	if prepared.Project != nil {
		prepared.Loaded = prepared.Project.Loaded
		prepared.Arguments = slices.Clone(prepared.Project.Arguments)
		for name, value := range prepared.Project.Environment {
			cli.env[name] = value
		}
	}
	if request.OnResolved != nil {
		request.OnResolved(prepared.Loaded, slices.Clone(prepared.Arguments))
	}
	prepared.policy = commandPolicy(prepared.Definition, prepared.Loaded.Context, prepared.Arguments, prepared.Project)
	if err = behavior.prepare(prepared, ctx, request.Bootstrap); err != nil {
		return nil, &commandPreparationError{phase: "prepare", err: err}
	}
	if prepared.displayOnly != nil {
		return prepared, nil
	}
	if prepared.execute == nil {
		return nil, errors.New("prepared command has no execution")
	}
	orchestrator := cli.operationOrchestrator(operationID, prepared.Loaded, nil, nil)
	if prepared.assess != nil {
		action, delta, assessErr := prepared.assess(ctx)
		if assessErr != nil {
			return nil, &commandPreparationError{phase: "assessment", err: assessErr}
		}
		prepared.Plan, err = orchestrator.PrepareAction(prepared.Loaded.Context,
			prepared.policy.Name, prepared.policy.RemotePolicy, action, delta)
	} else {
		prepared.Plan, err = orchestrator.Prepare(prepared.Loaded.Context,
			resolveCommandConfirmation(prepared.Definition, prepared.policy))
	}
	if err != nil {
		return nil, &commandPreparationError{phase: "plan", err: err}
	}
	return prepared, nil
}

func (prepared *preparedCommand) Close() error {
	if prepared == nil {
		return nil
	}
	prepared.closeOnce.Do(func() {
		prepared.closed = true
		if prepared.CLI != nil {
			prepared.CLI.abortProjectExecution(context.Background(), prepared.Project)
		}
		if prepared.closeResource != nil {
			prepared.closeErr = prepared.closeResource()
		}
	})
	return prepared.closeErr
}

func (prepared *preparedCommand) Execute(ctx context.Context, orchestrator *application.Orchestrator, diagnostics io.Writer) (domain.AdapterResult, error) {
	if prepared.closed || prepared.executed || prepared.execute == nil {
		return domain.AdapterResult{}, errors.New("prepared command is not executable")
	}
	if !prepared.Plan.Confirmed {
		return domain.AdapterResult{}, domain.ErrConfirmationRequired
	}
	prepared.executed = true
	noOp := func() (domain.AdapterResult, error) {
		return domain.AdapterResult{Schema: shelladapter.ProtocolSchema, OperationID: prepared.Plan.OperationID, Status: "ok"}, nil
	}
	if !prepared.executeNoOp && operationPlanNoOp(prepared.Plan) {
		return noOp()
	}
	if prepared.Plan.Assessment != nil && prepared.refresh != nil {
		action, delta, err := prepared.refresh(ctx)
		if err != nil {
			return domain.AdapterResult{}, err
		}
		if action != prepared.Plan.Assessment.Action {
			return domain.AdapterResult{}, fmt.Errorf("%w: structured action changed after confirmation", domain.ErrPlanStale)
		}
		if !delta.Changed {
			return prepared.commitResult(ctx, noOp)
		}
		if !slices.Equal(delta.Consequences, prepared.Plan.Assessment.Consequences) {
			return domain.AdapterResult{}, fmt.Errorf("%w: action consequences changed after confirmation", domain.ErrPlanStale)
		}
	}
	if err := prepared.CLI.reserveProjectExecution(ctx, prepared.Project); err != nil {
		return domain.AdapterResult{}, err
	}
	return prepared.commitResult(ctx, func() (domain.AdapterResult, error) { return prepared.execute(ctx, orchestrator, diagnostics) })
}

func (prepared *preparedCommand) commitResult(ctx context.Context, execute func() (domain.AdapterResult, error)) (domain.AdapterResult, error) {
	result, err := execute()
	if err == nil && result.Status == "ok" && prepared.Project != nil && !operationPlanNoOp(prepared.Plan) {
		if commitErr := prepared.CLI.commitProjectExecution(ctx, prepared.Project); commitErr != nil {
			return result, &commandCommitError{err: commitErr}
		}
	}
	return result, err
}

func (prepared *preparedCommand) prepareInit(ctx context.Context, bootstrap *initBootstrap) error {
	if prepared.Loaded.Context.AccessKind == domain.AccessRemote {
		prepared.execute = func(context.Context, *application.Orchestrator, io.Writer) (domain.AdapterResult, error) {
			return domain.AdapterResult{}, errors.New("init requires the owner host")
		}
		return nil
	}
	cli := prepared.CLI
	execution, err := cli.prepareInitExecution(ctx, prepared.Loaded, prepared.Arguments, bootstrap)
	if err != nil {
		return err
	}
	prepared.policy.Consequences = execution.consequences()
	prepared.assess = func(context.Context) (domain.ActionID, domain.ActionDelta, error) { return execution.actionPlan() }
	prepared.refresh = func(ctx context.Context) (domain.ActionID, domain.ActionDelta, error) {
		if err := execution.refreshAssessment(ctx); err != nil {
			return "", domain.ActionDelta{}, err
		}
		return execution.actionPlan()
	}
	prepared.preview = func() {
		cli.printInitPlan(execution)
		if operationPlanNoOp(prepared.Plan) && execution.mode == initReconcile {
			fmt.Fprintln(cli.options.Stdout, "  [ ok ] Everything is already set up")
		}
	}
	prepared.execute = func(ctx context.Context, orchestrator *application.Orchestrator, diagnostics io.Writer) (domain.AdapterResult, error) {
		if cli.options.InitPlatform == nil && execution.mode != initConfigs {
			if err := cli.prepareSudoPrivileges(ctx, diagnostics, cli.effectiveUID(), prepared.Definition.Name); err != nil {
				return domain.AdapterResult{}, err
			}
			execution.platform = cli.initPlatform(execution.loaded, execution.powerYards)
		}
		orchestrator.Runner = initAdapter{execution: execution, cli: cli, output: diagnostics}
		result, _, err := orchestrator.RunAdapter(ctx, prepared.Plan, domain.AdapterRequest{
			Schema: shelladapter.ProtocolSchema, OperationID: prepared.Plan.OperationID, Adapter: "init", Action: "reconcile",
		}, nil)
		return result, err
	}
	return nil
}

func (prepared *preparedCommand) prepareLifecycle(_ context.Context, _ *initBootstrap) error {
	execution, err := prepareLifecycleExecution(prepared.Definition, prepared.Arguments)
	if err != nil {
		return err
	}
	prepared.policy = execution.policy(prepared.Definition, prepared.Loaded.Context)
	if execution.action == "stop" {
		prepared.assess = func(ctx context.Context) (domain.ActionID, domain.ActionDelta, error) {
			if err := prepared.CLI.observeLifecycleExecution(ctx, prepared.Loaded.Context, execution); err != nil {
				return "", domain.ActionDelta{}, err
			}
			return execution.actionPlan(prepared.Definition, prepared.Loaded.Context)
		}
		prepared.refresh = prepared.assess
	}
	prepared.execute = func(ctx context.Context, orchestrator *application.Orchestrator, diagnostics io.Writer) (domain.AdapterResult, error) {
		return prepared.CLI.executeLifecycle(ctx, orchestrator, prepared.Loaded.Context, prepared.Plan, execution, diagnostics)
	}
	return nil
}

func (prepared *preparedCommand) prepareProvision(_ context.Context, _ *initBootstrap) error {
	execution, err := prepared.CLI.prepareProvisionExecution(prepared.Loaded, prepared.Arguments, prepared.Project)
	if err != nil {
		return err
	}
	if execution.list {
		prepared.displayOnly = func() { execution.printList(prepared.CLI.options.Stdout) }
		return nil
	}
	prepared.policy = execution.policy(prepared.Definition, prepared.Loaded.Context)
	prepared.assess = func(ctx context.Context) (domain.ActionID, domain.ActionDelta, error) {
		if err := prepared.CLI.observeProvisionExecution(ctx, prepared.Loaded, prepared.Definition, execution); err != nil {
			return "", domain.ActionDelta{}, err
		}
		return execution.actionPlan(prepared.Definition, prepared.Loaded.Context)
	}
	prepared.refresh = prepared.assess
	prepared.execute = func(ctx context.Context, orchestrator *application.Orchestrator, diagnostics io.Writer) (domain.AdapterResult, error) {
		return prepared.CLI.executeProvision(ctx, orchestrator, prepared.Loaded, prepared.Plan, execution, diagnostics)
	}
	return nil
}

func (prepared *preparedCommand) prepareTestVMs(ctx context.Context, _ *initBootstrap) error {
	execution, err := prepared.CLI.prepareTestVMExecution(ctx, prepared.Loaded, prepared.Arguments)
	if err != nil {
		return err
	}
	prepared.assess = func(context.Context) (domain.ActionID, domain.ActionDelta, error) { return execution.actionPlan() }
	prepared.remoteArguments = execution.remoteArguments
	prepared.execute = func(ctx context.Context, orchestrator *application.Orchestrator, diagnostics io.Writer) (domain.AdapterResult, error) {
		return prepared.CLI.executeTestVMs(ctx, orchestrator, prepared.Loaded, prepared.Plan, execution, diagnostics)
	}
	return nil
}

func (prepared *preparedCommand) prepareTeardown(_ context.Context, _ *initBootstrap) error {
	execution, err := prepareTeardownExecution(prepared.Arguments)
	if err != nil {
		return err
	}
	prepared.policy = execution.policy(prepared.Definition, prepared.Loaded.Context)
	prepared.assess = func(ctx context.Context) (domain.ActionID, domain.ActionDelta, error) {
		if err := prepared.CLI.observeTeardownExecution(ctx, prepared.Loaded, execution); err != nil {
			return "", domain.ActionDelta{}, err
		}
		return execution.actionPlan(prepared.Definition, prepared.Loaded.Context)
	}
	prepared.refresh = prepared.assess
	prepared.execute = func(ctx context.Context, orchestrator *application.Orchestrator, diagnostics io.Writer) (domain.AdapterResult, error) {
		return prepared.CLI.executeTeardown(ctx, orchestrator, prepared.Loaded, prepared.Plan, execution, diagnostics)
	}
	return nil
}

func (prepared *preparedCommand) prepareRemote(ctx context.Context, _ *initBootstrap) error {
	execution, err := prepared.CLI.prepareRemoteExecution(ctx, prepared.Loaded, prepared.Arguments)
	if err != nil {
		return err
	}
	prepared.policy = application.RemotePolicy(*execution)
	prepared.assess = func(context.Context) (domain.ActionID, domain.ActionDelta, error) {
		return application.RemoteActionPlan(*execution)
	}
	prepared.printResult = prepared.CLI.printRemoteResult
	prepared.execute = func(ctx context.Context, orchestrator *application.Orchestrator, _ io.Writer) (domain.AdapterResult, error) {
		orchestrator.Runner = application.RemoteRunner{Control: prepared.CLI.remoteService(prepared.Loaded).Control, Prepared: *execution}
		result, _, err := orchestrator.RunAdapter(ctx, prepared.Plan, domain.AdapterRequest{
			Schema: shelladapter.ProtocolSchema, OperationID: prepared.Plan.OperationID, Adapter: "remote", Action: string(execution.Action),
		}, nil)
		return result, err
	}
	return nil
}

func (prepared *preparedCommand) prepareUpdate(ctx context.Context, _ *initBootstrap) error {
	execution, err := prepared.CLI.prepareRelease(ctx, prepared.Loaded, prepared.Arguments)
	if err != nil {
		return err
	}
	prepared.closeResource = execution.Close
	prepared.executeNoOp = true
	prepared.assess = func(context.Context) (domain.ActionID, domain.ActionDelta, error) {
		return execution.prepared.Action, domain.ActionDelta{Changed: execution.prepared.Changed, Consequences: execution.prepared.Consequences}, nil
	}
	prepared.execute = func(ctx context.Context, orchestrator *application.Orchestrator, _ io.Writer) (domain.AdapterResult, error) {
		return prepared.CLI.executeRelease(ctx, orchestrator, prepared.Plan, execution)
	}
	return nil
}

func (prepared *preparedCommand) prepareProject(_ context.Context, _ *initBootstrap) error {
	project := prepared.Project
	if project == nil {
		return errors.New("project execution is required")
	}
	cli, loaded, definition := prepared.CLI, prepared.Loaded, prepared.Definition
	switch definition.Name {
	case "remove":
		prepared.assess = func(ctx context.Context) (domain.ActionID, domain.ActionDelta, error) {
			if err := cli.prepareProjectRemoval(ctx, project); err != nil {
				return "", domain.ActionDelta{}, err
			}
			return project.removeActionPlan()
		}
	case "sync", "bind", "clone", "export", "up", "down":
		prepared.assess = func(ctx context.Context) (domain.ActionID, domain.ActionDelta, error) {
			if err := cli.observeProjectAction(ctx, definition.Name, project); err != nil {
				return "", domain.ActionDelta{}, err
			}
			return project.actionPlan(definition.Name)
		}
	}
	prepared.refresh = prepared.assess
	if definition.Handler == "@project" {
		prepared.execute = func(ctx context.Context, orchestrator *application.Orchestrator, diagnostics io.Writer) (domain.AdapterResult, error) {
			incusPort, _ := cli.statusPorts()
			orchestrator.Runner = application.ProjectActionRunner{
				Data: cli.projectDataPlane(), Devices: cli.projectDeviceManager(), Archive: cli.projectArchiver(),
				Exports: cli.projectExportStore(loaded), Instances: incusPort, VSCode: cli.projectVSCode(),
				Extensions:         strings.Fields(cli.env["CODE_RECOMMENDED_EXTENSIONS"]),
				WorkspaceDirectory: filepath.Join(loaded.Context.Paths.ConfigHome, "workspaces"),
				Yard:               loaded.Context, Project: project.Record, YardIdentity: project.YardIdentity,
				SoftRemove: project.Environment["SUBYARD_PROJECT_REMOVE_SOFT"] == "1",
			}
			result, stderr, err := orchestrator.RunAdapter(ctx, prepared.Plan, domain.AdapterRequest{
				Schema: shelladapter.ProtocolSchema, OperationID: prepared.Plan.OperationID, Adapter: "project", Action: definition.Name,
			}, nil)
			writeAdapterDiagnostics(diagnostics, stderr)
			return result, err
		}
	} else {
		prepared.execute = func(ctx context.Context, orchestrator *application.Orchestrator, diagnostics io.Writer) (domain.AdapterResult, error) {
			var protected io.ReadCloser
			if project.SecretPath != "" {
				file, err := os.Open(project.SecretPath)
				if err != nil {
					return domain.AdapterResult{}, err
				}
				protected = file
				defer protected.Close()
			}
			orchestrator.Runner = application.ProjectEnvironmentRunner{
				Data: cli.projectDataPlane(), Yard: loaded.Context, Project: project.Record,
				Profile: project.Profile, HostLinks: project.HostLinks,
				Rebuild: project.Environment["SUBYARD_PROJECT_REBUILD"] == "1", HasSecret: project.SecretPath != "",
			}
			result, stderr, err := orchestrator.RunAdapter(ctx, prepared.Plan, domain.AdapterRequest{
				Schema: shelladapter.ProtocolSchema, OperationID: prepared.Plan.OperationID, Adapter: "project-env", Action: definition.Name,
			}, protected)
			writeAdapterDiagnostics(diagnostics, stderr)
			return result, err
		}
	}
	return nil
}

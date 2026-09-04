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

	"github.com/Subyard/Subyard/internal/adapters/shelladapter"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

type preparedCommandFamily string

const (
	preparedCommandUnknown            preparedCommandFamily = ""
	preparedCommandInit               preparedCommandFamily = "init"
	preparedCommandLifecycle          preparedCommandFamily = "lifecycle"
	preparedCommandProvision          preparedCommandFamily = "provision"
	preparedCommandTestVMs            preparedCommandFamily = "test-vms"
	preparedCommandTeardown           preparedCommandFamily = "teardown"
	preparedCommandProject            preparedCommandFamily = "project"
	preparedCommandProjectEnvironment preparedCommandFamily = "project-environment"
	preparedCommandRemote             preparedCommandFamily = "remote"
	preparedCommandRelease            preparedCommandFamily = "release"
)

type preparedCommand struct {
	CLI        *CLI
	Family     preparedCommandFamily
	Plan       domain.OperationPlan
	Definition command.Definition
	Arguments  []string
	Loaded     config.Loaded
	Project    *projectExecution
	Remote     *domain.RemotePrepared
	Init       *initExecution
	Lifecycle  *lifecycleExecution
	Provision  *provisionExecution
	TestVMs    *testVMExecution
	Teardown   *teardownExecution
	Release    *releaseExecution

	closeOnce sync.Once
	closeErr  error
}

type prepareCommandRequest struct {
	Loaded       config.Loaded
	Definition   command.Definition
	Arguments    []string
	ExplicitYard bool
	ReadOnly     bool
	Direct       bool
	Bootstrap    *initBootstrap
}

type prepareCommandError struct {
	err         error
	directExit  int
	rpcCode     string
	directStage string
}

type preparedCommandCommitError struct{ err error }

func (err *preparedCommandCommitError) Error() string { return err.err.Error() }
func (err *preparedCommandCommitError) Unwrap() error { return err.err }

func (err *prepareCommandError) Error() string { return err.err.Error() }
func (err *prepareCommandError) Unwrap() error { return err.err }

func commandPreparationError(err error, directExit int, rpcCode string, directStage ...string) error {
	if err == nil {
		return nil
	}
	stage := ""
	if len(directStage) != 0 {
		stage = directStage[0]
	}
	return &prepareCommandError{
		err: err, directExit: directExit, rpcCode: rpcCode, directStage: stage,
	}
}

func preparedCommandDirectExit(err error) int {
	var preparation *prepareCommandError
	if errors.As(err, &preparation) && preparation.directExit != 0 {
		return preparation.directExit
	}
	return 1
}

func preparedCommandDirectMessage(name string, err error) string {
	var preparation *prepareCommandError
	if errors.As(err, &preparation) {
		switch preparation.directStage {
		case "action":
			return fmt.Sprintf("prepare %s action: %v", name, preparation.err)
		case "plan":
			return fmt.Sprintf("plan %s: %v", name, preparation.err)
		case "command":
			return fmt.Sprintf("%s: %v", name, preparation.err)
		}
	}
	return fmt.Sprintf("prepare %s: %v", name, err)
}

func preparedCommandRPCCode(err error, fallback string) string {
	var preparation *prepareCommandError
	if errors.As(err, &preparation) && preparation.rpcCode != "" {
		return preparation.rpcCode
	}
	return fallback
}

func (prepared *preparedCommand) Close() error {
	if prepared == nil {
		return nil
	}
	prepared.closeOnce.Do(func() {
		if prepared.CLI != nil {
			prepared.CLI.abortProjectExecution(context.Background(), prepared.Project)
		}
		if prepared.Release != nil {
			prepared.closeErr = prepared.Release.Close()
		}
	})
	return prepared.closeErr
}

func (prepared *preparedCommand) Execute(
	ctx context.Context,
	orchestrator *application.Orchestrator,
	diagnostics io.Writer,
) (domain.AdapterResult, error) {
	if prepared == nil || prepared.CLI == nil || orchestrator == nil || prepared.Plan.OperationID == "" {
		return domain.AdapterResult{}, errors.New("prepared command is incomplete")
	}
	var (
		result domain.AdapterResult
		err    error
	)
	if prepared.Release != nil {
		result, err = prepared.CLI.executeRelease(
			ctx, orchestrator, prepared.Plan, prepared.Release,
		)
	} else {
		result, err = prepared.executeAdapter(ctx, orchestrator, diagnostics)
	}
	if err != nil || result.Status != "ok" || prepared.Project == nil || operationPlanNoOp(prepared.Plan) {
		return result, err
	}
	if err := prepared.CLI.commitProjectExecution(ctx, prepared.Project); err != nil {
		return result, &preparedCommandCommitError{err: err}
	}
	return result, nil
}

func (prepared *preparedCommand) executeAdapter(
	ctx context.Context,
	orchestrator *application.Orchestrator,
	diagnostics io.Writer,
) (domain.AdapterResult, error) {
	plan := prepared.Plan
	if operationPlanNoOp(plan) {
		return domain.AdapterResult{
			Schema: shelladapter.ProtocolSchema, OperationID: plan.OperationID, Status: "ok",
		}, nil
	}
	if plan.Assessment != nil && prepared.Remote == nil && prepared.TestVMs == nil &&
		(prepared.Project != nil || prepared.Init != nil || prepared.Lifecycle != nil ||
			prepared.Provision != nil || prepared.Teardown != nil) {
		if prepared.Init != nil {
			if err := prepared.Init.refreshAssessment(ctx); err != nil {
				return domain.AdapterResult{}, err
			}
		}
		action, delta, typed, err := prepared.CLI.assessStructuredAction(
			ctx, prepared.Loaded, prepared.Definition, prepared.Project, prepared.Init,
			prepared.Lifecycle, prepared.Provision, prepared.Teardown,
		)
		if err != nil {
			return domain.AdapterResult{}, err
		}
		if !typed || action != plan.Assessment.Action {
			return domain.AdapterResult{}, fmt.Errorf(
				"%w: structured action changed after confirmation", domain.ErrPlanStale,
			)
		}
		if !delta.Changed {
			return domain.AdapterResult{
				Schema: shelladapter.ProtocolSchema, OperationID: plan.OperationID, Status: "ok",
			}, nil
		}
		if !slices.Equal(delta.Consequences, plan.Assessment.Consequences) {
			return domain.AdapterResult{}, fmt.Errorf(
				"%w: action consequences changed after confirmation", domain.ErrPlanStale,
			)
		}
	}
	if err := prepared.CLI.reserveProjectExecution(ctx, prepared.Project); err != nil {
		return domain.AdapterResult{}, err
	}
	if prepared.Remote != nil {
		orchestrator.Runner = application.RemoteRunner{
			Control:  prepared.CLI.remoteService(prepared.Loaded).Control,
			Prepared: *prepared.Remote,
		}
		request := domain.AdapterRequest{
			Schema: shelladapter.ProtocolSchema, OperationID: plan.OperationID,
			Adapter: "remote", Action: string(prepared.Remote.Action),
		}
		result, _, err := orchestrator.RunAdapter(ctx, plan, request, nil)
		return result, err
	}
	if prepared.Init != nil {
		if prepared.CLI.options.InitPlatform == nil && prepared.Init.mode != initConfigs {
			if err := prepared.CLI.prepareSudoPrivileges(
				ctx, diagnostics, prepared.CLI.effectiveUID(), prepared.Definition.Name,
			); err != nil {
				return domain.AdapterResult{}, err
			}
			prepared.Init.platform = prepared.CLI.initPlatform(
				prepared.Init.loaded, prepared.Init.powerYards,
			)
		}
		orchestrator.Runner = initAdapter{
			execution: prepared.Init, cli: prepared.CLI, output: diagnostics,
		}
		request := domain.AdapterRequest{
			Schema: shelladapter.ProtocolSchema, OperationID: plan.OperationID,
			Adapter: "init", Action: "reconcile",
		}
		result, _, err := orchestrator.RunAdapter(ctx, plan, request, nil)
		return result, err
	}
	if prepared.Lifecycle != nil {
		return prepared.CLI.executeLifecycle(
			ctx, orchestrator, prepared.Loaded.Context, plan, prepared.Lifecycle, diagnostics,
		)
	}
	if prepared.Provision != nil {
		return prepared.CLI.executeProvision(
			ctx, orchestrator, prepared.Loaded, plan, prepared.Provision, diagnostics,
		)
	}
	if prepared.TestVMs != nil {
		return prepared.CLI.executeTestVMs(
			ctx, orchestrator, prepared.Loaded, plan, prepared.TestVMs, diagnostics,
		)
	}
	if prepared.Teardown != nil {
		return prepared.CLI.executeTeardown(
			ctx, orchestrator, prepared.Loaded, plan, prepared.Teardown, diagnostics,
		)
	}
	if prepared.Project != nil && prepared.Family == preparedCommandProject {
		incusPort, _ := prepared.CLI.statusPorts()
		orchestrator.Runner = application.ProjectActionRunner{
			Data: prepared.CLI.projectDataPlane(), Devices: prepared.CLI.projectDeviceManager(),
			Archive: prepared.CLI.projectArchiver(), Exports: prepared.CLI.projectExportStore(prepared.Loaded),
			Instances: incusPort, VSCode: prepared.CLI.projectVSCode(),
			Extensions:         strings.Fields(prepared.CLI.env["CODE_RECOMMENDED_EXTENSIONS"]),
			WorkspaceDirectory: filepath.Join(prepared.Loaded.Context.Paths.ConfigHome, "workspaces"),
			Yard:               prepared.Loaded.Context,
			Project:            prepared.Project.Record,
			YardIdentity:       prepared.Project.YardIdentity,
			SoftRemove:         prepared.Project.Environment["SUBYARD_PROJECT_REMOVE_SOFT"] == "1",
		}
		request := domain.AdapterRequest{
			Schema: shelladapter.ProtocolSchema, OperationID: plan.OperationID,
			Adapter: "project", Action: prepared.Definition.Name,
		}
		result, stderr, err := orchestrator.RunAdapter(ctx, plan, request, nil)
		writeAdapterDiagnostics(diagnostics, stderr)
		return result, err
	}
	if prepared.Project != nil && prepared.Family == preparedCommandProjectEnvironment {
		var protected io.ReadCloser
		if prepared.Project.SecretPath != "" {
			file, err := os.Open(prepared.Project.SecretPath)
			if err != nil {
				return domain.AdapterResult{}, err
			}
			protected = file
			defer protected.Close()
		}
		orchestrator.Runner = application.ProjectEnvironmentRunner{
			Data: prepared.CLI.projectDataPlane(), Yard: prepared.Loaded.Context,
			Project: prepared.Project.Record, Profile: prepared.Project.Profile,
			HostLinks: prepared.Project.HostLinks,
			Rebuild:   prepared.Project.Environment["SUBYARD_PROJECT_REBUILD"] == "1",
			HasSecret: prepared.Project.SecretPath != "",
		}
		request := domain.AdapterRequest{
			Schema: shelladapter.ProtocolSchema, OperationID: plan.OperationID,
			Adapter: "project-env", Action: prepared.Definition.Name,
		}
		result, stderr, err := orchestrator.RunAdapter(ctx, plan, request, protected)
		writeAdapterDiagnostics(diagnostics, stderr)
		return result, err
	}
	handlerArguments := append([]string(nil), prepared.Arguments...)
	if prepared.Definition.Arg0 != "" {
		handlerArguments = append([]string{prepared.Definition.Arg0}, handlerArguments...)
	}
	contextValues := structuredCommandContext(prepared.Loaded)
	if structuredCommandNeedsSudo(prepared.Definition.Name) {
		if prepared.CLI.options.AdapterRunner == nil {
			if err := prepared.CLI.prepareSudoPrivileges(
				ctx, diagnostics, prepared.CLI.effectiveUID(), prepared.Definition.Name,
			); err != nil {
				return domain.AdapterResult{}, err
			}
		}
		if prepared.CLI.env["SUBYARD_SUDO_PREAUTHORIZED"] == "1" {
			contextValues["SUBYARD_SUDO_PREAUTHORIZED"] = "1"
		}
	}
	if prepared.Project != nil {
		for key, value := range prepared.Project.Environment {
			contextValues[key] = value
		}
	}
	request := domain.AdapterRequest{
		Schema: shelladapter.ProtocolSchema, OperationID: plan.OperationID,
		Adapter: "command", Action: prepared.Definition.Name,
		Arguments: handlerArguments, Context: contextValues,
	}
	result, stderr, err := orchestrator.RunAdapter(ctx, plan, request, nil)
	writeAdapterDiagnostics(diagnostics, stderr)
	return result, err
}

func (prepared *preparedCommand) runDirect(ctx context.Context, assumeYes bool) int {
	for _, argument := range prepared.Arguments {
		if argument == "-y" || argument == "--yes" {
			assumeYes = true
		}
	}
	if prepared.Init != nil {
		prepared.CLI.printInitPlan(prepared.Init)
	}
	if prepared.Provision != nil && prepared.Provision.list {
		prepared.Provision.printList(prepared.CLI.options.Stdout)
		return 0
	}
	orchestrator := prepared.CLI.operationOrchestrator(
		prepared.Plan.OperationID, prepared.Loaded, nil, &prepared.Definition,
	)
	plan, err := orchestrator.Confirm(ctx, prepared.Plan, assumeYes)
	if err != nil {
		if errors.Is(err, application.ErrDeclined) {
			prepared.CLI.errorf("operation declined")
		} else {
			prepared.CLI.errorf("plan %s: %v", prepared.Definition.Name, err)
		}
		return 1
	}
	prepared.Plan = plan
	if operationPlanNoOp(plan) && prepared.Init != nil && prepared.Init.mode == initReconcile {
		fmt.Fprintln(prepared.CLI.options.Stdout, "  [ ok ] Everything is already set up")
	}
	if plan.Target == domain.TargetRemoteOwner {
		remoteArguments := append([]string(nil), prepared.Arguments...)
		if prepared.TestVMs != nil {
			remoteArguments, err = prepared.TestVMs.remoteArguments(prepared.Arguments)
			if err != nil {
				prepared.CLI.errorf("prepare remote test-vms: %v", err)
				return 1
			}
		}
		hasYes := false
		for _, argument := range remoteArguments {
			hasYes = hasYes || argument == "-y" || argument == "--yes"
		}
		if !hasYes {
			remoteArguments = append([]string{"--yes"}, remoteArguments...)
		}
		return prepared.CLI.forwardRemote(
			ctx, prepared.Loaded.Context, prepared.Definition.Name, remoteArguments,
		)
	}
	result, err := prepared.Execute(ctx, orchestrator, prepared.CLI.options.Stdout)
	if err != nil {
		var commitErr *preparedCommandCommitError
		if errors.As(err, &commitErr) {
			prepared.CLI.errorf("commit %s: %v", prepared.Definition.Name, commitErr.err)
		} else {
			prepared.CLI.errorf("%s: %v", prepared.Definition.Name, err)
		}
		return 1
	}
	if result.Status != "ok" {
		prepared.CLI.errorf(
			"%s adapter returned %s (%s)",
			prepared.Definition.Name, result.Status, result.ErrorCode,
		)
		return 1
	}
	if prepared.Remote != nil {
		prepared.CLI.printRemoteResult(result)
	}
	return 0
}

type commandPreparation struct {
	Family       preparedCommandFamily
	Direct       bool
	RPC          bool
	RPCExclusion string
}

func resolveCommandPreparation(definition command.Definition) commandPreparation {
	resolution := commandPreparation{}
	switch definition.Handler {
	case "@init":
		resolution.Family, resolution.Direct = preparedCommandInit, true
	case "@lifecycle":
		resolution.Family, resolution.Direct = preparedCommandLifecycle, true
	case "@provision":
		resolution.Family, resolution.Direct = preparedCommandProvision, true
	case "@test-vms":
		resolution.Family, resolution.Direct = preparedCommandTestVMs, true
	case "@teardown":
		resolution.Family, resolution.Direct = preparedCommandTeardown, true
	case "@project":
		resolution.Family, resolution.Direct = preparedCommandProject, true
	case "@project-env":
		resolution.Family, resolution.Direct = preparedCommandProjectEnvironment, true
	case "@remote":
		resolution.Family, resolution.Direct = preparedCommandRemote, true
	case "@update":
		resolution.Family, resolution.Direct = preparedCommandRelease, true
	case "@keys":
		resolution.RPCExclusion = "protected-payload"
	case "@shell":
		resolution.RPCExclusion = "interactive-session"
	case "@host", "@config":
		resolution.RPCExclusion = "dedicated-handler"
	}
	if !resolution.Direct {
		return resolution
	}
	if definition.Visibility != command.VisibilityPublic {
		resolution.RPCExclusion = "not-public"
		return resolution
	}
	if definition.Effect != command.EffectMutate {
		resolution.RPCExclusion = "not-mutating"
		return resolution
	}
	resolution.RPC = true
	return resolution
}

func validatePreparedCommandManifest(manifest command.Manifest) error {
	for _, definition := range manifest.Commands() {
		if definition.Visibility != command.VisibilityPublic || definition.Effect != command.EffectMutate {
			continue
		}
		resolution := resolveCommandPreparation(definition)
		if resolution.RPC || resolution.RPCExclusion != "" {
			continue
		}
		return fmt.Errorf(
			"public mutating command %q has no prepared-command behavior or RPC exclusion",
			definition.Name,
		)
	}
	return nil
}

func (cli *CLI) prepareCommand(
	ctx context.Context,
	request prepareCommandRequest,
) (prepared *preparedCommand, err error) {
	resolution := resolveCommandPreparation(request.Definition)
	if !resolution.Direct {
		return nil, fmt.Errorf("command %q does not support prepared execution", request.Definition.Name)
	}
	family := resolution.Family
	operationCLI := cli.rpcOperation(cli.ensureOperationID())
	prepared = &preparedCommand{
		CLI:        operationCLI,
		Family:     family,
		Definition: request.Definition,
		Arguments:  append([]string(nil), request.Arguments...),
		Loaded:     request.Loaded,
	}
	owned := prepared
	defer func() {
		if err != nil {
			_ = owned.Close()
		}
	}()

	prepared.Project, err = operationCLI.prepareProjectExecution(
		ctx, prepared.Loaded, prepared.Definition, prepared.Arguments,
		request.ExplicitYard, request.ReadOnly,
	)
	if err != nil {
		return nil, commandPreparationError(err, 1, "invalid_params")
	}
	if prepared.Project != nil {
		prepared.Loaded = prepared.Project.Loaded
		prepared.Arguments = append([]string(nil), prepared.Project.Arguments...)
		for key, value := range prepared.Project.Environment {
			operationCLI.env[key] = value
		}
	}

	switch family {
	case preparedCommandInit:
		if prepared.Loaded.Context.AccessKind != domain.AccessRemote {
			prepared.Init, err = operationCLI.prepareInitExecution(
				ctx, prepared.Loaded, prepared.Arguments, request.Bootstrap,
			)
		}
	case preparedCommandLifecycle:
		prepared.Lifecycle, err = prepareLifecycleExecution(prepared.Definition, prepared.Arguments)
	case preparedCommandProvision:
		prepared.Provision, err = operationCLI.prepareProvisionExecution(
			prepared.Loaded, prepared.Arguments, prepared.Project,
		)
	case preparedCommandTestVMs:
		prepared.TestVMs, err = operationCLI.prepareTestVMExecution(
			ctx, prepared.Loaded, prepared.Arguments,
		)
	case preparedCommandTeardown:
		prepared.Teardown, err = prepareTeardownExecution(prepared.Arguments)
	case preparedCommandRemote:
		prepared.Remote, err = operationCLI.prepareRemoteExecution(
			ctx, prepared.Loaded, prepared.Arguments,
		)
	case preparedCommandRelease:
		prepared.Release, err = operationCLI.prepareRelease(
			ctx, prepared.Loaded, prepared.Arguments,
		)
	case preparedCommandProject, preparedCommandProjectEnvironment:
	}
	if err != nil {
		directExit, rpcCode := 1, "plan_failed"
		switch family {
		case preparedCommandLifecycle, preparedCommandProvision, preparedCommandTestVMs,
			preparedCommandTeardown:
			directExit, rpcCode = 2, "invalid_params"
		}
		if family == preparedCommandTestVMs && errors.Is(err, domain.ErrPlanStale) {
			directExit = 1
		}
		if family == preparedCommandRemote {
			rpcCode = "invalid_params"
		}
		directStage := ""
		if family == preparedCommandRelease {
			directStage = "command"
		}
		return nil, commandPreparationError(err, directExit, rpcCode, directStage)
	}
	if prepared.Provision != nil && prepared.Provision.list {
		if request.Direct {
			return prepared, nil
		}
		return nil, commandPreparationError(
			errors.New("provision execution is required"), 2, "plan_failed",
		)
	}

	policy := commandPolicy(
		prepared.Definition, prepared.Loaded.Context, prepared.Arguments,
		prepared.Project, prepared.Remote,
	)
	if prepared.Init != nil {
		policy.Consequences = prepared.Init.consequences()
	}
	if prepared.Lifecycle != nil {
		policy = prepared.Lifecycle.policy(prepared.Definition, prepared.Loaded.Context)
	}
	if prepared.Provision != nil {
		policy = prepared.Provision.policy(prepared.Definition, prepared.Loaded.Context)
	}
	if prepared.Teardown != nil {
		policy = prepared.Teardown.policy(prepared.Definition, prepared.Loaded.Context)
	}
	action, delta, typedAction, actionErr := operationCLI.assessStructuredAction(
		ctx, prepared.Loaded, prepared.Definition, prepared.Project, prepared.Init,
		prepared.Lifecycle, prepared.Provision, prepared.Teardown,
	)
	if actionErr != nil {
		return nil, commandPreparationError(actionErr, 1, "plan_failed", "action")
	}
	orchestrator := operationCLI.operationOrchestrator(
		operationCLI.env["SUBYARD_OPERATION_ID"], prepared.Loaded, nil, &prepared.Definition,
	)
	switch {
	case prepared.Remote != nil:
		prepared.Plan, err = operationCLI.prepareRemoteOperation(
			orchestrator, prepared.Loaded, *prepared.Remote,
		)
	case prepared.Release != nil:
		prepared.Plan, err = prepared.Release.prepareAction(
			orchestrator, prepared.Loaded, prepared.Definition,
		)
	case prepared.TestVMs != nil:
		prepared.Plan, err = prepared.TestVMs.prepareAction(
			orchestrator, prepared.Loaded, prepared.Definition,
		)
	case typedAction:
		prepared.Plan, err = orchestrator.PrepareAction(
			prepared.Loaded.Context, prepared.Definition.Name,
			domain.RemotePolicy(prepared.Definition.Remote), action, delta,
		)
	default:
		policy = resolveCommandConfirmation(prepared.Definition, policy)
		prepared.Plan, err = orchestrator.Prepare(prepared.Loaded.Context, policy)
	}
	if err != nil {
		return nil, commandPreparationError(err, 1, "plan_failed", "plan")
	}
	return prepared, nil
}

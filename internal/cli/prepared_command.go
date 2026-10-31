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

	"github.com/Subyard/Subyard/internal/adapters/hostruntime"
	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/adapters/remotecontrol"
	"github.com/Subyard/Subyard/internal/adapters/shelladapter"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/uninstall"
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
	case "@client-projects":
		behavior.prepare = (*preparedCommand).prepareClientProjects
		behavior.nonRPCReason = "desktop integration requires the native controller"
		behavior.prepareExit, behavior.prepareRPCCode = 1, "plan_failed"
	case "@profile":
		behavior.prepare = (*preparedCommand).prepareProfile
		behavior.prepareExit, behavior.prepareRPCCode = 1, "plan_failed"
	case "@integration":
		behavior.prepare = (*preparedCommand).prepareIntegration
		behavior.prepareExit, behavior.prepareRPCCode = 1, "plan_failed"
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
	case "@service":
		behavior.prepare = (*preparedCommand).prepareService
		behavior.shellActions = lifecycleShellActions
	case "@test-vms":
		behavior.prepare = (*preparedCommand).prepareTestVMs
		behavior.shellActions = func(root string) map[string]map[string]shelladapter.Action {
			path := filepath.Join(root, "scripts/e2e-lab/invoke.sh")
			return map[string]map[string]shelladapter.Action{"test-vms": {
				"up": {Path: path, Direct: true}, "status": {Path: path, Direct: true, Capture: true},
				"down": {Path: path, Direct: true}, "revoke": {Path: path, Direct: true},
				"recover": {Path: path, Direct: true},
				"refresh": {Path: path, Direct: true, Timeout: 75 * time.Minute}, "retire-legacy": {Path: path, Direct: true},
			}}
		}
	case "@teardown":
		behavior.prepare = (*preparedCommand).prepareTeardown
		behavior.shellActions = func(root string) map[string]map[string]shelladapter.Action {
			actions := map[string]shelladapter.Action{}
			for _, phase := range []string{"apply", "prepare-lock", "check-sessions", "check-profiles", "remove-shared", "remove-files", "stop-services"} {
				actions[phase] = shelladapter.Action{Path: filepath.Join(root, "scripts/teardown-physical.sh"), Direct: true}
			}
			return map[string]map[string]shelladapter.Action{"teardown": actions}
		}
	case "@project", "@project-env":
		behavior.prepare = (*preparedCommand).prepareProject
	case "@remote":
		behavior.prepare = (*preparedCommand).prepareRemote
		behavior.prepareExit = 1
	case "@update":
		behavior.prepare = (*preparedCommand).prepareUpdate
		behavior.prepareExit, behavior.prepareRPCCode = 1, "plan_failed"
	case "@current-migration":
		behavior.prepare = (*preparedCommand).prepareCurrentMigration
		behavior.prepareExit, behavior.prepareRPCCode = 1, "plan_failed"
	case "@keys":
		behavior.prepare = (*preparedCommand).prepareKeys
		behavior.prepareExit, behavior.prepareRPCCode = 1, "plan_failed"
	case "@ssh-agent":
		behavior.nonRPCReason = "protected owner-host credential transport"
	case "@shell":
		behavior.nonRPCReason = "interactive terminal session"
	case "@config":
		behavior.prepare = (*preparedCommand).prepareConfig
		behavior.prepareExit, behavior.prepareRPCCode = 1, "plan_failed"
	case "@host":
		behavior.prepare = (*preparedCommand).prepareHost
		behavior.prepareExit, behavior.prepareRPCCode = 1, "plan_failed"
	case "@network":
		behavior.prepare = (*preparedCommand).prepareNetwork
		behavior.prepareExit, behavior.prepareRPCCode = 1, "plan_failed"
	case "@resource":
		if definition.Name == "svc" {
			behavior.nonRPCReason = "resource selector resolves its dedicated invocation"
			break
		}
		behavior.prepare = (*preparedCommand).prepareResourceInvocation
		behavior.prepareExit, behavior.prepareRPCCode = 1, "plan_failed"
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
	Loaded           config.Loaded
	Definition       command.Definition
	Arguments        []string
	ExplicitYard     bool
	ReadOnly         bool
	InteractiveSetup bool
	Bootstrap        *initBootstrap
	// OnResolved lets the direct boundary audit canonical inputs before assessment.
	OnResolved func(config.Loaded, []string)
}

type commandAssessment func(context.Context) (domain.ActionID, domain.ActionDelta, error)

// A prepared command owns one execution closure and its captured typed state.
// Project admission is shared lifecycle state, not a second execution variant.
type preparedCommand struct {
	CLI              *CLI
	Definition       command.Definition
	Arguments        []string
	Loaded           config.Loaded
	Plan             domain.OperationPlan
	exactState       string
	ownerPlan        bool
	interactiveSetup bool
	Project          *projectExecution
	release          *releaseExecution
	policy           domain.CommandPolicy
	assess           commandAssessment
	refresh          commandAssessment
	initExecution    *initExecution
	initFacts        string
	steps            func() []domain.OperationStep
	stepsComplete    bool
	execute          func(context.Context, *application.Orchestrator, io.Writer) (domain.AdapterResult, error)
	closeResource    func() error
	preview          func()
	displayOnly      func()
	printResult      func(domain.AdapterResult)
	remoteArguments  func([]string) ([]string, error)
	executeNoOp      bool
	lifecycle        *preparedLifecycle
	ownerAdmission   ownerWriteAdmission
	remotePhased     bool
}

// Both prepared commands and early source recovery capture these read-only
// facts before consent, then share the same owner barrier before any lock writer.
type ownerWriteAdmission struct {
	home       string
	generation []uninstall.Tree
	epoch      uint64
	err        error
}

func captureOwnerWriteAdmission(loaded config.Loaded, repositoryRoot string) (ownerWriteAdmission, error) {
	admission := ownerWriteAdmission{home: loaded.Context.Paths.DataHome}
	var err error
	admission.epoch, err = uninstall.ReadOwnerEpoch(admission.home)
	if err != nil {
		return admission, err
	}
	for _, path := range []string{admission.home, loaded.Context.Paths.ConfigHome, repositoryRoot} {
		if path == "" {
			continue
		}
		tree, err := uninstall.CaptureRoot(path)
		if err != nil {
			return admission, err
		}
		admission.generation = append(admission.generation, tree)
	}
	return admission, nil
}

// Unprepared native writers retain the first generation observed by this
// command, including startup recovery; later callbacks cannot adopt a new install.
func (cli *CLI) originalOwnerWrite(loaded config.Loaded) (ownerWriteAdmission, error) {
	cli.initializeOwnerAdmission()
	cli.ownerAdmissionMutex.Lock()
	defer cli.ownerAdmissionMutex.Unlock()
	if cli.ownerAdmission == nil {
		var contextErr error
		if loaded.Context.Paths.ConfigHome == "" {
			if operator := firstNonempty(cli.env["SUBYARD_OPERATOR_HOME"], cli.env["HOME"]); operator != "" {
				loaded.Context.Paths.ConfigHome, contextErr = config.ResolveConfigHome(operator, cli.env)
			}
		}
		admission, err := captureOwnerWriteAdmission(loaded, cli.options.RepositoryRoot)
		admission.err = errors.Join(contextErr, err)
		cli.ownerAdmission = &admission
	}
	if cli.ownerAdmission.home != loaded.Context.Paths.DataHome {
		return ownerWriteAdmission{}, domain.ErrPlanStale
	}
	original := *cli.ownerAdmission
	original.generation = append([]uninstall.Tree{}, original.generation...)
	return original, original.err
}

func (cli *CLI) admitOwnerWrite(ctx context.Context, loaded config.Loaded) (func(), error) {
	admission, err := cli.originalOwnerWrite(loaded)
	if err != nil {
		return nil, err
	}
	return cli.acquireOwnerMutation(ctx, admission, false, true)
}

func (cli *CLI) acquireOwnerMutation(ctx context.Context, admission ownerWriteAdmission, recovery, established bool) (func(), error) {
	cli.initializeOwnerAdmission()
	if cli.ownerMutationLock != nil {
		borrowed, err := uninstall.BorrowOwnerLock(admission.home)
		if err != nil {
			return nil, err
		}
		if borrowed == nil {
			return nil, errors.New("owner mutation barrier is no longer held")
		}
		defer borrowed.Close()
		actual, err := borrowed.Stat()
		current, statErr := os.Lstat(cli.ownerMutationLock.Path)
		if err != nil || statErr != nil || cli.ownerMutationLock.Path != uninstall.OwnerLockPath(admission.home) || !os.SameFile(actual, current) {
			return nil, domain.ErrPlanStale
		}
		if err := uninstall.CheckOwnerLockIdentity(*cli.ownerMutationLock); err != nil {
			return nil, err
		}
		return func() {}, uninstall.Admit(admission.home, admission.generation, admission.epoch, recovery, established)
	}
	lock, unlock, err := uninstall.OwnerLock(ctx, admission.home, cli.ownerMutationLockExpected)
	if err != nil {
		return nil, err
	}
	if err := uninstall.Admit(admission.home, admission.generation, admission.epoch, recovery, established); err != nil {
		unlock()
		return nil, err
	}
	cli.ownerMutationLock = &lock
	return func() {
		for _, root := range admission.generation {
			if root.Path == admission.home && len(root.Entries) == 0 {
				if created, err := uninstall.CaptureRoot(admission.home); err == nil {
					cli.ownerAdmissionMutex.Lock()
					cli.acceptOwnerHomePublication(admission.home, admission.epoch, created)
					cli.ownerAdmissionMutex.Unlock()
				}
			}
		}
		cli.ownerMutationLock = nil
		unlock()
	}, nil
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
	if request.Definition.Handler == "@teardown" {
		execution, parseErr := prepareTeardownExecution(request.Arguments)
		if parseErr != nil {
			return nil, parseErr
		}
		if execution.full && request.Loaded.Context.AccessKind != domain.AccessLocal {
			return nil, errors.New("--full must run locally on the owner host; remote operation plans cannot authorize full uninstall")
		}
	}
	behavior, err := resolveCoreCommand(request.Definition)
	if err != nil {
		return nil, err
	}
	if behavior.prepare == nil {
		return nil, fmt.Errorf("command has no prepared execution: %s", behavior.nonRPCReason)
	}
	if request.Definition.Handler == "@service" && request.Loaded.Context.AccessKind == domain.AccessLocal {
		yard, _, resolveErr := cli.selectServiceInvocation(ctx, request.Definition, request.Arguments)
		if resolveErr != nil {
			return nil, resolveErr
		}
		request.Loaded, request.Bootstrap, err = cli.loadServiceContext(yard, request.Arguments)
		if err != nil {
			return nil, err
		}
	}
	if request.Definition.Handler == "@test-vms" && request.Loaded.Context.AccessKind == domain.AccessLocal {
		selection, resolveErr := cli.resolveServiceCommand(request.Definition.Name, request.Arguments)
		if resolveErr != nil {
			return nil, resolveErr
		}
		request.Loaded, err = cli.loadServiceYard(selection.record.Yard)
		if err != nil {
			return nil, err
		}
		cli.serviceSelection = selection
	}
	cli.ensureOperationID()
	prepared := &preparedCommand{lifecycle: &preparedLifecycle{}, CLI: cli, Definition: request.Definition,
		Arguments: slices.Clone(request.Arguments), Loaded: request.Loaded, interactiveSetup: request.InteractiveSetup && !request.ReadOnly}
	defer func() {
		if err != nil {
			_ = prepared.Close()
		} else {
			prepared.seal()
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
		approved := prepared.Project.Record
		prepared.Project.recordApproved = &approved
		if prepared.Loaded.Context.AccessKind == domain.AccessLocal {
			prepared.Project.inputBaseline, err = cli.captureOwnerInputs(prepared.Loaded, "", nil)
			if err != nil {
				return nil, err
			}
			if prepared.Project.SecretPath != "" {
				info, statErr := os.Lstat(prepared.Project.SecretPath)
				if statErr != nil {
					return nil, statErr
				}
				identity, identityErr := ownerInputIdentity(prepared.Project.SecretPath, info)
				if identityErr != nil {
					return nil, identityErr
				}
				prepared.Project.secretIdentity = &identity
			}
		}
	}
	if request.OnResolved != nil {
		request.OnResolved(prepared.Loaded, slices.Clone(prepared.Arguments))
	}
	prepared.policy = commandPolicy(prepared.Definition, prepared.Loaded.Context, prepared.Arguments, prepared.Project)
	if prepared.Definition.Name == "sync" && prepared.Loaded.Context.AccessKind == domain.AccessRemote {
		if err = prepared.prepareRemoteProjectCopy(ctx); err != nil {
			return nil, &commandPreparationError{phase: "prepare", err: err}
		}
		return prepared, nil
	}
	if prepared.Definition.Name == "export" && prepared.Loaded.Context.AccessKind == domain.AccessRemote {
		if err = prepared.prepareRemoteProjectExport(ctx); err != nil {
			return nil, &commandPreparationError{phase: "prepare", err: err}
		}
		return prepared, nil
	}
	if ownerPreparedInvocation(prepared.Definition, prepared.Arguments) && prepared.Loaded.Context.AccessKind == domain.AccessRemote {
		if err = prepared.prepareRemoteOperation(ctx); err != nil {
			return nil, &commandPreparationError{phase: "prepare", err: err}
		}
		return prepared, nil
	}
	if err = behavior.prepare(prepared, ctx, request.Bootstrap); err != nil {
		return nil, &commandPreparationError{phase: "prepare", err: err}
	}
	if prepared.displayOnly != nil {
		return prepared, nil
	}
	if err := prepared.preparePlan(ctx); err != nil {
		return nil, err
	}
	return prepared, nil
}

func (prepared *preparedCommand) preparePlan(ctx context.Context) error {
	if prepared.execute == nil {
		return errors.New("prepared command has no execution")
	}
	orchestrator := prepared.CLI.operationPlanner(prepared.CLI.ensureOperationID())
	var action domain.ActionID
	var delta domain.ActionDelta
	var err error
	if prepared.assess != nil {
		action, delta, err = prepared.assess(ctx)
		if err != nil {
			return &commandPreparationError{phase: "assessment", err: err}
		}
	}
	var steps []domain.OperationStep
	if prepared.steps != nil {
		steps = domain.CloneOperationSteps(prepared.steps())
		if err := domain.ValidateOperationSteps(steps); err != nil {
			return &commandPreparationError{phase: "plan", err: err}
		}
	}
	if prepared.assess != nil {
		if prepared.steps != nil {
			delta.Consequences = domain.OperationStepConsequences(steps)
		}
		prepared.Plan, err = orchestrator.PrepareAction(prepared.Loaded.Context, prepared.policy.Name, prepared.policy.RemotePolicy, action, delta)
	} else {
		prepared.Plan, err = orchestrator.Prepare(prepared.Loaded.Context, resolveCommandConfirmation(prepared.Definition, prepared.policy))
	}
	if err != nil {
		return &commandPreparationError{phase: "plan", err: err}
	}
	prepared.Plan.Steps = steps

	if prepared.Loaded.Context.AccessKind == domain.AccessLocal {
		prepared.ownerAdmission, err = prepared.CLI.originalOwnerWrite(prepared.Loaded)
		if err != nil {
			return &commandPreparationError{phase: "owner generation", err: err}
		}
	}
	prepared.seal()
	return nil
}

// Copies share lifecycle ownership. Calls to Confirm, Execute and Close serialize;
// callers must not concurrently mutate the preview or other preparation inputs.
type preparedLifecycle struct {
	mu       sync.Mutex
	phase    preparedPhase
	scope    string
	plan     domain.OperationPlan
	closeErr error
}
type preparedPhase uint8

const (
	commandPrepared preparedPhase = iota
	commandAuthorized
	commandConsumed
	commandClosed
)

func (prepared *preparedCommand) scope() string {
	return operationStateDigest(struct {
		Plan       domain.OperationPlan
		Definition command.Definition
		Context    domain.Context
		Arguments  []string
		State      string
	}{prepared.Plan, prepared.Definition, prepared.Loaded.Context, prepared.Arguments, prepared.exactState})
}

func (prepared *preparedCommand) seal() {
	if prepared.lifecycle == nil {
		prepared.lifecycle = &preparedLifecycle{}
	}
	prepared.lifecycle.plan = prepared.Plan.Clone()
	prepared.lifecycle.scope = prepared.scope()
}

// Confirm is the sole transition that grants this command execution authority.
// An owner plan uses its resolved prompt policy, without a controller reassessment.
func (prepared *preparedCommand) Confirm(ctx context.Context, orchestrator *application.Orchestrator, assumeYes bool) (domain.OperationPlan, error) {
	if prepared == nil || prepared.lifecycle == nil {
		return domain.OperationPlan{}, errors.New("prepared command is not executable")
	}
	state := prepared.lifecycle
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.phase != commandPrepared {
		return domain.OperationPlan{}, errors.New("prepared command is not awaiting confirmation")
	}
	if state.scope != prepared.scope() {
		return domain.OperationPlan{}, domain.ErrPlanStale
	}
	confirmation := state.plan.Clone()
	confirm := orchestrator.Confirm
	if prepared.ownerPlan {
		confirm = orchestrator.ConfirmResolved
	}
	plan, err := confirm(ctx, confirmation, assumeYes)
	if err != nil {
		return domain.OperationPlan{}, err
	}
	prepared.Plan = plan.Clone()
	state.plan = plan.Clone()
	state.scope = prepared.scope()
	state.phase = commandAuthorized
	return plan.Clone(), nil
}

func (prepared *preparedCommand) Close() error {
	if prepared == nil {
		return nil
	}
	if prepared.lifecycle == nil {
		return nil
	}
	state := prepared.lifecycle
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.phase == commandClosed {
		return state.closeErr
	}
	state.phase = commandClosed
	if prepared.CLI != nil {
		prepared.CLI.abortProjectExecution(context.Background(), prepared.Project)
	}
	if prepared.Project != nil {
		state.closeErr = prepared.Project.closePreparedSource()
	}
	if prepared.closeResource != nil {
		state.closeErr = errors.Join(state.closeErr, prepared.closeResource())
	}
	return state.closeErr
}

// consume holds lifecycle ownership until the caller finishes its execution path.
func (prepared *preparedCommand) consume() (func(), error) {
	if prepared == nil || prepared.lifecycle == nil {
		return nil, errors.New("prepared command is not executable")
	}
	state := prepared.lifecycle
	state.mu.Lock()
	fail := func(err error) (func(), error) { state.mu.Unlock(); return nil, err }
	if state.phase == commandPrepared {
		return fail(domain.ErrConfirmationRequired)
	}
	if state.phase != commandAuthorized {
		return fail(errors.New("prepared command is not executable"))
	}
	if state.scope != prepared.scope() {
		return fail(domain.ErrPlanStale)
	}
	// Consume before admission: a refused attempt requires fresh preparation.
	state.phase = commandConsumed
	return state.mu.Unlock, nil
}

func (prepared *preparedCommand) Execute(ctx context.Context, orchestrator *application.Orchestrator, diagnostics io.Writer) (domain.AdapterResult, error) {
	if prepared == nil || prepared.lifecycle == nil || prepared.execute == nil {
		return domain.AdapterResult{}, errors.New("prepared command is not executable")
	}
	finish, err := prepared.consume()
	if err != nil {
		return domain.AdapterResult{}, err
	}
	defer finish()
	if prepared.Loaded.Context.AccessKind == domain.AccessLocal && !prepared.remotePhased {
		recovery := prepared.CLI.fullTeardownRecovery != "" || (prepared.Plan.Assessment != nil && prepared.Plan.Assessment.Action == "yard.teardown.full")
		unlock, lockErr := prepared.CLI.acquireOwnerMutation(ctx, prepared.ownerAdmission, recovery, false)
		if lockErr != nil {
			return domain.AdapterResult{}, lockErr
		}
		defer unlock()
	}
	noOp := func() (domain.AdapterResult, error) {
		return domain.AdapterResult{Schema: shelladapter.ProtocolSchema, OperationID: prepared.Plan.OperationID, Status: "ok"}, nil
	}
	release, err := prepared.CLI.beginProjectMutation(ctx, prepared.Project)
	if err != nil {
		return domain.AdapterResult{}, err
	}
	defer release()
	// Abort before releasing the host barrier, including failures after reservation.
	defer prepared.CLI.abortProjectExecution(context.Background(), prepared.Project)
	if prepared.initExecution != nil && prepared.initExecution.hooksOnly() && prepared.CLI.profileInitRepair == nil {
		var unlock func()
		ctx, unlock, err = prepared.initExecution.lock(ctx)
		if err != nil {
			return domain.AdapterResult{}, err
		}
		defer unlock()
	}
	if prepared.Plan.Assessment != nil && prepared.refresh != nil {
		action, delta, err := prepared.refresh(ctx)
		if err != nil {
			return domain.AdapterResult{}, err
		}
		if action != prepared.Plan.Assessment.Action {
			if prepared.initExecution != nil {
				return domain.AdapterResult{}, fmt.Errorf("%w: structured action changed after confirmation: approved={%s} current={%s}",
					domain.ErrPlanStale, prepared.initFacts, prepared.initExecution.actionFacts(action))
			}
			return domain.AdapterResult{}, fmt.Errorf("%w: structured action changed after confirmation", domain.ErrPlanStale)
		}
		if prepared.steps != nil {
			if err := domain.CheckOperationSteps(prepared.Plan.Steps, prepared.steps()); err != nil {
				return domain.AdapterResult{}, err
			}
		} else if delta.Changed && !slices.Equal(delta.Consequences, prepared.Plan.Assessment.Consequences) {
			return domain.AdapterResult{}, fmt.Errorf("%w: action consequences changed after confirmation", domain.ErrPlanStale)
		}
		if !prepared.Plan.Assessment.Changed && delta.Changed {
			return domain.AdapterResult{}, fmt.Errorf("%w: previously converged action requires new work", domain.ErrPlanStale)
		}
		if !delta.Changed {
			return prepared.commitResult(ctx, noOp)
		}
		if prepared.initExecution != nil {
			boundary, _ := ctx.Value(initExecutionLockKey{}).(*initExecutionBoundary)
			// Repair/provision paths can wait for privilege or another repair
			// lock before dispatch; retain their independent native guards.
			if boundary != nil {
				boundary.assessed = prepared.initExecution.hooksOnly()
			}
		}
	}
	if !prepared.executeNoOp && operationPlanNoOp(prepared.Plan) {
		return noOp()
	}
	if !prepared.ownerPlan {
		if err := prepared.CLI.reserveProjectExecution(ctx, prepared.Project); err != nil {
			return domain.AdapterResult{}, err
		}
	}
	return prepared.commitResult(ctx, func() (domain.AdapterResult, error) { return prepared.execute(ctx, orchestrator, diagnostics) })
}

func (prepared *preparedCommand) commitResult(ctx context.Context, execute func() (domain.AdapterResult, error)) (domain.AdapterResult, error) {
	result, err := execute()
	if err == nil && result.Status == "ok" && !prepared.ownerPlan && prepared.Project != nil && !operationPlanNoOp(prepared.Plan) {
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
	execution, err := cli.prepareInitExecutionPlan(ctx, prepared.Loaded, prepared.Arguments, bootstrap, true)
	if err != nil {
		return err
	}
	if err := cli.prepareInitProfileProvision(ctx, prepared.Loaded, execution, prepared.Arguments); err != nil {
		return err
	}
	if prepared.interactiveSetup {
		execution.profileSetup, err = cli.prepareInitProfiles(ctx, execution, prepared.Arguments)
		if err != nil {
			return err
		}
	}
	if err := execution.validateProfileRepair(ctx, cli); err != nil {
		return err
	}
	preset := ""
	if bootstrap != nil {
		preset = bootstrap.sourcePath
	}
	execution.inputBaseline, err = cli.captureOwnerInputs(prepared.Loaded, preset, execution.powerYards)
	if err != nil {
		return err
	}
	prepared.exactState = operationStateDigest(struct {
		Baseline *initIntegrationBaseline
		Adoption reconcileruntime.IntegrationPlan
		Inputs   string
		Captured string
	}{execution.integrationBaseline, execution.integrationAdoption, execution.inputBaseline.binding(), execution.stateBinding()})
	prepared.steps = execution.operationSteps
	prepared.stepsComplete = cli.options.InitPlatform == nil
	prepared.policy.Consequences = execution.consequences()
	prepared.initExecution = execution
	prepared.assess = func(context.Context) (domain.ActionID, domain.ActionDelta, error) {
		action, delta, err := execution.actionPlan()
		if err == nil {
			prepared.initFacts = execution.actionFacts(action)
		}
		return action, delta, err
	}
	prepared.refresh = func(ctx context.Context) (domain.ActionID, domain.ActionDelta, error) {
		if err := execution.legacyContinuation.check(ctx, cli, execution); err != nil {
			return "", domain.ActionDelta{}, err
		}
		if err := execution.inputBaseline.check(ctx, cli); err != nil {
			return "", domain.ActionDelta{}, err
		}
		if err := execution.checkIntegrationBaseline(cli); err != nil {
			return "", domain.ActionDelta{}, err
		}
		if err := execution.integrationSelection.check(ctx, cli, execution); err != nil {
			return "", domain.ActionDelta{}, err
		}
		if cli.options.InitPlatform == nil && execution.mode == initReconcile {
			if err := execution.refreshOrphanIngress(ctx, cli); err != nil {
				return "", domain.ActionDelta{}, err
			}
		}
		if err := execution.refreshAssessment(ctx, cli); err != nil {
			return "", domain.ActionDelta{}, err
		}
		if err := cli.observeInitProfileProvision(ctx, execution); err != nil {
			return "", domain.ActionDelta{}, err
		}
		// Another init can finish provisioning while this broader action awaits approval.
		// Keep its authorized upper bound; execution now performs only the bounded hook retry.
		if execution.hooksOnly() && prepared.Plan.Assessment.Action == "yard.init.reconcile" {
			return "yard.init.reconcile", domain.ActionDelta{
				Changed: execution.hooksApplicable, Consequences: slices.Clone(prepared.Plan.Assessment.Consequences),
			}, nil
		}
		return execution.actionPlan()
	}
	prepared.preview = func() {
		cli.printInitPlan(execution)
		if execution.hooksOnly() && execution.hooksApplicable {
			fmt.Fprintln(cli.options.Stdout, "  [ .. ] retry installed project hooks")
		}
	}
	prepared.printResult = func(domain.AdapterResult) {
		if execution.hooksOnly() {
			fmt.Fprintln(cli.options.Stdout, "  [ ok ] Everything is already set up")
		}
	}
	prepared.execute = func(ctx context.Context, orchestrator *application.Orchestrator, diagnostics io.Writer) (result domain.AdapterResult, runErr error) {
		defer func() {
			if runErr != nil && execution.adopting() && execution.legacyContinuation.published {
				runErr = fmt.Errorf("yard registration is complete; initialization did not finish; rerun yard init --adopt: %w", runErr)
			}
		}()
		if cli.profileInitRepair != nil {
			if err := execution.validateProfileRepair(ctx, cli); err != nil {
				return domain.AdapterResult{}, err
			}
			unlock, err := cli.lockConfigApplyRepair(ctx, cli.profileInitRepair)
			if err != nil {
				return domain.AdapterResult{}, err
			}
			defer unlock()
		}
		if cli.options.InitPlatform == nil && execution.mode != initConfigs && !execution.hooksOnly() {
			if err := cli.prepareSudoPrivileges(ctx, diagnostics, cli.effectiveUID(), prepared.Definition.Name); err != nil {
				return domain.AdapterResult{}, err
			}
			execution.rebuildPlatform(cli)
		}
		orchestrator.Runner = initAdapter{execution: execution, cli: cli, output: diagnostics}
		result, _, err := orchestrator.RunAdapter(ctx, prepared.Plan, domain.AdapterRequest{
			Schema: shelladapter.ProtocolSchema, OperationID: prepared.Plan.OperationID, Adapter: "init", Action: "reconcile",
		}, nil)
		if err == nil && execution.profileProvisionChanged() {
			result, err = cli.executeInitProfileProvision(ctx, execution, orchestrator, prepared.Plan, diagnostics)
		}
		if err == nil && cli.profileInitRepair != nil {
			err = cli.finishConfigApplyRepair(ctx, cli.profileInitRepair)
		}
		if err == nil && !cli.releaseTransitionChild {
			err = cli.verifyInitReleaseReadiness(ctx, execution.loaded.Context.YardName)
		}
		if err == nil && execution.mode != initConfigs && !execution.hooksOnly() {
			fmt.Fprintln(diagnostics, "  [ ok ] Subyard initialized")
		}
		return result, err
	}
	return nil
}

func (prepared *preparedCommand) prepareLifecycle(ctx context.Context, _ *initBootstrap) error {
	execution, err := prepareLifecycleExecution(prepared.Definition, prepared.Arguments)
	if err != nil {
		return err
	}
	prepared.policy = execution.policy(prepared.Definition, prepared.Loaded.Context)
	if err := prepared.CLI.observeLifecycleExecution(ctx, prepared.Loaded.Context, execution); err != nil {
		return err
	}
	var startup *resourceStartup
	var startupScope string
	if execution.action == "start" {
		startupScope, err = prepared.CLI.startupScopeBinding(ctx, prepared.Loaded)
		if err != nil {
			return err
		}
		startup, err = prepared.CLI.prepareResourceStartup(ctx, prepared.Loaded)
		if err != nil {
			return err
		}
		if startup != nil {
			prepared.policy.Consequences = append(prepared.policy.Consequences, startup.consequences...)
			prepared.assess = func(ctx context.Context) (domain.ActionID, domain.ActionDelta, error) {
				if err := startup.refresh(ctx, prepared.CLI); err != nil {
					return "", domain.ActionDelta{}, err
				}
				return "yard.start", domain.ActionDelta{Changed: true, Consequences: slices.Clone(prepared.policy.Consequences)}, nil
			}
			prepared.refresh = prepared.assess
		}
	}
	if execution.action == "stop" {
		prepared.assess = func(ctx context.Context) (domain.ActionID, domain.ActionDelta, error) {
			if err := prepared.CLI.observeLifecycleExecution(ctx, prepared.Loaded.Context, execution); err != nil {
				return "", domain.ActionDelta{}, err
			}
			return execution.actionPlan(prepared.Definition, prepared.Loaded.Context)
		}
		prepared.refresh = prepared.assess
	}
	prepared.exactState = operationStateDigest(struct{ Lifecycle, Startup, Scope string }{execution.binding(), startup.binding(), startupScope})
	prepared.stepsComplete = startup.stepsComplete()
	prepared.steps = func() []domain.OperationStep {
		return append(execution.steps(prepared.Loaded.Context), startup.steps()...)
	}
	prepared.executeNoOp = true
	prepared.execute = func(ctx context.Context, orchestrator *application.Orchestrator, diagnostics io.Writer) (domain.AdapterResult, error) {
		if err := prepared.CLI.observeLifecycleExecution(ctx, prepared.Loaded.Context, execution); err != nil {
			return domain.AdapterResult{}, err
		}
		if execution.action == "start" {
			current, err := prepared.CLI.startupScopeBinding(ctx, prepared.Loaded)
			if err != nil {
				return domain.AdapterResult{}, err
			}
			if current != startupScope {
				return domain.AdapterResult{}, fmt.Errorf("%w: startup resource scope changed", domain.ErrPlanStale)
			}
		}
		if startup == nil {
			return prepared.CLI.executeLifecycle(ctx, orchestrator, prepared.Loaded.Context, prepared.Plan, execution, diagnostics)
		}
		unlock, err := lockIntegrationYard(ctx, prepared.Loaded)
		if err != nil {
			return domain.AdapterResult{}, err
		}
		defer unlock()
		if err := startup.refresh(ctx, prepared.CLI); err != nil {
			return domain.AdapterResult{}, err
		}
		if prepared.CLI.options.NetworkPolicy == nil {
			if err := prepared.CLI.prepareSudoPrivileges(ctx, diagnostics, prepared.CLI.effectiveUID(), execution.action); err != nil {
				return domain.AdapterResult{}, err
			}
		}
		result, err := prepared.CLI.executeLifecycle(ctx, orchestrator, prepared.Loaded.Context, prepared.Plan, execution, diagnostics)
		if err != nil || result.Status != "ok" {
			return result, err
		}
		if err := startup.apply(ctx, prepared.CLI, prepared.Plan.OperationID); err != nil {
			return result, fmt.Errorf("yard started but resource activation is pending: %w", err)
		}
		return result, nil
	}
	return nil
}

func (prepared *preparedCommand) prepareProvision(ctx context.Context, _ *initBootstrap) error {
	prepared.stepsComplete = true
	execution, err := prepared.CLI.prepareProvisionExecution(prepared.Loaded, prepared.Arguments, prepared.Project)
	if err != nil {
		return err
	}
	if execution.list {
		prepared.displayOnly = func() { execution.printList(prepared.CLI.options.Stdout) }
		return nil
	}
	var bootstrap *profileBootstrap
	if execution.explicitProfile != "" {
		bootstrap, err = prepared.CLI.prepareProfileBootstrap(ctx, prepared.Loaded, execution.explicitProfile, prepared.Definition.Name, nil)
		if err != nil {
			return err
		}
		prepared.Loaded = bootstrap.loaded
	}
	var notes []string
	initial := prepared.Loaded
	if bootstrap != nil {
		initial = bootstrap.initial
	}
	inputs, err := prepared.CLI.captureOwnerInputs(initial, "", nil)
	if err != nil {
		return err
	}
	readAddresses := prepared.CLI.provisionEndpointAddresses
	if readAddresses == nil {
		readAddresses = hostruntime.OwnerIPv4Addresses
	}
	execution.endpoint, notes, err = prepared.CLI.prepareProvisionEndpoint(prepared.Loaded, execution.profiles, readAddresses)
	if err != nil {
		return err
	}
	for _, note := range notes {
		fmt.Fprintln(prepared.CLI.options.Stderr, note)
	}
	prepared.policy = execution.policy(prepared.Definition, prepared.Loaded.Context)
	prepared.assess = func(ctx context.Context) (domain.ActionID, domain.ActionDelta, error) {
		if err := bootstrap.refresh(ctx, prepared.CLI); err != nil {
			return "", domain.ActionDelta{}, err
		}
		var err error
		if bootstrap != nil && bootstrap.init != nil {
			observation := *bootstrap.init
			observation.profileProvision = execution
			err = prepared.CLI.observeInitProfileProvision(ctx, &observation)
		} else {
			err = prepared.CLI.observeProvisionExecution(ctx, prepared.Loaded, prepared.Definition, execution)
		}
		if err != nil {
			return "", domain.ActionDelta{}, err
		}
		action, delta, err := execution.actionPlan(prepared.Definition, prepared.Loaded.Context)
		assessment := bootstrap.augment(domain.ActionAssessment{Changed: delta.Changed, Consequences: delta.Consequences})
		delta.Changed, delta.Consequences = assessment.Changed, assessment.Consequences
		if prepared.exactState == "" {
			prepared.exactState = operationStateDigest(struct{ Inputs, Provision, Bootstrap string }{inputs.binding(), execution.stateBinding(), bootstrap.stateBinding()})
		}
		return action, delta, err
	}
	prepared.steps = func() []domain.OperationStep {
		return append(bootstrap.operationSteps(), execution.operationSteps(prepared.Loaded)...)
	}
	prepared.refresh = func(ctx context.Context) (domain.ActionID, domain.ActionDelta, error) {
		if err := inputs.check(ctx, prepared.CLI); err != nil {
			return "", domain.ActionDelta{}, err
		}
		return prepared.assess(ctx)
	}
	prepared.execute = func(ctx context.Context, orchestrator *application.Orchestrator, diagnostics io.Writer) (domain.AdapterResult, error) {
		if err := inputs.check(ctx, prepared.CLI); err != nil {
			return domain.AdapterResult{}, err
		}
		if bootstrap != nil {
			if err := bootstrap.apply(ctx, prepared.CLI); err != nil {
				return domain.AdapterResult{}, err
			}
			if err := execution.endpoint.acceptProfileBootstrap(prepared.CLI, bootstrap); err != nil {
				return domain.AdapterResult{}, err
			}
		}
		return prepared.CLI.executeProvision(ctx, orchestrator, prepared.Loaded, prepared.Plan, execution, diagnostics)
	}
	return nil
}

func (prepared *preparedCommand) prepareTestVMs(ctx context.Context, _ *initBootstrap) error {
	prepared.stepsComplete = true
	if prepared.Loaded.Context.AccessKind == domain.AccessRemote && testVMStatusInvocation(prepared.Arguments) {
		session, err := prepared.CLI.openOwnerRPC(ctx, prepared.Loaded.Context)
		if err != nil {
			return err
		}
		defer session.close()
		if err := session.negotiate(ctx); err != nil {
			return err
		}
		if !slices.Contains(session.capabilities, ownerServicesCapability) {
			return errors.New("owner service routing unavailable; update Subyard on the owner host")
		}
		if prepared.CLI.serviceExpectedOwner == "" {
			return errors.New("selected service owner identity is unavailable")
		}
		prepared.Arguments = append(slices.Clone(prepared.Arguments), "--expect-owner", prepared.CLI.serviceExpectedOwner)
	}
	selection := prepared.CLI.serviceSelection
	_, nativeArguments, err := serviceOptions(prepared.Arguments)
	if err != nil {
		return err
	}
	if selection != nil {
		if err := prepared.CLI.checkServiceSelection(ctx, prepared.Loaded, selection); err != nil {
			return err
		}
	}
	execution, err := prepared.CLI.prepareTestVMExecution(ctx, prepared.Loaded, nativeArguments)
	if err != nil {
		return err
	}
	prepared.assess = func(context.Context) (domain.ActionID, domain.ActionDelta, error) { return execution.actionPlan() }
	execution.service = selection
	prepared.exactState = execution.binding()
	prepared.steps = execution.steps
	prepared.executeNoOp = true
	prepared.remoteArguments = execution.remoteArguments
	prepared.execute = func(ctx context.Context, orchestrator *application.Orchestrator, diagnostics io.Writer) (domain.AdapterResult, error) {
		return prepared.CLI.executeTestVMs(ctx, orchestrator, prepared.Loaded, prepared.Plan, execution, diagnostics)
	}
	return nil
}

func (prepared *preparedCommand) prepareTeardown(ctx context.Context, _ *initBootstrap) error {
	prepared.stepsComplete = true
	prepared.executeNoOp = true
	execution, err := prepareTeardownExecution(prepared.Arguments)
	if err != nil {
		return err
	}
	if execution.full {
		if err := prepared.bindFullInstallation(); err != nil {
			return err
		}

		if prepared.CLI.fullTeardownRecovery != "" {
			if err := prepared.CLI.restoreFullTeardown(execution); err != nil {
				return err
			}
		}
	}
	prepared.policy = execution.policy(prepared.Definition, prepared.Loaded.Context)
	prepared.assess = func(ctx context.Context) (domain.ActionID, domain.ActionDelta, error) {
		if execution.full {
			full := execution
			if full.resumed {
				if err := full.RecoveryBinding.Check(); err != nil {
					return "", domain.ActionDelta{}, err
				}
			} else if err := prepared.CLI.observeFullTeardown(ctx, prepared.Loaded, full); err != nil {
				return "", domain.ActionDelta{}, err
			}
			prepared.exactState = full.installationBinding()
			if permit := prepared.CLI.teardownRepair; permit != nil {
				prepared.exactState = operationStateDigest([]string{prepared.exactState, permit.factsFingerprint})
			}
			return "yard.teardown.full", domain.ActionDelta{Changed: true, Consequences: []string{"permanently remove this installation and all its local yards"}}, nil
		}
		if err := prepared.CLI.observeTeardownExecution(ctx, prepared.Loaded, execution); err != nil {
			return "", domain.ActionDelta{}, err
		}
		if prepared.exactState == "" {
			prepared.exactState = execution.binding()
			if permit := prepared.CLI.teardownRepair; permit != nil {
				prepared.exactState = operationStateDigest([]string{prepared.exactState, permit.factsFingerprint})
			}
		}
		return execution.actionPlan(prepared.Definition, prepared.Loaded.Context)
	}
	prepared.refresh = prepared.assess
	prepared.steps = func() []domain.OperationStep {
		if execution.full {
			return execution.installationSteps(prepared.CLI)
		}
		return execution.steps(prepared.Loaded.Context)
	}
	prepared.execute = func(ctx context.Context, orchestrator *application.Orchestrator, diagnostics io.Writer) (domain.AdapterResult, error) {
		permit := prepared.CLI.teardownRepair
		if permit != nil {
			unlock, err := prepared.CLI.lockConfigApplyRepair(ctx, permit)
			if err != nil {
				return domain.AdapterResult{}, err
			}
			defer unlock()
		}
		result, err := prepared.CLI.executeTeardown(ctx, orchestrator, prepared.Loaded, prepared.Plan, execution, diagnostics)
		if err == nil && result.Status == "ok" && permit != nil && !execution.full {
			err = prepared.CLI.finishTeardownRepair(ctx, permit)
		}
		return result, err
	}
	if !execution.full {
		return prepared.bindServiceTeardown(ctx)
	}
	return nil
}

func (prepared *preparedCommand) prepareRemote(ctx context.Context, _ *initBootstrap) error {
	execution, err := prepared.CLI.prepareRemoteExecution(ctx, prepared.Loaded, prepared.Arguments)
	if err != nil {
		return err
	}
	prepared.policy = application.RemotePolicy(*execution)
	prepared.exactState = operationStateDigest(execution)
	prepared.exactState = operationStateDigest(struct{ State, Native string }{prepared.exactState, execution.Binding})
	if len(execution.Steps) != 0 {
		prepared.stepsComplete = execution.Binding != ""
		prepared.steps = func() []domain.OperationStep { return domain.CloneOperationSteps(execution.Steps) }
	}
	prepared.assess = func(context.Context) (domain.ActionID, domain.ActionDelta, error) {
		return application.RemoteActionPlan(*execution)
	}
	control := prepared.CLI.remoteService(prepared.Loaded).Control
	if native, ok := control.(remotecontrol.Runtime); ok && execution.Action == domain.RemoteAdd && execution.Binding != "" && prepared.Loaded.Context.AccessKind == domain.AccessLocal {
		prepared.remotePhased = true
		native.AdmitMutation = func(ctx context.Context) (func(), error) {
			// Retrieve only the cached first admission, including our own home publication.
			original, err := prepared.CLI.originalOwnerWrite(prepared.Loaded)
			if err != nil {
				return nil, err
			}
			return prepared.CLI.acquireOwnerMutation(ctx, original, false, false)
		}
		control = native
	}
	prepared.printResult = prepared.CLI.printRemoteResult
	prepared.execute = func(ctx context.Context, orchestrator *application.Orchestrator, _ io.Writer) (domain.AdapterResult, error) {
		orchestrator.Runner = application.RemoteRunner{Control: control, Prepared: *execution}
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
	prepared.release = execution
	prepared.exactState = execution.prepared.Binding
	prepared.steps = func() []domain.OperationStep { return domain.CloneOperationSteps(execution.prepared.Steps) }
	prepared.stepsComplete = execution.prepared.Binding != "" && len(execution.prepared.Steps) != 0
	prepared.executeNoOp = true
	prepared.preview = func() {
		prepared.CLI.printUpdatePreview(execution)
	}
	prepared.assess = func(context.Context) (domain.ActionID, domain.ActionDelta, error) {
		return execution.prepared.Action, domain.ActionDelta{Changed: execution.prepared.Changed, Consequences: execution.prepared.Consequences}, nil
	}
	prepared.execute = func(ctx context.Context, orchestrator *application.Orchestrator, _ io.Writer) (domain.AdapterResult, error) {
		if err := prepared.CLI.beginUpdateHistory(prepared.Plan, execution); err != nil {
			return domain.AdapterResult{}, fmt.Errorf("create update history: %w", err)
		}
		result, runErr := prepared.CLI.executeRelease(ctx, orchestrator, prepared.Plan, execution)
		return result, prepared.CLI.finishUpdateHistory(ctx, execution, result, runErr)
	}
	return nil
}

func (prepared *preparedCommand) prepareProject(ctx context.Context, _ *initBootstrap) error {
	project := prepared.Project
	if project == nil {
		return errors.New("project execution is required")
	}
	cli, loaded, definition := prepared.CLI, prepared.Loaded, prepared.Definition
	if definition.Name == "sync" || definition.Name == "clone" || definition.Name == "export" {
		if err := project.prepareSource(ctx, cli); err != nil {
			return err
		}
		if definition.Name == "export" {
			if err := project.prepareProjectExportDestination(ctx, cli); err != nil {
				return err
			}
		}
		prepared.exactState = project.sourceBinding()
	}
	prepared.steps = func() []domain.OperationStep { return project.operationSteps(definition.Name) }
	prepared.stepsComplete = true
	switch definition.Name {
	case "remove":
		prepared.assess = func(ctx context.Context) (domain.ActionID, domain.ActionDelta, error) {
			if err := cli.prepareProjectRemoval(ctx, project); err != nil {
				return "", domain.ActionDelta{}, err
			}
			if prepared.exactState == "" {
				prepared.exactState = project.sourceBinding()
			}
			return project.removeActionPlan()
		}
	case "sync", "bind", "clone", "export", "up", "down":
		prepared.assess = func(ctx context.Context) (domain.ActionID, domain.ActionDelta, error) {
			if err := cli.observeProjectAction(ctx, definition.Name, project); err != nil {
				return "", domain.ActionDelta{}, err
			}
			if prepared.Plan.OperationID == "" {
				prepared.exactState = project.sourceBinding()
			}
			return project.actionPlan(definition.Name)
		}
	}
	prepared.refresh = func(ctx context.Context) (domain.ActionID, domain.ActionDelta, error) {
		if err := project.inputBaseline.check(ctx, cli); err != nil {
			return "", domain.ActionDelta{}, err
		}
		if err := project.checkPreparedSource(ctx, cli); err != nil {
			return "", domain.ActionDelta{}, err
		}
		return prepared.assess(ctx)
	}
	if definition.Handler == "@project" {
		prepared.execute = func(ctx context.Context, orchestrator *application.Orchestrator, diagnostics io.Writer) (domain.AdapterResult, error) {
			incusPort, _ := cli.statusPorts()
			orchestrator.Runner = application.ProjectActionRunner{
				Data: cli.projectDataPlane(), Devices: cli.projectDeviceManager(), Archive: cli.projectArchiver(),
				PreparedArchive: project.preparedArchive, CloneRevision: project.cloneRevision, CloneUnborn: project.cloneUnborn, ExportObservedTree: project.exportObservedTree,
				ExportSourceTree: project.exportSourceTree,
				VerifyExport: func(ctx context.Context) error {
					return cli.verifyLocalProjectExport(ctx, project)
				},
				VerifyPrepared: true, RemovalApproval: project.removalApproval(),
				Exports: executionProjectExportStore(project, cli.projectExportStore(loaded)), Instances: incusPort, VSCode: cli.projectVSCode(loaded),
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
				info, err := os.Lstat(project.SecretPath)
				if err != nil {
					return domain.AdapterResult{}, err
				}
				identity, err := ownerInputIdentity(project.SecretPath, info)
				if err != nil {
					return domain.AdapterResult{}, err
				}
				if project.secretIdentity == nil || identity != *project.secretIdentity {
					return domain.AdapterResult{}, fmt.Errorf("%w: protected project input changed", domain.ErrPlanStale)
				}
				file, err := os.Open(project.SecretPath)
				if err != nil {
					return domain.AdapterResult{}, err
				}
				opened, err := file.Stat()
				if err != nil {
					_ = file.Close()
					return domain.AdapterResult{}, err
				}
				openedIdentity, err := ownerInputIdentity(project.SecretPath, opened)
				if err != nil || openedIdentity != identity {
					_ = file.Close()
					return domain.AdapterResult{}, fmt.Errorf("%w: protected project input was replaced", domain.ErrPlanStale)
				}
				protected = file
				defer protected.Close()
			}
			orchestrator.Runner = application.ProjectEnvironmentRunner{
				Data: cli.projectDataPlane(), Yard: loaded.Context, Project: project.Record,
				Profile: project.Profile, HostLinks: project.HostLinks,
				Rebuild: project.Environment["SUBYARD_PROJECT_REBUILD"] == "1", HasSecret: project.SecretPath != "",
				PreparedObservation: project.approvedEnvironment,
			}
			result, stderr, err := orchestrator.RunAdapter(ctx, prepared.Plan, domain.AdapterRequest{
				Schema: shelladapter.ProtocolSchema, OperationID: prepared.Plan.OperationID, Adapter: "project-env", Action: definition.Name,
			}, protected)
			if err == nil && result.Status == "ok" && project.SecretPath != "" {
				file := protected.(*os.File)
				opened, statErr := file.Stat()
				if statErr != nil {
					return result, statErr
				}
				identity, identityErr := ownerInputIdentity(project.SecretPath, opened)
				current, pathErr := os.Lstat(project.SecretPath)
				if identityErr != nil || pathErr != nil || identity != *project.secretIdentity || !os.SameFile(opened, current) {
					return result, fmt.Errorf("%w: protected project input changed during transfer", domain.ErrPlanStale)
				}
			}
			writeAdapterDiagnostics(diagnostics, stderr)
			return result, err
		}
	}
	return nil
}

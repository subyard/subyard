package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/shellquote"
)

const resourcePrepareTimeout = 30 * time.Second

func (cli *CLI) runResourceCommand(
	ctx context.Context,
	loaded config.Loaded,
	definition resource.Definition,
	arguments []string,
	globalAssumeYes bool,
) int {
	if len(arguments) == 2 && arguments[0] == "--session-wire" &&
		loaded.Context.AccessKind == domain.AccessLocal && slices.Contains(definition.ControllerSessions, arguments[1]) {
		cli.resourceWire = arguments[1]
		defer func() { cli.resourceWire = "" }()
		runner := &resourceApplyRunner{cli: cli, loaded: loaded, definition: definition,
			arguments: arguments, effect: domain.ActionSession}
		_, _, err := runner.Run(ctx, domain.AdapterRequest{Adapter: "resource", OperationID: cli.ensureOperationID(), Arguments: arguments[1:]}, nil)
		if err != nil {
			var sessionExit *resourceSessionExitError
			if errors.As(err, &sessionExit) {
				return sessionExit.code
			}
			cli.errorf("%s: session transport: %v", definition.Command, err)
			return 1
		}
		return 0
	}
	invocation, err := parseResourceInvocation(arguments)
	if err != nil {
		cli.errorf("%s: %v", definition.Command, err)
		return 2
	}
	if invocation.help {
		return cli.runResourceHelp(ctx, loaded, definition)
	}
	if !slices.Contains(definition.Verbs, invocation.verb) {
		cli.errorf("%s: %v", definition.Command, fmt.Errorf(
			"%w: resource %q does not declare verb %q",
			resource.ErrResourceActionUnknown, definition.Command, invocation.verb,
		))
		return 2
	}
	if loaded.Context.AccessKind == domain.AccessRemote && definition.RemotePolicy(invocation.verb) == domain.RemoteOnController {
		remote := []string{"yard"}
		if loaded.Context.OwnerYardName != "" {
			remote = append(remote, "-Y", loaded.Context.OwnerYardName)
		}
		remote = append(remote, definition.Command, "--session-wire", invocation.verb)
		for index := range remote {
			remote[index] = shellquote.Word(remote[index])
		}
		ssh, err := cli.sshArguments(ctx, loaded.Context.OwnerEndpoint, []string{"-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=3", "-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "ClearAllForwardings=yes", loaded.Context.OwnerEndpoint, "--", "bash", "-lc", shellquote.Word(strings.Join(remote, " "))})
		if err != nil {
			cli.errorf("SSH trust: %v", err)
			return 1
		}
		encoded, err := json.Marshal(append([]string{"ssh"}, ssh...))
		if err != nil {
			cli.errorf("session transport: %v", err)
			return 1
		}
		cli.resourceSessionTransport = string(encoded)
		defer func() { cli.resourceSessionTransport = "" }()
	}
	baseline := loaded
	bootstrap, err := cli.prepareResourceBootstrap(ctx, loaded, definition, invocation.arguments)
	if err != nil {
		cli.errorf("%s: prepare bootstrap: %v", definition.Command, err)
		return 1
	}
	if bootstrap != nil {
		loaded = bootstrap.loaded
	}
	if definition.Endpoint != nil && invocation.verb == "status" {
		cli.printResourceEndpoint(loaded, definition)
	}

	output, err := cli.prepareResource(ctx, loaded, definition, invocation.arguments)
	if err != nil {
		cli.errorf("%s: %v", definition.Command, err)
		if errors.Is(err, resource.ErrResourceUsageInvalid) {
			return 2
		}
		return 1
	}
	native, err := cli.resources.PrepareResult(
		cli.coreActions, definition.Command, invocation.verb, output,
	)
	if err != nil {
		cli.errorf("%s: %v", definition.Command, err)
		return 1
	}
	assessment := native.Assessment
	resourceConsequences := slices.Clone(assessment.Consequences)
	assessment = bootstrap.augment(assessment)
	ingress, err := cli.prepareResourceIngress(ctx, loaded, definition, invocation.verb)
	if err != nil {
		cli.errorf("%s: prepare ingress: %v", definition.Command, err)
		return 1
	}
	intent, err := cli.prepareResourceStartupIntent(ctx, loaded, definition, invocation.verb)
	if err != nil {
		cli.errorf("%s: prepare startup intent: %v", definition.Command, err)
		return 1
	}
	assessment = intent.augment(assessment)
	assessment = ingress.augment(assessment)
	localAction, ok := localResourceAction(definition, assessment.Action)
	if !ok {
		cli.errorf("%s: %v", definition.Command, fmt.Errorf(
			"%w: prepared action %q does not belong to the selected resource",
			resource.ErrResourceActionUnknown, assessment.Action,
		))
		return 1
	}

	runner := &resourceApplyRunner{
		cli: cli, loaded: loaded, definition: definition, verb: invocation.verb,
		localAction: localAction, effect: assessment.Effect, arguments: slices.Clone(invocation.arguments),
		bootstrap: bootstrap, consequences: resourceConsequences, ingress: ingress, startupIntent: intent,
	}
	if native.Schema == resource.PrepareAssessmentSchemaV2 && cli.resourceExactInvocation(definition, arguments) {
		prepared := &preparedCommand{CLI: cli, Definition: resourceCommandDefinition(definition), Arguments: slices.Clone(arguments), Loaded: loaded}
		if err := prepared.attachResourceExecution(runner, baseline, native); err != nil {
			cli.errorf("%s: prepare exact resource: %v", definition.Command, err)
			return 1
		}
		defer prepared.Close()
		if err := prepared.preparePlan(ctx); err != nil {
			cli.errorf("%s: plan exact resource: %v", definition.Command, err)
			return 1
		}
		return cli.runPreparedCommand(ctx, prepared, globalAssumeYes || invocation.assumeYes || cli.env["ASSUME_YES"] == "1")
	}

	operationID := cli.env["SUBYARD_OPERATION_ID"]
	orchestrator := cli.operationOrchestrator(operationID, loaded, nil, nil)
	plan, err := orchestrator.PlanAction(
		ctx,
		loaded.Context,
		definition.Command,
		definition.RemotePolicy(invocation.verb),
		assessment.Action,
		domain.ActionDelta{
			Changed: assessment.Changed, Consequences: slices.Clone(assessment.Consequences),
		},
		globalAssumeYes || invocation.assumeYes || cli.env["ASSUME_YES"] == "1",
	)
	if errors.Is(err, application.ErrDeclined) {
		cli.errorf("%s: operation declined", definition.Command)
		return 1
	}
	if err != nil {
		cli.errorf("%s: %v", definition.Command, err)
		return 1
	}
	if !assessment.Changed &&
		(assessment.Effect == domain.ActionMutation || assessment.Effect == domain.ActionDestruction) {
		return 0
	}
	if ingress != nil && assessment.Changed {
		unlock, lockErr := lockIntegrationYard(ctx, loaded)
		if lockErr != nil {
			cli.errorf("%s: lock ingress yard: %v", definition.Command, lockErr)
			return 1
		}
		defer unlock()
	}
	if assessment.Changed &&
		(assessment.Effect == domain.ActionMutation || assessment.Effect == domain.ActionDestruction) {
		if err := bootstrap.refresh(ctx, cli); err != nil {
			cli.errorf("%s: refresh bootstrap: %v", definition.Command, err)
			return 1
		}
		if err := intent.refresh(ctx, cli); err != nil {
			cli.errorf("%s: refresh startup intent: %v", definition.Command, err)
			return 1
		}
		refreshedOutput, refreshErr := cli.prepareResource(ctx, loaded, definition, invocation.arguments)
		if refreshErr != nil {
			cli.errorf("%s: refresh assessment: %v", definition.Command, refreshErr)
			return 1
		}
		refreshed, refreshErr := cli.resources.AssessPrepareResult(
			cli.coreActions, definition.Command, invocation.verb, refreshedOutput,
		)
		if refreshErr != nil {
			cli.errorf("%s: refresh assessment: %v", definition.Command, refreshErr)
			return 1
		}
		refreshed = bootstrap.augment(refreshed)
		if err := ingress.refresh(ctx, cli); err != nil {
			cli.errorf("%s: refresh ingress: %v", definition.Command, err)
			return 1
		}
		refreshed = intent.augment(refreshed)
		refreshed = ingress.augment(refreshed)
		if refreshed.Action != assessment.Action {
			cli.errorf("%s: %v: resource action changed after confirmation",
				definition.Command, domain.ErrPlanStale)
			return 1
		}
		if !refreshed.Changed {
			return 0
		}
		if !slices.Equal(refreshed.Consequences, assessment.Consequences) {
			cli.errorf("%s: %v: resource consequences changed after confirmation",
				definition.Command, domain.ErrPlanStale)
			return 1
		}
	}
	if ingress != nil && ingress.up && assessment.Changed && cli.options.NetworkPolicy == nil {
		if err := cli.prepareSudoPrivileges(ctx, cli.options.Stderr, cli.effectiveUID(), definition.Command); err != nil {
			cli.errorf("%s: authorize public UDP recovery: %v", definition.Command, err)
			return 1
		}
	}

	orchestrator.Runner = runner
	_, diagnostics, err := orchestrator.RunAdapter(ctx, plan, domain.AdapterRequest{
		Schema:      1,
		OperationID: plan.OperationID,
		Adapter:     "resource",
		Action:      localAction,
		Arguments:   slices.Clone(invocation.arguments[1:]),
	}, nil)
	writeAdapterDiagnostics(cli.options.Stderr, diagnostics)
	if err != nil {
		var sessionExit *resourceSessionExitError
		if errors.As(err, &sessionExit) {
			return sessionExit.code
		}
		cli.errorf("%s: apply: %v", definition.Command, err)
		return 1
	}
	return 0
}

type resourceInvocation struct {
	verb      string
	arguments []string
	assumeYes bool
	help      bool
}

func parseResourceInvocation(arguments []string) (resourceInvocation, error) {
	invocation := resourceInvocation{}
	beforeSeparator := true
	for _, argument := range arguments {
		if beforeSeparator && argument == "--" {
			beforeSeparator = false
			invocation.arguments = append(invocation.arguments, argument)
			continue
		}
		if beforeSeparator && (argument == "-y" || argument == "--yes") {
			invocation.assumeYes = true
			continue
		}
		invocation.arguments = append(invocation.arguments, argument)
	}
	if len(invocation.arguments) == 0 {
		invocation.help = true
		return invocation, nil
	}
	if invocation.arguments[0] == "-h" || invocation.arguments[0] == "--help" ||
		invocation.arguments[0] == "help" {
		invocation.help = true
		return invocation, nil
	}
	if invocation.arguments[0] == "--" {
		return resourceInvocation{}, fmt.Errorf("%w: resource verb is required", resource.ErrResourceActionUnknown)
	}
	invocation.verb = invocation.arguments[0]
	return invocation, nil
}

func (cli *CLI) prepareResource(
	ctx context.Context,
	loaded config.Loaded,
	definition resource.Definition,
	arguments []string,
) ([]byte, error) {
	return cli.prepareResourceMode(ctx, loaded, definition, arguments, "prepare")
}

func (cli *CLI) prepareResourceMode(
	ctx context.Context,
	loaded config.Loaded,
	definition resource.Definition,
	arguments []string,
	mode string,
) ([]byte, error) {
	return cli.prepareResourceModeTimeout(ctx, loaded, definition, arguments, mode, resourcePrepareTimeout)
}

func (cli *CLI) prepareResourceModeTimeout(
	ctx context.Context,
	loaded config.Loaded,
	definition resource.Definition,
	arguments []string,
	mode string,
	timeout time.Duration,
	nativeContext ...map[string]string,
) ([]byte, error) {
	if err := validateResourceHandler(definition.HandlerPath()); err != nil {
		return nil, err
	}
	prepareContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(prepareContext, definition.HandlerPath(), arguments...)
	configureResourceProcess(command)
	command.Dir = cli.options.WorkingDir
	command.Env = cli.resourceEnvironment(loaded, definition, mode, "", "")
	if len(nativeContext) != 0 {
		command.Env = environmentList(environmentMap(command.Env), nativeContext[0])
	}
	command.Stdin = nil
	stdout := &boundedResourceBuffer{limit: resource.MaxPrepareOutputBytes}
	stderr := &boundedResourceBuffer{limit: resource.MaxPrepareOutputBytes}
	command.Stdout = stdout
	command.Stderr = stderr
	err := command.Run()
	if prepareContext.Err() != nil {
		return nil, fmt.Errorf("%w: prepare timed out or was cancelled: %v",
			resource.ErrResourcePlanInvalid, prepareContext.Err())
	}
	if stdout.overflow || stderr.overflow {
		return nil, fmt.Errorf("%w: prepare output exceeded %d bytes",
			resource.ErrResourcePlanInvalid, resource.MaxPrepareOutputBytes)
	}
	if err != nil {
		failureClass := resource.ErrResourcePlanInvalid
		var exitError *exec.ExitError
		if errors.As(err, &exitError) && exitError.ExitCode() == 2 {
			failureClass = resource.ErrResourceUsageInvalid
		}
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return nil, fmt.Errorf("%w: prepare failed: %v: %s",
				failureClass, err, detail)
		}
		return nil, fmt.Errorf("%w: prepare failed: %v", failureClass, err)
	}
	return slices.Clone(stdout.Bytes()), nil
}

func (cli *CLI) runResourceHelp(
	ctx context.Context,
	loaded config.Loaded,
	definition resource.Definition,
) int {
	if err := validateResourceHandler(definition.HandlerPath()); err != nil {
		cli.errorf("%s: %v", definition.Command, err)
		return 2
	}
	command := exec.CommandContext(ctx, definition.HandlerPath(), "--help")
	configureResourceProcess(command)
	command.Dir = cli.options.WorkingDir
	command.Env = cli.resourceEnvironment(loaded, definition, "", "", "")
	command.Stdout = cli.options.Stdout
	command.Stderr = cli.options.Stderr
	if err := command.Run(); err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return exitError.ExitCode()
		}
		cli.errorf("%s: help: %v", definition.Command, err)
		return 1
	}
	return 0
}

func (cli *CLI) resourceEnvironment(
	loaded config.Loaded,
	definition resource.Definition,
	mode string,
	localAction string,
	operationID string,
) []string {
	values := structuredCommandContext(loaded)
	for _, name := range []string{
		"ASSUME_YES", "SUBYARD_OPERATION_ID", "SUBYARD_RESOURCE_ACTION",
		"SUBYARD_RESOURCE_MODE", "SUBYARD_RESOURCE_VERB", "SUBYARD_SUDO_PREAUTHORIZED",
	} {
		delete(values, name)
	}
	for name, value := range cli.handlerEnvironment(definition.Command, "") {
		values[name] = value
	}
	path := cli.baseEnv["PATH"]
	if path == "" {
		path = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	values["PATH"] = path
	values["LANG"] = "C.UTF-8"
	values["LC_ALL"] = "C.UTF-8"
	if cli.resourceSessionTransport != "" {
		values["SUBYARD_RESOURCE_SESSION_TRANSPORT"] = cli.resourceSessionTransport
		if agent, ok := cli.baseEnv["SSH_AUTH_SOCK"]; ok {
			values["SSH_AUTH_SOCK"] = agent
		}
	}
	if cli.resourceWire != "" {
		mode = "wire"
		values["SUBYARD_RESOURCE_VERB"] = cli.resourceWire
	}
	if mode != "" {
		values["SUBYARD_RESOURCE_MODE"] = mode
	}
	if localAction != "" {
		values["SUBYARD_RESOURCE_ACTION"] = localAction
	}
	if operationID != "" {
		values["SUBYARD_OPERATION_ID"] = operationID
	}
	return environmentList(values, nil)
}

var resourceSessionEnvironment = []string{
	"HOME", "USER", "LOGNAME", "SHELL", "TERM", "COLORTERM",
	"DISPLAY", "WAYLAND_DISPLAY", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS", "XAUTHORITY",
}

func (cli *CLI) resourceApplyEnvironment(
	loaded config.Loaded,
	definition resource.Definition,
	localAction string,
	operationID string,
	effect domain.ActionEffect,
) []string {
	values := environmentMap(cli.resourceEnvironment(loaded, definition, "apply", localAction, operationID))
	if effect == domain.ActionSession {
		for _, name := range resourceSessionEnvironment {
			if value, ok := cli.baseEnv[name]; ok {
				values[name] = value
			}
		}
	}
	return environmentList(values, nil)
}

func localResourceAction(definition resource.Definition, action domain.ActionID) (string, bool) {
	prefix := "resource." + definition.Profile + "." + definition.Name + "."
	local := strings.TrimPrefix(string(action), prefix)
	return local, local != string(action) && domain.SafeName(local)
}

type resourceApplyRunner struct {
	cli           *CLI
	loaded        config.Loaded
	definition    resource.Definition
	verb          string
	localAction   string
	effect        domain.ActionEffect
	arguments     []string
	bootstrap     *profileBootstrap
	consequences  []string
	ingress       *resourceIngress
	startupIntent *resourceStartupIntent
	exact         *resourceExecution
	verify        *resourceExecution
	skipHandler   bool
}

type resourceSessionExitError struct{ code int }

func (err *resourceSessionExitError) Error() string {
	return fmt.Sprintf("resource session exited with status %d", err.code)
}

func (runner *resourceApplyRunner) Run(
	ctx context.Context,
	request domain.AdapterRequest,
	protectedInput io.Reader,
) (domain.AdapterResult, string, error) {
	result := domain.AdapterResult{Schema: 1, OperationID: request.OperationID, Status: "error"}
	if protectedInput != nil {
		return result, "", errors.New("resource apply does not accept protected input")
	}
	if request.Adapter != "resource" || request.Action != runner.localAction ||
		!slices.Equal(request.Arguments, runner.arguments[1:]) {
		return result, "", errors.New("resource apply request does not match prepared action")
	}
	if err := validateResourceHandler(runner.definition.HandlerPath()); err != nil {
		return result, "", err
	}
	if runner.exact != nil {
		return runner.runExactResource(ctx, request)
	}
	if runner.bootstrap != nil {
		if err := runner.bootstrap.apply(ctx, runner.cli); err != nil {
			return result, "", err
		}
		output, err := runner.cli.prepareResource(ctx, runner.loaded, runner.definition, runner.arguments)
		if err != nil {
			return result, "", err
		}
		assessment, err := runner.cli.resources.AssessPrepareResult(runner.cli.coreActions, runner.definition.Command, runner.verb, output)
		if err != nil {
			return result, "", err
		}
		local, ok := localResourceAction(runner.definition, assessment.Action)
		if !ok || local != runner.localAction {
			return result, "", fmt.Errorf("%w: bootstrap changed resource action", domain.ErrPlanStale)
		}
		for _, consequence := range assessment.Consequences {
			if !slices.Contains(runner.consequences, consequence) {
				return result, "", fmt.Errorf("%w: bootstrap changed resource consequences", domain.ErrPlanStale)
			}
		}
	}
	if !runner.skipHandler {
		command := exec.CommandContext(ctx, runner.definition.HandlerPath(), runner.arguments...)
		configureResourceProcess(command)
		command.Dir = runner.cli.options.WorkingDir
		command.Env = runner.cli.resourceApplyEnvironment(
			runner.loaded, runner.definition, runner.localAction, request.OperationID, runner.effect,
		)
		if runner.verify != nil {
			steps, err := json.Marshal(runner.verify.current.Steps)
			if err != nil {
				return result, "", err
			}
			command.Env = environmentList(environmentMap(command.Env), map[string]string{
				"SUBYARD_RESOURCE_BINDING": runner.verify.current.Binding,
				"SUBYARD_RESOURCE_STEPS":   string(steps),
			})
		}
		var restoreForeground func() error
		// The engine owns confirmation. Non-session handlers must not inherit the
		// operator terminal: their separate process group can otherwise be stopped
		// when a child such as incus exec probes or reads it.
		if runner.effect == domain.ActionSession {
			command.Stdin = runner.cli.options.Stdin
			var err error
			restoreForeground, err = configureResourceSessionForeground(command, command.Stdin)
			if err != nil {
				return result, "", err
			}
		}
		command.Stdout = runner.cli.options.Stdout
		command.Stderr = runner.cli.options.Stderr
		var runErr error
		if restoreForeground != nil {
			runErr = runResourceTerminalSession(command)
		} else {
			runErr = command.Run()
		}
		if restoreForeground != nil {
			if err := restoreForeground(); err != nil {
				return result, "", err
			}
		}
		if runErr != nil {
			if rollbackErr := runner.ingress.rollback(runner, request.OperationID); rollbackErr != nil {
				return result, "", fmt.Errorf("run resource handler: %w; owned ingress rollback failed: %v", runErr, rollbackErr)
			}
			if ctx.Err() != nil {
				return result, "", fmt.Errorf("run resource handler: %w", ctx.Err())
			}
			var exitError *exec.ExitError
			if errors.As(runErr, &exitError) {
				if runner.effect == domain.ActionSession && exitError.ExitCode() >= 0 {
					return result, "", &resourceSessionExitError{code: exitError.ExitCode()}
				}
				return result, "", fmt.Errorf("resource handler exited with status %d", exitError.ExitCode())
			}
			return result, "", fmt.Errorf("run resource handler: %w", runErr)
		}
	}
	if runner.verify != nil {
		if err := runner.verify.verify(ctx); err != nil {
			return result, "", err
		}
	}
	if err := runner.ingress.apply(ctx, runner, request.OperationID); err != nil {
		return result, "", err
	}
	if err := runner.startupIntent.commit(ctx, runner.cli); err != nil {
		if runner.ingress != nil && runner.ingress.up {
			if rollbackErr := runner.ingress.rollback(runner, request.OperationID); rollbackErr != nil {
				return result, "", fmt.Errorf("record startup intent: %w; owned ingress rollback failed: %v", err, rollbackErr)
			}
		}
		return result, "", err
	}
	result.Status = "ok"
	return result, "", nil
}

func validateResourceHandler(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("resource handler is unavailable: %s", path)
	}
	return nil
}

func configureResourceProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	command.WaitDelay = 2 * time.Second
}

type boundedResourceBuffer struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func (buffer *boundedResourceBuffer) Write(value []byte) (int, error) {
	written := len(value)
	remaining := buffer.limit - buffer.buffer.Len()
	if remaining <= 0 {
		buffer.overflow = true
		return written, nil
	}
	if len(value) > remaining {
		buffer.overflow = true
		value = value[:remaining]
	}
	_, _ = buffer.buffer.Write(value)
	return written, nil
}

func (buffer *boundedResourceBuffer) Bytes() []byte  { return buffer.buffer.Bytes() }
func (buffer *boundedResourceBuffer) String() string { return buffer.buffer.String() }

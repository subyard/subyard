package cli

import (
	"bytes"
	"context"
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
)

const resourcePrepareTimeout = 30 * time.Second

func (cli *CLI) runResourceCommand(
	ctx context.Context,
	loaded config.Loaded,
	definition resource.Definition,
	arguments []string,
	globalAssumeYes bool,
) int {
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

	output, err := cli.prepareResource(ctx, loaded, definition, invocation.arguments)
	if err != nil {
		cli.errorf("%s: %v", definition.Command, err)
		return 1
	}
	assessment, err := cli.resources.AssessPrepareResult(
		cli.coreActions, definition.Command, invocation.verb, output,
	)
	if err != nil {
		cli.errorf("%s: %v", definition.Command, err)
		return 1
	}
	localAction, ok := localResourceAction(definition, assessment.Action)
	if !ok {
		cli.errorf("%s: %v", definition.Command, fmt.Errorf(
			"%w: prepared action %q does not belong to the selected resource",
			resource.ErrResourceActionUnknown, assessment.Action,
		))
		return 1
	}

	operationID := cli.env["SUBYARD_OPERATION_ID"]
	orchestrator := cli.operationOrchestrator(operationID, loaded, nil, nil)
	plan, err := orchestrator.PlanAction(
		ctx,
		loaded.Context,
		definition.Command,
		domain.RemoteOnOwner,
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
	if assessment.Changed &&
		(assessment.Effect == domain.ActionMutation || assessment.Effect == domain.ActionDestruction) {
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

	runner := &resourceApplyRunner{
		cli: cli, loaded: loaded, definition: definition, verb: invocation.verb,
		localAction: localAction, effect: assessment.Effect, arguments: slices.Clone(invocation.arguments),
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
	if err := validateResourceHandler(definition.HandlerPath()); err != nil {
		return nil, err
	}
	prepareContext, cancel := context.WithTimeout(ctx, resourcePrepareTimeout)
	defer cancel()
	command := exec.CommandContext(prepareContext, definition.HandlerPath(), arguments...)
	configureResourceProcess(command)
	command.Dir = cli.options.WorkingDir
	command.Env = cli.resourceEnvironment(loaded, definition, "prepare", "", "")
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
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return nil, fmt.Errorf("%w: prepare failed: %v: %s",
				resource.ErrResourcePlanInvalid, err, detail)
		}
		return nil, fmt.Errorf("%w: prepare failed: %v", resource.ErrResourcePlanInvalid, err)
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
	cli         *CLI
	loaded      config.Loaded
	definition  resource.Definition
	verb        string
	localAction string
	effect      domain.ActionEffect
	arguments   []string
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
	command := exec.CommandContext(ctx, runner.definition.HandlerPath(), runner.arguments...)
	configureResourceProcess(command)
	command.Dir = runner.cli.options.WorkingDir
	command.Env = runner.cli.resourceApplyEnvironment(
		runner.loaded, runner.definition, runner.localAction, request.OperationID, runner.effect,
	)
	command.Stdin = runner.cli.options.Stdin
	command.Stdout = runner.cli.options.Stdout
	command.Stderr = runner.cli.options.Stderr
	if err := command.Run(); err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return result, "", fmt.Errorf("resource handler exited with status %d", exitError.ExitCode())
		}
		return result, "", fmt.Errorf("run resource handler: %w", err)
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

package application

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

const projectHooksDispatcher = "/usr/local/libexec/subyard/projects-changed"
const projectHooksUnsuccessful = "exited unsuccessfully"

// RunProjectHooks bounds both the data-plane request and the guest process. Callers keep the
// primary operation's result and display the returned warning; hook output may contain secrets.
func RunProjectHooks(ctx context.Context, yard domain.Context,
	execute func(context.Context, ports.InstanceExecRequest) (ports.InstanceExecResult, error),
	approved ...string,
) error {
	hookContext, cancel := context.WithTimeout(ctx, 130*time.Second)
	defer cancel()
	dev := uint32(yard.DevUID)
	request := ports.InstanceExecRequest{
		Command:     []string{"sh", "-c", `[ -x "$1" ] && exec timeout --kill-after=5s 120s "$1"`, "subyard", projectHooksDispatcher},
		Environment: map[string]string{"HOME": "/home/" + yard.DevUser},
		User:        dev, Group: dev,
	}
	if len(approved) != 0 {
		request.Command[2] += " 3>&1 >/dev/null 2>&1"
		request.Environment["SUBYARD_PROJECT_HOOK_REPORT"] = "1"
	}
	result, err := execute(hookContext, request)
	if err == nil && result.ExitCode == 0 {
		return nil
	}
	failure := &ProjectHookFailure{Yard: yard.YardName, Reason: projectHooksUnsuccessful}
	if len(approved) != 0 {
		for _, line := range strings.Split(string(result.Stdout), "\n") {
			if path, ok := strings.CutPrefix(line, "subyard-project-hook-failed:"); ok && slices.Contains(approved, path) {
				failure.Hooks = append(failure.Hooks, path)
			}
		}
	}
	var guestExit interface{ GuestExitCode() (int, bool) }
	if errors.As(err, &guestExit) {
		if code, ok := guestExit.GuestExitCode(); ok {
			result.ExitCode = code
			err = nil
		}
	}
	switch {
	case errors.Is(err, context.Canceled):
		failure.Reason = "were cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		failure.Reason = "timed out"
	case err != nil:
		failure.Reason = "could not be executed"
	case result.ExitCode == 124 || result.ExitCode == 137:
		failure.Reason = "timed out"
	}
	return failure
}

// ProjectHookFailure carries fixed text, the validated yard and approved hook paths; hook
// output may contain secrets and never enters the error or its diagnostic.
type ProjectHookFailure struct {
	Yard, Reason string
	Hooks        []string
}

func (*ProjectHookFailure) Error() string {
	return "an optional agent project hook failed; run 'yard init' to repair wiring and retry active integrations, or use the integration's sync command"
}

// ActivationDiagnostic reports that the pass failed or its state is undetermined;
// retained state that an integration accepts is shown by that integration's status.
func (failure *ProjectHookFailure) ActivationDiagnostic() (string, string) {
	yard := failure.Yard
	if yard == "" {
		yard = "default"
	}
	var causes []error
	for _, hook := range failure.Hooks {
		causes = append(causes, fmt.Errorf("yard %s: project hook %s failed", yard, hook))
	}
	if combined := domain.CollectRefusals(causes...); combined != nil {
		message := combined.Error()
		if failure.Reason != projectHooksUnsuccessful {
			message += fmt.Sprintf("; yard %s: optional agent project hooks %s", yard, failure.Reason)
		}
		return message, "run yard migrate"
	}
	return fmt.Sprintf("yard %s: optional agent project hooks %s; the hook pass failed or its state is undetermined, see the integration's status", yard, failure.Reason), "run yard migrate"
}

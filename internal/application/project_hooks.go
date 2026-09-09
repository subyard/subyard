package application

import (
	"context"
	"errors"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

const projectHooksDispatcher = "/usr/local/libexec/subyard/projects-changed"

// RunProjectHooks bounds both the data-plane request and the guest process. Callers keep the
// primary operation's result and display the returned warning; hook output may contain secrets.
func RunProjectHooks(ctx context.Context, yard domain.Context,
	execute func(context.Context, ports.InstanceExecRequest) (ports.InstanceExecResult, error),
) error {
	hookContext, cancel := context.WithTimeout(ctx, 130*time.Second)
	defer cancel()
	dev := uint32(yard.DevUID)
	result, err := execute(hookContext, ports.InstanceExecRequest{
		Command:     []string{"sh", "-c", `[ -x "$1" ] && exec timeout --kill-after=5s 120s "$1"`, "subyard", projectHooksDispatcher},
		Environment: map[string]string{"HOME": "/home/" + yard.DevUser},
		User:        dev, Group: dev,
	})
	if err == nil && result.ExitCode == 0 {
		return nil
	}
	return errors.New("an optional agent project hook failed; run 'yard init' to repair wiring and retry active integrations, or use the integration's sync command")
}

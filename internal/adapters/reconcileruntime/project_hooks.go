package reconcileruntime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/ports"
)

func (runtime Runtime) projectHooksConverged(ctx context.Context) (bool, error) {
	dispatcher, err := os.ReadFile(filepath.Join(runtime.RepositoryRoot, "config", "projects-changed.sh"))
	if err != nil {
		return false, fmt.Errorf("read project dispatcher contract: %w", err)
	}
	var hooks []string
	for _, agent := range strings.Fields(runtime.environmentValue("CODING_TOOL_INTEGRATIONS")) {
		hook := runtime.environmentValue("AGENT_" + agent + "_PROJECTS_CHANGED")
		if hook == "" {
			continue
		}
		if strings.ContainsAny(hook, "\r\n") || strings.IndexFunc(hook, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._/-", r))
		}) >= 0 {
			return false, errors.New("invalid agent project hook command")
		}
		hooks = append(hooks, hook)
	}
	return runtime.guestCheck(ctx, []string{"sh", "-eu", "-c", `
dispatcher=$3
hooks=$4
[ -x "$dispatcher" ] && [ -d "$5" ]
[ "$(stat -c '%F|%a|%u:%g' "$dispatcher")" = "regular file|755|$6" ]
[ "$(stat -c '%F|%a|%u:%g' "$hooks")" = "regular file|644|$6" ]
[ "$(sha256sum "$dispatcher" | cut -d ' ' -f 1)" = "$1" ]
[ "$(sha256sum "$hooks" | cut -d ' ' -f 1)" = "$2" ]
`, "subyard", fmt.Sprintf("%x", sha256.Sum256(dispatcher)),
		fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(hooks, "\n")+"\n"))),
		"/usr/local/libexec/subyard/projects-changed", "/etc/subyard/agent-project-hooks",
		"/usr/local/libexec/subyard/projects-changed.d", "0:0"})
}

// Explicit init retries installed hooks once, including when provisioning was already converged.
// Each resource owns its service-state guard; an intentionally stopped yard needs no guest exec.
func (runtime Runtime) ProjectHooksApplicable(ctx context.Context) (bool, error) {
	state, err := runtime.reconcileState(ctx)
	if err != nil {
		return false, errors.New("project hooks unavailable: could not inspect the yard")
	}
	if state.InstanceFound && strings.EqualFold(state.Instance.Status, "stopped") && instanceIntentionallyStopped(state.Instance) {
		return false, nil
	}
	if !state.InstanceFound || !strings.EqualFold(state.Instance.Status, "running") || runtime.Executor == nil {
		return false, errors.New("project hooks unavailable: yard is not running")
	}
	return true, nil
}

func (runtime Runtime) RunProjectHooks(ctx context.Context) error {
	applicable, err := runtime.ProjectHooksApplicable(ctx)
	if err != nil || !applicable {
		return err
	}
	return application.RunProjectHooks(ctx, runtime.Yard, func(ctx context.Context, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
		return runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, request)
	})
}

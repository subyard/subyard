package reconcileruntime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/domain"
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
	if err := runtime.CheckProjectHookPlan(ctx, false); err != nil {
		return err
	}
	// Integration repair may already have run these hooks during this init.
	// Recheck captured inputs, then preserve that successful native result.
	if runtime.HookPlan != nil && runtime.HookPlan.completed {
		return nil
	}
	applicable, err := runtime.ProjectHooksApplicable(ctx)
	if err != nil || !applicable {
		return err
	}
	if runtime.HookPlan != nil && runtime.HookPlan.HasMetadataRepair() && !runtime.HookPlan.metadataCompleted {
		if err := runtime.repairProjectMetadata(ctx); err != nil {
			return err
		}
		if err := runtime.CheckProjectHookPlan(ctx, false); err != nil {
			return err
		}
	}
	var approved []string
	byPath := map[string][]integrationWorkItem{}
	if runtime.HookPlan != nil {
		for _, hook := range runtime.HookPlan.allowedHooks {
			if path := runtime.HookPlan.current.Resolved[hook]; path != "" {
				approved = append(approved, path)
			}
		}
	}
	if runtime.PersistFinishOnly || runtime.HookPlan != nil {
		add := func(path string, item integrationWorkItem) {
			if filepath.IsAbs(path) && filepath.Clean(path) == path {
				approved = append(approved, path)
				byPath[path] = append(byPath[path], item)
			}
		}
		profiles, err := runtime.profileRuntimeDefinitions()
		if err != nil {
			return err
		}
		selected := strings.Fields(runtime.environmentValue("ENVIRONMENT_PROFILES"))
		for _, definition := range profiles {
			if slices.Contains(selected, definition.Name) {
				for _, path := range definition.Runtime.ProjectsChangedHooks {
					add(path, runtime.integrationItem("apply hooks run", definition.Name).atDestination(path))
				}
			}
		}
		declared := map[string][]integrationWorkItem{}
		for _, owner := range strings.Fields(runtime.environmentValue("CODING_TOOL_INTEGRATIONS")) {
			hook := runtime.environmentValue("AGENT_" + owner + "_PROJECTS_CHANGED")
			if safeCommand(hook) {
				item := runtime.integrationItem("apply hooks run", owner).forSource(hook)
				declared[hook] = append(declared[hook], item)
			} else {
				add(hook, runtime.integrationItem("apply hooks run", owner).atDestination(hook))
			}
		}
		if len(declared) != 0 {
			var observation hookObservation
			if runtime.HookPlan != nil {
				observation = runtime.HookPlan.current
			} else {
				observation, _ = runtime.observeProjectHookInputs(ctx)
			}
			for hook, items := range declared {
				path := observation.Resolved[hook]
				if filepath.Base(path) == hook {
					for _, item := range items {
						add(path, item)
					}
				}
			}
		}
	}
	if err := application.RunProjectHooks(ctx, runtime.Yard, func(ctx context.Context, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
		for name, value := range runtime.HookPlan.Environment() {
			request.Environment[name] = value
		}
		return runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, request)
	}, approved...); err != nil {
		var failure *application.ProjectHookFailure
		if errors.As(err, &failure) {
			var findings []error
			for _, path := range failure.Hooks {
				for _, item := range byPath[path] {
					findings = append(findings, item.failure(err, ""))
				}
			}
			if reported := domain.CollectRefusals(findings...); reported != nil {
				return reported
			}
			failure.Hooks = nil // Only the matched declarations supply diagnostic labels.
		}
		return err
	}
	if err := runtime.CheckProjectHookPlan(ctx, false); err != nil {
		return err
	}
	if runtime.HookPlan != nil {
		runtime.HookPlan.completed = true
	}
	return nil
}

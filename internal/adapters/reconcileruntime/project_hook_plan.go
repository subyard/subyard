package reconcileruntime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
)

// ProjectHookPlan binds the guest's dispatcher inputs without exposing private
// metadata or hook payloads in the public operation plan.
type ProjectHookPlan struct {
	approved         hookObservation
	current          hookObservation
	conditional      bool
	allowedHooks     []string
	source           string
	ownerProjects    []domain.ProjectRecord
	resolvedProjects string
	executionWiring  string
	hookBindings     map[string]string
	unavailable      bool
}

type hookObservation struct {
	Projects string            `json:"projects"`
	Wiring   string            `json:"wiring"`
	Hooks    []string          `json:"hooks"`
	Roots    []string          `json:"roots"`
	Facts    map[string]string `json:"facts"`
	Resolved map[string]string `json:"resolved"`
}

func (runtime Runtime) observeProjectHookInputs(ctx context.Context) (hookObservation, error) {
	if runtime.Executor == nil {
		return hookObservation{}, fmt.Errorf("project hook observation requires an executor")
	}
	callContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	source, err := os.ReadFile(filepath.Join(runtime.RepositoryRoot, "config", "projects-changed.sh"))
	if err != nil {
		return hookObservation{}, err
	}
	result, err := runtime.Executor.Exec(callContext, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, ports.InstanceExecRequest{Command: []string{"bash", "-s", "--", "--observe"}, Stdin: source, User: uint32(runtime.Yard.DevUID), Group: uint32(runtime.Yard.DevUID)})
	if err != nil || result.ExitCode != 0 {
		return hookObservation{}, fmt.Errorf("project hook inputs could not be observed")
	}
	if len(result.Stdout) > 64<<10 {
		return hookObservation{}, fmt.Errorf("project hook observation exceeds output limit")
	}
	var observation hookObservation
	decoder := json.NewDecoder(strings.NewReader(string(result.Stdout)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&observation); err != nil || !profileRuntimeDigest.MatchString(observation.Projects) || !profileRuntimeDigest.MatchString(observation.Wiring) {
		return hookObservation{}, fmt.Errorf("invalid project hook observation")
	}
	return observation, nil
}

func (runtime Runtime) PrepareProjectHooks(ctx context.Context, unavailable bool) (*ProjectHookPlan, error) {
	plan := &ProjectHookPlan{conditional: unavailable, hookBindings: map[string]string{}}
	definitions, err := runtime.profileRuntimeDefinitions()
	if err != nil {
		return nil, err
	}
	for _, definition := range definitions {
		plan.allowedHooks = append(plan.allowedHooks, definition.Runtime.ProjectsChangedHooks...)
		binding := "conditional"
		if runtime.RuntimePlan != nil {
			if captured := runtime.RuntimePlan.approved[definition.Runtime.ActivationID]; captured.HookBinding != "" {
				binding = captured.HookBinding
			}
		}
		for _, hook := range definition.Runtime.ProjectsChangedHooks {
			plan.hookBindings[hook] = binding
		}
	}
	for _, agent := range strings.Fields(runtime.environmentValue("CODING_TOOL_INTEGRATIONS")) {
		if hook := runtime.environmentValue("AGENT_" + agent + "_PROJECTS_CHANGED"); hook != "" {
			plan.allowedHooks = append(plan.allowedHooks, hook)
		}
	}
	slices.Sort(plan.allowedHooks)
	plan.allowedHooks = slices.Compact(plan.allowedHooks)
	source, err := os.ReadFile(filepath.Join(runtime.RepositoryRoot, "config", "projects-changed.sh"))
	if err != nil {
		return nil, err
	}
	plan.source = fmt.Sprintf("%x", sha256.Sum256(source))
	state, err := runtime.reconcileState(ctx)
	if err != nil {
		if !unavailable || !runtime.nativeObservationUnavailable(err) {
			return nil, err
		}
		plan.unavailable = true
		plan.approved.Projects = fmt.Sprintf("%x", sha256.Sum256(nil))
		return plan, nil
	}
	if !state.InstanceFound || !strings.EqualFold(state.Instance.Status, "running") {
		if state.InstanceFound && !strings.EqualFold(state.Instance.Status, "stopped") {
			return nil, fmt.Errorf("project hook observation requires a running or stopped yard")
		}
		if state.InstanceFound && instanceIntentionallyStopped(state.Instance) && !unavailable {
			plan.conditional = false
			return plan, nil
		}
		plan.conditional = true
		plan.approved.Projects = fmt.Sprintf("%x", sha256.Sum256(nil))
		return plan, nil
	}
	plan.conditional = false

	plan.approved, err = runtime.observeProjectHookInputs(ctx)
	if err != nil {
		return nil, err
	}
	// Freeze already installed native and unmanaged hooks as well as declared cold slots.
	plan.allowedHooks = append(plan.allowedHooks, plan.approved.Hooks...)
	slices.Sort(plan.allowedHooks)
	plan.allowedHooks = slices.Compact(plan.allowedHooks)
	plan.current = plan.approved
	return plan, nil
}

func (runtime Runtime) CheckProjectHookPlan(ctx context.Context, beforeWrites bool) error {
	plan := runtime.HookPlan
	if plan == nil {
		return nil
	}
	state, err := runtime.reconcileState(ctx)
	if err != nil {
		if beforeWrites && plan.unavailable && runtime.nativeObservationUnavailable(err) {
			return nil
		}
		return err
	}
	if !state.InstanceFound || !strings.EqualFold(state.Instance.Status, "running") {
		if state.InstanceFound && instanceIntentionallyStopped(state.Instance) {
			return nil
		}
		if beforeWrites && plan.conditional {
			return nil
		}
		return fmt.Errorf("project hooks unavailable: yard is not running")
	}

	observation, err := runtime.observeProjectHookInputs(ctx)
	if err != nil {
		return err
	}
	for _, hook := range observation.Hooks {
		if !slices.Contains(plan.allowedHooks, hook) {
			return fmt.Errorf("%w: installed project hook scope expanded", domain.ErrPlanStale)
		}
	}
	for _, hook := range observation.Hooks {
		if fingerprint := observation.Facts[hook]; fingerprint != plan.approved.Facts[hook] {
			if beforeWrites && !plan.conditional {
				return fmt.Errorf("%w: captured project hook source changed", domain.ErrPlanStale)
			}
			owned, err := runtime.verifiedProjectHookSource(ctx, hook)
			if err != nil {
				return err
			}
			if !owned {
				return fmt.Errorf("%w: unmanaged project hook source changed", domain.ErrPlanStale)
			}
		}
	}
	if plan.approved.Projects == "" {
		return fmt.Errorf("%w: project hooks acquired new guest work", domain.ErrPlanStale)
	}
	if !plan.conditional && observation.Projects != plan.approved.Projects || beforeWrites && !plan.conditional && observation.Wiring != plan.approved.Wiring {
		return fmt.Errorf("%w: project hook inputs changed", domain.ErrPlanStale)
	}
	if plan.conditional {
		roots := []string{}
		for _, record := range plan.ownerProjects {
			roots = append(roots, filepath.Dir(record.YardPath))
		}
		slices.Sort(roots)
		roots = slices.Compact(roots)
		if !slices.Equal(roots, observation.Roots) {
			return fmt.Errorf("%w: conditional guest project scope differs from captured owner records", domain.ErrPlanStale)
		}
		if plan.resolvedProjects != "" && observation.Projects != plan.resolvedProjects {
			return fmt.Errorf("%w: conditional project hook inputs changed", domain.ErrPlanStale)
		}
		if !beforeWrites {
			plan.resolvedProjects = observation.Projects
		}
	}
	if !beforeWrites {
		if plan.executionWiring != "" && observation.Wiring != plan.executionWiring {
			return fmt.Errorf("project hook wiring changed during execution")
		}
		plan.executionWiring = observation.Wiring
		source, err := os.ReadFile(filepath.Join(runtime.RepositoryRoot, "config", "projects-changed.sh"))
		if err != nil {
			return err
		}
		if fmt.Sprintf("%x", sha256.Sum256(source)) != plan.source {
			return fmt.Errorf("%w: project dispatcher source changed", domain.ErrPlanStale)
		}
		converged, err := runtime.projectHooksConverged(ctx)
		if err != nil {
			return err
		}
		if !converged {
			return fmt.Errorf("project hook dispatcher did not converge")
		}
	}
	plan.current = observation
	return nil
}

func (plan *ProjectHookPlan) Binding() string {
	if plan == nil {
		return ""
	}
	payload, _ := json.Marshal(struct {
		Approved     hookObservation
		Conditional  bool
		Hooks        []string
		Source       string
		Owners       []domain.ProjectRecord
		HookBindings map[string]string
	}{plan.approved, plan.conditional, plan.allowedHooks, plan.source, plan.ownerProjects, plan.hookBindings})
	return fmt.Sprintf("%x", sha256.Sum256(payload))
}

func (plan *ProjectHookPlan) OperationStep(target string) domain.OperationStep {
	decision := domain.StepApply
	observed := "captured guest project and installed dispatcher inputs"
	if plan.conditional {
		decision = domain.StepConditional
		observed = "guest inputs unavailable"
	} else if plan.approved.Projects == "" {
		decision = domain.StepSkip
		observed = "intentionally stopped"
	}
	ids := []string{}
	for _, record := range plan.ownerProjects {
		ids = append(ids, record.ProjectID)
	}
	for _, root := range plan.approved.Roots {
		if id := filepath.Base(root); domain.SafeProjectName(id) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	target += fmt.Sprintf(":%d captured projects; %d captured hook paths", len(ids), len(plan.allowedHooks))
	return domain.OperationStep{ID: "init.project-hooks", Target: target, Observed: observed, Desired: "successful installed hooks for captured owner project roots", Decision: decision,
		Preconditions: []string{"captured project inventory and hook inputs remain unchanged", "guest hook paths remain inside the captured installed and declared scope"}, Verify: "native dispatcher exits successfully and captured project inputs remain unchanged", Consequence: "retry installed project hooks once for active resources"}
}

func (plan *ProjectHookPlan) CaptureOwnerProjects(records []domain.ProjectRecord) {
	if plan != nil {
		plan.ownerProjects = slices.Clone(records)
	}
}

func (plan *ProjectHookPlan) Environment() map[string]string {
	if plan == nil {
		return nil
	}
	payload, _ := json.Marshal(struct {
		Hooks    []string          `json:"hooks"`
		Projects string            `json:"projects"`
		Roots    []string          `json:"roots"`
		Bindings map[string]string `json:"bindings"`
		Facts    map[string]string `json:"facts"`
		Resolved map[string]string `json:"resolved"`
	}{plan.allowedHooks, plan.current.Projects, plan.current.Roots, plan.hookBindings, plan.current.Facts, plan.current.Resolved})
	return map[string]string{"SUBYARD_PROJECT_HOOK_SCOPE": string(payload)}
}

func (runtime Runtime) verifiedProjectHookSource(ctx context.Context, hook string) (bool, error) {
	if runtime.RuntimePlan != nil {
		plan := runtime.RuntimePlan
		inputs, err := runtime.profileRuntimeInputs(plan.definitions)
		if err != nil {
			return false, err
		}
		if inputs != plan.inputs {
			return false, fmt.Errorf("%w: native hook source inputs changed", domain.ErrPlanStale)
		}
		for _, definition := range plan.definitions {
			if !slices.Contains(definition.Runtime.ProjectsChangedHooks, hook) {
				continue
			}
			observations, err := runtime.observeProfileRuntimes(ctx, []profile.Definition{definition})
			if err != nil {
				return false, err
			}
			observation := observations[definition.Runtime.ActivationID]
			desired := plan.approved[definition.Runtime.ActivationID].Desired
			if resolved, ok := plan.resolved[definition.Runtime.ActivationID]; ok {
				desired = resolved.Desired
			}
			return observation.State == ports.RuntimeStateCurrent && observation.Actual == observation.Desired && (desired == "" || observation.Desired == desired), nil
		}
	}
	for _, agent := range strings.Fields(runtime.environmentValue("CODING_TOOL_INTEGRATIONS")) {
		if runtime.environmentValue("AGENT_"+agent+"_PROJECTS_CHANGED") == hook {
			plan, err := runtime.IntegrationPlan(ctx)
			return err == nil && !plan.Changed, err
		}
	}
	return false, nil
}

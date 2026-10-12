package reconcileruntime

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

//go:embed project_metadata.py
var projectMetadataProgram string

type projectMetadataTarget struct {
	Root    string `json:"root"`
	Payload string `json:"payload"`
}
type projectMetadataObservation struct {
	Expected json.RawMessage `json:"expected"`
	Projects string          `json:"projects"`
}
type projectMetadataPlan struct {
	Targets []projectMetadataTarget `json:"targets"`
	projectMetadataObservation
	ProjectedBindings map[string]string `json:"projectedBindings,omitempty"`
}

// ProjectMetadataError reports only captured owner identity and a controlled reason.
// Guest stderr, metadata bytes and transport details never become diagnostics.
type ProjectMetadataError struct {
	ProjectID string
	Root      string
	Reason    string
	stale     bool
}

func (failure *ProjectMetadataError) Error() string {
	if failure.ProjectID != "" {
		return fmt.Sprintf("project metadata repair refused for project %q at %q: %s", failure.ProjectID, failure.Root, failure.Reason)
	}
	return fmt.Sprintf("project metadata repair refused at %q: %s", failure.Root, failure.Reason)
}

func (failure *ProjectMetadataError) Unwrap() error {
	if failure.stale {
		return domain.ErrPlanStale
	}
	return nil
}

func metadataFailure(plan *projectMetadataPlan, output []byte) error {
	failure := &ProjectMetadataError{Root: "/srv/workspaces", Reason: "io_failure"}
	var envelope struct {
		Error *struct {
			Reason string `json:"reason"`
			Target *int   `json:"target"`
			Stale  bool   `json:"stale"`
		} `json:"error"`
	}
	if len(output) > 4096 || json.Unmarshal(output, &envelope) != nil || envelope.Error == nil || envelope.Error.Target == nil {
		return failure
	}
	detail := envelope.Error
	switch detail.Reason {
	case "missing_root", "missing_source", "unsafe_path", "unsafe_root", "unsafe_source", "unsafe_metadata", "invalid_metadata", "conflicting_metadata", "stale_inputs", "scope_changed", "scope_limit", "verification_failed", "io_failure":
	default:
		return failure
	}
	index := *detail.Target
	if index < -1 || index >= len(plan.Targets) {
		return failure
	}
	failure.Reason, failure.stale = detail.Reason, detail.Stale
	if index >= 0 {
		failure.ProjectID = filepath.Base(plan.Targets[index].Root)
		// Legacy owner IDs have no length limit; keep even invalid long roots bounded.
		if len(failure.ProjectID) > 255 {
			failure.ProjectID = failure.ProjectID[:252] + "..."
		}
		failure.Root = filepath.Join("/srv/workspaces", failure.ProjectID)
	}
	return failure
}

// HasMetadataRepair lets resource bring-up retain a bounded init dependency
// without retaining init's unconditional hook retry for an already-ready yard.
func (plan *ProjectHookPlan) HasMetadataRepair() bool {
	step := plan.MetadataOperationStep("")
	return step != nil && step.Decision != domain.StepSkip
}

func (runtime Runtime) prepareMetadataHookBindings(ctx context.Context) error {
	plan := runtime.HookPlan.metadata
	if runtime.RuntimePlan == nil || !runtime.HookPlan.HasMetadataRepair() {
		return nil
	}
	if err := runtime.CheckProfileRuntimePlan(ctx); err != nil {
		return err
	}
	var entries []struct {
		Metadata json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal(plan.Expected, &entries); err != nil {
		return err
	}
	missing := []projectMetadataTarget{}
	for index, entry := range entries {
		if string(entry.Metadata) == "null" {
			missing = append(missing, plan.Targets[index])
		}
	}
	projection, err := json.Marshal(struct {
		Schema  int                     `json:"schema"`
		Missing []projectMetadataTarget `json:"missingMetadata"`
	}{1, missing})
	if err != nil {
		return err
	}
	projected := runtime
	projected.projectMetadataProjection = projection
	observations, err := projected.observeProfileRuntimes(ctx, runtime.RuntimePlan.definitions)
	if err != nil {
		return err
	}
	plan.ProjectedBindings = map[string]string{}
	for id, observed := range observations {
		current := runtime.RuntimePlan.current[id]
		binding := observed.HookBinding
		observed.HookBinding = current.HookBinding
		if observed != current {
			return fmt.Errorf("%w: metadata projection changed native runtime contract", domain.ErrPlanStale)
		}
		if current.HookBinding != "" {
			plan.ProjectedBindings[id] = binding
		}
	}
	return nil
}

func (runtime Runtime) verifyMetadataHookBindings(ctx context.Context) error {
	metadata := runtime.HookPlan.metadata
	if len(metadata.ProjectedBindings) == 0 {
		return nil
	}
	observations, err := runtime.observeProfileRuntimes(ctx, runtime.RuntimePlan.definitions)
	if err != nil {
		return err
	}
	for _, definition := range runtime.RuntimePlan.definitions {
		id := definition.Runtime.ActivationID
		expected, ok := metadata.ProjectedBindings[id]
		if !ok {
			continue
		}
		observed := observations[id]
		baseline := runtime.RuntimePlan.current[id]
		observed.HookBinding = baseline.HookBinding
		if observations[id].HookBinding != expected || observed != baseline {
			return fmt.Errorf("%w: native metadata repair projection did not match; native projection support and unchanged catalog are required", domain.ErrPlanStale)
		}
		for _, hook := range definition.Runtime.ProjectsChangedHooks {
			runtime.HookPlan.hookBindings[hook] = expected
		}
		approved := runtime.RuntimePlan.approved[id]
		approved.HookBinding = expected
		runtime.RuntimePlan.approved[id] = approved
		if resolved, ok := runtime.RuntimePlan.resolved[id]; ok {
			resolved.HookBinding = expected
			runtime.RuntimePlan.resolved[id] = resolved
		}
	}
	runtime.RuntimePlan.current = observations
	return nil
}

func (plan *ProjectHookPlan) MetadataOperationStep(target string) *domain.OperationStep {
	if plan == nil || plan.metadata == nil {
		return nil
	}
	decision, observed := domain.StepApply, "captured missing owner project metadata"
	if plan.conditional {
		decision, observed = domain.StepConditional, "guest metadata unavailable"
	} else if plan.approved.Projects == "" {
		decision, observed = domain.StepSkip, "intentionally stopped"
	} else {
		var entries []struct {
			Metadata json.RawMessage `json:"metadata"`
		}
		if json.Unmarshal(plan.metadata.Expected, &entries) == nil {
			missing := false
			for _, entry := range entries {
				missing = missing || string(entry.Metadata) == "null"
			}
			if !missing {
				decision, observed = domain.StepSkip, "captured owner metadata already present"
			}
		}
	}
	return &domain.OperationStep{ID: "init.project-metadata", Target: fmt.Sprintf("%s:%d captured owner project roots", target, len(plan.metadata.Targets)), Observed: observed, Desired: "canonical metadata for every captured owner project root", Decision: decision,
		Preconditions: []string{"authoritative owner records and captured guest roots remain unchanged", "canonical roots are owned directories; existing metadata has matching identity"},
		Verify:        "all captured roots contain matching metadata; source and session files remain unchanged", Consequence: "atomically restore missing registered project metadata before installed hooks"}
}

func (runtime Runtime) metadataRequest(ctx context.Context, mode string, plan *projectMetadataPlan) (projectMetadataObservation, error) {
	payload, err := json.Marshal(struct {
		Mode string `json:"mode"`
		*projectMetadataPlan
	}{mode, plan})
	if err != nil {
		return projectMetadataObservation{}, err
	}
	callContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := runtime.Executor.Exec(callContext, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, ports.InstanceExecRequest{
		Command: []string{"python3", "-B", "-c", projectMetadataProgram}, Stdin: payload,
		User: uint32(runtime.Yard.DevUID), Group: uint32(runtime.Yard.DevUID),
	})
	if err != nil {
		return projectMetadataObservation{}, metadataFailure(plan, nil)
	}
	if result.ExitCode != 0 {
		return projectMetadataObservation{}, metadataFailure(plan, result.Stdout)
	}
	var observation projectMetadataObservation
	if len(result.Stdout) > 1<<20 || json.Unmarshal(result.Stdout, &observation) != nil || !profileRuntimeDigest.MatchString(observation.Projects) || len(observation.Expected) == 0 {
		return observation, fmt.Errorf("invalid project metadata observation")
	}
	return observation, nil
}

// PrepareProjectMetadata captures only authoritative owner records. Guest directories
// never authorize new targets. Stopped/absent guests retain bounded conditional targets.
func (runtime Runtime) PrepareProjectMetadata(ctx context.Context) error {
	hooks := runtime.HookPlan
	if hooks == nil || len(hooks.ownerProjects) == 0 {
		return nil
	}
	plan := &projectMetadataPlan{}
	for _, record := range hooks.ownerProjects {
		if err := record.Validate(record.ProjectID); err != nil {
			return fmt.Errorf("project metadata owner record: %w", err)
		}
		payload, err := application.ProjectMetadata(record, runtime.Yard.YardName)
		if err != nil {
			return err
		}
		plan.Targets = append(plan.Targets, projectMetadataTarget{filepath.Dir(record.YardPath), string(payload)})
	}
	hooks.metadata = plan
	if hooks.conditional || hooks.approved.Projects == "" {
		return nil
	}
	observation, err := runtime.metadataRequest(ctx, "inspect", plan)
	if err != nil {
		return err
	}
	if hooks.approved.Projects != "" && observation.Projects != hooks.approved.Projects {
		return fmt.Errorf("%w: project metadata inputs changed during preparation", domain.ErrPlanStale)
	}
	plan.projectMetadataObservation = observation
	return runtime.prepareMetadataHookBindings(ctx)
}

func (runtime Runtime) repairProjectMetadata(ctx context.Context) error {
	hooks := runtime.HookPlan
	if hooks == nil || hooks.metadata == nil || hooks.metadataCompleted {
		return nil
	}
	plan := hooks.metadata
	if len(plan.Expected) == 0 {
		observed, err := runtime.metadataRequest(ctx, "inspect", plan)
		if err != nil {
			return err
		}
		if hooks.current.Projects != "" && observed.Projects != hooks.current.Projects {
			return fmt.Errorf("%w: conditional project metadata inputs changed", domain.ErrPlanStale)
		}
		plan.projectMetadataObservation = observed
	}
	if err := runtime.CheckProfileRuntimePlan(ctx); err != nil {
		return err
	}
	observed, err := runtime.metadataRequest(ctx, "apply", plan)
	if err != nil {
		return err
	}
	// The native publisher checks all unchanged workspace facts before returning the
	// exact resulting dispatcher digest; only authorized publication may rebind it.
	if hooks.conditional {
		hooks.resolvedProjects = observed.Projects
	} else {
		hooks.approved.Projects = observed.Projects
	}
	hooks.current.Projects = observed.Projects
	if err := runtime.verifyMetadataHookBindings(ctx); err != nil {
		return err
	}
	hooks.metadataCompleted = true
	return nil
}

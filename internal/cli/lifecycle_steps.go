package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/sshagentruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/uninstall"
)

type teardownArtifact = ports.TeardownArtifact
type teardownSnapshot = uninstall.TeardownSnapshot

func teardownArtifactBinding(root string) (string, error) {
	artifact, err := uninstall.CaptureArtifact(root)
	return artifact.Binding, err
}

func (cli *CLI) captureTeardownSnapshot(ctx context.Context, loaded config.Loaded, state ports.ReconcileState, paths []string, registered []domain.Context) (*teardownSnapshot, error) {
	incusPort, _ := cli.statusPorts()
	resources, err := incusPort.TeardownInventory(ctx, loaded.Context.IncusProject)
	if err != nil {
		return nil, err
	}
	if resources == nil {
		resources = []ports.TeardownResource{}
	}
	shared, err := incusPort.TeardownSharedInventory(ctx, firstNonempty(loaded.Environment["STORAGE_POOL"], loaded.Environment["SRV_POOL"]), loaded.Context.IncusBridge)
	if err != nil {
		return nil, err
	}
	resources = append(resources, shared...)
	statePayload, _ := json.Marshal(state)
	var capturedState ports.ReconcileState
	_ = json.Unmarshal(statePayload, &capturedState)
	snapshot := &teardownSnapshot{Resources: resources, State: capturedState}
	agent, err := (sshagentruntime.Manager{Config: sshagentruntime.Config{Directory: sshagentruntime.Directory(loaded.Context.Paths.DataHome, loaded.Context.YardName)}}).PrepareLock(ctx)
	if err != nil {
		return nil, err
	}
	snapshot.Agent, snapshot.AgentBinding, snapshot.AgentState = agent, agent.Binding(), agent.State()
	for _, path := range paths {
		artifact, err := uninstall.CaptureArtifact(path)
		if err != nil {
			return nil, err
		}
		snapshot.Artifacts = append(snapshot.Artifacts, artifact)
	}
	if registered != nil {
		snapshot.Registered = append([]domain.Context{}, registered...)
	} else {
		snapshot.Registered, err = cli.powerYardContexts(loaded)
	}
	if err != nil {
		return nil, err
	}
	sort.Slice(snapshot.Registered, func(i, j int) bool { return snapshot.Registered[i].YardName < snapshot.Registered[j].YardName })
	return snapshot, nil
}

func checkTeardownSnapshot(before, after *teardownSnapshot) error {
	if before == nil || after == nil {
		return errors.New("exact teardown snapshot is required")
	}
	if before.Agent != nil {
		if err := before.Agent.Check(after.Agent); err != nil {
			return err
		}
	}
	if operationStateDigest(before.Registered) != operationStateDigest(after.Registered) {
		return fmt.Errorf("%w: registered teardown peers changed", domain.ErrPlanStale)
	}
	approved := make(map[string]string)
	for _, resource := range before.Resources {
		approved[resource.Kind+"/"+resource.Pool+"/"+resource.Name] = resource.Binding
	}
	for _, resource := range after.Resources {
		if approved[resource.Kind+"/"+resource.Pool+"/"+resource.Name] != resource.Binding {
			return fmt.Errorf("%w: teardown resource set or binding changed", domain.ErrPlanStale)
		}
	}
	if len(before.Artifacts) != len(after.Artifacts) {
		return domain.ErrPlanStale
	}
	for index, artifact := range before.Artifacts {
		fresh := after.Artifacts[index]
		if artifact.Path != fresh.Path || (len(artifact.Entries) == 0 && fresh.Binding != "" && fresh.Binding != artifact.Binding) {
			return fmt.Errorf("%w: teardown artifact changed", domain.ErrPlanStale)
		}
		if len(artifact.Entries) > 0 {
			if err := (uninstall.Tree{Path: artifact.Path, Entries: artifact.Entries}).Check(); err != nil {
				return err
			}
		}
	}
	old, new := before.State, after.State
	if (!old.ProjectFound && new.ProjectFound) || (!old.HostNetworkFound && new.HostNetworkFound) || (!old.HostPoolFound && new.HostPoolFound) || (!old.VolumeFound && new.VolumeFound) || (!old.InstanceFound && new.InstanceFound) || (!old.ProfileFound && new.ProfileFound) {
		return fmt.Errorf("%w: teardown scope expanded", domain.ErrPlanStale)
	}
	if old.ProjectFound && new.ProjectFound && operationStateDigest(old.ProjectConfig) != operationStateDigest(new.ProjectConfig) {
		return fmt.Errorf("%w: teardown project ownership changed", domain.ErrPlanStale)
	}
	return nil
}

func (execution *teardownExecution) binding() string {
	return operationStateDigest(struct {
		Physical      *teardownSnapshot
		Configuration *teardownConfigSnapshot
	}{execution.snapshot(), execution.configSnapshot})
}
func (execution *teardownExecution) steps(yard domain.Context) []domain.OperationStep {
	if execution == nil || execution.snapshot() == nil {
		return nil
	}
	var steps []domain.OperationStep
	if execution.revokeAgent && execution.snapshot().AgentState != "" {
		decision := domain.StepApply
		if execution.snapshot().AgentState == "locked" {
			decision = domain.StepSkip
		}
		steps = append(steps, domain.OperationStep{ID: "ssh-signing-grant", Target: "yard/" + yard.YardName + "/ssh-signing-grant", Observed: execution.snapshot().AgentState, Desired: "locked", Decision: decision, Preconditions: []string{"captured worker identity and grant deadline unchanged"}, Verify: "native signing grant is locked", Consequence: "revoke the captured yard SSH signing grant"})
	}

	for index, resource := range execution.snapshot().Resources {
		decision := domain.StepApply
		desired := "absent"
		if resource.Kind == "network" || resource.Kind == "pool" || resource.Kind == "host-profile" {
			decision = domain.StepConditional
			desired = "remove exact unused yard infrastructure; preserve surviving shared consumers"
		}
		if execution.completedReset || execution.keepData && resource.Kind != "instance" {
			decision = domain.StepSkip
			desired = "preserved"
		}
		steps = append(steps, domain.OperationStep{ID: fmt.Sprintf("incus-%d", index), Target: "incus/" + yard.IncusProject + "/" + resource.Kind + "/" + resource.Pool + "/" + resource.Name, Observed: "present", Desired: desired, Decision: decision, Preconditions: []string{"captured resource identity and metadata remain unchanged"}, Verify: desired + " exact Incus resource", Consequence: desired + " " + resource.Kind + " " + resource.Name})
	}
	for index, artifact := range execution.snapshot().Artifacts {
		decision := domain.StepApply
		if artifact.Binding == "" {
			decision = domain.StepSkip
		}
		steps = append(steps, domain.OperationStep{ID: fmt.Sprintf("artifact-%d", index), Target: "yard-artifact/" + artifact.Path, Observed: map[bool]string{true: "present", false: "absent"}[artifact.Binding != ""], Desired: "absent", Decision: decision, Preconditions: []string{"captured artifact metadata remains unchanged"}, Verify: "selected-yard artifact absent", Consequence: "remove the captured selected-yard artifact"})
	}
	if execution.resetConfig && execution.configSnapshot != nil {
		for index, artifact := range execution.configSnapshot.Artifacts {
			decision := domain.StepApply
			if artifact.Binding == "" {
				decision = domain.StepSkip
			}
			steps = append(steps, domain.OperationStep{ID: fmt.Sprintf("local-settings-%d", index), Target: "yard-local-settings/" + artifact.Path, Observed: map[bool]string{true: "present", false: "absent"}[artifact.Binding != ""], Desired: "absent", Decision: decision, Preconditions: []string{"captured local settings metadata and namespace unchanged", "physical teardown verified"}, Verify: "selected local settings absent", Consequence: "remove captured selected-yard local settings and overrides"})
		}
		decision := domain.StepApply
		if execution.configSnapshot.Reset {
			decision = domain.StepSkip
		}
		steps = append(steps, domain.OperationStep{ID: "yard-fallback-ownership", Target: "yard/" + yard.YardName + "/fallback-ownership", Observed: map[bool]string{true: "local", false: "inherited"}[execution.configSnapshot.Reset], Desired: "local", Decision: decision, Preconditions: []string{"captured ownership marker unchanged", "physical teardown verified"}, Verify: "previous nonlocal yard fallback suppressed", Consequence: "preserve shared sources and prevent old yard settings reactivation"})
	}
	return steps
}

func (cli *CLI) verifyTeardown(ctx context.Context, loaded config.Loaded, execution *teardownExecution) error {
	if execution.completedReset {
		return nil
	}
	if execution.revokeAgent {
		status, err := (sshagentruntime.Manager{Config: sshagentruntime.Config{Directory: sshagentruntime.Directory(loaded.Context.Paths.DataHome, loaded.Context.YardName)}}).Status(ctx)
		if err != nil {
			return err
		}
		if status.State != "locked" {
			return errors.New("SSH signing grant remains after teardown")
		}
	}
	incusPort, _ := cli.statusPorts()
	resources, err := incusPort.TeardownInventory(ctx, loaded.Context.IncusProject)
	if err != nil {
		return err
	}
	shared, err := incusPort.TeardownSharedInventory(ctx, firstNonempty(loaded.Environment["STORAGE_POOL"], loaded.Environment["SRV_POOL"]), loaded.Context.IncusBridge)
	if err != nil {
		return err
	}
	resources = append(resources, shared...)
	remaining := make(map[string]ports.TeardownResource)
	for _, resource := range resources {
		remaining[resource.Kind+"/"+resource.Pool+"/"+resource.Name] = resource
	}
	survivors, err := incusPort.ListInstances(ctx)
	if err != nil {
		return err
	}
	retainShared := execution.holdShared || hasOtherRegisteredLocalYard(loaded.Context.YardName, execution.snapshot().Registered) || len(survivors) != 0
	for _, resource := range shared {
		for _, consumer := range resource.UsedBy {
			if !strings.HasPrefix(consumer, "/1.0/profiles/default") {
				retainShared = true
			}
		}
	}
	for _, original := range execution.snapshot().Resources {
		current, exists := remaining[original.Kind+"/"+original.Pool+"/"+original.Name]
		preserve := execution.keepData && original.Kind != "instance"
		if original.Kind == "host-profile" && exists && current.Binding != original.Binding && !slices.Contains(original.Bindings, current.Binding) {
			return errors.New("shared host profile changed outside approved device removal")
		}
		if (original.Kind == "network" || original.Kind == "pool") && !execution.keepData && exists && !retainShared {
			return errors.New("unused shared teardown infrastructure remains after apply")
		}
		if preserve {
			if !exists || current.Binding != original.Binding {
				return errors.New("teardown did not preserve an approved retained resource")
			}
		} else if exists && original.Kind != "network" && original.Kind != "pool" && original.Kind != "host-profile" {
			return fmt.Errorf("teardown did not remove an approved resource: kind=%s name=%s pool=%s", original.Kind, original.Name, original.Pool)
		}
	}
	for _, artifact := range execution.snapshot().Artifacts {
		binding, err := teardownArtifactBinding(artifact.Path)
		if err != nil {
			return err
		}
		if binding != "" {
			return errors.New("teardown artifact remains after apply")
		}
	}
	state, err := incusPort.ReconcileState(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName, loaded.Environment["SRV_POOL"], loaded.Environment["SRV_VOLUME"], loaded.Context.IncusBridge)
	if err != nil {
		return err
	}
	if state.InstanceFound || (!execution.keepData && state.ProjectFound) {
		return errors.New("teardown physical postcondition did not converge")
	}
	if execution.keepData && ((execution.snapshot().State.HostPoolFound && !state.HostPoolFound) || (execution.snapshot().State.HostNetworkFound && !state.HostNetworkFound) || (execution.snapshot().State.VolumeFound && !state.VolumeFound)) {
		return errors.New("teardown did not retain approved data infrastructure")
	}
	return nil
}

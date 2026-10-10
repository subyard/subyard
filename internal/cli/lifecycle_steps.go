package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"syscall"

	"github.com/Subyard/Subyard/internal/adapters/sshagentruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

type teardownArtifact = ports.TeardownArtifact
type teardownSnapshot struct {
	Resources    []ports.TeardownResource
	Artifacts    []teardownArtifact
	Registered   []domain.Context
	State        ports.ReconcileState
	Agent        *sshagentruntime.PreparedLock `json:"-"`
	AgentBinding string
}

// Only filesystem metadata is inspected; protected configuration is never read.
func teardownArtifactBinding(root string) (string, error) {
	type artifactEntry struct {
		Path     string
		Mode     uint32
		Size     int64
		Modified int64
		Device   uint64
		Inode    uint64
		UID      uint32
		GID      uint32
		Changed  int64
		Link     string
	}
	var entries []artifactEntry
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if path == root && errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if len(entries) >= 4096 {
			return errors.New("teardown artifact exceeds bounded metadata inventory")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return errors.New("native teardown artifact identity is unavailable")
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			link, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		entries = append(entries, artifactEntry{relative, stat.Mode, info.Size(), info.ModTime().UnixNano(),
			uint64(stat.Dev), stat.Ino, stat.Uid, stat.Gid, stat.Ctim.Nano(), link})
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "", nil
	}
	return operationStateDigest(entries), nil
}

func (cli *CLI) captureTeardownSnapshot(ctx context.Context, loaded config.Loaded, state ports.ReconcileState, paths []string) (*teardownSnapshot, error) {
	incusPort, _ := cli.statusPorts()
	reader, ok := incusPort.(ports.TeardownInventoryReader)
	if !ok {
		return nil, errors.New("exact teardown inventory adapter is required")
	}
	resources, err := reader.TeardownInventory(ctx, loaded.Context.IncusProject)
	if err != nil {
		return nil, err
	}
	if resources == nil {
		resources = []ports.TeardownResource{}
	}
	sharedReader, ok := incusPort.(ports.TeardownSharedInventoryReader)
	if !ok {
		return nil, errors.New("exact shared teardown inventory adapter is required")
	}
	shared, err := sharedReader.TeardownSharedInventory(ctx, firstNonempty(loaded.Environment["STORAGE_POOL"], loaded.Environment["SRV_POOL"]), loaded.Context.IncusBridge)
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
	snapshot.Agent, snapshot.AgentBinding = agent, agent.Binding()
	for _, path := range paths {
		binding, err := teardownArtifactBinding(path)
		if err != nil {
			return nil, err
		}
		snapshot.Artifacts = append(snapshot.Artifacts, teardownArtifact{Path: path, Binding: binding})
	}
	snapshot.Registered, err = cli.powerYardContexts(loaded)
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
		if artifact.Path != fresh.Path || (fresh.Binding != "" && fresh.Binding != artifact.Binding) {
			return fmt.Errorf("%w: teardown artifact changed", domain.ErrPlanStale)
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
	}{execution.snapshot, execution.configSnapshot})
}
func (execution *teardownExecution) steps(yard domain.Context) []domain.OperationStep {
	if execution == nil || execution.snapshot == nil {
		return nil
	}
	var steps []domain.OperationStep
	if execution.revokeAgent && execution.snapshot.Agent != nil {
		decision := domain.StepApply
		if execution.snapshot.Agent.State() == "locked" {
			decision = domain.StepSkip
		}
		steps = append(steps, domain.OperationStep{ID: "ssh-signing-grant", Target: "yard/" + yard.YardName + "/ssh-signing-grant", Observed: execution.snapshot.Agent.State(), Desired: "locked", Decision: decision, Preconditions: []string{"captured worker identity and grant deadline unchanged"}, Verify: "native signing grant is locked", Consequence: "revoke the captured yard SSH signing grant"})
	}
	for index, resource := range execution.snapshot.Resources {
		decision := domain.StepApply
		desired := "absent"
		if resource.Kind == "network" || resource.Kind == "pool" {
			decision = domain.StepConditional
			desired = "absent only if no surviving instance or registered local yard uses shared infrastructure"
		}
		if execution.completedReset || execution.keepData && resource.Kind != "instance" {
			decision = domain.StepSkip
			desired = "preserved"
		}
		steps = append(steps, domain.OperationStep{ID: fmt.Sprintf("incus-%d", index), Target: "incus/" + yard.IncusProject + "/" + resource.Kind + "/" + resource.Pool + "/" + resource.Name, Observed: "present", Desired: desired, Decision: decision, Preconditions: []string{"captured resource identity and metadata remain unchanged"}, Verify: desired + " exact Incus resource", Consequence: desired + " " + resource.Kind + " " + resource.Name})
	}
	for index, artifact := range execution.snapshot.Artifacts {
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
	reader, ok := incusPort.(ports.TeardownInventoryReader)
	if !ok {
		return errors.New("teardown verification inventory adapter is required")
	}
	resources, err := reader.TeardownInventory(ctx, loaded.Context.IncusProject)
	if err != nil {
		return err
	}
	sharedReader, ok := incusPort.(ports.TeardownSharedInventoryReader)
	if !ok {
		return errors.New("shared teardown verification adapter is required")
	}
	shared, err := sharedReader.TeardownSharedInventory(ctx, firstNonempty(loaded.Environment["STORAGE_POOL"], loaded.Environment["SRV_POOL"]), loaded.Context.IncusBridge)
	if err != nil {
		return err
	}
	resources = append(resources, shared...)
	remaining := make(map[string]ports.TeardownResource)
	for _, resource := range resources {
		remaining[resource.Kind+"/"+resource.Pool+"/"+resource.Name] = resource
	}
	inventory, ok := incusPort.(ports.InstanceInventory)
	if !ok {
		return errors.New("shared teardown retention verification requires instance inventory")
	}
	survivors, err := inventory.ListInstances(ctx)
	if err != nil {
		return err
	}
	retainShared := hasOtherRegisteredLocalYard(loaded.Context.YardName, execution.snapshot.Registered) || len(survivors) != 0
	for _, original := range execution.snapshot.Resources {
		current, exists := remaining[original.Kind+"/"+original.Pool+"/"+original.Name]
		preserve := execution.keepData && original.Kind != "instance"
		if (original.Kind == "network" || original.Kind == "pool") && !execution.keepData && exists && !retainShared {
			return errors.New("unused shared teardown infrastructure remains after apply")
		}
		if preserve {
			if !exists || current.Binding != original.Binding {
				return errors.New("teardown did not preserve an approved retained resource")
			}
		} else if exists && original.Kind != "network" && original.Kind != "pool" {
			return fmt.Errorf("teardown did not remove an approved resource: kind=%s name=%s pool=%s", original.Kind, original.Name, original.Pool)
		}
	}
	for _, artifact := range execution.snapshot.Artifacts {
		binding, err := teardownArtifactBinding(artifact.Path)
		if err != nil {
			return err
		}
		if binding != "" {
			return errors.New("teardown artifact remains after apply")
		}
	}
	state, err := incusPort.ReconcileState(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName, execution.pool, execution.volume, loaded.Context.IncusBridge)
	if err != nil {
		return err
	}
	if state.InstanceFound || (!execution.keepData && state.ProjectFound) {
		return errors.New("teardown physical postcondition did not converge")
	}
	if execution.keepData && ((execution.snapshot.State.HostPoolFound && !state.HostPoolFound) || (execution.snapshot.State.HostNetworkFound && !state.HostNetworkFound) || (execution.snapshot.State.VolumeFound && !state.VolumeFound)) {
		return errors.New("teardown did not retain approved data infrastructure")
	}
	return nil
}

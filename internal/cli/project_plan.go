package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

// A prepared archive owns a private controller draft, never a target workspace.
// Replaying this artifact binds sync to the bytes inspected before confirmation.
type preparedProjectArchive struct {
	directory string
	path      string
	source    string
	digest    string
	size      int64
}

func (archive *preparedProjectArchive) Open(ctx context.Context, source string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if source != archive.source {
		return nil, fmt.Errorf("%w: project archive source changed", domain.ErrPlanStale)
	}
	root, err := os.Lstat(archive.directory)
	if err != nil || !root.IsDir() || root.Mode().Perm() != 0o700 {
		return nil, fmt.Errorf("%w: prepared archive private root changed", domain.ErrPlanStale)
	}
	info, err := os.Lstat(archive.path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("%w: prepared project archive permissions or identity changed", domain.ErrPlanStale)
	}
	file, err := os.Open(archive.path)
	if err != nil {
		return nil, err
	}
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil || size != archive.size || hex.EncodeToString(hash.Sum(nil)) != archive.digest {
		_ = file.Close()
		return nil, fmt.Errorf("%w: prepared project archive changed", domain.ErrPlanStale)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func (execution *projectExecution) prepareSource(ctx context.Context, cli *CLI) error {
	if execution == nil {
		return nil
	}
	switch execution.Record.Mode {
	case domain.ProjectSync:
		if execution.preparedArchive != nil {
			return execution.checkPreparedSource(ctx, cli)
		}
		input, err := cli.projectArchiver().Open(ctx, execution.Record.HostPath)
		if err != nil {
			return fmt.Errorf("prepare project archive: %w", err)
		}
		directory, err := os.MkdirTemp("", "subyard-project-plan-")
		if err != nil {
			return errors.Join(err, input.Close())
		}
		archive := &preparedProjectArchive{directory: directory, path: filepath.Join(directory, "source.tar"), source: execution.Record.HostPath}
		cleanup := func() { _ = os.Remove(archive.path); _ = os.Remove(directory) }
		if err := os.Chmod(directory, 0o700); err != nil {
			cleanup()
			return errors.Join(err, input.Close())
		}
		file, err := os.OpenFile(archive.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			cleanup()
			return errors.Join(err, input.Close())
		}
		if err := file.Chmod(0o600); err != nil {
			cleanup()
			return errors.Join(err, file.Close(), input.Close())
		}
		hash := sha256.New()
		archive.size, err = io.Copy(io.MultiWriter(file, hash), input)
		err = errors.Join(err, input.Close(), file.Close())
		if err != nil {
			cleanup()
			return fmt.Errorf("retain prepared project archive: %w", err)
		}
		archive.digest = hex.EncodeToString(hash.Sum(nil))
		execution.preparedArchive = archive
		execution.sourceDigest, execution.sourceSize = archive.digest, archive.size
	case domain.ProjectGit:
		if execution.cloneRevision != "" || execution.cloneUnborn {
			return execution.checkPreparedSource(ctx, cli)
		}
		result, err := cli.projectDataPlane().Execute(ctx, execution.Loaded.Context, ports.InstanceExecRequest{
			Command:     []string{"git", "ls-remote", "--", execution.Record.HostPath, "HEAD"},
			Environment: map[string]string{"HOME": "/home/" + execution.Loaded.Context.DevUser},
			User:        uint32(execution.Loaded.Context.DevUID), Group: uint32(execution.Loaded.Context.DevUID),
		})
		if err != nil || result.ExitCode != 0 {
			return fmt.Errorf("inspect clone source revision: %w", errors.Join(err, errors.New("git source probe failed")))
		}
		fields := strings.Fields(string(result.Stdout))
		if len(result.Stdout) == 0 {
			if err := application.CheckEmptyCloneSource(ctx, cli.projectDataPlane(), execution.Loaded.Context, execution.Record.HostPath); err != nil {
				return fmt.Errorf("inspect empty clone source: %w", err)
			}
			execution.cloneUnborn = true
			execution.sourceIdentity = execution.Record.HostPath
			return nil
		}
		if len(fields) != 2 || fields[1] != "HEAD" || !projectGitRevision(fields[0]) {
			return errors.New("clone source did not return one valid HEAD revision")
		}
		execution.cloneRevision = fields[0]
		execution.sourceIdentity = execution.Record.HostPath
	}
	return nil
}

func projectGitRevision(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			if char < 'a' || char > 'f' {
				return false
			}
		}
	}
	return true
}

func (execution *projectExecution) checkPreparedSource(ctx context.Context, cli *CLI) error {
	if execution == nil {
		return nil
	}
	if execution.controllerExport != nil {
		if err := execution.controllerExport.store.Check(); err != nil {
			return err
		}
	}
	if execution.bindSourceBefore != nil {
		current, err := os.Lstat(execution.Record.HostPath)
		if err != nil || !current.IsDir() || !os.SameFile(execution.bindSourceBefore, current) || current.Mode().Perm() != execution.bindSourceBefore.Mode().Perm() {
			return fmt.Errorf("%w: prepared bind source identity or permissions changed", domain.ErrPlanStale)
		}
	}
	if (execution.cloneRevision != "" || execution.cloneUnborn) && execution.Record.HostPath != execution.sourceIdentity {
		return fmt.Errorf("%w: prepared clone source changed", domain.ErrPlanStale)
	}
	if execution.cloneUnborn {
		if err := application.CheckEmptyCloneSource(ctx, cli.projectDataPlane(), execution.Loaded.Context, execution.Record.HostPath); err != nil {
			return err
		}
	}
	if execution.preparedArchive == nil {
		return nil
	}
	file, err := execution.preparedArchive.Open(ctx, execution.Record.HostPath)
	if err != nil {
		return err
	}
	return file.Close()
}

func (execution *projectExecution) closePreparedSource() error {
	if execution == nil || execution.preparedArchive == nil {
		return nil
	}
	archive := execution.preparedArchive
	execution.preparedArchive = nil
	fileErr := os.Remove(archive.path)
	if errors.Is(fileErr, os.ErrNotExist) {
		fileErr = nil
	}
	dirErr := os.Remove(archive.directory)
	if errors.Is(dirErr, os.ErrNotExist) {
		dirErr = nil
	}
	return errors.Join(fileErr, dirErr)
}

func (execution *projectExecution) operationSteps(commandName string) []domain.OperationStep {
	if execution == nil {
		return nil
	}
	if commandName != "sync" && commandName != "clone" {
		return execution.otherOperationSteps(commandName)
	}
	target := execution.Loaded.Context.IncusProject + "/" + execution.Loaded.Context.YardInstanceName + ":" + execution.Record.YardPath
	allocation := "exact name " + execution.RequestedName
	if execution.automaticCopy() {
		// Automatic suffix is an approved allocation rule, not a fixed preview ID.
		target = execution.Loaded.Context.IncusProject + "/" + execution.Loaded.Context.YardInstanceName + ":/srv/workspaces/new copy"
		allocation = "one fresh safe name using " + execution.RequestedName + " and its automatic suffix"
	}
	payload := "revision " + execution.cloneRevision
	verification := "checkout HEAD equals prepared revision before owner finalization"
	if execution.cloneUnborn {
		payload = "empty repository with unborn HEAD and no refs"
		verification = "valid unborn repository has no refs or working files before owner finalization"
	}
	if commandName == "sync" {
		payload = fmt.Sprintf("archive sha256 %s bytes %d", execution.sourceDigest, execution.sourceSize)
		verification = "extracted payload compares equal to retained archive before owner finalization"
	}
	return []domain.OperationStep{{
		ID: "project.copy", Target: target, Observed: "new independent copy; " + allocation,
		Desired: payload, Decision: domain.StepApply,
		Preconditions: []string{"unchanged owner identity and project role", "exclusive fresh workspace admission", allocation},
		Verify:        verification, Consequence: "create one independent project copy with the prepared source",
	}, {
		ID: "project.copy.commit", Target: target, Observed: "project admission not finalized", Desired: "verified project metadata registered on owner",
		Decision: domain.StepConditional, DependsOn: []string{"project.copy"},
		Preconditions: []string{"physical verification succeeded", allocation}, Verify: "owner finalization succeeds and controller inventory is invalidated",
		Consequence: "register the verified project copy",
	}}
}

// sourceBinding binds private source/admission facts without exposing host input.
func (execution *projectExecution) sourceBinding() string {
	if execution == nil {
		return ""
	}
	return operationStateDigest(struct {
		Source                                                    string
		Digest                                                    string
		Size                                                      int64
		Revision                                                  string
		Unborn                                                    bool
		RequestedName                                             string
		ExplicitName                                              bool
		Record                                                    domain.ProjectRecord
		Before                                                    *domain.ProjectRecord
		Profile                                                   application.ProjectEnvironmentProfile
		Environment                                               map[string]string
		Secret                                                    string
		SecretIdentity                                            *ownerInputFile
		HostLinks                                                 []string
		Removal                                                   *projectRemovalObservation
		ApprovedEnvironment, ExportSource, ExportObserved, Inputs string
		ExportDestination                                         string
	}{execution.Record.HostPath, execution.sourceDigest, execution.sourceSize, execution.cloneRevision, execution.cloneUnborn, execution.RequestedName, execution.ExplicitName,
		execution.Record, execution.recordBefore, execution.Profile, execution.Environment, execution.SecretPath, execution.secretIdentity, execution.HostLinks, execution.approvedRemoval,
		execution.approvedEnvironment, execution.exportSourceTree, execution.exportObservedTree, execution.inputBaseline.binding(), execution.exportDestinationBinding()})
}

func (execution *projectExecution) otherOperationSteps(commandName string) []domain.OperationStep {
	target := execution.Loaded.Context.IncusProject + "/" + execution.Loaded.Context.YardInstanceName + ":" + execution.Record.YardPath
	decision := domain.StepApply
	if !execution.ActionChanged {
		decision = domain.StepSkip
	}
	step := domain.OperationStep{ID: "project." + commandName, Target: target, Observed: "prepared project identity", Desired: "prepared project state", Decision: decision, Preconditions: []string{"unchanged owner identity, project record and role"}, Verify: "native postcondition is observed", Consequence: "apply the prepared project action"}
	switch commandName {
	case "export":
		return execution.exportOperationSteps()
	case "bind":
		deviceDecision, metadataDecision := domain.StepApply, domain.StepApply
		deviceObserved, metadataObserved := "missing", "different or missing"
		if execution.bindDeviceConverged {
			deviceDecision, deviceObserved = domain.StepSkip, "exact desired device"
		}
		if execution.bindMetadataConverged {
			metadataDecision, metadataObserved = domain.StepSkip, "exact desired metadata"
		}
		metadata, _ := application.ProjectMetadata(execution.Record, execution.Loaded.Context.YardName)
		return []domain.OperationStep{{ID: "project.bind.device", Target: target, Observed: deviceObserved, Desired: "bind source " + execution.Record.HostPath + " with shift enabled", Decision: deviceDecision, Preconditions: []string{"unchanged source directory identity and permissions", "exact source, target and shift device guard"}, Verify: "exact device source/path/shift is observed", Consequence: "attach the prepared host workspace bind device"},
			{ID: "project.bind.metadata", Target: target, Observed: metadataObserved, Desired: "metadata " + operationStateDigest(string(metadata)), Decision: metadataDecision, Preconditions: []string{"unchanged project identity"}, DependsOn: []string{"project.bind.device"}, Verify: "exact project metadata is read back", Consequence: "register prepared bind project metadata"}}
	case "up", "down":
		step.Target = execution.Loaded.Context.IncusProject + "/" + execution.Loaded.Context.YardInstanceName + ":project environment " + execution.Record.ProjectID
		step.Observed = execution.environmentObserved
		if power, _, structured := strings.Cut(step.Observed, "\t"); structured {
			step.Observed = power + " native identity sha256:" + operationStateDigest(execution.environmentObserved)
		}
		if step.Observed == "" {
			step.Observed = "owner preparation required"
		}
		manifest, _ := application.ProjectEnvironmentManifest(execution.Record, execution.Profile, execution.SecretPath != "")
		step.Desired = "running with manifest " + operationStateDigest(string(manifest))
		if commandName == "down" {
			step.Desired = "stopped owned environment"
		}
		step.Verify = "owned container identity, requested power and manifest are observed"
		step.Consequence = "apply the prepared project environment power and profile"
	case "remove":
		steps := make([]domain.OperationStep, 0, 4)
		observation := execution.Removal
		for _, item := range []struct {
			id               string
			checked, present bool
			suffix           string
		}{
			{"environment", observation.EnvironmentChecked, observation.EnvironmentPresent, "owned project environment"},
			{"environment-files", observation.StagedEnvironmentChecked, observation.StagedEnvironmentPresent, "project environment files"},
			{"device", observation.DeviceChecked, observation.DevicePresent, "owned project bind device"},
			{"workspace", observation.WorkspaceChecked, observation.WorkspacePresent, "owned workspace"},
		} {
			if !item.checked {
				continue
			}
			current := domain.StepSkip
			observed := "absent"
			if item.present {
				current = domain.StepApply
				observed = "present"
			}
			consequence := "remove only the prepared " + item.suffix
			if item.id == "workspace" {
				consequence = "delete the project workspace at its approved path"
			}
			if item.id == "device" {
				consequence += " without deleting the host directory"
			}
			steps = append(steps, domain.OperationStep{ID: "project.remove." + item.id, Target: target + ":" + item.suffix, Observed: observed, Desired: "absent", Decision: current, Preconditions: []string{"unchanged project ownership and scope"}, Verify: "target absence is observed", Consequence: consequence})
		}
		stateConsequence := "remove project registration after verified cleanup"
		if execution.Environment["SUBYARD_PROJECT_REMOVE_SOFT"] == "1" {
			stateConsequence += " and keep the project workspace"
		}
		steps = append(steps, domain.OperationStep{ID: "project.remove.state", Target: target, Observed: "registered project identity", Desired: "project registration removed after physical verification", Decision: domain.StepApply, Preconditions: []string{"unchanged project identity", "prepared physical cleanup verified"}, Verify: "owner registration removal succeeds", Consequence: stateConsequence})
		return steps
	default:
		return nil
	}
	return []domain.OperationStep{step}
}

func (execution *projectExecution) checkProjectRecord(ctx context.Context, cli *CLI) error {
	if execution == nil || execution.recordBefore == nil {
		return nil
	}
	var current domain.ProjectRecord
	var err error
	if execution.Loaded.Context.AccessKind == domain.AccessRemote {
		match, resolveErr := cli.resolveOwnerProject(ctx, execution.Loaded, execution.Record.ProjectID, true, true, true)
		err, current = resolveErr, match.Record
	} else {
		current, err = execution.Store.GetReadOnly(ctx, execution.Record.ProjectID)
	}
	if err != nil {
		return fmt.Errorf("%w: prepared project registration cannot be read: %v", domain.ErrPlanStale, err)
	}
	if current != *execution.recordBefore {
		return fmt.Errorf("%w: prepared project record changed", domain.ErrPlanStale)
	}
	if execution.recordApproved != nil && execution.Record != *execution.recordApproved {
		return fmt.Errorf("%w: prepared project execution identity changed", domain.ErrPlanStale)
	}
	return nil
}

func (execution *projectExecution) removalApproval() *application.ProjectRemovalApproval {
	if execution == nil || execution.approvedRemoval == nil {
		return nil
	}
	before := *execution.approvedRemoval
	return &application.ProjectRemovalApproval{WorkspaceChecked: before.WorkspaceChecked, WorkspacePresent: before.WorkspacePresent, EnvironmentChecked: before.EnvironmentChecked, EnvironmentPresent: before.EnvironmentPresent, DeviceChecked: before.DeviceChecked, DevicePresent: before.DevicePresent, EnvironmentID: before.EnvironmentID, StagedEnvironmentChecked: before.StagedEnvironmentChecked, StagedEnvironmentPresent: before.StagedEnvironmentPresent}
}

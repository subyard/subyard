package application

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/state"
)

type ProjectRemovalObservation struct {
	WorkspaceChecked         bool
	WorkspacePresent         bool
	EnvironmentChecked       bool
	EnvironmentPresent       bool
	EnvironmentID            string
	StagedEnvironmentChecked bool
	StagedEnvironmentPresent bool
	DeviceChecked            bool
	DevicePresent            bool
}

func PlanProjectRemoval(record domain.ProjectRecord, soft bool) (domain.ActionID, domain.ActionDelta, error) {
	if err := record.Validate(record.ProjectID); err != nil {
		return "", domain.ActionDelta{}, fmt.Errorf("validate project removal record: %w", err)
	}
	action := domain.ActionID("project.remove-workspace")
	if record.Mode == domain.ProjectBind {
		action = "project.bind-detach"
	} else if soft {
		action = "project.remove-soft"
	}
	consequences := ProjectConsequences("remove", record, soft)
	if record.Target != "" && record.Target != "yard" {
		consequences = append(consequences, "remove the project environment and staged environment data")
	}
	consequences = append(consequences, "delete project state only after physical cleanup succeeds")
	return action, domain.ActionDelta{Changed: true, Consequences: consequences}, nil
}

func PlanProjectAction(commandName string, record domain.ProjectRecord, changed, rebuild, automaticCopy bool, requestedName string) (domain.ActionID, domain.ActionDelta, error) {
	var action domain.ActionID
	switch commandName {
	case "sync":
		action = "project.sync"
	case "bind":
		action = "project.bind"
	case "clone":
		action = "project.clone"
	case "export":
		action = "project.export-patch"
	case "up":
		action = "project.environment.up"
		if rebuild {
			action = "project.environment.rebuild"
		}
	case "down":
		action = "project.environment.down"
	default:
		return "", domain.ActionDelta{}, fmt.Errorf("command %q has no project action assessment", commandName)
	}
	delta := domain.ActionDelta{Changed: changed}
	if !delta.Changed {
		return action, delta, nil
	}
	delta.Consequences = ProjectConsequences(commandName, record, false)
	if automaticCopy && (commandName == "sync" || commandName == "clone") {
		delta.Consequences = []string{fmt.Sprintf(
			"%s %s to a new yard-owned copy with an available name based on %s",
			commandName, record.HostPath, requestedName,
		)}
	}
	if commandName == "down" {
		delta.Consequences = []string{"stop the project environment"}
	}
	if commandName == "up" && action == "project.environment.rebuild" {
		delta.Consequences = []string{"rebuild and recreate the owned project environment"}
	}
	if len(delta.Consequences) == 0 {
		delta.Consequences = []string{"apply the prepared project action"}
	}
	return action, delta, nil
}

func (runner ProjectActionRunner) ObserveRemoval(ctx context.Context, action domain.ActionID, observation *ProjectRemovalObservation, approved **ProjectRemovalObservation) error {
	if runner.Project.Mode == domain.ProjectBind &&
		runner.Yard.AccessKind == domain.AccessRemote {
		return errors.New("remote yards cannot own bind projects")
	}

	needsRuntime := action == "project.remove-workspace" ||
		(runner.Project.Target != "" && runner.Project.Target != "yard")
	needsInstance := needsRuntime || action == "project.bind-detach"
	if runner.Yard.AccessKind != domain.AccessRemote && needsInstance {
		instance, instanceErr := runner.Instances.Instance(
			ctx, runner.Yard.IncusProject, runner.Yard.YardInstanceName,
		)
		if instanceErr != nil {
			return instanceErr
		}
		if needsRuntime && !strings.EqualFold(instance.Status, "running") {
			return fmt.Errorf("yard %q must be running", runner.Yard.YardInstanceName)
		}
		if action == "project.bind-detach" {
			observation.DeviceChecked = true
			_, observation.DevicePresent = instance.Devices[state.WorkspaceDeviceFor(runner.Project)]
		}
	}

	data := runner.Data
	if runner.Project.Target != "" && runner.Project.Target != "yard" {
		result, probeErr := data.Execute(ctx, runner.Yard, ports.InstanceExecRequest{
			Command: []string{"docker", "info"},
		})
		if probeErr != nil || result.ExitCode != 0 {
			if probeErr == nil {
				probeErr = fmt.Errorf("exit status %d", result.ExitCode)
			}
			return fmt.Errorf("reach project environment before removal: %w", probeErr)
		}
		box := "subyard-box-" + state.ProjectTechnicalID(runner.Project)
		present, probeErr := probeProjectRemovalPresence(
			ctx, data, runner.Yard,
			[]string{
				"sh", "-c",
				`if docker inspect "$1" >/dev/null 2>&1; then printf present; else printf missing; fi`,
				"subyard", box,
			},
		)
		if probeErr != nil {
			return fmt.Errorf("inspect project environment before removal: %w", probeErr)
		}
		observation.EnvironmentChecked = true
		observation.EnvironmentPresent = present
		if present {
			identity, err := data.Execute(ctx, runner.Yard, ports.InstanceExecRequest{Command: []string{"docker", "inspect", "-f", `{{ .Id }}{{ "\t" }}{{ index .Config.Labels "subyard.env" }}{{ "\t" }}{{ index .Config.Labels "subyard.project" }}{{ "\t" }}{{ index .Config.Labels "subyard.profile" }}`, box}})
			if err != nil || identity.ExitCode != 0 {
				return errors.New("cannot inspect removal environment identity")
			}
			fields := strings.Split(strings.TrimRight(string(identity.Stdout), "\r\n"), "\t")
			if len(fields) != 4 || fields[0] == "" || fields[1] != "1" || fields[2] != runner.Project.ProjectID || fields[3] != runner.Project.Target {
				return errors.New("removal environment ownership differs from prepared project")
			}
			observation.EnvironmentID = fields[0]
		}
		staged, err := probeProjectRemovalPresence(ctx, data, runner.Yard, []string{"sh", "-c", `if [ -e "$1" ] || [ -L "$1" ] || [ -e "$2" ] || [ -L "$2" ]; then printf present; else printf missing; fi`, "subyard", "/srv/env-secrets/" + runner.Project.ProjectID, "/srv/env-meta/" + runner.Project.ProjectID})
		if err != nil {
			return err
		}
		observation.StagedEnvironmentChecked, observation.StagedEnvironmentPresent = true, staged
	}
	if action == "project.remove-workspace" {
		present, probeErr := probeProjectRemovalPresence(
			ctx, data, runner.Yard,
			[]string{
				"sh", "-c", `if [ -d "$1" ]; then printf present; else printf missing; fi`,
				"subyard", filepath.Dir(runner.Project.YardPath),
			},
		)
		if probeErr != nil {
			return fmt.Errorf("inspect project workspace before removal: %w", probeErr)
		}
		observation.WorkspaceChecked = true
		observation.WorkspacePresent = present
	}
	if *approved != nil {
		before, after := **approved, *observation
		if before.WorkspaceChecked && !before.WorkspacePresent && after.WorkspacePresent ||
			before.EnvironmentChecked && !before.EnvironmentPresent && after.EnvironmentPresent ||
			before.DeviceChecked && !before.DevicePresent && after.DevicePresent ||
			before.StagedEnvironmentChecked && !before.StagedEnvironmentPresent && after.StagedEnvironmentPresent ||
			before.EnvironmentPresent && after.EnvironmentPresent && before.EnvironmentID != after.EnvironmentID {
			return fmt.Errorf("%w: project removal now includes an unapproved target", domain.ErrPlanStale)
		}
	} else {
		snapshot := *observation
		*approved = &snapshot
	}
	return nil
}

func probeProjectRemovalPresence(
	ctx context.Context,
	data ports.YardExecutor,
	yard domain.Context,
	command []string,
) (bool, error) {
	if data == nil {
		return false, errors.New("project data plane is required")
	}
	result, err := data.Execute(ctx, yard, ports.InstanceExecRequest{Command: command})
	if err != nil {
		return false, err
	}
	if result.ExitCode != 0 {
		return false, fmt.Errorf("exit status %d", result.ExitCode)
	}
	switch strings.TrimSpace(string(result.Stdout)) {
	case "present":
		return true, nil
	case "missing":
		return false, nil
	default:
		return false, errors.New("presence probe returned invalid output")
	}
}

func (runner ProjectActionRunner) ObserveBind(ctx context.Context, changed, deviceConverged, metadataConverged bool) (bool, bool, bool, error) {
	if runner.Yard.AccessKind == domain.AccessRemote {
		return changed, deviceConverged, metadataConverged, errors.New("bind is host-local; use sync or clone")
	}
	instance, err := runner.Instances.Instance(
		ctx, runner.Yard.IncusProject, runner.Yard.YardInstanceName,
	)
	if err != nil {
		return changed, deviceConverged, metadataConverged, fmt.Errorf("inspect bind device: %w", err)
	}
	deviceName := state.WorkspaceDeviceFor(runner.Project)
	current, exists := instance.LocalDevices[deviceName]
	deviceConverged = exists
	metadataConverged = false
	if !exists {
		changed = true
		return changed, deviceConverged, metadataConverged, nil
	}
	desired := map[string]string{
		"type": "disk", "source": runner.Project.HostPath,
		"path": runner.Project.YardPath, "shift": "true",
	}
	if !maps.Equal(current, desired) {
		return changed, deviceConverged, metadataConverged, fmt.Errorf("instance device %q already exists with different configuration", deviceName)
	}
	converged, err := runner.projectMetadataConverged(ctx)
	if err != nil {
		return changed, deviceConverged, metadataConverged, err
	}
	metadataConverged = converged
	changed = !converged
	return changed, deviceConverged, metadataConverged, nil
}

func (runner ProjectActionRunner) ObserveExport(ctx context.Context, approvedTree string) (changed bool, liveTree, sourceTree string, err error) {
	archive, err := runner.PreparedArchive.Open(ctx, runner.Project.HostPath)
	if err != nil {
		return false, "", "", err
	}
	source, err := ProjectArchiveTreeDigest(archive)
	if err := errors.Join(err, archive.Close()); err != nil {
		return false, "", "", err
	}
	live, err := ObserveProjectTree(ctx, runner.Data, runner.Yard, runner.Project.YardPath)
	if err != nil {
		return false, "", "", err
	}
	if approvedTree != "" && live != approvedTree && live != source {
		return false, "", "", fmt.Errorf("%w: project export source changed after assessment", domain.ErrPlanStale)
	}
	return live != source, live, source, nil
}

func (runner ProjectActionRunner) ObserveEnvironment(
	ctx context.Context, commandName string, profile ProjectEnvironmentProfile,
	hasSecret, rebuild, changed bool, observed, approved string,
) (bool, string, string, error) {
	if runner.Project.Target == "" || runner.Project.Target == "yard" {
		return changed, observed, approved, fmt.Errorf("project %q has no project environment", runner.Project.Name)
	}
	box := "subyard-box-" + state.ProjectTechnicalID(runner.Project)
	result, err := runner.Data.Execute(
		ctx, runner.Yard, ports.InstanceExecRequest{Command: []string{
			"sh", "-c",
			`if ! docker inspect "$1" >/dev/null 2>&1; then printf missing; else docker inspect -f '{{if .State.Running}}running{{else}}stopped{{end}}{{ "\t" }}{{ .Id }}{{ "\t" }}{{ index .Config.Labels "subyard.env" }}{{ "\t" }}{{ index .Config.Labels "subyard.project" }}{{ "\t" }}{{ index .Config.Labels "subyard.profile" }}' "$1"; fi`,
			"subyard", box,
		}},
	)
	if err != nil || result.ExitCode != 0 {
		return changed, observed, approved, fmt.Errorf("inspect project environment: %w", err)
	}
	observed = strings.TrimRight(string(result.Stdout), "\r\n")
	if approved == "" {
		approved = observed
	}
	if observed == "missing" {
		if commandName == "down" {
			return changed, observed, approved, fmt.Errorf("no box for %q", runner.Project.Name)
		}
		changed = true
		return changed, observed, approved, nil
	}
	fields := strings.Split(observed, "\t")
	if len(fields) != 5 || fields[1] == "" || fields[0] != "stopped" && fields[0] != "running" {
		return changed, observed, approved, errors.New("project environment probe returned invalid state")
	}
	if fields[2] != "1" || fields[3] != runner.Project.ProjectID ||
		fields[4] != runner.Project.Target {
		return changed, observed, approved, fmt.Errorf("project environment %q is not owned by project %q and profile %q",
			box, runner.Project.ProjectID, runner.Project.Target)
	}
	if approved != "missing" {
		before := strings.Split(approved, "\t")
		if len(before) != 5 || before[1] != fields[1] {
			return changed, observed, approved, fmt.Errorf("%w: project environment identity changed", domain.ErrPlanStale)
		}
	}
	if commandName == "down" {
		changed = fields[0] == "running"
		return changed, observed, approved, nil
	}
	if rebuild {
		changed = true
		return changed, observed, approved, nil
	}
	manifest, err := ProjectEnvironmentManifest(
		runner.Project, profile, hasSecret,
	)
	if err != nil {
		return changed, observed, approved, fmt.Errorf("build project environment manifest: %w", err)
	}
	manifest = append(manifest, '\n')
	converged, err := runner.yardFileConverged(
		ctx, "/srv/env-meta/"+runner.Project.ProjectID+"/profile.json", manifest,
	)
	if err != nil {
		return changed, observed, approved, err
	}
	if !converged {
		return changed, observed, approved, errors.New("project environment manifest differs; run up --rebuild")
	}
	changed = fields[0] != "running"
	return changed, observed, approved, nil
}

func (runner ProjectActionRunner) projectMetadataConverged(
	ctx context.Context,
) (bool, error) {
	payload, err := ProjectMetadata(runner.Project, runner.Yard.YardName)
	if err != nil {
		return false, fmt.Errorf("build project metadata: %w", err)
	}
	metadataPath := filepath.Join(filepath.Dir(runner.Project.YardPath), ".subyard-meta.json")
	return runner.yardFileConverged(ctx, metadataPath, payload)
}

func (runner ProjectActionRunner) yardFileConverged(
	ctx context.Context,
	path string,
	payload []byte,
) (bool, error) {
	digest := fmt.Sprintf("%x", sha256.Sum256(payload))
	result, err := runner.Data.Execute(
		ctx, runner.Yard, ports.InstanceExecRequest{Command: []string{
			"sh", "-c",
			`if [ ! -f "$2" ]; then printf missing; exit 0; fi; actual=$(sha256sum -- "$2") || exit; actual=${actual%% *}; if [ "$actual" = "$1" ]; then printf match; else printf different; fi`,
			"subyard", digest, path,
		}},
	)
	if err != nil || result.ExitCode != 0 {
		if err == nil {
			err = fmt.Errorf("exit status %d", result.ExitCode)
		}
		return false, fmt.Errorf("inspect yard file %s: %w", path, err)
	}
	switch strings.TrimSpace(string(result.Stdout)) {
	case "match":
		return true, nil
	case "missing", "different":
		return false, nil
	default:
		return false, errors.New("yard file probe returned invalid state")
	}
}

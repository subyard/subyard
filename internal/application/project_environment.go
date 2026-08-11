package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/state"
)

type ProjectEnvironmentProfile struct {
	BaseImage   string
	Dockerfile  string
	Context     string
	Image       string
	Caches      []string
	Features    []string
	Devices     []string
	Mounts      []string
	Environment map[string]string
}

type ProjectEnvironmentRunner struct {
	Data      ports.YardExecutor
	Yard      domain.Context
	Project   domain.ProjectRecord
	Profile   ProjectEnvironmentProfile
	HostLinks []string
	Rebuild   bool
	HasSecret bool
}

func (runner ProjectEnvironmentRunner) Run(
	ctx context.Context,
	request domain.AdapterRequest,
	protected io.Reader,
) (domain.AdapterResult, string, error) {
	if request.Adapter != "project-env" ||
		request.Action != "up" && request.Action != "down" && request.Action != "info" {
		return domain.AdapterResult{}, "", errors.New("unsupported project environment action")
	}
	if runner.Data == nil {
		return domain.AdapterResult{}, "", errors.New("project data plane is required")
	}
	if err := runner.Project.Validate(runner.Project.ProjectID); err != nil {
		return domain.AdapterResult{}, "", err
	}
	if runner.Project.Target == "" || runner.Project.Target == "yard" {
		return domain.AdapterResult{}, "", fmt.Errorf("project %q has no project environment", runner.Project.Name)
	}
	if err := runner.execute(ctx, "reach Docker", ports.InstanceExecRequest{Command: []string{"docker", "info"}}); err != nil {
		return domain.AdapterResult{}, "", err
	}

	message, err := runner.run(ctx, request.Action, protected)
	if err != nil {
		return domain.AdapterResult{}, "", err
	}
	return domain.AdapterResult{
		Schema: 1, OperationID: request.OperationID, Status: "ok",
		Output: map[string]any{"projectId": runner.Project.ProjectID, "yardPath": runner.Project.YardPath},
	}, message, nil
}

func (runner ProjectEnvironmentRunner) run(ctx context.Context, action string, protected io.Reader) (string, error) {
	technicalID := state.ProjectTechnicalID(runner.Project)
	box := "subyard-box-" + technicalID
	manifest := "/srv/env-meta/" + runner.Project.ProjectID + "/profile.json"
	switch action {
	case "down":
		if !runner.probe(ctx, []string{"docker", "inspect", box}) {
			return "", fmt.Errorf("no box for %q", runner.Project.Name)
		}
		if err := runner.validateBoxOwnership(ctx, box); err != nil {
			return "", err
		}
		if err := runner.execute(ctx, "stop project environment", ports.InstanceExecRequest{Command: []string{"docker", "stop", box}}); err != nil {
			return "", err
		}
		return fmt.Sprintf("box %q stopped\n", runner.Project.Name), nil
	case "info":
		if !runner.probe(ctx, []string{"docker", "inspect", box}) {
			return "", fmt.Errorf("no box for %q", runner.Project.Name)
		}
		result, err := runner.Data.Execute(ctx, runner.Yard, ports.InstanceExecRequest{Command: []string{"cat", manifest}})
		if err != nil {
			return "", executionError("read project environment manifest", result, err)
		}
		return string(result.Stdout), nil
	case "up":
		return runner.up(ctx, box, manifest, protected)
	default:
		return "", errors.New("unsupported project environment action")
	}
}

func (runner ProjectEnvironmentRunner) up(
	ctx context.Context,
	box string,
	manifestPath string,
	protected io.Reader,
) (string, error) {
	if err := runner.validateProfile(); err != nil {
		return "", err
	}
	shareSessions := runner.probe(ctx, []string{"test", "-d", "/mnt/host/agent-sessions"})
	if runner.probe(ctx, []string{"docker", "inspect", box}) {
		if err := runner.validateBoxOwnership(ctx, box); err != nil {
			return "", err
		}
		if !runner.Rebuild {
			if err := runner.execute(ctx, "start project environment", ports.InstanceExecRequest{Command: []string{"docker", "start", box}}); err != nil {
				return "", err
			}
			if shareSessions {
				runner.linkSessions(ctx, box)
			}
			return fmt.Sprintf("box %q already exists — started (profile %s)\n", runner.Project.Name, runner.Project.Target), nil
		}
		if err := runner.execute(ctx, "remove project environment for rebuild", ports.InstanceExecRequest{
			Command: []string{"docker", "rm", "-f", box},
		}); err != nil {
			return "", err
		}
	}
	for _, cache := range runner.Profile.Caches {
		uid := fmt.Sprint(runner.Yard.DevUID)
		if err := runner.execute(ctx, "create profile cache", ports.InstanceExecRequest{
			Command: []string{"install", "-d", "-o", uid, "-g", uid, "--", cache},
		}); err != nil {
			return "", err
		}
	}

	image := projectEnvironmentImage(runner.Project, runner.Profile)
	if runner.Profile.Dockerfile != "" {
		dockerfile := filepath.Join(runner.Project.YardPath, runner.Profile.Dockerfile)
		if !runner.probe(ctx, []string{"test", "-r", dockerfile}) {
			return "", fmt.Errorf("profile Dockerfile is missing in the workspace: %s", runner.Profile.Dockerfile)
		}
		if runner.Rebuild || !runner.probe(ctx, []string{"docker", "image", "inspect", image}) {
			buildContext := filepath.Join(runner.Project.YardPath, runner.Profile.Context)
			if err := runner.execute(ctx, "build project environment image", ports.InstanceExecRequest{
				Command: []string{"docker", "build", "-t", image, "-f", dockerfile, buildContext},
			}); err != nil {
				return "", err
			}
		}
	}

	secretPath := "/srv/env-secrets/" + runner.Project.ProjectID + "/profile.env"
	if runner.HasSecret {
		if protected == nil {
			return "", errors.New("profile secret input is required")
		}
		if err := runner.writeFile(ctx, secretPath, "0600", protected); err != nil {
			return "", fmt.Errorf("stage profile secret: %w", err)
		}
	}
	payload, err := ProjectEnvironmentManifest(runner.Project, runner.Profile, runner.HasSecret)
	if err != nil {
		return "", err
	}
	if err := runner.writeFile(ctx, manifestPath, "0644", strings.NewReader(string(payload)+"\n")); err != nil {
		return "", fmt.Errorf("stage project environment manifest: %w", err)
	}

	arguments := []string{"docker", "run", "-d", "--name", box,
		"--restart", "unless-stopped", "--label", "subyard.env=1", "--label", "subyard.project=" + runner.Project.ProjectID,
		"--label", "subyard.profile=" + runner.Project.Target, "-v", runner.Project.YardPath + ":/workspace", "-w", "/workspace"}
	for _, cache := range runner.Profile.Caches {
		arguments = append(arguments, "-v", cache+":"+cache)
	}
	for _, mount := range runner.Profile.Mounts {
		arguments = append(arguments, "-v", mount)
	}
	if shareSessions {
		arguments = append(arguments, "-v", "/mnt/host/agent-sessions:/mnt/host/agent-sessions:rw")
	}
	arguments = append(arguments, "-v", manifestPath+":/run/subyard/profile.json:ro")
	if runner.HasSecret {
		arguments = append(arguments, "-v", secretPath+":/run/subyard/profile.env:ro")
	}
	keys := make([]string, 0, len(runner.Profile.Environment))
	for key := range runner.Profile.Environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		arguments = append(arguments, "-e", key+"="+runner.Profile.Environment[key])
	}
	var warnings strings.Builder
	for _, device := range runner.Profile.Devices {
		switch device {
		case "kvm":
			if runner.probe(ctx, []string{"test", "-e", "/dev/kvm"}) {
				arguments = append(arguments, "--device", "/dev/kvm")
			} else {
				fmt.Fprintln(&warnings, "warning: /dev/kvm absent in yard — skipping")
			}
		default:
			fmt.Fprintf(&warnings, "warning: profile device %q is not supported — skipping\n", device)
		}
	}
	arguments = append(arguments, image, "sleep", "infinity")
	if err := runner.execute(ctx, "start project environment", ports.InstanceExecRequest{Command: arguments}); err != nil {
		return warnings.String(), err
	}
	if shareSessions {
		runner.linkSessions(ctx, box)
	}
	fmt.Fprintf(&warnings, "box %q up (profile %s, image %s)\n", runner.Project.Name, runner.Project.Target, image)
	return warnings.String(), nil
}

func (runner ProjectEnvironmentRunner) validateBoxOwnership(ctx context.Context, box string) error {
	result, err := runner.Data.Execute(ctx, runner.Yard, ports.InstanceExecRequest{Command: []string{
		"docker", "inspect", "-f",
		`{{ index .Config.Labels "subyard.env" }}{{ "\t" }}{{ index .Config.Labels "subyard.project" }}{{ "\t" }}{{ index .Config.Labels "subyard.profile" }}`,
		box,
	}})
	if err != nil {
		return executionError("inspect project environment ownership", result, err)
	}
	labels := strings.Split(strings.TrimSpace(string(result.Stdout)), "\t")
	if len(labels) != 3 || labels[0] != "1" ||
		labels[1] != runner.Project.ProjectID || labels[2] != runner.Project.Target {
		return fmt.Errorf("project environment %q is not owned by project %q and profile %q",
			box, runner.Project.ProjectID, runner.Project.Target)
	}
	return nil
}

func (runner ProjectEnvironmentRunner) validateProfile() error {
	if runner.Profile.BaseImage == "" {
		return fmt.Errorf("profile %q has no PROJECT_ENV_BASE_IMAGE", runner.Project.Target)
	}
	if !projectRelative(runner.Profile.Dockerfile) {
		return errors.New("IMAGE_DOCKERFILE must stay inside the project workspace")
	}
	if !projectRelative(runner.Profile.Context) {
		return errors.New("IMAGE_CONTEXT must stay inside the project workspace")
	}
	for _, mount := range runner.Profile.Mounts {
		lower := strings.ToLower(mount)
		if strings.Contains(lower, "docker.sock") || strings.Contains(lower, "incus.sock") || strings.Contains(lower, "lxd.sock") {
			return fmt.Errorf("ENV_MOUNTS %q exposes a control socket", mount)
		}
		if strings.Contains(lower, ".claude/") || strings.Contains(lower, ".claude:") ||
			strings.Contains(lower, ".codex/") || strings.Contains(lower, ".codex:") ||
			strings.Contains(lower, ".pi/agent/") || strings.Contains(lower, ".pi/agent:") ||
			strings.Contains(lower, "credentials") || strings.Contains(lower, "auth.json") {
			return fmt.Errorf("ENV_MOUNTS %q exposes coding-agent credentials", mount)
		}
	}
	return nil
}

func projectRelative(path string) bool {
	clean := filepath.Clean(path)
	return path == "" || !filepath.IsAbs(path) && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

func projectEnvironmentImage(
	project domain.ProjectRecord,
	profile ProjectEnvironmentProfile,
) string {
	if profile.Dockerfile == "" {
		return profile.BaseImage
	}
	if profile.Image != "" {
		return profile.Image
	}
	return "subyard-env-" + state.ProjectDockerImageID(project)
}

func ProjectEnvironmentManifest(
	project domain.ProjectRecord,
	profile ProjectEnvironmentProfile,
	hasSecret bool,
) ([]byte, error) {
	keys := make([]string, 0, len(profile.Environment))
	for key := range profile.Environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	secrets := []map[string]string{}
	if hasSecret {
		secrets = append(secrets, map[string]string{"name": "profile.env", "path": "/run/subyard/profile.env"})
	}
	return json.MarshalIndent(struct {
		Profile  string              `json:"profile"`
		Image    string              `json:"image"`
		Base     string              `json:"baseImage"`
		Features []string            `json:"features"`
		Caches   []string            `json:"caches"`
		EnvKeys  []string            `json:"envKeys"`
		Secrets  []map[string]string `json:"secrets"`
		Devices  []string            `json:"devices"`
	}{project.Target, projectEnvironmentImage(project, profile), profile.BaseImage, profile.Features,
		profile.Caches, keys, secrets, profile.Devices}, "", "  ")
}

func (runner ProjectEnvironmentRunner) writeFile(ctx context.Context, path, mode string, input io.Reader) error {
	uid := fmt.Sprint(runner.Yard.DevUID)
	if err := runner.execute(ctx, "create staged file directory", ports.InstanceExecRequest{
		Command: []string{"install", "-d", "-m", "0700", "-o", uid, "-g", uid, "--", filepath.Dir(path)},
	}); err != nil {
		return err
	}
	result, err := runner.Data.Stream(ctx, runner.Yard, ports.InstanceExecRequest{
		Command: []string{"sh", "-c", "umask 077; cat > \"$1\" && chmod \"$2\" \"$1\"", "_", path, mode},
		User:    uint32(runner.Yard.DevUID), Group: uint32(runner.Yard.DevUID),
	}, input)
	if err != nil {
		return executionError("write staged file", result, err)
	}
	return nil
}

func (runner ProjectEnvironmentRunner) linkSessions(ctx context.Context, box string) {
	for _, record := range runner.HostLinks {
		name, rest, ok := strings.Cut(strings.TrimSpace(record), ":")
		if !ok {
			continue
		}
		target, kind, _ := strings.Cut(rest, ":")
		if kind == "file" || name == "" || target == "" {
			continue
		}
		target = filepath.Clean(target)
		if filepath.IsAbs(name) || filepath.Clean(name) == ".." || strings.HasPrefix(filepath.Clean(name), ".."+string(filepath.Separator)) ||
			(target != "/mnt/host/agent-sessions" && !strings.HasPrefix(target, "/mnt/host/agent-sessions/")) {
			continue
		}
		_, _ = runner.Data.Execute(ctx, runner.Yard, ports.InstanceExecRequest{Command: []string{
			"docker", "exec", "-u", "0", box, "sh", "-c",
			`home="$(getent passwd "$3" | cut -d: -f6)"; home="${home:-/home/dev}"; link="$home/$1"; ` +
				`[ -d "$(dirname "$2")" ] || exit 0; install -d -o "$3" -g "$3" "$2" "$(dirname "$link")"; ` +
				`if [ -L "$link" ] || [ ! -e "$link" ]; then ln -sfn "$2" "$link"; chown -h "$3:$3" "$link"; fi`,
			"_", name, target, fmt.Sprint(runner.Yard.DevUID),
		}})
	}
}

func (runner ProjectEnvironmentRunner) probe(ctx context.Context, command []string) bool {
	_, err := runner.Data.Execute(ctx, runner.Yard, ports.InstanceExecRequest{Command: command})
	return err == nil
}

func (runner ProjectEnvironmentRunner) execute(ctx context.Context, step string, request ports.InstanceExecRequest) error {
	result, err := runner.Data.Execute(ctx, runner.Yard, request)
	if err == nil {
		return nil
	}
	return executionError(step, result, err)
}

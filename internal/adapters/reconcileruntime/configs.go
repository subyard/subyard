package reconcileruntime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/configmaterial"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

type guestConfigFile struct {
	integration    string
	label          string
	source         string
	destination    string
	followSymlinks bool
	ownedFormat    string
}

func (runtime Runtime) RefreshConfigs(ctx context.Context) error {
	if runtime.Executor == nil {
		return errors.New("Incus executor is required")
	}
	if !domain.SafeName(runtime.devUser()) {
		return errors.New("invalid developer user")
	}
	state, err := runtime.reconcileState(ctx)
	if err != nil || !state.InstanceFound {
		return firstError(err, errors.New("yard instance is missing"))
	}
	if !strings.EqualFold(state.Instance.Status, "running") {
		return errors.New("yard is not running")
	}
	files, err := runtime.guestConfigFiles()
	if err != nil {
		return err
	}
	output := runtime.Stdout
	if output == nil {
		output = io.Discard
	}
	fmt.Fprintf(output, "Refresh agent instructions and configs in %s\n", runtime.Yard.YardInstanceName)
	copied := make(map[string]bool)
	for _, file := range files {
		payload, err := file.readSource()
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Fprintf(output, "  [ ok ] %s: no source — skipping\n", file.label)
				continue
			}
			return err
		}
		if err := runtime.applyGuestConfig(ctx, file, payload); err != nil {
			return fmt.Errorf("apply %s: %w", file.label, err)
		}
		copied[file.label] = true
		fmt.Fprintf(output, "  [ ok ] %s -> ~%s/%s\n",
			file.label, runtime.devUser(), strings.TrimPrefix(file.destination, "/home/"+runtime.devUser()+"/"))
	}
	for _, agent := range strings.Fields(runtime.environmentValue("CODING_TOOL_INTEGRATIONS")) {
		if !copied[agent+" config"] && !copied[agent+" rules"] {
			fmt.Fprintf(output, "  [ ok ] %s: no default config — skipping\n", agent)
		}
	}
	fmt.Fprintln(output, "  [ ok ] Agent instructions and configs refreshed.")
	return nil
}

func (runtime Runtime) ConfigsConverged(ctx context.Context) (bool, error) {
	if runtime.Executor == nil {
		return false, errors.New("Incus executor is required")
	}
	if !domain.SafeName(runtime.devUser()) {
		return false, errors.New("invalid developer user")
	}
	state, err := runtime.reconcileState(ctx)
	if err != nil || !state.InstanceFound {
		return false, firstError(err, errors.New("yard instance is missing"))
	}
	if !strings.EqualFold(state.Instance.Status, "running") {
		return false, errors.New("yard is not running")
	}
	files, err := runtime.guestConfigFiles()
	if err != nil {
		return false, err
	}
	return runtime.guestConfigsConverged(ctx, files)
}

func (runtime Runtime) guestConfigsConverged(ctx context.Context, files []guestConfigFile) (bool, error) {
	for _, file := range files {
		payload, err := file.readSource()
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		request := ports.InstanceExecRequest{Command: []string{"sha256sum", "--", file.destination}}
		if file.ownedFormat != "" {
			request, err = configmaterial.Request(file.ownedFormat, configmaterial.ModeObserve, runtime.devUser(), file.destination, runtime.Yard.DevUID, payload)
			if err != nil {
				return false, err
			}
		}
		result, execErr := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, request)
		if execErr != nil && result.ExitCode == 0 {
			return false, execErr
		}
		if file.ownedFormat != "" {
			if execErr != nil || result.ExitCode != 0 {
				return false, fmt.Errorf("observe %s: structured configuration unavailable", file.label)
			}
			observed, err := configmaterial.ParseObservation(result.Stdout)
			if err != nil {
				return false, err
			}
			if !observed.Converged {
				return false, nil
			}
		} else {
			if execErr != nil {
				if result.ExitCode != 0 {
					return false, nil
				}
				return false, execErr
			}
			fields := strings.Fields(string(result.Stdout))
			if result.ExitCode != 0 || len(fields) == 0 || fields[0] != fmt.Sprintf("%x", sha256.Sum256(payload)) {
				return false, nil
			}
		}
	}

	return true, nil
}

func (runtime Runtime) applyGitIdentity(ctx context.Context) error {
	if runtime.Executor == nil {
		return errors.New("Incus executor is required")
	}
	state, err := runtime.reconcileState(ctx)
	if err != nil || !state.InstanceFound {
		return firstError(err, errors.New("yard instance is missing"))
	}
	if !strings.EqualFold(state.Instance.Status, "running") {
		return errors.New("yard is not running")
	}
	user := runtime.devUser()
	if !domain.SafeName(user) {
		return errors.New("invalid developer user")
	}
	home := "/home/" + user
	dropin := filepath.Join(runtime.Yard.Paths.DataHome, "gitconfig")
	if regularFile(dropin) {
		payload, err := os.ReadFile(dropin)
		if err != nil {
			return err
		}
		if err := runtime.writeGuestFile(ctx, home+"/.gitconfig", payload); err != nil {
			return err
		}
	} else {
		name := runtime.environmentValue("GIT_USER_NAME")
		email := runtime.environmentValue("GIT_USER_EMAIL")
		if name == "" {
			name = runtime.hostGitValue("user.name")
		}
		if email == "" {
			email = runtime.hostGitValue("user.email")
		}
		if name != "" {
			if err := runtime.runGuestAsDev(ctx,
				[]string{"git", "config", "--global", "user.name", name}); err != nil {
				return err
			}
		}
		if email != "" {
			if err := runtime.runGuestAsDev(ctx,
				[]string{"git", "config", "--global", "user.email", email}); err != nil {
				return err
			}
		}
	}
	if err := runtime.runGuestAsDev(ctx,
		[]string{"git", "config", "--global", "--replace-all", "safe.directory", "*"}); err != nil {
		return err
	}
	if runtime.Stdout != nil {
		fmt.Fprintf(runtime.Stdout, "  [ ok ] git config ready for %s\n", user)
	}
	return nil
}

func (runtime Runtime) guestConfigFiles() ([]guestConfigFile, error) {
	user := runtime.devUser()
	home := "/home/" + user
	files := []guestConfigFile{}
	instructions := map[string]guestConfigFile{
		"claude": {label: "Claude instructions", source: runtime.environmentValue("HOST_CLAUDE_MD"),
			destination: home + "/.claude/CLAUDE.md", followSymlinks: true},
		"codex": {label: "Codex instructions", source: runtime.environmentValue("HOST_CODEX_AGENTS_MD"),
			destination: home + "/.codex/AGENTS.md", followSymlinks: true},
		"opencode": {label: "OpenCode instructions", source: runtime.environmentValue("HOST_OPENCODE_AGENTS_MD"),
			destination: home + "/.config/opencode/AGENTS.md", followSymlinks: true},
	}
	values := make(map[string]string)
	for _, entry := range runtime.Environment {
		if key, value, ok := strings.Cut(entry, "="); ok {
			values[key] = value
		}
	}
	assets, err := config.MaterializedAssets(values, user)
	if err != nil {
		return nil, err
	}
	for _, agent := range strings.Fields(values["CODING_TOOL_INTEGRATIONS"]) {
		if instruction, ok := instructions[agent]; ok {
			instruction.integration = agent
			files = append(files, instruction)
		}
		for _, asset := range assets {
			if !strings.HasPrefix(asset.Name, agent+".") {
				continue
			}
			files = append(files, guestConfigFile{integration: agent, label: strings.Replace(asset.Name, ".", " ", 1), source: asset.Source, destination: asset.Destination, ownedFormat: asset.OwnedFormat})
		}
	}

	return files, nil
}

func (file guestConfigFile) readSource() ([]byte, error) {
	return (config.MaterializedAsset{Source: file.source, FollowSymlinks: file.followSymlinks}).ReadSource()
}

func (file guestConfigFile) sourceHash() (string, error) {
	payload, err := file.readSource()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(payload)), nil
}

func (runtime Runtime) applyGuestConfig(ctx context.Context, file guestConfigFile, payload []byte) error {
	if file.ownedFormat == "" {
		return runtime.writeGuestFile(ctx, file.destination, payload)
	}
	request, err := configmaterial.Request(file.ownedFormat, configmaterial.ModeApply, runtime.devUser(), file.destination, runtime.Yard.DevUID, payload)
	if err != nil {
		return err
	}
	result, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, request)
	if err != nil || result.ExitCode != 0 {
		return errors.New("structured configuration apply failed")
	}
	return nil
}

func (runtime Runtime) writeGuestFile(ctx context.Context, destination string, payload []byte) error {
	if !domain.SafeName(runtime.devUser()) ||
		!strings.HasPrefix(destination, "/home/"+runtime.devUser()+"/") {
		return errors.New("guest config destination leaves the developer home")
	}
	uid := runtime.Yard.DevUID
	if uid <= 0 {
		uid = 1000
	}
	request := ports.InstanceExecRequest{
		Command: []string{"sh", "-eu", "-c", `
destination=$1
uid=$2
directory=${destination%/*}
install -d -m 0755 -o "$uid" -g "$uid" "$directory"
temporary=$(mktemp "$directory/.subyard-config.XXXXXX")
trap 'rm -f -- "$temporary"' EXIT HUP INT TERM
cat > "$temporary"
chown "$uid:$uid" "$temporary"
chmod 0644 "$temporary"
mv -f -- "$temporary" "$destination"
trap - EXIT HUP INT TERM
`, "subyard", destination, fmt.Sprint(uid)},
		Stdin: payload,
	}
	return runtime.runGuest(ctx, request)
}

func (runtime Runtime) runGuestAsDev(ctx context.Context, command []string) error {
	if !domain.SafeName(runtime.devUser()) {
		return errors.New("invalid developer user")
	}
	uid := runtime.Yard.DevUID
	if uid <= 0 {
		uid = 1000
	}
	return runtime.runGuest(ctx, ports.InstanceExecRequest{
		Command: command, User: uint32(uid), Group: uint32(uid),
		Environment: map[string]string{"HOME": "/home/" + runtime.devUser()},
	})
}

func (runtime Runtime) runGuest(ctx context.Context, request ports.InstanceExecRequest) error {
	result, err := runtime.Executor.Exec(
		ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, request)
	if err == nil && result.ExitCode == 0 {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("guest command exited with status %d", result.ExitCode)
}

func (runtime Runtime) hostGitValue(key string) string {
	git, err := runtime.executableFromPath("git")
	if err != nil {
		return ""
	}
	command := exec.Command(git, "config", "--global", "--get", key)
	command.Env = runtime.Environment
	output, err := command.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func (runtime Runtime) devUser() string {
	if runtime.Yard.DevUser != "" {
		return runtime.Yard.DevUser
	}
	return runtime.environmentDefault("DEV_USER", "dev")
}

func safeGuestRelativePath(value string) (string, error) {
	if value == "" || filepath.IsAbs(value) {
		return "", errors.New("path must be relative")
	}
	clean := filepath.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("path leaves the developer home")
	}
	return clean, nil
}

func firstError(primary, fallback error) error {
	if primary != nil {
		return primary
	}
	return fallback
}

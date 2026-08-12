package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

type configAuthoringRequest struct {
	action    string
	name      string
	value     string
	inputPath string
	scope     config.SettingScope
	assumeYes bool
}

func (cli *CLI) runConfigAuthoring(
	ctx context.Context,
	loaded config.Loaded,
	action string,
	arguments []string,
	assumeYes bool,
) int {
	if loaded.Context.AccessKind == domain.AccessRemote {
		forwarded := append([]string{action}, arguments...)
		if assumeYes {
			forwarded = append(forwarded, "--yes")
		}
		return cli.forwardRemote(ctx, loaded.Context, "config", forwarded)
	}
	request, err := parseConfigAuthoringRequest(action, arguments, assumeYes)
	if err != nil {
		cli.errorf("config %s: %v", action, err)
		return 2
	}
	definition, err := config.ValidateSettingName(
		request.scope, request.name, action == "import" || action == "edit",
	)
	if err != nil {
		cli.errorf("config %s: %v", action, err)
		return 2
	}
	if action == "set" || action == "unset" {
		if definition.Kind != config.SettingScalar {
			cli.errorf(
				"config %s: %s is a file setting; use config import or config edit",
				action, request.name,
			)
			return 2
		}
		if action == "set" {
			if err := config.ValidateSetting(
				request.scope, request.name, request.value, false,
			); err != nil {
				cli.errorf("config set: %v", err)
				return 2
			}
		}
		path, err := configScalarAuthoringPath(loaded, request.scope)
		if err != nil {
			cli.errorf("config %s: %v", action, err)
			return 2
		}
		snapshot, err := readConfigAuthoringTarget(path)
		if err != nil {
			cli.errorf("config %s: %v", action, err)
			return 1
		}
		var value *string
		if action == "set" {
			value = &request.value
		}
		candidate, err := config.EditPersistentAssignmentContent(
			path, snapshot.Content, request.name, value,
		)
		if err != nil {
			cli.errorf("config %s: %v", action, err)
			return 1
		}
		intended := config.PersistentFileSnapshot{Exists: true, Content: candidate}
		if action == "unset" && len(candidate) == 0 {
			intended = config.PersistentFileSnapshot{Exists: false}
		}
		unchanged := sameConfigAuthoringSnapshot(snapshot, intended)
		if !cli.planConfigAction(ctx, loaded, request.action, request.assumeYes, unchanged,
			fmt.Sprintf("%s %s in persistent %s settings at %s",
				action, request.name, request.scope, path)) {
			return 1
		}
		if unchanged {
			fmt.Fprintf(cli.options.Stdout, "config %s: already current\n", action)
			return 0
		}
		if err := config.WritePersistentAssignmentIfUnchanged(
			loaded.Context.Paths.ConfigHome, path, request.name, value, snapshot,
		); err != nil {
			if errors.Is(err, config.ErrPersistentTargetStale) {
				err = fmt.Errorf("%w: persistent configuration changed after confirmation", domain.ErrPlanStale)
			}
			cli.errorf("config %s: %v", action, err)
			return 1
		}
		fmt.Fprintf(cli.options.Stdout, "config %s: updated %s\n", action, path)
		return 0
	}
	if definition.Kind != config.SettingFile {
		cli.errorf(
			"config %s: %s is a scalar setting; use config set",
			action, request.name,
		)
		return 2
	}
	target, err := cli.configFileAuthoringPath(loaded, request)
	if err != nil {
		cli.errorf("config %s: %v", action, err)
		return 2
	}
	snapshot, err := readConfigAuthoringTarget(target)
	if err != nil {
		cli.errorf("config %s: %v", action, err)
		return 1
	}
	var content []byte
	if action == "import" {
		source := request.inputPath
		if !filepath.IsAbs(source) {
			source = filepath.Join(cli.options.WorkingDir, source)
		}
		content, err = readConfigAuthoringFile(source)
	} else {
		content, err = cli.editConfigAuthoringFile(ctx, snapshot.Content)
	}
	if err != nil {
		cli.errorf("config %s: %v", action, err)
		return 1
	}
	if err := config.ValidateNonSecretContent(request.name, string(content)); err != nil {
		cli.errorf("config %s: %v", action, err)
		return 1
	}
	intended := config.PersistentFileSnapshot{Exists: true, Content: content}
	unchanged := sameConfigAuthoringSnapshot(snapshot, intended)
	if !cli.planConfigAction(ctx, loaded, request.action, request.assumeYes, unchanged,
		fmt.Sprintf("replace persistent %s file setting %s at %s",
			request.scope, request.name, target)) {
		return 1
	}
	if unchanged {
		fmt.Fprintf(cli.options.Stdout, "config %s: already current\n", action)
		return 0
	}
	if err := config.WritePersistentFileIfUnchanged(
		loaded.Context.Paths.ConfigHome, target, snapshot, content,
	); err != nil {
		if errors.Is(err, config.ErrPersistentTargetStale) {
			err = fmt.Errorf("%w: persistent configuration changed after confirmation", domain.ErrPlanStale)
		}
		cli.errorf("config %s: %v", action, err)
		return 1
	}
	fmt.Fprintf(cli.options.Stdout, "config %s: updated %s\n", action, target)
	return 0
}

func sameConfigAuthoringSnapshot(left, right config.PersistentFileSnapshot) bool {
	return left.Exists == right.Exists && bytes.Equal(left.Content, right.Content)
}

func parseConfigAuthoringRequest(
	action string,
	arguments []string,
	assumeYes bool,
) (configAuthoringRequest, error) {
	request := configAuthoringRequest{action: action, assumeYes: assumeYes}
	var positional []string
	for index := 0; index < len(arguments); index++ {
		switch arguments[index] {
		case "--scope":
			if index+1 >= len(arguments) {
				return request, errors.New("--scope needs a value")
			}
			index++
			if request.scope != "" {
				return request, errors.New("--scope may be specified only once")
			}
			request.scope = config.SettingScope(arguments[index])
		case "-y", "--yes":
			request.assumeYes = true
		default:
			positional = append(positional, arguments[index])
		}
	}
	switch request.scope {
	case config.ScopeShared, config.ScopeHost, config.ScopeYard:
	case "":
		return request, errors.New(
			"--scope shared, --scope host or --scope yard is required",
		)
	default:
		return request, fmt.Errorf("unsupported scope %q", request.scope)
	}
	expected := 1
	if action == "set" || action == "import" {
		expected = 2
	}
	if len(positional) != expected {
		return request, fmt.Errorf("expects %d positional argument(s)", expected)
	}
	request.name = positional[0]
	if action == "set" {
		request.value = positional[1]
	}
	if action == "import" {
		request.inputPath = positional[1]
	}
	return request, nil
}

func configScalarAuthoringPath(
	loaded config.Loaded,
	scope config.SettingScope,
) (string, error) {
	switch scope {
	case config.ScopeShared:
		return filepath.Join(
			loaded.Context.Paths.ConfigHome, "overrides", "shared", "config.env",
		), nil
	case config.ScopeHost:
		return filepath.Join(loaded.Context.Paths.ConfigHome, "config.env"), nil
	case config.ScopeYard:
		if loaded.Context.YardName == "" || loaded.Context.YardName == "default" {
			return "", errors.New(
				"yard scope requires selecting a non-default yard with -Y",
			)
		}
		return filepath.Join(
			loaded.Context.Paths.ConfigHome, "yards",
			loaded.Context.YardName, "config.env",
		), nil
	default:
		return "", errors.New("unsupported persistent scope")
	}
}

func (cli *CLI) configFileAuthoringPath(
	loaded config.Loaded,
	request configAuthoringRequest,
) (string, error) {
	var relative string
	for _, mapping := range config.SyncableFileMappings(loaded) {
		if mapping.Name == request.name {
			relative = filepath.FromSlash(mapping.Relative)
			break
		}
	}
	if relative == "" {
		return "", fmt.Errorf(
			"file setting %s has no shipped persistent mapping", request.name,
		)
	}
	root := loaded.Context.Paths.ConfigHome
	switch request.scope {
	case config.ScopeShared:
		root = filepath.Join(root, "overrides", "shared")
	case config.ScopeHost:
		root = filepath.Join(root, "overrides", "host")
	case config.ScopeYard:
		if loaded.Context.YardName == "" || loaded.Context.YardName == "default" {
			return "", errors.New(
				"yard scope requires selecting a non-default yard with -Y",
			)
		}
		root = filepath.Join(root, "yards", loaded.Context.YardName, "overrides")
	}
	return filepath.Join(root, "agents", relative), nil
}

func (cli *CLI) planConfigAction(
	ctx context.Context,
	loaded config.Loaded,
	action string,
	assumeYes bool,
	unchanged bool,
	consequence string,
) bool {
	orchestrator := cli.operationOrchestrator(cli.env["SUBYARD_OPERATION_ID"], loaded, nil, nil)
	_, err := orchestrator.PlanAction(
		ctx, loaded.Context, "config "+action, domain.RemoteOnOwner,
		domain.ActionID("config."+action), domain.ActionDelta{
			Changed: !unchanged, Consequences: []string{consequence},
		}, assumeYes,
	)
	if err != nil {
		if errors.Is(err, application.ErrDeclined) {
			cli.errorf("config %s: operation declined", action)
		} else {
			cli.errorf("config %s: %v", action, err)
		}
		return false
	}
	return true
}

func readConfigAuthoringTarget(path string) (config.PersistentFileSnapshot, error) {
	content, err := readConfigAuthoringFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return config.PersistentFileSnapshot{}, nil
	}
	if err != nil {
		return config.PersistentFileSnapshot{}, err
	}
	return config.PersistentFileSnapshot{Exists: true, Content: content}, nil
}

func readConfigAuthoringFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Size() > 8<<20 {
		return nil, errors.New("input must be a bounded regular non-symlink file")
	}
	return os.ReadFile(path)
}

func (cli *CLI) editConfigAuthoringFile(
	ctx context.Context,
	content []byte,
) ([]byte, error) {
	editor := cli.env["VISUAL"]
	if editor == "" {
		editor = cli.env["EDITOR"]
	}
	if editor == "" {
		return nil, errors.New("VISUAL or EDITOR must name an editor executable")
	}
	if strings.ContainsAny(editor, " \t\r\n\x00") {
		return nil, errors.New(
			"VISUAL or EDITOR must be one executable path without arguments",
		)
	}
	directory, err := os.MkdirTemp("", ".subyard-config-edit-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(directory)
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, err
	}
	draft := filepath.Join(directory, "setting")
	if err := os.WriteFile(draft, content, 0o600); err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, editor, draft)
	command.Env = environmentList(cli.env, nil)
	command.Stdin = cli.options.Stdin
	command.Stdout = cli.options.Stdout
	command.Stderr = cli.options.Stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("editor failed: %w", err)
	}
	return readConfigAuthoringFile(draft)
}

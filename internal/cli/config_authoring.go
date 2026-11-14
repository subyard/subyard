package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

type configAuthoringRequest struct {
	action        string
	name          string
	value         string
	inputPath     string
	scope         config.SettingScope
	assumeYes     bool
	git           bool
	storage       string
	sourceBinding string
	sourceGuard   func() error
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
	requests, err := parseConfigAuthoringRequests(action, arguments, assumeYes)
	if err != nil {
		cli.errorf("config %s: %v", action, err)
		return 2
	}
	if len(requests) > 1 {
		if err := cli.configGitScalarsRefusal(loaded, requests); err != nil {
			cli.errorf("config %s: %v", action, err)
			return 2
		}
		return cli.runConfigGitAuthoring(ctx, loaded, requests, nil)
	}
	request := requests[0]
	definition, err := loaded.Catalog.ValidateSettingName(
		request.scope, request.name, action == "import" || action == "edit",
	)
	if err != nil {
		cli.errorf("config %s: %v", action, err)
		return 2
	}
	if !request.git {
		if err := config.CheckLocalSettingsWritable(loaded.Context.Paths.ConfigHome); err != nil {
			cli.errorf("config %s: %v", action, err)
			return 1
		}
	}
	if request.git && (!definition.Syncable || definition.Sensitive) {
		cli.errorf("config %s: %s cannot be saved in Git", action, request.name)
		return 2
	}
	if action == "set" || action == "unset" {
		if err := configScalarAuthoringRefusal(loaded, definition, request); err != nil {
			cli.errorf("config %s: %v", action, err)
			return 2
		}
		if request.git {
			return cli.runConfigGitAuthoring(ctx, loaded, []configAuthoringRequest{request}, nil)
		}
		return cli.runPreparedConfigMutation(ctx, loaded, append([]string{action}, arguments...), request.assumeYes, func(prepared *preparedCommand) error {
			return prepared.prepareConfigScalar(ctx, action, arguments, true)
		})
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
		draft := snapshot.Content
		if request.git {
			draft, err = readConfigAuthoringFile(loaded.Environment[request.name])
			if err != nil {
				cli.errorf("config edit: %v", err)
				return 1
			}
		}
		content, err = cli.editConfigAuthoringFile(ctx, draft)
	}
	if err != nil {
		cli.errorf("config %s: %v", action, err)
		return 1
	}
	if err := config.ValidateNonSecretContent(request.name, string(content)); err != nil {
		cli.errorf("config %s: %v", action, err)
		return 1
	}
	if request.git {
		return cli.runConfigGitAuthoring(ctx, loaded, []configAuthoringRequest{request}, content)
	}
	return cli.runPreparedConfigMutation(ctx, loaded, append([]string{action}, arguments...), request.assumeYes, func(prepared *preparedCommand) error {
		return prepared.attachConfigFileDraft(request, target, snapshot, content)
	})
}

func sameConfigAuthoringSnapshot(left, right config.PersistentFileSnapshot) bool {
	return left.Exists == right.Exists && bytes.Equal(left.Content, right.Content)
}

// configScalarAuthoringRefusal reports every independent refusal of one typed
// scalar write before planning.
func configScalarAuthoringRefusal(loaded config.Loaded, definition config.SettingDefinition, request configAuthoringRequest) error {
	refusals, writable := configScalarSyntaxRefusals(loaded, definition, request)
	if writable {
		refusals = append(refusals, configScalarStateRefusals(loaded, request)...)
	}
	return domain.CollectRefusals(refusals...)
}

// configScalarSyntaxRefusals checks one value on its own. A setting that
// cannot be written this way is not writable, so state checks do not apply.
func configScalarSyntaxRefusals(loaded config.Loaded, definition config.SettingDefinition, request configAuthoringRequest) ([]error, bool) {
	if request.git && (!definition.Syncable || definition.Sensitive) {
		return []error{fmt.Errorf("%s cannot be saved in Git", request.name)}, false
	}
	if definition.Kind != config.SettingScalar {
		return []error{fmt.Errorf("%s is a file setting; use config import or config edit", request.name)}, false
	}
	var refusals []error
	if request.action == "set" {
		refusals = append(refusals, loaded.Catalog.ValidateSetting(request.scope, request.name, request.value, request.git))
		if request.name == "CODING_TOOL_INTEGRATIONS" {
			refusals = append(refusals, loaded.Catalog.CheckSelectableIntegrations(strings.Fields(request.value)))
		}
	}
	return refusals, true
}

// configScalarStateRefusals checks one value against the configuration state
// it must be consistent with: the current state for a single write, or the
// combined target state for several Git assignments.
func configScalarStateRefusals(state config.Loaded, request configAuthoringRequest) []error {
	var refusals []error
	if request.action == "set" && request.name == "CODING_TOOL_INTEGRATIONS" {
		requested := strings.Fields(request.value)
		if _, err := config.ResolveIntegrationSelection(state.Environment, requested); err != nil {
			refusals = append(refusals, err)
		}
		if request.scope == config.ScopeYard && !state.Integrations.AllowsCodingTools && len(requested) != 0 {
			refusals = append(refusals, errors.New("yard role forbids non-empty CODING_TOOL_INTEGRATIONS"))
		}
	}
	if request.scope == config.ScopeYard && request.name == "YARD_TEMPLATE" {
		refusals = append(refusals, config.ValidateYardTemplateIntegrations(state, request.value))
	}
	return refusals
}

// configGitScalarsRefusal validates several Git scalar assignments. Each value
// is checked on its own and each canonical setting may appear once. Checks
// that relate settings use the combined target state, loaded through the
// normal resolver with every assignment applied to the Git settings layer;
// that state exists only when every assignment is individually valid.
func (cli *CLI) configGitScalarsRefusal(loaded config.Loaded, requests []configAuthoringRequest) error {
	var refusals []error
	seen := make(map[string]string, len(requests))
	for _, request := range requests {
		definition, err := loaded.Catalog.ValidateSettingName(request.scope, request.name, false)
		if err != nil {
			refusals = append(refusals, fmt.Errorf("%s: %w", request.name, err))
			continue
		}
		if prior, ok := seen[definition.Name]; ok {
			refusals = append(refusals, fmt.Errorf("%s and %s assign the same setting", prior, request.name))
		}
		seen[definition.Name] = request.name
		syntax, _ := configScalarSyntaxRefusals(loaded, definition, request)
		refusals = append(refusals, syntax...)
	}
	if err := domain.CollectRefusals(refusals...); err != nil {
		return err
	}
	target, err := cli.configGitScalarsTarget(loaded, requests)
	if err != nil {
		return fmt.Errorf("combined configuration: %w", err)
	}
	for _, request := range requests {
		refusals = append(refusals, configScalarStateRefusals(target, request)...)
	}
	return domain.CollectRefusals(refusals...)
}

// configGitScalarsTarget loads the selected yard with the assignments applied
// to the Git settings cache of their scope, keeping every other layer.
func (cli *CLI) configGitScalarsTarget(loaded config.Loaded, requests []configAuthoringRequest) (config.Loaded, error) {
	paths := loaded.Context.Paths
	yard := loaded.Context.YardName
	if yard == "" {
		yard = "default"
	}
	root := filepath.Join(paths.ConfigHome, filepath.FromSlash(config.GitSettingsRelativePath))
	path := filepath.Join(root, "config.env")
	switch requests[0].scope {
	case config.ScopeShared:
		path = filepath.Join(root, "overrides", "shared", "config.env")
	case config.ScopeYard:
		path = filepath.Join(root, "yards", yard, "config.env")
	}
	content, err := readConfigAuthoringFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return config.Loaded{}, err
	}
	for _, request := range requests {
		value := request.value
		if content, err = config.EditPersistentAssignmentContent(path, content, request.name, &value); err != nil {
			return config.Loaded{}, err
		}
	}
	// The same temporary root as the isolated Git candidate.
	draft, err := os.CreateTemp("", "subyard-config-draft-*")
	if err != nil {
		return config.Loaded{}, err
	}
	defer os.Remove(draft.Name())
	err = draft.Chmod(0o600)
	if err == nil {
		_, err = draft.Write(content)
	}
	if closeErr := draft.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return config.Loaded{}, err
	}
	environment := maps.Clone(cli.baseEnv)
	environment["SUBYARD_CONFIG_HOME"] = paths.ConfigHome
	environment["SUBYARD_CONFIG_DIR"] = paths.ConfigDir
	environment["SUBYARD_HOME"] = paths.DataHome
	delete(environment, "SUBYARD_YARD")
	return config.Load(config.LoadOptions{Resources: cli.operationResourceRegistry, RepositoryRoot: cli.options.RepositoryRoot,
		OperatorHome: paths.OperatorHome, YardName: yard, Environment: environment, FileOverrides: map[string]string{path: draft.Name()}})
}

// configGitAuthoringMessage names the selected settings without their values.
func configGitAuthoringMessage(requests []configAuthoringRequest) string {
	names := make([]string, 0, len(requests))
	for _, request := range requests {
		names = append(names, request.name)
	}
	return fmt.Sprintf("%s %s in %s settings", requests[0].action, strings.Join(names, ", "), requests[0].scope)
}

func parseConfigAuthoringRequest(
	action string,
	arguments []string,
	assumeYes bool,
) (configAuthoringRequest, error) {
	requests, err := parseConfigAuthoringRequests(action, arguments, assumeYes)
	if err != nil {
		return configAuthoringRequest{}, err
	}
	if len(requests) != 1 {
		return configAuthoringRequest{}, errors.New("expects one setting")
	}
	return requests[0], nil
}

// parseConfigAuthoringRequests accepts one setting, or several SETTING VALUE
// pairs for config set --git, which share one commit and push.
func parseConfigAuthoringRequests(
	action string,
	arguments []string,
	assumeYes bool,
) ([]configAuthoringRequest, error) {
	request := configAuthoringRequest{action: action, assumeYes: assumeYes}
	var positional []string
	for index := 0; index < len(arguments); index++ {
		switch arguments[index] {
		case "--scope":
			if index+1 >= len(arguments) {
				return nil, errors.New("--scope needs a value")
			}
			index++
			if request.scope != "" {
				return nil, errors.New("--scope may be specified only once")
			}
			request.scope = config.SettingScope(arguments[index])
		case "--local", "--git":
			if request.storage != "" {
				return nil, errors.New("choose --local or --git only once")
			}
			request.storage = arguments[index]
			request.git = request.storage == "--git"
		case "-y", "--yes":
			request.assumeYes = true
		default:
			positional = append(positional, arguments[index])
		}
	}
	switch request.scope {
	case config.ScopeShared, config.ScopeHost, config.ScopeYard:
	case "":
		return nil, errors.New(
			"--scope shared, --scope host or --scope yard is required",
		)
	default:
		return nil, fmt.Errorf("unsupported scope %q", request.scope)
	}
	if action == "set" && (len(positional) == 0 || len(positional)%2 != 0) {
		return nil, errors.New("expects SETTING VALUE pairs")
	}
	if action == "set" && len(positional) > 2 {
		if !request.git {
			return nil, errors.New("several SETTING VALUE pairs require --git")
		}
		requests := make([]configAuthoringRequest, 0, len(positional)/2)
		for index := 0; index < len(positional); index += 2 {
			pair := request
			pair.name, pair.value = positional[index], positional[index+1]
			requests = append(requests, pair)
		}
		return requests, nil
	}
	expected := 1
	if action == "set" || action == "import" {
		expected = 2
	}
	if len(positional) != expected {
		return nil, fmt.Errorf("expects %d positional argument(s)", expected)
	}
	request.name = positional[0]
	if action == "set" {
		request.value = positional[1]
	}
	if action == "import" {
		request.inputPath = positional[1]
	}
	return []configAuthoringRequest{request}, nil
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
		name := loaded.Context.YardName
		if name == "" {
			name = "default"
		}
		return filepath.Join(
			loaded.Context.Paths.ConfigHome, "yards",
			name, "config.env",
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
		name := loaded.Context.YardName
		if name == "" {
			name = "default"
		}
		root = filepath.Join(root, "yards", name, "overrides")
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
	info, err := os.Lstat(path)
	if err != nil {
		return config.PersistentFileSnapshot{}, err
	}
	identity := config.PersistentFileIdentity{}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		identity = config.PersistentFileIdentity{Device: uint64(stat.Dev), Inode: stat.Ino, Mode: stat.Mode, UID: stat.Uid, GID: stat.Gid, Links: uint64(stat.Nlink)}
	}
	return config.PersistentFileSnapshot{Exists: true, Content: content, Identity: identity}, nil
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

func readConfigPersistentSnapshot(root, path string) (config.PersistentFileSnapshot, error) {
	snapshot, err := config.ReadPersistentFileSnapshot(root, path)
	if errors.Is(err, os.ErrNotExist) {
		return config.PersistentFileSnapshot{}, nil
	}
	return snapshot, err
}

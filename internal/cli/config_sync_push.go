package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/shellquote"
)

type configSyncPushOptions struct {
	message     string
	materialize bool
}

type preparedConfigSyncPush struct {
	checkout       string
	branch         string
	upstream       string
	remote         string
	remoteBranch   string
	expectedHead   string
	expectedRemote string
	remoteURL      string
	candidate      string
	preview        configsync.Plan
	options        configsync.Options
	repository     *configGitCandidate
	createdCommit  bool
	pushRequired   bool
}

func (cli *CLI) runConfigSyncPush(
	ctx context.Context,
	loaded config.Loaded,
	arguments []string,
	assumeYes bool,
) int {
	if loaded.Context.YardType == domain.YardRemote {
		forwarded := append([]string{"sync", "push"}, arguments...)
		if assumeYes {
			forwarded = append(forwarded, "--yes")
		}
		return cli.forwardRemote(ctx, loaded.Context, "config", forwarded)
	}
	request, parsedYes, err := parseConfigSyncPushOptions(arguments)
	if err != nil {
		cli.errorf("config sync push: %v", err)
		return 2
	}
	assumeYes = assumeYes || parsedYes
	prepared, err := cli.prepareConfigSyncPush(ctx, loaded, request)
	if err != nil {
		cli.errorf("config sync push: %v", err)
		return 1
	}
	defer prepared.cleanup(cli, ctx)

	fmt.Fprintln(cli.options.Stdout, "Versioned configuration push")
	fmt.Fprintf(cli.options.Stdout, "  checkout: %s\n", prepared.checkout)
	fmt.Fprintf(cli.options.Stdout, "  target: %s\n", prepared.upstream)
	if prepared.createdCommit {
		fmt.Fprintf(cli.options.Stdout, "  commit: %s\n", prepared.candidate)
	} else {
		fmt.Fprintln(cli.options.Stdout, "  commit: no new persistent configuration changes")
	}
	writeConfigSyncPlan(cli.options.Stdout, prepared.preview)
	changed := prepared.pushRequired || prepared.preview.NeedsApply()
	consequences := []string{}
	if changed && prepared.createdCommit {
		consequences = append(consequences,
			"advance the registered checkout with one configuration commit")
	}
	if changed && prepared.preview.InitializeHostID {
		consequences = append(consequences,
			"record owner host ID "+prepared.preview.HostID)
	}
	if changed {
		for _, change := range prepared.preview.Changes {
			consequences = append(consequences, change.Action+" "+change.Path)
		}
		if prepared.preview.ManifestChanged {
			consequences = append(consequences,
				"update versioned configuration manifest metadata")
		}
	}
	if changed && request.materialize && configSyncPlanNeedsMaterialization(prepared.preview) {
		consequences = append(consequences,
			"refresh affected file settings in running local yards")
	}
	if changed && prepared.pushRequired {
		consequences = append(consequences,
			"push HEAD to exact upstream "+prepared.upstream+" without force")
	}
	orchestrator, operation, err := cli.planConfigSyncOperation(
		ctx, loaded, "config sync push", "config.sync.push", changed,
		consequences, assumeYes,
	)
	if errors.Is(err, application.ErrDeclined) {
		cli.errorf("config sync push: operation declined")
		return 1
	}
	if err != nil {
		cli.errorf("config sync push: %v", err)
		return 1
	}
	if !changed {
		fmt.Fprintln(cli.options.Stdout,
			"config sync push: checkout, live configuration and upstream are already converged")
		return 0
	}
	adapter := &configSyncPushAdapter{cli: cli, prepared: prepared}
	orchestrator.Runner = adapter
	if _, _, err := orchestrator.RunAdapter(ctx, operation, domain.AdapterRequest{
		OperationID: operation.OperationID,
		Adapter:     "config-sync",
		Action:      "push-prepare",
	}, nil); err != nil {
		cli.errorf("config sync push: %v", err)
		return 1
	}
	if request.materialize {
		if err := cli.materializeConfigSyncPlan(
			ctx, loaded, adapter.plan, true,
		); err != nil {
			cli.errorf("config sync push --apply: %v; upstream was not changed", err)
			return 1
		}
	}
	if prepared.pushRequired {
		current, err := cli.configGitOutput(
			ctx, prepared.checkout, "rev-parse", "--verify", "HEAD",
		)
		if err != nil || strings.TrimSpace(current) != adapter.plan.SourceCommit {
			cli.errorf(
				"config sync push: checkout changed before push; upstream was not changed",
			)
			return 1
		}
		refspec := "HEAD:refs/heads/" + prepared.remoteBranch
		if err := cli.configGitRun(
			ctx, prepared.checkout, "push", "--porcelain", "--",
			prepared.remote, refspec,
		); err != nil {
			cli.errorf(
				"config sync push: push failed without force: %v; local checkout remains ahead and can be retried",
				err,
			)
			return 1
		}
	}
	if adapter.plan.NeedsApply() {
		fmt.Fprintf(cli.options.Stdout, "config sync: applied generation %d\n",
			adapter.plan.Generation)
	} else {
		fmt.Fprintln(cli.options.Stdout, "config sync: already converged")
	}
	fmt.Fprintf(cli.options.Stdout, "config sync push: pushed %s\n",
		adapter.plan.SourceCommit)
	cli.writeConfigSyncFollowups(loaded, adapter.plan, request.materialize)
	return 0
}

func parseConfigSyncPushOptions(
	arguments []string,
) (configSyncPushOptions, bool, error) {
	var result configSyncPushOptions
	assumeYes := false
	for index := 0; index < len(arguments); index++ {
		switch arguments[index] {
		case "-m", "--message":
			if index+1 >= len(arguments) {
				return result, false, errors.New("-m needs a value")
			}
			index++
			if result.message != "" {
				return result, false, errors.New("-m may be specified only once")
			}
			result.message = arguments[index]
		case "--apply":
			result.materialize = true
		case "-y", "--yes":
			assumeYes = true
		default:
			return result, false, fmt.Errorf(
				"unknown option %q", arguments[index],
			)
		}
	}
	if strings.TrimSpace(result.message) == "" ||
		strings.ContainsAny(result.message, "\x00\r\n") {
		return result, false, errors.New(
			"a single-line commit message is required with -m",
		)
	}
	return result, assumeYes, nil
}

func (cli *CLI) prepareConfigSyncPush(
	ctx context.Context,
	loaded config.Loaded,
	request configSyncPushOptions,
) (*preparedConfigSyncPush, error) {
	record, exists, err := configsync.ReadSourceRecord(
		loaded.Context.Paths.ConfigHome,
	)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf(
			"no source is registered; run %s config sync connect <git-url>",
			cli.options.Program,
		)
	}
	state := cli.inspectConfigGit(ctx, record.Checkout)
	verifyConfigGitRegistration(record, &state)
	if state.Problem != nil {
		return nil, state.Problem
	}
	if state.Branch == "" || state.Branch == "detached" {
		return nil, errors.New("registered checkout has detached HEAD")
	}
	if state.Upstream == "" || state.Upstream == "not configured" ||
		state.RemoteName == "" {
		return nil, errors.New(
			"current branch has no exact upstream; configure one with git push -u",
		)
	}
	if state.Worktree != "clean" {
		return nil, fmt.Errorf(
			"checkout is %s; review it with git -C %q status --short",
			state.Worktree, record.Checkout,
		)
	}
	if _, err := cli.configGitInspectOutput(
		ctx, record.Checkout, "var", "GIT_AUTHOR_IDENT",
	); err != nil {
		return nil, errors.New(
			"Git author identity is not configured; set user.name and user.email for the operator account",
		)
	}
	remoteBranch := strings.TrimPrefix(
		state.Upstream, state.RemoteName+"/",
	)
	if remoteBranch == state.Upstream || remoteBranch == "" ||
		strings.HasPrefix(remoteBranch, "refs/") {
		return nil, errors.New("upstream branch cannot be mapped to an exact remote branch")
	}
	syncState, err := configsync.ReadStatus(
		loaded.Context.Paths.ConfigHome, cli.baseEnv,
	)
	if err != nil {
		return nil, err
	}
	if syncState.RecoveryRequired {
		return nil, errors.New(
			"an interrupted configuration transaction requires recovery with a normal config sync",
		)
	}
	repository, err := cli.prepareConfigGitCandidate(ctx, record.Checkout, state)
	if err != nil {
		return nil, err
	}
	if repository.behind != 0 {
		relation := configGitRelation(repository.ahead, repository.behind)
		repository.cleanup()
		return nil, fmt.Errorf(
			"upstream is %s; run %s config sync pull before pushing",
			relation, cli.options.Program,
		)
	}
	options := configsync.Options{
		SourceRoot:     record.Checkout,
		ConfigHome:     loaded.Context.Paths.ConfigHome,
		RepositoryRoot: cli.options.RepositoryRoot,
		OperatorHome:   loaded.Context.Paths.OperatorHome,
		Environment:    cli.baseEnv,
		FileSettings:   config.SyncableFileMappings(loaded),
		Adopt:          true,
		YardInUse:      cli.configSyncYardInUse(loaded),
	}
	prepared := &preparedConfigSyncPush{
		checkout: record.Checkout, branch: state.Branch,
		upstream: state.Upstream, remote: state.RemoteName,
		remoteBranch: remoteBranch, expectedHead: state.Head,
		expectedRemote: repository.remoteCommit, remoteURL: state.RemoteRaw,
		options: options, repository: repository,
		pushRequired: repository.ahead != 0,
	}
	if err := hardenConfigCandidate(repository.checkout); err != nil {
		prepared.cleanup(cli, ctx)
		return nil, fmt.Errorf("protect export candidate: %w", err)
	}
	if err := cli.exportPersistentConfig(
		loaded, repository.checkout, syncState.HostID,
	); err != nil {
		prepared.cleanup(cli, ctx)
		return nil, fmt.Errorf("export persistent configuration: %w", err)
	}
	if err := hardenConfigCandidate(repository.checkout); err != nil {
		prepared.cleanup(cli, ctx)
		return nil, fmt.Errorf("protect exported configuration: %w", err)
	}
	if err := cli.configGitRun(
		ctx, repository.checkout, "add", "--all", "--", ".",
	); err != nil {
		prepared.cleanup(cli, ctx)
		return nil, fmt.Errorf("stage configuration export: %w", err)
	}
	staged, err := cli.configGitOutput(
		ctx, repository.checkout, "diff", "--cached", "--quiet", "--exit-code",
	)
	if err != nil {
		_ = staged
		if err := cli.configGitRun(
			ctx, repository.checkout, "commit", "--quiet", "-m", request.message,
		); err != nil {
			prepared.cleanup(cli, ctx)
			return nil, fmt.Errorf("create configuration commit: %w", err)
		}
		prepared.createdCommit = true
		prepared.pushRequired = true
	}
	prepared.candidate, err = cli.configGitOutput(
		ctx, repository.checkout, "rev-parse", "--verify", "HEAD",
	)
	if err != nil {
		prepared.cleanup(cli, ctx)
		return nil, err
	}
	prepared.candidate = strings.TrimSpace(prepared.candidate)
	candidateOptions := options
	candidateOptions.SourceRoot = repository.checkout
	candidateOptions.SourceIdentityRoot = record.Checkout
	prepared.preview, err = configsync.BuildPlan(candidateOptions)
	if err != nil {
		prepared.cleanup(cli, ctx)
		return nil, fmt.Errorf("validate exported candidate: %w", err)
	}
	return prepared, nil
}

func (cli *CLI) exportPersistentConfig(
	loaded config.Loaded,
	candidate string,
	hostID string,
) error {
	hostRoot := filepath.Join(candidate, "hosts", hostID)
	sharedRoot := filepath.Join(candidate, "shared")
	if err := os.RemoveAll(sharedRoot); err != nil {
		return err
	}
	if err := os.RemoveAll(hostRoot); err != nil {
		return err
	}
	if err := exportConfigScalarFile(
		filepath.Join(loaded.Context.Paths.ConfigHome, "overrides", "shared", "config.env"),
		filepath.Join(sharedRoot, "config.env"), config.ScopeShared,
	); err != nil {
		return err
	}
	if err := exportConfigScalarFile(
		filepath.Join(loaded.Context.Paths.ConfigHome, "config.env"),
		filepath.Join(hostRoot, "config.env"), config.ScopeHost,
	); err != nil {
		return err
	}
	targets, err := cli.localConfigTargets(loaded, true)
	if err != nil {
		return err
	}
	mappings := map[string]config.FileSettingMapping{}
	for _, target := range targets {
		for _, mapping := range config.SyncableFileMappings(target.Loaded) {
			mappings[mapping.Relative] = mapping
		}
		if target.Name == "default" {
			continue
		}
		source := configScalarLayerPath(target.Loaded, "yard")
		if source == "" {
			continue
		}
		destination := filepath.Join(
			hostRoot, "yards", target.Name, "config.env",
		)
		if err := exportConfigScalarFile(
			source, destination, config.ScopeYard,
		); err != nil {
			return fmt.Errorf("yard %s: %w", target.Name, err)
		}
		if _, err := os.Lstat(destination); errors.Is(err, os.ErrNotExist) {
			if err := writeExportedConfigFile(destination, nil, 0o600); err != nil {
				return fmt.Errorf("yard %s: %w", target.Name, err)
			}
		} else if err != nil {
			return fmt.Errorf("yard %s: %w", target.Name, err)
		}
	}
	relativeNames := make([]string, 0, len(mappings))
	for relative := range mappings {
		relativeNames = append(relativeNames, relative)
	}
	sort.Strings(relativeNames)
	for _, relative := range relativeNames {
		mapping := mappings[relative]
		for _, layer := range []struct {
			source string
			target string
		}{
			{
				filepath.Join(loaded.Context.Paths.ConfigHome,
					"overrides", "shared", "agents", filepath.FromSlash(relative)),
				filepath.Join(sharedRoot,
					"overrides", "agents", filepath.FromSlash(relative)),
			},
			{
				filepath.Join(loaded.Context.Paths.ConfigHome,
					"overrides", "host", "agents", filepath.FromSlash(relative)),
				filepath.Join(hostRoot,
					"overrides", "agents", filepath.FromSlash(relative)),
			},
		} {
			if err := exportConfigAsset(mapping.Name, layer.source, layer.target); err != nil {
				return err
			}
		}
		for _, target := range targets {
			if target.Name == "default" {
				continue
			}
			if err := exportConfigAsset(
				mapping.Name,
				filepath.Join(loaded.Context.Paths.ConfigHome, "yards", target.Name,
					"overrides", "agents", filepath.FromSlash(relative)),
				filepath.Join(hostRoot, "yards", target.Name,
					"overrides", "agents", filepath.FromSlash(relative)),
			); err != nil {
				return err
			}
		}
	}
	return nil
}

func exportConfigScalarFile(
	source string,
	destination string,
	scope config.SettingScope,
) error {
	values, err := config.ReadAssignments(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	names := make([]string, 0, len(values))
	for name, value := range values {
		definition, ok := config.LookupSetting(name)
		if !ok || definition.Kind != config.SettingScalar ||
			!definition.Syncable || definition.Sensitive {
			continue
		}
		if err := config.ValidateSetting(scope, name, value, true); err != nil {
			return err
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	var content strings.Builder
	for _, name := range names {
		content.WriteString(name)
		content.WriteByte('=')
		content.WriteString(shellquote.Word(values[name]))
		content.WriteByte('\n')
	}
	return writeExportedConfigFile(destination, []byte(content.String()), 0o600)
}

func exportConfigAsset(name, source, destination string) error {
	info, err := os.Lstat(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Size() > 8<<20 {
		return fmt.Errorf("file setting %s is not a bounded regular file", name)
	}
	content, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if err := config.ValidateNonSecretContent(name, string(content)); err != nil {
		return err
	}
	return writeExportedConfigFile(destination, content, 0o600)
}

func writeExportedConfigFile(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, content, mode); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

func configScalarLayerPath(loaded config.Loaded, scope string) string {
	for _, layer := range loaded.ConfigurationLayers {
		if layer.Scope == scope && layer.Role == "scalar settings" && layer.Present {
			return layer.Path
		}
	}
	return ""
}

func (prepared *preparedConfigSyncPush) cleanup(cli *CLI, ctx context.Context) {
	_ = cli
	_ = ctx
	if prepared == nil || prepared.repository == nil {
		return
	}
	prepared.repository.cleanup()
	prepared.repository = nil
}

type configSyncPushAdapter struct {
	cli      *CLI
	prepared *preparedConfigSyncPush
	plan     configsync.Plan
}

func (adapter *configSyncPushAdapter) Run(
	ctx context.Context,
	request domain.AdapterRequest,
	_ io.Reader,
) (domain.AdapterResult, string, error) {
	if request.Adapter != "config-sync" || request.Action != "push-prepare" {
		return domain.AdapterResult{}, "", errors.New(
			"invalid configuration push adapter request",
		)
	}
	prepared := adapter.prepared
	head, err := adapter.cli.configGitOutput(
		ctx, prepared.checkout, "rev-parse", "--verify", "HEAD",
	)
	if err != nil || strings.TrimSpace(head) != prepared.expectedHead {
		return domain.AdapterResult{}, "", errors.New(
			"checkout HEAD changed after preview; rerun push",
		)
	}
	if err := adapter.cli.fetchRegisteredConfigUpstream(
		ctx, prepared.checkout, prepared.remote, prepared.remoteURL,
		prepared.expectedRemote,
	); err != nil {
		return domain.AdapterResult{}, "", err
	}
	if prepared.createdCommit {
		if err := adapter.cli.configGitRun(
			ctx, prepared.checkout, "fetch", "--quiet", "--no-tags", "--",
			prepared.repository.checkout, prepared.candidate,
		); err != nil {
			return domain.AdapterResult{}, "", fmt.Errorf(
				"import prepared configuration commit: %w", err,
			)
		}
		if err := adapter.cli.configGitRun(
			ctx, prepared.checkout, "merge", "--ff-only", "--no-edit",
			prepared.candidate,
		); err != nil {
			return domain.AdapterResult{}, "", fmt.Errorf(
				"advance checkout to exported commit: %w", err,
			)
		}
	}
	if err := hardenClonedConfigSource(prepared.checkout); err != nil {
		return domain.AdapterResult{}, "", fmt.Errorf(
			"protect exported configuration checkout: %w", err,
		)
	}
	options := prepared.options
	options.SourceRoot = prepared.checkout
	options.SourceIdentityRoot = prepared.checkout
	plan, err := configsync.BuildPlan(options)
	if err != nil {
		return domain.AdapterResult{}, "", fmt.Errorf(
			"revalidate exported configuration: %w", err,
		)
	}
	if plan.Digest != prepared.preview.Digest {
		return domain.AdapterResult{}, "", errors.New(
			"configuration source or live settings changed after preview; rerun push",
		)
	}
	if err := configsync.Apply(plan); err != nil {
		return domain.AdapterResult{}, "", err
	}
	adapter.plan = plan
	return domain.AdapterResult{
		Schema: 1, OperationID: request.OperationID, Status: "ok",
		Output: map[string]any{
			"commit": plan.SourceCommit, "generation": plan.Generation,
		},
	}, "", nil
}

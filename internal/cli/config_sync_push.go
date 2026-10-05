package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
)

type configSyncPushOptions struct {
	message     string
	materialize bool
	authoring   *configAuthoringRequest
	content     []byte
}

type preparedConfigSyncPush struct {
	checkout          string
	branch            string
	upstream          string
	remote            string
	remoteBranch      string
	expectedHead      string
	expectedRemote    string
	remoteURL         string
	candidate         string
	preview           configsync.Plan
	options           configsync.Options
	repository        *configGitCandidate
	createdCommit     bool
	pushRequired      bool
	repairPermissions bool
}

func (cli *CLI) runConfigSyncPush(
	ctx context.Context,
	loaded config.Loaded,
	arguments []string,
	assumeYes bool,
) int {
	if loaded.Context.AccessKind == domain.AccessRemote {
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
	return cli.runConfigSyncPushRequest(ctx, loaded, request, assumeYes || parsedYes)
}

func (cli *CLI) runConfigSyncPushRequest(ctx context.Context, loaded config.Loaded, request configSyncPushOptions, assumeYes bool) int {
	return cli.runPreparedConfigMutation(ctx, loaded, []string{"sync", "push"}, assumeYes, func(prepared *preparedCommand) error {
		return prepared.prepareConfigPush(ctx, request)
	})
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
	if result.message != "" && (strings.TrimSpace(result.message) == "" ||
		strings.ContainsAny(result.message, "\x00\r\n")) {
		return result, false, errors.New(
			"-m must contain a non-empty single-line commit message",
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
	if err := cli.checkConfigGitPushTarget(ctx, record.Checkout, state.RemoteName, state.RemoteRaw); err != nil {
		return nil, err
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
	candidateOptions := options
	candidateOptions.SourceRoot = repository.checkout
	candidateOptions.SourceIdentityRoot = record.Checkout
	if request.authoring != nil {
		// Validate source boundaries before any writer can follow a tracked
		// symlink in the isolated checkout outside that checkout.
		if _, err := configsync.BuildPlan(candidateOptions); err != nil {
			prepared.cleanup(cli, ctx)
			return nil, fmt.Errorf("validate configuration before selected change: %w", err)
		}
		paths, err := cli.exportSelectedConfig(loaded, repository.checkout, syncState.HostID, *request.authoring, request.content)
		if err != nil {
			prepared.cleanup(cli, ctx)
			return nil, fmt.Errorf("prepare selected configuration change: %w", err)
		}
		if err := hardenConfigCandidate(repository.checkout); err != nil {
			prepared.cleanup(cli, ctx)
			return nil, err
		}
		if len(paths) != 0 {
			if err := cli.configGitRun(ctx, repository.checkout, append([]string{"add", "--all", "--"}, paths...)...); err != nil {
				prepared.cleanup(cli, ctx)
				return nil, fmt.Errorf("stage selected configuration change: %w", err)
			}
		}
		staged, err := cli.configGitOutput(ctx, repository.checkout, "diff", "--cached", "--name-only")
		if err != nil {
			prepared.cleanup(cli, ctx)
			return nil, err
		}
		if strings.TrimSpace(staged) != "" {
			if _, err := cli.configGitInspectOutput(ctx, record.Checkout, "var", "GIT_AUTHOR_IDENT"); err != nil {
				prepared.cleanup(cli, ctx)
				return nil, errors.New("Git author identity is not configured; set user.name and user.email for the operator account")
			}
			if err := cli.configGitRun(ctx, repository.checkout, "commit", "--quiet", "-m", request.message); err != nil {
				prepared.cleanup(cli, ctx)
				return nil, fmt.Errorf("create configuration commit: %w", err)
			}
			prepared.createdCommit = true
			prepared.pushRequired = true
		}
	}
	prepared.candidate, err = cli.configGitOutput(
		ctx, repository.checkout, "rev-parse", "--verify", "HEAD",
	)
	if err != nil {
		prepared.cleanup(cli, ctx)
		return nil, err
	}
	prepared.candidate = strings.TrimSpace(prepared.candidate)
	prepared.preview, err = configsync.BuildPlan(candidateOptions)
	if err != nil {
		prepared.cleanup(cli, ctx)
		return nil, fmt.Errorf("validate exported candidate: %w", err)
	}
	prepared.repairPermissions, err = clonedConfigSourcePermissions(record.Checkout, false)
	if err != nil {
		prepared.cleanup(cli, ctx)
		return nil, fmt.Errorf("inspect checkout permissions: %w", err)
	}
	return prepared, nil
}

func (cli *CLI) checkConfigGitPushTarget(ctx context.Context, checkout, remote, expectedURL string) error {
	urls, err := cli.configGitInspectOutput(ctx, checkout, "remote", "get-url", "--push", "--all", remote)
	if err != nil || strings.TrimSpace(urls) != expectedURL {
		return errors.New("upstream must have one push URL matching the registered configuration source")
	}
	return nil
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
	record, registered, err := configsync.ReadSourceRecord(prepared.options.ConfigHome)
	if err != nil || !registered || record.Checkout != prepared.checkout {
		return domain.AdapterResult{}, "", errors.New("configuration source registration changed after preview")
	}
	state := adapter.cli.inspectConfigGit(ctx, prepared.checkout)
	verifyConfigGitRegistration(record, &state)
	if state.Problem != nil || state.Worktree != "clean" || state.Branch != prepared.branch || state.Upstream != prepared.upstream {
		return domain.AdapterResult{}, "", errors.New("registered checkout or upstream changed after preview")
	}
	// Recheck local overrides before advancing the registered checkout. Apply
	// repeats this exact check under the configuration lock before publishing.
	previewOptions := prepared.options
	previewOptions.SourceRoot = prepared.repository.checkout
	previewOptions.SourceIdentityRoot = prepared.checkout
	preview, err := configsync.BuildPlan(previewOptions)
	if err != nil || preview.Digest != prepared.preview.Digest {
		return domain.AdapterResult{}, "", errors.New("configuration source or local settings changed after preview; rerun push")
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

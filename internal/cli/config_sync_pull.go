package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
)

type preparedConfigSyncPull struct {
	checkout          string
	expectedHead      string
	expectedRemote    string
	remote            string
	remoteURL         string
	preview           configsync.Plan
	options           configsync.Options
	candidate         *configGitCandidate
	fastForward       bool
	repairPermissions bool
}

func (cli *CLI) runConfigSyncPull(
	ctx context.Context,
	loaded config.Loaded,
	arguments []string,
	assumeYes bool,
) int {
	if loaded.Context.AccessKind == domain.AccessRemote {
		forwarded := append([]string{"sync", "pull"}, arguments...)
		if assumeYes {
			forwarded = append(forwarded, "--yes")
		}
		return cli.forwardRemote(ctx, loaded.Context, "config", forwarded)
	}

	for _, argument := range arguments {
		switch argument {
		case "--apply":
		case "-y", "--yes":
			assumeYes = true
		default:
			cli.errorf("config sync pull: unknown option %q", argument)
			return 2
		}
	}
	return cli.runPreparedConfigMutation(ctx, loaded, append([]string{"sync", "pull"}, arguments...), assumeYes, func(prepared *preparedCommand) error {
		return prepared.prepareConfigPull(ctx, arguments)
	})
}

func (cli *CLI) prepareConfigSyncPull(
	ctx context.Context,
	loaded config.Loaded,
) (*preparedConfigSyncPull, error) {
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
	if state.Branch == "detached" || state.Branch == "" {
		return nil, errors.New("registered checkout has detached HEAD")
	}
	if state.Upstream == "" || state.Upstream == "not configured" {
		return nil, errors.New(
			"current branch has no upstream; configure one with git push -u",
		)
	}
	if state.Worktree != "clean" {
		return nil, fmt.Errorf(
			"checkout is %s; review it with git -C %q status --short",
			state.Worktree, record.Checkout,
		)
	}
	candidate, err := cli.prepareConfigGitCandidate(ctx, record.Checkout, state)
	if err != nil {
		return nil, err
	}
	if candidate.ahead != 0 && candidate.behind != 0 {
		candidate.cleanup()
		return nil, errors.New(
			"branch has diverged from upstream; Subyard will not merge or rebase it",
		)
	}
	options := configsync.Options{
		SourceRoot:     record.Checkout,
		ConfigHome:     loaded.Context.Paths.ConfigHome,
		RepositoryRoot: cli.options.RepositoryRoot,
		OperatorHome:   loaded.Context.Paths.OperatorHome,
		Environment:    cli.baseEnv,
		FileSettings:   config.SyncableFileMappings(loaded),
		YardInUse:      cli.configSyncYardInUse(loaded),
	}
	prepared := &preparedConfigSyncPull{
		checkout: record.Checkout, expectedHead: state.Head,
		expectedRemote: candidate.remoteCommit, remote: state.RemoteName,
		remoteURL: state.RemoteRaw, options: options, candidate: candidate,
		fastForward: candidate.behind > 0,
	}
	if prepared.fastForward {
		if err := cli.configGitRun(
			ctx, candidate.checkout, "checkout", "--quiet", "--detach",
			candidate.remoteCommit,
		); err != nil {
			prepared.cleanup(cli, ctx)
			return nil, fmt.Errorf("prepare upstream candidate: %w", err)
		}
	}
	if err := hardenConfigCandidate(candidate.checkout); err != nil {
		prepared.cleanup(cli, ctx)
		return nil, fmt.Errorf("protect upstream candidate: %w", err)
	}
	candidateOptions := options
	candidateOptions.SourceRoot = candidate.checkout
	candidateOptions.SourceIdentityRoot = record.Checkout
	prepared.preview, err = configsync.BuildPlan(candidateOptions)
	if err != nil {
		prepared.cleanup(cli, ctx)
		return nil, fmt.Errorf("validate upstream candidate: %w", err)
	}
	prepared.repairPermissions, err = clonedConfigSourcePermissions(record.Checkout, false)
	if err != nil {
		prepared.cleanup(cli, ctx)
		return nil, fmt.Errorf("inspect checkout permissions: %w", err)
	}
	return prepared, nil
}

func (prepared *preparedConfigSyncPull) cleanup(cli *CLI, ctx context.Context) {
	_ = cli
	_ = ctx
	if prepared == nil || prepared.candidate == nil {
		return
	}
	prepared.candidate.cleanup()
	prepared.candidate = nil
}

type configSyncPullAdapter struct {
	cli      *CLI
	prepared *preparedConfigSyncPull
	plan     configsync.Plan
}

func (adapter *configSyncPullAdapter) Run(
	ctx context.Context,
	request domain.AdapterRequest,
	_ io.Reader,
) (domain.AdapterResult, string, error) {
	if request.Adapter != "config-sync" || request.Action != "pull" {
		return domain.AdapterResult{}, "", errors.New(
			"invalid configuration pull adapter request",
		)
	}
	prepared := adapter.prepared
	head, err := adapter.cli.configGitOutput(
		ctx, prepared.checkout, "rev-parse", "--verify", "HEAD",
	)
	if err != nil || strings.TrimSpace(head) != prepared.expectedHead {
		return domain.AdapterResult{}, "", errors.New(
			"checkout HEAD changed after preview; rerun pull",
		)
	}
	if err := adapter.cli.fetchRegisteredConfigUpstream(
		ctx, prepared.checkout, prepared.remote, prepared.remoteURL,
		prepared.expectedRemote,
	); err != nil {
		return domain.AdapterResult{}, "", err
	}
	if prepared.fastForward {
		if err := adapter.cli.configGitRun(
			ctx, prepared.checkout, "merge", "--ff-only", "--no-edit",
			prepared.expectedRemote,
		); err != nil {
			return domain.AdapterResult{}, "", fmt.Errorf(
				"fast-forward checkout: %w", err,
			)
		}
	}
	if err := hardenClonedConfigSource(prepared.checkout); err != nil {
		return domain.AdapterResult{}, "", fmt.Errorf(
			"protect pulled configuration checkout: %w", err,
		)
	}
	options := prepared.options
	options.SourceRoot = prepared.checkout
	options.SourceIdentityRoot = prepared.checkout
	plan, err := configsync.BuildPlan(options)
	if err != nil {
		return domain.AdapterResult{}, "", fmt.Errorf(
			"revalidate pulled configuration: %w", err,
		)
	}
	if plan.Digest != prepared.preview.Digest {
		return domain.AdapterResult{}, "", errors.New(
			"configuration source or live settings changed after preview; rerun pull",
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

func configSyncPlanNeedsMaterialization(plan configsync.Plan) bool {
	for _, change := range plan.Changes {
		for _, application := range change.Applications {
			if application == config.SettingConfigApply {
				return true
			}
		}
	}
	return false
}

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
)

type configSyncExecution struct {
	approved, current configsync.Plan
	action            domain.ActionID
	metadata          []domain.OperationStep
	consumers         *configSyncConsumers
	consumersApplied  bool
	guard             func(context.Context) error
	run               func(context.Context, *CLI, domain.AdapterRequest) (configsync.Plan, error)
}

func configSyncMetadata(id, target, before, desired, consequence string, changed bool) domain.OperationStep {
	decision := domain.StepSkip
	if changed {
		decision = domain.StepApply
	}
	return domain.OperationStep{ID: "config.sync." + id, Target: target, Observed: before, Desired: desired, Decision: decision,
		Preconditions: []string{"native source registration, checkout and exact upstream remain captured"}, Verify: "read native registration and checkout facts against the approved target", Consequence: consequence}
}

func (execution *configSyncExecution) refreshCheckoutPermissions(checkout string) error {
	needsRepair, err := clonedConfigSourcePermissions(checkout, false)
	if err != nil {
		return err
	}
	for index := range execution.metadata {
		step := &execution.metadata[index]
		if step.ID != "config.sync.permissions" {
			continue
		}
		if needsRepair && step.Decision == domain.StepSkip {
			return fmt.Errorf("%w: checkout permissions now require new work", domain.ErrPlanStale)
		}
		if !needsRepair {
			step.Decision = domain.StepSkip
			step.Observed = step.Desired
		}
	}
	return nil
}

func (execution *configSyncExecution) steps() []domain.OperationStep {
	steps := append(domain.CloneOperationSteps(execution.metadata), execution.current.OperationSteps(execution.approved)...)
	return append(steps, execution.consumers.steps()...)
}
func (execution *configSyncExecution) changed() bool {
	for _, step := range execution.steps() {
		if step.Decision != domain.StepSkip {
			return true
		}
	}
	return false
}
func configPreparedStale(err error) error {
	if errors.Is(err, configsync.ErrPlanStale) {
		return fmt.Errorf("%w: %w", domain.ErrPlanStale, err)
	}
	return err
}
func (prepared *preparedCommand) attachConfigSync(execution *configSyncExecution) {
	cli, loaded := prepared.CLI, prepared.Loaded
	execution.current = execution.approved
	prepared.exactState = operationStateDigest(struct {
		Native string
		Steps  []domain.OperationStep
	}{execution.approved.Digest, execution.steps()})
	prepared.assess = func(context.Context) (domain.ActionID, domain.ActionDelta, error) {
		return execution.action, domain.ActionDelta{Changed: execution.changed(), Consequences: domain.OperationStepConsequences(execution.steps())}, nil
	}
	prepared.stepsComplete = true
	prepared.steps = execution.steps
	prepared.executeNoOp = true
	prepared.execute = func(ctx context.Context, _ *application.Orchestrator, output io.Writer) (domain.AdapterResult, error) {
		if execution.guard != nil {
			if err := execution.guard(ctx); err != nil {
				return domain.AdapterResult{}, configPreparedStale(err)
			}
		}
		fresh, err := execution.approved.Refresh()
		if err != nil {
			return domain.AdapterResult{}, configPreparedStale(err)
		}
		execution.current = fresh
		if err := execution.consumers.refresh(ctx, cli); err != nil {
			return domain.AdapterResult{}, err
		}
		if err := domain.CheckOperationSteps(prepared.Plan.Steps, execution.steps()); err != nil {
			return domain.AdapterResult{}, err
		}
		copyCLI := *cli
		copyCLI.options.Stdout, copyCLI.options.Stderr = output, output
		applied := fresh
		if execution.changed() {
			applied, err = execution.run(ctx, &copyCLI, domain.AdapterRequest{OperationID: prepared.Plan.OperationID})
			if err != nil {
				return domain.AdapterResult{}, configPreparedStale(err)
			}
		}
		// Use the actual published checkout, not a private candidate, for the native
		// convergence check. This also verifies original no-ops without target writes.
		if err := applied.VerifyPublished(); err != nil {
			return domain.AdapterResult{}, err
		}
		if applied.NeedsApply() {
			fmt.Fprintf(output, "config sync: applied generation %d\n", applied.Generation)
		} else {
			fmt.Fprintln(output, "config sync: already converged")
		}
		if !execution.consumersApplied {
			if err := execution.consumers.apply(ctx, &copyCLI, loaded, applied, output); err != nil {
				return domain.AdapterResult{}, err
			}
		}
		copyCLI.writeConfigSyncFollowups(loaded, applied, execution.consumers != nil)
		return configPreparedResult(prepared), nil
	}
}

func (prepared *preparedCommand) prepareConfigSync(ctx context.Context, arguments []string) error {
	if len(arguments) > 0 {
		switch arguments[0] {
		case "pull":
			return prepared.prepareConfigPull(ctx, arguments[1:])
		case "push":
			request, _, err := parseConfigSyncPushOptions(arguments[1:])
			if err != nil {
				return fmt.Errorf("%w: %v", errConfigUsage, err)
			}
			return prepared.prepareConfigPush(ctx, request)
		case "connect":
			return prepared.prepareConfigConnect(ctx, arguments[1:])
		}
	}
	cli, loaded := prepared.CLI, prepared.Loaded
	source, adopt, materialize := "", false, false
	for _, argument := range arguments {
		switch argument {
		case "--adopt":
			adopt = true
		case "--apply":
			materialize = true
		case "-y", "--yes":
		default:
			if strings.HasPrefix(argument, "-") || source != "" {
				return fmt.Errorf("%w: invalid config sync arguments", errConfigUsage)
			}
			source = argument
		}
	}
	if source == "" {
		record, exists, err := configsync.ReadSourceRecord(loaded.Context.Paths.ConfigHome)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: no configuration source is registered", errConfigUsage)
		}
		source = record.Checkout
	}
	options := configsync.Options{SourceRoot: source, ConfigHome: loaded.Context.Paths.ConfigHome, RepositoryRoot: cli.options.RepositoryRoot,
		OperatorHome: loaded.Context.Paths.OperatorHome, Environment: cli.baseEnv, FileSettings: config.SyncableFileMappings(loaded), Adopt: adopt, YardInUse: cli.configSyncYardInUse(loaded)}
	plan, err := configsync.BuildPlan(options)
	if err != nil {
		return err
	}
	execution := &configSyncExecution{approved: plan, action: "config.sync"}
	execution.run = func(ctx context.Context, _ *CLI, request domain.AdapterRequest) (configsync.Plan, error) {
		request.Adapter, request.Action = "config-sync", "apply"
		_, _, err := (configSyncAdapter{plan: execution.current}).Run(ctx, request, nil)
		return execution.current, err
	}
	if materialize {
		if err := prepared.prepareConfigSyncConsumers(ctx, execution); err != nil {
			return err
		}
	}
	prepared.attachConfigSync(execution)
	return nil
}

func (cli *CLI) checkPreparedConfigGit(ctx context.Context, checkout, head, remote, remoteURL, expectedRemote string) error {
	current, err := cli.configGitInspectOutput(ctx, checkout, "rev-parse", "--verify", "HEAD")
	if err != nil || strings.TrimSpace(current) != head {
		return fmt.Errorf("%w: checkout HEAD changed after preview", domain.ErrPlanStale)
	}
	state := cli.inspectConfigGit(ctx, checkout)
	if state.Problem != nil || state.RemoteName != remote || state.RemoteRaw != remoteURL {
		return fmt.Errorf("%w: registered upstream changed after preview", domain.ErrPlanStale)
	}
	candidate, err := cli.prepareConfigGitCandidate(ctx, checkout, state)
	if err != nil {
		return fmt.Errorf("%w: %w", domain.ErrPlanStale, err)
	}
	defer candidate.cleanup()
	if candidate.remoteCommit != expectedRemote {
		return fmt.Errorf("%w: upstream changed after preview; rerun operation", domain.ErrPlanStale)
	}
	return nil
}

func (prepared *preparedCommand) prepareConfigPull(ctx context.Context, arguments []string) error {
	materialize := false
	for _, argument := range arguments {
		if argument == "--apply" {
			materialize = true
			continue
		}
		if argument != "-y" && argument != "--yes" {
			return fmt.Errorf("%w: invalid config sync pull option", errConfigUsage)
		}
	}
	cli := prepared.CLI
	native, err := cli.prepareConfigSyncPull(ctx, prepared.Loaded)
	if err != nil {
		return err
	}
	prepared.closeResource = func() error { native.cleanup(cli, context.Background()); return nil }
	execution := &configSyncExecution{approved: native.preview, action: "config.sync.pull", metadata: []domain.OperationStep{
		configSyncMetadata("checkout", "registered configuration checkout", native.expectedHead, native.expectedRemote, "fast-forward the registered configuration checkout", native.fastForward),
		configSyncMetadata("permissions", "configuration checkout permissions", "native permission baseline", "operator-owned protected files", "remove group/world write permissions from the registered configuration checkout", native.repairPermissions),
	}}
	guard, err := cli.configRegistrationGuard(prepared.Loaded)
	if err != nil {
		return err
	}
	initialState := cli.inspectConfigGit(ctx, native.checkout)
	execution.guard = func(ctx context.Context) error {
		if err := guard(ctx); err != nil {
			return err
		}
		state := cli.inspectConfigGit(ctx, native.checkout)
		if state.Problem != nil || state.Worktree != "clean" || state.Branch != initialState.Branch || state.Upstream != initialState.Upstream {
			return fmt.Errorf("%w: registered checkout changed after preview", domain.ErrPlanStale)
		}
		if err := execution.refreshCheckoutPermissions(native.checkout); err != nil {
			return err
		}
		return cli.checkPreparedConfigGit(ctx, native.checkout, native.expectedHead, native.remote, native.remoteURL, native.expectedRemote)
	}
	execution.run = func(ctx context.Context, copyCLI *CLI, request domain.AdapterRequest) (configsync.Plan, error) {
		native.preview = execution.current
		adapter := &configSyncPullAdapter{cli: copyCLI, prepared: native}
		request.Adapter, request.Action = "config-sync", "pull"
		_, _, err := adapter.Run(ctx, request, nil)
		return adapter.plan, err
	}
	if materialize {
		if err := prepared.prepareConfigSyncConsumers(ctx, execution); err != nil {
			return err
		}
	}
	prepared.attachConfigSync(execution)
	return nil
}

func (prepared *preparedCommand) prepareConfigPush(ctx context.Context, request configSyncPushOptions) error {
	cli, loaded := prepared.CLI, prepared.Loaded
	native, err := cli.prepareConfigSyncPush(ctx, loaded, request)
	if err != nil {
		return err
	}
	prepared.closeResource = func() error { native.cleanup(cli, context.Background()); return nil }
	if request.authoring != nil && (native.createdCommit || native.preview.NeedsApply()) {
		if err := cli.checkResourceConfigChange(ctx, loaded, *request.authoring); err != nil {
			return err
		}
	}
	execution := &configSyncExecution{approved: native.preview, action: "config.sync.push", metadata: []domain.OperationStep{
		configSyncMetadata("checkout", "registered configuration checkout", native.expectedHead, native.candidate, "advance registered checkout to the approved configuration commit", native.createdCommit),
		configSyncMetadata("permissions", "configuration checkout permissions", "native permission baseline", "operator-owned protected files", "remove group/world write permissions from the registered configuration checkout", native.repairPermissions),
		configSyncMetadata("upstream", "registered configuration upstream", "fingerprint:"+operationStateDigest(native.expectedRemote), "fingerprint:"+operationStateDigest(native.candidate), "push HEAD to exact upstream without force", native.pushRequired),
	}}
	guard, err := cli.configRegistrationGuard(loaded)
	if err != nil {
		return err
	}
	execution.guard = func(ctx context.Context) error {
		if err := guard(ctx); err != nil {
			return err
		}
		state := cli.inspectConfigGit(ctx, native.checkout)
		if state.Problem != nil || state.Worktree != "clean" || state.Branch != native.branch || state.Upstream != native.upstream {
			return fmt.Errorf("%w: registered checkout or upstream changed after preview", domain.ErrPlanStale)
		}
		if err := execution.refreshCheckoutPermissions(native.checkout); err != nil {
			return err
		}
		if request.authoring != nil {
			if err := cli.checkResourceConfigChange(ctx, loaded, *request.authoring); err != nil {
				return err
			}
		}
		return cli.checkPreparedConfigGit(ctx, native.checkout, native.expectedHead, native.remote, native.remoteURL, native.expectedRemote)
	}
	execution.run = func(ctx context.Context, copyCLI *CLI, adapterRequest domain.AdapterRequest) (configsync.Plan, error) {
		unlock := func() {}
		if request.authoring != nil && request.authoring.scope == config.ScopeYard {
			var err error
			unlock, err = lockIntegrationYard(ctx, loaded)
			if err != nil {
				return configsync.Plan{}, err
			}
		}
		defer unlock()
		if request.authoring != nil {
			if err := cli.checkResourceConfigChange(ctx, loaded, *request.authoring); err != nil {
				return configsync.Plan{}, err
			}
		}
		native.preview = execution.current
		adapter := &configSyncPushAdapter{cli: copyCLI, prepared: native}
		adapterRequest.Adapter, adapterRequest.Action = "config-sync", "push-prepare"
		_, _, err := adapter.Run(ctx, adapterRequest, nil)
		if err != nil {
			return adapter.plan, err
		}
		if err := adapter.plan.VerifyPublished(); err != nil {
			return adapter.plan, err
		}
		if err := execution.consumers.apply(ctx, copyCLI, loaded, adapter.plan, copyCLI.options.Stdout); err != nil {
			return adapter.plan, err
		}
		if native.pushRequired {
			head, err := copyCLI.configGitOutput(ctx, native.checkout, "rev-parse", "--verify", "HEAD")
			if err != nil || strings.TrimSpace(head) != adapter.plan.SourceCommit {
				return adapter.plan, fmt.Errorf("%w: checkout changed before push", domain.ErrPlanStale)
			}
			if err := copyCLI.checkConfigGitPushTarget(ctx, native.checkout, native.remote, native.remoteURL); err != nil {
				return adapter.plan, err
			}
			if err := copyCLI.configGitRun(ctx, native.checkout, "push", "--porcelain", "--", native.remote, adapter.plan.SourceCommit+":refs/heads/"+native.remoteBranch); err != nil {
				return adapter.plan, fmt.Errorf("push failed without force: %w; local checkout remains ahead and can be retried", err)
			}
		}
		execution.consumersApplied = true
		fmt.Fprintf(copyCLI.options.Stdout, "config sync push: pushed %s\n", adapter.plan.SourceCommit)
		return adapter.plan, nil
	}
	if request.materialize {
		if err := prepared.prepareConfigSyncConsumers(ctx, execution); err != nil {
			return err
		}
	}
	prepared.attachConfigSync(execution)
	return nil
}

func (cli *CLI) configRegistrationGuard(loaded config.Loaded) (func(context.Context) error, error) {
	record, exists, err := configsync.ReadSourceRecord(loaded.Context.Paths.ConfigHome)
	if err != nil {
		return nil, err
	}
	binding := operationStateDigest(struct {
		Record any
		Exists bool
	}{record, exists})
	return func(context.Context) error {
		current, present, err := configsync.ReadSourceRecord(loaded.Context.Paths.ConfigHome)
		if err != nil {
			return err
		}
		if binding != operationStateDigest(struct {
			Record any
			Exists bool
		}{current, present}) {
			return fmt.Errorf("%w: configuration source registration changed after preview", domain.ErrPlanStale)
		}
		return nil
	}, nil
}

func (prepared *preparedCommand) prepareConfigConnect(ctx context.Context, arguments []string) error {
	cli, loaded := prepared.CLI, prepared.Loaded
	request, _, err := cli.parseConfigSourceConnect(arguments)
	if err != nil {
		return fmt.Errorf("%w: %v", errConfigUsage, err)
	}
	native, err := cli.prepareConfigSource(ctx, loaded, request)
	if err != nil {
		return err
	}
	prepared.closeResource = func() error { native.cleanup(); return nil }
	execution := &configSyncExecution{approved: native.preview, action: "config.sync.connect", metadata: []domain.OperationStep{
		configSyncMetadata("checkout", "owner configuration checkout", "fingerprint:"+operationStateDigest(native.checkout), "fingerprint:"+operationStateDigest(struct{ Checkout, Commit string }{native.checkout, native.preview.SourceCommit}), "install or initialize the prepared private Git checkout", native.needsClone || native.initialCandidate != ""),
		configSyncMetadata("registration", "owner configuration source registration", "captured source registration", "fingerprint:"+operationStateDigest(struct{ Origin, Checkout string }{native.origin, native.checkout}), "register the approved owner configuration source", native.needsRegister),
		configSyncMetadata("upstream", "configuration source upstream", "native upstream baseline", "fingerprint:"+operationStateDigest(native.initialCommit), "push the initial approved configuration commit without force", native.initialPush),
	}}
	guard, err := cli.configRegistrationGuard(loaded)
	if err != nil {
		return err
	}
	initialState := operationStateDigest(cli.inspectConfigGit(ctx, native.checkout))
	execution.guard = func(ctx context.Context) error {
		if err := guard(ctx); err != nil {
			return err
		}
		if native.needsClone {
			if _, err := os.Lstat(native.checkout); !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("%w: configuration checkout appeared after preview", domain.ErrPlanStale)
			}
		} else if initialState != operationStateDigest(cli.inspectConfigGit(ctx, native.checkout)) {
			return fmt.Errorf("%w: configuration checkout changed after preview", domain.ErrPlanStale)
		}
		return nil
	}
	execution.run = func(ctx context.Context, copyCLI *CLI, request domain.AdapterRequest) (configsync.Plan, error) {
		native.preview = execution.current
		adapter := &configSourceConnectAdapter{cli: copyCLI, prepared: native}
		request.Adapter, request.Action = "config-source", "connect"
		_, _, err := adapter.Run(ctx, request, nil)
		if err == nil {
			fmt.Fprintf(copyCLI.options.Stdout, "config sync: connected %s\n", native.checkout)
		}
		return adapter.plan, err
	}
	if request.materialize {
		if err := prepared.prepareConfigSyncConsumers(ctx, execution); err != nil {
			return err
		}
	}
	prepared.attachConfigSync(execution)
	return nil
}

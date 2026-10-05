package cli

import (
	"context"
	"fmt"
	"io"
	"slices"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
)

type configSyncConsumers struct {
	execution     *configApplyExecution
	approvedSteps []domain.OperationStep
	all           bool
	names         map[string]bool
	conditional   bool
}

func configSyncConsumerScope(plan configsync.Plan) (bool, map[string]bool) {
	all, names := false, map[string]bool{}
	for _, change := range plan.Changes {
		if !slices.Contains(change.Applications, config.SettingConfigApply) {
			continue
		}
		if name, scoped := configSyncPathYard(change.Path); scoped {
			names[name] = true
		} else {
			all = true
		}
	}
	return all, names
}
func (consumers *configSyncConsumers) targets(plan configsync.Plan) ([]configTarget, error) {
	contexts, err := plan.CandidateConfigs()
	if err != nil {
		return nil, err
	}
	var targets []configTarget
	for _, loaded := range contexts {
		name := loaded.Context.YardName
		if (consumers.all || consumers.names[name]) && loaded.Context.AccessKind != domain.AccessRemote {
			targets = append(targets, configTarget{Name: name, Loaded: loaded})
		}
	}
	return targets, nil
}
func (prepared *preparedCommand) prepareConfigSyncConsumers(ctx context.Context, execution *configSyncExecution) error {
	all, names := configSyncConsumerScope(execution.approved)
	if !all && len(names) == 0 {
		return nil
	}
	consumers := &configSyncConsumers{all: all, names: names, conditional: execution.approved.NeedsApply()}
	targets, err := consumers.targets(execution.approved)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return nil
	}
	native, err := prepared.CLI.prepareConfigApply(ctx, targets, func() ([]configTarget, error) { return consumers.targets(execution.current) })
	if err != nil {
		return err
	}
	consumers.execution = native
	consumers.approvedSteps = native.steps()
	execution.consumers = consumers
	return nil
}
func (consumers *configSyncConsumers) steps() []domain.OperationStep {
	if consumers == nil {
		return nil
	}
	steps := consumers.execution.steps()
	for index := range steps {
		if consumers.conditional && steps[index].Decision == domain.StepApply {
			steps[index].Decision = domain.StepConditional
		}
		steps[index].DependsOn = []string{"config.sync.manifest"}
		steps[index].Preconditions = append(steps[index].Preconditions, "publish only the captured native configuration candidate before refreshing this target")
	}
	return steps
}
func (consumers *configSyncConsumers) refresh(ctx context.Context, cli *CLI) error {
	if consumers == nil {
		return nil
	}
	return cli.refreshConfigApply(ctx, consumers.execution)
}
func (consumers *configSyncConsumers) apply(ctx context.Context, cli *CLI, loaded config.Loaded, applied configsync.Plan, output io.Writer) error {
	if consumers == nil {
		return nil
	}
	// The native source publication can create a config-only release transition.
	// Reuse its protected repair admission without a second confirmation.
	names := make([]string, 0, len(consumers.execution.approved))
	for _, assessment := range consumers.execution.approved {
		names = append(names, assessment.Target.Name)
	}
	if !cli.releaseTransitionChild {
		outcome, err := cli.inspectMutationGate(ctx, loaded.Context.YardName)
		if err != nil {
			return err
		}
		if outcome != nil {
			permit, err := cli.prepareConfigApplyRepairMode(ctx, loaded.Context.YardName, false, *outcome, true, names)
			if err != nil {
				return err
			}
			if permit == nil {
				return fmt.Errorf("materialized configuration refresh blocked by release transition: %s; %s", outcome.Code, outcome.Retry)
			}
			previous := cli.configApplyRepair
			cli.configApplyRepair = permit
			defer func() { cli.configApplyRepair = previous }()
		}
	}
	consumers.execution.selector = func() ([]configTarget, error) {
		candidateTargets, err := consumers.targets(applied)
		if err != nil {
			return nil, err
		}
		if !sameConfigTargetSet(consumers.execution.approved, candidateTargets) {
			return nil, fmt.Errorf("%w: affected configuration yard set changed", domain.ErrPlanStale)
		}
		var targets []configTarget
		for _, captured := range candidateTargets {
			loaded, err := cli.loadInventoryLoaded(captured.Name, loaded)
			if err != nil {
				return nil, err
			}
			targets = append(targets, configTarget{Name: captured.Name, Loaded: loaded})
		}
		return targets, nil
	}
	return cli.executeConfigApply(ctx, consumers.execution, consumers.approvedSteps, output)
}

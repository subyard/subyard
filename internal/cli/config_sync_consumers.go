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
	execution   configApplyExecution
	all         bool
	names       map[string]bool
	conditional bool
	discovery   []error
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
func (consumers *configSyncConsumers) observe(ctx context.Context, cli *CLI, plan configsync.Plan, phase string) ([]configTargetAssessment, error) {
	contexts, err := plan.CandidateConfigs()
	if err != nil {
		return nil, err
	}
	var targets []configTarget
	for _, loaded := range contexts {
		name := loaded.Context.YardName
		if loaded.Context.AccessKind != domain.AccessRemote {
			targets = append(targets, configTarget{Name: name, Loaded: loaded})
		}
	}
	required := func(name string) bool {
		return consumers.all || consumers.names[name] || slices.ContainsFunc(consumers.execution.approved, func(item configTargetAssessment) bool {
			return item.Target.Name == name
		})
	}
	observed, discovery, err := cli.assessConfigTargets(ctx, targets, phase, required)
	consumers.discovery = append(consumers.discovery, discovery...)
	if err != nil {
		return nil, err
	}
	// Settings and their consumers can converge independently across syncs.
	// A yard outside this publication's scope joins only on observed drift.
	return slices.DeleteFunc(observed, func(item configTargetAssessment) bool {
		return !item.Changed && !required(item.Target.Name)
	}), nil
}

func (prepared *preparedCommand) prepareConfigSyncConsumers(ctx context.Context, execution *configSyncExecution) error {
	all, names := configSyncConsumerScope(execution.approved)
	consumers := &configSyncConsumers{all: all, names: names, conditional: execution.approved.NeedsApply()}
	approved, err := consumers.observe(ctx, prepared.CLI, execution.approved, "inspect")
	if err != nil {
		return domain.CollectRefusals(err, consumers.incomplete())
	}
	consumers.execution.approved = approved
	consumers.execution.current = slices.Clone(approved)
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
func (consumers *configSyncConsumers) refresh(ctx context.Context, cli *CLI, plan configsync.Plan) error {
	if consumers == nil {
		return nil
	}
	observed, err := consumers.observe(ctx, cli, plan, "verify")
	if err != nil {
		return err
	}
	return consumers.execution.accept(observed)
}

func (consumers *configSyncConsumers) incomplete() error {
	if consumers == nil || len(consumers.discovery) == 0 {
		return nil
	}
	return fmt.Errorf("configuration refresh is incomplete: %w", domain.CollectRefusals(consumers.discovery...))
}

func (consumers *configSyncConsumers) apply(ctx context.Context, cli *CLI, loaded config.Loaded, output io.Writer) error {
	if consumers == nil || len(consumers.execution.approved) == 0 {
		return nil
	}
	// Use published configuration for verification. The observation already
	// selected the writes, so only release admission and desired inputs can
	// refuse them here.
	var names []string
	var refusals []error
	for i, assessment := range consumers.execution.current {
		if !assessment.Changed {
			continue
		}
		name := assessment.Target.Name
		published, err := cli.loadInventoryLoaded(name, loaded)
		if err != nil {
			refusals = append(refusals, err)
			continue
		}
		target := configTarget{Name: name, Loaded: published}
		desired, _, _, err := materializationDesired(target, true, nil)
		if err != nil {
			refusals = append(refusals, err)
			continue
		}
		// A stopped assessment fingerprints the same desired inputs without
		// requiring any guest observations.
		facts, err := config.AssessMaterialization(desired, "stopped", true, nil)
		if err != nil {
			refusals = append(refusals, err)
			continue
		}
		if facts.DesiredFingerprint != assessment.DesiredFingerprint {
			refusals = append(refusals, fmt.Errorf("%w: yard %s: published desired configuration differs from the approved candidate", domain.ErrPlanStale, name))
			continue
		}
		consumers.execution.current[i].Target = target
		names = append(names, name)
	}
	if err := domain.CollectRefusals(refusals...); err != nil {
		return err
	}
	finish := func() error { return nil }
	if len(names) != 0 && !cli.releaseTransitionChild {
		var err error
		consumers.execution.inProcess, finish, err = cli.consumerRefreshAdmission(ctx, loaded.Context.YardName, names)
		if err != nil {
			return err
		}
	}
	if err := cli.applyObservedConfig(ctx, &consumers.execution, output); err != nil {
		return err
	}
	return finish()
}

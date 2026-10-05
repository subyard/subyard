package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

var errConfigUsage = errors.New("invalid config invocation")

// Internal activation and protected authoring already own their native capture;
// they do not discover a new public command in the active child registry.
func nativeConfigCommandDefinition() command.Definition {
	return command.Definition{Name: "config", Handler: "@config", Remote: command.RemoteLocal,
		Effect: command.EffectMutate, Confirmation: command.ConfirmationDynamic, Summary: "Manage configuration"}
}

func configArguments(arguments []string) []string {
	if len(arguments) > 0 && (arguments[0] == "--yes" || arguments[0] == "-y") {
		return arguments[1:]
	}
	return arguments
}

// Protected drafts and sensitive scalar values stay on the dedicated local
// authoring path. Exact planning never ingests their payloads through RPC.
func configExactInvocation(loaded config.Loaded, arguments []string) bool {
	arguments = configArguments(arguments)
	if len(arguments) == 0 || commandHelpRequested(arguments) {
		return false
	}
	switch arguments[0] {
	case "apply":
		ok, _ := configApplyInvocation(arguments)
		return ok
	case "set", "unset":
		request, err := parseConfigAuthoringRequest(arguments[0], arguments[1:], false)
		if err != nil {
			return false
		}
		definition, err := loaded.Catalog.ValidateSettingName(request.scope, request.name, false)
		return err == nil && definition.Kind == config.SettingScalar && !definition.Sensitive
	case "sync":
		_, check, _ := configSyncInvocation(arguments)
		if check || (len(arguments) > 1 && (arguments[1] == "status" || arguments[1] == "path" || arguments[1] == "help")) {
			return false
		}
		return true
	default:
		return false
	}
}

func (prepared *preparedCommand) prepareConfig(ctx context.Context, _ *initBootstrap) error {
	arguments := configArguments(prepared.Arguments)
	if !configExactInvocation(prepared.Loaded, arguments) {
		return fmt.Errorf("%w: configuration invocation requires its dedicated transport", errConfigUsage)
	}
	switch arguments[0] {
	case "set", "unset":
		return prepared.prepareConfigScalar(ctx, arguments[0], arguments[1:])
	case "apply":
		_, allLocal := configApplyInvocation(arguments)
		targets, err := prepared.CLI.localConfigTargets(prepared.Loaded, allLocal)
		if err != nil {
			return err
		}
		execution, err := prepared.CLI.prepareConfigApply(ctx, targets, func() ([]configTarget, error) {
			return prepared.CLI.refreshLocalConfigTargets(prepared.Loaded, allLocal)
		})
		if err != nil {
			return err
		}
		prepared.attachConfigApply(execution)
		return nil
	default:
		return prepared.prepareConfigSync(ctx, arguments[1:])
	}
}

func (prepared *preparedCommand) prepareConfigScalar(ctx context.Context, action string, arguments []string, protected ...bool) error {
	cli, loaded := prepared.CLI, prepared.Loaded
	request, err := parseConfigAuthoringRequest(action, arguments, false)
	if err != nil {
		return fmt.Errorf("%w: %v", errConfigUsage, err)
	}
	definition, err := loaded.Catalog.ValidateSettingName(request.scope, request.name, false)
	if err != nil || definition.Kind != config.SettingScalar || (definition.Sensitive && (len(protected) == 0 || !protected[0])) {
		return fmt.Errorf("%w: only non-sensitive scalar settings support exact authoring", errConfigUsage)
	}
	if action == "set" {
		if err := loaded.Catalog.ValidateSetting(request.scope, request.name, request.value, request.git); err != nil {
			return fmt.Errorf("%w: %v", errConfigUsage, err)
		}
	}
	if request.git {
		if !definition.Syncable {
			return fmt.Errorf("%w: %s cannot be saved in Git", errConfigUsage, request.name)
		}
		return prepared.prepareConfigPush(ctx, configSyncPushOptions{message: fmt.Sprintf("%s %s in %s settings", action, request.name, request.scope), authoring: &request})
	}
	if err := config.CheckLocalSettingsWritable(loaded.Context.Paths.ConfigHome); err != nil {
		return err
	}
	if action == "set" {
		if err := loaded.Catalog.ValidateSetting(request.scope, request.name, request.value, false); err != nil {
			return fmt.Errorf("%w: %v", errConfigUsage, err)
		}
	}
	if action == "set" && request.name == "CODING_TOOL_INTEGRATIONS" {
		requested := strings.Fields(request.value)
		if _, err := config.ResolveIntegrationSelection(loaded.Environment, requested); err != nil {
			return fmt.Errorf("%w: %v", errConfigUsage, err)
		}
		if request.scope == config.ScopeYard && !loaded.Integrations.AllowsCodingTools && len(requested) != 0 {
			return fmt.Errorf("%w: yard role forbids non-empty CODING_TOOL_INTEGRATIONS", errConfigUsage)
		}
	}
	if request.scope == config.ScopeYard && request.name == "YARD_TEMPLATE" {
		if err := config.ValidateYardTemplateIntegrations(loaded, request.value); err != nil {
			return fmt.Errorf("%w: %v", errConfigUsage, err)
		}
	}
	path, err := configScalarAuthoringPath(loaded, request.scope)
	if err != nil {
		return fmt.Errorf("%w: %v", errConfigUsage, err)
	}
	baseline, err := readConfigAuthoringTarget(path)
	if err != nil {
		return err
	}
	var value *string
	if action == "set" {
		value = &request.value
	}
	candidate, err := config.EditPersistentAssignmentContent(path, baseline.Content, request.name, value)
	if err != nil {
		return err
	}
	desired := config.PersistentFileSnapshot{Exists: true, Content: candidate}
	if action == "unset" && len(candidate) == 0 {
		desired = config.PersistentFileSnapshot{}
	}
	changed := !sameConfigAuthoringSnapshot(baseline, desired)
	if changed {
		if err := cli.checkResourceConfigChange(ctx, loaded, request); err != nil {
			return err
		}
	}
	step := domain.OperationStep{ID: "config." + action, Target: string(request.scope) + " setting " + request.name,
		Observed: "fingerprint:" + operationStateDigest(baseline), Desired: "fingerprint:" + operationStateDigest(desired),
		Decision: domain.StepSkip, Preconditions: []string{"persistent setting scope, permissions and ownership are valid", "the authored target matches the captured baseline or desired content"},
		Verify: "read the persistent target and compare the approved content fingerprint", Consequence: action + " " + request.name + " in persistent " + string(request.scope) + " settings"}
	if changed {
		step.Decision = domain.StepApply
	}
	prepared.exactState = operationStateDigest(struct {
		Action, Name, Value, Scope, Path string
		Before, Desired                  config.PersistentFileSnapshot
	}{action, request.name, request.value, string(request.scope), path, baseline, desired})
	prepared.assess = func(context.Context) (domain.ActionID, domain.ActionDelta, error) {
		return domain.ActionID("config." + action), domain.ActionDelta{Changed: changed, Consequences: []string{step.Consequence}}, nil
	}
	prepared.stepsComplete = true
	prepared.steps = func() []domain.OperationStep { return []domain.OperationStep{step} }
	prepared.executeNoOp = true
	prepared.execute = func(ctx context.Context, _ *application.Orchestrator, output io.Writer) (domain.AdapterResult, error) {
		unlock := func() {}
		if request.scope == config.ScopeYard {
			var err error
			unlock, err = lockIntegrationYard(ctx, loaded)
			if err != nil {
				return domain.AdapterResult{}, err
			}
		}
		defer unlock()
		if err := config.CheckLocalSettingsWritable(loaded.Context.Paths.ConfigHome); err != nil {
			return domain.AdapterResult{}, err
		}
		current, err := readConfigAuthoringTarget(path)
		if err != nil {
			return domain.AdapterResult{}, err
		}
		if sameConfigAuthoringSnapshot(current, desired) {
			fmt.Fprintf(output, "config %s: already current\n", action)
			return configPreparedResult(prepared), nil
		}
		if !changed || !sameConfigAuthoringSnapshot(current, baseline) {
			return domain.AdapterResult{}, fmt.Errorf("%w: persistent configuration changed after confirmation", domain.ErrPlanStale)
		}
		if err := cli.checkResourceConfigChange(ctx, loaded, request); err != nil {
			return domain.AdapterResult{}, err
		}
		if request.scope == config.ScopeYard && request.name == "YARD_TEMPLATE" {
			if err := config.ValidateYardTemplateIntegrations(loaded, request.value); err != nil {
				return domain.AdapterResult{}, err
			}
		}
		if err := config.WritePersistentAssignmentIfUnchanged(loaded.Context.Paths.ConfigHome, path, request.name, value, baseline); err != nil {
			if errors.Is(err, config.ErrPersistentTargetStale) {
				return domain.AdapterResult{}, fmt.Errorf("%w: %w", domain.ErrPlanStale, err)
			}
			return domain.AdapterResult{}, err
		}
		current, err = readConfigAuthoringTarget(path)
		if err != nil {
			return domain.AdapterResult{}, err
		}
		if !sameConfigAuthoringSnapshot(current, desired) {
			return domain.AdapterResult{}, errors.New("persistent configuration verification failed")
		}
		fmt.Fprintf(output, "config %s: updated %s\n", action, path)
		return configPreparedResult(prepared), nil
	}
	return nil
}

func configPreparedResult(prepared *preparedCommand) domain.AdapterResult {
	return domain.AdapterResult{Schema: 1, OperationID: prepared.Plan.OperationID, Status: "ok"}
}

type configApplyExecution struct {
	approved []configTargetAssessment
	current  []configTargetAssessment
	selector configTargetSelector
}

func (cli *CLI) prepareConfigApply(ctx context.Context, targets []configTarget, selector configTargetSelector) (*configApplyExecution, error) {
	if len(targets) == 0 {
		return nil, errors.New("config apply: no local yards selected")
	}
	if err := validateManagedConfigTree(targets[0].Loaded.Context.Paths.ConfigHome); err != nil {
		return nil, err
	}
	execution := &configApplyExecution{selector: selector}
	for _, target := range targets {
		if target.Loaded.Context.AccessKind == domain.AccessRemote {
			return nil, fmt.Errorf("config apply does not implicitly operate on remote yard %s", target.Name)
		}
		assessment, err := cli.assessConfigTarget(ctx, target, true)
		if err != nil {
			return nil, fmt.Errorf("yard %s: %w", target.Name, err)
		}
		execution.approved = append(execution.approved, assessment)
	}
	if cli.configApplyRepair != nil && !cli.configApplyRepair.matchesRequestedConfigs(execution.approved) {
		return nil, errors.New("config apply: release repair requires the persisted configuration; remove differing command overrides")
	}
	execution.current = slices.Clone(execution.approved)
	return execution, nil
}

func (execution *configApplyExecution) changed() bool {
	return slices.ContainsFunc(execution.current, func(item configTargetAssessment) bool { return item.Changed })
}

func (execution *configApplyExecution) steps() []domain.OperationStep {
	var steps []domain.OperationStep
	for _, assessment := range execution.current {
		decision := domain.StepSkip
		if assessment.Changed {
			decision = domain.StepApply
		}
		steps = append(steps, domain.OperationStep{ID: "config.apply." + assessment.Target.Name, Target: "local yard " + assessment.Target.Name,
			Observed: assessment.State + "; fingerprint:" + assessment.MaterializedFingerprint, Desired: "fingerprint:" + assessment.DesiredFingerprint,
			Decision: decision, Preconditions: []string{"the selected local target set and desired configuration are unchanged", "the yard is running and the captured materialized baseline matches, or its approved drift has converged"},
			Verify: "check native materialized file settings against the approved desired fingerprint", Consequence: "refresh materialized agent configs in local running yard " + assessment.Target.Name})
	}
	return steps
}

func (cli *CLI) refreshConfigApply(ctx context.Context, execution *configApplyExecution) error {
	if execution.selector == nil {
		return errors.New("config apply: target selector is required")
	}
	targets, err := execution.selector()
	if err != nil {
		return fmt.Errorf("revalidate targets: %w", err)
	}
	if !sameConfigTargetSet(execution.approved, targets) {
		return fmt.Errorf("%w: selected local yard set changed after confirmation", domain.ErrPlanStale)
	}
	if err := validateManagedConfigTree(targets[0].Loaded.Context.Paths.ConfigHome); err != nil {
		return err
	}
	byName := make(map[string]configTargetAssessment, len(targets))
	for _, target := range targets {
		assessment, err := cli.assessConfigTarget(ctx, target, true)
		if err != nil {
			return fmt.Errorf("revalidate yard %s: %w", target.Name, err)
		}
		byName[target.Name] = assessment
	}
	var current []configTargetAssessment
	for _, initial := range execution.approved {
		fresh := byName[initial.Target.Name]
		if initial.DesiredFingerprint != fresh.DesiredFingerprint {
			return fmt.Errorf("%w: yard %s: desired configuration or target changed after confirmation", domain.ErrPlanStale, initial.Target.Name)
		}
		if !(initial.Changed && !fresh.Changed) && (initial.MaterializedFingerprint != fresh.MaterializedFingerprint || initial.Changed != fresh.Changed) {
			return fmt.Errorf("%w: yard %s: materialized configuration changed after confirmation", domain.ErrPlanStale, initial.Target.Name)
		}
		current = append(current, fresh)
	}
	execution.current = current
	return nil
}

func (prepared *preparedCommand) attachConfigApply(execution *configApplyExecution) {
	cli := prepared.CLI
	prepared.exactState = operationStateDigest(execution.approved)
	consequences := domain.OperationStepConsequences(execution.steps())
	if len(consequences) == 0 {
		consequences = []string{"no running local yards have materialized configuration drift"}
	}
	prepared.assess = func(context.Context) (domain.ActionID, domain.ActionDelta, error) {
		return "config.apply", domain.ActionDelta{Changed: execution.changed(), Consequences: consequences}, nil
	}
	prepared.stepsComplete = true
	prepared.steps = execution.steps
	prepared.executeNoOp = true

	prepared.execute = func(ctx context.Context, _ *application.Orchestrator, output io.Writer) (domain.AdapterResult, error) {
		if err := cli.executeConfigApply(ctx, execution, prepared.Plan.Steps, output); err != nil {
			return domain.AdapterResult{}, err
		}
		return configPreparedResult(prepared), nil
	}
}

func (cli *CLI) executeConfigApply(ctx context.Context, execution *configApplyExecution, approvedSteps []domain.OperationStep, output io.Writer) error {
	copyCLI := *cli
	copyCLI.options.Stdout, copyCLI.options.Stderr = output, output
	if cli.configApplyRepair != nil {
		unlock, err := cli.lockConfigApplyRepair(ctx, cli.configApplyRepair)
		if err != nil {
			return err
		}
		defer unlock()
	}
	if err := cli.refreshConfigApply(ctx, execution); err != nil {
		return err
	}
	if err := domain.CheckOperationSteps(approvedSteps, execution.steps()); err != nil {
		return err
	}
	var targets []configTarget
	for index, assessment := range execution.current {
		if assessment.Changed {
			targets = append(targets, assessment.Target)
		} else if execution.approved[index].Changed {
			fmt.Fprintf(output, "yard %s materialized-config: %s after confirmation; skipped\n", assessment.Target.Name, assessment.State)
		}
	}
	applier := cli.options.Config
	if applier == nil && cli.configApplyRepair != nil {
		applier = releaseTransitionConfigApplier{cli: &copyCLI}
	}
	if applier == nil {
		applier = dispatcherConfigApplier{path: cli.options.DispatcherPath, environment: cli.baseEnv, stdout: output, stderr: output}
	}
	for _, target := range targets {
		if err := applier.ApplyConfig(ctx, target.Name); err != nil {
			return fmt.Errorf("yard %s: %w", target.Name, err)
		}
	}
	if len(targets) != 0 {
		expected := make(map[string]string, len(execution.approved))
		for _, target := range execution.approved {
			expected[target.Target.Name] = target.DesiredFingerprint
		}
		if err := copyCLI.configStatus(ctx, targets, true, expected); err != nil {
			return fmt.Errorf("config apply verification: %w", err)
		}
	} else {
		if slices.ContainsFunc(execution.approved, func(item configTargetAssessment) bool { return item.Changed }) {
			fmt.Fprintln(output, "config apply: drift converged after confirmation; nothing to refresh")
		} else {
			fmt.Fprintln(output, "config apply: no running local yards to refresh")
		}
	}
	if cli.configApplyRepair != nil {
		return cli.finishConfigApplyRepair(ctx, cli.configApplyRepair)
	}
	return nil
}

// Protected drafts are captured on the dedicated owner-local authoring path.
// Only their public metadata joins the same consent and execution boundary.
func (cli *CLI) runPreparedConfigMutation(ctx context.Context, loaded config.Loaded, arguments []string, assumeYes bool, capture func(*preparedCommand) error) int {
	definition := nativeConfigCommandDefinition()
	prepared := &preparedCommand{CLI: cli, Definition: definition, Loaded: loaded, Arguments: arguments}
	prepared.policy = commandPolicy(definition, loaded.Context, arguments, nil)
	defer prepared.Close()
	if err := capture(prepared); err != nil {
		return cli.reportPreparationError(definition, err)
	}
	if err := prepared.preparePlan(ctx); err != nil {
		return cli.reportPreparationError(definition, err)
	}
	return cli.runPreparedCommand(ctx, prepared, assumeYes || cli.env["ASSUME_YES"] == "1")
}

func (prepared *preparedCommand) attachConfigFileDraft(request configAuthoringRequest, path string, baseline config.PersistentFileSnapshot, content []byte) {
	loaded := prepared.Loaded
	desired := config.PersistentFileSnapshot{Exists: true, Content: slices.Clone(content)}
	changed := !sameConfigAuthoringSnapshot(baseline, desired)
	step := domain.OperationStep{ID: "config." + request.action, Target: string(request.scope) + " file setting " + request.name,
		Observed: "fingerprint:" + operationStateDigest(baseline), Desired: "fingerprint:" + operationStateDigest(desired), Decision: domain.StepSkip,
		Preconditions: []string{"the retained owner-local draft is validated", "persistent setting scope, permissions, ownership and captured target remain valid"},
		Verify:        "read persistent file and compare its approved draft fingerprint", Consequence: fmt.Sprintf("replace persistent %s file setting %s", request.scope, request.name)}
	if changed {
		step.Decision = domain.StepApply
	}
	prepared.exactState = operationStateDigest(struct {
		Path, Action, Name, Scope string
		Before, Desired           config.PersistentFileSnapshot
	}{path, request.action, request.name, string(request.scope), baseline, desired})
	prepared.assess = func(context.Context) (domain.ActionID, domain.ActionDelta, error) {
		return domain.ActionID("config." + request.action), domain.ActionDelta{Changed: changed, Consequences: []string{step.Consequence}}, nil
	}
	prepared.stepsComplete = true
	prepared.steps = func() []domain.OperationStep { return []domain.OperationStep{step} }
	prepared.executeNoOp = true
	prepared.execute = func(ctx context.Context, _ *application.Orchestrator, output io.Writer) (domain.AdapterResult, error) {
		unlock := func() {}
		if request.scope == config.ScopeYard {
			var err error
			unlock, err = lockIntegrationYard(ctx, loaded)
			if err != nil {
				return domain.AdapterResult{}, err
			}
		}
		defer unlock()
		if err := config.CheckLocalSettingsWritable(loaded.Context.Paths.ConfigHome); err != nil {
			return domain.AdapterResult{}, err
		}
		current, err := readConfigAuthoringTarget(path)
		if err != nil {
			return domain.AdapterResult{}, err
		}
		if sameConfigAuthoringSnapshot(current, desired) {
			fmt.Fprintf(output, "config %s: already current\n", request.action)
			return configPreparedResult(prepared), nil
		}
		if !changed || !sameConfigAuthoringSnapshot(current, baseline) {
			return domain.AdapterResult{}, fmt.Errorf("%w: persistent configuration changed after confirmation", domain.ErrPlanStale)
		}
		if err := config.WritePersistentFileIfUnchanged(loaded.Context.Paths.ConfigHome, path, baseline, desired.Content); err != nil {
			if errors.Is(err, config.ErrPersistentTargetStale) {
				return domain.AdapterResult{}, fmt.Errorf("%w: %w", domain.ErrPlanStale, err)
			}
			return domain.AdapterResult{}, err
		}
		current, err = readConfigAuthoringTarget(path)
		if err != nil {
			return domain.AdapterResult{}, err
		}
		if !sameConfigAuthoringSnapshot(current, desired) {
			return domain.AdapterResult{}, errors.New("persistent file configuration verification failed")
		}
		fmt.Fprintf(output, "config %s: updated %s\n", request.action, path)
		return configPreparedResult(prepared), nil
	}
}

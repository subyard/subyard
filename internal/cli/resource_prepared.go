package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/resource"
)

func resourceCommandDefinition(definition resource.Definition) command.Definition {
	return command.Definition{Name: definition.Command, Handler: "@resource", Remote: command.RemoteForward,
		Effect: command.EffectMutate, Confirmation: command.ConfirmationDynamic, Visibility: command.VisibilityPublic,
		Summary: definition.Title, Verbs: slices.Clone(definition.Verbs)}
}

// Exact resource execution accepts ordinary mutations. Session, credential lease
// and external pairing workflows retain their dedicated transports.
func (cli *CLI) resourceExactInvocation(definition resource.Definition, arguments []string) bool {
	invocation, err := parseResourceInvocation(arguments)
	if err != nil || invocation.help {
		return false
	}
	found := false
	for _, metadata := range cli.resources.ActionDefinitions() {
		local, ok := localResourceAction(definition, metadata.Action)
		if !ok {
			continue
		}
		if action, ok := cli.resources.LookupAction(definition.Command, invocation.verb, local); !ok || action != metadata.Action {
			continue
		}
		found = true
		if (metadata.Effect != domain.ActionMutation && metadata.Effect != domain.ActionDestruction) ||
			slices.Contains(metadata.Impacts, domain.ImpactExternalSystem) {
			return false
		}
	}
	return found
}

// resourceExecution owns the retained native handler facts alongside the
// existing bootstrap, ingress and desired-startup transactions.
type resourceExecution struct {
	runner        *resourceApplyRunner
	baseline      config.Loaded
	approved      resource.PrepareResult
	current       resource.PrepareResult
	approvedSteps []domain.OperationStep
	consequences  []string
	handlerDigest string
	configDigest  string
}

func resourceSettingsDigest(loaded config.Loaded) string {
	settings := make(map[string]string, len(loaded.Settings))
	for name, trace := range loaded.Settings {
		settings[name] = trace.EffectiveValue
	}
	return operationStateDigest(struct {
		Context  domain.Context
		Settings map[string]string
	}{loaded.Context, settings})
}

func (prepared *preparedCommand) prepareResourceInvocation(ctx context.Context, _ *initBootstrap) error {
	cli := prepared.CLI
	definition, ok := cli.resources.Lookup(prepared.Definition.Name)
	if !ok || !cli.resourceExactInvocation(definition, prepared.Arguments) {
		return fmt.Errorf("%w: resource invocation requires a dedicated transport", resource.ErrResourceActionUnknown)
	}
	invocation, err := parseResourceInvocation(prepared.Arguments)
	if err != nil {
		return err
	}
	baseline, loaded := prepared.Loaded, prepared.Loaded
	bootstrap, err := cli.prepareResourceBootstrap(ctx, loaded, definition, invocation.arguments)
	if err != nil {
		return err
	}
	if bootstrap != nil {
		loaded = bootstrap.loaded
	}
	output, err := cli.prepareResource(ctx, loaded, definition, invocation.arguments)
	if err != nil {
		return err
	}
	native, err := cli.resources.PrepareResult(cli.coreActions, definition.Command, invocation.verb, output)
	if err != nil {
		return err
	}
	if native.Schema != resource.PrepareAssessmentSchemaV2 {
		return fmt.Errorf("%w: exact resource mutations require a v2 native plan", resource.ErrResourcePlanInvalid)
	}
	localAction, ok := localResourceAction(definition, native.Assessment.Action)
	if !ok {
		return resource.ErrResourceActionUnknown
	}
	ingress, err := cli.prepareResourceIngress(ctx, loaded, definition, invocation.verb)
	if err != nil {
		return err
	}
	intent, err := cli.prepareResourceStartupIntent(ctx, loaded, definition, invocation.verb)
	if err != nil {
		return err
	}
	runner := &resourceApplyRunner{cli: cli, loaded: loaded, definition: definition, verb: invocation.verb,
		localAction: localAction, effect: native.Assessment.Effect, arguments: slices.Clone(invocation.arguments),
		bootstrap: bootstrap, ingress: ingress, startupIntent: intent, consequences: slices.Clone(native.Assessment.Consequences)}
	prepared.Loaded = loaded
	return prepared.attachResourceExecution(runner, baseline, native)
}

func (prepared *preparedCommand) attachResourceExecution(runner *resourceApplyRunner, baseline config.Loaded, native resource.PrepareResult) error {
	handler, err := os.ReadFile(runner.definition.HandlerPath())
	if err != nil {
		return err
	}
	execution := &resourceExecution{runner: runner, baseline: baseline, approved: native, current: native,
		handlerDigest: operationStateDigest(handler), configDigest: resourceSettingsDigest(baseline)}
	assessment := execution.assessment()
	execution.consequences = slices.Clone(assessment.Consequences)
	execution.approvedSteps = domain.CloneOperationSteps(execution.steps())
	runner.exact = execution
	prepared.policy = commandPolicy(prepared.Definition, prepared.Loaded.Context, prepared.Arguments, nil)
	prepared.policy.RemotePolicy = domain.RemoteOnOwner
	prepared.exactState = operationStateDigest(struct {
		Binding, Handler, Configuration, Bootstrap string
		Steps                                      []domain.OperationStep
	}{native.Binding, execution.handlerDigest, execution.configDigest, runner.bootstrap.stateBinding(), execution.approvedSteps})
	prepared.assess = func(context.Context) (domain.ActionID, domain.ActionDelta, error) {
		assessment := execution.assessment()
		return assessment.Action, domain.ActionDelta{Changed: assessment.Changed, Consequences: slices.Clone(execution.consequences)}, nil
	}
	prepared.stepsComplete = true
	prepared.steps = execution.steps
	// Even an initially converged resource must validate its skips under the
	// owner lock and query the native verifier before reporting success.
	prepared.executeNoOp = true
	prepared.execute = func(ctx context.Context, orchestrator *application.Orchestrator, diagnostics io.Writer) (domain.AdapterResult, error) {
		// RPC supplies its diagnostic stream here; handler output must never
		// enter framed protocol stdout. Pairing is excluded before preparation.
		operationCLI := *runner.cli
		operationCLI.options.Stdout, operationCLI.options.Stderr = diagnostics, diagnostics
		runner.cli = &operationCLI
		orchestrator.Runner = runner
		result, stderr, err := orchestrator.RunAdapter(ctx, prepared.Plan, domain.AdapterRequest{
			Schema: 1, OperationID: prepared.Plan.OperationID, Adapter: "resource", Action: runner.localAction,
			Arguments: slices.Clone(runner.arguments[1:]),
		}, nil)
		writeAdapterDiagnostics(diagnostics, stderr)
		return result, err
	}
	return nil
}

func (execution *resourceExecution) assessment() domain.ActionAssessment {
	runner := execution.runner
	assessment := runner.bootstrap.augment(execution.current.Assessment.Clone())
	assessment = runner.startupIntent.augment(assessment)
	return runner.ingress.augment(assessment)
}

func (execution *resourceExecution) steps() []domain.OperationStep {
	runner := execution.runner
	var steps []domain.OperationStep
	if bootstrap := runner.bootstrap; bootstrap != nil {
		steps = append(steps, bootstrap.operationSteps()...)
	}

	for _, step := range domain.CloneOperationSteps(execution.current.Steps) {
		step.ID = "native." + step.ID
		for index, dependency := range step.DependsOn {
			step.DependsOn[index] = "native." + dependency
		}
		if len(steps) != 0 && len(step.DependsOn) == 0 && runner.bootstrap != nil {
			step.DependsOn = []string{steps[len(steps)-1].ID}
		}
		steps = append(steps, step)
	}
	if ingress := runner.ingress; ingress != nil {
		decision := domain.StepSkip
		if ingress.preview.After.Changed {
			decision = domain.StepApply
		}
		step := domain.OperationStep{ID: "owner.ingress", Target: "yard " + ingress.preview.Target.Name + " resource ingress",
			Observed: "fingerprint:" + ingress.preview.Before.Fingerprint, Desired: "fingerprint:" + ingress.preview.After.Fingerprint,
			Decision: decision, Preconditions: []string{"owner ingress configuration and exact network snapshot match their captured baselines"},
			Verify: "check the owned route, isolation policy and guest readiness", Consequence: "converge the exact owned public UDP ingress"}
		if len(steps) != 0 {
			step.DependsOn = []string{steps[len(steps)-1].ID}
		}
		steps = append(steps, step)
	}
	if intent := runner.startupIntent; intent != nil {
		decision := domain.StepSkip
		if intent.before != intent.after {
			decision = domain.StepApply
		}
		observed := intent.before
		if observed == "" {
			observed = "absent"
		}
		step := domain.OperationStep{ID: "owner.startup-intent", Target: "yard " + intent.loaded.Context.YardName + " resource " + runner.definition.Command,
			Observed: observed, Desired: intent.after, Decision: decision,
			Preconditions: []string{"the selected dedicated VM and captured desired-startup intent are unchanged"},
			Verify:        "read the persisted desired-startup intent", Consequence: "record resource startup intent as " + intent.after}
		if len(steps) != 0 {
			step.DependsOn = []string{steps[len(steps)-1].ID}
		}
		steps = append(steps, step)
	}
	return steps
}

func (execution *resourceExecution) refresh(ctx context.Context) error {
	runner := execution.runner
	handler, err := os.ReadFile(runner.definition.HandlerPath())
	if err != nil {
		return err
	}
	if operationStateDigest(handler) != execution.handlerDigest {
		return fmt.Errorf("%w: resource handler changed after preparation", domain.ErrPlanStale)
	}
	freshLoaded, err := config.Load(config.LoadOptions{Catalog: &runner.cli.catalog, RepositoryRoot: runner.cli.options.RepositoryRoot,
		OperatorHome: execution.baseline.Context.Paths.OperatorHome, YardName: execution.baseline.Context.YardName, Environment: runner.cli.baseEnv})
	if err != nil {
		return err
	}
	if resourceSettingsDigest(freshLoaded) != execution.configDigest {
		return fmt.Errorf("%w: resource owner context or configuration changed", domain.ErrPlanStale)
	}
	if err := runner.bootstrap.refresh(ctx, runner.cli); err != nil {
		return err
	}
	if intent := runner.startupIntent; intent != nil {
		fresh, err := runner.cli.prepareResourceStartupIntent(ctx, runner.loaded, runner.definition, runner.verb)
		if err != nil {
			return err
		}
		if fresh == nil || fresh.after != intent.after || (fresh.before != intent.before && fresh.before != intent.after) {
			return fmt.Errorf("%w: desired startup intent changed", domain.ErrPlanStale)
		}
		runner.startupIntent = fresh
	}
	if err := runner.ingress.refresh(ctx, runner.cli); err != nil {
		return err
	}
	if err := execution.refreshNative(ctx); err != nil {
		return err
	}
	return domain.CheckOperationSteps(execution.approvedSteps, execution.steps())
}

func (execution *resourceExecution) refreshNative(ctx context.Context) error {
	runner := execution.runner
	approvedSteps, err := json.Marshal(execution.approved.Steps)
	if err != nil {
		return err
	}
	output, err := runner.cli.prepareResourceModeTimeout(ctx, runner.loaded, runner.definition, runner.arguments, "prepare", resourcePrepareTimeout,
		map[string]string{"SUBYARD_RESOURCE_BINDING": execution.approved.Binding, "SUBYARD_RESOURCE_STEPS": string(approvedSteps)})
	if err != nil {
		return err
	}
	fresh, err := runner.cli.resources.PrepareResult(runner.cli.coreActions, runner.definition.Command, runner.verb, output)
	if err != nil {
		return err
	}
	if fresh.Schema != resource.PrepareAssessmentSchemaV2 || fresh.Assessment.Action != execution.approved.Assessment.Action || fresh.Binding != execution.approved.Binding {
		return fmt.Errorf("%w: native resource action or target binding changed", domain.ErrPlanStale)
	}
	if err := domain.CheckOperationSteps(execution.approved.Steps, fresh.Steps); err != nil {
		return err
	}
	for _, consequence := range fresh.Assessment.Consequences {
		if !slices.Contains(execution.approved.Assessment.Consequences, consequence) {
			return fmt.Errorf("%w: native resource consequences expanded", domain.ErrPlanStale)
		}
	}
	execution.current = fresh
	return nil
}

func (execution *resourceExecution) verify(ctx context.Context) error {
	runner := execution.runner
	steps, err := json.Marshal(execution.current.Steps)
	if err != nil {
		return err
	}
	output, err := runner.cli.prepareResourceModeTimeout(ctx, runner.loaded, runner.definition, runner.arguments, "verify", resourcePrepareTimeout,
		map[string]string{"SUBYARD_RESOURCE_BINDING": execution.current.Binding, "SUBYARD_RESOURCE_STEPS": string(steps)})
	if err != nil {
		return fmt.Errorf("verify resource postcondition: %w", err)
	}
	verified, err := runner.cli.resources.PrepareResult(runner.cli.coreActions, runner.definition.Command, runner.verb, output)
	if err != nil {
		return fmt.Errorf("verify resource postcondition: %w", err)
	}
	if verified.Schema != resource.PrepareAssessmentSchemaV2 || verified.Assessment.Action != execution.approved.Assessment.Action ||
		verified.Binding != execution.approved.Binding || verified.Assessment.Changed {
		return fmt.Errorf("resource native verifier did not report the approved converged postcondition")
	}
	if err := domain.CheckOperationSteps(execution.approved.Steps, verified.Steps); err != nil {
		return fmt.Errorf("verify resource postcondition: %w", err)
	}
	for _, step := range verified.Steps {
		if step.Decision != domain.StepSkip || step.Observed != step.Desired {
			return fmt.Errorf("resource native verifier did not prove step %q converged", step.ID)
		}
	}
	return nil
}

// Startup owns the enclosing yard lock and conditional prerequisite handling.
// Attach only native postcondition verification to that already-approved path.
func attachApprovedResourceVerification(runner *resourceApplyRunner, approved, current resource.PrepareResult) error {
	if approved.Schema != resource.PrepareAssessmentSchemaV2 {
		return nil
	}
	if current.Schema != resource.PrepareAssessmentSchemaV2 || current.Assessment.Action != approved.Assessment.Action || current.Binding != approved.Binding {
		return fmt.Errorf("%w: native startup resource action or target binding changed", domain.ErrPlanStale)
	}
	if err := domain.CheckOperationSteps(approved.Steps, current.Steps); err != nil {
		return err
	}
	runner.verify = &resourceExecution{runner: runner, approved: approved, current: current}
	runner.skipHandler = !current.Assessment.Changed
	return nil
}

func (runner *resourceApplyRunner) runExactResource(ctx context.Context, request domain.AdapterRequest) (domain.AdapterResult, string, error) {
	result := domain.AdapterResult{Schema: 1, OperationID: request.OperationID, Status: "error"}
	unlock, err := lockIntegrationYard(ctx, runner.loaded)
	if err != nil {
		return result, "", err
	}
	defer func() { unlock() }()
	if err := runner.exact.refresh(ctx); err != nil {
		return result, "", err
	}
	if !runner.exact.assessment().Changed {
		if err := runner.exact.verify(ctx); err != nil {
			return result, "", err
		}
		result.Status = "ok"
		return result, "", nil
	}
	if runner.bootstrap != nil {
		// Bootstrap owns its native initialization lock and captured CAS guards.
		// Avoid holding that same lock recursively while it converges prerequisites.
		unlock()
		unlock = func() {}
		if err := runner.bootstrap.apply(ctx, runner.cli); err != nil {
			return result, "", err
		}
		unlock, err = lockIntegrationYard(ctx, runner.loaded)
		if err != nil {
			unlock = func() {}
			return result, "", err
		}
		// Intentional bootstrap publication changed configuration. Retain the
		// approved resource target, then inspect its now-readable native facts.
		if err := runner.exact.refreshNative(ctx); err != nil {
			return result, "", err
		}
	}
	if runner.ingress != nil && runner.ingress.up && runner.cli.options.NetworkPolicy == nil {
		if err := runner.cli.prepareSudoPrivileges(ctx, runner.cli.options.Stderr, runner.cli.effectiveUID(), runner.definition.Command); err != nil {
			return result, "", err
		}
	}
	// The native apply path still owns ingress rollback and startup publication.
	copy := *runner
	copy.bootstrap, copy.exact = nil, nil
	copy.verify = runner.exact
	copy.skipHandler = !runner.exact.current.Assessment.Changed
	return copy.Run(ctx, request, nil)
}

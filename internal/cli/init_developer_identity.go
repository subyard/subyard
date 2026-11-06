package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
)

// prepareDeveloperIdentityInit is a private prerequisite of the same approved
// init operation. It never creates a second confirmation or authorizes a child
// operation. Ordinary observations are discarded and rebuilt after the rename.
func (prepared *preparedCommand) prepareDeveloperIdentityInit(ctx context.Context, bootstrap *initBootstrap) (bool, error) {
	cli := prepared.CLI
	request, err := parseInitArguments(prepared.Arguments)
	if err != nil {
		return false, err
	}
	if cli.options.InitPlatform != nil || cli.releaseTransitionChild || bootstrap != nil {
		return false, nil
	}
	runtime := cli.initPlatform(prepared.Loaded, nil).(reconcileruntime.Runtime)
	if identityInspectionWithoutIncus(ctx, runtime) {
		return false, nil
	}
	markerPath := config.DeveloperIdentityMarkerPath(prepared.Loaded.Context.Paths.ConfigHome, prepared.Loaded.Context.YardName)
	if _, err := os.Lstat(prepared.Loaded.Context.Paths.ConfigHome); os.IsNotExist(err) {
		return false, nil
	}
	markerBefore, err := config.ReadPersistentFileSnapshot(prepared.Loaded.Context.Paths.ConfigHome, markerPath)
	if err != nil {
		return false, err
	}
	plain := request.mode == initReconcile && request.profile == "" && !request.adopt && !request.continueLegacy
	if !plain && !markerBefore.Exists {
		// Only plain init renames. Other modes keep their own account handling
		// and must not depend on reading the guest; a pending rename still fences them.
		return false, nil
	}
	var identity *reconcileruntime.DeveloperIdentityPlan
	if markerBefore.Exists {
		var marker config.DeveloperIdentityMarker
		if err := json.Unmarshal(markerBefore.Content, &marker); err != nil {
			return false, err
		}
		identity, err = runtime.PrepareDeveloperIdentityProof(ctx, marker.Proof, marker.Identity)
	} else {
		identity, err = runtime.PrepareDeveloperIdentity(ctx)
	}
	if err != nil {
		return false, err
	}
	if identity == nil {
		if !markerBefore.Exists {
			return false, nil
		}
		if _, err := cli.admitDeveloperIdentity(ctx, prepared.Loaded.Context.YardName, "@init", prepared.Arguments); err != nil {
			return false, err
		}
		var completed config.DeveloperIdentityMarker
		if err := json.Unmarshal(markerBefore.Content, &completed); err != nil {
			return false, err
		}
		if err := runtime.VerifyCompletedDeveloperIdentity(ctx, completed.Proof, completed.Identity); err != nil {
			return false, err
		}
		if err := prepared.prepareOrdinaryInit(ctx, bootstrap); err != nil {
			return false, err
		}
		run := prepared.execute
		prepared.execute = func(ctx context.Context, orchestrator *application.Orchestrator, diagnostics io.Writer) (domain.AdapterResult, error) {
			if err := runtime.VerifyCompletedDeveloperIdentity(ctx, completed.Proof, completed.Identity); err != nil {
				return domain.AdapterResult{}, err
			}
			if err := cli.publishAppliedIdentityHint(ctx, prepared.Loaded); err != nil {
				return domain.AdapterResult{}, err
			}
			if err := config.RemovePersistentFileSnapshot(prepared.Loaded.Context.Paths.ConfigHome, markerPath, markerBefore); err != nil {
				return domain.AdapterResult{}, err
			}
			return run(ctx, orchestrator, diagnostics)
		}
		return true, nil
	}
	if !plain {
		return false, fmt.Errorf("developer identity transition requires plain init")
	}
	peers, err := cli.powerYardContexts(prepared.Loaded)
	if err != nil {
		return false, err
	}
	inputs, err := cli.captureOwnerInputs(prepared.Loaded, "", peers)
	if err != nil {
		return false, err
	}
	store, err := openProjectStoreReadOnly(prepared.Loaded.Context.Paths.StateDir)
	if err != nil {
		return false, err
	}
	projects, err := store.List(ctx)
	if err != nil {
		return false, err
	}
	runtimes, err := runtime.DeferredIdentityRuntimes()
	if err != nil {
		return false, err
	}
	execution := &initExecution{loaded: prepared.Loaded, mode: initReconcile, powerYards: peers, runtimePlan: runtimes}
	execution.hostID, execution.hostIDPending, err = configsync.ResolveHostID(prepared.Loaded.Context.Paths.ConfigHome, prepared.Loaded.Environment)
	if err != nil {
		return false, err
	}
	execution.hookPlan, err = runtime.PrepareProjectHooks(ctx, false)
	if err != nil {
		return false, err
	}
	execution.hookPlan.CaptureOwnerProjects(projects)
	prepared.identityHookPlan = execution.hookPlan
	hookRuntime := runtime
	hookRuntime.HookPlan = execution.hookPlan
	if err := hookRuntime.PrepareProjectMetadata(ctx); err != nil {
		return false, err
	}
	for _, stage := range application.InitStages(prepared.Loaded.Context) {
		execution.plan.Steps = append(execution.plan.Steps, application.ReconcileStep{Stage: stage, Conditional: true})
	}
	if prepared.Loaded.Integrations.AllowsCodingTools {
		execution.integrationRetirements, execution.integrationWarnings, err = identity.RetiredIntegrationNotices(ctx)
		if err != nil {
			return false, err
		}
	}
	steps := execution.operationSteps()
	target := prepared.Loaded.Context.IncusProject + "/" + prepared.Loaded.Context.YardInstanceName
	steps = identityPrerequisiteSteps(steps, target, identity.Description())
	binding := identity.Binding()
	releaseBinding, err := identityReleaseBinding(prepared.Loaded.Context.Paths.ConfigHome)
	if err != nil {
		return false, err
	}
	marker := config.DeveloperIdentityMarker{Schema: 1, Yard: prepared.Loaded.Context.YardName, Inputs: inputs.binding(), Identity: binding, Release: releaseBinding, Proof: identity.CompletionProof()}
	markerContent, err := json.Marshal(marker)
	if err != nil {
		return false, err
	}
	if markerBefore.Exists {
		var existing config.DeveloperIdentityMarker
		if json.Unmarshal(markerBefore.Content, &existing) != nil || existing != marker {
			return false, fmt.Errorf("%w: pending developer identity marker changed", domain.ErrPlanStale)
		}
	}
	check := func(ctx context.Context) error {
		if err := inputs.check(ctx, cli); err != nil {
			return err
		}
		current, err := store.List(ctx)
		if err != nil {
			return err
		}
		if operationStateDigest(current) != operationStateDigest(projects) {
			return fmt.Errorf("%w: identity continuation projects changed", domain.ErrPlanStale)
		}
		release, err := identityReleaseBinding(prepared.Loaded.Context.Paths.ConfigHome)
		if err != nil {
			return err
		}
		if release != releaseBinding {
			return fmt.Errorf("%w: completed release changed", domain.ErrPlanStale)
		}
		return nil
	}
	prepared.exactState = operationStateDigest([]string{binding, inputs.binding(), operationStateDigest(projects)})
	prepared.stepsComplete = true
	prepared.steps = func() []domain.OperationStep { return domain.CloneOperationSteps(steps) }
	prepared.assess = func(context.Context) (domain.ActionID, domain.ActionDelta, error) {
		return "yard.init.reconcile", domain.ActionDelta{Changed: true, Consequences: append([]string{identity.Description(), "reconcile captured init stages, profile runtimes and project hooks after the identity prerequisite"}, execution.consequences()...)}, nil
	}
	prepared.refresh = func(ctx context.Context) (domain.ActionID, domain.ActionDelta, error) {
		if err := check(ctx); err != nil {
			return "", domain.ActionDelta{}, err
		}
		if err := hookRuntime.CheckProjectHookPlan(ctx, true); err != nil {
			return "", domain.ActionDelta{}, err
		}
		current, err := runtime.PrepareDeveloperIdentityProof(ctx, identity.CompletionProof(), binding)
		if err != nil {
			return "", domain.ActionDelta{}, err
		}
		if current == nil || current.Binding() != binding {
			return "", domain.ActionDelta{}, domain.ErrPlanStale
		}
		return prepared.assess(ctx)
	}
	prepared.execute = func(ctx context.Context, orchestrator *application.Orchestrator, diagnostics io.Writer) (domain.AdapterResult, error) {
		if err := check(ctx); err != nil {
			return domain.AdapterResult{}, err
		}
		if err := hookRuntime.CheckProjectHookPlan(ctx, true); err != nil {
			return domain.AdapterResult{}, err
		}
		if !markerBefore.Exists {
			markerBefore, err = createDeveloperIdentityMarker(prepared.Loaded.Context.Paths.ConfigHome, markerPath, markerBefore, markerContent)
			if err != nil {
				return domain.AdapterResult{}, err
			}
		}
		if err := runtime.AdvanceDeveloperIdentity(ctx, identity); err != nil {
			return domain.AdapterResult{}, err
		}
		current, err := runtime.PrepareDeveloperIdentity(ctx, true)
		if err != nil {
			return domain.AdapterResult{}, err
		}
		if current == nil || current.Binding() != binding {
			return domain.AdapterResult{}, fmt.Errorf("%w: native identity continuation changed", domain.ErrPlanStale)
		}
		if err := check(ctx); err != nil {
			return domain.AdapterResult{}, err
		}
		// Identity readiness is its own committed boundary. A later ordinary
		// hook failure must not strand an otherwise usable renamed account.
		if err := cli.publishAppliedIdentityHint(ctx, prepared.Loaded); err != nil {
			return domain.AdapterResult{}, err
		}
		if err := runtime.FinishDeveloperIdentity(ctx, identity); err != nil {
			return domain.AdapterResult{}, err
		}
		if err := config.RemovePersistentFileSnapshot(prepared.Loaded.Context.Paths.ConfigHome, markerPath, markerBefore); err != nil {
			return domain.AdapterResult{}, err
		}
		fresh := *prepared
		fresh.interactiveSetup = false
		if err := fresh.prepareOrdinaryInit(ctx, nil); err != nil {
			return domain.AdapterResult{}, err
		}
		if err := identityContinuationScope(steps, fresh.steps()); err != nil {
			return domain.AdapterResult{}, err
		}
		if err := execution.hookPlan.CheckContinuationScope(fresh.initExecution.hookPlan); err != nil {
			return domain.AdapterResult{}, err
		}
		// A retirement can delete integration data. One the approval did not
		// disclose needs a new plan; a stopped yard's plan disclosed its own.
		if !prepared.retirementsDisclosed {
			for _, retirement := range fresh.initExecution.integrationRetirements {
				if !slices.Contains(execution.integrationRetirements, retirement) {
					return domain.AdapterResult{}, fmt.Errorf("%w: integration retirement was not approved; run yard init again", domain.ErrPlanStale)
				}
			}
		}
		return fresh.execute(ctx, orchestrator, diagnostics)
	}
	return true, nil
}

func createDeveloperIdentityMarker(root, path string, before config.PersistentFileSnapshot, content []byte) (config.PersistentFileSnapshot, error) {
	if before.Exists {
		return config.PersistentFileSnapshot{}, fmt.Errorf("developer identity marker creation requires an absent target")
	}
	if err := config.CompareAndSwapLocalSettingsFile(root, path, before, content); err != nil {
		return config.PersistentFileSnapshot{}, err
	}
	return config.ReadPersistentFileSnapshot(root, path)
}

func identityPrerequisiteSteps(steps []domain.OperationStep, target, description string) []domain.OperationStep {
	steps = append([]domain.OperationStep{{ID: "init.developer-identity", Target: target, Observed: "captured account, home and profile owner identity", Desired: description, Decision: domain.StepApply, Verify: "protected identity journal and owner receipts agree", Consequence: description}}, steps...)
	for index := 1; index < len(steps); index++ {
		steps[index].DependsOn = []string{"init.developer-identity"}
	}
	return steps
}

func identityContinuationScope(approved, current []domain.OperationStep) error {
	for _, step := range current {
		if !slices.ContainsFunc(approved, func(before domain.OperationStep) bool { return before.ID == step.ID && before.Target == step.Target }) {
			return fmt.Errorf("%w: identity continuation acquired step %s", domain.ErrPlanStale, step.ID)
		}
	}
	return nil
}

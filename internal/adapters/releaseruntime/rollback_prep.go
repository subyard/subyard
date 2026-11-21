package releaseruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

// rollbackPreparation is the active release's confirmed down plan: the profile
// runtime owners whose installed format the rollback target does not read,
// observed in every local yard through rollback-observe.
type rollbackPreparation struct {
	activeRoot, targetRoot, configHome string
	owners                             []string
	yards                              []string
	active, target                     map[string]config.Loaded
	items                              []releasetransition.RollbackPrepItem
}

// rollbackPrepRefusal is one independent refusal of the down plan.
type rollbackPrepRefusal struct{ message, retry string }

func (refusal rollbackPrepRefusal) Error() string { return refusal.message }

// planRollbackPreparation selects the owners to prepare and observes them. It
// returns nil when no owner's format differs, which keeps the ordinary
// rollback path. Refusals are collected; err reports a read that failed.
func (runtime *Runtime) planRollbackPreparation(ctx context.Context, activeRoot, targetRoot, configHome string) (*rollbackPreparation, error, error) {
	active, err := profile.Load(activeRoot)
	if err != nil {
		return nil, nil, err
	}
	target, err := profile.Load(targetRoot)
	if err != nil {
		return nil, nil, err
	}
	formats := map[string]int{}
	for _, definition := range target {
		if definition.Runtime != nil {
			formats[definition.Runtime.ActivationID] = definition.Runtime.EffectiveFormat()
		}
	}
	prep := &rollbackPreparation{activeRoot: activeRoot, targetRoot: targetRoot, configHome: configHome}
	var refusals []error
	for _, definition := range active {
		hook := definition.Runtime
		if hook == nil {
			continue
		}
		// A target without the owner is the retained-owner admission's decision.
		format, retained := formats[hook.ActivationID]
		if !retained || format == hook.EffectiveFormat() {
			continue
		}
		if hook.PreparesRollbackTo != format {
			refusals = append(refusals, rollbackPrepRefusal{
				message: fmt.Sprintf("profile runtime %s format %d cannot be prepared for the retained release's format %d", hook.ActivationID, hook.EffectiveFormat(), format),
				retry:   "keep the current release active and retain a release whose profile runtime formats this release prepares, then run yard update --check",
			})
			continue
		}
		prep.owners = append(prep.owners, hook.ActivationID)
	}
	if len(prep.owners) == 0 {
		return nil, domain.CollectRefusals(refusals...), nil
	}
	if runtime.config.ProfileRuntimeRollbackObservation == nil || runtime.config.ProfileRuntimeRollbackApply == nil || runtime.config.ProfileRuntimeObservation == nil {
		return nil, nil, errors.New("profile runtime rollback preparation is unavailable")
	}
	if err := prep.loadYards(runtime.config); err != nil {
		return nil, nil, err
	}
	hooks := rollbackPrepHooks{runtime: runtime, prep: prep}
	for _, yard := range prep.yards {
		for _, owner := range prep.owners {
			item, err := hooks.Observe(ctx, owner, yard)
			var refusal rollbackPrepRefusal
			if errors.As(err, &refusal) {
				refusals = append(refusals, refusal)
				continue
			}
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return nil, nil, err
				}
				refusals = append(refusals, rollbackPrepRefusal{
					message: fmt.Sprintf("profile runtime %s in yard %s cannot be observed for rollback preparation", owner, yard),
					retry:   "run yard update --check",
				})
				continue
			}
			prep.items = append(prep.items, item)
		}
	}
	return prep, domain.CollectRefusals(refusals...), nil
}

func (prep *rollbackPreparation) loadYards(configuration Config) error {
	environment := configuration.Inputs.environment(configuration.Context)
	operator := first(environment["SUBYARD_OPERATOR_HOME"], environment["HOME"])
	names, err := config.YardNames(prep.configHome)
	if err != nil {
		return err
	}
	prep.active, prep.target = map[string]config.Loaded{}, map[string]config.Loaded{}
	for _, name := range names {
		load := func(root string) (config.Loaded, error) {
			return config.Load(config.LoadOptions{RepositoryRoot: root, OperatorHome: operator, YardName: name, Environment: environment})
		}
		active, err := load(prep.activeRoot)
		if err != nil {
			return fmt.Errorf("load yard %s for rollback preparation: %w", name, err)
		}
		if active.Context.AccessKind != domain.AccessLocal {
			continue
		}
		target, err := load(prep.targetRoot)
		if err != nil {
			return fmt.Errorf("load yard %s for the retained release: %w", name, err)
		}
		prep.yards = append(prep.yards, name)
		prep.active[name], prep.target[name] = active, target
	}
	return nil
}

// rollbackPrepHooks runs the active release's rollback actions per yard.
type rollbackPrepHooks struct {
	runtime *Runtime
	prep    *rollbackPreparation
}

func (hooks rollbackPrepHooks) Observe(ctx context.Context, owner, yard string) (releasetransition.RollbackPrepItem, error) {
	observation, err := hooks.runtime.config.ProfileRuntimeRollbackObservation(ctx, hooks.prep.activeRoot, hooks.prep.active[yard], owner)
	if err != nil {
		return releasetransition.RollbackPrepItem{}, err
	}
	item := releasetransition.RollbackPrepItem{Owner: owner, Yard: yard, State: string(observation.State),
		Actual: releasetransition.Fingerprint(observation.Actual), Desired: releasetransition.Fingerprint(observation.Desired)}
	if observation.State == ports.RuntimeStateDeferred {
		return item, stoppedYardRefusal(owner, yard)
	}
	return item, item.Validate()
}

func (hooks rollbackPrepHooks) Apply(ctx context.Context, item releasetransition.RollbackPrepItem, operation releasetransition.TransactionID) error {
	return hooks.runtime.config.ProfileRuntimeRollbackApply(ctx, hooks.prep.activeRoot, hooks.prep.active[item.Yard], item.Owner,
		string(operation), string(item.Actual), string(item.Desired))
}

func stoppedYardRefusal(owner, yard string) rollbackPrepRefusal {
	return rollbackPrepRefusal{
		message: fmt.Sprintf("yard %s is stopped, so profile runtime %s cannot be prepared for rollback; start it, then run yard update --rollback", yard, owner),
		retry:   "run " + domain.RenderAction(domain.Action{Verb: domain.VerbStart, Yard: yard}),
	}
}

// gate proves, after preparation, that the target release observes every
// prepared owner from its own root. A target that cannot observe the state,
// or a yard stopped meanwhile, refuses before dispatch.
func (prep *rollbackPreparation) gate(ctx context.Context, configuration Config) error {
	var refusals []error
	for _, item := range prep.items {
		observation, err := configuration.ProfileRuntimeObservation(ctx, prep.targetRoot, prep.target[item.Yard], item.Owner)
		switch {
		case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
			return err
		case err != nil:
			refusals = append(refusals, rollbackPrepRefusal{
				message: fmt.Sprintf("the retained release does not accept the prepared profile runtime %s in yard %s", item.Owner, item.Yard),
				retry:   "run " + domain.RenderAction(domain.Action{Verb: domain.VerbMigrate}),
			})
		case observation.State == ports.RuntimeStateDeferred:
			refusals = append(refusals, stoppedYardRefusal(item.Owner, item.Yard))
		}
	}
	return domain.CollectRefusals(refusals...)
}

// rollbackPrepOutcome renders collected refusals as one public outcome. The
// first refusal in stable order supplies the next step.
func rollbackPrepOutcome(links releasetransition.ReleaseLinks, target releasetransition.ReleaseID, refusals error) releasetransition.Outcome {
	var causes []error
	var collected *domain.Refusals
	if errors.As(refusals, &collected) {
		causes = collected.Unwrap()
	} else {
		causes = []error{refusals}
	}
	messages := make([]string, 0, len(causes))
	retry := ""
	for _, cause := range causes {
		messages = append(messages, cause.Error())
		var refusal rollbackPrepRefusal
		if retry == "" && errors.As(cause, &refusal) {
			retry = refusal.retry
		}
	}
	if retry == "" {
		retry = "run yard update --check"
	}
	return publicReleaseOutcome(links, target, nil, releasetransition.CodeRollbackIncompatible,
		boundedPrepText(strings.Join(messages, "; ")), retry)
}

// mergeRollbackPrepRefusals reports the down plan's refusals beside the
// rollback compatibility refusal, which keeps its own next step.
func mergeRollbackPrepRefusals(links releasetransition.ReleaseLinks, target releasetransition.ReleaseID, prep, compatibility error) error {
	if prep == nil {
		return compatibility
	}
	outcome := rollbackPrepOutcome(links, target, prep)
	var public publicReleaseInspectionError
	if errors.As(compatibility, &public) {
		merged := public.outcome
		merged.Message = boundedPrepText(merged.Message + "; " + outcome.Message)
		return publicReleaseInspectionError{cause: transitionOutcomeError(merged), outcome: merged}
	}
	if compatibility != nil {
		return compatibility
	}
	return publicReleaseInspectionError{cause: transitionOutcomeError(outcome), outcome: outcome}
}

func boundedPrepText(message string) string {
	const limit = 1024
	if len(message) <= limit {
		return message
	}
	return message[:limit-3] + "..."
}

func (prep *rollbackPreparation) consequences(target releasetransition.ReleaseID) []string {
	consequences := []string{"reactivate the verified retained previous runtime"}
	for _, item := range prep.items {
		if item.State == releasetransition.RollbackPrepStale {
			consequences = append(consequences, fmt.Sprintf("prepare profile runtime %s in yard %s for release %s", item.Owner, item.Yard, target))
		}
	}
	return append(consequences, "inspect the retained release's own plan after preparation and apply it under this confirmation")
}

func (prep *rollbackPreparation) steps(request releasetransition.ProcessRequest) []domain.OperationStep {
	steps := []domain.OperationStep{}
	for _, item := range prep.items {
		decision := domain.StepSkip
		if item.State == releasetransition.RollbackPrepStale {
			decision = domain.StepApply
		}
		step := domain.OperationStep{ID: fmt.Sprintf("release.rollback-prep.%d", len(steps)), Target: "rollback-prep." + item.Owner + " yard." + item.Yard,
			Observed: "rollback-observe " + item.State, Desired: "format of release " + string(request.Target), Decision: decision,
			Preconditions: []string{"the confirmed rollback-observe result is unchanged"},
			Verify:        "rollback-observe reports current, then the retained release observes the owner",
			Consequence:   fmt.Sprintf("prepare profile runtime %s in yard %s for release %s", item.Owner, item.Yard, request.Target)}
		if len(steps) != 0 {
			step.DependsOn = []string{steps[len(steps)-1].ID}
		}
		steps = append(steps, step)
	}
	activate := domain.OperationStep{ID: "release.activate", Target: request.RuntimeRoot + "/current", Observed: "unknown", Desired: "verified active release " + string(request.Target),
		Decision: domain.StepConditional, Preconditions: []string{"the retained release accepts the prepared state and its own plan is admitted"},
		Verify: "read actual runtime links and obtain native ready transition inspection", Consequence: "activate and verify release " + string(request.Target)}
	if len(steps) != 0 {
		activate.DependsOn = []string{steps[len(steps)-1].ID}
	}
	return append(steps, activate)
}

func (prep *rollbackPreparation) binding(request releasetransition.ProcessRequest, active, target candidateVerification, links any) (string, error) {
	scope, err := releasetransition.RollbackPrepBinding(prep.items)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(struct {
		Request                        releasetransition.ProcessRequest
		Scope                          releasetransition.Fingerprint
		ActiveRoot, TargetRoot         string
		ActiveDigest, TargetDigest     releasetransition.Fingerprint
		ActiveRegistry, TargetRegistry releasetransition.Fingerprint
		Links                          any
	}{request, scope, active.candidate.root, target.candidate.root, active.digest, target.digest, active.registryDigest, target.registryDigest, links})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

// admitFreshRollbackPlan decides whether the retained release's own plan,
// inspected after preparation, runs under the operator's one confirmation of
// the down plan. This is the single switch point for that policy.
// PROVISIONAL: admit a plan without blockers or an unfinished resume.
func admitFreshRollbackPlan(inspection releasetransition.Inspection) error {
	if len(inspection.Blockers) != 0 || inspection.Resume != nil {
		return errors.New("the retained release's plan after preparation is not admitted under this confirmation")
	}
	return nil
}

// prepareRollbackPreparation offers the down plan for confirmation. Execution
// runs the down transaction; its dispatch inspects the retained release
// afresh, admits that plan, and runs the ordinary retained execution.
func (runtime *Runtime) prepareRollbackPreparation(
	parsed options,
	request releasetransition.ProcessRequest,
	active, target candidateVerification,
	prep *rollbackPreparation,
	targetOwners map[string]bool,
) (Prepared, error) {
	binding, err := prep.binding(request, active, target, parsed.expectedLinks)
	if err != nil {
		return Prepared{}, err
	}
	converged := new(releasetransition.Outcome)
	return Prepared{
		Request: request, Action: "update.rollback", Changed: true,
		Consequences:   prep.consequences(request.Target),
		ActiveLauncher: filepath.Join(parsed.root, "current", "bin", "yard"),
		SourceRelease:  string(active.candidate.release),
		TargetRelease:  string(target.candidate.release), TargetVersion: target.version,
		Steps: prep.steps(request), Binding: binding, convergedOutcome: converged,
		check: func(ctx context.Context) (releasetransition.Inspection, error) {
			checked, _, err := runtime.inspectCompletedTransition(ctx, request.RuntimeRoot, request.ConfigHome, request.Target, target, request)
			return checked, err
		},
		run: func(ctx context.Context) error {
			*converged = releasetransition.Outcome{}
			ctx = selectionContext(ctx, parsed.selection)
			if err := requirePreparedReleaseRoots(parsed, false); err != nil {
				return err
			}
			verified, err := runtime.reverifyRollbackPrep(ctx, request.RuntimeRoot, active, target)
			if err != nil {
				return err
			}
			defer verified.Close()
			if observed, err := runtime.inspectRuntimeLinks(parsed.root); err != nil || observed != *parsed.expectedLinks {
				return staleRetainedSelection(request.RuntimeRoot, request.Target)
			}
			if err := runtime.inspectRollbackCompatibility(ctx, verified, request, parsed.selection); err != nil {
				return err
			}
			grant, err := newReleaseTransitionGrant()
			if err != nil {
				return err
			}
			links := releaseLinksFromRuntimeSnapshot(*parsed.expectedLinks)
			// The down journal changes history, so the gate and the target's
			// plan use a selection captured after the preparation.
			prepared := parsed
			down := releasetransition.RollbackPrep{
				ConfigHome: request.ConfigHome, Active: active.candidate.release, Target: request.Target,
				ArtifactDigest: active.digest, RegistryDigest: active.registryDigest,
				Items: prep.items, Hooks: rollbackPrepHooks{runtime: runtime, prep: prep},
				Gate: func(ctx context.Context) error {
					refusals := prep.gate(ctx, runtime.config)
					if errors.Is(refusals, context.Canceled) || errors.Is(refusals, context.DeadlineExceeded) {
						return refusals
					}
					selection, err := runtime.captureReleaseSelection(ctx, verified, request, *parsed.expectedLinks)
					if err != nil {
						return err
					}
					prepared.selection = selection
					return mergeRollbackPrepRefusals(links, request.Target, refusals,
						runtime.inspectRollbackCompatibility(ctx, verified, request, selection))
				},
				Dispatch: func(ctx context.Context) error {
					return runtime.dispatchPreparedRollback(selectionContext(ctx, prepared.selection), prepared, verified, request, prep, targetOwners, converged)
				},
			}
			return down.Run(ctx, grant)
		},
	}, nil
}

// reverifyRollbackPrep binds execution to the exact confirmed active and
// target artifacts and returns the verified target.
func (runtime *Runtime) reverifyRollbackPrep(ctx context.Context, root string, active, target candidateVerification) (*verifiedPublishedCandidate, error) {
	current, err := runtime.verifyPublishedCandidate(ctx, active.candidate, root, &active.digest)
	if err != nil {
		return nil, fmt.Errorf("%w: the active release changed after confirmation", domain.ErrPlanStale)
	}
	registry := current.registryDigest
	current.Close()
	if registry != active.registryDigest {
		return nil, fmt.Errorf("%w: the active release registry changed after confirmation", domain.ErrPlanStale)
	}
	verified, err := runtime.verifyPublishedCandidate(ctx, target.candidate, root, &target.digest)
	if err != nil {
		return nil, staleRetainedSelection(root, target.candidate.release)
	}
	if verified.version != target.version || verified.registryDigest != target.registryDigest {
		verified.Close()
		return nil, fmt.Errorf("%w: the retained release changed after confirmation", domain.ErrPlanStale)
	}
	return verified, nil
}

// dispatchPreparedRollback inspects the retained release afresh, admits its
// plan and hands the rollback to it through the ordinary retained execution.
func (runtime *Runtime) dispatchPreparedRollback(
	ctx context.Context,
	parsed options,
	verified *verifiedPublishedCandidate,
	request releasetransition.ProcessRequest,
	prep *rollbackPreparation,
	targetOwners map[string]bool,
	converged *releasetransition.Outcome,
) error {
	fresh, err := runtime.inspectVerifiedTransition(ctx, parsed, verified, verified, request)
	if err != nil {
		return err
	}
	links := releaseLinksFromRuntimeSnapshot(*parsed.expectedLinks)
	if err := admitFreshRollbackPlan(fresh.inspection); err != nil {
		return transitionOutcomeError(publicReleaseOutcome(links, request.Target, nil, releasetransition.CodeRollbackIncompatible,
			err.Error(), "run "+domain.RenderAction(domain.Action{Verb: domain.VerbMigrate})))
	}
	binding, err := runtime.retainedRuntimeBinding(ctx, prep.activeRoot, prep.targetRoot, prep.configHome, targetOwners)
	if err != nil {
		return transitionOutcomeError(publicReleaseOutcome(links, request.Target, nil, releasetransition.CodeRollbackIncompatible,
			"the retained release cannot verify installed runtime activation after preparation", "run "+domain.RenderAction(domain.Action{Verb: domain.VerbMigrate})))
	}
	fresh = runtime.guardRetainedRuntimeInputs(fresh, binding, prep.activeRoot, prep.targetRoot, prep.configHome, targetOwners)
	if err := fresh.Execute(ctx); err != nil {
		return err
	}
	if fresh.convergedOutcome != nil {
		*converged = *fresh.convergedOutcome
	}
	return nil
}

package releaseruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

// Rollback through a profile runtime hook takes two confirmations: the
// active release's down plan, which stops with this release active, then the
// retained release's own plan.
const (
	reactivateRetainedConsequence = "reactivate the verified retained previous runtime"
	rollbackRetry                 = "run yard update --rollback"
	preparedRollbackRetry         = "run yard update --rollback to review and confirm the rollback"
	maximumOutcomeMessage         = 512
)

var migrateRetry = "run " + domain.RenderAction(domain.Action{Verb: domain.VerbMigrate})

// rollbackPreparation is the active release's down plan: the profile runtime
// owners whose installed format the rollback target does not read, observed
// in every local yard through rollback-observe. Hooks run from pinned,
// verified release roots.
type rollbackPreparation struct {
	activeRoot, targetRoot, configHome string
	owners                             []string
	definitions                        map[string]profile.Definition
	yards                              []string
	active, target                     map[string]config.Loaded
	items                              []releasetransition.RollbackPrepItem
}

// rollbackPrepRefusal is one independent refusal with its next step.
type rollbackPrepRefusal struct {
	message, retry string
	cause          error
}

func (refusal rollbackPrepRefusal) Error() string { return refusal.message }

func (refusal rollbackPrepRefusal) Unwrap() error { return refusal.cause }

// pinnedReleaseRoot names a verified root through this process's descriptor,
// so a hook and its children run the exact verified files.
func pinnedReleaseRoot(candidate *verifiedPublishedCandidate) string {
	return fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), candidate.root.Fd())
}

// rollbackFormatOwners selects the active runtimes whose format differs from
// the format the target's own profile declares. An owner without a prepared
// hook for that format is refused; one missing from the target is the
// retained-owner admission's decision.
func rollbackFormatOwners(active, target []profile.Definition) ([]string, map[string]profile.Definition, []error) {
	formats := map[string]int{}
	for _, definition := range target {
		if definition.Runtime != nil {
			formats[definition.Runtime.ActivationID] = definition.Runtime.EffectiveFormat()
		}
	}
	owners, definitions, refusals := []string{}, map[string]profile.Definition{}, []error{}
	for _, definition := range active {
		hook := definition.Runtime
		if hook == nil {
			continue
		}
		format, retained := formats[hook.ActivationID]
		if !retained || format == hook.EffectiveFormat() {
			continue
		}
		if hook.PreparesRollbackTo != format {
			refusals = append(refusals, rollbackPrepRefusal{
				message: fmt.Sprintf("this release cannot convert profile runtime %s from format %d back to the retained release's format %d", hook.ActivationID, hook.EffectiveFormat(), format),
				retry:   "keep the current release active",
			})
			continue
		}
		owners = append(owners, hook.ActivationID)
		definitions[hook.ActivationID] = definition
	}
	return owners, definitions, refusals
}

func loadReleaseProfiles(active, target *verifiedPublishedCandidate) ([]profile.Definition, []profile.Definition, []error) {
	var refusals []error
	activeDefinitions, err := profile.Load(pinnedReleaseRoot(active))
	if err != nil {
		refusals = append(refusals, rollbackPrepRefusal{message: "the active release's profile definitions cannot be read for rollback", retry: "run yard update --check", cause: err})
	}
	targetDefinitions, err := profile.Load(pinnedReleaseRoot(target))
	if err != nil {
		refusals = append(refusals, rollbackPrepRefusal{message: "the retained release's profile definitions cannot be read for rollback", retry: "keep the current release active", cause: err})
	}
	return activeDefinitions, targetDefinitions, refusals
}

// planRollbackPreparation selects the owners to prepare and observes them. It
// returns nil when no owner's format differs, which keeps the ordinary
// rollback path. Refusals are collected; err reports a failure to proceed.
func (runtime *Runtime) planRollbackPreparation(ctx context.Context, active, target *verifiedPublishedCandidate, configHome string) (*rollbackPreparation, error, error) {
	activeDefinitions, targetDefinitions, refusals := loadReleaseProfiles(active, target)
	if len(refusals) != 0 {
		return nil, domain.CollectRefusals(refusals...), nil
	}
	owners, definitions, refusals := rollbackFormatOwners(activeDefinitions, targetDefinitions)
	if len(owners) == 0 {
		return nil, domain.CollectRefusals(refusals...), nil
	}
	if runtime.config.ProfileRuntimeRollbackObservation == nil || runtime.config.ProfileRuntimeRollbackApply == nil || runtime.config.ProfileRuntimeObservation == nil {
		return nil, nil, errors.New("profile runtime rollback preparation is unavailable")
	}
	prep := &rollbackPreparation{activeRoot: pinnedReleaseRoot(active), targetRoot: pinnedReleaseRoot(target), configHome: configHome, owners: owners, definitions: definitions}
	loadRefusals, err := prep.loadYards(runtime.config)
	if err != nil {
		return nil, nil, err
	}
	refusals = append(refusals, loadRefusals...)
	hooks := rollbackPrepHooks{runtime: runtime, prep: prep}
	for _, yard := range prep.yards {
		for _, owner := range prep.owners {
			item, err := hooks.Observe(ctx, owner, yard)
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, nil, err
			}
			if err != nil {
				refusals = append(refusals, observationRefusal(owner, yard, err))
				continue
			}
			prep.items = append(prep.items, item)
		}
	}
	if len(refusals) != 0 {
		return prep, domain.CollectRefusals(refusals...), nil
	}
	if prep.needsApply() {
		if _, err := prep.down(active, target, configHome).Plan(); err != nil {
			refusals = append(refusals, rollbackPrepRefusal{message: "the rollback preparation exceeds what one down transaction records", retry: "keep the current release active", cause: err})
		}
	} else {
		// Every owner is prepared: the rerun confirms the target's own plan
		// once the target accepts the prepared state.
		gate := prep.gate(ctx, runtime.config)
		if errors.Is(gate, context.Canceled) || errors.Is(gate, context.DeadlineExceeded) {
			return nil, nil, gate
		}
		refusals = append(refusals, refusalCauses(gate)...)
	}
	return prep, domain.CollectRefusals(refusals...), nil
}

func observationRefusal(owner, yard string, err error) error {
	var refusal rollbackPrepRefusal
	if errors.As(err, &refusal) {
		return refusal
	}
	return rollbackPrepRefusal{
		message: boundedPrepText(fmt.Sprintf("profile runtime %s in yard %s cannot be observed for rollback preparation: %v", owner, yard, err)),
		retry:   "run yard update --check", cause: err,
	}
}

// loadYards loads every local yard with both releases. A yard that cannot be
// loaded is a refusal of its own.
func (prep *rollbackPreparation) loadYards(configuration Config) ([]error, error) {
	environment := configuration.Inputs.environment(configuration.Context)
	operator := first(environment["SUBYARD_OPERATOR_HOME"], environment["HOME"])
	names, err := config.YardNames(prep.configHome)
	if err != nil {
		return nil, err
	}
	var refusals []error
	prep.yards, prep.active, prep.target = nil, map[string]config.Loaded{}, map[string]config.Loaded{}
	for _, name := range names {
		load := func(root string) (config.Loaded, error) {
			return config.Load(config.LoadOptions{RepositoryRoot: root, OperatorHome: operator, YardName: name, Environment: environment})
		}
		active, activeErr := load(prep.activeRoot)
		if activeErr == nil && active.Context.AccessKind != domain.AccessLocal {
			continue
		}
		target, targetErr := load(prep.targetRoot)
		if err := errors.Join(activeErr, targetErr); err != nil {
			refusals = append(refusals, rollbackPrepRefusal{
				message: fmt.Sprintf("yard %s configuration cannot be loaded for rollback preparation", name),
				retry:   "run " + domain.RenderAction(domain.Action{Verb: domain.VerbConfigValidate, Yard: name}), cause: err,
			})
			continue
		}
		prep.yards = append(prep.yards, name)
		prep.active[name], prep.target[name] = active, target
	}
	return refusals, nil
}

// reload binds the confirmed plan to freshly verified roots and freshly
// loaded yards before the first effect. The rechecked observations then
// name each yard's current native instance.
func (prep *rollbackPreparation) reload(configuration Config, active, target *verifiedPublishedCandidate) (*rollbackPreparation, error) {
	fresh := &rollbackPreparation{activeRoot: pinnedReleaseRoot(active), targetRoot: pinnedReleaseRoot(target), configHome: prep.configHome,
		owners: prep.owners, definitions: prep.definitions, items: prep.items}
	refusals, err := fresh.loadYards(configuration)
	if err != nil || len(refusals) != 0 || !slices.Equal(fresh.yards, prep.yards) {
		return nil, fmt.Errorf("%w: yard registrations changed after confirmation", domain.ErrPlanStale)
	}
	return fresh, nil
}

// needsApply reports whether any owner is still stale. A rerun after the
// down transaction finds every owner current and confirms the retained
// release's own plan instead.
func (prep *rollbackPreparation) needsApply() bool {
	return slices.ContainsFunc(prep.items, func(item releasetransition.RollbackPrepItem) bool {
		return item.State == releasetransition.RollbackPrepStale
	})
}

func (prep *rollbackPreparation) down(active, target *verifiedPublishedCandidate, configHome string) releasetransition.RollbackPrep {
	return releasetransition.RollbackPrep{
		ConfigHome: configHome, Active: active.candidate.release, Target: target.candidate.release,
		ArtifactDigest: active.manifestDigest, RegistryDigest: active.registryDigest, Items: prep.items,
	}
}

// rollbackPrepHooks runs the active release's rollback actions per yard.
type rollbackPrepHooks struct {
	runtime *Runtime
	prep    *rollbackPreparation
}

func (hooks rollbackPrepHooks) Observe(ctx context.Context, owner, yard string) (releasetransition.RollbackPrepItem, error) {
	loaded, ok := hooks.prep.active[yard]
	if !ok {
		return releasetransition.RollbackPrepItem{}, fmt.Errorf("%w: yard %s is no longer registered", domain.ErrPlanStale, yard)
	}
	observation, err := hooks.runtime.config.ProfileRuntimeRollbackObservation(ctx, hooks.prep.activeRoot, loaded, owner)
	if err != nil {
		return releasetransition.RollbackPrepItem{}, err
	}
	item := releasetransition.RollbackPrepItem{Owner: owner, Yard: yard, Instance: loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName,
		State: string(observation.State), Actual: releasetransition.Fingerprint(observation.Actual), Desired: releasetransition.Fingerprint(observation.Desired),
		Consequence: observation.Consequence}
	if observation.State == ports.RuntimeStateDeferred {
		installed, err := hooks.prep.possiblyInstalled(owner, yard, loaded)
		if err != nil {
			return item, err
		}
		if installed {
			return item, stoppedYardRefusal(owner, yard)
		}
		item = releasetransition.RollbackPrepItem{Owner: owner, Yard: yard, Instance: item.Instance, State: releasetransition.RollbackPrepAbsent}
	}
	return item, item.Validate()
}

// possiblyInstalled reports, without the guest, whether a stopped yard may
// hold the owner's installed state: the profile is selected there or its
// managed paths exist.
func (prep *rollbackPreparation) possiblyInstalled(owner, yard string, loaded config.Loaded) (bool, error) {
	definition := prep.definitions[owner]
	if definition.Selected(yard, loaded.Environment) {
		return true, nil
	}
	for _, managed := range definition.ManagedPaths {
		root := loaded.Context.Paths.DataHome
		if managed.Root == "operator" {
			root = loaded.Context.Paths.OperatorHome
		}
		_, err := os.Lstat(filepath.Join(root, strings.ReplaceAll(managed.Path, "{yard}", yard)))
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

func (hooks rollbackPrepHooks) Apply(ctx context.Context, item releasetransition.RollbackPrepItem, operation releasetransition.TransactionID) error {
	return hooks.runtime.config.ProfileRuntimeRollbackApply(ctx, hooks.prep.activeRoot, hooks.prep.active[item.Yard], item.Owner,
		string(operation), string(item.Actual), string(item.Desired))
}

func stoppedYardRefusal(owner, yard string) rollbackPrepRefusal {
	return rollbackPrepRefusal{
		message: fmt.Sprintf("yard %s is stopped, so its installed profile runtime %s cannot be prepared for rollback; start it, then run yard update --rollback", yard, owner),
		retry:   "run " + domain.RenderAction(domain.Action{Verb: domain.VerbStart, Yard: yard}),
	}
}

// gate proves that the target release observes every prepared owner from
// its own root. A target that cannot observe the state, or a yard stopped
// meanwhile, refuses.
func (prep *rollbackPreparation) gate(ctx context.Context, configuration Config) error {
	var refusals []error
	for _, item := range prep.items {
		if item.State == releasetransition.RollbackPrepAbsent {
			continue
		}
		observation, err := configuration.ProfileRuntimeObservation(ctx, prep.targetRoot, prep.target[item.Yard], item.Owner)
		switch {
		case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
			return err
		case err != nil:
			refusals = append(refusals, rollbackPrepRefusal{
				message: fmt.Sprintf("the retained release does not accept the prepared profile runtime %s in yard %s", item.Owner, item.Yard),
				retry:   migrateRetry, cause: err,
			})
		case observation.State == ports.RuntimeStateDeferred:
			refusals = append(refusals, stoppedYardRefusal(item.Owner, item.Yard))
		}
	}
	return domain.CollectRefusals(refusals...)
}

func refusalCauses(refusals error) []error {
	var collected *domain.Refusals
	if errors.As(refusals, &collected) {
		return collected.Unwrap()
	}
	if refusals == nil {
		return nil
	}
	return []error{refusals}
}

// asRollbackRefusal keeps a public refusal's own message and next step.
func asRollbackRefusal(err error) rollbackPrepRefusal {
	var refusal rollbackPrepRefusal
	if errors.As(err, &refusal) {
		return refusal
	}
	if outcome, ok := TransitionOutcome(err); ok {
		return rollbackPrepRefusal{message: outcome.Message, retry: outcome.Retry, cause: err}
	}
	return rollbackPrepRefusal{message: err.Error(), cause: err}
}

// publicRollbackRefusal renders independent refusals as one public outcome;
// the first one with a next step supplies it unless retry is given. Only the
// wire message is bounded: the full causes stay on the error when it is cut.
func publicRollbackRefusal(links releasetransition.ReleaseLinks, target releasetransition.ReleaseID, code releasetransition.OutcomeCode,
	prefix, retry string, causes []error,
) error {
	messages := make([]string, 0, len(causes))
	for _, cause := range causes {
		refusal := asRollbackRefusal(cause)
		messages = append(messages, refusal.message)
		if retry == "" {
			retry = refusal.retry
		}
	}
	if retry == "" {
		retry = "run yard update --check"
	}
	message := prefix + strings.Join(messages, "; ")
	outcome := publicReleaseOutcome(links, target, nil, code, boundedPrepText(message), retry)
	cause := transitionOutcomeError(outcome)
	if outcome.Message != message {
		cause = domain.CollectOrderedRefusals(append([]error{cause}, causes...)...)
	}
	return publicReleaseInspectionError{cause: cause, outcome: outcome}
}

// mergeRollbackPrepRefusals reports the down plan's refusals beside the
// rollback compatibility refusal, which comes first and keeps its code and
// next step.
func mergeRollbackPrepRefusals(links releasetransition.ReleaseLinks, target releasetransition.ReleaseID, prep, compatibility error) error {
	if prep == nil {
		return compatibility
	}
	causes := refusalCauses(prep)
	code := releasetransition.CodeRollbackIncompatible
	if compatibility != nil {
		outcome, public := TransitionOutcome(compatibility)
		if !public {
			return domain.CollectRefusals(publicRollbackRefusal(links, target, code, "", "", causes), compatibility)
		}
		code = outcome.Code
		causes = append([]error{compatibility}, causes...)
	}
	return publicRollbackRefusal(links, target, code, "", "", causes)
}

func boundedPrepText(message string) string {
	if len(message) <= maximumOutcomeMessage {
		return message
	}
	return message[:maximumOutcomeMessage-3] + "..."
}

func (prep *rollbackPreparation) consequences(target releasetransition.ReleaseID) []string {
	return append(releasetransition.RollbackPrepConsequences(prep.items, target),
		"keep this release active; then run yard update --rollback to review and confirm the retained release's own plan")
}

func (prep *rollbackPreparation) steps(target releasetransition.ReleaseID) []domain.OperationStep {
	steps := []domain.OperationStep{}
	for _, item := range prep.items {
		decision := domain.StepSkip
		if item.State == releasetransition.RollbackPrepStale {
			decision = domain.StepApply
		}
		consequence := fmt.Sprintf("prepare profile runtime %s in yard %s for release %s", item.Owner, item.Yard, target)
		if item.Consequence != "" {
			consequence += ": " + item.Consequence
		}
		step := domain.OperationStep{ID: fmt.Sprintf("release.rollback-prep.%d", len(steps)), Target: "rollback-prep." + item.Owner + " yard." + item.Yard,
			Observed: "rollback-observe " + item.State, Desired: "format of release " + string(target), Decision: decision,
			Preconditions: []string{"the confirmed rollback-observe result and yard instance are unchanged"},
			Verify:        "rollback-observe reports current, then the retained release observes the owner",
			Consequence:   consequence}
		if len(steps) != 0 {
			step.DependsOn = []string{steps[len(steps)-1].ID}
		}
		steps = append(steps, step)
	}
	return steps
}

// prepareRollbackPreparation offers the down plan for confirmation. Its run
// prepares the owners and passes the gate; this release stays active and the
// rerun confirms the retained release's own plan.
func (runtime *Runtime) prepareRollbackPreparation(
	parsed options,
	request releasetransition.ProcessRequest,
	activeVerified, targetVerified *verifiedPublishedCandidate,
	prep *rollbackPreparation,
) (Prepared, error) {
	active, target := verificationOf(activeVerified), verificationOf(targetVerified)
	token, err := prep.down(activeVerified, targetVerified, request.ConfigHome).Plan()
	if err != nil {
		return Prepared{}, err
	}
	links := releaseLinksFromRuntimeSnapshot(*parsed.expectedLinks)
	prepared := releasetransition.Outcome{
		Status: releasetransition.StatusReady, Active: links.Active, Previous: links.Previous, Target: links.Active,
		Message: fmt.Sprintf("profile runtimes are prepared for %s; release %s stays active", request.Target, links.Active),
		Retry:   preparedRollbackRetry,
	}
	return Prepared{
		Request: request, Action: "update.rollback", Changed: true,
		Consequences:   prep.consequences(request.Target),
		ActiveLauncher: filepath.Join(parsed.root, "current", "bin", "yard"),
		SourceRelease:  string(active.candidate.release),
		TargetRelease:  string(target.candidate.release), TargetVersion: target.version,
		Steps:   prep.steps(request.Target),
		Binding: releaseOperationBinding(request, releasetransition.Inspection{Plan: token}, active, target, parsed.expectedLinks),
		check: func(context.Context) (releasetransition.Inspection, error) {
			if observed, err := runtime.inspectRuntimeLinks(parsed.root); err != nil || observed != *parsed.expectedLinks {
				return releasetransition.Inspection{}, staleRetainedSelection(request.RuntimeRoot, request.Target)
			}
			return releasetransition.Inspection{Outcome: &prepared}, nil
		},
		run: func(ctx context.Context) error {
			ctx = selectionContext(ctx, parsed.selection)
			if err := requirePreparedReleaseRoots(parsed, false); err != nil {
				return err
			}
			activeNow, targetNow, err := runtime.reverifyRollbackPrep(ctx, request.RuntimeRoot, active, target)
			if err != nil {
				return err
			}
			defer activeNow.Close()
			defer targetNow.Close()
			fresh, err := prep.reload(runtime.config, activeNow, targetNow)
			if err != nil {
				return err
			}
			if observed, err := runtime.inspectRuntimeLinks(parsed.root); err != nil || observed != *parsed.expectedLinks {
				return staleRetainedSelection(request.RuntimeRoot, request.Target)
			}
			if err := runtime.inspectRollbackCompatibility(ctx, targetNow, request, parsed.selection); err != nil {
				return err
			}
			grant, err := newReleaseTransitionGrant()
			if err != nil {
				return err
			}
			down := fresh.down(activeNow, targetNow, request.ConfigHome)
			down.Hooks = rollbackPrepHooks{runtime: runtime, prep: fresh}
			// Protected history must still be the confirmed one when the down
			// journal is written.
			down.Admit = func(ctx context.Context) (func(), error) {
				unlock, err := runtime.lockReleaseSelection(ctx, false)
				if err != nil {
					return nil, err
				}
				if err := runtime.revalidateCurrentSnapshot(request.RuntimeRoot, request.ConfigHome, parsed.selection.state); err != nil {
					unlock()
					return nil, err
				}
				return unlock, nil
			}
			// The down journal changes history, so the gate checks a selection
			// captured after it.
			down.Gate = func(ctx context.Context) error {
				refusals := fresh.gate(ctx, runtime.config)
				if errors.Is(refusals, context.Canceled) || errors.Is(refusals, context.DeadlineExceeded) {
					return refusals
				}
				selection, err := runtime.captureReleaseSelection(ctx, targetNow, request, *parsed.expectedLinks)
				if err != nil {
					return domain.CollectRefusals(refusals, err)
				}
				return mergeRollbackPrepRefusals(links, request.Target, refusals, runtime.inspectRollbackCompatibility(ctx, targetNow, request, selection))
			}
			return runtime.rollbackPrepResult(ctx, parsed.root, links, request.Target, down.Run(ctx, grant))
		},
	}, nil
}

// rollbackPrepResult types a down transaction failure. Before its journal
// nothing changed and the refusal keeps its own next step. After it, hooks
// may have run while this release stays active: the refusal names the
// forward repair, or the refusal's own step when that resumes the rollback.
func (runtime *Runtime) rollbackPrepResult(ctx context.Context, root string, links releasetransition.ReleaseLinks, target releasetransition.ReleaseID, err error) error {
	if err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var interrupted releasetransition.RollbackPrepInterrupted
	if !errors.As(err, &interrupted) {
		if _, public := TransitionOutcome(err); public || errors.Is(err, domain.ErrPlanStale) {
			return err
		}
		return publicRollbackRefusal(links, target, releasetransition.CodeRollbackIncompatible, "", rollbackRetryUnlessOwn(err), []error{err})
	}
	if observed, linkErr := runtime.inspectRuntimeLinks(root); linkErr != nil || releaseLinksFromRuntimeSnapshot(observed).Active != links.Active {
		return err
	}
	causes := refusalCauses(interrupted.Err)
	retry := migrateRetry
	if first := asRollbackRefusal(causes[0]); first.retry != "" && (strings.HasPrefix(first.retry, "run yard -Y ") || first.retry == rollbackRetry) {
		retry = first.retry
	}
	return publicRollbackRefusal(links, target, releasetransition.CodeRollbackIncompatible,
		fmt.Sprintf("release %s stays active and its profile runtimes may be partly prepared; yard migrate repairs them forward: ", links.Active), retry, causes)
}

// rollbackRetryUnlessOwn keeps a refusal's own next step; nothing changed,
// so any other failure retries the rollback.
func rollbackRetryUnlessOwn(err error) string {
	var refusal rollbackPrepRefusal
	if errors.As(err, &refusal) && refusal.retry != "" {
		return refusal.retry
	}
	return rollbackRetry
}

// guardPreparedOwners rechecks, before the retained release's own plan runs,
// that every prepared owner still observes as confirmed from freshly
// verified roots and yard registrations.
func (runtime *Runtime) guardPreparedOwners(prepared Prepared, root string, prep *rollbackPreparation, active, target candidateVerification) Prepared {
	run := prepared.run
	if run == nil {
		return prepared
	}
	prepared.run = func(ctx context.Context) error {
		activeNow, targetNow, err := runtime.reverifyRollbackPrep(ctx, root, active, target)
		if err != nil {
			return err
		}
		fresh, err := prep.reload(runtime.config, activeNow, targetNow)
		if err == nil {
			hooks := rollbackPrepHooks{runtime: runtime, prep: fresh}
			for _, item := range prep.items {
				observed, observeErr := hooks.Observe(ctx, item.Owner, item.Yard)
				if observeErr != nil {
					err = observeErr
					break
				}
				if observed != item {
					err = fmt.Errorf("%w: prepared profile runtime %s in yard %s changed after confirmation", domain.ErrPlanStale, item.Owner, item.Yard)
					break
				}
			}
		}
		activeNow.Close()
		targetNow.Close()
		if err != nil {
			return err
		}
		return run(ctx)
	}
	return prepared
}

func verificationOf(candidate *verifiedPublishedCandidate) candidateVerification {
	return candidateVerification{candidate: candidate.candidate, digest: candidate.manifestDigest, version: candidate.version, registryDigest: candidate.registryDigest}
}

// reverifyRollbackPrep binds execution to the exact confirmed active and
// target artifacts. The caller closes both.
func (runtime *Runtime) reverifyRollbackPrep(ctx context.Context, root string, active, target candidateVerification) (*verifiedPublishedCandidate, *verifiedPublishedCandidate, error) {
	activeNow, err := runtime.verifyPublishedCandidate(ctx, active.candidate, root, &active.digest)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: the active release changed after confirmation", domain.ErrPlanStale)
	}
	if activeNow.registryDigest != active.registryDigest {
		activeNow.Close()
		return nil, nil, fmt.Errorf("%w: the active release registry changed after confirmation", domain.ErrPlanStale)
	}
	targetNow, err := runtime.verifyPublishedCandidate(ctx, target.candidate, root, &target.digest)
	if err != nil {
		activeNow.Close()
		return nil, nil, staleRetainedSelection(root, target.candidate.release)
	}
	if targetNow.version != target.version || targetNow.registryDigest != target.registryDigest {
		activeNow.Close()
		targetNow.Close()
		return nil, nil, fmt.Errorf("%w: the retained release changed after confirmation", domain.ErrPlanStale)
	}
	return activeNow, targetNow, nil
}

// downgradeFormatRefusal refuses an activation of an older artifact that
// would hand an owner's state to an earlier format without the active
// release's preparation, whichever command selected it.
func (runtime *Runtime) downgradeFormatRefusal(selection *releaseSelection, target *verifiedPublishedCandidate, retained bool) error {
	if selection == nil || selection.refusal != "" {
		return nil
	}
	active, err := runtime.verifySelectedActive(selection, &selection.activeArtifact)
	if err != nil {
		return nil
	}
	defer active.Close()
	activeDefinitions, targetDefinitions, unreadable := loadReleaseProfiles(active, target)
	if len(unreadable) != 0 {
		return nil
	}
	formats := map[string]int{}
	for _, definition := range targetDefinitions {
		if definition.Runtime != nil {
			formats[definition.Runtime.ActivationID] = definition.Runtime.EffectiveFormat()
		}
	}
	var refusals []error
	for _, definition := range activeDefinitions {
		hook := definition.Runtime
		if hook == nil {
			continue
		}
		if format, ok := formats[hook.ActivationID]; ok && format < hook.EffectiveFormat() {
			retry := "keep the current release active"
			if retained && hook.PreparesRollbackTo == format {
				retry = rollbackRetry
			}
			refusals = append(refusals, rollbackPrepRefusal{
				message: fmt.Sprintf("release %s reads profile runtime %s in format %d, but its installed state has format %d", target.candidate.release, hook.ActivationID, format, hook.EffectiveFormat()),
				retry:   retry,
			})
		}
	}
	return domain.CollectRefusals(refusals...)
}

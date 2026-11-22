package releaseruntime

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/blang/semver/v4"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

// Rollback through a profile runtime hook takes two commands. The first
// yard update --rollback converts, with the active release's rollback-observe
// and rollback-apply, each owner whose declared format the retained release
// does not read, and stops with this release active. The second is the
// ordinary rollback.

// PrepareRollbackAction is the first command's action; the second is the
// ordinary rollback.
const (
	PrepareRollbackAction domain.ActionID = "update.prepare-rollback"
	rollbackAction        domain.ActionID = "update.rollback"
)

const keepActiveRetry = "keep the current release active"

var (
	rollbackRetry = "run " + domain.RenderAction(domain.Action{Verb: domain.VerbUpdateRollback})
	migrateRetry  = "run " + domain.RenderAction(domain.Action{Verb: domain.VerbMigrate})
)

// pinnedReleaseRoot names a verified root through this process's descriptor,
// so a hook and its children run the exact verified files.
func pinnedReleaseRoot(candidate *verifiedPublishedCandidate) string {
	return fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), candidate.root.Fd())
}

// candidateVersion is the version a verified release reports, or else the
// one its release ID names.
func candidateVersion(candidate *verifiedPublishedCandidate) (semver.Version, error) {
	if candidate.version != "" {
		return semver.Parse(candidate.version)
	}
	return releasetransition.ReleaseIDVersion(candidate.candidate.release)
}

// profileFormatAdmission compares the runtime formats that the active
// release and a target that is not newer declare per activation ID. A
// rollback returns the owners the active release prepares for the target,
// also beside a refusal. Any other difference refuses, as do profiles that
// cannot be read. Only established versions exempt a newer target.
func profileFormatAdmission(active, target *verifiedPublishedCandidate, rollback, retained bool) (owners []string, refusal, retry string, cause error) {
	activeVersion, activeErr := candidateVersion(active)
	targetVersion, targetErr := candidateVersion(target)
	versionErr := errors.Join(activeErr, targetErr)
	if versionErr == nil && targetVersion.GT(activeVersion) {
		return nil, "", "", nil
	}
	if versionErr != nil {
		versionErr = fmt.Errorf("release versions cannot be compared: %w", versionErr)
	}
	activeDefinitions, activeErr := profile.Load(pinnedReleaseRoot(active))
	targetDefinitions, targetErr := profile.Load(pinnedReleaseRoot(target))
	if err := errors.Join(activeErr, targetErr); err != nil {
		return nil, "the profile runtime formats of release " + string(target.candidate.release) + " cannot be compared", keepActiveRetry, errors.Join(err, versionErr)
	}
	formats := map[string]profile.RuntimeFormat{}
	for _, definition := range targetDefinitions {
		if definition.Runtime != nil {
			formats[definition.Runtime.ActivationID] = definition.Runtime.EffectiveFormat()
		}
	}
	var differing []string
	preparable := true
	for _, definition := range activeDefinitions {
		hook := definition.Runtime
		if hook == nil {
			continue
		}
		format, declared := formats[hook.ActivationID]
		if !declared || format == hook.EffectiveFormat() {
			continue
		}
		if rollback && hook.PreparesRollbackTo == format {
			owners = append(owners, hook.ActivationID)
			continue
		}
		preparable = preparable && hook.PreparesRollbackTo == format
		differing = append(differing, fmt.Sprintf("%s %d instead of %d", hook.ActivationID, format, hook.EffectiveFormat()))
	}
	slices.Sort(owners)
	if len(differing) == 0 {
		return owners, "", "", nil
	}
	describe := func(listed []string) string {
		return fmt.Sprintf("release %s declares other profile runtime formats than the active release: %s", target.candidate.release, strings.Join(listed, ", "))
	}
	cause = errors.Join(errors.New(describe(differing)), versionErr)
	if len(differing) > 4 {
		differing = append(differing[:4:4], fmt.Sprintf("%d more", len(differing)-4))
	}
	retry = keepActiveRetry
	if preparable && retained {
		retry = rollbackRetry
	}
	return owners, describe(differing), retry, cause
}

// ownerRefusal is an owner's own public refusal and next command, or else
// the given ones.
func ownerRefusal(err error, message, retry string) (string, string) {
	var diagnostic interface{ ActivationDiagnostic() (string, string) }
	if errors.As(err, &diagnostic) {
		if owned, next := diagnostic.ActivationDiagnostic(); owned != "" {
			if strings.HasPrefix(next, "run ") {
				if message != "" {
					owned = message + ": " + owned
				}
				return owned, next
			}
		}
	}
	return message, retry
}

// formatItem is one owner's rollback-observe result in one local yard.
type formatItem struct {
	owner, yard, instance string
	state                 ports.RuntimeState
	actual, desired       string
	consequence           string
}

// formatObservation collects every yard's items and independent refusals,
// in yard and owner order.
type formatObservation struct {
	items    []formatItem
	messages []string
	retry    string
	causes   []error
}

func (observation *formatObservation) refuse(message, retry string, cause error) {
	observation.messages = append(observation.messages, message)
	if observation.retry == "" {
		observation.retry = retry
	}
	if cause != nil {
		observation.causes = append(observation.causes, fmt.Errorf("%s: %w", message, cause))
	}
}

func (observation formatObservation) pending() []formatItem {
	var pending []formatItem
	for _, item := range observation.items {
		if item.state == ports.RuntimeStateStale {
			pending = append(pending, item)
		}
	}
	return pending
}

// inspectProfileFormatRollback observes the owners the selection prepares
// from the verified active release.
func (runtime *Runtime) inspectProfileFormatRollback(ctx context.Context, target *verifiedPublishedCandidate, request releasetransition.ProcessRequest, selection *releaseSelection) (formatObservation, error) {
	if selection == nil || len(selection.formatOwners) == 0 {
		return formatObservation{}, nil
	}
	active, err := runtime.verifySelectedActive(selection, &selection.activeArtifact)
	if err != nil {
		observation := formatObservation{}
		observation.refuse("the active release cannot be verified for rollback preparation", keepActiveRetry, err)
		return observation, nil
	}
	defer active.Close()
	return runtime.observeFormatOwners(ctx, active, target, request.ConfigHome, selection.formatOwners)
}

// rollbackYardLoader reads one yard's settings with a release root. What it
// loads resolves that release's files, hook handlers included, under that
// root, so the root must stay open while they are used.
func (runtime *Runtime) rollbackYardLoader(configHome string) func(root, name string) (config.Loaded, error) {
	environment := runtime.config.Inputs.environment(runtime.config.Context)
	environment["SUBYARD_CONFIG_HOME"] = configHome
	operator := first(environment["SUBYARD_OPERATOR_HOME"], environment["HOME"])
	return func(root, name string) (config.Loaded, error) {
		return config.Load(config.LoadOptions{RepositoryRoot: root, OperatorHome: operator, YardName: name, Environment: environment})
	}
}

// observeFormatOwners runs rollback-observe for each owner in every local
// yard, and reads each local yard's settings with the target release. absent
// and stale pass; current passes once the target's own observe accepts the
// prepared state. A stopped yard, a failed observation and settings the
// target cannot read refuse.
func (runtime *Runtime) observeFormatOwners(ctx context.Context, active, target *verifiedPublishedCandidate, configHome string, owners []string) (formatObservation, error) {
	observation := formatObservation{}
	observe, accept := runtime.config.ProfileRuntimeRollbackObservation, runtime.config.ProfileRuntimeObservation
	if observe == nil || accept == nil || runtime.config.ProfileRuntimeRollbackApply == nil {
		observation.refuse("profile runtime rollback preparation is unavailable", keepActiveRetry, nil)
		return observation, nil
	}
	activeRoot, targetRoot := pinnedReleaseRoot(active), pinnedReleaseRoot(target)
	load := runtime.rollbackYardLoader(configHome)
	names, err := config.YardNames(configHome)
	if err != nil {
		observation.refuse("cannot safely list yards for rollback preparation", rollbackRetry, err)
		return observation, nil
	}
	for _, name := range names {
		loaded, err := load(activeRoot, name)
		if err != nil {
			observation.refuse(fmt.Sprintf("yard %s: cannot safely read settings for rollback preparation", name),
				"run "+domain.RenderAction(domain.Action{Verb: domain.VerbConfigValidate, Yard: name}), err)
			continue
		}
		if loaded.Context.AccessKind != domain.AccessLocal {
			continue
		}
		targetLoaded, targetErr := load(targetRoot, name)
		stopped := "run " + domain.RenderAction(domain.Action{Verb: domain.VerbStart, Yard: name})
		prepared := false
		for _, owner := range owners {
			state, err := observe(ctx, activeRoot, loaded, owner)
			if ctx.Err() != nil {
				return formatObservation{}, ctx.Err()
			}
			item := formatItem{owner: owner, yard: name, instance: loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName,
				state: state.State, actual: state.Actual, desired: state.Desired, consequence: state.Consequence}
			switch {
			case err != nil:
				message, retry := ownerRefusal(err, fmt.Sprintf("yard %s: profile runtime %s cannot be observed for rollback preparation", name, owner), rollbackRetry)
				observation.refuse(message, retry, err)
				continue
			case state.State == ports.RuntimeStateDeferred:
				observation.refuse(fmt.Sprintf("yard %s is stopped, so profile runtime %s cannot be prepared for rollback", name, owner), stopped, nil)
				continue
			case state.State == ports.RuntimeStateCurrent:
				prepared = true
				if targetErr != nil {
					break
				}
				accepted, err := accept(ctx, targetRoot, targetLoaded, owner)
				if ctx.Err() != nil {
					return formatObservation{}, ctx.Err()
				}
				if err != nil || accepted.State == ports.RuntimeStateDeferred {
					message, retry := ownerRefusal(err, fmt.Sprintf("yard %s: release %s does not accept the prepared profile runtime %s", name, target.candidate.release, owner), migrateRetry)
					observation.refuse(message, retry, err)
					continue
				}
			case state.State != ports.RuntimeStateAbsent && state.State != ports.RuntimeStateStale:
				observation.refuse(fmt.Sprintf("yard %s: profile runtime %s reports no rollback preparation state", name, owner), rollbackRetry, nil)
				continue
			}
			observation.items = append(observation.items, item)
		}
		if targetErr != nil {
			// The target cannot run this yard whatever its runtimes hold;
			// yard migrate undoes what preparation already converted.
			retry := keepActiveRetry
			if prepared {
				retry = migrateRetry
			}
			observation.refuse(fmt.Sprintf("yard %s: release %s cannot read the yard settings", name, target.candidate.release), retry, targetErr)
		}
	}
	return observation, nil
}

// retainedBindingMessage reports installed runtime state the retained
// release cannot activate.
const retainedBindingMessage = "the retained release cannot verify installed runtime activation"

func retainedBindingRefusal(links releasetransition.ReleaseLinks, target releasetransition.ReleaseID, cause error) error {
	outcome := publicReleaseOutcome(links, target, nil, releasetransition.CodeRollbackIncompatible,
		retainedBindingMessage, "retain a release with compatible component activation owners, then run yard update --check")
	return publicReleaseInspectionError{cause: domain.CollectOrderedRefusals(transitionOutcomeError(outcome), cause), outcome: outcome}
}

// joinReleaseRefusals reports a further independent finding beside a public
// refusal, whose next step stays first. The wire message keeps 512 bytes;
// the cause keeps every detail once.
func joinReleaseRefusals(first publicReleaseInspectionError, message string, detail error) publicReleaseInspectionError {
	outcome := first.outcome
	if len(outcome.Message)+2+len(message) <= 512 {
		outcome.Message += "; " + message
	} else {
		detail = fmt.Errorf("%s: %w", message, detail)
	}
	return publicReleaseInspectionError{cause: domain.CollectOrderedRefusals(transitionOutcomeError(outcome), outcomeDetails(first.cause), detail), outcome: outcome}
}

// outcomeDetails is a refusal's cause without its rendered outcomes.
func outcomeDetails(cause error) error {
	if public := (publicReleaseInspectionError{}); errors.As(cause, &public) {
		cause = public.cause
	}
	if _, outcome := cause.(transitionOutcomeFailure); outcome {
		return nil
	}
	refusals, ok := cause.(*domain.Refusals)
	if !ok {
		return cause
	}
	var details []error
	for _, part := range refusals.Unwrap() {
		if _, outcome := part.(transitionOutcomeFailure); !outcome {
			details = append(details, part)
		}
	}
	return domain.CollectOrderedRefusals(details...)
}

// preparationRetry is a preparation failure's own next step: a typed
// outcome's, else an owner's, else rerunning the rollback.
func preparationRetry(err error) string {
	_, retry := ownerRefusal(err, "", rollbackRetry)
	if outcome, ok := TransitionOutcome(err); ok {
		if _, _, typed := domain.ParseRetry(outcome.Retry); typed {
			retry = outcome.Retry
		}
	}
	return retry
}

// staleAfterConfirmation reports a refusal of the admission rerun before
// the first conversion, with its own next step.
func staleAfterConfirmation(request releasetransition.ProcessRequest, refusal error) error {
	const changed = "release state changed after confirmation; nothing was converted"
	outcome, typed := TransitionOutcome(refusal)
	if !typed {
		return refusal
	}
	message, retry := changed+"; "+outcome.Message, preparationRetry(refusal)
	if len(message) > 512 {
		message = changed + "; full details: stderr of yard update --rollback"
	}
	stale := observedPublicReleaseOutcome(request.RuntimeRoot, request.Target, nil, releasetransition.CodePlanStale, message, retry)
	cause := fmt.Errorf("%w: %w", domain.ErrPlanStale, transitionOutcomeError(stale))
	if message != changed+"; "+outcome.Message {
		cause = fmt.Errorf("%w; %s", cause, outcome.Message)
	}
	return publicReleaseInspectionError{cause: domain.CollectOrderedRefusals(cause, outcomeDetails(refusal)), outcome: stale}
}

// ownerPreparationSteps is one step per owner and distinct declared
// consequence, in the order observed. A step shows its consequence whole and
// names its yards as far as the step text allows; the plan binding covers
// every yard exactly.
func ownerPreparationSteps(items []formatItem, target releasetransition.ReleaseID) []domain.OperationStep {
	type group struct{ owner, consequence string }
	var groups []group
	yards := map[group][]string{}
	for _, item := range items {
		if item.state != ports.RuntimeStateStale {
			continue
		}
		key := group{item.owner, item.consequence}
		if _, seen := yards[key]; !seen {
			groups = append(groups, key)
		}
		yards[key] = append(yards[key], item.yard)
	}
	steps := []domain.OperationStep{}
	for _, key := range groups {
		named := yards[key]
		prefix, effect := fmt.Sprintf("prepare profile runtime %s for release %s in ", key.owner, target), ""
		if key.consequence != "" {
			effect = ": " + key.consequence
		}
		where := yardList(named, min(200, 512-len(prefix)-len(effect)))
		step := domain.OperationStep{ID: fmt.Sprintf("release.prepare-rollback.%d", len(steps)), Target: "activation." + key.owner,
			Observed: "rollback-observe stale in " + yardList(named, 200), Desired: "format of release " + string(target), Decision: domain.StepApply,
			Preconditions: []string{"a rerun of the rollback admission offers this same plan"},
			Verify:        "rollback-observe reports current in each yard, then the retained release accepts the owner", Consequence: prefix + where + effect}
		if len(steps) != 0 {
			step.DependsOn = []string{steps[len(steps)-1].ID}
		}
		steps = append(steps, step)
	}
	return steps
}

// yardList names the yards within limit bytes, counting those it leaves out.
func yardList(yards []string, limit int) string {
	if len(yards) == 1 {
		if text := "yard " + yards[0]; len(text) <= limit {
			return text
		}
		return "1 yard"
	}
	text := fmt.Sprintf("%d yards: ", len(yards))
	for index, yard := range yards {
		next := text + yard
		if index != 0 {
			next = text + ", " + yard
		}
		rest := ""
		if left := len(yards) - index - 1; left != 0 {
			rest = fmt.Sprintf(" and %d more", left)
		}
		if len(next)+len(rest) > limit {
			if index == 0 {
				return fmt.Sprintf("%d yards", len(yards))
			}
			return fmt.Sprintf("%s and %d more", text, len(yards)-index)
		}
		text = next
	}
	return text
}

// boundItems is every observed item a confirmation binds.
func boundItems(items []formatItem) []string {
	bound := make([]string, 0, len(items))
	for _, item := range items {
		bound = append(bound, strings.Join([]string{item.owner, item.yard, item.instance, string(item.state), item.actual, item.desired, item.consequence}, "\x00"))
	}
	return bound
}

// prepareRollbackPreparation offers the conversion of the stale owners among
// the observed items as the whole plan. Its run reruns the rollback
// admission, which must offer this same plan; converts; then reruns it as
// the gate, which must offer the ordinary rollback. It keeps no record: a
// rerun resumes, and yard migrate undoes it.
func (runtime *Runtime) prepareRollbackPreparation(parsed options, request releasetransition.ProcessRequest, target *verifiedPublishedCandidate,
	observation formatObservation, retained string,
) (Prepared, error) {
	selection := parsed.selection
	links := releaseLinksFromRuntimeSnapshot(*parsed.expectedLinks)
	activeVerification := candidateVerification{candidate: publishedCandidate{release: links.Active}, digest: selection.activeArtifact}
	targetVerification := verificationOf(target)
	items := observation.items
	steps := ownerPreparationSteps(items, request.Target)
	steps = append(steps, domain.OperationStep{ID: "release.prepare-rollback.gate", Target: "release." + string(links.Active),
		Observed: "active", Desired: "active", Decision: domain.StepApply, DependsOn: []string{steps[len(steps)-1].ID},
		Verify:      "a rerun of the rollback admission offers the rollback to release " + string(request.Target),
		Consequence: fmt.Sprintf("keep release %s active; then run yard update --rollback to review and confirm the rollback", links.Active)})
	// A plan that cannot show every consequence is refused, never shortened.
	if err := domain.ValidateOperationSteps(steps); err != nil {
		message := fmt.Sprintf("rollback preparation needs %d confirmation steps, more than the %d a plan can show; nothing was converted", len(steps), domain.MaxOperationSteps)
		if len(steps) <= domain.MaxOperationSteps {
			message = "rollback preparation cannot show every profile runtime conversion in its plan; nothing was converted"
		}
		outcome := publicReleaseOutcome(links, request.Target, nil, releasetransition.CodeRollbackIncompatible, message, keepActiveRetry)
		return Prepared{}, publicReleaseInspectionError{cause: domain.CollectOrderedRefusals(transitionOutcomeError(outcome), err), outcome: outcome}
	}
	consequences := domain.OperationStepConsequences(steps)
	prepared := false
	plan := Prepared{
		Request: request, Action: PrepareRollbackAction, Changed: true, Consequences: consequences,
		AgentCleanup:   slices.Clone(selection.agentCleanup),
		ActiveLauncher: filepath.Join(parsed.root, "current", "bin", "yard"),
		SourceRelease:  string(links.Active), TargetRelease: string(target.candidate.release), TargetVersion: target.version, Steps: steps,
		Binding: releaseOperationBinding(request, releasetransition.Inspection{Assessment: domain.ActionAssessment{Consequences: consequences}},
			activeVerification, targetVerification, struct {
				Links    any
				Steps    []domain.OperationStep
				Items    []string
				Retained string
			}{parsed.expectedLinks, steps, boundItems(items), retained}),
		preparation: &observation,
	}
	plan.Consequences = append(plan.Consequences, plan.AgentCleanupConsequences()...)
	plan.check = func(context.Context) (releasetransition.Inspection, error) {
		if observed, err := runtime.inspectRuntimeLinks(parsed.root); err != nil || observed != *parsed.expectedLinks {
			return releasetransition.Inspection{}, staleRetainedSelection(request.RuntimeRoot, request.Target)
		}
		message := fmt.Sprintf("profile runtimes are not yet prepared for %s; release %s stays active", request.Target, links.Active)
		if prepared {
			message = fmt.Sprintf("profile runtimes are prepared for %s; release %s stays active until the rollback is confirmed, and yard migrate undoes the preparation", request.Target, links.Active)
		}
		return releasetransition.Inspection{Outcome: &releasetransition.Outcome{
			Status: releasetransition.StatusMigrationRequired, Active: links.Active, Previous: links.Previous, Target: request.Target,
			Code: releasetransition.CodeTransitionRequired, Message: message, Retry: rollbackRetry,
		}}, nil
	}
	// readmit is the rollback admission a rerun of this command makes, with
	// the observation it binds when it again offers the preparation. The
	// plan, each conversion and the gate use only this admission, so owner
	// refusals, next steps and causes reach each the same way.
	readmit := func(ctx context.Context) (domain.ActionID, string, *formatObservation, error) {
		if parsed.rerun == nil {
			return "", "", nil, errors.New("the rollback admission cannot be rerun")
		}
		fresh, err := parsed.rerun(ctx)
		if fresh.Close != nil {
			_ = fresh.Close()
		}
		return fresh.Action, fresh.Binding, fresh.preparation, err
	}
	plan.run = func(outer context.Context) error {
		ctx := outer
		action, binding, now, err := readmit(ctx)
		if err != nil {
			return staleAfterConfirmation(request, err)
		}
		// The binding covers every observed item, so an equal one is the
		// confirmed plan.
		if action != PrepareRollbackAction || binding != plan.Binding || now == nil {
			return staleReleaseState(request.RuntimeRoot, request.Target, "release state changed after confirmation; nothing was converted", rollbackRetry)
		}
		ctx = selectionContext(ctx, selection)
		if err := requirePreparedReleaseRoots(parsed, false); err != nil {
			return err
		}
		// The hooks and their handlers resolve under this pinned root, open
		// until the last hook and the gate return. The admission's own roots
		// are closed, so each bound yard is read again here before any
		// conversion, and must still be the confirmed instance.
		activeNow, err := runtime.verifySelectedActive(selection, &selection.activeArtifact)
		if err != nil {
			return staleRetainedSelection(request.RuntimeRoot, request.Target)
		}
		defer activeNow.Close()
		hookRoot, load := pinnedReleaseRoot(activeNow), runtime.rollbackYardLoader(request.ConfigHome)
		// Each bound yard is checked on its own; every change is reported.
		loaded, read := map[string]config.Loaded{}, map[string]bool{}
		var messages []string
		var causes []error
		for _, item := range now.pending() {
			if read[item.yard] {
				continue
			}
			read[item.yard] = true
			yard, err := load(hookRoot, item.yard)
			switch {
			case err != nil:
				message := fmt.Sprintf("yard %s: settings cannot be read again after confirmation", item.yard)
				messages, causes = append(messages, message), append(causes, fmt.Errorf("%s: %w", message, err))
			case yard.Context.AccessKind != domain.AccessLocal || yard.Context.IncusProject+"/"+yard.Context.YardInstanceName != item.instance:
				message := fmt.Sprintf("yard %s: settings no longer name the confirmed instance %s", item.yard, item.instance)
				messages, causes = append(messages, message), append(causes, errors.New(message))
			default:
				loaded[item.yard] = yard
			}
		}
		if len(messages) != 0 {
			return changedYardSettings(request, messages, causes)
		}
		var operation [12]byte
		if _, err := crand.Read(operation[:]); err != nil {
			return err
		}
		for _, item := range now.pending() {
			if err := runtime.config.ProfileRuntimeRollbackApply(ctx, hookRoot, loaded[item.yard], item.owner,
				"rollback-prep-"+hex.EncodeToString(operation[:]), item.actual, item.desired, item.consequence); err != nil {
				return partialPreparation(links, request.Target, fmt.Errorf("prepare profile runtime %s in yard %s: %w", item.owner, item.yard, err))
			}
		}
		// The gate is the second command's own admission.
		if action, _, _, err := readmit(outer); err != nil {
			return partialPreparation(links, request.Target, err)
		} else if action != rollbackAction {
			return partialPreparation(links, request.Target, errors.New("the rollback admission still requires preparation"))
		}
		prepared = true
		return nil
	}
	return plan, nil
}

// changedYardSettings reports every bound yard whose settings changed after
// confirmation, in yard order; nothing was converted. A failed read's cause
// stays out of the public message.
func changedYardSettings(request releasetransition.ProcessRequest, messages []string, causes []error) error {
	message := strings.Join(messages, "; ") + "; nothing was converted"
	if len(message) > 512 {
		message = fmt.Sprintf("the settings of %d yards changed after confirmation; nothing was converted; full details: stderr of yard update --rollback", len(messages))
	}
	outcome := observedPublicReleaseOutcome(request.RuntimeRoot, request.Target, nil, releasetransition.CodePlanStale, message, rollbackRetry)
	stale := fmt.Errorf("%w: %w", domain.ErrPlanStale, transitionOutcomeError(outcome))
	return publicReleaseInspectionError{cause: domain.CollectOrderedRefusals(append([]error{stale}, causes...)...), outcome: outcome}
}

// partialPreparation reports a failure after the first conversion could
// have run: this release stays active, a rerun resumes and yard migrate
// undoes the preparation. An owner's or refusal's own next step comes first.
func partialPreparation(links releasetransition.ReleaseLinks, target releasetransition.ReleaseID, err error) error {
	outcome := publicReleaseOutcome(links, target, nil, releasetransition.CodeRollbackIncompatible,
		fmt.Sprintf("release %s stays active and its profile runtimes may be partly prepared for %s; rerun yard update --rollback to continue, or run yard migrate to undo the preparation", links.Active, target), preparationRetry(err))
	return publicReleaseInspectionError{cause: domain.CollectOrderedRefusals(transitionOutcomeError(outcome), err), outcome: outcome}
}

// preparedDriftOnly admits a rollback from an active release whose only
// unconverged owners are those it prepares for the retained release: a
// complete journal without blockers whose other decisions only preserve.
// Yard attribution is not consulted; rollback-observe routes each yard.
func (runtime *Runtime) preparedDriftOnly(ctx context.Context, root string, protected *protectedTransitionInspection) bool {
	// The protected inspection already restored the fresh-plan status the
	// V1 wire presents as the completed transaction's pending recovery.
	inspection := protected.inspection
	if protected.journal.Checkpoint != releasetransition.JournalComplete || len(inspection.Blockers) != 0 || inspection.Resume != nil ||
		inspection.Outcome == nil || inspection.Outcome.Status != releasetransition.StatusMigrationRequired {
		return false
	}
	var drifted []string
	for _, decision := range inspection.Decisions {
		owner, activation := strings.CutPrefix(decision.Resource, "activation.")
		switch {
		case decision.Decision == releasetransition.DecisionPreserve:
		case activation && decision.Scope == "activation" && decision.Decision == releasetransition.DecisionCanonicalize:
			drifted = append(drifted, owner)
		default:
			return false
		}
	}
	links, err := runtime.inspectRuntimeLinks(root)
	if err != nil || len(drifted) == 0 || !links.current.present || !links.previous.present {
		return false
	}
	var releases []*verifiedPublishedCandidate
	for _, link := range []runtimeLinkState{links.current, links.previous} {
		candidate := publishedCandidate{release: releasetransition.ReleaseID(strings.TrimPrefix(link.target, "releases/")), root: filepath.Join(root, link.target)}
		verified, err := runtime.verifyPublishedCandidate(ctx, candidate, root, nil)
		if err != nil {
			return false
		}
		defer verified.Close()
		releases = append(releases, verified)
	}
	owners, refusal, _, _ := profileFormatAdmission(releases[0], releases[1], true, true)
	for _, owner := range drifted {
		if !slices.Contains(owners, owner) {
			return false
		}
	}
	return refusal == ""
}

func verificationOf(candidate *verifiedPublishedCandidate) candidateVerification {
	return candidateVerification{candidate: candidate.candidate, digest: candidate.manifestDigest, version: candidate.version, registryDigest: candidate.registryDigest}
}

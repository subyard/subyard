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

// PrepareRollbackAction is the first command's action.
const PrepareRollbackAction domain.ActionID = "update.prepare-rollback"

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

// profileFormatAdmission compares the runtime formats that the active
// release and a target that is not newer declare per activation ID. A
// rollback returns the owners the active release prepares for the target,
// also beside a refusal. Any other difference refuses, as do profiles that
// cannot be read.
func profileFormatAdmission(active, target *verifiedPublishedCandidate, rollback, retained bool) (owners []string, refusal, retry string, cause error) {
	activeVersion, activeErr := semver.Parse(active.version)
	targetVersion, targetErr := semver.Parse(target.version)
	if activeErr == nil && targetErr == nil && targetVersion.GT(activeVersion) {
		return nil, "", "", nil
	}
	activeDefinitions, activeErr := profile.Load(pinnedReleaseRoot(active))
	targetDefinitions, targetErr := profile.Load(pinnedReleaseRoot(target))
	if err := errors.Join(activeErr, targetErr); err != nil {
		return nil, "the profile runtime formats of release " + string(target.candidate.release) + " cannot be compared", keepActiveRetry, err
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
	if len(differing) > 4 {
		differing = append(differing[:4], fmt.Sprintf("%d more", len(differing)-4))
	}
	refusal = fmt.Sprintf("release %s declares other profile runtime formats than the active release: %s", target.candidate.release, strings.Join(differing, ", "))
	retry = keepActiveRetry
	if preparable && retained {
		retry = rollbackRetry
	}
	return owners, refusal, retry, errors.New(refusal)
}

// formatItem is one owner's rollback-observe result in one local yard.
type formatItem struct {
	owner, yard, instance string
	state                 ports.RuntimeState
	actual, desired       string
	consequence           string
}

// formatObservation collects every yard's items and independent refusals,
// in yard and owner order. loaded holds each local yard read with the
// active release.
type formatObservation struct {
	items    []formatItem
	loaded   map[string]config.Loaded
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
	return runtime.observeFormatOwners(ctx, active, target, request, selection.formatOwners)
}

// observeFormatOwners runs rollback-observe for each owner in every local
// yard. absent and stale pass; current passes once the target's own observe
// accepts the prepared state. A stopped yard or a failed observation refuses.
func (runtime *Runtime) observeFormatOwners(ctx context.Context, active, target *verifiedPublishedCandidate, request releasetransition.ProcessRequest, owners []string) (formatObservation, error) {
	observation := formatObservation{loaded: map[string]config.Loaded{}}
	observe, accept := runtime.config.ProfileRuntimeRollbackObservation, runtime.config.ProfileRuntimeObservation
	if observe == nil || accept == nil || runtime.config.ProfileRuntimeRollbackApply == nil {
		observation.refuse("profile runtime rollback preparation is unavailable", keepActiveRetry, nil)
		return observation, nil
	}
	activeRoot, targetRoot := pinnedReleaseRoot(active), pinnedReleaseRoot(target)
	environment := runtime.config.Inputs.environment(runtime.config.Context)
	environment["SUBYARD_CONFIG_HOME"] = request.ConfigHome
	operator := first(environment["SUBYARD_OPERATOR_HOME"], environment["HOME"])
	load := func(root, name string) (config.Loaded, error) {
		return config.Load(config.LoadOptions{RepositoryRoot: root, OperatorHome: operator, YardName: name, Environment: environment})
	}
	names, err := config.YardNames(request.ConfigHome)
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
		observation.loaded[name] = loaded
		stopped := "run " + domain.RenderAction(domain.Action{Verb: domain.VerbStart, Yard: name})
		for _, owner := range owners {
			state, err := observe(ctx, activeRoot, loaded, owner)
			if ctx.Err() != nil {
				return formatObservation{}, ctx.Err()
			}
			item := formatItem{owner: owner, yard: name, instance: loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName,
				state: state.State, actual: state.Actual, desired: state.Desired, consequence: state.Consequence}
			switch {
			case err != nil:
				observation.refuse(fmt.Sprintf("yard %s: profile runtime %s cannot be observed for rollback preparation", name, owner), rollbackRetry, err)
				continue
			case state.State == ports.RuntimeStateDeferred:
				observation.refuse(fmt.Sprintf("yard %s is stopped, so profile runtime %s cannot be prepared for rollback", name, owner), stopped, nil)
				continue
			case state.State == ports.RuntimeStateCurrent:
				targetLoaded, err := load(targetRoot, name)
				var accepted ports.RuntimeObservation
				if err == nil {
					accepted, err = accept(ctx, targetRoot, targetLoaded, owner)
				}
				if ctx.Err() != nil {
					return formatObservation{}, ctx.Err()
				}
				if err != nil || accepted.State == ports.RuntimeStateDeferred {
					observation.refuse(fmt.Sprintf("yard %s: release %s does not accept the prepared profile runtime %s", name, target.candidate.release, owner), migrateRetry, err)
					continue
				}
			case state.State != ports.RuntimeStateAbsent && state.State != ports.RuntimeStateStale:
				observation.refuse(fmt.Sprintf("yard %s: profile runtime %s reports no rollback preparation state", name, owner), rollbackRetry, nil)
				continue
			}
			observation.items = append(observation.items, item)
		}
	}
	return observation, nil
}

// prepareRollbackPreparation offers the conversion of the stale owners among
// the observed items as the whole plan. Its run rechecks the confirmed
// selection and observations, converts, then reruns the rollback admission
// as the gate. It keeps no record: a rerun resumes, and yard migrate undoes it.
func (runtime *Runtime) prepareRollbackPreparation(parsed options, request releasetransition.ProcessRequest, target *verifiedPublishedCandidate, items []formatItem) Prepared {
	selection := parsed.selection
	links := releaseLinksFromRuntimeSnapshot(*parsed.expectedLinks)
	activeVerification := candidateVerification{candidate: publishedCandidate{release: links.Active}, digest: selection.activeArtifact}
	targetVerification := verificationOf(target)
	steps := []domain.OperationStep{}
	consequences := []string{}
	for _, item := range items {
		if item.state != ports.RuntimeStateStale {
			continue
		}
		consequence := fmt.Sprintf("prepare profile runtime %s in yard %s for release %s", item.owner, item.yard, request.Target)
		if item.consequence != "" {
			consequence += ": " + item.consequence
		}
		step := domain.OperationStep{ID: fmt.Sprintf("release.prepare-rollback.%d", len(steps)), Target: "activation." + item.owner + " yard." + item.yard,
			Observed: "rollback-observe stale", Desired: "format of release " + string(request.Target), Decision: domain.StepApply,
			Preconditions: []string{"the confirmed selection, yard instance, rollback-observe result and effect are unchanged"},
			Verify:        "rollback-observe reports current, then the retained release accepts the owner", Consequence: consequence}
		if len(steps) != 0 {
			step.DependsOn = []string{steps[len(steps)-1].ID}
		}
		steps, consequences = append(steps, step), append(consequences, consequence)
	}
	consequences = append(consequences, fmt.Sprintf("keep release %s active; then run yard update --rollback to review and confirm the rollback", links.Active))
	prepared := false
	return Prepared{
		Request: request, Action: PrepareRollbackAction, Changed: true, Consequences: consequences,
		ActiveLauncher: filepath.Join(parsed.root, "current", "bin", "yard"),
		SourceRelease:  string(links.Active), TargetRelease: string(target.candidate.release), TargetVersion: target.version, Steps: steps,
		Binding: releaseOperationBinding(request, releasetransition.Inspection{Assessment: domain.ActionAssessment{Consequences: consequences}},
			activeVerification, targetVerification, struct {
				Links any
				Steps []domain.OperationStep
			}{parsed.expectedLinks, steps}),
		check: func(context.Context) (releasetransition.Inspection, error) {
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
		},
		run: func(ctx context.Context) error {
			ctx = selectionContext(ctx, selection)
			if err := requirePreparedReleaseRoots(parsed, false); err != nil {
				return err
			}
			targetNow, err := runtime.verifyPublishedCandidate(ctx, targetVerification.candidate, request.RuntimeRoot, &targetVerification.digest)
			if err != nil {
				return staleRetainedSelection(request.RuntimeRoot, request.Target)
			}
			defer targetNow.Close()
			unlock, err := runtime.lockReleaseSelection(ctx, false)
			if err != nil {
				return err
			}
			err = runtime.revalidateReleaseSelection(selection, targetNow)
			unlock()
			if err != nil {
				return err
			}
			activeNow, err := runtime.verifySelectedActive(selection, &selection.activeArtifact)
			if err != nil {
				return staleRetainedSelection(request.RuntimeRoot, request.Target)
			}
			defer activeNow.Close()
			observed, err := runtime.observeFormatOwners(ctx, activeNow, targetNow, request, selection.formatOwners)
			if err != nil {
				return err
			}
			if len(observed.messages) != 0 || !slices.Equal(observed.items, items) {
				return staleReleaseState(request.RuntimeRoot, request.Target, "profile runtime state changed after confirmation; nothing was converted", rollbackRetry)
			}
			var operation [12]byte
			if _, err := crand.Read(operation[:]); err != nil {
				return err
			}
			for _, item := range observed.pending() {
				if err := runtime.config.ProfileRuntimeRollbackApply(ctx, pinnedReleaseRoot(activeNow), observed.loaded[item.yard], item.owner,
					"rollback-prep-"+hex.EncodeToString(operation[:]), item.actual, item.desired, item.consequence); err != nil {
					return partialPreparation(links, request.Target, fmt.Errorf("prepare profile runtime %s in yard %s: %w", item.owner, item.yard, err))
				}
			}
			if err := runtime.inspectRollbackCompatibility(ctx, targetNow, request, selection); err != nil {
				return partialPreparation(links, request.Target, err)
			}
			prepared = true
			return nil
		},
	}
}

// partialPreparation reports a failure after the first conversion could
// have run: this release stays active, a rerun resumes and yard migrate
// undoes the preparation. A refusal's own typed next step comes first.
func partialPreparation(links releasetransition.ReleaseLinks, target releasetransition.ReleaseID, err error) error {
	retry := rollbackRetry
	if outcome, ok := TransitionOutcome(err); ok {
		if _, _, typed := domain.ParseRetry(outcome.Retry); typed {
			retry = outcome.Retry
		}
	}
	outcome := publicReleaseOutcome(links, target, nil, releasetransition.CodeRollbackIncompatible,
		fmt.Sprintf("release %s stays active and its profile runtimes may be partly prepared for %s; rerun yard update --rollback to continue, or run yard migrate to undo the preparation", links.Active, target), retry)
	return publicReleaseInspectionError{cause: domain.CollectOrderedRefusals(transitionOutcomeError(outcome), err), outcome: outcome}
}

// preparedDriftOnly admits a rollback from an active release whose only
// unconverged owners are those it prepares for the retained release: a
// complete journal without blockers whose decisions all name such owners.
func (runtime *Runtime) preparedDriftOnly(ctx context.Context, root string, protected *protectedTransitionInspection) bool {
	inspection := protected.inspection
	if protected.journal.Checkpoint != releasetransition.JournalComplete || len(inspection.Blockers) != 0 || inspection.Resume != nil ||
		inspection.Outcome == nil || inspection.Outcome.Status != releasetransition.StatusMigrationRequired {
		return false
	}
	drifted := map[string]bool{}
	for _, decision := range inspection.Decisions {
		owner, activation := strings.CutPrefix(decision.Resource, "activation.")
		attributed := decision.Scope == releasetransition.YardAttributionScope && strings.HasPrefix(decision.Result, releasetransition.YardFindingDrift+" ")
		if !activation || decision.Scope != "activation" && !attributed {
			return false
		}
		drifted[owner] = true
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
	for owner := range drifted {
		if !slices.Contains(owners, owner) {
			return false
		}
	}
	return refusal == ""
}

func verificationOf(candidate *verifiedPublishedCandidate) candidateVerification {
	return candidateVerification{candidate: candidate.candidate, digest: candidate.manifestDigest, version: candidate.version, registryDigest: candidate.registryDigest}
}

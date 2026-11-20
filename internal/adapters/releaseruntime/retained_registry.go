package releaseruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/uninstall"
	"github.com/blang/semver/v4"
)

type releaseSelectionKey struct{}
type journalGateInspectionKey struct{}

type journalGateInspection struct {
	request     releasetransition.ProcessRequest
	fingerprint releasetransition.Fingerprint
}

// releaseSelection is immutable after selection. Capturing both history records
// also pins the projection when the checkpoint is authoritative.
type releaseSelection struct {
	request                        releasetransition.ProcessRequest
	links                          runtimeLinkSnapshot
	activeArtifact, targetArtifact releasetransition.Fingerprint
	activeRegistry, targetRegistry releasetransition.Fingerprint
	state                          currentSnapshot
	draft                          os.FileInfo
	draftPath                      string
	firstPublication               bool
	refusal                        string
	retry                          string
	cause                          error

	// compatibility is the history verdict for an installed engine; a fresh
	// forward draft has its own migration planner and no verdict.
	compatibility *releasetransition.HistoryCompatibilityVerdict
}

func selectionContext(ctx context.Context, selection *releaseSelection) context.Context {
	if selection == nil {
		return ctx
	}
	return context.WithValue(ctx, releaseSelectionKey{}, selection)
}

func retainedRegistryRetry(selection *releaseSelection) string {
	if selection.retry != "" {
		return selection.retry
	}
	return "keep the current release active and resolve all reported release compatibility requirements before retrying"
}

func (runtime *Runtime) ownerDataHome() string {
	home := runtime.config.Inputs.value("SUBYARD_HOME", runtime.config.Context)
	if home == "" {
		home = filepath.Join(runtime.config.Inputs.value("HOME", runtime.config.Context), ".subyard")
	}
	return home
}

// The installation owner barrier serializes journal and runtime link writers.
// Borrow it during CLI execution, or acquire it for standalone preparation.
// The child inherits this barrier and can still acquire its own V2 store lock.
// An installation observation never waits, whichever variant its caller asks for.
func (runtime *Runtime) lockReleaseSelection(ctx context.Context, nonblocking bool) (func(), error) {
	home := runtime.ownerDataHome()
	if runtime.config.OwnerLockHeld != nil && runtime.config.OwnerLockHeld() {
		lock, err := uninstall.BorrowOwnerLock(home)
		if err != nil {
			return nil, err
		}
		if lock == nil {
			return nil, errors.New("release owner mutation lock is no longer held")
		}
		return func() { _ = lock.Close() }, nil
	}
	if nonblocking || observing(ctx) {
		_, unlock, err := uninstall.TryOwnerLock(ctx, home)
		return unlock, err
	}
	_, unlock, err := uninstall.OwnerLock(ctx, home, nil)
	return unlock, err
}

func staleRetainedSelection(root string, target releasetransition.ReleaseID) error {
	return staleReleaseState(root, target, "release state changed; retry", "run yard update --check")
}

func staleReleaseState(root string, target releasetransition.ReleaseID, message, retry string) error {
	outcome := observedPublicReleaseOutcome(root, target, nil, releasetransition.CodePlanStale,
		message, retry)
	return publicReleaseInspectionError{cause: fmt.Errorf("%w: %w", domain.ErrPlanStale, transitionOutcomeError(outcome)), outcome: outcome}
}

func (runtime *Runtime) captureReleaseSelection(ctx context.Context, target *verifiedPublishedCandidate, request releasetransition.ProcessRequest, links runtimeLinkSnapshot) (*releaseSelection, error) {
	return runtime.captureTransitionSelection(ctx, target, request, links, false)
}

func (runtime *Runtime) captureTransitionSelection(ctx context.Context, target *verifiedPublishedCandidate, request releasetransition.ProcessRequest, links runtimeLinkSnapshot, freshDraft bool) (*releaseSelection, error) {
	unlock, err := runtime.lockReleaseSelection(ctx, false)
	if err != nil {
		return nil, err
	}
	defer unlock()
	selection := &releaseSelection{request: request, links: links, targetArtifact: target.manifestDigest}
	if observed, err := runtime.inspectRuntimeLinks(request.RuntimeRoot); err != nil || observed != links {
		return nil, staleRetainedSelection(request.RuntimeRoot, request.Target)
	}
	if freshDraft {
		selection.draftPath = target.candidate.root
		selection.draft, err = target.root.Stat()
		if err != nil || !runtime.matchesReleaseDraft(selection, target) {
			return nil, staleRetainedSelection(request.RuntimeRoot, request.Target)
		}
	}
	active, err := runtime.verifySelectedActive(selection, nil)
	if err != nil {
		selection.refusal, selection.cause = "the active release's sealed migration registry cannot be verified", err
		return selection, nil
	}
	defer active.Close()
	selection.activeArtifact = active.manifestDigest
	selection.state, err = runtime.readCurrentSnapshot(request.RuntimeRoot, request.ConfigHome)
	if err != nil {
		selection.refusal, selection.cause = "the selected release cannot verify the authoritative active migration history", err
		return selection, nil
	}
	if selection.state.links != selection.links {
		return nil, staleRetainedSelection(request.RuntimeRoot, request.Target)
	}
	// A newly downloaded forward candidate has its own migration planner.
	// Registry identity is required when selecting an already installed engine.
	if freshDraft {
		return selection, nil
	}
	history, refusal, err := assembleHistorySelection(request, links, selection.state, active, target, "")
	selection.activeRegistry, selection.targetRegistry = history.activeRegistry, history.targetRegistry
	if refusal != "" {
		selection.refusal, selection.cause = refusal, err
		return selection, nil
	}
	selection.firstPublication, selection.compatibility = history.firstPublication, &history.verdict
	if history.needsSourceFloor() {
		if err := runtime.stagedSourceRefusal(ctx, selection, active); err != nil {
			return nil, err
		}
	}
	selection.refusal, selection.cause = history.refusal()
	return selection, nil
}

func solePublishedRelease(root string, target releasetransition.ReleaseID) (bool, error) {
	entries, err := os.ReadDir(filepath.Join(root, "releases"))
	if err != nil {
		return false, fmt.Errorf("cannot list published releases: %w", err)
	}
	found := false
	for _, entry := range entries {
		if entry.IsDir() {
			if entry.Name() != string(target) {
				return false, nil
			}
			found = true
		}
	}
	return found, nil
}

// Preserve the source floor's upgrade advice without admitting an already
// published engine, including candidates left by earlier bootstrap versions.
func (runtime *Runtime) stagedSourceRefusal(ctx context.Context, selection *releaseSelection, active *verifiedPublishedCandidate) error {
	request := selection.request
	if request.Direction != releasetransition.DirectionActivateTarget || request.Replacement != nil ||
		request.MigrationOwnerReplacement != nil || request.Target == active.candidate.release ||
		selection.links.previous.target == "releases/"+string(request.Target) {
		return nil
	}
	version, err := releasetransition.ReleaseIDVersion(active.candidate.release)
	if err != nil || !version.LT(semver.MustParse(UpdateAnchorVersion)) {
		return nil
	}
	source, err := runtime.verifyPublishedCandidate(ctx, active.candidate, request.RuntimeRoot, &active.manifestDigest)
	if err != nil {
		return nil
	}
	defer source.Close()
	if source.version != version.String() {
		return nil
	}
	message, retry := sourceAnchorGuidance(source.version)
	if version.LT(semver.MustParse(MinimumSourceVersion)) {
		message, retry = "the installed source predates v"+MinimumSourceVersion, LegacyReinstallGuidance
	}
	outcome := publicReleaseOutcome(releaseLinksFromRuntimeSnapshot(selection.links), request.Target, nil,
		releasetransition.CodePreconditionBlocked, message, retry)
	return publicReleaseInspectionError{cause: transitionOutcomeError(outcome), outcome: outcome}
}

func (runtime *Runtime) verifySelectedActive(selection *releaseSelection, digest *releasetransition.Fingerprint) (*verifiedPublishedCandidate, error) {
	root := selection.request.RuntimeRoot
	link := selection.links.current
	if !link.present {
		return nil, errors.New("active release is unavailable")
	}
	active := publishedCandidate{release: releaseLinksFromRuntimeSnapshot(selection.links).Active, root: filepath.Join(root, link.target)}
	return runtime.verifyPublishedCandidateFiles(active, root, digest, false)
}

func selectedRegistry(candidate *verifiedPublishedCandidate) (releasetransition.RegistryV2, releasetransition.Fingerprint, error) {
	if candidate.registryDigest == "" {
		return releasetransition.RegistryV2{}, "", nil
	}
	registry, _, err := readVerifiedTransitionRegistry(candidate)
	if err != nil {
		return registry, "", err
	}
	identity, err := registry.CanonicalDigest()
	return registry, identity, err
}

// Called under the owner barrier immediately before starting the transition child.
func (runtime *Runtime) revalidateReleaseSelection(selection *releaseSelection, target *verifiedPublishedCandidate) error {
	request := selection.request
	stale := func() error { return staleRetainedSelection(request.RuntimeRoot, request.Target) }
	if selection.refusal != "" || target.candidate.release != request.Target || target.manifestDigest != selection.targetArtifact {
		return stale()
	}
	if observed, err := runtime.inspectRuntimeLinks(request.RuntimeRoot); err != nil || observed != selection.links {
		return stale()
	}
	active, err := runtime.verifySelectedActive(selection, &selection.activeArtifact)
	if err != nil {
		return stale()
	}
	defer active.Close()
	if selection.draft != nil {
		if !runtime.matchesReleaseDraft(selection, target) {
			return stale()
		}
	} else {
		_, activeIdentity, err := selectedRegistry(active)
		if err != nil || activeIdentity != selection.activeRegistry {
			return stale()
		}
		if !runtime.installedTransitionEngine(target, request.RuntimeRoot) {
			return stale()
		}
		_, targetIdentity, err := selectedRegistry(target)
		if err != nil || targetIdentity != selection.targetRegistry {
			return stale()
		}
	}
	observed, err := runtime.readCurrentSnapshot(request.RuntimeRoot, request.ConfigHome)
	if err != nil || !sameCurrentSnapshot(observed, selection.state) {
		return stale()
	}
	if selection.firstPublication {
		sole, err := solePublishedRelease(request.RuntimeRoot, request.Target)
		if err != nil || !sole {
			return stale()
		}
	}
	return nil
}

// Only the draft we prepared, or that same directory after our publication
// rename, may use the forward path. A competing installation requires reselection.
func (runtime *Runtime) matchesReleaseDraft(selection *releaseSelection, target *verifiedPublishedCandidate) bool {
	request := selection.request
	canonical := filepath.Join(request.RuntimeRoot, "releases", string(request.Target))
	current, err := target.root.Stat()
	if err != nil || !os.SameFile(selection.draft, current) {
		return false
	}
	if runtime.relocatedCandidates[selection.draftPath] == canonical {
		return target.candidate.root == canonical && runtime.installedTransitionEngine(target, request.RuntimeRoot)
	}
	if target.candidate.root != selection.draftPath {
		return false
	}
	_, err = os.Lstat(canonical)
	return errors.Is(err, os.ErrNotExist) && runtime.installedTransitionEngine(target, filepath.Dir(filepath.Dir(selection.draftPath)))
}

// admitTransitionLaunch is the last admission check before a transition child
// starts. The caller holds the owner barrier through invocation.
func (runtime *Runtime) admitTransitionLaunch(ctx context.Context, engine *verifiedPublishedCandidate, request releasetransition.ProcessRequest) error {
	if gate := journalGateLaunch(ctx, &request); gate != nil {
		if runtime.exactJournalTransitionOwner(engine, request, gate.fingerprint) {
			return nil
		}
		return staleRetainedSelection(request.RuntimeRoot, request.Target)
	}
	if selection, _ := ctx.Value(releaseSelectionKey{}).(*releaseSelection); selection != nil {
		if request.RuntimeRoot != selection.request.RuntimeRoot || request.ConfigHome != selection.request.ConfigHome {
			return staleRetainedSelection(request.RuntimeRoot, request.Target)
		}
		if selection.refusal != "" {
			outcome := observedPublicReleaseOutcome(request.RuntimeRoot, request.Target, nil, releasetransition.CodeRollbackIncompatible,
				selection.refusal, retainedRegistryRetry(selection))
			return publicReleaseInspectionError{cause: domain.CollectRefusals(transitionOutcomeError(outcome), selection.cause), outcome: outcome}
		}
		if selection.compatibility != nil && !selection.compatibility.Admitted() {
			return staleRetainedSelection(request.RuntimeRoot, request.Target)
		}
		return runtime.revalidateReleaseSelection(selection, engine)
	}
	if current, err := inspectRuntimeLink(request.RuntimeRoot, "current"); err == nil &&
		current.target == "releases/"+string(engine.candidate.release) && runtime.installedTransitionEngine(engine, request.RuntimeRoot) {
		return nil
	}
	if runtime.exactJournalTransitionOwner(engine, request, "") {
		return nil
	}
	return staleRetainedSelection(request.RuntimeRoot, request.Target)
}

func journalGateLaunch(ctx context.Context, request *releasetransition.ProcessRequest) *journalGateInspection {
	gate, _ := ctx.Value(journalGateInspectionKey{}).(*journalGateInspection)
	if gate != nil && gate.fingerprint != "" && request != nil &&
		request.Mode == releasetransition.ProcessInspect && request.Execution == nil {
		return gate
	}
	return nil
}

func (runtime *Runtime) installedTransitionEngine(engine *verifiedPublishedCandidate, runtimeRoot string) bool {
	installed, err := runtime.verifyPublishedCandidateFiles(engine.candidate, runtimeRoot, &engine.manifestDigest, false)
	if err != nil {
		return false
	}
	defer installed.Close()
	pinned, pinnedErr := engine.root.Stat()
	current, currentErr := installed.root.Stat()
	return pinnedErr == nil && currentErr == nil && os.SameFile(pinned, current)
}

func (runtime *Runtime) exactJournalTransitionOwner(engine *verifiedPublishedCandidate, request releasetransition.ProcessRequest, expected releasetransition.Fingerprint) bool {
	if request.Replacement != nil || request.MigrationOwnerReplacement != nil || request.ConfigHome == "" {
		return false
	}
	store, err := releasetransition.NewPOSIXV2Store(request.ConfigHome)
	if err != nil {
		return false
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil || !snapshot.Exists || (expected != "" && snapshot.Fingerprint != expected) {
		return false
	}
	journal, err := releasetransition.ParseJournal(snapshot.Payload)
	if err != nil || journal.Goal.Target != request.Target || journal.Goal.Direction != request.Direction ||
		journal.ArtifactDigest != request.ArtifactDigest || journal.RegistryDigest != request.RegistryDigest ||
		journal.RegistryDigest != engine.registryDigest || journal.CheckIngressSupport() != nil || store.ValidateCurrentRecovery(journal) != nil {
		return false
	}
	if !runtime.installedTransitionEngine(engine, request.RuntimeRoot) {
		return false
	}
	if engine.candidate.release == journal.Goal.Target {
		return engine.manifestDigest == journal.ArtifactDigest &&
			engine.candidate.root == filepath.Join(request.RuntimeRoot, "releases", string(journal.Goal.Target))
	}
	// Historical rollback can bind the source engine to a registryless target.
	if journal.Goal.Direction != releasetransition.DirectionActivatePrevious || engine.candidate.release != journal.Releases.From {
		return false
	}
	target := publishedCandidate{release: journal.Goal.Target, root: filepath.Join(request.RuntimeRoot, "releases", string(journal.Goal.Target))}
	verifiedTarget, err := runtime.verifyPublishedCandidateFiles(target, request.RuntimeRoot, &journal.ArtifactDigest, false)
	if err != nil {
		return false
	}
	defer verifiedTarget.Close()
	return verifiedTarget.registryDigest != journal.RegistryDigest &&
		engine.candidate.root == filepath.Join(request.RuntimeRoot, "releases", string(journal.Releases.From))
}

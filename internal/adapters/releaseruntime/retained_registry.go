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
)

type releaseSelectionKey struct{}

// releaseSelection is immutable after selection. Capturing both history records
// also pins the projection when the checkpoint is authoritative.
type releaseSelection struct {
	request                        releasetransition.ProcessRequest
	links                          runtimeLinkSnapshot
	activeArtifact, targetArtifact releasetransition.Fingerprint
	activeRegistry, targetRegistry releasetransition.Fingerprint
	state                          currentSnapshot
	refusal                        string
	cause                          error
}

func selectionContext(ctx context.Context, selection *releaseSelection) context.Context {
	if selection == nil {
		return ctx
	}
	return context.WithValue(ctx, releaseSelectionKey{}, selection)
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
func (runtime *Runtime) lockReleaseSelection(ctx context.Context) (func(), error) {
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
	_, unlock, err := uninstall.OwnerLock(ctx, home, nil)
	return unlock, err
}

func staleRetainedSelection(root string, target releasetransition.ReleaseID) error {
	outcome := observedPublicReleaseOutcome(root, target, nil, releasetransition.CodePlanStale,
		"release state changed; retry", "run yard update --check")
	return publicReleaseInspectionError{cause: fmt.Errorf("%w: %w", domain.ErrPlanStale, transitionOutcomeError(outcome)), outcome: outcome}
}

func (runtime *Runtime) captureReleaseSelection(ctx context.Context, target *verifiedPublishedCandidate, request releasetransition.ProcessRequest, links runtimeLinkSnapshot) (*releaseSelection, error) {
	unlock, err := runtime.lockReleaseSelection(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	selection := &releaseSelection{request: request, links: links, targetArtifact: target.manifestDigest}
	if observed, err := runtime.inspectRuntimeLinks(request.RuntimeRoot); err != nil || observed != links {
		return nil, staleRetainedSelection(request.RuntimeRoot, request.Target)
	}
	active, err := runtime.verifySelectedActive(selection, nil)
	if err != nil {
		selection.refusal, selection.cause = "the active release's sealed migration registry cannot be verified", err
		return selection, nil
	}
	defer active.Close()
	selection.activeArtifact = active.manifestDigest
	_, selection.activeRegistry, err = selectedRegistry(active)
	if err != nil {
		selection.refusal, selection.cause = "the active release's sealed migration registry cannot be verified", err
		return selection, nil
	}
	retained, identity, err := selectedRegistry(target)
	if err != nil {
		selection.refusal, selection.cause = "the selected release's sealed migration registry cannot be verified", err
		return selection, nil
	}
	selection.targetRegistry = identity
	selection.state, err = runtime.readCurrentSnapshot(request.RuntimeRoot, request.ConfigHome)
	if err != nil {
		selection.refusal, selection.cause = "the selected release cannot verify the authoritative active migration history", err
		return selection, nil
	}
	if selection.state.links != selection.links {
		return nil, staleRetainedSelection(request.RuntimeRoot, request.Target)
	}
	if selection.activeRegistry != selection.targetRegistry {
		selection.refusal = "the selected release's migration registry differs from the active release's registry"
		return selection, nil
	}
	if identity != "" {
		selection.refusal, selection.cause = verifyRetainedHistory(target, retained, selection.state.ledger, selection.state.checkpoint)
	}
	return selection, nil
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
	_, activeIdentity, err := selectedRegistry(active)
	if err != nil || activeIdentity != selection.activeRegistry {
		return stale()
	}
	// The draft and its published destination share the same selected manifest.
	targetRoot := filepath.Dir(filepath.Dir(target.candidate.root))
	installed, err := runtime.verifyPublishedCandidateFiles(target.candidate, targetRoot, &selection.targetArtifact, false)
	if err != nil {
		return stale()
	}
	defer installed.Close()
	pinnedInfo, pinnedErr := target.root.Stat()
	installedInfo, installedErr := installed.root.Stat()
	if pinnedErr != nil || installedErr != nil || !os.SameFile(pinnedInfo, installedInfo) {
		return stale()
	}
	_, targetIdentity, err := selectedRegistry(installed)
	if err != nil || targetIdentity != selection.targetRegistry {
		return stale()
	}
	observed, err := runtime.readCurrentSnapshot(request.RuntimeRoot, request.ConfigHome)
	if err != nil || !sameCurrentSnapshot(observed, selection.state) {
		return stale()
	}
	return nil
}

func verifyRetainedHistory(target *verifiedPublishedCandidate, registry releasetransition.RegistryV2, ledger, checkpoint releasetransition.ProtectedSnapshot) (string, error) {
	state, err := releasetransition.ParseMigrationLedger(ledger, checkpoint, registry, target.registryDigest)
	if err != nil {
		return "the selected release cannot verify the authoritative active migration history", err
	}
	if !state.AuthoritySnapshot.Exists {
		return "the authoritative active migration history is missing", nil
	}
	if state.Checkpointed {
		reader, err := verifiedMigrationCheckpointReader(target)
		if err != nil {
			return "the selected release's migration checkpoint capability cannot be verified", err
		}
		if !reader {
			if _, _, err := releasetransition.ParseLedgerV2(ledger.Payload, registry); err != nil {
				return "the retained release cannot read the legacy migration ledger projection", err
			}
			if state.DiffersFromProjection(registry) {
				return "the retained release requires a legacy ledger that matches the authoritative migration checkpoint", nil
			}
		}
	}
	return "", nil
}

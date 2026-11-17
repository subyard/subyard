package releaseruntime

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

// verifyRetainedSelection checks the link again against the already verified
// artifact. The pinned directory and manifest must still be the installed ones.
func verifyRetainedSelection(root string, target *verifiedPublishedCandidate, selected releasetransition.Fingerprint) error {
	if selected == "" {
		return nil
	}
	previous, err := inspectRuntimeLink(root, "previous")
	if err != nil || previous.target != "releases/"+string(target.candidate.release) || target.manifestDigest != selected {
		return staleRetainedSelection(root, target.candidate.release)
	}
	installed, err := openVerifiedCandidateRoot(target.candidate, root)
	if err != nil {
		return staleRetainedSelection(root, target.candidate.release)
	}
	defer installed.Close()
	installedInfo, installedErr := installed.Stat()
	pinnedInfo, pinnedErr := target.root.Stat()
	if installedErr != nil || pinnedErr != nil || !os.SameFile(installedInfo, pinnedInfo) {
		return staleRetainedSelection(root, target.candidate.release)
	}
	file, err := openCandidateFile(int(installed.Fd()), "runtime-files.sha256")
	if err != nil {
		return staleRetainedSelection(root, target.candidate.release)
	}
	defer file.Close()
	payload, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
	if err != nil || releasetransition.Fingerprint(fmt.Sprintf("%x", sha256.Sum256(payload))) != selected {
		return staleRetainedSelection(root, target.candidate.release)
	}
	return nil
}

func staleRetainedSelection(root string, target releasetransition.ReleaseID) error {
	outcome := observedPublicReleaseOutcome(root, target, nil, releasetransition.CodePlanStale,
		"the selected retained release no longer matches the previous runtime", "run yard update --check")
	return publicReleaseInspectionError{cause: fmt.Errorf("%w: %w", domain.ErrPlanStale, transitionOutcomeError(outcome)), outcome: outcome}
}

func (runtime *Runtime) inspectRetainedRegistry(ctx context.Context, target *verifiedPublishedCandidate, request releasetransition.ProcessRequest, links runtimeLinkSnapshot) ([]string, error) {
	const sourceRefusal = "the active release's sealed migration registry cannot be verified"
	if !links.current.present {
		return []string{sourceRefusal}, nil
	}
	root := filepath.Join(request.RuntimeRoot, links.current.target)
	source, err := runtime.verifyPublishedCandidate(ctx, publishedCandidate{release: releaseLinksFromRuntimeSnapshot(links).Active, root: root}, request.RuntimeRoot, nil)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return []string{sourceRefusal}, err
	}
	defer source.Close()
	if source.registryDigest == "" && target.registryDigest == "" {
		return nil, nil
	}
	const incompatible = "the retained release's migration registry differs from the active release's registry"
	if source.registryDigest == "" || target.registryDigest == "" {
		return []string{incompatible + "; only one release has a sealed migration registry"}, nil
	}
	registry, _, err := readVerifiedTransitionRegistry(source)
	if err != nil {
		return []string{sourceRefusal}, err
	}
	retained, retainedDigest, err := readVerifiedTransitionRegistry(target)
	if err != nil {
		return []string{"the retained release's sealed migration registry cannot be verified"}, err
	}
	activeIdentity, err := registry.CanonicalDigest()
	if err != nil {
		return []string{sourceRefusal}, err
	}
	retainedIdentity, err := retained.CanonicalDigest()
	if err != nil {
		return []string{"the retained release's sealed migration registry cannot be verified"}, err
	}
	if activeIdentity != retainedIdentity {
		return []string{incompatible}, nil
	}
	return verifyRetainedHistory(target, retained, retainedDigest, request.ConfigHome)
}

func verifyRetainedHistory(target *verifiedPublishedCandidate, registry releasetransition.RegistryV2, digest releasetransition.Fingerprint, configHome string) ([]string, error) {
	const unreadable = "the retained release cannot verify the authoritative active migration history"
	store, err := releasetransition.NewPOSIXV2Store(configHome)
	if err != nil {
		return []string{unreadable}, err
	}
	state, err := store.ReadMigrationLedger(registry, digest)
	if err != nil {
		return []string{unreadable}, err
	}
	if !state.AuthoritySnapshot.Exists {
		return []string{"the authoritative active migration history is missing"}, nil
	}
	if state.Checkpointed {
		reader, err := verifiedMigrationCheckpointReader(target)
		if err != nil {
			return []string{"the retained release's migration checkpoint capability cannot be verified"}, err
		}
		if !reader {
			if _, _, err := releasetransition.ParseLedgerV2(state.ProjectionSnapshot.Payload, registry); err != nil {
				return []string{"the retained release cannot read the legacy migration ledger projection"}, err
			}
			if state.DiffersFromProjection(registry) {
				return []string{"the retained release requires a legacy ledger that matches the authoritative migration checkpoint"}, nil
			}
		}
	}
	return nil, nil
}

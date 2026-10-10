package releaseruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/shellquote"
	"github.com/blang/semver/v4"
)

// The published v0.17.3 engine reopens settings inspection while resuming an
// activation-only journal. A verified newer engine can resume that exact work
// with the original runtime assets and durable authorization.
func isV0173ActivationOnlyBlocker(protected *protectedTransitionInspection) bool {
	if protected == nil || protected.target.version != "0.17.3" ||
		protected.owner != protected.target || !protected.activationReconciliationOwned {
		return false
	}
	journal, inspection := protected.journal, protected.inspection
	return journal.Checkpoint == releasetransition.JournalReconciling &&
		journal.Releases.From == journal.Releases.Target &&
		journal.Releases.Target == journal.Goal.Target &&
		journal.Goal.Direction == releasetransition.DirectionActivateTarget &&
		journal.SourceIngress == nil && len(journal.Steps) == 0 &&
		journal.CatalogDigest == releasetransition.BuiltinCapabilityCatalog().Digest() &&
		inspection.Resume != nil && *inspection.Resume == journal.Transaction &&
		inspection.Plan == journal.ResumePlan && inspection.Outcome != nil &&
		inspection.Outcome.Active == journal.Goal.Target &&
		inspection.Outcome.Status == releasetransition.StatusOperatorActionRequired &&
		inspection.Outcome.Code == releasetransition.CodePreconditionBlocked &&
		len(inspection.Blockers) == 1 &&
		inspection.Blockers[0].Code == releasetransition.CodePreconditionBlocked &&
		strings.HasPrefix(inspection.Blockers[0].Resource, "yard.") &&
		domain.SafeName(strings.TrimPrefix(inspection.Blockers[0].Resource, "yard.")) &&
		inspection.Blockers[0].Message == "the yard template is not supported by this migration"
}

func (runtime *Runtime) setV0173StandaloneRetry(ctx context.Context, root string, protected *protectedTransitionInspection) {
	if !isV0173ActivationOnlyBlocker(protected) {
		return
	}
	retry := "publish a verified standalone release newer than v0.17.3, then run its bin/yard migrate --check and bin/yard migrate before retrying yard update"
	if candidate, available := runtime.verifiedStandaloneCandidateRoot(ctx, root); available {
		defer candidate.Close()
		version, err := semver.Parse(candidate.version)
		if err == nil && version.GT(semver.MustParse("0.17.3")) && candidate.registryDigest == protected.journal.RegistryDigest {
			launcher := shellquote.Word(filepath.Join(candidate.candidate.root, "bin", "yard"))
			retry = fmt.Sprintf("run %s migrate --check, then %s migrate; retry yard update after recovery", launcher, launcher)
		}
	}
	protected.inspection.Blockers[0].Retry = retry
	protected.inspection.Outcome.Retry = retry
}

func (runtime *Runtime) inspectV0173ActivationOnlyRecovery(
	ctx context.Context,
	root string,
	before currentSnapshot,
	protected *protectedTransitionInspection,
) (*protectedTransitionInspection, *candidateVerification) {
	if !isV0173ActivationOnlyBlocker(protected) || !before.ledger.Exists {
		return protected, nil
	}
	domains, err := runtime.currentDomains(ctx, root, protected.owner, before)
	if err != nil {
		return protected, nil
	}
	for _, state := range domains {
		if len(state.Pending) != 0 || state.Epoch != state.RequiredEpoch {
			return protected, nil
		}
	}
	delegate, available := runtime.verifiedStandaloneCandidateRoot(ctx, root)
	if !available {
		return protected, nil
	}
	defer delegate.Close()
	version, err := semver.Parse(delegate.version)
	if err != nil || !version.GT(semver.MustParse("0.17.3")) ||
		delegate.registryDigest != protected.journal.RegistryDigest ||
		!strings.HasPrefix(string(delegate.candidate.release), delegate.version+"-") {
		return protected, nil
	}
	assets, err := runtime.verifyPublishedCandidate(ctx, protected.owner.candidate, root, &protected.owner.digest)
	if err != nil {
		return protected, nil
	}
	defer assets.Close()
	response, err := runtime.invokeVerifiedRuntimeTransition(ctx, assets, delegate, protected.request, "")
	if err != nil || response.Inspection == nil || response.Outcome != nil || !response.ActivationReconciliationOwned {
		return protected, nil
	}
	inspection := *response.Inspection
	if err := releasetransition.ValidateProcessInspection(protected.journal.Goal, inspection); err != nil ||
		len(inspection.Blockers) != 0 || inspection.Resume == nil ||
		*inspection.Resume != protected.journal.Transaction || inspection.Plan != protected.journal.ResumePlan ||
		inspection.Outcome.Status != releasetransition.StatusRecovering ||
		inspection.Outcome.Transaction == nil || *inspection.Outcome.Transaction != protected.journal.Transaction {
		return protected, nil
	}
	recovered := *protected
	recovered.inspection = inspection
	receipt := candidateVerification{
		candidate: delegate.candidate, digest: delegate.manifestDigest,
		version: delegate.version, registryDigest: delegate.registryDigest,
	}
	return &recovered, &receipt
}

func (runtime *Runtime) invokeTransitionExecution(
	ctx context.Context,
	assets *verifiedPublishedCandidate,
	delegate *candidateVerification,
	request releasetransition.ProcessRequest,
	grant releasetransition.Authorization,
) (releasetransition.ProcessResponse, error) {
	if delegate == nil {
		return runtime.invokeVerifiedCandidateTransition(ctx, assets, request, grant)
	}
	verified, err := runtime.verifyPublishedCandidate(ctx, delegate.candidate, request.RuntimeRoot, &delegate.digest)
	if err != nil {
		return releasetransition.ProcessResponse{}, fmt.Errorf("reverify recovery engine: %w", err)
	}
	defer verified.Close()
	if verified.version != delegate.version || verified.registryDigest != delegate.registryDigest {
		return releasetransition.ProcessResponse{}, fmt.Errorf("%w: recovery engine changed after inspection", domain.ErrPlanStale)
	}
	return runtime.invokeVerifiedRuntimeTransition(ctx, assets, verified, request, grant)
}

func delegatedReleaseOperationBinding(binding string, delegate *candidateVerification) string {
	if delegate == nil {
		return binding
	}
	payload, err := json.Marshal(struct {
		Binding, Root, Version string
		Digest, Registry       releasetransition.Fingerprint
	}{binding, delegate.candidate.root, delegate.version, delegate.digest, delegate.registryDigest})
	if err != nil {
		panic("recovery engine binding must be encodable")
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

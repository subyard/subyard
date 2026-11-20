package releaseruntime

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/shellquote"
	"github.com/blang/semver/v4"
)

// Preserve the published v0.17.3 recovery diagnostic for ordinary owner resume.
func isV0173ActivationOnlyBlocker(protected *protectedTransitionInspection) bool {
	if protected == nil || protected.target.version != "0.17.3" ||
		protected.owner != protected.target || !protected.activationReconciliationOwned {
		return false
	}
	journal, inspection := protected.journal, protected.inspection
	_, supportedCatalog := releasetransition.SupportedBuiltinCatalog(journal.CatalogDigest)
	return journal.Checkpoint == releasetransition.JournalReconciling &&
		journal.Releases.From == journal.Releases.Target &&
		journal.Releases.Target == journal.Goal.Target &&
		journal.Goal.Direction == releasetransition.DirectionActivateTarget &&
		journal.SourceIngress == nil && len(journal.Steps) == 0 &&
		supportedCatalog &&
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
	retry := "publish a verified historical Go standalone bridge newer than v0.17.3 with the exact original migration registry and catalog, then run its bin/yard migrate --check and bin/yard migrate before retrying yard update"
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

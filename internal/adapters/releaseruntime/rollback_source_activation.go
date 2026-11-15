package releaseruntime

import (
	"context"
	"errors"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

// The frozen inspection vocabulary distinguishes source mutations from ledger
// advancement and activation. A migration-required status alone does not.
func rollbackHasSourceWork(inspection releasetransition.Inspection) bool {
	for _, decision := range inspection.Decisions {

		switch decision.Decision {
		case releasetransition.DecisionCanonicalize, releasetransition.DecisionReset,
			releasetransition.DecisionTransform, releasetransition.DecisionQuarantine:
			// These producers declare metadata/identity/activation work only.
			// Other mutating scopes (including yard.<name>) must prove the
			// capability; future source scopes cannot silently bypass it.
			switch decision.Scope {
			case "activation", "power-metadata", "migration-history", "legacy-v1":
				continue
			default:
				return true
			}
		case releasetransition.DecisionPreserve, releasetransition.DecisionBlock:
			continue
		default:
			return true
		}
	}
	return false
}

func (runtime *Runtime) inspectRollbackSourceActivation(ctx context.Context, owner, target *verifiedPublishedCandidate,
	request releasetransition.ProcessRequest, inspection releasetransition.Inspection,
) (*releasetransition.Outcome, error) {
	if request.Direction != releasetransition.DirectionActivatePrevious || target.registryDigest == "" ||
		owner.candidate.release != target.candidate.release || inspection.Resume != nil || len(inspection.Blockers) != 0 {
		return nil, nil
	}
	required := rollbackHasSourceWork(inspection)
	var err error
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if err == nil && !required {
		return nil, nil
	}
	if err == nil {
		_, err = runtime.invokeVerifiedRecoveryTransition(ctx, owner, request.RuntimeRoot, releasetransition.RecoveryProcessRequest{
			SchemaVersion: releasetransition.ProcessRecoverySchemaV2,
			Mode:          releasetransition.RecoveryProcessRollbackSourceCapabilities,
		}, "")
	}
	if err == nil {
		return nil, nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, err
	}
	outcome := publicReleaseOutcome(releasetransition.ReleaseLinks{Active: inspection.Outcome.Active, Previous: inspection.Outcome.Previous},
		request.Target, nil, releasetransition.CodeRollbackIncompatible,
		"the retained release cannot safely bind pending source work to rollback activation",
		"run yard migrate to complete current-release migrations, then retry yard update --rollback; otherwise retain a release with compatible rollback source planning")
	return &outcome, nil
}

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

// These original producers declare every source mutation in Decisions; pending
// metadata-only edges do not require another capability. New kinds stay opaque
// until their inspection contract is explicitly established here.
func rollbackSourceDecisionsComplete(kind string) bool {
	switch kind {
	case "test-vms-settings-v1-to-v2", "test-yard-owner-v1-to-v2",
		"local-ssh-port-settings-v2-to-v3", "owner-publication-state-v1":
		return true
	default:
		return false
	}
}

func rollbackHasPendingOpaqueWork(ctx context.Context, target *verifiedPublishedCandidate, configHome string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	registry, digest, err := readVerifiedTransitionRegistry(target)
	if err != nil {
		return false, err
	}
	opaque := false
	for _, migration := range registry.Migrations {
		opaque = opaque || !rollbackSourceDecisionsComplete(migration.Kind)
	}
	if !opaque {
		return false, nil
	}
	store, err := releasetransition.NewPOSIXV2Store(configHome)
	if err != nil {
		return false, err
	}
	state, err := store.ReadMigrationLedger(registry, digest)
	if err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	pending, err := registry.PendingPath(state.Ledger)
	if err != nil {
		return false, err
	}
	for _, migration := range pending {
		if !rollbackSourceDecisionsComplete(migration.Kind) {
			return true, nil
		}
	}
	return false, nil
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
	if !required {
		required, err = rollbackHasPendingOpaqueWork(ctx, target, request.ConfigHome)
	}
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

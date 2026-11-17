package releasetransition

import (
	"context"
	"fmt"

	"github.com/Subyard/Subyard/internal/migrationhistory"
)

// rollbackHistoryPlan is an assessment of history and inverse work. Publication
// still requires the selected journal and verified step evidence.
type rollbackHistoryPlan struct {
	edges    []migrationhistory.ReverseEdge
	work     []v2Work
	native   MigrationLedgerSnapshot
	blockers []Blocker
}

func rollbackHistoryBlocker(resource, message string) Blocker {
	return Blocker{
		Code: CodeRollbackIncompatible, Resource: resource, Message: message,
		Retry: "keep the current release active and select a compatible retained release",
	}
}

func (transition *V2Transition) assessRollbackHistory(ctx context.Context, origin rollbackHistoryOrigin) (rollbackHistoryPlan, error) {
	source, target, initial, err := origin.inspect()
	if err != nil {
		return rollbackHistoryPlan{}, err
	}
	if transition.registryDigest != origin.Owner.SourceRegistry || transition.options.ConfigHome != origin.Owner.ConfigHome || !releasePairsEqual(transition.options.Releases, origin.Owner.Releases) {
		return rollbackHistoryPlan{}, invalid("rollback assessment belongs to another source")
	}
	owner, authority, err := source.historyAuthority(initial.Ledger)
	if err != nil {
		return rollbackHistoryPlan{}, err
	}
	epochs, err := rollbackTargetEpochs(source, target)
	if err != nil {
		return rollbackHistoryPlan{}, err
	}
	edges, err := owner.RewindPath(authority, epochs)
	if err != nil {
		return rollbackHistoryPlan{blockers: []Blocker{rollbackHistoryBlocker("migration-history.checkpoint", "the checkpoint cannot reach the retained release's native migration frontier")}}, nil
	}
	plan := rollbackHistoryPlan{edges: edges}
	policies := builtinRollbackPolicies()
	for _, edge := range edges {
		id := edge.Migration().ID
		migration, registered := transition.migration(id)
		definition, compiled := compiledMigration(migration)
		policy, covered := policies[id]
		reason := ""
		switch {
		case !registered || !compiled || !covered || !sameMigrationDefinition(migration, definition.migration):
			reason = "no exact compiled rollback policy"
		case policy.class == rollbackIrreversible:
			reason = policy.reason
		case policy.class == rollbackRewindOnly:
			if err := policy.verify(transition, ctx); err != nil {
				reason = "the backward compatibility predicate could not be verified"
			}
		case policy.class == rollbackReversible:
			work, err := policy.reverse(transition, ctx, migration)
			if err != nil || len(work) == 0 {
				reason = "the typed inverse could not be assessed"
			} else {
				plan.work = append(plan.work, work...)
			}
		default:
			reason = "unsupported compiled rollback policy"
		}
		if err := ctx.Err(); err != nil {
			return rollbackHistoryPlan{}, err
		}
		if reason != "" {
			plan.blockers = append(plan.blockers, rollbackHistoryBlocker(id, fmt.Sprintf("cannot rewind migration %s: %s", id, reason)))
		}
	}

	// Preview intermediate cuts under S. Only the final checkpoint needs to
	// have R's native shape, including when no applied edge needs rewinding.
	state := initial
	for _, edge := range edges {
		payload, rewindErr := rewindCheckpointPayload(state, source, origin.Owner.SourceRegistry, source, origin.Owner.SourceRegistry, edge)
		if rewindErr != nil {
			err = rewindErr
			break
		}
		state, err = ParseMigrationLedger(state.ProjectionSnapshot, protectedSnapshotFromPayload(payload), source, origin.Owner.SourceRegistry)
		if err != nil {
			break
		}
	}
	if err == nil {
		var payload []byte
		payload, err = nativeRollbackCheckpointPayload(state, source, origin.Owner.SourceRegistry, target, origin.Owner.TargetRegistry)
		if err == nil {
			plan.native, err = ParseMigrationLedger(state.ProjectionSnapshot, protectedSnapshotFromPayload(payload), target, origin.Owner.TargetRegistry)
		}
	}
	if err != nil {
		plan.blockers = append(plan.blockers, rollbackHistoryBlocker("migration-history.checkpoint", "the pinned checkpoint and projection cannot represent the retained release's native history"))
	}
	return plan, nil
}

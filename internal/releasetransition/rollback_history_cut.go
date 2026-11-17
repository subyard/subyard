package releasetransition

import "github.com/Subyard/Subyard/internal/migrationhistory"

const rollbackSourcePlanStep = "rollback-source-plan-v1"

// rollbackHistoryProgress describes the checked history prefix, not resource
// readiness. An uncertain effect must be resolved through its owning V2 work
// before this progress can be consumed as an abort cut.
type rollbackHistoryProgress struct {
	ledger          LedgerV2
	remaining       []migrationhistory.ReverseEdge
	needsResolution bool
}

func rollbackTargetEpochs(source, target RegistryV2) (map[string]int, error) {
	epochs := make(map[string]int, len(source.CurrentEpochs))
	for name := range source.CurrentEpochs {
		epoch, exists := target.CurrentEpochs[name]
		if !exists {
			if source.MinimumEpochs[name] != 1 || source.RetiredPrefixes[name].Digest != "" {
				return nil, invalid("rollback cannot omit a retired or non-baseline source domain")
			}
			epoch = 1
		}
		epochs[name] = epoch
	}
	for name, epoch := range target.CurrentEpochs {
		left, err := source.MigrationPrefixDigest(name, epoch)
		if err != nil {
			return nil, err
		}
		right, err := target.MigrationPrefixDigest(name, epoch)
		if err != nil || left != right {
			return nil, invalid("rollback target changes a retained migration prefix")
		}
	}
	return epochs, nil
}

func inspectRollbackHistoryProgress(source, target RegistryV2, initial LedgerV2, journal JournalRecord) (rollbackHistoryProgress, error) {
	if err := journal.Validate(); err != nil {
		return rollbackHistoryProgress{}, err
	}
	if journal.Goal.Direction != DirectionActivatePrevious || journal.SourceIngress != nil || IsRecoveryTransaction(journal.Transaction) {
		return rollbackHistoryProgress{}, invalid("history cut is not owned by a native rollback")
	}
	owner, authority, err := source.historyAuthority(initial)
	if err != nil {
		return rollbackHistoryProgress{}, err
	}
	epochs, err := rollbackTargetEpochs(source, target)
	if err != nil {
		return rollbackHistoryProgress{}, err
	}
	edges, err := owner.RewindPath(authority, epochs)
	if err != nil {
		return rollbackHistoryProgress{}, err
	}
	position := rollbackHistoryProgress{ledger: ledgerFromAuthority(authority), remaining: edges}
	index := 0
	groupVerified, groupUntouched := true, true
	groupRows := 0
	stopped := false
	sawWork, finalized := false, false
	for _, step := range journal.Steps {
		switch step.ID {
		case RollbackSourceOwnerStep, rollbackSourcePlanStep, RollbackTargetActivationStep:
			if sawWork || finalized || step.Migration != step.ID || step.Decision != DecisionPreserve || step.Expected != step.Desired {
				return position, invalid("rollback prologue differs from its bound history plan")
			}
			continue
		case rollbackNativeCheckpointStep:
			if step.Migration != step.ID || step.Resource != "ledger.native-target" || step.Decision != DecisionTransform {
				return position, invalid("rollback finalization row has invalid grammar")
			}
			if finalized || index != len(edges) || stopped && step.Checkpoint != StepIntent {
				return position, invalid("rollback finalization precedes unfinished reverse edges")
			}
			finalized = true
			continue
		}
		sawWork = true
		if finalized || index >= len(edges) || step.Migration != edges[index].Migration().ID {
			return position, invalid("rollback work differs from reverse dependency order")
		}
		edge := edges[index]
		migration := edge.Migration()
		if stopped && step.Checkpoint != StepIntent {
			return position, invalid("rollback work advanced beyond its first unfinished boundary")
		}
		if step.ID != migration.ID+".rewind-ledger" {
			groupRows++
			groupVerified = groupVerified && step.Checkpoint == StepVerified
			groupUntouched = groupUntouched && step.Checkpoint == StepIntent
			continue
		}
		_, before, err := MarshalLedgerV2(ledgerFromAuthority(edge.Before()), source)
		if err != nil {
			return position, err
		}
		_, after, err := MarshalLedgerV2(ledgerFromAuthority(edge.After()), source)
		if err != nil {
			return position, err
		}
		if groupRows == 0 || step.Resource != "ledger."+migration.Domain || step.Decision != DecisionTransform || step.Expected != before || step.Desired != after || step.Evidence != nil && step.Evidence.Recovery != "" {
			return position, invalid("rollback ledger row differs from its checked reverse edge")
		}
		if step.Checkpoint == StepVerified {
			if stopped || !groupVerified {
				return position, invalid("rollback ledger advanced before verified inverse work")
			}
			position.ledger = ledgerFromAuthority(edge.After())
			position.remaining = edges[index+1:]
		} else {
			stopped = true
			position.needsResolution = position.needsResolution || !groupUntouched || step.Checkpoint != StepIntent
		}
		index++
		groupVerified, groupUntouched = true, true
		groupRows = 0
	}
	if index != len(edges) || !groupVerified || !groupUntouched {
		return position, invalid("rollback history plan omits a reverse ledger boundary")
	}
	return position, nil
}

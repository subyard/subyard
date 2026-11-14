package cli

import (
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

// A completed owner's pending configuration, such as integration configuration
// or a yard-scoped hook failure awaiting init, remains visible in readiness. It
// cannot prevent unrelated commands from reaching their own guards. No
// unfinished transaction, source/history blocker, or untyped failure qualifies.
func pendingIntegrationConfigurationShape(outcome *releasetransition.Outcome, inspection *releasetransition.Inspection) bool {
	if outcome == nil || inspection == nil || inspection.Outcome != outcome ||
		outcome.ReachedGoal || outcome.Active == "" || outcome.Active != outcome.Target || inspection.Resume != nil ||
		outcome.Status != releasetransition.StatusOperatorActionRequired || outcome.Code != releasetransition.CodeDependencyUnavailable || len(inspection.Blockers) == 0 {
		return false
	}
	for _, blocker := range inspection.Blockers {
		if blocker.Code != releasetransition.CodeDependencyUnavailable || !pendingConfigurationResource(blocker.Resource) {
			return false
		}
	}
	return true
}

// pendingConfigurationResource accepts only the typed resource of one owner:
// activation.<owner>.pending-configuration, with no other qualifier.
func pendingConfigurationResource(resource string) bool {
	owner, found := strings.CutPrefix(resource, "activation.")
	if !found {
		return false
	}
	owner, found = strings.CutSuffix(owner, ".pending-configuration")
	return found && domain.SafeID(owner) && !strings.Contains(owner, ".")
}

func ordinaryCommandDuringPendingConfiguration(handler string) bool {
	switch handler {
	case "@init", "@config", "@integration", "@profile", "@provision", "@update", "@migrate", "@release-transition":
		return false
	default:
		return true
	}
}

// The transaction on an operator outcome can name completed history. Its mere
// presence cannot decide admission: read the protected journal and match it.
func (cli *CLI) pendingIntegrationConfigurationGate(outcome *releasetransition.Outcome, inspection *releasetransition.Inspection) bool {
	if !pendingIntegrationConfigurationShape(outcome, inspection) {
		return false
	}
	options, available, err := cli.mutationGateReleaseOptions()
	if err != nil || !available {
		return false
	}
	store, err := releasetransition.NewPOSIXV2Store(options.ConfigHome)
	if err != nil {
		return false
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil || !snapshot.Exists {
		return false
	}
	journal, err := releasetransition.ParseJournal(snapshot.Payload)
	return err == nil && configApplyRepairJournalMatches(journal, *outcome) && (outcome.Transaction == nil || *outcome.Transaction == journal.Transaction)
}

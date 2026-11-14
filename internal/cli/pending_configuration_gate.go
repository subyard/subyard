package cli

import (
	"fmt"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/shellquote"
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
	// An unconverged owner is release work, reported as a decision rather than
	// a blocker; it is never pending configuration.
	for _, decision := range inspection.Decisions {
		if decision.Scope == "activation" && decision.Decision == releasetransition.DecisionCanonicalize {
			return false
		}
	}
	return true
}

// otherYardsPendingOnly reports a completed readiness whose remaining blockers
// are only typed pending configuration of other yards: after this yard's init,
// those items wait for their own inits and do not fail this one.
func (cli *CLI) otherYardsPendingOnly(outcome *releasetransition.Outcome, inspection *releasetransition.Inspection, yard string) bool {
	if !cli.pendingIntegrationConfigurationGate(outcome, inspection) {
		return false
	}
	retry := shellquote.Command([]string{"yard", "-Y", yard, "init"})
	for _, blocker := range inspection.Blockers {
		if blocker.Retry == retry {
			return false
		}
	}
	return true
}

func (cli *CLI) reportOtherYardsPending(inspection *releasetransition.Inspection) {
	for _, blocker := range inspection.Blockers {
		fmt.Fprintf(cli.options.Stdout, "Still pending: %s; next: %s\n", blocker.Message, blocker.Retry)
	}
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

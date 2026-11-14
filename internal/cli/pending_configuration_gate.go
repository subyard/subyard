package cli

import (
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/shellquote"
)

// A completed owner's pending integration configuration remains visible in
// readiness. It cannot prevent unrelated commands from reaching their own guards.
// No unfinished transaction, source/history blocker, or untyped failure qualifies.
func pendingIntegrationConfigurationShape(outcome *releasetransition.Outcome, inspection *releasetransition.Inspection) bool {
	if outcome == nil || inspection == nil || inspection.Outcome != outcome ||
		outcome.ReachedGoal || outcome.Active == "" || outcome.Active != outcome.Target || inspection.Resume != nil ||
		outcome.Status != releasetransition.StatusOperatorActionRequired || outcome.Code != releasetransition.CodeDependencyUnavailable || len(inspection.Blockers) == 0 {
		return false
	}
	for _, blocker := range inspection.Blockers {
		if blocker.Code != releasetransition.CodeDependencyUnavailable || blocker.Resource != "activation.materialized-config.pending-configuration" {
			return false
		}
	}
	// Any owner's remaining activation drift is release work, not pending configuration.
	for _, decision := range inspection.Decisions {
		if decision.Scope == "activation" && decision.Decision == releasetransition.DecisionCanonicalize {
			return false
		}
	}
	return true
}

// ordinaryCommandAdmitted is the one pending-configuration admission of the
// direct CLI and RPC: an ordinary command passes a gate blocked only by a
// completed owner's pending configuration and reaches its own guards.
func (cli *CLI) ordinaryCommandAdmitted(handler string, outcome *releasetransition.Outcome, inspection *releasetransition.Inspection) bool {
	return ordinaryCommandDuringPendingConfiguration(handler) && cli.pendingIntegrationConfigurationGate(outcome, inspection)
}

// pendingInitYards returns each yard whose plain init a blocker names as its
// next step. It fails when any blocker names another step.
func pendingInitYards(inspection *releasetransition.Inspection) (map[string]bool, bool) {
	if inspection == nil || len(inspection.Blockers) == 0 {
		return nil, false
	}
	yards := map[string]bool{}
	for _, blocker := range inspection.Blockers {
		// The exact quoted-command comparison below also rejects a missing prefix.
		yard, _ := strings.CutPrefix(blocker.Retry, "'yard' '-Y' '")
		yard, found := strings.CutSuffix(yard, "' 'init'")
		if !found || !domain.SafeName(yard) ||
			shellquote.Command([]string{"yard", "-Y", yard, "init"}) != blocker.Retry {
			return nil, false
		}
		yards[yard] = true
	}
	return yards, true
}

// otherYardsPendingOnly reports a gate blocked only by other yards' pending
// configuration whose next steps are those yards' inits. They wait for their own
// inits and do not fail this yard's.
func (cli *CLI) otherYardsPendingOnly(outcome *releasetransition.Outcome, inspection *releasetransition.Inspection, yard string) bool {
	if !cli.pendingIntegrationConfigurationGate(outcome, inspection) {
		return false
	}
	yards, ok := pendingInitYards(inspection)
	return ok && !yards[yard]
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

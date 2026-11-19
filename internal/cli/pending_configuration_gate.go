package cli

import (
	"slices"
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
	// Any owner's remaining activation drift is release work, not pending configuration.
	for _, decision := range inspection.Decisions {
		if decision.Scope == "activation" && decision.Decision == releasetransition.DecisionCanonicalize {
			return false
		}
	}
	return true
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

// pendingConfigurationOwnersTrusted accepts pending configuration only from the
// materialized-config owner and from profile runtimes that declare the yard
// failure scope.
func (cli *CLI) pendingConfigurationOwnersTrusted(inspection *releasetransition.Inspection) bool {
	owners := cli.yardScopedRuntimeIDs()
	for _, blocker := range inspection.Blockers {
		owner := strings.TrimSuffix(strings.TrimPrefix(blocker.Resource, "activation."), ".pending-configuration")
		if owner != "materialized-config" && !slices.Contains(owners, owner) {
			return false
		}
	}
	return true
}

// The transaction on an operator outcome can name completed history. Its mere
// presence cannot decide admission: read the protected journal and match it.
func (cli *CLI) pendingIntegrationConfigurationGate(outcome *releasetransition.Outcome, inspection *releasetransition.Inspection) bool {
	if !pendingIntegrationConfigurationShape(outcome, inspection) || !cli.pendingConfigurationOwnersTrusted(inspection) {
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

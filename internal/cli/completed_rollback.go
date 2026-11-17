package cli

import (
	"bytes"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

// Completion admits protected history only. The operation's ordinary readiness
// and source guards still own activation observations and sealed runtime checks.
func (cli *CLI) completedRollbackJournal(store *releasetransition.POSIXV2Store, journal releasetransition.JournalRecord) bool {
	payload, err := readConfigApplyRegistry(cli.options.RepositoryRoot)
	if err != nil {
		return false
	}
	registry, digest, err := releasetransition.ParseRegistryV2(payload, releasetransition.BuiltinCapabilityCatalog())
	if err != nil {
		return false
	}
	proof, err := store.InspectCompletedRollback(registry, digest, journal.ArtifactDigest)
	if err != nil || proof == nil {
		return false
	}
	expected, err := releasetransition.MarshalJournal(journal)
	if err != nil {
		return false
	}
	actual, err := releasetransition.MarshalJournal(proof.Journal)
	return err == nil && bytes.Equal(actual, expected)
}

func (cli *CLI) completedRepairJournalMatches(store *releasetransition.POSIXV2Store, journal releasetransition.JournalRecord, outcome releasetransition.Outcome) bool {
	return configApplyRepairJournalMatches(journal, outcome) ||
		journal.Checkpoint == releasetransition.JournalComplete && journal.Goal.Direction == releasetransition.DirectionActivatePrevious &&
			journal.Goal.Target == outcome.Target && journal.Goal.Target == outcome.Active && cli.completedRollbackJournal(store, journal)
}

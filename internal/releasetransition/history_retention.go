package releasetransition

import "errors"

// cleanupClosedMigrationTransactions retires unreferenced supported closed artifact sets.
// This is retention policy only: legacy bytes without a historical ledger
// anchor cannot become source authority or authorize any migration work.
func (store *POSIXV2Store) cleanupClosedMigrationTransactions(current TransactionID, registry RegistryV2, registryDigest Fingerprint, endpoints ReleaseLinks) error {
	state, err := store.ReadMigrationLedger(registry, registryDigest)
	if err != nil {
		return err
	}
	if err := validateReleaseID(endpoints.Active, "retention active release"); err != nil {
		return err
	}
	if endpoints.Previous != nil {
		if err := validateReleaseID(*endpoints.Previous, "retention previous release"); err != nil {
			return err
		}
	}
	// A live recovery grant retains both ends even when neither is a runtime
	// endpoint. The graph expands any protected member to its whole component.
	live, err := store.readRecoveryReceiptsAt(recoveryLifecycleParts)
	if err != nil {
		return err
	}
	legacy, err := store.readRecoveryReceipts()
	if err != nil {
		return err
	}
	for transaction, receipt := range legacy {
		if other, exists := live[transaction]; exists && !sameRecoveryReceipt(other, receipt) {
			return invalid("recovery receipt namespaces conflict during migration cleanup")
		}
		live[transaction] = receipt
	}
	return store.cleanupTransactions(current, func(graph *v2TransactionGraph) error {
		for transaction, receipt := range live {
			graph.protected[string(transaction)] = struct{}{}
			graph.protected[string(receipt.Replacement.Transaction)] = struct{}{}
		}
		for transaction := range graph.directories {
			if _, protected := graph.protected[string(transaction)]; protected {
				continue
			}
			journal, err := store.sourceRetainedTerminalEvidence(transaction, registry)
			if err != nil {
				return err
			}
			if err := store.sourceTransactionArtifacts(journal); err != nil {
				return err
			}
			advanced := make(map[string]bool)
			for _, step := range journal.Steps {
				if step.ID == step.Migration+".ledger" {
					advanced[step.Migration] = true
				}
			}
			for _, step := range journal.Steps {
				var migration MigrationDefinitionV2
				for _, definition := range registry.Migrations {
					if definition.ID == step.Migration {
						migration = definition
						break
					}
				}
				if migration.ID == "" || !advanced[step.Migration] {
					return errors.New("retained migration artifacts have no supported closed ledger step")
				}
				if err := store.terminalStepArtifacts(journal, step, migration); err != nil {
					return err
				}
			}
			// Closure is not a retention reference. Only live verified runtime
			// endpoints or the validated frozen projection retain an otherwise
			// unreferenced closed component. Existing graph roots remain protected.
			retained := journal.Releases.Target == endpoints.Active ||
				(endpoints.Previous != nil && journal.Releases.Target == *endpoints.Previous)
			if state.Checkpointed && state.Checkpoint.LegacyProjection.Exists {
				terminalProjection := false
				for _, step := range journal.Steps {
					if step.ID == step.Migration+".ledger" && step.Desired == state.ProjectionSnapshot.Fingerprint {
						terminalProjection = true
					}
				}
				// Historical independent migrations may have a different
				// order. A terminal pin is not the before-pin of another edge.
				for _, step := range journal.Steps {
					if step.ID == step.Migration+".ledger" && step.Expected == state.ProjectionSnapshot.Fingerprint {
						terminalProjection = false
						break
					}
				}
				retained = retained || terminalProjection
			}
			if retained {
				graph.protected[string(transaction)] = struct{}{}
			}
		}
		return nil
	})
}

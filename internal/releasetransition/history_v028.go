package releasetransition

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
)

// These are frozen reader contracts, not executable migrations. Preserve the
// original bytes: historical ledger/evidence pins bind this ordered vocabulary.
//
//go:embed history_v028.json
var historyV028Payload []byte

const historyV028Digest Fingerprint = "372ccefc089e8b31099df7e08d8a783f2dbf5af5dbaf8e4afa6f41c6a4f1f28e"

var historyV028 = func() RegistryV2 {
	var registry RegistryV2
	if fingerprintPayload(historyV028Payload) != historyV028Digest || json.Unmarshal(historyV028Payload, &registry) != nil {
		panic("invalid frozen v0.28 migration history")
	}
	return registry
}()

// historyData expands only the exact known retired prefix. Live admission uses
// MinimumEpochs; record codecs still need the original IDs and earlier frontiers.
func (registry RegistryV2) historyData() RegistryV2 {
	for domain, top := range historyV028.CurrentEpochs {
		if registry.MinimumEpochs[domain] != top {
			return registry
		}
		if top == historyV028.MinimumEpochs[domain] {
			continue
		}
		prefix := registry.RetiredPrefixes[domain]
		digest, err := historyV028.MigrationPrefixDigest(domain, top)
		if err != nil || prefix.ThroughEpoch != top || prefix.Digest != digest {
			return registry
		}
	}
	registry.MinimumEpochs = cloneEpochs(registry.MinimumEpochs)
	registry.RetiredPrefixes = cloneRetiredPrefixes(registry.RetiredPrefixes)
	for domain, minimum := range historyV028.MinimumEpochs {
		registry.MinimumEpochs[domain] = minimum
		delete(registry.RetiredPrefixes, domain)
	}
	registry.Migrations = append(append([]MigrationDefinitionV2(nil), historyV028.Migrations...), registry.Migrations...)
	return registry
}

func cloneRetiredPrefixes(source map[string]RetiredMigrationPrefixV2) map[string]RetiredMigrationPrefixV2 {
	result := make(map[string]RetiredMigrationPrefixV2, len(source))
	for domain, prefix := range source {
		result[domain] = prefix
	}
	return result
}

const manualMigrationRepair = "migration-manual-repair-required: protected migration history needs a retired migration handler; restore the exact protected history from a known-good backup or obtain manual repair before updating; do not uninstall"

// ErrRetiredMigrationRepair distinguishes a readable below-floor history from
// damaged metadata, whose existing recovery diagnosis remains authoritative.
var ErrRetiredMigrationRepair = errors.New(manualMigrationRepair)

func retiredMigrationRepairError() error {
	return fmt.Errorf("%w: %w", ErrInvalid, ErrRetiredMigrationRepair)
}

func (registry RegistryV2) requireMinimumLedger(ledger LedgerV2) error {
	for domain, minimum := range registry.MinimumEpochs {
		if ledger.Domains[domain].Epoch < minimum {
			return retiredMigrationRepairError()
		}
	}
	return nil
}

// RequireRetiredMigrationCompletion is read-only admission. Existing history is
// interpreted with its frozen vocabulary before applying the current floor.
func (store *POSIXV2Store) RequireRetiredMigrationCompletion(registry RegistryV2, digest Fingerprint) error {
	snapshot, err := store.ReadCurrentJournal()
	if err != nil {
		return err
	}
	if snapshot.Exists {
		journal, err := ParseJournal(snapshot.Payload)
		if err != nil {
			return err
		}
		if journal.Checkpoint != JournalComplete {
			for _, step := range journal.Steps {
				for _, migration := range historyV028.Migrations {
					if step.Migration == migration.ID && step.Checkpoint != StepVerified {
						return retiredMigrationRepairError()
					}
				}
			}
		}
	}
	state, err := store.ReadMigrationLedger(registry, digest)
	if err != nil {
		return err
	}
	pending, err := registry.PendingPath(BaselineLedgerV2(registry))
	if err != nil {
		return err
	}
	if state.AuthoritySnapshot.Exists || len(pending) != 0 {
		for domain, top := range historyV028.CurrentEpochs {
			if state.Ledger.Domains[domain].Epoch < top {
				return retiredMigrationRepairError()
			}
		}
	}
	return nil
}

const localSSHPortMigration = "local-ssh-port-settings-v2-to-v3"

func (transition *V2Transition) migration(id string) (MigrationDefinitionV2, bool) {
	for _, migration := range transition.registry.historyData().Migrations {
		if migration.ID == id {
			return migration, true
		}
	}
	return MigrationDefinitionV2{}, false
}

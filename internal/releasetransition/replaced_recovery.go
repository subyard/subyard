package releasetransition

import (
	"bytes"
	"strings"
)

// Recovery evidence leaves the strict history namespaces with its superseded
// journal. These are exact copies of existing records, not a new recovery schema
// or a completion claim. Older engines can then read the successor's history.
func (store *POSIXV2Store) preserveReplacedRecovery(snapshot ProtectedSnapshot) error {
	journal, err := ParseJournal(snapshot.Payload)
	if err != nil || !strings.HasPrefix(string(journal.Transaction), RecoveryTransactionPrefixV2) {
		return err
	}
	receipt, err := store.ValidatedCurrentRecovery(journal)
	if err != nil {
		return err
	}
	live, err := store.readRecoveryReceiptsAt(recoveryLifecycleParts)
	if err != nil {
		return err
	}
	archive, err := store.readLifecycleArchive()
	if err != nil {
		return err
	}
	if _, err := store.readRecoveryFrontiers(); err != nil {
		return err
	}
	copyPair := func(parts []string, transaction TransactionID) error {
		for _, name := range []string{string(transaction) + ".json", "." + string(transaction) + ".json.pending"} {
			value, err := store.readRecord(parts, name)
			if err != nil {
				return err
			}
			if value.Exists {
				if err := store.createImmutable(replacedRecoveryParts(journal.Transaction, parts[len(parts)-1]), name, value.Payload); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for transaction, value := range live {
		if value.Replacement == receipt.Replacement {
			if err := copyPair(recoveryLifecycleParts, transaction); err != nil {
				return err
			}
		}
	}
	for transaction, value := range archive {
		if value.Receipt.Replacement == receipt.Replacement {
			if err := copyPair(recoveryArchiveParts, transaction); err != nil {
				return err
			}
		}
	}
	return copyPair(recoveryFrontierParts, receipt.Replacement.Transaction)
}

func replacedRecoveryParts(transaction TransactionID, namespace string) []string {
	return append(append([]string(nil), replacedJournalParts...), string(transaction)+".recovery", namespace)
}

// Called only after a different successor has reached its verified fixed point.
// Exact comparisons make partial retirement retryable; a changed live record
// stays protected. A receipt-only crash before publication changes no live data.
func (store *POSIXV2Store) retireReplacedRecovery(transaction TransactionID) error {
	if !strings.HasPrefix(string(transaction), RecoveryTransactionPrefixV2) {
		return nil
	}
	for _, parts := range [][]string{recoveryLifecycleParts, recoveryArchiveParts, recoveryFrontierParts} {
		archivedParts := replacedRecoveryParts(transaction, parts[len(parts)-1])
		entries, _, err := store.readDirectoryEntries(archivedParts, maxRecoveryLifecycleEntries*2)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if _, err := recoveryEntryTransaction(entry.Name()); err != nil {
				return err
			}
			archived, err := store.readRecord(archivedParts, entry.Name())
			if err != nil {
				return err
			}
			live, err := store.readRecord(parts, entry.Name())
			if err != nil {
				return err
			}
			if !live.Exists {
				continue
			}
			if !archived.Exists {
				return invalid("replaced recovery evidence changed before retirement")
			}
			if !bytes.Equal(live.Payload, archived.Payload) {
				// An archive-only attempt may precede a new recovery generation.
				// Preserve its changed live record for the later replacement's
				// exact archive; this older copy cannot retire or block it.
				continue
			}
			if err := store.removeProtectedRecord(parts, entry.Name(), archived.Payload); err != nil {
				return err
			}
			if err := store.inject("after-replaced-recovery-retirement"); err != nil {
				return err
			}
		}
	}
	return nil
}

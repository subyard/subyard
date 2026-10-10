package releasetransition

import (
	"bytes"
	"slices"
	"strings"
)

var recoveryReceiptParts = []string{"release-transition", "recovery", "v1", "transactions"}

// Recovery receipts live outside the frozen V2 transaction graph so retained
// cleanup cannot erase predecessor evidence it does not understand.
func (store *POSIXV2Store) ReadRecoveryReceipt(transaction TransactionID) (ProtectedSnapshot, error) {
	if err := validateTransactionID(transaction); err != nil {
		return ProtectedSnapshot{}, err
	}
	return store.readRecord(recoveryParts(transaction), string(transaction)+".json")
}

// ReadRecoveryReceiptForPublication includes a fsynced pending receipt so a
// fresh authorized process can reuse the exact initial successor after a crash.
// The caller still revalidates the plan and obtains fresh authorization; it
// cannot manufacture or recover the original ephemeral grant from this data.
func (store *POSIXV2Store) ReadRecoveryReceiptForPublication(transaction TransactionID) (RecoveryReceiptV1, bool, error) {
	if err := validateTransactionID(transaction); err != nil {
		return RecoveryReceiptV1{}, false, err
	}
	if strings.HasPrefix(string(transaction), RecoveryTransactionPrefixV2) {
		return store.readLifecycleReceiptForPublication(transaction)
	}
	receipts, err := store.readRecoveryReceipts()
	if err != nil {
		return RecoveryReceiptV1{}, false, err
	}
	receipt, found := receipts[transaction]
	if found {
		terminal, present, err := store.readLifecycleRecord(transaction)
		if err != nil {
			return RecoveryReceiptV1{}, false, err
		}
		if present && (!sameRecoveryReceipt(terminal.Receipt, receipt) || terminal.Cancellation != nil || terminal.PredecessorCompleted != nil || terminal.Completed != nil) {
			return RecoveryReceiptV1{}, false, invalid("closed legacy recovery intent cannot be published")
		}
	}
	return receipt, found, nil
}

// CreateRecoveryReceipt publishes immutable evidence before journal CAS. The
// caller holds the shared update lock through assessment, publication and CAS.
func (store *POSIXV2Store) CreateRecoveryReceipt(transaction TransactionID, payload []byte) error {
	receipt, err := ParseRecoveryReceipt(payload)
	if err != nil {
		return err
	}
	canonical, err := MarshalRecoveryReceipt(receipt)
	if err != nil {
		return err
	}
	if !bytes.Equal(payload, canonical) || receipt.Successor.Transaction != transaction {
		return invalid("recovery receipt is not the canonical selected successor")
	}
	if receipt.Contract == ActivationOnlyRecoveryContractV2 {
		return store.createLifecycleRecoveryReceipt(transaction, receipt, payload)
	}
	resolved, err := store.ResolveRecoveryTransaction(transaction, receipt.Replacement, receipt.Successor.AuthorizationPlan)
	if err != nil {
		return err
	}
	if resolved != transaction {
		return invalid("recovery predecessor already has another selected successor")
	}
	return store.createImmutable(recoveryReceiptParts, string(transaction)+".json", payload)
}

// ResolveRecoveryTransaction admits one successor per predecessor and reuses
// exact evidence already published (or fsynced pending) before a failed CAS.
// Recovery V1 intentionally excludes replacement chains. The caller holds Lock.
func (store *POSIXV2Store) ResolveRecoveryTransaction(
	proposed TransactionID,
	replacement ActivationOnlyRecoveryRequest,
	authorizationPlan PlanToken,
) (TransactionID, error) {
	if err := replacement.Validate(); err != nil {
		return "", err
	}
	if err := validateTransactionID(proposed); err != nil {
		return "", err
	}
	if err := validatePlanToken(authorizationPlan); err != nil {
		return "", err
	}
	if !strings.HasPrefix(string(proposed), RecoveryTransactionPrefixV1) ||
		IsRecoveryTransaction(replacement.Transaction) || proposed == replacement.Transaction {
		return "", invalid("activation recovery V1 does not admit replacement chains")
	}
	if err := store.rejectLifecycleReservation(replacement.Transaction); err != nil {
		return "", err
	}
	receipts, err := store.readRecoveryReceipts()
	if err != nil {
		return "", err
	}
	for transaction, receipt := range receipts {
		if receipt.Replacement.Transaction == replacement.Transaction {
			if receipt.Replacement == replacement && receipt.Successor.AuthorizationPlan == authorizationPlan {
				return transaction, nil
			}
			return "", invalid("recovery predecessor already has a different successor plan")
		}
		if transaction == proposed {
			return "", invalid("recovery successor transaction already exists")
		}
	}
	if err := store.retireClosedRecoveryEvidence(); err != nil {
		return "", err
	}
	receipts, err = store.readRecoveryReceipts()
	if err != nil {
		return "", err
	}
	if len(receipts) >= maxTransactionGraphEntries {
		return "", invalid("activation recovery receipt horizon is full")
	}
	return proposed, nil
}

// ValidateCurrentRecovery requires provenance for the reserved successor IDs.
// Ordinary journals remain independent of unrelated recovery receipts.
func (store *POSIXV2Store) ValidateCurrentRecovery(current JournalRecord) error {
	_, err := store.ValidatedCurrentRecovery(current)
	return err
}

func (store *POSIXV2Store) ValidatedCurrentRecovery(current JournalRecord) (RecoveryReceiptV1, error) {
	if !IsRecoveryTransaction(current.Transaction) {
		return RecoveryReceiptV1{}, nil
	}
	snapshot, err := store.ReadRecoveryReceipt(current.Transaction)
	if err != nil {
		return RecoveryReceiptV1{}, err
	}
	if !snapshot.Exists {
		return RecoveryReceiptV1{}, invalid("activation recovery successor has no protected receipt")
	}
	receipt, err := ParseRecoveryReceipt(snapshot.Payload)
	if err != nil {
		return RecoveryReceiptV1{}, err
	}
	if _, err := store.readRecoveryReceiptsAt(recoveryParts(current.Transaction)); err != nil {
		return RecoveryReceiptV1{}, err
	}
	if err := receipt.MatchesSuccessor(current); err != nil {
		return RecoveryReceiptV1{}, err
	}
	terminal, present, err := store.readLifecycleRecord(current.Transaction)
	if err != nil {
		return RecoveryReceiptV1{}, err
	}
	if present && (terminal.Cancellation != nil || terminal.PredecessorCompleted != nil || !sameRecoveryReceipt(terminal.Receipt, receipt) ||
		terminal.Completed != nil && current.Checkpoint != JournalComplete) {
		return RecoveryReceiptV1{}, invalid("current recovery successor has conflicting terminal evidence")
	}
	if strings.HasPrefix(string(current.Transaction), RecoveryTransactionPrefixV2) {
		frontier, found, err := store.recoveryFrontier(receipt.Replacement)
		if err != nil || !found || frontier.Generation != receipt.Generation || frontier.Reservation() != receipt.Reservation {
			return RecoveryReceiptV1{}, invalid("current recovery successor does not match its durable frontier")
		}
	}
	return receipt, nil
}

func (store *POSIXV2Store) readRecoveryReceipts() (map[TransactionID]RecoveryReceiptV1, error) {
	return store.readRecoveryReceiptsAt(recoveryReceiptParts)
}

func (store *POSIXV2Store) readRecoveryReceiptsAt(parts []string) (map[TransactionID]RecoveryReceiptV1, error) {
	entries, _, err := store.recoveryEntries(parts)
	if err != nil {
		return nil, err
	}
	receipts := make(map[TransactionID]RecoveryReceiptV1, len(entries))
	predecessors := make(map[TransactionID]TransactionID, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		transactionName := name
		if strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".json.pending") {
			transactionName = strings.TrimSuffix(strings.TrimPrefix(name, "."), ".json.pending")
		} else if strings.HasSuffix(name, ".json") {
			transactionName = strings.TrimSuffix(name, ".json")
		} else {
			return nil, invalid("unrecognized activation recovery receipt entry")
		}
		transaction := TransactionID(transactionName)
		if err := validateTransactionID(transaction); err != nil {
			return nil, err
		}
		snapshot, err := store.readRecord(parts, name)
		if err != nil {
			return nil, err
		}
		if !snapshot.Exists {
			return nil, invalid("activation recovery receipt disappeared during inspection")
		}
		receipt, err := ParseRecoveryReceipt(snapshot.Payload)
		if err != nil {
			return nil, err
		}
		canonical, err := MarshalRecoveryReceipt(receipt)
		if err != nil || !bytes.Equal(canonical, snapshot.Payload) || receipt.Successor.Transaction != transaction ||
			!slices.Equal(recoveryParts(transaction), parts) {
			return nil, invalid("activation recovery receipt does not match canonical successor")
		}
		if prior, exists := receipts[transaction]; exists {
			priorPayload, err := MarshalRecoveryReceipt(prior)
			if err != nil || !bytes.Equal(priorPayload, canonical) {
				return nil, invalid("pending activation recovery receipt conflicts with published evidence")
			}
		}
		if successor, exists := predecessors[receipt.Replacement.Transaction]; exists && successor != transaction {
			return nil, invalid("activation recovery receipts share a predecessor")
		}
		receipts[transaction] = receipt
		predecessors[receipt.Replacement.Transaction] = transaction
	}
	limit := maxTransactionGraphEntries
	if slices.Equal(parts, recoveryLifecycleParts) {
		limit = maxRecoveryLifecycleEntries
	}
	if len(receipts) > limit {
		return nil, invalid("too many activation recovery receipts")
	}
	return receipts, nil
}

package releasetransition

import (
	"bytes"
	"strings"

	"golang.org/x/sys/unix"
)

var (
	recoveryLifecycleParts = []string{"release-transition", "recovery", "v2", "transactions"}
	recoveryArchiveParts   = []string{"release-transition", "recovery", "v2", "archive"}
)

func recoveryParts(transaction TransactionID) []string {
	if strings.HasPrefix(string(transaction), RecoveryTransactionPrefixV2) {
		return recoveryLifecycleParts
	}
	return recoveryReceiptParts
}

func sameRecoveryReceipt(left, right RecoveryReceiptV1) bool {
	a, err := MarshalRecoveryReceipt(left)
	if err != nil {
		return false
	}
	b, err := MarshalRecoveryReceipt(right)
	return err == nil && bytes.Equal(a, b)
}

// A record and its fsynced pending copy must describe the same immutable intent.
func (store *POSIXV2Store) readRecoveryPair(parts []string, transaction TransactionID) (ProtectedSnapshot, error) {
	if err := validateTransactionID(transaction); err != nil {
		return ProtectedSnapshot{}, err
	}
	name := string(transaction) + ".json"
	result, err := store.readRecord(parts, name)
	if err != nil {
		return ProtectedSnapshot{}, err
	}
	pending, err := store.readRecord(parts, "."+name+".pending")
	if err != nil {
		return ProtectedSnapshot{}, err
	}
	if result.Exists && pending.Exists && !bytes.Equal(result.Payload, pending.Payload) {
		return ProtectedSnapshot{}, invalid("pending recovery lifecycle evidence conflicts with published evidence")
	}
	if !result.Exists {
		result = pending
	}
	return result, nil
}

func (store *POSIXV2Store) readLifecycleRecord(transaction TransactionID) (RecoveryLifecycleRecord, bool, error) {
	snapshot, err := store.readRecoveryPair(recoveryArchiveParts, transaction)
	if err != nil || !snapshot.Exists {
		return RecoveryLifecycleRecord{}, false, err
	}
	record, err := ParseRecoveryLifecycleRecord(snapshot.Payload)
	if err != nil {
		return RecoveryLifecycleRecord{}, false, err
	}
	canonical, err := MarshalRecoveryLifecycleRecord(record)
	if err != nil || !bytes.Equal(canonical, snapshot.Payload) || record.Receipt.Successor.Transaction != transaction {
		return RecoveryLifecycleRecord{}, false, invalid("recovery lifecycle archive is not canonical selected evidence")
	}
	return record, true, nil
}

func (store *POSIXV2Store) readLifecycleArchive() (map[TransactionID]RecoveryLifecycleRecord, error) {
	entries, _, err := store.readDirectoryEntries(recoveryArchiveParts, maxTransactionGraphEntries*2)
	if err != nil {
		return nil, err
	}
	if len(entries) > maxTransactionGraphEntries*2 {
		return nil, invalid("too many recovery lifecycle archive entries")
	}
	result := make(map[TransactionID]RecoveryLifecycleRecord)
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".json.pending") {
			name = strings.TrimSuffix(strings.TrimPrefix(name, "."), ".json.pending")
		} else if strings.HasSuffix(name, ".json") {
			name = strings.TrimSuffix(name, ".json")
		} else {
			return nil, invalid("unrecognized recovery lifecycle archive entry")
		}
		transaction := TransactionID(name)
		if _, exists := result[transaction]; exists {
			continue
		}
		record, present, err := store.readLifecycleRecord(transaction)
		if err != nil {
			return nil, err
		}
		if !present {
			return nil, invalid("recovery lifecycle archive disappeared during inspection")
		}
		result[transaction] = record
	}
	if len(result) > maxTransactionGraphEntries {
		return nil, invalid("recovery lifecycle archive horizon is full")
	}
	return result, nil
}

func (store *POSIXV2Store) createLifecycleRecord(record RecoveryLifecycleRecord) error {
	payload, err := MarshalRecoveryLifecycleRecord(record)
	if err != nil {
		return err
	}
	archive, err := store.readLifecycleArchive()
	if err != nil {
		return err
	}
	transaction := record.Receipt.Successor.Transaction
	if _, exists := archive[transaction]; !exists && len(archive) >= maxTransactionGraphEntries {
		return invalid("recovery lifecycle archive horizon is full")
	}
	return store.createImmutable(recoveryArchiveParts, string(transaction)+".json", payload)
}

func (store *POSIXV2Store) readLifecycleReceiptForPublication(transaction TransactionID) (RecoveryReceiptV1, bool, error) {
	snapshot, err := store.readRecoveryPair(recoveryLifecycleParts, transaction)
	if err != nil || !snapshot.Exists {
		return RecoveryReceiptV1{}, false, err
	}
	receipt, err := ParseRecoveryReceipt(snapshot.Payload)
	if err != nil {
		return RecoveryReceiptV1{}, false, err
	}
	canonical, err := MarshalRecoveryReceipt(receipt)
	if err != nil || !bytes.Equal(canonical, snapshot.Payload) || receipt.Successor.Transaction != transaction || receipt.Contract != ActivationOnlyRecoveryContractV2 {
		return RecoveryReceiptV1{}, false, invalid("recovery lifecycle receipt does not match selected successor")
	}
	terminal, present, err := store.readLifecycleRecord(transaction)
	if err != nil {
		return RecoveryReceiptV1{}, false, err
	}
	if present && (terminal.Cancellation != nil || !sameRecoveryReceipt(terminal.Receipt, receipt)) {
		return RecoveryReceiptV1{}, false, invalid("recovery lifecycle successor is cancelled or conflicts with terminal evidence")
	}
	return receipt, true, nil
}

func (store *POSIXV2Store) rejectLifecycleReservation(predecessor TransactionID) error {
	receipts, err := store.readRecoveryReceiptsAt(recoveryLifecycleParts)
	if err != nil {
		return err
	}
	for _, receipt := range receipts {
		if receipt.Replacement.Transaction == predecessor {
			return invalid("recovery V1 cannot replace a lifecycle reservation")
		}
	}
	archive, err := store.readLifecycleArchive()
	if err != nil {
		return err
	}
	for _, record := range archive {
		if record.Cancellation != nil && record.Receipt.Replacement.Transaction == predecessor {
			return invalid("recovery V1 cannot bypass a cancelled lifecycle reservation")
		}
	}
	return nil
}

func (store *POSIXV2Store) lifecycleReservation(request ActivationOnlyRecoveryRequest) (RecoveryReceiptV1, bool, error) {
	if err := request.Validate(); err != nil {
		return RecoveryReceiptV1{}, false, err
	}
	if IsRecoveryTransaction(request.Transaction) {
		return RecoveryReceiptV1{}, false, invalid("activation recovery does not admit replacement chains")
	}
	legacy, err := store.readRecoveryReceipts()
	if err != nil {
		return RecoveryReceiptV1{}, false, err
	}
	for _, receipt := range legacy {
		if receipt.Replacement.Transaction == request.Transaction {
			return RecoveryReceiptV1{}, false, invalid("recovery V1 reservation requires exact original plan continuation")
		}
	}
	receipts, err := store.readRecoveryReceiptsAt(recoveryLifecycleParts)
	if err != nil {
		return RecoveryReceiptV1{}, false, err
	}
	for _, receipt := range receipts {
		if receipt.Replacement.Transaction == request.Transaction {
			if receipt.Replacement != request {
				return RecoveryReceiptV1{}, false, invalid("lifecycle reservation refers to another predecessor fingerprint")
			}
			return receipt, true, nil
		}
	}
	return RecoveryReceiptV1{}, false, nil
}

// RecoveryReservation binds an already durable reservation into the fresh plan.
// Reads never cancel or remove evidence, including after a pre-CAS crash.
func (store *POSIXV2Store) RecoveryReservation(request ActivationOnlyRecoveryRequest, basePlan PlanToken) (Fingerprint, error) {
	if err := validatePlanToken(basePlan); err != nil {
		return "", err
	}
	receipt, found, err := store.lifecycleReservation(request)
	if err != nil {
		return "", err
	}
	if found {
		terminal, present, err := store.readLifecycleRecord(receipt.Successor.Transaction)
		if err != nil {
			return "", err
		}
		if present && (!sameRecoveryReceipt(terminal.Receipt, receipt) || terminal.Completed != nil) {
			return "", invalid("lifecycle reservation has conflicting or completed terminal evidence")
		}
		if present && terminal.Cancellation != nil && terminal.Cancellation.BasePlan != basePlan {
			return "", invalid("restore the exactly authorized cancellation inputs before retrying lifecycle recovery")
		}
		if receipt.BasePlan != basePlan {
			payload, err := MarshalRecoveryReceipt(receipt)
			return fingerprintPayload(payload), err
		}
		if present && terminal.Cancellation != nil {
			return "", invalid("cancelled lifecycle plan cannot reuse its original reservation")
		}
		return receipt.Reservation, nil
	}
	archive, err := store.readLifecycleArchive()
	if err != nil {
		return "", err
	}
	var reservation Fingerprint
	for _, record := range archive {
		if record.Cancellation == nil || record.Receipt.Replacement != request || record.Cancellation.BasePlan != basePlan {
			continue
		}
		payload, err := MarshalRecoveryReceipt(record.Receipt)
		if err != nil {
			return "", err
		}
		fingerprint := fingerprintPayload(payload)
		if reservation != "" && reservation != fingerprint {
			return "", invalid("multiple cancelled reservations match the new lifecycle assessment")
		}
		reservation = fingerprint
	}
	return reservation, nil
}

// ResolveLifecycleRecoveryTransaction runs under the shared update lock, after
// fresh authorization of the plan that includes the cancelled receipt binding.
func (store *POSIXV2Store) ResolveLifecycleRecoveryTransaction(proposed TransactionID, request ActivationOnlyRecoveryRequest, basePlan, plan PlanToken, grantDigest Fingerprint) (TransactionID, error) {
	if err := validateTransactionID(proposed); err != nil {
		return "", err
	}
	if !strings.HasPrefix(string(proposed), RecoveryTransactionPrefixV2) || proposed == request.Transaction {
		return "", invalid("lifecycle recovery requires a new reserved successor")
	}
	if err := validateFingerprint(grantDigest, "lifecycle cancellation authorization"); err != nil {
		return "", err
	}
	current, err := store.ReadCurrentJournal()
	if err != nil {
		return "", err
	}
	if !current.Exists || current.Fingerprint != request.Fingerprint {
		return "", ErrProtectedStoreStale
	}
	journal, err := ParseJournal(current.Payload)
	if err != nil || journal.Transaction != request.Transaction {
		return "", invalid("lifecycle predecessor is not the exact current journal")
	}
	reservation, err := store.RecoveryReservation(request, basePlan)
	if err != nil {
		return "", err
	}
	expected, err := BindRecoveryLifecyclePlan(basePlan, reservation)
	if err != nil || expected != plan {
		return "", invalid("lifecycle authorization does not bind the exact reservation")
	}
	receipt, found, err := store.lifecycleReservation(request)
	if err != nil {
		return "", err
	}
	if found && receipt.BasePlan == basePlan {
		if receipt.Successor.AuthorizationPlan != plan {
			return "", invalid("lifecycle reservation has another authorization plan")
		}
		return receipt.Successor.Transaction, nil
	}
	if found {
		if err := store.cancelLifecycleReceipt(receipt, current, basePlan, plan, grantDigest); err != nil {
			return "", err
		}
	}
	archive, err := store.readLifecycleArchive()
	if err != nil {
		return "", err
	}
	if _, exists := archive[proposed]; exists {
		return "", invalid("lifecycle successor transaction already has terminal evidence")
	}
	receipts, err := store.readRecoveryReceiptsAt(recoveryLifecycleParts)
	if err != nil {
		return "", err
	}
	if _, exists := receipts[proposed]; exists || len(receipts) >= maxTransactionGraphEntries {
		return "", invalid("lifecycle successor exists or active recovery horizon is full")
	}
	return proposed, nil
}

func (store *POSIXV2Store) createLifecycleRecoveryReceipt(transaction TransactionID, receipt RecoveryReceiptV1, payload []byte) error {
	reservation, err := store.RecoveryReservation(receipt.Replacement, receipt.BasePlan)
	if err != nil {
		return err
	}
	if reservation != receipt.Reservation {
		return invalid("lifecycle receipt does not bind the selected reservation")
	}
	if _, present, err := store.readLifecycleRecord(transaction); err != nil {
		return err
	} else if present {
		return invalid("terminal lifecycle transaction cannot be published again")
	}
	selected, found, err := store.lifecycleReservation(receipt.Replacement)
	if err != nil {
		return err
	}
	if found && selected.Successor.Transaction != transaction {
		return invalid("lifecycle predecessor already has another successor")
	}
	receipts, err := store.readRecoveryReceiptsAt(recoveryLifecycleParts)
	if err != nil {
		return err
	}
	if !found && len(receipts) >= maxTransactionGraphEntries {
		return invalid("active lifecycle recovery horizon is full")
	}
	return store.createImmutable(recoveryLifecycleParts, string(transaction)+".json", payload)
}

func (store *POSIXV2Store) cancelLifecycleReceipt(receipt RecoveryReceiptV1, current ProtectedSnapshot, basePlan, plan PlanToken, grantDigest Fingerprint) error {
	predecessor, err := MarshalJournal(receipt.Predecessor)
	if err != nil || !bytes.Equal(predecessor, current.Payload) {
		return invalid("cancellation requires the unchanged unselected predecessor")
	}
	roots, err := store.recoveryReferenceRoots(&receipt)
	if err != nil {
		return err
	}
	if _, referenced := roots[receipt.Successor.Transaction]; referenced {
		return invalid("selected or archived lifecycle successor cannot be cancelled")
	}
	fresh, err := store.ReadCurrentJournal()
	if err != nil {
		return err
	}
	if !sameProtectedSnapshot(fresh, current) {
		return ErrProtectedStoreStale
	}
	if err := store.discardLifecycleJournalPending(receipt); err != nil {
		return err
	}
	record, present, err := store.readLifecycleRecord(receipt.Successor.Transaction)
	if err != nil {
		return err
	}
	if present {
		if record.Cancellation == nil || !sameRecoveryReceipt(record.Receipt, receipt) || record.Cancellation.BasePlan != basePlan || record.Cancellation.Plan != plan {
			return invalid("lifecycle reservation has conflicting terminal evidence")
		}
	} else {
		record = RecoveryLifecycleRecord{SchemaVersion: RecoveryLifecycleSchemaV1, Receipt: receipt,
			Cancellation: &RecoveryCancellation{BasePlan: basePlan, Plan: plan, AuthorizationDigest: grantDigest}}
	}
	if err := store.inject("before-recovery-cancellation"); err != nil {
		return err
	}
	if err := store.createLifecycleRecord(record); err != nil {
		return err
	}
	if err := store.inject("after-recovery-cancellation"); err != nil {
		return err
	}
	return store.removeRecoveryPair(recoveryLifecycleParts, receipt.Successor.Transaction, mustRecoveryReceiptPayload(receipt))
}

func mustRecoveryReceiptPayload(receipt RecoveryReceiptV1) []byte {
	payload, _ := MarshalRecoveryReceipt(receipt)
	return payload
}

// A prepared CAS file is an unselected write, not an active journal. It may be
// invalidated only when its exact successor belongs to the cancelled receipt.
func (store *POSIXV2Store) discardLifecycleJournalPending(receipt RecoveryReceiptV1) error {
	parts := []string{"release-transition", "v2"}
	snapshot, err := store.readRecord(parts, ".journal.json.pending")
	if err != nil || !snapshot.Exists {
		return err
	}
	journal, err := ParseJournal(snapshot.Payload)
	if err != nil {
		return err
	}
	if err := receipt.MatchesSuccessor(journal); err != nil || journal.Checkpoint != JournalAuthorized {
		return invalid("unrecognized pending journal prevents lifecycle cancellation")
	}
	return store.removeProtectedRecord(parts, ".journal.json.pending", snapshot.Payload)
}

func (store *POSIXV2Store) removeProtectedRecord(parts []string, name string, expected []byte) error {
	parent, present, err := store.openParent(parts, false)
	if err != nil || !present {
		return err
	}
	defer unix.Close(parent)
	snapshot, err := readProtectedAt(parent, name)
	if err != nil || !snapshot.Exists {
		return err
	}
	if !bytes.Equal(snapshot.Payload, expected) {
		return ErrProtectedStoreStale
	}
	if err := unix.Unlinkat(parent, name, 0); err != nil {
		return err
	}
	if err := store.inject("after-recovery-lifecycle-unlink"); err != nil {
		return err
	}
	return unix.Fsync(parent)
}

func (store *POSIXV2Store) removeRecoveryPair(parts []string, transaction TransactionID, expected []byte) error {
	name := string(transaction) + ".json"
	// Validate both copies before either deletion; a conflict preserves evidence.
	if _, err := store.readRecoveryPair(parts, transaction); err != nil {
		return err
	}
	for _, candidate := range []string{"." + name + ".pending", name} {
		if err := store.removeProtectedRecord(parts, candidate, expected); err != nil {
			return err
		}
	}
	return nil
}

// Include both selected and prepared journals and every ordinary immutable
// predecessor archive. Unrecognized or unsafe evidence makes retirement unsafe.
func (store *POSIXV2Store) recoveryReferenceRoots(cancelling ...*RecoveryReceiptV1) (map[TransactionID]struct{}, error) {
	roots := make(map[TransactionID]struct{})
	for _, name := range []string{"journal.json", ".journal.json.pending"} {
		snapshot, err := store.readRecord([]string{"release-transition", "v2"}, name)
		if err != nil {
			return nil, err
		}
		if snapshot.Exists {
			journal, err := ParseJournal(snapshot.Payload)
			if err != nil {
				return nil, err
			}
			canonical, err := MarshalJournal(journal)
			if err != nil || !bytes.Equal(snapshot.Payload, canonical) {
				return nil, invalid("noncanonical journal prevents recovery retirement")
			}
			if name == ".journal.json.pending" && len(cancelling) != 0 {
				if cancelling[0].MatchesSuccessor(journal) != nil || journal.Checkpoint != JournalAuthorized {
					return nil, invalid("unrecognized pending journal prevents lifecycle cancellation")
				}
				continue
			}
			roots[journal.Transaction] = struct{}{}
		}
	}
	parts := []string{"release-transition", "v2", "transactions"}
	entries, _, err := store.readDirectoryEntries(parts, maxTransactionGraphEntries)
	if err != nil {
		return nil, err
	}
	if len(entries) > maxTransactionGraphEntries {
		return nil, invalid("ordinary transaction horizon prevents recovery retirement")
	}
	for _, entry := range entries {
		transaction := TransactionID(entry.Name())
		if err := validateTransactionID(transaction); err != nil {
			return nil, err
		}
		directoryParts := append(append([]string(nil), parts...), string(transaction))
		parent, present, err := store.openParent(directoryParts, false)
		if err != nil || !present {
			return nil, invalid("unsafe ordinary transaction prevents recovery retirement")
		}
		unix.Close(parent)
		roots[transaction] = struct{}{}
		var published ProtectedSnapshot
		for _, name := range []string{"superseded-journal.json", ".superseded-journal.json.pending"} {
			snapshot, err := store.readRecord(directoryParts, name)
			if err != nil {
				return nil, err
			}
			if !snapshot.Exists {
				continue
			}
			if published.Exists && !bytes.Equal(published.Payload, snapshot.Payload) {
				return nil, invalid("pending ordinary archive conflicts with published evidence")
			}
			published = snapshot
			archive, err := ParseSupersededJournal(snapshot.Payload)
			if err != nil {
				return nil, err
			}
			canonical, err := MarshalSupersededJournal(archive)
			if err != nil || !bytes.Equal(canonical, snapshot.Payload) {
				return nil, invalid("noncanonical ordinary archive prevents recovery retirement")
			}
			roots[archive.Journal.Transaction] = struct{}{}
			roots[archive.Replacement.Transaction] = struct{}{}
		}
	}
	return roots, nil
}

// RecordCompletedRecovery captures terminal proof while the completed journal
// is still selected. Old owners without this witness remain conservative.
func (store *POSIXV2Store) RecordCompletedRecovery(current JournalRecord) error {
	if !IsRecoveryTransaction(current.Transaction) {
		return nil
	}
	if current.Checkpoint != JournalComplete {
		return invalid("recovery completion witness requires a completed journal")
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil {
		return err
	}
	payload, err := MarshalJournal(current)
	if err != nil || !snapshot.Exists || !bytes.Equal(payload, snapshot.Payload) {
		return invalid("completion witness is not the selected completed journal")
	}
	receipt, err := store.ValidatedCurrentRecovery(current)
	if err != nil {
		return err
	}
	if err := store.inject("before-recovery-completion"); err != nil {
		return err
	}
	if err := store.createLifecycleRecord(RecoveryLifecycleRecord{SchemaVersion: RecoveryLifecycleSchemaV1, Receipt: receipt, Completed: &current}); err != nil {
		return err
	}
	return store.inject("after-recovery-completion")
}

// CleanupRecovery only retires evidence with a validated terminal witness and
// no current, pending or ordinary-archive references. Receipt V1 stays intact.
func (store *POSIXV2Store) CleanupRecovery(current TransactionID) error {
	if store.retirementCaptureFailed {
		return invalid("recovery completion evidence could not be captured before journal replacement")
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil {
		return err
	}
	if !snapshot.Exists {
		return invalid("recovery cleanup requires a selected completed journal")
	}
	journal, err := ParseJournal(snapshot.Payload)
	if err != nil || journal.Transaction != current || journal.Checkpoint != JournalComplete {
		return invalid("recovery cleanup journal is not the completed current transaction")
	}
	roots, err := store.recoveryReferenceRoots()
	if err != nil {
		return err
	}
	receipts, err := store.readRecoveryReceiptsAt(recoveryLifecycleParts)
	if err != nil {
		return err
	}
	archive, err := store.readLifecycleArchive()
	if err != nil {
		return err
	}
	const retention = 32
	remaining := retention
	for transaction, receipt := range receipts {
		terminal, exists := archive[transaction]
		_, referenced := roots[transaction]
		if referenced || !exists || remaining == 0 {
			continue
		}
		if !sameRecoveryReceipt(terminal.Receipt, receipt) {
			return invalid("recovery retirement terminal witness differs from live receipt")
		}
		if err := store.createLifecycleRecord(terminal); err != nil {
			return err
		}
		if err := store.removeRecoveryPair(recoveryLifecycleParts, transaction, mustRecoveryReceiptPayload(receipt)); err != nil {
			return err
		}
		delete(receipts, transaction)
		remaining--
	}
	// Live reservations can depend on cancelled witnesses even before journal CAS.
	for _, receipt := range receipts {
		for transaction, terminal := range archive {
			if receipt.Reservation == fingerprintPayload(mustRecoveryReceiptPayload(terminal.Receipt)) {
				roots[transaction] = struct{}{}
			}
		}
	}
	remaining = retention
	archiveRetention := retention
	if _, captured := archive[current]; IsRecoveryTransaction(current) && !captured {
		archiveRetention--
	}
	for transaction, terminal := range archive {
		if len(archive) <= archiveRetention || remaining == 0 {
			break
		}
		_, referenced := roots[transaction]
		_, predecessorReferenced := roots[terminal.Receipt.Replacement.Transaction]
		_, live := receipts[transaction]
		if referenced || predecessorReferenced || live {
			continue
		}
		payload, err := MarshalRecoveryLifecycleRecord(terminal)
		if err != nil {
			return err
		}
		if err := store.removeRecoveryPair(recoveryArchiveParts, transaction, payload); err != nil {
			return err
		}
		delete(archive, transaction)
		remaining--
	}
	// The current receipt is a reference root even before its terminal witness
	// exists. Free eligible history first so a full archive cannot prevent that
	// witness from being captured forever.
	return store.RecordCompletedRecovery(journal)
}

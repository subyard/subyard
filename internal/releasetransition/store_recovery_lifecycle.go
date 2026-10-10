package releasetransition

import (
	"bytes"
	"os"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

var (
	recoveryLifecycleParts = []string{"release-transition", "recovery", "v2", "transactions"}
	recoveryArchiveParts   = []string{"release-transition", "recovery", "v2", "archive"}
	recoveryFrontierParts  = []string{"release-transition", "recovery", "v2", "frontiers"}
)

const (
	maxRecoveryLifecycleEntries = 512
	maxRecoveryLifecycleBytes   = 32 << 20
)

func recoveryEntryTransaction(name string) (TransactionID, error) {
	if strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".json.pending") {
		name = strings.TrimSuffix(strings.TrimPrefix(name, "."), ".json.pending")
	} else if strings.HasSuffix(name, ".json") {
		name = strings.TrimSuffix(name, ".json")
	} else {
		return "", invalid("unrecognized recovery entry")
	}
	transaction := TransactionID(name)
	return transaction, validateTransactionID(transaction)
}

// Validate the physical read budget before retaining parsed payloads. Published
// and pending copies count as one logical record but both consume bytes.
func (store *POSIXV2Store) recoveryEntries(parts []string) ([]os.DirEntry, int64, error) {
	limit, byteLimit := maxTransactionGraphEntries, int64(maxTransactionGraphEntries*2*MaxProtectedRecordBytes)
	if !slices.Equal(parts, recoveryReceiptParts) {
		limit, byteLimit = maxRecoveryLifecycleEntries, maxRecoveryLifecycleBytes
	}
	entries, _, err := store.readDirectoryEntries(parts, limit*2)
	if err != nil {
		return nil, 0, err
	}
	if len(entries) > limit*2 {
		return nil, 0, invalid("recovery namespace physical entry limit is full")
	}
	parent, present, err := store.openParent(parts, false)
	if err != nil || !present {
		return entries, 0, err
	}
	defer unix.Close(parent)
	logical := make(map[TransactionID]struct{})
	var total int64
	for _, entry := range entries {
		transaction, err := recoveryEntryTransaction(entry.Name())
		if err != nil {
			return nil, 0, err
		}
		logical[transaction] = struct{}{}
		var info unix.Stat_t
		if err := unix.Fstatat(parent, entry.Name(), &info, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return nil, 0, err
		}
		if info.Mode&unix.S_IFMT != unix.S_IFREG || info.Mode&0o777 != 0o600 || info.Size > MaxProtectedRecordBytes {
			return nil, 0, invalid("unsafe recovery record prevents bounded inspection")
		}
		total += info.Size
		if total > byteLimit {
			return nil, 0, invalid("recovery namespace aggregate byte limit is full")
		}
	}
	if len(logical) > limit {
		return nil, 0, invalid("recovery namespace logical entry limit is full")
	}
	return entries, total, nil
}

func (store *POSIXV2Store) reserveLifecycleBytes(parts []string, transaction TransactionID, payload []byte) error {
	entries, total, err := store.recoveryEntries(parts)
	if err != nil {
		return err
	}
	known := make(map[TransactionID]struct{})
	for _, entry := range entries {
		id, _ := recoveryEntryTransaction(entry.Name())
		known[id] = struct{}{}
	}
	for _, name := range []string{string(transaction) + ".json", "." + string(transaction) + ".json.pending"} {
		snapshot, err := store.readRecord(parts, name)
		if err != nil {
			return err
		}
		if snapshot.Exists && bytes.Equal(snapshot.Payload, payload) {
			return nil
		}
	}
	if _, exists := known[transaction]; !exists && len(known) >= maxRecoveryLifecycleEntries {
		return invalid("recovery lifecycle namespace horizon is full")
	}
	// Reserve the temporary pending write as well as the existing durable copy.
	if total+int64(len(payload)) > maxRecoveryLifecycleBytes {
		return invalid("recovery lifecycle namespace aggregate byte budget is full")
	}
	return nil
}

func (store *POSIXV2Store) readFrontierPair(transaction TransactionID) (RecoveryFrontier, ProtectedSnapshot, ProtectedSnapshot, error) {
	name := string(transaction) + ".json"
	published, err := store.readRecord(recoveryFrontierParts, name)
	if err != nil {
		return RecoveryFrontier{}, published, ProtectedSnapshot{}, err
	}
	pending, err := store.readRecord(recoveryFrontierParts, "."+name+".pending")
	if err != nil {
		return RecoveryFrontier{}, published, pending, err
	}
	parse := func(snapshot ProtectedSnapshot) (RecoveryFrontier, error) {
		if !snapshot.Exists {
			return RecoveryFrontier{}, nil
		}
		value, err := ParseRecoveryFrontier(snapshot.Payload)
		if err != nil {
			return value, err
		}
		canonical, err := MarshalRecoveryFrontier(value)
		if err != nil || !bytes.Equal(canonical, snapshot.Payload) || value.Replacement.Transaction != transaction {
			return value, invalid("recovery frontier is not canonical predecessor state")
		}
		return value, nil
	}
	before, err := parse(published)
	if err != nil {
		return before, published, pending, err
	}
	if !pending.Exists {
		return before, published, pending, nil
	}
	after, err := parse(pending)
	if err != nil {
		return after, published, pending, err
	}
	if published.Exists && bytes.Equal(published.Payload, pending.Payload) {
		return after, published, pending, nil
	}
	if (!published.Exists && after.Generation != 1) || (published.Exists &&
		(after.Replacement != before.Replacement || after.Generation <= before.Generation || after.Generation-before.Generation != 1 ||
			after.Cancellation.Receipt.Reservation != before.Reservation())) {
		return after, published, pending, invalid("pending recovery frontier is not the exact next generation")
	}
	return after, published, pending, nil
}

func (store *POSIXV2Store) recoveryFrontier(request ActivationOnlyRecoveryRequest) (RecoveryFrontier, bool, error) {
	if err := request.Validate(); err != nil {
		return RecoveryFrontier{}, false, err
	}
	frontier, published, pending, err := store.readFrontierPair(request.Transaction)
	found := published.Exists || pending.Exists
	if err == nil && found && frontier.Replacement != request {
		err = invalid("recovery frontier refers to another predecessor fingerprint")
	}
	return frontier, found, err
}

func (store *POSIXV2Store) publishRecoveryFrontier(frontier RecoveryFrontier) error {
	payload, err := MarshalRecoveryFrontier(frontier)
	if err != nil {
		return err
	}
	actual, published, pending, err := store.readFrontierPair(frontier.Replacement.Transaction)
	if err != nil {
		return err
	}
	name := string(frontier.Replacement.Transaction) + ".json"
	if pending.Exists {
		// Never discard a fsynced generation intent when subsequent inputs differ.
		if err := store.compareAndSwap(recoveryFrontierParts, name, published, pending.Payload); err != nil {
			return err
		}
		published = pending
	}
	if published.Exists && bytes.Equal(published.Payload, payload) {
		return nil
	}
	if (!published.Exists && frontier.Generation != 1) || (published.Exists &&
		(frontier.Replacement != actual.Replacement || frontier.Generation <= actual.Generation || frontier.Generation-actual.Generation != 1 ||
			frontier.Cancellation.Receipt.Reservation != actual.Reservation())) {
		return invalid("recovery frontier cannot skip or overwrite a generation")
	}
	if err := store.reserveLifecycleBytes(recoveryFrontierParts, frontier.Replacement.Transaction, payload); err != nil {
		return err
	}
	if err := store.inject("before-recovery-frontier"); err != nil {
		return err
	}
	if err := store.compareAndSwap(recoveryFrontierParts, name, published, payload); err != nil {
		return err
	}
	return store.inject("after-recovery-frontier")
}

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
	entries, _, err := store.recoveryEntries(recoveryArchiveParts)
	if err != nil {
		return nil, err
	}
	if len(entries) > maxRecoveryLifecycleEntries*2 {
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
	if len(result) > maxRecoveryLifecycleEntries {
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
	if _, exists := archive[transaction]; !exists && len(archive) >= maxRecoveryLifecycleEntries {
		return invalid("recovery lifecycle archive horizon is full")
	}
	if err := store.reserveLifecycleBytes(recoveryArchiveParts, transaction, payload); err != nil {
		return err
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
	if present && (terminal.Cancellation != nil || terminal.PredecessorCompleted != nil || !sameRecoveryReceipt(terminal.Receipt, receipt)) {
		return RecoveryReceiptV1{}, false, invalid("recovery lifecycle successor is cancelled or conflicts with terminal evidence")
	}
	frontier, found, err := store.recoveryFrontier(receipt.Replacement)
	if err != nil || !found || frontier.Generation != receipt.Generation || frontier.Reservation() != receipt.Reservation {
		return RecoveryReceiptV1{}, false, invalid("lifecycle publication does not match its frontier")
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
	frontier, published, pending, err := store.readFrontierPair(predecessor)
	if err != nil {
		return err
	}
	if published.Exists || pending.Exists || frontier.Generation != 0 {
		return invalid("recovery V1 cannot bypass a lifecycle frontier")
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
	frontier, hasFrontier, err := store.recoveryFrontier(request)
	if err != nil {
		return "", err
	}
	if found {
		if !hasFrontier {
			return "", invalid("live lifecycle receipt has no durable frontier")
		}
		terminal, present, err := store.readLifecycleRecord(receipt.Successor.Transaction)
		if err != nil {
			return "", err
		}
		if present && (!sameRecoveryReceipt(terminal.Receipt, receipt) || terminal.Completed != nil || terminal.PredecessorCompleted != nil) {
			return "", invalid("lifecycle reservation has conflicting or completed terminal evidence")
		}
		if receipt.Generation != frontier.Generation {
			if receipt.Generation == ^uint64(0) || frontier.Generation != receipt.Generation+1 || frontier.Cancellation == nil || !sameRecoveryReceipt(frontier.Cancellation.Receipt, receipt) {
				return "", invalid("live lifecycle receipt disagrees with its durable frontier")
			}
			return frontier.Reservation(), nil
		}
		if receipt.Reservation != frontier.Reservation() {
			return "", invalid("lifecycle receipt does not bind its frontier reservation")
		}
		if receipt.Generation == ^uint64(0) && (present && terminal.Cancellation != nil || receipt.BasePlan != basePlan) {
			return "", invalid("recovery lifecycle generation exhausted")
		}
		if present && terminal.Cancellation != nil {
			// A durable cancellation is irrevocable. A later desired state binds
			// that exact intent and still requires a newly verified grant.
			return fingerprintPayload(mustRecoveryReceiptPayload(receipt)), nil
		}
		if receipt.BasePlan != basePlan {
			return fingerprintPayload(mustRecoveryReceiptPayload(receipt)), nil
		}
		return receipt.Reservation, nil
	}
	if hasFrontier {
		return frontier.Reservation(), nil
	}
	// A missing frontier cannot silently convert historical evidence to a new
	// baseline. Modern writers always publish it before the first live receipt.
	archive, err := store.readLifecycleArchive()
	if err != nil {
		return "", err
	}
	for _, record := range archive {
		if record.Receipt.Replacement.Transaction == request.Transaction {
			return "", invalid("lifecycle history has no protected predecessor frontier")
		}
	}
	return "", nil
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
	if err := store.retireClosedRecoveryEvidence(); err != nil {
		return "", err
	}
	receipt, found, err := store.lifecycleReservation(request)
	if err != nil {
		return "", err
	}
	if found {
		terminal, cancelled, err := store.readLifecycleRecord(receipt.Successor.Transaction)
		if err != nil {
			return "", err
		}
		if !cancelled && receipt.BasePlan == basePlan {
			if receipt.Successor.AuthorizationPlan != plan {
				return "", invalid("lifecycle reservation has another authorization plan")
			}
			return receipt.Successor.Transaction, nil
		}
		if cancelled && terminal.Cancellation == nil {
			return "", invalid("completed lifecycle successor cannot be cancelled")
		}
		if err := store.cancelLifecycleReceipt(receipt, current, basePlan, plan, grantDigest); err != nil {
			return "", err
		}
	}
	frontier, present, err := store.recoveryFrontier(request)
	if err != nil {
		return "", err
	}
	if !present {
		frontier = RecoveryFrontier{SchemaVersion: RecoveryLifecycleSchemaV1, Replacement: request, Generation: 1}
	}
	if err := store.publishRecoveryFrontier(frontier); err != nil {
		return "", err
	}
	if err := store.compactLifecycleCancellations(request); err != nil {
		return "", err
	}
	if err := store.pruneLifecycleHistory(32, 32); err != nil {
		return "", err
	}
	transaction, err := RecoveryLifecycleTransaction(request, frontier.Generation)
	if err != nil {
		return "", err
	}
	archive, err := store.readLifecycleArchive()
	if err != nil {
		return "", err
	}
	if _, exists := archive[transaction]; exists {
		return "", invalid("lifecycle successor transaction already has terminal evidence")
	}
	receipts, err := store.readRecoveryReceiptsAt(recoveryLifecycleParts)
	if err != nil {
		return "", err
	}
	if _, exists := receipts[transaction]; exists || len(receipts) >= maxRecoveryLifecycleEntries {
		return "", invalid("lifecycle successor exists or active recovery horizon is full")
	}
	return transaction, nil
}

func (store *POSIXV2Store) createLifecycleRecoveryReceipt(transaction TransactionID, receipt RecoveryReceiptV1, payload []byte) error {
	current, err := store.ReadCurrentJournal()
	if err != nil {
		return err
	}
	if !current.Exists || current.Fingerprint != receipt.Replacement.Fingerprint || !bytes.Equal(current.Payload, mustJournalRecoveryPayload(receipt.Predecessor)) {
		return invalid("lifecycle publication requires its exact selected predecessor")
	}
	reservation, err := store.RecoveryReservation(receipt.Replacement, receipt.BasePlan)
	if err != nil {
		return err
	}
	if reservation != receipt.Reservation {
		return invalid("lifecycle receipt does not bind the selected reservation")
	}
	frontier, present, err := store.recoveryFrontier(receipt.Replacement)
	if err != nil {
		return err
	}
	if !present {
		frontier = RecoveryFrontier{SchemaVersion: RecoveryLifecycleSchemaV1, Replacement: receipt.Replacement, Generation: 1}
		if err := store.retireClosedRecoveryEvidence(); err != nil {
			return err
		}
	}
	if frontier.Generation != receipt.Generation || frontier.Reservation() != receipt.Reservation {
		return invalid("lifecycle receipt attempts to reuse or skip a generation")
	}
	if err := store.publishRecoveryFrontier(frontier); err != nil {
		return err
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
	if _, err := store.readRecoveryReceiptsAt(recoveryLifecycleParts); err != nil {
		return err
	}
	if err := store.reserveLifecycleBytes(recoveryLifecycleParts, transaction, payload); err != nil {
		return err
	}
	return store.createImmutable(recoveryLifecycleParts, string(transaction)+".json", payload)
}

func mustJournalRecoveryPayload(journal JournalRecord) []byte {
	payload, _ := MarshalJournal(journal)
	return payload
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
	if receipt.Generation == ^uint64(0) {
		return invalid("recovery lifecycle generation exhausted")
	}
	if err := store.discardLifecycleJournalPending(receipt); err != nil {
		return err
	}
	record, present, err := store.readLifecycleRecord(receipt.Successor.Transaction)
	if err != nil {
		return err
	}
	if present {
		if record.Cancellation == nil || !sameRecoveryReceipt(record.Receipt, receipt) {
			return invalid("lifecycle reservation has conflicting terminal evidence")
		}
	} else {
		record = RecoveryLifecycleRecord{SchemaVersion: RecoveryLifecycleSchemaV1, Receipt: receipt,
			Cancellation: &RecoveryCancellation{BasePlan: basePlan, Plan: plan, AuthorizationDigest: grantDigest}}
	}
	if err := store.inject("before-recovery-cancellation"); err != nil {
		return err
	}
	if err := store.compactLifecycleCancellations(receipt.Replacement); err != nil {
		return err
	}
	if err := store.pruneLifecycleHistory(32, 32); err != nil {
		return err
	}
	if err := store.createLifecycleRecord(record); err != nil {
		return err
	}
	if err := store.inject("after-recovery-cancellation"); err != nil {
		return err
	}
	frontier := RecoveryFrontier{SchemaVersion: RecoveryLifecycleSchemaV1, Replacement: receipt.Replacement, Generation: receipt.Generation + 1, Cancellation: &record}
	if err := store.publishRecoveryFrontier(frontier); err != nil {
		return err
	}
	return store.removeRecoveryPair(recoveryLifecycleParts, receipt.Successor.Transaction, mustRecoveryReceiptPayload(receipt))
}

// The frontier makes older unselected cancellations unreachable and their
// generation identities ineligible. Keep its latest witness and every actual
// journal/live reservation reference; no count or age authorizes retirement.
func (store *POSIXV2Store) compactLifecycleCancellations(request ActivationOnlyRecoveryRequest) error {
	frontier, present, err := store.recoveryFrontier(request)
	if err != nil || !present {
		return err
	}
	archive, err := store.readLifecycleArchive()
	if err != nil {
		return err
	}
	roots, err := store.recoveryReferenceRoots()
	if err != nil {
		return err
	}
	live, err := store.readRecoveryReceiptsAt(recoveryLifecycleParts)
	if err != nil {
		return err
	}
	remaining := 32
	for transaction, record := range archive {
		if record.Cancellation == nil || record.Receipt.Replacement != request || record.Receipt.Generation >= frontier.Generation-1 || remaining == 0 {
			continue
		}
		if _, referenced := roots[transaction]; referenced {
			continue
		}
		if _, exists := live[transaction]; exists {
			continue
		}
		fingerprint := fingerprintPayload(mustRecoveryReceiptPayload(record.Receipt))
		referenced := false
		for _, receipt := range live {
			if receipt.Reservation == fingerprint {
				referenced = true
			}
		}
		if referenced {
			continue
		}
		payload, err := MarshalRecoveryLifecycleRecord(record)
		if err != nil {
			return err
		}
		if err := store.inject("before-recovery-cancellation-compaction"); err != nil {
			return err
		}
		if err := store.removeRecoveryPair(recoveryArchiveParts, transaction, payload); err != nil {
			return err
		}
		remaining--
	}
	return nil
}

// This pre-admission cleanup is also valid while the predecessor is unfinished:
// only terminal evidence outside all roots can retire.
func (store *POSIXV2Store) pruneLifecycleHistory(budget, retention int) error {
	archive, err := store.readLifecycleArchive()
	if err != nil {
		return err
	}
	roots, err := store.recoveryReferenceRoots()
	if err != nil {
		return err
	}
	live, err := store.readRecoveryReceiptsAt(recoveryLifecycleParts)
	if err != nil {
		return err
	}
	legacy, err := store.readRecoveryReceipts()
	if err != nil {
		return err
	}
	for transaction, receipt := range legacy {
		live[transaction] = receipt
	}
	frontiers, err := store.readRecoveryFrontiers()
	if err != nil {
		return err
	}
	for _, frontier := range frontiers {
		if frontier.Cancellation != nil {
			roots[frontier.Cancellation.Receipt.Successor.Transaction] = struct{}{}
		}
	}
	for transaction, record := range archive {
		if len(archive) <= retention || budget == 0 {
			break
		}
		_, selected := roots[transaction]
		_, predecessor := roots[record.Receipt.Replacement.Transaction]
		_, exists := live[transaction]
		if selected || predecessor || exists {
			continue
		}
		fingerprint := fingerprintPayload(mustRecoveryReceiptPayload(record.Receipt))
		referenced := false
		for _, receipt := range live {
			if receipt.Reservation == fingerprint {
				referenced = true
			}
		}
		if referenced {
			continue
		}
		payload, err := MarshalRecoveryLifecycleRecord(record)
		if err != nil {
			return err
		}
		if err := store.removeRecoveryPair(recoveryArchiveParts, transaction, payload); err != nil {
			return err
		}
		delete(archive, transaction)
		budget--
	}
	return nil
}

func (store *POSIXV2Store) readRecoveryFrontiers() (map[TransactionID]RecoveryFrontier, error) {
	entries, _, err := store.recoveryEntries(recoveryFrontierParts)
	if err != nil {
		return nil, err
	}
	frontiers := make(map[TransactionID]RecoveryFrontier)
	for _, entry := range entries {
		transaction, err := recoveryEntryTransaction(entry.Name())
		if err != nil {
			return nil, err
		}
		if _, exists := frontiers[transaction]; exists {
			continue
		}
		frontier, _, _, err := store.readFrontierPair(transaction)
		if err != nil {
			return nil, err
		}
		frontiers[transaction] = frontier
	}
	return frontiers, nil
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

// RecordCompletedRecovery captures a selected completed successor or the exact
// completed original predecessor that makes an unselected receipt obsolete.
// The overwrite boundary calls this before replacing any completed journal.
func (store *POSIXV2Store) RecordCompletedRecovery(current JournalRecord) error {
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
	if IsRecoveryTransaction(current.Transaction) {
		receipt, err := store.ValidatedCurrentRecovery(current)
		if err != nil {
			return err
		}
		return store.captureRecoveryCompletion(RecoveryLifecycleRecord{SchemaVersion: RecoveryLifecycleSchemaV1, Receipt: receipt, Completed: &current})
	}
	// Validate reference metadata before declaring any legacy/current reservation
	// closed; unknown journals or archives must leave all evidence intact.
	if _, err := store.recoveryReferenceRoots(); err != nil {
		return err
	}
	live, err := store.readRecoveryReceiptsAt(recoveryLifecycleParts)
	if err != nil {
		return err
	}
	legacy, err := store.readRecoveryReceipts()
	if err != nil {
		return err
	}
	for transaction, receipt := range legacy {
		live[transaction] = receipt
	}
	var matching *RecoveryReceiptV1
	for _, receipt := range live {
		if receipt.Replacement.Transaction != current.Transaction {
			continue
		}
		if matching != nil {
			return invalid("multiple recovery reservations refer to the completed predecessor")
		}
		copy := receipt
		matching = &copy
	}
	if matching == nil {
		return nil
	}
	record := RecoveryLifecycleRecord{SchemaVersion: RecoveryLifecycleSchemaV1, Receipt: *matching, PredecessorCompleted: &current}
	if err := record.Validate(); err != nil {
		return err
	}
	terminal, present, err := store.readLifecycleRecord(matching.Successor.Transaction)
	if err != nil {
		return err
	}
	if present {
		if !sameRecoveryReceipt(terminal.Receipt, *matching) || terminal.Completed != nil {
			return invalid("completed predecessor conflicts with selected successor evidence")
		}
		// A pre-CAS cancellation is already a durable closure witness.
		if terminal.Cancellation != nil {
			return nil
		}
	}
	return store.captureRecoveryCompletion(record)
}

func (store *POSIXV2Store) captureRecoveryCompletion(record RecoveryLifecycleRecord) error {
	if err := store.inject("before-recovery-completion"); err != nil {
		return err
	}
	archive, err := store.readLifecycleArchive()
	if err != nil {
		return err
	}
	payload, err := MarshalRecoveryLifecycleRecord(record)
	if err != nil {
		return err
	}
	_, total, err := store.recoveryEntries(recoveryArchiveParts)
	if err != nil {
		return err
	}
	if _, exists := archive[record.Receipt.Successor.Transaction]; !exists &&
		(len(archive) >= maxRecoveryLifecycleEntries || total+int64(len(payload)) > maxRecoveryLifecycleBytes) {
		// The overwrite hook must also free eligible capacity before capture;
		// otherwise its best-effort warning would strand a provably closed receipt.
		if err := store.retireClosedRecoveryEvidence(); err != nil {
			return err
		}
		if err := store.pruneLifecycleHistory(32, 31); err != nil {
			return err
		}
	}
	if err := store.createLifecycleRecord(record); err != nil {
		return err
	}
	return store.inject("after-recovery-completion")
}

// retireClosedRecoveryEvidence also runs before admission. Completion is proved
// by an exact archived witness; absent current references alone never suffice.
func (store *POSIXV2Store) retireClosedRecoveryEvidence() error {
	roots, err := store.recoveryReferenceRoots()
	if err != nil {
		return err
	}
	live, err := store.readRecoveryReceiptsAt(recoveryLifecycleParts)
	if err != nil {
		return err
	}
	legacy, err := store.readRecoveryReceipts()
	if err != nil {
		return err
	}
	archive, err := store.readLifecycleArchive()
	if err != nil {
		return err
	}
	frontiers, err := store.readRecoveryFrontiers()
	if err != nil {
		return err
	}
	for transaction, receipt := range legacy {
		live[transaction] = receipt
	}
	remaining := 32
	for transaction, receipt := range live {
		terminal, exists := archive[transaction]
		_, selected := roots[transaction]
		_, predecessor := roots[receipt.Replacement.Transaction]
		if selected || predecessor || !exists || remaining == 0 {
			continue
		}
		if !sameRecoveryReceipt(terminal.Receipt, receipt) {
			return invalid("recovery retirement witness differs from live receipt")
		}
		if terminal.Cancellation != nil {
			frontier, present := frontiers[receipt.Replacement.Transaction]
			if !present || receipt.Generation >= frontier.Generation {
				return invalid("cancelled receipt has no durable retired generation")
			}
		}
		// Another live binding must not lose the receipt that supplies it.
		referenced := false
		fingerprint := fingerprintPayload(mustRecoveryReceiptPayload(receipt))
		for other, dependent := range live {
			if other != transaction && (dependent.Reservation == fingerprint || dependent.Replacement.Transaction == transaction) {
				referenced = true
			}
		}
		if referenced {
			continue
		}
		if err := store.createLifecycleRecord(terminal); err != nil {
			return err
		}
		if err := store.inject("before-recovery-live-retirement"); err != nil {
			return err
		}
		if err := store.removeRecoveryPair(recoveryParts(transaction), transaction, mustRecoveryReceiptPayload(receipt)); err != nil {
			return err
		}
		delete(live, transaction)
		remaining--
	}
	remaining = 32
	for predecessor, frontier := range frontiers {
		if remaining == 0 {
			break
		}
		transaction, err := RecoveryLifecycleTransaction(frontier.Replacement, frontier.Generation)
		if err != nil {
			return err
		}
		_, selected := roots[transaction]
		_, predecessorSelected := roots[predecessor]
		if selected || predecessorSelected {
			continue
		}
		pinned := false
		for _, receipt := range live {
			if receipt.Replacement.Transaction == predecessor {
				pinned = true
			}
		}
		if pinned {
			continue
		}
		// Every receipt has either been retired with proof or remains in live;
		// no active generation or selected predecessor survives this frontier.
		_, published, pending, err := store.readFrontierPair(predecessor)
		if err != nil {
			return err
		}
		// Mutable pending and published can differ by one generation. Each was
		// validated above; remove its exact payload separately under the lock.
		name := string(predecessor) + ".json"
		if pending.Exists {
			if err := store.removeProtectedRecord(recoveryFrontierParts, "."+name+".pending", pending.Payload); err != nil {
				return err
			}
		}
		if published.Exists {
			if err := store.removeProtectedRecord(recoveryFrontierParts, name, published.Payload); err != nil {
				return err
			}
		}
		remaining--
	}
	return nil
}

// CleanupRecovery preserves the current completed journal and retires only
// closed, unreferenced evidence. Unwitnessed legacy V1 reservations stay intact.
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
	if err := store.retireClosedRecoveryEvidence(); err != nil {
		return err
	}
	archive, err := store.readLifecycleArchive()
	if err != nil {
		return err
	}
	retention := 32
	if _, captured := archive[current]; IsRecoveryTransaction(current) && !captured {
		retention--
	} else if !IsRecoveryTransaction(current) {
		// Reserve a slot for a possible unselected predecessor closure witness.
		retention--
	}
	if err := store.pruneLifecycleHistory(32, retention); err != nil {
		return err
	}
	// Free eligible history before capturing the selected completion witness.
	return store.RecordCompletedRecovery(journal)
}

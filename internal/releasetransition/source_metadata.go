package releasetransition

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"golang.org/x/sys/unix"
)

// SourceMetadata is read-only admission evidence, never a release authorization
// or a claim that the original owner's operational resources remain ready.
type SourceMetadata struct {
	Journal *JournalRecord
	Ledger  MigrationLedgerSnapshot
	Binding Fingerprint
}

// InspectSourceMetadata validates the finite protected namespaces without
// publishing, cleaning up, observing historical resources, or changing history.
// The adapter must separately verify the exact sealed owner, assets and links.
func (store *POSIXV2Store) InspectSourceMetadata(registry RegistryV2, digest Fingerprint) (SourceMetadata, error) {
	before, err := store.sourceMetadataInventory()
	if err != nil {
		return SourceMetadata{}, err
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil {
		return SourceMetadata{}, err
	}
	proof := SourceMetadata{}
	if snapshot.Exists {
		journal, err := ParseJournal(snapshot.Payload)
		if err != nil {
			return proof, err
		}
		canonical, err := MarshalJournal(journal)
		if err != nil || !bytes.Equal(canonical, snapshot.Payload) {
			return proof, errors.New("source repair requires a canonical protected journal")
		}
		if journal.RegistryDigest != digest || journal.CatalogDigest != BuiltinCapabilityCatalog().Digest() || journal.SourceIngress != nil {
			return proof, errors.New("source repair requires the exact supported original registry and catalog without source ingress")
		}
		if journal.Checkpoint != JournalComplete && (journal.Checkpoint != JournalReconciling || journal.Goal.Direction != DirectionActivateTarget || len(journal.Steps) != 0 || IsRecoveryTransaction(journal.Transaction)) {
			return proof, errors.New("source repair requires completed history or an original activation-only reconciling journal")
		}
		if err := store.ValidateCurrentRecovery(journal); err != nil {
			return proof, err
		}
		proof.Journal = &journal
	}
	proof.Ledger, err = store.ReadMigrationLedger(registry, digest)
	if err != nil {
		return proof, err
	}
	if proof.Journal != nil {
		if err := store.sourceTransactionArtifacts(*proof.Journal); err != nil {
			return proof, err
		}
		pending, err := registry.PendingPath(proof.Ledger.Ledger)
		if err != nil || len(pending) != 0 || !proof.Ledger.AuthoritySnapshot.Exists {
			return proof, errors.New("source repair requires the complete original migration ledger")
		}
		if proof.Journal.Checkpoint == JournalComplete {
			if err := store.sourceTerminalEvidence(*proof.Journal, registry, proof.Ledger); err != nil {
				return proof, err
			}
		}
	}
	current := TransactionID("")
	if proof.Journal != nil {
		current = proof.Journal.Transaction
	}
	graph, err := store.inspectTransactionGraph(current, proof.Journal)
	if err != nil {
		return proof, err
	}
	if graph != nil {
		if len(graph.pending) != 0 {
			return proof, errors.New("finish the pending protected archive publication before source repair")
		}
		terminals, err := store.readLifecycleArchive()
		if err != nil {
			return proof, err
		}
		journals := map[TransactionID]JournalRecord{}
		if proof.Journal != nil {
			journals[proof.Journal.Transaction] = *proof.Journal
		}
		for _, archive := range graph.archives {
			journals[archive.Journal.Transaction] = archive.Journal
		}
		for transaction, terminal := range terminals {
			journals[terminal.Receipt.Predecessor.Transaction] = terminal.Receipt.Predecessor
			journals[terminal.Receipt.Successor.Transaction] = terminal.Receipt.Successor
			if terminal.Completed != nil {
				journals[transaction] = *terminal.Completed
			}
			if terminal.PredecessorCompleted != nil {
				journals[terminal.PredecessorCompleted.Transaction] = *terminal.PredecessorCompleted
			}
		}
		if proof.Journal == nil {
			for transaction := range graph.directories {
				closed, exists := journals[transaction]
				if !exists || closed.Checkpoint != JournalComplete {
					return proof, errors.New("source repair without a current journal requires terminal typed provenance for every retained transaction")
				}
			}
		}
		for transaction := range graph.directories {
			bound, exists := journals[transaction]
			if !exists {
				bound, err = store.sourceRetainedTerminalEvidence(transaction, registry)
				if err != nil {
					return proof, err
				}
			}
			if err := store.sourceTerminalBindings(bound, registry, ""); err != nil {
				return proof, err
			}
		}
		// Existing typed supersession can retain unfinished predecessors at
		// several generations. Follow the already validated unique graph to a
		// terminal successor rather than reopening any predecessor's resources.
		next := map[TransactionID]TransactionID{}
		for successor, archive := range graph.archives {
			next[archive.Journal.Transaction] = successor
		}
		for successor, archive := range graph.archives {
			selected, exists := journals[successor]
			if !exists || selected.AuthorizationPlan != archive.AuthorizationPlan {
				return proof, errors.New("source repair requires bound typed archive authorization")
			}
			for count := 0; selected.Checkpoint != JournalComplete; count++ {
				if count >= len(graph.archives) {
					return proof, errors.New("source repair requires terminal typed closure of archived authorization")
				}
				selected, exists = journals[next[selected.Transaction]]
				if !exists {
					return proof, errors.New("source repair requires terminal typed closure of archived authorization")
				}
			}
		}

	}
	if err := store.sourceRecoveryClosed(); err != nil {
		return proof, err
	}
	after, err := store.sourceMetadataInventory()
	if err != nil {
		return proof, err
	}
	if before != after {
		return proof, ErrProtectedStoreStale
	}
	proof.Binding = before
	return proof, nil
}

func (store *POSIXV2Store) sourceTerminalEvidence(journal JournalRecord, registry RegistryV2, state MigrationLedgerSnapshot) error {
	return store.sourceTerminalBindings(journal, registry, state.ProjectionSnapshot.Fingerprint)
}

func (store *POSIXV2Store) sourceTerminalBindings(journal JournalRecord, registry RegistryV2, projection Fingerprint) error {
	if err := store.sourceTransactionArtifacts(journal); err != nil {
		return err
	}
	ledger := BaselineLedgerV2(registry)
	path, err := registry.PendingPath(ledger)
	if err != nil {
		return err
	}
	type ledgerEdge struct {
		migration     MigrationDefinitionV2
		before, after Fingerprint
	}
	edges := map[string]ledgerEdge{}
	_, prior, err := MarshalLedgerV2(ledger, registry)
	if err != nil {
		return err
	}
	for _, migration := range path {
		next, err := ledger.Advance(registry, migration)
		if err != nil {
			return err
		}
		_, desired, err := MarshalLedgerV2(next, registry)
		if err != nil {
			return err
		}
		edges[migration.ID] = ledgerEdge{migration, prior, desired}
		ledger, prior = next, desired
	}
	last := Fingerprint("")
	advanced := map[string]bool{}
	for _, step := range journal.Steps {
		if step.ID == step.Migration+".ledger" {
			advanced[step.Migration] = true
		}
	}
	for _, step := range journal.Steps {
		edge, known := edges[step.Migration]
		if !known || !advanced[step.Migration] || step.Checkpoint != StepVerified || step.Evidence == nil {
			return errors.New("source repair requires supported terminal migration evidence")
		}
		if step.ID == edge.migration.ID+".ledger" {
			if step.Evidence.Recovery != "" {
				return errors.New("terminal ledger evidence cannot carry a recovery payload")
			}
			initialAbsent := len(path) != 0 && edge.migration.ID == path[0].ID && step.Expected == absentProtectedSnapshot().Fingerprint
			if step.Resource != "ledger."+edge.migration.Domain || step.Decision != DecisionTransform || step.Desired != edge.after || (!initialAbsent && step.Expected != edge.before) || (last != "" && last != step.Expected) {
				return errors.New("terminal ledger evidence does not bind the original registry prefix")
			}
			last = step.Desired
		} else {
			supported := edge.migration.Kind == "test-vms-settings-v1-to-v2" && domain.SafeName(step.Resource) && step.ID == edge.migration.ID+"."+step.Resource && (step.Decision == DecisionCanonicalize || step.Decision == DecisionReset)
			supported = supported || edge.migration.Kind == "test-yard-owner-v1-to-v2" && step.Resource == ownerRegistrationResource && step.ID == edge.migration.ID+"."+ownerRegistrationResource && step.Decision == DecisionCanonicalize
			if !supported {
				return errors.New("source repair requires a supported original terminal step grammar")
			}
			if err := store.sourceTypedRecovery(journal, step, edge.migration); err != nil {
				return err
			}
		}
		for _, checkpoint := range []EvidenceCheckpoint{EvidenceCaptured, EvidenceApplied, EvidenceVerified} {
			snapshot, err := store.ReadCheckpointEvidence(journal.Transaction, step.ID, checkpoint)
			if err != nil || !snapshot.Exists {
				return errors.New("terminal checkpoint evidence is missing or unavailable")
			}
			evidence, err := ParseEvidence(snapshot.Payload)
			canonical, marshalErr := MarshalEvidence(evidence)
			redacted := evidence
			redacted.Recovery = ""
			if err != nil || marshalErr != nil || !bytes.Equal(canonical, snapshot.Payload) || evidence.Recovery != step.Evidence.Recovery || !postActivationEvidenceMatches(journal, step, checkpoint, redacted) || checkpoint == EvidenceVerified && !evidenceRecordsEqual(evidence, *step.Evidence) {
				return errors.New("terminal checkpoint evidence does not match the original journal")
			}
		}
	}
	if projection != "" && last != "" && last != projection {
		return errors.New("terminal ledger evidence differs from the authoritative projection")
	}
	return nil
}

// Older ordinary publication can replace a completed journal without archiving
// it. Its retained records prove closed checkpoints only, never an authorization
// or readiness claim. Recover their finite supported step bindings and validate
// every triple and typed before-image through the same terminal grammar.
func (store *POSIXV2Store) sourceRetainedTerminalEvidence(transaction TransactionID, registry RegistryV2) (JournalRecord, error) {
	bound := JournalRecord{Transaction: transaction}
	parts := []string{"release-transition", "v2", "transactions", string(transaction), "evidence"}
	entries, _, err := store.readDirectoryEntries(parts, 8192)
	if err != nil {
		return bound, err
	}
	verified := map[string]EvidenceRecord{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".verified.json") {
			continue
		}
		step := strings.TrimSuffix(entry.Name(), ".verified.json")
		snapshot, err := store.ReadCheckpointEvidence(transaction, step, EvidenceVerified)
		if err != nil {
			return bound, err
		}
		evidence, err := ParseEvidence(snapshot.Payload)
		if err != nil {
			return bound, err
		}
		verified[step] = evidence
	}
	path, err := registry.PendingPath(BaselineLedgerV2(registry))
	if err != nil {
		return bound, err
	}
	for _, migration := range path {
		ids := []string{}
		for id := range verified {
			if strings.HasPrefix(id, migration.ID+".") {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
		for _, id := range ids {
			evidence := verified[id]
			if len(bound.Steps) == 0 {
				bound.Releases = evidence.Releases
			}
			resource := strings.TrimPrefix(id, migration.ID+".")
			decision := DecisionCanonicalize
			if resource == "ledger" {
				resource, decision = "ledger."+migration.Domain, DecisionTransform
			}
			bound.Steps = append(bound.Steps, JournalStep{ID: id, Migration: migration.ID, Resource: resource, Decision: decision, Expected: evidence.Expected, Desired: evidence.Desired, Checkpoint: StepVerified, Evidence: &evidence})
			delete(verified, id)
		}
	}
	if len(bound.Steps) == 0 || len(verified) != 0 {
		return bound, errors.New("source repair requires supported terminal provenance for retained transaction artifacts")
	}
	return bound, nil
}

func (store *POSIXV2Store) sourceTransactionArtifacts(journal JournalRecord) error {
	parts := []string{"release-transition", "v2", "transactions", string(journal.Transaction)}
	entries, present, err := store.readDirectoryEntries(parts, 3)
	if err != nil {
		return err
	}
	if !present {
		if len(journal.Steps) == 0 {
			return nil
		}
		return errors.New("terminal transaction artifacts are missing")
	}
	if len(entries) > 3 {
		return errors.New("terminal transaction artifacts are unsupported")
	}
	for _, entry := range entries {
		if entry.Name() != "evidence" && entry.Name() != "recovery" && entry.Name() != "superseded-journal.json" {
			return errors.New("terminal transaction artifacts are unsupported")
		}
	}
	expected := map[string]bool{}
	recovery := map[string]bool{}
	for _, step := range journal.Steps {
		for _, checkpoint := range []EvidenceCheckpoint{EvidenceCaptured, EvidenceApplied, EvidenceVerified} {
			name, err := checkpointEvidenceName(journal.Transaction, step.ID, checkpoint)
			if err != nil {
				return err
			}
			expected[name] = true
		}
		if step.Evidence != nil && step.Evidence.Recovery != "" {
			recovery[step.ID+".json"] = true
		}
	}
	for directory, wanted := range map[string]map[string]bool{"evidence": expected, "recovery": recovery} {
		actual, _, err := store.readDirectoryEntries(append(slices.Clone(parts), directory), len(wanted))
		if err != nil {
			return err
		}
		if len(actual) != len(wanted) {
			return errors.New("terminal transaction artifacts differ from the journal")
		}
		for _, entry := range actual {
			if !wanted[entry.Name()] {
				return errors.New("terminal transaction artifacts are unbound")
			}
		}
	}
	return nil
}

func (store *POSIXV2Store) sourceTypedRecovery(journal JournalRecord, step JournalStep, migration MigrationDefinitionV2) error {
	snapshot, err := store.ReadRecovery(journal.Transaction, step.ID)
	if err != nil || !snapshot.Exists || snapshot.Fingerprint != step.Evidence.Recovery {
		return errors.New("terminal typed recovery does not match journal evidence")
	}
	var canonical []byte
	switch migration.Kind {
	case "test-vms-settings-v1-to-v2":
		var recovery settingsRecoveryV1
		if err := decodeBoundedRecord(snapshot.Payload, 4096, &recovery); err != nil {
			return err
		}
		if err := validateSettingsRecovery(recovery, journal, step); err != nil {
			return err
		}
		canonical, err = json.Marshal(recovery)
	case "test-yard-owner-v1-to-v2":
		var recovery ownerRegistrationRecoveryV1
		if err := decodeBoundedRecord(snapshot.Payload, 4096, &recovery); err != nil {
			return err
		}
		if err := validateOwnerRegistrationRecovery(recovery, journal, step); err != nil {
			return err
		}
		canonical, err = json.Marshal(recovery)
	default:
		return errors.New("terminal typed recovery grammar is unsupported")
	}
	if err != nil || !bytes.Equal(append(canonical, '\n'), snapshot.Payload) {
		return errors.New("terminal typed recovery is noncanonical")
	}
	return nil
}

func (store *POSIXV2Store) sourceRecoveryClosed() error {
	archive, err := store.readLifecycleArchive()
	if err != nil {
		return err
	}
	for _, parts := range [][]string{recoveryReceiptParts, recoveryLifecycleParts} {
		live, err := store.readRecoveryReceiptsAt(parts)
		if err != nil {
			return err
		}
		for transaction, receipt := range live {
			terminal, exists := archive[transaction]
			if !exists || !sameRecoveryReceipt(terminal.Receipt, receipt) {
				return errors.New("finish or cancel the live recovery reservation with its sealed owner before source repair")
			}
		}
	}
	frontiers, err := store.readRecoveryFrontiers()
	if err != nil {
		return err
	}
	for _, frontier := range frontiers {
		transaction, err := RecoveryLifecycleTransaction(frontier.Replacement, frontier.Generation)
		terminal, exists := archive[transaction]
		if !exists && frontier.Cancellation != nil {
			closed, found := archive[frontier.Cancellation.Receipt.Successor.Transaction]
			want, wantErr := MarshalRecoveryLifecycleRecord(*frontier.Cancellation)
			got, gotErr := MarshalRecoveryLifecycleRecord(closed)
			if found && wantErr == nil && gotErr == nil && bytes.Equal(want, got) {
				continue
			}
		}
		if err != nil || !exists || terminal.Receipt.Replacement != frontier.Replacement || terminal.Receipt.Generation != frontier.Generation || terminal.Completed == nil && terminal.PredecessorCompleted == nil {
			return errors.New("finish the unresolved recovery frontier with its sealed owner before source repair")
		}
	}
	return nil
}

type sourceMetadataInput struct {
	Present              bool
	Fingerprint          Fingerprint
	Device, Inode, Links uint64
	Mode, UID, GID       uint32
}

// This is a bounded inventory of the existing typed store, including absence,
// pending records, directory identities, and unrelated recovery namespaces.
func (store *POSIXV2Store) sourceMetadataInventory() (Fingerprint, error) {
	inputs := map[string]sourceMetadataInput{}
	remaining := 8192
	remainingBytes := int64(32 << 20)
	var visit func([]string) error
	visit = func(parts []string) error {
		key := strings.Join(parts, "/")
		fd, present, err := store.openParent(parts, false)
		if err != nil {
			return err
		}
		if !present {
			inputs[key] = sourceMetadataInput{}
			return nil
		}
		defer unix.Close(fd)
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil {
			return err
		}
		inputs[key] = metadataInput(stat, "")
		entries, _, err := store.readDirectoryEntries(parts, remaining)
		if err != nil {
			return err
		}
		remaining -= len(entries)
		if remaining < 0 {
			return errors.New("protected source admission inventory exceeds its bound")
		}
		for _, entry := range entries {
			name := entry.Name()
			child := append(slices.Clone(parts), name)
			if strings.HasSuffix(name, ".pending") {
				return errors.New("finish the pending protected publication with its sealed owner before source repair")
			}
			if entry.IsDir() {
				if !sourceMetadataDirectory(child) {
					return errors.New("source admission contains an unsupported protected namespace")
				}
				if err := visit(child); err != nil {
					return err
				}
				continue
			}
			if !sourceMetadataFile(child) {
				return errors.New("source admission contains an unsupported protected record")
			}
			if err := unix.Fstatat(fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				return err
			}
			remainingBytes -= stat.Size
			if stat.Size <= 0 || stat.Size > MaxProtectedRecordBytes || remainingBytes < 0 {
				return errors.New("protected source admission inventory exceeds its byte bound")
			}
			snapshot, err := store.readRecord(parts, name)
			if err != nil || !snapshot.Exists {
				return errors.New("protected source admission record is unavailable")
			}
			if len(child) == 4 && child[1] == "v2" && child[2] == "compatibility" {
				record, err := ParseCompatibilityEvidence(snapshot.Payload)
				canonical, marshalErr := MarshalCompatibilityEvidence(record)
				if err != nil || marshalErr != nil || !bytes.Equal(canonical, snapshot.Payload) || string(record.Identity)+".json" != name {
					return errors.New("source admission contains unbound compatibility evidence")
				}
			}
			if len(child) == 6 && child[1] == "v2" && child[4] == "evidence" {
				evidence, err := ParseEvidence(snapshot.Payload)
				canonical, marshalErr := MarshalEvidence(evidence)
				expected, nameErr := checkpointEvidenceName(evidence.Transaction, evidence.Step, evidence.Checkpoint)
				if err != nil || marshalErr != nil || nameErr != nil || !bytes.Equal(canonical, snapshot.Payload) || string(evidence.Transaction) != child[3] || expected != name {
					return errors.New("source admission contains unbound checkpoint evidence")
				}
			}
			if err := unix.Fstatat(fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				return err
			}
			inputs[strings.Join(child, "/")] = metadataInput(stat, snapshot.Fingerprint)
		}
		return nil
	}
	if err := visit([]string{"release-transition"}); err != nil {
		return "", err
	}
	// The roots bind absence even when their parent already exists.
	for _, parts := range [][]string{{"release-transition", "v2"}, recoveryReceiptParts, recoveryLifecycleParts, recoveryArchiveParts, recoveryFrontierParts} {
		if _, seen := inputs[strings.Join(parts, "/")]; !seen {
			if err := visit(parts); err != nil {
				return "", err
			}
		}
	}
	payload, err := json.Marshal(inputs)
	return fingerprintPayload(payload), err
}

func metadataInput(stat unix.Stat_t, fingerprint Fingerprint) sourceMetadataInput {
	return sourceMetadataInput{true, fingerprint, uint64(stat.Dev), stat.Ino, uint64(stat.Nlink), stat.Mode, stat.Uid, stat.Gid}
}

func sourceMetadataDirectory(parts []string) bool {
	path := strings.Join(parts, "/")
	for _, root := range []string{"release-transition/v2", "release-transition/v2/compatibility", "release-transition/v2/transactions", "release-transition/recovery", "release-transition/recovery/v1", "release-transition/recovery/v1/transactions", "release-transition/recovery/v2", "release-transition/recovery/v2/transactions", "release-transition/recovery/v2/archive", "release-transition/recovery/v2/frontiers"} {
		if path == root {
			return true
		}
	}
	if len(parts) >= 4 && len(parts) <= 5 && slices.Equal(parts[:3], []string{"release-transition", "v2", "transactions"}) && validateTransactionID(TransactionID(parts[3])) == nil {
		return len(parts) == 4 || parts[4] == "evidence" || parts[4] == "recovery"
	}
	return false
}

func sourceMetadataFile(parts []string) bool {
	if len(parts) == 3 && parts[1] == "v2" {
		return parts[2] == "journal.json" || parts[2] == "ledger.json" || parts[2] == migrationLedgerCheckpointName
	}
	if len(parts) == 5 && parts[1] == "v2" && parts[2] == "transactions" {
		return parts[4] == "superseded-journal.json"
	}
	if len(parts) == 6 && parts[1] == "v2" && parts[2] == "transactions" && parts[4] == "evidence" {
		return strings.HasSuffix(parts[5], ".json")
	}
	if len(parts) == 6 && parts[1] == "v2" && parts[2] == "transactions" && parts[4] == "recovery" {
		return strings.HasSuffix(parts[5], ".json") && validateSafeID(strings.TrimSuffix(parts[5], ".json"), "typed recovery step") == nil
	}
	if len(parts) == 4 && parts[1] == "v2" && parts[2] == "compatibility" {
		return strings.HasSuffix(parts[3], ".json") && validateFingerprint(Fingerprint(strings.TrimSuffix(parts[3], ".json")), "compatibility identity") == nil
	}
	if len(parts) == 5 && parts[1] == "recovery" && (parts[2] == "v1" && parts[3] == "transactions" || parts[2] == "v2" && (parts[3] == "transactions" || parts[3] == "archive" || parts[3] == "frontiers")) {
		_, err := recoveryEntryTransaction(filepath.Base(parts[4]))
		return err == nil
	}
	return false
}

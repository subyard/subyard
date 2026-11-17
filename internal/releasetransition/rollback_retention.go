package releasetransition

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

// Retirement records share the closure directory so one fsync makes intent
// durable. The prefix cannot collide with an ordinary transaction ID.
const (
	rollbackRetirementPrefix   = "-retire-"
	maxRollbackRetirementBytes = 1024
)

// Retirement pins the old closure and inventory, selected journal and verified
// successor. The original closure supplies the inventory during deletion.
type rollbackRetirement struct {
	SchemaVersion       int           `json:"schemaVersion"`
	Transaction         TransactionID `json:"transaction"`
	Original            Fingerprint   `json:"original"`
	Inventory           Fingerprint   `json:"inventory"`
	SelectedJournal     Fingerprint   `json:"selectedJournal"`
	SelectedTransaction TransactionID `json:"selectedTransaction"`
	Superseding         Fingerprint   `json:"superseding,omitempty"`
	Projection          Fingerprint   `json:"projection"`
	ProjectionProducer  Fingerprint   `json:"projectionProducer,omitempty"`
}

func parseRollbackRetirement(payload []byte, transaction TransactionID) (rollbackRetirement, error) {
	var retirement rollbackRetirement
	if err := decodeBoundedRecord(payload, maxRollbackRetirementBytes, &retirement); err != nil {
		return retirement, err
	}
	if retirement.SchemaVersion != 1 || retirement.Transaction != transaction || retirement.SelectedTransaction == transaction {
		return retirement, invalid("rollback retirement has inconsistent transaction bindings")
	}
	if err := validateTransactionID(retirement.SelectedTransaction); err != nil {
		return retirement, err
	}
	for _, digest := range []Fingerprint{retirement.Original, retirement.Inventory, retirement.SelectedJournal, retirement.Projection} {
		if err := validateFingerprint(digest, "rollback retirement binding"); err != nil {
			return retirement, err
		}
	}
	for _, digest := range []Fingerprint{retirement.Superseding, retirement.ProjectionProducer} {
		if digest != "" {
			if err := validateFingerprint(digest, "rollback retirement binding"); err != nil {
				return retirement, err
			}
		}
	}
	if retirement.ProjectionProducer == retirement.Projection {
		return retirement, invalid("rollback retirement would remove the pinned projection producer")
	}
	canonical, err := json.Marshal(retirement)
	if err != nil || !bytes.Equal(append(canonical, '\n'), payload) {
		return retirement, invalid("rollback retirement is not canonical")
	}
	return retirement, nil
}

func (store *POSIXV2Store) readRollbackRetirement(transaction TransactionID) (rollbackRetirement, ProtectedSnapshot, error) {
	snapshot, err := store.readRecord(rollbackClosureParts, rollbackRetirementPrefix+string(transaction)+".json")
	if err != nil || !snapshot.Exists {
		return rollbackRetirement{}, snapshot, err
	}
	retirement, err := parseRollbackRetirement(snapshot.Payload, transaction)
	return retirement, snapshot, err
}

func sameRollbackPair(left, right rollbackClosureV1) bool {
	return left.SourceArtifact == right.SourceArtifact && left.Completion.TargetArtifact == right.Completion.TargetArtifact &&
		bytes.Equal(left.SourceRegistry, right.SourceRegistry) && bytes.Equal(left.TargetRegistry, right.TargetRegistry) &&
		left.ActivationRequest.Releases.From == right.ActivationRequest.Releases.From && left.ActivationRequest.Releases.Target == right.ActivationRequest.Releases.Target &&
		left.ActivationRequest.RuntimeRoot == right.ActivationRequest.RuntimeRoot && left.ActivationRequest.ConfigHome == right.ActivationRequest.ConfigHome &&
		left.Completion.Projection == right.Completion.Projection && left.Completion.NativeHistory == right.Completion.NativeHistory
}

func (store *POSIXV2Store) remainingRetiringRollbackArtifacts(closure rollbackClosureV1) ([]rollbackArtifact, error) {
	actual, err := store.rollbackArtifactInventory(closure.Transaction)
	if err != nil {
		return nil, err
	}
	for _, remaining := range actual {
		if !slices.Contains(closure.Artifacts, remaining) {
			return nil, invalid("retiring rollback artifacts contain changed or unbound bytes")
		}
	}
	return actual, nil
}

func (store *POSIXV2Store) prepareRollbackRetirement(transaction TransactionID, selected ProtectedSnapshot, state MigrationLedgerSnapshot, endpoints ReleaseLinks, successor *rollbackClosureV1) error {
	closure, err := store.readRollbackClosure(transaction)
	if err != nil || closure == nil || closure.Retirement != nil {
		return err
	}
	name := string(transaction) + ".json"
	expected, err := store.readRecord(rollbackClosureParts, name)
	if err != nil {
		return err
	}
	current, err := store.ReadCurrentJournal()
	if err != nil {
		return err
	}
	if !sameProtectedSnapshot(current, selected) {
		return ErrProtectedStoreStale
	}
	journal, err := ParseJournal(selected.Payload)
	if err != nil {
		return err
	}
	canonical, err := MarshalJournal(journal)
	if err != nil || !bytes.Equal(canonical, selected.Payload) || journal.Checkpoint != JournalComplete || journal.Transaction == transaction {
		return invalid("rollback retirement requires a distinct completed selected journal")
	}
	if !activatedReleaseLinks(endpoints, journal.Releases) && !initialReleaseLinks(endpoints, journal.Releases) {
		return invalid("rollback retirement endpoints differ from the selected journal")
	}
	inventory, err := json.Marshal(closure.Artifacts)
	if err != nil {
		return err
	}
	retirement := rollbackRetirement{
		SchemaVersion: 1, Transaction: transaction, Original: expected.Fingerprint,
		Inventory:       fingerprintPayload(inventory),
		SelectedJournal: selected.Fingerprint, SelectedTransaction: journal.Transaction,
		Projection: state.ProjectionSnapshot.Fingerprint, ProjectionProducer: closure.ProjectionProducer,
	}
	if successor != nil && sameRollbackPair(*closure, *successor) && closure.Completion.Transaction != successor.Completion.Transaction {
		if !bytes.Equal(successor.RollbackJournal, selected.Payload) {
			return invalid("rollback retirement successor differs from the selected journal")
		}
		payload, err := marshalRollbackClosure(*successor)
		if err != nil {
			return err
		}
		retirement.Superseding = fingerprintPayload(payload)
	} else if closure.Releases.Target == endpoints.Active || endpoints.Previous != nil && closure.Releases.Target == *endpoints.Previous {
		return invalid("rollback retirement has no supersession proof for a retained endpoint")
	}
	payload, err := json.Marshal(retirement)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	if _, err := parseRollbackRetirement(payload, transaction); err != nil {
		return err
	}
	if err := store.createImmutable(rollbackClosureParts, rollbackRetirementPrefix+name, payload); err != nil {
		return err
	}
	return store.inject("after-rollback-retirement-publication")
}

// Called only for a whole component selected by the existing protected graph.
// Exact conditional removals preserve any byte changed after the inventory read.
func (store *POSIXV2Store) removeRetiringRollbackArtifacts(transaction TransactionID) (bool, error) {
	closure, err := store.readRollbackClosure(transaction)
	if err != nil {
		return true, err
	}
	retirement, receipt, err := store.readRollbackRetirement(transaction)
	if err != nil {
		return true, err
	}
	if !receipt.Exists {
		if closure != nil {
			return true, invalid("rollback artifacts have no durable retirement evidence")
		}
		return false, nil
	}
	var actual []rollbackArtifact
	if closure != nil {
		actual, err = store.remainingRetiringRollbackArtifacts(*closure)
		if err != nil {
			return true, err
		}
	}
	// A previous attempt may have stopped between the rename and its fsync.
	// Make the deletion intent durable before touching any covered artifact.
	parent, present, err := store.openParent(rollbackClosureParts, false)
	if err != nil {
		return true, err
	}
	if !present {
		return true, ErrProtectedStoreStale
	}
	defer unix.Close(parent)
	if err := store.inject("before-rollback-retirement-dir-fsync"); err != nil {
		return true, err
	}
	if err := unix.Fsync(parent); err != nil {
		return true, err
	}
	// Descendants precede their directory marker in reverse lexical order.
	slices.Reverse(actual)
	for _, artifact := range actual {
		parts, name, directory, err := rollbackArtifactLocation(transaction, artifact.Path)
		if err != nil {
			return true, err
		}
		if directory {
			parent, present, err := store.openParent(parts, false)
			if err != nil {
				return true, err
			}
			if present {
				err = unix.Unlinkat(parent, name, unix.AT_REMOVEDIR)
				if err == nil {
					err = unix.Fsync(parent)
				}
				_ = unix.Close(parent)
				if err != nil {
					return true, err
				}
			}
		} else {
			snapshot, err := store.readRecord(parts, name)
			if err != nil {
				return true, err
			}
			if snapshot.Fingerprint != artifact.Fingerprint {
				return true, ErrProtectedStoreStale
			}
			if err := store.removeProtectedRecord(parts, name, snapshot.Payload); err != nil {
				return true, err
			}
		}
		if err := store.inject("after-rollback-artifact-retirement"); err != nil {
			return true, err
		}
	}
	if closure != nil {
		name := string(transaction) + ".json"
		original, err := store.readRecord(rollbackClosureParts, name)
		if err != nil {
			return true, err
		}
		if original.Fingerprint != retirement.Original {
			return true, ErrProtectedStoreStale
		}
		if err := store.removeProtectedRecord(rollbackClosureParts, name, original.Payload); err != nil {
			return true, err
		}
		if err := store.inject("after-rollback-closure-retirement"); err != nil {
			return true, err
		}
	}
	return true, store.removeProtectedRecord(rollbackClosureParts, rollbackRetirementPrefix+string(transaction)+".json", receipt.Payload)
}

func rollbackArtifactLocation(transaction TransactionID, path string) ([]string, string, bool, error) {
	directory := strings.HasSuffix(path, "/")
	components := strings.Split(strings.TrimSuffix(path, "/"), "/")
	var parts []string
	switch components[0] {
	case "transaction":
		parts = []string{"release-transition", "v2", "transactions", string(transaction)}
	case "native-identities":
		parts = nativeIdentityParts(transaction)
	default:
		return nil, "", false, invalid("rollback artifact path has an unknown root")
	}
	for _, component := range components[1:] {
		if err := validateSafeID(component, "rollback artifact path component"); err != nil {
			return nil, "", false, err
		}
		parts = append(parts, component)
	}
	return parts[:len(parts)-1], parts[len(parts)-1], directory, nil
}

// The transaction directory may disappear before its native sibling or receipt.
// Finish those bounded orphan retirements without inventing a transaction root.
func (store *POSIXV2Store) finishRollbackRetirements(current TransactionID, retain func(*v2TransactionGraph) error) error {
	snapshot, err := store.ReadCurrentJournal()
	if err != nil {
		return err
	}
	journal, err := ParseJournal(snapshot.Payload)
	if err != nil {
		return err
	}
	if journal.Transaction != current || journal.Checkpoint != JournalComplete {
		return ErrProtectedStoreStale
	}
	graph, err := store.inspectTransactionGraph(current, &journal)
	if err != nil {
		return err
	}
	if graph == nil {
		graph = &v2TransactionGraph{directories: map[TransactionID]struct{}{}, protected: map[string]struct{}{string(current): {}}}
		owners, err := store.migrationOwnerReceipts()
		if err != nil {
			return err
		}
		for _, transaction := range migrationOwnerProtectedTransactions(owners) {
			graph.protected[string(transaction)] = struct{}{}
		}
		pending, err := store.readRecord([]string{"release-transition", "v2"}, ".journal.json.pending")
		if err != nil {
			return err
		}
		if pending.Exists {
			prepared, err := ParseJournal(pending.Payload)
			if err != nil {
				return err
			}
			graph.protected[string(prepared.Transaction)] = struct{}{}
		}
	}
	if err := retain(graph); err != nil {
		return err
	}
	entries, _, err := store.readDirectoryEntries(rollbackClosureParts, 2*maxTransactionGraphEntries)
	if err != nil {
		return err
	}
	if len(entries) > 2*maxTransactionGraphEntries {
		return invalid("rollback closure namespace exceeds its entry bound")
	}
	removed := 0
	for _, entry := range entries {
		name, retiring := strings.CutPrefix(entry.Name(), rollbackRetirementPrefix)
		if !retiring {
			continue
		}
		transaction := TransactionID(strings.TrimSuffix(name, ".json"))
		if err := validateTransactionID(transaction); err != nil || !strings.HasSuffix(name, ".json") {
			return invalid("rollback retirement namespace contains an unsupported record")
		}
		if _, present := graph.directories[transaction]; present {
			continue
		}
		if _, protected := graph.protected[string(transaction)]; protected {
			continue
		}
		if _, err := store.removeRetiringRollbackArtifacts(transaction); err != nil {
			return err
		}
		removed++
		if removed == 32 {
			break
		}
	}
	return nil
}

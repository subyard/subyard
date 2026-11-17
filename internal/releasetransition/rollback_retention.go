package releasetransition

import (
	"bytes"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

// Retirement is durable deletion intent, never history or execution authority.
// The original inventory stays intact until all its bytes have been retired.
type rollbackRetirementV1 struct {
	Original        Fingerprint  `json:"original"`
	SelectedJournal []byte       `json:"selectedJournal"`
	Endpoints       ReleaseLinks `json:"endpoints"`
	Projection      Fingerprint  `json:"projection"`
	Superseding     []byte       `json:"superseding,omitempty"`
}

func (retirement rollbackRetirementV1) validate(closure rollbackClosureV1) error {
	original := closure
	original.Retirement = nil
	payload, err := marshalRollbackClosure(original)
	if err != nil || fingerprintPayload(payload) != retirement.Original {
		return invalid("rollback retirement does not bind the original closure")
	}
	selected, err := ParseJournal(retirement.SelectedJournal)
	if err != nil {
		return err
	}
	canonical, err := MarshalJournal(selected)
	if err != nil || !bytes.Equal(canonical, retirement.SelectedJournal) || selected.Checkpoint != JournalComplete || selected.Transaction == closure.Transaction {
		return invalid("rollback retirement has no distinct completed current journal")
	}
	if err := validateFingerprint(retirement.Projection, "rollback retirement projection"); err != nil {
		return err
	}
	if !activatedReleaseLinks(retirement.Endpoints, selected.Releases) && !initialReleaseLinks(retirement.Endpoints, selected.Releases) {
		return invalid("rollback retirement endpoints differ from the selected journal")
	}
	if len(retirement.Superseding) != 0 {
		var successor rollbackClosureV1
		if err := decodeBoundedRecord(retirement.Superseding, MaxProtectedRecordBytes, &successor); err != nil {
			return err
		}
		// Forbid nested certificates before validation, bounding recursion and
		// preventing a chain of receipts from becoming a permanent retention root.
		if successor.Retirement != nil {
			return invalid("rollback retirement contains nested retirement evidence")
		}
		if _, err := parseRollbackClosure(retirement.Superseding); err != nil {
			return err
		}
		if !bytes.Equal(successor.RollbackJournal, retirement.SelectedJournal) || !sameRollbackPair(original, successor) || original.Completion.Transaction == successor.Completion.Transaction {
			return invalid("rollback retirement does not bind a newer completion of the same sealed pair")
		}
	} else if closure.Releases.Target == retirement.Endpoints.Active || retirement.Endpoints.Previous != nil && closure.Releases.Target == *retirement.Endpoints.Previous {
		return invalid("rollback retirement has no supersession proof for a retained endpoint")
	}
	if closure.ProjectionProducer != "" && closure.ProjectionProducer == retirement.Projection {
		return invalid("rollback retirement would remove the pinned projection producer")
	}
	return nil
}

func sameRollbackPair(left, right rollbackClosureV1) bool {
	return left.SourceArtifact == right.SourceArtifact && left.Completion.TargetArtifact == right.Completion.TargetArtifact &&
		bytes.Equal(left.SourceRegistry, right.SourceRegistry) && bytes.Equal(left.TargetRegistry, right.TargetRegistry) &&
		left.ActivationRequest.Releases.From == right.ActivationRequest.Releases.From && left.ActivationRequest.Releases.Target == right.ActivationRequest.Releases.Target &&
		left.ActivationRequest.RuntimeRoot == right.ActivationRequest.RuntimeRoot && left.ActivationRequest.ConfigHome == right.ActivationRequest.ConfigHome &&
		left.Completion.Projection == right.Completion.Projection && left.Completion.NativeHistory == right.Completion.NativeHistory
}

func (store *POSIXV2Store) checkClosedRollbackArtifacts(closure rollbackClosureV1) error {
	if closure.Retirement == nil {
		return store.checkRollbackArtifactInventory(closure.Transaction, closure.Artifacts)
	}
	actual, err := store.rollbackArtifactInventory(closure.Transaction)
	if err != nil {
		return err
	}
	for _, remaining := range actual {
		if !slices.Contains(closure.Artifacts, remaining) {
			return invalid("retiring rollback artifacts contain changed or unbound bytes")
		}
	}
	return nil
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
	retirement := &rollbackRetirementV1{Original: expected.Fingerprint, SelectedJournal: selected.Payload, Endpoints: endpoints, Projection: state.ProjectionSnapshot.Fingerprint}
	if successor != nil && sameRollbackPair(*closure, *successor) && closure.Completion.Transaction != successor.Completion.Transaction {
		retirement.Superseding, err = marshalRollbackClosure(*successor)
		if err != nil {
			return err
		}
	}
	closure.Retirement = retirement
	payload, err := marshalRollbackClosure(*closure)
	if err != nil {
		return err
	}
	if err := store.compareAndSwap(rollbackClosureParts, name, expected, payload); err != nil {
		return err
	}
	return store.inject("after-rollback-retirement-publication")
}

// Called only for a whole component selected by the existing protected graph.
// Exact conditional removals preserve any byte changed after the inventory read.
func (store *POSIXV2Store) removeRetiringRollbackArtifacts(transaction TransactionID) (bool, error) {
	closure, err := store.readRollbackClosure(transaction)
	if err != nil || closure == nil {
		return false, err
	}
	if closure.Retirement == nil {
		return true, invalid("rollback artifacts have no durable retirement evidence")
	}
	actual, err := store.rollbackArtifactInventory(transaction)
	if err != nil {
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
	payload, err := marshalRollbackClosure(*closure)
	if err != nil {
		return true, err
	}
	return true, store.removeProtectedRecord(rollbackClosureParts, string(transaction)+".json", payload)
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
	entries, _, err := store.readDirectoryEntries(rollbackClosureParts, maxTransactionGraphEntries)
	if err != nil {
		return err
	}
	if len(entries) > maxTransactionGraphEntries {
		return invalid("rollback closure namespace exceeds its entry bound")
	}
	removed := 0
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			return invalid("rollback retirement namespace contains an unsupported record")
		}
		transaction := TransactionID(strings.TrimSuffix(entry.Name(), ".json"))
		if _, present := graph.directories[transaction]; present {
			continue
		}
		if _, protected := graph.protected[string(transaction)]; protected {
			continue
		}
		closure, err := store.readRollbackClosure(transaction)
		if err != nil {
			return err
		}
		if closure == nil || closure.Retirement == nil {
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

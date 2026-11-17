package releasetransition

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"slices"
)

// rollbackClosureV1 is per-transaction evidence of closed artifact bytes. It
// cannot authorize execution, select history, or establish current readiness.
// Each covered transaction has its own record so ordinary graph retention can
// collect unrelated components independently.
type rollbackClosureV1 struct {
	SchemaVersion      int                      `json:"schemaVersion"`
	Transaction        TransactionID            `json:"transaction"`
	Releases           ReleasePair              `json:"releases"`
	SourceArtifact     Fingerprint              `json:"sourceArtifact"`
	SourceRegistry     []byte                   `json:"sourceRegistry"`
	TargetRegistry     []byte                   `json:"targetRegistry"`
	RollbackJournal    []byte                   `json:"rollbackJournal"`
	ActivationRequest  RollbackTargetRequest    `json:"activationRequest"`
	ActivationPlan     RollbackActivationPlan   `json:"activationPlan"`
	Completion         RollbackTargetCompletion `json:"completion"`
	ProjectionProducer Fingerprint              `json:"projectionProducer,omitempty"`
	Artifacts          []rollbackArtifact       `json:"artifacts"`
	Retirement         *rollbackRetirement      `json:"-"`
}

var rollbackClosureParts = []string{"reconcile", "rollback", "v1", "closures"}

func parseRollbackClosure(payload []byte) (rollbackClosureV1, error) {
	var closure rollbackClosureV1
	if err := decodeBoundedRecord(payload, MaxProtectedRecordBytes, &closure); err != nil {
		return closure, err
	}
	if err := closure.validate(); err != nil {
		return closure, err
	}
	canonical, err := marshalRollbackClosure(closure)
	if err != nil || !bytes.Equal(canonical, payload) {
		return closure, invalid("rollback closure is not canonical")
	}
	return closure, nil
}

func marshalRollbackClosure(closure rollbackClosureV1) ([]byte, error) {
	if closure.Retirement != nil {
		return nil, invalid("rollback retirement is a separate record format")
	}
	if err := closure.validate(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(closure)
	if err != nil {
		return nil, err
	}
	if len(payload)+1 > MaxProtectedRecordBytes {
		return nil, invalid("rollback closure exceeds its byte bound")
	}
	return append(payload, '\n'), nil
}

func (closure rollbackClosureV1) validate() error {
	if closure.SchemaVersion != 1 {
		return invalid("unsupported rollback closure schema")
	}
	if err := validateTransactionID(closure.Transaction); err != nil {
		return err
	}
	if err := closure.Releases.Validate(); err != nil {
		return err
	}
	if err := validateFingerprint(closure.SourceArtifact, "rollback closure source artifact"); err != nil {
		return err
	}
	source, sourceDigest, err := ParseRegistryV2Provenance(closure.SourceRegistry)
	if err != nil {
		return err
	}
	target, targetDigest, err := ParseRegistryV2Provenance(closure.TargetRegistry)
	if err != nil {
		return err
	}
	if err := CheckRollbackTargetPrefixes(source, target); err != nil {
		return err
	}
	journal, err := ParseJournal(closure.RollbackJournal)
	if err != nil {
		return err
	}
	canonical, err := MarshalJournal(journal)
	if err != nil || !bytes.Equal(canonical, closure.RollbackJournal) {
		return invalid("rollback closure journal is not canonical")
	}
	response := RollbackTargetResponse{SchemaVersion: RollbackTargetProcessSchemaV1, Completion: &closure.Completion}
	if err := response.Validate(); err != nil {
		return err
	}
	completion := closure.Completion
	if journal.Checkpoint != JournalComplete || journal.Goal.Direction != DirectionActivatePrevious || journal.SourceIngress != nil || IsRecoveryTransaction(journal.Transaction) || journal.RegistryDigest != sourceDigest || journal.ArtifactDigest != completion.TargetArtifact || journal.Transaction != completion.Transaction || journal.IntentDigest != completion.Intent || targetDigest != completion.TargetRegistry {
		return invalid("rollback closure does not bind completed source execution to target completion")
	}
	request := closure.ActivationRequest
	if err := request.Validate(); err != nil {
		return err
	}
	if request.Mode != "activate-under-v2" || request.Execution != nil || request.Plan != nil || request.Journal != "" || !releasePairsEqual(request.Releases, journal.Releases) || request.SourceArtifact != closure.SourceArtifact || request.SourceRegistry != sourceDigest || request.TargetRegistry != targetDigest || request.TargetArtifact != completion.TargetArtifact || request.Projection != completion.Projection {
		return invalid("rollback closure activation inputs differ from their sealed bindings")
	}
	if err := rollbackRequestSourceOwner(request).CheckJournal(journal); err != nil {
		return err
	}
	if err := closure.ActivationPlan.validate(); err != nil {
		return err
	}
	_, history, err := MarshalLedgerV2(*request.NativeHistory, target)
	if err != nil {
		return err
	}
	pending, err := target.PendingPath(*request.NativeHistory)
	if err != nil || len(pending) != 0 || history != completion.NativeHistory {
		return invalid("rollback closure does not contain its complete native target result")
	}
	owner := &V2Transition{registry: target}
	token, err := owner.rollbackActivationPlan(request, closure.ActivationPlan.Assessment, closure.ActivationPlan.Scopes)
	if err != nil || token != completion.Plan || token != closure.ActivationPlan.Token {
		return invalid("rollback closure activation assessment binding changed")
	}
	if closure.Releases.Target != journal.Releases.From && closure.Releases.Target != journal.Releases.Target {
		return invalid("rollback closure artifact owner is outside its verified endpoints")
	}
	if closure.Transaction == journal.Transaction && !releasePairsEqual(closure.Releases, journal.Releases) {
		return invalid("rollback closure endpoint bindings differ from its journal")
	}
	bound := false
	for _, step := range journal.Steps {
		if step.ID == RollbackTargetActivationStep && step.Migration == RollbackTargetActivationStep && step.Resource == "activation.rollback-target-plan" && step.Decision == DecisionPreserve && step.Expected == step.Desired && step.Desired == fingerprintPayload([]byte(completion.Plan)) {
			bound = true
		}
	}
	if !bound {
		return invalid("rollback closure has no assessed target activation binding")
	}
	if closure.ProjectionProducer != "" {
		if closure.ProjectionProducer != completion.Projection {
			return invalid("rollback closure projection producer differs from its frozen projection")
		}
	}
	if len(closure.Artifacts) == 0 || len(closure.Artifacts) > maxRollbackArtifactEntries {
		return invalid("rollback closure artifact inventory is empty or exceeds its bound")
	}
	last := ""
	for _, artifact := range closure.Artifacts {
		if artifact.Path <= last {
			return invalid("rollback closure artifact inventory is not unique and sorted")
		}
		if err := validateFingerprint(artifact.Fingerprint, "closed rollback artifact"); err != nil {
			return err
		}
		last = artifact.Path
	}
	return nil
}

// CheckRollbackTargetPrefixes compares the complete native target frontier to
// the structurally verified source registry, including retained dependencies.
func CheckRollbackTargetPrefixes(source, target RegistryV2) error {
	var refusals []error
	for _, domain := range slices.Sorted(maps.Keys(target.CurrentEpochs)) {
		epoch := target.CurrentEpochs[domain]
		left, leftErr := source.MigrationPrefixDigest(domain, epoch)
		right, rightErr := target.MigrationPrefixDigest(domain, epoch)
		if leftErr != nil || rightErr != nil || left != right {
			refusals = append(refusals, invalid("rollback target has a different migration prefix for domain %q", domain))
		}
	}
	return errors.Join(refusals...)
}

func (store *POSIXV2Store) readRollbackClosure(transaction TransactionID) (*rollbackClosureV1, error) {
	if err := validateTransactionID(transaction); err != nil {
		return nil, err
	}
	snapshot, err := store.readRecord(rollbackClosureParts, string(transaction)+".json")
	if err != nil {
		return nil, err
	}
	retirement, receipt, err := store.readRollbackRetirement(transaction)
	if err != nil {
		return nil, err
	}
	if !snapshot.Exists {
		if receipt.Exists {
			remaining, err := store.rollbackArtifactInventory(transaction)
			if err != nil {
				return nil, err
			}
			if len(remaining) != 0 {
				return nil, invalid("rollback retirement lost the closure for remaining artifacts")
			}
		}
		return nil, nil
	}
	closure, err := parseRollbackClosure(snapshot.Payload)
	if err != nil {
		return nil, err
	}
	if closure.Transaction != transaction {
		return nil, invalid("rollback closure is filed under a different transaction")
	}
	if receipt.Exists {
		inventory, err := json.Marshal(closure.Artifacts)
		if err != nil || retirement.Original != snapshot.Fingerprint || retirement.Inventory != fingerprintPayload(inventory) || retirement.ProjectionProducer != closure.ProjectionProducer {
			return nil, invalid("rollback retirement does not bind the original closure and inventory")
		}
		closure.Retirement = &retirement
		if _, err := store.remainingRetiringRollbackArtifacts(closure); err != nil {
			return nil, err
		}
		return &closure, nil
	}
	if err := store.checkRollbackArtifactInventory(transaction, closure.Artifacts); err != nil {
		return nil, err
	}
	journal, err := ParseJournal(closure.RollbackJournal)
	if err != nil {
		return nil, err
	}
	if transaction == journal.Transaction {
		if err := store.sourceTransactionArtifacts(journal); err != nil {
			return nil, err
		}
		for _, step := range journal.Steps {
			if err := store.checkRewindStepEvidence(journal, step); err != nil {
				return nil, err
			}
			if step.Evidence.Recovery != "" {
				value, err := store.ReadRecovery(transaction, step.ID)
				if err != nil {
					return nil, err
				}
				if !value.Exists || value.Fingerprint != step.Evidence.Recovery {
					return nil, invalid("closed rollback recovery bytes differ from their evidence")
				}
			}
		}
	}
	return &closure, nil
}

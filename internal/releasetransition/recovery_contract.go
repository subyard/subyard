package releasetransition

import (
	"encoding/json"
	"slices"
	"strings"
)

const (
	ActivationOnlyRecoveryContractV1 = "activation-only-replacement-v1"
	ActivationOnlyRecoveryContractV2 = "activation-only-replacement-v2"
	RecoveryReceiptSchemaV1          = 1
	RecoveryReceiptSchemaV2          = 2
	RecoveryLifecycleSchemaV1        = 1
	RecoveryTransactionPrefixV1      = "recovery-v1-"
	RecoveryTransactionPrefixV2      = "recovery-v2-"
)

// ActivationOnlyRecoveryRequest selects an exact unfinished journal. It is
// separate from the frozen V1 replacement reasons and never grants authority.
type ActivationOnlyRecoveryRequest struct {
	Transaction TransactionID `json:"transaction"`
	Fingerprint Fingerprint   `json:"fingerprint"`
}

func (request ActivationOnlyRecoveryRequest) Validate() error {
	if err := validateTransactionID(request.Transaction); err != nil {
		return err
	}
	return validateFingerprint(request.Fingerprint, "recovery predecessor fingerprint")
}

// RecoveryOwner identifies the sealed runtime whose ordinary V2 semantics the
// successor keeps. Capability support is verified at the process boundary;
// this receipt is evidence of that selection, not a substitute for verification.
type RecoveryOwner struct {
	Release  ReleaseID   `json:"release"`
	Artifact Fingerprint `json:"artifactDigest"`
	Registry Fingerprint `json:"registryDigest"`
	Catalog  Fingerprint `json:"catalogDigest"`
}

// RecoveryNativePlan records bounded bindings obtained from owner-native
// planning. A valid fingerprint alone never proves safe ownership or apply.
type RecoveryNativePlan struct {
	ID        string      `json:"id"`
	Binding   Fingerprint `json:"binding"`
	Actual    Fingerprint `json:"actual"`
	Desired   Fingerprint `json:"desired"`
	Converged bool        `json:"converged"`
}

// RecoveryReceiptV1 preserves canonical predecessor evidence and the exact
// initial successor. Neither frozen journal V2 nor its archive envelope changes.
type RecoveryReceiptV1 struct {
	SchemaVersion int                           `json:"schemaVersion"`
	Contract      string                        `json:"contract"`
	Replacement   ActivationOnlyRecoveryRequest `json:"replacement"`
	Predecessor   JournalRecord                 `json:"predecessor"`
	Successor     JournalRecord                 `json:"successor"`
	Ledger        Fingerprint                   `json:"ledgerFingerprint"`
	Links         ReleaseLinks                  `json:"links"`
	Owner         RecoveryOwner                 `json:"owner"`
	NativePlans   []RecoveryNativePlan          `json:"nativePlans"`
	BasePlan      PlanToken                     `json:"basePlan,omitempty"`
	Reservation   Fingerprint                   `json:"reservation,omitempty"`
}

func (receipt RecoveryReceiptV1) Validate() error {
	prefix := RecoveryTransactionPrefixV1
	if receipt.Contract == ActivationOnlyRecoveryContractV2 && receipt.SchemaVersion == RecoveryReceiptSchemaV2 {
		prefix = RecoveryTransactionPrefixV2
		bound, err := BindRecoveryLifecyclePlan(receipt.BasePlan, receipt.Reservation)
		if err != nil || bound != receipt.Successor.AuthorizationPlan {
			return invalid("recovery receipt does not bind its assessed reservation")
		}
	} else if receipt.SchemaVersion != RecoveryReceiptSchemaV1 || receipt.Contract != ActivationOnlyRecoveryContractV1 || receipt.BasePlan != "" || receipt.Reservation != "" {
		return invalid("unsupported activation recovery receipt contract")
	}
	if err := receipt.Replacement.Validate(); err != nil {
		return err
	}
	before, after := receipt.Predecessor, receipt.Successor
	if err := before.Validate(); err != nil {
		return err
	}
	if err := after.Validate(); err != nil {
		return err
	}
	if before.Checkpoint != JournalReconciling || before.Goal.Direction != DirectionActivateTarget ||
		before.SourceIngress != nil || len(before.Steps) != 0 || receipt.Replacement.Transaction != before.Transaction {
		return invalid("recovery predecessor is not an activation-only reconciling journal")
	}
	payload, err := MarshalJournal(before)
	if err != nil {
		return err
	}
	if fingerprintPayload(payload) != receipt.Replacement.Fingerprint {
		return invalid("recovery predecessor fingerprint does not match canonical journal")
	}
	if err := receipt.Links.Validate(); err != nil {
		return err
	}
	if !recoveryActiveLinksMatch(receipt.Links, before.Releases) {
		return invalid("recovery links do not match the predecessor active target")
	}
	if after.Checkpoint != JournalAuthorized || after.SourceIngress != nil || len(after.Steps) != 0 ||
		after.Goal != before.Goal || after.Releases.From != receipt.Links.Active ||
		after.Releases.Target != receipt.Links.Active || !releaseIDsEqual(after.Releases.Previous, receipt.Links.Previous) ||
		after.Transaction == before.Transaction || !strings.HasPrefix(string(after.Transaction), prefix) ||
		IsRecoveryTransaction(before.Transaction) || after.AuthorizationPlan == before.AuthorizationPlan ||
		after.ResumePlan == before.ResumePlan || after.AuthorizationDigest == before.AuthorizationDigest ||
		after.ObservationScope == before.ObservationScope {
		return invalid("recovery successor does not bind a fresh same-target activation-only grant")
	}
	if before.ArtifactDigest != after.ArtifactDigest || before.RegistryDigest != after.RegistryDigest ||
		before.CatalogDigest != after.CatalogDigest || receipt.Owner.Release != after.Goal.Target ||
		receipt.Owner.Artifact != after.ArtifactDigest || receipt.Owner.Registry != after.RegistryDigest ||
		receipt.Owner.Catalog != after.CatalogDigest {
		return invalid("recovery owner does not match the unchanged sealed runtime")
	}
	if err := validateFingerprint(receipt.Ledger, "recovery ledger fingerprint"); err != nil {
		return err
	}
	if len(receipt.NativePlans) == 0 || len(receipt.NativePlans) > MaxPlanItems {
		return invalid("recovery native plan count is invalid")
	}
	for index, plan := range receipt.NativePlans {
		if err := validateSafeID(plan.ID, "recovery native plan ID"); err != nil {
			return err
		}
		if index != 0 && receipt.NativePlans[index-1].ID >= plan.ID {
			return invalid("recovery native plans are not uniquely ordered")
		}
		for _, fingerprint := range []Fingerprint{plan.Binding, plan.Actual, plan.Desired} {
			if err := validateFingerprint(fingerprint, "recovery native plan fingerprint"); err != nil {
				return err
			}
		}
		if plan.Converged && plan.Actual != plan.Desired {
			return invalid("converged recovery native plan has differing fingerprints")
		}
	}
	return nil
}

func IsRecoveryTransaction(transaction TransactionID) bool {
	return strings.HasPrefix(string(transaction), RecoveryTransactionPrefixV1) || strings.HasPrefix(string(transaction), RecoveryTransactionPrefixV2)
}

// BindRecoveryLifecyclePlan keeps the grant stable across its own receipt
// publication while binding any exact unselected reservation it may cancel.
func BindRecoveryLifecyclePlan(base PlanToken, reservation Fingerprint) (PlanToken, error) {
	if err := validatePlanToken(base); err != nil {
		return "", err
	}
	if reservation == "" {
		return base, nil
	}
	if err := validateFingerprint(reservation, "recovery reservation fingerprint"); err != nil {
		return "", err
	}
	payload, err := json.Marshal(struct {
		Contract    string      `json:"contract"`
		Base        PlanToken   `json:"base"`
		Reservation Fingerprint `json:"reservation"`
	}{ActivationOnlyRecoveryContractV2, base, reservation})
	if err != nil {
		return "", err
	}
	return PlanToken("plan-v1-" + string(fingerprintPayload(payload))), nil
}

type RecoveryCancellation struct {
	BasePlan            PlanToken   `json:"basePlan"`
	Plan                PlanToken   `json:"plan"`
	AuthorizationDigest Fingerprint `json:"authorizationDigest"`
}

// Terminal evidence is separate from the live receipt. It never authorizes
// publication, and only a completed witness or proven pre-CAS cancellation
// allows the live record to retire.
type RecoveryLifecycleRecord struct {
	SchemaVersion int                   `json:"schemaVersion"`
	Receipt       RecoveryReceiptV1     `json:"receipt"`
	Completed     *JournalRecord        `json:"completed,omitempty"`
	Cancellation  *RecoveryCancellation `json:"cancellation,omitempty"`
}

func (record RecoveryLifecycleRecord) Validate() error {
	if record.SchemaVersion != RecoveryLifecycleSchemaV1 || (record.Completed == nil) == (record.Cancellation == nil) {
		return invalid("invalid recovery lifecycle record")
	}
	if err := record.Receipt.Validate(); err != nil {
		return err
	}
	if record.Completed != nil {
		if record.Completed.Checkpoint != JournalComplete {
			return invalid("recovery completion witness is not complete")
		}
		return record.Receipt.MatchesSuccessor(*record.Completed)
	}
	cancellation := record.Cancellation
	payload, err := MarshalRecoveryReceipt(record.Receipt)
	if err != nil {
		return err
	}
	plan, err := BindRecoveryLifecyclePlan(cancellation.BasePlan, fingerprintPayload(payload))
	if err != nil || record.Receipt.Contract != ActivationOnlyRecoveryContractV2 || cancellation.BasePlan == record.Receipt.BasePlan || plan != cancellation.Plan {
		return invalid("recovery cancellation does not bind a new assessed plan")
	}
	return validateFingerprint(cancellation.AuthorizationDigest, "recovery cancellation authorization")
}

func ParseRecoveryLifecycleRecord(payload []byte) (RecoveryLifecycleRecord, error) {
	var record RecoveryLifecycleRecord
	if err := decodeBoundedRecord(payload, MaxProtectedRecordBytes, &record); err != nil {
		return record, err
	}
	return record, record.Validate()
}

func MarshalRecoveryLifecycleRecord(record RecoveryLifecycleRecord) ([]byte, error) {
	if err := record.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	payload = append(payload, '\n')
	if err := validateProtectedPayload(payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// MatchesSuccessor validates immutable receipt bindings against any ordinary
// later checkpoint of the successor journal.
func (receipt RecoveryReceiptV1) MatchesSuccessor(current JournalRecord) error {
	if err := receipt.Validate(); err != nil {
		return err
	}
	if err := current.Validate(); err != nil {
		return err
	}
	current.Checkpoint = receipt.Successor.Checkpoint
	want, err := MarshalJournal(receipt.Successor)
	if err != nil {
		return err
	}
	got, err := MarshalJournal(current)
	if err != nil {
		return err
	}
	if !slices.Equal(got, want) {
		return invalid("recovery receipt does not match the current successor journal")
	}
	return nil
}

func ParseRecoveryReceipt(payload []byte) (RecoveryReceiptV1, error) {
	var receipt RecoveryReceiptV1
	if err := decodeBoundedRecord(payload, MaxProtectedRecordBytes, &receipt); err != nil {
		return RecoveryReceiptV1{}, err
	}
	return receipt, receipt.Validate()
}

func MarshalRecoveryReceipt(receipt RecoveryReceiptV1) ([]byte, error) {
	if err := receipt.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(receipt)
	if err != nil {
		return nil, err
	}
	if len(payload)+1 > MaxProtectedRecordBytes {
		return nil, invalid("recovery receipt is too large")
	}
	return append(payload, '\n'), nil
}

package releasetransition

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
)

const (
	JournalSchemaV2           = 2
	SupersededJournalSchemaV1 = 1
	MaxJournalSteps           = 256
	MaxJournalBytes           = 1 << 20
)

type JournalCheckpoint string
type StepCheckpoint string
type EvidenceCheckpoint string

const (
	JournalAuthorized       JournalCheckpoint = "authorized"
	JournalMigrating        JournalCheckpoint = "migrating"
	JournalActivationIntent JournalCheckpoint = "activation-intent"
	JournalTargetActive     JournalCheckpoint = "target-active"
	JournalReconciling      JournalCheckpoint = "reconciling"
	JournalComplete         JournalCheckpoint = "complete"
)

const (
	StepIntent   StepCheckpoint = "intent"
	StepEvidence StepCheckpoint = "evidence"
	StepApplied  StepCheckpoint = "applied"
	StepVerified StepCheckpoint = "verified"
)

const (
	EvidenceCaptured EvidenceCheckpoint = "captured"
	EvidenceApplied  EvidenceCheckpoint = "applied"
	EvidenceVerified EvidenceCheckpoint = "verified"
)

type ReleasePair struct {
	From     ReleaseID  `json:"from"`
	Previous *ReleaseID `json:"previous,omitempty"`
	Target   ReleaseID  `json:"target"`
}

type ReleaseLinks struct {
	Active   ReleaseID  `json:"active"`
	Previous *ReleaseID `json:"previous,omitempty"`
}

type JournalRecord struct {
	SchemaVersion       int                   `json:"schemaVersion"`
	Transaction         TransactionID         `json:"transaction"`
	Goal                Goal                  `json:"goal"`
	Releases            ReleasePair           `json:"releases"`
	AuthorizationPlan   PlanToken             `json:"authorizationPlan"`
	ResumePlan          PlanToken             `json:"resumePlan"`
	ArtifactDigest      Fingerprint           `json:"artifactDigest"`
	RegistryDigest      Fingerprint           `json:"registryDigest"`
	CatalogDigest       Fingerprint           `json:"catalogDigest"`
	ObservationScope    Fingerprint           `json:"observationScope"`
	AuthorizationDigest Fingerprint           `json:"authorizationDigest"`
	IntentDigest        Fingerprint           `json:"intentDigest"`
	SourceIngress       *SourceIngressRequest `json:"sourceIngress,omitempty"`
	Checkpoint          JournalCheckpoint     `json:"checkpoint"`
	Steps               []JournalStep         `json:"steps"`
}

type JournalStep struct {
	ID         string          `json:"id"`
	Migration  string          `json:"migration"`
	Resource   string          `json:"resource"`
	Decision   Decision        `json:"decision"`
	Expected   Fingerprint     `json:"expectedFingerprint"`
	Desired    Fingerprint     `json:"desiredFingerprint"`
	Checkpoint StepCheckpoint  `json:"checkpoint"`
	Evidence   *EvidenceRecord `json:"evidence,omitempty"`
}

type EvidenceRecord struct {
	SchemaVersion int                `json:"schemaVersion"`
	Transaction   TransactionID      `json:"transaction"`
	Releases      ReleasePair        `json:"releases"`
	Step          string             `json:"step"`
	Expected      Fingerprint        `json:"expectedFingerprint"`
	Desired       Fingerprint        `json:"desiredFingerprint"`
	Observed      Fingerprint        `json:"observedFingerprint"`
	Recovery      Fingerprint        `json:"recoveryFingerprint,omitempty"`
	Checkpoint    EvidenceCheckpoint `json:"checkpoint"`
}

// SupersededJournalRecord preserves the exact canonical source journal behind
// a post-activation recovery transaction. It is immutable predecessor
// evidence, not a new current-journal schema.
type SupersededJournalRecord struct {
	SchemaVersion     int                `json:"schemaVersion"`
	AuthorizationPlan PlanToken          `json:"authorizationPlan"`
	Replacement       JournalReplacement `json:"replacement"`
	Journal           JournalRecord      `json:"journal"`
}

const CompatibilityEvidenceSchemaV1 = 1

// CompatibilityEvidence is a stable receipt for an imported predecessor
// history. It deliberately excludes the outer transaction and target so an
// exact import remains provable after later release journals replace it.
type CompatibilityEvidence struct {
	SchemaVersion int                    `json:"schemaVersion"`
	Kind          V2IngressOperationKind `json:"kind"`
	Identity      Fingerprint            `json:"identity"`
	From          ReleaseID              `json:"from"`
	Previous      ReleaseID              `json:"previous"`
}

func (pair ReleasePair) Validate() error {
	if err := validateReleaseID(pair.From, "source release"); err != nil {
		return err
	}
	if err := validateReleaseID(pair.Target, "target release"); err != nil {
		return err
	}
	if pair.Previous != nil {
		if err := validateReleaseID(*pair.Previous, "previous release"); err != nil {
			return err
		}
		if *pair.Previous == pair.From {
			return invalid("previous release equals the source release")
		}
	}
	return nil
}

func (links ReleaseLinks) Validate() error {
	if err := validateReleaseID(links.Active, "active release"); err != nil {
		return err
	}
	if links.Previous != nil {
		if err := validateReleaseID(*links.Previous, "previous link release"); err != nil {
			return err
		}
		if *links.Previous == links.Active {
			return invalid("active and previous links resolve to the same release")
		}
	}
	return nil
}

func (record JournalRecord) Validate() error {
	if record.SchemaVersion != JournalSchemaV2 {
		return invalid("unsupported journal schema %d", record.SchemaVersion)
	}
	if err := validateTransactionID(record.Transaction); err != nil {
		return err
	}
	if err := record.Goal.Validate(); err != nil {
		return err
	}
	if err := record.Releases.Validate(); err != nil {
		return err
	}
	if record.Goal.Target != record.Releases.Target {
		return invalid("journal goal does not match the exact release pair")
	}
	if err := validatePlanToken(record.AuthorizationPlan); err != nil {
		return err
	}
	if err := validateResumePlanToken(record.ResumePlan); err != nil {
		return err
	}
	if record.AuthorizationPlan == record.ResumePlan {
		return invalid("authorization and resume plans must be independently bound")
	}
	for field, value := range map[string]Fingerprint{
		"artifact digest":      record.ArtifactDigest,
		"registry digest":      record.RegistryDigest,
		"catalog digest":       record.CatalogDigest,
		"observation scope":    record.ObservationScope,
		"authorization digest": record.AuthorizationDigest,
	} {
		if err := validateFingerprint(value, field); err != nil {
			return err
		}
	}
	if record.SourceIngress != nil {
		if err := record.SourceIngress.Validate(); err != nil {
			return invalid("journal source ingress descriptor is invalid: %v", err)
		}
	}
	if !validJournalCheckpoint(record.Checkpoint) {
		return invalid("unknown journal checkpoint %q", record.Checkpoint)
	}
	if len(record.Steps) > MaxJournalSteps {
		return invalid("journal has too many steps")
	}
	stepIDs := make(map[string]struct{}, len(record.Steps))
	for index := range record.Steps {
		step := record.Steps[index]
		if err := validateSafeID(step.ID, "journal step ID"); err != nil {
			return err
		}
		if _, exists := stepIDs[step.ID]; exists {
			return invalid("duplicate journal step %q", step.ID)
		}
		stepIDs[step.ID] = struct{}{}
		if err := validateSafeID(step.Migration, "journal migration ID"); err != nil {
			return err
		}
		if err := validateSafeID(step.Resource, "journal resource ID"); err != nil {
			return err
		}
		if !validDecision(step.Decision) || step.Decision == DecisionBlock {
			return invalid("journal step %q has invalid decision %q", step.ID, step.Decision)
		}
		if err := validateFingerprint(step.Expected, "expected step fingerprint"); err != nil {
			return err
		}
		if err := validateFingerprint(step.Desired, "desired step fingerprint"); err != nil {
			return err
		}
		if !validStepCheckpoint(step.Checkpoint) {
			return invalid("journal step %q has unknown checkpoint %q", step.ID, step.Checkpoint)
		}
		if err := validateStepEvidence(record, step); err != nil {
			return err
		}
		if journalAfterMigrations(record.Checkpoint) && step.Checkpoint != StepVerified {
			return invalid("journal activation starts before step %q is verified", step.ID)
		}
	}
	if err := validateFingerprint(record.IntentDigest, "journal intent digest"); err != nil {
		return err
	}
	if record.IntentDigest != bindJournalIntent(
		record.AuthorizationPlan, record.ResumePlan, record.ObservationScope, record.Steps,
	) {
		return invalid("journal step intent does not match its authorized plan binding")
	}
	return nil
}

func (record EvidenceRecord) validate() error {
	if record.SchemaVersion != JournalSchemaV2 {
		return invalid("unsupported evidence schema %d", record.SchemaVersion)
	}
	if err := validateTransactionID(record.Transaction); err != nil {
		return err
	}
	if err := record.Releases.Validate(); err != nil {
		return err
	}
	if err := validateSafeID(record.Step, "evidence step ID"); err != nil {
		return err
	}
	for field, value := range map[string]Fingerprint{
		"evidence expected fingerprint": record.Expected,
		"evidence desired fingerprint":  record.Desired,
		"evidence observed fingerprint": record.Observed,
	} {
		if err := validateFingerprint(value, field); err != nil {
			return err
		}
	}
	if record.Recovery != "" {
		if err := validateFingerprint(record.Recovery, "recovery fingerprint"); err != nil {
			return err
		}
	}
	switch record.Checkpoint {
	case EvidenceCaptured:
		if record.Observed != record.Expected {
			return invalid("captured evidence does not match expected-before")
		}
	case EvidenceApplied, EvidenceVerified:
		if record.Observed != record.Desired {
			return invalid("post-apply evidence does not match desired-after")
		}
	default:
		return invalid("unknown evidence checkpoint %q", record.Checkpoint)
	}
	return nil
}

func ParseJournal(payload []byte) (JournalRecord, error) {
	var record JournalRecord
	if err := decodeBoundedRecord(payload, MaxJournalBytes, &record); err != nil {
		return JournalRecord{}, fmt.Errorf("decode release transition journal: %w", err)
	}
	if err := record.Validate(); err != nil {
		return JournalRecord{}, err
	}
	return record, nil
}

func MarshalJournal(record JournalRecord) ([]byte, error) {
	if err := record.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}

func ParseEvidence(payload []byte) (EvidenceRecord, error) {
	var record EvidenceRecord
	if err := decodeBoundedRecord(payload, MaxJournalBytes, &record); err != nil {
		return EvidenceRecord{}, fmt.Errorf("decode release transition evidence: %w", err)
	}
	if err := record.validate(); err != nil {
		return EvidenceRecord{}, err
	}
	return record, nil
}

func MarshalEvidence(record EvidenceRecord) ([]byte, error) {
	if err := record.validate(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}

func ParseSupersededJournal(payload []byte) (SupersededJournalRecord, error) {
	var record SupersededJournalRecord
	if err := decodeBoundedRecord(payload, MaxJournalBytes, &record); err != nil {
		return SupersededJournalRecord{}, fmt.Errorf("decode superseded release transition journal: %w", err)
	}
	if err := record.validate(); err != nil {
		return SupersededJournalRecord{}, err
	}
	return record, nil
}

func MarshalSupersededJournal(record SupersededJournalRecord) ([]byte, error) {
	if err := record.validate(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}

func (record SupersededJournalRecord) validate() error {
	if record.SchemaVersion != SupersededJournalSchemaV1 {
		return invalid("unsupported superseded journal schema %d", record.SchemaVersion)
	}
	if err := validatePlanToken(record.AuthorizationPlan); err != nil {
		return err
	}
	if err := record.Replacement.Validate(); err != nil {
		return err
	}
	if record.Replacement.Reason != JournalReplacementPostActivationScopeV0111 {
		return invalid("superseded journal requires post-activation replacement")
	}
	if err := record.Journal.Validate(); err != nil {
		return err
	}
	if record.Replacement.Transaction != record.Journal.Transaction {
		return invalid("superseded journal transaction does not match replacement")
	}
	payload, err := MarshalJournal(record.Journal)
	if err != nil {
		return err
	}
	if record.Replacement.Fingerprint != fingerprintPayload(payload) {
		return invalid("superseded journal fingerprint does not match canonical journal")
	}
	return nil
}

func ParseCompatibilityEvidence(payload []byte) (CompatibilityEvidence, error) {
	var record CompatibilityEvidence
	if err := decodeBoundedRecord(payload, MaxJournalBytes, &record); err != nil {
		return CompatibilityEvidence{}, fmt.Errorf("decode compatibility evidence: %w", err)
	}
	if err := record.validate(); err != nil {
		return CompatibilityEvidence{}, err
	}
	return record, nil
}

func MarshalCompatibilityEvidence(record CompatibilityEvidence) ([]byte, error) {
	if err := record.validate(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}

func (record CompatibilityEvidence) validate() error {
	if record.SchemaVersion != CompatibilityEvidenceSchemaV1 ||
		record.Kind != V2LegacyV1Import {
		return invalid("unsupported compatibility evidence schema or kind")
	}
	if err := validateFingerprint(record.Identity, "compatibility evidence identity"); err != nil {
		return err
	}
	if err := validateReleaseID(record.From, "compatibility source release"); err != nil {
		return err
	}
	if err := validateReleaseID(record.Previous, "compatibility previous release"); err != nil {
		return err
	}
	if record.From == record.Previous {
		return invalid("compatibility evidence releases are identical")
	}
	return nil
}

func decodeBoundedRecord(payload []byte, maximum int, target any) error {
	if len(payload) == 0 || len(payload) > maximum {
		return invalid("record size is outside the allowed bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return invalid("record contains trailing JSON")
		}
		return err
	}
	return nil
}

func validateStepEvidence(journal JournalRecord, step JournalStep) error {
	if step.Checkpoint == StepIntent {
		if step.Evidence != nil {
			return invalid("intent-only step %q has premature evidence", step.ID)
		}
		return nil
	}
	if step.Evidence == nil {
		return invalid("step %q is missing evidence", step.ID)
	}
	if err := step.Evidence.validate(); err != nil {
		return err
	}
	evidence := step.Evidence
	if evidence.Transaction != journal.Transaction || !releasePairsEqual(evidence.Releases, journal.Releases) ||
		evidence.Step != step.ID || evidence.Expected != step.Expected || evidence.Desired != step.Desired {
		return invalid("step %q evidence does not match its exact journal intent", step.ID)
	}
	want := map[StepCheckpoint]EvidenceCheckpoint{
		StepEvidence: EvidenceCaptured,
		StepApplied:  EvidenceApplied,
		StepVerified: EvidenceVerified,
	}[step.Checkpoint]
	if evidence.Checkpoint != want {
		return invalid("step %q evidence checkpoint does not match journal checkpoint", step.ID)
	}
	return nil
}

func validJournalCheckpoint(value JournalCheckpoint) bool {
	switch value {
	case JournalAuthorized, JournalMigrating, JournalActivationIntent,
		JournalTargetActive, JournalReconciling, JournalComplete:
		return true
	default:
		return false
	}
}

func validStepCheckpoint(value StepCheckpoint) bool {
	return value == StepIntent || value == StepEvidence || value == StepApplied || value == StepVerified
}

func journalAfterMigrations(value JournalCheckpoint) bool {
	return value == JournalActivationIntent || value == JournalTargetActive ||
		value == JournalReconciling || value == JournalComplete
}

func bindJournalIntent(
	authorizationPlan, resumePlan PlanToken,
	observationScope Fingerprint,
	steps []JournalStep,
) Fingerprint {
	type intent struct {
		ID        string      `json:"id"`
		Migration string      `json:"migration"`
		Resource  string      `json:"resource"`
		Decision  Decision    `json:"decision"`
		Expected  Fingerprint `json:"expected"`
		Desired   Fingerprint `json:"desired"`
	}
	bound := struct {
		AuthorizationPlan PlanToken   `json:"authorizationPlan"`
		ResumePlan        PlanToken   `json:"resumePlan"`
		ObservationScope  Fingerprint `json:"observationScope"`
		Steps             []intent    `json:"steps"`
	}{
		AuthorizationPlan: authorizationPlan,
		ResumePlan:        resumePlan,
		ObservationScope:  observationScope,
		Steps:             make([]intent, len(steps)),
	}
	for index, step := range steps {
		bound.Steps[index] = intent{
			ID: step.ID, Migration: step.Migration, Resource: step.Resource,
			Decision: step.Decision, Expected: step.Expected, Desired: step.Desired,
		}
	}
	payload, _ := json.Marshal(bound)
	digest := sha256.Sum256(payload)
	return Fingerprint(hex.EncodeToString(digest[:]))
}

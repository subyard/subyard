package releasetransition

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestJournalV2ValidatesEmbeddedEvidenceAsItsOnlyAuthority(t *testing.T) {
	pair := releasePair()
	journal := validJournal(pair)
	if err := journal.Validate(); err != nil {
		t.Fatalf("valid journal: %v", err)
	}
	evidence := validEvidence(pair, EvidenceCaptured, digestA)
	journal.Steps[0].Checkpoint = StepEvidence
	journal.Steps[0].Evidence = &evidence
	if err := journal.Validate(); err != nil {
		t.Fatalf("valid embedded evidence: %v", err)
	}

	tests := map[string]func(*JournalRecord){
		"schema":         func(record *JournalRecord) { record.SchemaVersion = 1 },
		"transaction ID": func(record *JournalRecord) { record.Transaction = "../unsafe" },
		"direction":      func(record *JournalRecord) { record.Goal.Direction = Direction("sideways") },
		"release pair":   func(record *JournalRecord) { record.Goal.Target = "3.0.0" },
		"authorization plan": func(record *JournalRecord) {
			record.AuthorizationPlan = "plaintext-plan"
		},
		"resume plan": func(record *JournalRecord) { record.ResumePlan = "plaintext-plan" },
		"digest":      func(record *JournalRecord) { record.CatalogDigest = "not-a-digest" },
		"observation scope": func(record *JournalRecord) {
			record.ObservationScope = "not-a-digest"
		},
		"rebound observation scope": func(record *JournalRecord) {
			record.ObservationScope = digestB
		},
		"checkpoint":  func(record *JournalRecord) { record.Checkpoint = JournalCheckpoint("unknown") },
		"step ID":     func(record *JournalRecord) { record.Steps[0].ID = "../unsafe" },
		"decision":    func(record *JournalRecord) { record.Steps[0].Decision = Decision("execute") },
		"fingerprint": func(record *JournalRecord) { record.Steps[0].Expected = "raw-state" },
		"missing evidence": func(record *JournalRecord) {
			record.Steps[0].Checkpoint = StepEvidence
			record.Steps[0].Evidence = nil
		},
		"foreign evidence transaction": func(record *JournalRecord) {
			evidence := validEvidence(pair, EvidenceCaptured, digestA)
			evidence.Transaction = "tx-foreign"
			record.Steps[0].Checkpoint = StepEvidence
			record.Steps[0].Evidence = &evidence
		},
		"foreign evidence release pair": func(record *JournalRecord) {
			evidence := validEvidence(pair, EvidenceCaptured, digestA)
			evidence.Releases.Target = "3.0.0"
			record.Steps[0].Checkpoint = StepEvidence
			record.Steps[0].Evidence = &evidence
		},
		"activation before verified migrations": func(record *JournalRecord) {
			record.Checkpoint = JournalActivationIntent
		},
		"too many steps": func(record *JournalRecord) {
			record.Steps = make([]JournalStep, MaxJournalSteps+1)
		},
	}
	for name, corrupt := range tests {
		t.Run(name, func(t *testing.T) {
			record := validJournal(pair)
			corrupt(&record)
			if err := record.Validate(); err == nil {
				t.Fatalf("Validate() accepted %#v", record)
			}
		})
	}
}

func TestParseJournalRejectsUnknownFieldsTrailingJSONAndOversizeInput(t *testing.T) {
	journal := validJournal(releasePair())
	payload, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseJournal(payload); err != nil {
		t.Fatalf("ParseJournal(valid) = %v", err)
	}

	unknown := append([]byte(nil), payload[:len(payload)-1]...)
	unknown = append(unknown, []byte(`,"rawPayload":"secret"}`)...)
	if _, err := ParseJournal(unknown); err == nil {
		t.Fatal("ParseJournal accepted an unknown field")
	}
	if _, err := ParseJournal(append(payload, []byte(` {}`)...)); err == nil {
		t.Fatal("ParseJournal accepted trailing JSON")
	}
	if _, err := ParseJournal([]byte(strings.Repeat("x", MaxJournalBytes+1))); err == nil {
		t.Fatal("ParseJournal accepted oversized input")
	}
}

func TestParseJournalAcceptsSemanticallyEqualEmbeddedEvidenceReleasePair(t *testing.T) {
	pair := releasePair()
	journal := validJournal(pair)
	evidence := validEvidence(pair, EvidenceCaptured, digestA)
	journal.Steps[0].Checkpoint = StepEvidence
	journal.Steps[0].Evidence = &evidence
	payload, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseJournal(payload); err != nil {
		t.Fatalf("ParseJournal(valid embedded evidence) = %v", err)
	}
}

func TestCompatibilityEvidenceRoundTripsAndRejectsRebinding(t *testing.T) {
	record := CompatibilityEvidence{
		SchemaVersion: CompatibilityEvidenceSchemaV1,
		Kind:          V2LegacyV1Import,
		Identity:      digestA,
		From:          "release-b",
		Previous:      "release-a",
	}
	payload, err := MarshalCompatibilityEvidence(record)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseCompatibilityEvidence(payload)
	if err != nil || parsed != record {
		t.Fatalf("compatibility evidence = %#v, err=%v", parsed, err)
	}

	for _, corrupt := range []func(*CompatibilityEvidence){
		func(value *CompatibilityEvidence) { value.Kind = V2SourceImport },
		func(value *CompatibilityEvidence) { value.Identity = "raw-state" },
		func(value *CompatibilityEvidence) { value.Previous = value.From },
	} {
		changed := record
		corrupt(&changed)
		if _, err := MarshalCompatibilityEvidence(changed); err == nil {
			t.Fatalf("compatibility evidence accepted %#v", changed)
		}
	}
}

func TestSupersededJournalRoundTripsAndRejectsRebinding(t *testing.T) {
	source := validJournal(ReleasePair{
		From: v2PostActivationPreviousRelease, Target: v2PostActivationSourceRelease,
	})
	source.Transaction = "tx-source"
	for index := range source.Steps {
		source.Steps[index].Evidence = nil
	}
	source.IntentDigest = bindJournalIntent(
		source.AuthorizationPlan, source.ResumePlan, source.ObservationScope, source.Steps,
	)
	sourcePayload, err := MarshalJournal(source)
	if err != nil {
		t.Fatal(err)
	}
	record := SupersededJournalRecord{
		SchemaVersion:     SupersededJournalSchemaV1,
		AuthorizationPlan: PlanToken("plan-v1-" + strings.Repeat("c", 64)),
		Replacement: JournalReplacement{
			Transaction: source.Transaction, Fingerprint: fingerprintPayload(sourcePayload),
			Reason: JournalReplacementPostActivationScopeV0111, SourceVersion: "0.11.1",
		},
		Journal: source,
	}
	payload, err := MarshalSupersededJournal(record)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseSupersededJournal(payload)
	if err != nil {
		t.Fatal(err)
	}
	parsedJournal, err := MarshalJournal(parsed.Journal)
	if err != nil || !bytes.Equal(parsedJournal, sourcePayload) || parsed.Replacement != record.Replacement ||
		parsed.AuthorizationPlan != record.AuthorizationPlan {
		t.Fatalf("superseded journal round trip = %#v, err=%v", parsed, err)
	}

	for name, corrupt := range map[string]func(*SupersededJournalRecord){
		"schema":        func(value *SupersededJournalRecord) { value.SchemaVersion++ },
		"authorization": func(value *SupersededJournalRecord) { value.AuthorizationPlan = "plain" },
		"transaction":   func(value *SupersededJournalRecord) { value.Replacement.Transaction = "tx-other" },
		"fingerprint":   func(value *SupersededJournalRecord) { value.Replacement.Fingerprint = digestA },
		"reason": func(value *SupersededJournalRecord) {
			value.Replacement.Reason = JournalReplacementPreActivationPlanStale
			value.Replacement.SourceVersion = ""
		},
		"source version": func(value *SupersededJournalRecord) { value.Replacement.SourceVersion = "not-semver" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := record
			corrupt(&changed)
			if _, err := MarshalSupersededJournal(changed); err == nil {
				t.Fatalf("superseded journal accepted %#v", changed)
			}
		})
	}
	unknown := append([]byte(nil), payload[:len(payload)-2]...)
	unknown = append(unknown, []byte(`,"unknown":true}\n`)...)
	if _, err := ParseSupersededJournal(unknown); err == nil {
		t.Fatal("superseded journal accepted an unknown field")
	}
	if _, err := ParseSupersededJournal([]byte(strings.Repeat("x", MaxJournalBytes+1))); err == nil {
		t.Fatal("superseded journal accepted oversized input")
	}
}

func TestRedactedPlanFactsRejectUnsafeOrUnboundedPublicFields(t *testing.T) {
	tests := map[string]func(*PlanFacts){
		"unsafe resource":      func(facts *PlanFacts) { facts.Decisions[0].Resource = "../../secret" },
		"unknown decision":     func(facts *PlanFacts) { facts.Decisions[0].Decision = "run-script" },
		"raw multiline result": func(facts *PlanFacts) { facts.Decisions[0].Result = "value\nsecret" },
		"unknown outcome code": func(facts *PlanFacts) {
			facts.Blockers = []Blocker{{Code: "other", Resource: "settings.power-mode", Message: "blocked", Retry: "run yard update --check"}}
		},
		"multiple retry actions": func(facts *PlanFacts) {
			facts.Blockers = []Blocker{{Code: CodePreconditionBlocked, Resource: "settings.power-mode", Message: "blocked", Retry: "run one; run two"}}
		},
		"too many observations": func(facts *PlanFacts) {
			facts.Observations = make([]ResourceObservation, MaxPlanItems+1)
		},
		"oversized assessment consequence": func(facts *PlanFacts) {
			facts.Assessment.Consequences = []string{strings.Repeat("x", maxDiagnosticText+1)}
		},
		"too many assessment consequences": func(facts *PlanFacts) {
			facts.Assessment.Consequences = make([]string, 65)
			for index := range facts.Assessment.Consequences {
				facts.Assessment.Consequences[index] = "bounded consequence"
			}
		},
	}
	for name, corrupt := range tests {
		t.Run(name, func(t *testing.T) {
			facts := basePlanFacts()
			corrupt(&facts)
			if _, err := BindPlan(facts); err == nil {
				t.Fatalf("BindPlan() accepted %#v", facts)
			}
		})
	}
}

func validJournal(pair ReleasePair) JournalRecord {
	record := JournalRecord{
		SchemaVersion:       JournalSchemaV2,
		Transaction:         "tx-001",
		Goal:                targetGoal(pair),
		Releases:            pair,
		AuthorizationPlan:   PlanToken("plan-v1-" + strings.Repeat("a", 64)),
		ResumePlan:          PlanToken("resume-v1-" + strings.Repeat("b", 64)),
		ArtifactDigest:      digestA,
		RegistryDigest:      digestB,
		CatalogDigest:       digestD,
		ObservationScope:    digestA,
		AuthorizationDigest: digestC,
		Checkpoint:          JournalAuthorized,
		Steps: []JournalStep{{
			ID: "settings-v2", Migration: "settings-v2", Resource: "yard.hermes",
			Decision: DecisionTransform, Expected: digestA,
			Desired: digestB, Checkpoint: StepIntent,
		}},
	}
	record.IntentDigest = bindJournalIntent(
		record.AuthorizationPlan, record.ResumePlan, record.ObservationScope, record.Steps,
	)
	return record
}

func TestJournalPreservesSourceIngressRecoveryDescriptor(t *testing.T) {
	record := validJournal(releasePair())
	record.SourceIngress = &SourceIngressRequest{
		SchemaVersion: SourceIngressRequestSchemaV1,
		Kind:          SourceIngressPreGoV1,
		SourceRoot:    "/home/operator/source",
		DataHome:      "/home/operator/.subyard",
		BinDir:        "/home/operator/.local/bin",
		RC:            "/home/operator/.bashrc",
		LoginRC:       "/home/operator/.profile",
	}
	payload, err := MarshalJournal(record)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseJournal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.SourceIngress == nil || *parsed.SourceIngress != *record.SourceIngress {
		t.Fatalf("source ingress recovery descriptor changed: %#v", parsed.SourceIngress)
	}

	record.SourceIngress.SourceRoot = "relative"
	if _, err := MarshalJournal(record); err == nil {
		t.Fatal("journal accepted an unsafe source ingress recovery descriptor")
	}
}

func validEvidence(pair ReleasePair, checkpoint EvidenceCheckpoint, observed Fingerprint) EvidenceRecord {
	return EvidenceRecord{
		SchemaVersion: JournalSchemaV2,
		Transaction:   "tx-001",
		Releases:      pair,
		Step:          "settings-v2",
		Expected:      digestA,
		Desired:       digestB,
		Observed:      observed,
		Checkpoint:    checkpoint,
	}
}

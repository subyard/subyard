package releasetransition

import (
	"bytes"
	"encoding/json"
	"testing"
)

func rollbackClosureFixture(t *testing.T) (*POSIXV2Store, rollbackClosureV1) {
	t.Helper()
	store, transaction, inventory := rollbackArtifactFixture(t)
	source, target := rewindRegistries()
	sourcePayload, err := json.MarshalIndent(source, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	targetPayload, err := json.MarshalIndent(target, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	journal := validJournal(ReleasePair{From: "source", Target: "target"})
	journal.Transaction = "tx-rollback"
	journal.Goal.Direction = DirectionActivatePrevious
	journal.Checkpoint = JournalComplete
	journal.RegistryDigest = fingerprintPayload(sourcePayload)
	native := completeRewindLedger(target)
	request := RollbackTargetRequest{SchemaVersion: RollbackTargetProcessSchemaV1, Mode: "activate-under-v2", RuntimeRoot: store.configHome + "/runtime", ConfigHome: store.configHome, Releases: journal.Releases, SourceArtifact: digestD, SourceRegistry: journal.RegistryDigest, TargetArtifact: journal.ArtifactDigest, TargetRegistry: fingerprintPayload(targetPayload), Projection: digestB, NativeHistory: &native}
	policy, err := newV2ActionPolicy()
	if err != nil {
		t.Fatal(err)
	}
	assessment, err := assessV2Action(policy, true, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan := RollbackActivationPlan{Assessment: assessment, Scopes: []RollbackActivationScope{}}
	owner := &V2Transition{registry: target}
	plan.Token, err = owner.rollbackActivationPlan(request, plan.Assessment, plan.Scopes)
	if err != nil {
		t.Fatal(err)
	}
	_, history, err := MarshalLedgerV2(native, target)
	if err != nil {
		t.Fatal(err)
	}
	pin := fingerprintPayload([]byte(plan.Token))
	step := JournalStep{ID: RollbackTargetActivationStep, Migration: RollbackTargetActivationStep, Resource: "activation.rollback-target-plan", Decision: DecisionPreserve, Expected: pin, Desired: pin, Checkpoint: StepVerified}
	evidence := evidenceFor(journal, step, EvidenceVerified, pin, "")
	step.Evidence = &evidence
	ownerStep := rollbackOwnerFixtureStep(t, request)
	ownerEvidence := evidenceFor(journal, ownerStep, EvidenceVerified, ownerStep.Desired, "")
	ownerStep.Evidence = &ownerEvidence
	journal.Steps = []JournalStep{ownerStep, step}
	journal.IntentDigest = bindJournalIntent(journal.AuthorizationPlan, journal.ResumePlan, journal.ObservationScope, journal.Steps)
	journalPayload, err := MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	closure := rollbackClosureV1{SchemaVersion: 1, Transaction: transaction, Releases: ReleasePair{From: "older", Target: "source"}, SourceArtifact: digestD, SourceRegistry: sourcePayload, TargetRegistry: targetPayload, RollbackJournal: journalPayload, ActivationRequest: request, ActivationPlan: plan,
		Completion: RollbackTargetCompletion{Contract: RollbackTargetContractV1, Transaction: journal.Transaction, Intent: journal.IntentDigest, Plan: plan.Token, TargetRegistry: fingerprintPayload(targetPayload), TargetArtifact: journal.ArtifactDigest, Checkpoint: digestC, Projection: digestB, NativeHistory: history}, Artifacts: inventory}
	return store, closure
}

func TestRollbackClosurePreservesExactSealedBytesAndClosedInventory(t *testing.T) {
	store, closure := rollbackClosureFixture(t)
	payload, err := marshalRollbackClosure(closure)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := parseRollbackClosure(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded.SourceRegistry, closure.SourceRegistry) || !bytes.Equal(decoded.RollbackJournal, closure.RollbackJournal) {
		t.Fatal("closure changed sealed or journal bytes")
	}
	if err := store.createImmutable(rollbackClosureParts, string(closure.Transaction)+".json", payload); err != nil {
		t.Fatal(err)
	}
	fresh, err := NewPOSIXV2Store(store.configHome)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.readRollbackClosure(closure.Transaction); err != nil {
		t.Fatal(err)
	}
	// This parser admits closed provenance containing a future kind. It still
	// cannot admit that kind to the executable catalog or native history.
	if _, _, err := ParseRegistryV2(decoded.SourceRegistry, BuiltinCapabilityCatalog()); err == nil {
		t.Fatal("future source registry unexpectedly became executable")
	}
}

func TestRollbackClosureRejectsUnboundCompletion(t *testing.T) {
	for _, change := range []string{"intent", "target-registry", "transaction", "plan", "artifact", "projection", "source-registry", "foreign-owner", "duplicate-inventory", "missing-inventory", "source-artifact", "roots", "scope", "native-history"} {
		t.Run(change, func(t *testing.T) {
			_, closure := rollbackClosureFixture(t)
			switch change {
			case "intent":
				closure.Completion.Intent = digestD
			case "target-registry":
				closure.Completion.TargetRegistry = digestD
			case "transaction":
				closure.Completion.Transaction = "different-transaction"
			case "plan":
				closure.Completion.Plan = PlanToken("plan-v1-" + string(digestD))
			case "artifact":
				closure.Completion.TargetArtifact = digestD
			case "projection":
				closure.ProjectionProducer = digestD
			case "source-registry":
				closure.SourceRegistry = append(closure.SourceRegistry, '\n')
			case "foreign-owner":
				closure.Releases.Target = "foreign-owner"
			case "duplicate-inventory":
				closure.Artifacts = append(closure.Artifacts, closure.Artifacts[0])
			case "missing-inventory":
				closure.Artifacts = nil
			case "source-artifact":
				closure.SourceArtifact = digestA
				closure.ActivationRequest.SourceArtifact = digestA
			case "roots":
				closure.ActivationRequest.ConfigHome += "/other"
			case "scope":
				closure.ActivationPlan.Scopes = []RollbackActivationScope{{ID: "foreign-owner", Desired: digestA}}
			case "native-history":
				closure.ActivationRequest.NativeHistory.Domains["settings"] = DomainLedgerV2{Epoch: 1, Applied: []string{}}
			}
			if _, err := marshalRollbackClosure(closure); err == nil {
				t.Fatal("unbound closure was accepted")
			}
		})
	}
}

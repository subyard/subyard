package releasetransition

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
)

type rollbackTargetFixture struct {
	transition *V2Transition
	owner      *v2TestReconciler
	request    RollbackTargetRequest
	journal    ProtectedSnapshot
	state      MigrationLedgerSnapshot
}

func newRollbackTargetFixture(t *testing.T) rollbackTargetFixture {
	t.Helper()
	transition, home, _ := v2TransitionFixture(t, nil)
	pair := ReleasePair{From: "release-s", Previous: releaseIDPointer("release-a"), Target: "release-a"}
	links := ReleaseLinks{Active: pair.From, Previous: pair.Previous}
	transition.options.Releases = pair
	transition.options.ObserveLinks = func(context.Context) (ReleaseLinks, error) { return links, nil }
	transition.options.ActivateLinks = func(context.Context, ReleasePair) (ReleaseLinks, error) {
		t.Fatal("R rotated links under S's journal")
		return ReleaseLinks{}, nil
	}
	transition.options.VerifyAuthorization = func(_ PlanToken, authorization Authorization) bool { return authorization == "S-grant" }
	owner := &v2TestReconciler{id: "target-runtime"}
	transition.options.Reconcilers = []V2ActivationReconciler{owner}
	native := completeRewindLedger(transition.registry)
	payload, _, err := MarshalLedgerV2(native, transition.registry)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := transition.store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	if err := transition.store.CompareAndSwapLedger(missing, payload); err != nil {
		t.Fatal(err)
	}
	state, err := transition.store.ReadMigrationLedger(transition.registry, transition.registryDigest)
	if err != nil {
		t.Fatal(err)
	}
	if err := transition.store.ConvertMigrationLedger(state, transition.registry, transition.registryDigest, transition.registry, transition.registryDigest); err != nil {
		t.Fatal(err)
	}
	state, err = transition.store.ReadMigrationLedger(transition.registry, transition.registryDigest)
	if err != nil {
		t.Fatal(err)
	}
	request := RollbackTargetRequest{SchemaVersion: RollbackTargetProcessSchemaV1, Mode: "activate-under-v2", RuntimeRoot: home + "/runtime", ConfigHome: home, Yard: "default", Releases: pair, SourceArtifact: digestC, SourceRegistry: digestD, TargetArtifact: transition.options.ArtifactDigest, TargetRegistry: transition.registryDigest, Projection: state.ProjectionSnapshot.Fingerprint, NativeHistory: &native}
	inspected, err := transition.InspectRollbackTargetActivation(context.Background(), request)
	if err != nil || len(inspected.Blockers) != 0 {
		t.Fatalf("activation assessment = %+v, %v", inspected, err)
	}
	if owner.reconciles != 0 {
		t.Fatal("inspection performed activation")
	}
	journal := validJournal(pair)
	journal.Goal.Direction = DirectionActivatePrevious
	journal.ArtifactDigest = request.TargetArtifact
	journal.RegistryDigest = request.SourceRegistry
	journal.AuthorizationDigest = fingerprintPayload([]byte("S-grant"))
	journal.Checkpoint = JournalReconciling
	pin := fingerprintPayload([]byte(inspected.Plan.Token))
	journal.Steps = []JournalStep{{ID: RollbackTargetActivationStep, Migration: RollbackTargetActivationStep, Resource: "activation.rollback-target-plan", Decision: DecisionPreserve, Expected: pin, Desired: pin, Checkpoint: StepVerified}}
	evidence := evidenceFor(journal, journal.Steps[0], EvidenceVerified, pin, "")
	journal.Steps[0].Evidence = &evidence
	journal.IntentDigest = bindJournalIntent(journal.AuthorizationPlan, journal.ResumePlan, journal.ObservationScope, journal.Steps)
	payload, err = MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	absent, err := transition.store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	if err := transition.store.CompareAndSwapCurrentJournal(absent, payload); err != nil {
		t.Fatal(err)
	}
	selected, err := transition.store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	request.Journal = selected.Fingerprint
	request.Plan = &inspected.Plan
	request.Execution = &Execution{Plan: inspected.Plan.Token, Authorization: "S-grant"}
	// The owner S performs the link switch before invoking R's activation mode.
	links = ReleaseLinks{Active: pair.Target, Previous: releaseIDPointer(pair.From)}
	return rollbackTargetFixture{transition, owner, request, selected, state}
}

func TestRollbackTargetActivationUsesForeignJournalWithoutWritingHistory(t *testing.T) {
	f := newRollbackTargetFixture(t)
	for attempt := 0; attempt < 2; attempt++ {
		response, err := f.transition.ConvergeRollbackTargetActivation(context.Background(), f.request)
		if err != nil || response.Completion == nil {
			t.Fatalf("target activation = %+v, %v", response, err)
		}
		if err := response.Validate(); err != nil {
			t.Fatal(err)
		}
		if response.Completion.TargetRegistry != f.transition.registryDigest || response.Completion.Transaction != "tx-001" || response.Completion.Intent == "" {
			t.Fatalf("foreign completion = %+v", response.Completion)
		}
		selected, err := f.transition.store.ReadCurrentJournal()
		if err != nil || !sameProtectedSnapshot(selected, f.journal) {
			t.Fatal("R changed S's journal")
		}
		history, err := f.transition.store.ReadMigrationLedger(f.transition.registry, f.transition.registryDigest)
		if err != nil || !sameProtectedSnapshot(history.AuthoritySnapshot, f.state.AuthoritySnapshot) || !sameProtectedSnapshot(history.ProjectionSnapshot, f.state.ProjectionSnapshot) {
			t.Fatal("R changed migration authority")
		}
	}
	if !f.owner.converged {
		t.Fatal("target resource did not converge")
	}
}

func TestRollbackTargetActivationRejectsUnboundExecutionBeforeEffects(t *testing.T) {
	for _, change := range []string{"grant", "journal", "source artifact", "native history", "plan", "desired scope", "new consequence"} {
		t.Run(change, func(t *testing.T) {
			f := newRollbackTargetFixture(t)
			switch change {
			case "grant":
				f.request.Execution.Authorization = "foreign-grant"
			case "journal":
				f.request.Journal = digestB
			case "source artifact":
				f.request.SourceArtifact = digestB
			case "native history":
				f.request.NativeHistory.Domains["settings"] = DomainLedgerV2{Epoch: 1, Applied: []string{}}
			case "plan":
				f.request.Plan.Token = PlanToken("plan-v1-" + string(digestB))
			case "desired scope":
				f.owner.desired = digestD
			case "new consequence":
				f.owner.consequences = []string{"unassessed restart"}
			}
			if _, err := f.transition.ConvergeRollbackTargetActivation(context.Background(), f.request); err == nil {
				t.Fatal("unbound target execution accepted")
			}
			if f.owner.reconciles != 0 {
				t.Fatal("unbound target activation performed effects")
			}
		})
	}
}

func TestRollbackTargetProtocolIsBoundedAndStrict(t *testing.T) {
	probe := []byte(`{"schemaVersion":3,"mode":"capabilities"}`)
	request, err := ParseRollbackTargetRequest(probe)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseRollbackTargetRequest(encoded); err != nil {
		t.Fatal(err)
	}
	for _, payload := range [][]byte{
		[]byte(`{"schemaVersion":3,"mode":"capabilities","configHome":"/private"}`),
		[]byte(`{"schemaVersion":3,"mode":"capabilities","unknown":true}`),
		append(append([]byte{}, probe...), []byte(" {}")...), bytes.Repeat([]byte("x"), MaxJournalBytes+1),
	} {
		if _, err := ParseRollbackTargetRequest(payload); err == nil {
			t.Fatal("invalid target request accepted")
		}
	}
	response := RollbackTargetResponse{SchemaVersion: 3, Contract: RollbackTargetContractV1}
	payload, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseRollbackTargetResponse(payload); err != nil {
		t.Fatal(err)
	}
	response.Inspection = &RollbackActivationInspection{}
	if err := response.Validate(); err == nil {
		t.Fatal("ambiguous target response accepted")
	}
}

func TestRegistryProvenanceDoesNotGrantExecutionSupport(t *testing.T) {
	source, _ := rewindRegistries()
	payload, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ParseRegistryV2(payload, BuiltinCapabilityCatalog()); err == nil {
		t.Fatal("strict executor accepted unknown kind")
	}
	if registry, digest, err := ParseRegistryV2Provenance(payload); err != nil || digest != fingerprintPayload(payload) || len(registry.Migrations) != 2 {
		t.Fatalf("structural provenance = %+v, %s, %v", registry, digest, err)
	}
	source.Migrations[1].FromEpoch = 5
	payload, err = json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ParseRegistryV2Provenance(payload); err == nil {
		t.Fatal("structural reader accepted broken domain history")
	}
}

func TestRollbackTargetActivationResumesAfterUncertainResult(t *testing.T) {
	f := newRollbackTargetFixture(t)
	interrupted := false
	f.transition.options.fault = func(point string) error {
		if point == "after-reconciler-0" && !interrupted {
			interrupted = true
			return context.Canceled
		}
		return nil
	}
	response, err := f.transition.ConvergeRollbackTargetActivation(context.Background(), f.request)
	if err == nil || response.Completion != nil || !f.owner.converged {
		t.Fatalf("uncertain activation = %+v, %v", response, err)
	}
	options := f.transition.options
	options.fault = nil
	f.transition, err = NewV2Transition(options)
	if err != nil {
		t.Fatal(err)
	}
	response, err = f.transition.ConvergeRollbackTargetActivation(context.Background(), f.request)
	if err != nil || response.Completion == nil {
		t.Fatalf("fresh target process could not resume = %+v, %v", response, err)
	}
	selected, err := f.transition.store.ReadCurrentJournal()
	if err != nil || !sameProtectedSnapshot(selected, f.journal) {
		t.Fatal("target resume changed S's transaction")
	}
}

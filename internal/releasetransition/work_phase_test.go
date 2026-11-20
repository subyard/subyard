package releasetransition

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/migrationhistory"
)

func TestV2InvalidWorkRecipesRefuseBeforeWrites(t *testing.T) {
	transition, home, _ := v2TransitionFixture(t, nil)
	transition.options.Ingress = &v2IngressFixture{}
	operation := V2IngressOperation{Kind: V2SourceImport, Decision: DecisionTransform, Expected: digestA, Desired: digestB}
	intent := ingressIntent(operation)
	step := JournalStep{ID: intent.ID, Migration: intent.Migration, Resource: intent.Resource, Decision: intent.Decision, Expected: intent.Expected, Desired: intent.Desired, Checkpoint: StepIntent}
	journal := JournalRecord{Transaction: "tx-refused-work", Releases: transition.options.Releases, Steps: []JournalStep{step}}
	before := snapshotStoreV2TreeIfPresent(t, home)
	for _, work := range []v2Work{{intent: intent}, {intent: intent, recipe: &ingressWorkRecipe{}}} {
		if _, err := transition.bindWorkStep(journal, 0, work); err == nil {
			t.Fatal("invalid recipe accepted")
		}
		if !bytes.Equal(before, snapshotStoreV2TreeIfPresent(t, home)) {
			t.Fatal("invalid recipe wrote")
		}
	}
}

func TestV2WorkConstructorsOwnNestedInputs(t *testing.T) {
	previous := ReleaseID("previous")
	binding := V2IngressBinding{Releases: ReleasePair{Previous: &previous}, Steps: []V2IngressStepBinding{{Kind: V2SourceImport}}}
	work := ingressMigrationWork(PlannerStepIntent{}, V2IngressOperation{}, binding)
	previous = "changed"
	binding.Steps[0].Kind = V2SourceEntrypoints
	if *work.ingressRecipe().binding.Releases.Previous == "changed" || work.ingressRecipe().binding.Steps[0].Kind != V2SourceImport {
		t.Fatal("ingress work aliases binding")
	}
}

func TestRuntimeReadyProofGuardsPublicationAndSuccess(t *testing.T) {
	transition, store := completedSourceMetadataFixture(t)
	snapshot, err := store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	journal, err := ParseJournal(snapshot.Payload)
	if err != nil {
		t.Fatal(err)
	}
	outcome, proof, err := transition.evaluateCurrent(context.Background(), journal)
	if err != nil || outcome.Status != StatusReady {
		t.Fatalf("ready observation: %+v %v", outcome, err)
	}
	if err := proof.validateRuntimeReadyProof(transition, journal.Transaction); err != nil {
		t.Fatal(err)
	}
	foreign, err := NewV2Transition(transition.options)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotStoreV2Tree(t, store.configHome)
	badCompletion := proof
	badCompletion.completion = migrationhistory.Completion{}
	for _, candidate := range []runtimeReadyProof{{}, badCompletion} {
		if _, err := transition.publishCheckpointAfterReady(context.Background(), journal, candidate); err == nil {
			t.Fatal("invalid readiness proof permitted checkpoint publication")
		}
		if result, err := transition.cleanupReady(context.Background(), journal.Transaction, candidate); err == nil || result.Status == StatusReady {
			t.Fatal("invalid readiness proof produced success")
		}
	}
	if err := proof.validateRuntimeReadyProof(foreign, journal.Transaction); err == nil {
		t.Fatal("foreign readiness proof accepted")
	}
	changed := journal
	changed.ArtifactDigest = digestB
	if _, err := transition.publishCheckpointAfterReady(context.Background(), changed, proof); err == nil {
		t.Fatal("readiness proof admitted different journal facts")
	}
	first, err := transition.readyResult(proof)
	if err != nil {
		t.Fatal(err)
	}
	if first.Transaction != nil {
		*first.Transaction = "changed"
		if *proof.outcome.Transaction == "changed" {
			t.Fatal("readiness result aliases transaction")
		}
	}

	if !bytes.Equal(before, snapshotStoreV2Tree(t, store.configHome)) {
		t.Fatal("invalid readiness proof wrote protected state")
	}
	transition.options.Reconcilers = []V2ActivationReconciler{&v2FailingReconciler{id: "runtime"}}
	outcome, unready, err := transition.evaluateCurrent(context.Background(), journal)
	if err != nil || outcome.Status == StatusReady || unready.validateRuntimeReadyProof(transition, journal.Transaction) == nil {
		t.Fatalf("migration completion alone created runtime readiness: %+v %v", outcome, err)
	}
}

func TestObservedJournalReadyProofBindsCompletedHistory(t *testing.T) {
	transition, _ := completedSourceMetadataFixture(t)
	observation, err := transition.observe(context.Background(), Goal{Target: transition.options.Releases.Target, Direction: transition.options.Direction})
	if err != nil {
		t.Fatal(err)
	}
	proof, err := transition.observedJournalReadyProof(observation)
	if err != nil || proof.validateJournal(transition, *observation.journal) != nil || proof.outcome.Transaction == nil {
		t.Fatalf("observed journal proof: %+v %v", proof, err)
	}
	changed := *observation.journal
	changed.ArtifactDigest = digestB
	if proof.validateJournal(transition, changed) == nil {
		t.Fatal("observed proof accepted changed journal facts")
	}
	observation.journal = &changed
	if _, err := transition.observedJournalReadyProof(observation); err == nil {
		t.Fatal("foreign completed history produced a journal readiness proof")
	}
}

func TestV2RejectsTypedNilAdapters(t *testing.T) {
	transition, _, _ := v2TransitionFixture(t, nil)
	options := transition.options
	var ingress *v2IngressFixture
	options.Ingress = ingress
	if _, err := NewV2Transition(options); err == nil {
		t.Fatal("typed nil ingress accepted")
	}
	options = transition.options
	var reconciler *v2FailingReconciler
	options.Reconcilers = []V2ActivationReconciler{reconciler}
	if _, err := NewV2Transition(options); err == nil {
		t.Fatal("typed nil reconciler accepted")
	}
}

func TestBoundWorkStepRejectsZeroForeignAndChangedEvidencePhase(t *testing.T) {
	transition, _, _ := v2TransitionFixture(t, nil)
	operation := V2IngressOperation{Kind: V2SourceImport, Decision: DecisionTransform, Expected: digestA, Desired: digestB}
	transition.options.Ingress = &v2IngressFixture{}
	work := ingressMigrationWork(ingressIntent(operation), operation, V2IngressBinding{})
	observation := v2Observation{links: ReleaseLinks{Active: "release-a"}}

	intent := work.intent
	step := JournalStep{ID: intent.ID, Migration: intent.Migration, Resource: intent.Resource, Decision: intent.Decision, Expected: intent.Expected, Desired: intent.Desired, Checkpoint: StepIntent}
	journal := JournalRecord{Transaction: "tx-bound-step", Releases: transition.options.Releases, Steps: []JournalStep{step}}
	bound, err := transition.bindWorkStep(journal, 0, work)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotStoreV2TreeIfPresent(t, transition.options.ConfigHome)
	if _, err := transition.convergeStep(context.Background(), &journal, nil, bound, observation.links); err == nil {
		t.Fatal("nil journal snapshot accepted")
	}
	malformed := ProtectedSnapshot{}
	if _, err := transition.convergeStep(context.Background(), &journal, &malformed, bound, observation.links); err == nil {
		t.Fatal("malformed journal snapshot accepted")
	}
	if !bytes.Equal(before, snapshotStoreV2TreeIfPresent(t, transition.options.ConfigHome)) {
		t.Fatal("invalid journal snapshot wrote protected state")
	}

	foreign, err := NewV2Transition(transition.options)
	if err != nil {
		t.Fatal(err)
	}
	if err := bound.validateBoundWorkStep(foreign, &journal); err == nil {
		t.Fatal("foreign bound work accepted")
	}
	if err := (boundWorkStep{}).validateBoundWorkStep(transition, &journal); err == nil {
		t.Fatal("zero bound work accepted")
	}
	journal.Steps[0].Checkpoint = StepEvidence
	evidence := evidenceFor(journal, journal.Steps[0], EvidenceCaptured, step.Expected, digestA)
	evidence.Transaction = "tx-foreign"
	journal.Steps[0].Evidence = &evidence
	if _, err := transition.bindWorkStep(journal, 0, work); err == nil {
		t.Fatal("foreign evidence transaction accepted")
	}
	if err := bound.validateBoundWorkStep(transition, &journal); err == nil {
		t.Fatal("stale evidence phase accepted")
	}
}

func TestV2IngressStepAndTransitionOwnReleasePointers(t *testing.T) {
	transition, _, _ := v2TransitionFixture(t, nil)
	previous := ReleaseID("release-old")
	options := transition.options
	options.Releases.Previous = &previous
	owned, err := NewV2Transition(options)
	if err != nil {
		t.Fatal(err)
	}
	previous = "changed"
	if *owned.options.Releases.Previous == "changed" {
		t.Fatal("transition aliases caller release pointer")
	}
	previous = "release-old"
	plan, err := BindPlan(basePlanFacts())
	if err != nil {
		t.Fatal(err)
	}
	binding := V2IngressBinding{Transaction: "tx-ingress-owned", Plan: plan, Releases: ReleasePair{From: "release-a", Target: "release-b", Previous: &previous}, Steps: []V2IngressStepBinding{{Kind: V2SourceImport, Checkpoint: StepIntent, Expected: digestA, Desired: digestB}}}
	step, err := ingressStep(binding, V2IngressOperation{Kind: V2SourceImport, Decision: DecisionCanonicalize, Expected: digestA, Desired: digestB})
	if err != nil {
		t.Fatal(err)
	}
	*step.Binding.Releases.Previous = "changed"
	step.Binding.Steps[0].Expected = digestC
	if previous != "release-old" || binding.Steps[0].Expected != digestA {
		t.Fatal("ingress step aliases its authoritative binding")
	}
}

func snapshotStoreV2TreeIfPresent(t *testing.T, home string) []byte {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(home, "release-transition", "v2")); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		t.Fatal(err)
	}
	return snapshotStoreV2Tree(t, home)
}

func TestNewPhaseConsumersRejectNilOwners(t *testing.T) {
	var transition *V2Transition
	if _, err := transition.bindWorkStep(JournalRecord{}, 0, v2Work{}); err == nil {
		t.Fatal("nil work owner accepted")
	}
	if _, err := transition.observedReadyProof(v2Observation{}); err == nil {
		t.Fatal("nil readiness producer accepted")
	}
	if _, err := transition.readyResult(runtimeReadyProof{}); err == nil {
		t.Fatal("nil success consumer accepted")
	}
	if _, err := transition.cleanupReady(context.Background(), "tx", runtimeReadyProof{}); err == nil {
		t.Fatal("nil cleanup consumer accepted")
	}
	if _, err := transition.publishCheckpointAfterReady(context.Background(), JournalRecord{}, runtimeReadyProof{}); err == nil {
		t.Fatal("nil publication consumer accepted")
	}
}

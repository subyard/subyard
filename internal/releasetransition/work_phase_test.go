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
	observation, err := transition.observe(context.Background(), Goal{Target: "release-a", Direction: DirectionActivateTarget})
	if err != nil {
		t.Fatal(err)
	}
	var valid v2Work
	for _, work := range observation.work {
		if work.kind() == v2SettingsWork {
			valid = work
			break
		}
	}
	intent := valid.intent
	step := JournalStep{ID: intent.ID, Migration: intent.Migration, Resource: intent.Resource, Decision: intent.Decision, Expected: intent.Expected, Desired: intent.Desired, Checkpoint: StepIntent}
	journal := JournalRecord{Transaction: "tx-refused-work", Releases: transition.options.Releases, Steps: []JournalStep{step}}
	var nilSettings *settingsWorkRecipe
	var nilOwner *ownerWorkRecipe
	var nilLedger *ledgerWorkRecipe
	var nilIngress *ingressWorkRecipe
	recipes := []v2WorkRecipe{nil, nilSettings, nilOwner, nilLedger, nilIngress, &settingsWorkRecipe{}, &ownerWorkRecipe{}, &ledgerWorkRecipe{}, &ingressWorkRecipe{}, &recordedSettingsWork{intent: intent}}
	payload, _, err := MarshalLedgerV2(BaselineLedgerV2(transition.registry), transition.registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := transition.store.CompareAndSwapLedger(absentProtectedSnapshot(), payload); err != nil {
		t.Fatal(err)
	}
	settingsBefore, err := os.ReadFile(valid.settingsFile().Path)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotStoreV2Tree(t, home)
	for _, recipe := range recipes {
		work := v2Work{intent: intent, migration: valid.migration, recipe: recipe}
		snapshot := absentProtectedSnapshot()
		bound, bindErr := transition.bindWorkStep(journal, 0, work)
		if bindErr == nil {
			if _, err := transition.convergeStep(context.Background(), &journal, &snapshot, bound, observation.links); err == nil {
				t.Fatalf("invalid recipe %T accepted", recipe)
			}
		}
		if !bytes.Equal(before, snapshotStoreV2Tree(t, home)) {
			t.Fatalf("invalid recipe %T wrote protected state", recipe)
		}
		current, err := os.ReadFile(valid.settingsFile().Path)
		if err != nil || !bytes.Equal(current, settingsBefore) {
			t.Fatalf("invalid recipe %T changed settings", recipe)
		}
	}
}

func TestV2WorkConstructorsOwnNestedInputs(t *testing.T) {
	transition, _, _ := v2TransitionFixture(t, nil)
	observation, err := transition.observe(context.Background(), Goal{Target: "release-a", Direction: DirectionActivateTarget})
	if err != nil {
		t.Fatal(err)
	}
	for _, work := range observation.work {
		if work.kind() == v2SettingsWork {
			file := *work.settingsFile()
			file.Desired = append([]byte(nil), file.Desired...)
			file.Expected.Content = append([]byte(nil), file.Expected.Content...)
			file.Before = append([]settingsV2BeforeValue(nil), file.Before...)
			migration := work.migration
			migration.DependsOn = []string{"dependency"}
			owned := settingsMigrationWork(work.intent, migration, file)
			file.Desired[0] ^= 1
			file.Expected.Content[0] ^= 1
			file.Before[0].Value = "changed"
			migration.DependsOn[0] = "changed"
			if bytes.Equal(file.Desired, owned.settingsFile().Desired) || bytes.Equal(file.Expected.Content, owned.settingsFile().Expected.Content) || owned.settingsFile().Before[0].Value == "changed" || owned.migration.DependsOn[0] == "changed" {
				t.Fatal("settings work aliases constructor input")
			}
		}
		if work.kind() == v2LedgerWork {
			from := work.ledgerRecipe().from
			from.snapshot.Payload = append([]byte(nil), from.snapshot.Payload...)
			to := append([]byte(nil), work.ledgerRecipe().to...)
			owned := ledgerMigrationWork(work.intent, work.migration, work.ledgerRecipe().edge, from, to)
			if len(from.snapshot.Payload) > 0 {
				from.snapshot.Payload[0] ^= 1
				if bytes.Equal(from.snapshot.Payload, owned.ledgerRecipe().from.snapshot.Payload) {
					t.Fatal("ledger work aliases before bytes")
				}
			}
			to[0] ^= 1
			if bytes.Equal(to, owned.ledgerRecipe().to) {
				t.Fatal("ledger work aliases after bytes")
			}
		}
	}
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

func TestV2RejectsTypedNilAdapters(t *testing.T) {
	transition, _, _ := v2TransitionFixture(t, nil)
	options := transition.options
	var owner *v2TestOwnerRegistration
	options.OwnerRegistration = owner
	if _, err := NewV2Transition(options); err == nil {
		t.Fatal("typed nil owner accepted")
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
	observation, err := transition.observe(context.Background(), Goal{Target: "release-a", Direction: DirectionActivateTarget})
	if err != nil {
		t.Fatal(err)
	}
	var work v2Work
	for _, candidate := range observation.work {
		if candidate.kind() == v2SettingsWork {
			work = candidate
			break
		}
	}
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
	if err := transition.validateLedgerWork(v2Work{}, JournalStep{}); err == nil {
		t.Fatal("nil ledger owner accepted")
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

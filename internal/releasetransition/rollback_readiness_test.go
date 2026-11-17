package releasetransition

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func rollbackReadinessFixture(t *testing.T, converged bool, direction Direction) (*V2Transition, *v2TestReconciler, rollbackClosureV1) {
	t.Helper()
	store, _, closure := rollbackRetentionFixture(t)
	owner := &v2TestReconciler{id: "target-runtime", converged: converged}
	links := ReleaseLinks{Active: "target", Previous: releaseIDPointer("source")}
	var rollbackTarget *RollbackTarget
	if direction == DirectionActivatePrevious {
		rollbackTarget = &RollbackTarget{Version: "0.30.0", RegistryDigest: closure.Completion.TargetRegistry}
	}
	transition, err := NewV2Transition(V2Options{ConfigHome: store.configHome,
		RegistryPayload: closure.TargetRegistry, ArtifactDigest: closure.Completion.TargetArtifact,
		RollbackTarget: rollbackTarget,
		Releases:       ReleasePair{From: "target", Previous: links.Previous, Target: "target"}, Direction: direction,
		ObserveLinks: func(context.Context) (ReleaseLinks, error) { return links, nil },
		ActivateLinks: func(context.Context, ReleasePair) (ReleaseLinks, error) {
			t.Fatal("target readiness rotated runtime links")
			return links, nil
		},
		Reconcilers: []V2ActivationReconciler{owner}, SourceMigrationCheckpoint: true,
		NewTransactionID: func() TransactionID { return "tx-target-repair" },
		VerifyAuthorization: func(plan PlanToken, authorization Authorization) bool {
			return authorization == v2TestAuthorization(plan)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return transition, owner, closure
}

func TestRollbackReadinessUsesTargetHistoryAndFreshActivation(t *testing.T) {
	for _, direction := range []Direction{DirectionActivateTarget, DirectionActivatePrevious} {
		t.Run(string(direction), func(t *testing.T) {
			transition, owner, closure := rollbackReadinessFixture(t, true, direction)
			before, err := transition.store.ReadCurrentJournal()
			if err != nil {
				t.Fatal(err)
			}
			inspection, err := transition.Inspect(context.Background(), Goal{Target: "target", Direction: direction})
			if err != nil || inspection.Outcome == nil || inspection.Outcome.Status != StatusReady || inspection.Resume != nil {
				t.Fatalf("target readiness = %+v, %v", inspection, err)
			}
			if owner.observes == 0 || owner.reconciles != 0 {
				t.Fatal("readiness did not freshly observe activation without effects")
			}
			outcome, err := transition.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
			if err != nil || outcome.Status != StatusReady || outcome.Transaction == nil || *outcome.Transaction != closure.Transaction {
				t.Fatalf("ready convergence = %+v, %v", outcome, err)
			}
			after, err := transition.store.ReadCurrentJournal()
			if err != nil || !bytes.Equal(before.Payload, after.Payload) {
				t.Fatalf("readiness rewrote S's completed journal: %v", err)
			}
		})
	}
}

func TestRollbackReadinessRepairsWithAFreshTargetTransaction(t *testing.T) {
	transition, owner, closure := rollbackReadinessFixture(t, false, DirectionActivateTarget)
	inspection, err := transition.Inspect(context.Background(), Goal{Target: "target", Direction: DirectionActivateTarget})
	if err != nil || inspection.Outcome == nil || inspection.Outcome.Status != StatusMigrationRequired || inspection.Resume != nil || inspection.Outcome.Transaction != nil {
		t.Fatalf("target repair assessment = %+v, %v", inspection, err)
	}
	if owner.reconciles != 0 {
		t.Fatal("repair assessment performed effects")
	}
	outcome, err := transition.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
	if err != nil || outcome.Status != StatusReady || owner.reconciles != 1 {
		t.Fatalf("target repair = %+v, %v", outcome, err)
	}
	selected, err := transition.store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	journal, err := ParseJournal(selected.Payload)
	if err != nil || journal.Transaction == closure.Transaction || journal.RegistryDigest != transition.registryDigest || journal.CatalogDigest != transition.catalog.Digest() {
		t.Fatalf("repair did not acquire a fresh target-owned journal: %+v, %v", journal, err)
	}
}

func TestRollbackReadinessRefusesChangedClosure(t *testing.T) {
	transition, owner, closure := rollbackReadinessFixture(t, true, DirectionActivateTarget)
	testkit.WriteFile(t, filepath.Join(transition.store.configHome, "reconcile", "rollback", "v1", "closures", string(closure.Transaction)+".json"), []byte("{}\n"), 0o600)
	if _, err := transition.Inspect(context.Background(), Goal{Target: "target", Direction: DirectionActivateTarget}); err == nil {
		t.Fatal("changed closure established readiness")
	}
	if owner.reconciles != 0 {
		t.Fatal("changed closure performed activation")
	}
}

func TestRollbackJournalCannotAuthorizeForwardNativeRetirement(t *testing.T) {
	for _, complete := range []bool{false, true} {
		store, err := NewPOSIXV2Store(testkit.TempDir(t))
		if err != nil {
			t.Fatal(err)
		}
		journal := validJournal(ReleasePair{From: "source", Target: "target"})
		journal.Goal.Direction = DirectionActivatePrevious
		if complete {
			journal.Checkpoint = JournalComplete
			journal.Steps[0].Checkpoint = StepVerified
			evidence := evidenceFor(journal, journal.Steps[0], EvidenceVerified, journal.Steps[0].Desired, "")
			journal.Steps[0].Evidence = &evidence
		}
		payload, err := MarshalJournal(journal)
		if err != nil {
			t.Fatal(err)
		}
		absent, err := store.ReadCurrentJournal()
		if err != nil {
			t.Fatal(err)
		}
		if err := store.CompareAndSwapCurrentJournal(absent, payload); err != nil {
			t.Fatal(err)
		}
		binding, err := store.CaptureRegisteredYardBinding("yard.hermes")
		if binding.Snapshot.Exists || !complete && err == nil || complete && err != nil {
			t.Fatalf("rollback acquired forward retirement binding: %+v, %v", binding, err)
		}
	}
}

package releasetransition

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestV2CheckpointActivationRepairRetainsAuthorizationAcrossRestart(t *testing.T) {
	ctx := context.Background()
	activeFault := ""
	faultTriggered := false
	transition, _, _ := v2TransitionFixture(t, func(point string) error {
		if activeFault != "" && point == activeFault {
			faultTriggered = true
			return errors.New("interrupt activation-only checkpoint repair")
		}
		return nil
	})
	transition.options.MigrationCheckpoint = true
	transition.options.SourceMigrationCheckpoint = true
	owner := &v2TestReconciler{}
	transition.options.Reconcilers = []V2ActivationReconciler{owner}
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	initial, err := transition.Inspect(ctx, goal)
	if err != nil || len(initial.Blockers) != 0 {
		t.Fatalf("initial checkpoint inspection: %#v err=%v", initial, err)
	}
	completed, err := transition.Converge(ctx, Execution{
		Plan: initial.Plan, Authorization: v2TestAuthorization(initial.Plan),
	})
	if err != nil || completed.Status != StatusReady {
		t.Fatalf("complete checkpoint migration: %#v err=%v", completed, err)
	}
	completedSnapshot, err := transition.store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	predecessor, err := ParseJournal(completedSnapshot.Payload)
	if err != nil || predecessor.Checkpoint != JournalComplete {
		t.Fatalf("invalid completed predecessor: %#v %v", predecessor, err)
	}

	checkpointBefore, err := transition.store.ReadMigrationCheckpoint()
	if err != nil || !checkpointBefore.Exists {
		t.Fatalf("completed checkpoint: %#v err=%v", checkpointBefore, err)
	}
	ledgerBefore, err := transition.store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	owner.converged = false
	transition.options.NewTransactionID = func() TransactionID { return "tx-checkpoint-repair" }
	repair, err := transition.Inspect(ctx, goal)
	if err != nil || repair.Resume != nil || !repair.Assessment.Changed || len(repair.Blockers) != 0 {
		t.Fatalf("fresh activation repair: %#v err=%v", repair, err)
	}
	activeFault = "after-reconciler-0"
	interrupted, err := transition.Converge(ctx, Execution{
		Plan: repair.Plan, Authorization: v2TestAuthorization(repair.Plan),
	})
	if err != nil || interrupted.Status != StatusRecovering || !faultTriggered {
		t.Fatalf("interrupt activation repair: %#v faultTriggered=%t err=%v", interrupted, faultTriggered, err)
	}
	snapshot, err := transition.store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	journal, err := ParseJournal(snapshot.Payload)
	if err != nil || journal.Checkpoint != JournalReconciling || len(journal.Steps) != 0 || journal.Transaction != "tx-checkpoint-repair" {
		t.Fatalf("activation-only journal: %#v err=%v", journal, err)
	}
	options := transition.options
	options.fault = nil
	freshOwner := &v2TestReconciler{converged: true}
	options.Reconcilers = []V2ActivationReconciler{freshOwner}
	options.VerifyAuthorization = func(PlanToken, Authorization) bool {
		t.Fatal("authorized activation resume requested a fresh grant")
		return false
	}
	fresh, err := NewV2Transition(options)
	if err != nil {
		t.Fatal(err)
	}
	resume, err := fresh.Inspect(ctx, goal)
	if err != nil || resume.Resume == nil || *resume.Resume != journal.Transaction || resume.Plan != journal.ResumePlan || len(resume.Blockers) != 0 || resume.Outcome.Status != StatusRecovering {
		t.Fatalf("unchanged checkpoint repair lost durable authorization: %#v err=%v", resume, err)
	}
	freshOwner.desired = digestC
	blocked, err := fresh.Converge(ctx, Execution{Plan: resume.Plan})
	if err != nil || blocked.Code != CodePlanStale || freshOwner.reconciles != 0 {
		t.Fatalf("changed activation inputs bypassed scope guard: %#v err=%v", blocked, err)
	}
	unchanged, err := fresh.store.ReadCurrentJournal()
	if err != nil || !bytes.Equal(snapshot.Payload, unchanged.Payload) {
		t.Fatalf("blocked resume changed authorized journal: %#v err=%v", unchanged, err)
	}
	freshOwner.desired = ""
	settled, err := fresh.Converge(ctx, Execution{Plan: resume.Plan})
	if err != nil || settled.Status != StatusReady || settled.Transaction == nil || *settled.Transaction != journal.Transaction {
		t.Fatalf("resume activation repair without grant: %#v err=%v", settled, err)
	}
	finalSnapshot, err := fresh.store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	final, err := ParseJournal(finalSnapshot.Payload)
	if err != nil || final.Checkpoint != JournalComplete || final.Transaction != journal.Transaction || final.AuthorizationPlan != journal.AuthorizationPlan || final.AuthorizationDigest != journal.AuthorizationDigest || final.ObservationScope != journal.ObservationScope || len(final.Steps) != 0 {
		t.Fatalf("resume replaced activation authorization: %#v err=%v", final, err)
	}
	checkpointAfter, checkpointErr := fresh.store.ReadMigrationCheckpoint()
	ledgerAfter, ledgerErr := fresh.store.ReadLedger()
	if checkpointErr != nil || ledgerErr != nil || !bytes.Equal(checkpointBefore.Payload, checkpointAfter.Payload) || !bytes.Equal(ledgerBefore.Payload, ledgerAfter.Payload) {
		t.Fatalf("activation repair changed checkpoint/history: checkpointErr=%v ledgerErr=%v", checkpointErr, ledgerErr)
	}
}

func TestV2CheckpointConversionRequiresConsentAndCompletesAtReadyBoundary(t *testing.T) {
	for _, faultPoint := range []string{"", "before-checkpoint-publication", "after-checkpoint-publication"} {
		name := faultPoint
		if name == "" {
			name = "normal"
		}
		t.Run(name, func(t *testing.T) {
			activeFault := ""
			transition, _, _ := v2TransitionFixture(t, func(point string) error {
				if activeFault != "" && point == activeFault {
					return errors.New("interrupt checkpoint publication")
				}
				return nil
			})
			transition.options.MigrationCheckpoint = false
			goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
			legacyPlan, err := transition.Inspect(context.Background(), goal)
			if err != nil {
				t.Fatal(err)
			}
			if outcome, err := transition.Converge(context.Background(), Execution{
				Plan: legacyPlan.Plan, Authorization: v2TestAuthorization(legacyPlan.Plan),
			}); err != nil || outcome.Status != StatusReady {
				t.Fatalf("legacy migration = %#v, err=%v", outcome, err)
			}
			projectionBefore, err := transition.store.ReadLedger()
			if err != nil || !projectionBefore.Exists {
				t.Fatalf("legacy projection before conversion = %#v, err=%v", projectionBefore, err)
			}
			transition.options.MigrationCheckpoint = true
			conversionPlan, err := transition.Inspect(context.Background(), goal)
			if err != nil || len(conversionPlan.Blockers) != 0 || !conversionPlan.Assessment.Changed ||
				conversionPlan.Outcome == nil || conversionPlan.Outcome.Status != StatusMigrationRequired {
				t.Fatalf("conversion assessment = %#v, err=%v", conversionPlan, err)
			}
			foundDecision := false
			for _, decision := range conversionPlan.Decisions {
				foundDecision = foundDecision || (decision.Resource == "migration-history.checkpoint" &&
					decision.Result == "compact-migration-history")
			}
			if !foundDecision {
				t.Fatal("checkpoint conversion was not visible in the assessed decisions")
			}
			denied, err := transition.Converge(context.Background(), Execution{Plan: conversionPlan.Plan})
			if err != nil || denied.Code != CodeConfirmationRequired {
				t.Fatalf("conversion without authorization = %#v, err=%v", denied, err)
			}
			missing, err := transition.store.ReadMigrationCheckpoint()
			if err != nil || missing.Exists {
				t.Fatalf("checkpoint published without consent: %#v, err=%v", missing, err)
			}
			activeFault = faultPoint
			outcome, err := transition.Converge(context.Background(), Execution{
				Plan: conversionPlan.Plan, Authorization: v2TestAuthorization(conversionPlan.Plan),
			})
			if faultPoint == "before-checkpoint-publication" {
				if err != nil || outcome.Status != StatusRecovering || outcome.Transaction == nil {
					t.Fatalf("pre-publication interruption = %#v, err=%v", outcome, err)
				}
				activeFault = ""
				transition, err = NewV2Transition(transition.options)
				if err != nil {
					t.Fatal(err)
				}
				resume, inspectErr := transition.Inspect(context.Background(), goal)
				if inspectErr != nil || len(resume.Blockers) != 0 || resume.Resume == nil ||
					*resume.Resume != *outcome.Transaction || resume.Plan == "" ||
					resume.Outcome == nil || resume.Outcome.Status != StatusRecovering {
					t.Fatalf("checkpoint publication resume inspection = %#v, err=%v", resume, inspectErr)
				}
				outcome, err = transition.Converge(context.Background(), Execution{
					Plan: resume.Plan,
				})
			}
			// A process loss after the atomic authority switch already leaves
			// the reducer observing ready state on the new authority.
			if err != nil || outcome.Status != StatusReady {
				t.Fatalf("checkpoint conversion = %#v, err=%v", outcome, err)
			}
			checkpoint, err := transition.store.ReadMigrationCheckpoint()
			if err != nil || !checkpoint.Exists {
				t.Fatalf("checkpoint was not published after authorization: %#v, err=%v", checkpoint, err)
			}
			projectionAfter, err := transition.store.ReadLedger()
			if err != nil || !bytes.Equal(projectionAfter.Payload, projectionBefore.Payload) {
				t.Fatalf("conversion changed the retained legacy projection: %#v, err=%v", projectionAfter, err)
			}
			repeat, err := transition.Inspect(context.Background(), goal)
			if err != nil || repeat.Assessment.Changed || repeat.Outcome == nil || repeat.Outcome.Status != StatusReady {
				t.Fatalf("checkpoint fixed point = %#v, err=%v", repeat, err)
			}
		})
	}
}

func TestV2MigrationAndCheckpointPublicationShareOneConsent(t *testing.T) {
	for _, faultPoint := range []string{"", "before-checkpoint-publication", "after-checkpoint-publication"} {
		name := faultPoint
		if name == "" {
			name = "normal"
		}
		t.Run(name, func(t *testing.T) {
			activeFault := ""
			transition, _, _ := v2TransitionFixture(t, func(point string) error {
				if activeFault != "" && point == activeFault {
					return errors.New("interrupt marker migration")
				}
				return nil
			})
			transition.options.MigrationCheckpoint = true
			transition.options.SourceMigrationCheckpoint = true
			goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
			inspection, err := transition.Inspect(context.Background(), goal)
			if err != nil || !inspection.Assessment.Changed || len(inspection.Blockers) != 0 {
				t.Fatalf("checkpoint-enabled migration inspection = %#v, err=%v", inspection, err)
			}
			if _, err := transition.Converge(context.Background(), Execution{Plan: inspection.Plan}); err != nil {
				t.Fatal(err)
			}
			activeFault = faultPoint
			outcome, err := transition.Converge(context.Background(), Execution{
				Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
			})
			if faultPoint == "before-checkpoint-publication" {
				if err != nil || outcome.Status != StatusRecovering || outcome.Transaction == nil {
					t.Fatalf("pre-publication interruption = %#v, err=%v", outcome, err)
				}
				activeFault = ""
				outcome, err = transition.Converge(context.Background(), Execution{
					Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
				})
			} else if faultPoint == "after-checkpoint-publication" {
				activeFault = ""
			}
			if err != nil || outcome.Status != StatusReady {
				t.Fatalf("checkpoint-enabled migration = %#v, err=%v", outcome, err)
			}
			checkpoint, err := transition.store.ReadMigrationCheckpoint()
			if err != nil || !checkpoint.Exists {
				t.Fatalf("checkpoint missing after the original consent: %#v, err=%v", checkpoint, err)
			}
			legacy, err := transition.store.ReadLedger()
			if err != nil || !legacy.Exists {
				t.Fatalf("compatible legacy ledger projection missing: %#v, err=%v", legacy, err)
			}
		})
	}
}

package releasetransition

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

type nativeRecoveryFixture struct {
	v2TestReconciler
	unsafe bool
}

func (reconciler *nativeRecoveryFixture) PrepareActivationRecovery(context.Context, ReleasePair, ReleaseLinks) (Fingerprint, error) {
	if reconciler.unsafe {
		return "", invalid("unknown partial activation state")
	}
	return reconciler.drift, nil
}

func interruptedActivationRecoveryFixture(t *testing.T) (*V2Transition, *nativeRecoveryFixture, ActivationOnlyRecoveryRequest, ProtectedSnapshot, ProtectedSnapshot) {
	t.Helper()
	ctx := context.Background()
	transition, _, _ := v2TransitionFixture(t, nil)
	reconciler := &nativeRecoveryFixture{v2TestReconciler: v2TestReconciler{drift: digestB}}
	transition.options.Reconcilers = []V2ActivationReconciler{reconciler}
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	initial, err := transition.Inspect(ctx, goal)
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := transition.Converge(ctx, Execution{Plan: initial.Plan, Authorization: v2TestAuthorization(initial.Plan)}); err != nil || outcome.Status != StatusReady {
		t.Fatalf("initial convergence: %#v %v", outcome, err)
	}
	transition.options.NewTransactionID = func() TransactionID { return "tx-test-002" }
	reconciler.converged = false
	transition.options.fault = func(point string) error {
		if point == "after-reconciling" {
			return errors.New("interruption")
		}
		return nil
	}
	initial, err = transition.Inspect(ctx, goal)
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := transition.Converge(ctx, Execution{Plan: initial.Plan, Authorization: v2TestAuthorization(initial.Plan)}); err != nil || outcome.Status != StatusRecovering {
		t.Fatalf("interruption: %#v %v", outcome, err)
	}
	transition.options.fault = nil
	journal, err := transition.store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	before, err := ParseJournal(journal.Payload)
	if err != nil || len(before.Steps) != 0 || before.Checkpoint != JournalReconciling {
		t.Fatalf("predecessor: %#v %v", before, err)
	}
	ledger, err := transition.store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	reconciler.desired = digestC
	return transition, reconciler, ActivationOnlyRecoveryRequest{Transaction: before.Transaction, Fingerprint: journal.Fingerprint}, journal, ledger
}

func TestActivationRecoveryFreshGrantAndImmutablePredecessor(t *testing.T) {
	ctx := context.Background()
	transition, _, request, before, ledger := interruptedActivationRecoveryFixture(t)
	inspection, err := transition.InspectActivationRecovery(ctx, request)
	if err != nil || inspection.Resume != nil || len(inspection.Blockers) != 0 || !inspection.Assessment.Changed {
		t.Fatalf("fresh assessment: %#v %v", inspection, err)
	}
	original, _ := ParseJournal(before.Payload)
	for _, execution := range []Execution{
		{Plan: inspection.Plan},
		{Plan: inspection.Plan, Authorization: v2TestAuthorization(original.AuthorizationPlan)},
		{Plan: original.ResumePlan, Authorization: v2TestAuthorization(inspection.Plan)},
	} {
		outcome, err := transition.ConvergeActivationRecovery(ctx, request, execution)
		if err != nil || outcome.Status != StatusOperatorActionRequired {
			t.Fatalf("old/missing grant: %#v %v", outcome, err)
		}
		current, _ := transition.store.ReadCurrentJournal()
		if !bytes.Equal(current.Payload, before.Payload) {
			t.Fatal("rejected grant replaced the predecessor")
		}
	}
	outcome, err := transition.ConvergeActivationRecovery(ctx, request, Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
	if err != nil || outcome.Status != StatusReady || outcome.Transaction == nil || *outcome.Transaction == request.Transaction {
		t.Fatalf("fresh convergence: %#v %v", outcome, err)
	}
	current, _ := transition.store.ReadCurrentJournal()
	after, _ := ParseJournal(current.Payload)
	if after.Goal != original.Goal || after.Releases.From != after.Releases.Target || len(after.Steps) != 0 {
		t.Fatalf("replacement widened scope: %#v", after)
	}
	unchanged, _ := transition.store.ReadLedger()
	if !bytes.Equal(ledger.Payload, unchanged.Payload) {
		t.Fatal("recovery changed completed migrations")
	}
	receiptSnapshot, err := transition.store.ReadRecoveryReceipt(after.Transaction)
	if err != nil || !receiptSnapshot.Exists {
		t.Fatalf("missing predecessor evidence: %v", err)
	}
	receipt, err := ParseRecoveryReceipt(receiptSnapshot.Payload)
	preserved, _ := MarshalJournal(receipt.Predecessor)
	if err != nil || !bytes.Equal(before.Payload, preserved) {
		t.Fatalf("predecessor changed: %v", err)
	}
	if err := transition.store.ValidateCurrentRecovery(after); err != nil {
		t.Fatal(err)
	}
}

func TestActivationRecoveryDurableBoundaryRetry(t *testing.T) {
	for _, point := range []string{"before-recovery-receipt", "after-recovery-receipt", "before-recovery-journal-cas", "after-recovery-journal-cas"} {
		t.Run(point, func(t *testing.T) {
			ctx := context.Background()
			transition, _, request, before, ledger := interruptedActivationRecoveryFixture(t)
			inspection, err := transition.InspectActivationRecovery(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			transition.options.fault = func(actual string) error {
				if actual == point {
					return errors.New("interruption")
				}
				return nil
			}
			outcome, interruptedErr := transition.ConvergeActivationRecovery(ctx, request, Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
			transition.options.fault = nil
			current, _ := transition.store.ReadCurrentJournal()
			if point != "after-recovery-journal-cas" {
				if interruptedErr == nil || !bytes.Equal(current.Payload, before.Payload) {
					t.Fatalf("pre-CAS failure lost original: %#v %v", outcome, interruptedErr)
				}
				// A restarted process receives a different ephemeral grant for
				// the same assessment; durable intent must remain byte-identical.
				newGrant := Authorization("new-grant." + string(inspection.Plan))
				transition.options.VerifyAuthorization = func(plan PlanToken, grant Authorization) bool {
					return plan == inspection.Plan && grant == newGrant
				}
				outcome, err = transition.ConvergeActivationRecovery(ctx, request, Execution{Plan: inspection.Plan, Authorization: newGrant})
			} else {
				if interruptedErr != nil || outcome.Status != StatusRecovering {
					t.Fatalf("post-CAS checkpoint: %#v %v", outcome, interruptedErr)
				}
				journal, _ := ParseJournal(current.Payload)
				options := transition.options
				options.Releases = journal.Releases
				resumed, newErr := NewV2Transition(options)
				if newErr != nil {
					t.Fatal(newErr)
				}
				outcome, err = resumed.Converge(ctx, Execution{Plan: journal.ResumePlan})
			}
			if err != nil || outcome.Status != StatusReady {
				t.Fatalf("boundary retry: %#v %v", outcome, err)
			}
			unchanged, _ := transition.store.ReadLedger()
			if !bytes.Equal(ledger.Payload, unchanged.Payload) {
				t.Fatal("boundary retry changed ledger")
			}
		})
	}
}

func TestActivationRecoveryPublicationRechecksDesiredAndConcurrentJournal(t *testing.T) {
	for _, race := range []string{"desired", "journal"} {
		t.Run(race, func(t *testing.T) {
			ctx := context.Background()
			transition, reconciler, request, before, ledger := interruptedActivationRecoveryFixture(t)
			inspection, err := transition.InspectActivationRecovery(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			wantCurrent := before.Payload
			transition.options.fault = func(point string) error {
				if point != "before-recovery-journal-cas" {
					return nil
				}
				if race == "desired" {
					reconciler.desired = digestA
					return nil
				}
				competing, err := ParseJournal(before.Payload)
				if err != nil {
					return err
				}
				competing.Transaction = "tx-concurrent"
				wantCurrent, err = MarshalJournal(competing)
				if err != nil {
					return err
				}
				return transition.store.CompareAndSwapCurrentJournal(before, wantCurrent)
			}
			outcome, err := transition.ConvergeActivationRecovery(ctx, request,
				Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
			if err != nil || outcome.Code != CodePlanStale || outcome.Status != StatusOperatorActionRequired {
				t.Fatalf("publication race admitted replacement: %#v %v", outcome, err)
			}
			current, err := transition.store.ReadCurrentJournal()
			if err != nil || !bytes.Equal(current.Payload, wantCurrent) {
				t.Fatalf("publication race overwrote current journal: %v", err)
			}
			evidence, err := transition.store.ReadRecoveryReceipt("recovery-v1-tx-test-002")
			if err != nil || !evidence.Exists {
				t.Fatalf("publication race lost immutable evidence: %v", err)
			}
			receipt, err := ParseRecoveryReceipt(evidence.Payload)
			preserved, _ := MarshalJournal(receipt.Predecessor)
			if err != nil || !bytes.Equal(preserved, before.Payload) {
				t.Fatalf("publication race changed predecessor evidence: %v", err)
			}
			unchanged, err := transition.store.ReadLedger()
			if err != nil || !bytes.Equal(unchanged.Payload, ledger.Payload) {
				t.Fatalf("publication race changed completed ledger: %v", err)
			}
		})
	}
}

func TestActivationRecoveryRejectsStaleAndUnknownNativeState(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"stale desired", "unknown partial", "no native plan", "foreign fingerprint", "settings step", "links"} {
		t.Run(kind, func(t *testing.T) {
			transition, reconciler, request, before, _ := interruptedActivationRecoveryFixture(t)
			inspection, err := transition.InspectActivationRecovery(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "stale desired":
				reconciler.desired = digestA
			case "unknown partial":
				reconciler.unsafe = true
			case "no native plan":
				transition.options.Reconcilers = []V2ActivationReconciler{&reconciler.v2TestReconciler}
			case "foreign fingerprint":
				request.Fingerprint = digestA
			case "links":
				transition.options.ObserveLinks = func(context.Context) (ReleaseLinks, error) { return ReleaseLinks{Active: "foreign"}, nil }
			case "settings step":
				// A journal with even verified migration steps never gets generic replacement.
				transition, _, _ = v2TransitionFixture(t, func(point string) error {
					if point == "after-reconciling" {
						return errors.New("interruption")
					}
					return nil
				})
				transition.options.Reconcilers = []V2ActivationReconciler{reconciler}
				initial, _ := transition.Inspect(ctx, Goal{Target: "release-a", Direction: DirectionActivateTarget})
				_, _ = transition.Converge(ctx, Execution{Plan: initial.Plan, Authorization: v2TestAuthorization(initial.Plan)})
				transition.options.fault = nil
				before, _ = transition.store.ReadCurrentJournal()
				journal, _ := ParseJournal(before.Payload)
				request = ActivationOnlyRecoveryRequest{Transaction: journal.Transaction, Fingerprint: before.Fingerprint}
			}
			outcome, err := transition.ConvergeActivationRecovery(ctx, request, Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
			if err != nil || outcome.Status != StatusOperatorActionRequired {
				t.Fatalf("unsafe recovery %s: %#v %v", kind, outcome, err)
			}
			current, _ := transition.store.ReadCurrentJournal()
			if !bytes.Equal(current.Payload, before.Payload) {
				t.Fatal("unsafe recovery mutated journal")
			}
		})
	}
}

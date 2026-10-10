package releasetransition

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type activationLifecycleFixture struct {
	transition              *V2Transition
	reconciler              *nativeRecoveryFixture
	request                 ActivationOnlyRecoveryRequest
	before, ledger, receipt ProtectedSnapshot
	initial                 Inspection
	links                   ReleaseLinks
	reserved                RecoveryReceiptV1
}

func TestActivationRecoveryLifecycleRepeatedPreCASDesiredCycle(t *testing.T) {
	fixture := interruptedLifecycleReservation(t, false)
	interruption := errors.New("stop after receipt before journal CAS")
	fixture.transition.options.fault = func(point string) error {
		if point == "after-recovery-receipt" {
			return interruption
		}
		return nil
	}
	// The original protected journal/ledger/native actual state never changes.
	// Returning to B must select its latest generation rather than an older
	// cancellation whose desired base happens to be the same.
	desired := []Fingerprint{digestD, Fingerprint(strings.Repeat("e", 64)), digestD}
	for index, value := range desired {
		fixture.reconciler.desired = value
		fixture.transition.options.NewTransactionID = func() TransactionID {
			return TransactionID(fmt.Sprintf("cycle-%d", index))
		}
		inspection, err := fixture.transition.InspectActivationRecovery(context.Background(), fixture.request)
		if err != nil || len(inspection.Blockers) != 0 {
			t.Fatalf("cycle %d inspection: %#v %v", index, inspection, err)
		}
		outcome, err := fixture.transition.ConvergeActivationRecovery(context.Background(), fixture.request,
			Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(fixture.initial.Plan)})
		if err != nil || outcome.Code != CodeConfirmationRequired {
			t.Fatalf("old grant accepted cycle %d: %#v %v", index, outcome, err)
		}
		_, err = fixture.transition.ConvergeActivationRecovery(context.Background(), fixture.request,
			Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
		if !errors.Is(err, interruption) {
			t.Fatalf("cycle %d did not publish its fresh receipt: %v", index, err)
		}
		fixture.assertState(t, fixture.before)
		frontier, present, err := fixture.transition.store.recoveryFrontier(fixture.request)
		if err != nil || !present || frontier.Generation != uint64(index+2) {
			t.Fatalf("cycle %d frontier: %#v %v", index, frontier, err)
		}
	}
	fixture.transition.options.fault = nil
	restarted, err := NewV2Transition(fixture.transition.options)
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := restarted.InspectActivationRecovery(context.Background(), fixture.request)
	if err != nil || len(inspection.Blockers) != 0 {
		t.Fatalf("final inspection: %#v %v", inspection, err)
	}
	outcome, err := restarted.ConvergeActivationRecovery(context.Background(), fixture.request,
		Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
	if err != nil || outcome.Status != StatusReady {
		t.Fatalf("B -> C -> B did not reach ready: %#v %v", outcome, err)
	}
}

func TestActivationRecoveryLifecycleChangedInputsAfterCancellationCrash(t *testing.T) {
	for _, point := range []string{"after-recovery-cancellation", "before-recovery-frontier", "after-recovery-frontier", "after-recovery-lifecycle-unlink"} {
		t.Run(point, func(t *testing.T) {
			fixture := interruptedLifecycleReservation(t, false)
			second := fixture.changedPlan(t)
			interruption := errors.New("interrupted durable cancellation")
			fixture.transition.store.fault = func(actual string) error {
				if actual == point {
					return interruption
				}
				return nil
			}
			_, err := fixture.transition.ConvergeActivationRecovery(context.Background(), fixture.request,
				Execution{Plan: second.Plan, Authorization: v2TestAuthorization(second.Plan)})
			if !errors.Is(err, interruption) {
				t.Fatalf("cancellation crash: %v", err)
			}
			fixture.transition.store.fault = nil
			fixture.reconciler.desired = Fingerprint(strings.Repeat("e", 64))
			restarted, err := NewV2Transition(fixture.transition.options)
			if err != nil {
				t.Fatal(err)
			}
			fixture.transition = restarted
			before, err := restarted.store.ReadRecoveryReceipt(fixture.reserved.Successor.Transaction)
			if err != nil {
				t.Fatal(err)
			}
			third, err := restarted.InspectActivationRecovery(context.Background(), fixture.request)
			if err != nil || len(third.Blockers) != 0 || third.Plan == second.Plan {
				t.Fatalf("third inputs were not assessed: %#v %v", third, err)
			}
			after, err := restarted.store.ReadRecoveryReceipt(fixture.reserved.Successor.Transaction)
			if err != nil || !sameProtectedSnapshot(before, after) {
				t.Fatal("read-only assessment mutated cancellation")
			}
			for _, authorization := range []Authorization{"", v2TestAuthorization(second.Plan)} {
				outcome, err := restarted.ConvergeActivationRecovery(context.Background(), fixture.request,
					Execution{Plan: third.Plan, Authorization: authorization})
				if err != nil || outcome.Code != CodeConfirmationRequired {
					t.Fatalf("third inputs used old/missing grant: %#v %v", outcome, err)
				}
				fixture.assertState(t, fixture.before)
			}
			outcome, err := restarted.ConvergeActivationRecovery(context.Background(), fixture.request,
				Execution{Plan: third.Plan, Authorization: v2TestAuthorization(third.Plan)})
			if err != nil || outcome.Status != StatusReady {
				t.Fatalf("third fresh grant did not finish durable cancellation: %#v %v", outcome, err)
			}
		})
	}
}

func TestActivationRecoveryLifecyclePendingFrontierReplaysBeforeThirdInputs(t *testing.T) {
	fixture := interruptedLifecycleReservation(t, false)
	second := fixture.changedPlan(t)
	interruption := errors.New("interrupted pending frontier fsync")
	cancelling := false
	fixture.transition.store.fault = func(point string) error {
		if point == "after-recovery-cancellation" {
			cancelling = true
		}
		if cancelling && point == "after-pending-fsync" {
			return interruption
		}
		return nil
	}
	_, err := fixture.transition.ConvergeActivationRecovery(context.Background(), fixture.request,
		Execution{Plan: second.Plan, Authorization: v2TestAuthorization(second.Plan)})
	if !errors.Is(err, interruption) {
		t.Fatalf("pending frontier fault: %v", err)
	}
	fixture.transition.store.fault = nil
	fixture.reconciler.desired = Fingerprint(strings.Repeat("e", 64))
	restarted, err := NewV2Transition(fixture.transition.options)
	if err != nil {
		t.Fatal(err)
	}
	third, err := restarted.InspectActivationRecovery(context.Background(), fixture.request)
	if err != nil || len(third.Blockers) != 0 {
		t.Fatalf("pending frontier third-input inspection: %#v %v", third, err)
	}
	outcome, err := restarted.ConvergeActivationRecovery(context.Background(), fixture.request,
		Execution{Plan: third.Plan, Authorization: v2TestAuthorization(third.Plan)})
	if err != nil || outcome.Status != StatusReady {
		t.Fatalf("pending frontier did not replay before third inputs: %#v %v", outcome, err)
	}
	frontier, published, pending, err := restarted.store.readFrontierPair(fixture.request.Transaction)
	if err != nil || !published.Exists || pending.Exists || frontier.Generation != 2 || frontier.Cancellation.Cancellation.Plan != second.Plan {
		t.Fatalf("durable cancellation intent was replaced: %#v %v", frontier, err)
	}
}

// Interrupt the real publication path with an immutable receipt durable and
// the predecessor still selected. The pending variant also prepares its CAS.
func interruptedLifecycleReservation(t *testing.T, pending bool) activationLifecycleFixture {
	t.Helper()
	ctx := context.Background()
	transition, reconciler, request, before, ledger := interruptedActivationRecoveryFixture(t)
	transition.options.RecoveryContract = ActivationOnlyRecoveryContractV2
	initial, err := transition.InspectActivationRecovery(ctx, request)
	if err != nil || len(initial.Blockers) != 0 {
		t.Fatalf("initial lifecycle plan: %#v %v", initial, err)
	}
	interruption := errors.New("interrupted lifecycle publication")
	transition.options.fault = func(point string) error {
		if !pending && point == "after-recovery-receipt" {
			return interruption
		}
		if pending && point == "before-recovery-journal-cas" {
			transition.store.fault = func(point string) error {
				if point == "after-pending-fsync" {
					return interruption
				}
				return nil
			}
		}
		return nil
	}
	_, err = transition.ConvergeActivationRecovery(ctx, request, Execution{Plan: initial.Plan, Authorization: v2TestAuthorization(initial.Plan)})
	transition.options.fault, transition.store.fault = nil, nil
	if !errors.Is(err, interruption) {
		t.Fatalf("publication interruption: %v", err)
	}
	receipt, err := transition.store.ReadRecoveryReceipt(lifecycleTransaction(t, request, 1))
	if err != nil || !receipt.Exists {
		t.Fatalf("live reservation: %#v %v", receipt, err)
	}
	reserved, err := ParseRecoveryReceipt(receipt.Payload)
	if err != nil || reserved.Contract != ActivationOnlyRecoveryContractV2 || reserved.SchemaVersion != RecoveryReceiptSchemaV2 {
		t.Fatalf("V2 receipt: %#v %v", reserved, err)
	}
	preserved, err := MarshalJournal(reserved.Predecessor)
	if err != nil || !bytes.Equal(preserved, before.Payload) {
		t.Fatalf("receipt changed the exact predecessor: %v", err)
	}
	links, err := transition.options.ObserveLinks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fixture := activationLifecycleFixture{transition, reconciler, request, before, ledger, receipt, initial, links, reserved}
	fixture.assertState(t, before)
	return fixture
}

func (fixture activationLifecycleFixture) assertState(t *testing.T, journal ProtectedSnapshot) {
	t.Helper()
	current, err := fixture.transition.store.ReadCurrentJournal()
	if err != nil || !sameProtectedSnapshot(current, journal) {
		t.Fatalf("selected journal changed: %v", err)
	}
	ledger, err := fixture.transition.store.ReadLedger()
	if err != nil || !sameProtectedSnapshot(ledger, fixture.ledger) {
		t.Fatalf("completed ledger changed: %v", err)
	}
	links, err := fixture.transition.options.ObserveLinks(context.Background())
	if err != nil || !reflect.DeepEqual(links, fixture.links) {
		t.Fatalf("runtime links changed: %#v %v", links, err)
	}
}

func (fixture activationLifecycleFixture) changedPlan(t *testing.T) Inspection {
	t.Helper()
	fixture.reconciler.desired = digestD
	fixture.transition.options.NewTransactionID = func() TransactionID { return "tx-test-003" }
	inspection, err := fixture.transition.InspectActivationRecovery(context.Background(), fixture.request)
	if err != nil || len(inspection.Blockers) != 0 || inspection.Plan == fixture.initial.Plan || !inspection.Assessment.Changed {
		t.Fatalf("changed lifecycle plan: %#v %v", inspection, err)
	}
	return inspection
}

func (fixture activationLifecycleFixture) assertCancelled(t *testing.T, plan PlanToken) {
	t.Helper()
	record, present, err := fixture.transition.store.readLifecycleRecord(fixture.reserved.Successor.Transaction)
	if err != nil || !present || record.Cancellation == nil || record.Cancellation.Plan != plan {
		t.Fatalf("authorized cancellation archive: %#v present=%t %v", record, present, err)
	}
	preserved, err := MarshalRecoveryReceipt(record.Receipt)
	if err != nil || !bytes.Equal(preserved, fixture.receipt.Payload) {
		t.Fatalf("cancellation altered immutable receipt: %v", err)
	}
	live, err := fixture.transition.store.ReadRecoveryReceipt(fixture.reserved.Successor.Transaction)
	if err != nil || live.Exists {
		t.Fatalf("cancelled receipt remains live: %#v %v", live, err)
	}
}

func TestActivationRecoveryLifecycleChangedReservationNeedsFreshGrant(t *testing.T) {
	ctx := context.Background()
	fixture := interruptedLifecycleReservation(t, false)
	inspection := fixture.changedPlan(t)
	for _, test := range []struct {
		execution Execution
		code      OutcomeCode
	}{
		{Execution{Plan: fixture.initial.Plan, Authorization: v2TestAuthorization(fixture.initial.Plan)}, CodePlanStale},
		{Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(fixture.initial.Plan)}, CodeConfirmationRequired},
		{Execution{Plan: inspection.Plan}, CodeConfirmationRequired},
	} {
		outcome, err := fixture.transition.ConvergeActivationRecovery(ctx, fixture.request, test.execution)
		if err != nil || outcome.Status != StatusOperatorActionRequired || outcome.Code != test.code {
			t.Fatalf("stale/missing grant: %#v %v", outcome, err)
		}
		fixture.assertState(t, fixture.before)
		live, err := fixture.transition.store.ReadRecoveryReceipt(fixture.reserved.Successor.Transaction)
		if err != nil || !sameProtectedSnapshot(live, fixture.receipt) {
			t.Fatalf("rejected grant changed receipt: %v", err)
		}
		if _, present, err := fixture.transition.store.readLifecycleRecord(fixture.reserved.Successor.Transaction); err != nil || present {
			t.Fatalf("rejected grant cancelled reservation: present=%t %v", present, err)
		}
	}
	outcome, err := fixture.transition.ConvergeActivationRecovery(ctx, fixture.request, Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
	if err != nil || outcome.Status != StatusReady || outcome.Transaction == nil || *outcome.Transaction != lifecycleTransaction(t, fixture.request, 2) {
		t.Fatalf("fresh authorized reassessment: %#v %v", outcome, err)
	}
	fixture.assertCancelled(t, inspection.Plan)
	current, err := fixture.transition.store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	journal, err := ParseJournal(current.Payload)
	if err != nil || journal.Checkpoint != JournalComplete || len(journal.Steps) != 0 {
		t.Fatalf("completed successor: %#v %v", journal, err)
	}
	fixture.assertState(t, current)
}

func TestActivationRecoveryLifecycleCancellationBoundaryRetry(t *testing.T) {
	for _, test := range []struct {
		point   string
		pending bool
	}{
		{"before-recovery-cancellation", false},
		{"after-recovery-cancellation", false},
		{"after-recovery-lifecycle-unlink", false},
		{"after-recovery-lifecycle-unlink", true},
	} {
		name := test.point
		if test.pending {
			name += "/pending-journal"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			fixture := interruptedLifecycleReservation(t, test.pending)
			inspection := fixture.changedPlan(t)
			interruption := errors.New("interrupted cancellation")
			fixture.transition.store.fault = func(point string) error {
				if point == test.point {
					return interruption
				}
				return nil
			}
			execution := Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)}
			_, err := fixture.transition.ConvergeActivationRecovery(ctx, fixture.request, execution)
			fixture.transition.store.fault = nil
			if !errors.Is(err, interruption) {
				t.Fatalf("cancellation boundary was not reached: %v", err)
			}
			fixture.assertState(t, fixture.before)
			// A new process must see the same grant binding after its own durable
			// cancellation writes, including removal of an unselected pending CAS.
			restarted, err := NewV2Transition(fixture.transition.options)
			if err != nil {
				t.Fatal(err)
			}
			fixture.transition = restarted
			again, err := restarted.InspectActivationRecovery(ctx, fixture.request)
			if err != nil || len(again.Blockers) != 0 || again.Plan != inspection.Plan {
				t.Fatalf("cancellation changed authorization plan: %#v %v", again, err)
			}
			restarted.options.fault = func(point string) error {
				if point == "after-recovery-receipt" {
					return interruption
				}
				return nil
			}
			_, err = restarted.ConvergeActivationRecovery(ctx, fixture.request, execution)
			restarted.options.fault = nil
			if !errors.Is(err, interruption) {
				t.Fatalf("replacement publication retry: %v", err)
			}
			fixture.assertState(t, fixture.before)
			fixture.assertCancelled(t, inspection.Plan)
			again, err = restarted.InspectActivationRecovery(ctx, fixture.request)
			if err != nil || len(again.Blockers) != 0 || again.Plan != inspection.Plan {
				t.Fatalf("own receipt write changed authorization plan: %#v %v", again, err)
			}
			options := restarted.options
			options.NewTransactionID = func() TransactionID { return "tx-test-004" }
			restarted, err = NewV2Transition(options)
			if err != nil {
				t.Fatal(err)
			}
			fixture.transition = restarted
			outcome, err := restarted.ConvergeActivationRecovery(ctx, fixture.request, execution)
			if err != nil || outcome.Status != StatusReady || outcome.Transaction == nil || *outcome.Transaction != lifecycleTransaction(t, fixture.request, 2) {
				t.Fatalf("exact authorized durable retry: %#v %v", outcome, err)
			}
			current, err := restarted.store.ReadCurrentJournal()
			if err != nil {
				t.Fatal(err)
			}
			fixture.assertState(t, current)
		})
	}
}

func TestActivationRecoveryLifecycleSelectedSuccessorRejectsReplacementChains(t *testing.T) {
	ctx := context.Background()
	fixture := interruptedLifecycleReservation(t, false)
	fixture.transition.options.fault = func(point string) error {
		if point == "after-recovery-journal-cas" {
			return errors.New("interrupted selected successor")
		}
		return nil
	}
	outcome, err := fixture.transition.ConvergeActivationRecovery(ctx, fixture.request, Execution{Plan: fixture.initial.Plan, Authorization: v2TestAuthorization(fixture.initial.Plan)})
	fixture.transition.options.fault = nil
	if err != nil || outcome.Status != StatusRecovering {
		t.Fatalf("selected successor: %#v %v", outcome, err)
	}
	selected, err := fixture.transition.store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	journal, err := ParseJournal(selected.Payload)
	if err != nil {
		t.Fatal(err)
	}
	reconciles := fixture.reconciler.reconciles
	fixture.reconciler.desired = digestA
	request := ActivationOnlyRecoveryRequest{Transaction: journal.Transaction, Fingerprint: selected.Fingerprint}
	inspection, err := fixture.transition.InspectActivationRecovery(ctx, request)
	if err != nil || len(inspection.Blockers) == 0 || inspection.Outcome == nil || inspection.Outcome.Code != CodeRecoveryAmbiguous {
		t.Fatalf("selected successor admitted fresh replacement: %#v %v", inspection, err)
	}
	outcome, err = fixture.transition.ConvergeActivationRecovery(ctx, request, Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
	if err != nil || outcome.Status != StatusOperatorActionRequired || outcome.Code != CodeRecoveryAmbiguous {
		t.Fatalf("replacement chain applied: %#v %v", outcome, err)
	}
	if fixture.reconciler.reconciles != reconciles {
		t.Fatal("rejected replacement chain applied activation work")
	}
	fixture.assertState(t, selected)
	live, err := fixture.transition.store.ReadRecoveryReceipt(journal.Transaction)
	if err != nil || !sameProtectedSnapshot(live, fixture.receipt) {
		t.Fatalf("selected receipt changed: %v", err)
	}
	if _, present, err := fixture.transition.store.readLifecycleRecord(journal.Transaction); err != nil || present {
		t.Fatalf("selected successor was cancelled: present=%t %v", present, err)
	}
}

func TestActivationRecoveryLifecycleCompletionCleanupFailureKeepsReady(t *testing.T) {
	ctx := context.Background()
	fixture := interruptedLifecycleReservation(t, false)
	var completed ProtectedSnapshot
	interruption := errors.New("interrupted completion cleanup")
	fixture.transition.store.fault = func(point string) error {
		if point != "after-recovery-completion" {
			return nil
		}
		var err error
		completed, err = fixture.transition.store.ReadCurrentJournal()
		if err != nil {
			return err
		}
		return interruption
	}
	outcome, err := fixture.transition.ConvergeActivationRecovery(ctx, fixture.request, Execution{Plan: fixture.initial.Plan, Authorization: v2TestAuthorization(fixture.initial.Plan)})
	fixture.transition.store.fault = nil
	if err != nil || outcome.Status != StatusReady || len(outcome.Warnings) != 1 || outcome.Warnings[0] != "recovery cleanup is pending" || !completed.Exists {
		t.Fatalf("completion cleanup failure lost readiness: %#v %v", outcome, err)
	}
	journal, err := ParseJournal(completed.Payload)
	if err != nil || journal.Checkpoint != JournalComplete {
		t.Fatalf("completion fault preceded readiness: %#v %v", journal, err)
	}
	fixture.assertState(t, completed)
	live, err := fixture.transition.store.ReadRecoveryReceipt(journal.Transaction)
	if err != nil || !sameProtectedSnapshot(live, fixture.receipt) {
		t.Fatalf("cleanup fault lost live evidence: %v", err)
	}
	record, present, err := fixture.transition.store.readLifecycleRecord(journal.Transaction)
	if err != nil || !present || record.Completed == nil || record.Cancellation != nil {
		t.Fatalf("terminal completion evidence: %#v present=%t %v", record, present, err)
	}
	outcome, err = fixture.transition.Converge(ctx, Execution{Plan: journal.ResumePlan})
	if err != nil || outcome.Status != StatusReady || len(outcome.Warnings) != 0 {
		t.Fatalf("completion cleanup retry: %#v %v", outcome, err)
	}
	fixture.assertState(t, completed)
}

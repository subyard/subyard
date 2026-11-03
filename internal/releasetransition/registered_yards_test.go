package releasetransition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
)

func TestRegisteredYardMigrationUsesDurableExistingJournal(t *testing.T) {
	for _, scenario := range []string{"complete", "cut-before-effect", "cut-after-effect", "stale-native", "late-decline", "per-yard-partial"} {
		t.Run(scenario, func(t *testing.T) {
			transition, _, _ := v2TransitionFixture(t, nil)
			owner := &nativeConsentOwner{v2TestReconciler: &v2TestReconciler{id: "yard.hermes"}, decline: scenario == "late-decline"}
			var registry RegistryV2
			if err := json.Unmarshal(transition.options.RegistryPayload, &registry); err != nil {
				t.Fatal(err)
			}
			registry.CurrentEpochs["owner-registration"] = 3
			registry.Migrations = append(registry.Migrations, MigrationDefinitionV2{ID: "registered-local-v3", Domain: "owner-registration", Kind: registeredYardsMigration, FromEpoch: 2, ToEpoch: 3, DependsOn: []string{"canonicalize-test-yard-owner-v2"}})
			payload, _ := json.Marshal(registry)
			transition.options.RegistryPayload = payload
			transition.options.RegisteredYards = []V2ActivationReconciler{owner}
			second := &nativeConsentOwner{v2TestReconciler: &v2TestReconciler{id: "yard.other"}, decline: true}
			if scenario == "per-yard-partial" {
				transition.options.RegisteredYards = append(transition.options.RegisteredYards, second)
			}
			var err error
			transition, err = NewV2Transition(transition.options)
			if err != nil {
				t.Fatal(err)
			}
			goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
			plan, err := transition.Inspect(context.Background(), goal)
			if err != nil || len(plan.Blockers) != 0 {
				t.Fatalf("inspect: %#v %v", plan, err)
			}
			sentinel := errors.New("durable cut")
			if scenario == "stale-native" {
				owner.drift = digestC
			}
			if scenario == "cut-before-effect" {
				transition.options.fault = func(point string) error {
					if point == "after-step-evidence" {
						snapshot, _ := transition.store.ReadCurrentJournal()
						journal, _ := ParseJournal(snapshot.Payload)
						for _, step := range journal.Steps {
							if step.Resource == "yard.hermes" && step.Checkpoint == StepEvidence {
								return sentinel
							}
						}
					}
					return nil
				}
			}
			if scenario == "cut-after-effect" {
				transition.options.fault = func(point string) error {
					if point == "after-step-applied" && owner.reconciles > 0 {
						return sentinel
					}
					return nil
				}
			}
			outcome, err := transition.Converge(context.Background(), Execution{Plan: plan.Plan, Authorization: v2TestAuthorization(plan.Plan)})
			if scenario == "stale-native" {
				if owner.reconciles != 0 || outcome.Status == StatusReady {
					t.Fatalf("stale work ran: %#v %v", outcome, err)
				}
				return
			}
			if scenario == "complete" {
				if err != nil || outcome.Status != StatusReady || owner.reconciles != 1 {
					t.Fatalf("complete: %#v count=%d %v", outcome, owner.reconciles, err)
				}
			} else {
				if err == nil && outcome.Status == StatusReady {
					t.Fatal("cut marked ready")
				}
				snapshot, readErr := transition.store.ReadCurrentJournal()
				journal, parseErr := ParseJournal(snapshot.Payload)
				if readErr != nil || parseErr != nil || journal.Checkpoint == JournalComplete {
					t.Fatalf("cut lost journal: %#v %v %v", journal, readErr, parseErr)
				}
				transition.options.fault = nil
				owner.decline = false
				second.decline = false
				fresh, inspectErr := transition.Inspect(context.Background(), goal)
				if inspectErr != nil {
					t.Fatal(inspectErr)
				}
				outcome, err = transition.Converge(context.Background(), Execution{Plan: fresh.Plan, Authorization: v2TestAuthorization(fresh.Plan)})
				if err != nil || outcome.Status != StatusReady || owner.reconciles != 1 {
					t.Fatalf("resume: %#v count=%d %v", outcome, owner.reconciles, err)
				}
			}
			reopened, err := NewPOSIXV2Store(transition.options.ConfigHome)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := reopened.InspectSourceMetadata(transition.registry, transition.registryDigest); err != nil {
				t.Fatalf("fresh-process source admission: %v", err)
			}
			repeat, err := transition.Inspect(context.Background(), goal)
			if err != nil || repeat.Assessment.Changed {
				t.Fatalf("repeat: %#v %v", repeat, err)
			}
		})
	}
}

type refusingRegisteredOwner struct {
	*v2TestReconciler
	refusal error
}

func (owner *refusingRegisteredOwner) Observe(ctx context.Context, releases ReleasePair, links ReleaseLinks) (V2ActivationObservation, error) {
	if owner.refusal != nil {
		return V2ActivationObservation{}, owner.refusal
	}
	return owner.v2TestReconciler.Observe(ctx, releases, links)
}

// One yard's refusal must not hide another's, in either fixed native migration.
func TestRegisteredOwnersAllReportRefusalsBeforeAnyJournal(t *testing.T) {
	stale := fmt.Errorf("yard beta: %w", domain.ErrPlanStale)
	refused := errors.New("yard alpha: first refusal")
	owners := []V2ActivationReconciler{
		&refusingRegisteredOwner{v2TestReconciler: &v2TestReconciler{id: "yard.beta"}, refusal: stale},
		&refusingRegisteredOwner{v2TestReconciler: &v2TestReconciler{id: "yard.ready"}},
		&refusingRegisteredOwner{v2TestReconciler: &v2TestReconciler{id: "yard.alpha"}, refusal: refused},
	}
	transition, _, _ := v2TransitionFixture(t, nil)
	var registry RegistryV2
	if err := json.Unmarshal(transition.options.RegistryPayload, &registry); err != nil {
		t.Fatal(err)
	}
	registry.CurrentEpochs["owner-registration"] = 3
	registry.Migrations = append(registry.Migrations, MigrationDefinitionV2{ID: "registered-local-v3", Domain: "owner-registration", Kind: registeredYardsMigration, FromEpoch: 2, ToEpoch: 3, DependsOn: []string{"canonicalize-test-yard-owner-v2"}})
	payload, _ := json.Marshal(registry)
	transition.options.RegistryPayload = payload
	transition.options.RegisteredYards = owners
	transition, err := NewV2Transition(transition.options)
	if err != nil {
		t.Fatal(err)
	}
	want := "yard alpha: first refusal; yard beta: " + domain.ErrPlanStale.Error()
	for range 5 {
		_, err := transition.Inspect(context.Background(), Goal{Target: "release-a", Direction: DirectionActivateTarget})
		if err == nil || err.Error() != want {
			t.Fatalf("got %v, want %s", err, want)
		}
		if !errors.Is(err, domain.ErrPlanStale) || !errors.Is(err, refused) {
			t.Fatalf("classification lost: %v", err)
		}
	}
	snapshot, err := transition.store.ReadCurrentJournal()
	if err != nil || snapshot.Exists {
		t.Fatalf("refused inspection wrote a journal: %v", err)
	}
	guest := MigrationDefinitionV2{Kind: guestDiskTmpMigration, Domain: "power-metadata", FromEpoch: 2, ToEpoch: 3}
	transition.options.GuestDiskTmp = owners
	observation := &v2Observation{}
	if _, err := transition.appendNativeMigration(context.Background(), observation, guest, nil); err == nil || err.Error() != want {
		t.Fatalf("guest migration hid a yard: %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := transition.inspectRegisteredSettings(canceled, MigrationDefinitionV2{Kind: registeredYardsMigration, Domain: "owner-registration", FromEpoch: 2, ToEpoch: 3}, nil, ReleaseLinks{}); err == nil || err.Error() != stale.Error() {
		t.Fatalf("canceled inspection kept observing yards: %v", err)
	}
}

type registeredVerdict struct{ message string }

func (verdict *registeredVerdict) Error() string { return verdict.message }
func (verdict *registeredVerdict) ActivationDiagnostic() (string, string) {
	return verdict.message, "correct the reported guest state, then run yard update --check"
}
func (*registeredVerdict) MigrationRefusal() bool { return true }

// An owner's verdict about existing state is a blocker naming every yard, not a
// failure of the engine that would be reported as a missing dependency.
func TestRegisteredOwnerVerdictsBlockAndNameEveryYard(t *testing.T) {
	transition, _, _ := v2TransitionFixture(t, nil)
	var registry RegistryV2
	if err := json.Unmarshal(transition.options.RegistryPayload, &registry); err != nil {
		t.Fatal(err)
	}
	registry.CurrentEpochs["power-metadata"] = 3
	registry.Migrations = append(registry.Migrations,
		MigrationDefinitionV2{ID: "publication-v2", Domain: "power-metadata", Kind: "owner-publication-state-v1", FromEpoch: 1, ToEpoch: 2},
		MigrationDefinitionV2{ID: "guest-tmp-v3", Domain: "power-metadata", Kind: guestDiskTmpMigration, FromEpoch: 2, ToEpoch: 3})
	payload, _ := json.Marshal(registry)
	transition.options.RegistryPayload = payload
	beta := &refusingRegisteredOwner{v2TestReconciler: &v2TestReconciler{id: "yard.beta"}, refusal: &registeredVerdict{"yard beta: guest refused: second"}}
	alpha := &refusingRegisteredOwner{v2TestReconciler: &v2TestReconciler{id: "yard.alpha"}, refusal: &registeredVerdict{"yard alpha: guest refused: first"}}
	ready := &refusingRegisteredOwner{v2TestReconciler: &v2TestReconciler{id: "yard.ready"}}
	transition.options.GuestDiskTmp = []V2ActivationReconciler{beta, ready, alpha}
	transition, err := NewV2Transition(transition.options)
	if err != nil {
		t.Fatal(err)
	}
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatalf("verdict became an engine failure: %v", err)
	}
	if err := ValidateProcessInspection(goal, inspection); err != nil {
		t.Fatalf("blocked inspection crosses no process boundary: %v", err)
	}
	if len(inspection.Blockers) != 1 || inspection.Blockers[0].Code != CodePreconditionBlocked ||
		inspection.Blockers[0].Message != "yard alpha: guest refused: first; yard beta: guest refused: second" ||
		inspection.Blockers[0].Retry != "correct the reported guest state, then run yard update --check" {
		t.Fatalf("blockers: %#v", inspection.Blockers)
	}
	outcome := inspection.Outcome
	if outcome == nil || outcome.Status != StatusOperatorActionRequired || !strings.Contains(outcome.Message, "yard beta") || strings.Contains(outcome.Retry, "install a compatible release") {
		t.Fatalf("outcome: %#v", outcome)
	}
	if snapshot, err := transition.store.ReadCurrentJournal(); err != nil || snapshot.Exists {
		t.Fatalf("refused inspection wrote a journal: %v", err)
	}

	// An unrelated inspection failure ends the observation and must carry the verdicts.
	ready.refusal = errors.New("yard ready: registry unreadable")
	_, err = transition.Inspect(context.Background(), goal)
	for _, want := range []string{"yard alpha: guest refused", "yard beta: guest refused", "yard ready: registry unreadable"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%q hidden by another refusal: %v", want, err)
		}
	}
}

func TestRegisteredOwnerVerdictBlocksResumeWithoutFailingInspection(t *testing.T) {
	transition, _, _ := v2TransitionFixture(t, nil)
	var registry RegistryV2
	if err := json.Unmarshal(transition.options.RegistryPayload, &registry); err != nil {
		t.Fatal(err)
	}
	registry.CurrentEpochs["power-metadata"] = 3
	registry.Migrations = append(registry.Migrations,
		MigrationDefinitionV2{ID: "publication-v2", Domain: "power-metadata", Kind: "owner-publication-state-v1", FromEpoch: 1, ToEpoch: 2},
		MigrationDefinitionV2{ID: "guest-tmp-v3", Domain: "power-metadata", Kind: guestDiskTmpMigration, FromEpoch: 2, ToEpoch: 3})
	payload, _ := json.Marshal(registry)
	transition.options.RegistryPayload = payload
	owner := &refusingRegisteredOwner{v2TestReconciler: &v2TestReconciler{id: "yard.hermes"}}
	transition.options.GuestDiskTmp = []V2ActivationReconciler{owner}
	transition, err := NewV2Transition(transition.options)
	if err != nil {
		t.Fatal(err)
	}
	transition.options.fault = func(point string) error {
		if point == "after-step-evidence" {
			return errors.New("durable cut")
		}
		return nil
	}
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil || len(inspection.Blockers) != 0 {
		t.Fatalf("inspect: %#v %v", inspection, err)
	}
	if outcome, err := transition.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)}); err == nil && outcome.Status == StatusReady {
		t.Fatal("cut marked ready")
	}
	snapshot, err := transition.store.ReadCurrentJournal()
	if err != nil || !snapshot.Exists {
		t.Fatalf("cut left no journal: %v", err)
	}
	owner.refusal = &registeredVerdict{"yard hermes: guest refused: unsafe"}
	transition.options.fault = nil
	resumed, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatalf("verdict failed resume inspection: %v", err)
	}
	if len(resumed.Blockers) != 1 || resumed.Blockers[0].Resource != "yard.hermes" || resumed.Blockers[0].Message != "yard hermes: guest refused: unsafe" {
		t.Fatalf("resume blockers: %#v", resumed.Blockers)
	}
}

type nativeConsentOwner struct {
	*v2TestReconciler
	decline bool
	retired func(JournalRecord) bool
}

func (owner *nativeConsentOwner) RetiredJournalBinding(_ context.Context, journal JournalRecord) (bool, error) {
	return owner.retired != nil && owner.retired(journal), nil
}

func TestRegisteredYardRetirementRequiresExactPredecessorAndReauthorization(t *testing.T) {
	for _, valid := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing-proof", true: "bound-proof"}[valid], func(t *testing.T) {
			transition, _, _ := v2TransitionFixture(t, nil)
			owner := &nativeConsentOwner{v2TestReconciler: &v2TestReconciler{id: "yard.selected"}}
			neighbor := &nativeConsentOwner{v2TestReconciler: &v2TestReconciler{id: "yard.neighbor"}, decline: true}
			var registry RegistryV2
			if err := json.Unmarshal(transition.options.RegistryPayload, &registry); err != nil {
				t.Fatal(err)
			}
			registry.CurrentEpochs["owner-registration"] = 3
			registry.Migrations = append(registry.Migrations, MigrationDefinitionV2{ID: "registered-local-v3", Domain: "owner-registration", Kind: registeredYardsMigration, FromEpoch: 2, ToEpoch: 3, DependsOn: []string{"canonicalize-test-yard-owner-v2"}})
			transition.options.RegistryPayload, _ = json.Marshal(registry)
			transition.options.RegisteredYards = []V2ActivationReconciler{owner, neighbor}
			var err error
			transition, err = NewV2Transition(transition.options)
			if err != nil {
				t.Fatal(err)
			}
			goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
			plan, err := transition.Inspect(context.Background(), goal)
			if err != nil {
				t.Fatal(err)
			}
			transition.options.fault = func(point string) error {
				if point == "after-step-evidence" {
					snapshot, _ := transition.store.ReadCurrentJournal()
					journal, _ := ParseJournal(snapshot.Payload)
					for _, step := range journal.Steps {
						if step.Resource == owner.ID() && step.Checkpoint == StepEvidence {
							return errors.New("cut before old guest effect")
						}
					}
				}
				return nil
			}
			_, _ = transition.Converge(context.Background(), Execution{Plan: plan.Plan, Authorization: v2TestAuthorization(plan.Plan)})
			binding, err := transition.store.CaptureRegisteredYardBinding(owner.ID())
			if err != nil {
				t.Fatal(err)
			}
			before, _ := transition.store.ReadCurrentJournal()
			ledgerBefore, _ := transition.store.ReadLedger()
			owner.desired = digestC
			owner.converged = true
			if valid {
				owner.retired = func(journal JournalRecord) bool { return binding.Matches(journal, owner.ID()) }
			}
			transition.options.fault = nil
			fresh, err := NewV2Transition(transition.options)
			if err != nil {
				t.Fatal(err)
			}
			inspection, err := fresh.Inspect(context.Background(), goal)
			if !valid {
				if err == nil {
					t.Fatal("changed UUID/config binding accepted without retirement proof")
				}
			} else {
				if err != nil || len(inspection.Blockers) != 0 || inspection.Plan == plan.Plan {
					t.Fatalf("fresh authorization missing: %#v %v", inspection, err)
				}
				if owner.reconciles != 0 || neighbor.reconciles != 0 {
					t.Fatal("read-only retirement inspection reconciled native work")
				}
			}
			after, _ := transition.store.ReadCurrentJournal()
			ledgerAfter, _ := transition.store.ReadLedger()
			if !sameProtectedSnapshot(before, after) || !sameProtectedSnapshot(ledgerBefore, ledgerAfter) {
				t.Fatal("retirement inspection changed shared history")
			}
		})
	}
}

func TestRegisteredYardRetirementAfterVerifiedNativeWork(t *testing.T) {
	for _, valid := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "verified"}[valid], func(t *testing.T) {
			transition, _, _ := v2TransitionFixture(t, nil)
			owner := &nativeConsentOwner{v2TestReconciler: &v2TestReconciler{id: "yard.selected"}}
			var registry RegistryV2
			if err := json.Unmarshal(transition.options.RegistryPayload, &registry); err != nil {
				t.Fatal(err)
			}
			registry.CurrentEpochs["owner-registration"] = 3
			registry.Migrations = append(registry.Migrations, MigrationDefinitionV2{ID: "registered-local-v3", Domain: "owner-registration", Kind: registeredYardsMigration, FromEpoch: 2, ToEpoch: 3, DependsOn: []string{"canonicalize-test-yard-owner-v2"}})
			transition.options.RegistryPayload, _ = json.Marshal(registry)
			transition.options.RegisteredYards = []V2ActivationReconciler{owner}
			var err error
			transition, err = NewV2Transition(transition.options)
			if err != nil {
				t.Fatal(err)
			}
			goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
			plan, err := transition.Inspect(context.Background(), goal)
			if err != nil {
				t.Fatal(err)
			}
			transition.options.fault = func(point string) error {
				if point == "after-reconciling" {
					return errors.New("cut after native verification")
				}
				return nil
			}
			_, _ = transition.Converge(context.Background(), Execution{Plan: plan.Plan, Authorization: v2TestAuthorization(plan.Plan)})
			binding, err := transition.store.CaptureRegisteredYardBinding(owner.ID())
			if err != nil {
				t.Fatal(err)
			}
			old, err := ParseJournal(binding.Snapshot.Payload)
			if err != nil || old.Checkpoint != JournalReconciling {
				t.Fatalf("wrong cut: %s %v", old.Checkpoint, err)
			}
			owner.desired = digestC
			owner.converged = true
			if valid {
				owner.retired = func(journal JournalRecord) bool { return binding.Matches(journal, owner.ID()) }
			}
			transition.options.fault = nil
			fresh, err := NewV2Transition(transition.options)
			if err != nil {
				t.Fatal(err)
			}
			inspection, err := fresh.Inspect(context.Background(), goal)
			// Activation may change a verified native row's inputs. At this
			// checkpoint it is history, with or without a retirement receipt;
			// both paths must finish without replaying the old native work.
			if err != nil || len(inspection.Blockers) != 0 {
				t.Fatalf("verified native history: %#v %v", inspection, err)
			}
			outcome, err := fresh.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
			if err != nil || outcome.Status != StatusReady || owner.reconciles != 1 {
				t.Fatalf("verified work replayed: %#v count=%d %v", outcome, owner.reconciles, err)
			}
			if binding.Matches(old, "yard.other") {
				t.Fatal("wrong-yard receipt accepted")
			}
			tampered := old
			tampered.AuthorizationPlan = PlanToken(digestC)
			if binding.Matches(tampered, owner.ID()) {
				t.Fatal("wrong authorization receipt accepted")
			}
			tampered = old
			tampered.Transaction = "different-transaction"
			if binding.Matches(tampered, owner.ID()) {
				t.Fatal("wrong predecessor receipt accepted")
			}
		})
	}
}

func (owner *nativeConsentOwner) Reconcile(ctx context.Context, links ReleaseLinks) error {
	if owner.decline {
		return domain.ErrOperationDeclined
	}
	return owner.v2TestReconciler.Reconcile(ctx, links)
}

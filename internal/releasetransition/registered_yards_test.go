package releasetransition

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

type dependencyRegisteredOwner struct {
	*nativeConsentOwner
	failure   error
	remaining int
}

func (owner *dependencyRegisteredOwner) Observe(ctx context.Context, releases ReleasePair, links ReleaseLinks) (V2ActivationObservation, error) {
	if owner.remaining != 0 {
		if owner.remaining > 0 {
			owner.remaining--
		}
		return V2ActivationObservation{}, owner.failure
	}
	return owner.nativeConsentOwner.Observe(ctx, releases, links)
}

func TestRegisteredDependencyFailurePreservesRecoveryAdvice(t *testing.T) {
	for _, point := range []string{"after-step-evidence", "after-step-applied"} {
		for _, scenario := range []string{"raw-once", "public-once", "persistent"} {
			t.Run(point+"/"+scenario, func(t *testing.T) {
				transition, _, _ := v2TransitionFixture(t, nil)
				owner := &dependencyRegisteredOwner{nativeConsentOwner: &nativeConsentOwner{v2TestReconciler: &v2TestReconciler{id: "yard.hermes"}}, failure: errors.New("private dependency detail")}
				message, retry := "the release transition was interrupted after a durable mutation checkpoint", "run yard update"
				status, code := StatusRecovering, CodeVerificationFailed
				remaining := 1
				if scenario == "public-once" {
					message, retry = "registered yard maintenance observe failed (permission-denied)", "run yard update --check"
					owner.failure = v2PublicActivationError{message, retry}
				} else if scenario == "persistent" {
					remaining = -1
					status, code = StatusOperatorActionRequired, CodeRecoveryAmbiguous
					message, retry = "release facts cannot be reduced to a safe recovery checkpoint", "run yard update --check"
				}
				transition.options.RegistryPayload = []byte(`{"schemaVersion":2,"minimumEpochs":{"owner-registration":2},"currentEpochs":{"owner-registration":3},"migrations":[{"id":"registered-v3","domain":"owner-registration","fromEpoch":2,"toEpoch":3,"kind":"registered-yards-v2-to-v3"}]}`)
				transition.options.RegisteredYards = []V2ActivationReconciler{owner}
				var err error
				transition, err = NewV2Transition(transition.options)
				if err != nil {
					t.Fatal(err)
				}
				armed := false
				transition.options.fault = func(current string) error {
					if current == point && !armed {
						armed = true
						owner.remaining = remaining
					}
					return nil
				}
				plan, err := transition.Inspect(context.Background(), Goal{Target: "release-a", Direction: DirectionActivateTarget})
				if err != nil || len(plan.Blockers) != 0 {
					t.Fatalf("inspect: %#v %v", plan, err)
				}
				outcome, err := transition.Converge(context.Background(), Execution{Plan: plan.Plan, Authorization: v2TestAuthorization(plan.Plan)})
				if err != nil || !armed || outcome.Status != status || outcome.Code != code || outcome.Message != message || outcome.Retry != retry {
					t.Fatalf("dependency changed recovery advice: %#v %v", outcome, err)
				}
			})
		}
	}
}

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

	// An independent dependency retains its classification and every safe verdict.
	ready.refusal = errors.New("yard ready: registry unreadable")
	var stderr bytes.Buffer
	mixed, err := transition.Inspect(WithMigrationDiagnosticWriter(context.Background(), &stderr), goal)
	if err != nil || mixed.Outcome == nil || mixed.Outcome.Code != CodeDependencyUnavailable {
		t.Fatalf("mixed result: %#v %v", mixed, err)
	}
	for _, want := range []string{"yard alpha: guest refused", "yard beta: guest refused"} {
		if !strings.Contains(mixed.Outcome.Message, want) {
			t.Fatalf("hidden %q: %#v", want, mixed.Outcome)
		}
	}
	if !strings.Contains(stderr.String(), "yard ready: registry unreadable") || !strings.Contains(mixed.Outcome.Message, "yard ready:") || !strings.Contains(mixed.Outcome.Retry, "correct the reported guest state") {
		t.Fatalf("lost dependency yard/detail or verdict repair: %#v stderr=%s", mixed.Outcome, stderr.String())
	}

	if !strings.Contains(mixed.Outcome.Retry, "install a compatible release") {
		t.Fatalf("dependency advice changed: %#v", mixed.Outcome)
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
	dependency := errors.New("registry unreadable")
	owner.refusal = domain.CollectRefusals(&registeredVerdict{"yard hermes: guest refused: unsafe"}, dependency)
	var stderr bytes.Buffer
	mixed, err := transition.Inspect(WithMigrationDiagnosticWriter(context.Background(), &stderr), goal)
	if err != nil || mixed.Outcome == nil || mixed.Outcome.Code != CodeDependencyUnavailable || !strings.Contains(mixed.Outcome.Message, "yard hermes: guest refused") || !strings.Contains(mixed.Outcome.Retry, "install a compatible release") || !strings.Contains(mixed.Outcome.Retry, "correct the reported guest state") || !strings.Contains(stderr.String(), "yard hermes: registry unreadable") {
		t.Fatalf("journaled mixed verdict lost classification/details/actions: %#v err=%v stderr=%s", mixed, err, stderr.String())
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

func TestRegisteredStateRefusalsCrossSettingsBoundary(t *testing.T) {
	for _, kind := range []string{registeredYardsMigration, guestDiskTmpMigration} {
		t.Run(kind, func(t *testing.T) {
			transition, _, _ := v2TransitionFixture(t, nil)
			first := domain.MigrationStateRefusal("alpha", errors.New("private assignment"), "adoption refuses conflicting project ownership")
			second := domain.MigrationStateRefusal("beta", errors.New("private address"), "network policy must already be converged")
			owners := []V2ActivationReconciler{
				&refusingRegisteredOwner{v2TestReconciler: &v2TestReconciler{id: "yard.beta"}, refusal: second},
				&refusingRegisteredOwner{v2TestReconciler: &v2TestReconciler{id: "yard.alpha"}, refusal: domain.CollectRefusals(first, domain.MigrationStateRefusal("alpha", errors.New("private device"), "srv device differs from persistent data"))},
			}
			migration := MigrationDefinitionV2{ID: "native-state", Kind: kind, Domain: "owner-registration", FromEpoch: 2, ToEpoch: 3}
			if kind == guestDiskTmpMigration {
				migration.Domain = "power-metadata"
			}
			transition.options.RegisteredYards, transition.options.GuestDiskTmp = owners, owners
			observation := &v2Observation{}
			_, err := transition.appendNativeMigration(context.Background(), observation, migration, nil)
			if err != nil || len(observation.blockers) != 1 {
				t.Fatalf("refusal escaped as dependency: %#v %v", observation.blockers, err)
			}
			blocker := observation.blockers[0]
			for _, want := range []string{"yard alpha: adoption refuses", "yard alpha: srv device differs", "yard beta: network policy"} {
				if !strings.Contains(blocker.Message, want) {
					t.Fatalf("missing %q: %#v", want, blocker)
				}
			}
			if blocker.Code != CodePreconditionBlocked || strings.Contains(blocker.Message, "private") || strings.Contains(blocker.Retry, "install a compatible release") {
				t.Fatalf("unsafe or misleading blocker: %#v", blocker)
			}
			// A read failure beside a verdict keeps the dependency code and public reasons.
			dependency := errors.New("private transport failure")
			owners[0].(*refusingRegisteredOwner).refusal = domain.CollectRefusals(second, dependency)
			mixed := &v2Observation{}
			_, err = transition.appendNativeMigration(context.Background(), mixed, migration, nil)
			if err != nil || len(mixed.blockers) != 1 || mixed.blockers[0].Code != CodeDependencyUnavailable || !strings.Contains(mixed.blockers[0].Message, "yard beta: network policy") || strings.Contains(mixed.blockers[0].Message, "private") {
				t.Fatalf("mixed result lost safe verdicts: %#v %v", mixed.blockers, err)
			}

		})
	}
}

func TestRegisteredSettingsVerdictBlocksResume(t *testing.T) {
	transition, _, _ := v2TransitionFixture(t, nil)
	var registry RegistryV2
	if err := json.Unmarshal(transition.options.RegistryPayload, &registry); err != nil {
		t.Fatal(err)
	}
	registry.CurrentEpochs["owner-registration"] = 3
	registry.Migrations = append(registry.Migrations, MigrationDefinitionV2{ID: "registered-local-v3", Domain: "owner-registration", Kind: registeredYardsMigration, FromEpoch: 2, ToEpoch: 3, DependsOn: []string{"canonicalize-test-yard-owner-v2"}})
	transition.options.RegistryPayload, _ = json.Marshal(registry)
	owner := &refusingRegisteredOwner{v2TestReconciler: &v2TestReconciler{id: "yard.hermes"}}
	transition.options.RegisteredYards = []V2ActivationReconciler{owner}
	var err error
	transition, err = NewV2Transition(transition.options)
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
	plan, err := transition.Inspect(context.Background(), goal)
	if err != nil || len(plan.Blockers) != 0 {
		t.Fatalf("plan: %#v %v", plan, err)
	}
	if outcome, err := transition.Converge(context.Background(), Execution{Plan: plan.Plan, Authorization: v2TestAuthorization(plan.Plan)}); err == nil && outcome.Status == StatusReady {
		t.Fatal("cut marked ready")
	}
	owner.refusal = domain.MigrationStateRefusal("hermes", errors.New("private ownership value"), "adoption refuses conflicting project ownership")
	transition.options.fault = nil
	resumed, err := transition.Inspect(context.Background(), goal)
	if err != nil || len(resumed.Blockers) == 0 || resumed.Blockers[0].Code != CodePreconditionBlocked || !strings.Contains(resumed.Blockers[0].Message, "yard hermes: adoption refuses conflicting project ownership") {
		t.Fatalf("resume lost verdict: %#v %v", resumed, err)
	}
}

type migrationActionVerdict struct{ message, retry string }

func (failure migrationActionVerdict) Error() string          { return failure.message }
func (failure migrationActionVerdict) MigrationRefusal() bool { return true }
func (failure migrationActionVerdict) ActivationDiagnostic() (string, string) {
	return failure.message, failure.retry
}

func TestNativeRefusalKeepsDistinctActionsAndCountsOverflow(t *testing.T) {
	unsafe := domain.MigrationStateRefusal("alpha", errors.New("private-value"), "unsupported device", "/private-value")
	safe := nativeRefusalBlocker("yard.alpha", unsafe)
	if strings.Contains(safe.Message, "private-value") || !strings.Contains(safe.Message, "unrecognized name") {
		t.Fatalf("unsafe name escaped: %#v", safe)
	}
	one := migrationActionVerdict{"yard alpha: owner refused", "repair alpha ownership"}
	two := migrationActionVerdict{"yard beta: network refused", "run yard network reconcile"}
	blocker := nativeRefusalBlocker("migration.native", domain.CollectRefusals(one, one, two))
	if strings.Count(blocker.Message, one.message) != 1 || !strings.Contains(blocker.Retry, one.retry) || !strings.Contains(blocker.Retry, two.retry) {
		t.Fatalf("lost reason/action: %#v", blocker)
	}
	var findings []error
	for index := 0; index < 40; index++ {
		findings = append(findings, domain.MigrationStateRefusal("alpha", fmt.Errorf("refused-%d", index), "unsupported instance configuration key", fmt.Sprintf("foreign.option%d", index)))
	}
	findings = append(findings, domain.MigrationStateRefusal("alpha", errors.New("srv"), "srv device differs from persistent data"))
	blocker = nativeRefusalBlocker("migration.native", domain.CollectRefusals(findings...))
	if len(blocker.Message) > maxDiagnosticText || (!strings.Contains(blocker.Message, "unsupported instance configuration key (40)") || !strings.Contains(blocker.Message, "srv device differs from persistent data (1)")) || strings.HasSuffix(blocker.Message, "...") {
		t.Fatalf("silently discarded overflowing reasons: %#v", blocker)
	}
}

type diagnosticOwnerRegistration struct {
	*v2TestOwnerRegistration
	refusal error
}

func (owner *diagnosticOwnerRegistration) Prepare(ctx context.Context, view V2SettingsSnapshotView) (OwnerRegistrationObservation, error) {
	if owner.refusal != nil {
		return OwnerRegistrationObservation{}, owner.refusal
	}
	return owner.v2TestOwnerRegistration.Prepare(ctx, view)
}
func (owner *diagnosticOwnerRegistration) Observe(ctx context.Context, before OwnerRegistrationObservation) (OwnerRegistrationProgress, error) {
	if owner.refusal != nil {
		return "", owner.refusal
	}
	return owner.v2TestOwnerRegistration.Observe(ctx, before)
}

func TestOwnerRegistrationPublicRefusalAtRecoveryBoundaries(t *testing.T) {
	for _, scenario := range []string{"intent", "evidence", "new-impact"} {
		t.Run(scenario, func(t *testing.T) {
			transition, _, _ := v2TransitionFixture(t, nil)
			owner := &diagnosticOwnerRegistration{v2TestOwnerRegistration: &v2TestOwnerRegistration{state: OwnerRegistrationLegacyDirectory}}
			if scenario == "new-impact" {
				owner.state = OwnerRegistrationAbsent
			}
			transition.options.RegistryPayload = []byte(`{"schemaVersion":2,"minimumEpochs":{"owner-registration":1},"currentEpochs":{"owner-registration":2},"migrations":[{"id":"owner-v2","domain":"owner-registration","fromEpoch":1,"toEpoch":2,"kind":"test-yard-owner-v1-to-v2"}]}`)
			transition.options.OwnerRegistration = owner
			var err error
			transition, err = NewV2Transition(transition.options)
			if err != nil {
				t.Fatal(err)
			}
			cut := "after-step-evidence"
			if scenario == "intent" {
				cut = "after-journal-authorized"
			}
			transition.options.fault = func(point string) error {
				if point == cut {
					return errors.New("durable cut")
				}
				return nil
			}
			goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
			plan, err := transition.Inspect(context.Background(), goal)
			if err != nil || len(plan.Blockers) != 0 {
				t.Fatalf("plan: %#v %v", plan, err)
			}
			result, err := transition.Converge(context.Background(), Execution{Plan: plan.Plan, Authorization: v2TestAuthorization(plan.Plan)})
			if err != nil || result.Status != StatusRecovering {
				t.Fatalf("cut: %#v %v", result, err)
			}
			owner.refusal = migrationActionVerdict{"yard hermes: yards/hermes/config.env must select its supported template", "correct yards/hermes/config.env, then run yard update --check"}
			transition.options.fault = nil
			plan, err = transition.Inspect(context.Background(), goal)
			if err != nil || plan.Outcome == nil || !strings.Contains(plan.Outcome.Message, "yards/hermes/config.env") || !strings.Contains(plan.Outcome.Retry, "correct yards/hermes/config.env") {
				t.Fatalf("recovery lost public owner refusal: %#v %v", plan, err)
			}
			if err := ValidateProcessInspection(goal, plan); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRealRegistryOwnerVerdictBlocksWithoutMutation(t *testing.T) {
	for _, state := range []string{"absent", "ledger", "journal"} {
		t.Run(state, func(t *testing.T) {
			transition, home, path := v2TransitionFixture(t, nil)
			payload, err := os.ReadFile(filepath.Join("..", "..", "config", "release-transition.json"))
			if err != nil {
				t.Fatal(err)
			}
			candidate, err := filepath.Abs(filepath.Join("..", "..", "config"))
			if err != nil {
				t.Fatal(err)
			}
			settings := []byte("YARD_TEMPLATE=test-vms\nSSH_PORT=2240\n")
			testkit.WriteFile(t, path, settings, 0600)
			owner := &diagnosticOwnerRegistration{v2TestOwnerRegistration: &v2TestOwnerRegistration{state: OwnerRegistrationLegacyDirectory}}
			native := &refusingRegisteredOwner{v2TestReconciler: &v2TestReconciler{id: "yard.hermes"}}
			transition.options.RegistryPayload, transition.options.CandidateConfigDir = payload, candidate
			transition.options.OwnerRegistration = owner
			transition.options.RegisteredYards = []V2ActivationReconciler{native}
			transition.options.GuestDiskTmp = []V2ActivationReconciler{native}
			transition, err = NewV2Transition(transition.options)
			if err != nil {
				t.Fatal(err)
			}
			if state != "absent" {
				ledger := BaselineLedgerV2(transition.registry)
				pending, err := transition.registry.PendingPath(ledger)
				if err != nil {
					t.Fatal(err)
				}
				for _, migration := range pending {
					if migration.Kind == "test-yard-owner-v1-to-v2" {
						break
					}
					ledger, err = ledger.Advance(transition.registry, migration)
					if err != nil {
						t.Fatal(err)
					}
				}
				projection, _, err := MarshalLedgerV2(ledger, transition.registry)
				if err != nil {
					t.Fatal(err)
				}
				if err := transition.store.CompareAndSwapLedger(absentProtectedSnapshot(), projection); err != nil {
					t.Fatal(err)
				}
			}
			goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
			if state == "journal" {
				plan, err := transition.Inspect(context.Background(), goal)
				if err != nil || len(plan.Blockers) != 0 {
					t.Fatalf("seed plan: %#v %v", plan, err)
				}
				transition.options.fault = func(point string) error {
					if point == "after-journal-authorized" {
						return errors.New("durable cut before effects")
					}
					return nil
				}
				outcome, err := transition.Converge(context.Background(), Execution{Plan: plan.Plan, Authorization: v2TestAuthorization(plan.Plan)})
				if err != nil || outcome.Status != StatusRecovering {
					t.Fatalf("seed journal: %#v %v", outcome, err)
				}
				transition.options.fault = nil
			}
			owner.refusal = migrationActionVerdict{"reserved yard name test-yard: yards/test-yard/config.env must select YARD_TEMPLATE=test-vms", "set YARD_TEMPLATE=test-vms in yards/test-yard/config.env"}
			native.refusal = domain.MigrationStateRefusal("hermes", errors.New("private snapshot"), "yard adoption refuses unproven snapshots", "snap-hermes")
			transition.options.VerifyAuthorization = func(PlanToken, Authorization) bool {
				t.Fatal("refused plan reached authorization")
				return false
			}
			before := snapshotStoreV2TreeIfPresent(t, home)
			plan, err := transition.Inspect(context.Background(), goal)
			if err != nil || plan.Outcome == nil || plan.Outcome.Status != StatusOperatorActionRequired || len(plan.Blockers) < 2 {
				t.Fatalf("real registry lost structured independent refusals: %#v %v", plan, err)
			}
			if state != "journal" && plan.Outcome.Code != CodePreconditionBlocked {
				t.Fatalf("owner verdict became a dependency: %#v", plan.Outcome)
			}
			encoded, err := json.Marshal(plan)
			if err != nil || !bytes.Contains(encoded, []byte("yards/test-yard/config.env")) || !bytes.Contains(encoded, []byte("snap-hermes")) {
				t.Fatalf("missing owner or later native refusal: %s %v", encoded, err)
			}
			if err := ValidateProcessInspection(goal, plan); err != nil {
				t.Fatal(err)
			}
			outcome, err := transition.Converge(context.Background(), Execution{Plan: plan.Plan, Authorization: "must-not-authorize"})
			if err != nil || outcome.Status != StatusOperatorActionRequired || strings.Contains(outcome.Retry, "install a compatible release") || owner.commits != 0 || native.reconciles != 0 {
				t.Fatalf("refused work ran: %#v %v", outcome, err)
			}
			afterSettings, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(settings, afterSettings) || !bytes.Equal(before, snapshotStoreV2TreeIfPresent(t, home)) {
				t.Fatalf("refused inspection/convergence wrote state: %v", err)
			}
		})
	}
}

func TestRegisteredMixedPublicDependenciesRetainEveryYard(t *testing.T) {
	transition, _, _ := v2TransitionFixture(t, nil)
	transition.options.RegistryPayload = []byte(`{"schemaVersion":2,"minimumEpochs":{"owner-registration":2},"currentEpochs":{"owner-registration":3},"migrations":[{"id":"registered-v3","domain":"owner-registration","fromEpoch":2,"toEpoch":3,"kind":"registered-yards-v2-to-v3"}]}`)
	public := v2PublicActivationError{"registered yard maintenance observe failed (permission-denied)", "run yard update --check"}
	transition.options.RegisteredYards = []V2ActivationReconciler{
		&refusingRegisteredOwner{v2TestReconciler: &v2TestReconciler{id: "yard.alpha"}, refusal: domain.MigrationStateRefusal("alpha", errors.New("private snapshot"), "yard adoption refuses unproven snapshots", "snap-alpha")},
		&refusingRegisteredOwner{v2TestReconciler: &v2TestReconciler{id: "yard.beta"}, refusal: public},
		&refusingRegisteredOwner{v2TestReconciler: &v2TestReconciler{id: "yard.gamma"}, refusal: public},
	}
	transition, err := NewV2Transition(transition.options)
	if err != nil {
		t.Fatal(err)
	}
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	plan, err := transition.Inspect(context.Background(), goal)
	if err != nil || plan.Outcome == nil || plan.Outcome.Code != CodeDependencyUnavailable {
		t.Fatalf("mixed inspection: %#v %v", plan, err)
	}
	encoded, err := json.Marshal(plan)
	for _, want := range []string{"yard alpha:", "yard beta: " + public.message, "yard gamma: " + public.message} {
		if err != nil || !bytes.Contains(encoded, []byte(want)) {
			t.Fatalf("JSON lost dependency identity %q: %s %v", want, encoded, err)
		}
	}
	if err := ValidateProcessInspection(goal, plan); err != nil {
		t.Fatal(err)
	}
}

func TestRealRegistryRetainsOwnerRefusalBesideLaterReadFailure(t *testing.T) {
	transition, home, path := v2TransitionFixture(t, nil)
	payload, err := os.ReadFile(filepath.Join("..", "..", "config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := filepath.Abs(filepath.Join("..", "..", "config"))
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, []byte("YARD_TEMPLATE=test-vms\nSSH_PORT=2240\n"), 0600)
	refusal := migrationActionVerdict{"reserved yard name test-yard: yards/test-yard/config.env must select YARD_TEMPLATE=test-vms", "set YARD_TEMPLATE=test-vms in yards/test-yard/config.env"}
	owner := &diagnosticOwnerRegistration{v2TestOwnerRegistration: &v2TestOwnerRegistration{state: OwnerRegistrationAbsent}, refusal: refusal}
	dependency := errors.New("yard ready: registry unreadable")
	transition.options.RegistryPayload = payload
	transition.options.CandidateConfigDir = candidate
	transition.options.OwnerRegistration = owner
	transition.options.Ingress = &v2IngressFixture{inspection: V2IngressInspection{Prospective: &failingMigrationReadView{base: posixV2SettingsSnapshotView{configHome: home}, path: filepath.Join(home, config.GitSettingsRelativePath, "config.env"), failure: dependency}}}
	transition.options.RegisteredYards = []V2ActivationReconciler{&refusingRegisteredOwner{v2TestReconciler: &v2TestReconciler{id: "yard.ready"}, refusal: dependency}}
	transition.options.GuestDiskTmp = []V2ActivationReconciler{&v2TestReconciler{id: "yard.ready"}}
	transition, err = NewV2Transition(transition.options)
	if err != nil {
		t.Fatal(err)
	}
	_, err = transition.Inspect(context.Background(), Goal{Target: "release-a", Direction: DirectionActivateTarget})
	if !errors.Is(err, dependency) || !strings.Contains(err.Error(), refusal.message) || !strings.Contains(err.Error(), dependency.Error()) {
		t.Fatalf("real registry lost independent findings: %v", err)
	}
	snapshot, readErr := transition.store.ReadCurrentJournal()
	if readErr != nil || snapshot.Exists {
		t.Fatalf("refused inspection wrote a journal under %s: %v", home, readErr)
	}
}

type failingMigrationReadView struct {
	base    V2SettingsSnapshotView
	path    string
	failure error
}

func (view *failingMigrationReadView) ListYards() ([]string, error) { return view.base.ListYards() }
func (view *failingMigrationReadView) ReadSnapshot(path string) (config.PersistentFileSnapshot, error) {
	if path == view.path {
		return config.PersistentFileSnapshot{}, view.failure
	}
	return view.base.ReadSnapshot(path)
}

func TestNativeOverflowRetainsReasonClassesAndFullFilteredDetails(t *testing.T) {
	reasons := []string{"legacy continuation refuses unknown project permissions", "legacy continuation refuses unsupported default-profile configuration", "yard adoption requires canonical project feature booleans", "yard adoption refuses noncanonical project root or NIC properties"}
	var findings []error
	for _, yard := range []string{"alpha", "beta"} {
		for index, reason := range reasons {
			findings = append(findings, scopedNativeRefusals("yard."+yard, domain.MigrationStateRefusal(yard, fmt.Errorf("private-value-%d", index), reason, strings.Repeat("field", 15)+fmt.Sprint(index))))
		}
	}
	var stderr bytes.Buffer
	ctx, flush := captureMigrationDiagnostics(WithMigrationDiagnosticWriter(context.Background(), &stderr))
	blocker := nativeRefusalBlocker("migration.native", domain.CollectRefusals(findings...), ctx)
	flush()
	for _, reason := range []string{"unknown project permissions", "unsupported default-profile configuration", "canonical project feature booleans", "noncanonical project root or NIC properties"} {
		if strings.Count(blocker.Message, reason) != 2 {
			t.Fatalf("lost reason class %q: %s", reason, blocker.Message)
		}
	}
	if len(blocker.Message) > 512 || !strings.Contains(blocker.Message, "yard alpha") || !strings.Contains(blocker.Message, "yard beta") || !strings.Contains(blocker.Message, "stderr of yard update --check") {
		t.Fatalf("unusable overflow: %#v", blocker)
	}
	findings = nil
	for index := 0; index < 30; index++ {
		findings = append(findings, domain.MigrationStateRefusal("alpha", fmt.Errorf("private-value-%d", index), fmt.Sprintf("distinct refusal class %02d with an independently checked prerequisite", index), "safe-name"))
	}
	findings = append(findings, domain.MigrationStateRefusal("alpha", errors.New("private-volume"), "srv device differs from persistent data"), domain.MigrationStateRefusal("beta", errors.New("private-path"), "host-access boundary refuses the native mount"))
	stderr.Reset()
	ctx, flush = captureMigrationDiagnostics(WithMigrationDiagnosticWriter(context.Background(), &stderr))
	blocker = nativeRefusalBlocker("migration.native", domain.CollectRefusals(findings...), ctx)
	flush()
	if len(blocker.Message) > 512 || !strings.Contains(blocker.Message, "srv") || !strings.Contains(blocker.Message, "host-access") || !strings.Contains(blocker.Message, "more reason classes") || !strings.Contains(blocker.Message, "stderr of yard update --check") {
		t.Fatalf("data-protection reason lost at final reduction: %#v", blocker)
	}
	if !strings.Contains(stderr.String(), "distinct refusal class 29") || strings.Contains(stderr.String(), "private-") {
		t.Fatalf("full filtered remainder is not available: %s", stderr.String())
	}
}

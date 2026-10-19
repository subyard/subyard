package application

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

type reconcileFixture struct {
	converged map[ports.ReconcileStageID]bool
	checkErr  map[ports.ReconcileStageID]error
	checked   []ports.ReconcileStageID
	failOnce  map[ports.ReconcileStageID]bool
	verified  map[ports.ReconcileStageID]bool
	applied   []ports.ReconcileStageID
}

type observedReconcileFixture struct {
	*reconcileFixture
	observation ports.ReconcileStageObservation
}

type diagnosticReconcileFixture struct {
	*reconcileFixture
	failure error
	calls   int
}

func (fixture *diagnosticReconcileFixture) VerifyStageWithDiagnostic(context.Context, ports.ReconcileStageID) (bool, error) {
	fixture.calls++
	return false, fixture.failure
}

func TestReconcilerUsesDiagnosticOnlyAfterApply(t *testing.T) {
	stage := ReconcileStage{ID: ports.ReconcileStageProvision, Label: "Provision"}
	failure := errors.New("component requires a native reboot")
	fixture := &diagnosticReconcileFixture{reconcileFixture: &reconcileFixture{
		converged: map[ports.ReconcileStageID]bool{},
	}, failure: failure}
	reconciler := Reconciler{Stages: []ReconcileStage{stage}, Runner: fixture}
	plan, err := reconciler.Plan(context.Background())
	if err != nil || plan.Pending() != 1 || fixture.calls != 0 {
		t.Fatalf("expected drift was not a plain observation: plan=%#v calls=%d err=%v", plan, fixture.calls, err)
	}
	err = reconciler.Apply(context.Background(), plan)
	if !errors.Is(err, failure) || fixture.calls != 1 || len(fixture.applied) != 1 {
		t.Fatalf("post-apply diagnostic was lost: calls=%d applied=%v err=%v", fixture.calls, fixture.applied, err)
	}
}

func (fixture observedReconcileFixture) ObserveStage(context.Context, ports.ReconcileStageID) (ports.ReconcileStageObservation, error) {
	return fixture.observation, nil
}

func TestReconcilerRetainsDriftWithoutApplyingNewWork(t *testing.T) {
	stage := ReconcileStage{ID: ports.ReconcileStageProfileServices, Label: "Services"}
	base := &reconcileFixture{}
	fixture := observedReconcileFixture{base, ports.ReconcileStageObservation{Owner: "sample", NativeExit: 1}}
	reconciler := Reconciler{Stages: []ReconcileStage{stage}, Runner: fixture}
	plan, err := reconciler.Plan(context.Background())
	if err != nil || plan.Pending() != 1 {
		t.Fatalf("ordinary drift was not planable: %#v %v", plan, err)
	}
	before, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	fixture.observation.Owner = "another"
	reconciler.Runner = fixture
	refreshed, err := reconciler.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(refreshed)
	if err != nil || string(before) != string(after) {
		t.Fatalf("diagnostic-only owner changed plan identity: %s -> %s (%v)", before, after, err)
	}
	approved := ReconcilePlan{Steps: []ReconcileStep{{Stage: stage, Converged: true}}}
	for _, owner := range []string{"sample", "private-canary/config"} {
		fixture.observation.Owner = owner
		reconciler.Runner = fixture
		err := reconciler.Apply(context.Background(), approved)
		if !errors.Is(err, domain.ErrPlanStale) || len(base.applied) != 0 || strings.Contains(err.Error(), "private-canary") {
			t.Fatalf("fresh drift refusal unsafe: %v applied=%v", err, base.applied)
		}
		if owner == "sample" && !strings.Contains(err.Error(), "owner=sample native-exit=1") {
			t.Fatalf("fresh refusal lost finite cause: %v", err)
		}
	}
}

func (fixture *reconcileFixture) CheckStage(_ context.Context, id ports.ReconcileStageID) (bool, error) {
	fixture.checked = append(fixture.checked, id)
	if err := fixture.checkErr[id]; err != nil {
		return false, err
	}
	return fixture.converged[id], nil
}

func (fixture *reconcileFixture) ApplyStage(_ context.Context, id ports.ReconcileStageID) error {
	fixture.applied = append(fixture.applied, id)
	if fixture.failOnce[id] {
		delete(fixture.failOnce, id)
		return errors.New("fixture failure")
	}
	fixture.converged[id] = true
	return nil
}

func (fixture *reconcileFixture) VerifyStage(_ context.Context, id ports.ReconcileStageID) (bool, error) {
	if value, exists := fixture.verified[id]; exists {
		return value, nil
	}
	return fixture.converged[id], nil
}

func TestReconcilerPlansLiveStateAndResumesAfterFailure(t *testing.T) {
	stages := []ReconcileStage{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}}
	fixture := &reconcileFixture{
		converged: map[ports.ReconcileStageID]bool{"a": true},
		failOnce:  map[ports.ReconcileStageID]bool{"b": true},
		verified:  map[ports.ReconcileStageID]bool{},
	}
	reconciler := Reconciler{Stages: stages, Runner: fixture}
	plan, err := reconciler.Plan(context.Background())
	if err != nil || plan.Pending() != 1 || !plan.Steps[0].Converged || plan.Steps[1].Converged {
		t.Fatalf("unexpected plan: %#v, %v", plan, err)
	}
	if err := reconciler.Apply(context.Background(), plan); err == nil {
		t.Fatal("partial failure was reported as success")
	}
	if err := reconciler.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fixture.applied, []ports.ReconcileStageID{"b", "b"}) {
		t.Fatalf("resume reapplied converged work: %#v", fixture.applied)
	}
	fixture.converged["a"] = false
	if err := reconciler.Apply(context.Background(), plan); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("observed skip expansion accepted: %v", err)
	}
	if !reflect.DeepEqual(fixture.applied, []ports.ReconcileStageID{"b", "b"}) {
		t.Fatalf("drift repair disturbed converged work: %#v", fixture.applied)
	}
}

func TestReconcilerPlanDoesNotCheckStagesAfterFirstPendingStage(t *testing.T) {
	stages := []ReconcileStage{{ID: "base", Label: "Base"}, {ID: "dependent", Label: "Dependent"}}
	fixture := &reconcileFixture{
		converged: map[ports.ReconcileStageID]bool{"base": false},
		checkErr:  map[ports.ReconcileStageID]error{"dependent": errors.New("base is unavailable")},
	}
	plan, err := (Reconciler{Stages: stages, Runner: fixture}).Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Steps[1].Conditional || plan.Steps[0].Conditional || plan.Pending() != 2 || !reflect.DeepEqual(fixture.checked, []ports.ReconcileStageID{"base"}) {
		t.Fatalf("dependent stages were checked before their prerequisites: plan=%#v checked=%v",
			plan, fixture.checked)
	}
}

func TestReconcilerFailsClosedOnRegistryAndVerification(t *testing.T) {
	fixture := &reconcileFixture{
		converged: map[ports.ReconcileStageID]bool{},
		failOnce:  map[ports.ReconcileStageID]bool{},
		verified:  map[ports.ReconcileStageID]bool{"a": false},
	}
	reconciler := Reconciler{Stages: []ReconcileStage{{ID: "a", Label: "A"}}, Runner: fixture}
	plan, err := reconciler.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Apply(context.Background(), plan); err == nil || !strings.Contains(err.Error(), "did not converge") {
		t.Fatalf("failed verification was accepted: %v", err)
	}
	for _, stages := range [][]ReconcileStage{
		nil,
		{{ID: "a", Label: "A"}, {ID: "a", Label: "duplicate"}},
		{{ID: "../bad", Label: "bad"}},
	} {
		if _, err := (Reconciler{Stages: stages, Runner: fixture}).Plan(context.Background()); err == nil {
			t.Fatalf("invalid registry was accepted: %#v", stages)
		}
	}
}

func TestInitStagesEnrollNetworkPolicyBeforeInstanceCreation(t *testing.T) {
	stages := InitStages(domain.Context{IncusProject: "subyard"})
	index := make(map[ports.ReconcileStageID]int, len(stages))
	for position, stage := range stages {
		index[stage.ID] = position
	}
	hostNetwork, hostFound := index[ports.ReconcileStageNetwork]
	policy, policyFound := index[ports.ReconcileStageNetworkPolicy]
	instance, instanceFound := index[ports.ReconcileStageInstance]
	if !hostFound || !policyFound || !instanceFound || !(hostNetwork < policy && policy < instance) {
		t.Fatalf("network policy stage order is unsafe: %#v", stages)
	}
	if !strings.Contains(stages[policy].Label, "restart affected running yards") {
		t.Fatalf("network policy consequence omits peer restarts: %q", stages[policy].Label)
	}
}

func TestReconcilerAllowsApprovedWorkToConvergeAndConditionalWork(t *testing.T) {
	stages := []ReconcileStage{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}}
	fixture := &reconcileFixture{converged: map[ports.ReconcileStageID]bool{}, verified: map[ports.ReconcileStageID]bool{}}
	reconciler := Reconciler{Stages: stages, Runner: fixture}
	plan, err := reconciler.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fixture.converged["a"] = true
	if err := reconciler.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fixture.applied, []ports.ReconcileStageID{"b"}) {
		t.Fatalf("applied=%v", fixture.applied)
	}
}

func TestReconcilerRejectsMissingOrChangedApprovedScope(t *testing.T) {
	stage := ReconcileStage{ID: "a", Label: "A"}
	fixture := &reconcileFixture{converged: map[ports.ReconcileStageID]bool{}}
	reconciler := Reconciler{Stages: []ReconcileStage{stage}, Runner: fixture}
	for _, plan := range []ReconcilePlan{{}, {Steps: []ReconcileStep{{Stage: ReconcileStage{ID: "b", Label: "B"}}}}} {
		if err := reconciler.Apply(context.Background(), plan); !errors.Is(err, domain.ErrPlanStale) {
			t.Fatalf("error=%v", err)
		}
	}
	if len(fixture.applied) != 0 {
		t.Fatalf("applied=%v", fixture.applied)
	}
}

func TestReconcilerRejectsLaterSkipExpansionBeforeEarlierApply(t *testing.T) {
	stages := []ReconcileStage{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}}
	fixture := &reconcileFixture{converged: map[ports.ReconcileStageID]bool{}}
	approved := ReconcilePlan{Steps: []ReconcileStep{{Stage: stages[0]}, {Stage: stages[1], Converged: true}}}
	if err := (Reconciler{Stages: stages, Runner: fixture}).Apply(context.Background(), approved); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("error=%v", err)
	}
	if len(fixture.applied) != 0 {
		t.Fatalf("applied before stale guard: %v", fixture.applied)
	}
}

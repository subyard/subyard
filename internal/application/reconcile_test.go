package application

import (
	"context"
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
	if err := reconciler.Apply(context.Background()); err == nil {
		t.Fatal("partial failure was reported as success")
	}
	if err := reconciler.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fixture.applied, []ports.ReconcileStageID{"b", "b"}) {
		t.Fatalf("resume reapplied converged work: %#v", fixture.applied)
	}
	fixture.converged["a"] = false
	if err := reconciler.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fixture.applied, []ports.ReconcileStageID{"b", "b", "a"}) {
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
	if plan.Pending() != 2 || !reflect.DeepEqual(fixture.checked, []ports.ReconcileStageID{"base"}) {
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
	if err := reconciler.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "did not converge") {
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

package cli

import (
	"errors"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testyardmigration"
)

func TestOriginalOwnerInitBranchRequiresItsOwnRemovalResult(t *testing.T) {
	yard := testyardmigration.LegacyYard
	current := &initExecution{hostID: "original-current"}
	future := &initExecution{hostID: "original-future"}
	p := &preparedOwnerChildren{inits: map[string]preparedOwnerInitChild{yard: {execution: current, sourceRaw: []byte("current-original")}}, recreate: map[string]preparedOwnerInitChild{yard: {execution: future, sourceRaw: []byte("future-original"), projection: &reconcileruntime.OwnerInitProjection{}}}, removed: map[string]bool{}}
	child, err := p.originalInitChild(yard)
	if err != nil || child.execution != current {
		t.Fatal("original current branch changed", err)
	}
	// No resource observation or name is consulted to choose this branch.
	// Only runTeardown's successful original-handle result sets this private bit.
	p.removed[yard] = true
	child, err = p.originalInitChild(yard)
	if err != nil || child.execution != future {
		t.Fatal("original conditional branch not selected", err)
	}
	delete(p.recreate, yard)
	if _, err := p.originalInitChild(yard); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("missing conditional capsule fell back to current plan", err)
	}
	if _, err := p.originalInitChild("unapproved"); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("new yard admitted", err)
	}
}

func TestOriginalOwnerInitCapsuleKindsAreClosed(t *testing.T) {
	current := ownerInitChildRecord{Yard: testyardmigration.CurrentYard}
	legacy := ownerInitChildRecord{Yard: testyardmigration.LegacyYard}
	future := ownerInitChildRecord{Yard: testyardmigration.LegacyYard, Kind: "recreate", Projection: []byte("original"), Project: &ports.PreparedImageProject{Project: "subyard-" + testyardmigration.LegacyYard, Absent: true}}
	if !ownerInitChildSlot(current, 0) || !ownerInitChildSlot(legacy, 1) || !ownerInitChildSlot(future, 2) {
		t.Fatal("original closed roles refused")
	}
	for _, change := range []string{"yard", "kind", "projection", "project", "preflight", "ordinal"} {
		value := future
		index := 2
		switch change {
		case "yard":
			value.Yard = "foreign"
		case "kind":
			value.Kind = "arbitrary"
		case "projection":
			value.Projection = nil
		case "project":
			value.Project = nil
		case "preflight":
			value.Power = &reconcileruntime.OwnerHostPowerRecord{}
		case "ordinal":
			index = 3
		}
		if ownerInitChildSlot(value, index) {
			t.Fatalf("%s expanded fixed child graph", change)
		}
	}
}

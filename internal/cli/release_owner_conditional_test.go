package cli

import (
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testyardmigration"
)

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

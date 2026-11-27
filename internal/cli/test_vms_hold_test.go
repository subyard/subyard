package cli

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/testvmsruntime"
	"github.com/Subyard/Subyard/internal/domain"
)

// The recover plan tells the operator that it lifts an active hold.
func TestTestVMRecoverPlanNamesALiftedHold(t *testing.T) {
	until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	for _, scenario := range []struct {
		name string
		slot testvmsruntime.LeaseSlot
		want bool
	}{
		{"held", testvmsruntime.LeaseSlot{State: testvmsruntime.SlotQuarantined, HoldLevel: 2, HoldUntil: until}, true},
		{"expired hold", testvmsruntime.LeaseSlot{State: testvmsruntime.SlotQuarantined, HoldLevel: 2, HoldUntil: time.Now().Add(-time.Hour)}, false},
		{"no hold", testvmsruntime.LeaseSlot{State: testvmsruntime.SlotQuarantined}, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			execution := &testVMExecution{action: "recover", slot: 3, snapshot: &scenario.slot}
			_, delta, err := execution.actionPlan()
			if err != nil {
				t.Fatal(err)
			}
			// The confirmation shows the operation steps' consequences.
			for _, consequences := range [][]string{delta.Consequences, domain.OperationStepConsequences(execution.steps())} {
				lifted := slices.ContainsFunc(consequences, func(line string) bool {
					return strings.Contains(line, "lift the loop-protection hold that keeps slot 3 until "+until.Format(time.RFC3339))
				})
				if lifted != scenario.want {
					t.Fatalf("consequences = %q", consequences)
				}
			}
		})
	}
}

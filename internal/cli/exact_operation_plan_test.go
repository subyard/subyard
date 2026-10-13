package cli

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
)

func TestExactOperationPlanExpiry(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	prepared := &preparedCommand{Plan: domain.OperationPlan{OperationID: "fixture", Command: "fixture"}}
	plan := bindExactOperationPlan(prepared, now)
	if plan.Schema != 1 || plan.StepSchema != 0 || len(plan.Digest) != 64 || !plan.ExpiresAt.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("invalid exact plan: %#v", plan)
	}
	for _, test := range []struct {
		name      string
		time      time.Time
		wantStale bool
	}{
		{name: "before expiry", time: plan.ExpiresAt.Add(-time.Nanosecond)},
		{name: "at expiry", time: plan.ExpiresAt, wantStale: true},
		{name: "after expiry", time: plan.ExpiresAt.Add(time.Nanosecond), wantStale: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateExactOperationPlan(plan, prepared, plan.Digest, test.time)
			if test.wantStale {
				if !errors.Is(err, domain.ErrPlanStale) {
					t.Fatalf("expired plan error=%v, want ErrPlanStale", err)
				}
			} else if err != nil {
				t.Fatalf("valid plan rejected before expiry: %v", err)
			}
		})
	}
}

func TestExactOperationPlanRejectsChangedBinding(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	for _, kind := range []string{"missing digest", "tampered digest", "state", "context", "arguments", "stored plan", "incomplete steps"} {
		t.Run(kind, func(t *testing.T) {
			prepared := &preparedCommand{
				Plan: domain.OperationPlan{
					OperationID: "fixture", Command: "fixture",
					Steps: []domain.OperationStep{{
						ID: "fixture.apply", Target: "owned fixture", Observed: "absent", Desired: "present",
						Decision: domain.StepApply, Verify: "fixture is ready",
					}},
				},
				Arguments: []string{"apply"}, exactState: "owned state", stepsComplete: true,
			}
			prepared.Loaded.Context.YardName = "fixture"
			plan := bindExactOperationPlan(prepared, now)
			if plan.StepSchema != 1 {
				t.Fatalf("complete steps did not bind step schema: %#v", plan)
			}
			if err := validateExactOperationPlan(plan, prepared, plan.Digest, now); err != nil {
				t.Fatalf("unchanged binding rejected: %v", err)
			}
			digest := plan.Digest
			switch kind {
			case "missing digest":
				digest = ""
			case "tampered digest":
				digest = strings.Repeat("0", 64)
			case "state":
				prepared.exactState = "changed state"
			case "context":
				prepared.Loaded.Context.YardName = "another-yard"
			case "arguments":
				prepared.Arguments = []string{"different-action"}
			case "stored plan":
				prepared.Plan.Command = "different-command"
			case "incomplete steps":
				prepared.stepsComplete = false
			}
			if err := validateExactOperationPlan(plan, prepared, digest, now); err == nil || errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("changed binding error=%v, want a binding rejection", err)
			}
		})
	}
}

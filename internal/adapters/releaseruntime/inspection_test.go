package releaseruntime

import (
	"reflect"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

func TestInstalledReleaseInspectionClassifiesTypedBlockers(t *testing.T) {
	pending := releasetransition.Blocker{Code: releasetransition.CodeDependencyUnavailable, Resource: "activation.sample.pending-configuration", Message: "sample waits for init", Retry: "yard -Y a init"}
	report := currentReport{
		Outcome: releasetransition.Outcome{Status: releasetransition.StatusOperatorActionRequired, Warnings: []string{"first", "second"}},
		Blockers: []releasetransition.Blocker{pending, {Code: releasetransition.CodePlanStale, Resource: "transition.journal", Message: "stale"},
			{Code: releasetransition.CodePlanStale, Resource: "transition.journal", Message: "still stale", Retry: "yard migrate"}},
	}
	want := []domain.Inspected{
		{Key: "dependency-unavailable.activation.sample.pending-configuration", State: domain.InspectedPending, Resource: pending.Resource, Message: pending.Message, Next: pending.Retry},
		{Key: "plan-stale.transition.journal", State: domain.InspectedBlocked, Resource: "transition.journal", Message: "stale; still stale", Next: "yard migrate"},
		{Key: "notices", State: domain.InspectedNotice, Resource: "release", Message: "first; second"},
	}
	if got := report.inspect(); !reflect.DeepEqual(got, want) {
		t.Fatalf("inspection=%#v", got)
	}
	ready := currentReport{Outcome: releasetransition.Outcome{Status: releasetransition.StatusReady}}
	if got := ready.inspect(); len(got) != 0 {
		t.Fatalf("ready release reported %#v", got)
	}
}

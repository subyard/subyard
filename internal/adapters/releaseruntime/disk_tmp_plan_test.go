package releaseruntime

import (
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/migration"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

func TestDiskTmpDisclosureReachesReleasedStepProjection(t *testing.T) {
	for _, running := range []bool{true, false} {
		text := migration.DiskTmpDisclosure(strings.Repeat("a", 123), running)
		decision := releasetransition.DecisionTransform
		if !running {
			decision = releasetransition.DecisionPreserve
		}
		steps := releaseOperationSteps(releasetransition.ProcessRequest{}, releasetransition.Inspection{Decisions: []releasetransition.RedactedDecision{{Resource: "yard.synthetic", Scope: "guest-disk-tmp", Decision: decision, Result: text}}, Outcome: &releasetransition.Outcome{}})
		if len(text) > 256 || len(steps) != 2 || !strings.Contains(steps[0].Desired, text) || steps[0].Decision == domain.StepConditional {
			t.Fatalf("lost restart disclosure: %#v", steps)
		}
	}
}

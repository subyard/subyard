package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

type pendingIntegrationFinding string

func (finding pendingIntegrationFinding) Error() string                         { return string(finding) }
func (finding pendingIntegrationFinding) PendingIntegrationConfiguration() bool { return true }

// The materialized-config reconciler returns its own activation aggregate. A
// blocker is pending configuration only when every cause of that aggregate is.
func TestActivationAggregateIsPendingOnlyWhenEveryCauseIs(t *testing.T) {
	finding := func(yard string, cause error) error {
		return materializedConfigActivationError{cause: cause, yard: yard, phase: "integration plan", action: "inspect", command: "integration status"}
	}
	for name, scenario := range map[string]struct {
		causes  []error
		pending bool
	}{
		"all-pending": {[]error{finding("default", pendingIntegrationFinding("adoption pending")), finding("work", pendingIntegrationFinding("adoption pending"))}, true},
		"mixed":       {[]error{finding("default", pendingIntegrationFinding("adoption pending")), finding("work", errors.New("transport failed"))}, false},
	} {
		t.Run(name, func(t *testing.T) {
			cause := collectActivationRefusals(scenario.causes)
			if _, ok := cause.(*activationRefusals); !ok {
				t.Fatalf("fixture is not the reconciler's aggregate: %T", cause)
			}
			transition, err := releasetransition.NewV2Transition(releasetransition.V2Options{
				ConfigHome: testkit.TempDir(t), Releases: releasetransition.ReleasePair{From: "release-a", Target: "release-a"},
				RegistryPayload:     []byte(`{"schemaVersion":2,"minimumEpochs":{"settings":1},"currentEpochs":{"settings":1},"migrations":[]}`),
				ArtifactDigest:      releasetransition.Fingerprint(strings.Repeat("d", 64)),
				VerifyAuthorization: func(releasetransition.PlanToken, releasetransition.Authorization) bool { return true },
				Reconcilers:         []releasetransition.V2ActivationReconciler{diagnosticFailingReconciler{cause: cause}},
				ObserveLinks: func(context.Context) (releasetransition.ReleaseLinks, error) {
					return releasetransition.ReleaseLinks{Active: "release-a"}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			inspection, err := transition.Inspect(context.Background(), releasetransition.Goal{Target: "release-a", Direction: releasetransition.DirectionActivateTarget})
			if err != nil || len(inspection.Blockers) == 0 {
				t.Fatalf("inspection: %#v %v", inspection, err)
			}
			for _, blocker := range inspection.Blockers {
				if pending := strings.HasSuffix(blocker.Resource, ".pending-configuration"); pending != scenario.pending {
					t.Fatalf("blocker %q pending=%t, want %t", blocker.Resource, pending, scenario.pending)
				}
			}
		})
	}
}

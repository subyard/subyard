package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

type pendingIntegrationFinding string

func (finding pendingIntegrationFinding) Error() string                         { return string(finding) }
func (finding pendingIntegrationFinding) PendingIntegrationConfiguration() bool { return true }

// unboundedPendingFinding is pending configuration whose public diagnostic
// exceeds the observation protocol bound.
type unboundedPendingFinding struct{}

func (unboundedPendingFinding) Error() string                        { return "unbounded finding" }
func (unboundedPendingFinding) ActivationPendingConfiguration() bool { return true }
func (unboundedPendingFinding) ActivationDiagnostic() (string, string) {
	return strings.Repeat("x", 600), "'yard' '-Y' 'default' 'init'"
}

// deferPendingFixture decides deferral for the pending findings of the default
// yard beside the work yard, which drifts when drift is set. An unassessed
// default yard has no desired configuration to keep.
func deferPendingFixture(t *testing.T, findings []error, deferring, drift, assessed bool) ([]releasetransition.V2PendingConfiguration, error) {
	t.Helper()
	reconciler := &materializedConfigActivationReconciler{}
	var deferred *config.MaterializationActivationTarget
	if assessed {
		deferred = &config.MaterializationActivationTarget{Name: "default", Assessment: config.MaterializationAssessment{State: "deferred", DesiredFingerprint: strings.Repeat("a", 64)}}
	}
	targets := []config.MaterializationActivationTarget{{Name: "default"}, {Name: "work", Assessment: config.MaterializationAssessment{Changed: drift}}}
	consequences := []string{}
	return reconciler.deferPendingYards(findings, []yardObservationSpan{{name: "default", deferred: deferred}, {name: "work", refusals: len(findings)}}, &targets, &consequences, deferring)
}

func deferPendingRefusal(t *testing.T, finding error, deferring, drift, assessed bool) error {
	t.Helper()
	pending, err := deferPendingFixture(t, []error{finding}, deferring, drift, assessed)
	if err == nil {
		t.Fatalf("pending yard was deferred: %#v", pending)
	}
	return err
}

// namedPendingFinding is pending configuration with a given public diagnostic.
type namedPendingFinding struct{ message, retry string }

func (finding namedPendingFinding) Error() string                { return finding.message }
func (namedPendingFinding) ActivationPendingConfiguration() bool { return true }
func (finding namedPendingFinding) ActivationDiagnostic() (string, string) {
	return finding.message, finding.retry
}

type unsafePendingFinding struct{}

func (unsafePendingFinding) Error() string                        { return "unsafe finding" }
func (unsafePendingFinding) ActivationPendingConfiguration() bool { return true }
func (unsafePendingFinding) ActivationDiagnostic() (string, string) {
	return "persist adoption of \"/home/dev/a\nb\" is pending", "rm -rf x; yard init"
}

// Release repair reports one finding per deferred yard, whatever the number or
// size of its refusals: the deferral record is exactly the set of yards with a
// finding, and each finding's next step is its own yard's init only when every
// refusal names it, so it decodes as that yard's init and never another's.
func TestReleaseRepairReportsOneFindingPerDeferredYard(t *testing.T) {
	keep := func(name string) *config.MaterializationActivationTarget {
		return &config.MaterializationActivationTarget{Name: name, Assessment: config.MaterializationAssessment{State: "deferred", DesiredFingerprint: strings.Repeat("a", 64)}}
	}
	release := func(yard string, index int) error {
		return pendingInitOwnershipRelease{yard: yard, paths: []string{fmt.Sprintf("/home/dev/%d", index)}}
	}
	for name, scenario := range map[string]struct {
		b     []error
		retry map[string]string
	}{
		// 300 refusals of a, then one of b: both yards keep their own init.
		"overflow-then-other-yard": {[]error{release("b", 0)}, map[string]string{"a": "'yard' '-Y' 'a' 'init'", "b": "'yard' '-Y' 'b' 'init'"}},
		// Oversized text and an unsafe step or another yard's init never name an init.
		"oversized-and-unsafe": {[]error{unboundedPendingFinding{}, unsafePendingFinding{}}, map[string]string{"a": "'yard' '-Y' 'a' 'init'", "b": "run yard -Y b integration status"}},
		// A yard named later in the text still leads its finding.
		"named-later": {[]error{namedPendingFinding{`persist adoption of "/home/dev/x" in yard b is pending`, "'yard' '-Y' 'b' 'init'"}}, map[string]string{"a": "'yard' '-Y' 'a' 'init'", "b": "'yard' '-Y' 'b' 'init'"}},
		// The first usable step across all of the yard's refusals is chosen.
		"other-yard-init-then-step": {[]error{namedPendingFinding{"yard b: first", "'yard' '-Y' 'default' 'init'"}, namedPendingFinding{"yard b: second", "run yard -Y b config show"}}, map[string]string{"a": "'yard' '-Y' 'a' 'init'", "b": "run yard -Y b config show"}},
		"unsafe-then-step":          {[]error{namedPendingFinding{"yard b: first", "rm x; yard init"}, namedPendingFinding{"yard b: second", "run yard -Y b config show"}}, map[string]string{"a": "'yard' '-Y' 'a' 'init'", "b": "run yard -Y b config show"}},
		"other-step":                {[]error{materializedConfigActivationError{cause: pendingIntegrationFinding("x"), yard: "b", phase: "integration plan", action: "inspect", command: "integration status"}, release("b", 0)}, map[string]string{"a": "'yard' '-Y' 'a' 'init'", "b": "run yard -Y b integration status"}},
	} {
		t.Run(name, func(t *testing.T) {
			var refusals []error
			for index := range 300 {
				refusals = append(refusals, release("a", index))
			}
			first := len(refusals)
			refusals = append(refusals, scenario.b...)
			targets := []config.MaterializationActivationTarget{{Name: "a"}, {Name: "b"}, {Name: "work"}}
			consequences := []string{}
			reconciler := &materializedConfigActivationReconciler{}
			spans := []yardObservationSpan{{name: "a", deferred: keep("a")}, {name: "b", refusals: first, deferred: keep("b")}, {name: "work", refusals: len(refusals)}}
			findings, err := reconciler.deferPendingYards(refusals, spans, &targets, &consequences, true)
			if err != nil || len(findings) != 2 {
				t.Fatalf("findings=%#v err=%v", findings, err)
			}
			blockers := map[string]bool{}
			for index, yard := range []string{"a", "b"} {
				finding := findings[index]
				if finding.Validate() != nil || !strings.HasPrefix(finding.Message, "yard "+yard+": ") || finding.Retry != scenario.retry[yard] {
					t.Fatalf("yard %s finding=%#v", yard, finding)
				}
				blockers[yard] = true
				initYards, ok := pendingInitYards(&releasetransition.Inspection{Blockers: []releasetransition.Blocker{{Retry: finding.Retry}}})
				if ok != strings.HasSuffix(finding.Retry, "'init'") || ok && (len(initYards) != 1 || !initYards[yard]) {
					t.Fatalf("yard %s next step %q decodes as %v", yard, finding.Retry, initYards)
				}
			}
			if count := findings[0].Message; count != "yard a: 300 pending configuration findings; full details: yard -Y a integration status" {
				t.Fatalf("oversized yard finding=%q", count)
			}
			if !maps.Equal(reconciler.pendingDeferral().decided(context.Background(), releasetransition.ReleasePair{}), blockers) {
				t.Fatalf("deferral record differs from the pending yards: %v", blockers)
			}
		})
	}
	// More pending yards than the protocol reports cannot be deferred.
	refusals, spans := []error{}, []yardObservationSpan{}
	targets, consequences := []config.MaterializationActivationTarget{}, []string{}
	for index := range releasetransition.MaxPlanItems + 1 {
		yard := fmt.Sprintf("y%d", index)
		spans = append(spans, yardObservationSpan{name: yard, refusals: len(refusals), deferred: keep(yard)})
		refusals = append(refusals, release(yard, 0))
	}
	reconciler := &materializedConfigActivationReconciler{}
	if findings, err := reconciler.deferPendingYards(refusals, spans, &targets, &consequences, true); err == nil || reconciler.pendingDeferral().deferred("y0") {
		t.Fatalf("deferred beyond the protocol bound: %d %v", len(findings), err)
	}
}

// The materialized-config reconciler returns its own activation aggregate. A
// blocker is pending configuration only when every cause of that aggregate is.
func TestActivationAggregateIsPendingOnlyWhenEveryCauseIs(t *testing.T) {
	finding := func(yard string, cause error) error {
		return materializedConfigActivationError{cause: cause, yard: yard, phase: "integration plan", action: "inspect", command: "integration status"}
	}
	adoption := finding("default", pendingIntegrationFinding("adoption pending"))
	for name, scenario := range map[string]struct {
		causes  []error
		cause   func(*testing.T) error
		pending bool
		retry   string
		message string
	}{
		"all-pending": {causes: []error{adoption, finding("work", pendingIntegrationFinding("adoption pending"))}, pending: true},
		"mixed":       {causes: []error{adoption, finding("work", errors.New("transport failed"))}},
		// Without release repair the pending yard is not deferred: the activation
		// stops with the pending refusal and its own next step, with or without
		// other yards' changes.
		"undeferred-beside-drift": {cause: func(t *testing.T) error { return deferPendingRefusal(t, adoption, false, true, true) }, pending: true, retry: "run yard -Y default integration status"},
		"undeferred-alone":        {cause: func(t *testing.T) error { return deferPendingRefusal(t, adoption, false, false, true) }, pending: true, retry: "run yard -Y default integration status"},
		// A yard release repair cannot keep in Desired is never pending
		// configuration, with or without other drift, and names a read-only next step.
		"unassessed-alone": {cause: func(t *testing.T) error { return deferPendingRefusal(t, adoption, true, false, false) }, retry: "run yard migrate --check"},
	} {
		t.Run(name, func(t *testing.T) {
			var cause error
			if scenario.cause != nil {
				cause = scenario.cause(t)
			} else if cause = collectActivationRefusals(scenario.causes); true {
				if _, ok := cause.(*activationRefusals); !ok {
					t.Fatalf("fixture is not the reconciler's aggregate: %T", cause)
				}
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
				if !strings.Contains(blocker.Message, scenario.message) {
					t.Fatalf("blocker message %q lacks %q", blocker.Message, scenario.message)
				}
				if scenario.retry != "" && blocker.Retry != scenario.retry {
					t.Fatalf("blocker next step %q, want %q: %s", blocker.Retry, scenario.retry, blocker.Message)
				}
			}
		})
	}
}

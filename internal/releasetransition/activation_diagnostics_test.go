package releasetransition

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type activationDiagnosticPostApplyFixture struct {
	*v2TestReconciler
	post                          V2ActivationObservation
	postErr                       error
	onPost                        func()
	applied                       bool
	errorAt, postAt, postObserves int
}

func (fixture *activationDiagnosticPostApplyFixture) Observe(ctx context.Context, releases ReleasePair, links ReleaseLinks) (V2ActivationObservation, error) {
	if fixture.applied {
		fixture.postObserves++
		if fixture.onPost != nil {
			fixture.onPost()
		}
		if fixture.postObserves < fixture.postAt {
			return V2ActivationObservation{Actual: digestA, Desired: digestA, Converged: true}, nil
		}
		if fixture.errorAt == 0 || fixture.postObserves >= fixture.errorAt {
			return fixture.post, fixture.postErr
		}
		return fixture.post, nil
	}
	return fixture.v2TestReconciler.Observe(ctx, releases, links)
}

func TestActivationDiagnosticsPreserveVerificationCauseBehindLinkGuard(t *testing.T) {
	for _, errorAt := range []int{1, 2} {
		for _, unavailable := range []bool{false, true} {
			t.Run(fmt.Sprintf("post-observation=%d/unavailable=%t", errorAt, unavailable), func(t *testing.T) {
				transition, _, _ := v2TransitionFixture(t, nil)
				diagnostic := v2PublicActivationError{"yard named: config verification failed", "run yard -Y named config status"}
				reconciler := &activationDiagnosticPostApplyFixture{
					v2TestReconciler: &v2TestReconciler{}, errorAt: errorAt, postErr: diagnostic,
					post: V2ActivationObservation{Actual: digestA, Desired: digestA, Converged: true},
				}
				reconciler.onPost = func() {
					if reconciler.postObserves == errorAt {
						transition.options.ObserveLinks = func(context.Context) (ReleaseLinks, error) {
							if unavailable {
								return ReleaseLinks{}, errors.New("synthetic-private-link-failure")
							}
							return ReleaseLinks{Active: "foreign"}, nil
						}
					}
				}
				transition.options.Reconcilers = []V2ActivationReconciler{reconciler}
				inspection, err := transition.Inspect(context.Background(), Goal{Target: "release-a", Direction: DirectionActivateTarget})
				if err != nil {
					t.Fatal(err)
				}
				outcome, err := transition.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
				wantCode := CodeActivationAmbiguous
				if unavailable {
					wantCode = CodeRecoveryAmbiguous
				}
				if err != nil || outcome.Status != StatusOperatorActionRequired || outcome.Code != wantCode ||
					!strings.Contains(outcome.Message, diagnostic.message) || !strings.Contains(outcome.Message, "link") ||
					strings.Contains(outcome.Message, "synthetic-private") {
					t.Fatalf("verification guard lost primary cause: %#v %v", outcome, err)
				}
			})
		}
	}
}

func (fixture *activationDiagnosticPostApplyFixture) Reconcile(context.Context, ReleaseLinks) error {
	fixture.applied = true
	fixture.reconciles++
	return nil
}

func TestActivationDiagnosticsPreservePostApplyObservationCause(t *testing.T) {
	for _, errorAt := range []int{1, 2} {
		t.Run(fmt.Sprintf("post-observation=%d", errorAt), func(t *testing.T) {
			transition, _, _ := v2TransitionFixture(t, nil)
			diagnostic := v2PublicActivationError{"yard named: profile runtime verification failed", "run yard -Y named status"}
			reconciler := &activationDiagnosticPostApplyFixture{
				v2TestReconciler: &v2TestReconciler{},
				postErr:          fmt.Errorf("synthetic-private-wrapper: %w", diagnostic),
				post:             V2ActivationObservation{Actual: digestA, Desired: digestA, Converged: true},
				errorAt:          errorAt,
			}
			transition.options.Reconcilers = []V2ActivationReconciler{reconciler}
			goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
			inspection, err := transition.Inspect(context.Background(), goal)
			if err != nil || len(inspection.Blockers) != 0 {
				t.Fatalf("inspection = %#v, error=%v", inspection, err)
			}
			outcome, err := transition.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
			if err != nil || reconciler.reconciles != 1 || outcome.Status != StatusRecovering || outcome.Code != CodeDependencyUnavailable || outcome.Message != diagnostic.message || outcome.Retry != diagnostic.retry || outcome.Transaction == nil || strings.Contains(outcome.Message, "synthetic-private") {
				t.Fatalf("successful apply lost its verification diagnostic: %#v, error=%v", outcome, err)
			}
			if err := ValidateProcessConvergence(goal, inspection, outcome); err != nil {
				t.Fatalf("invalid public outcome: %v", err)
			}
		})
	}
}

func TestActivationDiagnosticsDistinguishInvalidObservationFromRemainingDrift(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		for _, postAt := range []int{1, 2} {
			t.Run(fmt.Sprintf("invalid=%t/post-observation=%d", invalid, postAt), func(t *testing.T) {
				transition, _, _ := v2TransitionFixture(t, nil)
				post := V2ActivationObservation{Actual: digestB, Desired: digestA, Converged: false}
				if invalid {
					post.Actual = "synthetic-private-invalid-fingerprint"
				}
				reconciler := &activationDiagnosticPostApplyFixture{v2TestReconciler: &v2TestReconciler{}, post: post, postAt: postAt}
				transition.options.Reconcilers = []V2ActivationReconciler{reconciler}
				goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
				inspection, err := transition.Inspect(context.Background(), goal)
				if err != nil {
					t.Fatal(err)
				}
				outcome, err := transition.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
				if err != nil || reconciler.reconciles != 1 || outcome.Transaction == nil || strings.Contains(outcome.Message, "synthetic-private") {
					t.Fatalf("invalid verification outcome: %#v, error=%v", outcome, err)
				}
				if invalid {
					if outcome.Status != StatusOperatorActionRequired || outcome.Code != CodeRecoveryAmbiguous || (!strings.Contains(outcome.Message, "invalid") || !strings.Contains(outcome.Message, "verification")) {
						t.Fatalf("invalid observation was downgraded to drift: %#v", outcome)
					}
				} else if outcome.Status != StatusRecovering || outcome.Code != CodeDependencyUnavailable || (!strings.Contains(outcome.Message, "fixed point") || !strings.Contains(outcome.Message, "verification")) {
					t.Fatalf("remaining drift was classified as invalid: %#v", outcome)
				}
			})
		}
	}
}

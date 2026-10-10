package releasetransition

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

type sourceScopeReconciler struct {
	wide        bool
	wideDesired Fingerprint
	converged   bool
	interrupt   bool
	hooks       int
	restores    int
	applies     int
}

func (*sourceScopeReconciler) ID() string { return "synthetic-assets" }

func (reconciler *sourceScopeReconciler) UseSourceStableActivationScope() func() {
	previous := reconciler.wide
	reconciler.wide = true
	reconciler.hooks++
	return func() {
		reconciler.wide = previous
		reconciler.restores++
	}
}

func (reconciler *sourceScopeReconciler) Observe(context.Context, ReleasePair, ReleaseLinks) (V2ActivationObservation, error) {
	desired := digestA
	if reconciler.wide {
		desired = reconciler.wideDesired
		if desired == "" {
			desired = digestB
		}
	}
	actual := digestD
	if reconciler.converged {
		actual = desired
	}
	return V2ActivationObservation{
		Actual: actual, Desired: desired, Converged: reconciler.converged,
		Consequences: []string{"reconcile synthetic assets"},
	}, nil
}

func (reconciler *sourceScopeReconciler) Reconcile(context.Context, ReleaseLinks) error {
	reconciler.applies++
	if reconciler.interrupt {
		return errors.New("synthetic activation interrupted")
	}
	reconciler.converged = true
	return nil
}

// A historical owner implements the original activation contract only.
type historicalSourceScopeReconciler struct{ reconciler *sourceScopeReconciler }

func (owner historicalSourceScopeReconciler) ID() string { return owner.reconciler.ID() }
func (owner historicalSourceScopeReconciler) Observe(ctx context.Context, pair ReleasePair, links ReleaseLinks) (V2ActivationObservation, error) {
	return owner.reconciler.Observe(ctx, pair, links)
}
func (owner historicalSourceScopeReconciler) Reconcile(ctx context.Context, links ReleaseLinks) error {
	return owner.reconciler.Reconcile(ctx, links)
}

func sourceScopeTransitionFixture(t *testing.T, owner V2ActivationReconciler) *V2Transition {
	t.Helper()
	transition, _, settings := v2TransitionFixtureWithReleases(t, nil,
		ReleasePair{From: "release-a", Target: "release-b"}, ReleaseLinks{Active: "release-a"})
	// Canonical generic settings make pending source work ledger-only.
	testkit.WriteFile(t, settings, []byte("SSH_PORT=2224\n"), 0o600)
	links := ReleaseLinks{Active: "release-a"}
	transition.options.ObserveLinks = func(context.Context) (ReleaseLinks, error) { return links, nil }
	transition.options.ActivateLinks = func(_ context.Context, pair ReleasePair) (ReleaseLinks, error) {
		links = ReleaseLinks{Active: pair.Target, Previous: releaseIDPointer(pair.From)}
		return links, nil
	}
	transition.options.Reconcilers = []V2ActivationReconciler{owner}
	return transition
}

func freshSourceScopeOwner(t *testing.T, original *V2Transition, owner V2ActivationReconciler) *V2Transition {
	t.Helper()
	options := original.options
	options.Reconcilers = []V2ActivationReconciler{owner}
	options.fault = nil
	options.VerifyAuthorization = func(PlanToken, Authorization) bool {
		t.Error("exact resume asked for a new grant")
		return false
	}
	fresh, err := NewV2Transition(options)
	if err != nil {
		t.Fatal(err)
	}
	return fresh
}

func sourceScopeJournal(t *testing.T, transition *V2Transition) JournalRecord {
	t.Helper()
	snapshot, err := transition.store.ReadCurrentJournal()
	if err != nil || !snapshot.Exists {
		t.Fatalf("read authorized scope journal: exists=%t err=%v", snapshot.Exists, err)
	}
	journal, err := ParseJournal(snapshot.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(journal.Steps) == 0 {
		t.Fatal("fixture did not prepare pending ledger work")
	}
	for _, step := range journal.Steps {
		if !strings.HasPrefix(step.Resource, "ledger.") {
			t.Fatalf("fixture prepared non-ledger source work: %s", step.Resource)
		}
	}
	return journal
}

func interruptSourceScopeTransition(t *testing.T, transition *V2Transition) JournalRecord {
	t.Helper()
	inspection, err := transition.Inspect(context.Background(), Goal{Target: "release-b", Direction: DirectionActivateTarget})
	if err != nil || len(inspection.Blockers) != 0 {
		t.Fatalf("inspect synthetic activation: blockers=%v err=%v", inspection.Blockers, err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	if err != nil || outcome.Status != StatusRecovering {
		t.Fatalf("interrupt synthetic activation: outcome=%#v err=%v", outcome, err)
	}
	journal := sourceScopeJournal(t, transition)
	if journal.Checkpoint != JournalReconciling {
		t.Fatalf("interrupted checkpoint = %s", journal.Checkpoint)
	}
	return journal
}

func TestSourceStableActivationScopeIncludedBeforeGrant(t *testing.T) {
	owner := &sourceScopeReconciler{}
	transition := sourceScopeTransitionFixture(t, owner)
	goal := Goal{Target: "release-b", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil || len(inspection.Blockers) != 0 || !owner.wide || owner.applies != 0 {
		t.Fatalf("pre-grant assessment: wide=%t applies=%d blockers=%v err=%v", owner.wide, owner.applies, inspection.Blockers, err)
	}
	before, err := transition.store.ReadCurrentJournal()
	if err != nil || before.Exists {
		t.Fatalf("inspection published authorization: exists=%t err=%v", before.Exists, err)
	}
	grants := 0
	transition.options.VerifyAuthorization = func(plan PlanToken, grant Authorization) bool {
		grants++
		if !owner.wide || plan != inspection.Plan {
			t.Error("grant did not bind the original wide assessment")
		}
		return grant == v2TestAuthorization(plan)
	}
	outcome, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	if err != nil || outcome.Status != StatusReady || grants != 1 || owner.applies != 1 {
		t.Fatalf("wide convergence: outcome=%#v grants=%d applies=%d err=%v", outcome, grants, owner.applies, err)
	}
	sourceScopeJournal(t, transition)
	final, err := transition.Inspect(context.Background(), goal)
	if err != nil || final.Outcome.Status != StatusReady {
		t.Fatalf("wide final readiness: outcome=%#v err=%v", final.Outcome, err)
	}
}

func TestSourceStableActivationScopeExcludesActualSourceMutations(t *testing.T) {
	for _, kind := range []v2WorkKind{v2SettingsWork, v2OwnerWork, v2IngressWork} {
		for _, durable := range []bool{false, true} {
			name := string(kind) + "/prepared"
			if durable {
				name = string(kind) + "/durable"
			}
			t.Run(name, func(t *testing.T) {
				owner := &sourceScopeReconciler{}
				transition := sourceScopeTransitionFixture(t, owner)
				observation := v2Observation{work: []v2Work{{kind: v2LedgerWork}, {kind: kind}}}
				if durable {
					observation.work = nil
					observation.journal = &JournalRecord{
						Checkpoint: JournalReconciling, ObservationScope: digestD,
						Steps: []JournalStep{{Resource: "source." + string(kind), Checkpoint: StepVerified}},
					}
				}
				if err := transition.observeActivationScope(context.Background(), &observation); err != nil {
					t.Fatal(err)
				}
				if owner.hooks != 0 || owner.wide || observation.activationScope[0].Desired != digestA {
					t.Fatalf("source mutation expanded authorization: hooks=%d wide=%t scope=%v", owner.hooks, owner.wide, observation.activationScope)
				}
			})
		}
	}
}

func TestCheckpointPublicationPreservesMigratingSourceScope(t *testing.T) {
	for _, interrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "resume"}[interrupt], func(t *testing.T) {
			owner := &sourceScopeReconciler{}
			transition := sourceScopeTransitionFixture(t, owner)
			testkit.WriteFile(t, filepath.Join(transition.options.ConfigHome, "yards", "hermes", "config.env"),
				[]byte("YARD_TEMPLATE=e2e-vms\nNESTED_E2E_VMS=0\nSSH_PORT=2224\n"), 0o600)
			transition.options.MigrationCheckpoint = true
			if interrupt {
				transition.options.fault = func(point string) error {
					if point == "before-checkpoint-publication" {
						return errors.New("interrupt terminal checkpoint")
					}
					return nil
				}
			}
			goal := Goal{Target: "release-b", Direction: DirectionActivateTarget}
			inspection, err := transition.Inspect(context.Background(), goal)
			if err != nil || len(inspection.Blockers) != 0 || owner.wide {
				t.Fatalf("source migration assessment: %#v wide=%t err=%v", inspection, owner.wide, err)
			}
			outcome, err := transition.Converge(context.Background(), Execution{
				Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
			})
			if interrupt {
				if err != nil || outcome.Status != StatusRecovering {
					t.Fatalf("checkpoint interruption: %#v err=%v", outcome, err)
				}
				transition = freshSourceScopeOwner(t, transition, &sourceScopeReconciler{converged: true})
				inspection, err = transition.Inspect(context.Background(), goal)
				if err != nil || inspection.Resume == nil || len(inspection.Blockers) != 0 {
					t.Fatalf("checkpoint resume: %#v err=%v", inspection, err)
				}
				outcome, err = transition.Converge(context.Background(), Execution{Plan: inspection.Plan})
			}
			if err != nil || outcome.Status != StatusReady {
				t.Fatalf("checkpoint completion: %#v err=%v", outcome, err)
			}
			checkpoint, err := transition.store.ReadMigrationCheckpoint()
			if err != nil || !checkpoint.Exists {
				t.Fatalf("authorized checkpoint missing: %#v err=%v", checkpoint, err)
			}
			final, err := transition.Inspect(context.Background(), goal)
			if err != nil || final.Outcome.Status != StatusReady || final.Assessment.Changed {
				t.Fatalf("completed release-wide fixed point: %#v err=%v", final, err)
			}
		})
	}
}

func TestSourceStableActivationScopePreservesHistoricalNarrowJournal(t *testing.T) {
	originalOwner := &sourceScopeReconciler{interrupt: true}
	transition := sourceScopeTransitionFixture(t, historicalSourceScopeReconciler{originalOwner})
	journal := interruptSourceScopeTransition(t, transition)
	owner := &sourceScopeReconciler{}
	fresh := freshSourceScopeOwner(t, transition, owner)
	inspection, err := fresh.Inspect(context.Background(), journal.Goal)
	if err != nil || inspection.Resume == nil || len(inspection.Blockers) != 0 || owner.wide || owner.hooks != 0 {
		t.Fatalf("historical narrow resume: wide=%t hooks=%d blockers=%v err=%v", owner.wide, owner.hooks, inspection.Blockers, err)
	}
	outcome, err := fresh.Converge(context.Background(), Execution{Plan: inspection.Plan})
	if err != nil || outcome.Status != StatusReady || owner.wide || owner.applies != 1 {
		t.Fatalf("historical narrow convergence: outcome=%#v wide=%t applies=%d err=%v", outcome, owner.wide, owner.applies, err)
	}
}

func TestSourceStableActivationScopeResumesWideJournalWithFreshOwner(t *testing.T) {
	transition := sourceScopeTransitionFixture(t, &sourceScopeReconciler{interrupt: true})
	journal := interruptSourceScopeTransition(t, transition)
	owner := &sourceScopeReconciler{}
	fresh := freshSourceScopeOwner(t, transition, owner)
	inspection, err := fresh.Inspect(context.Background(), journal.Goal)
	if err != nil || inspection.Resume == nil || len(inspection.Blockers) != 0 || !owner.wide || owner.hooks == 0 || owner.restores != 0 {
		t.Fatalf("wide exact-scope fallback: wide=%t hooks=%d restores=%d blockers=%v err=%v", owner.wide, owner.hooks, owner.restores, inspection.Blockers, err)
	}
	outcome, err := fresh.Converge(context.Background(), Execution{Plan: inspection.Plan})
	if err != nil || outcome.Status != StatusReady || owner.applies != 1 {
		t.Fatalf("wide resumed convergence: outcome=%#v applies=%d err=%v", outcome, owner.applies, err)
	}
}

func TestSourceStableActivationScopeRejectsChangedWideDesired(t *testing.T) {
	transition := sourceScopeTransitionFixture(t, &sourceScopeReconciler{interrupt: true})
	journal := interruptSourceScopeTransition(t, transition)
	beforeJournal, err := transition.store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	beforeLedger, err := transition.store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	owner := &sourceScopeReconciler{wideDesired: digestC}
	fresh := freshSourceScopeOwner(t, transition, owner)
	inspection, err := fresh.Inspect(context.Background(), journal.Goal)
	if err != nil || inspection.Resume == nil || owner.wide || owner.hooks == 0 || owner.restores != owner.hooks {
		t.Fatalf("changed wide scope fallback: wide=%t hooks=%d restores=%d err=%v", owner.wide, owner.hooks, owner.restores, err)
	}
	outcome, err := fresh.Converge(context.Background(), Execution{Plan: inspection.Plan})
	if err != nil || outcome.Status != StatusOperatorActionRequired || outcome.Code != CodePlanStale || owner.applies != 0 {
		t.Fatalf("changed wide grant rejection: outcome=%#v applies=%d err=%v", outcome, owner.applies, err)
	}
	afterJournal, err := fresh.store.ReadCurrentJournal()
	if err != nil || !bytes.Equal(beforeJournal.Payload, afterJournal.Payload) {
		t.Fatalf("changed desired scope mutated journal: err=%v", err)
	}
	afterLedger, err := fresh.store.ReadLedger()
	if err != nil || !bytes.Equal(beforeLedger.Payload, afterLedger.Payload) {
		t.Fatalf("changed desired scope mutated ledger: err=%v", err)
	}
	links, err := fresh.options.ObserveLinks(context.Background())
	if err != nil || links.Active != "release-b" || links.Previous == nil || *links.Previous != "release-a" {
		t.Fatalf("changed desired scope mutated links: links=%#v err=%v", links, err)
	}
}

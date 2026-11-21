package releasetransition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

// fileRollbackHooks keeps each owner's installed format in a file, so a
// crashed child process leaves exactly the effects it completed. Format 2 is
// the active release's, 1 the target's. Every apply is logged.
type fileRollbackHooks struct{ dir string }

func (hooks fileRollbackHooks) formats(t interface{ Fatal(...any) }) map[string]int {
	payload, err := os.ReadFile(filepath.Join(hooks.dir, "formats.json"))
	if err != nil {
		t.Fatal(err)
	}
	formats := map[string]int{}
	if err := json.Unmarshal(payload, &formats); err != nil {
		t.Fatal(err)
	}
	return formats
}

func (hooks fileRollbackHooks) read() (map[string]int, error) {
	payload, err := os.ReadFile(filepath.Join(hooks.dir, "formats.json"))
	if err != nil {
		return nil, err
	}
	formats := map[string]int{}
	return formats, json.Unmarshal(payload, &formats)
}

func (hooks fileRollbackHooks) Observe(_ context.Context, owner, yard string) (RollbackPrepItem, error) {
	formats, err := hooks.read()
	if err != nil {
		return RollbackPrepItem{}, err
	}
	item := RollbackPrepItem{Owner: owner, Yard: yard}
	switch formats[owner+"@"+yard] {
	case 0:
		item.State = RollbackPrepAbsent
	case 1:
		item.State, item.Actual, item.Desired = RollbackPrepCurrent, digestA, digestA
	default:
		item.State, item.Actual, item.Desired = RollbackPrepStale, digestB, digestA
	}
	return item, nil
}

func (hooks fileRollbackHooks) Apply(ctx context.Context, item RollbackPrepItem, operation TransactionID) error {
	observed, err := hooks.Observe(ctx, item.Owner, item.Yard)
	if err != nil || observed != item {
		return fmt.Errorf("%w: assessment changed", domain.ErrPlanStale)
	}
	formats, err := hooks.read()
	if err != nil {
		return err
	}
	formats[item.Owner+"@"+item.Yard] = 1
	payload, _ := json.Marshal(formats)
	if err := os.WriteFile(filepath.Join(hooks.dir, "formats.json"), payload, 0o600); err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(hooks.dir, "applies"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	_, err = fmt.Fprintf(log, "%s@%s %s\n", item.Owner, item.Yard, operation)
	return err
}

func (hooks fileRollbackHooks) applies(t *testing.T) []string {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join(hooks.dir, "applies"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(payload))
}

var rollbackPrepTargets = [][2]string{{"alpha-runtime", "default"}, {"beta-runtime", "default"}, {"alpha-runtime", "work"}}

type rollbackPrepFixture struct {
	configHome string
	hooks      fileRollbackHooks
	registry   []byte
}

func newRollbackPrepFixture(t *testing.T) rollbackPrepFixture {
	t.Helper()
	configHome := settingsV2Fixture(t, map[string]string{
		"yards/hermes/config.env": "YARD_TEMPLATE=e2e-vms\nNESTED_E2E_VMS=0\n",
	})
	seedAppliedV2Ledger(t, configHome)
	hooks := fileRollbackHooks{dir: testkit.TempDir(t)}
	formats, _ := json.Marshal(map[string]int{"alpha-runtime@default": 2, "beta-runtime@default": 1, "alpha-runtime@work": 2})
	testkit.WriteFile(t, filepath.Join(hooks.dir, "formats.json"), formats, 0o600)
	return rollbackPrepFixture{configHome: configHome, hooks: hooks, registry: v2RegistryPayload(t)}
}

func (fixture rollbackPrepFixture) plan(t *testing.T) []RollbackPrepItem {
	t.Helper()
	items := make([]RollbackPrepItem, 0, len(rollbackPrepTargets))
	for _, target := range rollbackPrepTargets {
		item, err := fixture.hooks.Observe(context.Background(), target[0], target[1])
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, item)
	}
	return items
}

func (fixture rollbackPrepFixture) prep(items []RollbackPrepItem, gate, dispatch func(context.Context) error) RollbackPrep {
	return RollbackPrep{
		ConfigHome: fixture.configHome, Active: "release-b", Target: "release-a",
		ArtifactDigest: digestA, RegistryDigest: fingerprintPayload(fixture.registry),
		Items: items, Hooks: fixture.hooks, Gate: gate, Dispatch: dispatch,
	}
}

// forward is the active release's own transition, as yard migrate runs it.
func (fixture rollbackPrepFixture) forward(t *testing.T, reconciler *v2TestReconciler) *V2Transition {
	t.Helper()
	links := ReleaseLinks{Active: "release-b", Previous: releaseIDPointer("release-a")}
	transition, err := NewV2Transition(V2Options{
		ConfigHome:   fixture.configHome,
		Releases:     ReleasePair{From: "release-b", Previous: releaseIDPointer("release-a"), Target: "release-b"},
		ObserveLinks: func(context.Context) (ReleaseLinks, error) { return links, nil },
		ActivateLinks: func(context.Context, ReleasePair) (ReleaseLinks, error) {
			return ReleaseLinks{}, errors.New("a same-release repair must not switch links")
		},
		Reconcilers: []V2ActivationReconciler{reconciler}, OwnerRegistration: &v2TestOwnerRegistration{state: OwnerRegistrationAbsent},
		RegistryPayload: fixture.registry, ArtifactDigest: digestA,
		NewTransactionID:    func() TransactionID { return "tx-forward-001" },
		VerifyAuthorization: func(PlanToken, Authorization) bool { return true },
	})
	if err != nil {
		t.Fatal(err)
	}
	return transition
}

// rollback is the target's own transition with an identical registry. It
// reads the down transaction's journal as completed history and switches.
func (fixture rollbackPrepFixture) rollback(t *testing.T, links *ReleaseLinks) error {
	transition, err := NewV2Transition(V2Options{
		ConfigHome: fixture.configHome,
		Releases:   ReleasePair{From: "release-b", Previous: releaseIDPointer("release-a"), Target: "release-a"},
		Direction:  DirectionActivatePrevious, RollbackTarget: &RollbackTarget{Version: "0.30.0", RegistryDigest: fingerprintPayload(fixture.registry)},
		ObserveLinks: func(context.Context) (ReleaseLinks, error) { return *links, nil },
		ActivateLinks: func(_ context.Context, pair ReleasePair) (ReleaseLinks, error) {
			*links = ReleaseLinks{Active: pair.Target, Previous: releaseIDPointer(pair.From)}
			return *links, nil
		},
		Reconcilers: []V2ActivationReconciler{&v2TestReconciler{}}, OwnerRegistration: &v2TestOwnerRegistration{state: OwnerRegistrationAbsent},
		RegistryPayload: fixture.registry, ArtifactDigest: digestC,
		NewTransactionID:    func() TransactionID { return "tx-rollback-001" },
		VerifyAuthorization: func(PlanToken, Authorization) bool { return true },
	})
	if err != nil {
		return err
	}
	goal := Goal{Target: "release-a", Direction: DirectionActivatePrevious}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		return err
	}
	if len(inspection.Blockers) != 0 || inspection.Resume != nil {
		return fmt.Errorf("target refused the prepared state: %#v", inspection.Outcome)
	}
	outcome, err := transition.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: "rollback-grant"})
	if err != nil {
		return err
	}
	if outcome.Status != StatusReady {
		return fmt.Errorf("target rollback outcome %#v", outcome)
	}
	return nil
}

func (fixture rollbackPrepFixture) currentJournal(t *testing.T) JournalRecord {
	t.Helper()
	store, err := NewPOSIXV2Store(fixture.configHome)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil || !snapshot.Exists {
		t.Fatalf("no current journal: %v", err)
	}
	journal, err := ParseJournal(snapshot.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return journal
}

func assertRollbackPrepJournal(t *testing.T, journal JournalRecord) {
	t.Helper()
	if !strings.HasPrefix(string(journal.Transaction), RollbackPrepTransactionPrefixV1) || journal.Checkpoint != JournalComplete || len(journal.Steps) != 0 ||
		journal.Goal != (Goal{Target: "release-b", Direction: DirectionActivateTarget}) ||
		!releasePairsEqual(journal.Releases, ReleasePair{From: "release-b", Previous: releaseIDPointer("release-a"), Target: "release-b"}) ||
		journal.ArtifactDigest != digestA {
		t.Fatalf("rollback preparation journal = %#v", journal)
	}
}

const rollbackPrepChildEnv = "SUBYARD_TEST_ROLLBACK_PREP_CHILD"

// TestRollbackPrepCrashChild is the crashing child of the harness below.
func TestRollbackPrepCrashChild(t *testing.T) {
	var spec struct{ ConfigHome, Hooks, Fault string }
	if json.Unmarshal([]byte(os.Getenv(rollbackPrepChildEnv)), &spec) != nil || spec.ConfigHome == "" {
		t.Skip("crash harness child")
	}
	fixture := rollbackPrepFixture{configHome: spec.ConfigHome, hooks: fileRollbackHooks{dir: spec.Hooks}, registry: v2RegistryPayload(t)}
	prep := fixture.prep(fixture.plan(t), func(context.Context) error { return nil }, func(context.Context) error {
		return os.WriteFile(filepath.Join(spec.Hooks, "dispatched"), nil, 0o600)
	})
	prep.fault = func(point string) error {
		if point == spec.Fault {
			os.Exit(97)
		}
		return nil
	}
	_ = prep.Run(context.Background(), "child-grant")
	t.Fatal("child passed its fault point")
}

func crashRollbackPrep(t *testing.T, fixture rollbackPrepFixture, fault string) {
	t.Helper()
	spec, _ := json.Marshal(map[string]string{"ConfigHome": fixture.configHome, "Hooks": fixture.hooks.dir, "Fault": fault})
	child := exec.Command(os.Args[0], "-test.run=^TestRollbackPrepCrashChild$", "-test.count=1")
	child.Env = append(os.Environ(), rollbackPrepChildEnv+"="+string(spec))
	output, err := child.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 97 {
		t.Fatalf("child did not crash at %s: %v\n%s", fault, err, output)
	}
}

// An interruption at either fault point leaves the active release active
// with a completed down transaction. Retrying the rollback resumes from the
// observed hooks and switches; yard migrate instead repairs forward.
func TestRollbackPrepInterruptionResumesRollbackOrMigratesForward(t *testing.T) {
	for _, fault := range []string{FaultAfterRollbackHook, FaultBeforeTargetDispatch} {
		for _, resume := range []string{"rollback", "migrate"} {
			t.Run(fault+"/"+resume, func(t *testing.T) {
				fixture := newRollbackPrepFixture(t)
				crashRollbackPrep(t, fixture, fault)
				assertRollbackPrepJournal(t, fixture.currentJournal(t))
				if _, err := os.Stat(filepath.Join(fixture.hooks.dir, "dispatched")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("an interrupted down transaction dispatched the rollback")
				}
				wantApplied := 2
				if fault == FaultAfterRollbackHook {
					wantApplied = 1
				}
				if applied := fixture.hooks.applies(t); len(applied)/2 != wantApplied {
					t.Fatalf("applied before %s: %v", fault, applied)
				}
				switch resume {
				case "rollback":
					links := ReleaseLinks{Active: "release-b", Previous: releaseIDPointer("release-a")}
					gates := 0
					prep := fixture.prep(fixture.plan(t), func(context.Context) error { gates++; return nil },
						func(context.Context) error { return fixture.rollback(t, &links) })
					if err := prep.Run(context.Background(), "retry-grant"); err != nil {
						t.Fatal(err)
					}
					if links.Active != "release-a" || gates != 1 {
						t.Fatalf("resumed rollback links=%#v gates=%d", links, gates)
					}
					if applied := fixture.hooks.applies(t); len(applied)/2 != 2 {
						t.Fatalf("a converted owner was applied again: %v", applied)
					}
					for key, format := range fixture.hooks.formats(t) {
						if format != 1 {
							t.Fatalf("%s kept format %d", key, format)
						}
					}
				case "migrate":
					// The converted owner now drifts from the active release.
					reconciler := &v2TestReconciler{}
					transition := fixture.forward(t, reconciler)
					goal := Goal{Target: "release-b", Direction: DirectionActivateTarget}
					inspection, err := transition.Inspect(context.Background(), goal)
					if err != nil || len(inspection.Blockers) != 0 || inspection.Resume != nil ||
						inspection.Outcome.Status != StatusMigrationRequired || inspection.Outcome.Code != CodeTransitionRequired {
						t.Fatalf("forward inspection after abandoned preparation = %#v, %v", inspection.Outcome, err)
					}
					outcome, err := transition.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: "migrate-grant"})
					if err != nil || outcome.Status != StatusReady || outcome.Active != "release-b" || reconciler.reconciles != 1 {
						t.Fatalf("forward repair = %#v, %v (reconciles %d)", outcome, err, reconciler.reconciles)
					}
				}
			})
		}
	}
}

func TestRollbackPrepRefusesBeforeAnyEffect(t *testing.T) {
	never := func(context.Context) error { return errors.New("must not run") }
	t.Run("changed observation", func(t *testing.T) {
		fixture := newRollbackPrepFixture(t)
		items := fixture.plan(t)
		items[1].State, items[1].Actual, items[1].Desired = RollbackPrepStale, digestB, digestA
		err := fixture.prep(items, never, never).Run(context.Background(), "grant")
		if !errors.Is(err, domain.ErrPlanStale) || fixture.hooks.applies(t) != nil {
			t.Fatalf("changed observation = %v, applies %v", err, fixture.hooks.applies(t))
		}
		store, _ := NewPOSIXV2Store(fixture.configHome)
		if snapshot, err := store.ReadCurrentJournal(); err != nil || snapshot.Exists {
			t.Fatalf("a refused preparation wrote a journal: %v", err)
		}
	})
	t.Run("unfinished journal", func(t *testing.T) {
		fixture := newRollbackPrepFixture(t)
		unfinished := fixture.prep(fixture.plan(t), never, never)
		record, err := unfinished.journal("tx-unfinished-001", "grant")
		if err != nil {
			t.Fatal(err)
		}
		record.Checkpoint = JournalReconciling
		record.IntentDigest = bindJournalIntent(record.AuthorizationPlan, record.ResumePlan, record.ObservationScope, record.Steps)
		payload, err := MarshalJournal(record)
		if err != nil {
			t.Fatal(err)
		}
		store, _ := NewPOSIXV2Store(fixture.configHome)
		if err := store.CompareAndSwapCurrentJournal(absentProtectedSnapshot(), payload); err != nil {
			t.Fatal(err)
		}
		if err := unfinished.Run(context.Background(), "grant"); !errors.Is(err, domain.ErrPlanStale) || fixture.hooks.applies(t) != nil {
			t.Fatalf("preparation over an unfinished journal = %v", err)
		}
	})
	t.Run("invalid inputs", func(t *testing.T) {
		fixture := newRollbackPrepFixture(t)
		for name, prep := range map[string]RollbackPrep{
			"no authorization": fixture.prep(fixture.plan(t), never, never),
			"same release":     func() RollbackPrep { prep := fixture.prep(nil, never, never); prep.Target = prep.Active; return prep }(),
			"no prefix": func() RollbackPrep {
				prep := fixture.prep(nil, never, never)
				prep.NewTransactionID = func() TransactionID { return "tx-other" }
				return prep
			}(),
		} {
			grant := Authorization("grant")
			if name == "no authorization" {
				grant = ""
			}
			if err := prep.Run(context.Background(), grant); err == nil {
				t.Fatalf("%s accepted", name)
			}
		}
	})
}

func TestRollbackPrepGateFailureKeepsActiveReleaseAndDispatchesNothing(t *testing.T) {
	fixture := newRollbackPrepFixture(t)
	refused := errors.New("target refuses the prepared state")
	dispatched := false
	err := fixture.prep(fixture.plan(t), func(context.Context) error { return refused },
		func(context.Context) error { dispatched = true; return nil }).Run(context.Background(), "grant")
	if !errors.Is(err, refused) || dispatched {
		t.Fatalf("gate failure = %v, dispatched %v", err, dispatched)
	}
	assertRollbackPrepJournal(t, fixture.currentJournal(t))
	if applied := fixture.hooks.applies(t); len(applied) != 4 || !strings.HasPrefix(applied[1], RollbackPrepTransactionPrefixV1) || applied[1] != applied[3] {
		t.Fatalf("hooks did not share the down transaction: %v", applied)
	}
}

func TestRollbackPrepItemsValidate(t *testing.T) {
	for _, item := range []RollbackPrepItem{
		{Owner: "x", Yard: "default", State: "deferred"},
		{Owner: "x", Yard: "default", State: RollbackPrepAbsent, Desired: digestA},
		{Owner: "x", Yard: "default", State: RollbackPrepCurrent, Actual: digestA, Desired: digestB},
		{Owner: "x", Yard: "default", State: RollbackPrepStale},
		{Owner: "x y", Yard: "default", State: RollbackPrepAbsent},
		{Owner: "x", Yard: "../d", State: RollbackPrepAbsent},
	} {
		if _, err := RollbackPrepBinding([]RollbackPrepItem{item}); err == nil {
			t.Fatalf("invalid item accepted: %#v", item)
		}
	}
}

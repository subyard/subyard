package releaseruntime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

const (
	prepOld = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	prepNew = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// rollbackPrepRoots declares a synthetic runtime in both releases. The active
// release's declaration and the target's format decide preparation.
func rollbackPrepRoots(t *testing.T, active, target string) (*Runtime, string, string, string, *[]string) {
	t.Helper()
	runtime, activeRoot, targetRoot, home, _ := retainedRuntimeFixture(t)
	declare := func(root, runtime string) {
		testkit.WriteFile(t, filepath.Join(root, "config/profiles/sample/profile.json"),
			[]byte(`{"schema_version":1,"default_yards":["default"],"runtime":{"activation_id":"sample-runtime","handler":"runtime.sh"`+runtime+`}}`), 0o600)
	}
	declare(activeRoot, active)
	declare(targetRoot, target)
	declareRollbackPrepYard(t, home, "named")
	calls := &[]string{}
	runtime.config.ProfileRuntimeRollbackObservation = func(_ context.Context, root string, loaded config.Loaded, id string) (ports.RuntimeObservation, error) {
		*calls = append(*calls, "rollback-observe "+loaded.Context.YardName+" "+id+" "+filepath.Base(root))
		return ports.RuntimeObservation{State: ports.RuntimeStateStale, Actual: prepNew, Desired: prepOld}, nil
	}
	runtime.config.ProfileRuntimeRollbackApply = func(context.Context, string, config.Loaded, string, string, string, string) error {
		return errors.New("planning must not apply")
	}
	return runtime, activeRoot, targetRoot, home, calls
}

func TestRollbackPreparationSelectsOwnersByDeclaredFormats(t *testing.T) {
	for _, test := range []struct {
		name, active, target string
		prepared             bool
		refusal              string
	}{
		{name: "undeclared formats", prepared: false},
		{name: "same declared format", active: `,"format":2`, target: `,"format":2`},
		{name: "absent target format is 1", active: `,"format":2,"prepares_rollback_to":1`, prepared: true},
		{name: "declared target format", active: `,"format":3,"prepares_rollback_to":2`, target: `,"format":2`, prepared: true},
		{name: "unprepared format", active: `,"format":2`, refusal: "profile runtime sample-runtime format 2 cannot be prepared for the retained release's format 1"},
		{name: "other prepared format", active: `,"format":3,"prepares_rollback_to":2`, refusal: "format 3 cannot be prepared for the retained release's format 1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, active, target, home, calls := rollbackPrepRoots(t, test.active, test.target)
			prep, refusals, err := runtime.planRollbackPreparation(context.Background(), active, target, home)
			if err != nil {
				t.Fatal(err)
			}
			if test.refusal != "" {
				if refusals == nil || !strings.Contains(refusals.Error(), test.refusal) || prep != nil || len(*calls) != 0 {
					t.Fatalf("refusal = %v, prep %#v, calls %v", refusals, prep, *calls)
				}
				return
			}
			if refusals != nil || (prep != nil) != test.prepared {
				t.Fatalf("prep = %#v, refusals %v", prep, refusals)
			}
			if !test.prepared {
				if len(*calls) != 0 {
					t.Fatalf("an unprepared rollback ran hooks: %v", *calls)
				}
				return
			}
			want := []string{"rollback-observe default sample-runtime active", "rollback-observe named sample-runtime active"}
			if strings.Join(*calls, "|") != strings.Join(want, "|") || len(prep.items) != 2 ||
				prep.items[0] != (releasetransition.RollbackPrepItem{Owner: "sample-runtime", Yard: "default", State: "stale", Actual: prepNew, Desired: prepOld}) {
				t.Fatalf("observed %v, items %#v", *calls, prep.items)
			}
		})
	}
}

// Every independent refusal is reported: a stopped yard, an owner that cannot
// be observed and a format this release does not prepare.
func TestRollbackPreparationCollectsEveryRefusalWithTypedNextStep(t *testing.T) {
	runtime, active, target, home, _ := rollbackPrepRoots(t, `,"format":2,"prepares_rollback_to":1`, ``)
	other := filepath.Join(active, "config/profiles/other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(other, "runtime.sh"), []byte("#!/bin/sh\n"), 0o700)
	testkit.WriteFile(t, filepath.Join(other, "profile.json"), []byte(`{"schema_version":1,"runtime":{"activation_id":"other-runtime","handler":"runtime.sh","format":4}}`), 0o600)
	otherTarget := filepath.Join(target, "config/profiles/other")
	if err := os.MkdirAll(otherTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(otherTarget, "runtime.sh"), []byte("#!/bin/sh\n"), 0o700)
	testkit.WriteFile(t, filepath.Join(otherTarget, "profile.json"), []byte(`{"schema_version":1,"runtime":{"activation_id":"other-runtime","handler":"runtime.sh","format":3}}`), 0o600)
	declareRollbackPrepYard(t, home, "third")
	runtime.config.ProfileRuntimeRollbackObservation = func(_ context.Context, _ string, loaded config.Loaded, _ string) (ports.RuntimeObservation, error) {
		switch loaded.Context.YardName {
		case "named":
			return ports.RuntimeObservation{State: ports.RuntimeStateDeferred}, nil
		case "third":
			return ports.RuntimeObservation{}, errors.New("private native detail")
		}
		return ports.RuntimeObservation{State: ports.RuntimeStateCurrent, Actual: prepOld, Desired: prepOld}, nil
	}
	_, refusals, err := runtime.planRollbackPreparation(context.Background(), active, target, home)
	if err != nil || refusals == nil {
		t.Fatalf("refusals = %v, err %v", refusals, err)
	}
	outcome := rollbackPrepOutcome(releasetransition.ReleaseLinks{Active: "b"}, "a", refusals)
	for _, part := range []string{
		"profile runtime other-runtime format 4 cannot be prepared for the retained release's format 3",
		"profile runtime sample-runtime in yard third cannot be observed for rollback preparation",
		"yard named is stopped, so profile runtime sample-runtime cannot be prepared for rollback",
	} {
		if !strings.Contains(outcome.Message, part) {
			t.Fatalf("outcome %q lacks %q", outcome.Message, part)
		}
	}
	if strings.Contains(outcome.Message, "private") || outcome.Code != releasetransition.CodeRollbackIncompatible ||
		outcome.Retry != "keep the current release active and retain a release whose profile runtime formats this release prepares, then run yard update --check" {
		t.Fatalf("outcome = %#v", outcome)
	}
	stopped := rollbackPrepOutcome(releasetransition.ReleaseLinks{Active: "b"}, "a", stoppedYardRefusal("sample-runtime", "named"))
	if action, _, ok := domain.ParseRetry(stopped.Retry); !ok || action != (domain.Action{Verb: domain.VerbStart, Yard: "named"}) {
		t.Fatalf("stopped yard retry = %q", stopped.Retry)
	}
}

func TestRollbackPreparationGateObservesTheTargetFromItsOwnRoot(t *testing.T) {
	runtime, active, target, home, _ := rollbackPrepRoots(t, `,"format":2,"prepares_rollback_to":1`, ``)
	prep, refusals, err := runtime.planRollbackPreparation(context.Background(), active, target, home)
	if err != nil || refusals != nil || prep == nil {
		t.Fatalf("prep = %#v, %v, %v", prep, refusals, err)
	}
	states := map[string]ports.RuntimeObservation{
		"default": {State: ports.RuntimeStateStale, Desired: prepOld},
		"named":   {State: ports.RuntimeStateCurrent, Actual: prepOld, Desired: prepOld},
	}
	var roots []string
	runtime.config.ProfileRuntimeObservation = func(_ context.Context, root string, loaded config.Loaded, id string) (ports.RuntimeObservation, error) {
		roots = append(roots, filepath.Base(root)+" "+filepath.Base(loaded.Context.Paths.RepositoryRoot)+" "+id)
		if state, ok := states[loaded.Context.YardName]; ok {
			return state, nil
		}
		return ports.RuntimeObservation{}, errors.New("target rejects the prepared state")
	}
	if err := prep.gate(context.Background(), runtime.config); err != nil {
		t.Fatal(err)
	}
	for _, root := range roots {
		if !strings.HasPrefix(root, "target ") {
			t.Fatalf("gate observed from another root: %v", roots)
		}
	}
	states["named"] = ports.RuntimeObservation{State: ports.RuntimeStateDeferred}
	delete(states, "default")
	err = prep.gate(context.Background(), runtime.config)
	outcome := rollbackPrepOutcome(releasetransition.ReleaseLinks{Active: "b"}, "a", err)
	if !strings.Contains(outcome.Message, "does not accept the prepared profile runtime sample-runtime in yard default") ||
		!strings.Contains(outcome.Message, "yard named is stopped") || outcome.Retry != "run yard migrate" {
		t.Fatalf("gate refusal = %#v", outcome)
	}
}

func TestRollbackPrepRefusalsJoinCompatibilityRefusals(t *testing.T) {
	links := releasetransition.ReleaseLinks{Active: "b"}
	compatibility := publicReleaseInspectionError{outcome: publicReleaseOutcome(links, "a", nil, releasetransition.CodeRollbackIncompatible, "registry refused", "compat retry")}
	compatibility.cause = transitionOutcomeError(compatibility.outcome)
	prep := stoppedYardRefusal("sample-runtime", "named")
	var merged publicReleaseInspectionError
	if err := mergeRollbackPrepRefusals(links, "a", prep, compatibility); !errors.As(err, &merged) ||
		!strings.HasPrefix(merged.outcome.Message, "registry refused; yard named is stopped") || merged.outcome.Retry != "compat retry" {
		t.Fatalf("merged = %v", err)
	}
	if err := mergeRollbackPrepRefusals(links, "a", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := mergeRollbackPrepRefusals(links, "a", prep, nil); !errors.As(err, &merged) || merged.outcome.Retry != "run yard -Y named start" {
		t.Fatalf("prep-only refusal = %v", err)
	}
}

func TestFreshRollbackPlanAdmission(t *testing.T) {
	transaction := releasetransition.TransactionID("tx-1")
	plain := []string{"reactivate the verified retained previous runtime", "apply the exact typed migration and release activation plan"}
	for name, test := range map[string]struct {
		fresh Prepared
		retry string
	}{
		"plain":       {Prepared{Consequences: plain}, ""},
		"blocked":     {Prepared{Consequences: plain, inspection: releasetransition.Inspection{Blockers: []releasetransition.Blocker{{Code: releasetransition.CodePlanStale, Message: "x"}}}}, "run yard migrate"},
		"resume":      {Prepared{Consequences: plain, inspection: releasetransition.Inspection{Resume: &transaction}}, "run yard migrate"},
		"consequence": {Prepared{Consequences: append(slices.Clone(plain), "canonicalize setting YARD_TEMPLATE")}, "run yard update --rollback"},
	} {
		err := admitFreshRollbackPlan(test.fresh)
		var refusal rollbackPrepRefusal
		if test.retry == "" && err != nil || test.retry != "" && (!errors.As(err, &refusal) || refusal.retry != test.retry) {
			t.Fatalf("%s: admission %v", name, err)
		}
	}
}

func declareRollbackPrepYard(t *testing.T, home, yard string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, "yards", yard), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(home, "yards", yard, "config.env"), []byte("\n"), 0o600)
}

// preparedRollbackFixture publishes an active release whose sample runtime
// has format 2 and prepares format 1, and a retained target at format 1.
// The target engine logs each inspection, noting whether the down journal
// already exists, and fails its convergence after logging it.
type preparedRollbackFixture struct {
	runtime                *Runtime
	runtimeRoot, home, log string
	activeID, targetID     string
	formats                map[string]int
	applies, gates         []string
	gateErr                error
}

// consequences are the target plan's own; a plain rollback has only the
// engine's generic line.
func newPreparedRollbackFixture(t *testing.T, consequences ...string) *preparedRollbackFixture {
	t.Helper()
	consequenceJSON, err := json.Marshal(append([]string{"apply the exact typed migration and release activation plan"}, consequences...))
	if err != nil {
		t.Fatal(err)
	}
	runtime, active, target, home, _ := retainedRuntimeFixture(t)
	t.Cleanup(func() { _ = runtime.Close() })
	declareRollbackPrepYard(t, home, "named")
	root := filepath.Dir(active)
	fixture := &preparedRollbackFixture{runtime: runtime, runtimeRoot: filepath.Join(root, "runtime"), home: home, log: filepath.Join(root, "engine.log"),
		activeID: "0.31.0-active", targetID: UpdateAnchorVersion + "-target", formats: map[string]int{"default": 2, "named": 1}}
	if err := os.MkdirAll(filepath.Join(fixture.runtimeRoot, "releases"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(active, "config/profiles/sample/profile.json"),
		[]byte(`{"schema_version":1,"default_yards":["default"],"runtime":{"activation_id":"sample-runtime","handler":"runtime.sh","format":2,"prepares_rollback_to":1},"managed_paths":[{"root":"data","path":"service/{yard}"}]}`), 0o600)
	inspection := fmt.Sprintf(`{"schemaVersion":1,"activationReconciliationOwned":true,"inspection":{"plan":"plan-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible","consequences":%s},"outcome":{"status":"migration-required","reachedGoal":false,"active":"%s","previous":"%s","target":"%s","code":"transition-required","message":"confirmation required","retry":"confirm the inspected rollback"}}}`, consequenceJSON, fixture.activeID, fixture.targetID, fixture.targetID)
	journal := filepath.Join(home, "release-transition/v2/journal.json")
	// The active release's own inspection after a down transaction reports
	// its owners' prepared state as drift.
	drift := strings.NewReplacer(`"previous":"`+fixture.targetID+`","target":"`+fixture.targetID+`"`, `"previous":"`+fixture.targetID+`","target":"`+fixture.activeID+`"`).Replace(inspection)
	for _, candidate := range []struct{ path, id, version string }{{active, fixture.activeID, "0.31.0"}, {target, fixture.targetID, UpdateAnchorVersion}} {
		if err := os.MkdirAll(filepath.Join(candidate.path, "bin"), 0o700); err != nil {
			t.Fatal(err)
		}
		engine := fmt.Sprintf(`#!/bin/sh
case "${1:-}" in
 --version) printf 'yard-engine %s\n' ;;
 _release-transition) request=$(cat); case "$request" in
  *runtime-activation-capabilities*) exit 1 ;;
  *'"mode":"converge"'*) printf 'converge\n' >>%q; exit 64 ;;
  *'"direction":"activate-target"'*) printf '%%s\n' %q ;;
  *) if grep -q rollback-prep-v1- %q 2>/dev/null; then printf 'inspect prepared\n' >>%q; else printf 'inspect unprepared\n' >>%q; fi; printf '%%s\n' %q ;;
 esac ;;
 *) exit 64 ;;
esac
`, candidate.version, fixture.log, drift, journal, fixture.log, fixture.log, inspection)
		testkit.WriteFile(t, filepath.Join(candidate.path, "bin/yard-engine"), []byte(engine), 0o700)
		testkit.WriteFile(t, filepath.Join(candidate.path, "config/release-transition.json"), []byte(sourceAdmissionRegistry), 0o600)
		var manifest strings.Builder
		if err := filepath.WalkDir(candidate.path, func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			payload, err := os.ReadFile(path)
			if err == nil {
				relative, _ := filepath.Rel(candidate.path, path)
				fmt.Fprintf(&manifest, "%x  ./%s\n", sha256.Sum256(payload), relative)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(candidate.path, "runtime-files.sha256"), []byte(manifest.String()), 0o600)
		if err := os.Rename(candidate.path, filepath.Join(fixture.runtimeRoot, "releases", candidate.id)); err != nil {
			t.Fatal(err)
		}
	}
	for name, id := range map[string]string{"current": fixture.activeID, "previous": fixture.targetID} {
		if err := os.Symlink("releases/"+id, filepath.Join(fixture.runtimeRoot, name)); err != nil {
			t.Fatal(err)
		}
	}
	writeRollbackFixtureBaselineLedger(t, home)
	observe := func(yard string) ports.RuntimeObservation {
		if fixture.formats[yard] == 1 {
			return ports.RuntimeObservation{State: ports.RuntimeStateCurrent, Actual: prepOld, Desired: prepOld}
		}
		return ports.RuntimeObservation{State: ports.RuntimeStateStale, Actual: prepNew, Desired: prepOld}
	}
	runtime.config.ProfileRuntimeRollbackObservation = func(_ context.Context, root string, loaded config.Loaded, _ string) (ports.RuntimeObservation, error) {
		if filepath.Base(root) != fixture.activeID {
			t.Errorf("rollback-observe ran from %s", root)
		}
		return observe(loaded.Context.YardName), nil
	}
	runtime.config.ProfileRuntimeRollbackApply = func(_ context.Context, root string, loaded config.Loaded, id, operation, actual, desired string) error {
		if observed := observe(loaded.Context.YardName); string(observed.Actual) != actual || string(observed.Desired) != desired {
			return domain.ErrPlanStale
		}
		fixture.formats[loaded.Context.YardName] = 1
		fixture.applies = append(fixture.applies, loaded.Context.YardName+" "+id+" "+operation)
		return nil
	}
	runtime.config.ProfileRuntimeObservation = func(_ context.Context, root string, loaded config.Loaded, id string) (ports.RuntimeObservation, error) {
		if filepath.Base(root) == fixture.targetID {
			fixture.gates = append(fixture.gates, loaded.Context.YardName+" "+id)
			return ports.RuntimeObservation{State: ports.RuntimeStateStale, Desired: prepOld}, fixture.gateErr
		}
		return ports.RuntimeObservation{State: ports.RuntimeStateAbsent}, nil
	}
	runtime.config.InstalledRuntimePresence = func(context.Context, string, config.Loaded, string) (ports.RuntimePresence, error) {
		return ports.RuntimePresence{Actual: "absent"}, nil
	}
	return fixture
}

func (fixture *preparedRollbackFixture) prepare(t *testing.T, arguments ...string) (Prepared, error) {
	t.Helper()
	return fixture.runtime.PrepareTransition(context.Background(), append([]string{"--rollback", "--runtime-root", fixture.runtimeRoot}, arguments...), fixture.home, "default")
}

func (fixture *preparedRollbackFixture) engineLog(t *testing.T) string {
	t.Helper()
	payload, err := os.ReadFile(fixture.log)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

func (fixture *preparedRollbackFixture) assertLinks(t *testing.T) {
	t.Helper()
	for name, id := range map[string]string{"current": fixture.activeID, "previous": fixture.targetID} {
		if got, err := os.Readlink(filepath.Join(fixture.runtimeRoot, name)); err != nil || got != "releases/"+id {
			t.Fatalf("%s link = %q, %v", name, got, err)
		}
	}
}

func (fixture *preparedRollbackFixture) journal(t *testing.T) *releasetransition.JournalRecord {
	t.Helper()
	store, err := releasetransition.NewPOSIXV2Store(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Exists {
		return nil
	}
	journal, err := releasetransition.ParseJournal(snapshot.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return &journal
}

func TestPreparedRollbackRunsTheDownTransactionBeforeTheTargetPlan(t *testing.T) {
	fixture := newPreparedRollbackFixture(t)
	prepared, err := fixture.prepare(t)
	if err != nil {
		t.Fatal(err)
	}
	if log := fixture.engineLog(t); log != "" || fixture.journal(t) != nil || len(fixture.applies) != 0 {
		t.Fatalf("planning inspected the target or mutated: log %q applies %v", log, fixture.applies)
	}
	if prepared.Action != "update.rollback" || !prepared.Changed ||
		!slicesContains(prepared.Consequences, "prepare profile runtime sample-runtime in yard default for release "+fixture.targetID) ||
		slicesContains(prepared.Consequences, "prepare profile runtime sample-runtime in yard named for release "+fixture.targetID) {
		t.Fatalf("down plan = %#v", prepared)
	}
	err = prepared.Execute(context.Background())
	if err == nil {
		t.Fatal("the fixture target's convergence failure was lost")
	}
	journal := fixture.journal(t)
	if journal == nil || !strings.HasPrefix(string(journal.Transaction), releasetransition.RollbackPrepTransactionPrefixV1) ||
		journal.Checkpoint != releasetransition.JournalComplete || journal.Goal.Target != releasetransition.ReleaseID(fixture.activeID) {
		t.Fatalf("down journal = %#v", journal)
	}
	if len(fixture.applies) != 1 || fixture.applies[0] != "default sample-runtime "+string(journal.Transaction) {
		t.Fatalf("applies = %v", fixture.applies)
	}
	if strings.Join(fixture.gates, "|") != "default sample-runtime|named sample-runtime" {
		t.Fatalf("gate observations = %v", fixture.gates)
	}
	if log := fixture.engineLog(t); log != "inspect prepared\nconverge\n" {
		t.Fatalf("target engine log = %q after %v", log, err)
	}
	fixture.assertLinks(t)
}

func TestPreparedRollbackRefusesBeforeTheTargetWhenTheGateFails(t *testing.T) {
	fixture := newPreparedRollbackFixture(t)
	fixture.gateErr = errors.New("target observer rejects the state")
	prepared, err := fixture.prepare(t)
	if err != nil {
		t.Fatal(err)
	}
	err = prepared.Execute(context.Background())
	outcome, ok := TransitionOutcome(err)
	if !ok || outcome.Code != releasetransition.CodeRollbackIncompatible || outcome.Retry != "run yard migrate" ||
		!strings.Contains(outcome.Message, "does not accept the prepared profile runtime sample-runtime in yard default") {
		t.Fatalf("gate refusal = %#v, %v", outcome, err)
	}
	if log := fixture.engineLog(t); log != "" {
		t.Fatalf("a refused gate reached the target: %q", log)
	}
	fixture.assertLinks(t)
}

func TestPreparedRollbackRechecksConfirmedObservations(t *testing.T) {
	fixture := newPreparedRollbackFixture(t)
	prepared, err := fixture.prepare(t)
	if err != nil {
		t.Fatal(err)
	}
	fixture.formats["named"] = 2
	if err := prepared.Execute(context.Background()); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("changed observation = %v", err)
	}
	if fixture.journal(t) != nil || len(fixture.applies) != 0 || fixture.engineLog(t) != "" {
		t.Fatalf("a stale down plan had effects: applies %v", fixture.applies)
	}
}

func TestPreparedRollbackRefusesAStoppedYard(t *testing.T) {
	fixture := newPreparedRollbackFixture(t)
	observe := fixture.runtime.config.ProfileRuntimeRollbackObservation
	fixture.runtime.config.ProfileRuntimeRollbackObservation = func(ctx context.Context, root string, loaded config.Loaded, id string) (ports.RuntimeObservation, error) {
		if loaded.Context.YardName == "named" {
			return ports.RuntimeObservation{State: ports.RuntimeStateDeferred}, nil
		}
		return observe(ctx, root, loaded, id)
	}
	_, err := fixture.prepare(t)
	outcome, ok := TransitionOutcome(err)
	if !ok || outcome.Retry != "run yard -Y named start" || !strings.Contains(outcome.Message, "yard named is stopped") {
		t.Fatalf("stopped yard = %#v, %v", outcome, err)
	}
	fixture.assertLinks(t)
}

func TestPreparedRollbackStopsAfterTheGateForAPlanNeedingItsOwnConfirmation(t *testing.T) {
	const extra = "compact completed migration history checkpoint"
	fixture := newPreparedRollbackFixture(t, extra)
	prepared, err := fixture.prepare(t)
	if err != nil {
		t.Fatal(err)
	}
	err = prepared.Execute(context.Background())
	outcome, ok := TransitionOutcome(err)
	if !ok || outcome.Retry != "run yard update --rollback" || !strings.Contains(outcome.Message, "needs its own confirmation") {
		t.Fatalf("fresh plan refusal = %#v, %v", outcome, err)
	}
	if len(fixture.applies) != 1 || len(fixture.gates) != 2 || fixture.engineLog(t) != "inspect prepared\n" {
		t.Fatalf("refusal did not stop after the hooks and gate: applies %v gates %v log %q", fixture.applies, fixture.gates, fixture.engineLog(t))
	}
	fixture.assertLinks(t)

	// The rerun finds every owner current and confirms the target's own plan.
	rerun, err := fixture.prepare(t)
	if err != nil {
		t.Fatal(err)
	}
	if !slicesContains(rerun.Consequences, extra) || slicesContains(rerun.Consequences, "prepare profile runtime sample-runtime in yard default for release "+fixture.targetID) {
		t.Fatalf("rerun plan = %#v", rerun.Consequences)
	}
	fixture.formats["named"] = 2
	if err := rerun.Execute(context.Background()); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("rerun ignored a changed prepared owner: %v", err)
	}
	fixture.formats["named"] = 1
	if err := rerun.Execute(context.Background()); err == nil {
		t.Fatal("the fixture target's convergence failure was lost")
	}
	if len(fixture.applies) != 1 || !strings.HasSuffix(fixture.engineLog(t), "converge\n") {
		t.Fatalf("rerun did not dispatch the confirmed target plan alone: applies %v log %q", fixture.applies, fixture.engineLog(t))
	}
}

func slicesContains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

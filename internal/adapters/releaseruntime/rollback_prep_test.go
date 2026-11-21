package releaseruntime

import (
	"context"
	"crypto/sha256"
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

func declareRollbackPrepYard(t *testing.T, home, yard string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, "yards", yard), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(home, "yards", yard, "config.env"), []byte("\n"), 0o600)
}

// preparedRollbackFixture publishes sealed active and retained releases with
// a synthetic sample runtime in yards default and named. Each yard's state is
// format 2 (the active release's) or 1. The target engine logs inspections,
// noting whether the down journal exists, and fails its convergence.
type preparedRollbackFixture struct {
	runtime                 *Runtime
	runtimeRoot, home, log  string
	data                    string
	activeID, targetID      string
	formats                 map[string]int
	consequence             map[string]string
	observed, applies, gate []string
	gateErr, applyErr       error
}

type preparedRollbackOptions struct {
	active, target string // the sample runtime's extra declaration fields
	seed           func(active, target string)
}

var preparedSample = preparedRollbackOptions{active: `,"format":2,"prepares_rollback_to":1`}

func newPreparedRollbackFixture(t *testing.T, options preparedRollbackOptions) *preparedRollbackFixture {
	t.Helper()
	runtime, active, target, home, _ := retainedRuntimeFixture(t)
	t.Cleanup(func() { _ = runtime.Close() })
	declareRollbackPrepYard(t, home, "named")
	root := filepath.Dir(active)
	fixture := &preparedRollbackFixture{runtime: runtime, runtimeRoot: filepath.Join(root, "runtime"), home: home, log: filepath.Join(root, "engine.log"),
		data: filepath.Join(root, "data"), activeID: "0.31.0-active", targetID: UpdateAnchorVersion + "-target",
		formats: map[string]int{"default": 2, "named": 1}, consequence: map[string]string{}}
	if err := os.MkdirAll(filepath.Join(fixture.runtimeRoot, "releases"), 0o700); err != nil {
		t.Fatal(err)
	}
	for path, fields := range map[string]string{active: options.active, target: options.target} {
		testkit.WriteFile(t, filepath.Join(path, "config/profiles/sample/profile.json"),
			[]byte(`{"schema_version":1,"default_yards":["default"],"runtime":{"activation_id":"sample-runtime","handler":"runtime.sh"`+fields+`},"managed_paths":[{"root":"data","path":"service/{yard}"}]}`), 0o600)
	}
	if options.seed != nil {
		options.seed(active, target)
	}
	inspection := fmt.Sprintf(`{"schemaVersion":1,"activationReconciliationOwned":true,"inspection":{"plan":"plan-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible","consequences":["apply the exact typed migration and release activation plan"]},"outcome":{"status":"migration-required","reachedGoal":false,"active":"%s","previous":"%s","target":"%s","code":"transition-required","message":"confirmation required","retry":"confirm the inspected rollback"}}}`, fixture.activeID, fixture.targetID, fixture.targetID)
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
	runtime.config.ProfileRuntimeRollbackObservation = func(_ context.Context, root string, loaded config.Loaded, id string) (ports.RuntimeObservation, error) {
		fixture.observed = append(fixture.observed, loaded.Context.YardName+" "+id+" "+releaseRootName(t, root))
		return fixture.observe(loaded.Context.YardName), nil
	}
	runtime.config.ProfileRuntimeRollbackApply = func(_ context.Context, root string, loaded config.Loaded, id, operation, actual, desired string) error {
		if observed := fixture.observe(loaded.Context.YardName); string(observed.Actual) != actual || string(observed.Desired) != desired {
			return domain.ErrPlanStale
		}
		if fixture.applyErr != nil {
			return fixture.applyErr
		}
		fixture.formats[loaded.Context.YardName] = 1
		fixture.applies = append(fixture.applies, loaded.Context.YardName+" "+id+" "+operation+" "+loaded.Context.YardInstanceName+" "+releaseRootName(t, root))
		return nil
	}
	runtime.config.ProfileRuntimeObservation = func(_ context.Context, root string, loaded config.Loaded, id string) (ports.RuntimeObservation, error) {
		if releaseRootName(t, root) == fixture.targetID {
			fixture.gate = append(fixture.gate, loaded.Context.YardName+" "+id)
			return ports.RuntimeObservation{State: ports.RuntimeStateStale, Desired: prepOld}, fixture.gateErr
		}
		return ports.RuntimeObservation{State: ports.RuntimeStateAbsent}, nil
	}
	runtime.config.InstalledRuntimePresence = func(context.Context, string, config.Loaded, string) (ports.RuntimePresence, error) {
		return ports.RuntimePresence{Actual: "absent"}, nil
	}
	return fixture
}

func (fixture *preparedRollbackFixture) observe(yard string) ports.RuntimeObservation {
	switch fixture.formats[yard] {
	case 0:
		return ports.RuntimeObservation{State: ports.RuntimeStateDeferred}
	case 1:
		return ports.RuntimeObservation{State: ports.RuntimeStateCurrent, Actual: prepOld, Desired: prepOld}
	}
	return ports.RuntimeObservation{State: ports.RuntimeStateStale, Actual: prepNew, Desired: prepOld, Consequence: fixture.consequence[yard]}
}

// releaseRootName names the release a pinned or plain root resolves to.
func releaseRootName(t *testing.T, root string) string {
	t.Helper()
	if resolved, err := os.Readlink(root); err == nil {
		root = resolved
	}
	return filepath.Base(root)
}

func (fixture *preparedRollbackFixture) verified(t *testing.T) (*verifiedPublishedCandidate, *verifiedPublishedCandidate) {
	t.Helper()
	var candidates []*verifiedPublishedCandidate
	for _, id := range []string{fixture.activeID, fixture.targetID} {
		candidate, err := fixture.runtime.verifyPublishedCandidate(context.Background(), publishedCandidate{
			release: releasetransition.ReleaseID(id), root: filepath.Join(fixture.runtimeRoot, "releases", id)}, fixture.runtimeRoot, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = candidate.Close() })
		candidates = append(candidates, candidate)
	}
	return candidates[0], candidates[1]
}

func (fixture *preparedRollbackFixture) plan(t *testing.T) (*rollbackPreparation, error) {
	t.Helper()
	active, target := fixture.verified(t)
	prep, refusals, err := fixture.runtime.planRollbackPreparation(context.Background(), active, target, fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	return prep, refusals
}

func (fixture *preparedRollbackFixture) prepare(t *testing.T) (Prepared, error) {
	t.Helper()
	return fixture.runtime.PrepareTransition(context.Background(), []string{"--rollback", "--runtime-root", fixture.runtimeRoot}, fixture.home, "default")
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

func outcomeOf(t *testing.T, err error) releasetransition.Outcome {
	t.Helper()
	outcome, ok := TransitionOutcome(err)
	if !ok {
		t.Fatalf("untyped refusal: %v", err)
	}
	return outcome
}

func TestRollbackPreparationSelectsOwnersByDeclaredFormats(t *testing.T) {
	for _, test := range []struct {
		name, active, target string
		prepared             bool
		refusal              string
	}{
		{name: "undeclared formats"},
		{name: "same declared format", active: `,"format":2`, target: `,"format":2`},
		{name: "absent target format is 1", active: `,"format":2,"prepares_rollback_to":1`, prepared: true},
		{name: "declared target format", active: `,"format":3,"prepares_rollback_to":2`, target: `,"format":2`, prepared: true},
		{name: "unprepared format", active: `,"format":2`, refusal: "this release cannot convert profile runtime sample-runtime from format 2 back to the retained release's format 1"},
		{name: "other prepared format", active: `,"format":3,"prepares_rollback_to":2`, refusal: "from format 3 back to the retained release's format 1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPreparedRollbackFixture(t, preparedRollbackOptions{active: test.active, target: test.target})
			prep, refusals := fixture.plan(t)
			if test.refusal != "" {
				if refusals == nil || !strings.Contains(refusals.Error(), test.refusal) || prep != nil || len(fixture.observed) != 0 {
					t.Fatalf("refusal = %v, prep %#v, observed %v", refusals, prep, fixture.observed)
				}
				return
			}
			if refusals != nil || (prep != nil) != test.prepared {
				t.Fatalf("prep = %#v, refusals %v", prep, refusals)
			}
			if !test.prepared {
				if len(fixture.observed) != 0 {
					t.Fatalf("an unprepared rollback ran hooks: %v", fixture.observed)
				}
				return
			}
			want := []string{"default sample-runtime " + fixture.activeID, "named sample-runtime " + fixture.activeID}
			if !slices.Equal(fixture.observed, want) || len(prep.items) != 2 ||
				prep.items[0] != (releasetransition.RollbackPrepItem{Owner: "sample-runtime", Yard: "default", Instance: prep.active["default"].Context.IncusProject + "/" + prep.active["default"].Context.YardInstanceName,
					State: "stale", Actual: prepNew, Desired: prepOld}) || prep.items[0].Instance == "/" {
				t.Fatalf("observed %v, items %#v", fixture.observed, prep.items)
			}
		})
	}
}

// Every independent refusal is reported with its cause: a format this release
// does not prepare, an owner that cannot be observed, a yard that cannot be
// loaded and a stopped yard holding installed state. A stopped yard without
// it has nothing to prepare.
func TestRollbackPreparationCollectsEveryRefusalWithTypedNextStep(t *testing.T) {
	fixture := newPreparedRollbackFixture(t, preparedRollbackOptions{active: preparedSample.active, seed: func(active, target string) {
		for root, format := range map[string]int{active: 4, target: 3} {
			other := filepath.Join(root, "config/profiles/other")
			if err := os.MkdirAll(other, 0o700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(other, "runtime.sh"), []byte("#!/bin/sh\n"), 0o700)
			testkit.WriteFile(t, filepath.Join(other, "profile.json"), []byte(fmt.Sprintf(`{"schema_version":1,"runtime":{"activation_id":"other-runtime","handler":"runtime.sh","format":%d}}`, format)), 0o600)
		}
	}})
	declareRollbackPrepYard(t, fixture.home, "third")
	declareRollbackPrepYard(t, fixture.home, "spare")
	declareRollbackPrepYard(t, fixture.home, "broken")
	testkit.WriteFile(t, filepath.Join(fixture.home, "yards/broken/config.env"), []byte("NOT_A_SETTING=1\n"), 0o600)
	if err := os.MkdirAll(filepath.Join(fixture.data, "service/named"), 0o700); err != nil {
		t.Fatal(err)
	}
	fixture.formats = map[string]int{"default": 1}
	observe := fixture.runtime.config.ProfileRuntimeRollbackObservation
	fixture.runtime.config.ProfileRuntimeRollbackObservation = func(ctx context.Context, root string, loaded config.Loaded, id string) (ports.RuntimeObservation, error) {
		if loaded.Context.YardName == "third" {
			return ports.RuntimeObservation{}, errors.New("guest exec failed")
		}
		return observe(ctx, root, loaded, id)
	}
	prep, refusals := fixture.plan(t)
	if refusals == nil {
		t.Fatal("no refusals")
	}
	if prep == nil || !slices.ContainsFunc(prep.items, func(item releasetransition.RollbackPrepItem) bool {
		return item.Yard == "spare" && item.State == releasetransition.RollbackPrepAbsent
	}) {
		t.Fatalf("a stopped yard without installed state was not absent: %#v", prep)
	}
	outcome := outcomeOf(t, mergeRollbackPrepRefusals(releasetransition.ReleaseLinks{Active: "b"}, "a", refusals, nil))
	for _, part := range []string{
		"this release cannot convert profile runtime other-runtime from format 4 back to the retained release's format 3",
		"profile runtime sample-runtime in yard third cannot be observed for rollback preparation: guest exec failed",
		"yard broken configuration cannot be loaded for rollback preparation",
		"yard named is stopped, so its installed profile runtime sample-runtime cannot be prepared for rollback",
	} {
		if !strings.Contains(outcome.Message, part) {
			t.Fatalf("outcome %q lacks %q", outcome.Message, part)
		}
	}
	if strings.Contains(outcome.Message, "spare") || outcome.Code != releasetransition.CodeRollbackIncompatible || outcome.Retry == "" {
		t.Fatalf("outcome = %#v", outcome)
	}
	stopped := outcomeOf(t, mergeRollbackPrepRefusals(releasetransition.ReleaseLinks{Active: "b"}, "a", stoppedYardRefusal("sample-runtime", "named"), nil))
	if action, _, ok := domain.ParseRetry(stopped.Retry); !ok || action != (domain.Action{Verb: domain.VerbStart, Yard: "named"}) {
		t.Fatalf("stopped yard retry = %q", stopped.Retry)
	}
}

func TestRollbackPreparationGateObservesTheTargetFromItsOwnRoot(t *testing.T) {
	fixture := newPreparedRollbackFixture(t, preparedSample)
	prep, refusals := fixture.plan(t)
	if refusals != nil || prep == nil {
		t.Fatalf("prep = %#v, %v", prep, refusals)
	}
	if err := prep.gate(context.Background(), fixture.runtime.config); err != nil || !slices.Equal(fixture.gate, []string{"default sample-runtime", "named sample-runtime"}) {
		t.Fatalf("gate = %v, observed %v", err, fixture.gate)
	}
	fixture.gateErr = errors.New("target rejects the prepared state")
	outcome := outcomeOf(t, mergeRollbackPrepRefusals(releasetransition.ReleaseLinks{Active: "b"}, "a", prep.gate(context.Background(), fixture.runtime.config), nil))
	if !strings.Contains(outcome.Message, "does not accept the prepared profile runtime sample-runtime in yard default") || outcome.Retry != "run yard migrate" {
		t.Fatalf("gate refusal = %#v", outcome)
	}
}

func TestRollbackPrepRefusalsJoinCompatibilityRefusalsAndKeepEveryCause(t *testing.T) {
	links := releasetransition.ReleaseLinks{Active: "b"}
	compatibility := publicReleaseInspectionError{outcome: publicReleaseOutcome(links, "a", nil, releasetransition.CodeUnsupportedKind, "registry refused", "compat retry")}
	compatibility.cause = transitionOutcomeError(compatibility.outcome)
	prep := stoppedYardRefusal("sample-runtime", "named")
	merged := outcomeOf(t, mergeRollbackPrepRefusals(links, "a", prep, compatibility))
	if !strings.HasPrefix(merged.Message, "registry refused; yard named is stopped") || merged.Retry != "compat retry" || merged.Code != releasetransition.CodeUnsupportedKind {
		t.Fatalf("merged = %#v", merged)
	}
	if err := mergeRollbackPrepRefusals(links, "a", nil, nil); err != nil {
		t.Fatal(err)
	}
	var many []error
	for index := range 40 {
		many = append(many, stoppedYardRefusal("sample-runtime", fmt.Sprintf("yard%02d", index)))
	}
	err := mergeRollbackPrepRefusals(links, "a", domain.CollectRefusals(many...), nil)
	if outcome := outcomeOf(t, err); len(outcome.Message) > maximumOutcomeMessage || !strings.HasSuffix(outcome.Message, "...") ||
		!strings.Contains(err.Error(), "yard yard39 is stopped") {
		t.Fatalf("bounded outcome %q lost causes: %v", outcome.Message, err)
	}
}

// Step 1 confirms only the down plan; it prepares the owners, passes the gate
// and stops with this release active. Step 2 confirms the target's own plan.
func TestPreparedRollbackTakesTwoConfirmations(t *testing.T) {
	fixture := newPreparedRollbackFixture(t, preparedSample)
	fixture.consequence["default"] = "restart the sample service"
	prepared, err := fixture.prepare(t)
	if err != nil {
		t.Fatal(err)
	}
	if log := fixture.engineLog(t); log != "" || fixture.journal(t) != nil || len(fixture.applies) != 0 {
		t.Fatalf("planning inspected the target or mutated: log %q applies %v", log, fixture.applies)
	}
	if prepared.Action != "update.rollback" || !prepared.Changed ||
		!slices.Contains(prepared.Consequences, "prepare profile runtime sample-runtime for release "+fixture.targetID+" in 1 yard(s)") ||
		!slices.Contains(prepared.Consequences, "restart the sample service") ||
		slices.Contains(prepared.Consequences, reactivateRetainedConsequence) || slices.ContainsFunc(prepared.Consequences, func(line string) bool {
		return strings.Contains(line, "under this confirmation")
	}) {
		t.Fatalf("down plan = %#v", prepared.Consequences)
	}
	if err := prepared.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	journal := fixture.journal(t)
	if journal == nil || !strings.HasPrefix(string(journal.Transaction), releasetransition.RollbackPrepTransactionPrefixV1) ||
		journal.Checkpoint != releasetransition.JournalComplete || journal.Goal.Target != releasetransition.ReleaseID(fixture.activeID) {
		t.Fatalf("down journal = %#v", journal)
	}
	if len(fixture.applies) != 1 || !strings.HasPrefix(fixture.applies[0], "default sample-runtime "+string(journal.Transaction)+" ") ||
		!strings.HasSuffix(fixture.applies[0], " "+fixture.activeID) {
		t.Fatalf("applies = %v", fixture.applies)
	}
	if !slices.Equal(fixture.gate, []string{"default sample-runtime", "named sample-runtime"}) || fixture.engineLog(t) != "" {
		t.Fatalf("gate %v, target engine log %q", fixture.gate, fixture.engineLog(t))
	}
	fixture.assertLinks(t)
	inspection, err := prepared.Check(context.Background())
	if err != nil || inspection.Outcome.Status != releasetransition.StatusReady || inspection.Outcome.Retry != preparedRollbackRetry ||
		inspection.Outcome.Active != releasetransition.ReleaseID(fixture.activeID) || !strings.Contains(inspection.Outcome.Message, "prepared for "+fixture.targetID) {
		t.Fatalf("step 1 result = %#v, %v", inspection.Outcome, err)
	}

	// Step 2 skips the current hooks and confirms the target's own plan.
	fixture.gate = nil
	rerun, err := fixture.prepare(t)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(rerun.Consequences, reactivateRetainedConsequence) || slices.ContainsFunc(rerun.Consequences, func(line string) bool {
		return strings.HasPrefix(line, "prepare profile runtime")
	}) || len(fixture.gate) != 2 {
		t.Fatalf("rerun plan = %#v, gate %v", rerun.Consequences, fixture.gate)
	}
	fixture.formats["named"] = 2
	if err := rerun.Execute(context.Background()); !errors.Is(err, domain.ErrPlanStale) || strings.Contains(fixture.engineLog(t), "converge") {
		t.Fatalf("rerun ignored a changed prepared owner: %v", err)
	}
	fixture.formats["named"] = 1
	if err := rerun.Execute(context.Background()); err == nil {
		t.Fatal("the fixture target's convergence failure was lost")
	}
	if len(fixture.applies) != 1 || !strings.Contains(fixture.engineLog(t), "inspect prepared\n") || !strings.HasSuffix(fixture.engineLog(t), "converge\n") {
		t.Fatalf("rerun did not dispatch the confirmed target plan alone: applies %v log %q", fixture.applies, fixture.engineLog(t))
	}
}

// A refusal after the down journal names the forward repair, or its own
// resuming step, and keeps this release active.
func TestPreparedRollbackFailuresAfterTheJournalNameTheForwardRepair(t *testing.T) {
	for name, test := range map[string]struct {
		arrange func(*preparedRollbackFixture)
		message string
		retry   string
	}{
		"gate refusal": {func(fixture *preparedRollbackFixture) { fixture.gateErr = errors.New("rejected") },
			"does not accept the prepared profile runtime sample-runtime in yard default", "run yard migrate"},
		"apply failure": {func(fixture *preparedRollbackFixture) { fixture.applyErr = errors.New("conversion failed") },
			"prepare sample-runtime in yard default for rollback: conversion failed", "run yard migrate"},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newPreparedRollbackFixture(t, preparedSample)
			prepared, err := fixture.prepare(t)
			if err != nil {
				t.Fatal(err)
			}
			test.arrange(fixture)
			outcome := outcomeOf(t, prepared.Execute(context.Background()))
			if outcome.Retry != test.retry || !strings.Contains(outcome.Message, test.message) ||
				!strings.Contains(outcome.Message, "stays active") || !strings.Contains(outcome.Message, "yard migrate repairs them forward") {
				t.Fatalf("refusal = %#v", outcome)
			}
			if fixture.journal(t) == nil || fixture.engineLog(t) != "" {
				t.Fatal("the down transaction left no journal or reached the target")
			}
			fixture.assertLinks(t)
		})
	}
}

// Execution rechecks the confirmed facts before the down journal: hook
// observations and their declared effects, protected history, yard
// registrations, a stopped yard, and the verified release roots.
func TestPreparedRollbackRechecksBeforeAnyEffect(t *testing.T) {
	for name, test := range map[string]struct {
		change func(*testing.T, *preparedRollbackFixture)
		retry  string
	}{
		"observation":     {change: func(_ *testing.T, fixture *preparedRollbackFixture) { fixture.formats["named"] = 2 }},
		"declared effect": {change: func(_ *testing.T, fixture *preparedRollbackFixture) { fixture.consequence["default"] = "restart" }},
		"protected history": {change: func(t *testing.T, fixture *preparedRollbackFixture) {
			writeRollbackFixtureLedgerEpoch(t, fixture.home, 2)
		}},
		"yard registration": {change: func(t *testing.T, fixture *preparedRollbackFixture) {
			testkit.WriteFile(t, filepath.Join(fixture.home, "yards/named/config.env"), []byte("YARD_INSTANCE_NAME=changed-guest\n"), 0o600)
		}},
		"stopped yard": {change: func(t *testing.T, fixture *preparedRollbackFixture) {
			fixture.formats["named"] = 0
			if err := os.MkdirAll(filepath.Join(fixture.data, "service/named"), 0o700); err != nil {
				t.Fatal(err)
			}
		}, retry: "run yard -Y named start"},
		"active release": {change: func(t *testing.T, fixture *preparedRollbackFixture) {
			testkit.WriteFile(t, filepath.Join(fixture.runtimeRoot, "releases", fixture.activeID, "config/profiles/sample/runtime.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o700)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newPreparedRollbackFixture(t, preparedSample)
			prepared, err := fixture.prepare(t)
			if err != nil {
				t.Fatal(err)
			}
			test.change(t, fixture)
			err = prepared.Execute(context.Background())
			if test.retry != "" {
				if outcome := outcomeOf(t, err); outcome.Retry != test.retry || strings.Contains(outcome.Message, "stays active") {
					t.Fatalf("refusal = %#v", outcome)
				}
			} else if !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("changed %s = %v", name, err)
			}
			if fixture.journal(t) != nil || len(fixture.applies) != 0 || fixture.engineLog(t) != "" {
				t.Fatalf("a stale down plan had effects: applies %v", fixture.applies)
			}
		})
	}
}

func writeRollbackFixtureLedgerEpoch(t *testing.T, configHome string, epoch int) {
	t.Helper()
	testkit.WriteFile(t, filepath.Join(configHome, "release-transition/v2/ledger.json"), []byte(fmt.Sprintf(`{"schemaVersion":2,"domains":{"settings":{"epoch":%d,"applied":[]}}}`, epoch)), 0o600)
}

func TestPreparedRollbackRunsNoHookFromAnUnverifiedRelease(t *testing.T) {
	fixture := newPreparedRollbackFixture(t, preparedSample)
	testkit.WriteFile(t, filepath.Join(fixture.runtimeRoot, "releases", fixture.activeID, "config/profiles/sample/runtime.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o700)
	if _, err := fixture.prepare(t); err == nil || len(fixture.observed) != 0 {
		t.Fatalf("unverified active release: err %v, observed %v", err, fixture.observed)
	}
}

func TestDowngradeActivationRefusesAnUnpreparedFormat(t *testing.T) {
	fixture := newPreparedRollbackFixture(t, preparedSample)
	active, target := fixture.verified(t)
	links, err := fixture.runtime.inspectRuntimeLinks(fixture.runtimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	request := releasetransition.ProcessRequest{SchemaVersion: 1, Mode: releasetransition.ProcessInspect, RuntimeRoot: fixture.runtimeRoot, ConfigHome: fixture.home,
		Target: target.candidate.release, Direction: releasetransition.DirectionActivateTarget, ArtifactDigest: target.manifestDigest}
	selection, err := fixture.runtime.captureReleaseSelection(context.Background(), target, request, links)
	if err != nil {
		t.Fatal(err)
	}
	for retained, retry := range map[bool]string{true: rollbackRetry, false: "keep the current release active"} {
		refusal := asRollbackRefusal(fixture.runtime.downgradeFormatRefusal(selection, target, retained))
		if refusal.retry != retry || !strings.Contains(refusal.message, "reads profile runtime sample-runtime in format 1, but its installed state has format 2") {
			t.Fatalf("downgrade refusal = %#v", refusal)
		}
	}
	// Equal formats need nothing.
	if err := fixture.runtime.downgradeFormatRefusal(selection, active, false); err != nil {
		t.Fatal(err)
	}
}

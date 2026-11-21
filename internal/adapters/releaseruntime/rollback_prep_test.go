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

func declareRollbackPrepYard(t *testing.T, home, yard string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, "yards", yard), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(home, "yards", yard, "config.env"), []byte("\n"), 0o600)
}

// preparedRollbackFixture publishes sealed active and retained releases with
// a synthetic sample runtime in yards default and named, after a completed
// update to the active release. Each yard's state has format 2 (the active
// release's), 1, or 0 for a stopped yard. The active engine reports its own
// inspection as ready, or as the drift its conversions cause; the target
// engine logs its inspections and fails its convergence.
type preparedRollbackFixture struct {
	runtime                 *Runtime
	runtimeRoot, home, log  string
	drift                   string
	activeID, targetID      string
	formats                 map[string]int
	consequence             map[string]string
	observed, applies, gate []string
	gateErr, applyErr       error
	apply                   func()
}

type preparedRollbackOptions struct {
	active, target string // the sample runtime's extra declaration fields
	seed           func(active, target string)
}

var preparedSample = preparedRollbackOptions{active: `,"format":2,"prepares_rollback_to":1`}

const prepTransaction = "tx-0123456789abcdef"

func newPreparedRollbackFixture(t *testing.T, options preparedRollbackOptions) *preparedRollbackFixture {
	t.Helper()
	runtime, active, target, home, _ := retainedRuntimeFixture(t)
	t.Cleanup(func() { _ = runtime.Close() })
	declareRollbackPrepYard(t, home, "named")
	root := filepath.Dir(active)
	fixture := &preparedRollbackFixture{runtime: runtime, runtimeRoot: filepath.Join(root, "runtime"), home: home, log: filepath.Join(root, "engine.log"),
		drift: filepath.Join(root, "drift.json"), activeID: "0.31.0-active", targetID: UpdateAnchorVersion + "-target",
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
	ready := fmt.Sprintf(`{"schemaVersion":1,"activationReconciliationOwned":true,"inspection":{"plan":"plan-v1-cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","assessment":{"action":"release.transition.v2","effect":"mutation","changed":false,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible"},"outcome":{"status":"ready","reachedGoal":true,"active":"%s","previous":"%s","target":"%s","code":"ready","message":"verified","transaction":"%s"}}}`, fixture.activeID, fixture.targetID, fixture.activeID, prepTransaction)
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
  *'"direction":"activate-target"'*) cat %q 2>/dev/null || printf '%%s\n' %q ;;
  *) printf 'inspect\n' >>%q; printf '%%s\n' %q ;;
 esac ;;
 *) exit 64 ;;
esac
`, candidate.version, fixture.log, fixture.drift, ready, fixture.log, inspection)
		testkit.WriteFile(t, filepath.Join(candidate.path, "bin/yard-engine"), []byte(engine), 0o700)
		testkit.WriteFile(t, filepath.Join(candidate.path, "config/release-transition.json"), []byte(sourceAdmissionRegistry), 0o600)
		sealPrepRelease(t, candidate.path)
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
	fixture.writeCompletedUpdate(t)
	runtime.config.ProfileRuntimeRollbackObservation = func(_ context.Context, root string, loaded config.Loaded, id string) (ports.RuntimeObservation, error) {
		fixture.observed = append(fixture.observed, loaded.Context.YardName+" "+id+" "+releaseRootName(t, root))
		return fixture.observe(loaded.Context.YardName), nil
	}
	runtime.config.ProfileRuntimeRollbackApply = func(_ context.Context, root string, loaded config.Loaded, id, operation, actual, desired, consequence string) error {
		if fixture.apply != nil {
			fixture.apply()
		}
		if observed := fixture.observe(loaded.Context.YardName); string(observed.Actual) != actual || string(observed.Desired) != desired || observed.Consequence != consequence {
			return domain.ErrPlanStale
		}
		if fixture.applyErr != nil {
			return fixture.applyErr
		}
		fixture.formats[loaded.Context.YardName] = 1
		fixture.applies = append(fixture.applies, loaded.Context.YardName+" "+id+" "+loaded.Context.YardInstanceName+" "+releaseRootName(t, root))
		// The active release's own observe now reports the converted owner.
		fixture.reportDrift(t, `{"resource":"activation.sample-runtime","scope":"activation","decision":"canonicalize","result":"converged"},{"resource":"activation.sample-runtime","scope":"yard-attribution","decision":"preserve","result":"drift `+loaded.Context.YardName+`"}`)
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

func writePrepProfile(t *testing.T, root, name, declaration string) {
	t.Helper()
	directory := filepath.Join(root, "config/profiles", name)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(directory, "runtime.sh"), []byte("#!/bin/sh\n"), 0o700)
	testkit.WriteFile(t, filepath.Join(directory, "profile.json"), []byte(declaration), 0o600)
}

func sealPrepRelease(t *testing.T, root string) {
	t.Helper()
	var manifest strings.Builder
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || path == filepath.Join(root, "runtime-files.sha256") {
			return err
		}
		payload, err := os.ReadFile(path)
		if err == nil {
			relative, _ := filepath.Rel(root, path)
			fmt.Fprintf(&manifest, "%x  ./%s\n", sha256.Sum256(payload), relative)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "runtime-files.sha256"), []byte(manifest.String()), 0o600)
}

// writeCompletedUpdate records the completed update to the active release.
func (fixture *preparedRollbackFixture) writeCompletedUpdate(t *testing.T) {
	t.Helper()
	active, _ := fixture.verified(t)
	digest := releasetransition.Fingerprint(strings.Repeat("e", 64))
	authorization, resume := "plan-v1-"+releasetransition.PlanToken(strings.Repeat("a", 64)), "resume-v1-"+releasetransition.PlanToken(strings.Repeat("b", 64))
	intent, err := json.Marshal(struct {
		AuthorizationPlan releasetransition.PlanToken     `json:"authorizationPlan"`
		ResumePlan        releasetransition.PlanToken     `json:"resumePlan"`
		ObservationScope  releasetransition.Fingerprint   `json:"observationScope"`
		Steps             []releasetransition.JournalStep `json:"steps"`
	}{authorization, resume, digest, []releasetransition.JournalStep{}})
	if err != nil {
		t.Fatal(err)
	}
	journal := releasetransition.JournalRecord{
		SchemaVersion: releasetransition.JournalSchemaV2, Transaction: prepTransaction,
		Goal:              releasetransition.Goal{Target: releasetransition.ReleaseID(fixture.activeID), Direction: releasetransition.DirectionActivateTarget},
		Releases:          releasetransition.ReleasePair{From: releasetransition.ReleaseID(fixture.targetID), Target: releasetransition.ReleaseID(fixture.activeID)},
		AuthorizationPlan: authorization, ResumePlan: resume,
		ArtifactDigest: active.manifestDigest, RegistryDigest: active.registryDigest, CatalogDigest: digest, ObservationScope: digest,
		AuthorizationDigest: digest, IntentDigest: releasetransition.Fingerprint(fmt.Sprintf("%x", sha256.Sum256(intent))), Checkpoint: releasetransition.JournalComplete, Steps: []releasetransition.JournalStep{},
	}
	payload, err := releasetransition.MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(fixture.home, "release-transition/v2/journal.json"), payload, 0o600)
}

// reportDrift makes the active release's own inspection report the given
// unconverged decisions.
func (fixture *preparedRollbackFixture) reportDrift(t *testing.T, decisions string) {
	t.Helper()
	testkit.WriteFile(t, fixture.drift, []byte(fmt.Sprintf(`{"schemaVersion":1,"activationReconciliationOwned":true,"inspection":{"plan":"plan-v1-dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible","consequences":["refresh profile runtime"]},"decisions":[%s],"outcome":{"status":"migration-required","reachedGoal":false,"active":"%s","previous":"%s","target":"%s","code":"transition-required","message":"activation drift","retry":"run yard update"}}}`+"\n",
		decisions, fixture.activeID, fixture.targetID, fixture.activeID)), 0o600)
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

func (fixture *preparedRollbackFixture) assertUnchanged(t *testing.T) {
	t.Helper()
	for name, id := range map[string]string{"current": fixture.activeID, "previous": fixture.targetID} {
		if got, err := os.Readlink(filepath.Join(fixture.runtimeRoot, name)); err != nil || got != "releases/"+id {
			t.Fatalf("%s link = %q, %v", name, got, err)
		}
	}
	if strings.Contains(fixture.engineLog(t), "converge") {
		t.Fatal("the target converged")
	}
}

func outcomeOf(t *testing.T, err error) releasetransition.Outcome {
	t.Helper()
	outcome, ok := TransitionOutcome(err)
	if !ok {
		t.Fatalf("untyped refusal: %v", err)
	}
	return outcome
}

func retryAction(t *testing.T, retry string) domain.Action {
	t.Helper()
	action, _, ok := domain.ParseRetry(retry)
	if !ok {
		t.Fatalf("untyped next step %q", retry)
	}
	return action
}

func TestRollbackPreparesOwnersByDeclaredFormats(t *testing.T) {
	for _, test := range []struct {
		name, active, target string
		prepared             bool
		refusal              string
	}{
		{name: "undeclared formats"},
		{name: "same declared format", active: `,"format":2`, target: `,"format":2`},
		{name: "absent target format is 1", active: `,"format":2,"prepares_rollback_to":1`, prepared: true},
		{name: "declared target format", active: `,"format":3,"prepares_rollback_to":2`, target: `,"format":2`, prepared: true},
		{name: "unprepared format", active: `,"format":2`, refusal: "declares other profile runtime formats than the active release: sample-runtime 1 instead of 2"},
		{name: "other prepared format", active: `,"format":3,"prepares_rollback_to":2`, refusal: "sample-runtime 1 instead of 3"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPreparedRollbackFixture(t, preparedRollbackOptions{active: test.active, target: test.target})
			prepared, err := fixture.prepare(t)
			if test.refusal != "" {
				if outcome := outcomeOf(t, err); !strings.Contains(outcome.Message, test.refusal) || outcome.Retry != keepActiveRetry || len(fixture.observed) != 0 {
					t.Fatalf("refusal = %#v, observed %v", outcome, fixture.observed)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !test.prepared {
				if prepared.Action != "update.rollback" || len(fixture.observed) != 0 {
					t.Fatalf("an unprepared rollback became %s and ran hooks %v", prepared.Action, fixture.observed)
				}
				return
			}
			want := []string{"default sample-runtime " + fixture.activeID, "named sample-runtime " + fixture.activeID}
			if prepared.Action != PrepareRollbackAction || !slices.Equal(fixture.observed, want) || !slices.Equal(fixture.gate, []string{"named sample-runtime"}) {
				t.Fatalf("action %s, observed %v, gate %v", prepared.Action, fixture.observed, fixture.gate)
			}
		})
	}
}

// Step 1 is only the conversion: it stops with this release active and
// keeps no record. Step 2 is the ordinary rollback, admitted although the
// active release now reports the converted owner as drift.
func TestRollbackThroughAProfileHookTakesTwoCommands(t *testing.T) {
	fixture := newPreparedRollbackFixture(t, preparedSample)
	fixture.consequence["default"] = "restart the sample service"
	prepared, err := fixture.prepare(t)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.engineLog(t) != "" || len(fixture.applies) != 0 {
		t.Fatalf("planning inspected the target or converted: log %q applies %v", fixture.engineLog(t), fixture.applies)
	}
	if prepared.Action != PrepareRollbackAction || !prepared.Changed || len(prepared.Steps) != 1 ||
		prepared.Consequences[0] != "prepare profile runtime sample-runtime in yard default for release "+fixture.targetID+": restart the sample service" ||
		slices.Contains(prepared.Consequences, "reactivate the verified retained previous runtime") {
		t.Fatalf("step 1 plan = %#v", prepared)
	}
	if err := prepared.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fixture.applies, []string{"default sample-runtime yard " + fixture.activeID}) ||
		!slices.Contains(fixture.gate, "default sample-runtime") || fixture.engineLog(t) != "" {
		t.Fatalf("applies %v, gate %v, target engine log %q", fixture.applies, fixture.gate, fixture.engineLog(t))
	}
	fixture.assertUnchanged(t)
	inspection, err := prepared.Check(context.Background())
	if err != nil || inspection.Outcome.Status != releasetransition.StatusMigrationRequired || inspection.Outcome.Active != releasetransition.ReleaseID(fixture.activeID) ||
		retryAction(t, inspection.Outcome.Retry) != (domain.Action{Verb: domain.VerbUpdateRollback}) || !strings.Contains(inspection.Outcome.Message, "yard migrate undoes") {
		t.Fatalf("step 1 result = %#v, %v", inspection.Outcome, err)
	}

	rerun, err := fixture.prepare(t)
	if err != nil {
		t.Fatal(err)
	}
	if rerun.Action != "update.rollback" || !slices.Contains(rerun.Consequences, "reactivate the verified retained previous runtime") {
		t.Fatalf("step 2 plan = %s %#v", rerun.Action, rerun.Consequences)
	}
	// Execution observes the prepared owners again before the target runs.
	fixture.formats["named"] = 2
	outcome := outcomeOf(t, rerun.Execute(context.Background()))
	if !strings.Contains(outcome.Message, "yard named: profile runtime sample-runtime must first be prepared for rollback") ||
		retryAction(t, outcome.Retry) != (domain.Action{Verb: domain.VerbUpdateRollback}) {
		t.Fatalf("a changed owner did not refuse step 2: %#v", outcome)
	}
	fixture.assertUnchanged(t)
	fixture.formats["named"] = 1
	if err := rerun.Execute(context.Background()); err == nil || !strings.HasSuffix(fixture.engineLog(t), "converge\n") || len(fixture.applies) != 1 {
		t.Fatalf("step 2 did not dispatch the target plan alone: %v, log %q", err, fixture.engineLog(t))
	}
}

// Only drift of owners prepared for the retained release admits a rollback
// from an active release that is not ready; any other drift keeps its repair.
func TestRollbackAdmitsOnlyPreparedOwnerDrift(t *testing.T) {
	for name, test := range map[string]struct {
		decisions string
		admitted  bool
	}{
		"prepared owner": {`{"resource":"activation.sample-runtime","scope":"activation","decision":"canonicalize","result":"converged"}`, true},
		"another owner":  {`{"resource":"activation.other-runtime","scope":"activation","decision":"canonicalize","result":"converged"}`, false},
		"failed yard":    {`{"resource":"activation.sample-runtime","scope":"yard-attribution","decision":"preserve","result":"failed default"}`, false},
		"migration":      {`{"resource":"migration-history.checkpoint","scope":"migration-history","decision":"transform","result":"compact-migration-history"}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newPreparedRollbackFixture(t, preparedSample)
			fixture.formats["default"] = 1
			fixture.reportDrift(t, test.decisions)
			prepared, err := fixture.prepare(t)
			if test.admitted && (err != nil || prepared.Action != "update.rollback") ||
				!test.admitted && (err == nil || !strings.Contains(err.Error(), "permits only exact target "+fixture.activeID)) {
				t.Fatalf("action %s, %v", prepared.Action, err)
			}
		})
	}
}

// The plan reports every independent refusal with its next step and cause:
// a format this release does not prepare, a failed observation, a yard that
// cannot be loaded and a stopped yard.
func TestRollbackPreparationCollectsEveryRefusal(t *testing.T) {
	fixture := newPreparedRollbackFixture(t, preparedRollbackOptions{active: preparedSample.active, seed: func(active, target string) {
		for root, format := range map[string]int{active: 4, target: 3} {
			writePrepProfile(t, root, "other", fmt.Sprintf(`{"schema_version":1,"runtime":{"activation_id":"other-runtime","handler":"runtime.sh","format":%d}}`, format))
		}
	}})
	for _, yard := range []string{"third", "broken"} {
		declareRollbackPrepYard(t, fixture.home, yard)
	}
	testkit.WriteFile(t, filepath.Join(fixture.home, "yards/broken/config.env"), []byte("NOT_A_SETTING=1\n"), 0o600)
	fixture.formats = map[string]int{"default": 2, "named": 0, "third": 2}
	observe := fixture.runtime.config.ProfileRuntimeRollbackObservation
	fixture.runtime.config.ProfileRuntimeRollbackObservation = func(ctx context.Context, root string, loaded config.Loaded, id string) (ports.RuntimeObservation, error) {
		if loaded.Context.YardName == "third" {
			return ports.RuntimeObservation{}, errors.New("guest exec failed")
		}
		return observe(ctx, root, loaded, id)
	}
	_, err := fixture.prepare(t)
	outcome := outcomeOf(t, err)
	for _, part := range []string{
		"other-runtime 3 instead of 4",
		"yard named is stopped, so profile runtime sample-runtime cannot be prepared for rollback",
		"yard third: profile runtime sample-runtime cannot be observed for rollback preparation",
		"yard broken: cannot safely read settings for rollback preparation",
	} {
		if !strings.Contains(outcome.Message, part) {
			t.Fatalf("outcome %q lacks %q", outcome.Message, part)
		}
	}
	if outcome.Code != releasetransition.CodeRollbackIncompatible || outcome.Retry != keepActiveRetry || !strings.Contains(err.Error(), "guest exec failed") || len(fixture.applies) != 0 {
		t.Fatalf("outcome %#v, cause %v", outcome, err)
	}
	// Alone, each refusal names its own next step.
	fixture = newPreparedRollbackFixture(t, preparedSample)
	fixture.formats["named"] = 0
	_, err = fixture.prepare(t)
	if action := retryAction(t, outcomeOf(t, err).Retry); action != (domain.Action{Verb: domain.VerbStart, Yard: "named"}) {
		t.Fatalf("stopped yard next step = %#v", action)
	}
	fixture = newPreparedRollbackFixture(t, preparedSample)
	fixture.formats["default"] = 1
	fixture.gateErr = errors.New("unknown state layout")
	if _, err = fixture.prepare(t); outcomeOf(t, err).Retry != migrateRetry || !strings.Contains(err.Error(), "unknown state layout") {
		t.Fatalf("gate refusal = %v", err)
	}
}

// The conversion is offered only once every other rollback admission passes.
func TestRollbackPreparationIsOfferedOnlyForAnAdmissibleRollback(t *testing.T) {
	fixture := newPreparedRollbackFixture(t, preparedRollbackOptions{active: preparedSample.active, seed: func(active, _ string) {
		writePrepProfile(t, active, "extra", `{"schema_version":1,"default_yards":["default"],"runtime":{"activation_id":"extra-runtime","handler":"runtime.sh"}}`)
	}})
	_, err := fixture.prepare(t)
	if outcome := outcomeOf(t, err); outcome.Message != "the retained release cannot verify installed runtime activation" || len(fixture.applies) != 0 {
		t.Fatalf("refusal = %#v", outcome)
	}
}

// Execution rechecks the confirmed selection and observations before the
// first conversion; any change converts nothing.
func TestRollbackPreparationRechecksBeforeAnyEffect(t *testing.T) {
	for name, change := range map[string]func(*testing.T, *preparedRollbackFixture){
		"observation":     func(_ *testing.T, fixture *preparedRollbackFixture) { fixture.formats["named"] = 2 },
		"declared effect": func(_ *testing.T, fixture *preparedRollbackFixture) { fixture.consequence["default"] = "restart" },
		"stopped yard":    func(_ *testing.T, fixture *preparedRollbackFixture) { fixture.formats["named"] = 0 },
		"protected history": func(t *testing.T, fixture *preparedRollbackFixture) {
			writeRollbackFixtureLedgerEpoch(t, fixture.home, 2)
		},
		"yard instance": func(t *testing.T, fixture *preparedRollbackFixture) {
			testkit.WriteFile(t, filepath.Join(fixture.home, "yards/named/config.env"), []byte("YARD_INSTANCE_NAME=changed-guest\n"), 0o600)
		},
		"active release": func(t *testing.T, fixture *preparedRollbackFixture) {
			testkit.WriteFile(t, filepath.Join(fixture.runtimeRoot, "releases", fixture.activeID, "config/profiles/sample/runtime.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o700)
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newPreparedRollbackFixture(t, preparedSample)
			prepared, err := fixture.prepare(t)
			if err != nil {
				t.Fatal(err)
			}
			change(t, fixture)
			err = prepared.Execute(context.Background())
			if outcome := outcomeOf(t, err); !errors.Is(err, domain.ErrPlanStale) || outcome.Code != releasetransition.CodePlanStale || outcome.Retry == "" {
				t.Fatalf("changed %s = %v", name, err)
			}
			if len(fixture.applies) != 0 {
				t.Fatalf("a stale plan converted %v", fixture.applies)
			}
			fixture.assertUnchanged(t)
		})
	}
}

func writeRollbackFixtureLedgerEpoch(t *testing.T, configHome string, epoch int) {
	t.Helper()
	testkit.WriteFile(t, filepath.Join(configHome, "release-transition/v2/ledger.json"), []byte(fmt.Sprintf(`{"schemaVersion":2,"domains":{"settings":{"epoch":%d,"applied":[]}}}`, epoch)), 0o600)
}

// After a conversion could have run, a failure keeps this release active and
// names the rerun, the refusal's own next step, or yard migrate.
func TestRollbackPreparationFailuresNameTheirNextStep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	for name, test := range map[string]struct {
		arrange func(*preparedRollbackFixture)
		cause   error
		retry   string
	}{
		"apply failure": {func(fixture *preparedRollbackFixture) { fixture.applyErr = errors.New("conversion failed") }, nil, rollbackRetry},
		"gate refusal": {func(fixture *preparedRollbackFixture) {
			fixture.apply = func() { fixture.gateErr = errors.New("rejected") }
		}, nil, migrateRetry},
		"cancellation": {func(fixture *preparedRollbackFixture) { fixture.apply = cancel; fixture.applyErr = context.Canceled }, context.Canceled, rollbackRetry},
		"changed effect": {func(fixture *preparedRollbackFixture) {
			fixture.apply = func() { fixture.consequence["default"] = "other" }
		}, domain.ErrPlanStale, rollbackRetry},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newPreparedRollbackFixture(t, preparedSample)
			prepared, err := fixture.prepare(t)
			if err != nil {
				t.Fatal(err)
			}
			test.arrange(fixture)
			run := context.Background()
			if test.cause == context.Canceled {
				run = ctx
			}
			err = prepared.Execute(run)
			outcome := outcomeOf(t, err)
			if outcome.Retry != test.retry || !strings.Contains(outcome.Message, "stays active and its profile runtimes may be partly prepared") ||
				!strings.Contains(outcome.Message, "yard migrate to undo") || test.cause != nil && !errors.Is(err, test.cause) {
				t.Fatalf("failure = %#v, %v", outcome, err)
			}
			fixture.assertUnchanged(t)
		})
	}
}

func TestRollbackPreparationRunsNoHookFromAnUnverifiedRelease(t *testing.T) {
	fixture := newPreparedRollbackFixture(t, preparedSample)
	testkit.WriteFile(t, filepath.Join(fixture.runtimeRoot, "releases", fixture.activeID, "config/profiles/sample/runtime.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o700)
	if _, err := fixture.prepare(t); err == nil || len(fixture.observed) != 0 {
		t.Fatalf("unverified active release: err %v, observed %v", err, fixture.observed)
	}
}

// yard update --version to an older release refuses another declared runtime
// format, also when the profiles cannot be read; for the retained release
// that this release prepares for, the next step is the rollback.
func TestExplicitVersionRefusesAnotherRuntimeFormat(t *testing.T) {
	for name, test := range map[string]struct {
		active, target, refusal, retry string
	}{
		"preparable":   {`,"format":2,"prepares_rollback_to":1`, ``, "sample-runtime 1 instead of 2", rollbackRetry},
		"unpreparable": {`,"format":2`, ``, "sample-runtime 1 instead of 2", keepActiveRetry},
		"unreadable":   {`,"format":2,"prepares_rollback_to":1`, `,"format":"1"`, "cannot be compared", keepActiveRetry},
	} {
		t.Run(name, func(t *testing.T) {
			runtime, candidate, request, called, args := retainedVersionFixture(t)
			request.Direction = releasetransition.DirectionActivateTarget
			active := publishedCandidate{release: "release-a", root: filepath.Join(request.RuntimeRoot, "releases", "release-a")}
			for _, release := range []struct {
				candidate publishedCandidate
				fields    string
			}{{active, test.active}, {candidate, test.target}} {
				writePrepProfile(t, release.candidate.root, "sample", `{"schema_version":1,"runtime":{"activation_id":"sample-runtime","handler":"runtime.sh"`+release.fields+`}}`)
			}
			// Equal registries leave the format rule as the only refusal.
			writeMigrationRollbackSource(t, candidate, request, []byte(sourceAdmissionRegistry))
			rewriteReleaseOwnerManifest(t, candidate)
			_, err := runtime.PrepareTransition(context.Background(), args, request.ConfigHome, request.Yard)
			outcome := outcomeOf(t, err)
			if outcome.Code != releasetransition.CodeRollbackIncompatible || !strings.Contains(outcome.Message, test.refusal) || outcome.Retry != test.retry {
				t.Fatalf("refusal = %#v", outcome)
			}
			assertMigrationRollbackUnchanged(t, request, called)
		})
	}
}

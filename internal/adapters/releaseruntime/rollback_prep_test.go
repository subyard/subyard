package releaseruntime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
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
		// The active release's own inspection now reports the converted owner.
		var converted []string
		for _, apply := range fixture.applies {
			converted = append(converted, strings.Fields(apply)[0])
		}
		fixture.reportEngineDrift(t, converted...)
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
		ArtifactDigest: active.manifestDigest, RegistryDigest: active.registryDigest, CatalogDigest: releasetransition.BuiltinCapabilityCatalog().Digest(), ObservationScope: digest,
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

// preparedDriftOwner is the sample runtime as the active release's engine
// sees it after preparation: unconverged, drifting in the given yards.
type preparedDriftOwner struct {
	v0111FixtureReconciler
	yards []string
}

func (preparedDriftOwner) ID() string { return "sample-runtime" }

func (owner preparedDriftOwner) Observe(ctx context.Context, pair releasetransition.ReleasePair, links releasetransition.ReleaseLinks) (releasetransition.V2ActivationObservation, error) {
	observation, err := owner.v0111FixtureReconciler.Observe(ctx, pair, links)
	for _, yard := range owner.yards {
		observation.Yards = append(observation.Yards, releasetransition.V2YardAttribution{Kind: releasetransition.YardFindingDrift, Yard: yard})
	}
	return observation, err
}

// reportEngineDrift makes the active release report the inspection the real
// V2 engine produces for the completed update with the sample runtime
// drifting in the given yards, completed-history metadata included.
func (fixture *preparedRollbackFixture) reportEngineDrift(t *testing.T, yards ...string) {
	t.Helper()
	ctx := context.Background()
	active, _ := fixture.verified(t)
	raw, err := os.ReadFile(filepath.Join(fixture.home, "release-transition/v2/journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	journal, err := releasetransition.ParseJournal(raw)
	if err != nil {
		t.Fatal(err)
	}
	links, err := releasetransition.NewRuntimeLinkStore(fixture.runtimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := releasetransition.NewV2Transition(releasetransition.V2Options{
		ConfigHome: fixture.home, RegistryPayload: []byte(sourceAdmissionRegistry), ArtifactDigest: active.manifestDigest,
		Releases: journal.Releases, Direction: journal.Goal.Direction,
		Reconcilers: []releasetransition.V2ActivationReconciler{preparedDriftOwner{
			v0111FixtureReconciler{statePath: filepath.Join(fixture.home, "native-state"), desired: v0111FixtureFingerprint("normal")}, yards}},
		ObserveLinks: func(context.Context) (releasetransition.ReleaseLinks, error) { return links.Observe() },
		ActivateLinks: func(_ context.Context, pair releasetransition.ReleasePair) (releasetransition.ReleaseLinks, error) {
			return links.Activate(pair)
		},
		CompletedMetadata: func(ctx context.Context) (releasetransition.Fingerprint, error) {
			proof, err := fixture.runtime.InspectSourceAdmission(ctx, fixture.runtimeRoot, fixture.home)
			return releasetransition.Fingerprint(proof.Binding), err
		},
		VerifyAuthorization: func(releasetransition.PlanToken, releasetransition.Authorization) bool { return false },
	})
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := engine.InspectProcessV1(ctx, journal.Goal)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(inspection.Decisions, func(decision releasetransition.RedactedDecision) bool {
		return decision.Resource == "transition.source-metadata"
	}) {
		t.Fatalf("the engine reported no completed history: %#v", inspection.Decisions)
	}
	payload, err := json.Marshal(releasetransition.ProcessResponse{SchemaVersion: 1, ActivationReconciliationOwned: true, Inspection: &inspection})
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, fixture.drift, append(payload, '\n'), 0o600)
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
	var progress strings.Builder
	fixture.runtime.config.Progress = &progress
	prepared, err := fixture.prepare(t)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.engineLog(t) != "" || len(fixture.applies) != 0 {
		t.Fatalf("planning inspected the target or converted: log %q applies %v", fixture.engineLog(t), fixture.applies)
	}
	// The confirmation shows each conversion and that this release stays
	// active until a second confirmation.
	if prepared.Action != PrepareRollbackAction || !prepared.Changed || len(prepared.Steps) != 2 ||
		!slices.Equal(domain.OperationStepConsequences(prepared.Steps), prepared.Consequences) || !slices.Equal(prepared.Consequences, []string{
		"prepare profile runtime sample-runtime for release " + fixture.targetID + " in yard default: restart the sample service",
		"keep release " + fixture.activeID + " active; then run yard update --rollback to review and confirm the rollback",
	}) {
		t.Fatalf("step 1 plan = %#v", prepared)
	}
	if err := prepared.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The admission reruns print no progress of their own.
	if strings.Count(progress.String(), "Inspecting the installed release...") != 1 || strings.Count(progress.String(), "Installed version: ") > 1 {
		t.Fatalf("step 1 progress:\n%s", progress.String())
	}
	// The gate is the second command's admission, target inspection included.
	if !slices.Equal(fixture.applies, []string{"default sample-runtime yard " + fixture.activeID}) ||
		!slices.Contains(fixture.gate, "default sample-runtime") || fixture.engineLog(t) != "inspect\n" {
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
// from an active release that is not ready; yard attribution is not
// consulted. Any other decision keeps its repair. The engine cases are the
// real engine's inspection, completed history included.
func TestRollbackAdmitsOnlyPreparedOwnerDrift(t *testing.T) {
	const activation = `{"resource":"activation.sample-runtime","scope":"activation","decision":"canonicalize","result":"converged"}`
	for name, test := range map[string]struct {
		decisions string   // hand-written decisions, or
		yards     []string // the engine's drift attribution
		admitted  bool
	}{
		"prepared yards":         {yards: []string{"default", "named"}, admitted: true},
		"unattributed drift":     {yards: []string{}, admitted: true},
		"failed yard":            {decisions: activation + `,{"resource":"activation.sample-runtime","scope":"yard-attribution","decision":"preserve","result":"failed default"}`, admitted: true},
		"another owner":          {decisions: `{"resource":"activation.other-runtime","scope":"activation","decision":"canonicalize","result":"converged"}`},
		"migration":              {decisions: `{"resource":"migration-history.checkpoint","scope":"migration-history","decision":"transform","result":"compact-migration-history"}`},
		"other history decision": {decisions: activation + `,{"resource":"transition.source-metadata","decision":"transform","result":"metadata"}`},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newPreparedRollbackFixture(t, preparedSample)
			fixture.formats["default"] = 1
			if test.decisions != "" {
				fixture.reportDrift(t, test.decisions)
			} else {
				fixture.reportEngineDrift(t, test.yards...)
			}
			prepared, err := fixture.prepare(t)
			if test.admitted && (err != nil || prepared.Action != rollbackAction) ||
				!test.admitted && (err == nil || !strings.Contains(err.Error(), "permits only exact target "+fixture.activeID)) {
				t.Fatalf("action %s, %v", prepared.Action, err)
			}
		})
	}
}

// Past that rule, rollback-observe alone routes each yard: stale, also
// partway through a conversion, offers its preparation; a stopped yard or a
// failed observation refuses with its own next step and cause.
func TestPreparedDriftRoutesEachYardByRollbackObserve(t *testing.T) {
	intermediate := strings.Repeat("9", 64)
	for name, test := range map[string]struct {
		arrange func(*preparedRollbackFixture)
		check   func(*testing.T, *preparedRollbackFixture, Prepared, error)
	}{
		"partway through a conversion": {func(fixture *preparedRollbackFixture) {
			observe := fixture.runtime.config.ProfileRuntimeRollbackObservation
			fixture.runtime.config.ProfileRuntimeRollbackObservation = func(ctx context.Context, root string, loaded config.Loaded, id string) (ports.RuntimeObservation, error) {
				observation, err := observe(ctx, root, loaded, id)
				if loaded.Context.YardName == "default" && fixture.formats["default"] == 3 {
					observation = ports.RuntimeObservation{State: ports.RuntimeStateStale, Actual: intermediate, Desired: prepOld}
				}
				return observation, err
			}
			fixture.formats["default"] = 3
		}, func(t *testing.T, fixture *preparedRollbackFixture, prepared Prepared, err error) {
			if err != nil || prepared.Action != PrepareRollbackAction || len(prepared.Steps) != 2 || prepared.Steps[0].Observed != "rollback-observe stale in yard default" {
				t.Fatalf("resumed plan = %s %#v, %v", prepared.Action, prepared.Steps, err)
			}
		}},
		"stopped yard": {func(fixture *preparedRollbackFixture) { fixture.formats["default"] = 0 }, func(t *testing.T, _ *preparedRollbackFixture, _ Prepared, err error) {
			if outcome := outcomeOf(t, err); retryAction(t, outcome.Retry) != (domain.Action{Verb: domain.VerbStart, Yard: "default"}) || !strings.Contains(outcome.Message, "yard default is stopped") {
				t.Fatalf("stopped prepared yard = %#v", outcome)
			}
		}},
		"failed retained observation": {func(fixture *preparedRollbackFixture) {
			fixture.gateErr = errors.New("retained observation unavailable")
		},
			func(t *testing.T, fixture *preparedRollbackFixture, _ Prepared, err error) {
				if outcome := outcomeOf(t, err); outcome.Retry != migrateRetry || !errors.Is(err, fixture.gateErr) {
					t.Fatalf("failed observation = %#v, %v", outcome, err)
				}
			}},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newPreparedRollbackFixture(t, preparedSample)
			fixture.formats["default"] = 1
			fixture.reportEngineDrift(t, "default")
			test.arrange(fixture)
			prepared, err := fixture.prepare(t)
			test.check(t, fixture, prepared, err)
		})
	}
}

// The engine omits yard attribution beyond its wire limit, and the plan
// stays within the operation step limit: step 2 stays reachable after
// preparing more yards than a plan has steps.
func TestRollbackPreparationOfEveryYardReachesTheRollback(t *testing.T) {
	fixture := newPreparedRollbackFixture(t, preparedSample)
	fixture.formats["named"] = 2
	for index := 2; index < 300; index++ {
		yard := fmt.Sprintf("y%03d", index)
		declareRollbackPrepYard(t, fixture.home, yard)
		fixture.formats[yard] = 2
	}
	prepared, err := fixture.prepare(t)
	if err != nil || len(prepared.Steps) != 2 || domain.ValidateOperationSteps(prepared.Steps) != nil ||
		!strings.HasPrefix(prepared.Steps[0].Observed, "rollback-observe stale in 300 yards: default, named, ") || !strings.HasSuffix(prepared.Steps[0].Consequence, " and 271 more") {
		t.Fatalf("plan = %#v, %v", prepared.Steps, err)
	}
	if err := prepared.Execute(context.Background()); err != nil || len(fixture.applies) != 300 {
		t.Fatalf("preparation = %v, %d conversions", err, len(fixture.applies))
	}
	if rollback, err := fixture.prepare(t); err != nil || rollback.Action != rollbackAction {
		t.Fatalf("after preparation = %s, %v", rollback.Action, err)
	}
}

// Each owner and declared consequence is one step whatever its yard count.
// A step shows its consequence whole and shortens only its yard list; the
// binding covers every yard.
func TestRollbackPreparationPlansOneStepPerConsequence(t *testing.T) {
	var items []formatItem
	for index := range 400 {
		owner, consequence := "first-runtime", "restart the first service"
		if index%2 == 1 {
			owner, consequence = "second-runtime", fmt.Sprintf("rewrite unit %d of the second service %s", index%3, strings.Repeat("x", 150))
		}
		items = append(items, formatItem{owner: owner, yard: fmt.Sprintf("yard-%03d", index/2), state: ports.RuntimeStateStale, consequence: consequence})
	}
	items = append(items, formatItem{owner: "third-runtime", yard: "yard-000", state: ports.RuntimeStateCurrent},
		formatItem{owner: "third-runtime", yard: "yard-001", state: ports.RuntimeStateStale})
	steps := ownerPreparationSteps(items, "0.28.0")
	if err := domain.ValidateOperationSteps(steps); err != nil || len(steps) != 5 {
		t.Fatalf("steps = %#v, %v", steps, err)
	}
	for _, item := range items {
		if item.consequence != "" && !slices.ContainsFunc(steps, func(step domain.OperationStep) bool {
			return step.Target == "activation."+item.owner && strings.HasSuffix(step.Consequence, ": "+item.consequence)
		}) {
			t.Fatalf("the plan hides %s's consequence %q: %#v", item.owner, item.consequence, steps)
		}
	}
	if steps[0].Target != "activation.first-runtime" || steps[1].Target != "activation.second-runtime" || steps[1].DependsOn[0] != steps[0].ID ||
		steps[0].Observed != "rollback-observe stale in 200 yards: yard-000, yard-001, yard-002, yard-003, yard-004, yard-005, yard-006, yard-007, yard-008, yard-009, yard-010, yard-011, yard-012, yard-013, yard-014, yard-015, yard-016 and 183 more" ||
		!strings.HasPrefix(steps[0].Consequence, "prepare profile runtime first-runtime for release 0.28.0 in 200 yards: yard-000, yard-001, ") ||
		!strings.HasSuffix(steps[0].Consequence, " more: restart the first service") ||
		!strings.HasPrefix(steps[1].Consequence, "prepare profile runtime second-runtime for release 0.28.0 in 67 yards: yard-000, yard-003, ") ||
		steps[4].Consequence != "prepare profile runtime third-runtime for release 0.28.0 in yard yard-001" {
		t.Fatalf("steps = %#v", steps)
	}
	if got := yardList([]string{strings.Repeat("y", 300), "z"}, 200); got != "2 yards" {
		t.Fatalf("yard list = %q", got)
	}
	changed := slices.Clone(items)
	changed[398].yard = "yard-other"
	if slices.Equal(boundItems(items), boundItems(changed)) {
		t.Fatal("the binding omits a yard the step text leaves out")
	}
}

// A plan that cannot show every declared consequence is refused before any
// conversion, never shortened.
func TestRollbackPreparationRefusesAPlanItCannotShow(t *testing.T) {
	fixture := newPreparedRollbackFixture(t, preparedSample)
	fixture.formats["named"] = 2
	fixture.consequence["default"], fixture.consequence["named"] = "restart service 0", "restart service 1"
	for index := 2; index < domain.MaxOperationSteps; index++ {
		yard := fmt.Sprintf("y%03d", index)
		declareRollbackPrepYard(t, fixture.home, yard)
		fixture.formats[yard], fixture.consequence[yard] = 2, fmt.Sprintf("restart service %d", index)
	}
	_, err := fixture.prepare(t)
	outcome := outcomeOf(t, err)
	if outcome.Code != releasetransition.CodeRollbackIncompatible || outcome.Retry != keepActiveRetry ||
		outcome.Message != "rollback preparation needs 257 confirmation steps, more than the 256 a plan can show; nothing was converted" || len(fixture.applies) != 0 {
		t.Fatalf("refusal = %#v, %v, applies %v", outcome, err, fixture.applies)
	}
	// One step fewer is shown whole.
	delete(fixture.consequence, "y255")
	if err := os.RemoveAll(filepath.Join(fixture.home, "yards", "y255")); err != nil {
		t.Fatal(err)
	}
	delete(fixture.formats, "y255")
	if prepared, err := fixture.prepare(t); err != nil || len(prepared.Steps) != domain.MaxOperationSteps {
		t.Fatalf("plan = %d steps, %v", len(prepared.Steps), err)
	}
}

// An interrupted step 1 leaves the converted yards drifting; the rerun
// offers the remaining conversion, then the rollback.
func TestInterruptedRollbackPreparationResumes(t *testing.T) {
	fixture := newPreparedRollbackFixture(t, preparedSample)
	fixture.formats["named"] = 2
	apply, interrupted := fixture.runtime.config.ProfileRuntimeRollbackApply, false
	fixture.runtime.config.ProfileRuntimeRollbackApply = func(ctx context.Context, root string, loaded config.Loaded, id, operation, actual, desired, consequence string) error {
		if loaded.Context.YardName == "named" && !interrupted {
			interrupted = true
			return errors.New("guest exec failed")
		}
		return apply(ctx, root, loaded, id, operation, actual, desired, consequence)
	}
	prepared, err := fixture.prepare(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.Execute(context.Background()); err == nil || len(fixture.applies) != 1 {
		t.Fatalf("interruption = %v, applies %v", err, fixture.applies)
	}
	resumed, err := fixture.prepare(t)
	if err != nil || resumed.Action != PrepareRollbackAction || len(resumed.Steps) != 2 || resumed.Steps[0].Observed != "rollback-observe stale in yard named" {
		t.Fatalf("rerun = %s %#v, %v", resumed.Action, resumed.Steps, err)
	}
	if err := resumed.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rollback, err := fixture.prepare(t); err != nil || rollback.Action != "update.rollback" {
		t.Fatalf("after preparation = %s, %v", rollback.Action, err)
	}
	fixture.assertUnchanged(t)
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
	// A stopped yard and installed state the retained release cannot
	// activate are independent.
	fixture = newPreparedRollbackFixture(t, preparedRollbackOptions{active: preparedSample.active, seed: func(active, _ string) {
		writePrepProfile(t, active, "extra", `{"schema_version":1,"default_yards":["default"],"runtime":{"activation_id":"extra-runtime","handler":"runtime.sh"}}`)
	}})
	fixture.formats["named"] = 0
	_, err = fixture.prepare(t)
	if outcome := outcomeOf(t, err); !strings.Contains(outcome.Message, "yard named is stopped") ||
		!strings.Contains(outcome.Message, retainedBindingMessage) || strings.Count(err.Error(), retainedBindingMessage) != 1 ||
		!strings.HasSuffix(err.Error(), "; retained release cannot activate installed profile runtime extra-runtime") ||
		retryAction(t, outcome.Retry) != (domain.Action{Verb: domain.VerbStart, Yard: "named"}) {
		t.Fatalf("combined refusal = %#v, %v", outcome, err)
	}
	// The wire lists four format differences; the cause lists all.
	fixture = newPreparedRollbackFixture(t, preparedRollbackOptions{seed: func(active, target string) {
		for index := range 7 {
			for root, format := range map[string]int{active: 3, target: 2} {
				writePrepProfile(t, root, fmt.Sprintf("owner%d", index), fmt.Sprintf(`{"schema_version":1,"runtime":{"activation_id":"owner%d","handler":"runtime.sh","format":%d}}`, index, format))
			}
		}
	}})
	_, err = fixture.prepare(t)
	if outcome := outcomeOf(t, err); !strings.Contains(outcome.Message, "owner3 2 instead of 3, 3 more") || strings.Contains(outcome.Message, "owner6") ||
		!strings.Contains(err.Error(), "owner6 2 instead of 3") {
		t.Fatalf("format refusal = %#v, %v", outcome, err)
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
		"retained binding": func(t *testing.T, fixture *preparedRollbackFixture) {
			testkit.WriteFile(t, filepath.Join(fixture.home, "yards/named/config.env"), []byte("ENVIRONMENT_PROFILES=sample\n"), 0o600)
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
			// A finding keeps its own next step.
			if outcome := outcomeOf(t, err); name == "stopped yard" &&
				(retryAction(t, outcome.Retry) != (domain.Action{Verb: domain.VerbStart, Yard: "named"}) || !strings.Contains(outcome.Message, "yard named is stopped") ||
					strings.Count(err.Error(), "nothing was converted") != 1 || strings.Count(err.Error(), "yard named is stopped") != 1) {
				t.Fatalf("stopped yard after confirmation = %#v", outcome)
			}
			fixture.assertUnchanged(t)
		})
	}
}

// Installed state the retained release cannot activate, appearing after
// confirmation, converts nothing.
func TestRollbackPreparationRechecksTheRetainedBinding(t *testing.T) {
	fixture := newPreparedRollbackFixture(t, preparedRollbackOptions{active: preparedSample.active, seed: func(active, _ string) {
		writePrepProfile(t, active, "other", `{"schema_version":1,"managed_paths":[{"root":"data","path":"other/{yard}"}],"runtime":{"activation_id":"other-runtime","handler":"runtime.sh"}}`)
	}})
	prepared, err := fixture.prepare(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(filepath.Dir(fixture.home), "data/other/named"), 0o700); err != nil {
		t.Fatal(err)
	}
	err = prepared.Execute(context.Background())
	if outcome := outcomeOf(t, err); outcome.Code != releasetransition.CodePlanStale || !strings.HasSuffix(outcome.Message, "nothing was converted; "+retainedBindingMessage) ||
		!strings.Contains(err.Error(), "other-runtime") || len(fixture.applies) != 0 {
		t.Fatalf("changed installed state = %#v, %v, applies %v", outcome, err, fixture.applies)
	}
	fixture.assertUnchanged(t)
}

// A VM configuration the retained release cannot read, appearing after
// confirmation, converts nothing.
func TestRollbackPreparationRechecksVMCompatibility(t *testing.T) {
	fixture := newPreparedRollbackFixture(t, preparedSample)
	writeVMRollbackSettings(t, fixture.home, "named", "1")
	headless := false
	fixture.runtime.config.YardInstance = func(context.Context, config.Loaded) (ports.InstanceInfo, error) {
		configuration := map[string]string{}
		if headless {
			configuration["raw.qemu.conf"] = domain.VMRawQEMUConfig(true)
		}
		return ports.InstanceInfo{Type: domain.YardVM, LocalConfig: configuration}, nil
	}
	prepared, err := fixture.prepare(t)
	if err != nil || prepared.Action != PrepareRollbackAction {
		t.Fatal(prepared.Action, err)
	}
	headless = true
	err = prepared.Execute(context.Background())
	if outcome := outcomeOf(t, err); outcome.Code != releasetransition.CodePlanStale || !strings.Contains(err.Error(), "free page reporting") || len(fixture.applies) != 0 {
		t.Fatalf("changed VM configuration = %#v, %v, applies %v", outcome, err, fixture.applies)
	}
	fixture.assertUnchanged(t)
}

// Settings a sealed target cannot read refuse before any conversion.
func TestRollbackPreparationReadsTargetSettingsFirst(t *testing.T) {
	fixture := newPreparedRollbackFixture(t, preparedRollbackOptions{active: preparedSample.active, seed: func(_, target string) {
		directory := filepath.Join(target, "config/agents/other")
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(directory, "integration.json"), []byte(`{"schema_version":1,"legacy_metadata":true}`), 0o600)
	}})
	fixture.formats["named"] = 2
	_, err := fixture.prepare(t)
	outcome := outcomeOf(t, err)
	for _, yard := range []string{"default", "named"} {
		if !strings.Contains(outcome.Message, "yard "+yard+": release "+fixture.targetID+" cannot read the yard settings") {
			t.Fatalf("refusal = %#v", outcome)
		}
	}
	if outcome.Retry != keepActiveRetry || !strings.Contains(err.Error(), "legacy_metadata") || len(fixture.applies) != 0 {
		t.Fatalf("refusal = %#v, %v, applies %v", outcome, err, fixture.applies)
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
		// A hook that reports success without converting fails the gate.
		"unconverted owner": {func(fixture *preparedRollbackFixture) {
			fixture.runtime.config.ProfileRuntimeRollbackApply = func(context.Context, string, config.Loaded, string, string, string, string, string) error { return nil }
		}, nil, rollbackRetry},
		// The gate reads the history afresh.
		"changed history": {func(fixture *preparedRollbackFixture) {
			apply := fixture.runtime.config.ProfileRuntimeRollbackApply
			fixture.runtime.config.ProfileRuntimeRollbackApply = func(ctx context.Context, root string, loaded config.Loaded, id, operation, actual, desired, consequence string) error {
				err := apply(ctx, root, loaded, id, operation, actual, desired, consequence)
				return errors.Join(err, os.WriteFile(filepath.Join(fixture.home, "release-transition/v2/ledger.json"), []byte(`{"schemaVersion":2,"domains":{"settings":{"epoch":2,"applied":[]}}}`), 0o600))
			}
		}, nil, rollbackRetry},
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

// A forward update ignores runtime formats: from the active 0.31.0 release,
// 0.32.0 is admitted with another declared format, and with no formats but a
// profile field only it can read, whether staged fresh or already retained.
func TestForwardUpdateIgnoresRuntimeFormats(t *testing.T) {
	for _, staged := range []string{"fresh", "retained"} {
		for name, test := range map[string]struct{ target, extra string }{
			"another format":        {target: `,"format":2`},
			"no formats, new field": {extra: `{"schema_version":1,"legacy_field":true}`},
		} {
			t.Run(staged+" "+name, func(t *testing.T) {
				const version = "0.32.0"
				fixture := newPreparedRollbackFixture(t, preparedRollbackOptions{target: test.target})
				if err := os.Remove(filepath.Join(fixture.home, "release-transition/v2/journal.json")); err != nil {
					t.Fatal(err)
				}
				root := filepath.Dir(fixture.runtimeRoot)
				cache := filepath.Join(root, "cache")
				bundle := filepath.Join(cache, version, fmt.Sprintf("subyard-%s-linux-%s.tar.gz", version, goruntime.GOARCH))
				if err := os.MkdirAll(filepath.Dir(bundle), 0o700); err != nil {
					t.Fatal(err)
				}
				for _, suffix := range []string{"", ".sha256", ".manifest.json", ".provenance.json"} {
					testkit.WriteFile(t, bundle+suffix, []byte("test bundle"), 0o600)
				}
				release, _, err := releaseBundleIdentity(version, bundle)
				if err != nil {
					t.Fatal(err)
				}
				// The retained target becomes the newer release.
				newer := filepath.Join(fixture.runtimeRoot, release)
				if err := os.Rename(filepath.Join(fixture.runtimeRoot, "releases", fixture.targetID), newer); err != nil {
					t.Fatal(err)
				}
				engine := filepath.Join(newer, "bin/yard-engine")
				payload := strings.NewReplacer(fixture.targetID, filepath.Base(release), "yard-engine "+UpdateAnchorVersion, "yard-engine "+version,
					`"activate-target"`, `"unused-mode"`).Replace(string(mustReadFile(t, engine)))
				testkit.WriteFile(t, engine, []byte(payload), 0o700)
				if test.extra != "" {
					writePrepProfile(t, newer, "other", test.extra)
				}
				rewriteReleaseOwnerManifest(t, publishedCandidate{root: newer})
				previous := filepath.Join(fixture.runtimeRoot, "previous")
				if err := os.Remove(previous); err != nil {
					t.Fatal(err)
				}
				source := newer
				if staged == "fresh" {
					source = filepath.Join(root, "install-source")
					if err := os.Rename(newer, source); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Symlink(release, previous); err != nil {
					t.Fatal(err)
				}
				fixture.runtime.config.Installer = filepath.Join(root, "installer")
				testkit.WriteFile(t, fixture.runtime.config.Installer, []byte(fmt.Sprintf("#!/bin/sh\n# --publish-only\nset -eu\nmkdir -p \"$2/releases\"\n[ -e \"$2/%s\" ] || cp -a %q \"$2/%s\"\nprintf '%%s\\n' %q\n", release, source, release, release)), 0o700)
				fixture.runtime.config.Inputs.values["YARD_RELEASE_CACHE"] = cache
				prepared, err := fixture.runtime.PrepareTransition(context.Background(), []string{"--offline", "--version", version, "--runtime-root", fixture.runtimeRoot}, fixture.home, "default")
				if err != nil || prepared.TargetVersion != version || len(fixture.observed) != 0 {
					t.Fatalf("forward update = %s, %v, observed %v", prepared.Action, err, fixture.observed)
				}
			})
		}
	}
}

type ownerDiagnostic struct{ message, retry string }

func (diagnostic ownerDiagnostic) Error() string { return "handler exited 3" }
func (diagnostic ownerDiagnostic) ActivationDiagnostic() (string, string) {
	return diagnostic.message, diagnostic.retry
}

// An owner's own public refusal keeps its message and typed next step.
func TestRollbackPreparationKeepsAnOwnersNextStep(t *testing.T) {
	fixture := newPreparedRollbackFixture(t, preparedSample)
	refusal := ownerDiagnostic{"sample export is enabled", "run yard -Y named profile disable sample"}
	fixture.formats["named"] = 2
	observe := fixture.runtime.config.ProfileRuntimeRollbackObservation
	fixture.runtime.config.ProfileRuntimeRollbackObservation = func(ctx context.Context, root string, loaded config.Loaded, id string) (ports.RuntimeObservation, error) {
		if loaded.Context.YardName == "named" {
			return ports.RuntimeObservation{}, refusal
		}
		return observe(ctx, root, loaded, id)
	}
	_, err := fixture.prepare(t)
	outcome := outcomeOf(t, err)
	if !strings.Contains(outcome.Message, "yard named: profile runtime sample-runtime cannot be observed for rollback preparation: sample export is enabled") ||
		outcome.Retry != refusal.retry || !errors.Is(err, refusal) {
		t.Fatalf("owner refusal = %#v, %v", outcome, err)
	}
	// A failed conversion keeps it too.
	fixture.runtime.config.ProfileRuntimeRollbackObservation = observe
	prepared, err := fixture.prepare(t)
	if err != nil {
		t.Fatal(err)
	}
	fixture.applyErr = refusal
	if outcome := outcomeOf(t, prepared.Execute(context.Background())); outcome.Retry != refusal.retry {
		t.Fatalf("failed conversion = %#v", outcome)
	}
	// So does an owner refusal of the recheck before any conversion.
	fixture.applyErr = nil
	if prepared, err = fixture.prepare(t); err != nil {
		t.Fatal(err)
	}
	fixture.runtime.config.ProfileRuntimeRollbackObservation = func(context.Context, string, config.Loaded, string) (ports.RuntimeObservation, error) {
		return ports.RuntimeObservation{}, refusal
	}
	if outcome := outcomeOf(t, prepared.Execute(context.Background())); outcome.Retry != refusal.retry ||
		!strings.Contains(outcome.Message, "nothing was converted") || !strings.Contains(outcome.Message, "sample export is enabled") {
		t.Fatalf("recheck refusal = %#v", outcome)
	}
}

// Planning, the recheck before the first conversion and the gate are one
// admission: an owner refusal at any of them keeps its message, next step
// and cause.
func TestRollbackPreparationAdmissionsKeepOwnerRefusalsAlike(t *testing.T) {
	refusal := ownerDiagnostic{"sample export is enabled", "run yard -Y default profile disable sample"}
	for _, test := range []struct {
		name      string
		refuseAt  int
		converted int
		message   string
	}{
		{"planning", 1, 0, "yard default: profile runtime sample-runtime cannot be observed"},
		{"recheck", 2, 0, "nothing was converted"},
		{"gate", 3, 1, "may be partly prepared"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPreparedRollbackFixture(t, preparedSample)
			observe, seen := fixture.runtime.config.ProfileRuntimeRollbackObservation, 0
			fixture.runtime.config.ProfileRuntimeRollbackObservation = func(ctx context.Context, root string, loaded config.Loaded, id string) (ports.RuntimeObservation, error) {
				if loaded.Context.YardName == "default" {
					if seen++; seen == test.refuseAt {
						return ports.RuntimeObservation{}, refusal
					}
				}
				return observe(ctx, root, loaded, id)
			}
			prepared, err := fixture.prepare(t)
			if test.refuseAt != 1 {
				if err != nil {
					t.Fatal(err)
				}
				err = prepared.Execute(context.Background())
			}
			outcome := outcomeOf(t, err)
			if outcome.Retry != refusal.retry || !strings.Contains(outcome.Message, test.message) || !errors.Is(err, refusal) ||
				!strings.Contains(err.Error(), refusal.message) || len(fixture.applies) != test.converted {
				t.Fatalf("refusal = %#v, %v, applies %v", outcome, err, fixture.applies)
			}
		})
	}
}

// A yard added or removed after confirmation makes the plan stale before
// any conversion.
func TestRollbackPreparationRefusesAChangedYardSet(t *testing.T) {
	for name, change := range map[string]func(*preparedRollbackFixture){
		"added": func(fixture *preparedRollbackFixture) {
			declareRollbackPrepYard(t, fixture.home, "extra")
			fixture.formats["extra"] = 2
		},
		"removed": func(fixture *preparedRollbackFixture) {
			if err := os.RemoveAll(filepath.Join(fixture.home, "yards", "named")); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newPreparedRollbackFixture(t, preparedSample)
			fixture.formats["named"] = 2
			prepared, err := fixture.prepare(t)
			if err != nil {
				t.Fatal(err)
			}
			change(fixture)
			err = prepared.Execute(context.Background())
			if outcome := outcomeOf(t, err); !errors.Is(err, domain.ErrPlanStale) || !strings.Contains(outcome.Message, "nothing was converted") ||
				outcome.Retry != rollbackRetry || len(fixture.applies) != 0 {
				t.Fatalf("changed yard set = %#v, %v, applies %v", outcome, err, fixture.applies)
			}
		})
	}
}

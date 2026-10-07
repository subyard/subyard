package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/configmaterial"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

// Model the guest observation and config application boundary while retaining
// native desired-asset loading, target selection, and release journal handling.
type sourceScopeConfigBoundary struct {
	converged map[string]bool
	applied   []string
}

func (boundary *sourceScopeConfigBoundary) Exec(
	_ context.Context, project, instance string, request ports.InstanceExecRequest,
) (ports.InstanceExecResult, error) {
	digest, err := configmaterial.DesiredDigest(request.Stdin)
	if err != nil {
		return ports.InstanceExecResult{}, err
	}
	payload, err := json.Marshal(configmaterial.Observation{
		Converged: boundary.converged[project+"/"+instance], Fingerprint: digest,
	})
	return ports.InstanceExecResult{Stdout: payload}, err
}

func (boundary *sourceScopeConfigBoundary) ApplyConfig(_ context.Context, yard string) error {
	boundary.applied = append(boundary.applied, yard)
	key := "subyard/yard"
	if yard != "default" {
		key = "subyard-" + yard + "/yard-" + yard
	}
	boundary.converged[key] = true
	return nil
}

type sourceScopeMaterialized struct {
	*materializedConfigActivationReconciler
	interrupt bool
}

func (reconciler *sourceScopeMaterialized) Reconcile(ctx context.Context, links releasetransition.ReleaseLinks) error {
	if reconciler.interrupt {
		return errors.New("injected interruption before config application")
	}
	return reconciler.materializedConfigActivationReconciler.Reconcile(ctx, links)
}

// An interface embed exposes the original activation contract without the new
// optional scope hook, reproducing a journal authorized by an older engine.
type sourceScopeHistoricalReconciler struct {
	releasetransition.V2ActivationReconciler
}

type sourceScopeFixture struct {
	t          *testing.T
	root       string
	configHome string
	namedAsset string
	program    *CLI
	request    releasetransition.ProcessRequest
	boundary   *sourceScopeConfigBoundary
	store      *releasetransition.POSIXV2Store
}

func newSourceScopeFixture(t *testing.T) *sourceScopeFixture {
	t.Helper()
	root, environment, _ := nativeFixture(t)
	configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	// Fresh migration operations load shipped defaults instead of inherited
	// native-fixture settings; keep the synthetic paths in that supported layer.
	writeCLIFile(t, filepath.Join(root, "config", "subyard.env"), strings.Join(environment, "\n")+"\n", 0o600)
	registry, err := os.ReadFile(filepath.Join(repositoryRoot(t), "config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeReleaseTransitionTestFile(t, filepath.Join(root, "config", "release-transition.json"), registry, 0o600)
	hostAsset := filepath.Join(configHome, "overrides", "host", "agents", "claude", "settings.json")
	namedAsset := filepath.Join(configHome, "yards", "named", "overrides", "agents", "claude", "settings.json")
	writeConfigCommandFile(t, hostAsset, `{"env":{"SCOPE":"default-v1"}}`)
	writeConfigCommandFile(t, namedAsset, `{"env":{"SCOPE":"named-v1"}}`)
	writeConfigCommandFile(t, filepath.Join(configHome, "config.env"),
		"CODING_TOOL_INTEGRATIONS=claude\nAGENT_claude_CONFIG='"+hostAsset+"'\nAGENT_claude_CONFIG_DEST=.claude/settings.json\n")
	writeConfigCommandFile(t, filepath.Join(configHome, "yards", "named", "config.env"),
		"SSH_PORT=2233\nAGENT_claude_CONFIG='"+namedAsset+"'\n")
	runtimeRoot := testkit.TempDir(t)
	for _, release := range []string{"release-a", "release-b"} {
		if err := os.MkdirAll(filepath.Join(runtimeRoot, "releases", release), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("releases/release-a", filepath.Join(runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
		"subyard/yard":             {Name: "yard", Project: "subyard", Status: "Running"},
		"subyard-named/yard-named": {Name: "yard-named", Project: "subyard-named", Status: "Running"},
	}}
	boundary := &sourceScopeConfigBoundary{converged: map[string]bool{}}
	program, err := New(Options{RepositoryRoot: root, Environment: environment,
		Incus: incus, Executor: boundary, Config: boundary, InitPlatform: newInitPlatformFixture(),
		Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	store, err := releasetransition.NewPOSIXV2Store(configHome)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := store.ReadLedger()
	if err != nil || ledger.Exists {
		t.Fatalf("fresh fixture already has ledger: %#v %v", ledger, err)
	}
	return &sourceScopeFixture{t: t, root: root, configHome: configHome, namedAsset: namedAsset,
		program: program, boundary: boundary, store: store,
		request: releasetransition.ProcessRequest{SchemaVersion: releasetransition.ProcessProtocolSchemaV1,
			RuntimeRoot: runtimeRoot, ConfigHome: configHome, Target: "release-b",
			Direction:           releasetransition.DirectionActivateTarget,
			ArtifactDigest:      releasetransition.Fingerprint(strings.Repeat("a", 64)),
			InheritedSettingIDs: program.releaseTransitionInheritedSettingIDs()},
	}
}

func (fixture *sourceScopeFixture) reconciler(interrupt bool) *sourceScopeMaterialized {
	return &sourceScopeMaterialized{materializedConfigActivationReconciler: &materializedConfigActivationReconciler{
		cli: fixture.program, configHome: fixture.configHome, yard: "default",
		goal:           releasetransition.Goal{Target: fixture.request.Target, Direction: fixture.request.Direction},
		artifactDigest: fixture.request.ArtifactDigest,
	}, interrupt: interrupt}
}

func (fixture *sourceScopeFixture) execute(mode releasetransition.ProcessMode, plan releasetransition.PlanToken, reconciler releasetransition.V2ActivationReconciler) releasetransition.ProcessResponse {
	fixture.t.Helper()
	request := fixture.request
	request.Mode = mode
	if mode == releasetransition.ProcessConverge {
		request.Execution = &releasetransition.Execution{Plan: plan, Authorization: "confirmed"}
	}
	response, err := executeReleaseTransitionRequest(context.Background(), fixture.root, request,
		func(releasetransition.PlanToken, releasetransition.Authorization) bool { return true },
		[]releasetransition.V2ActivationReconciler{reconciler}, releaseTransitionOwnerFixture{}, nil)
	if err != nil {
		fixture.t.Fatal(err)
	}
	return response
}

func (fixture *sourceScopeFixture) journal() releasetransition.JournalRecord {
	fixture.t.Helper()
	snapshot, err := fixture.store.ReadCurrentJournal()
	if err != nil {
		fixture.t.Fatal(err)
	}
	journal, err := releasetransition.ParseJournal(snapshot.Payload)
	if err != nil {
		fixture.t.Fatal(err)
	}
	return journal
}

func TestLedgerOnlyForwardActivationIncludesNamedConfig(t *testing.T) {
	for _, interrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "resume"}[interrupt], func(t *testing.T) {
			fixture := newSourceScopeFixture(t)
			initial := fixture.execute(releasetransition.ProcessInspect, "", fixture.reconciler(false))
			if initial.Inspection == nil || !initial.Inspection.Assessment.Changed {
				t.Fatalf("initial inspection: %#v", initial)
			}
			// A named asset must be bound by the original forward plan, before
			// any ledger step or release activation has occurred.
			writeConfigCommandFile(t, fixture.namedAsset, `{"env":{"SCOPE":"named-v2"}}`)
			changed := fixture.execute(releasetransition.ProcessInspect, "", fixture.reconciler(false))
			if changed.Inspection == nil || changed.Inspection.Plan == initial.Inspection.Plan {
				t.Fatal("original forward plan did not bind named config")
			}
			writeConfigCommandFile(t, fixture.namedAsset, `{"env":{"SCOPE":"named-v1"}}`)
			result := fixture.execute(releasetransition.ProcessConverge, initial.Inspection.Plan, fixture.reconciler(interrupt))
			journal := fixture.journal()
			if len(journal.Steps) != 2 {
				t.Fatalf("expected only two ledger steps: %#v", journal.Steps)
			}
			for _, step := range journal.Steps {
				if !strings.HasPrefix(step.Resource, "ledger.") {
					t.Fatalf("unexpected source mutation: %#v", step)
				}
			}
			if interrupt {
				if result.Outcome == nil || result.Outcome.Status != releasetransition.StatusRecovering || len(fixture.boundary.applied) != 0 {
					t.Fatalf("interruption: %#v applied=%v", result, fixture.boundary.applied)
				}
				resume := fixture.execute(releasetransition.ProcessInspect, "", fixture.reconciler(false))
				if resume.Inspection == nil || resume.Inspection.Resume == nil || *resume.Inspection.Resume != journal.Transaction || resume.Inspection.Plan != journal.ResumePlan {
					t.Fatalf("fresh-process resume lost authorization: %#v journal=%#v", resume, journal)
				}
				result = fixture.execute(releasetransition.ProcessConverge, resume.Inspection.Plan, fixture.reconciler(false))
			}
			if result.Outcome == nil || result.Outcome.Status != releasetransition.StatusReady || !reflect.DeepEqual(fixture.boundary.applied, []string{"default", "named"}) {
				t.Fatalf("forward convergence: %#v applied=%v", result, fixture.boundary.applied)
			}
			completed := fixture.journal()
			if completed.Transaction != journal.Transaction || completed.AuthorizationPlan != initial.Inspection.Plan || completed.ObservationScope != journal.ObservationScope {
				t.Fatal("completion replaced authorized transaction or scope")
			}
			ready := fixture.execute(releasetransition.ProcessInspect, "", fixture.reconciler(false))
			if ready.Inspection == nil || ready.Inspection.Outcome == nil || ready.Inspection.Outcome.Status != releasetransition.StatusReady {
				t.Fatalf("completed forward transition needs another activation: %#v", ready)
			}
		})
	}
}

func TestLedgerOnlyHistoricalResumePreservesSelectedConfigScope(t *testing.T) {
	fixture := newSourceScopeFixture(t)
	historical := func(interrupt bool) releasetransition.V2ActivationReconciler {
		return sourceScopeHistoricalReconciler{fixture.reconciler(interrupt)}
	}
	initial := fixture.execute(releasetransition.ProcessInspect, "", historical(false))
	if initial.Inspection == nil {
		t.Fatalf("historical inspection: %#v", initial)
	}
	interrupted := fixture.execute(releasetransition.ProcessConverge, initial.Inspection.Plan, historical(true))
	if interrupted.Outcome == nil || interrupted.Outcome.Status != releasetransition.StatusRecovering {
		t.Fatalf("historical interruption: %#v", interrupted)
	}
	before := fixture.journal()
	resume := fixture.execute(releasetransition.ProcessInspect, "", fixture.reconciler(false))
	if resume.Inspection == nil || resume.Inspection.Resume == nil || resume.Inspection.Plan != before.ResumePlan {
		t.Fatalf("historical scope could not resume: %#v", resume)
	}
	result := fixture.execute(releasetransition.ProcessConverge, resume.Inspection.Plan, fixture.reconciler(false))
	if result.Outcome == nil || result.Outcome.Status != releasetransition.StatusReady || !reflect.DeepEqual(fixture.boundary.applied, []string{"default"}) || fixture.boundary.converged["subyard-named/yard-named"] {
		t.Fatalf("historical grant expanded: %#v applied=%v", result, fixture.boundary.applied)
	}
	after := fixture.journal()
	if after.Transaction != before.Transaction || after.AuthorizationPlan != before.AuthorizationPlan || after.ObservationScope != before.ObservationScope {
		t.Fatal("historical authorization was replaced")
	}
}

func TestLedgerOnlyWideResumeRejectsChangedNamedConfig(t *testing.T) {
	fixture := newSourceScopeFixture(t)
	initial := fixture.execute(releasetransition.ProcessInspect, "", fixture.reconciler(false))
	if initial.Inspection == nil {
		t.Fatalf("inspection: %#v", initial)
	}
	interrupted := fixture.execute(releasetransition.ProcessConverge, initial.Inspection.Plan, fixture.reconciler(true))
	if interrupted.Outcome == nil || interrupted.Outcome.Status != releasetransition.StatusRecovering {
		t.Fatalf("interruption: %#v", interrupted)
	}
	before, err := fixture.store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	writeConfigCommandFile(t, fixture.namedAsset, `{"env":{"SCOPE":"named-v2"}}`)
	stale := fixture.execute(releasetransition.ProcessInspect, "", fixture.reconciler(false))
	if stale.Inspection == nil || stale.Inspection.Outcome == nil || stale.Inspection.Outcome.Code != releasetransition.CodePlanStale {
		t.Fatalf("changed named config was admitted: %#v", stale)
	}
	after, err := fixture.store.ReadCurrentJournal()
	if err != nil || !reflect.DeepEqual(before.Payload, after.Payload) || len(fixture.boundary.applied) != 0 {
		t.Fatal("stale scope inspection changed authorized state")
	}
}

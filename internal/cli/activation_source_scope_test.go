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
	"github.com/Subyard/Subyard/internal/config"
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

func TestMaterializedActivationPlanExplainsEachYardsInheritedAndEmptySelection(t *testing.T) {
	fixture := newSourceScopeFixture(t)
	writeConfigCommandFile(t, filepath.Join(fixture.configHome, "config.env"),
		"AGENT_claude_CONFIG='"+filepath.Join(fixture.configHome, "overrides/host/agents/claude/settings.json")+"'\nAGENT_claude_CONFIG_DEST=.claude/settings.json\n")
	writeConfigCommandFile(t, filepath.Join(fixture.configHome, ".sync/settings/config.env"), "CODING_TOOL_INTEGRATIONS=claude\n")
	writeConfigCommandFile(t, filepath.Join(fixture.configHome, "yards/named/config.env"), "SSH_PORT=2233\nCODING_TOOL_INTEGRATIONS=''\n")
	reconciler := fixture.reconciler(false)
	reconciler.allLocal, reconciler.scopeResolved = true, true
	observation, err := reconciler.Observe(context.Background(), releasetransition.ReleasePair{From: "release-a", Target: "release-b"}, releasetransition.ReleaseLinks{Active: "release-a"})
	if err != nil {
		t.Fatal(err)
	}
	consequences := strings.Join(observation.Consequences, "\n")
	inspection := fixture.execute(releasetransition.ProcessInspect, "", reconciler).Inspection
	if inspection == nil || !inspection.Assessment.Changed {
		t.Fatalf("materialized activation lost its assessment: %#v", inspection)
	}
	for _, explanation := range []string{
		"yard default: requested coding integrations [claude], effective [claude], from Git settings (host scope",
		"yard named: requested coding integrations [], effective [], from local overrides (yard scope",
	} {
		if !strings.Contains(consequences, explanation) {
			t.Fatalf("missing per-yard source explanation %q: %s", explanation, consequences)
		}
		if !strings.Contains(strings.Join(inspection.Assessment.Consequences, "\n"), explanation) {
			t.Fatalf("materialized actor explanation lost from owner assessment: %q", explanation)
		}
	}
	if len(fixture.boundary.applied) != 0 {
		t.Fatal("inspection applied materialized configuration")
	}
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
			ledgerMigrations := map[string]string{
				"canonicalize-test-vms-settings-v2":    "settings",
				"canonicalize-test-yard-owner-v2":      "owner-registration",
				"introduce-owner-publication-state-v1": "power-metadata",
				"localize-ssh-port-settings-v3":        "settings",
			}
			if len(journal.Steps) != len(ledgerMigrations) {
				t.Fatalf("expected only shipped ledger steps: %#v", journal.Steps)
			}
			seen := map[string]bool{}
			for _, step := range journal.Steps {
				domain, known := ledgerMigrations[step.Migration]
				if !known || seen[step.Migration] || step.ID != step.Migration+".ledger" || step.Resource != "ledger."+domain {
					t.Fatalf("unexpected source mutation: %#v", step)
				}
				seen[step.Migration] = true
			}
			ledgerSnapshot, err := fixture.store.ReadLedger()
			if err != nil {
				t.Fatal(err)
			}
			var ledger releasetransition.LedgerV2
			if err := json.Unmarshal(ledgerSnapshot.Payload, &ledger); err != nil {
				t.Fatal(err)
			}
			for domain, expected := range map[string]releasetransition.DomainLedgerV2{
				"settings":           {Epoch: 3, Applied: []string{"canonicalize-test-vms-settings-v2", "localize-ssh-port-settings-v3"}},
				"owner-registration": {Epoch: 2, Applied: []string{"canonicalize-test-yard-owner-v2"}},
				"power-metadata":     {Epoch: 2, Applied: []string{"introduce-owner-publication-state-v1"}},
			} {
				state := ledger.Domains[domain]
				if !reflect.DeepEqual(state, expected) {
					t.Fatalf("ledger domain %s = %#v", domain, state)
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

func newSSHPinSourceScopeFixture(t *testing.T) *sourceScopeFixture {
	t.Helper()
	f := newSourceScopeFixture(t)
	writeConfigCommandFile(t, filepath.Join(f.configHome, "yards/named/config.env"),
		"AGENT_claude_CONFIG='"+f.namedAsset+"'\n")
	writeConfigCommandFile(t, filepath.Join(f.configHome, config.GitSettingsRelativePath, "config.env"), "SSH_PORT=2240\n")
	writeConfigCommandFile(t, filepath.Join(f.configHome, config.GitSettingsRelativePath, "yards/named/config.env"), "SSH_PORT=2241\n")
	payload := mustReadConfigFixture(t, filepath.Join(f.root, "config/release-transition.json"))
	registry, _, err := releasetransition.ParseRegistryV2(payload, releasetransition.BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	ledger := releasetransition.BaselineLedgerV2(registry)
	for _, migration := range registry.Migrations {
		if migration.Kind == "local-ssh-port-settings-v2-to-v3" {
			break
		}
		ledger, err = ledger.Advance(registry, migration)
		if err != nil {
			t.Fatal(err)
		}
	}
	encoded, _, err := releasetransition.MarshalLedgerV2(ledger, registry)
	if err != nil {
		t.Fatal(err)
	}
	writeReleaseTransitionTestFile(t, filepath.Join(f.configHome, "release-transition/v2/ledger.json"), encoded, 0o600)
	return f
}

func assertNativeSSHPins(t *testing.T, f *sourceScopeFixture) {
	t.Helper()
	for yard, port := range map[string]string{"default": "2240", "named": "2241"} {
		path := filepath.Join(f.configHome, "yards", yard, "config.env")
		values, err := config.ReadAssignments(path)
		info, statErr := os.Stat(path)
		if err != nil || statErr != nil || values["SSH_PORT"] != port || info.Mode().Perm() != 0o600 {
			t.Fatalf("actual native SSH pin CAS missing for yard %s: err=%v stat=%v", yard, err, statErr)
		}
	}
	journal := f.journal()
	if len(journal.Steps) != 3 {
		t.Fatalf("expected two actual pin CAS steps and one ledger CAS: steps=%d", len(journal.Steps))
	}
	for _, step := range journal.Steps {
		if step.Checkpoint != releasetransition.StepVerified {
			t.Fatalf("native migration step not verified: resource=%s checkpoint=%s", step.Resource, step.Checkpoint)
		}
	}
}

func TestSSHPinForwardActivationBindsBothYardsAndFinalInspectionReady(t *testing.T) {
	f := newSSHPinSourceScopeFixture(t)
	initial := f.execute(releasetransition.ProcessInspect, "", f.reconciler(false))
	if initial.Inspection == nil || len(initial.Inspection.Blockers) != 0 {
		t.Fatalf("initial inspection unavailable: %#v", initial)
	}
	t.Logf("initial activation decisions=%v", initial.Inspection.Decisions)
	writeConfigCommandFile(t, f.namedAsset, `{"env":{"SCOPE":"named-v2"}}`)
	changed := f.execute(releasetransition.ProcessInspect, "", f.reconciler(false))
	if changed.Inspection == nil {
		t.Fatal("changed named inspection unavailable")
	}
	if changed.Inspection.Plan == initial.Inspection.Plan {
		t.Error("original SSH-pin forward grant did not bind named desired asset")
	}
	writeConfigCommandFile(t, f.namedAsset, `{"env":{"SCOPE":"named-v1"}}`)
	result := f.execute(releasetransition.ProcessConverge, initial.Inspection.Plan, f.reconciler(false))
	if result.Outcome == nil || result.Outcome.Status != releasetransition.StatusReady {
		t.Fatalf("original authorized convergence failed: %#v", result)
	}
	assertNativeSSHPins(t, f)
	if !reflect.DeepEqual(f.boundary.applied, []string{"default", "named"}) {
		t.Errorf("original SSH-pin grant did not apply both materialized targets: applied=%v", f.boundary.applied)
	}
	final := f.execute(releasetransition.ProcessInspect, "", f.reconciler(false))
	if final.Inspection == nil || final.Inspection.Outcome == nil {
		t.Fatal("fresh final inspection missing")
	}
	t.Logf("fresh final status=%s code=%s message=%s activation decisions=%v", final.Inspection.Outcome.Status, final.Inspection.Outcome.Code, final.Inspection.Outcome.Message, final.Inspection.Decisions)
	if final.Inspection.Outcome.Status != releasetransition.StatusReady {
		t.Errorf("completed SSH-pin forward activation needs fresh activation consent: status=%s decisions=%v", final.Inspection.Outcome.Status, final.Inspection.Decisions)
	}
}

func TestSSHPinWideResumeRetainsBoundNamedInputs(t *testing.T) {
	for _, drift := range []bool{false, true} {
		t.Run(map[bool]string{false: "resume", true: "named-drift"}[drift], func(t *testing.T) {
			f := newSSHPinSourceScopeFixture(t)
			initial := f.execute(releasetransition.ProcessInspect, "", f.reconciler(false))
			if initial.Inspection == nil || len(initial.Inspection.Blockers) != 0 {
				t.Fatal("initial SSH pin plan unavailable")
			}
			interrupted := f.execute(releasetransition.ProcessConverge, initial.Inspection.Plan, f.reconciler(true))
			if interrupted.Outcome == nil || interrupted.Outcome.Status != releasetransition.StatusRecovering || len(f.boundary.applied) != 0 {
				t.Fatal("SSH pin interruption did not precede materialized application")
			}
			original := f.journal()
			assertNativeSSHPins(t, f)
			if drift {
				writeConfigCommandFile(t, f.namedAsset, `{"env":{"SCOPE":"named-drift"}}`)
			}
			resume := f.execute(releasetransition.ProcessInspect, "", f.reconciler(false))
			if resume.Inspection == nil {
				t.Fatal("SSH pin resume inspection missing")
			}
			if drift {
				if len(resume.Inspection.Blockers) == 0 || len(f.boundary.applied) != 0 {
					t.Fatal("named desired drift expanded the original SSH pin grant")
				}
			} else {
				if resume.Inspection.Resume == nil || resume.Inspection.Plan != original.ResumePlan {
					t.Fatal("SSH pin wide resume lost its original grant")
				}
				result := f.execute(releasetransition.ProcessConverge, resume.Inspection.Plan, f.reconciler(false))
				if result.Outcome == nil || result.Outcome.Status != releasetransition.StatusReady || !reflect.DeepEqual(f.boundary.applied, []string{"default", "named"}) {
					t.Fatalf("SSH pin wide resume failed: applied=%v result=%#v", f.boundary.applied, result.Outcome)
				}
			}
			after := f.journal()
			if after.Transaction != original.Transaction || after.AuthorizationPlan != original.AuthorizationPlan || after.ObservationScope != original.ObservationScope {
				t.Fatal("SSH pin resume rewrote sealed history or observation scope")
			}
		})
	}
}

func TestSSHPinHistoricalNarrowGrantRetainsScope(t *testing.T) {
	f := newSSHPinSourceScopeFixture(t)
	historical := func(interrupt bool) releasetransition.V2ActivationReconciler {
		return sourceScopeHistoricalReconciler{f.reconciler(interrupt)}
	}
	initial := f.execute(releasetransition.ProcessInspect, "", historical(false))
	if initial.Inspection == nil {
		t.Fatal("historical initial inspection missing")
	}
	interrupted := f.execute(releasetransition.ProcessConverge, initial.Inspection.Plan, historical(true))
	if interrupted.Outcome == nil || interrupted.Outcome.Status != releasetransition.StatusRecovering {
		t.Fatalf("historical interruption missing: %#v", interrupted)
	}
	original := f.journal()
	resume := f.execute(releasetransition.ProcessInspect, "", f.reconciler(false))
	if resume.Inspection == nil || resume.Inspection.Resume == nil || resume.Inspection.Plan != original.ResumePlan {
		t.Fatal("historical resume grant changed")
	}
	result := f.execute(releasetransition.ProcessConverge, resume.Inspection.Plan, f.reconciler(false))
	assertNativeSSHPins(t, f)
	if result.Outcome == nil || result.Outcome.Status != releasetransition.StatusReady || !reflect.DeepEqual(f.boundary.applied, []string{"default"}) || f.boundary.converged["subyard-named/yard-named"] {
		t.Fatalf("historical narrow consent expanded: applied=%v outcome=%#v", f.boundary.applied, result.Outcome)
	}
	completed := f.journal()
	if completed.Transaction != original.Transaction || completed.AuthorizationPlan != original.AuthorizationPlan || completed.ObservationScope != original.ObservationScope {
		t.Fatal("historical sealed authorization rewritten")
	}
	t.Logf("historical narrow grant retained: applied=%v steps=%d scope=%s", f.boundary.applied, len(completed.Steps), strings.TrimSpace(string(completed.ObservationScope)))
}

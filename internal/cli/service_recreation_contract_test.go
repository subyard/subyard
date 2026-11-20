package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/migration"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

// releaseRuntimeFixture is a runtime root whose active release is previous-release.
func releaseRuntimeFixture(t *testing.T) string {
	t.Helper()
	root := testkit.TempDir(t)
	if err := os.MkdirAll(filepath.Join(root, "releases/previous-release"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/previous-release", filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	return root
}

// The release transition entry point binds recreation onto the native broker owner.
func TestReleaseTransitionReconcilersBindServiceRecreation(t *testing.T) {
	program, _, _, _, _ := serviceRecreationFixtureFor(t, "test-vm-broker")
	home := environmentValue(program.options.Environment, "SUBYARD_CONFIG_HOME")
	runtimeRoot := releaseRuntimeFixture(t)
	for _, test := range []struct {
		target    releasetransition.ReleaseID
		direction releasetransition.Direction
		bound     bool
	}{
		{"next-release", releasetransition.DirectionActivateTarget, true},
		{"previous-release", releasetransition.DirectionActivateTarget, true},
		{"next-release", releasetransition.DirectionActivatePrevious, false},
	} {
		request := releasetransition.ProcessRequest{ConfigHome: home, RuntimeRoot: runtimeRoot, Target: test.target, Direction: test.direction}
		var broker *activationStageReconciler
		for _, owner := range program.releaseTransitionReconcilers(request) {
			if stage, ok := owner.(*activationStageReconciler); ok && stage.ID() == "test-vm-broker" {
				broker = stage
			}
		}
		if broker == nil || (broker.recreation != nil) != test.bound {
			t.Fatalf("%+v: broker owner %+v", test, broker)
		}
	}
}

// A refused recreation keeps the previous engine on the next same-release gate.
func TestRefusedRecreationStaysRefusedOnSameReleaseGate(t *testing.T) {
	_, _, selection, stage, calls := serviceRecreationFixture(t)
	foreign := selection.record
	foreign.Owner = strings.Repeat("f", 32)
	if err := selection.registry.Remove(selection.record, selection.publication); err != nil {
		t.Fatal(err)
	}
	if err := selection.registry.Save(foreign); err != nil {
		t.Fatal(err)
	}
	for _, releases := range []releasetransition.ReleasePair{{From: "previous-release", Target: "next-release"}, {From: "next-release", Target: "next-release"}} {
		links := releasetransition.ReleaseLinks{Active: releases.Target}
		observed, err := stage.Observe(context.Background(), releases, links)
		if err != nil || !observed.Converged || len(observed.Warnings) != 1 || !strings.Contains(observed.Warnings[0], "keeps its previous engine") {
			t.Fatalf("%+v: refused observation %+v %v", releases, observed, err)
		}
		if err := stage.Reconcile(context.Background(), links); err != nil || len(calls.calls) != 0 || len(calls.platform.applied) != 0 {
			t.Fatalf("%+v: refused runtime changed: %v %v %v", releases, err, calls.calls, calls.platform.applied)
		}
	}
}

// legacyTestYardFixture registers a pre-v0.11 test yard beside a default yard.
func legacyTestYardFixture(t *testing.T) (*CLI, string) {
	t.Helper()
	root, environment, _ := nativeFixture(t)
	home := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	writeConfigCommandFile(t, filepath.Join(home, "yards", legacyTestYardName, "config.env"), "SSH_PORT=2240\n")
	program, err := New(Options{RepositoryRoot: root, Environment: environment, Incus: &testkit.Incus{}})
	if err != nil {
		t.Fatal(err)
	}
	return program, home
}

func TestLegacyTestYardIsLeftToItsRecreation(t *testing.T) {
	program, home := legacyTestYardFixture(t)
	request := releasetransition.ProcessRequest{ConfigHome: home}
	t.Run("readiness-warning", func(t *testing.T) {
		applicability, err := program.brokerActivationWithFiles(request, nil).inspectApplicability(context.Background())
		if err != nil || !slices.Contains(applicability.warnings, legacyTestYardWarning) {
			t.Fatalf("legacy warning: %+v %v", applicability, err)
		}
	})
	t.Run("config-targets", func(t *testing.T) {
		legacy, err := program.loadContext(legacyTestYardName)
		if err != nil {
			t.Fatal(err)
		}
		if targets, err := program.releaseConfigTargets(context.Background(), legacy, false); err != nil || len(targets) != 0 {
			t.Fatalf("release work selected the legacy yard: %+v %v", targets, err)
		}
		if targets, err := program.localConfigTargets(legacy, false); err != nil || len(targets) != 1 {
			t.Fatalf("ordinary config work lost the legacy yard: %+v %v", targets, err)
		}
		all, err := program.releaseConfigTargets(context.Background(), legacy, true)
		if err != nil || slices.ContainsFunc(all, func(target configTarget) bool { return target.Name == legacyTestYardName }) || len(all) == 0 {
			t.Fatalf("all-local release work: %+v %v", all, err)
		}
	})
	t.Run("registered-yard-migration", func(t *testing.T) {
		owner, ok := program.registeredYardMigration(request, false, legacyTestYardName).(*migration.RegisteredYardMigration)
		if !ok {
			t.Fatal("registered yard migration owner changed type")
		}
		if targets, err := owner.LoadTargets(context.Background(), nil); err != nil || len(targets) != 0 {
			t.Fatalf("legacy yard became a migration target: %+v %v", targets, err)
		}
	})
}

// Prospective materialized activation leaves a staged legacy yard to its recreation.
func TestPreparedActivationSkipsLegacyTestYard(t *testing.T) {
	fixture := newSourceScopeFixture(t)
	reconciler := fixture.reconciler(false).materializedConfigActivationReconciler
	reconciler.allLocal = true
	releases := releasetransition.ReleasePair{From: "release-b", Target: "release-b"}
	plan := func(names []string, snapshots map[string]config.PersistentFileSnapshot) releasetransition.V2SourceActivationPlan {
		t.Helper()
		view := preparedActivationSnapshotFixture{home: fixture.configHome, names: names, snapshots: snapshots}
		projected, err := reconciler.PrepareSourceActivation(context.Background(), releases, releasetransition.ReleaseLinks{}, releasetransition.V2SourceActivationView{Settings: view})
		if err != nil {
			t.Fatal(err)
		}
		return projected
	}
	names, err := config.YardNames(fixture.configHome)
	if err != nil {
		t.Fatal(err)
	}
	without := plan(names, map[string]config.PersistentFileSnapshot{})
	legacy := filepath.Join(fixture.configHome, "yards", legacyTestYardName, "config.env")
	with := plan(append(slices.Clone(names), legacyTestYardName), map[string]config.PersistentFileSnapshot{legacy: {Exists: true, Content: []byte("SSH_PORT=2240\n")}})
	if with.Desired != without.Desired {
		t.Fatal("prospective activation planned the legacy yard")
	}
}

// A same-release repair caches each yard's installed view. Teardown still uses
// it; the install after teardown reads the desired context it applies.
func TestRecreationInstallReadsDesiredContextAfterTeardown(t *testing.T) {
	program, _, _, stage, _ := serviceRecreationFixture(t)
	home := environmentValue(program.options.Environment, "SUBYARD_CONFIG_HOME")
	installed, err := program.loadReleaseTransitionContext("fixture-runtime", home, nil)
	if err != nil {
		t.Fatal(err)
	}
	program.activation = &releaseConfiguration{request: releasetransition.ProcessRequest{ConfigHome: home}, repair: true,
		yards: map[string]releaseYardConfiguration{home + "/fixture-runtime": {loaded: installed}}}
	run := stage.recreation.runYard
	var views []string
	stage.recreation.runYard = func(ctx context.Context, yard string, output io.Writer, arguments ...string) error {
		if _, err := program.resolveReleaseTransitionContext(ctx, yard, home); err != nil {
			return err
		}
		views = append(views, arguments[0]+map[bool]string{false: ":installed", true: ":desired"}[program.unobservedReleaseYard(yard, home)])
		return run(ctx, yard, output, arguments...)
	}
	releases := releasetransition.ReleasePair{From: "next-release", Target: "next-release"}
	if _, err := stage.Observe(context.Background(), releases, releasetransition.ReleaseLinks{Active: releases.Target}); err != nil {
		t.Fatal(err)
	}
	if err := stage.Reconcile(context.Background(), releasetransition.ReleaseLinks{Active: releases.Target}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(views, []string{"teardown:installed", "service:desired"}) {
		t.Fatalf("child context views: %v", views)
	}
}

// The engine-only transition ingress hands the engine its broker owner with
// recreation bound.
func TestReleaseTransitionIngressBindsServiceRecreation(t *testing.T) {
	fixture, _, _, _, _ := serviceRecreationFixtureFor(t, "test-vm-broker")
	home := environmentValue(fixture.options.Environment, "SUBYARD_CONFIG_HOME")
	// The ingress verifies the active release's sealed files before composing.
	runtimeRoot := testkit.TempDir(t)
	registry, err := os.ReadFile(filepath.Join(repositoryRoot(t), "config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeSealedRollbackTarget(t, runtimeRoot, "0.30.0-aaaaaaaaaaaa", "0.30.0", registry)
	if err := os.Symlink("releases/0.30.0-aaaaaaaaaaaa", filepath.Join(runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(releasetransition.ProcessRequest{
		SchemaVersion: releasetransition.ProcessProtocolSchemaV1, Mode: releasetransition.ProcessInspect,
		RuntimeRoot: runtimeRoot, ConfigHome: home, Yard: "default",
		Target: "next-release", Direction: releasetransition.DirectionActivateTarget,
		ArtifactDigest: releasetransition.Fingerprint(strings.Repeat("a", 64)),
	})
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: fixture.options.RepositoryRoot, Environment: fixture.options.Environment,
		Incus: fixture.options.Incus, Stdin: bytes.NewReader(payload), Stdout: io.Discard, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	executor := releaseTransitionExecutor
	t.Cleanup(func() { releaseTransitionExecutor = executor })
	var broker *activationStageReconciler
	releaseTransitionExecutor = func(_ context.Context, _ string, _ releasetransition.ProcessRequest, _ func(releasetransition.PlanToken, releasetransition.Authorization) bool,
		reconcilers []releasetransition.V2ActivationReconciler, _ releasetransition.V2OwnerRegistration,
		_ func(releasetransition.ReleasePair) (releasetransition.V2Ingress, error), _ ...*releasetransition.ActivationOnlyRecoveryRequest,
	) (releasetransition.ProcessResponse, error) {
		for _, owner := range reconcilers {
			if stage, ok := owner.(*activationStageReconciler); ok && stage.ID() == "test-vm-broker" {
				broker = stage
			}
		}
		return releasetransition.ProcessResponse{}, errors.New("observed participants")
	}
	if code := program.runReleaseTransition(context.Background(), nil); code != 1 || !strings.Contains(stderr.String(), "observed participants") {
		t.Fatalf("transition ingress: code=%d stderr=%q", code, stderr.String())
	}
	if broker == nil || broker.recreation == nil {
		t.Fatalf("ingress broker owner has no recreation: %+v", broker)
	}
}

// productionBroker is the shipped broker owner, with its projection of
// prepared settings, bound to the recreation fixture's runtime and children.
func productionBroker(t *testing.T) (*CLI, *activationStageReconciler, *serviceRecreationCalls, *int) {
	t.Helper()
	program, _, _, fixture, calls := serviceRecreationFixtureFor(t, "test-vm-broker")
	home := environmentValue(program.options.Environment, "SUBYARD_CONFIG_HOME")
	broker := program.brokerActivationWithFiles(releasetransition.ProcessRequest{ConfigHome: home}, nil)
	broker.platform, broker.recreation = fixture.platform, fixture.recreation
	projections := new(int)
	project := broker.sourceActivation
	broker.sourceActivation = func(ctx context.Context, releases releasetransition.ReleasePair, links releasetransition.ReleaseLinks, view releasetransition.V2SourceActivationView) (releasetransition.V2SourceActivationPlan, error) {
		*projections++
		return project(ctx, releases, links, view)
	}
	return program, broker, calls, projections
}

// brokerUpdate runs `yard update` through the engine with the broker owner.
// With pending source work the engine first projects the broker's Desired
// from the prepared settings, then verifies it against the native one.
func brokerUpdate(t *testing.T, program *CLI, broker *activationStageReconciler, pendingSource bool) func() releasetransition.ProcessResponse {
	t.Helper()
	root := program.options.RepositoryRoot
	home := environmentValue(program.options.Environment, "SUBYARD_CONFIG_HOME")
	payload, err := os.ReadFile(filepath.Join(repositoryRoot(t), "config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeReleaseTransitionTestFile(t, filepath.Join(root, "config", "release-transition.json"), payload, 0o600)
	if pendingSource {
		registry, _, err := releasetransition.ParseRegistryV2(payload, releasetransition.BuiltinCapabilityCatalog())
		if err != nil {
			t.Fatal(err)
		}
		ledger := releasetransition.BaselineLedgerV2(registry)
		for _, migration := range registry.Migrations {
			if migration.Kind == "local-ssh-port-settings-v2-to-v3" {
				break
			}
			if ledger, err = ledger.Advance(registry, migration); err != nil {
				t.Fatal(err)
			}
		}
		encoded, _, err := releasetransition.MarshalLedgerV2(ledger, registry)
		if err != nil {
			t.Fatal(err)
		}
		writeReleaseTransitionTestFile(t, filepath.Join(home, "release-transition/v2/ledger.json"), encoded, 0o600)
		// The broker yard's port still comes from Git settings; the update
		// pins the same effective port into its local settings first.
		writeConfigCommandFile(t, filepath.Join(home, "yards/fixture-runtime/config.env"), "YARD_KIND=container\n")
		writeConfigCommandFile(t, filepath.Join(home, config.GitSettingsRelativePath, "yards/fixture-runtime/config.env"), "SSH_PORT=3333\n")
	}
	runtimeRoot := testkit.TempDir(t)
	for _, release := range []string{"release-a", "release-b"} {
		if err := os.MkdirAll(filepath.Join(runtimeRoot, "releases", release), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("releases/release-a", filepath.Join(runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	request := releasetransition.ProcessRequest{SchemaVersion: releasetransition.ProcessProtocolSchemaV1,
		RuntimeRoot: runtimeRoot, ConfigHome: home, Target: "release-b", Direction: releasetransition.DirectionActivateTarget,
		ArtifactDigest: releasetransition.Fingerprint(strings.Repeat("a", 64))}
	verify := func(releasetransition.PlanToken, releasetransition.Authorization) bool { return true }
	return func() releasetransition.ProcessResponse {
		t.Helper()
		inspect := request
		inspect.Mode = releasetransition.ProcessInspect
		inspected, err := executeReleaseTransitionRequest(context.Background(), root, inspect, verify,
			[]releasetransition.V2ActivationReconciler{broker}, releaseTransitionOwnerFixture{}, nil)
		if err != nil || inspected.Inspection == nil {
			t.Fatalf("inspect: %#v %v", inspected, err)
		}
		converge := request
		converge.Mode = releasetransition.ProcessConverge
		converge.Execution = &releasetransition.Execution{Plan: inspected.Inspection.Plan, Authorization: "confirmed"}
		converged, err := executeReleaseTransitionRequest(context.Background(), root, converge, verify,
			[]releasetransition.V2ActivationReconciler{broker}, releaseTransitionOwnerFixture{}, nil)
		if err != nil || converged.Outcome == nil {
			t.Fatalf("converge: %#v %v", converged, err)
		}
		return converged
	}
}

// deferBrokerYard models release repair with pending configuration of the
// broker yard, which leaves that yard to its own decision.
func deferBrokerYard(program *CLI) {
	program.activation = &releaseConfiguration{repair: true, yards: map[string]releaseYardConfiguration{}}
	program.activation.pending.record(map[string]bool{"fixture-runtime": true})
}

// The projection of prepared settings and the native observation derive the
// same Desired for every broker yard state.
func TestBrokerProjectionMatchesNativeDesired(t *testing.T) {
	for _, state := range []string{"absent", "unfinished-init", "running", "deferred"} {
		t.Run(state, func(t *testing.T) {
			program, broker, calls, projections := productionBroker(t)
			home := environmentValue(program.options.Environment, "SUBYARD_CONFIG_HOME")
			switch state {
			case "absent":
				delete(calls.incus.Instances, calls.key)
			case "unfinished-init":
				half := calls.instance
				half.Config = maps.Clone(half.Config)
				half.Config["user.subyard.initialized"] = "false"
				calls.incus.Instances[calls.key] = half
			case "deferred":
				deferBrokerYard(program)
			}
			names, err := config.YardNames(home)
			if err != nil {
				t.Fatal(err)
			}
			releases := releasetransition.ReleasePair{From: "release-a", Target: "release-b"}
			view := releasetransition.V2SourceActivationView{Settings: preparedActivationSnapshotFixture{home: home, names: names}}
			projected, err := broker.PrepareSourceActivation(context.Background(), releases, releasetransition.ReleaseLinks{Active: releases.From}, view)
			if err != nil || *projections != 1 {
				t.Fatalf("projection: %+v %v (%d)", projected, err, *projections)
			}
			native, err := broker.Observe(context.Background(), releases, releasetransition.ReleaseLinks{Active: releases.From})
			if err != nil || native.Desired != projected.Desired {
				t.Fatalf("native Desired %s differs from projected %s: %v", native.Desired, projected.Desired, err)
			}
		})
	}
}

// An update whose reinstall fails reports its next step; retrying the update
// finishes the release ready and keeps naming the reinstall, whether the
// failed install left no runtime or a half-initialized one, and whether the
// update also carries pending source work.
func TestRetriedUpdateAfterFailedReinstallEndsReadyWithItsNextStep(t *testing.T) {
	for _, half := range []bool{false, true} {
		for _, pendingSource := range []bool{false, true} {
			t.Run(fmt.Sprintf("half=%v/pending-source=%v", half, pendingSource), func(t *testing.T) {
				program, broker, calls, projections := productionBroker(t)
				calls.fail, calls.halfInstall = "service", half
				update := brokerUpdate(t, program, broker, pendingSource)
				install := "yard service install fixture-service --yard fixture-runtime"
				failed := update()
				if failed.Outcome.Status == releasetransition.StatusReady || !strings.Contains(fmt.Sprintf("%+v", *failed.Outcome), install) {
					t.Fatalf("failed reinstall outcome: %+v", failed.Outcome)
				}
				retried := update()
				if retried.Outcome.Status != releasetransition.StatusReady ||
					!slices.ContainsFunc(retried.Outcome.Warnings, func(warning string) bool { return strings.Contains(warning, install) }) {
					t.Fatalf("retried update outcome: %+v", retried.Outcome)
				}
				if want := [][]string{{"stop"}, {"-Y", "fixture-runtime", "teardown", "--yes"}, {"-Y", "fixture-runtime", "service", "install", "fixture-service", "--yard", "fixture-runtime", "--yes"}}; !slices.EqualFunc(calls.calls, want, slices.Equal) {
					t.Fatalf("retry repeated effects: %v", calls.calls)
				}
				if pendingSource != (*projections != 0) {
					t.Fatalf("source projections: %d", *projections)
				}
				if values, err := config.ReadAssignments(filepath.Join(environmentValue(program.options.Environment, "SUBYARD_CONFIG_HOME"), "yards/fixture-runtime/config.env")); err != nil || values["SSH_PORT"] != "3333" {
					t.Fatalf("pending settings work did not finish: %v %v", values, err)
				}
			})
		}
	}
}

// An update with pending source work ends ready whatever state the broker
// yard is in: the engine verifies the projected Desired against the native one.
func TestUpdateWithPendingSourceWorkEndsReadyForEveryBrokerState(t *testing.T) {
	install := "yard service install fixture-service --yard fixture-runtime"
	for _, state := range []string{"absent", "unfinished-init", "running", "deferred"} {
		t.Run(state, func(t *testing.T) {
			program, broker, calls, projections := productionBroker(t)
			switch state {
			case "absent":
				delete(calls.incus.Instances, calls.key)
			case "unfinished-init":
				half := calls.instance
				half.Config = maps.Clone(half.Config)
				half.Config["user.subyard.initialized"] = "false"
				calls.incus.Instances[calls.key] = half
			case "running":
				calls.sameEngine = true
			case "deferred":
				deferBrokerYard(program)
			}
			outcome := brokerUpdate(t, program, broker, true)().Outcome
			if outcome.Status != releasetransition.StatusReady || *projections == 0 {
				t.Fatalf("update with pending source work: %+v (%d projections)", outcome, *projections)
			}
			if named := slices.ContainsFunc(outcome.Warnings, func(warning string) bool { return strings.Contains(warning, install) }); named != (state == "absent" || state == "unfinished-init") {
				t.Fatalf("reinstall warning: %v", outcome.Warnings)
			}
			if len(calls.calls) != 0 {
				t.Fatalf("unexpected children: %v", calls.calls)
			}
		})
	}
}

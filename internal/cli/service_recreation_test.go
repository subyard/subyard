package cli

import (
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

type serviceRecreationCalls struct {
	calls    [][]string
	fail     string
	platform *initPlatformFixture
	// The children act on the fixture runtime like teardown and install do.
	incus     *recreationIncus
	key       string
	instance  ports.InstanceInfo
	selection *serviceSelection
	// keepRecord models an interrupted teardown after its native cleanup.
	keepRecord bool
	// sameEngine models drift other than an engine change.
	sameEngine bool
	engineErr  error
	// halfInstall leaves the failed install's instance uninitialized.
	halfInstall       bool
	prepareRecreation func() error
	checkRecreation   func() error
}

func (fixture *serviceRecreationCalls) run(_ context.Context, yard string, _ io.Writer, arguments ...string) error {
	fixture.calls = append(fixture.calls, append([]string{"-Y", yard}, arguments...))
	switch arguments[0] {
	case "teardown":
		delete(fixture.incus.Instances, fixture.key)
		if fixture.keepRecord {
			return errors.New("injected interruption before registration removal")
		}
		if err := fixture.selection.registry.Remove(fixture.selection.record, fixture.selection.publication); err != nil {
			return err
		}
	case "service":
		if fixture.fail == "service" {
			if fixture.halfInstall {
				half := fixture.instance
				half.Config = maps.Clone(half.Config)
				half.Config["user.subyard.initialized"] = "false"
				fixture.incus.Instances[fixture.key] = half
			}
			return errors.New("injected service failure")
		}
		fixture.incus.Instances[fixture.key] = fixture.instance
		if err := fixture.selection.registry.Save(fixture.selection.record); err != nil {
			return err
		}
		fixture.platform.converged[ports.ReconcileStageTestVMs] = true
	}
	if fixture.fail != "" && arguments[0] == fixture.fail {
		return errors.New("injected " + fixture.fail + " failure")
	}
	return nil
}

// recreationIncus fails only the native ownership read when asked to.
type recreationIncus struct {
	*testkit.Incus
	reconcileErr error
}

func (incus *recreationIncus) ReconcileState(ctx context.Context, project, instance, bridge, pool, volume string) (ports.ReconcileState, error) {
	if incus.reconcileErr != nil {
		return ports.ReconcileState{}, incus.reconcileErr
	}
	return incus.Incus.ReconcileState(ctx, project, instance, bridge, pool, volume)
}

// recreationPlatform records the approved stop that precedes teardown.
type recreationPlatform struct {
	*initPlatformFixture
	calls *serviceRecreationCalls
}

func (platform recreationPlatform) PrepareServiceRecreation(context.Context) error {
	if platform.calls.prepareRecreation != nil {
		return platform.calls.prepareRecreation()
	}
	return nil
}
func (platform recreationPlatform) CheckServiceRecreation(context.Context) error {
	if platform.calls.checkRecreation != nil {
		return platform.calls.checkRecreation()
	}
	return nil
}

func (platform recreationPlatform) StageEngineChanged(_ context.Context, stage ports.ReconcileStageID) (bool, error) {
	if stage != ports.ReconcileStageTestVMs {
		return false, errors.New("unexpected engine stage")
	}
	if platform.calls.engineErr != nil {
		return false, platform.calls.engineErr
	}
	return !platform.calls.sameEngine, nil
}

func (platform recreationPlatform) RunApprovedMigrationStop(context.Context) error {
	platform.calls.calls = append(platform.calls.calls, []string{"stop"})
	if platform.calls.fail == "stop" {
		return errors.New("injected stop failure")
	}
	return nil
}

// serviceRecreationFixture binds a synthetic service to the native owner
// "fixture-owner" in its default yard, with a drifted stage.
func serviceRecreationFixture(t *testing.T) (*CLI, *testkit.Incus, *serviceSelection, *activationStageReconciler, *serviceRecreationCalls) {
	t.Helper()
	return serviceRecreationFixtureFor(t, "fixture-owner")
}

func serviceRecreationFixtureFor(t *testing.T, activation string) (*CLI, *testkit.Incus, *serviceSelection, *activationStageReconciler, *serviceRecreationCalls) {
	t.Helper()
	cliFixture := newFixture(t, withWorkingDir(""))
	root, environment := cliFixture.Root, cliFixture.Environment
	home := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	if err := os.MkdirAll(filepath.Join(home, "yards/fixture-runtime"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(home, "yards/fixture-runtime/config.env"), []byte("SSH_PORT=3333\nYARD_KIND=container\n"), 0o600)
	instance := ports.InstanceInfo{Name: "yard-fixture-runtime", Project: "subyard-fixture-runtime", Status: "Running", Config: map[string]string{"user.subyard.managed": "true", "user.subyard.name": "fixture-runtime", "user.subyard.initialized": "true", "user.subyard.desired_power": "running", "user.subyard.bridge": "incusbr0"}}
	incus := &recreationIncus{Incus: &testkit.Incus{Instances: map[string]ports.InstanceInfo{instance.Project + "/" + instance.Name: instance}}}
	program := cliFixture.New(Options{RepositoryRoot: root, Environment: environment, Incus: incus})
	selection := infrastructureYardFixture(t, program, incus.Incus, "fixture-service", "fixture-runtime")
	path := filepath.Join(root, "config/profiles/synthetic-service/profile.json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, []byte(strings.Replace(string(body), `"id":"fixture-service"`, `"id":"fixture-service","registration_activation":"`+activation+`"`, 1)), 0o644)
	platform := newInitPlatformFixture()
	platform.converged[ports.ReconcileStageTestVMs] = false
	program = cliFixture.New(Options{RepositoryRoot: root, Environment: environment, Incus: incus, InitPlatform: platform, Stdout: io.Discard, Stderr: io.Discard})
	seedOwnerHostID(t, home)
	definition, declared := program.activationService(activation)
	if !declared {
		t.Fatal("synthetic service does not bind its native owner")
	}
	key := selection.record.Project + "/" + selection.record.Instance
	calls := &serviceRecreationCalls{platform: platform, incus: incus, key: key, instance: incus.Instances[key], selection: selection}
	stage := &activationStageReconciler{configuration: program, id: activation, stage: ports.ReconcileStageTestVMs,
		inspectApplicability: func(ctx context.Context) (activationApplicability, error) {
			names, err := config.YardNames(home)
			if err != nil {
				return activationApplicability{}, err
			}
			return program.serviceStageApplicability(ctx, activation, names, func(name string) (config.Loaded, error) {
				return program.resolveReleaseTransitionContext(ctx, name, home)
			})
		},
		platform: func(context.Context, activationApplicability) (ports.ReconcileStageRunner, error) {
			return recreationPlatform{platform, calls}, nil
		},
		recreation: &serviceRecreation{cli: program, configHome: home, definition: definition},
	}
	stage.recreation.runYard = calls.run
	return program, incus.Incus, selection, stage, calls
}

func TestServiceRecreationReplacesInPlaceUpdateOfProvenRuntime(t *testing.T) {
	stop := []string{"stop"}
	teardown := []string{"-Y", "fixture-runtime", "teardown", "--yes"}
	install := []string{"-Y", "fixture-runtime", "service", "install", "fixture-service", "--yard", "fixture-runtime", "--yes"}
	recreated := [][]string{stop, teardown, install}
	for _, scenario := range []string{"recreate", "pool", "converged", "stopped", "unregistered", "other-yard", "foreign", "UUID", "foreign-UUID", "several", "read-failure", "engine-read-failure", "same-engine", "same-engine-stopped", "same-engine-foreign", "recheck", "stop-failure", "teardown-failure", "record-left", "install-failure"} {
		t.Run(scenario, func(t *testing.T) {
			_, incus, selection, stage, calls := serviceRecreationFixture(t)
			key := selection.record.Project + "/" + selection.record.Instance
			replace := func(change func(*serviceSelection)) {
				if err := selection.registry.Remove(selection.record, selection.publication); err != nil {
					t.Fatal(err)
				}
				change(selection)
				if err := selection.registry.Save(selection.record); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "pool":
				replace(func(selection *serviceSelection) { selection.record.Pool = "gpu" })
			case "converged":
				calls.platform.converged[ports.ReconcileStageTestVMs] = true
			case "stopped":
				instance := incus.Instances[key]
				instance.Status, instance.Config["user.subyard.desired_power"] = "Stopped", "stopped"
				incus.Instances[key] = instance
			case "unregistered":
				if err := selection.registry.Remove(selection.record, selection.publication); err != nil {
					t.Fatal(err)
				}
			case "other-yard":
				// Only a registration of the observed yard selects recreation.
				replace(func(selection *serviceSelection) { selection.record.Yard = "other-runtime" })
			case "foreign", "several", "foreign-UUID":
				foreign := selection.record
				foreign.Owner = strings.Repeat("f", 32)
				if scenario == "several" {
					foreign.Pool = "aaa"
				} else if err := selection.registry.Remove(selection.record, selection.publication); err != nil {
					t.Fatal(err)
				}
				if err := selection.registry.Save(foreign); err != nil {
					t.Fatal(err)
				}
				if scenario == "foreign-UUID" {
					incus.Reconcile.Instance.Config["volatile.uuid"] = "replaced-uuid"
				}
			case "UUID":
				incus.Reconcile.Instance.Config["volatile.uuid"] = "replaced-uuid"
			case "read-failure":
				calls.incus.reconcileErr = errors.New("injected Incus read failure")
			case "engine-read-failure":
				calls.engineErr = errors.New("injected Incus read failure")
			case "same-engine":
				// A stale route, host unit or settings drift keeps the engine.
				calls.sameEngine = true
			case "same-engine-stopped":
				// A broker stopped by a host reboot keeps its running intent.
				calls.sameEngine = true
				instance := incus.Instances[key]
				instance.Status = "Stopped"
				incus.Instances[key] = instance
			case "same-engine-foreign":
				// Ownership matters only to a recreation.
				calls.sameEngine = true
				replace(func(selection *serviceSelection) { selection.record.Owner = strings.Repeat("f", 32) })
			case "recheck":
				// The registration changes after assessment, before any effect.
				stage.authorize = func(context.Context) error {
					changed := selection.record
					changed.Generation = strings.Repeat("e", len(changed.Generation))
					if err := selection.registry.Remove(selection.record, selection.publication); err != nil {
						return err
					}
					return selection.registry.Save(changed)
				}
			case "stop-failure":
				calls.fail = "stop"
			case "teardown-failure":
				calls.fail = "teardown"
			case "record-left":
				calls.keepRecord = true
			case "install-failure":
				calls.fail = "service"
			}
			releases := releasetransition.ReleasePair{From: "previous-release", Target: "next-release"}
			before, err := stage.Observe(context.Background(), releases, releasetransition.ReleaseLinks{Active: releases.From})
			if scenario == "read-failure" || scenario == "engine-read-failure" {
				// A failed read blocks the stage; it is never an ownership refusal.
				if err == nil || !strings.Contains(err.Error(), "injected Incus read failure") {
					t.Fatalf("failed read observation: %+v %v", before, err)
				}
				if err := stage.Reconcile(context.Background(), releasetransition.ReleaseLinks{Active: releases.Target}); err == nil || len(calls.calls) != 0 || len(calls.platform.applied) != 0 {
					t.Fatalf("failed read reconciled: %v %v %v", err, calls.calls, calls.platform.applied)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			refused := scenario == "foreign" || scenario == "UUID" || scenario == "foreign-UUID" || scenario == "several"
			inPlace := scenario == "unregistered" || scenario == "other-yard" || strings.HasPrefix(scenario, "same-engine")
			proven := !refused && !inPlace && scenario != "converged" && scenario != "stopped"
			if proven != (len(before.Consequences) == 1 && strings.Contains(before.Consequences[0], "recreate service fixture-service in yard fixture-runtime") && strings.Contains(before.Consequences[0], "SSH sessions are interrupted")) {
				t.Fatalf("recreation consequence: %+v", before)
			}
			if refused != (len(before.Warnings) == 1 && before.Converged) || before.Converged != (refused || scenario == "converged" || scenario == "stopped") {
				t.Fatalf("observation: %+v", before)
			}
			if scenario == "recreate" {
				// The plan binds the decision: an in-place update observes another drift.
				if err := selection.registry.Remove(selection.record, selection.publication); err != nil {
					t.Fatal(err)
				}
				inPlace, err := stage.Observe(context.Background(), releases, releasetransition.ReleaseLinks{Active: releases.From})
				if err != nil || inPlace.Actual == before.Actual || inPlace.Desired != before.Desired {
					t.Fatalf("recreation decision is not bound into the plan: %+v %v", inPlace, err)
				}
				if err := selection.registry.Save(selection.record); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "foreign":
				if !strings.Contains(before.Warnings[0], "belongs to installation") || !strings.Contains(before.Warnings[0], "yard service remove fixture-service, then yard service install fixture-service --yard fixture-runtime") {
					t.Fatalf("foreign owner warning: %v", before.Warnings)
				}
			case "several":
				// Independent findings are reported together, with the native proof.
				if !strings.Contains(before.Warnings[0], "has 2 registrations") || !strings.Contains(before.Warnings[0], "belongs to installation") {
					t.Fatalf("collected refusals: %v", before.Warnings)
				}
			case "foreign-UUID":
				// The native proof is collected even beside an ownership finding.
				if !strings.Contains(before.Warnings[0], "belongs to installation") || !strings.Contains(before.Warnings[0], "runtime changed") {
					t.Fatalf("independent native proof: %v", before.Warnings)
				}
			case "UUID":
				if !strings.Contains(before.Warnings[0], "runtime changed") {
					t.Fatalf("unproven runtime warning: %v", before.Warnings)
				}
			}
			err = stage.Reconcile(context.Background(), releasetransition.ReleaseLinks{Active: releases.Target})
			want := [][]string(nil)
			switch scenario {
			case "recreate", "install-failure":
				want = recreated
			case "pool":
				want = [][]string{stop, teardown, append(slices.Clone(install[:len(install)-1]), "--pool", "gpu", "--yes")}
			case "stop-failure":
				want = recreated[:1]
			case "teardown-failure", "record-left":
				want = recreated[:2]
			}
			if !reflect.DeepEqual(calls.calls, want) {
				t.Fatalf("children: %v", calls.calls)
			}
			if inPlace != reflect.DeepEqual(calls.platform.applied, []ports.ReconcileStageID{ports.ReconcileStageTestVMs}) || !inPlace && len(calls.platform.applied) != 0 {
				t.Fatalf("in-place apply: %v", calls.platform.applied)
			}
			failed := strings.HasSuffix(scenario, "-failure") || scenario == "recheck" || scenario == "record-left"
			if failed != (err != nil) {
				t.Fatalf("reconcile error: %v", err)
			}
			if scenario == "recheck" && !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("changed registration reached effects: %v", err)
			}
			if scenario == "install-failure" {
				var diagnostic interface{ ActivationDiagnostic() (string, string) }
				if !errors.As(err, &diagnostic) {
					t.Fatalf("install failure has no diagnostic: %v", err)
				}
				if _, retry := diagnostic.ActivationDiagnostic(); retry != "yard service install fixture-service --yard fixture-runtime" {
					t.Fatalf("install retry: %q", retry)
				}
			}
			after, err := stage.Observe(context.Background(), releases, releasetransition.ReleaseLinks{Active: releases.Target})
			if err != nil {
				t.Fatal(err)
			}
			// An interrupted recreation stays visible after the journal completes.
			switch scenario {
			case "install-failure", "teardown-failure":
				if !after.Converged || len(after.Warnings) != 1 || after.Warnings[0] != "service fixture-service has no runtime in yard fixture-runtime; run yard service install fixture-service --yard fixture-runtime" {
					t.Fatalf("missing runtime after %s: %+v", scenario, after)
				}
				return
			case "record-left":
				if !after.Converged || len(after.Warnings) != 1 || after.Warnings[0] != "service fixture-service has no runtime in yard fixture-runtime but keeps its registration; run yard service remove fixture-service, then yard service install fixture-service --yard fixture-runtime" {
					t.Fatalf("missing runtime with a registration: %+v", after)
				}
				return
			}
			if err != nil || failed {
				return
			}
			if !after.Converged || after.Desired != before.Desired || len(after.Consequences) != 0 {
				t.Fatalf("after reconcile: %+v %v", after, err)
			}
		})
	}
}

// Readiness and same-release gates keep a refused recreation refused; only
// rollback to an older engine updates the runtime in place.
func TestServiceRecreationBindsEveryForwardActivation(t *testing.T) {
	program, _, _, stage, _ := serviceRecreationFixture(t)
	runtimeRoot := testkit.TempDir(t)
	if err := os.MkdirAll(filepath.Join(runtimeRoot, "releases/previous-release"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/previous-release", filepath.Join(runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	home := environmentValue(program.options.Environment, "SUBYARD_CONFIG_HOME")
	for _, test := range []struct {
		target    releasetransition.ReleaseID
		direction releasetransition.Direction
		bound     bool
	}{
		{"next-release", releasetransition.DirectionActivateTarget, true},
		{"previous-release", releasetransition.DirectionActivateTarget, true},
		{"next-release", releasetransition.DirectionActivatePrevious, false},
	} {
		stage.recreation = nil
		other := &activationStageReconciler{id: "unrelated-owner"}
		request := releasetransition.ProcessRequest{ConfigHome: home, RuntimeRoot: runtimeRoot, Target: test.target, Direction: test.direction}
		owners := program.bindServiceRecreation(request, []releasetransition.V2ActivationReconciler{stage, other})
		if len(owners) != 2 || (stage.recreation != nil) != test.bound || other.recreation != nil {
			t.Fatalf("binding %+v: %+v %+v", test, stage.recreation, other.recreation)
		}
		if test.bound && (stage.recreation.definition.InfrastructureService.ID != "fixture-service" || stage.recreation.configHome != home) {
			t.Fatalf("bound recreation: %+v", stage.recreation)
		}
	}
}

// A same-release gate repairs drift other than an engine change in place:
// a broker stopped by a reboot, a missing host sink timer or a stale route
// never recreates it. Only an engine change does.
func TestSameReleaseGateRecreatesOnlyOnEngineChange(t *testing.T) {
	recreated := [][]string{{"stop"}, {"-Y", "fixture-runtime", "teardown", "--yes"}, {"-Y", "fixture-runtime", "service", "install", "fixture-service", "--yard", "fixture-runtime", "--yes"}}
	for _, drift := range []string{"stopped-running-intent", "sink-timer", "route", "engine"} {
		t.Run(drift, func(t *testing.T) {
			_, incus, selection, stage, calls := serviceRecreationFixture(t)
			calls.sameEngine = drift != "engine"
			if drift == "stopped-running-intent" {
				key := selection.record.Project + "/" + selection.record.Instance
				instance := incus.Instances[key]
				instance.Status = "Stopped"
				incus.Instances[key] = instance
			}
			releases := releasetransition.ReleasePair{From: "next-release", Target: "next-release"}
			links := releasetransition.ReleaseLinks{Active: releases.Target}
			observed, err := stage.Observe(context.Background(), releases, links)
			if err != nil || observed.Converged || len(observed.Warnings) != 0 {
				t.Fatalf("observation: %+v %v", observed, err)
			}
			if recreate := drift == "engine"; recreate != (len(observed.Consequences) == 1) {
				t.Fatalf("consequences: %v", observed.Consequences)
			}
			if err := stage.Reconcile(context.Background(), links); err != nil {
				t.Fatal(err)
			}
			if drift == "engine" {
				if !reflect.DeepEqual(calls.calls, recreated) || len(calls.platform.applied) != 0 {
					t.Fatalf("engine change: %v %v", calls.calls, calls.platform.applied)
				}
				return
			}
			if len(calls.calls) != 0 || !reflect.DeepEqual(calls.platform.applied, []ports.ReconcileStageID{ports.ReconcileStageTestVMs}) {
				t.Fatalf("%s was not repaired in place: %v %v", drift, calls.calls, calls.platform.applied)
			}
		})
	}
}

func TestServiceStageApplicabilityLeavesAbsentAndUnfinishedRuntimes(t *testing.T) {
	program, incus, selection, stage, _ := serviceRecreationFixture(t)
	key := selection.record.Project + "/" + selection.record.Instance
	install := "yard service install fixture-service --yard fixture-runtime"
	for _, test := range []struct {
		name, status, desired, initialized string
		state                              string
		applies                            bool
		warning                            string
	}{
		{"running", "Running", "running", "true", "active", true, ""},
		{"stopped-running-intent", "Stopped", "running", "true", "active", true, ""},
		{"stopped", "Stopped", "stopped", "true", "inactive", false, ""},
		{"stopped-without-intent", "Stopped", "", "true", "inactive", false, ""},
		{"unfinished-init", "Running", "running", "false", "inactive", false, "service fixture-service in yard fixture-runtime has not finished installing; run " + install},
		{"absent", "", "", "", "absent", false, "service fixture-service has no runtime in yard fixture-runtime but keeps its registration; run yard service remove fixture-service, then " + install},
	} {
		instance := incus.Instances[key]
		if test.status == "" {
			delete(incus.Instances, key)
		} else {
			instance.Status = test.status
			instance.Config["user.subyard.desired_power"], instance.Config["user.subyard.initialized"] = test.desired, test.initialized
			incus.Instances[key] = instance
		}
		applicability, err := stage.inspectApplicability(context.Background())
		if err != nil || applicability.state != test.state || applicability.applies != test.applies || strings.Join(applicability.warnings, "\n") != test.warning {
			t.Fatalf("%s: %+v %v", test.name, applicability, err)
		}
		incus.Instances[key] = instance
	}
	// A named pool is part of each reinstall command.
	if err := selection.registry.Remove(selection.record, selection.publication); err != nil {
		t.Fatal(err)
	}
	selection.record.Pool = "gpu"
	if err := selection.registry.Save(selection.record); err != nil {
		t.Fatal(err)
	}
	unfinished := incus.Instances[key]
	unfinished.Config["user.subyard.initialized"] = "false"
	incus.Instances[key] = unfinished
	if applicability, err := stage.inspectApplicability(context.Background()); err != nil || strings.Join(applicability.warnings, "\n") != "service fixture-service in yard fixture-runtime has not finished installing; run "+install+" --pool gpu" {
		t.Fatalf("unfinished pool hint: %+v %v", applicability, err)
	}
	delete(incus.Instances, key)
	if applicability, err := stage.inspectApplicability(context.Background()); err != nil || strings.Join(applicability.warnings, "\n") != "service fixture-service has no runtime in yard fixture-runtime but keeps its registration; run yard service remove fixture-service --pool gpu, then "+install+" --pool gpu" {
		t.Fatalf("missing runtime pool hint: %+v %v", applicability, err)
	}
	unfinished.Config["user.subyard.initialized"] = "true"
	incus.Instances[key] = unfinished
	instance := incus.Instances[key]
	instance.Status, instance.Config["user.subyard.initialized"] = "Frozen", "true"
	incus.Instances[key] = instance
	if _, err := stage.inspectApplicability(context.Background()); err == nil {
		t.Fatal("an unsupported runtime state was treated as observed")
	}
	if _, err := program.serviceStageApplicability(context.Background(), "undeclared-owner", []string{"fixture-runtime"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(environmentValue(program.options.Environment, "SUBYARD_CONFIG_HOME"), "yards/fixture-runtime/config.env"), []byte("SSH_PORT=3333\nYARD_KIND=vm\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if applicability, err := stage.inspectApplicability(context.Background()); err != nil || applicability.state != "absent" || len(applicability.warnings) != 0 {
		t.Fatalf("a yard without the declared settings is not the service runtime: %+v %v", applicability, err)
	}
}

func TestLegacyTestYardInitNamesRecreation(t *testing.T) {
	cliFixture := newFixture(t, withWorkingDir(""))
	root, env := cliFixture.Root, cliFixture.Environment
	home := environmentValue(env, "SUBYARD_CONFIG_HOME")
	path := filepath.Join(home, "yards", legacyTestYardName, "config.env")
	writeConfigCommandFile(t, path, "SSH_PORT=2240\n")
	program := cliFixture.New(Options{RepositoryRoot: root, Environment: env})
	loaded, err := program.loadContext(legacyTestYardName)
	if err != nil {
		t.Fatal(err)
	}
	definition, _ := program.manifest.Lookup("init")
	prepared := &preparedCommand{CLI: admitPreparedFixture(program), Loaded: loaded, Definition: definition}
	if err := prepared.prepareInit(context.Background(), nil); err == nil || !strings.Contains(err.Error(), legacyTestYardRecreation) {
		t.Fatalf("init lost the recreation commands: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "SSH_PORT=2240\n" {
		t.Fatal("refusal changed registration", err)
	}
}

// The native stage owner observes a registration staged by the release exactly
// as it observes the published one, whatever its ordinary mode.
func TestServiceStageProspectiveRegistrationPreservesDesired(t *testing.T) {
	program, _, _, _, _ := serviceRecreationFixtureFor(t, "test-vm-broker")
	configHome := environmentValue(program.options.Environment, "SUBYARD_CONFIG_HOME")
	path := filepath.Join(configHome, "yards/fixture-runtime/config.env")
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	actor := program.brokerActivationReconciler(releasetransition.ProcessRequest{ConfigHome: configHome}).(*activationStageReconciler)
	pair := releasetransition.ReleasePair{From: "a", Target: "a"}
	ordinary, err := actor.Observe(context.Background(), pair, releasetransition.ReleaseLinks{})
	if err != nil {
		t.Fatal(err)
	}
	view := prospectiveOwnerSettingsView{yards: []string{"fixture-runtime"}, snapshots: map[string]config.PersistentFileSnapshot{path: {Exists: true, Content: payload}}}
	projected, err := actor.ObserveProspective(context.Background(), pair, releasetransition.ReleaseLinks{}, view)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, payload, 0o600)
	published, err := actor.Observe(context.Background(), pair, releasetransition.ReleaseLinks{})
	if err != nil {
		t.Fatal(err)
	}
	if ordinary.Desired == published.Desired || projected.Desired != published.Desired {
		t.Fatalf("registration desired scope: absent=%s prospective=%s published=%s", ordinary.Desired, projected.Desired, published.Desired)
	}
	for _, mode := range []os.FileMode{0o640, 0o644} {
		t.Run(mode.String(), func(t *testing.T) {
			testkit.WriteFile(t, path, payload, mode)
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			retained, normalErr := actor.Observe(context.Background(), pair, releasetransition.ReleaseLinks{})
			prospective, projectedErr := actor.ObserveProspective(context.Background(), pair, releasetransition.ReleaseLinks{}, retainedActivationSettingsView{configHome})
			if normalErr != nil || projectedErr != nil || prospective.Desired != retained.Desired {
				t.Fatalf("retained registration bridge changed desired: %v/%v", normalErr, projectedErr)
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Fatal("prospective observation mutated registration")
			}
		})
	}
}

// A refused recreation names the commands for the registration's own pool.
func TestRefusedRecreationNamesTheRegisteredPool(t *testing.T) {
	_, _, selection, stage, calls := serviceRecreationFixture(t)
	if err := selection.registry.Remove(selection.record, selection.publication); err != nil {
		t.Fatal(err)
	}
	foreign := selection.record
	foreign.Owner, foreign.Pool = strings.Repeat("f", 32), "gpu"
	if err := selection.registry.Save(foreign); err != nil {
		t.Fatal(err)
	}
	releases := releasetransition.ReleasePair{From: "previous-release", Target: "next-release"}
	observed, err := stage.Observe(context.Background(), releases, releasetransition.ReleaseLinks{Active: releases.From})
	want := "recreate it with yard service remove fixture-service --pool gpu, then yard service install fixture-service --yard fixture-runtime --pool gpu"
	if err != nil || len(observed.Warnings) != 1 || !strings.HasSuffix(observed.Warnings[0], want) || len(calls.calls) != 0 {
		t.Fatalf("refusal for a named pool: %+v %v", observed, err)
	}
}

func TestRecreationPreflightReportsPreparationAndIndependentSafetyRefusals(t *testing.T) {
	_, _, _, stage, calls := serviceRecreationFixture(t)
	calls.prepareRecreation = func() error { return errors.New("guard record write failed") }
	calls.checkRecreation = func() error { return errors.New("unsafe host default route") }
	err := stage.PreflightActivation(context.Background())
	if err == nil || !strings.Contains(err.Error(), "guard record write failed") || !strings.Contains(err.Error(), "unsafe host default route") || len(calls.calls) != 0 {
		t.Fatalf("preparation hid safety refusal or changed runtime: %v %v", err, calls.calls)
	}
}

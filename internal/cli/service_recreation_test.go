package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

type serviceRecreationCalls struct {
	calls    [][]string
	fail     string
	platform *initPlatformFixture
}

func (fixture *serviceRecreationCalls) run(_ context.Context, yard string, _ io.Writer, arguments ...string) error {
	fixture.calls = append(fixture.calls, append([]string{"-Y", yard}, arguments...))
	if fixture.fail != "" && arguments[0] == fixture.fail {
		return errors.New("injected " + fixture.fail + " failure")
	}
	if arguments[0] == "service" {
		fixture.platform.converged[ports.ReconcileStageTestVMs] = true
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
	root, environment, _ := nativeFixture(t)
	home := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	if err := os.MkdirAll(filepath.Join(home, "yards/fixture-runtime"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(home, "yards/fixture-runtime/config.env"), []byte("SSH_PORT=3333\nYARD_KIND=container\n"), 0o600)
	instance := ports.InstanceInfo{Name: "yard-fixture-runtime", Project: "subyard-fixture-runtime", Status: "Running", Config: map[string]string{"user.subyard.managed": "true", "user.subyard.name": "fixture-runtime", "user.subyard.initialized": "true", "user.subyard.desired_power": "running", "user.subyard.bridge": "incusbr0"}}
	incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{instance.Project + "/" + instance.Name: instance}}
	program, err := New(Options{RepositoryRoot: root, Environment: environment, Incus: incus})
	if err != nil {
		t.Fatal(err)
	}
	selection := infrastructureYardFixture(t, program, incus, "fixture-service", "fixture-runtime")
	path := filepath.Join(root, "config/profiles/synthetic-service/profile.json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, []byte(strings.Replace(string(body), `"id":"fixture-service"`, `"id":"fixture-service","registration_activation":"`+activation+`"`, 1)), 0o644)
	platform := newInitPlatformFixture()
	platform.converged[ports.ReconcileStageTestVMs] = false
	if program, err = New(Options{RepositoryRoot: root, Environment: environment, Incus: incus, InitPlatform: platform, Stdout: io.Discard, Stderr: io.Discard}); err != nil {
		t.Fatal(err)
	}
	seedOwnerHostID(t, home)
	definition, declared := program.activationService(activation)
	if !declared {
		t.Fatal("synthetic service does not bind its native owner")
	}
	calls := &serviceRecreationCalls{platform: platform}
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
			return platform, nil
		},
		recreation: &serviceRecreation{cli: program, configHome: home, definition: definition},
	}
	stage.recreation.runYard = calls.run
	return program, incus, selection, stage, calls
}

func TestServiceRecreationReplacesInPlaceUpdateOfProvenRuntime(t *testing.T) {
	recreated := [][]string{{"-Y", "fixture-runtime", "teardown", "--yes"}, {"-Y", "fixture-runtime", "service", "install", "fixture-service", "--yard", "fixture-runtime", "--yes"}}
	for _, scenario := range []string{"recreate", "converged", "stopped", "unregistered", "foreign", "UUID", "several", "teardown-failure", "install-failure"} {
		t.Run(scenario, func(t *testing.T) {
			_, incus, selection, stage, calls := serviceRecreationFixture(t)
			key := selection.record.Project + "/" + selection.record.Instance
			switch scenario {
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
			case "foreign", "several":
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
			case "UUID":
				incus.Reconcile.Instance.Config["volatile.uuid"] = "replaced-uuid"
			case "teardown-failure":
				calls.fail = "teardown"
			case "install-failure":
				calls.fail = "service"
			}
			releases := releasetransition.ReleasePair{From: "previous-release", Target: "next-release"}
			before, err := stage.Observe(context.Background(), releases, releasetransition.ReleaseLinks{Active: releases.From})
			if err != nil {
				t.Fatal(err)
			}
			refused := scenario == "foreign" || scenario == "UUID" || scenario == "several"
			proven := scenario == "recreate" || strings.HasSuffix(scenario, "-failure")
			if proven != (len(before.Consequences) == 1 && strings.Contains(before.Consequences[0], "recreate service fixture-service in yard fixture-runtime")) {
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
				if !strings.Contains(before.Warnings[0], "belongs to installation") {
					t.Fatalf("foreign owner warning: %v", before.Warnings)
				}
			case "several":
				// Independent findings are reported together.
				if !strings.Contains(before.Warnings[0], "has 2 registrations") || !strings.Contains(before.Warnings[0], "belongs to installation") {
					t.Fatalf("collected refusals: %v", before.Warnings)
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
			case "teardown-failure":
				want = recreated[:1]
			}
			if !reflect.DeepEqual(calls.calls, want) {
				t.Fatalf("children: %v", calls.calls)
			}
			if inPlace := scenario == "unregistered"; inPlace != reflect.DeepEqual(calls.platform.applied, []ports.ReconcileStageID{ports.ReconcileStageTestVMs}) || !inPlace && len(calls.platform.applied) != 0 {
				t.Fatalf("in-place apply: %v", calls.platform.applied)
			}
			if failed := strings.HasSuffix(scenario, "-failure"); failed != (err != nil) {
				t.Fatalf("reconcile error: %v", err)
			}
			if scenario == "install-failure" {
				var diagnostic interface{ ActivationDiagnostic() (string, string) }
				if !errors.As(err, &diagnostic) {
					t.Fatalf("install failure has no diagnostic: %v", err)
				}
				if _, retry := diagnostic.ActivationDiagnostic(); retry != "yard service install fixture-service --yard fixture-runtime" {
					t.Fatalf("install retry: %q", retry)
				}
				return
			}
			if err != nil {
				return
			}
			after, err := stage.Observe(context.Background(), releases, releasetransition.ReleaseLinks{Active: releases.Target})
			if err != nil || !after.Converged || after.Desired != before.Desired || len(after.Consequences) != 0 {
				t.Fatalf("after reconcile: %+v %v", after, err)
			}
		})
	}
}

func TestServiceRecreationBindsOnlyNewReleaseActivation(t *testing.T) {
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
		{"previous-release", releasetransition.DirectionActivateTarget, false},
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

func TestServiceStageApplicabilityLeavesAbsentAndUnfinishedRuntimes(t *testing.T) {
	program, incus, selection, stage, _ := serviceRecreationFixture(t)
	key := selection.record.Project + "/" + selection.record.Instance
	for _, test := range []struct {
		name, status, desired, initialized string
		state                              string
		applies                            bool
	}{
		{"running", "Running", "running", "true", "active", true},
		{"stopped-running-intent", "Stopped", "running", "true", "active", true},
		{"stopped", "Stopped", "stopped", "true", "inactive", false},
		{"unfinished-init", "Running", "running", "false", "inactive", false},
		{"absent", "", "", "", "absent", false},
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
		if err != nil || applicability.state != test.state || applicability.applies != test.applies {
			t.Fatalf("%s: %+v %v", test.name, applicability, err)
		}
		incus.Instances[key] = instance
	}
	if _, err := program.serviceStageApplicability(context.Background(), "undeclared-owner", []string{"fixture-runtime"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(environmentValue(program.options.Environment, "SUBYARD_CONFIG_HOME"), "yards/fixture-runtime/config.env"), []byte("SSH_PORT=3333\nYARD_KIND=vm\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if applicability, err := stage.inspectApplicability(context.Background()); err != nil || applicability.state != "absent" {
		t.Fatalf("a yard without the declared settings is not the service runtime: %+v %v", applicability, err)
	}
}

func TestLegacyTestYardInitNamesRecreation(t *testing.T) {
	root, env, _ := nativeFixture(t)
	home := environmentValue(env, "SUBYARD_CONFIG_HOME")
	path := filepath.Join(home, "yards", legacyTestYardName, "config.env")
	writeConfigCommandFile(t, path, "SSH_PORT=2240\n")
	program, err := New(Options{RepositoryRoot: root, Environment: env})
	if err != nil {
		t.Fatal(err)
	}
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

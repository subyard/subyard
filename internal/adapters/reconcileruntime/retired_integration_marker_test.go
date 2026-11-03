package reconcileruntime

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

const retiredMarkerKey = retiredIntegrationMarkerPrefix + "widget"

func retiredDefinition() profile.IntegrationDefinition {
	return profile.IntegrationDefinition{Name: "widget", Retired: true, Lifecycle: &profile.IntegrationLifecycle{
		Handler: "retire.py", RetirementNotice: "retire integration widget: remove its data",
	}}
}

func stoppedRetiredRuntime(t *testing.T) (Runtime, *testkit.Incus) {
	t.Helper()
	incus := &testkit.Incus{
		ServerInfo: ports.ServerInfo{Environment: "incus"},
		Reconcile: ports.ReconcileState{InstanceFound: true, VolumeFound: true, Instance: ports.InstanceInfo{
			Status: "Stopped",
			Config: map[string]string{
				"user.subyard.managed": "true", "user.subyard.initialized": "true", "user.subyard.desired_power": "stopped",
			},
			LocalConfig: map[string]string{"security.nesting": "true"},
			LocalDevices: map[string]map[string]string{
				"srv": {"source": "yard-srv", "path": "/srv", "pool": "default"},
				"subyard-e2e-routes": {
					"type": "disk", "source": "/data/e2e/routes", "path": "/var/lib/subyard/e2e-routes", "readonly": "true",
				},
			},
		}},
	}
	bin := t.TempDir()
	testkit.WriteFile(t, filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\nexit 0\n"), 0o700)
	return Runtime{
		Incus: incus, Executor: incus, ConfigWriter: incus, HostDeviceRoot: t.TempDir(),
		Integrations:     []profile.IntegrationDefinition{retiredDefinition()},
		RetiredSelection: []string{"widget"},
		Environment:      []string{"PATH=" + bin},
		Yard: domain.Context{
			YardName: "example", IncusProject: "subyard", YardInstanceName: "yard", YardKind: domain.YardContainer,
			Paths: domain.RuntimePaths{DataHome: "/data"},
		},
	}, incus
}

func TestStoppedYardBootsOnlyToProveASelectedRetiredPackage(t *testing.T) {
	runtime, incus := stoppedRetiredRuntime(t)
	assertStageConverged(t, runtime, false, "retired selection without proof")
	retirements, warnings, err := runtime.RetiredIntegrationNotices(context.Background())
	if err != nil || len(warnings) != 0 || len(retirements) != 2 ||
		!strings.Contains(retirements[0], "temporarily start this stopped yard to retire integration widget") ||
		retirements[1] != "retire integration widget: remove its data" {
		t.Fatalf("plan text=%q warnings=%q err=%v", retirements, warnings, err)
	}
	if len(incus.ExecCalls) != 0 {
		t.Fatal("stopped yard planning probed the guest")
	}

	incus.Reconcile.Instance.Config[retiredMarkerKey] = "true"
	assertStageConverged(t, runtime, true, "proof present")
	if retirements, _, err := runtime.RetiredIntegrationNotices(context.Background()); err != nil || len(retirements) != 0 {
		t.Fatalf("proven yard still plans a boot: %q %v", retirements, err)
	}
	delete(incus.Reconcile.Instance.Config, retiredMarkerKey)

	ordinary := runtime
	ordinary.RetiredSelection = nil
	assertStageConverged(t, ordinary, true, "ordinary stopped yard")
	forbidden := runtime
	forbidden.Environment = append(slices.Clone(runtime.Environment), "ALLOWS_CODING_TOOLS=false")
	assertStageConverged(t, forbidden, true, "role that forbids coding tools")
	unknown := runtime
	unknown.RetiredSelection = []string{"another"}
	assertStageConverged(t, unknown, true, "selected ID without a lifecycle package")
	running := runtime
	incus.Reconcile.Instance.Status = "Running"
	assertStageConverged(t, running, true, "instance stage after the temporary boot")
}

func TestRunningProvisionStagePublishesMissingProof(t *testing.T) {
	previewLoopbackFixture(t)
	runtime, incus := stoppedRetiredRuntime(t)
	runtime.Environment = append(runtime.Environment, "CCUSAGE_VERSION=1.2.3")
	incus.Reconcile.Instance.Status = "Running"
	incus.Reconcile.Instance.Config["user.subyard.ccusage_version"] = "1.2.3"
	assertStage(t, runtime, "provision", false, "already clean running yard without proof")
	if len(incus.ExecCalls) != 0 {
		t.Fatal("missing proof needs no guest probe")
	}
}

func retiredApplyFixture(t *testing.T, next string) (Runtime, *testkit.Incus) {
	t.Helper()
	runtime, definition, _, _ := lifecycleFixture(t)
	definition.Retired = true
	root := definition.Root
	for _, directory := range []string{"config", "scripts"} {
		if err := os.Mkdir(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	testkit.WriteFile(t, filepath.Join(root, "config/projects-changed.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o644)
	testkit.WriteFile(t, filepath.Join(root, "scripts/reconcile-integrations.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	testkit.WriteFile(t, filepath.Join(root, "next"), []byte(next), 0o600)
	incus := &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{
		Status: "Running", Config: map[string]string{}, Devices: map[string]map[string]string{},
	}}}
	incus.Instances = map[string]ports.InstanceInfo{"test/yard": incus.Reconcile.Instance}
	runtime.RepositoryRoot = root
	runtime.Integrations = []profile.IntegrationDefinition{definition}
	runtime.RetiredSelection = []string{definition.Name}
	runtime.Incus, runtime.ConfigWriter, runtime.Executor = incus, incus, &retryableIntegrationExecutor{}
	runtime.Yard = domain.Context{YardName: "example", IncusProject: "test", YardInstanceName: "yard", DevUser: "dev", DevUID: os.Getuid()}
	runtime.Environment[len(runtime.Environment)-1] = "CODING_TOOL_INTEGRATIONS="
	runtime.Environment = append(runtime.Environment, "SUBYARD_OPERATION_ID=operation-1")
	return runtime, incus
}

func TestIntegrationApplyPublishesProofOnlyAfterVerifiedRetirement(t *testing.T) {
	proof := retiredIntegrationMarkerPrefix + "sample"
	t.Run("verified retirement", func(t *testing.T) {
		runtime, incus := retiredApplyFixture(t, `{"state":"absent","actual":"","desired":""}`)
		plan, err := runtime.IntegrationPlan(context.Background())
		if err != nil || !plan.Changed {
			t.Fatalf("plan=%+v err=%v", plan, err)
		}
		if err := runtime.ApplyIntegrations(context.Background(), plan); err != nil {
			t.Fatal(err)
		}
		if len(incus.ConfigUpdates) != 1 || incus.ConfigUpdates[0].Values[proof] != "true" {
			t.Fatalf("config updates=%+v", incus.ConfigUpdates)
		}
	})
	t.Run("failed retirement", func(t *testing.T) {
		runtime, incus := retiredApplyFixture(t, `{"state":"stale","actual":"`+strings.Repeat("c", 64)+`","desired":"`+strings.Repeat("d", 64)+`"}`)
		plan, err := runtime.IntegrationPlan(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := runtime.ApplyIntegrations(context.Background(), plan); err == nil {
			t.Fatal("unconverged retirement succeeded")
		}
		if len(incus.ConfigUpdates) != 0 {
			t.Fatalf("failed retirement wrote proof: %+v", incus.ConfigUpdates)
		}
	})
	t.Run("kept foreign state and already clean guests", func(t *testing.T) {
		runtime, incus := retiredApplyFixture(t, `{"state":"absent","actual":"","desired":""}`)
		testkit.WriteFile(t, filepath.Join(runtime.RepositoryRoot, "state"), []byte(`{"state":"absent","actual":"","desired":"","notice":"foreign unit stays"}`), 0o600)
		plan, err := runtime.IntegrationPlan(context.Background())
		if err != nil || plan.Changed || len(plan.Warnings) != 1 {
			t.Fatalf("plan=%+v err=%v", plan, err)
		}
		if err := runtime.ApplyIntegrations(context.Background(), plan); err != nil {
			t.Fatal(err)
		}
		if len(incus.ConfigUpdates) != 1 || incus.ConfigUpdates[0].Values[proof] != "true" {
			t.Fatalf("config updates=%+v", incus.ConfigUpdates)
		}
		instance := incus.Instances["test/yard"]
		instance.Config = map[string]string{proof: "true"}
		incus.Instances["test/yard"] = instance
		if err := runtime.recordRetiredIntegrations(context.Background()); err != nil || len(incus.ConfigUpdates) != 1 {
			t.Fatalf("proven yard rewrote proof: %+v, %v", incus.ConfigUpdates, err)
		}
	})
	t.Run("write failure stays visible", func(t *testing.T) {
		runtime, incus := retiredApplyFixture(t, `{"state":"absent","actual":"","desired":""}`)
		incus.Err = errors.New("write refused")
		if err := runtime.recordRetiredIntegrations(context.Background()); err == nil || len(incus.ConfigUpdates) != 0 {
			t.Fatalf("write failure=%v updates=%+v", err, incus.ConfigUpdates)
		}
	})
}

// A failed temporary-start retirement returns before finalize, so the yard stays running
// behind its fence with the stopped intent. Nothing persists between the two inits but that
// instance metadata and the guest, so each run below uses a fresh runtime value.
func TestFreshInitConvergesAfterFailedTemporaryStartRetirement(t *testing.T) {
	fenced := map[string]string{
		"user.subyard.managed": "true", "user.subyard.initialized": "false", "user.subyard.desired_power": "stopped",
		"user.subyard.name": "example", "user.subyard.bridge": "incusbr0", "boot.autostart": "false",
	}
	t.Run("the retry reaches provision and finalize again", func(t *testing.T) {
		previewLoopbackFixture(t)
		runtime, incus := stoppedRetiredRuntime(t)
		runtime.Environment = append(runtime.Environment, "CCUSAGE_VERSION=1.2.3")
		incus.Reconcile.Instance.Status = "Running"
		incus.Reconcile.Instance.Config["user.subyard.ccusage_version"] = "1.2.3"
		maps.Copy(incus.Reconcile.Instance.Config, fenced)
		assertStage(t, runtime, "instance", true, "the running yard needs no second temporary start")
		assertStage(t, runtime, "provision", false, "the retry retires again")
		assertStage(t, runtime, ports.ReconcileStageFinalize, false, "the fence still requires finalize")
	})
	t.Run("the retry retires, publishes proof and restores stopped", func(t *testing.T) {
		stale := `{"state":"stale","actual":"` + strings.Repeat("a", 64) + `","desired":"` + strings.Repeat("b", 64) + `"}`
		runtime, incus := retiredApplyFixture(t, stale)
		root := runtime.RepositoryRoot
		log := filepath.Join(root, "guard.log")
		testkit.WriteFile(t, filepath.Join(root, "scripts/lifecycle-guard.sh"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >>"+log+"\n"), 0o755)
		instance := incus.Instances["test/yard"]
		instance.Config = maps.Clone(fenced)
		incus.Instances["test/yard"] = instance
		incus.Reconcile.Instance = instance
		failed := runtime
		plan, err := failed.IntegrationPlan(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := failed.ApplyIntegrations(context.Background(), plan); err == nil {
			t.Fatal("unconverged retirement succeeded")
		}
		if len(incus.ConfigUpdates) != 0 {
			t.Fatalf("failed retirement changed instance metadata: %+v", incus.ConfigUpdates)
		}
		if application.PowerMetadataReadyForFinalize(incus.Instances["test/yard"], "incusbr0") {
			t.Fatal("fixture lost the fence that requires finalize")
		}

		testkit.WriteFile(t, filepath.Join(root, "next"), []byte(`{"state":"absent","actual":"","desired":""}`), 0o600)
		retried := runtime
		plan, err = retried.IntegrationPlan(context.Background())
		if err != nil || !plan.Changed {
			t.Fatalf("retry plan=%+v err=%v", plan, err)
		}
		if err := retried.ApplyIntegrations(context.Background(), plan); err != nil {
			t.Fatal(err)
		}
		proof := retiredIntegrationMarkerPrefix + "sample"
		after := incus.Instances["test/yard"]
		if after.LocalConfig[proof] != "true" && after.Config[proof] != "true" {
			t.Fatalf("retry published no proof: %+v", incus.ConfigUpdates)
		}
		if err := retried.ApplyStage(context.Background(), ports.ReconcileStageFinalize); err != nil {
			t.Fatal(err)
		}
		guard, err := os.ReadFile(log)
		if err != nil || string(guard) != "stop --reconcile\n" {
			t.Fatalf("finalize lifecycle=%q err=%v", guard, err)
		}
		restored := incus.Instances["test/yard"]
		restored.Status = "Stopped"
		pending, err := retried.pendingRetiredIntegrations(restored)
		if err != nil || len(pending) != 0 || !application.InstanceIntentionallyStopped(restored) ||
			!application.PowerMetadataReadyForFinalize(restored, "incusbr0") {
			t.Fatalf("finalize left pending=%v config=%+v err=%v", pending, restored.Config, err)
		}
	})
}

func TestRetiredProofStaysOutsideIntegrationBinding(t *testing.T) {
	config := map[string]string{"limits.cpu": "2", retiredMarkerKey: "true"}
	bound := cloneIntegrationConfig(config, false)
	if _, present := bound[retiredMarkerKey]; present || bound["limits.cpu"] != "2" {
		t.Fatalf("bound config=%v", bound)
	}
	if config[retiredMarkerKey] != "true" {
		t.Fatal("binding clone changed the observed config")
	}
}

func TestRetiredIntegrationReminderStopsAtProof(t *testing.T) {
	loaded := config.Loaded{Context: domain.Context{YardName: "example"}, Integrations: config.IntegrationSelection{AllowsCodingTools: true, Retired: []string{"widget"}}}
	metadata, err := profile.DecodeIntegration("widget", []byte(`{"schema_version":1,"retired":true,"lifecycle":{"handler":"retire.py","instance_config_keys":[],"instance_devices":[]}}`))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := config.BuildCatalog(nil, []profile.IntegrationMetadata{metadata})
	if err != nil {
		t.Fatal(err)
	}
	loaded.Catalog = catalog
	instance := ports.InstanceInfo{Status: "Stopped", Config: map[string]string{}}
	want := "retired integration widget may still be installed in yard example; run: yard -Y example init"
	if got := RetiredIntegrationReminder(loaded, instance); got != want {
		t.Fatalf("reminder=%q", got)
	}
	instance.Config[retiredMarkerKey] = "true"
	if got := RetiredIntegrationReminder(loaded, instance); got != "" {
		t.Fatalf("proven reminder=%q", got)
	}
	delete(instance.Config, retiredMarkerKey)
	loaded.Integrations.AllowsCodingTools = false
	if got := RetiredIntegrationReminder(loaded, instance); got != "" {
		t.Fatalf("forbidden role reminder=%q", got)
	}
	loaded.Integrations.AllowsCodingTools = true
	loaded.Integrations.Retired = nil
	if got := RetiredIntegrationReminder(loaded, instance); got != "" {
		t.Fatalf("ordinary reminder=%q", got)
	}
}

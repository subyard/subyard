package application

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func validPowerMetadata() map[string]string {
	return map[string]string{
		"boot.autostart":             "false",
		"user.subyard.managed":       "true",
		"user.subyard.name":          "default",
		"user.subyard.bridge":        "incusbr0",
		"user.subyard.desired_power": PowerRunning,
		"user.subyard.initialized":   "true",
	}
}

func TestPowerMetadataConvergencePhaseMatrix(t *testing.T) {
	yard := domain.Context{
		YardName: "default", IncusProject: "subyard", YardInstanceName: "yard", IncusBridge: "incusbr0",
	}
	tests := []struct {
		name    string
		key     string
		value   string
		delete  bool
		want    bool
		wantErr bool
	}{
		{name: "valid", want: true},
		{name: "managed absent", key: "user.subyard.managed", delete: true},
		{name: "managed empty", key: "user.subyard.managed"},
		{name: "managed false", key: "user.subyard.managed", value: "false", wantErr: true},
		{name: "initialized false", key: "user.subyard.initialized", value: "false", want: true},
		{name: "initialized absent", key: "user.subyard.initialized", delete: true, wantErr: true},
		{name: "initialized empty", key: "user.subyard.initialized", wantErr: true},
		{name: "initialized malformed", key: "user.subyard.initialized", value: "yes", wantErr: true},
		{name: "desired absent", key: "user.subyard.desired_power", delete: true, wantErr: true},
		{name: "desired empty", key: "user.subyard.desired_power", wantErr: true},
		{name: "desired malformed", key: "user.subyard.desired_power", value: "paused", wantErr: true},
		{name: "name absent", key: "user.subyard.name", delete: true},
		{name: "name mismatch", key: "user.subyard.name", value: "other"},
		{name: "bridge absent", key: "user.subyard.bridge", delete: true},
		{name: "bridge mismatch", key: "user.subyard.bridge", value: "incusbr1"},
		{name: "autostart absent", key: "boot.autostart", delete: true},
		{name: "autostart empty", key: "boot.autostart"},
		{name: "autostart true", key: "boot.autostart", value: "true"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := maps.Clone(validPowerMetadata())
			if test.key != "" {
				if test.delete {
					delete(config, test.key)
				} else {
					config[test.key] = test.value
				}
			}
			converged, err := powerMetadataConverged(ports.InstanceInfo{Config: config}, yard)
			if converged != test.want || (err != nil) != test.wantErr {
				t.Fatalf("converged=%v error=%v, want converged=%v error=%v",
					converged, err, test.want, test.wantErr)
			}
		})
	}
}

func TestPowerServiceImportTreatsMissingAndEmptyManagedAsUnmanaged(t *testing.T) {
	for _, test := range []struct {
		name   string
		config map[string]string
	}{
		{name: "missing", config: map[string]string{}},
		{name: "empty", config: map[string]string{"user.subyard.managed": ""}},
	} {
		t.Run(test.name, func(t *testing.T) {
			incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
				"subyard/yard": {
					Name: "yard", Project: "subyard", Status: "Stopped", Config: test.config,
				},
			}}
			service := PowerService{Instances: incus, Config: incus}
			intent, err := service.Ensure(context.Background(), domain.Context{
				YardName: "default", IncusProject: "subyard", YardInstanceName: "yard",
			})
			if err != nil || !intent.Imported || intent.Desired != PowerStopped {
				t.Fatalf("intent=%#v error=%v", intent, err)
			}
		})
	}
}

func TestPowerServiceWriterShapesUseEffectiveConfigAndPreserveUnownedKeys(t *testing.T) {
	const unownedKey = "user.fixture.keep"
	effective := maps.Clone(validPowerMetadata())
	effective[unownedKey] = "keep"
	incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
		"subyard/yard": {
			Name: "yard", Project: "subyard", Status: "Running",
			Config: effective,
			LocalConfig: map[string]string{
				"user.subyard.managed": "false",
				unownedKey:             "keep",
			},
		},
	}}
	yard := domain.Context{
		YardName: "default", IncusProject: "subyard", YardInstanceName: "yard", IncusBridge: "incusbr0",
	}
	service := PowerService{Instances: incus, Config: incus}
	intent, err := service.Ensure(context.Background(), yard)
	if err != nil || intent.Imported || intent.Desired != PowerRunning {
		t.Fatalf("managed ensure failed: intent=%#v error=%v", intent, err)
	}
	wantPatch := map[string]string{
		"boot.autostart":      "false",
		"user.subyard.name":   "default",
		"user.subyard.bridge": "incusbr0",
	}
	if len(incus.ConfigUpdates) != 1 || !maps.Equal(incus.ConfigUpdates[0].Values, wantPatch) {
		t.Fatalf("managed ensure writer shape = %#v, want %#v", incus.ConfigUpdates, wantPatch)
	}
	if err := service.Set(context.Background(), yard, PowerStopped, false); err != nil {
		t.Fatal(err)
	}
	wantFull := map[string]string{
		"boot.autostart":             "false",
		"user.subyard.managed":       "true",
		"user.subyard.name":          "default",
		"user.subyard.bridge":        "incusbr0",
		"user.subyard.desired_power": PowerStopped,
		"user.subyard.initialized":   "false",
	}
	if len(incus.ConfigUpdates) != 2 || !maps.Equal(incus.ConfigUpdates[1].Values, wantFull) {
		t.Fatalf("full writer shape = %#v, want %#v", incus.ConfigUpdates, wantFull)
	}
	instance := incus.Instances["subyard/yard"]
	if instance.LocalConfig[unownedKey] != "keep" || instance.Config[unownedKey] != "keep" {
		t.Fatalf("unowned config was not preserved: %#v", instance)
	}
}

func TestPowerServiceImportsAndCommitsAtomically(t *testing.T) {
	incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
		"subyard/yard": {Name: "yard", Project: "subyard", Status: "Stopped"},
	}}
	yard := domain.Context{
		YardName: "default", IncusProject: "subyard", YardInstanceName: "yard", IncusBridge: "incusbr0",
	}
	service := PowerService{Instances: incus, Config: incus}
	intent, err := service.Ensure(context.Background(), yard)
	if err != nil || !intent.Imported || intent.Desired != PowerStopped {
		t.Fatalf("power import failed: intent=%#v err=%v", intent, err)
	}
	if err := service.Commit(context.Background(), yard, PowerRunning); err != nil {
		t.Fatal(err)
	}
	instance := incus.Instances["subyard/yard"]
	if instance.Config["user.subyard.desired_power"] != PowerRunning ||
		instance.Config["user.subyard.initialized"] != "true" ||
		instance.Config["boot.autostart"] != "false" {
		t.Fatalf("power commit did not converge metadata: %#v", instance.Config)
	}
}

func TestInitialPowerAndInitFence(t *testing.T) {
	if InitialPower(domain.Context{}) != PowerRunning ||
		InitialPower(domain.Context{YardName: "build"}) != PowerStopped ||
		InitialPower(domain.Context{YardName: "test-lab", NestedE2EVMs: true}) != PowerRunning {
		t.Fatal("initial power policy drifted")
	}
	incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
		"subyard-build/yard-build": {Name: "yard-build", Project: "subyard-build", Status: "Running"},
	}}
	yard := domain.Context{
		YardName: "build", IncusProject: "subyard-build", YardInstanceName: "yard-build",
	}
	service := PowerService{Instances: incus, Config: incus}
	if err := service.Set(context.Background(), yard, PowerStopped, false); err != nil {
		t.Fatal(err)
	}
	config := incus.Instances["subyard-build/yard-build"].Config
	if config["user.subyard.desired_power"] != PowerStopped ||
		config["user.subyard.initialized"] != "false" {
		t.Fatalf("init fence did not preserve named-yard intent: %#v", config)
	}
}

func TestLifecycleRunnerCommitsOnlyAfterPhysicalSuccess(t *testing.T) {
	yard := domain.Context{YardName: "default", IncusProject: "subyard", YardInstanceName: "yard"}
	newIncus := func() *testkit.Incus {
		return &testkit.Incus{Instances: map[string]ports.InstanceInfo{"subyard/yard": {
			Name: "yard", Project: "subyard", Status: "Stopped", Config: map[string]string{
				"user.subyard.managed": "true", "user.subyard.initialized": "true",
				"user.subyard.desired_power": PowerStopped, "user.subyard.name": "default",
				"user.subyard.bridge": "incusbr0", "boot.autostart": "false",
			},
		}}}
	}
	request := domain.AdapterRequest{
		Schema: 1, OperationID: "operation-lifecycle", Adapter: "lifecycle", Action: "start",
	}

	failedIncus := newIncus()
	failed := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Err: errors.New("guard failed")}}}
	runner := LifecycleRunner{
		Power: PowerService{Instances: failedIncus, Config: failedIncus}, Physical: failed, Yard: yard,
	}
	if _, _, err := runner.Run(context.Background(), request, nil); err == nil {
		t.Fatal("failed physical start committed desired power")
	}
	if got := failedIncus.Instances["subyard/yard"].Config["user.subyard.desired_power"]; got != PowerStopped {
		t.Fatalf("failed start changed desired power to %q", got)
	}

	okIncus := newIncus()
	physical := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{
		Schema: 1, OperationID: request.OperationID, Status: "ok",
	}}}}
	runner = LifecycleRunner{
		Power: PowerService{Instances: okIncus, Config: okIncus}, Physical: physical, Yard: yard,
	}
	result, _, err := runner.Run(context.Background(), request, nil)
	if err != nil || result.Output["desiredPower"] != PowerRunning {
		t.Fatalf("successful lifecycle failed: result=%#v err=%v", result, err)
	}
	if got := okIncus.Instances["subyard/yard"].Config["user.subyard.desired_power"]; got != PowerRunning {
		t.Fatalf("successful start did not commit desired power: %q", got)
	}
	if len(physical.Requests) != 1 || len(physical.Requests[0].Arguments) != 1 ||
		physical.Requests[0].Arguments[0] != "start" {
		t.Fatalf("physical adapter did not receive typed start: %#v", physical.Requests)
	}
}

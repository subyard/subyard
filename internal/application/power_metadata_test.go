package application

import (
	"maps"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

func TestParsePowerMetadataGrammar(t *testing.T) {
	booleans := map[string]bool{"true": true, "false": false}
	invalidBoolean := []string{"yes", "TRUE", "False", " true", "false ", "\t"}
	testPowerFieldGrammar(t, powerManagedKey, func(m powerMetadata) metadataField[bool] { return m.Managed }, booleans, invalidBoolean)
	testPowerFieldGrammar(t, powerInitializedKey, func(m powerMetadata) metadataField[bool] { return m.Initialized }, booleans, invalidBoolean)
	testPowerFieldGrammar(t, powerAutostartKey, func(m powerMetadata) metadataField[bool] { return m.Autostart }, booleans, invalidBoolean)
	testPowerFieldGrammar(t, powerDesiredKey, func(m powerMetadata) metadataField[domain.DesiredPower] { return m.Desired },
		map[string]domain.DesiredPower{"running": domain.PowerRunning, "stopped": domain.PowerStopped},
		[]string{"paused", "RUNNING", "Stopped", " running", "stopped ", "\n"})
	opaque := map[string]string{"yard": "yard", " other/yard\n": " other/yard\n", "\t": "\t", "false": "false"}
	testPowerFieldGrammar(t, powerYardNameKey, func(m powerMetadata) metadataField[string] { return m.YardName }, opaque, nil)
	testPowerFieldGrammar(t, powerBridgeKey, func(m powerMetadata) metadataField[string] { return m.Bridge }, opaque, nil)
}

func testPowerFieldGrammar[T comparable](t *testing.T, key string, selectField func(powerMetadata) metadataField[T], valid map[string]T, invalid []string) {
	t.Helper()
	t.Run(key, func(t *testing.T) {
		var zero T
		// Local config never supplies a missing effective field, even with valid local metadata.
		instance := ports.InstanceInfo{LocalConfig: validPowerMetadata()}
		if got := selectField(parsePowerMetadata(instance)); got != (metadataField[T]{State: fieldAbsent}) {
			t.Fatalf("absent field = %#v", got)
		}
		instance.Config = map[string]string{key: "", "unknown": "ignored"}
		if got := selectField(parsePowerMetadata(instance)); got != (metadataField[T]{State: fieldEmpty}) {
			t.Fatalf("empty field = %#v", got)
		}
		for raw, value := range valid {
			instance.Config[key] = raw
			if got := selectField(parsePowerMetadata(instance)); got != (metadataField[T]{State: fieldValid, Raw: raw, Value: value}) {
				t.Errorf("valid %q = %#v, want value %#v", raw, got, value)
			}
		}
		for _, raw := range invalid {
			instance.Config[key] = raw
			if got := selectField(parsePowerMetadata(instance)); got != (metadataField[T]{State: fieldInvalid, Raw: raw, Value: zero}) {
				t.Errorf("invalid %q = %#v", raw, got)
			}
		}
	})
}

func TestParsePowerMetadataFieldsRemainIndependent(t *testing.T) {
	metadata := parsePowerMetadata(ports.InstanceInfo{Config: map[string]string{
		powerManagedKey: "TRUE", powerInitializedKey: "", powerDesiredKey: "paused",
		powerYardNameKey: " other yard ", powerAutostartKey: "true",
	}})
	if metadata.Managed.State != fieldInvalid || metadata.Initialized.State != fieldEmpty ||
		metadata.Desired.State != fieldInvalid || metadata.Bridge.State != fieldAbsent ||
		metadata.YardName.State != fieldValid || metadata.YardName.Value != " other yard " ||
		metadata.Autostart.State != fieldValid || !metadata.Autostart.Value {
		t.Fatalf("independent fields = %#v", metadata)
	}
}

func TestPowerPhasesIgnoreUnusedInvalidFields(t *testing.T) {
	instance := ports.InstanceInfo{Project: "p", Name: "yard", Status: "Stopped", Config: validPowerMetadata()}
	instance.Config[powerDesiredKey] = string(domain.PowerStopped)
	instance.Config[powerAutostartKey] = "invalid"
	delete(instance.Config, powerBridgeKey)
	if !InstanceIntentionallyStopped(instance) {
		t.Fatal("stopped shortcut consumed unused autostart or bridge")
	}
	instance.Config[powerInitializedKey] = "invalid"
	if !ProjectHookStoppedIntentPreserved(instance) {
		t.Fatal("post-write project hook check consumed initialized fence or other unused fields")
	}
	instance.Config[powerManagedKey] = ""
	if converged, err := powerMetadataConverged(instance, domain.Context{}); converged || err != nil {
		t.Fatalf("unmanaged convergence consumed invalid fields: converged=%v err=%v", converged, err)
	}
	if managed, err := filterManagedInstances([]ports.InstanceInfo{instance}, true); len(managed) != 0 || err != nil {
		t.Fatalf("boot filter consumed invalid unmanaged fields: managed=%#v err=%v", managed, err)
	}
	// Boot validation ignores managed/name; filtering owns managed classification.
	instance.Config = maps.Clone(validPowerMetadata())
	instance.Config[powerManagedKey] = "invalid"
	delete(instance.Config, powerYardNameKey)
	if err := validateManagedPower(instance); err != nil {
		t.Fatalf("boot validation consumed unused managed/name: %v", err)
	}
	instance.Config[powerManagedKey] = "true"
	if !PowerMetadataReadyForFinalize(instance, "incusbr0") {
		t.Fatal("runtime finalization consumed unused yard name")
	}
}

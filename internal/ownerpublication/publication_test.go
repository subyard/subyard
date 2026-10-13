package ownerpublication

import (
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/netip"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/ports"
)

func TestPublicationUnitEnvelopeHasSeparateClosedVersionBoundary(t *testing.T) {
	contract := Contract{Device: "retired", Key: "user.subyard.retired", Codec: "dashboard", GuestPort: 3000, CleanupOnly: true}
	encoded, err := Encode([]Contract{contract})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"version":2,"readerEpoch":2`) {
		t.Fatal("unit schema boundary confused durable reader epoch")
	}
	decoded, err := Decode(encoded)
	if err != nil || len(decoded) != 1 || !decoded[0].CleanupOnly || decoded[0].Owns == nil {
		t.Fatalf("installed cleanup contract lost: %v %v", decoded, err)
	}
	historical, _ := Encode(CoreContracts())
	raw, _ := base64.StdEncoding.DecodeString(historical)
	v1 := strings.Replace(string(raw), `"version":2`, `"version":1`, 1)
	if _, err := Decode(base64.StdEncoding.EncodeToString([]byte(v1))); err != nil {
		t.Fatal("historical closed snapshot rejected", err)
	}
	for _, invalid := range []string{
		strings.Replace(string(payload), `"version":2`, `"version":1`, 1),
		strings.Replace(strings.Replace(string(payload), `"version":2`, `"version":1`, 1), `"cleanupOnly":true`, `"cleanupOnly":false`, 1),
		strings.Replace(string(payload), `"version":2`, `"version":3`, 1),
	} {
		if _, err := Decode(base64.StdEncoding.EncodeToString([]byte(invalid))); err == nil {
			t.Fatal("unsupported unit envelope accepted")
		}
	}
}

func syntheticContract() (Contract, map[string]string) {
	device := map[string]string{"type": "proxy", "bind": "host", "listen": "tcp:192.0.2.12:8080", "connect": "tcp:127.0.0.1:80"}
	return Contract{Device: "optional", Key: "user.fixture.owned", Owns: func(marker string, actual map[string]string) bool {
		return marker == "exact-v1" && maps.Equal(actual, device)
	}}, device
}
func TestExactDeferredPublicationLifecycle(t *testing.T) {
	contract, device := syntheticContract()
	instance := ports.InstanceInfo{LocalConfig: map[string]string{contract.Key: "exact-v1", "foreign": "preserved"}, LocalDevices: map[string]map[string]string{contract.Device: device, "local": {"type": "proxy", "listen": "tcp:127.0.0.1:2222"}}, Devices: map[string]map[string]string{contract.Device: device}}
	config, devices, waiting, err := Plan(instance, []Contract{contract}, nil)
	if err != nil || len(waiting) != 1 || devices[contract.Device] != nil || config["foreign"] != "preserved" || devices["local"] == nil {
		t.Fatalf("detach: %v %v %v %v", config, devices, waiting, err)
	}
	if instance.LocalDevices[contract.Device] == nil || instance.LocalConfig[Prefix+contract.Device] != "" {
		t.Fatal("planner mutated observation")
	}
	instance.LocalConfig, instance.LocalDevices, instance.Devices = config, devices, devices
	again, unchanged, pending, err := Plan(instance, []Contract{contract}, nil)
	if err != nil || !maps.Equal(config, again) || !reflect.DeepEqual(devices, unchanged) || !reflect.DeepEqual(waiting, pending) {
		t.Fatal("repeated deferral changed desired state")
	}
	config, devices, waiting, err = Plan(instance, []Contract{contract}, []netip.Addr{netip.MustParseAddr("192.0.2.12")})
	if err != nil || len(waiting) != 0 || config[Prefix+contract.Device] != "" || !maps.Equal(devices[contract.Device], device) {
		t.Fatalf("restore: %v %v %v %v", config, devices, waiting, err)
	}
	// A second disappearance retains the same exact route, never a wildcard.
	instance.LocalConfig, instance.LocalDevices, instance.Devices = config, devices, devices
	_, devices, waiting, err = Plan(instance, []Contract{contract}, nil)
	if err != nil || len(waiting) != 1 || devices[contract.Device] != nil {
		t.Fatal("second disappearance did not defer")
	}
}
func TestDeferredPublicationRejectsContradictoryRecovery(t *testing.T) {
	contract, device := syntheticContract()
	payload, _ := json.Marshal(Receipt{Version: 1, Ownership: "exact-v1", Device: device})
	for _, scenario := range []string{"version", "ownership", "saved device", "foreign device", "inherited device", "unknown contract", "trailing JSON"} {
		t.Run(scenario, func(t *testing.T) {
			instance := ports.InstanceInfo{LocalConfig: map[string]string{contract.Key: "exact-v1", Prefix + contract.Device: string(payload)}, LocalDevices: map[string]map[string]string{}, Devices: map[string]map[string]string{}}
			contracts := []Contract{contract}
			switch scenario {
			case "version":
				instance.LocalConfig[Prefix+contract.Device] = `{"version":2}`
			case "ownership":
				instance.LocalConfig[contract.Key] = "changed"
			case "saved device":
				instance.LocalConfig[Prefix+contract.Device] = `{"version":1,"ownership":"exact-v1","device":{"type":"proxy"}}`
			case "foreign device":
				instance.Devices[contract.Device] = map[string]string{"type": "disk"}
				instance.LocalDevices[contract.Device] = instance.Devices[contract.Device]
			case "inherited device":
				instance.Devices[contract.Device] = device
			case "unknown contract":
				contracts = nil
			case "trailing JSON":
				instance.LocalConfig[Prefix+contract.Device] += " {}"
			}
			if _, _, _, err := Plan(instance, contracts, []netip.Addr{netip.MustParseAddr("192.0.2.12")}); err == nil {
				t.Fatal("contradictory recovery accepted")
			}
		})
	}
}

func TestCanceledPublicationCannotRestoreWhenAddressReturns(t *testing.T) {
	contract, device := syntheticContract()
	instance := ports.InstanceInfo{LocalConfig: map[string]string{contract.Key: "exact-v1", "user.subyard.desired_power": "running"},
		LocalDevices: map[string]map[string]string{contract.Device: device}, Devices: map[string]map[string]string{contract.Device: device}}
	config, devices, _, err := Plan(instance, []Contract{contract}, nil)
	if err != nil || config[Prefix+contract.Device] == "" {
		t.Fatalf("detach: %v", err)
	}
	// The owning cancellation removes both its journal and receipt, even when
	// the listener is already detached and there is no running guest service.
	delete(config, Prefix+contract.Device)
	delete(config, contract.Key)
	instance.LocalConfig, instance.LocalDevices, instance.Devices = config, devices, devices
	for range 2 {
		actualConfig, actualDevices, waiting, err := Plan(instance, []Contract{contract}, []netip.Addr{netip.MustParseAddr("192.0.2.12")})
		if err != nil || len(waiting) != 0 || !maps.Equal(config, actualConfig) || !reflect.DeepEqual(devices, actualDevices) || actualDevices[contract.Device] != nil {
			t.Fatalf("canceled listener resurrected: %v %v %v %v", actualConfig, actualDevices, waiting, err)
		}
	}
}

func TestCleanupOnlyRetiredPublicationCancelsAndNeverRestores(t *testing.T) {
	contract, device := syntheticContract()
	contract.CleanupOnly = true
	payload, _ := json.Marshal(Receipt{Version: 1, Ownership: "exact-v1", Device: device})
	for _, present := range []bool{false, true} {
		instance := ports.InstanceInfo{LocalConfig: map[string]string{contract.Key: "exact-v1", Prefix + contract.Device: string(payload), "foreign": "preserved"}, LocalDevices: map[string]map[string]string{}, Devices: map[string]map[string]string{}}
		if present {
			instance.LocalDevices[contract.Device] = device
			instance.Devices[contract.Device] = device
		}
		config, devices, waiting, err := Plan(instance, []Contract{contract}, []netip.Addr{netip.MustParseAddr("192.0.2.12")})
		if err != nil || len(waiting) != 0 || config[Prefix+contract.Device] != "" || devices[contract.Device] != nil || config[contract.Key] != "exact-v1" || config["foreign"] != "preserved" {
			t.Fatalf("retired restore authority survived: %v %v %v %v", config, devices, waiting, err)
		}
		instance.LocalConfig, instance.LocalDevices, instance.Devices = config, devices, devices
		if _, again, _, err := Plan(instance, []Contract{contract}, []netip.Addr{netip.MustParseAddr("192.0.2.12")}); err != nil || again[contract.Device] != nil {
			t.Fatal("retired publication resurrected")
		}
	}
}

func TestCleanupOnlyRetiredPublicationRejectsNonDeferredListener(t *testing.T) {
	contract, device := syntheticContract()
	contract.CleanupOnly = true
	device["listen"] = "tcp:127.0.0.1:8080"
	payload, _ := json.Marshal(Receipt{Version: 1, Ownership: "exact-v1", Device: device})
	instance := ports.InstanceInfo{LocalConfig: map[string]string{contract.Key: "exact-v1", Prefix + contract.Device: string(payload)}}
	if _, _, _, err := Plan(instance, []Contract{contract}, nil); err == nil {
		t.Fatal("retirement accepted a journal that could never be deferred")
	}
}
func TestUnownedAndInheritedPublicationRetainsRequiredDevice(t *testing.T) {
	contract, device := syntheticContract()
	for _, marker := range []string{"", "foreign"} {
		instance := ports.InstanceInfo{LocalConfig: map[string]string{contract.Key: marker}, LocalDevices: map[string]map[string]string{contract.Device: device}, Devices: map[string]map[string]string{contract.Device: device}}
		config, devices, waiting, err := Plan(instance, []Contract{contract}, nil)
		if err != nil || len(waiting) != 0 || !maps.Equal(devices[contract.Device], device) || config[Prefix+contract.Device] != "" {
			t.Fatal("unowned device detached")
		}
	}
}

func TestPublicationContractSnapshotRejectsUnknownReaderState(t *testing.T) {
	encoded, err := Encode(CoreContracts())
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(encoded)
	if err != nil || len(decoded) != 1 {
		t.Fatalf("snapshot: %v %v", decoded, err)
	}
	if _, err := Decode("invalid"); err == nil {
		t.Fatal("invalid snapshot accepted")
	}
	contract := CoreContracts()[0]
	contract.Codec = "future"
	encoded, _ = Encode([]Contract{contract})
	if _, err := Decode(encoded); err == nil {
		t.Fatal("unknown codec accepted")
	}
	encoded, _ = Encode(append(CoreContracts(), CoreContracts()...))
	if _, err := Decode(encoded); err == nil {
		t.Fatal("duplicate ownership accepted")
	}
}

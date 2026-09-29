package cli

import (
	"maps"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

func TestStartupSeedOnlyArmsCleanProvisionedResource(t *testing.T) {
	definition := resource.Definition{Command: "service", Proxy: &resource.ProxyContract{
		Device: "service-port", AdvertiseHostSetting: "SERVICE_IPV4", HostPortSetting: "SERVICE_PORT",
		Connect: "udp:guest:51820", AddressPolicy: resource.ProxyAddressOwnerIPv4UDP,
	}}
	loaded := config.Loaded{Environment: map[string]string{"SERVICE_IPV4": "10.20.30.40", "SERVICE_PORT": "51820"}}
	device := map[string]string{"type": "proxy", "bind": "host", "nat": "true",
		"listen": "udp:10.20.30.40:51820", "connect": "udp:10.80.0.10:51820"}
	owned := func() ports.InstanceInfo {
		marker := definition.Proxy.OwnershipValue(device)
		return ports.InstanceInfo{
			LocalDevices: map[string]map[string]string{definition.Proxy.Device: maps.Clone(device)},
			Devices: map[string]map[string]string{definition.Proxy.Device: maps.Clone(device),
				"eth0": {"ipv4.address": "10.80.0.10"}},
			LocalConfig: map[string]string{definition.Proxy.OwnershipKey(): marker},
			Config:      map[string]string{definition.Proxy.OwnershipKey(): marker},
		}
	}
	for _, test := range []struct {
		name         string
		makeInstance func() ports.InstanceInfo
		eligible     bool
		bad          bool
	}{
		{name: "new clean resource", makeInstance: func() ports.InstanceInfo {
			return ports.InstanceInfo{Devices: map[string]map[string]string{"eth0": {"ipv4.address": "10.80.0.10"}}}
		}, eligible: true},
		{name: "legacy owned active route", makeInstance: owned},
		{name: "explicit down stays disabled", makeInstance: func() ports.InstanceInfo {
			instance := ports.InstanceInfo{LocalConfig: map[string]string{}, Config: map[string]string{}}
			key := startupIntentKey(definition)
			instance.LocalConfig[key], instance.Config[key] = startupDisabled, startupDisabled
			return instance
		}},
		{name: "partial marker", makeInstance: func() ports.InstanceInfo {
			instance := owned()
			delete(instance.LocalDevices, definition.Proxy.Device)
			delete(instance.Devices, definition.Proxy.Device)
			return instance
		}, bad: true},
		{name: "inherited route", makeInstance: func() ports.InstanceInfo {
			instance := owned()
			delete(instance.LocalDevices, definition.Proxy.Device)
			delete(instance.LocalConfig, definition.Proxy.OwnershipKey())
			return instance
		}, bad: true},
		{name: "changed endpoint with matching fingerprint", makeInstance: func() ports.InstanceInfo {
			instance := owned()
			instance.LocalDevices[definition.Proxy.Device]["listen"] = "udp:10.20.30.41:51820"
			instance.Devices[definition.Proxy.Device]["listen"] = "udp:10.20.30.41:51820"
			marker := definition.Proxy.OwnershipValue(instance.LocalDevices[definition.Proxy.Device])
			instance.LocalConfig[definition.Proxy.OwnershipKey()] = marker
			instance.Config[definition.Proxy.OwnershipKey()] = marker
			return instance
		}, bad: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			eligible, err := startupSeedEligible(loaded, test.makeInstance(), definition)
			if eligible != test.eligible || (err != nil) != test.bad {
				t.Fatalf("eligible=%v error=%v", eligible, err)
			}
		})
	}
}

func TestStartupIntentRequiresLocalRecognizedState(t *testing.T) {
	definition := resource.Definition{Command: "service", Proxy: &resource.ProxyContract{Device: "service-port"}}
	key := startupIntentKey(definition)
	for _, test := range []struct {
		name      string
		local     map[string]string
		effective map[string]string
		want      string
		bad       bool
	}{
		{name: "unprovisioned", local: map[string]string{}, effective: map[string]string{}},
		{name: "pending", local: map[string]string{key: startupPending}, effective: map[string]string{key: startupPending}, want: startupPending},
		{name: "disabled", local: map[string]string{key: startupDisabled}, effective: map[string]string{key: startupDisabled}, want: startupDisabled},
		{name: "inherited", local: map[string]string{}, effective: map[string]string{key: startupPending}, bad: true},
		{name: "conflicting", local: map[string]string{key: startupPending}, effective: map[string]string{key: startupEnabled}, bad: true},
		{name: "invalid", local: map[string]string{key: "surprise"}, effective: map[string]string{key: "surprise"}, bad: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := startupIntent(ports.InstanceInfo{LocalConfig: test.local, Config: test.effective}, definition)
			if (err != nil) != test.bad || got != test.want {
				t.Fatalf("state=%q error=%v", got, err)
			}
		})
	}
}

func TestStartupIngressAcceptsPowerObservationOnly(t *testing.T) {
	yard := yardnetwork.Yard{Name: "vpn", Project: "p", Instance: "vm", Network: "br"}
	before := yardnetwork.IngressPreview{
		Target: yard,
		Before: yardnetwork.Plan{Policy: yardnetwork.Policy{Revision: 1}, Fingerprint: "stopped"},
		After: yardnetwork.Plan{Policy: yardnetwork.Policy{Revision: 2}, Changed: true, Physical: true,
			Updates: []yardnetwork.Update{{Yard: yardnetwork.ObservedYard{Yard: yard},
				ACL: yardnetwork.ACL{Exists: true}, Access: "br", NIC: map[string]string{"ipv4.address": "10.0.0.2"}}}},
	}
	after := before
	after.Before.Fingerprint = "running"
	after.After.Fingerprint = "running-after"
	after.After.Updates = append([]yardnetwork.Update(nil), before.After.Updates...)
	after.After.Updates[0].Yard.InstanceInfo.Status = "Running"
	if !sameStartupIngressEffects(before, after) {
		t.Fatal("power observation changed authorized ingress effects")
	}
	after.After.Updates[0].NIC = map[string]string{"ipv4.address": "10.0.0.3"}
	if sameStartupIngressEffects(before, after) {
		t.Fatal("changed guest address was accepted")
	}
}

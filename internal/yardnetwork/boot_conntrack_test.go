package yardnetwork

import (
	"context"
	"maps"
	"net/netip"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/resource"
)

func TestBootUDPRecoveryNeedsExactOwnedRoute(t *testing.T) {
	host := newMemoryHost()
	observed := &host.snapshot.Yards[0]
	observed.InstanceInfo.Type = domain.YardVM
	device := map[string]string{"type": "proxy", "listen": "udp:10.20.30.40:51820",
		"connect": "udp:10.80.0.10:51820", "bind": "host", "nat": "true"}
	observed.InstanceInfo.LocalDevices = map[string]map[string]string{"relay": maps.Clone(device)}
	observed.InstanceInfo.Devices = map[string]map[string]string{"relay": maps.Clone(device),
		"eth0": {"type": "nic", "ipv4.address": "10.80.0.10"}}
	marker := (resource.ProxyContract{}).OwnershipValue(device)
	observed.InstanceInfo.LocalConfig = map[string]string{"user.subyard.resource.relay": marker}
	service := memoryService(host)
	service.UseApprovedIngress = true
	var endpoints []netip.AddrPort
	service.ClearStaleUDP = func(_ context.Context, endpoint netip.AddrPort) error {
		endpoints = append(endpoints, endpoint)
		return nil
	}
	check := func(want int, wantErr bool) {
		t.Helper()
		endpoints = nil
		err := service.ClearBootStaleUDP(context.Background(), observed.Yard)
		if (err != nil) != wantErr || len(endpoints) != want {
			t.Fatalf("endpoints=%v error=%v, want %d endpoints error=%t", endpoints, err, want, wantErr)
		}
		if want != 0 && endpoints[0] != netip.MustParseAddrPort("10.20.30.40:51820") {
			t.Fatalf("wrong verified endpoint: %v", endpoints)
		}
	}
	check(1, false) // isolation off: final local/effective route and marker suffice
	observed.InstanceInfo.Status = "Stopped"
	check(0, true) // a concurrent stop invalidates the fresh route
	observed.InstanceInfo.Status = "Running"
	observed.InstanceFound = false
	check(0, true) // a concurrent removal invalidates it too
	observed.InstanceFound = true
	delete(observed.InstanceInfo.LocalConfig, "user.subyard.resource.relay")
	check(0, false) // ordinary proxy
	observed.InstanceInfo.LocalConfig["user.subyard.resource.relay"] = "v1:pending:" + marker[3:]
	check(0, false) // shutdown still pending
	observed.InstanceInfo.LocalConfig["user.subyard.resource.relay"] = marker
	observed.InstanceInfo.Devices["relay"]["nat"] = "false"
	check(0, true) // effective device differs
	observed.InstanceInfo.Devices["relay"]["nat"] = "true"
	observed.InstanceInfo.Devices["eth0"]["ipv4.address"] = ""
	check(0, false) // unpinned VM
	observed.InstanceInfo.Devices["eth0"]["ipv4.address"] = "10.80.0.10"

	approved := ApprovedIngress{Device: "relay", Listen: device["listen"], GuestPort: 51820}
	host.stored.Policy.Isolation, host.stored.Policy.AppliedIsolation = true, true
	host.stored.Policy.Bindings = []Binding{{Yard: observed.Yard, IPv4: "10.80.0.10",
		MAC: "00:16:3e:00:00:10", OriginalNIC: map[string]string{"type": "nic", "network": "incusbr0"},
		ApprovedIngress: []ApprovedIngress{approved}}}
	check(1, false)
	host.stored.Policy.Bindings[0].ApprovedIngress = nil
	check(0, false) // no authority may be inferred from an owned device
	host.stored.Policy.Bindings[0].ApprovedIngress = []ApprovedIngress{approved}
	observed.InstanceInfo.LocalConfig["user.subyard.resource.relay"] = "v1:wrong"
	check(0, true)
	observed.InstanceInfo.LocalConfig["user.subyard.resource.relay"] = marker
	observed.InstanceInfo.Devices["eth0"]["ipv4.address"] = ""
	check(0, true) // an approved isolated route cannot lose its pin
}

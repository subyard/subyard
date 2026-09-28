package incusclient

import (
	"context"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/yardnetwork"
)

func TestPinVMIPv4UsesGuestLeaseAndProfileETag(t *testing.T) {
	fake := vmNetworkFixture(t)
	yard := yardnetwork.Yard{Name: "vpn", Project: "project-vpn", Instance: "yard-vpn", Network: "incusbr0"}
	client := New(fake.socket, "projects")

	pinned, err := client.PinVMIPv4(context.Background(), yard, false)
	if err != nil || pinned {
		t.Fatalf("unreserved address = pinned %t, err %v", pinned, err)
	}
	pinned, err = client.PinVMIPv4(context.Background(), yard, true)
	if err != nil || !pinned {
		t.Fatalf("first pin = pinned %t, err %v", pinned, err)
	}
	profile := fake.profiles["project-vpn/default"]
	if fake.profileIfMatch != "profile-v1" || profile.Devices["eth0"]["ipv4.address"] != "10.44.0.20" ||
		profile.Devices["root"]["pool"] != "default" {
		t.Fatalf("profile pin lost identity or used wrong ETag: %+v, If-Match %q", profile, fake.profileIfMatch)
	}
	pinned, err = client.PinVMIPv4(context.Background(), yard, true)
	if err != nil || !pinned {
		t.Fatalf("repeat validation = pinned %t, err %v", pinned, err)
	}
	if fake.profileIfMatch != "profile-v1" {
		t.Fatal("repeat validation changed the existing pin")
	}
}

func TestPinVMIPv4MatchesGuestNICByMACAndIgnoresIPv6Lease(t *testing.T) {
	fake := vmNetworkFixture(t)
	instance := fake.instances["project-vpn/yard-vpn"]
	instance.State["network"] = map[string]any{
		"ens3": map[string]any{
			"hwaddr": "00:16:3e:aa:bb:01",
			"addresses": []map[string]any{
				{"family": "inet6", "address": "fd42::20", "scope": "global"},
				{"family": "inet", "address": "10.44.0.20", "scope": "global"},
			},
		},
	}
	fake.instances["project-vpn/yard-vpn"] = instance
	fake.leases["project-vpn/incusbr0"] = append(fake.leases["project-vpn/incusbr0"], map[string]any{
		"hostname": "yard-vpn", "hwaddr": "00:16:3e:aa:bb:01", "address": "fd42::20", "type": "dynamic",
	})
	yard := yardnetwork.Yard{Name: "vpn", Project: "project-vpn", Instance: "yard-vpn", Network: "incusbr0"}
	pinned, err := New(fake.socket, "projects").PinVMIPv4(context.Background(), yard, true)
	if err != nil || !pinned {
		t.Fatalf("guest ens3 with IPv4 and IPv6 leases = pinned %t, err %v", pinned, err)
	}
	if got := fake.profiles["project-vpn/default"].Devices["eth0"]["ipv4.address"]; got != "10.44.0.20" {
		t.Fatalf("profile eth0 IPv4 pin = %q", got)
	}
}

func TestPinVMIPv4FailsClosedOnCollisionAndLocalNIC(t *testing.T) {
	fake := vmNetworkFixture(t)
	yard := yardnetwork.Yard{Name: "vpn", Project: "project-vpn", Instance: "yard-vpn", Network: "incusbr0"}
	client := New(fake.socket, "projects")
	fake.leases["default/incusbr0"] = []map[string]any{{
		"hostname": "other", "hwaddr": "00:16:3e:aa:bb:02", "address": "10.44.0.20", "type": "dynamic",
	}}
	if _, err := client.PinVMIPv4(context.Background(), yard, true); err == nil ||
		!strings.Contains(err.Error(), "another reservation owner") {
		t.Fatalf("colliding lease accepted: %v", err)
	}
	delete(fake.leases, "default/incusbr0")
	instance := fake.instances["project-vpn/yard-vpn"]
	instance.Data["devices"] = map[string]map[string]string{
		"eth0": {"type": "nic", "network": "incusbr0"},
	}
	fake.instances["project-vpn/yard-vpn"] = instance
	if _, err := client.PinVMIPv4(context.Background(), yard, true); err == nil ||
		!strings.Contains(err.Error(), "must not override") {
		t.Fatalf("local NIC override accepted: %v", err)
	}
}

func vmNetworkFixture(t *testing.T) *networkIncusServer {
	t.Helper()
	fake := newNetworkIncusServer(t)
	fake.networks["incusbr0"] = fakeNetwork{
		Type: "bridge", Managed: true, Config: map[string]string{"ipv4.address": "10.44.0.1/24"},
	}
	fake.projects["default"] = fakeProject{ETag: "default-v1"}
	fake.projects["project-vpn"] = fakeProject{ETag: "project-v1"}
	fake.profiles["project-vpn/default"] = fakeProfile{
		ETag: "profile-v1", UsedBy: []string{"/1.0/instances/yard-vpn?project=project-vpn"},
		Devices: map[string]map[string]string{
			"root": {"type": "disk", "path": "/", "pool": "default"},
			"eth0": {"type": "nic", "network": "incusbr0"},
		},
	}
	fake.leases["project-vpn/incusbr0"] = []map[string]any{{
		"hostname": "yard-vpn", "hwaddr": "00:16:3e:aa:bb:01", "address": "10.44.0.20", "type": "dynamic",
	}}
	fake.instances["project-vpn/yard-vpn"] = fakeInstance{Data: map[string]any{
		"name": "yard-vpn", "project": "project-vpn", "type": "virtual-machine", "status": "Running",
		"config":          map[string]string{"volatile.eth0.hwaddr": "00:16:3e:aa:bb:01"},
		"expanded_config": map[string]string{"volatile.eth0.hwaddr": "00:16:3e:aa:bb:01"},
		"devices":         map[string]map[string]string{},
		"expanded_devices": map[string]map[string]string{
			"eth0": {"type": "nic", "network": "incusbr0", "name": "eth0"},
		},
	}, State: map[string]any{"status": "Running", "network": map[string]any{
		"eth0": map[string]any{
			"hwaddr":    "00:16:3e:aa:bb:01",
			"addresses": []map[string]any{{"family": "inet", "address": "10.44.0.20", "scope": "global"}},
		},
	}}}
	return fake
}

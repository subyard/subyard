package yardnetwork

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/previewroute"
	"github.com/Subyard/Subyard/internal/resource"
)

func TestBootUsesOnlyAppliedSelectedIngressApproval(t *testing.T) {
	ctx := context.Background()
	host := newMemoryHost()
	normal := memoryService(host)
	contract := resource.ProxyContract{Profile: "sample", Resource: "relay", Device: "sample-relay",
		Connect: "udp:guest:41999", OwnerInterfaceSetting: "SAMPLE_INTERFACE",
		AddressPolicy: resource.ProxyAddressOwnerIPv4UDP, OwnershipMetadata: true}
	selected := true
	normal.ContractSource = func(Yard) ([]resource.ProxyContract, error) {
		if selected {
			return []resource.ProxyContract{contract}, nil
		}
		return nil, nil
	}
	target := &host.snapshot.Yards[0]
	target.InstanceInfo.Type = domain.YardVM
	device := map[string]string{"type": "proxy", "listen": "udp:10.20.30.40:42000",
		"connect": "udp:10.80.0.10:41999", "bind": "host", "nat": "true"}
	target.InstanceInfo.LocalDevices = map[string]map[string]string{contract.Device: maps.Clone(device)}
	target.InstanceInfo.Devices = map[string]map[string]string{contract.Device: maps.Clone(device),
		"eth0": {"type": "nic", "ipv4.address": "10.80.0.10"}}
	target.InstanceInfo.LocalConfig = map[string]string{contract.OwnershipKey(): contract.OwnershipValue(device)}
	on := true
	initial, err := normal.Prepare(ctx, fixtureYards(host), Change{Isolation: &on})
	if err != nil {
		t.Fatal(err)
	}
	if err := normal.Apply(ctx, initial); err != nil {
		t.Fatal(err)
	}
	if approved := host.stored.Policy.Bindings[0].ApprovedIngress; len(approved) != 1 ||
		approved[0].Device != contract.Device || !maps.Equal(approved[0].proxy(host.stored.Policy.Bindings[0]), device) {
		t.Fatalf("isolation apply did not persist selected exact route: %+v", approved)
	}
	boot := memoryService(host)
	boot.UseApprovedIngress = true
	boot.ContractSource = nil
	called := false
	start := func() error { called = true; return nil }
	if err := boot.WithStart(ctx, target.Yard, start); err != nil || !called {
		t.Fatalf("approved route blocked boot: called=%v err=%v", called, err)
	}
	if err := boot.Apply(ctx, initial); err == nil {
		t.Fatal("boot verification mode could mutate policy")
	}
	for _, malformed := range []func(*Policy){
		func(policy *Policy) { policy.Bindings[0].ApprovedIngress[0].Listen = "udp:0.0.0.0:42000" },
		func(policy *Policy) {
			policy.Bindings[0].ApprovedIngress = append(policy.Bindings[0].ApprovedIngress,
				policy.Bindings[0].ApprovedIngress[0])
		},
	} {
		policy := cloneValue(host.stored.Policy)
		malformed(&policy)
		if _, err := Encode(policy); err == nil {
			t.Fatal("malformed or duplicate boot approval was encoded")
		}
	}

	approvedPolicy := cloneValue(host.stored)
	host.stored.Policy.Bindings[0].ApprovedIngress = nil
	called = false
	if err := boot.WithStart(ctx, target.Yard, start); !errors.Is(err, ErrNotConverged) || called {
		t.Fatalf("boot inferred ingress authorization from a proxy without saved approval: called=%v err=%v", called, err)
	}
	host.stored = approvedPolicy
	target.InstanceInfo.Devices["unapproved"] = map[string]string{"type": "proxy", "bind": "host",
		"listen": "udp:0.0.0.0:42002-42003", "connect": "udp:10.80.0.10:41999"}
	called = false
	if err := boot.WithStart(ctx, target.Yard, start); !errors.Is(err, ErrNotConverged) || called {
		t.Fatalf("unapproved second public endpoint booted through approved ACL: called=%v err=%v", called, err)
	}
	target.InstanceInfo.Devices["unapproved"]["bind"] = "instance"
	if err := boot.WithStart(ctx, target.Yard, start); err != nil || !called {
		t.Fatalf("instance-bound proxy was treated as an owner endpoint: called=%v err=%v", called, err)
	}
	delete(target.InstanceInfo.Devices, "unapproved")

	called = false
	target.InstanceInfo.LocalDevices[contract.Device]["listen"] = "udp:10.20.30.41:42000"
	if err := boot.WithStart(ctx, target.Yard, start); !errors.Is(err, ErrNotConverged) || called {
		t.Fatalf("modified endpoint booted: called=%v err=%v", called, err)
	}
	target.InstanceInfo.LocalDevices[contract.Device]["listen"] = device["listen"]
	delete(target.InstanceInfo.LocalConfig, contract.OwnershipKey())
	if err := boot.WithStart(ctx, target.Yard, start); !errors.Is(err, ErrNotConverged) || called {
		t.Fatalf("missing ownership marker booted: called=%v err=%v", called, err)
	}
	target.InstanceInfo.LocalConfig[contract.OwnershipKey()] = contract.OwnershipValue(device)
	target.ACL.Ingress = nil
	if err := boot.WithStart(ctx, target.Yard, start); !errors.Is(err, ErrNotConverged) || called {
		t.Fatalf("missing approved ACL booted: called=%v err=%v", called, err)
	}

	// Ordinary registry authority must remove the approval when the profile is
	// deselected, even while the old proxy still exists.
	selected = false
	plan, err := normal.Prepare(ctx, fixtureYards(host), Change{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Policy.Bindings[0].ApprovedIngress) != 0 || !plan.Changed {
		t.Fatalf("deselection retained boot ingress approval: %+v", plan.Policy.Bindings[0].ApprovedIngress)
	}
}

func TestBootPreservesOnlyAppliedOwnedPreviewIngress(t *testing.T) {
	ctx := context.Background()
	host := newMemoryHost()
	normal := memoryService(host)
	target := &host.snapshot.Yards[0]
	target.InstanceInfo.Type = domain.YardVM
	device := previewroute.Device("192.168.1.20", "32222", "10.80.0.10")
	marker := "v2:192.168.1.20:32222:10.80.0.10"
	target.InstanceInfo.LocalDevices = map[string]map[string]string{previewroute.DeviceName: maps.Clone(device)}
	target.InstanceInfo.Devices = map[string]map[string]string{previewroute.DeviceName: maps.Clone(device), "eth0": {"type": "nic", "ipv4.address": "10.80.0.10"}}
	target.InstanceInfo.LocalConfig = map[string]string{previewroute.Key: marker}
	on := true
	initial, err := normal.Prepare(ctx, fixtureYards(host), Change{Isolation: &on})
	if err != nil {
		t.Fatal(err)
	}
	if err := normal.Apply(ctx, initial); err != nil {
		t.Fatal(err)
	}
	approved := host.stored.Policy.Bindings[0].ApprovedIngress
	if len(approved) != 1 || approved[0].Device != previewroute.DeviceName || approved[0].GuestPort != 8765 || approved[0].Transport() != "tcp" {
		t.Fatalf("preview approval missing: %+v", approved)
	}
	found := false
	for _, rule := range target.ACL.Ingress {
		if rule.Protocol == "tcp" && rule.DestinationPort == "8765" {
			found = true
		}
	}
	if !found {
		t.Fatal("preview ingress is blocked")
	}
	boot := memoryService(host)
	boot.UseApprovedIngress = true
	called := false
	start := func() error { called = true; return nil }
	if err := boot.WithStart(ctx, target.Yard, start); err != nil || !called {
		t.Fatalf("approved preview could not boot: %v", err)
	}
	saved := cloneValue(host.stored)
	host.stored.Policy.Bindings[0].ApprovedIngress = nil
	called = false
	if err := boot.WithStart(ctx, target.Yard, start); !errors.Is(err, ErrNotConverged) || called {
		t.Fatalf("unapproved preview booted: %v", err)
	}
	host.stored = saved
	for _, fault := range []string{"receipt", "target", "pending", "NIC pin"} {
		t.Run(fault, func(t *testing.T) {
			target.InstanceInfo.LocalConfig[previewroute.Key] = marker
			target.InstanceInfo.Devices[previewroute.DeviceName] = maps.Clone(device)
			target.InstanceInfo.Devices["eth0"]["ipv4.address"] = "10.80.0.10"
			switch fault {
			case "receipt":
				delete(target.InstanceInfo.LocalConfig, previewroute.Key)
			case "target":
				target.InstanceInfo.Devices[previewroute.DeviceName]["connect"] = "tcp:10.80.0.11:8765"
			case "pending":
				target.InstanceInfo.LocalConfig[previewroute.Key] = "v2:pending:192.168.1.20:32222:10.80.0.10"
			case "NIC pin":
				target.InstanceInfo.Devices["eth0"]["ipv4.address"] = "10.80.0.11"
			}
			called = false
			if err := boot.WithStart(ctx, target.Yard, start); !errors.Is(err, ErrNotConverged) || called {
				t.Fatalf("divergent preview booted: %v", err)
			}
		})
	}
	delete(target.InstanceInfo.Devices, previewroute.DeviceName)
	delete(target.InstanceInfo.LocalDevices, previewroute.DeviceName)
	delete(target.InstanceInfo.LocalConfig, previewroute.Key)
	target.InstanceInfo.Devices["eth0"]["ipv4.address"] = "10.80.0.10"
	plan, err := normal.Prepare(ctx, fixtureYards(host), Change{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Policy.Bindings[0].ApprovedIngress) != 0 || !plan.Changed {
		t.Fatal("removed preview retained its ACL approval")
	}
}

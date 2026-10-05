package yardnetwork

import (
	"context"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/resource"
)

func TestPublicIngressTransportLifecycleAndBoot(t *testing.T) {
	for _, test := range []struct {
		protocol, port string
		guestPort      int
		shared         bool
	}{
		{"tcp", "22", 22, false}, {"tcp", "host-port", 42000, false}, {"udp", "host-port", 42000, false}, {"tcp", "22", 22, true},
	} {
		t.Run(test.protocol+"/"+test.port+"/shared="+strconv.FormatBool(test.shared), func(t *testing.T) {
			ctx := context.Background()
			host := newMemoryHost()
			service := memoryService(host)
			contract := resource.ProxyContract{Profile: "sample", Resource: "relay", Device: "sample-relay",
				Connect: test.protocol + ":guest:" + test.port, OwnerInterfaceSetting: "SAMPLE_INTERFACE",
				AddressPolicy: resource.ProxyAddressPolicy("owner-ipv4-" + test.protocol), OwnershipMetadata: true}
			// Keep a UDP peer at the same numeric guest port. Closing TCP must preserve
			// its UDP rule; closing UDP must preserve the shared rule and peer approval.
			peer := contract
			peer.Device, peer.Resource, peer.Connect, peer.AddressPolicy = "sample-zpeer", "peer", "udp:guest:"+strconv.Itoa(test.guestPort), resource.ProxyAddressOwnerIPv4UDP
			shared := contract
			shared.Device, shared.Resource = "sample-zzpeer", "shared"
			service.ContractSource = func(Yard) ([]resource.ProxyContract, error) {
				contracts := []resource.ProxyContract{contract, peer}
				if test.shared {
					contracts = append(contracts, shared)
				}
				return contracts, nil
			}
			target := &host.snapshot.Yards[0]
			instance := &target.InstanceInfo
			instance.Type = domain.YardVM
			instance.Devices = map[string]map[string]string{"eth0": {"type": "nic", "ipv4.address": "10.80.0.10"}}
			instance.LocalDevices = map[string]map[string]string{}
			instance.LocalConfig = map[string]string{"volatile.vm.needs_reset": "true"}
			instance.Config = maps.Clone(instance.LocalConfig)
			set := func(c resource.ProxyContract, protocol string, hostPort, guestPort int) {
				device := map[string]string{"type": "proxy", "listen": protocol + ":10.20.30.40:" + strconv.Itoa(hostPort), "connect": protocol + ":10.80.0.10:" + strconv.Itoa(guestPort), "bind": "host", "nat": "true"}
				instance.Devices[c.Device], instance.LocalDevices[c.Device] = maps.Clone(device), maps.Clone(device)
				instance.Config[c.OwnershipKey()], instance.LocalConfig[c.OwnershipKey()] = c.OwnershipValue(device), c.OwnershipValue(device)
			}
			set(peer, "udp", 42001, test.guestPort)
			if test.shared {
				set(shared, "tcp", 42002, test.guestPort)
			}
			on := true
			initial, err := service.Prepare(ctx, fixtureYards(host), Change{Isolation: &on})
			if err != nil {
				t.Fatal(err)
			}
			if err := service.Apply(ctx, initial); err != nil {
				t.Fatal(err)
			}
			preview, err := service.PreviewResourceIngress(ctx, fixtureYards(host), target.Yard, contract, "10.20.30.40", 42000, true)
			if err != nil {
				t.Fatal(err)
			}
			if preview.Protocol != test.protocol || preview.GuestPort != test.guestPort {
				t.Fatalf("wrong resolved endpoint: %+v", preview)
			}
			set(contract, test.protocol, 42000, test.guestPort)
			actual, err := service.Prepare(ctx, fixtureYards(host), Change{})
			if err != nil {
				t.Fatal(err)
			}
			if err := preview.ValidateAfter(actual); err != nil {
				t.Fatal(err)
			}
			if err := service.Apply(ctx, actual); err != nil {
				t.Fatal(err)
			}
			boot := memoryService(host)
			boot.UseApprovedIngress = true
			if err := boot.Check(ctx, target.Yard); err != nil {
				t.Fatal(err)
			}
			var endpoints []netip.AddrPort
			boot.ClearStaleUDP = func(_ context.Context, endpoint netip.AddrPort) error {
				endpoints = append(endpoints, endpoint)
				return nil
			}
			if err := boot.ClearBootStaleUDP(ctx, target.Yard); err != nil {
				t.Fatal(err)
			}
			wantEndpoints := 1
			if test.protocol == "udp" {
				wantEndpoints = 2
			}
			if len(endpoints) != wantEndpoints {
				t.Fatalf("UDP cleaner received wrong protocols: %v", endpoints)
			}
			saved := instance.LocalDevices[contract.Device]["connect"]
			instance.LocalDevices[contract.Device]["connect"] = test.protocol + ":10.80.0.11:" + strconv.Itoa(test.guestPort)
			if err := boot.Check(ctx, target.Yard); err == nil {
				t.Fatal("boot accepted modified guest route")
			}
			instance.LocalDevices[contract.Device]["connect"] = saved
			// Closure must still refuse drift in any unrelated ACL rule.
			if test.shared {
				before := slices.Clone(target.ACL.Ingress)
				target.ACL.Ingress = append(target.ACL.Ingress, Rule{Action: "allow", State: "enabled", Source: "10.80.0.99/32"})
				if _, err := service.PreviewResourceIngress(ctx, fixtureYards(host), target.Yard, contract, "", 0, false); err == nil {
					t.Fatal("shared allowance permitted unrelated ACL drift")
				}
				target.ACL.Ingress = before
			}
			// Closure needs no live endpoint configuration, even for host-port.
			down, err := service.PreviewResourceIngress(ctx, fixtureYards(host), target.Yard, contract, "invalid", 0, false)
			if err != nil {
				t.Fatal(err)
			}
			if test.shared && down.After.Physical {
				t.Fatal("shared TCP allowance was reordered during closure")
			}
			if down.GuestPort != test.guestPort {
				t.Fatalf("closure lost approved port: %d", down.GuestPort)
			}
			delete(instance.Devices, contract.Device)
			delete(instance.LocalDevices, contract.Device)
			delete(instance.Config, contract.OwnershipKey())
			delete(instance.LocalConfig, contract.OwnershipKey())
			actual, err = service.Prepare(ctx, fixtureYards(host), Change{})
			if err != nil {
				t.Fatal(err)
			}
			if err := down.ValidateAfter(actual); err != nil {
				t.Fatal(err)
			}
			if err := service.Apply(ctx, actual); err != nil {
				t.Fatal(err)
			}
			approvals := host.stored.Policy.Bindings[0].ApprovedIngress
			wantApprovals := 1
			if test.shared {
				wantApprovals = 2
			}
			if len(approvals) != wantApprovals || approvals[0].Device != peer.Device {
				t.Fatalf("closure damaged peer approval: %+v", approvals)
			}
			rule := Rule{Action: "allow", State: "enabled", Protocol: "udp", DestinationPort: strconv.Itoa(test.guestPort)}
			if !slices.Contains(target.ACL.Ingress, rule) {
				t.Fatal("closure removed peer UDP ACL")
			}
			if test.protocol == "tcp" && slices.Contains(target.ACL.Ingress, Rule{Action: "allow", State: "enabled", Protocol: "tcp", DestinationPort: strconv.Itoa(test.guestPort)}) != test.shared {
				t.Fatal("closure changed the peer TCP ACL incorrectly")
			}
		})
	}
}

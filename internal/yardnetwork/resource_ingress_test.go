package yardnetwork

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/resource"
)

func TestResourceIngressPredictsVMResetMarkerOnlyForNativeMutation(t *testing.T) {
	for _, test := range []struct {
		name, status          string
		present, up, mutation bool
	}{
		{"first-up-after-boot", "Running", false, true, true},
		{"first-down-after-boot", "Running", true, false, true},
		{"service-only-up", "Running", true, true, false},
		{"service-only-down", "Running", false, false, false},
		{"ready-vm", "Ready", true, false, true},
		{"frozen-vm", "Frozen", true, false, true},
		{"stopped-vm", "Stopped", true, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			host := newMemoryHost()
			service := memoryService(host)
			contract := resource.ProxyContract{Profile: "sample", Resource: "relay", Device: "sample-relay",
				Connect: "udp:guest:41999", OwnerInterfaceSetting: "SAMPLE_INTERFACE",
				AddressPolicy: resource.ProxyAddressOwnerIPv4UDP, OwnershipMetadata: true}
			service.ContractSource = func(Yard) ([]resource.ProxyContract, error) { return []resource.ProxyContract{contract}, nil }
			observed := &host.snapshot.Yards[0]
			instance := &observed.InstanceInfo
			instance.Type = domain.YardVM
			instance.Status = test.status
			instance.Devices = map[string]map[string]string{"eth0": {"type": "nic", "ipv4.address": "10.80.0.10"}}
			instance.LocalDevices = map[string]map[string]string{}
			instance.Config = map[string]string{}
			instance.LocalConfig = map[string]string{}
			device := map[string]string{"type": "proxy", "listen": "udp:10.20.30.40:42000",
				"connect": "udp:10.80.0.10:41999", "bind": "host", "nat": "true"}
			setRoute := func(present bool) {
				if present {
					instance.Devices[contract.Device] = maps.Clone(device)
					instance.LocalDevices[contract.Device] = maps.Clone(device)
					instance.Config[contract.OwnershipKey()] = contract.OwnershipValue(device)
					instance.LocalConfig[contract.OwnershipKey()] = contract.OwnershipValue(device)
				} else {
					delete(instance.Devices, contract.Device)
					delete(instance.LocalDevices, contract.Device)
					delete(instance.Config, contract.OwnershipKey())
					delete(instance.LocalConfig, contract.OwnershipKey())
				}
			}
			setRoute(test.present)
			preview, err := service.PreviewResourceIngress(ctx, fixtureYards(host), observed.Yard, contract, "10.20.30.40", 42000, test.up)
			if err != nil {
				t.Fatal(err)
			}
			setRoute(test.up)
			if test.mutation {
				instance.Config["volatile.vm.needs_reset"] = "true"
				instance.LocalConfig["volatile.vm.needs_reset"] = "true"
			}
			actual, err := service.Prepare(ctx, fixtureYards(host), Change{})
			if err != nil {
				t.Fatal(err)
			}
			if err := preview.ValidateAfter(actual); err != nil {
				t.Fatal(err)
			}
			instance.Config["volatile.eth0.hwaddr"] = "00:16:3e:aa:bb:cc"
			drifted, err := service.Prepare(ctx, fixtureYards(host), Change{})
			if err != nil {
				t.Fatal(err)
			}
			if err := preview.ValidateAfter(drifted); err == nil {
				t.Fatal("unrelated volatile network change escaped the snapshot guard")
			}
		})
	}
}

func TestResourceIngressPreviewScopesOneACLAndVerifiesActualRoute(t *testing.T) {
	ctx := context.Background()
	host := newMemoryHost()
	service := memoryService(host)
	contract := resource.ProxyContract{Profile: "sample", Resource: "relay", Device: "sample-relay", Connect: "udp:guest:41999", OwnerInterfaceSetting: "SAMPLE_INTERFACE", AddressPolicy: resource.ProxyAddressOwnerIPv4UDP, OwnershipMetadata: true}
	selected := true
	service.ContractSource = func(Yard) ([]resource.ProxyContract, error) {
		if !selected {
			return nil, nil
		}
		return []resource.ProxyContract{contract}, nil
	}
	on := true
	initial, err := service.Prepare(ctx, fixtureYards(host), Change{Isolation: &on})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Apply(ctx, initial); err != nil {
		t.Fatal(err)
	}
	target := host.snapshot.Yards[0].Yard
	host.snapshot.Yards[0].InstanceInfo.Type = domain.YardVM
	host.snapshot.Yards[0].InstanceInfo.Devices = map[string]map[string]string{
		"eth0": {"type": "nic", "ipv4.address": "10.80.0.10"},
	}
	host.snapshot.Yards[0].InstanceInfo.LocalDevices = map[string]map[string]string{}
	host.snapshot.Yards[0].InstanceInfo.LocalConfig = map[string]string{"volatile.vm.needs_reset": "true"}
	host.snapshot.Yards[0].InstanceInfo.Config = map[string]string{"volatile.vm.needs_reset": "true"}
	yards := fixtureYards(host)
	selected = false
	if _, err := service.PreviewResourceIngress(ctx, yards, target, contract, "10.20.30.40", 42000, true); err == nil {
		t.Fatal("deselected public ingress contract was accepted")
	}
	selected = true
	preview, err := service.PreviewResourceIngress(ctx, yards, target, contract, "10.20.30.40", 42000, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.After.Updates) != 1 || preview.After.Updates[0].Yard.Name != target.Name {
		t.Fatalf("unscoped update: %+v", preview.After.Updates)
	}
	device := map[string]string{"type": "proxy", "listen": "udp:10.20.30.40:42000", "connect": "udp:10.80.0.10:41999", "bind": "host", "nat": "true"}
	host.snapshot.Yards[0].InstanceInfo.LocalDevices["sample-relay"] = device
	host.snapshot.Yards[0].InstanceInfo.Devices["sample-relay"] = device
	host.snapshot.Yards[0].InstanceInfo.LocalConfig[contract.OwnershipKey()] = contract.OwnershipValue(device)
	host.snapshot.Yards[0].InstanceInfo.Config = map[string]string{contract.OwnershipKey(): contract.OwnershipValue(device), "volatile.vm.needs_reset": "true"}
	actual, err := service.Prepare(ctx, yards, Change{})
	if err != nil {
		t.Fatal(err)
	}
	if err := preview.ValidateAfter(actual); err != nil {
		t.Fatal(err)
	}
	if err := service.Apply(ctx, actual); err != nil {
		t.Fatal(err)
	}
	selected = false
	deselected, err := service.Prepare(ctx, yards, Change{})
	if err != nil || deselected.Fingerprint == actual.Fingerprint {
		t.Fatalf("selection change did not invalidate network plan: %v", err)
	}
	if err := preview.ValidateAfter(deselected); err == nil {
		t.Fatal("deselected resource route passed approved network preview")
	}
	selected = true
	replacement, err := service.PreviewResourceIngress(ctx, yards, target, contract, "10.20.30.41", 42000, true)
	if err != nil || replacement.After.Fingerprint == preview.After.Fingerprint {
		t.Fatalf("owned endpoint replacement was not previewed: %v", err)
	}
	host.snapshot.Yards[0].InstanceInfo.LocalConfig[contract.OwnershipKey()] =
		"v1:pending:" + strings.TrimPrefix(contract.OwnershipValue(device), "v1:")
	if _, err := service.PreviewResourceIngress(ctx, yards, target, contract, "", 0, false); err != nil {
		t.Fatalf("owned pending endpoint could not finish shutdown: %v", err)
	}
	host.snapshot.Yards[0].InstanceInfo.LocalConfig[contract.OwnershipKey()] = contract.OwnershipValue(device)
	delete(host.snapshot.Yards[0].InstanceInfo.LocalDevices, "sample-relay")
	delete(host.snapshot.Yards[0].InstanceInfo.Devices, "sample-relay")
	host.snapshot.Yards[0].InstanceInfo.LocalConfig[contract.OwnershipKey()] = "v1:pending:" + strings.Repeat("a", 64)
	if _, err := service.PreviewResourceIngress(ctx, yards, target, contract, "", 0, false); err != nil {
		t.Fatalf("orphaned pending marker could not finish shutdown: %v", err)
	}
	host.snapshot.Yards[0].InstanceInfo.LocalConfig[contract.OwnershipKey()] = "untrusted"
	if _, err := service.PreviewResourceIngress(ctx, yards, target, contract, "10.20.30.41", 42000, true); err == nil {
		t.Fatal("untrusted orphaned marker was accepted")
	}
	host.snapshot.Yards[0].InstanceInfo.LocalDevices["sample-relay"] = device
	host.snapshot.Yards[0].InstanceInfo.Devices["sample-relay"] = device
	host.snapshot.Yards[0].InstanceInfo.LocalConfig[contract.OwnershipKey()] = contract.OwnershipValue(device)
	device["listen"] = "udp:10.20.30.41:42000"
	host.snapshot.Yards[0].InstanceInfo.LocalConfig[contract.OwnershipKey()] = contract.OwnershipValue(device)
	host.snapshot.Yards[0].InstanceInfo.Config[contract.OwnershipKey()] = contract.OwnershipValue(device)
	wrongRoute, err := service.Prepare(ctx, yards, Change{})
	if err != nil {
		t.Fatal(err)
	}
	if err := preview.ValidateAfter(wrongRoute); err == nil || !strings.Contains(err.Error(), "snapshot changed") {
		t.Fatalf("changed owner route accepted: %v", err)
	}
	device["listen"] = "udp:10.20.30.40:42000"
	host.snapshot.Yards[0].InstanceInfo.LocalConfig[contract.OwnershipKey()] = contract.OwnershipValue(device)
	host.snapshot.Yards[0].InstanceInfo.Config[contract.OwnershipKey()] = contract.OwnershipValue(device)
	host.snapshot.Yards[1].ACL.Ingress = append(host.snapshot.Yards[1].ACL.Ingress, Rule{Action: "allow", State: "enabled", Source: "192.0.2.1/32"})
	drift, err := service.Prepare(ctx, yards, Change{})
	if err != nil {
		t.Fatal(err)
	}
	if err := preview.ValidateAfter(drift); err == nil || !strings.Contains(err.Error(), "unrelated drift") {
		t.Fatalf("unrelated yard drift accepted: %v", err)
	}
}

func TestResourceIngressRollbackClosesOnlyApprovedTargetACLAllowance(t *testing.T) {
	ctx := context.Background()
	host := newMemoryHost()
	service := memoryService(host)
	contract := resource.ProxyContract{Profile: "sample", Resource: "relay", Device: "sample-relay", Connect: "udp:guest:41999", OwnerInterfaceSetting: "SAMPLE_INTERFACE", AddressPolicy: resource.ProxyAddressOwnerIPv4UDP, OwnershipMetadata: true}
	service.ContractSource = func(Yard) ([]resource.ProxyContract, error) {
		return []resource.ProxyContract{contract}, nil
	}
	on := true
	initial, err := service.Prepare(ctx, fixtureYards(host), Change{Isolation: &on})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Apply(ctx, initial); err != nil {
		t.Fatal(err)
	}
	target := host.snapshot.Yards[0].Yard
	host.snapshot.Yards[0].InstanceInfo.Type = domain.YardVM
	host.snapshot.Yards[0].InstanceInfo.Devices = map[string]map[string]string{
		"eth0": {"type": "nic", "ipv4.address": "10.80.0.10"},
	}
	host.snapshot.Yards[0].InstanceInfo.LocalDevices = map[string]map[string]string{}
	host.snapshot.Yards[0].InstanceInfo.LocalConfig = map[string]string{"volatile.vm.needs_reset": "true"}
	yards := fixtureYards(host)
	preview, err := service.PreviewResourceIngress(ctx, yards, target, contract, "10.20.30.40", 42000, true)
	if err != nil {
		t.Fatal(err)
	}
	device := map[string]string{"type": "proxy", "listen": "udp:10.20.30.40:42000", "connect": "udp:10.80.0.10:41999", "bind": "host", "nat": "true"}
	host.snapshot.Yards[0].InstanceInfo.LocalDevices[contract.Device] = maps.Clone(device)
	host.snapshot.Yards[0].InstanceInfo.Devices[contract.Device] = maps.Clone(device)
	host.snapshot.Yards[0].InstanceInfo.LocalConfig[contract.OwnershipKey()] = contract.OwnershipValue(device)
	host.snapshot.Yards[0].InstanceInfo.Config = map[string]string{contract.OwnershipKey(): contract.OwnershipValue(device), "volatile.vm.needs_reset": "true"}
	opened, err := service.Prepare(ctx, yards, Change{})
	if err != nil {
		t.Fatal(err)
	}
	if err := preview.ValidateAfter(opened); err != nil {
		t.Fatal(err)
	}
	if err := service.Apply(ctx, opened); err != nil {
		t.Fatal(err)
	}
	allowance := Rule{Action: "allow", State: "enabled", Protocol: "udp", DestinationPort: "41999"}
	if !slices.Contains(host.snapshot.Yards[0].ACL.Ingress, allowance) {
		t.Fatal("published route did not open its target ACL allowance")
	}

	// A later readiness failure triggers handler rollback, removing only its
	// owned route. The rollback coordinator must then close the ACL allowance.
	delete(host.snapshot.Yards[0].InstanceInfo.LocalDevices, contract.Device)
	delete(host.snapshot.Yards[0].InstanceInfo.Devices, contract.Device)
	delete(host.snapshot.Yards[0].InstanceInfo.LocalConfig, contract.OwnershipKey())
	delete(host.snapshot.Yards[0].InstanceInfo.Config, contract.OwnershipKey())
	closure, err := service.PreviewResourceIngress(ctx, yards, target, contract, "", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := preview.ValidateRollback(closure, contract); err != nil {
		t.Fatal(err)
	}
	if err := service.Apply(ctx, closure.Before); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(host.snapshot.Yards[0].ACL.Ingress, allowance) {
		t.Fatal("rollback left the guest UDP allowance open")
	}
	if len(host.stored.Policy.Bindings[0].ApprovedIngress) != 0 {
		t.Fatal("rollback left durable boot ingress approval")
	}
	if after, err := service.Prepare(ctx, yards, Change{}); err != nil || after.Changed {
		t.Fatalf("rollback ACL did not converge: changed=%v err=%v", after.Changed, err)
	}

	// A drifted target NIC must never be repaired under rollback authority.
	host.snapshot.Yards[0].ProfileDevices["eth0"]["security.mac_filtering"] = "false"
	if _, err := service.PreviewResourceIngress(ctx, yards, target, contract, "", 0, false); err == nil {
		t.Fatal("unrelated target NIC drift passed closure preview")
	}
}

func TestResourceIngressRejectsTargetDriftButAllowsInterruptedShutdown(t *testing.T) {
	ctx := context.Background()
	host := newMemoryHost()
	service := memoryService(host)
	contract := resource.ProxyContract{Profile: "sample", Resource: "relay", Device: "sample-relay",
		Connect: "udp:guest:41999", OwnerInterfaceSetting: "SAMPLE_INTERFACE",
		AddressPolicy: resource.ProxyAddressOwnerIPv4UDP, OwnershipMetadata: true}
	service.ContractSource = func(Yard) ([]resource.ProxyContract, error) { return []resource.ProxyContract{contract}, nil }
	target := host.snapshot.Yards[0].Yard
	device := map[string]string{"type": "proxy", "listen": "udp:10.20.30.40:42000",
		"connect": "udp:10.80.0.10:41999", "bind": "host", "nat": "true"}
	instance := &host.snapshot.Yards[0].InstanceInfo
	instance.Type = domain.YardVM
	instance.LocalDevices = map[string]map[string]string{contract.Device: maps.Clone(device)}
	instance.Devices = map[string]map[string]string{contract.Device: maps.Clone(device),
		"eth0": {"type": "nic", "ipv4.address": "10.80.0.10"}}
	instance.LocalConfig = map[string]string{contract.OwnershipKey(): contract.OwnershipValue(device), "volatile.vm.needs_reset": "true"}
	instance.Config = maps.Clone(instance.LocalConfig)
	yards := fixtureYards(host)
	on := true
	initial, err := service.Prepare(ctx, yards, Change{Isolation: &on})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Apply(ctx, initial); err != nil {
		t.Fatal(err)
	}
	published, stored := cloneValue(host.snapshot), cloneValue(host.stored)
	for _, drift := range []string{"extra ACL rule", "missing ACL rule", "NIC", "project"} {
		t.Run(drift, func(t *testing.T) {
			host.snapshot = cloneValue(published)
			observed := &host.snapshot.Yards[0]
			switch drift {
			case "extra ACL rule":
				observed.ACL.Ingress = append(observed.ACL.Ingress, Rule{Action: "allow", State: "enabled", Source: "192.0.2.1/32"})
			case "missing ACL rule":
				observed.ACL.Ingress = slices.DeleteFunc(observed.ACL.Ingress, func(rule Rule) bool { return rule.Protocol == "tcp" })
			case "NIC":
				observed.ProfileDevices["eth0"]["security.mac_filtering"] = "false"
			case "project":
				observed.ProjectConfig["restricted.networks.access"] = ""
			}
			for _, up := range []bool{true, false} {
				if _, err := service.PreviewResourceIngress(ctx, yards, target, contract, "10.20.30.40", 42000, up); err == nil {
					t.Fatalf("up=%v accepted unrelated target drift", up)
				}
			}
		})
	}
	for _, routePresent := range []bool{false, true} {
		host.snapshot, host.stored = cloneValue(published), cloneValue(stored)
		instance := &host.snapshot.Yards[0].InstanceInfo
		if !routePresent {
			delete(instance.LocalDevices, contract.Device)
			delete(instance.Devices, contract.Device)
		}
		instance.LocalConfig[contract.OwnershipKey()] = "v1:pending:" + contract.OwnershipValue(device)[3:]
		instance.Config[contract.OwnershipKey()] = instance.LocalConfig[contract.OwnershipKey()]
		retry, err := service.PreviewResourceIngress(ctx, yards, target, contract, "", 0, false)
		if err != nil || !retry.Before.Changed || !retry.After.Changed {
			t.Fatalf("pending shutdown cannot clean only its allowance: %+v, %v", retry, err)
		}
		delete(instance.LocalDevices, contract.Device)
		delete(instance.Devices, contract.Device)
		delete(instance.LocalConfig, contract.OwnershipKey())
		delete(instance.Config, contract.OwnershipKey())
		actual, err := service.Prepare(ctx, yards, Change{})
		if err != nil {
			t.Fatal(err)
		}
		if err := retry.ValidateAfter(actual); err != nil {
			t.Fatal(err)
		}
		if err := service.Apply(ctx, actual); err != nil {
			t.Fatal(err)
		}
		verified, err := service.Prepare(ctx, yards, Change{})
		if err != nil || verified.Changed || len(host.stored.Policy.Bindings[0].ApprovedIngress) != 0 {
			t.Fatalf("shutdown retry did not converge: changed=%v err=%v", verified.Changed, err)
		}
	}
}

func TestResourceIngressRollbackRequiresAbsentRouteAndMarker(t *testing.T) {
	for _, isolation := range []bool{false, true} {
		name := "isolation-off"
		if isolation {
			name = "isolation-on"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			host := newMemoryHost()
			service := memoryService(host)
			contract := resource.ProxyContract{Profile: "sample", Resource: "relay", Device: "sample-relay",
				Connect: "udp:guest:41999", OwnerInterfaceSetting: "SAMPLE_INTERFACE",
				AddressPolicy: resource.ProxyAddressOwnerIPv4UDP, OwnershipMetadata: true}
			service.ContractSource = func(Yard) ([]resource.ProxyContract, error) { return []resource.ProxyContract{contract}, nil }
			target := host.snapshot.Yards[0].Yard
			instance := &host.snapshot.Yards[0].InstanceInfo
			instance.Type = domain.YardVM
			device := map[string]string{"type": "proxy", "listen": "udp:10.20.30.40:42000",
				"connect": "udp:10.80.0.10:41999", "bind": "host", "nat": "true"}
			instance.LocalDevices = map[string]map[string]string{contract.Device: maps.Clone(device)}
			instance.Devices = map[string]map[string]string{contract.Device: maps.Clone(device),
				"eth0": {"type": "nic", "ipv4.address": "10.80.0.10"}}
			instance.LocalConfig = map[string]string{contract.OwnershipKey(): contract.OwnershipValue(device)}
			instance.Config = maps.Clone(instance.LocalConfig)
			yards := fixtureYards(host)
			initial, err := service.Prepare(ctx, yards, Change{Isolation: &isolation})
			if err != nil {
				t.Fatal(err)
			}
			if err := service.Apply(ctx, initial); err != nil {
				t.Fatal(err)
			}
			preview, err := service.PreviewResourceIngress(ctx, yards, target, contract, "10.20.30.40", 42000, true)
			if err != nil || preview.Before.Changed {
				t.Fatalf("expected converged published route: changed=%v err=%v", preview.Before.Changed, err)
			}
			published := cloneValue(host.snapshot)
			for _, state := range []string{"published", "pending-marker", "final-marker", "effective-proxy", "effective-marker", "closed", "closed-nil-maps"} {
				t.Run(state, func(t *testing.T) {
					host.snapshot = cloneValue(published)
					instance := &host.snapshot.Yards[0].InstanceInfo
					if state != "published" {
						delete(instance.LocalDevices, contract.Device)
						if state != "effective-proxy" {
							delete(instance.Devices, contract.Device)
						}
						delete(instance.LocalConfig, contract.OwnershipKey())
						if state != "effective-marker" {
							delete(instance.Config, contract.OwnershipKey())
						}
					}
					switch state {
					case "pending-marker":
						instance.LocalConfig[contract.OwnershipKey()] = "v1:pending:" + strings.Repeat("a", 64)
					case "final-marker":
						instance.LocalConfig[contract.OwnershipKey()] = contract.OwnershipValue(device)
					case "closed-nil-maps":
						instance.LocalDevices, instance.LocalConfig, instance.Config = nil, nil, nil
					}
					closure, err := service.PreviewResourceIngress(ctx, yards, target, contract, "", 0, false)
					if err == nil {
						err = preview.ValidateRollback(closure, contract)
					}
					closed := strings.HasPrefix(state, "closed")
					if (err == nil) != closed {
						t.Fatalf("rollback accepted=%v, want=%v: %v", err == nil, closed, err)
					}
				})
			}
		})
	}
}

func TestResourceIngressEndpointReplacementUpdatesApprovalWithoutACLRestart(t *testing.T) {
	ctx := context.Background()
	host := newMemoryHost()
	service := memoryService(host)
	contract := resource.ProxyContract{Profile: "sample", Resource: "relay", Device: "sample-relay",
		Connect: "udp:guest:41999", OwnerInterfaceSetting: "SAMPLE_INTERFACE",
		AddressPolicy: resource.ProxyAddressOwnerIPv4UDP, OwnershipMetadata: true}
	service.ContractSource = func(Yard) ([]resource.ProxyContract, error) { return []resource.ProxyContract{contract}, nil }
	target := host.snapshot.Yards[0].Yard
	host.snapshot.Yards[0].InstanceInfo.Type = domain.YardVM
	host.snapshot.Yards[0].InstanceInfo.Devices = map[string]map[string]string{"eth0": {"type": "nic", "ipv4.address": "10.80.0.10"}}
	host.snapshot.Yards[0].InstanceInfo.LocalDevices = map[string]map[string]string{}
	host.snapshot.Yards[0].InstanceInfo.LocalConfig = map[string]string{"volatile.vm.needs_reset": "true"}
	host.snapshot.Yards[0].InstanceInfo.Config = map[string]string{"volatile.vm.needs_reset": "true"}
	on := true
	initial, err := service.Prepare(ctx, fixtureYards(host), Change{Isolation: &on})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Apply(ctx, initial); err != nil {
		t.Fatal(err)
	}
	yards := fixtureYards(host)
	for _, endpoint := range []string{"udp:10.20.30.40:42000", "udp:10.20.30.41:42001"} {
		address, port := "10.20.30.40", 42000
		if strings.Contains(endpoint, ".41:") {
			address, port = "10.20.30.41", 42001
		}
		preview, err := service.PreviewResourceIngress(ctx, yards, target, contract, address, port, true)
		if err != nil {
			t.Fatal(err)
		}
		device := map[string]string{"type": "proxy", "listen": endpoint,
			"connect": "udp:10.80.0.10:41999", "bind": "host", "nat": "true"}
		host.snapshot.Yards[0].InstanceInfo.LocalDevices[contract.Device] = maps.Clone(device)
		host.snapshot.Yards[0].InstanceInfo.Devices[contract.Device] = maps.Clone(device)
		host.snapshot.Yards[0].InstanceInfo.LocalConfig[contract.OwnershipKey()] = contract.OwnershipValue(device)
		host.snapshot.Yards[0].InstanceInfo.Config[contract.OwnershipKey()] = contract.OwnershipValue(device)
		actual, err := service.Prepare(ctx, yards, Change{})
		if err != nil {
			t.Fatal(err)
		}
		if err := preview.ValidateAfter(actual); err != nil {
			t.Fatal(err)
		}
		if endpoint == "udp:10.20.30.41:42001" && (actual.Physical || len(actual.Updates) != 0) {
			t.Fatalf("owner-only endpoint replacement changed guest ACL: %+v", actual.Updates)
		}
		if err := service.Apply(ctx, actual); err != nil {
			t.Fatal(err)
		}
		approved := host.stored.Policy.Bindings[0].ApprovedIngress
		if len(approved) != 1 || approved[0].Listen != endpoint {
			t.Fatalf("approved endpoint was not persisted: %+v", approved)
		}
	}
}

func TestResourceIngressRollbackRemovesOnlyItsApprovalWhenPeerKeepsUDPPort(t *testing.T) {
	target := Yard{Name: "private", Project: "subyard-private", Instance: "yard-private", Network: "incusbr0"}
	contract := resource.ProxyContract{Profile: "sample", Resource: "relay", Device: "sample-relay",
		Connect: "udp:guest:41999", AddressPolicy: resource.ProxyAddressOwnerIPv4UDP}
	first := ApprovedIngress{Device: contract.Device, Listen: "udp:10.20.30.40:42000", GuestPort: 41999}
	second := ApprovedIngress{Device: "other-relay", Listen: "udp:10.20.30.41:42001", GuestPort: 41999}
	binding := Binding{Yard: target, IPv4: "10.80.0.10", ApprovedIngress: []ApprovedIngress{first, second}}
	before := Policy{Schema: SchemaVersion, Isolation: true, AppliedIsolation: true,
		Revision: 3, AppliedRevision: 3, Bindings: []Binding{binding}}
	after := before
	after.Revision++
	after.Bindings = []Binding{{Yard: target, IPv4: binding.IPv4, ApprovedIngress: []ApprovedIngress{second}}}
	actual := IngressPreview{Target: target,
		Before: Plan{Before: StoredPolicy{Policy: before}, Policy: after, Changed: true, Fingerprint: "closed"},
		After:  Plan{Fingerprint: "closed"}}
	preview := IngressPreview{Target: target}
	if err := preview.ValidateRollback(actual, contract); err != nil {
		t.Fatalf("shared UDP ACL port prevented exact approval cleanup: %v", err)
	}
	actual.Before.Policy.Bindings[0].ApprovedIngress = nil
	if err := preview.ValidateRollback(actual, contract); err == nil {
		t.Fatal("rollback removed another route's approval")
	}
}

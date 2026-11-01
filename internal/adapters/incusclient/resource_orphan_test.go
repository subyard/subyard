package incusclient

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/yardnetwork"
	"github.com/lxc/incus/v6/shared/api"
)

func TestOriginalOrphanClosureCASBindsUUIDRouteMarkerAndNIC(t *testing.T) {
	contract := resource.ProxyContract{Profile: "fixture", Resource: "service", Device: "service-route", AddressPolicy: resource.ProxyAddressOwnerIPv4UDP, Connect: "udp:guest:41999", OwnershipMetadata: true}
	yard := yardnetwork.Yard{Name: "named", Project: "project-a", Instance: "yard-a"}
	nic := map[string]string{"type": "nic", "network": "incusbr0", "ipv4.address": "10.80.0.10"}
	route := map[string]string{"type": "proxy", "bind": "host", "nat": "true", "listen": "udp:10.20.30.40:42000", "connect": "udp:10.80.0.10:41999"}
	pending := "v1:pending:" + strings.TrimPrefix(contract.OwnershipValue(route), "v1:")
	for _, mode := range []string{"route-pending", "pending-clear", "pending-retain", "uuid", "pending-uuid", "route", "marker", "nic", "inherited"} {
		t.Run(mode, func(t *testing.T) {
			fake := newNetworkIncusServer(t)
			before := route
			marker := contract.OwnershipValue(route)
			retain := true
			if strings.HasPrefix(mode, "pending-") {
				before = nil
				marker = pending
				retain = mode == "pending-retain"
			}
			config := map[string]string{"volatile.uuid": "original-uuid", contract.OwnershipKey(): marker, "foreign": "keep"}
			devices := map[string]map[string]string{"eth0": maps.Clone(nic), "foreign": {"type": "disk", "path": "/keep"}}
			if len(before) != 0 {
				devices[contract.Device] = maps.Clone(before)
			}
			expanded := cloneDevices(devices)
			switch mode {
			case "uuid", "pending-uuid":
				config["volatile.uuid"] = "replacement-uuid"
			case "route":
				devices[contract.Device]["listen"] = "udp:10.20.30.40:42001"
			case "marker":
				config[contract.OwnershipKey()] = "foreign"
			case "nic":
				expanded["eth0"]["ipv4.address"] = "10.80.0.11"
			case "inherited":
				delete(devices, contract.Device)
			}
			fake.instances["project-a/yard-a"] = fakeInstance{ETag: "original-etag", Data: map[string]any{"name": "yard-a", "project": "project-a", "type": "virtual-machine", "config": config, "expanded_config": maps.Clone(config), "devices": devices, "expanded_devices": expanded}}
			client := New(fake.socket, "projects")
			err := client.RemoveOwnedResourceIngressIf(context.Background(), yard, contract, "original-uuid", nic, before, marker, retain)
			if mode != "route-pending" && mode != "pending-clear" && mode != "pending-retain" {
				if !errors.Is(err, domain.ErrPlanStale) || fake.instanceIfMatch != "" {
					t.Fatalf("foreign write: error=%v If-Match=%q", err, fake.instanceIfMatch)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "pending-retain" {
				if fake.instanceIfMatch != "" {
					t.Fatal("unchanged pending state wrote")
				}
				return
			}
			if fake.instanceIfMatch != "original-etag" {
				t.Fatal("closure did not use original ETag")
			}
			actual := fake.instances["project-a/yard-a"].Data
			actualConfig := actual["config"].(api.ConfigMap)
			actualDevices := actual["devices"].(api.DevicesMap)
			wanted := ""
			if retain {
				wanted = pending
			}
			if actualConfig[contract.OwnershipKey()] != wanted || len(actualDevices[contract.Device]) != 0 || actualConfig["foreign"] != "keep" || actualDevices["foreign"]["path"] != "/keep" {
				t.Fatal("closure changed unrelated state or pending intent")
			}
		})
	}
}

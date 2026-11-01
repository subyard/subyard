package incusclient

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/yardnetwork"
	"github.com/lxc/incus/v6/shared/api"
)

func TestOriginalStartupIngressCASBindsUUIDRouteAndNIC(t *testing.T) {
	contract := resource.ProxyContract{Profile: "fixture", Resource: "service", Device: "service-route", AddressPolicy: resource.ProxyAddressOwnerIPv4UDP, Connect: "udp:guest:41999", OwnershipMetadata: true}
	yard := yardnetwork.Yard{Name: "named", Project: "project-a", Instance: "yard-a"}
	nic := map[string]string{"type": "nic", "network": "incusbr0", "ipv4.address": "10.80.0.10"}
	want := map[string]string{"type": "proxy", "bind": "host", "nat": "true", "listen": "udp:10.20.30.40:42000", "connect": "udp:10.80.0.10:41999"}
	for _, mode := range []string{"forward", "rollback", "uuid", "device", "marker", "nic", "inherited", "rollback-uuid", "rollback-marker"} {
		t.Run(mode, func(t *testing.T) {
			fake := newNetworkIncusServer(t)
			config := map[string]string{"volatile.uuid": "original-uuid", "foreign": "keep"}
			devices := map[string]map[string]string{"eth0": maps.Clone(nic), "foreign": {"type": "disk", "path": "/keep"}}
			var before, desired map[string]string
			marker, wanted := "", contract.OwnershipValue(want)
			desired = want
			if mode != "forward" && mode != "inherited" {
				before = maps.Clone(want)
				marker = contract.OwnershipValue(want)
				devices[contract.Device] = maps.Clone(want)
				config[contract.OwnershipKey()] = marker
			}
			if mode == "rollback" || mode == "rollback-uuid" || mode == "rollback-marker" {
				desired = nil
				wanted = ""
			}
			expanded := cloneDevices(devices)
			switch mode {
			case "uuid", "rollback-uuid":
				config["volatile.uuid"] = "replacement-uuid"
			case "device":
				devices[contract.Device]["listen"] = "udp:10.20.30.40:42001"
			case "marker", "rollback-marker":
				config[contract.OwnershipKey()] = "foreign"
			case "nic":
				expanded["eth0"]["ipv4.address"] = "10.80.0.11"
			case "inherited":
				expanded[contract.Device] = maps.Clone(want)
			}
			fake.instances["project-a/yard-a"] = fakeInstance{ETag: "original-etag", Data: map[string]any{"name": "yard-a", "project": "project-a", "type": "virtual-machine", "config": config, "expanded_config": maps.Clone(config), "devices": devices, "expanded_devices": expanded}}
			client := New(fake.socket, "projects")
			err := client.SetOwnedResourceIngressIf(context.Background(), yard, contract, "original-uuid", nic, before, marker, desired, wanted)
			if mode != "forward" && mode != "rollback" {
				if !errors.Is(err, domain.ErrPlanStale) || fake.instanceIfMatch != "" {
					t.Fatalf("guard=%v PUT If-Match=%q", err, fake.instanceIfMatch)
				}
				return
			}
			if err != nil || fake.instanceIfMatch != "original-etag" {
				t.Fatal("original ETag mutation", err, fake.instanceIfMatch)
			}
			actual := fake.instances["project-a/yard-a"].Data
			if actual["config"].(api.ConfigMap)["foreign"] != "keep" || actual["devices"].(api.DevicesMap)["foreign"]["path"] != "/keep" {
				t.Fatal("unrelated instance fields changed")
			}
			if !maps.Equal(actual["devices"].(api.DevicesMap)[contract.Device], desired) || actual["config"].(api.ConfigMap)[contract.OwnershipKey()] != wanted {
				t.Fatal("wrong exact route result")
			}
		})
	}
}

func TestOriginalStartupIntentCASBindsUUIDAndOriginalKey(t *testing.T) {
	for _, mode := range []string{"commit", "uuid", "intent"} {
		t.Run(mode, func(t *testing.T) {
			fake := newNetworkIncusServer(t)
			config := map[string]string{"volatile.uuid": "original-uuid", "user.subyard.startup.fixture": "pending", "foreign": "keep"}
			if mode == "uuid" {
				config["volatile.uuid"] = "replacement-uuid"
			}
			if mode == "intent" {
				config["user.subyard.startup.fixture"] = "disabled"
			}
			fake.instances["project-a/yard-a"] = fakeInstance{ETag: "original-etag", Data: map[string]any{"name": "yard-a", "project": "project-a", "type": "virtual-machine", "config": config, "devices": map[string]map[string]string{"foreign": {"type": "disk", "path": "/keep"}}}}
			client := New(fake.socket, "projects")
			err := client.SetInstanceConfigIf(context.Background(), "project-a", "yard-a", "original-uuid", map[string]string{"user.subyard.startup.fixture": "pending"}, map[string]string{"user.subyard.startup.fixture": "enabled"})
			if mode != "commit" {
				if !errors.Is(err, domain.ErrPlanStale) || fake.instanceIfMatch != "" {
					t.Fatal("replaced original admitted", err, fake.instanceIfMatch)
				}
				return
			}
			if err != nil || fake.instanceIfMatch != "original-etag" {
				t.Fatal("original ETag intent commit", err)
			}
			actual := fake.instances["project-a/yard-a"].Data["config"].(api.ConfigMap)
			if actual["foreign"] != "keep" || actual["user.subyard.startup.fixture"] != "enabled" {
				t.Fatal("intent commit changed wrong fields")
			}
		})
	}
}

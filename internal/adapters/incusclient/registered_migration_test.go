package incusclient

import (
	"context"
	"maps"
	"math"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/resource"
)

func TestGuestExitMetadataIsAuthoritative(t *testing.T) {
	for _, raw := range []any{nil, "10", 10, float64(10.5), float64(-1), float64(256), math.NaN(), math.Inf(1)} {
		if _, ok := nativeGuestExitCode(raw); ok {
			t.Fatalf("untrusted metadata accepted: %T", raw)
		}
	}
	if code, ok := nativeGuestExitCode(float64(10)); !ok || code != 10 {
		t.Fatal(code, ok)
	}
}
func TestRegisteredInstanceMetadataCASChecksEffectiveSnapshot(t *testing.T) {
	for _, kind := range []struct {
		native string
		yard   domain.YardKind
	}{{"container", domain.YardContainer}, {"virtual-machine", domain.YardVM}} {
		for _, scenario := range []string{"valid", "inherited-config", "inherited-device", "uuid", "profile", "kind", "no-op"} {
			t.Run(kind.native+"/"+scenario, func(t *testing.T) {
				fake := newNetworkIncusServer(t)
				configuration := map[string]string{"volatile.uuid": "uuid-original", "user.preserve": "unchanged"}
				devices := map[string]map[string]string{"owned": {"type": "proxy", "bind": "host", "listen": "tcp:127.0.0.1:32000", "connect": "tcp:127.0.0.1:9000"}}
				receipt := (resource.ProxyContract{}).OwnershipValue(devices["owned"])
				metadata := map[string]string{"user.subyard.resource.owned": receipt, "user.subyard.disk_tmp": "v1"}
				if scenario == "no-op" {
					maps.Copy(configuration, metadata)
				}
				data := map[string]any{"name": "yard", "type": kind.native, "config": configuration, "expanded_config": maps.Clone(configuration), "devices": devices, "expanded_devices": devices, "profiles": []string{"default"}}
				fake.instances["project-a/yard"] = fakeInstance{ETag: "bound-v1", Data: data}
				client := New(fake.socket, "projects")
				before, err := client.Instance(context.Background(), "project-a", "yard")
				if err != nil || before.Type != kind.yard {
					t.Fatalf("observe instance kind: %s %v", before.Type, err)
				}
				switch scenario {
				case "inherited-config":
					data["expanded_config"].(map[string]string)["security.privileged"] = "true"
				case "inherited-device":
					data["expanded_devices"] = map[string]map[string]string{"foreign": {"type": "disk", "source": "/foreign", "path": "/foreign"}}
				case "uuid":
					configuration["volatile.uuid"] = "replacement"
				case "profile":
					data["profiles"] = []string{"foreign"}
				case "kind":
					data["type"] = "container"
					if kind.yard == domain.YardContainer {
						data["type"] = "virtual-machine"
					}
				}
				err = client.AdoptRegisteredInstance(context.Background(), before, metadata)
				if scenario == "valid" || scenario == "no-op" {
					if err != nil {
						t.Fatal(err)
					}
					after, err := client.Instance(context.Background(), "project-a", "yard")
					if err != nil || after.Config["user.subyard.disk_tmp"] != "v1" || after.Config["user.subyard.resource.owned"] != receipt || after.Config["volatile.uuid"] != before.Config["volatile.uuid"] || after.Config["user.preserve"] != "unchanged" {
						t.Fatalf("metadata publication lost native state: %v", err)
					}
				} else if err == nil || fake.instanceIfMatch != "" {
					t.Fatalf("stale native state received grant: %v", err)
				}
				if scenario == "no-op" && fake.instanceIfMatch != "" || scenario == "valid" && fake.instanceIfMatch != "bound-v1" {
					t.Fatalf("unexpected metadata CAS: scenario=%s If-Match=%q", scenario, fake.instanceIfMatch)
				}
			})
		}
	}
}

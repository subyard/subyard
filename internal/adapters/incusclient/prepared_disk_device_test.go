package incusclient

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
)

func TestPreparedReadOnlyDiskRetainsScopeAndRefusesDrift(t *testing.T) {
	for _, change := range []string{"none", "uuid", "local-device", "inherited-device"} {
		t.Run(change, func(t *testing.T) {
			fake := newNetworkIncusServer(t)
			fake.instances["project-a/yard-a"] = fakeInstance{ETag: "original-etag", Data: map[string]any{
				"name": "yard-a", "project": "project-a", "type": "container", "status": "Running",
				"config":  map[string]string{"volatile.uuid": "original-uuid", "foreign": "keep"},
				"devices": map[string]map[string]string{"foreign": {"type": "none"}},
			}}
			client := New(fake.socket, "projects")
			p, err := client.PrepareReadOnlyDisk(context.Background(), "project-a", "yard-a", "owned-routes", "/owned/routes", "/guest/routes")
			if err != nil {
				t.Fatal(err)
			}
			instance := fake.instances["project-a/yard-a"]
			switch change {
			case "uuid":
				instance.Data["config"] = map[string]string{"volatile.uuid": "replacement-uuid"}
			case "local-device":
				instance.Data["devices"] = map[string]map[string]string{"owned-routes": {"type": "none"}}
			case "inherited-device":
				instance.Data["expanded_devices"] = map[string]map[string]string{"owned-routes": {"type": "none"}}
			}
			fake.instances["project-a/yard-a"] = instance
			err = client.ApplyPreparedReadOnlyDisk(context.Background(), p)
			if change != "none" {
				if !errors.Is(err, domain.ErrPlanStale) || fake.instanceIfMatch != "" {
					t.Fatal("changed native target reached POST", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			actual, err := client.Instance(context.Background(), p.Project, p.Instance)
			if err != nil || !maps.Equal(actual.LocalDevices[p.Device], readOnlyDisk(p)) || actual.LocalDevices["foreign"]["type"] != "none" || actual.LocalConfig["foreign"] != "keep" || fake.instanceIfMatch != "original-etag" {
				t.Fatal("prepared disk changed unrelated state or omitted CAS", err)
			}
		})
	}
}

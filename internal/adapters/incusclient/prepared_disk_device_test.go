package incusclient

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/lxc/incus/v6/shared/api"
)

func TestPreparedReadOnlyDiskRetainsScopeAndRefusesDrift(t *testing.T) {
	for _, change := range []string{"none", "uuid", "name", "project", "missing-etag", "captured-desired-removed", "invalid-before", "empty-device", "local-device", "inherited-device", "wrong-source", "wrong-destination", "writable-device", "extra-device-key", "inherited-only-desired", "expanded-drift"} {
		t.Run(change, func(t *testing.T) {
			fake := newNetworkIncusServer(t)
			fake.instances["project-a/yard-a"] = fakeInstance{ETag: "original-etag", Data: map[string]any{
				"name": "yard-a", "project": "project-a", "type": "container", "status": "Running",
				"config":  map[string]string{"volatile.uuid": "original-uuid", "foreign": "keep"},
				"devices": map[string]map[string]string{"foreign": {"type": "none"}},
			}}
			if change == "captured-desired-removed" {
				fake.instances["project-a/yard-a"].Data["devices"] = map[string]map[string]string{"owned-routes": {"type": "disk", "source": "/owned/routes", "path": "/guest/routes", "readonly": "true"}}
			}
			client := New(fake.socket, "projects")
			p, err := client.PrepareReadOnlyDisk(context.Background(), "project-a", "yard-a", "owned-routes", "/owned/routes", "/guest/routes")
			if err != nil {
				t.Fatal(err)
			}
			instance := fake.instances["project-a/yard-a"]
			switch change {
			case "captured-desired-removed":
				instance.Data["devices"] = map[string]map[string]string{}
			case "invalid-before":
				p.Before = map[string]string{"type": "none"}
			case "name":
				instance.Data["name"] = "other-yard"
			case "project":
				instance.Data["project"] = "other-project"
			case "missing-etag":
				instance.ETag = ""
			case "empty-device":
				instance.Data["devices"] = map[string]map[string]string{"owned-routes": {}}
			case "wrong-source", "wrong-destination", "writable-device", "extra-device-key", "expanded-drift":
				device := readOnlyDisk(p)
				switch change {
				case "wrong-source":
					device["source"] = "/foreign/routes"
				case "wrong-destination":
					device["path"] = "/foreign/guest"
				case "writable-device":
					device["readonly"] = "false"
				case "extra-device-key":
					device["shift"] = "true"
				case "expanded-drift":
					instance.Data["expanded_devices"] = map[string]map[string]string{"owned-routes": {"type": "none"}}
				}
				instance.Data["devices"] = map[string]map[string]string{"owned-routes": device}
			case "inherited-only-desired":
				instance.Data["expanded_devices"] = map[string]map[string]string{"owned-routes": readOnlyDisk(p)}
			case "uuid":
				instance.Data["config"] = map[string]string{"volatile.uuid": "replacement-uuid"}
			case "local-device":
				instance.Data["devices"] = map[string]map[string]string{"owned-routes": {"type": "none"}}
			case "inherited-device":
				instance.Data["expanded_devices"] = map[string]map[string]string{"owned-routes": {"type": "none"}}
			}
			fake.instances["project-a/yard-a"] = instance
			checkErr := client.CheckPreparedReadOnlyDisk(context.Background(), p)
			err = client.ApplyPreparedReadOnlyDisk(context.Background(), p)
			if change != "none" {
				if !errors.Is(checkErr, domain.ErrPlanStale) || !errors.Is(err, domain.ErrPlanStale) || fake.instanceIfMatch != "" {
					t.Fatal("changed native target reached POST", err)
				}
				return
			}
			if checkErr != nil || err != nil {
				t.Fatal(checkErr, err)
			}
			actual, err := client.Instance(context.Background(), p.Project, p.Instance)
			if err != nil || !maps.Equal(actual.LocalDevices[p.Device], readOnlyDisk(p)) || actual.LocalDevices["foreign"]["type"] != "none" || actual.LocalConfig["foreign"] != "keep" || fake.instanceIfMatch != "original-etag" {
				t.Fatal("prepared disk changed unrelated state or omitted CAS", err)
			}
		})
	}
}

func TestPreparedReadOnlyDiskRecognizesOnlyOriginalBeforeOrExactDesiredOnReplay(t *testing.T) {
	for _, alreadyPresent := range []bool{false, true} {
		t.Run(map[bool]string{false: "original-absent", true: "original-desired"}[alreadyPresent], func(t *testing.T) {
			fake := newNetworkIncusServer(t)
			devices := map[string]map[string]string{"foreign": {"type": "none"}}
			if alreadyPresent {
				devices["owned-routes"] = map[string]string{"type": "disk", "source": "/owned/routes", "path": "/guest/routes", "readonly": "true"}
			}
			fake.instances["project-a/yard-a"] = fakeInstance{ETag: "before", Data: map[string]any{"name": "yard-a", "project": "project-a", "type": "container", "config": map[string]string{"volatile.uuid": "original-uuid", "foreign": "keep"}, "devices": devices}}
			client := New(fake.socket, "projects")
			original, err := client.PrepareReadOnlyDisk(context.Background(), "project-a", "yard-a", "owned-routes", "/owned/routes", "/guest/routes")
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(original)
			if err != nil {
				t.Fatal(err)
			}
			fake.mu.Lock()
			instance := fake.instances["project-a/yard-a"]
			instance.ETag = "current-before-apply"
			fake.instances["project-a/yard-a"] = instance
			fake.mu.Unlock()
			if err := client.CheckPreparedReadOnlyDisk(context.Background(), original); err != nil {
				t.Fatal("original Before refused", err)
			}
			if err := client.ApplyPreparedReadOnlyDisk(context.Background(), original); err != nil {
				t.Fatal(err)
			}
			fake.mu.Lock()
			match := fake.instanceIfMatch
			fake.instanceIfMatch = ""
			fake.mu.Unlock()
			if !alreadyPresent && match != "current-before-apply" || alreadyPresent && match != "" {
				t.Fatal("initial mutation did not use current GET CAS or skip exact desired")
			}
			var restored ports.PreparedReadOnlyDisk
			if err := json.Unmarshal(raw, &restored); err != nil {
				t.Fatal(err)
			}
			fresh := New(fake.socket, "projects")
			if err := fresh.CheckPreparedReadOnlyDisk(context.Background(), restored); err != nil {
				t.Fatal("fresh original record refused exact Desired", err)
			}
			if err := fresh.ApplyPreparedReadOnlyDisk(context.Background(), restored); err != nil {
				t.Fatal("fresh replay refused", err)
			}
			actual, err := fresh.Instance(context.Background(), original.Project, original.Instance)
			if err != nil {
				t.Fatal(err)
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if fake.instanceIfMatch != "" || !maps.Equal(actual.LocalDevices[original.Device], readOnlyDisk(original)) {
				t.Fatal("exact Desired repeated mutation")
			}
			if (restored.Before != nil) != alreadyPresent {
				t.Fatal("replay recaptured a new baseline")
			}
		})
	}
}

func TestPreparedReadOnlyDiskKeepsSameGETETagAcrossReplacement(t *testing.T) {
	observer := &ownerCreateObserverFixture{}
	client, fixture := ownerCreateClientFixture(t, observer, "instance-success")
	fixture.instance = &api.Instance{Name: "yard", Project: "original-project", InstancePut: api.InstancePut{Config: map[string]string{"volatile.uuid": "original-uuid"}}}
	original, err := client.PrepareReadOnlyDisk(context.Background(), "original-project", "yard", "owned-routes", "/owned/routes", "/guest/routes")
	if err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	fixture.afterInstanceGet = func() {
		fixture.instance.Config["volatile.uuid"] = "replacement"
		fixture.instanceETag = "replacement-etag"
		fixture.afterInstanceGet = nil
	}
	fixture.mu.Unlock()
	if err := client.ApplyPreparedReadOnlyDisk(context.Background(), original); err == nil {
		t.Fatal("replacement after guarded GET accepted original CAS")
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.updates != 0 {
		t.Fatal("replacement gained original disk")
	}
}

func TestPreparedReadOnlyDiskPartialBatchReplaysBothOriginalRecords(t *testing.T) {
	fake := newNetworkIncusServer(t)
	client := New(fake.socket, "projects")
	var originals []ports.PreparedReadOnlyDisk
	for _, name := range []string{"yard-a", "yard-b"} {
		fake.instances["project-a/"+name] = fakeInstance{ETag: "original-" + name, Data: map[string]any{"name": name, "project": "project-a", "type": "container", "config": map[string]string{"volatile.uuid": "uuid-" + name}, "devices": map[string]map[string]string{}}}
		p, err := client.PrepareReadOnlyDisk(context.Background(), "project-a", name, "owned-routes", "/owned/routes", "/guest/routes")
		if err != nil {
			t.Fatal(err)
		}
		originals = append(originals, p)
	}
	raw, err := json.Marshal(originals)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ApplyPreparedReadOnlyDisk(context.Background(), originals[0]); err != nil {
		t.Fatal(err)
	}
	// Restore the original records as a new helper would. In particular, do not
	// prepare another Before from the partially applied live state.
	var restored []ports.PreparedReadOnlyDisk
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	fresh := New(fake.socket, "projects")
	for _, p := range restored {
		if err := fresh.CheckPreparedReadOnlyDisk(context.Background(), p); err != nil {
			t.Fatal("partial first device blocked batch replay", err)
		}
	}
	for _, p := range restored {
		if err := fresh.ApplyPreparedReadOnlyDisk(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range restored {
		actual, err := fresh.Instance(context.Background(), p.Project, p.Instance)
		if err != nil || p.Before != nil || !maps.Equal(actual.LocalDevices[p.Device], readOnlyDisk(p)) {
			t.Fatal("original batch scope or desired state lost", err)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.nextOperation != 2 {
		t.Fatal("partial replay repeated an already desired mutation", fake.nextOperation)
	}
}

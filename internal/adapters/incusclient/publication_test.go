package incusclient

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/netip"
	"testing"

	"github.com/Subyard/Subyard/internal/ownerpublication"
)

func TestOptionalPublicationNoOpDoesNotRequireWriterEpoch(t *testing.T) {
	for _, devices := range []map[string]map[string]string{nil, {}} {
		fake := newNetworkIncusServer(t)
		fake.instances["p/yard"] = fakeInstance{ETag: "instance-v1", Data: map[string]any{
			"name": "yard", "status": "Stopped", "type": "container",
			"config": map[string]string{}, "devices": devices,
		}}
		checks := 0
		waiting, err := New(fake.socket, "projects").ReconcilePublications(context.Background(), "p", "yard", nil, nil, func() error {
			checks++
			return errors.New("uncommitted writer epoch")
		})
		if err != nil || len(waiting) != 0 || checks != 0 || fake.instanceIfMatch != "" || len(fake.powerCalls) != 0 {
			t.Fatalf("empty publication changed fresh instance: waiting=%v err=%v checks=%d etag=%s", waiting, err, checks, fake.instanceIfMatch)
		}
	}
}

func TestOptionalPublicationMutationRequiresWriterEpoch(t *testing.T) {
	fake := newNetworkIncusServer(t)
	device := map[string]string{"type": "proxy", "bind": "host", "listen": "tcp:192.0.2.12:8080", "connect": "tcp:127.0.0.1:80"}
	devices := map[string]map[string]string{"optional": device}
	config := map[string]string{"user.fixture.owned": "v1"}
	fake.instances["p/yard"] = fakeInstance{ETag: "instance-v1", Data: map[string]any{
		"name": "yard", "status": "Stopped", "type": "container", "config": config,
		"expanded_config": config, "devices": devices, "expanded_devices": devices,
	}}
	contract := ownerpublication.Contract{Device: "optional", Key: "user.fixture.owned", Owns: func(marker string, actual map[string]string) bool {
		return marker == "v1" && maps.Equal(actual, device)
	}}
	blocked := errors.New("uncommitted writer epoch")
	_, err := New(fake.socket, "projects").ReconcilePublications(context.Background(), "p", "yard", []ownerpublication.Contract{contract}, nil, func() error { return blocked })
	if !errors.Is(err, blocked) || fake.instanceIfMatch != "" || len(fake.powerCalls) != 0 {
		t.Fatalf("uncommitted mutation was admitted: err=%v etag=%s", err, fake.instanceIfMatch)
	}
	observed, err := New(fake.socket, "projects").Instance(context.Background(), "p", "yard")
	if err != nil || !maps.Equal(observed.Devices["optional"], device) || observed.Config[ownerpublication.Prefix+"optional"] != "" {
		t.Fatalf("blocked writer changed exact publication: %+v %v", observed, err)
	}
}

func TestOptionalPublicationAtomicUpdatePreservesForeignStateAndFailedRestore(t *testing.T) {
	fake := newNetworkIncusServer(t)
	device := map[string]string{"type": "proxy", "bind": "host", "listen": "tcp:192.0.2.12:8080", "connect": "tcp:127.0.0.1:80"}
	devices := map[string]map[string]string{"optional": device, "root": {"type": "disk", "path": "/"}}
	config := map[string]string{"user.fixture.owned": "v1", "foreign": "keep"}
	fake.instances["p/yard"] = fakeInstance{ETag: "instance-v1", Data: map[string]any{"name": "yard", "status": "Running", "type": "container", "description": "keep description", "profiles": []string{"default"}, "config": config, "expanded_config": config, "devices": devices, "expanded_devices": devices}}
	contract := ownerpublication.Contract{Device: "optional", Key: "user.fixture.owned", Owns: func(marker string, actual map[string]string) bool {
		return marker == "v1" && maps.Equal(actual, device)
	}}
	client := New(fake.socket, "projects")
	waiting, err := client.ReconcilePublications(context.Background(), "p", "yard", []ownerpublication.Contract{contract}, nil)
	if err != nil || len(waiting) != 1 || fake.instanceIfMatch != "instance-v1" {
		t.Fatalf("detach: %v %v etag=%s", waiting, err, fake.instanceIfMatch)
	}
	observed, err := client.Instance(context.Background(), "p", "yard")
	if err != nil || observed.Status != "Running" || observed.Devices["optional"] != nil || observed.Config[ownerpublication.Prefix+"optional"] == "" || observed.Config["foreign"] != "keep" || observed.Devices["root"]["path"] != "/" {
		t.Fatalf("atomic capture/removal: %+v %v", observed, err)
	}
	fake.failures["PUT /1.0/instances/yard"] = http.StatusBadRequest
	_, err = client.ReconcilePublications(context.Background(), "p", "yard", []ownerpublication.Contract{contract}, []netip.Addr{netip.MustParseAddr("192.0.2.12")})
	if err == nil {
		t.Fatal("failed restore accepted")
	}
	after, _ := client.Instance(context.Background(), "p", "yard")
	if after.Config[ownerpublication.Prefix+"optional"] != observed.Config[ownerpublication.Prefix+"optional"] || after.Devices["optional"] != nil {
		t.Fatal("failed restore lost captured exact state")
	}
	delete(fake.failures, "PUT /1.0/instances/yard")
	_, err = client.ReconcilePublications(context.Background(), "p", "yard", []ownerpublication.Contract{contract}, []netip.Addr{netip.MustParseAddr("192.0.2.12")})
	if err != nil {
		t.Fatal(err)
	}
	after, _ = client.Instance(context.Background(), "p", "yard")
	if !maps.Equal(after.Devices["optional"], device) || after.Config[ownerpublication.Prefix+"optional"] != "" || len(fake.powerCalls) != 0 {
		t.Fatal("restore did not retain exact device independently of power")
	}
	if fake.instances["p/yard"].Data["description"] != "keep description" {
		t.Fatal("instance description lost")
	}
}

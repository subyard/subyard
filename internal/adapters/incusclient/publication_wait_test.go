package incusclient

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerpublication"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/previewroute"
	"github.com/lxc/incus/v6/shared/api"
)

func TestRunningPublicationOriginalSnapshotAndAcknowledgement(t *testing.T) {
	for _, mode := range []string{"restore", "container", "wrong-kind", "replacement", "intent", "unmanaged", "uninitialized", "stopped", "post-replacement", "post-intent", "missing-etag", "missing-ack"} {
		t.Run(mode, func(t *testing.T) {
			fake := newNetworkIncusServer(t)
			device := previewroute.Device("100.127.255.254", "23456")
			cfg := map[string]string{"volatile.uuid": "original", "user.subyard.managed": "true", "user.subyard.initialized": "true", "user.subyard.desired_power": "running", previewroute.Key: "v1:100.127.255.254:23456", "foreign": "keep"}
			payload, _ := json.Marshal(ownerpublication.Receipt{Version: 1, Ownership: cfg[previewroute.Key], Device: device})
			cfg[ownerpublication.Prefix+previewroute.DeviceName] = string(payload)
			kind := domain.YardVM
			apiKind := "virtual-machine"
			if mode == "container" {
				kind = domain.YardContainer
				apiKind = "container"
			}
			if mode == "wrong-kind" {
				kind = domain.YardContainer
			}
			status := "Running"
			etag := "original-etag"
			switch mode {
			case "replacement":
				cfg["volatile.uuid"] = "foreign"
			case "intent":
				cfg["user.subyard.desired_power"] = "stopped"
			case "unmanaged":
				cfg["user.subyard.managed"] = "false"
			case "uninitialized":
				cfg["user.subyard.initialized"] = "false"
			case "stopped":
				status = "Stopped"
			case "missing-etag":
				etag = ""
			}
			fake.instances["p/yard"] = fakeInstance{ETag: etag, Data: map[string]any{"name": "yard", "type": apiKind, "status": status, "config": cfg, "expanded_config": maps.Clone(cfg), "devices": map[string]map[string]string{}, "expanded_devices": map[string]map[string]string{}}}
			reads := 0
			fake.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/1.0/instances/yard" {
					reads++
					if reads == 2 && (mode == "post-replacement" || mode == "post-intent") {
						fake.mu.Lock()
						item := fake.instances["p/yard"]
						key, value := "volatile.uuid", "foreign"
						if mode == "post-intent" {
							key, value = "user.subyard.desired_power", "stopped"
						}
						setPublicationFixtureConfig(item, key, value)
						fake.instances["p/yard"] = item
						fake.mu.Unlock()
					}
				}
				if mode == "missing-ack" && r.Method == http.MethodPut && r.URL.Path == "/1.0/instances/yard" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusAccepted)
					_ = json.NewEncoder(w).Encode(map[string]any{"type": "async", "status": "Operation created", "status_code": 100, "metadata": map[string]any{"class": "task", "status": "Running", "status_code": 103}})
					return
				}
				recorder := httptest.NewRecorder()
				fake.ServeHTTP(recorder, r)
				var body map[string]any
				if json.Unmarshal(recorder.Body.Bytes(), &body) == nil && (r.Method == http.MethodPut || r.Method == http.MethodGet && len(r.URL.Path) > len("/1.0/operations/") && r.URL.Path[:len("/1.0/operations/")] == "/1.0/operations/") {
					if meta, ok := body["metadata"].(map[string]any); ok {
						meta["resources"] = map[string][]string{"instances": {"/1.0/instances/yard?project=p"}}
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(recorder.Code)
					_ = json.NewEncoder(w).Encode(body)
					return
				}
				for key, values := range recorder.Header() {
					w.Header()[key] = values
				}
				w.WriteHeader(recorder.Code)
				_, _ = w.Write(recorder.Body.Bytes())
			})
			observer := &custodyObserver{}
			ctx := ports.WithNativeOperationObserver(context.Background(), observer)
			checks := 0
			_, err := New(fake.socket, "projects").ReconcileRunningPublicationsIf(ctx, "p", "yard", "original", kind, ownerpublication.CoreContracts(), []netip.Addr{netip.MustParseAddr("100.127.255.254")}, func() error { checks++; return nil })
			if mode == "restore" || mode == "container" {
				if err != nil {
					t.Fatal(err)
				}
				actual, _ := New(fake.socket, "projects").Instance(context.Background(), "p", "yard")
				if actual.Config[ownerpublication.Prefix+previewroute.DeviceName] != "" || !maps.Equal(actual.Devices[previewroute.DeviceName], device) || actual.Config["foreign"] != "keep" || checks != 1 || fake.instanceIfMatch != "original-etag" {
					t.Fatal("restore lost original state", actual, checks)
				}
			} else if err == nil || mode != "missing-ack" && !errors.Is(err, domain.ErrPlanStale) {
				t.Fatal("invalid finalization outcome", err)
			}
			before := mode == "wrong-kind" || mode == "replacement" || mode == "intent" || mode == "unmanaged" || mode == "uninitialized" || mode == "stopped" || mode == "missing-etag"
			if before && (!errors.Is(err, domain.ErrPlanStale) || fake.instanceIfMatch != "" || checks != 0) {
				t.Fatal("drift reached write", err, checks)
			}
			if observer.uncertain != (mode == "missing-ack") {
				t.Fatal("ack outcome fabricated", mode, observer, err)
			}
			if len(fake.powerCalls) != 0 {
				t.Fatal("publication changed power")
			}
		})
	}
}

// This real SDK HTTP matrix isolates exact kind/UUID, metadata and ETag fences.
// The owning runtime test also exercises its native operation acknowledgements.
func TestOriginalConfigRetainsKindMetadataAndUUID(t *testing.T) {
	for _, mode := range []string{"vm", "container", "wrong-kind", "replacement", "post-replacement", "metadata-drift", "missing-etag", "effective-replacement"} {
		t.Run(mode, func(t *testing.T) {
			fake := newNetworkIncusServer(t)
			kind := domain.YardVM
			apiKind := "virtual-machine"
			if mode == "container" {
				kind = domain.YardContainer
				apiKind = "container"
			}
			if mode == "wrong-kind" {
				kind = domain.YardContainer
			}
			cfg := map[string]string{"volatile.uuid": "original", "user.subyard.managed": "true", "user.subyard.initialized": "false", "user.subyard.desired_power": "running", "foreign": "keep"}
			expanded := maps.Clone(cfg)
			etag := "original-etag"
			switch mode {
			case "replacement":
				cfg["volatile.uuid"] = "foreign"
				expanded = maps.Clone(cfg)
			case "metadata-drift":
				cfg["user.subyard.desired_power"] = "stopped"
				expanded = maps.Clone(cfg)
			case "missing-etag":
				etag = ""
			case "effective-replacement":
				expanded["volatile.uuid"] = "foreign"
			}
			fake.instances["p/yard"] = fakeInstance{ETag: etag, Data: map[string]any{"name": "yard", "type": apiKind, "status": "Running", "config": cfg, "expanded_config": expanded, "devices": map[string]map[string]string{}, "expanded_devices": map[string]map[string]string{}}}
			reads := 0
			fake.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/1.0/instances/yard" {
					reads++
					if mode == "post-replacement" && reads == 2 {
						fake.mu.Lock()
						item := fake.instances["p/yard"]
						setPublicationFixtureConfig(item, "volatile.uuid", "foreign")

						fake.instances["p/yard"] = item
						fake.mu.Unlock()
					}
				}
				fake.ServeHTTP(w, r)
			})
			err := New(fake.socket, "projects").SetOriginalInstanceConfigIf(context.Background(), "p", "yard", "original", kind, map[string]string{"user.subyard.managed": "true", "user.subyard.initialized": "false", "user.subyard.desired_power": "running"}, map[string]string{"user.subyard.initialized": "true"})
			valid := mode == "vm" || mode == "container"
			if valid && err != nil || !valid && !errors.Is(err, domain.ErrPlanStale) {
				t.Fatal("original config fence", mode, err)
			}
			if (fake.instanceIfMatch != "") != (valid || mode == "post-replacement") {
				t.Fatal("unexpected original config write", mode, fake.instanceIfMatch)
			}
			if valid {
				actual, _ := New(fake.socket, "projects").Instance(context.Background(), "p", "yard")
				if actual.Type != kind || actual.Config["user.subyard.initialized"] != "true" || actual.Config["foreign"] != "keep" {
					t.Fatal(actual)
				}
			}
		})
	}
}

func setPublicationFixtureConfig(item fakeInstance, key, value string) {
	for _, field := range []string{"config", "expanded_config"} {
		switch cfg := item.Data[field].(type) {
		case map[string]string:
			cfg[key] = value
		case api.ConfigMap:
			cfg[key] = value
		}
	}
}

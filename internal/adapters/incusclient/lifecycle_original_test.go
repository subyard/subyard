package incusclient

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerpublication"
)

func TestOriginalLifecyclePowerKeepsSDKForceTimeoutAndUUID(t *testing.T) {
	originalLifecyclePowerMatrix(t, domain.YardVM, false)
}
func TestOriginalLifecycleKindPowerKeepsSDKForceTimeoutAndUUID(t *testing.T) {
	for _, kind := range []domain.YardKind{domain.YardVM, domain.YardContainer} {
		t.Run(string(kind), func(t *testing.T) { originalLifecyclePowerMatrix(t, kind, true) })
	}
}
func originalLifecyclePowerMatrix(t *testing.T, kind domain.YardKind, original bool) {
	for _, mode := range []string{"start", "stop", "graceful-stop", "replacement-before", "replacement-after", "missing", "wrong-kind"} {
		t.Run(mode, func(t *testing.T) {
			fake := newNetworkIncusServer(t)
			apiKind := "virtual-machine"
			if kind == domain.YardContainer {
				apiKind = "container"
			}
			if mode == "wrong-kind" {
				if apiKind == "container" {
					apiKind = "virtual-machine"
				} else {
					apiKind = "container"
				}
			}
			config := map[string]string{"volatile.uuid": "original", vmCPUWeightKey: "invalid"}
			fake.instances["subyard/yard"] = fakeInstance{Data: map[string]any{"name": "yard", "type": apiKind, "status": "Stopped", "config": config, "expanded_config": maps.Clone(config)}}
			reads := 0
			fake.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/1.0/instances/yard" {
					reads++
					if mode == "replacement-before" && reads == 1 || mode == "replacement-after" && reads == 2 {
						fake.mu.Lock()
						data := fake.instances["subyard/yard"]
						data.Data["config"].(map[string]string)["volatile.uuid"] = "replacement"
						data.Data["expanded_config"].(map[string]string)["volatile.uuid"] = "replacement"
						fake.instances["subyard/yard"] = data
						fake.mu.Unlock()
					}
				}
				fake.ServeHTTP(w, r)
			})
			action, uuid := "start", "original"
			if mode == "stop" || mode == "graceful-stop" {
				action = "stop"
			}
			if mode == "missing" {
				uuid = ""
			}
			client := New(fake.socket, "projects")
			force := mode == "stop"
			var err error
			if original {
				err = client.SetOriginalInstancePowerIf(context.Background(), "subyard", "yard", action, force, uuid, kind)
			} else {
				err = client.SetInstancePowerIf(context.Background(), "subyard", "yard", action, force, uuid)
			}
			if mode == "start" || mode == "stop" || mode == "graceful-stop" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("original UUID was not enforced: %v", err)
			}
			fake.mu.Lock()
			calls := append([]fakePowerCall(nil), fake.powerCalls...)
			fake.mu.Unlock()
			want := 1
			if mode == "replacement-before" || mode == "missing" || mode == "wrong-kind" {
				want = 0
			}
			if len(calls) != want {
				t.Fatalf("foreign or eager CPU fallback power: %+v", calls)
			}
			if want == 1 && (calls[0].Timeout != -1 || calls[0].Force != force || calls[0].Action != action) {
				t.Fatalf("changed physical SDK semantics: %+v", calls)
			}
		})
	}
}

func TestOriginalPublicationChecksFetchedAndFinalUUID(t *testing.T) {
	for _, mode := range []string{"original", "replacement-before", "replacement-after", "missing"} {
		t.Run(mode, func(t *testing.T) {
			fake := newNetworkIncusServer(t)
			config := map[string]string{"volatile.uuid": "original"}
			fake.instances["subyard/yard"] = fakeInstance{ETag: "original-etag", Data: map[string]any{"name": "yard", "type": "virtual-machine", "status": "Stopped", "config": config, "expanded_config": maps.Clone(config)}}
			reads := 0
			fake.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/1.0/instances/yard" {
					reads++
					if mode == "replacement-before" && reads == 1 || mode == "replacement-after" && reads == 2 {
						fake.mu.Lock()
						data := fake.instances["subyard/yard"]
						data.Data["config"].(map[string]string)["volatile.uuid"] = "replacement"
						data.Data["expanded_config"].(map[string]string)["volatile.uuid"] = "replacement"
						fake.instances["subyard/yard"] = data
						fake.mu.Unlock()
					}
				}
				fake.ServeHTTP(w, r)
			})
			uuid := "original"
			if mode == "missing" {
				uuid = ""
			}
			_, err := New(fake.socket, "projects").ReconcilePublicationsIf(context.Background(), "subyard", "yard", uuid, nil, nil)
			if mode == "original" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, domain.ErrPlanStale) {
				t.Fatal("foreign publication accepted", err)
			}
			if fake.instanceIfMatch != "" {
				t.Fatal("read-only publication changed instance")
			}
		})
	}
}

func TestOriginalPublicationUpdatePreservesETagAndForeignState(t *testing.T) {
	originalPublicationUpdateMatrix(t, domain.YardVM, false)
}
func TestOriginalLifecycleKindPublicationUpdatePreservesETagAndForeignState(t *testing.T) {
	for _, kind := range []domain.YardKind{domain.YardVM, domain.YardContainer} {
		t.Run(string(kind), func(t *testing.T) { originalPublicationUpdateMatrix(t, kind, true) })
	}
}
func originalPublicationUpdateMatrix(t *testing.T, kind domain.YardKind, original bool) {
	for _, mode := range []string{"original", "replacement", "missing-etag", "wrong-kind"} {
		t.Run(mode, func(t *testing.T) {
			fake := newNetworkIncusServer(t)
			apiKind := "virtual-machine"
			if kind == domain.YardContainer {
				apiKind = "container"
			}
			if mode == "wrong-kind" {
				if apiKind == "container" {
					apiKind = "virtual-machine"
				} else {
					apiKind = "container"
				}
			}
			device := map[string]string{"type": "proxy", "bind": "host", "listen": "tcp:192.0.2.12:8080", "connect": "tcp:127.0.0.1:80"}
			cfg := map[string]string{"volatile.uuid": "original", "user.fixture.owned": "v1", "foreign": "keep"}
			if mode == "replacement" {
				cfg["volatile.uuid"] = "replacement"
			}
			devices := map[string]map[string]string{"optional": device, "root": {"type": "disk", "path": "/"}}
			etag := "original-etag"
			if mode == "missing-etag" {
				etag = ""
			}
			fake.instances["p/yard"] = fakeInstance{ETag: etag, Data: map[string]any{"name": "yard", "type": apiKind, "status": "Stopped", "config": cfg, "expanded_config": maps.Clone(cfg), "devices": devices, "expanded_devices": devices}}
			contract := ownerpublication.Contract{Device: "optional", Key: "user.fixture.owned", Owns: func(marker string, actual map[string]string) bool {
				return marker == "v1" && maps.Equal(actual, device)
			}}
			checks := 0
			client := New(fake.socket, "projects")
			check := func() error { checks++; return nil }
			var err error
			if original {
				_, err = client.ReconcileOriginalPublicationsIf(context.Background(), "p", "yard", "original", kind, []ownerpublication.Contract{contract}, nil, check)
			} else {
				_, err = client.ReconcilePublicationsIf(context.Background(), "p", "yard", "original", []ownerpublication.Contract{contract}, nil, check)
			}
			if mode != "original" {
				if !errors.Is(err, domain.ErrPlanStale) || fake.instanceIfMatch != "" || checks != 0 {
					t.Fatal("foreign publication write", err, fake.instanceIfMatch, checks)
				}
				return
			}
			observed, readErr := New(fake.socket, "projects").Instance(context.Background(), "p", "yard")
			if err != nil || readErr != nil || checks != 1 || fake.instanceIfMatch != "original-etag" || observed.Config["foreign"] != "keep" || observed.Devices["root"]["path"] != "/" || observed.Devices["optional"] != nil || observed.Config[ownerpublication.Prefix+"optional"] == "" {
				t.Fatal("original atomic publication changed", err, readErr, observed)
			}
		})
	}
}

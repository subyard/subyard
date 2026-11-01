package incusclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerpublication"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/previewroute"
	"github.com/lxc/incus/v6/shared/api"
)

func TestPublicationDeliveryCASOwnsOnlyExactSidecar(t *testing.T) {
	for _, mode := range []string{"publish", "ack", "cleanup", "changed", "uuid", "stop", "retired", "device", "etag", "consumer", "missing-acceptance", "restored"} {
		t.Run(mode, func(t *testing.T) {
			fake := newNetworkIncusServer(t)
			cfg := map[string]string{"volatile.uuid": "original", "user.subyard.managed": "true", "user.subyard.initialized": "true", "user.subyard.desired_power": "running", previewroute.Key: "v1:100.127.255.254:23456", "foreign": "keep"}
			device := previewroute.Device("100.127.255.254", "23456")
			receipt, _ := json.Marshal(ownerpublication.Receipt{Version: 1, Ownership: cfg[previewroute.Key], Device: device})
			key := ownerpublication.Prefix + previewroute.DeviceName
			cfg[key] = string(receipt)
			i := ports.InstanceInfo{Project: "p", Name: "yard", Type: domain.YardVM, Status: "Running", LocalConfig: maps.Clone(cfg), Config: maps.Clone(cfg), LocalDevices: map[string]map[string]string{}, Devices: map[string]map[string]string{}}
			consumer := ownerpublication.Consumer{EngineDigest: strings.Repeat("a", 64), UnitDigest: strings.Repeat("b", 64), ContractsDigest: strings.Repeat("c", 64), FilesDigest: strings.Repeat("d", 64), Generations: []ports.NativeResourceGeneration{{Role: "engine", Value: "0"}, {Role: "service", Value: "0"}, {Role: "publication", Value: "0"}}}
			contracts := ownerpublication.CoreContracts()
			h, e := ownerpublication.NewHandoff(ownerpublication.OriginalTarget{Project: "p", Name: "yard", UUID: "original", Kind: domain.YardVM, Desired: domain.PowerRunning}, previewroute.DeviceName, strings.Repeat("e", 64), strings.Repeat("f", 64), strings.Repeat("a", 64), consumer, i, contracts)
			if e != nil {
				t.Fatal(e)
			}
			before, after := "", h.Encode()
			status, etag := "Running", "exact-etag"
			devices := map[string]map[string]string{}
			if mode == "ack" || mode == "restored" {
				before = h.Encode()
				h.Ack = h.ID
				after = h.Encode()
				cfg[h.Key()] = before
			}
			if mode == "cleanup" {
				h.Ack = h.ID
				before = h.Encode()
				after = ""
				cfg[h.Key()] = before
			}
			switch mode {
			case "changed":
				cfg[h.Key()] = "foreign"
			case "uuid":
				cfg["volatile.uuid"] = "other"
			case "stop":
				status = "Stopped"
			case "retired":
				contracts[0].CleanupOnly = true
			case "device":
				devices[previewroute.DeviceName] = maps.Clone(device)
				devices[previewroute.DeviceName]["listen"] = "tcp:192.0.2.1:123"
			case "etag":
				etag = ""
			case "restored":
				delete(cfg, key)
				devices[previewroute.DeviceName] = device
			}
			fake.instances["p/yard"] = fakeInstance{ETag: etag, Data: map[string]any{"name": "yard", "type": "virtual-machine", "status": status, "config": cfg, "expanded_config": maps.Clone(cfg), "devices": devices, "expanded_devices": maps.Clone(devices)}}
			fake.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "missing-acceptance" && r.Method == http.MethodPut {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusAccepted)
					json.NewEncoder(w).Encode(map[string]any{"type": "async", "status": "Operation created", "status_code": 100, "metadata": map[string]any{"class": "task", "status": "Running", "status_code": 103}})
					return
				}
				publicationDeliveryTestResponse(fake, w, r)
			})
			observer := &custodyObserver{}
			ctx := ports.WithNativeOperationObserver(context.Background(), observer)
			guards := 0
			err := New(fake.socket, "projects").ExchangePublicationHandoff(ctx, h, contracts, before, after, func() error {
				guards++
				if mode == "consumer" {
					return domain.ErrPlanStale
				}
				return nil
			})
			valid := mode == "publish" || mode == "ack" || mode == "cleanup" || mode == "restored"
			if (err == nil) != valid {
				t.Fatal(mode, err)
			}
			if !valid && mode != "missing-acceptance" && mode != "device" && !errors.Is(err, domain.ErrPlanStale) {
				t.Fatal(err)
			}
			if valid {
				actual, e := New(fake.socket, "projects").Instance(context.Background(), "p", "yard")
				if e != nil || actual.Config[h.Key()] != after || actual.Config["foreign"] != "keep" || actual.Config[key] != cfg[key] || !maps.EqualFunc(actual.LocalDevices, devices, maps.Equal) || fake.instanceIfMatch != "exact-etag" || guards != 1 || len(observer.accepted) != 1 || len(observer.terminal) != 1 {
					t.Fatal("delivery changed receipt/device or lost SDK custody", actual, e, observer)
				}
			}
			if !valid && mode != "missing-acceptance" && fake.instanceIfMatch != "" {
				t.Fatal("invalid delivery reached a PUT", mode)
			}
			if observer.uncertain != (mode == "missing-acceptance") || len(fake.powerCalls) != 0 {
				t.Fatal("fabricated completion or power mutation", observer)
			}
		})
	}
}

// A partial original delivery cannot claim callback completion. The independent
// worker retains normal v1 restoration authority after the original helper fails.
func TestPublicationDeliveryPartialCASRecoversOnlyThroughV1(t *testing.T) {
	for _, mode := range []string{"conflict", "lost-acceptance"} {
		t.Run(mode, func(t *testing.T) {
			fake := newNetworkIncusServer(t)
			cfg := map[string]string{"volatile.uuid": "original", "user.subyard.managed": "true", "user.subyard.initialized": "true", "user.subyard.desired_power": "running"}
			contracts := []ownerpublication.Contract{}
			for n := 0; n < 2; n++ {
				c := ownerpublication.CoreContracts()[0]
				c.Device = fmt.Sprintf("optional-%d", n)
				c.Key = fmt.Sprintf("user.fixture.%d", n)
				contracts = append(contracts, c)
				cfg[c.Key] = fmt.Sprintf("v1:100.127.255.254:%d", 23456+n)
				raw, _ := json.Marshal(ownerpublication.Receipt{Version: 1, Ownership: cfg[c.Key], Device: previewroute.Device("100.127.255.254", fmt.Sprint(23456+n))})
				cfg[ownerpublication.Prefix+c.Device] = string(raw)
			}
			snapshot := ports.InstanceInfo{Project: "p", Name: "yard", Type: domain.YardVM, Status: "Running", Config: maps.Clone(cfg), LocalConfig: maps.Clone(cfg), Devices: map[string]map[string]string{}, LocalDevices: map[string]map[string]string{}}
			consumer := ownerpublication.Consumer{EngineDigest: strings.Repeat("a", 64), UnitDigest: strings.Repeat("b", 64), ContractsDigest: strings.Repeat("c", 64), FilesDigest: strings.Repeat("d", 64), Generations: []ports.NativeResourceGeneration{{Role: "engine", Value: "0"}, {Role: "service", Value: "0"}, {Role: "publication", Value: "0"}}}
			requests := []ownerpublication.Handoff{}
			for _, c := range contracts {
				h, e := ownerpublication.NewHandoff(ownerpublication.OriginalTarget{Project: "p", Name: "yard", UUID: "original", Kind: domain.YardVM, Desired: domain.PowerRunning}, c.Device, strings.Repeat("e", 64), strings.Repeat("f", 64), strings.Repeat("a", 64), consumer, snapshot, contracts)
				if e != nil {
					t.Fatal(e)
				}
				requests = append(requests, h)
			}
			fake.instances["p/yard"] = fakeInstance{ETag: "original", Data: map[string]any{"name": "yard", "type": "virtual-machine", "status": "Running", "config": maps.Clone(cfg), "expanded_config": maps.Clone(cfg), "devices": map[string]map[string]string{}, "expanded_devices": map[string]map[string]string{}}}
			puts := 0
			fake.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut {
					puts++
					if puts == 2 && mode == "conflict" {
						fake.mu.Lock()
						instance := fake.instances["p/yard"]
						current := maps.Clone(instance.Data["config"].(api.ConfigMap))
						current[requests[1].Key()] = "foreign"
						instance.Data["config"] = current
						instance.Data["expanded_config"] = maps.Clone(current)
						instance.ETag += "-foreign"
						fake.instances["p/yard"] = instance
						fake.mu.Unlock()
						writeNetworkIncusError(w, http.StatusPreconditionFailed, "changed before second delivery")
						return
					}
					if puts == 2 && mode == "lost-acceptance" {
						recorder := httptest.NewRecorder()
						fake.ServeHTTP(recorder, r)
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusAccepted)
						_ = json.NewEncoder(w).Encode(map[string]any{"type": "async", "status": "Operation created", "status_code": 100, "metadata": map[string]any{"class": "task", "status": "Running", "status_code": 103}})
						return
					}
				}
				publicationDeliveryTestResponse(fake, w, r)
			})
			client := New(fake.socket, "projects")
			observer := &custodyObserver{}
			ctx := ports.WithNativeOperationObserver(context.Background(), observer)
			starts := 0
			err := ownerpublication.Deliver(ctx, requests, func(call context.Context) (ports.InstanceInfo, error) { return client.Instance(call, "p", "yard") }, func(context.Context) error { return nil }, func(call context.Context, h ownerpublication.Handoff, before, after string) error {
				return client.ExchangePublicationHandoff(call, h, contracts, before, after, nil)
			}, func(context.Context) error { starts++; return nil })
			if err == nil || starts != 0 || len(observer.accepted) != 1 || len(observer.terminal) != 1 || observer.uncertain != (mode == "lost-acceptance") {
				t.Fatal("partial producer fabricated completion", err, starts, observer)
			}
			partial, e := client.Instance(context.Background(), "p", "yard")
			if e != nil {
				t.Fatal(e)
			}
			for _, h := range requests {
				if partial.LocalConfig[h.ReceiptKey] != h.Receipt {
					t.Fatal("partial CAS changed v1 authority")
				}
			}
			if partial.LocalConfig[requests[0].Key()] != requests[0].Encode() {
				t.Fatal("first durable delivery lost")
			}
			worker := ownerpublication.PublicationWorker{Contracts: contracts, Inventory: func(call context.Context) ([]ports.InstanceInfo, error) {
				i, e := client.Instance(call, "p", "yard")
				return []ports.InstanceInfo{i}, e
			}, Consumer: func(context.Context, ownerpublication.Consumer) error { return nil }, Ack: func(call context.Context, h ownerpublication.Handoff, before, after string) error {
				return client.ExchangePublicationHandoff(call, h, contracts, before, after, nil)
			}, Restore: func(call context.Context, i ports.InstanceInfo) ([]string, error) {
				return client.ReconcileRunningPublicationsIf(call, "p", "yard", "original", domain.YardVM, contracts, []netip.Addr{netip.MustParseAddr("100.127.255.254")})
			}}
			if waiting, e := worker.Run(ports.WithoutNativeOperationObserver(ctx)); e != nil || waiting {
				t.Fatal("normal v1 recovery failed", e, waiting)
			}
			actual, e := client.Instance(context.Background(), "p", "yard")
			if e != nil {
				t.Fatal(e)
			}
			for _, c := range contracts {
				if actual.LocalConfig[ownerpublication.Prefix+c.Device] != "" || !c.Owns(actual.LocalConfig[c.Key], actual.LocalDevices[c.Device]) || !maps.Equal(actual.LocalDevices[c.Device], actual.Devices[c.Device]) {
					t.Fatal("v1 recovery lost exact original ownership")
				}
			}
			if mode == "conflict" && actual.LocalConfig[requests[1].Key()] != "foreign" {
				t.Fatal("foreign sidecar was overwritten")
			}
			if len(fake.powerCalls) != 0 || starts != 0 {
				t.Fatal("publication recovery performed power or producer callback")
			}
		})
	}
}

func publicationDeliveryTestResponse(fake *networkIncusServer, w http.ResponseWriter, r *http.Request) {
	recorder := httptest.NewRecorder()
	fake.ServeHTTP(recorder, r)
	var body map[string]any
	if json.Unmarshal(recorder.Body.Bytes(), &body) == nil && (r.Method == http.MethodPut || strings.HasPrefix(r.URL.Path, "/1.0/operations/")) {
		if meta, ok := body["metadata"].(map[string]any); ok {
			meta["resources"] = map[string][]string{"instances": {"/1.0/instances/yard?project=p"}}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(recorder.Code)
		_ = json.NewEncoder(w).Encode(body)
		return
	}
	for k, v := range recorder.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(recorder.Code)
	_, _ = w.Write(recorder.Body.Bytes())
}

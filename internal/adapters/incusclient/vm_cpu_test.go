package incusclient

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"os"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

func TestVMCPUMetadataIsLocalOwnedAndTyped(t *testing.T) {
	for _, scenario := range []string{"unset", "stopped", "malformed", "out of range", "inherited", "foreign", "container"} {
		t.Run(scenario, func(t *testing.T) {
			server, err := testkit.NewIncusServer(testkit.TempDir(t))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = server.Close() })
			local := map[string]string{vmCPUWeightKey: "1000", "user.subyard.managed": "true", "user.subyard.name": "synthetic"}
			effective := maps.Clone(local)
			kind := "virtual-machine"
			switch scenario {
			case "unset":
				delete(local, vmCPUWeightKey)
				delete(effective, vmCPUWeightKey)
			case "malformed":
				local[vmCPUWeightKey] = "-5"
				effective[vmCPUWeightKey] = "-5"
			case "out of range":
				local[vmCPUWeightKey] = "10001"
				effective[vmCPUWeightKey] = "10001"
			case "inherited":
				delete(local, vmCPUWeightKey)
			case "foreign":
				delete(local, "user.subyard.managed")
			case "container":
				kind = "container"
			}
			server.SetInstance("subyard", "yard", map[string]any{"name": "yard", "type": kind, "status": "Stopped", "config": local, "expanded_config": effective})
			ready, err := New(server.SocketPath).VMCPUConverged(context.Background(), "subyard", "yard", false)
			want := scenario == "unset" || scenario == "stopped"
			if ready != want || (err == nil) != want {
				t.Fatalf("ready=%t err=%v", ready, err)
			}
			if len(server.PowerCalls()) != 0 {
				t.Fatal("readiness changed power")
			}
		})
	}
}

func TestNativeStartStopsVMWhenPersistedSchedulingIsInvalid(t *testing.T) {
	for _, path := range []string{"boot", "network"} {
		t.Run(path, func(t *testing.T) {
			server, err := testkit.NewIncusServer(testkit.TempDir(t))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = server.Close() })
			config := map[string]string{vmCPUWeightKey: "invalid"}
			server.SetInstance("subyard", "yard", map[string]any{"name": "yard", "type": "virtual-machine", "status": "Stopped", "config": config, "expanded_config": config})
			client := New(server.SocketPath)
			if path == "boot" {
				err = client.SetInstancePower(context.Background(), "subyard", "yard", "start", false)
			} else {
				err = client.NetworkPower(context.Background(), yardnetwork.Yard{Project: "subyard", Instance: "yard"}, "start")
			}
			calls := server.PowerCalls()
			if err == nil || len(calls) != 2 || calls[0].Action != "start" || calls[1].Action != "stop" {
				t.Fatalf("unsafe start: calls=%+v err=%v", calls, err)
			}
		})
	}
}

func TestNetworkPowerIfKeepsUUIDAcrossCPUReadbackAndFallback(t *testing.T) {
	for _, mode := range []string{"original", "before-power", "before-cpu", "before-fallback", "readback", "stop-readback"} {
		t.Run(mode, func(t *testing.T) {
			fake := newNetworkIncusServer(t)
			config := map[string]string{"volatile.uuid": "original"}
			if mode == "before-fallback" {
				config[vmCPUWeightKey] = "invalid"
			}
			fake.instances["subyard/yard"] = fakeInstance{Data: map[string]any{"name": "yard", "type": "virtual-machine", "status": "Running", "config": config, "expanded_config": maps.Clone(config)}}
			reads := 0
			fake.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/1.0/instances/yard" {
					reads++
					replace := mode == "before-power" && reads == 1 || mode == "before-cpu" && reads == 2 || mode == "before-fallback" && reads == 3 || mode == "readback" && reads == 3 || mode == "stop-readback" && reads == 2
					if replace {
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
			action := "start"
			if mode == "stop-readback" {
				action = "stop"
			}
			err := New(fake.socket, "projects").NetworkPowerIf(context.Background(), yardnetwork.Yard{Project: "subyard", Instance: "yard"}, action, "original")
			fake.mu.Lock()
			calls := append([]fakePowerCall(nil), fake.powerCalls...)
			fake.mu.Unlock()
			if mode == "original" {
				if err != nil || len(calls) != 1 {
					t.Fatal(err, calls)
				}
				return
			}
			if !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("replacement accepted: %v", err)
			}
			want := 1
			if mode == "before-power" {
				want = 0
			}
			if len(calls) != want {
				t.Fatalf("replacement caused foreign fallback/power: %+v", calls)
			}
		})
	}
}

func TestVMCPUNativeEntryRequiresOriginalUUIDAndOwnerStdin(t *testing.T) {
	fake := newNetworkIncusServer(t)
	config := map[string]string{"volatile.uuid": "original"}
	fake.instances["subyard/yard"] = fakeInstance{Data: map[string]any{"name": "yard", "type": "virtual-machine", "status": "Stopped", "config": config, "expanded_config": maps.Clone(config)}}
	home := testkit.TempDir(t)
	_, release, err := uninstall.OwnerLock(context.Background(), home, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	owner, err := uninstall.BorrowOwnerLock(home)
	if err != nil || owner == nil {
		t.Fatal(err)
	}
	defer owner.Close()
	foreign, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	original := os.Stdin
	defer func() { os.Stdin = original }()
	client := New(fake.socket, "projects")
	for _, mode := range []string{"original", "uuid", "foreign", "missing"} {
		t.Run(mode, func(t *testing.T) {
			uuid := "original"
			os.Stdin = owner
			switch mode {
			case "uuid":
				uuid = "replacement"
			case "foreign":
				os.Stdin = foreign
			case "missing":
				os.Stdin = nil
			}
			ready, err := client.VMCPUConvergedNative(context.Background(), "subyard", "yard", uuid, uint32(os.Getuid()), home)
			if mode == "original" {
				if !ready || err != nil {
					t.Fatal(ready, err)
				}
			} else if ready || err == nil {
				t.Fatal("unbound privileged entry accepted", mode)
			}
		})
	}
}

package incusclient

import (
	"context"
	"maps"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
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

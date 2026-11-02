package testvmsruntime

import (
	"context"
	"io"
	"testing"
)

func TestHostMemoryRecoverableStatesConverge(t *testing.T) {
	for _, scenario := range []string{"absent", "converged", "pending-absent", "pending-device", "final-absent"} {
		t.Run(scenario, func(t *testing.T) {
			config := map[string]string{}
			local, expanded := map[string]map[string]string{}, map[string]map[string]string{}
			if scenario != "absent" {
				config[hostMemoryOwnerKey] = hostMemoryOwnerVersion
				local[hostMemoryDevice] = hostMemoryDeviceSpec()
				expanded[hostMemoryDevice] = hostMemoryDeviceSpec()
			}
			switch scenario {
			case "pending-absent", "pending-device":
				config[hostMemoryOwnerKey] = "pending:v1"
				if scenario == "pending-absent" {
					delete(local, hostMemoryDevice)
					delete(expanded, hostMemoryDevice)
				}
			case "final-absent":
				delete(local, hostMemoryDevice)
				delete(expanded, hostMemoryDevice)
			}
			memory := &hostMemoryInstance{Config: config, Devices: local, ExpandedDevices: expanded}
			backend := fixtureBackend(t)
			backend.Runner = &fakeRunner{handler: func(_ string, arguments, _ []string, _ io.Reader) ([]byte, []byte, error) {
				body, handled := memory.handle(t, arguments)
				if !handled {
					t.Fatalf("unexpected recovery operation: %v", arguments)
				}
				return body, nil, nil
			}}
			if err := backend.reconcileHostMemory(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			if err := HostMemoryPolicy(config, local, expanded, true); err != nil {
				t.Fatal("recoverable telemetry state did not converge", err)
			}
		})
	}
}

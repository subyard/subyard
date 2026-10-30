package testvmsruntime

import (
	"context"
	"io"
	"maps"
	"testing"
)

func TestHostMemoryAdoptionPolicy(t *testing.T) {
	for _, scenario := range []string{"absent", "converged", "pending-absent", "pending-device", "final-absent", "foreign-marker", "unmarked", "source", "writable", "inherited", "divergent", "local-path", "expanded-path", "unselected"} {
		t.Run(scenario, func(t *testing.T) {
			config := map[string]string{}
			local, expanded := map[string]map[string]string{}, map[string]map[string]string{}
			enabled := scenario != "unselected"
			if scenario != "absent" && scenario != "inherited" && scenario != "local-path" && scenario != "expanded-path" {
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
			case "foreign-marker":
				config[hostMemoryOwnerKey] = "v2"
			case "unmarked":
				delete(config, hostMemoryOwnerKey)
			case "source":
				local[hostMemoryDevice]["source"] = "/tmp/meminfo"
				expanded[hostMemoryDevice] = maps.Clone(local[hostMemoryDevice])
			case "writable":
				delete(local[hostMemoryDevice], "readonly")
				expanded[hostMemoryDevice] = maps.Clone(local[hostMemoryDevice])
			case "inherited":
				expanded[hostMemoryDevice] = hostMemoryDeviceSpec()
			case "divergent":
				expanded[hostMemoryDevice]["source"] = "/tmp/meminfo"
			case "local-path":
				local["foreign"] = hostMemoryDeviceSpec()
			case "expanded-path":
				expanded["foreign"] = hostMemoryDeviceSpec()
			}
			want := scenario == "absent" || scenario == "converged" || scenario == "pending-absent" || scenario == "pending-device" || scenario == "final-absent"
			if err := HostMemoryAdoptionPolicy(config, local, expanded, enabled); (err == nil) != want {
				t.Fatalf("eligible=%t: %v", want, err)
			}
			if scenario == "absent" && HostMemoryPolicy(config, local, expanded, true) == nil {
				t.Fatal("strict readiness admitted absent selected telemetry")
			}
			if want {
				// Every admitted incomplete state must be finishable by the
				// existing owning reconciler, without lifecycle operations.
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
					t.Fatal("admitted telemetry state did not converge", err)
				}
			}
		})
	}
}

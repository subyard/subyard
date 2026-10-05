package testvmsruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestSlotCountRepairRequiresAnOtherwiseConvergedBackend(t *testing.T) {
	for _, scenario := range []string{"expand", "shrink", "current", "invalid count", "invalid desired count", "engine drift", "budget drift", "memory source missing", "route drift", "worker drift", "marker race"} {
		t.Run(scenario, func(t *testing.T) {
			backend := fixtureBackend(t)
			backend.Environment["E2E_VM_SLOT_COUNT"] = "3"
			state, err := backend.state()
			if err != nil {
				t.Fatal(err)
			}
			marker := state.marker
			backend.DesiredPower = "running"
			key := fixturePublicKey(t)
			memory := backendMemoryFixture(true)
			reads := 0
			backend.Runner = &fakeRunner{handler: func(_ string, arguments, _ []string, _ io.Reader) ([]byte, []byte, error) {
				if body, handled := memory.handle(t, arguments); handled {
					return body, nil, nil
				}
				switch strings.Join(arguments, " ") {
				case "config get yard-test user.subyard.test_vms_revision --project subyard-test":
					reads++
					if scenario == "marker race" && reads > 1 {
						return []byte("changed"), nil, nil
					}
					return []byte(marker + "\n"), nil, nil
				case "list yard-test --project subyard-test -f csv -c s":
					return []byte("RUNNING\n"), nil, nil
				case "exec yard-test --project subyard-test -- ip -4 -o route show default":
					return []byte("default via 10.10.0.1 dev eth0\n"), nil, nil
				case "exec yard-test --project subyard-test -- ip -4 -o address show dev eth0 scope global":
					return []byte("2: eth0 inet 10.10.0.4/24 scope global eth0\n"), nil, nil
				case "exec yard-test --project subyard-test -- cat /etc/ssh/ssh_host_ed25519_key.pub":
					return []byte(key + " fixture\n"), nil, nil
				case "exec yard-test --project subyard-test --env WANT_ENABLED=1 --env WANT_ENGINE_HASH=" + state.engineHash +
					" -- " + DefaultInstalledPath + " _test-vms-worker doctor":
					if scenario == "worker drift" {
						return nil, nil, errors.New("worker drift")
					}
					return nil, nil, nil
				default:
					return nil, nil, fmt.Errorf("unexpected call: %v", arguments)
				}
			}}
			if err := backend.publishRoute(context.Background(), state); err != nil {
				t.Fatal(err)
			}
			backend.Environment["E2E_VM_SLOT_COUNT"] = "4"
			switch scenario {
			case "shrink":
				backend.Environment["E2E_VM_SLOT_COUNT"] = "2"
			case "current":
				backend.Environment["E2E_VM_SLOT_COUNT"] = "3"
			case "invalid count":
				parts := strings.Split(marker, ":")
				parts[len(parts)-8] = "0"
				marker = strings.Join(parts, ":")
			case "invalid desired count":
				backend.Environment["E2E_VM_SLOT_COUNT"] = "0"
			case "engine drift":
				testkit.WriteFile(t, backend.Dispatcher, []byte("changed engine"), 0o755)
			case "budget drift":
				backend.Environment["E2E_CACHE_BUDGET"] = "1GiB"
			case "memory source missing":
				delete(memory.Devices, hostMemoryDevice)
				delete(memory.ExpandedDevices, hostMemoryDevice)
			case "route drift":
				testkit.WriteFile(t, filepath.Join(state.clientDirectory, "current", "route.tsv"),
					[]byte("subyard-e2e-route-v1\nhostname\t10.10.0.5\nport\t22\nhost_key_alias\tsubyard-e2e-bastion\n"), 0o644)
			}
			wantedCount := backend.Environment["E2E_VM_SLOT_COUNT"]
			observed, err := backend.ObserveSlotCountRepair(context.Background())
			if scenario == "invalid desired count" {
				if err == nil || observed.State != ports.RuntimeStateAbsent {
					t.Fatalf("invalid desired count admitted: %#v err=%v", observed, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			admitted := scenario == "expand" || scenario == "shrink" || scenario == "current"
			if !admitted {
				if observed.State != ports.RuntimeStateAbsent || observed.Actual != "" || observed.Desired != "" {
					t.Fatalf("unrelated drift admitted: %#v", observed)
				}
			} else if scenario == "current" {
				if observed.State != ports.RuntimeStateCurrent || observed.Actual != observed.Desired || len(observed.Desired) != 64 {
					t.Fatalf("current backend: %#v", observed)
				}
			} else if observed.State != ports.RuntimeStateStale || observed.Actual == observed.Desired || len(observed.Actual) != 64 || len(observed.Desired) != 64 {
				t.Fatalf("count-only drift: %#v", observed)
			}
			if backend.Environment["E2E_VM_SLOT_COUNT"] != wantedCount {
				t.Fatal("observation replaced the desired slot count")
			}
		})
	}
}

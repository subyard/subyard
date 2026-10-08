package testvmsruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixtureBackend(t *testing.T) *Backend {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts", "e2e-lab"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "scripts", "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	dispatcher := filepath.Join(root, "yard-engine")
	if err := os.WriteFile(dispatcher, []byte("fixture-engine"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "e2e-lab", "provision.sh"),
		[]byte("fixture-provision\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "lib", "download.sh"),
		[]byte("fixture-download\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range recipeFiles {
		path := filepath.Join(root, filepath.FromSlash(name))
		if _, err := os.Stat(path); err == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture recipe: "+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	client := filepath.Join(root, "client")
	return &Backend{
		RepositoryRoot: root, Dispatcher: dispatcher, Project: "subyard-test",
		Instance: "yard-test", YardName: "test-yard", DesiredPower: "stopped",
		Environment: map[string]string{
			"NESTED_E2E_VMS": "1", "DEV_USER": "dev",
			"E2E_VM_IMAGE": "images:debian/13/cloud", "E2E_VM_CPU": "2",
			"E2E_VM_MEMORY": "4GiB", "E2E_VM_DISK": "10GiB",
			"E2E_VM_SLOT_COUNT": "2", "E2E_VM_BOOT_TIMEOUT": "300",
			"E2E_DISK_BUDGET": "120GiB", "E2E_CACHE_BUDGET": "20GiB",
			"E2E_DISK_RESERVE": "6GiB", "E2E_MEMORY_RESERVE": "3GiB", "E2E_VM_OVERHEAD": "768MiB",
			"SUBYARD_E2E_CLIENT_EXPORT_DIR": client,
		},
		Output: io.Discard,
	}
}

func TestBackendApplyInstallsCurrentEngineAndPublishesRoute(t *testing.T) {
	backend := fixtureBackend(t)
	delete(backend.Environment, "E2E_MEMORY_RESERVE")
	// Consumers traverse this directory as root inside an unprivileged yard, where
	// the host owner uid is unmapped: publication must set the mode explicitly
	// instead of leaving it to MkdirAll under the operator's umask.
	clientExport := backend.Environment["SUBYARD_E2E_CLIENT_EXPORT_DIR"]
	if err := os.MkdirAll(clientExport, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(clientExport, 0o700); err != nil {
		t.Fatal(err)
	}
	var power []string
	recipesInstalled := false
	memory := backendMemoryFixture(false)
	memoryValidated := false
	backend.Start = func(context.Context) error {
		power = append(power, "start")
		return nil
	}
	backend.Stop = func(context.Context) error {
		power = append(power, "stop")
		return nil
	}
	runner := &fakeRunner{handler: func(_ string, arguments, _ []string, stdin io.Reader) ([]byte, []byte, error) {
		if body, handled := memory.handle(t, arguments); handled {
			return body, nil, nil
		}
		joined := strings.Join(arguments, " ")
		switch {
		case joined == "list yard-test --project subyard-test -f csv -c s":
			return []byte("STOPPED\n"), nil, nil
		case strings.HasPrefix(joined, "file push "):
			if memory.Devices[hostMemoryDevice] == nil {
				return nil, nil, errors.New("engine staged before physical source installation")
			}
			if !strings.Contains(joined,
				backend.Dispatcher+" yard-test"+DefaultInstalledPath+".new") {
				return nil, nil, fmt.Errorf("wrong engine push: %s", joined)
			}
			return nil, nil, nil
		case joined == "exec yard-test --project subyard-test -- "+DefaultInstalledPath+".new _test-vms-host-memory-check":
			memoryValidated = true
			return nil, nil, nil
		case joined == "exec yard-test --project subyard-test -- mv -f -- "+
			DefaultInstalledPath+".new "+DefaultInstalledPath:
			if !memoryValidated {
				return nil, nil, errors.New("engine published before physical source validation")
			}
			return nil, nil, nil
		case strings.Contains(joined, " install-recipes "):
			archive, err := io.ReadAll(stdin)
			if err != nil {
				return nil, nil, err
			}
			expected, _, err := recipeBundle(backend.RepositoryRoot)
			if err != nil || !bytes.Equal(archive, expected) {
				return nil, nil, fmt.Errorf("wrong recipe payload: %v", err)
			}
			recipesInstalled = true
			return nil, nil, nil
		case strings.HasSuffix(joined, "-- bash -euo pipefail -s"):
			if !recipesInstalled {
				return nil, nil, fmt.Errorf("provisioning started before recipe installation")
			}
			payload, err := io.ReadAll(stdin)
			if err != nil || string(payload) != "fixture-download\nfixture-provision\n" {
				return nil, nil, fmt.Errorf("wrong provision payload: %q", payload)
			}
			for name, expected := range map[string]string{
				"E2E_DISK_BUDGET": "120GiB", "E2E_CACHE_BUDGET": "20GiB",
				"E2E_DISK_RESERVE": "6GiB", "E2E_MEMORY_RESERVE": "4GiB", "E2E_VM_OVERHEAD": "768MiB",
			} {
				if !strings.Contains(joined, "--env "+name+"="+expected+" ") {
					return nil, nil, fmt.Errorf("provisioning lost %s", name)
				}
			}
			if !strings.Contains(joined, "--env E2E_AGENT_PUBLIC_KEY= --") {
				return nil, nil, fmt.Errorf("default-open admission retained a static controller key")
			}
			return nil, nil, nil
		case joined == "exec yard-test --project subyard-test -- ip -4 -o route show default":
			return []byte("default via 10.10.0.1 dev eth0\n"), nil, nil
		case joined == "exec yard-test --project subyard-test -- ip -4 -o address show dev eth0 scope global":
			return []byte("2: eth0 inet 10.10.0.5/24 scope global eth0\n"), nil, nil
		case joined == "exec yard-test --project subyard-test -- cat /etc/ssh/ssh_host_ed25519_key.pub":
			return []byte(fixturePublicKey(t) + "\n"), nil, nil
		case strings.HasPrefix(joined,
			"config set yard-test user.subyard.test_vms_revision "):
			return nil, nil, nil
		case joined == "config set yard-test user.subyard.test_vms_spool_schema 1 --project subyard-test":
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("unexpected incus call: %s", joined)
	}}
	backend.Runner = runner
	if err := backend.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(power, ",") != "start,stop" {
		t.Fatalf("temporary power = %v", power)
	}
	current, err := PublishedRouteDirectory(
		backend.Environment["SUBYARD_E2E_CLIENT_EXPORT_DIR"],
	)
	if err != nil {
		t.Fatal(err)
	}
	route, err := os.ReadFile(filepath.Join(current, "route.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(route), "hostname\t10.10.0.5\n") {
		t.Fatalf("route = %q", route)
	}
	info, err := os.Stat(current)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("route generation mode = %v", info.Mode().Perm())
	}
	clientInfo, err := os.Stat(clientExport)
	if err != nil {
		t.Fatal(err)
	}
	if clientInfo.Mode().Perm() != 0o755 {
		t.Fatalf("route client directory mode = %v", clientInfo.Mode().Perm())
	}
	known, err := os.ReadFile(filepath.Join(current, "known_hosts"))
	if err != nil || !strings.HasPrefix(string(known), "subyard-e2e-bastion ssh-ed25519 ") {
		t.Fatalf("known_hosts = %q, %v", known, err)
	}
}

func TestBackendConvergenceUsesExactBundleAndLiveRoute(t *testing.T) {
	backend := fixtureBackend(t)
	state, err := backend.state()
	if err != nil {
		t.Fatal(err)
	}
	generation := filepath.Join(state.clientDirectory, ".route-fixture")
	if err := os.MkdirAll(generation, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(generation, "route.tsv"), []byte(
		"subyard-e2e-route-v1\nhostname\t10.10.0.4\nport\t22\n"+
			"host_key_alias\tsubyard-e2e-bastion\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}
	hostKey := fixturePublicKey(t)
	if err := os.WriteFile(filepath.Join(generation, "known_hosts"),
		[]byte("subyard-e2e-bastion "+hostKey+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(generation),
		filepath.Join(state.clientDirectory, "current")); err != nil {
		t.Fatal(err)
	}
	outer := "STOPPED"
	memory := backendMemoryFixture(true)
	runner := &fakeRunner{handler: func(_ string, arguments, _ []string, _ io.Reader) ([]byte, []byte, error) {
		if body, handled := memory.handle(t, arguments); handled {
			return body, nil, nil
		}
		switch strings.Join(arguments, " ") {
		case "config get yard-test user.subyard.test_vms_revision --project subyard-test":
			return []byte(state.marker + "\n"), nil, nil
		case "list yard-test --project subyard-test -f csv -c s":
			return []byte(outer + "\n"), nil, nil
		case "exec yard-test --project subyard-test -- ip -4 -o route show default":
			return []byte("default via 10.10.0.1 dev eth0\n"), nil, nil
		case "exec yard-test --project subyard-test -- ip -4 -o address show dev eth0 scope global":
			return []byte("2: eth0 inet 10.10.0.5/24 scope global eth0\n"), nil, nil
		case "exec yard-test --project subyard-test -- cat /etc/ssh/ssh_host_ed25519_key.pub":
			return []byte(hostKey + " fixture\n"), nil, nil
		case "exec yard-test --project subyard-test --env WANT_ENABLED=1 --env WANT_ENGINE_HASH=" +
			state.engineHash + " -- " + DefaultInstalledPath + " _test-vms-worker doctor":
			return nil, nil, nil
		default:
			return nil, nil, fmt.Errorf("unexpected call: %v", arguments)
		}
	}}
	backend.Runner = runner
	converged, err := backend.Converged(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !converged {
		t.Fatal("exact stopped backend was not converged")
	}
	delete(memory.Devices, hostMemoryDevice)
	delete(memory.ExpandedDevices, hostMemoryDevice)
	if converged, err := backend.Converged(context.Background()); err != nil || converged {
		t.Fatalf("missing telemetry ignored despite matching engine marker: %t %v", converged, err)
	}
	memory.Devices[hostMemoryDevice] = hostMemoryDeviceSpec()
	memory.ExpandedDevices[hostMemoryDevice] = hostMemoryDeviceSpec()
	outer = "RUNNING"
	converged, err = backend.Converged(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if converged {
		t.Fatal("stale running route was accepted")
	}
	if err := os.WriteFile(backend.Dispatcher, []byte("drift"), 0o755); err != nil {
		t.Fatal(err)
	}
	converged, err = backend.Converged(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if converged {
		t.Fatal("engine drift was accepted")
	}
	if err := os.WriteFile(backend.Dispatcher, []byte("fixture-engine"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backend.RepositoryRoot, "scripts", "lib", "download.sh"),
		[]byte("drift"), 0o644); err != nil {
		t.Fatal(err)
	}
	converged, err = backend.Converged(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if converged {
		t.Fatal("download helper drift was accepted")
	}
	if err := os.WriteFile(filepath.Join(backend.RepositoryRoot, "scripts", "lib", "download.sh"),
		[]byte("fixture-download\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	backend.Environment["E2E_VM_MEMORY"] = "2GiB"
	converged, err = backend.Converged(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if converged {
		t.Fatal("physical VM limit drift was accepted")
	}
}
func TestDisabledBackendRemovesPublishedRoute(t *testing.T) {
	for _, hostMemory := range []bool{false, true} {
		t.Run(fmt.Sprintf("host-memory=%t", hostMemory), func(t *testing.T) {
			backend := fixtureBackend(t)
			backend.HostMemory = hostMemory
			backend.Environment["NESTED_E2E_VMS"] = "0"
			client := backend.Environment["SUBYARD_E2E_CLIENT_EXPORT_DIR"]
			if err := os.MkdirAll(client, 0o755); err != nil {
				t.Fatal(err)
			}
			generation := filepath.Join(client, ".route-stale")
			if err := os.MkdirAll(generation, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Base(generation), filepath.Join(client, "current")); err != nil {
				t.Fatal(err)
			}
			memory := backendMemoryFixture(true)
			provisioned := false
			backend.Runner = &fakeRunner{handler: func(_ string, arguments, _ []string, stdin io.Reader) ([]byte, []byte, error) {
				if body, handled := memory.handle(t, arguments); handled {
					if !provisioned && !hostMemory {
						t.Fatal("physical source retired before disabling the broker")
					}
					return body, nil, nil
				}
				joined := strings.Join(arguments, " ")
				switch {
				case joined == "list yard-test --project subyard-test -f csv -c s":
					return []byte("RUNNING\n"), nil, nil
				case strings.HasSuffix(joined, "_test-vms-host-memory-check"):
					return nil, nil, nil
				case strings.HasPrefix(joined, "file push "):
					return nil, nil, nil
				case joined == "exec yard-test --project subyard-test -- mv -f -- "+
					DefaultInstalledPath+".new "+DefaultInstalledPath:
					return nil, nil, nil
				case strings.Contains(joined, " install-recipes "):
					_, _ = io.Copy(io.Discard, stdin)
					return nil, nil, nil
				case strings.HasSuffix(joined, "-- bash -euo pipefail -s"):
					_, _ = io.Copy(io.Discard, stdin)
					provisioned = true
					return nil, nil, nil
				case strings.HasPrefix(joined, "config set yard-test user.subyard.test_vms_revision "):
					return nil, nil, nil
				case joined == "config set yard-test user.subyard.test_vms_spool_schema 1 --project subyard-test":
					return nil, nil, nil
				}
				return nil, nil, fmt.Errorf("unexpected call: %s", joined)
			}}
			if err := backend.Apply(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(filepath.Join(client, "current")); !os.IsNotExist(err) {
				t.Fatalf("current route remains: %v", err)
			}
			if present := memory.Config[hostMemoryOwnerKey] == hostMemoryOwnerVersion && memory.Devices[hostMemoryDevice] != nil; present != hostMemory {
				t.Fatalf("physical memory device after disabling broker: present=%t requested=%t", present, hostMemory)
			}
		})
	}
}

func backendMemoryFixture(installed bool) *hostMemoryInstance {
	instance := &hostMemoryInstance{Config: map[string]string{}, Devices: map[string]map[string]string{}, ExpandedDevices: map[string]map[string]string{}}
	if installed {
		instance.Config[hostMemoryOwnerKey] = hostMemoryOwnerVersion
		instance.Devices[hostMemoryDevice] = hostMemoryDeviceSpec()
		instance.ExpandedDevices[hostMemoryDevice] = hostMemoryDeviceSpec()
	}
	return instance
}

func (instance *hostMemoryInstance) handle(t *testing.T, args []string) ([]byte, bool) {
	t.Helper()
	joined := strings.Join(args, " ")
	switch {
	case joined == "query /1.0/instances/yard-test?project=subyard-test":
		body, err := json.Marshal(instance)
		if err != nil {
			t.Fatal(err)
		}
		return body, true
	case joined == "config set yard-test "+hostMemoryOwnerKey+" pending:v1 --project subyard-test":
		instance.Config[hostMemoryOwnerKey] = "pending:v1"
	case joined == "config set yard-test "+hostMemoryOwnerKey+" v1 --project subyard-test":
		instance.Config[hostMemoryOwnerKey] = "v1"
	case joined == "config device add yard-test "+hostMemoryDevice+" disk --project subyard-test source=/proc/meminfo path="+hostMemoryPath+" readonly=true":
		instance.Devices[hostMemoryDevice] = hostMemoryDeviceSpec()
		instance.ExpandedDevices[hostMemoryDevice] = hostMemoryDeviceSpec()
	case joined == "config device remove yard-test "+hostMemoryDevice+" --project subyard-test":
		delete(instance.Devices, hostMemoryDevice)
		delete(instance.ExpandedDevices, hostMemoryDevice)
	case joined == "config unset yard-test "+hostMemoryOwnerKey+" --project subyard-test":
		delete(instance.Config, hostMemoryOwnerKey)
	default:
		return nil, false
	}
	return nil, true
}

func TestBackendRejectsUnsafePhysicalSourceBeforeEnginePublication(t *testing.T) {
	for _, nested := range []bool{true, false} {
		t.Run(fmt.Sprintf("nested=%t", nested), func(t *testing.T) {
			backend := fixtureBackend(t)
			if !nested {
				backend.Environment["NESTED_E2E_VMS"] = "0"
				backend.HostMemory = true
			}
			backend.DesiredPower = "running"
			backend.Start = func(context.Context) error { t.Fatal("running yard was restarted"); return nil }
			backend.Stop = backend.Start
			memory := backendMemoryFixture(false)
			backend.Runner = &fakeRunner{handler: func(_ string, arguments, _ []string, _ io.Reader) ([]byte, []byte, error) {
				if body, handled := memory.handle(t, arguments); handled {
					return body, nil, nil
				}
				joined := strings.Join(arguments, " ")
				switch {
				case joined == "list yard-test --project subyard-test -f csv -c s":
					return []byte("RUNNING\n"), nil, nil
				case strings.HasPrefix(joined, "file push "):
					return nil, nil, nil
				case strings.HasSuffix(joined, "_test-vms-host-memory-check"):
					return nil, nil, errors.New("unverified memory source")
				default:
					t.Fatalf("mutation after failed source validation: %v", arguments)
					return nil, nil, nil
				}
			}}
			if err := backend.Apply(context.Background()); err == nil {
				t.Fatal("unverified physical source accepted")
			}
		})
	}
}

func TestBackendPhysicalSourcePreservesConflictingDevices(t *testing.T) {
	for _, name := range []string{"unowned", "writable", "extra option", "inherited", "missing effective device", "occupied target", "unknown owner"} {
		t.Run(name, func(t *testing.T) {
			backend := fixtureBackend(t)
			memory := backendMemoryFixture(true)
			switch name {
			case "unowned":
				delete(memory.Config, hostMemoryOwnerKey)
			case "writable":
				memory.Devices[hostMemoryDevice]["readonly"] = "false"
			case "extra option":
				memory.Devices[hostMemoryDevice]["recursive"] = "true"
			case "inherited":
				delete(memory.Devices, hostMemoryDevice)
			case "missing effective device":
				delete(memory.ExpandedDevices, hostMemoryDevice)
			case "occupied target":
				memory.ExpandedDevices["foreign"] = map[string]string{"path": hostMemoryPath}
			case "unknown owner":
				memory.Config[hostMemoryOwnerKey] = "foreign"
			}
			backend.Runner = &fakeRunner{handler: func(_ string, arguments, _ []string, _ io.Reader) ([]byte, []byte, error) {
				if strings.Join(arguments, " ") != "query /1.0/instances/yard-test?project=subyard-test" {
					t.Fatalf("conflicting device was mutated: %v", arguments)
				}
				body, _ := json.Marshal(memory)
				return body, nil, nil
			}}
			for _, enabled := range []bool{true, false} {
				if converged, err := backend.hostMemoryConverged(context.Background(), enabled); err == nil || converged {
					t.Fatalf("conflicting device reported convergence: enabled=%t converged=%t err=%v", enabled, converged, err)
				}
				if err := backend.reconcileHostMemory(context.Background(), enabled); err == nil {
					t.Fatalf("conflicting device accepted: enabled=%t", enabled)
				}
			}
		})
	}
}

func TestProfileHostMemoryReadinessChecksLiveSource(t *testing.T) {
	backend := fixtureBackend(t)
	backend.Environment["NESTED_E2E_VMS"] = "0"
	backend.HostMemory = true
	backend.DesiredPower = "running"
	state, err := backend.state()
	if err != nil {
		t.Fatal(err)
	}
	memory := backendMemoryFixture(true)
	verified := false
	backend.Runner = &fakeRunner{handler: func(_ string, arguments, _ []string, _ io.Reader) ([]byte, []byte, error) {
		if body, handled := memory.handle(t, arguments); handled {
			return body, nil, nil
		}
		joined := strings.Join(arguments, " ")
		switch {
		case joined == "config get yard-test user.subyard.test_vms_revision --project subyard-test":
			return []byte(state.marker), nil, nil
		case joined == "list yard-test --project subyard-test -f csv -c s":
			return []byte("RUNNING\n"), nil, nil
		case strings.HasSuffix(joined, "_test-vms-host-memory-check"):
			if !verified {
				return nil, nil, errors.New("unverified live physical source")
			}
			return nil, nil, nil
		case strings.HasSuffix(joined, "_test-vms-worker doctor"):
			return nil, nil, nil
		default:
			t.Fatalf("unexpected readiness call: %v", arguments)
			return nil, nil, nil
		}
	}}
	if ready, err := backend.Converged(context.Background()); err != nil || ready {
		t.Fatalf("unsafe live source passed quiet readiness: ready=%t err=%v", ready, err)
	}
	if ready, err := backend.Verify(context.Background()); err == nil || ready || !strings.Contains(err.Error(), "physical host memory source") {
		t.Fatalf("unsafe live source had no diagnostic: ready=%t err=%v", ready, err)
	}
	verified = true
	if ready, err := backend.Converged(context.Background()); err != nil || !ready {
		t.Fatalf("verified live source failed readiness: ready=%t err=%v", ready, err)
	}
}

func TestBackendBudgetsAndRecipeChangesRequireReconcile(t *testing.T) {
	backend := fixtureBackend(t)
	original, err := backend.state()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"E2E_DISK_BUDGET", "E2E_CACHE_BUDGET", "E2E_DISK_RESERVE", "E2E_MEMORY_RESERVE", "E2E_VM_OVERHEAD"} {
		before := backend.Environment[name]
		backend.Environment[name] = "7GiB"
		changed, err := backend.state()
		if err != nil {
			t.Fatal(err)
		}
		if changed.marker == original.marker {
			t.Errorf("%s change did not invalidate convergence", name)
		}
		backend.Environment[name] = before
	}
	if err := os.WriteFile(filepath.Join(backend.RepositoryRoot, recipeFiles[0]), []byte("changed recipe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := backend.state()
	if err != nil {
		t.Fatal(err)
	}
	if changed.marker == original.marker {
		t.Fatal("recipe change did not invalidate convergence")
	}

}

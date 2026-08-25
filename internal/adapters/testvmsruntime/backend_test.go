package testvmsruntime

import (
	"context"
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
	client := filepath.Join(root, "client")
	return &Backend{
		RepositoryRoot: root, Dispatcher: dispatcher, Project: "subyard-test",
		Instance: "yard-test", YardName: "test-yard", DesiredPower: "stopped",
		Environment: map[string]string{
			"NESTED_E2E_VMS": "1", "DEV_USER": "dev",
			"E2E_VM_IMAGE": "images:debian/13/cloud", "E2E_VM_CPU": "2",
			"E2E_VM_MEMORY": "4GiB", "E2E_VM_DISK": "10GiB",
			"E2E_VM_SLOT_COUNT": "2", "E2E_VM_BOOT_TIMEOUT": "300",
			"SUBYARD_E2E_CLIENT_EXPORT_DIR": client,
		},
		Output: io.Discard,
	}
}

func TestBackendApplyInstallsCurrentEngineAndPublishesRoute(t *testing.T) {
	backend := fixtureBackend(t)
	var power []string
	backend.Start = func(context.Context) error {
		power = append(power, "start")
		return nil
	}
	backend.Stop = func(context.Context) error {
		power = append(power, "stop")
		return nil
	}
	runner := &fakeRunner{handler: func(_ string, arguments, _ []string, stdin io.Reader) ([]byte, []byte, error) {
		joined := strings.Join(arguments, " ")
		switch {
		case joined == "list yard-test --project subyard-test -f csv -c s":
			return []byte("STOPPED\n"), nil, nil
		case strings.HasPrefix(joined, "file push "):
			if !strings.Contains(joined,
				backend.Dispatcher+" yard-test"+DefaultInstalledPath+".new") {
				return nil, nil, fmt.Errorf("wrong engine push: %s", joined)
			}
			return nil, nil, nil
		case joined == "exec yard-test --project subyard-test -- mv -f -- "+
			DefaultInstalledPath+".new "+DefaultInstalledPath:
			return nil, nil, nil
		case strings.HasSuffix(joined, "-- bash -euo pipefail -s"):
			payload, err := io.ReadAll(stdin)
			if err != nil || string(payload) != "fixture-download\nfixture-provision\n" {
				return nil, nil, fmt.Errorf("wrong provision payload: %q", payload)
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
	runner := &fakeRunner{handler: func(_ string, arguments, _ []string, _ io.Reader) ([]byte, []byte, error) {
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
	backend := fixtureBackend(t)
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
	backend.Runner = &fakeRunner{handler: func(_ string, arguments, _ []string, stdin io.Reader) ([]byte, []byte, error) {
		joined := strings.Join(arguments, " ")
		switch {
		case joined == "list yard-test --project subyard-test -f csv -c s":
			return []byte("RUNNING\n"), nil, nil
		case strings.HasPrefix(joined, "file push "):
			return nil, nil, nil
		case joined == "exec yard-test --project subyard-test -- mv -f -- "+
			DefaultInstalledPath+".new "+DefaultInstalledPath:
			return nil, nil, nil
		case strings.HasSuffix(joined, "-- bash -euo pipefail -s"):
			_, _ = io.Copy(io.Discard, stdin)
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
}

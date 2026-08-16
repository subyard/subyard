package securityruntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/resource"
)

func TestSecurityRuntimeRejectsStaticSocketMountWithoutHostAccess(t *testing.T) {
	runtime := testRuntime(t)
	runtime.Environment["HOST_MOUNTS"] = "daemon:/run/docker.sock:rw:0755"
	_, err := runtime.CheckSecurity(context.Background(), false, true)
	if !errors.Is(err, ErrContract) {
		t.Fatalf("expected contract failure, got %v", err)
	}
}

func TestSecurityRuntimeValidatesLivePolicyWithoutIncusHost(t *testing.T) {
	runtime := testRuntime(t)
	runtime.State = func(context.Context, Runtime) (ports.ReconcileState, bool, error) {
		return safeState(), true, nil
	}
	state, err := runtime.CheckSecurity(context.Background(), true, true)
	if err != nil || state != "live" {
		t.Fatalf("state=%q err=%v", state, err)
	}
}

func TestSecurityRuntimeRejectsManagedDiskOutsideHostBase(t *testing.T) {
	runtime := testRuntime(t)
	state := safeState()
	state.Instance.Devices["host-source"] = map[string]string{
		"type": "disk", "source": "/etc", "path": "/mnt/host/source",
	}
	runtime.State = func(context.Context, Runtime) (ports.ReconcileState, bool, error) {
		return state, true, nil
	}
	_, err := runtime.CheckSecurity(context.Background(), true, true)
	if !errors.Is(err, ErrContract) {
		t.Fatalf("expected contract failure, got %v", err)
	}
}

func TestSecurityRuntimeAcceptsExactOwnedTailscaleProxy(t *testing.T) {
	runtime := testRuntime(t)
	runtime.Environment["HERMES_DASHBOARD_ADVERTISE_HOST"] = "owner.tailnet.ts.net"
	runtime.Environment["HERMES_DASHBOARD_HOST_PORT"] = "19119"
	contract := resource.ProxyContract{
		Profile: "hermes", Resource: "dashboard", Device: "hermes-dashboard",
		AdvertiseHostSetting: "HERMES_DASHBOARD_ADVERTISE_HOST",
		HostPortSetting:      "HERMES_DASHBOARD_HOST_PORT",
		Connect:              "tcp:127.0.0.1:9119",
		AddressPolicy:        resource.ProxyAddressTailscaleOnly,
		OwnershipMetadata:    true,
	}
	runtime.ProxyContracts = []resource.ProxyContract{contract}
	runtime.ResolveOwnerAddress = func(context.Context, string) (string, error) {
		return "100.101.102.103", nil
	}
	state := safeState()
	state.Instance.Devices["hermes-dashboard"] = map[string]string{
		"type": "proxy", "listen": "tcp:100.101.102.103:19119",
		"connect": "tcp:127.0.0.1:9119", "bind": "host",
	}
	state.Instance.LocalConfig[contract.OwnershipKey()] = contract.OwnershipValue(
		state.Instance.Devices["hermes-dashboard"],
	)
	runtime.State = func(context.Context, Runtime) (ports.ReconcileState, bool, error) {
		return state, true, nil
	}
	if _, err := runtime.CheckSecurity(context.Background(), true, true); err != nil {
		t.Fatal(err)
	}
}

func TestSecurityRuntimeRejectsLoopbackForTailscaleOnlyProxy(t *testing.T) {
	runtime := testRuntime(t)
	runtime.Environment["HERMES_DASHBOARD_ADVERTISE_HOST"] = "127.0.0.1"
	runtime.Environment["HERMES_DASHBOARD_HOST_PORT"] = "19119"
	contract := resource.ProxyContract{
		Profile: "hermes", Resource: "dashboard", Device: "hermes-dashboard",
		AdvertiseHostSetting: "HERMES_DASHBOARD_ADVERTISE_HOST",
		HostPortSetting:      "HERMES_DASHBOARD_HOST_PORT",
		Connect:              "tcp:127.0.0.1:9119",
		AddressPolicy:        resource.ProxyAddressTailscaleOnly,
		OwnershipMetadata:    true,
	}
	runtime.ProxyContracts = []resource.ProxyContract{contract}
	state := safeState()
	state.Instance.Devices[contract.Device] = map[string]string{
		"type": "proxy", "listen": "tcp:127.0.0.1:19119",
		"connect": contract.Connect, "bind": "host",
	}
	state.Instance.LocalConfig[contract.OwnershipKey()] = contract.OwnershipValue(
		state.Instance.Devices[contract.Device],
	)
	runtime.State = func(context.Context, Runtime) (ports.ReconcileState, bool, error) {
		return state, true, nil
	}
	if _, err := runtime.CheckSecurity(context.Background(), true, true); !errors.Is(err, ErrContract) {
		t.Fatalf("expected tailscale-only loopback rejection, got %v", err)
	}
}

func TestSecurityRuntimeRejectsProxyOutsideOwnedContract(t *testing.T) {
	tests := map[string]map[string]string{
		"unexpected device": {"type": "proxy", "listen": "tcp:100.101.102.103:19119", "connect": "tcp:127.0.0.1:9119", "bind": "host"},
		"wrong type":        {"type": "disk", "listen": "tcp:100.101.102.103:19119", "connect": "tcp:127.0.0.1:9119", "bind": "host"},
		"loopback listener": {"type": "proxy", "listen": "tcp:127.0.0.1:19119", "connect": "tcp:127.0.0.1:9119", "bind": "host"},
		"wrong address":     {"type": "proxy", "listen": "tcp:100.101.102.104:19119", "connect": "tcp:127.0.0.1:9119", "bind": "host"},
		"wrong port":        {"type": "proxy", "listen": "tcp:100.101.102.103:19120", "connect": "tcp:127.0.0.1:9119", "bind": "host"},
		"wrong connect":     {"type": "proxy", "listen": "tcp:100.101.102.103:19119", "connect": "tcp:0.0.0.0:9119", "bind": "host"},
		"wildcard":          {"type": "proxy", "listen": "tcp:0.0.0.0:19119", "connect": "tcp:127.0.0.1:9119", "bind": "host"},
		"unexpected option": {"type": "proxy", "listen": "tcp:100.101.102.103:19119", "connect": "tcp:127.0.0.1:9119", "bind": "host", "proxy_protocol": "true"},
	}
	for name, device := range tests {
		t.Run(name, func(t *testing.T) {
			runtime := testRuntime(t)
			runtime.Environment["HERMES_DASHBOARD_ADVERTISE_HOST"] = "owner.tailnet.ts.net"
			runtime.Environment["HERMES_DASHBOARD_HOST_PORT"] = "19119"
			contract := resource.ProxyContract{
				Profile: "hermes", Resource: "dashboard", Device: "hermes-dashboard",
				AdvertiseHostSetting: "HERMES_DASHBOARD_ADVERTISE_HOST",
				HostPortSetting:      "HERMES_DASHBOARD_HOST_PORT",
				Connect:              "tcp:127.0.0.1:9119",
				AddressPolicy:        resource.ProxyAddressTailscaleOnly,
				OwnershipMetadata:    true,
			}
			runtime.ProxyContracts = []resource.ProxyContract{contract}
			runtime.ResolveOwnerAddress = func(context.Context, string) (string, error) {
				return "100.101.102.103", nil
			}
			state := safeState()
			deviceName := "hermes-dashboard"
			if name == "unexpected device" {
				deviceName = "foreign-dashboard"
			}
			state.Instance.Devices[deviceName] = device
			state.Instance.LocalConfig[contract.OwnershipKey()] = contract.OwnershipValue(device)
			runtime.State = func(context.Context, Runtime) (ports.ReconcileState, bool, error) {
				return state, true, nil
			}
			if _, err := runtime.CheckSecurity(context.Background(), true, true); !errors.Is(err, ErrContract) {
				t.Fatalf("expected proxy contract failure, got %v", err)
			}
		})
	}
}

func TestSecurityRuntimeRejectsOwnedProxyWithoutMatchingMetadata(t *testing.T) {
	runtime := testRuntime(t)
	runtime.Environment["HERMES_DASHBOARD_ADVERTISE_HOST"] = "owner.tailnet.ts.net"
	runtime.Environment["HERMES_DASHBOARD_HOST_PORT"] = "19119"
	contract := resource.ProxyContract{
		Profile: "hermes", Resource: "dashboard", Device: "hermes-dashboard",
		AdvertiseHostSetting: "HERMES_DASHBOARD_ADVERTISE_HOST",
		HostPortSetting:      "HERMES_DASHBOARD_HOST_PORT",
		Connect:              "tcp:127.0.0.1:9119",
		AddressPolicy:        resource.ProxyAddressTailscaleOnly,
		OwnershipMetadata:    true,
	}
	runtime.ProxyContracts = []resource.ProxyContract{contract}
	runtime.ResolveOwnerAddress = func(context.Context, string) (string, error) {
		return "100.101.102.103", nil
	}
	state := safeState()
	state.Instance.Devices[contract.Device] = map[string]string{
		"type": "proxy", "listen": "tcp:100.101.102.103:19119",
		"connect": contract.Connect, "bind": "host",
	}
	runtime.State = func(context.Context, Runtime) (ports.ReconcileState, bool, error) {
		return state, true, nil
	}
	if _, err := runtime.CheckSecurity(context.Background(), true, true); !errors.Is(err, ErrContract) {
		t.Fatalf("expected missing proxy ownership metadata failure, got %v", err)
	}
}

func TestSecurityRuntimeAcceptsExactTypedProxyWithoutOptionalOwnershipMetadata(t *testing.T) {
	runtime := testRuntime(t)
	runtime.Environment["ORCA_ADVERTISE_HOST"] = "127.0.0.1"
	runtime.Environment["ORCA_HOST_PORT"] = "17678"
	contract := resource.ProxyContract{
		Profile: "orca", Resource: "orca", Device: "orca-server",
		AdvertiseHostSetting: "ORCA_ADVERTISE_HOST", HostPortSetting: "ORCA_HOST_PORT",
		Connect: "tcp:127.0.0.1:6768", AddressPolicy: resource.ProxyAddressLoopbackOrTailscale,
	}
	runtime.ProxyContracts = []resource.ProxyContract{contract}
	state := safeState()
	state.Instance.Devices[contract.Device] = map[string]string{
		"type": "proxy", "listen": "tcp:127.0.0.1:17678",
		"connect": contract.Connect, "bind": "host",
	}
	runtime.State = func(context.Context, Runtime) (ports.ReconcileState, bool, error) {
		return state, true, nil
	}
	if _, err := runtime.CheckSecurity(context.Background(), true, true); err != nil {
		t.Fatal(err)
	}
}

func TestSelectOwnerAddressRejectsAmbiguousAndUntrustedDNS(t *testing.T) {
	tailscale := map[string]struct{}{"100.101.102.103": {}}
	active := map[string]struct{}{"100.101.102.103": {}, "192.0.2.10": {}}
	tests := map[string][]string{
		"mixed public and Tailscale": {"100.101.102.103", "203.0.113.10"},
		"multiple local answers":     {"100.101.102.103", "192.0.2.10"},
		"public only":                {"203.0.113.10"},
		"no IPv4":                    {"2001:db8::1"},
	}
	for name, resolved := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := selectOwnerAddress(resolved, tailscale, active); err == nil {
				t.Fatal("expected owner-address rejection")
			}
		})
	}
	address, err := selectOwnerAddress(
		[]string{"100.101.102.103", "100.101.102.103"}, tailscale, active,
	)
	if err != nil || address != "100.101.102.103" {
		t.Fatalf("exact Tailscale address rejected: address=%q err=%v", address, err)
	}
}

func TestSecurityRuntimeRejectsProfileSocketMount(t *testing.T) {
	runtime := testRuntime(t)
	profile := filepath.Join(runtime.RepositoryRoot, "config", "profiles", "unsafe")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, "profile.conf"),
		[]byte(`ENV_MOUNTS="/var/run/docker.sock:/var/run/docker.sock"`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := runtime.CheckSecurity(context.Background(), false, true)
	if !errors.Is(err, ErrContract) {
		t.Fatalf("expected profile socket failure, got %v", err)
	}
}

func TestSecurityRuntimeRejectsUnsupportedAndDisabledNestedDevices(t *testing.T) {
	for name, source := range map[string]string{
		"unsupported": "/dev/mem",
		"disabled":    "/dev/vsock",
	} {
		t.Run(name, func(t *testing.T) {
			runtime := testRuntime(t)
			state := safeState()
			state.Instance.Devices["fixture"] = map[string]string{"type": "unix-char", "source": source}
			runtime.State = func(context.Context, Runtime) (ports.ReconcileState, bool, error) {
				return state, true, nil
			}
			_, err := runtime.CheckSecurity(context.Background(), true, true)
			if !errors.Is(err, ErrContract) {
				t.Fatalf("expected unix-char failure, got %v", err)
			}
		})
	}
}

func TestSecurityRuntimeAcceptsNestedDevicePolicy(t *testing.T) {
	runtime := testRuntime(t)
	runtime.Yard.NestedE2EVMs = true
	state := safeState()
	state.ProjectConfig["restricted.containers.interception"] = "allow"
	state.Instance.LocalConfig["security.syscalls.intercept.bpf"] = "true"
	state.Instance.LocalConfig["security.syscalls.intercept.bpf.devices"] = "true"
	state.Instance.Devices["vsock"] = map[string]string{"type": "unix-char", "source": "/dev/vsock"}
	runtime.State = func(context.Context, Runtime) (ports.ReconcileState, bool, error) {
		return state, true, nil
	}
	if _, err := runtime.CheckSecurity(context.Background(), true, true); err != nil {
		t.Fatal(err)
	}
}

func TestSecurityRuntimeWarnsForExplicitDiskOutsideHostBase(t *testing.T) {
	runtime := testRuntime(t)
	state := safeState()
	state.Instance.Devices["fixture"] = map[string]string{
		"type": "disk", "source": "/etc", "path": "/workspace",
	}
	runtime.State = func(context.Context, Runtime) (ports.ReconcileState, bool, error) {
		return state, true, nil
	}
	var diagnostics bytes.Buffer
	runtime.Stderr = &diagnostics
	if _, err := runtime.CheckSecurity(context.Background(), true, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diagnostics.String(), "encapsulation is reduced") {
		t.Fatalf("missing explicit-disk warning: %q", diagnostics.String())
	}
}

func TestSecurityRuntimeRequiresPrivateIdentityMode(t *testing.T) {
	runtime := testRuntime(t)
	root := filepath.Join(t.TempDir(), "keys")
	if err := os.MkdirAll(filepath.Join(root, "identity"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "identity", "age.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	runtime.Environment["SUBYARD_KEYS_ROOT"] = root
	var diagnostics bytes.Buffer
	runtime.Stderr = &diagnostics
	_, err := runtime.CheckSecurity(context.Background(), false, true)
	if !errors.Is(err, ErrContract) || !strings.Contains(diagnostics.String(), "mode 0600") {
		t.Fatalf("expected identity-mode failure, err=%v output=%q", err, diagnostics.String())
	}
}

func TestSecurityRuntimeRejectsLedgerUnderHostBase(t *testing.T) {
	runtime := testRuntime(t)
	runtime.Environment["SUBYARD_KEYS_ROOT"] = filepath.Join(runtime.Yard.Paths.HostBase, "keys")
	_, err := runtime.CheckSecurity(context.Background(), false, true)
	if !errors.Is(err, ErrContract) {
		t.Fatalf("expected ledger boundary failure, got %v", err)
	}
}

func testRuntime(t *testing.T) Runtime {
	t.Helper()
	root := t.TempDir()
	operator := t.TempDir()
	profiles := filepath.Join(root, "config", "profiles")
	if err := os.MkdirAll(profiles, 0o700); err != nil {
		t.Fatal(err)
	}
	return Runtime{
		RepositoryRoot: root,
		Environment:    map[string]string{"SUBYARD_SECURITY_SKIP_LIVE": "1"},
		Yard: domain.Context{
			IncusProject: "subyard", YardInstanceName: "yard",
			Paths: domain.RuntimePaths{
				ConfigHome: filepath.Join(operator, "config-home"),
				HostBase:   filepath.Join(operator, "host"),
			},
		},
	}
}

func safeState() ports.ReconcileState {
	return ports.ReconcileState{
		ProjectFound: true,
		ProjectConfig: map[string]string{
			"restricted":                         "true",
			"restricted.containers.privilege":    "unprivileged",
			"restricted.containers.interception": "block",
		},
		InstanceFound: true,
		Instance: ports.InstanceInfo{
			Config:       map[string]string{},
			LocalConfig:  map[string]string{},
			Devices:      map[string]map[string]string{},
			LocalDevices: map[string]map[string]string{},
		},
	}
}

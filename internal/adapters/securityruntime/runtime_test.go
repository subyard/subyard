package securityruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/sshagentruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/previewroute"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/testkit"
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

func TestSecurityRuntimeValidatesPreviewTailscaleRoute(t *testing.T) {
	for _, failure := range []string{
		"", "foreign", "inherited receipt", "inherited device", "local device divergence",
		"divergent", "wrong port", "inactive", "different active address", "pending",
		"extra option", "loopback", "wildcard", "LAN", "wrong type", "VM", "unknown kind",
	} {
		t.Run(failure, func(t *testing.T) {
			runtime := testRuntime(t)
			runtime.Yard.YardKind = domain.YardContainer
			runtime.Environment["WEB_PREVIEW_HOST_PORT"] = "32222"
			runtime.ResolveOwnerAddress = func(context.Context, string) (string, error) {
				if failure == "inactive" {
					return "", errors.New("inactive")
				}
				if failure == "different active address" {
					return "100.101.102.104", nil
				}
				return "100.101.102.103", nil
			}
			state := safeState()
			device := previewroute.Device("100.101.102.103", "32222")
			marker := "v1:100.101.102.103:32222"
			state.Instance.Config[previewroute.Key] = marker
			state.Instance.LocalConfig[previewroute.Key] = marker
			state.Instance.Devices[previewroute.DeviceName] = device
			state.Instance.LocalDevices[previewroute.DeviceName] = device
			switch failure {
			case "foreign":
				delete(state.Instance.Config, previewroute.Key)
				delete(state.Instance.LocalConfig, previewroute.Key)
			case "inherited receipt":
				delete(state.Instance.LocalConfig, previewroute.Key)
			case "inherited device":
				delete(state.Instance.LocalDevices, previewroute.DeviceName)
			case "local device divergence":
				local := maps.Clone(device)
				local["connect"] = "tcp:127.0.0.1:9999"
				state.Instance.LocalDevices[previewroute.DeviceName] = local
			case "divergent":
				device["connect"] = "tcp:127.0.0.1:9999"
			case "wrong port":
				runtime.Environment["WEB_PREVIEW_HOST_PORT"] = "32223"
			case "pending":
				state.Instance.LocalConfig[previewroute.Key] = "v1:pending:100.101.102.103:32222"
			case "extra option":
				device["nat"] = "true"
			case "loopback", "wildcard", "LAN":
				host := map[string]string{"loopback": "127.0.0.1", "wildcard": "0.0.0.0", "LAN": "192.168.1.2"}[failure]
				device["listen"] = "tcp:" + host + ":32222"
				state.Instance.LocalConfig[previewroute.Key] = "v1:" + host + ":32222"
			case "wrong type":
				device["type"] = "disk"
			case "VM":
				runtime.Yard.YardKind = domain.YardVM
			case "unknown kind":
				runtime.Yard.YardKind = ""
			}
			runtime.State = func(context.Context, Runtime) (ports.ReconcileState, bool, error) { return state, true, nil }
			security, err := runtime.CheckSecurity(context.Background(), true, true)
			if failure == "" {
				if err != nil || security != "live" {
					t.Fatalf("owned preview security=%q error=%v", security, err)
				}
			} else if !errors.Is(err, ErrContract) || security != "FAIL" {
				t.Fatalf("unsafe preview security=%q error=%v", security, err)
			}
		})
	}
}

func TestSecurityRuntimeValidatesObserverTailscaleRoute(t *testing.T) {
	for _, failure := range []string{"", "unselected", "foreign", "divergent", "inactive", "pending", "extra option"} {
		t.Run(failure, func(t *testing.T) {
			runtime := testRuntime(t)
			runtime.Environment["CODING_TOOL_INTEGRATIONS"] = "aiobserver"
			runtime.Environment["AI_OBSERVER_HOST_PORT"] = "22222"
			runtime.ResolveOwnerAddress = func(context.Context, string) (string, error) {
				if failure == "inactive" {
					return "", errors.New("inactive")
				}
				return "100.101.102.103", nil
			}
			state := safeState()
			device := map[string]string{"type": "proxy", "bind": "host", "listen": "tcp:100.101.102.103:22222", "connect": "tcp:127.0.0.1:8080"}
			state.Instance.Devices["ai-observer"] = device
			state.Instance.LocalConfig["user.subyard.ai_observer_proxy"] = "v2:100.101.102.103:22222"
			switch failure {
			case "unselected":
				runtime.Environment["CODING_TOOL_INTEGRATIONS"] = "codex"
			case "foreign":
				delete(state.Instance.LocalConfig, "user.subyard.ai_observer_proxy")
			case "divergent":
				device["connect"] = "tcp:127.0.0.1:9999"
			case "pending":
				state.Instance.LocalConfig["user.subyard.ai_observer_proxy"] = "v2:pending:100.101.102.103:22222"
			case "extra option":
				device["nat"] = "true"
			}
			runtime.State = func(context.Context, Runtime) (ports.ReconcileState, bool, error) { return state, true, nil }
			_, err := runtime.CheckSecurity(context.Background(), true, true)
			if (err != nil) != (failure != "") {
				t.Fatalf("security error = %v", err)
			}
		})
	}
}

func TestSecurityRuntimeAcceptsExactOwnedTailscaleProxy(t *testing.T) {
	runtime := testRuntime(t)
	runtime.Environment["SAMPLE_DASHBOARD_ADVERTISE_HOST"] = "owner.tailnet.ts.net"
	runtime.Environment["SAMPLE_DASHBOARD_HOST_PORT"] = "19119"
	contract := resource.ProxyContract{
		Profile: "sample", Resource: "dashboard", Device: "sample-dashboard",
		AdvertiseHostSetting: "SAMPLE_DASHBOARD_ADVERTISE_HOST",
		HostPortSetting:      "SAMPLE_DASHBOARD_HOST_PORT",
		Connect:              "tcp:127.0.0.1:9119",
		AddressPolicy:        resource.ProxyAddressTailscaleOnly,
		OwnershipMetadata:    true,
	}
	runtime.ProxyContracts = []resource.ProxyContract{contract}
	runtime.ResolveOwnerAddress = func(context.Context, string) (string, error) {
		return "100.101.102.103", nil
	}
	state := safeState()
	state.Instance.Devices["sample-dashboard"] = map[string]string{
		"type": "proxy", "listen": "tcp:100.101.102.103:19119",
		"connect": "tcp:127.0.0.1:9119", "bind": "host",
	}
	state.Instance.LocalConfig[contract.OwnershipKey()] = contract.OwnershipValue(
		state.Instance.Devices["sample-dashboard"],
	)
	runtime.State = func(context.Context, Runtime) (ports.ReconcileState, bool, error) {
		return state, true, nil
	}
	if _, err := runtime.CheckSecurity(context.Background(), true, true); err != nil {
		t.Fatal(err)
	}
}

func TestSecurityRuntimePublicUDPProxyRequiresExactOwnerAndGuest(t *testing.T) {
	contract := resource.ProxyContract{
		Profile: "sample", Resource: "relay", Device: "sample-relay",
		AdvertiseHostSetting: "SAMPLE_IPV4", HostPortSetting: "SAMPLE_PORT",
		OwnerInterfaceSetting: "SAMPLE_INTERFACE", Connect: "udp:guest:41999",
		AddressPolicy: resource.ProxyAddressOwnerIPv4UDP, OwnershipMetadata: true,
	}
	for _, scenario := range []struct {
		name   string
		mutate func(*Runtime, *ports.ReconcileState)
		valid  bool
	}{
		{"exact", nil, true},
		{"wrong profile", func(r *Runtime, _ *ports.ReconcileState) { r.Environment["ENVIRONMENT_PROFILES"] = "" }, false},
		{"default yard", func(r *Runtime, _ *ports.ReconcileState) { r.Yard.YardName = "default" }, false},
		{"other template", func(r *Runtime, _ *ports.ReconcileState) { r.Environment["YARD_TEMPLATE"] = "other" }, true},
		{"wrong kind", func(r *Runtime, _ *ports.ReconcileState) { r.Yard.YardKind = domain.YardContainer }, false},
		{"owner IP moved", func(r *Runtime, _ *ports.ReconcileState) {
			r.OwnerIPv4OnInterface = func(string, string) bool { return false }
		}, false},
		{"wrong guest pin", func(_ *Runtime, s *ports.ReconcileState) { s.Instance.Devices["eth0"]["ipv4.address"] = "10.80.0.11" }, false},
		{"wrong NAT", func(_ *Runtime, s *ports.ReconcileState) { s.Instance.Devices["sample-relay"]["nat"] = "false" }, false},
		{"extra option", func(_ *Runtime, s *ports.ReconcileState) {
			s.Instance.Devices["sample-relay"]["proxy_protocol"] = "true"
		}, false},
		{"inherited proxy", func(_ *Runtime, s *ports.ReconcileState) { delete(s.Instance.LocalDevices, "sample-relay") }, false},
		{"unowned", func(_ *Runtime, s *ports.ReconcileState) { delete(s.Instance.LocalConfig, contract.OwnershipKey()) }, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			runtime := testRuntime(t)
			runtime.Yard.YardName = "personal-relay"
			runtime.Yard.YardKind = domain.YardVM
			runtime.Environment["YARD_TEMPLATE"] = "sample"
			runtime.Environment["ENVIRONMENT_PROFILES"] = "sample"
			runtime.Environment["SAMPLE_IPV4"] = "10.20.30.40"
			runtime.Environment["SAMPLE_PORT"] = "42000"
			runtime.Environment["SAMPLE_INTERFACE"] = "eth0"
			runtime.OwnerIPv4OnInterface = func(name, address string) bool { return name == "eth0" && address == "10.20.30.40" }
			runtime.ProxyContracts = []resource.ProxyContract{contract}
			state := safeState()
			state.Instance.Devices["eth0"] = map[string]string{"type": "nic", "ipv4.address": "10.80.0.10"}
			device := map[string]string{"type": "proxy", "listen": "udp:10.20.30.40:42000", "connect": "udp:10.80.0.10:41999", "bind": "host", "nat": "true"}
			state.Instance.Devices["sample-relay"] = device
			state.Instance.LocalDevices["sample-relay"] = device
			state.Instance.LocalConfig[contract.OwnershipKey()] = contract.OwnershipValue(device)
			if scenario.mutate != nil {
				scenario.mutate(&runtime, &state)
			}
			runtime.State = func(context.Context, Runtime) (ports.ReconcileState, bool, error) { return state, true, nil }
			_, err := runtime.CheckSecurity(context.Background(), true, true)
			if (err == nil) != scenario.valid {
				t.Fatalf("valid=%t err=%v", scenario.valid, err)
			}
		})
	}
}

func TestSecurityRuntimeRejectsLoopbackForTailscaleOnlyProxy(t *testing.T) {
	runtime := testRuntime(t)
	runtime.Environment["SAMPLE_DASHBOARD_ADVERTISE_HOST"] = "127.0.0.1"
	runtime.Environment["SAMPLE_DASHBOARD_HOST_PORT"] = "19119"
	contract := resource.ProxyContract{
		Profile: "sample", Resource: "dashboard", Device: "sample-dashboard",
		AdvertiseHostSetting: "SAMPLE_DASHBOARD_ADVERTISE_HOST",
		HostPortSetting:      "SAMPLE_DASHBOARD_HOST_PORT",
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
			runtime.Environment["SAMPLE_DASHBOARD_ADVERTISE_HOST"] = "owner.tailnet.ts.net"
			runtime.Environment["SAMPLE_DASHBOARD_HOST_PORT"] = "19119"
			contract := resource.ProxyContract{
				Profile: "sample", Resource: "dashboard", Device: "sample-dashboard",
				AdvertiseHostSetting: "SAMPLE_DASHBOARD_ADVERTISE_HOST",
				HostPortSetting:      "SAMPLE_DASHBOARD_HOST_PORT",
				Connect:              "tcp:127.0.0.1:9119",
				AddressPolicy:        resource.ProxyAddressTailscaleOnly,
				OwnershipMetadata:    true,
			}
			runtime.ProxyContracts = []resource.ProxyContract{contract}
			runtime.ResolveOwnerAddress = func(context.Context, string) (string, error) {
				return "100.101.102.103", nil
			}
			state := safeState()
			deviceName := "sample-dashboard"
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
	runtime.Environment["SAMPLE_DASHBOARD_ADVERTISE_HOST"] = "owner.tailnet.ts.net"
	runtime.Environment["SAMPLE_DASHBOARD_HOST_PORT"] = "19119"
	contract := resource.ProxyContract{
		Profile: "sample", Resource: "dashboard", Device: "sample-dashboard",
		AdvertiseHostSetting: "SAMPLE_DASHBOARD_ADVERTISE_HOST",
		HostPortSetting:      "SAMPLE_DASHBOARD_HOST_PORT",
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
	runtime.Environment["SAMPLE_SERVER_ADVERTISE_HOST"] = "127.0.0.1"
	runtime.Environment["SAMPLE_SERVER_HOST_PORT"] = "17678"
	contract := resource.ProxyContract{
		Profile: "sample", Resource: "sample-service", Device: "sample-server",
		AdvertiseHostSetting: "SAMPLE_SERVER_ADVERTISE_HOST", HostPortSetting: "SAMPLE_SERVER_HOST_PORT",
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

func TestSecurityRuntimeReportsForwardedSSHAgentBoundaryOnlyWhenEnabled(t *testing.T) {
	for _, test := range []struct {
		name    string
		enabled bool
		want    bool
	}{
		{name: "enabled", enabled: true, want: true},
		{name: "disabled", enabled: false, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := testRuntime(t)
			runtime.Yard.ForwardSSHAgent = test.enabled
			var diagnostics bytes.Buffer
			runtime.Stderr = &diagnostics
			if _, err := runtime.CheckSecurity(context.Background(), false, false); err != nil {
				t.Fatal(err)
			}
			warning := diagnostics.String()
			for _, expected := range []string{
				"SSH agent forwarding is enabled",
				"git push and other host access from inside the yard",
				"forwarded, write-enabled credential",
				"while the SSH session is active",
				"No private key is copied into the yard",
				"any process that can reach the forwarded agent can exercise it",
				"agent ask-rules are a UX safeguard, not a security boundary",
			} {
				if strings.Contains(warning, expected) != test.want {
					t.Fatalf("forwarding warning presence for %q = %v, want %v: %q",
						expected, strings.Contains(warning, expected), test.want, warning)
				}
			}
		})
	}
}

func TestSecurityRuntimeRequiresPrivateIdentityMode(t *testing.T) {
	runtime := testRuntime(t)
	root := filepath.Join(testkit.TempDir(t), "keys")
	if err := os.MkdirAll(filepath.Join(root, "identity"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "identity", "age.txt"), []byte("x"), 0o644)
	runtime.Environment["SUBYARD_KEYS_ROOT"] = root
	var diagnostics bytes.Buffer
	runtime.Stderr = &diagnostics
	_, err := runtime.CheckSecurity(context.Background(), false, true)
	if !errors.Is(err, ErrContract) || !strings.Contains(diagnostics.String(), "mode 0600") {
		t.Fatalf("expected identity-mode failure, err=%v output=%q", err, diagnostics.String())
	}
}

func TestSecurityRuntimeSSHAgentStateIsOptionalAndReadOnly(t *testing.T) {
	runtime := testRuntime(t)
	runtime.Yard.YardName = "test"
	runtime.Yard.Paths.DataHome = t.TempDir()
	runtime.Yard.Paths.OperatorHome = t.TempDir()
	var diagnostics bytes.Buffer
	runtime.Stderr = &diagnostics
	if _, err := runtime.CheckSecurity(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(diagnostics.String(), "Temporary SSH-agent") ||
		strings.Contains(diagnostics.String(), "SSH-agent access is granted") {
		t.Fatalf("missing state produced SSH-agent finding: %q", diagnostics.String())
	}
	if _, err := os.Stat(sshagentruntime.Directory(runtime.Yard.Paths.DataHome, runtime.Yard.YardName)); !os.IsNotExist(err) {
		t.Fatalf("security check created SSH-agent state: %v", err)
	}
}

func TestSecurityRuntimeReportsActiveSSHAgentAccess(t *testing.T) {
	runtime := securitySSHAgentFixture(t, "unlocked")
	var diagnostics bytes.Buffer
	runtime.Stderr = &diagnostics
	if _, err := runtime.CheckSecurity(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diagnostics.String(), "Temporary SSH-agent access is granted to this yard") {
		t.Fatalf("active agent warning missing: %q", diagnostics.String())
	}
}

func TestSecurityRuntimeWarnsWhenSSHAgentStateCannotBeInspected(t *testing.T) {
	runtime := testRuntime(t)
	runtime.Yard.YardName = "test"
	runtime.Yard.Paths.DataHome = testkit.TempDir(t)
	directory := sshagentruntime.Directory(runtime.Yard.Paths.DataHome, runtime.Yard.YardName)
	if err := os.MkdirAll(filepath.Dir(directory), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(directory, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	var diagnostics bytes.Buffer
	runtime.Stderr = &diagnostics
	if _, err := runtime.CheckSecurity(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diagnostics.String(), "Temporary SSH-agent state could not be inspected") ||
		strings.Contains(diagnostics.String(), "access is granted") {
		t.Fatalf("inspection failure finding = %q", diagnostics.String())
	}
}

func securitySSHAgentFixture(t *testing.T, state string) Runtime {
	t.Helper()
	runtime := testRuntime(t)
	runtime.Yard.YardName = "test"
	runtime.Yard.Paths.DataHome = t.TempDir()
	directory := sshagentruntime.Directory(runtime.Yard.Paths.DataHome, runtime.Yard.YardName)
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	listener, err := net.Listen("unix", filepath.Join(directory, "control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		var request map[string]string
		_ = json.NewDecoder(connection).Decode(&request)
		_ = json.NewEncoder(connection).Encode(map[string]any{"status": map[string]any{
			"state": state, "expiresAt": time.Now().Add(time.Hour).UTC(), "remainingSeconds": 3600,
		}})
	}()
	return runtime
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

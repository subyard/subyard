package reconcileruntime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/previewroute"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

type previewFileExecutor struct {
	helper, endpoint           string
	helperOwner, endpointOwner string
}

func (fixture previewFileExecutor) Exec(ctx context.Context, _, _ string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	command := append([]string(nil), request.Command...)
	command[3] = strings.NewReplacer("/usr/local/bin/subyard-preview", fixture.helper,
		"/etc/subyard/preview.json", fixture.endpoint,
		"regular file|755|0:0", "regular file|755|"+fixture.helperOwner,
		"regular file|644|0:0", "regular file|644|"+fixture.endpointOwner).Replace(command[3])
	output, err := exec.CommandContext(ctx, command[0], command[1:]...).CombinedOutput()
	result := ports.InstanceExecResult{Stdout: output}
	if err != nil {
		result.ExitCode = 1
	}
	return result, err
}

func TestPreviewConvergenceChecksHelperAndEndpointBytesPermissionsAndOwnership(t *testing.T) {
	previewLoopbackFixture(t)
	for _, scenario := range []string{
		"ready", "helper missing", "helper stale", "helper permissions", "helper owner", "helper symlink", "helper directory",
		"endpoint missing", "endpoint stale", "endpoint permissions", "endpoint owner", "endpoint symlink", "endpoint directory",
	} {
		t.Run(scenario, func(t *testing.T) {
			root := testkit.TempDir(t)
			source := filepath.Join(root, "config", "preview", "subyard-preview")
			if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
				t.Fatal(err)
			}
			payload := []byte("#!/usr/bin/python3\nprint('preview')\n")
			testkit.WriteFile(t, source, payload, 0o755)
			helper := filepath.Join(root, "installed-preview")
			endpoint := filepath.Join(root, "preview.json")
			owner := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
			fixture := previewFileExecutor{helper: helper, endpoint: endpoint, helperOwner: owner, endpointOwner: owner}
			runtime := Runtime{RepositoryRoot: root, Yard: domain.Context{YardKind: domain.YardVM}, Executor: fixture}
			target, mode := helper, os.FileMode(0o755)
			var err error
			component, fault, _ := strings.Cut(scenario, " ")
			if component == "endpoint" {
				testkit.WriteFile(t, helper, payload, 0o755)
				target, mode = endpoint, 0o644
				payload, err = runtime.previewEndpoint(context.Background())
				if err != nil {
					t.Fatal(err)
				}
			} else {
				endpointPayload, err := runtime.previewEndpoint(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, endpoint, endpointPayload, 0o644)
			}
			switch fault {
			case "missing":
			case "directory":
				if err := os.Mkdir(target, 0o755); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(source, target); err != nil {
					t.Fatal(err)
				}
			default:
				if fault == "stale" {
					payload = []byte("stale")
				}
				if fault == "permissions" {
					mode = 0o777
				}
				if fault == "owner" {
					if component == "endpoint" {
						fixture.endpointOwner = "4294967294:4294967294"
					} else {
						fixture.helperOwner = "4294967294:4294967294"
					}
				}
				testkit.WriteFile(t, target, payload, mode)
			}
			runtime.Executor = fixture
			ready, err := runtime.previewConverged(context.Background())
			if err != nil || ready != (scenario == "ready") {
				t.Fatalf("ready=%v err=%v", ready, err)
			}
		})
	}
}

func TestPreviewRouteConvergenceWithoutOwnerTailAddress(t *testing.T) {
	bin := testkit.TempDir(t)
	testkit.WriteFile(t, filepath.Join(bin, "tailscale"), []byte("#!/bin/sh\nexit 1\n"), 0o755)
	t.Setenv("PATH", bin)
	for _, kind := range []domain.YardKind{domain.YardContainer, domain.YardVM} {
		runtime := Runtime{Yard: domain.Context{YardKind: kind}, Environment: []string{"WEB_PREVIEW_HOST_PORT=32222"}}
		for _, scenario := range []string{"absent", "receipt", "device", "local device", "receipt and device"} {
			t.Run(string(kind)+"/"+scenario, func(t *testing.T) {
				instance := ports.InstanceInfo{LocalConfig: map[string]string{}, Devices: map[string]map[string]string{}}
				if strings.Contains(scenario, "receipt") {
					instance.LocalConfig[previewroute.Key] = "v1:100.101.102.103:32222"
				}
				if scenario == "local device" {
					instance.LocalDevices = map[string]map[string]string{previewroute.DeviceName: previewroute.Device("100.101.102.103", "32222")}
				} else if strings.Contains(scenario, "device") {
					instance.Devices[previewroute.DeviceName] = previewroute.Device("100.101.102.103", "32222")
				}
				if ready := runtime.previewRouteConverged(context.Background(), instance); ready != (scenario == "absent") {
					t.Fatalf("route converged = %v", ready)
				}
			})
		}
	}
}

func TestPreviewConvergenceRequiresRegularSource(t *testing.T) {
	root := testkit.TempDir(t)
	runtime := Runtime{RepositoryRoot: root, Executor: runningIncus(0)}
	if _, err := runtime.previewSourceHash(); err == nil {
		t.Fatal("missing helper source accepted")
	}
	source := filepath.Join(root, "config", "preview", "subyard-preview")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	testkit.WriteFile(t, target, []byte("fixture"), 0o755)
	if err := os.Symlink(target, source); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.previewConverged(context.Background()); err == nil {
		t.Fatal("symlinked helper source accepted")
	}
}

// Fixtures must not infer publication from the developer host's real routes.
func previewLoopbackFixture(t *testing.T) {
	t.Helper()
	bin := testkit.TempDir(t)
	testkit.WriteFile(t, filepath.Join(bin, "tailscale"), []byte("#!/bin/sh\nexit 1\n"), 0o755)
	testkit.WriteFile(t, filepath.Join(bin, "ip"), []byte("#!/bin/sh\nprintf '[]\\n'\n"), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
}

func TestVMPreviewEndpointRequiresReadablePrivatePin(t *testing.T) {
	private := previewPrivateHostFixture(t)
	fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{"fixture/yard": {Devices: map[string]map[string]string{"eth0": {"ipv4.address": "10.80.0.10"}}}}}
	runtime := Runtime{Incus: fake, Yard: domain.Context{YardKind: domain.YardVM, IncusProject: "fixture", YardInstanceName: "yard"}, Environment: []string{"WEB_PREVIEW_HOST_PORT=32222"}}
	payload, err := runtime.previewEndpoint(context.Background())
	if err != nil || string(payload) != string(previewroute.Metadata(private, "32222", "10.80.0.10")) {
		t.Fatalf("metadata=%s err=%v", payload, err)
	}
	fake.Err = errors.New("fixture query error")
	if _, err := runtime.previewEndpointHash(context.Background()); err == nil {
		t.Fatal("query failure was accepted")
	}
	fake.Err = nil
	fake.Instances["fixture/yard"].Devices["eth0"]["ipv4.address"] = "203.0.113.1"
	if _, err := runtime.previewEndpoint(context.Background()); err == nil {
		t.Fatal("public guest pin was accepted")
	}
}

func previewPrivateHostFixture(t *testing.T) string {
	t.Helper()
	private := ""
	interfaces, _ := net.Interfaces()
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addresses, _ := iface.Addrs()
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err == nil {
				parsed, err := netip.ParseAddr(ip.String())
				if err == nil && parsed.Is4() && parsed.IsPrivate() {
					private = parsed.String()
				}
			}
		}
	}
	if private == "" {
		t.Skip("no active private owner address on this host")
	}
	bin := testkit.TempDir(t)
	testkit.WriteFile(t, filepath.Join(bin, "tailscale"), []byte("#!/bin/sh\nexit 1\n"), 0o755)
	testkit.WriteFile(t, filepath.Join(bin, "ip"), []byte(fmt.Sprintf("#!/bin/sh\nprintf '[{\"prefsrc\":\"%s\"}]\\n'\n", private)), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	return private
}

type previewRecoveryIncus struct {
	*testkit.Incus
	repaired, host string
}

func (fixture previewRecoveryIncus) Instance(ctx context.Context, project, name string) (ports.InstanceInfo, error) {
	instance, err := fixture.Incus.Instance(ctx, project, name)
	if _, statErr := os.Stat(fixture.repaired); statErr == nil {
		instance.Devices[previewroute.DeviceName] = previewroute.Device(fixture.host, "32222", "10.80.0.10")
		instance.LocalDevices = map[string]map[string]string{previewroute.DeviceName: instance.Devices[previewroute.DeviceName]}
	}
	return instance, err
}

type previewRecoveryPolicy struct {
	networkPolicyFixture
	repaired string
	restarts int
}

func (fixture *previewRecoveryPolicy) Ensure(ctx context.Context, yard yardnetwork.Yard) error {
	if _, err := os.Stat(fixture.repaired); err != nil {
		fixture.restarts++ // Removing the approved ingress would require a physical restart.
	}
	return fixture.networkPolicyFixture.Ensure(ctx, yard)
}

func TestNetworkPolicyRestoresOwnedVMPreviewBeforeReconcilingIngress(t *testing.T) {
	host := previewPrivateHostFixture(t)
	root := testkit.TempDir(t)
	if err := os.Mkdir(filepath.Join(root, "scripts"), 0o700); err != nil {
		t.Fatal(err)
	}
	repaired := filepath.Join(root, "repaired")
	testkit.WriteFile(t, filepath.Join(root, "scripts", "prepare-preview-route.sh"), []byte(
		"#!/bin/sh\nset -eu\n[ \"$1\" = \"$EXPECTED_HOST\" ]\nprintf 'route\\n' >> \"$REPAIRED\"\n"), 0o700)
	instance := ports.InstanceInfo{Status: "Stopped", LocalConfig: map[string]string{
		previewroute.Key: "v2:" + host + ":32222:10.80.0.10",
	}, Devices: map[string]map[string]string{"eth0": {"ipv4.address": "10.80.0.10"}}}
	policy := &previewRecoveryPolicy{repaired: repaired}
	runtime := Runtime{RepositoryRoot: root, NetworkPolicy: policy,
		Yard:        domain.Context{YardKind: domain.YardVM, IncusProject: "fixture", YardInstanceName: "yard"},
		Incus:       previewRecoveryIncus{Incus: &testkit.Incus{Instances: map[string]ports.InstanceInfo{"fixture/yard": instance}}, repaired: repaired, host: host},
		Environment: []string{"WEB_PREVIEW_HOST_PORT=32222", "EXPECTED_HOST=" + host, "REPAIRED=" + repaired},
	}
	ctx := context.Background()
	ready, err := runtime.CheckStage(ctx, ports.ReconcileStageNetworkPolicy)
	if err != nil || ready {
		t.Fatalf("missing owned route: converged=%v err=%v", ready, err)
	}
	if err := runtime.ApplyStage(ctx, ports.ReconcileStageNetworkPolicy); err != nil {
		t.Fatal(err)
	}
	ready, err = runtime.VerifyStage(ctx, ports.ReconcileStageNetworkPolicy)
	if err != nil || !ready {
		t.Fatalf("repaired route: converged=%v err=%v", ready, err)
	}
	if _, err := runtime.preparePreview(ctx); err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(repaired)
	if err != nil || string(calls) != "route\n" || policy.restarts != 0 {
		t.Fatalf("route recovery repeated or revoked ingress: calls=%q restarts=%d err=%v", calls, policy.restarts, err)
	}
}

func TestPreviewRouteDefersOnlyMissingInstanceOrMissingVMPin(t *testing.T) {
	for _, scenario := range []struct {
		name        string
		incus       *testkit.Incus
		host        string
		drift, fail bool
	}{
		{name: "fresh instance", incus: &testkit.Incus{}, host: "100.101.102.103"},
		{name: "unpinned VM", incus: &testkit.Incus{Instances: map[string]ports.InstanceInfo{"fixture/yard": {}}}, host: "100.101.102.103"},
		{name: "inspection failure", incus: &testkit.Incus{Err: errors.New("fixture failure")}, host: "100.101.102.103", fail: true},
		{name: "loopback cleanup without pin", incus: &testkit.Incus{Instances: map[string]ports.InstanceInfo{"fixture/yard": {
			LocalConfig: map[string]string{previewroute.Key: "v1:pending:100.101.102.103:32222"},
		}}}, host: "127.0.0.1", drift: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			runtime := Runtime{Incus: scenario.incus, Yard: domain.Context{YardKind: domain.YardVM, IncusProject: "fixture", YardInstanceName: "yard"}}
			drift, err := runtime.previewRouteDrift(context.Background(), scenario.host)
			if drift != scenario.drift || (err != nil) != scenario.fail {
				t.Fatalf("drift=%v err=%v", drift, err)
			}
		})
	}
}

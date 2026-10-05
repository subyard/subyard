package cli

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

type rollbackIngressHost struct {
	yardnetwork.Host
	snapshot yardnetwork.Snapshot
}

func (host *rollbackIngressHost) ReadPolicy(context.Context) (yardnetwork.StoredPolicy, error) {
	return yardnetwork.StoredPolicy{Policy: yardnetwork.Policy{Schema: yardnetwork.SchemaVersion}}, nil
}

func (host *rollbackIngressHost) InspectNetwork(context.Context, []yardnetwork.Yard) (yardnetwork.Snapshot, error) {
	content, err := json.Marshal(host.snapshot)
	if err != nil {
		return yardnetwork.Snapshot{}, err
	}
	var snapshot yardnetwork.Snapshot
	err = json.Unmarshal(content, &snapshot)
	return snapshot, err
}

func TestResourceIngressRollbackVerifiesSuccessfulHandlerClosedRoute(t *testing.T) {
	program, loaded, _ := orphanIngressFixture(t, true) // Handler exits successfully without changing Incus.
	definition := program.resources.Definitions()[0]
	contract := *definition.Proxy
	yard := networkYard(loaded.Context)
	device := map[string]string{"type": "proxy", "listen": "udp:10.20.30.40:42000",
		"connect": "udp:10.80.0.10:41999", "bind": "host", "nat": "true"}
	marker := map[string]string{contract.OwnershipKey(): contract.OwnershipValue(device)}
	host := &rollbackIngressHost{snapshot: yardnetwork.Snapshot{Yards: []yardnetwork.ObservedYard{{
		Yard: yard, InstanceFound: true, InstanceInfo: ports.InstanceInfo{Type: domain.YardVM,
			LocalDevices: map[string]map[string]string{contract.Device: maps.Clone(device)},
			Devices: map[string]map[string]string{contract.Device: maps.Clone(device),
				"eth0": {"type": "nic", "ipv4.address": "10.80.0.10"}},
			LocalConfig: maps.Clone(marker), Config: maps.Clone(marker)},
	}}}}
	service := &yardnetwork.Service{Host: host,
		ContractSource: func(yardnetwork.Yard) ([]resource.ProxyContract, error) {
			return []resource.ProxyContract{contract}, nil
		}}
	yards := []yardnetwork.Yard{yard}
	preview, err := service.PreviewResourceIngress(context.Background(), yards, yard, contract, "10.20.30.40", 42000, true)
	if err != nil || preview.Before.Changed {
		t.Fatalf("expected unchanged network policy with published route: changed=%v err=%v", preview.Before.Changed, err)
	}
	ingress := &resourceIngress{service: service, yards: yards, preview: preview, contract: contract, up: true}
	runner := &resourceApplyRunner{cli: program, loaded: loaded, definition: definition,
		localAction: definition.BringUp, effect: domain.ActionMutation}
	if err := ingress.rollback(runner, "rollback-test"); err == nil || !strings.Contains(err.Error(), "left a proxy or ownership marker") {
		t.Fatalf("successful handler hid an open route: %v", err)
	}
	instance := &host.snapshot.Yards[0].InstanceInfo
	delete(instance.LocalDevices, contract.Device)
	delete(instance.Devices, contract.Device)
	delete(instance.LocalConfig, contract.OwnershipKey())
	delete(instance.Config, contract.OwnershipKey())
	if err := ingress.rollback(runner, "rollback-test"); err != nil {
		t.Fatalf("verified closed route failed rollback: %v", err)
	}
}

func TestResourceIngressRefreshRejectsChangedPersistedSettings(t *testing.T) {
	for _, test := range []struct {
		name, changed string
	}{
		{"selection", "YARD_KIND=vm\nENVIRONMENT_PROFILES=\nRESOURCE_RELAY_PORT=42000\n"},
		{"endpoint", "YARD_KIND=vm\nENVIRONMENT_PROFILES=sample\nRESOURCE_RELAY_PORT=42001\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			yardFile := filepath.Join(root, "state", "yards", "private", "config.env")
			if err := os.MkdirAll(filepath.Dir(yardFile), 0o700); err != nil {
				t.Fatal(err)
			}
			writeCLIFile(t, yardFile, "YARD_KIND=vm\nENVIRONMENT_PROFILES=sample\nRESOURCE_RELAY_PORT=42000\n", 0o600)
			resourceDir := filepath.Join(root, "config", "profiles", "sample", "resources")
			if err := os.MkdirAll(filepath.Join(resourceDir, "relay"), 0o700); err != nil {
				t.Fatal(err)
			}
			writeCLIFile(t, filepath.Join(resourceDir, "relay.res"), `
COMMAND=relay
HANDLER=resources/relay/handler.sh
TITLE="Sample relay"
PROXY="sample-relay RESOURCE_RELAY_IPV4 RESOURCE_RELAY_PORT RESOURCE_RELAY_INTERFACE udp:guest:41999 owner-metadata-v1 owner-ipv4-udp"
ACTION="up up public-ingress-change reversible"
ACTION="down down public-ingress-change reversible"
BRINGUP=up
SHUTDOWN=down
`, 0o600)
			writeCLIFile(t, filepath.Join(resourceDir, "relay", "handler.sh"), "#!/bin/sh\nexit 0\n", 0o700)
			program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := program.loadContext("private")
			if err != nil {
				t.Fatal(err)
			}
			contract := resource.ProxyContract{Profile: "sample", Resource: "relay", Device: "sample-relay", Connect: "udp:guest:41999", HostPortSetting: "RESOURCE_RELAY_PORT", AdvertiseHostSetting: "RESOURCE_RELAY_IPV4", OwnerInterfaceSetting: "RESOURCE_RELAY_INTERFACE", AddressPolicy: resource.ProxyAddressOwnerIPv4UDP, OwnershipMetadata: true}
			ingress := &resourceIngress{loaded: loaded, contract: contract}
			writeCLIFile(t, yardFile, test.changed, 0o600)
			if err := ingress.refresh(context.Background(), program); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("persisted %s change passed ingress refresh: %v", test.name, err)
			}
		})
	}
}

func TestResourceShutdownProbeWaitsForReadOnlyGuestAssessment(t *testing.T) {
	root, environment, applyLog := resourceCommandFixture(t)
	handler := filepath.Join(root, "config", "profiles", "fixture", "resources", "demo", "handler.sh")
	writeCLIFile(t, handler, `#!/bin/sh
set -eu
[ "${SUBYARD_RESOURCE_MODE:-}" = prepare ] || exit 70
counter="$SUBYARD_REPOSITORY_ROOT/shutdown-probe"
if [ ! -e "$counter" ]; then
  : >"$counter"
  exit 1
fi
printf '{"schema":"yard.resource-action-assessment.v1","action":"purge","changed":false,"consequences":[]}\n'
`, 0o700)
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	definition, ok := program.resources.Lookup("demo")
	if !ok {
		t.Fatal("fixture resource unavailable")
	}
	runner := &resourceApplyRunner{cli: program, loaded: loaded, definition: definition, localAction: "purge"}
	if err := (&resourceIngress{}).probeShutdown(context.Background(), runner); err != nil {
		t.Fatalf("shutdown probe did not recover after guest became ready: %v", err)
	}
	if _, err := os.Stat(applyLog); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only retry reached mutation: %v", err)
	}
}

func TestResourceShutdownProbeRejectsRemainingRuntime(t *testing.T) {
	root, environment, _ := resourceCommandFixture(t)
	handler := filepath.Join(root, "config", "profiles", "fixture", "resources", "demo", "handler.sh")
	writeCLIFile(t, handler, `#!/bin/sh
set -eu
printf '{"schema":"yard.resource-action-assessment.v1","action":"purge","changed":true,"consequences":["fixture remains enabled"]}\n'
`, 0o700)
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	definition, _ := program.resources.Lookup("demo")
	runner := &resourceApplyRunner{cli: program, loaded: loaded, definition: definition, localAction: "purge"}
	if err := (&resourceIngress{}).probeShutdown(context.Background(), runner); err == nil || !strings.Contains(err.Error(), "did not converge") {
		t.Fatalf("remaining enabled resource passed shutdown verification: %v", err)
	}
}

func TestResourceIngressClearsOnlySelectedVerifiedUDPRoute(t *testing.T) {
	yard := yardnetwork.Yard{Name: "private", Project: "subyard-private", Instance: "yard-private", Network: "incusbr0"}
	device := func(port string) map[string]string {
		return map[string]string{"type": "proxy", "listen": "udp:10.20.30.40:" + port,
			"connect": "udp:10.80.0.10:41999", "bind": "host", "nat": "true"}
	}
	selected, other := device("42000"), device("42001")
	marker := resource.ProxyContract{}
	instance := ports.InstanceInfo{Type: domain.YardVM, Status: "Running",
		LocalDevices: map[string]map[string]string{"selected": maps.Clone(selected), "other": maps.Clone(other)},
		Devices: map[string]map[string]string{"selected": maps.Clone(selected), "other": maps.Clone(other),
			"eth0": {"type": "nic", "ipv4.address": "10.80.0.10"}},
		LocalConfig: map[string]string{"user.subyard.resource.selected": marker.OwnershipValue(selected),
			"user.subyard.resource.other": marker.OwnershipValue(other)}}
	host := &rollbackIngressHost{snapshot: yardnetwork.Snapshot{Yards: []yardnetwork.ObservedYard{{
		Yard: yard, InstanceFound: true, InstanceInfo: instance,
	}}}}
	var cleared []netip.AddrPort
	service := &yardnetwork.Service{Host: host, Lock: testNetworkPolicyLock{},
		ClearStaleUDP: func(_ context.Context, endpoint netip.AddrPort) error {
			cleared = append(cleared, endpoint)
			return nil
		}}
	ingress := &resourceIngress{service: service, preview: yardnetwork.IngressPreview{Target: yard, Protocol: "udp"},
		address: "10.20.30.40", port: 42000}
	if err := ingress.clearStaleUDP(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(cleared) != 1 || cleared[0] != netip.MustParseAddrPort("10.20.30.40:42000") {
		t.Fatalf("cleanup escaped selected endpoint: %v", cleared)
	}
	cleared = nil
	ingress.port = 42002
	if err := ingress.clearStaleUDP(context.Background()); err == nil || !strings.Contains(err.Error(), "not found") || len(cleared) != 0 {
		t.Fatalf("missing selected endpoint passed recovery: cleaned=%v err=%v", cleared, err)
	}
	ingress.port = 42000
	host.snapshot.Yards[0].InstanceInfo.Devices["other"]["nat"] = "false"
	if err := ingress.clearStaleUDP(context.Background()); err == nil || len(cleared) != 0 {
		t.Fatalf("drifted owned route passed recovery: cleaned=%v err=%v", cleared, err)
	}
	host.snapshot.Yards[0].InstanceInfo.Devices["other"]["nat"] = "true"
	failure := errors.New("cleanup failed")
	service.ClearStaleUDP = func(context.Context, netip.AddrPort) error { return failure }
	if err := ingress.clearStaleUDP(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("cleanup failure did not block activation: %v", err)
	}
}

func TestResourceIngressNoOpUpHasNoUDPRecoveryEffect(t *testing.T) {
	ingress := &resourceIngress{up: true}
	assessment := domain.ActionAssessment{Changed: false}
	got := ingress.augment(assessment)
	if got.Changed || len(got.Consequences) != 0 {
		t.Fatalf("no-op up unexpectedly requested UDP cleanup: %+v", got)
	}
}

func TestTCPIngressSkipsUDPRecoveryAndDescribesExactACL(t *testing.T) {
	contract := resource.ProxyContract{AddressPolicy: resource.ProxyAddressOwnerIPv4TCP, Connect: "tcp:guest:22"}
	ingress := &resourceIngress{up: true, contract: contract, service: &yardnetwork.Service{},
		preview: yardnetwork.IngressPreview{Protocol: "tcp", GuestPort: 22, Target: yardnetwork.Yard{Name: "private"}, After: yardnetwork.Plan{Changed: true}}}
	if err := ingress.clearStaleUDP(context.Background()); err != nil {
		t.Fatalf("TCP used UDP cleaner: %v", err)
	}
	assessment := ingress.augment(domain.ActionAssessment{Changed: true})
	if len(assessment.Consequences) != 1 || !strings.Contains(assessment.Consequences[0], "TCP/22") {
		t.Fatalf("wrong transport confirmation: %v", assessment.Consequences)
	}
}

func TestOrphanClosureResolvesDynamicPortFromOwnedDevice(t *testing.T) {
	for _, protocol := range []string{"udp", "tcp"} {
		contract := resource.ProxyContract{AddressPolicy: resource.ProxyAddressPolicy("owner-ipv4-" + protocol), Connect: protocol + ":guest:host-port"}
		device := map[string]string{"type": "proxy", "listen": protocol + ":10.20.30.40:42000", "connect": protocol + ":10.80.0.10:42000", "bind": "host", "nat": "true"}
		if !validOrphanPublicIngressDevice(device, contract) {
			t.Fatalf("valid %s closure refused", protocol)
		}
		device["connect"] = protocol + ":10.80.0.10:42001"
		if validOrphanPublicIngressDevice(device, contract) {
			t.Fatalf("wrong %s guest port accepted", protocol)
		}
	}
}

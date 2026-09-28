package cli

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
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

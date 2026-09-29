package cli

import (
	"context"
	"errors"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/shellquote"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

type startupRollbackHost struct {
	*rollbackIngressHost
	closed   string
	contract resource.ProxyContract
}

func (host *startupRollbackHost) InspectNetwork(ctx context.Context, yards []yardnetwork.Yard) (yardnetwork.Snapshot, error) {
	snapshot, err := host.rollbackIngressHost.InspectNetwork(ctx, yards)
	if err != nil {
		return snapshot, err
	}
	if _, err := os.Stat(host.closed); err == nil {
		instance := &snapshot.Yards[0].InstanceInfo
		delete(instance.Devices, host.contract.Device)
		delete(instance.LocalDevices, host.contract.Device)
		delete(instance.Config, host.contract.OwnershipKey())
		delete(instance.LocalConfig, host.contract.OwnershipKey())
	} else if !errors.Is(err, os.ErrNotExist) {
		return snapshot, err
	}
	return snapshot, nil
}

type failingStartupWriter struct {
	*testkit.Incus
	failure error
	writes  int
}

func (writer *failingStartupWriter) SetInstanceConfig(context.Context, string, string, map[string]string) error {
	writer.writes++
	return writer.failure
}

func TestResourceStartupCommitFailureRollsBackPublishedIngress(t *testing.T) {
	program, loaded, _ := orphanIngressFixture(t, true)
	definition := program.resources.Definitions()[0]
	contract := *definition.Proxy
	yard := networkYard(loaded.Context)
	closed := filepath.Join(program.options.RepositoryRoot, "closed-ingress")
	calls := filepath.Join(program.options.RepositoryRoot, "handler-calls")
	writeCLIFile(t, definition.HandlerPath(), "#!/bin/sh\nprintf '%s\\n' \"$1\" >> "+shellquote.Word(calls)+"\ncase \"$1\" in\n  rollback-ingress) : > "+shellquote.Word(closed)+" ;;\n  up|is-up) ;;\n  *) exit 70 ;;\nesac\n", 0o700)
	device := map[string]string{"type": "proxy", "listen": "udp:10.20.30.40:42000", "connect": "udp:10.80.0.10:41999", "bind": "host", "nat": "true"}
	metadata := map[string]string{contract.OwnershipKey(): contract.OwnershipValue(device), startupIntentKey(definition): startupPending}
	instance := ports.InstanceInfo{Name: loaded.Context.YardInstanceName, Project: loaded.Context.IncusProject, Type: domain.YardVM, Status: "Running",
		LocalDevices: map[string]map[string]string{contract.Device: maps.Clone(device)},
		Devices:      map[string]map[string]string{contract.Device: maps.Clone(device), "eth0": {"type": "nic", "ipv4.address": "10.80.0.10"}},
		LocalConfig:  maps.Clone(metadata), Config: maps.Clone(metadata)}
	host := &startupRollbackHost{closed: closed, contract: contract, rollbackIngressHost: &rollbackIngressHost{
		snapshot: yardnetwork.Snapshot{Yards: []yardnetwork.ObservedYard{{Yard: yard, InstanceFound: true, InstanceInfo: instance}}}}}
	service := &yardnetwork.Service{Host: host, Lock: testNetworkPolicyLock{},
		ClearStaleUDP: func(_ context.Context, endpoint netip.AddrPort) error {
			if endpoint != netip.MustParseAddrPort("10.20.30.40:42000") {
				t.Fatalf("unexpected stale UDP endpoint: %v", endpoint)
			}
			return nil
		}, ContractSource: func(yardnetwork.Yard) ([]resource.ProxyContract, error) {
			return []resource.ProxyContract{contract}, nil
		}}
	yards := []yardnetwork.Yard{yard}
	preview, err := service.PreviewResourceIngress(context.Background(), yards, yard, contract, "10.20.30.40", 42000, true)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("startup metadata write failed")
	writer := &failingStartupWriter{Incus: &testkit.Incus{Instances: map[string]ports.InstanceInfo{instance.Project + "/" + instance.Name: instance}}, failure: failure}
	program.options.Incus = writer
	runner := &resourceApplyRunner{cli: program, loaded: loaded, definition: definition, verb: definition.BringUp, localAction: definition.BringUp,
		effect: domain.ActionMutation, arguments: []string{definition.BringUp},
		ingress:       &resourceIngress{service: service, yards: yards, preview: preview, contract: contract, up: true, address: "10.20.30.40", port: 42000},
		startupIntent: &resourceStartupIntent{loaded: loaded, definition: definition, before: startupPending, after: startupEnabled}}
	result, _, err := runner.Run(context.Background(), domain.AdapterRequest{Schema: 1, OperationID: "startup-rollback", Adapter: "resource", Action: definition.BringUp}, nil)
	if !errors.Is(err, failure) || result.Status != "error" || writer.writes != 1 {
		t.Fatalf("result=%+v err=%v writes=%d", result, err, writer.writes)
	}
	observedCalls, err := os.ReadFile(calls)
	if err != nil || string(observedCalls) != "up\nis-up\nrollback-ingress\n" {
		t.Fatalf("handler calls=%q err=%v", observedCalls, err)
	}
	snapshot, err := host.InspectNetwork(context.Background(), yards)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Yards[0].InstanceInfo.Devices[contract.Device] != nil || snapshot.Yards[0].InstanceInfo.Config[contract.OwnershipKey()] != "" {
		t.Fatal("failed startup left published ingress")
	}
}

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

type orphanIngressSnapshotHost struct {
	yardnetwork.Host
	*testkit.Incus
	snapshot      yardnetwork.Snapshot
	instanceErr   error
	instanceCalls []string
	networkCalls  int
}

type orphanBootstrapStage struct {
	*initPlatformFixture
	target  string
	stopped error
}

func (stage orphanBootstrapStage) ApplyStage(_ context.Context, id ports.ReconcileStageID) error {
	if id != ports.ReconcileStageIncus {
		return errors.New("unexpected later init stage")
	}
	if _, err := os.Stat(stage.target); err != nil {
		return fmt.Errorf("named yard registration was not published before Incus enrollment: %w", err)
	}
	return stage.stopped
}

func (host *orphanIngressSnapshotHost) InspectNetwork(context.Context, []yardnetwork.Yard) (yardnetwork.Snapshot, error) {
	host.networkCalls++
	return yardnetwork.Snapshot{}, errors.New("whole-network inspection is forbidden")
}

func (host *orphanIngressSnapshotHost) Instance(_ context.Context, project, name string) (ports.InstanceInfo, error) {
	host.instanceCalls = append(host.instanceCalls, project+"/"+name)
	if host.instanceErr != nil {
		return ports.InstanceInfo{}, host.instanceErr
	}
	for _, observed := range host.snapshot.Yards {
		if observed.Yard.Project == project && observed.Yard.Instance == name && observed.InstanceFound {
			return observed.InstanceInfo, nil
		}
	}
	return ports.InstanceInfo{}, ports.ErrInstanceNotFound
}

func orphanIngressFixture(t *testing.T, selected bool) (*CLI, config.Loaded, *orphanIngressSnapshotHost) {
	t.Helper()
	root, environment, _ := nativeFixture(t)
	yardFile := filepath.Join(root, "state", "yards", "private", "config.env")
	if err := os.MkdirAll(filepath.Dir(yardFile), 0o700); err != nil {
		t.Fatal(err)
	}
	selection := ""
	if selected {
		selection = "sample"
	}
	writeCLIFile(t, yardFile, "YARD_KIND=vm\nENVIRONMENT_PROFILES="+selection+"\n", 0o600)
	resourceDir := filepath.Join(root, "config", "profiles", "sample", "resources")
	if err := os.MkdirAll(resourceDir, 0o700); err != nil {
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
	if err := os.MkdirAll(filepath.Join(resourceDir, "relay"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(resourceDir, "relay", "handler.sh"), "#!/bin/sh\nexit 0\n", 0o700)
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment,
		WorkingDir: root, Stdout: io.Discard, Stderr: io.Discard, Incus: &testkit.Incus{}})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("private")
	if err != nil {
		t.Fatal(err)
	}
	host := &orphanIngressSnapshotHost{Incus: &testkit.Incus{}}
	program.options.Incus = host
	program.options.NetworkPolicy = &yardnetwork.Service{Host: host}
	return program, loaded, host
}

func orphanIngressUnixPermission() error {
	return &url.Error{Op: "Get", URL: "http://unix.socket/1.0", Err: &net.OpError{
		Op: "dial", Net: "unix", Err: &os.SyscallError{Syscall: "connect", Err: syscall.EACCES}}}
}

func TestSelectedPublicIngressSkipsColdIncusInspection(t *testing.T) {
	program, loaded, host := orphanIngressFixture(t, true)
	host.instanceErr = orphanIngressUnixPermission()
	plan, err := program.prepareOrphanIngress(context.Background(), loaded)
	if err != nil || plan != nil || len(host.instanceCalls) != 0 || host.networkCalls != 0 {
		t.Fatalf("selected profile probed inaccessible Incus socket: plan=%+v instance calls=%v network calls=%d err=%v",
			plan, host.instanceCalls, host.networkCalls, err)
	}
}

func TestOrphanIngressReadsOnlyExactInstance(t *testing.T) {
	program, loaded, host := orphanIngressFixture(t, false)
	yard := networkYard(loaded.Context)
	if plan, err := program.prepareOrphanIngress(context.Background(), loaded); err != nil || plan != nil {
		t.Fatalf("missing target instance was not treated as absent: plan=%+v err=%v", plan, err)
	}
	failure := errors.New("instance inspection failed")
	host.instanceErr = failure
	if _, err := program.prepareOrphanIngress(context.Background(), loaded); !errors.Is(err, failure) {
		t.Fatalf("instance inspection failure was hidden: %v", err)
	}
	for _, call := range host.instanceCalls {
		if call != yard.Project+"/"+yard.Instance {
			t.Fatalf("unexpected instance inspection: %s", call)
		}
	}
	if len(host.instanceCalls) != 2 || host.networkCalls != 0 {
		t.Fatalf("orphan planning inspected more than target instance: instance calls=%v network calls=%d",
			host.instanceCalls, host.networkCalls)
	}
}

func TestColdNamedYardDefersOnlyUnixSocketPermission(t *testing.T) {
	program, loaded, host := orphanIngressFixture(t, false)
	host.instanceErr = orphanIngressUnixPermission()
	if _, err := program.prepareOrphanIngress(context.Background(), loaded); !errors.Is(err, errOrphanIngressAccessDeferred) {
		t.Fatalf("cold named yard was not deferred: %v", err)
	}
	execution := &initExecution{loaded: loaded, orphanIngressDeferred: true,
		platform: &initPlatformFixture{converged: map[ports.ReconcileStageID]bool{}}}
	if err := execution.refreshOrphanIngress(context.Background(), program); err != nil {
		t.Fatalf("pre-consent refresh rejected the same inaccessible socket: %v", err)
	}
	host.instanceErr = nil
	if err := execution.finishDeferredOrphanIngress(context.Background(), program, io.Discard); err != nil {
		t.Fatalf("fresh named yard did not pass post-bootstrap inspection: %v", err)
	}
	if got := execution.platform.(*initPlatformFixture).applied; len(got) != 1 || got[0] != ports.ReconcileStageIncus {
		t.Fatalf("deferred inspection bypassed the normal Incus stage: %v", got)
	}
	host.instanceErr = &url.Error{Op: "Get", Err: os.ErrPermission}
	if _, err := program.prepareOrphanIngress(context.Background(), loaded); errors.Is(err, errOrphanIngressAccessDeferred) {
		t.Fatalf("non-socket permission error was deferred: %v", err)
	}
}

func TestDeferredOrphanIngressRequiresNewAssessmentAfterIncusStage(t *testing.T) {
	program, loaded, host := orphanIngressFixture(t, false)
	contract := program.resources.Definitions()[0].Proxy
	device := map[string]string{"type": "proxy", "listen": "udp:10.20.30.40:42000",
		"connect": "udp:10.80.0.10:41999", "bind": "host", "nat": "true"}
	host.snapshot.Yards = []yardnetwork.ObservedYard{{Yard: networkYard(loaded.Context), InstanceFound: true,
		InstanceInfo: ports.InstanceInfo{Type: domain.YardVM,
			LocalDevices: map[string]map[string]string{contract.Device: maps.Clone(device)},
			Devices:      map[string]map[string]string{contract.Device: maps.Clone(device)},
			LocalConfig:  map[string]string{contract.OwnershipKey(): contract.OwnershipValue(device)}}}}
	execution := &initExecution{loaded: loaded, orphanIngressDeferred: true,
		platform: &initPlatformFixture{converged: map[ports.ReconcileStageID]bool{}}}
	if err := execution.finishDeferredOrphanIngress(context.Background(), program, io.Discard); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("newly visible orphan reused old consent: %v", err)
	}
	if len(host.snapshot.Yards[0].InstanceInfo.LocalDevices) != 1 ||
		host.snapshot.Yards[0].InstanceInfo.LocalConfig[contract.OwnershipKey()] == "" {
		t.Fatal("deferred assessment changed owned route before new approval")
	}
}

func TestGroupReexecCannotAutoApproveNewOrphanIngress(t *testing.T) {
	program, loaded, host := orphanIngressFixture(t, false)
	contract := program.resources.Definitions()[0].Proxy
	device := map[string]string{"type": "proxy", "listen": "udp:10.20.30.40:42000",
		"connect": "udp:10.80.0.10:41999", "bind": "host", "nat": "true"}
	host.snapshot.Yards = []yardnetwork.ObservedYard{{Yard: networkYard(loaded.Context), InstanceFound: true,
		InstanceInfo: ports.InstanceInfo{Type: domain.YardVM,
			LocalDevices: map[string]map[string]string{contract.Device: maps.Clone(device)},
			Devices:      map[string]map[string]string{contract.Device: maps.Clone(device)},
			LocalConfig:  map[string]string{contract.OwnershipKey(): contract.OwnershipValue(device)}}}}
	program.baseEnv["SUBYARD_SG_REEXEC"] = "1"
	if _, err := program.prepareInitExecution(context.Background(), loaded, nil, nil); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("group re-exec would approve a newly visible public route: %v", err)
	}
	if len(host.snapshot.Yards[0].InstanceInfo.LocalDevices) != 1 {
		t.Fatal("group re-exec changed the route before a fresh approval")
	}
}

func TestDeferredIngressKeepsNamedBootstrapBeforeIncusEnrollment(t *testing.T) {
	program, _, host := orphanIngressFixture(t, false)
	root := program.options.RepositoryRoot
	writeCLIFile(t, filepath.Join(root, "config", "profiles", "sample", "yard.env"),
		"YARD_KIND=vm\nENVIRONMENT_PROFILES=\n", 0o600)
	loaded, bootstrap, err := program.loadInitContext("fresh", true, []string{"--profile", "sample"})
	if err != nil || bootstrap == nil {
		t.Fatalf("load named profile bootstrap: %v", err)
	}
	host.instanceErr = orphanIngressUnixPermission()
	stopped := errors.New("stop after approved Incus prerequisite")
	execution := &initExecution{loaded: loaded, bootstrap: bootstrap, orphanIngressDeferred: true,
		platform: orphanBootstrapStage{initPlatformFixture: &initPlatformFixture{
			converged: map[ports.ReconcileStageID]bool{}}, target: bootstrap.targetPath, stopped: stopped}}
	if err := execution.run(context.Background(), program, io.Discard); !errors.Is(err, stopped) {
		t.Fatalf("named bootstrap did not precede Incus re-exec boundary: %v", err)
	}
	if data, err := os.ReadFile(bootstrap.targetPath); err != nil || string(data) != string(bootstrap.content) {
		t.Fatalf("approved registration was not published before Incus enrollment: %v", err)
	}
}

type orphanIngressCloserStub struct {
	ports.Incus
	host *orphanIngressSnapshotHost
}

func (closer orphanIngressCloserStub) RemoveOwnedResourceIngress(_ context.Context, yard yardnetwork.Yard,
	contract resource.ProxyContract, expected map[string]string, marker string, retainPending bool) error {
	instance := &closer.host.snapshot.Yards[0].InstanceInfo
	if closer.host.snapshot.Yards[0].Yard != yard || !maps.Equal(instance.LocalDevices[contract.Device], expected) ||
		instance.LocalConfig[contract.OwnershipKey()] != marker {
		return domain.ErrPlanStale
	}
	delete(instance.LocalDevices, contract.Device)
	delete(instance.Devices, contract.Device)
	if retainPending {
		if len(expected) != 0 {
			instance.LocalConfig[contract.OwnershipKey()] = "v1:pending:" + strings.TrimPrefix(contract.OwnershipValue(expected), "v1:")
		}
	} else {
		delete(instance.LocalConfig, contract.OwnershipKey())
	}
	return nil
}

func TestOrphanIngressRecoversDedicatedRuntimeAfterRouteAlreadyRemoved(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	yardFile := filepath.Join(root, "state", "yards", "private", "config.env")
	if err := os.MkdirAll(filepath.Dir(yardFile), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, yardFile, "YARD_KIND=vm\nENVIRONMENT_PROFILES=\n", 0o600)
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
	writeCLIFile(t, filepath.Join(resourceDir, "relay", "handler.sh"), `#!/bin/sh
set -eu
[ "$1" = down ]
if [ "${SUBYARD_RESOURCE_MODE:-}" = prepare ]; then
  changed=false
  [ ! -e "$SUBYARD_REPOSITORY_ROOT/service-enabled" ] || changed=true
  printf '{"schema":"yard.resource-action-assessment.v1","action":"down","changed":%s,"consequences":["stop sample relay runtime while preserving state"]}\n' "$changed"
else
  [ "${SUBYARD_RESOURCE_MODE:-}" = apply ]
  [ "${SUBYARD_RESOURCE_ACTION:-}" = down ]
  [ -n "${SUBYARD_OPERATION_ID:-}" ]
  mv "$SUBYARD_REPOSITORY_ROOT/service-enabled" "$SUBYARD_REPOSITORY_ROOT/service-disabled"
fi
`, 0o700)
	writeCLIFile(t, filepath.Join(root, "service-enabled"), "synthetic\n", 0o600)
	writeCLIFile(t, filepath.Join(root, "state-key"), "synthetic-key\n", 0o600)
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment,
		WorkingDir: root, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("private")
	if err != nil {
		t.Fatal(err)
	}
	loaded.Environment["EXCLUSIVE_ENVIRONMENT_PROFILE"] = "sample"
	loaded.Environment["YARD_KIND"] = "vm"
	loaded.Context.YardKind = domain.YardVM
	yard := networkYard(loaded.Context)
	host := &orphanIngressSnapshotHost{snapshot: yardnetwork.Snapshot{Yards: []yardnetwork.ObservedYard{{
		Yard: yard, InstanceFound: true, InstanceInfo: ports.InstanceInfo{
			Type: domain.YardVM, Status: "Running", LocalDevices: map[string]map[string]string{},
			Devices: map[string]map[string]string{}, LocalConfig: map[string]string{},
		},
	}}}}
	host.Incus = &testkit.Incus{}
	program.options.Incus = host
	program.options.NetworkPolicy = &yardnetwork.Service{Host: host}
	plan, err := program.prepareOrphanIngress(context.Background(), loaded)
	if err != nil || plan == nil || len(plan.entries) != 1 || plan.entries[0].shutdown == nil || !plan.entries[0].shutdown.Changed {
		t.Fatalf("route-free enabled runtime was not assessed: plan=%+v err=%v", plan, err)
	}
	if _, err := os.Stat(filepath.Join(root, "service-enabled")); err != nil {
		t.Fatalf("prepare changed runtime before init consent: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "service-disabled")); !os.IsNotExist(err) {
		t.Fatalf("prepare ran shutdown before consent: %v", err)
	}
	if err := plan.apply(context.Background(), program, loaded, "init-test-operation"); err != nil {
		t.Fatal(err)
	}
	for _, call := range host.instanceCalls {
		if call != yard.Project+"/"+yard.Instance {
			t.Fatalf("orphan recovery inspected unexpected instance: %s", call)
		}
	}
	if len(host.instanceCalls) < 3 || host.networkCalls != 0 {
		t.Fatalf("orphan recovery skipped exact final verification or scanned network: instance calls=%v network calls=%d",
			host.instanceCalls, host.networkCalls)
	}
	if _, err := os.Stat(filepath.Join(root, "service-disabled")); err != nil {
		t.Fatalf("declared shutdown did not run: %v", err)
	}
	key, err := os.ReadFile(filepath.Join(root, "state-key"))
	if err != nil || string(key) != "synthetic-key\n" {
		t.Fatalf("persistent state was not preserved: %v", err)
	}
	if retry, err := program.prepareOrphanIngress(context.Background(), loaded); err != nil || retry != nil {
		t.Fatalf("recovered runtime still needs shutdown: %+v %v", retry, err)
	}

	// If a running guest cannot be assessed, init still closes the exact owned
	// route, then fails before any later stage can claim guest shutdown.
	contract := program.resources.Definitions()[0].Proxy
	device := map[string]string{"type": "proxy", "listen": "udp:10.20.30.40:42000",
		"connect": "udp:10.80.0.10:41999", "bind": "host", "nat": "true"}
	host.snapshot.Yards[0].InstanceInfo.LocalDevices[contract.Device] = maps.Clone(device)
	host.snapshot.Yards[0].InstanceInfo.Devices[contract.Device] = maps.Clone(device)
	host.snapshot.Yards[0].InstanceInfo.LocalConfig[contract.OwnershipKey()] = contract.OwnershipValue(device)
	writeCLIFile(t, filepath.Join(resourceDir, "relay", "handler.sh"), "#!/bin/sh\nexit 1\n", 0o700)
	program.options.Incus = orphanIngressCloserStub{Incus: host, host: host}
	unavailable, err := program.prepareOrphanIngress(context.Background(), loaded)
	if err != nil || unavailable == nil || !unavailable.entries[0].shutdownUnavailable {
		t.Fatalf("unavailable guest did not preserve native closure plan: %+v %v", unavailable, err)
	}
	if err := unavailable.apply(context.Background(), program, loaded, "init-test-operation"); err == nil ||
		!strings.Contains(err.Error(), "guest shutdown") {
		t.Fatalf("unverified guest shutdown was reported as success: %v", err)
	}
	if len(host.snapshot.Yards[0].InstanceInfo.LocalDevices) != 0 ||
		host.snapshot.Yards[0].InstanceInfo.LocalConfig[contract.OwnershipKey()] !=
			"v1:pending:"+strings.TrimPrefix(contract.OwnershipValue(device), "v1:") {
		t.Fatal("unavailable guest did not close the route and retain pending ownership")
	}
	loaded.Environment["EXCLUSIVE_ENVIRONMENT_PROFILE"] = "other"
	retry, err := program.prepareOrphanIngress(context.Background(), loaded)
	if err != nil || retry == nil || len(retry.entries) != 1 {
		t.Fatalf("pending route disappeared after role change: %+v %v", retry, err)
	}
	if err := retry.apply(context.Background(), program, loaded, "init-retry-operation"); err == nil ||
		!strings.Contains(err.Error(), "guest shutdown") {
		t.Fatalf("role-changed retry falsely reported complete cleanup: %v", err)
	}
	if host.snapshot.Yards[0].InstanceInfo.LocalConfig[contract.OwnershipKey()] == "" {
		t.Fatal("role-changed retry erased pending cleanup intent")
	}
}

func TestOrphanIngressPlanClosesOnlyDeselectedOwnedRoute(t *testing.T) {
	yard := yardnetwork.Yard{Name: "private", Project: "subyard-private", Instance: "yard-private", Network: "incusbr0"}
	contract := resource.ProxyContract{Profile: "sample", Resource: "relay", Device: "sample-relay",
		Connect: "udp:guest:41999", AddressPolicy: resource.ProxyAddressOwnerIPv4UDP, OwnershipMetadata: true}
	definitions := []resource.Definition{{Profile: "sample", Name: "relay", Command: "relay", Shutdown: "down", Proxy: &contract}}
	device := map[string]string{"type": "proxy", "listen": "udp:10.20.30.40:42000",
		"connect": "udp:10.80.0.10:41999", "bind": "host", "nat": "true"}
	instance := ports.InstanceInfo{Type: domain.YardVM,
		LocalDevices: map[string]map[string]string{contract.Device: device},
		Devices:      map[string]map[string]string{contract.Device: device},
		LocalConfig:  map[string]string{contract.OwnershipKey(): contract.OwnershipValue(device)}}
	plan, err := planOrphanIngress(yard, &instance, nil, "", definitions)
	if err != nil || plan == nil || len(plan.entries) != 1 || plan.entries[0].contract != contract {
		t.Fatalf("owned route was not planned for closure: %+v, %v", plan, err)
	}
	if got := plan.consequences(); len(got) != 2 || !strings.Contains(got[0], contract.Device) {
		t.Fatalf("public route closure consequence = %v", got)
	}
	if selected, err := planOrphanIngress(yard, &instance, []string{"sample"}, "sample", definitions); err != nil || selected != nil {
		t.Fatalf("selected route was treated as orphan: %+v, %v", selected, err)
	}
	instance.LocalConfig[contract.OwnershipKey()] = "foreign"
	if _, err := planOrphanIngress(yard, &instance, nil, "", definitions); err == nil {
		t.Fatal("foreign device ownership was accepted")
	}
	instance.LocalConfig[contract.OwnershipKey()] = contract.OwnershipValue(device)
	device["nat"] = "false"
	instance.LocalConfig[contract.OwnershipKey()] = contract.OwnershipValue(device)
	if _, err := planOrphanIngress(yard, &instance, nil, "", definitions); err == nil {
		t.Fatal("nonconforming proxy with matching metadata was accepted")
	}
	device["nat"] = "true"
	instance.LocalConfig[contract.OwnershipKey()] = contract.OwnershipValue(device)
	delete(instance.LocalDevices, contract.Device)
	if _, err := planOrphanIngress(yard, &instance, nil, "", definitions); err == nil {
		t.Fatal("inherited route was accepted")
	}
	delete(instance.Devices, contract.Device)
	instance.LocalConfig[contract.OwnershipKey()] = "v1:pending:" + strings.Repeat("a", 64)
	pending, err := planOrphanIngress(yard, &instance, nil, "", definitions)
	if err != nil || pending == nil || len(pending.entries[0].device) != 0 {
		t.Fatalf("pending marker could not be closed: %+v, %v", pending, err)
	}
}

func TestOrphanIngressRejectsUnsafeDeclaredShutdown(t *testing.T) {
	definition := resource.Definition{Profile: "sample", Name: "relay", Command: "relay", Shutdown: "down"}
	for _, test := range []struct {
		name       string
		assessment domain.ActionAssessment
		allowed    bool
	}{
		{"reversible closure", domain.ActionAssessment{Action: "resource.sample.relay.down", Effect: domain.ActionMutation,
			Changed: true, Recovery: domain.RecoveryReversible, Impacts: []domain.ActionImpact{domain.ImpactHostNetwork}}, true},
		{"read-only no-op", domain.ActionAssessment{Action: "resource.sample.relay.down", Effect: domain.ActionRead,
			Recovery: domain.RecoveryNotNeeded}, true},
		{"destructive", domain.ActionAssessment{Action: "resource.sample.relay.down", Effect: domain.ActionDestruction,
			Changed: true, Recovery: domain.RecoveryIrreversible}, false},
		{"persistent data", domain.ActionAssessment{Action: "resource.sample.relay.down", Effect: domain.ActionMutation,
			Changed: true, Recovery: domain.RecoveryReversible, Impacts: []domain.ActionImpact{domain.ImpactPersistentData}}, false},
		{"nonreversible", domain.ActionAssessment{Action: "resource.sample.relay.down", Effect: domain.ActionMutation,
			Changed: true, Recovery: domain.RecoveryRecreatable}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateOrphanShutdown(definition, test.assessment); (err == nil) != test.allowed {
				t.Fatalf("shutdown policy allowed=%v, want=%v: %v", err == nil, test.allowed, err)
			}
		})
	}
}

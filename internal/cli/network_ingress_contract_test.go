package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

func TestNetworkServiceResolvesPublicIngressForSavedNeighborBinding(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	for name, contents := range map[string]string{
		"work": "YARD_KIND=vm\nENVIRONMENT_PROFILES=\n",
		"vpn":  "YARD_KIND=vm\nENVIRONMENT_PROFILES=sample\n",
	} {
		path := filepath.Join(root, "state", "yards", name, "config.env")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		writeCLIFile(t, path, contents, 0o600)
	}
	writeNetworkIngressFixture(t, root)
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment,
		NetworkPolicy: &yardnetwork.Service{}})
	if err != nil {
		t.Fatal(err)
	}
	work, err := program.loadContext("work")
	if err != nil {
		t.Fatal(err)
	}
	vpn, err := program.loadInventoryLoaded("vpn", work)
	if err != nil {
		t.Fatal(err)
	}
	service := program.networkService([]domain.Context{work.Context})
	savedVPN := networkYard(vpn.Context)
	if _, err := program.networkService(nil).ContractSource(savedVPN); err == nil {
		t.Fatal("missing network inventory root was accepted")
	}
	contracts, err := service.ContractSource(savedVPN)
	if err != nil || len(contracts) != 1 || contracts[0].Profile != "sample" || contracts[0].Device != "sample-relay" {
		t.Fatalf("saved neighbor VPN binding lost its selected contract: %+v, %v", contracts, err)
	}
	wrongIdentity := savedVPN
	wrongIdentity.Project = "other-project"
	if _, err := service.ContractSource(wrongIdentity); err == nil {
		t.Fatal("saved binding with mismatched yard identity was accepted")
	}
	if err := os.Remove(filepath.Join(root, "state", "yards", "vpn", "config.env")); err != nil {
		t.Fatal(err)
	}
	contracts, err = service.ContractSource(savedVPN)
	if err != nil || len(contracts) != 0 {
		t.Fatalf("removed yard registration still authorized ingress: %+v, %v", contracts, err)
	}
}

func writeNetworkIngressFixture(t *testing.T, root string) {
	t.Helper()
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
}

func TestRegisteredMigrationRetainsCurrentNetworkInventoryWithoutPowerScope(t *testing.T) {
	program, execution, native := adoptionFixture(t)
	writeNetworkIngressFixture(t, program.options.RepositoryRoot)
	helper, err := os.ReadFile(filepath.Join(repositoryRoot(t), "scripts/lib/project-config.sh"))
	if err != nil {
		t.Fatal(err)
	}
	helperPath := filepath.Join(program.options.RepositoryRoot, "scripts/lib/project-config.sh")
	if err := os.MkdirAll(filepath.Dir(helperPath), 0700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, helperPath, string(helper), 0600)
	policy, err := yardnetwork.Decode(nil)
	if err != nil {
		t.Fatal(err)
	}
	host := &networkCLIHost{policy: policy, ready: true, instances: map[string]ports.InstanceInfo{execution.loaded.Context.YardName: native.Reconcile.Instance}}
	options := program.options
	options.InitPlatform, options.Incus = nil, native
	options.NetworkPolicy = &yardnetwork.Service{Host: host, Lock: testNetworkPolicyLock{}}
	program, err = New(options)
	if err != nil {
		t.Fatal(err)
	}
	native.Instances = map[string]ports.InstanceInfo{execution.loaded.Context.IncusProject + "/" + execution.loaded.Context.YardInstanceName: native.Reconcile.Instance}
	loaded, err := program.resolveReleaseTransitionContext("default", execution.loaded.Context.Paths.ConfigHome)
	if err != nil {
		t.Fatal(err)
	}
	for _, selected := range strings.Fields(loaded.Environment["ENVIRONMENT_PROFILES"]) {
		if selected == "sample" {
			t.Fatal("fixture must leave the global ingress profile unselected")
		}
	}
	runtime := program.initPlatform(loaded, nil).(reconcileruntime.Runtime)
	if runtime.PowerYards != nil {
		t.Fatal("network context expanded desired power scope")
	}
	owner := program.registeredYardMigration(releasetransition.ProcessRequest{ConfigHome: loaded.Context.Paths.ConfigHome}, false, loaded.Context.YardName)
	if _, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{}); err != nil {
		t.Fatal("registered migration lost current network inventory", err)
	}
	if host.reads == 0 || host.writes != 0 || native.writes != 0 {
		t.Fatalf("assessment bypassed native policy or wrote state: reads=%d writes=%d native=%d", host.reads, host.writes, native.writes)
	}
}

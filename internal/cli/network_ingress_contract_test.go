package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
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

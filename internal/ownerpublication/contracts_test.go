package ownerpublication

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestProfileDashboardContractSurvivesInstalledSnapshot(t *testing.T) {
	root := testkit.TempDir(t)
	dir := filepath.Join(root, "config", "profiles", "synthetic")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(dir, "owner.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o700)
	definition := profile.Definition{SchemaVersion: 1, OwnerService: "owner.sh",
		Runtime:   &profile.RuntimeHook{ActivationID: "sample-runtime", Handler: "owner.sh"},
		Settings:  []profile.Setting{{Name: "SAMPLE_PORT", Type: "port", Scopes: []string{"shipped"}, Application: "yard-init", Minimum: 1024, Maximum: 65535, HostListener: true}},
		Dashboard: &profile.IntegrationDashboard{Scheme: "http", Path: "/", PortSetting: "SAMPLE_PORT", GuestPort: 3000, ContainerRoute: &profile.IntegrationRoute{MarkerKey: "user.subyard.sample", DeviceName: "sample-dashboard"}}}
	data, _ := json.Marshal(definition)
	testkit.WriteFile(t, filepath.Join(dir, "profile.json"), data, 0o600)
	loaded, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := Encode(loaded)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	for _, contracts := range [][]Contract{loaded, installed} {
		found := false
		for _, contract := range contracts {
			if contract.Device != "sample-dashboard" {
				continue
			}
			found = true
			device := map[string]string{"type": "proxy", "bind": "host", "listen": "tcp:100.64.1.2:22222", "connect": "tcp:127.0.0.1:3000"}
			if !contract.Owns("v2:100.64.1.2:22222", device) || contract.Owns("foreign", device) {
				t.Fatal("profile ownership differs")
			}
		}
		if !found {
			t.Fatal("profile dashboard declaration was omitted")
		}
	}
}

func TestRetiredRouteLoadsOnlyCancellationAuthority(t *testing.T) {
	root := testkit.TempDir(t)
	dir := filepath.Join(root, "config", "retired-integrations", "synthetic")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(dir, "retire.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o700)
	definition := profile.IntegrationDefinition{SchemaVersion: 1, Retired: true,
		Lifecycle:     &profile.IntegrationLifecycle{Handler: "retire.sh", InstanceConfigKeys: []string{"user.subyard.retired"}, InstanceDevices: []string{"retired-dashboard"}},
		RetiredRoutes: []profile.RetiredRoute{{MarkerKey: "user.subyard.retired", DeviceName: "retired-dashboard", GuestPort: 3000}}}
	data, _ := json.Marshal(definition)
	testkit.WriteFile(t, filepath.Join(dir, "integration.json"), data, 0o600)
	loaded, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := Encode(loaded)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	for _, contracts := range [][]Contract{loaded, installed} {
		found := false
		for _, contract := range contracts {
			if contract.Device == "retired-dashboard" {
				found = true
				if !contract.CleanupOnly {
					t.Fatal("retired declaration gained restoration authority")
				}
			}
		}
		if !found {
			t.Fatal("known retired journal omitted")
		}
	}
}

func TestResourceOwnershipMatchesLoadedAndInstalledContracts(t *testing.T) {
	for _, policy := range []resource.ProxyAddressPolicy{resource.ProxyAddressTailscaleOnly, resource.ProxyAddressLoopbackOrTailscale} {
		t.Run(string(policy), func(t *testing.T) {
			root := testkit.TempDir(t)
			directory := filepath.Join(root, "config", "profiles", "synthetic", "resources")
			if err := os.MkdirAll(filepath.Join(directory, "endpoint"), 0o700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(directory, "endpoint", "handler.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o700)
			descriptor := fmt.Sprintf("COMMAND=endpoint\nHANDLER=resources/endpoint/handler.sh\nTITLE=Endpoint\nPROXY=\"optional HOST PORT tcp:127.0.0.1:80 owner-metadata-v1 %s\"\nACTION=\"up up security-change reversible\"\nACTION=\"down down security-change reversible\"\nBRINGUP=up\nSHUTDOWN=down\n", policy)
			testkit.WriteFile(t, filepath.Join(directory, "endpoint.res"), []byte(descriptor), 0o600)
			loaded, err := Load(root)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := Encode(loaded)
			if err != nil {
				t.Fatal(err)
			}
			installed, err := Decode(encoded)
			if err != nil {
				t.Fatal(err)
			}
			var live, snapshot Contract
			for i, contract := range loaded {
				if contract.Device == "optional" {
					live, snapshot = contract, installed[i]
				}
			}
			if live.Owns == nil || snapshot.Owns == nil {
				t.Fatal("synthetic owning declaration was omitted")
			}
			proxy := resource.ProxyContract{Device: "optional", Connect: "tcp:127.0.0.1:80", AddressPolicy: policy}
			for _, host := range []string{"100.64.1.20", "127.0.0.1", "0.0.0.0", "192.168.1.20"} {
				device := map[string]string{"type": "proxy", "bind": "host", "listen": "tcp:" + host + ":8080", "connect": proxy.Connect}
				marker := proxy.OwnershipValue(device)
				want := host == "100.64.1.20" || (host == "127.0.0.1" && policy == resource.ProxyAddressLoopbackOrTailscale)
				for _, receipt := range []string{marker, strings.Replace(marker, "v1:", "v1:pending:", 1), "foreign"} {
					expected := want && receipt != "foreign"
					if live.Owns(receipt, device) != expected || snapshot.Owns(receipt, device) != expected {
						t.Fatalf("ownership differs for %s %s", host, receipt)
					}
				}
				for _, change := range []string{"connect", "extra-key"} {
					divergent := maps.Clone(device)
					if change == "connect" {
						divergent["connect"] = "tcp:127.0.0.1:81"
					} else {
						divergent["nat"] = "false"
					}
					if live.Owns(marker, divergent) || snapshot.Owns(marker, divergent) {
						t.Fatalf("%s accepted divergent %s device", host, change)
					}
				}
			}
		})
	}
}

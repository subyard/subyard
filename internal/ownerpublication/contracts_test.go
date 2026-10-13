package ownerpublication

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/testkit"
)

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

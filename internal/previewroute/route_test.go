package previewroute

import (
	"context"
	"fmt"
	"github.com/Subyard/Subyard/internal/testkit"
	"maps"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOwnedPreviewRoute(t *testing.T) {
	for _, test := range []struct {
		name, marker, host, port string
		owned, ready             bool
	}{
		{"ready", "v1:100.101.102.103:32222", "100.101.102.103", "32222", true, true},
		{"pending", "v1:pending:100.101.102.103:32222", "100.101.102.103", "32222", true, false},
		{"foreign", "", "100.101.102.103", "32222", false, false},
		{"unknown version", "v2:100.101.102.103:32222", "100.101.102.103", "32222", false, false},
		{"loopback", "v1:127.0.0.1:32222", "127.0.0.1", "32222", false, false},
		{"wildcard", "v1:0.0.0.0:32222", "0.0.0.0", "32222", false, false},
		{"LAN", "v1:192.168.1.2:32222", "192.168.1.2", "32222", true, true},
		{"privileged port", "v1:100.101.102.103:80", "100.101.102.103", "80", false, false},
		{"noncanonical port", "v1:100.101.102.103:032222", "100.101.102.103", "032222", false, false},
		{"different address", "v1:100.101.102.103:32222", "100.101.102.104", "32222", false, false},
		{"different port", "v1:100.101.102.103:32222", "100.101.102.103", "32223", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			device := map[string]string{"type": "proxy", "bind": "host",
				"listen": "tcp:" + test.host + ":" + test.port, "connect": "tcp:127.0.0.1:8765"}
			host, port, ready := Owned(test.marker, device)
			if (host != "") != test.owned || ready != test.ready || (test.owned && (host != test.host || port != test.port)) {
				t.Fatalf("Owned = %q, %q, %v", host, port, ready)
			}
		})
	}
	for _, mutation := range []string{"target", "extra option"} {
		t.Run(mutation, func(t *testing.T) {
			device := Device("100.101.102.103", "32222")
			if mutation == "target" {
				device["connect"] = "tcp:127.0.0.1:8080"
			} else {
				device["nat"] = "true"
			}
			if host, _, _ := Owned("v1:100.101.102.103:32222", device); host != "" {
				t.Fatal("divergent device accepted")
			}
		})
	}
}

func TestPreviewMetadata(t *testing.T) {
	for _, test := range []struct{ host, port, want string }{
		{"100.101.102.103", "32222", "{\"version\":1,\"host\":\"100.101.102.103\",\"port\":32222}\n"},
		{"127.0.0.1", "32222", "{\"version\":1,\"host\":\"127.0.0.1\",\"port\":8765}\n"},
	} {
		if got := string(Metadata(test.host, test.port)); got != test.want {
			t.Fatalf("metadata = %q, want %q", got, test.want)
		}
	}
}

func TestOwnedVMPreviewRoute(t *testing.T) {
	for _, marker := range []string{"v2:192.168.1.20:32222:10.80.0.10", "v2:pending:192.168.1.20:32222:10.80.0.10"} {
		device := Device("192.168.1.20", "32222", "10.80.0.10")
		host, port, ready := Owned(marker, device)
		if host != "192.168.1.20" || port != "32222" || ready != !strings.Contains(marker, "pending:") {
			t.Fatalf("Owned(%q) = %q %q %v", marker, host, port, ready)
		}
		for _, change := range []string{"nat", "target", "public guest", "foreign option"} {
			bad := maps.Clone(device)
			badMarker := marker
			switch change {
			case "nat":
				delete(bad, "nat")
			case "target":
				bad["connect"] = "tcp:10.80.0.11:8765"
			case "public guest":
				badMarker = strings.ReplaceAll(marker, "10.80.0.10", "203.0.113.1")
				bad["connect"] = "tcp:203.0.113.1:8765"
			case "foreign option":
				bad["proxy_protocol"] = "true"
			}
			if host, _, _ := Owned(badMarker, bad); host != "" {
				t.Fatalf("accepted %s", change)
			}
		}
	}
	want := "{\"version\":1,\"host\":\"192.168.1.20\",\"port\":32222,\"bindHost\":\"10.80.0.10\"}\n"
	if got := string(Metadata("192.168.1.20", "32222", "10.80.0.10")); got != want {
		t.Fatalf("metadata = %q", got)
	}
	if got := string(Metadata("127.0.0.1", "32222", "10.80.0.10")); strings.Contains(got, "bindHost") {
		t.Fatal("fallback exposed a guest listener")
	}
}

func TestPreviewAddresses(t *testing.T) {
	for _, host := range []string{"10.0.0.1", "172.16.0.1", "172.31.255.255", "192.168.0.1", "100.64.0.1"} {
		if !Address(host) {
			t.Fatalf("private host rejected: %s", host)
		}
	}
	for _, host := range []string{"172.15.0.1", "172.32.0.1", "192.169.0.1", "203.0.113.1", "127.0.0.1", "0.0.0.0", "10.00.0.1", "::1"} {
		if Address(host) {
			t.Fatalf("unsafe host accepted: %s", host)
		}
	}
}

func TestHostUsesOnlyActivePrivateDefaultSource(t *testing.T) {
	bin := testkit.TempDir(t)
	testkit.WriteFile(t, filepath.Join(bin, "tailscale"), []byte("#!/bin/sh\nexit 1\n"), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
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
	for _, source := range []string{"203.0.113.1", "127.0.0.1", "10.254.255.254", private} {
		if source == "" {
			continue
		}
		testkit.WriteFile(t, filepath.Join(bin, "ip"), []byte(fmt.Sprintf("#!/bin/sh\nprintf '[{\"prefsrc\":\"%s\"}]\\n'\n", source)), 0o755)
		want := "127.0.0.1"
		if source == private {
			want = private
		}
		if got := Host(context.Background()); got != want {
			t.Fatalf("source %s: got %s want %s", source, got, want)
		}
	}
}

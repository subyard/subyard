// Package previewroute defines the owned static-preview endpoint.
package previewroute

import (
	"context"
	"encoding/json"
	"maps"
	"net"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/observerroute"
)

const Key = "user.subyard.preview_proxy"
const DeviceName = "subyard-preview"
const EndpointKey = "user.subyard.preview_endpoint_sha256"

func Device(host, port string, guest ...string) map[string]string {
	device := map[string]string{"type": "proxy", "bind": "host",
		"listen": "tcp:" + host + ":" + port, "connect": "tcp:127.0.0.1:8765"}
	if len(guest) != 0 && guest[0] != "" {
		device["nat"] = "true"
		device["connect"] = "tcp:" + guest[0] + ":8765"
	}
	return device
}

// Address permits only canonical Tailscale or RFC1918 IPv4 addresses.
func Address(host string) bool {
	address, err := netip.ParseAddr(host)
	return err == nil && address.Is4() && address.String() == host &&
		(address.IsPrivate() || observerroute.TailscaleAddress(host))
}

// Host prefers active Tailscale, then the private source of the owner's default route.
func Host(ctx context.Context) string {
	if host := observerroute.Host(ctx); host != "127.0.0.1" {
		return host
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "ip", "-j", "-4", "route", "get", "1.1.1.1").Output()
	var routes []struct {
		Source string `json:"prefsrc"`
	}
	if err != nil || json.Unmarshal(output, &routes) != nil || len(routes) != 1 {
		return "127.0.0.1"
	}
	host := routes[0].Source
	address, err := netip.ParseAddr(host)
	if err != nil || !address.Is4() || !address.IsPrivate() || address.String() != host {
		return "127.0.0.1"
	}
	interfaces, _ := net.Interfaces()
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addresses, _ := iface.Addrs()
		for _, local := range addresses {
			if ip, _, err := net.ParseCIDR(local.String()); err == nil && ip.String() == host {
				return host
			}
		}
	}
	return "127.0.0.1"
}

// Owned recognizes interrupted installation, but never a foreign device.
func Owned(marker string, device map[string]string) (host, port string, ready bool) {
	version, value, ok := strings.Cut(marker, ":")
	if !ok || (version != "v1" && version != "v2") {
		return "", "", false
	}
	value, pending := strings.CutPrefix(value, "pending:")
	var guest string
	parts := strings.Split(value, ":")
	if version == "v1" && len(parts) == 2 {
		host, port = parts[0], parts[1]
	} else if version == "v2" && len(parts) == 3 {
		host, port, guest = parts[0], parts[1], parts[2]
		address, err := netip.ParseAddr(guest)
		if err != nil || !address.Is4() || !address.IsPrivate() || address.String() != guest {
			return "", "", false
		}
	} else {
		return "", "", false
	}
	number, err := strconv.Atoi(port)
	if !Address(host) || err != nil ||
		number < 1024 || number > 65535 || strconv.Itoa(number) != port ||
		!maps.Equal(device, Device(host, port, guest)) {
		return "", "", false
	}
	return host, port, !pending
}

// Metadata describes the installed route and any additional exact VM listener.
func Metadata(host, port string, guest ...string) []byte {
	if host == "127.0.0.1" {
		port = "8765"
	}
	number, _ := strconv.Atoi(port)
	bindHost := ""
	if host != "127.0.0.1" && len(guest) != 0 {
		bindHost = guest[0]
	}
	payload, _ := json.Marshal(struct {
		Version  int    `json:"version"`
		Host     string `json:"host"`
		Port     int    `json:"port"`
		BindHost string `json:"bindHost,omitempty"`
	}{1, host, number, bindHost})
	return append(payload, '\n')
}

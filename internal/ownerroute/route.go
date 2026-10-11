// Package ownerroute provides owned loopback/Tailscale dashboard route primitives.
package ownerroute

import (
	"context"
	"maps"
	"net"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func TailscaleAddress(host string) bool {
	address, err := netip.ParseAddr(host)
	return err == nil && netip.MustParsePrefix("100.64.0.0/10").Contains(address)
}

// Host prefers the owner's active Tailscale IPv4; hosts without one retain SSH access.
func Host(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "tailscale", "ip", "-4").Output()
	host := strings.TrimSpace(string(output))
	if err != nil || !TailscaleAddress(host) {
		return "127.0.0.1"
	}
	interfaces, _ := net.Interfaces()
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addresses, _ := iface.Addrs()
		for _, address := range addresses {
			if ip, _, err := net.ParseCIDR(address.String()); err == nil && ip.String() == host {
				return host
			}
		}
	}
	return "127.0.0.1"
}

func Device(host, port string, guestPort int) map[string]string {
	return map[string]string{"type": "proxy", "bind": "host",
		"listen": "tcp:" + host + ":" + port, "connect": "tcp:127.0.0.1:" + strconv.Itoa(guestPort)}
}

// Owned accepts pending receipts for recovery, but never divergent or foreign devices.
func Owned(marker string, device map[string]string, guestPort int) (host, port string, ready bool) {
	version, value, ok := strings.Cut(marker, ":")
	if !ok {
		return "", "", false
	}
	value, pending := strings.CutPrefix(value, "pending:")
	switch version {
	case "v1":
		host, port = "127.0.0.1", value
	case "v2":
		host, port, ok = strings.Cut(value, ":")
		if !ok || !TailscaleAddress(host) {
			return "", "", false
		}
	default:
		return "", "", false
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1024 || number > 65535 || strconv.Itoa(number) != port ||
		!maps.Equal(device, Device(host, port, guestPort)) {
		return "", "", false
	}
	return host, port, !pending
}

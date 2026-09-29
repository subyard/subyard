package hostruntime

import (
	"net"
	"net/netip"
)

type OwnerIPv4 struct {
	Interface string
	Address   netip.Addr
}

// OwnerIPv4Addresses observes addresses assigned to active local interfaces.
// It does not use an external IP service, which could return an upstream NAT IP.
func OwnerIPv4Addresses() ([]OwnerIPv4, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var result []OwnerIPv4
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			return nil, err
		}
		for _, address := range addresses {
			prefix, err := netip.ParsePrefix(address.String())
			if err == nil && prefix.Addr().Is4() {
				result = append(result, OwnerIPv4{Interface: iface.Name, Address: prefix.Addr()})
			}
		}
	}
	return result, nil
}

// PublicEndpointIPv4 is deliberately conservative for automatic publication.
// Explicit owner addresses may still use private networks for test deployments.
func PublicEndpointIPv4(address netip.Addr) bool {
	if !address.Is4() || !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() {
		return false
	}
	for _, reserved := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24",
		"192.88.99.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
	} {
		if netip.MustParsePrefix(reserved).Contains(address) {
			return false
		}
	}
	return true
}

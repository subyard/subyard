package hostruntime

import (
	"net"
	"net/netip"
)

// LocalAddresses observes active local interfaces without contacting a network service.
func LocalAddresses() ([]netip.Addr, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var result []netip.Addr
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			return nil, err
		}
		for _, address := range addresses {
			prefix, err := netip.ParsePrefix(address.String())
			if err != nil {
				return nil, err
			}
			local := prefix.Addr().Unmap()
			result = append(result, local)
			if local.Is6() {
				result = append(result, local.WithZone(iface.Name))
			}
		}
	}
	return result, nil
}

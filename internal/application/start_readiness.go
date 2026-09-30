package application

import (
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

// StartAddressReadiness describes a local prerequisite, without changing desired power
// or probing the listener, DNS, or an external network service.
func StartAddressReadiness(
	instance ports.InstanceInfo,
	localAddresses func() ([]netip.Addr, error),
) (domain.StartState, []string, error) {
	if !strings.EqualFold(instance.Status, "stopped") ||
		instance.Config["user.subyard.managed"] != "true" ||
		instance.Config["user.subyard.initialized"] != "true" ||
		instance.Config["user.subyard.desired_power"] != PowerRunning {
		return "", nil, nil
	}
	required := make(map[netip.Addr]struct{})
	for name, device := range instance.Devices {
		if device["type"] != "proxy" || device["nat"] == "true" ||
			(device["bind"] != "" && device["bind"] != "host") {
			continue
		}
		protocol, endpoint, _ := strings.Cut(device["listen"], ":")
		if protocol != "tcp" && protocol != "udp" {
			continue
		}
		host, port, err := net.SplitHostPort(endpoint)
		if err != nil || port == "" {
			return "", nil, fmt.Errorf("proxy device %q has an invalid listen endpoint", name)
		}
		address, err := netip.ParseAddr(host)
		if err != nil {
			return "", nil, fmt.Errorf("proxy device %q requires a literal listen address", name)
		}
		address = address.Unmap()
		if !address.IsUnspecified() && !address.IsLoopback() {
			required[address] = struct{}{}
		}
	}
	if len(required) == 0 {
		return "", nil, nil
	}
	if localAddresses == nil {
		return "", nil, fmt.Errorf("local host address reader is required")
	}
	addresses, err := localAddresses()
	if err != nil {
		return "", nil, fmt.Errorf("read local host addresses: %w", err)
	}
	for _, address := range addresses {
		delete(required, address.Unmap())
	}
	var missing []string
	for address := range required {
		missing = append(missing, address.String())
	}
	slices.Sort(missing)
	if len(missing) == 0 {
		return "", nil, nil
	}
	return domain.StartWaitingForAddress, missing, nil
}

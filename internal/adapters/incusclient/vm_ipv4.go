package incusclient

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/yardnetwork"
	"github.com/lxc/incus/v6/shared/api"
)

// PinVMIPv4 validates the dedicated VM's default-profile reservation. On
// its first guarded boot, apply reserves the address proven by both the guest
// NIC and the bridge's MAC-matched DHCP lease. Existing pins are never changed.
func (client *Client) PinVMIPv4(ctx context.Context, yard yardnetwork.Yard, apply bool) (bool, error) {
	if yard.Project == "" || yard.Instance == "" || yard.Network == "" {
		return false, errors.New("VM network identity is incomplete")
	}
	snapshot, err := client.InspectNetwork(ctx, []yardnetwork.Yard{yard})
	if err != nil {
		return false, err
	}
	var observed *yardnetwork.ObservedYard
	for index := range snapshot.Yards {
		if snapshot.Yards[index].Yard == yard {
			observed = &snapshot.Yards[index]
			break
		}
	}
	if observed == nil || !observed.ProjectFound || !observed.ProfileFound || !observed.InstanceFound {
		return false, nil
	}
	bridge, found := snapshot.Networks[yard.Network]
	if !found || bridge.Type != "bridge" {
		return false, fmt.Errorf("VM network %q must be a managed bridge", yard.Network)
	}
	subnet, err := netip.ParsePrefix(bridge.Config["ipv4.address"])
	if err != nil || !subnet.Addr().Is4() {
		return false, fmt.Errorf("VM bridge %q has no valid IPv4 subnet", yard.Network)
	}
	if observed.InstanceInfo.Type != domain.YardVM {
		return false, errors.New("VM yard instance must be a virtual machine")
	}
	if _, exists := observed.InstanceInfo.LocalDevices["eth0"]; exists {
		return false, errors.New("VM must not override the default-profile eth0 device")
	}
	for name, device := range observed.InstanceInfo.Devices {
		if name != "eth0" && device["type"] == "nic" {
			return false, fmt.Errorf("VM has unexpected NIC %q", name)
		}
	}
	nic := observed.ProfileDevices["eth0"]
	if nic["type"] != "nic" || deviceNetwork(nic) != yard.Network ||
		(nic["name"] != "" && nic["name"] != "eth0") {
		return false, errors.New("VM default-profile eth0 does not match its managed bridge")
	}
	users := profileInstanceClaims(yard.Project, observed.ProfileUsedBy)
	if len(users) != 1 || users[0].Project != yard.Project || users[0].Instance != yard.Instance {
		return false, errors.New("VM default profile must be used by its sole instance")
	}
	mac := canonicalMAC(observed.MAC)
	if mac == "" {
		return false, errors.New("VM eth0 has no valid MAC")
	}
	address := nic["ipv4.address"]
	if address != "" {
		if err := validateVMIPv4Address(address, subnet); err != nil {
			return false, err
		}
		if observed.IPv4 != "" && observed.IPv4 != address {
			return false, fmt.Errorf("VM pinned IPv4 %q differs from observed address %q", address, observed.IPv4)
		}
		if err := validateVMIPv4Claims(snapshot, yard, address, mac); err != nil {
			return false, err
		}
		return true, nil
	}
	if !apply {
		return false, nil
	}
	if !strings.EqualFold(observed.InstanceInfo.Status, "running") {
		return false, errors.New("VM must be running for its first IPv4 pin")
	}
	server, err := client.networkServer(ctx)
	if err != nil {
		return false, err
	}
	projectServer := server.UseProject(yard.Project)
	instance, _, err := projectServer.GetInstance(yard.Instance)
	if err != nil {
		return false, normalizeError("get VM for IPv4 pin", err)
	}
	state, _, err := projectServer.GetInstanceState(yard.Instance)
	if err != nil {
		return false, normalizeError("get VM NIC state for IPv4 pin", err)
	}
	address = vmGuestIPv4(instance, state, mac)
	if address == "" {
		return false, errors.New("VM guest eth0 has no unique IPv4 address matching its MAC")
	}
	if err := validateVMIPv4Address(address, subnet); err != nil {
		return false, err
	}
	leases, err := projectServer.GetNetworkLeases(yard.Network)
	if err != nil {
		return false, normalizeError("get VM bridge leases for IPv4 pin", err)
	}
	matching := 0
	for _, lease := range leases {
		if canonicalMAC(lease.Hwaddr) != mac {
			continue
		}
		leaseIP, err := netip.ParseAddr(lease.Address)
		if err != nil || !leaseIP.Is4() {
			continue
		}
		if lease.Address != address {
			return false, fmt.Errorf("VM MAC has conflicting lease %q", lease.Address)
		}
		matching++
	}
	if matching != 1 {
		return false, errors.New("VM guest IPv4 needs exactly one MAC-matched bridge lease")
	}
	if err := validateVMIPv4Claims(snapshot, yard, address, mac); err != nil {
		return false, err
	}
	nic = cloneMap(nic)
	nic["ipv4.address"] = address
	if err := client.WriteProfileNIC(ctx, *observed, nic); err != nil {
		return false, err
	}
	return true, nil
}

func validateVMIPv4Address(address string, subnet netip.Prefix) error {
	ip, err := netip.ParseAddr(address)
	if err != nil || !ip.Is4() || !ip.IsPrivate() || !subnet.Contains(ip) ||
		ip == subnet.Addr() || ip == subnet.Masked().Addr() {
		return fmt.Errorf("VM IPv4 %q is outside the private bridge subnet", address)
	}
	return nil
}

func validateVMIPv4Claims(snapshot yardnetwork.Snapshot, yard yardnetwork.Yard, address, mac string) error {
	claims := snapshot.ReservationClaims[yard.Network][address]
	if len(claims) == 0 {
		return errors.New("VM IPv4 has no bridge reservation")
	}
	for _, claim := range claims {
		if claim.Project != yard.Project || claim.Instance != yard.Instance ||
			(claim.MAC != "" && claim.MAC != mac) {
			return fmt.Errorf("VM IPv4 %q has another reservation owner", address)
		}
	}
	return nil
}

func vmGuestIPv4(instance *api.Instance, state *api.InstanceState, mac string) string {
	if instance == nil || state == nil || mac == "" {
		return ""
	}
	result := ""
	matched := false
	for _, nic := range state.Network {
		if canonicalMAC(nic.Hwaddr) != mac {
			continue
		}
		if matched {
			return ""
		}
		matched = true
		for _, address := range nic.Addresses {
			ip, err := netip.ParseAddr(address.Address)
			if err != nil || address.Family != "inet" || address.Scope == "link" || !ip.Is4() {
				continue
			}
			if result != "" && result != address.Address {
				return ""
			}
			result = address.Address
		}
	}
	return result
}

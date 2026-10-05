package yardnetwork

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/resource"
)

// ClearBootStaleUDP runs after a successful boot-time start or resource
// activation, while the caller holds the host network lock. It derives
// endpoints from fresh Incus state, never from a user-supplied command or a
// profile-specific setting.
func (s Service) ClearBootStaleUDP(ctx context.Context, target Yard) error {
	if !s.UseApprovedIngress {
		return errors.New("boot UDP cleanup requires boot network policy mode")
	}
	stored, err := s.Host.ReadPolicy(ctx)
	if err != nil {
		return err
	}
	policy := stored.Policy
	if policy.Isolation != policy.AppliedIsolation || policy.Revision != policy.AppliedRevision ||
		len(policy.PendingStart) != 0 || len(policy.Removing) != 0 {
		return ErrNotConverged
	}
	snapshot, err := s.Host.InspectNetwork(ctx, []Yard{target})
	if err != nil {
		return err
	}
	if policy.Isolation {
		if err := applyApprovedBootIngress(&snapshot, policy); err != nil {
			return err
		}
	} else if len(policy.Bindings) != 0 {
		return ErrNotConverged
	}
	var observed *ObservedYard
	for index := range snapshot.Yards {
		if snapshot.Yards[index].Yard == target {
			observed = &snapshot.Yards[index]
			break
		}
	}
	if observed == nil {
		return errors.New("boot network observation omitted the started yard")
	}
	if !observed.InstanceFound {
		return ErrNotConverged
	}
	if observed.InstanceInfo.Type != domain.YardVM {
		return nil
	}
	if !strings.EqualFold(observed.InstanceInfo.Status, "running") {
		return ErrNotConverged
	}
	guest, err := netip.ParseAddr(observed.InstanceInfo.Devices["eth0"]["ipv4.address"])
	if err != nil || !guest.Is4() || !guest.IsPrivate() {
		if policy.Isolation {
			return ErrNotConverged
		}
		// Ordinary VMs without a pinned address have no eligible public route.
		return nil
	}
	var endpoints []netip.AddrPort
	if policy.Isolation {
		for _, binding := range policy.Bindings {
			if binding.Yard != target {
				continue
			}
			if binding.IPv4 != guest.String() {
				return ErrNotConverged
			}
			for _, approved := range binding.ApprovedIngress {
				if approved.Transport() != "udp" {
					continue
				}
				endpoint, parseErr := netip.ParseAddrPort(strings.TrimPrefix(approved.Listen, "udp:"))
				if parseErr != nil {
					return parseErr
				}
				endpoints = append(endpoints, endpoint)
			}
		}
	} else {
		for name, device := range observed.InstanceInfo.LocalDevices {
			if device["type"] != "proxy" || device["bind"] != "host" ||
				!strings.HasPrefix(device["listen"], "udp:") {
				continue
			}
			marker := observed.InstanceInfo.LocalConfig["user.subyard.resource."+name]
			if marker == "" || strings.HasPrefix(marker, "v1:pending:") {
				continue
			}
			if len(device) != 5 || device["nat"] != "true" ||
				!maps.Equal(device, observed.InstanceInfo.Devices[name]) ||
				marker != (resource.ProxyContract{}).OwnershipValue(device) {
				return fmt.Errorf("%w: owned public UDP proxy changed", ErrNotConverged)
			}
			endpoint, parseErr := netip.ParseAddrPort(strings.TrimPrefix(device["listen"], "udp:"))
			if parseErr != nil || !resource.ExplicitOwnerIPv4(endpoint.Addr()) || endpoint.Port() == 0 ||
				device["listen"] != "udp:"+endpoint.String() {
				return fmt.Errorf("%w: owned public UDP proxy has invalid endpoint", ErrNotConverged)
			}
			connect, connectErr := netip.ParseAddrPort(strings.TrimPrefix(device["connect"], "udp:"))
			if connectErr != nil || connect.Addr() != guest || connect.Port() == 0 ||
				device["connect"] != "udp:"+connect.String() {
				return fmt.Errorf("%w: owned public UDP proxy has invalid guest route", ErrNotConverged)
			}
			if !slices.Contains(endpoints, endpoint) {
				endpoints = append(endpoints, endpoint)
			}
		}
	}
	if len(endpoints) == 0 {
		return nil
	}
	if s.ClearStaleUDP == nil {
		return errors.New("boot UDP conntrack cleaner is unavailable")
	}
	for _, endpoint := range endpoints {
		if err := s.ClearStaleUDP(ctx, endpoint); err != nil {
			return fmt.Errorf("clear stale UDP state for verified endpoint: %w", err)
		}
	}
	return nil
}

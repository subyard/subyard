package incusclient

import (
	"context"
	"errors"
	"maps"
	"net/netip"
	"regexp"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

// SetOwnedResourceIngressIf applies one exact original startup route transition.
// Identity, primary NIC and causal route state are checked in the GET whose
// ETag protects the PUT; no name-based reinspection can rebase the request.
func (client *Client) SetOwnedResourceIngressIf(ctx context.Context, yard yardnetwork.Yard, contract resource.ProxyContract, uuid string,
	nic, before map[string]string, beforeMarker string, desired map[string]string, desiredMarker string) error {
	if uuid == "" || !domain.SafeID(uuid) || !contract.IsOwnerIPv4Ingress() || !contract.OwnershipMetadata || contract.Device == "" || contract.Profile == "" || contract.Resource == "" {
		return domain.ErrPlanStale
	}
	validMarker := func(device map[string]string, marker string) bool {
		if len(device) == 0 {
			return marker == "" || pendingOwnedIngress.MatchString(marker)
		}
		fingerprint := contract.OwnershipValue(device)
		return marker == fingerprint || marker == "v1:pending:"+strings.TrimPrefix(fingerprint, "v1:")
	}
	if !validMarker(before, beforeMarker) || !validMarker(desired, desiredMarker) {
		return domain.ErrPlanStale
	}
	if len(desired) != 0 {
		protocol := "udp"
		if contract.AddressPolicy == resource.ProxyAddressOwnerIPv4TCP {
			protocol = "tcp"
		}
		listen, listenErr := netip.ParseAddrPort(strings.TrimPrefix(desired["listen"], protocol+":"))
		guest, guestErr := netip.ParseAddrPort(strings.TrimPrefix(desired["connect"], protocol+":"))
		_, port, valid := contract.GuestEndpoint(int(listen.Port()))
		if listenErr != nil || guestErr != nil || !resource.ExplicitOwnerIPv4(listen.Addr()) || !guest.Addr().Is4() || !guest.Addr().IsPrivate() || !valid || int(guest.Port()) != port || guest.Addr().String() != nic["ipv4.address"] || len(desired) != 5 || desired["type"] != "proxy" || desired["bind"] != "host" || desired["nat"] != "true" || desired["listen"] != protocol+":"+listen.String() || desired["connect"] != protocol+":"+guest.String() || desiredMarker != contract.OwnershipValue(desired) {
			return domain.ErrPlanStale
		}
	}
	server, err := client.connect(ctx, true)
	if err != nil {
		return err
	}
	if err := client.validateServerExtensions(server); err != nil {
		return err
	}
	project := server.UseProject(yard.Project)
	instance, etag, err := project.GetInstance(yard.Instance)
	if err != nil {
		return normalizeError("get original startup ingress instance", err)
	}
	if instance == nil || instance.Name != yard.Instance || instance.Project != "" && instance.Project != yard.Project || instance.Type != "virtual-machine" || instance.Config["volatile.uuid"] != uuid || etag == "" || !maps.Equal(instance.Devices[contract.Device], before) || !maps.Equal(instance.ExpandedDevices[contract.Device], before) || instance.Config[contract.OwnershipKey()] != beforeMarker || instance.ExpandedConfig[contract.OwnershipKey()] != "" && instance.ExpandedConfig[contract.OwnershipKey()] != beforeMarker || !maps.Equal(instance.ExpandedDevices["eth0"], nic) {
		return domain.ErrPlanStale
	}
	if maps.Equal(before, desired) && beforeMarker == desiredMarker {
		return nil
	}
	if instance.Devices == nil {
		instance.Devices = map[string]map[string]string{}
	}
	if instance.Config == nil {
		instance.Config = map[string]string{}
	}
	if len(desired) == 0 {
		delete(instance.Devices, contract.Device)
		if desiredMarker == "" {
			delete(instance.Config, contract.OwnershipKey())
		} else {
			instance.Config[contract.OwnershipKey()] = desiredMarker
		}
	} else {
		instance.Devices[contract.Device] = maps.Clone(desired)
		instance.Config[contract.OwnershipKey()] = desiredMarker
	}
	return updateInstanceDevices(ctx, project, yard.Instance, instance, etag)
}

var pendingOwnedIngress = regexp.MustCompile(`^v1:pending:[0-9a-f]{64}$`)

// RemoveOwnedResourceIngress atomically closes the exact previously approved
// local route. When guest shutdown remains unverified, its existing pending
// ownership marker preserves retry intent. Incus ETag protects concurrent edits.
func (client *Client) RemoveOwnedResourceIngress(ctx context.Context, yard yardnetwork.Yard,
	contract resource.ProxyContract, expectedDevice map[string]string, expectedMarker string, retainPending bool) error {
	return client.removeOwnedResourceIngress(ctx, yard, contract, "", nil, expectedDevice, expectedMarker, retainPending)
}

func (client *Client) RemoveOwnedResourceIngressIf(ctx context.Context, yard yardnetwork.Yard, contract resource.ProxyContract, uuid string, nic, expectedDevice map[string]string, expectedMarker string, retainPending bool) error {
	if uuid == "" || !domain.SafeID(uuid) {
		return domain.ErrPlanStale
	}
	return client.removeOwnedResourceIngress(ctx, yard, contract, uuid, nic, expectedDevice, expectedMarker, retainPending)
}
func (client *Client) removeOwnedResourceIngress(ctx context.Context, yard yardnetwork.Yard, contract resource.ProxyContract, uuid string, nic, expectedDevice map[string]string, expectedMarker string, retainPending bool) error {
	if !contract.IsOwnerIPv4Ingress() || !contract.OwnershipMetadata ||
		contract.Device == "" || contract.Profile == "" || contract.Resource == "" {
		return errors.New("invalid public ingress contract")
	}
	if _, _, valid := contract.GuestEndpoint(1); !valid {
		return errors.New("invalid public ingress guest port")
	}
	if len(expectedDevice) == 0 {
		if !pendingOwnedIngress.MatchString(expectedMarker) {
			return errors.New("invalid pending ingress marker")
		}
	} else {
		fingerprint := contract.OwnershipValue(expectedDevice)
		if expectedMarker != fingerprint && expectedMarker != "v1:pending:"+strings.TrimPrefix(fingerprint, "v1:") {
			return errors.New("invalid owned ingress fingerprint")
		}
	}
	server, err := client.connect(ctx, true)
	if err != nil {
		return err
	}
	if err := client.validateServerExtensions(server); err != nil {
		return err
	}
	project := server.UseProject(yard.Project)
	instance, etag, err := project.GetInstance(yard.Instance)
	if err != nil {
		return normalizeError("get instance for ingress closure", err)
	}
	if uuid != "" && (instance == nil || instance.Name != yard.Instance || instance.Project != "" && instance.Project != yard.Project || instance.Type != "virtual-machine" || instance.Config["volatile.uuid"] != uuid || etag == "" || !maps.Equal(instance.ExpandedDevices["eth0"], nic) || !maps.Equal(instance.ExpandedDevices[contract.Device], expectedDevice) || instance.ExpandedConfig[contract.OwnershipKey()] != "" && instance.ExpandedConfig[contract.OwnershipKey()] != expectedMarker) {
		return domain.ErrPlanStale
	}
	if !maps.Equal(instance.Devices[contract.Device], expectedDevice) ||
		instance.Config[contract.OwnershipKey()] != expectedMarker ||
		len(expectedDevice) == 0 && len(instance.ExpandedDevices[contract.Device]) != 0 {
		return domain.ErrPlanStale
	}
	if len(expectedDevice) == 0 && retainPending {
		return nil
	}
	delete(instance.Devices, contract.Device)
	if retainPending {
		if len(expectedDevice) != 0 {
			instance.Config[contract.OwnershipKey()] = "v1:pending:" +
				strings.TrimPrefix(contract.OwnershipValue(expectedDevice), "v1:")
		}
	} else {
		delete(instance.Config, contract.OwnershipKey())
	}
	return updateInstanceDevices(ctx, project, yard.Instance, instance, etag)
}

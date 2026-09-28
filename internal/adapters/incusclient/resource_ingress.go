package incusclient

import (
	"context"
	"errors"
	"maps"
	"regexp"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

var pendingOwnedIngress = regexp.MustCompile(`^v1:pending:[0-9a-f]{64}$`)

// RemoveOwnedResourceIngress atomically closes the exact previously approved
// local route. When guest shutdown remains unverified, its existing pending
// ownership marker preserves retry intent. Incus ETag protects concurrent edits.
func (client *Client) RemoveOwnedResourceIngress(ctx context.Context, yard yardnetwork.Yard,
	contract resource.ProxyContract, expectedDevice map[string]string, expectedMarker string, retainPending bool) error {
	if contract.AddressPolicy != resource.ProxyAddressOwnerIPv4UDP || !contract.OwnershipMetadata ||
		contract.Device == "" || contract.Profile == "" || contract.Resource == "" {
		return errors.New("invalid public ingress contract")
	}
	if _, valid := contract.GuestUDPPort(); !valid {
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
	return updateInstanceDevices(project, yard.Instance, instance, etag)
}

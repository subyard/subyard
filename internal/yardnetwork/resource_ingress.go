package yardnetwork

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/resource"
)

// IngressPreview compares the current policy with the policy after one exact
// typed proxy mutation. It performs no writes and never changes links or
// isolation mode.
type IngressPreview struct {
	Before Plan
	After  Plan
	Target Yard
}

var pendingIngressMarker = regexp.MustCompile(`^v1:pending:[0-9a-f]{64}$`)

// ValidateAfter accepts only the target ACL update predicted before consent.
// The route mutation must produce the predicted snapshot; concurrent network
// or instance changes require a fresh assessment before applying any ACL update.
func (preview IngressPreview) ValidateAfter(actual Plan) error {
	if err := scopedResourceIngressPlan(actual, preview.Target); err != nil {
		return err
	}
	if actual.Fingerprint != preview.After.Fingerprint {
		return errors.New("resource ingress route or network snapshot changed after approval")
	}
	if !reflect.DeepEqual(actual.Policy, preview.After.Policy) || actual.Changed != preview.After.Changed ||
		actual.Physical != preview.After.Physical || len(actual.Updates) != len(preview.After.Updates) {
		return errors.New("resource ingress network effects changed after approval")
	}
	for i, expected := range preview.After.Updates {
		got := actual.Updates[i]
		if got.Yard.Yard != expected.Yard.Yard || !maps.Equal(got.NIC, expected.NIC) ||
			got.Access != expected.Access || got.RemoveACL != expected.RemoveACL || !reflect.DeepEqual(got.ACL, expected.ACL) {
			return errors.New("resource ingress network update changed after approval")
		}
	}
	return nil
}

// ValidateRollback requires a fresh down preview with no route or marker left
// to remove, then permits only removal of the target's exact UDP allowance.
// A failed resource up must not turn this cleanup into a general reconcile.
func (preview IngressPreview) ValidateRollback(closure IngressPreview, contract resource.ProxyContract) error {
	if closure.Target != preview.Target || closure.Before.Fingerprint == "" ||
		closure.Before.Fingerprint != closure.After.Fingerprint {
		return errors.New("resource ingress rollback left a proxy or ownership marker")
	}
	actual := closure.Before
	if err := scopedResourceIngressPlan(actual, preview.Target); err != nil {
		return err
	}
	if !actual.Changed {
		return nil
	}
	guestPort, valid := contract.GuestUDPPort()
	if !valid || contract.AddressPolicy != resource.ProxyAddressOwnerIPv4UDP ||
		preview.Before.Changed || len(actual.Updates) > 1 || actual.Change != (Change{}) {
		return errors.New("resource ingress rollback has unapproved network effects")
	}
	expectedPolicy := actual.Before.Policy
	expectedPolicy.Bindings = slices.Clone(expectedPolicy.Bindings)
	removedApproval := false
	for index := range expectedPolicy.Bindings {
		binding := &expectedPolicy.Bindings[index]
		if binding.Yard != preview.Target {
			continue
		}
		binding.ApprovedIngress = slices.Clone(binding.ApprovedIngress)
		for approvedIndex, approved := range binding.ApprovedIngress {
			if approved.Device == contract.Device && approved.GuestPort == guestPort {
				binding.ApprovedIngress = slices.Delete(binding.ApprovedIngress, approvedIndex, approvedIndex+1)
				if len(binding.ApprovedIngress) == 0 {
					binding.ApprovedIngress = nil
				}
				removedApproval = true
				break
			}
		}
	}
	if !removedApproval {
		return errors.New("resource ingress rollback has no approved route to close")
	}
	expectedPolicy.Revision++
	if !reflect.DeepEqual(actual.Policy, expectedPolicy) {
		return errors.New("resource ingress rollback would change network policy")
	}
	if !actual.Physical {
		if len(actual.Updates) != 0 {
			return errors.New("resource ingress rollback has unapproved network effects")
		}
		return nil // Another owned route may still require the same guest UDP ACL port.
	}
	if len(actual.Updates) != 1 {
		return errors.New("resource ingress rollback has unapproved network effects")
	}
	update := actual.Updates[0]
	current := update.Yard.ACL
	wanted := update.ACL
	if update.Yard.Yard != preview.Target || update.RemoveACL ||
		!maps.Equal(update.NIC, update.Yard.ProfileDevices["eth0"]) ||
		update.Access != update.Yard.ProjectConfig["restricted.networks.access"] ||
		!current.Exists || !wanted.Exists || current.Name != wanted.Name || current.ETag != wanted.ETag ||
		!maps.Equal(current.Config, wanted.Config) || !slices.Equal(current.Egress, wanted.Egress) {
		return errors.New("resource ingress rollback would change more than the target ACL")
	}
	allowance := Rule{Action: "allow", State: "enabled", Protocol: "udp", DestinationPort: strconv.Itoa(guestPort)}
	removed := false
	remaining := make([]Rule, 0, len(current.Ingress))
	for _, rule := range current.Ingress {
		if rule == allowance && !removed {
			removed = true
			continue
		}
		remaining = append(remaining, rule)
	}
	if !removed || !slices.Equal(remaining, wanted.Ingress) {
		return errors.New("resource ingress rollback would change unrelated ACL rules")
	}
	return nil
}

func (s Service) PreviewResourceIngress(ctx context.Context, yards []Yard, target Yard, contract resource.ProxyContract, address string, port int, up bool) (IngressPreview, error) {
	if s.UseApprovedIngress {
		return IngressPreview{}, errors.New("boot ingress verification is read-only")
	}
	guestPort, valid := contract.GuestUDPPort()
	if !valid || contract.AddressPolicy != resource.ProxyAddressOwnerIPv4UDP || !contract.OwnershipMetadata ||
		contract.Profile == "" || contract.Resource == "" || contract.Device == "" || contract.OwnerInterfaceSetting == "" {
		return IngressPreview{}, errors.New("resource has no valid public UDP ingress contract")
	}
	stored, err := s.Host.ReadPolicy(ctx)
	if err != nil {
		return IngressPreview{}, err
	}
	yards, err = mergeYards(yards, stored.Policy)
	if err != nil {
		return IngressPreview{}, err
	}
	if !slices.Contains(yards, target) || target.Name == "" || target.Name == "default" {
		return IngressPreview{}, errors.New("resource ingress requires one registered named yard")
	}
	snapshot, err := s.inspectNetwork(ctx, yards)
	if err != nil {
		return IngressPreview{}, err
	}
	before, err := buildPlan(stored, snapshot, Change{})
	if err != nil {
		return IngressPreview{}, err
	}
	if err := scopedResourceIngressPlan(before, target); err != nil {
		return IngressPreview{}, err
	}
	index := -1
	for i := range snapshot.Yards {
		if snapshot.Yards[i].Yard == target {
			index = i
			break
		}
	}
	if index < 0 || !snapshot.Yards[index].InstanceFound || snapshot.Yards[index].InstanceInfo.Type != domain.YardVM {
		return IngressPreview{}, errors.New("resource ingress requires an existing VM instance")
	}
	observed := &snapshot.Yards[index]
	if !slices.Contains(observed.IngressContracts, contract) {
		return IngressPreview{}, errors.New("resource ingress contract is not selected for the yard")
	}
	instance := &observed.InstanceInfo
	current := instance.LocalDevices[contract.Device]
	if len(current) == 0 && len(instance.Devices[contract.Device]) != 0 ||
		len(current) != 0 && !maps.Equal(current, instance.Devices[contract.Device]) {
		return IngressPreview{}, errors.New("resource ingress proxy must be an exact local device")
	}
	marker := instance.LocalConfig[contract.OwnershipKey()]
	if len(current) != 0 && marker != contract.OwnershipValue(current) &&
		marker != "v1:pending:"+contract.OwnershipValue(current)[3:] {
		return IngressPreview{}, errors.New("resource ingress proxy has missing or stale ownership metadata")
	}
	if len(current) == 0 && marker != "" && !pendingIngressMarker.MatchString(marker) {
		return IngressPreview{}, errors.New("resource ingress has stale ownership metadata")
	}
	if up || len(current) != 0 || marker != "" {
		if instance.LocalDevices == nil {
			instance.LocalDevices = make(map[string]map[string]string)
		}
		if instance.Devices == nil {
			instance.Devices = make(map[string]map[string]string)
		}
		if instance.LocalConfig == nil {
			instance.LocalConfig = make(map[string]string)
		}
		if instance.Config == nil {
			instance.Config = make(map[string]string)
		}
	}
	if up {
		owner, parseErr := netip.ParseAddr(address)
		if parseErr != nil || !resource.ExplicitOwnerIPv4(owner) || port < 1 || port > 65535 {
			return IngressPreview{}, errors.New("resource ingress requires an explicit owner IPv4 and UDP port")
		}
		guest, parseErr := netip.ParseAddr(instance.Devices["eth0"]["ipv4.address"])
		if parseErr != nil || !guest.Is4() || !guest.IsPrivate() {
			return IngressPreview{}, errors.New("resource ingress requires a pinned private guest IPv4")
		}
		wanted := map[string]string{
			"type": "proxy", "listen": "udp:" + owner.String() + ":" + strconv.Itoa(port),
			"connect": "udp:" + guest.String() + ":" + strconv.Itoa(guestPort), "bind": "host", "nat": "true",
		}
		instance.LocalDevices[contract.Device] = wanted
		instance.Devices[contract.Device] = wanted
		instance.LocalConfig[contract.OwnershipKey()] = contract.OwnershipValue(wanted)
		instance.Config[contract.OwnershipKey()] = contract.OwnershipValue(wanted)
	} else {
		delete(instance.LocalDevices, contract.Device)
		delete(instance.Devices, contract.Device)
		delete(instance.LocalConfig, contract.OwnershipKey())
		delete(instance.Config, contract.OwnershipKey())
	}
	// Incus marks a running VM for a full reset on its next restart after any
	// config/device update. Predict that exact side effect, including the first
	// ingress change after boot, without ignoring unrelated snapshot drift.
	switch strings.ToLower(instance.Status) {
	case "running", "ready", "frozen":
		if !maps.Equal(current, instance.LocalDevices[contract.Device]) || marker != instance.LocalConfig[contract.OwnershipKey()] {
			instance.LocalConfig["volatile.vm.needs_reset"] = "true"
			instance.Config["volatile.vm.needs_reset"] = "true"
		}
	}
	after, err := buildPlan(stored, snapshot, Change{})
	if err != nil {
		return IngressPreview{}, err
	}
	if err := scopedResourceIngressPlan(after, target); err != nil {
		return IngressPreview{}, err
	}
	return IngressPreview{Before: before, After: after, Target: target}, nil
}

func scopedResourceIngressPlan(plan Plan, target Yard) error {
	if plan.Before.Policy.Revision != plan.Before.Policy.AppliedRevision ||
		plan.Before.Policy.Isolation != plan.Before.Policy.AppliedIsolation ||
		len(plan.Before.Policy.PendingStart) != 0 || len(plan.Before.Policy.Removing) != 0 {
		return errors.New("yard network policy has pending changes; reconcile it before changing resource ingress")
	}
	for _, update := range plan.Updates {
		if update.Yard.Yard != target {
			return fmt.Errorf("yard network policy has unrelated drift in %s", update.Yard.Name)
		}
	}
	if plan.Changed && len(plan.Updates) == 0 {
		expected := plan.Before.Policy
		expected.Bindings = slices.Clone(expected.Bindings)
		found := false
		approvalChanged := false
		for index := range expected.Bindings {
			if expected.Bindings[index].Yard != target {
				continue
			}
			for _, binding := range plan.Policy.Bindings {
				if binding.Yard == target {
					approvalChanged = !reflect.DeepEqual(expected.Bindings[index].ApprovedIngress, binding.ApprovedIngress)
					expected.Bindings[index].ApprovedIngress = binding.ApprovedIngress
					found = true
					break
				}
			}
		}
		expected.Revision++
		if !found || !approvalChanged || plan.Physical ||
			!reflect.DeepEqual(plan.Policy, expected) {
			return errors.New("yard network policy has unrelated metadata drift")
		}
	}
	return nil
}

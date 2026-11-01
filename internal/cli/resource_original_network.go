package cli

import (
	"context"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

// resourceOriginalNetworkHost keeps rollback cleanup tied to its retained VM.
// Checks fence managed actors under the original owner OFD. Incus has no atomic
// UUID precondition on state PUT or across instance/ACL/profile/project objects;
// unmanaged replacement between a check and a write cannot be fenced here.
type resourceOriginalNetworkHost struct {
	yardnetwork.Host
	target yardnetwork.Yard
	uuid   string
}

func (h resourceOriginalNetworkHost) validate(snapshot yardnetwork.Snapshot) error {
	for _, observed := range snapshot.Yards {
		if observed.Yard == h.target {
			if !observed.InstanceFound || observed.InstanceInfo.Config["volatile.uuid"] != h.uuid {
				return domain.ErrPlanStale
			}
			return nil
		}
	}
	return domain.ErrPlanStale
}

func (h resourceOriginalNetworkHost) check(ctx context.Context) error {
	snapshot, err := h.Host.InspectNetwork(ctx, []yardnetwork.Yard{h.target})
	if err != nil {
		return err
	}
	return h.validate(snapshot)
}

func (h resourceOriginalNetworkHost) InspectNetwork(ctx context.Context, yards []yardnetwork.Yard) (yardnetwork.Snapshot, error) {
	snapshot, err := h.Host.InspectNetwork(ctx, yards)
	if err != nil {
		return snapshot, err
	}
	// Some service observations cover another yard; still check the original VM.
	for _, yard := range yards {
		if yard == h.target {
			return snapshot, h.validate(snapshot)
		}
	}
	return snapshot, h.check(ctx)
}

func (h resourceOriginalNetworkHost) WritePolicy(ctx context.Context, before yardnetwork.StoredPolicy, desired yardnetwork.Policy) (yardnetwork.StoredPolicy, error) {
	if err := h.check(ctx); err != nil {
		return yardnetwork.StoredPolicy{}, err
	}
	return h.Host.WritePolicy(ctx, before, desired)
}
func (h resourceOriginalNetworkHost) WriteNetworkACL(ctx context.Context, acl yardnetwork.ACL) error {
	if err := h.check(ctx); err != nil {
		return err
	}
	return h.Host.WriteNetworkACL(ctx, acl)
}
func (h resourceOriginalNetworkHost) DeleteNetworkACL(ctx context.Context, acl yardnetwork.ACL) error {
	if err := h.check(ctx); err != nil {
		return err
	}
	return h.Host.DeleteNetworkACL(ctx, acl)
}
func (h resourceOriginalNetworkHost) WriteProfileNIC(ctx context.Context, observed yardnetwork.ObservedYard, nic map[string]string) error {
	if err := h.check(ctx); err != nil {
		return err
	}
	return h.Host.WriteProfileNIC(ctx, observed, nic)
}
func (h resourceOriginalNetworkHost) WriteProjectNetworks(ctx context.Context, observed yardnetwork.ObservedYard, access string) error {
	if err := h.check(ctx); err != nil {
		return err
	}
	return h.Host.WriteProjectNetworks(ctx, observed, access)
}
func (h resourceOriginalNetworkHost) NetworkPower(ctx context.Context, yard yardnetwork.Yard, action string) error {
	if err := h.check(ctx); err != nil {
		return err
	}
	var err error
	if guarded, ok := h.Host.(interface {
		NetworkPowerIf(context.Context, yardnetwork.Yard, string, string) error
	}); ok && yard == h.target {
		err = guarded.NetworkPowerIf(ctx, yard, action, h.uuid)
	} else {
		err = h.Host.NetworkPower(ctx, yard, action)
	}
	if err != nil {
		return err
	}
	return h.check(ctx)
}

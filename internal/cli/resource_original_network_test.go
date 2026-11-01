package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

type replacingResourceNetworkHost struct {
	yardnetwork.Host
	target                   yardnetwork.Yard
	reads, replaceAt, writes int
}

func (h *replacingResourceNetworkHost) InspectNetwork(context.Context, []yardnetwork.Yard) (yardnetwork.Snapshot, error) {
	h.reads++
	uuid := "original"
	if h.reads >= h.replaceAt {
		uuid = "replacement"
	}
	return yardnetwork.Snapshot{Yards: []yardnetwork.ObservedYard{{Yard: h.target, InstanceFound: true, InstanceInfo: ports.InstanceInfo{Type: domain.YardVM, Config: map[string]string{"volatile.uuid": uuid}}}}}, nil
}
func (h *replacingResourceNetworkHost) WritePolicy(context.Context, yardnetwork.StoredPolicy, yardnetwork.Policy) (yardnetwork.StoredPolicy, error) {
	h.writes++
	return yardnetwork.StoredPolicy{}, nil
}
func (h *replacingResourceNetworkHost) WriteNetworkACL(context.Context, yardnetwork.ACL) error {
	h.writes++
	return nil
}
func (h *replacingResourceNetworkHost) DeleteNetworkACL(context.Context, yardnetwork.ACL) error {
	h.writes++
	return nil
}
func (h *replacingResourceNetworkHost) WriteProfileNIC(context.Context, yardnetwork.ObservedYard, map[string]string) error {
	h.writes++
	return nil
}
func (h *replacingResourceNetworkHost) WriteProjectNetworks(context.Context, yardnetwork.ObservedYard, string) error {
	h.writes++
	return nil
}
func (h *replacingResourceNetworkHost) NetworkPower(context.Context, yardnetwork.Yard, string) error {
	h.writes++
	return nil
}

func TestResourceRollbackNetworkRejectsOriginalUUIDReplacementAtEveryBoundary(t *testing.T) {
	for _, boundary := range []string{"observation", "policy", "acl", "acl-delete", "nic", "project", "power"} {
		t.Run(boundary, func(t *testing.T) {
			target := yardnetwork.Yard{Name: "fixture", Project: "fixture", Instance: "fixture", Network: "incusbr0"}
			base := &replacingResourceNetworkHost{target: target, replaceAt: 2}
			h := resourceOriginalNetworkHost{Host: base, target: target, uuid: "original"}
			if _, err := h.InspectNetwork(context.Background(), []yardnetwork.Yard{target}); err != nil {
				t.Fatal(err)
			}
			var err error
			ctx := context.Background()
			switch boundary {
			case "observation":
				_, err = h.InspectNetwork(ctx, []yardnetwork.Yard{target})
			case "policy":
				_, err = h.WritePolicy(ctx, yardnetwork.StoredPolicy{}, yardnetwork.Policy{})
			case "acl":
				err = h.WriteNetworkACL(ctx, yardnetwork.ACL{})
			case "acl-delete":
				err = h.DeleteNetworkACL(ctx, yardnetwork.ACL{})
			case "nic":
				err = h.WriteProfileNIC(ctx, yardnetwork.ObservedYard{Yard: target}, nil)
			case "project":
				err = h.WriteProjectNetworks(ctx, yardnetwork.ObservedYard{Yard: target}, "")
			case "power":
				err = h.NetworkPower(ctx, target, "stop")
			}
			if !errors.Is(err, domain.ErrPlanStale) || base.writes != 0 {
				t.Fatalf("replacement: writes=%d err=%v", base.writes, err)
			}
		})
	}
}

func (h *replacingResourceNetworkHost) ReadPolicy(context.Context) (yardnetwork.StoredPolicy, error) {
	return yardnetwork.StoredPolicy{Policy: yardnetwork.Policy{Schema: yardnetwork.SchemaVersion, Revision: 1}}, nil
}
func TestResourceRollbackNetworkRejectsReplacementAfterApplyRefresh(t *testing.T) {
	target := yardnetwork.Yard{Name: "fixture", Project: "fixture", Instance: "fixture", Network: "incusbr0"}
	base := &replacingResourceNetworkHost{target: target, replaceAt: 3}
	service := yardnetwork.Service{Host: resourceOriginalNetworkHost{Host: base, target: target, uuid: "original"}, Lock: testNetworkPolicyLock{}}
	approved, err := service.Prepare(context.Background(), []yardnetwork.Yard{target}, yardnetwork.Change{})
	if err != nil {
		t.Fatal(err)
	}
	if !approved.Changed {
		t.Fatal("fixture must enter Apply")
	}
	err = service.Apply(context.Background(), approved)
	if !errors.Is(err, domain.ErrPlanStale) || base.writes != 0 || base.reads != 3 {
		t.Fatalf("post-refresh replacement: reads=%d writes=%d err=%v", base.reads, base.writes, err)
	}
}

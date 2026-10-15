package yardnetwork

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/previewroute"
	"github.com/Subyard/Subyard/internal/resource"
)

type Service struct {
	Host  Host
	Lock  Locker
	Guard func(context.Context) error
	// ContractSource resolves selected, registry-validated resource contracts for
	// each yard. An error aborts planning before any network mutation.
	ContractSource func(Yard) ([]resource.ProxyContract, error)
	// UseApprovedIngress verifies the exact previously applied ingress projection
	// without reading yard settings. Boot restore and post-activation recovery use it.
	UseApprovedIngress bool
	// ClearStaleUDP removes only stale connection tracking for a verified,
	// owned public UDP endpoint after boot or resource activation.
	ClearStaleUDP func(context.Context, netip.AddrPort) error
}

type Status struct {
	Policy
	Converged  bool   `json:"converged"`
	Diagnostic string `json:"diagnostic,omitempty"`
}

// Status retains the saved policy when the host cannot be reconciled or inspected.
func (s Service) Status(ctx context.Context, yards []Yard) (Status, error) {
	plan, err := s.Prepare(ctx, yards, Change{})
	if err == nil {
		return Status{Policy: plan.Before.Policy, Converged: !plan.Changed}, nil
	}
	stored, readErr := s.Host.ReadPolicy(ctx)
	if readErr != nil {
		return Status{}, readErr
	}
	return Status{Policy: stored.Policy, Diagnostic: err.Error()}, nil
}

func (s Service) Prepare(ctx context.Context, yards []Yard, change Change) (Plan, error) {
	stored, err := s.Host.ReadPolicy(ctx)
	if err != nil {
		return Plan{}, err
	}
	yards, err = mergeYards(yards, stored.Policy)
	if err != nil {
		return Plan{}, err
	}
	snapshot, err := s.inspectNetwork(ctx, yards)
	if err != nil {
		return Plan{}, err
	}
	if s.UseApprovedIngress {
		if s.ContractSource != nil {
			return Plan{}, errors.New("boot ingress verification cannot use a resource contract source")
		}
		if err := applyApprovedBootIngress(&snapshot, stored.Policy); err != nil {
			return Plan{}, err
		}
	}
	return buildPlan(stored, snapshot, change)
}

func applyApprovedBootIngress(snapshot *Snapshot, policy Policy) error {
	bindings := make(map[Yard]Binding, len(policy.Bindings))
	for _, binding := range policy.Bindings {
		bindings[binding.Yard] = binding
	}
	for index := range snapshot.Yards {
		yard := &snapshot.Yards[index]
		binding, exists := bindings[yard.Yard]
		if !exists {
			continue
		}
		approvedDevices := make(map[string]bool, len(binding.ApprovedIngress))
		for _, approved := range binding.ApprovedIngress {
			expected := approved.proxy(binding)
			if approved.Device == previewroute.DeviceName {
				if approved.Transport() != "tcp" || approved.GuestPort != 8765 || !ownedPreviewIngress(*yard, binding) ||
					!maps.Equal(yard.InstanceInfo.Devices[approved.Device], expected) {
					return fmt.Errorf("%w: approved static preview ingress for yard %s changed", ErrNotConverged, yard.Name)
				}
				approvedDevices[approved.Device] = true
				continue
			}
			if !yard.InstanceFound || yard.InstanceInfo.Type != domain.YardVM ||
				!maps.Equal(yard.InstanceInfo.LocalDevices[approved.Device], expected) ||
				!maps.Equal(yard.InstanceInfo.Devices[approved.Device], expected) ||
				yard.InstanceInfo.LocalConfig["user.subyard.resource."+approved.Device] != (resource.ProxyContract{}).OwnershipValue(expected) {
				return fmt.Errorf("%w: approved public ingress for yard %s changed", ErrNotConverged, yard.Name)
			}
			approvedDevices[approved.Device] = true
			yard.IngressContracts = append(yard.IngressContracts, resource.ProxyContract{
				Profile: approved.Device, Resource: approved.Device, Device: approved.Device,
				Connect:       fmt.Sprintf("%s:guest:%d", approved.Transport(), approved.GuestPort),
				AddressPolicy: resource.ProxyAddressPolicy("owner-ipv4-" + approved.Transport()), OwnershipMetadata: true,
			})
		}
		if len(yard.InstanceInfo.Devices[previewroute.DeviceName]) != 0 &&
			yard.InstanceInfo.Devices[previewroute.DeviceName]["nat"] == "true" && !approvedDevices[previewroute.DeviceName] {
			return fmt.Errorf("%w: unapproved static preview ingress in yard %s", ErrNotConverged, yard.Name)
		}
		if len(approvedDevices) != 0 {
			for name, device := range yard.InstanceInfo.Devices {
				if device["type"] == "proxy" && device["bind"] != "instance" &&
					(strings.HasPrefix(device["listen"], "udp:") || strings.HasPrefix(device["connect"], "udp:") ||
						(device["nat"] == "true" || strings.HasPrefix(device["connect"], "tcp:"+binding.IPv4+":")) &&
							(strings.HasPrefix(device["listen"], "tcp:") || strings.HasPrefix(device["connect"], "tcp:"))) &&
					!approvedDevices[name] {
					return fmt.Errorf("%w: unapproved host ingress proxy in yard %s", ErrNotConverged, yard.Name)
				}
			}
		}
	}
	return nil
}

func (s Service) inspectNetwork(ctx context.Context, yards []Yard) (Snapshot, error) {
	snapshot, err := s.Host.InspectNetwork(ctx, yards)
	if err != nil || s.ContractSource == nil {
		return snapshot, err
	}
	for i := range snapshot.Yards {
		contracts, resolveErr := s.ContractSource(snapshot.Yards[i].Yard)
		if resolveErr != nil {
			return Snapshot{}, fmt.Errorf("resolve resource contracts for yard %s: %w", snapshot.Yards[i].Name, resolveErr)
		}
		snapshot.Yards[i].IngressContracts = contracts
	}
	return snapshot, nil
}

func mergeYards(yards []Yard, p Policy) ([]Yard, error) {
	byName := map[string]Yard{}
	for _, y := range yards {
		if !domain.SafeName(y.Name) || !domain.SafeName(y.Project) || !domain.SafeName(y.Instance) || !domain.SafeName(y.Network) {
			return nil, errors.New("invalid local yard network identity")
		}
		if old, ok := byName[y.Name]; ok && old != y {
			return nil, errors.New("ambiguous local yard name")
		}
		byName[y.Name] = y
	}
	for _, b := range p.Bindings {
		if old, ok := byName[b.Yard.Name]; ok && old != b.Yard {
			return nil, fmt.Errorf("yard %s registration differs from its network binding", b.Yard.Name)
		}
		byName[b.Yard.Name] = b.Yard
	}
	result := make([]Yard, 0, len(byName))
	for _, y := range byName {
		result = append(result, y)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func (s Service) Apply(ctx context.Context, approved Plan) error {
	if s.UseApprovedIngress {
		return errors.New("boot ingress verification is read-only")
	}
	if s.Lock == nil {
		return errors.New("host network policy lock is required")
	}
	release, err := s.Lock.Acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	current, err := s.Prepare(ctx, approved.Yards, approved.Change)
	if err != nil {
		return err
	}
	if err := CheckApproved(approved, current); err != nil {
		return err
	}
	if !current.Changed {
		return nil
	}
	return s.applyLocked(ctx, current)
}

func (s Service) applyLocked(ctx context.Context, plan Plan) error {
	p := plan.Policy
	if plan.Physical {
		liveChange := false
		for _, u := range plan.Updates {
			liveChange = liveChange || u.Yard.InstanceFound
		}
		if liveChange {
			if s.Guard == nil {
				return errors.New("host network safety guard is required")
			}
			if err := s.Guard(ctx); err != nil {
				return err
			}
		}
		for _, u := range plan.Updates {
			if u.Yard.InstanceFound && strings.EqualFold(u.Yard.InstanceInfo.Status, "running") && !slices.Contains(p.PendingStart, u.Yard.Name) {
				p.PendingStart = append(p.PendingStart, u.Yard.Name)
			}
		}
		sort.Strings(p.PendingStart)
	}
	stored, err := s.Host.WritePolicy(ctx, plan.Before, p)
	if err != nil {
		return err
	}
	// Persist intent first. A failed apply remains visible and prevents managed starts.
	if plan.Physical {
		for _, u := range plan.Updates {
			if u.Yard.InstanceFound && !strings.EqualFold(u.Yard.InstanceInfo.Status, "stopped") {
				if err = s.Host.NetworkPower(ctx, u.Yard.Yard, "stop"); err != nil {
					return fmt.Errorf("stop %s before changing network policy: %w; run yard network reconcile", u.Yard.Name, err)
				}
				observed, err := s.observe(ctx, u.Yard.Yard)
				if err != nil {
					return err
				}
				if !strings.EqualFold(observed.InstanceInfo.Status, "stopped") {
					return errors.New("network stop postcondition failed")
				}
			}
		}
		for _, u := range plan.Updates {
			observed, err := s.observe(ctx, u.Yard.Yard)
			if err != nil {
				return err
			}
			// Incus 6.0.6 cannot update ACLs referenced by restricted-project NICs.
			// Detach only after every affected instance has stopped.
			if observed.ProfileDevices["eth0"]["security.acls"] != "" {
				nic := maps.Clone(observed.ProfileDevices["eth0"])
				delete(nic, "security.acls")
				if err = s.Host.WriteProfileNIC(ctx, observed, nic); err != nil {
					return err
				}
			}
		}
		for _, u := range plan.Updates {
			observed, err := s.observe(ctx, u.Yard.Yard)
			if err != nil {
				return err
			}
			if !u.RemoveACL {
				acl := u.ACL
				acl.Exists = observed.ACL.Exists
				acl.ETag = observed.ACL.ETag
				if err = s.Host.WriteNetworkACL(ctx, acl); err != nil {
					return err
				}
			}
			if observed.ProjectFound && observed.ProjectConfig["restricted.networks.access"] != u.Access {
				if err = s.Host.WriteProjectNetworks(ctx, observed, u.Access); err != nil {
					return err
				}
			}
			if observed.ProfileFound && !maps.Equal(observed.ProfileDevices["eth0"], u.NIC) {
				if err = s.Host.WriteProfileNIC(ctx, observed, u.NIC); err != nil {
					return err
				}
			}
			if u.RemoveACL && observed.ACL.Exists {
				if err = s.Host.DeleteNetworkACL(ctx, observed.ACL); err != nil {
					return err
				}
			}
		}
	}
	verification, err := s.Prepare(ctx, plan.Yards, Change{})
	if err != nil {
		return err
	}
	if verification.Physical {
		return fmt.Errorf("%w: applied network settings differ from the plan", ErrNotConverged)
	}
	p.AppliedRevision = p.Revision
	p.AppliedIsolation = p.Isolation
	for _, name := range p.Removing {
		p.Forget(name)
	}
	p.Removing = nil
	if !p.Isolation {
		p.Bindings = nil
	}
	stored, err = s.Host.WritePolicy(ctx, stored, p)
	if err != nil {
		return err
	}
	for len(p.PendingStart) > 0 {
		name := p.PendingStart[0]
		var target Yard
		for _, y := range plan.Yards {
			if y.Name == name {
				target = y
				break
			}
		}
		if target.Name == "" {
			return fmt.Errorf("pending yard %s is not registered", name)
		}
		if s.Guard == nil {
			return errors.New("host network safety guard is required")
		}
		if err = s.Guard(ctx); err != nil {
			return err
		}
		observed, observeErr := s.observe(ctx, target)
		if observeErr != nil {
			return observeErr
		}
		if !strings.EqualFold(observed.InstanceInfo.Status, "running") {
			err = s.Host.NetworkPower(ctx, target, "start")
		} else {
			err = nil
		}
		if err != nil {
			return fmt.Errorf("restart %s: %w; run yard network reconcile", name, err)
		}
		verified, verifyErr := s.observe(ctx, target)
		if verifyErr != nil {
			return verifyErr
		}
		if !strings.EqualFold(verified.InstanceInfo.Status, "running") {
			return errors.New("network restart postcondition failed")
		}
		if err = s.Guard(ctx); err != nil {
			stopContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
			_ = s.Host.NetworkPower(stopContext, target, "stop")
			cancel()
			return err
		}
		p.PendingStart = p.PendingStart[1:]
		stored, err = s.Host.WritePolicy(ctx, stored, p)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s Service) observe(ctx context.Context, y Yard) (ObservedYard, error) {
	snapshot, err := s.Host.InspectNetwork(ctx, []Yard{y})
	if err != nil {
		return ObservedYard{}, err
	}
	for _, observed := range snapshot.Yards {
		if observed.Yard == y {
			if err := validateACLOwner(observed); err != nil {
				return ObservedYard{}, err
			}
			return observed, nil
		}
	}
	return ObservedYard{}, errors.New("network observation omitted target yard")
}

func (s Service) Check(ctx context.Context, y Yard) error {
	stored, err := s.Host.ReadPolicy(ctx)
	if err != nil {
		return err
	}
	if !stored.Policy.Isolation && !stored.Policy.AppliedIsolation && stored.Policy.AppliedRevision == stored.Policy.Revision && len(stored.Policy.PendingStart) == 0 && len(stored.Policy.Removing) == 0 {
		return nil
	}
	plan, err := s.Prepare(ctx, []Yard{y}, Change{})
	if err != nil {
		return err
	}
	if plan.Changed || len(plan.Policy.PendingStart) > 0 {
		return ErrNotConverged
	}
	return nil
}

func (s Service) Ensure(ctx context.Context, y Yard) error {
	stored, err := s.Host.ReadPolicy(ctx)
	if err != nil {
		return err
	}
	if !stored.Policy.Isolation && !stored.Policy.AppliedIsolation && stored.Policy.AppliedRevision == stored.Policy.Revision && len(stored.Policy.PendingStart) == 0 && len(stored.Policy.Removing) == 0 {
		return nil
	}
	plan, err := s.Prepare(ctx, []Yard{y}, Change{})
	if err != nil {
		return err
	}
	return s.Apply(ctx, plan)
}

func (s Service) WithStart(ctx context.Context, y Yard, start func() error) error {
	if s.Lock == nil {
		return errors.New("host network policy lock is required")
	}
	release, err := s.Lock.Acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err = s.Check(ctx, y); err != nil {
		return err
	}
	return start()
}

// Keep cleanup intent pending on failure so reconcile can remove stale allow
// rules before any managed start. The caller holds the host network lock.

type RemovalPlan struct {
	Yard         Yard
	Stored       StoredPolicy
	Cleanup      bool
	Consequences []string
}

func (s Service) PrepareRemoval(ctx context.Context, y Yard) (RemovalPlan, error) {
	stored, err := s.Host.ReadPolicy(ctx)
	if err != nil {
		return RemovalPlan{}, err
	}
	plan := RemovalPlan{Yard: y, Stored: stored}
	bound := false
	for _, b := range stored.Policy.Bindings {
		if b.Yard.Name == y.Name {
			if b.Yard != y {
				return RemovalPlan{}, errors.New("teardown yard differs from network identity")
			}
			bound = true
			break
		}
	}
	plan.Cleanup = bound || len(stored.Policy.Peers(y.Name)) > 0 || slices.Contains(stored.Policy.Removing, y.Name)
	if plan.Cleanup {
		plan.Consequences = []string{"remove this yard's saved network links and owned ACL"}
		if bound && stored.Policy.Isolation {
			names := []string{}
			for _, b := range stored.Policy.Bindings {
				if b.Yard.Name != y.Name {
					names = append(names, b.Yard.Name)
				}
			}
			if len(names) > 0 {
				plan.Consequences = append(plan.Consequences, "reconcile network policy and briefly restart affected running yards: "+strings.Join(names, ", "))
			}
		}
	}
	return plan, nil
}

// WithRemoval serializes deletion and policy cleanup with every policy apply/start.
func (s Service) WithRemoval(ctx context.Context, approved RemovalPlan, remove func() error) error {
	return s.WithRemovals(ctx, []RemovalPlan{approved}, remove, nil)
}

// WithRemovals validates one original policy under the real host lock, removes
// the approved yards, then reconciles their combined removal intent once.
func (s Service) WithRemovals(ctx context.Context, approved []RemovalPlan, remove, after func() error) error {
	if s.Lock == nil {
		return errors.New("host network policy lock is required")
	}
	release, err := s.Lock.Acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	if len(approved) == 0 {
		if err := remove(); err != nil {
			return err
		}
		if after != nil {
			return after()
		}
		return nil
	}
	before := approved[0].Stored
	targets := []string{}
	seen := map[string]bool{}
	for _, plan := range approved {
		if plan.Stored.Content != before.Content || plan.Stored.ETag != before.ETag || seen[plan.Yard.Name] {
			return domain.ErrPlanStale
		}
		seen[plan.Yard.Name] = true
		bound := slices.Contains(before.Policy.Removing, plan.Yard.Name) || len(before.Policy.Peers(plan.Yard.Name)) > 0
		for _, binding := range before.Policy.Bindings {
			if binding.Yard.Name == plan.Yard.Name {
				if binding.Yard != plan.Yard {
					return domain.ErrPlanStale
				}
				bound = true
			}
		}
		if plan.Cleanup != bound {
			return domain.ErrPlanStale
		}
		if plan.Cleanup {
			targets = append(targets, plan.Yard.Name)
		}
	}
	current, err := s.Host.ReadPolicy(ctx)
	if err != nil {
		return err
	}
	pending, applying, complete, err := removalPolicies(before.Policy, targets)
	if err != nil {
		return err
	}
	progress := current.Content != before.Content
	if !progress {
		if current.ETag != before.ETag {
			return domain.ErrPlanStale
		}
	} else if len(targets) == 0 || !removalProgress(current.Policy, pending, applying, complete, before.Policy, targets) {
		return domain.ErrPlanStale
	}
	if len(targets) > 0 {
		yards := []Yard{}
		for _, plan := range approved {
			yards = append(yards, plan.Yard)
		}
		yards, err = mergeYards(yards, current.Policy)
		if err != nil {
			return err
		}
		snapshot, inspectErr := s.inspectNetwork(ctx, yards)
		if inspectErr != nil {
			return inspectErr
		}
		for _, observed := range snapshot.Yards {
			if seen[observed.Name] || slices.ContainsFunc(before.Policy.Bindings, func(binding Binding) bool { return binding.Yard == observed.Yard }) {
				if err = validateACLOwner(observed); err != nil {
					return err
				}
			}
		}
		if err != nil {
			return err
		}
	}
	if err = remove(); err != nil {
		return err
	}
	if len(targets) > 0 {
		yards := []Yard{}
		for _, plan := range approved {
			yards = append(yards, plan.Yard)
		}
		yards, err = mergeYards(yards, current.Policy)
		if err != nil {
			return err
		}
		snapshot, inspectErr := s.inspectNetwork(ctx, yards)
		if inspectErr != nil {
			return inspectErr
		}
		for _, target := range approved {
			found := false
			for _, observed := range snapshot.Yards {
				if observed.Yard == target.Yard {
					found = true
					if err = validateACLOwner(observed); err != nil {
						return err
					}
					if observed.InstanceFound {
						return errors.New("cannot forget a network endpoint before its instance is deleted")
					}
				}
			}
			if !found {
				return errors.New("network observation omitted target yard")
			}
		}
		if !progress {
			current, err = s.Host.WritePolicy(ctx, current, pending)
			if err != nil {
				return err
			}
		}
		plan, planErr := buildPlan(current, snapshot, Change{})
		if planErr != nil {
			return planErr
		}
		// Retrying the same intent does not allocate another desired revision.
		plan.Policy.Revision = applying.Revision
		if !removalProgress(plan.Policy, pending, applying, complete, before.Policy, targets) {
			return domain.ErrPlanStale
		}
		if err = s.applyLocked(ctx, plan); err != nil {
			return err
		}
	}
	if after != nil {
		return after()
	}
	return nil
}

func removalPolicies(before Policy, targets []string) (Policy, Policy, Policy, error) {
	pending := before
	pending.PendingStart = slices.Clone(before.PendingStart)
	for _, name := range pending.Removing {
		if !slices.Contains(targets, name) {
			return Policy{}, Policy{}, Policy{}, domain.ErrPlanStale
		}
	}
	if len(targets) == 0 {
		return pending, pending, pending, nil
	}
	if before.Revision > ^uint64(0)-2 {
		return Policy{}, Policy{}, Policy{}, errors.New("network policy revision exhausted")
	}
	pending.Removing = slices.Clone(targets)
	sort.Strings(pending.Removing)
	pending.PendingStart = slices.DeleteFunc(pending.PendingStart, func(name string) bool { return slices.Contains(targets, name) })
	pending.Revision++
	applying := pending
	applying.Links = slices.Clone(pending.Links)
	for _, name := range targets {
		applying.Links = slices.DeleteFunc(applying.Links, func(link Link) bool { return link.A == name || link.B == name })
	}
	applying.Revision++
	complete := applying
	complete.Links = slices.Clone(applying.Links)
	complete.Bindings = slices.Clone(applying.Bindings)
	for _, name := range targets {
		complete.Forget(name)
	}
	complete.Removing, complete.PendingStart = nil, nil
	if !complete.Isolation {
		complete.Bindings = nil
	}
	complete.AppliedRevision, complete.AppliedIsolation = complete.Revision, complete.Isolation
	return pending, applying, complete, nil
}

func removalProgress(actual, pending, applying, complete, original Policy, targets []string) bool {
	// Existing applyLocked may persist restart acknowledgements for surviving
	// peers whose owned ACL lost an approved link. No unrelated name is admitted.
	allowed := slices.Clone(original.PendingStart)
	for _, link := range original.Links {
		if slices.Contains(targets, link.A) && !slices.Contains(targets, link.B) {
			allowed = append(allowed, link.B)
		}
		if slices.Contains(targets, link.B) && !slices.Contains(targets, link.A) {
			allowed = append(allowed, link.A)
		}
	}
	for _, name := range actual.PendingStart {
		if !slices.Contains(allowed, name) || slices.Contains(targets, name) {
			return false
		}
	}
	actual.PendingStart = nil
	body, err := Encode(actual)
	if err != nil {
		return false
	}
	for _, expected := range []Policy{pending, applying, complete} {
		expected.PendingStart = nil
		encoded, err := Encode(expected)
		if err == nil && string(body) == string(encoded) {
			return true
		}
	}
	return false
}

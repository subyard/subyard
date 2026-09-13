package yardnetwork

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
)

type Service struct {
	Host  Host
	Lock  Locker
	Guard func(context.Context) error
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
	snapshot, err := s.Host.InspectNetwork(ctx, yards)
	if err != nil {
		return Plan{}, err
	}
	return buildPlan(stored, snapshot, change)
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
	if approved.Fingerprint != current.Fingerprint {
		return domain.ErrPlanStale
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
func (s Service) forgetLocked(ctx context.Context, y Yard) error {
	stored, err := s.Host.ReadPolicy(ctx)
	if err != nil {
		return err
	}
	if stored.Content == "" {
		return nil
	}
	observed, err := s.observe(ctx, y)
	if err != nil {
		return err
	}
	if observed.InstanceFound {
		return errors.New("cannot forget a network endpoint before its instance is deleted")
	}
	p := stored.Policy
	if !slices.Contains(p.Removing, y.Name) {
		p.Removing = append(p.Removing, y.Name)
	}
	p.PendingStart = slices.DeleteFunc(slices.Clone(p.PendingStart), func(name string) bool { return name == y.Name })
	p.Revision++
	stored, err = s.Host.WritePolicy(ctx, stored, p)
	if err != nil {
		return err
	}
	yards, err := mergeYards([]Yard{y}, p)
	if err != nil {
		return err
	}
	snapshot, err := s.Host.InspectNetwork(ctx, yards)
	if err != nil {
		return err
	}
	plan, err := buildPlan(stored, snapshot, Change{})
	if err != nil {
		return err
	}
	return s.applyLocked(ctx, plan)
}

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
	if s.Lock == nil {
		return errors.New("host network policy lock is required")
	}
	release, err := s.Lock.Acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	fresh, err := s.PrepareRemoval(ctx, approved.Yard)
	if err != nil {
		return err
	}
	if fresh.Stored.Content != approved.Stored.Content || fresh.Stored.ETag != approved.Stored.ETag {
		return domain.ErrPlanStale
	}
	if err = remove(); err != nil {
		return err
	}
	if !fresh.Cleanup {
		return nil
	}
	return s.forgetLocked(ctx, approved.Yard)
}

package yardnetwork

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
)

type memoryHost struct {
	stored               StoredPolicy
	snapshot             Snapshot
	events               []string
	failACL              bool
	failPolicyAfterStart bool
	version              int
}

func newMemoryHost() *memoryHost {
	p, _ := Decode(nil)
	s := networkFixture()
	for i := range s.Yards {
		s.Yards[i].InstanceFound = true
		s.Yards[i].InstanceInfo.Status = "Running"
	}
	return &memoryHost{stored: StoredPolicy{Policy: p, ETag: "0"}, snapshot: s}
}

func TestStatusReportsStoredIntentAndConvergenceWithoutWriting(t *testing.T) {
	for _, scenario := range []string{"converged", "pending", "unsupported-host"} {
		t.Run(scenario, func(t *testing.T) {
			h := newMemoryHost()
			if scenario != "converged" {
				h.stored.Policy.Revision = 3
			}
			if scenario == "unsupported-host" {
				h.stored.Policy.Isolation = true
				h.snapshot.Firewall = "xtables"
			}
			status, err := memoryService(h).Status(context.Background(), fixtureYards(h))
			if err != nil {
				t.Fatal(err)
			}
			if status.Revision != h.stored.Policy.Revision || status.Isolation != h.stored.Policy.Isolation || status.Converged != (scenario == "converged") {
				t.Fatalf("status lost stored intent or convergence: %+v", status)
			}
			if scenario == "unsupported-host" {
				if !strings.Contains(status.Diagnostic, "standalone Incus host using nftables") {
					t.Fatalf("missing host diagnostic: %+v", status)
				}
			} else if status.Diagnostic != "" {
				t.Fatalf("unexpected diagnostic: %s", status.Diagnostic)
			}
			if len(h.events) != 0 {
				t.Fatalf("status mutated host: %v", h.events)
			}
		})
	}
}

type unreadablePolicyHost struct{ Host }

func (unreadablePolicyHost) ReadPolicy(context.Context) (StoredPolicy, error) {
	return StoredPolicy{}, errors.New("policy unavailable")
}

func TestStatusFailsWhenStoredPolicyCannotBeRead(t *testing.T) {
	s := Service{Host: unreadablePolicyHost{}}
	if _, err := s.Status(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "policy unavailable") {
		t.Fatalf("unreadable policy status: %v", err)
	}
}

func cloneValue[T any](v T) T {
	content, _ := json.Marshal(v)
	var result T
	_ = json.Unmarshal(content, &result)
	return result
}
func (h *memoryHost) ReadPolicy(context.Context) (StoredPolicy, error) {
	return cloneValue(h.stored), nil
}
func (h *memoryHost) WritePolicy(_ context.Context, old StoredPolicy, p Policy) (StoredPolicy, error) {
	if old.ETag != h.stored.ETag {
		return StoredPolicy{}, domain.ErrPlanStale
	}
	if h.failPolicyAfterStart && len(h.events) > 0 && strings.HasPrefix(h.events[len(h.events)-1], "start:") {
		h.failPolicyAfterStart = false
		return StoredPolicy{}, errors.New("injected restart acknowledgement failure")
	}
	h.version++
	content, err := Encode(p)
	if err != nil {
		return StoredPolicy{}, err
	}
	h.stored = StoredPolicy{Policy: cloneValue(p), Content: string(content), ETag: fmt.Sprint(h.version)}
	h.events = append(h.events, "policy")
	return h.stored, nil
}
func (h *memoryHost) InspectNetwork(context.Context, []Yard) (Snapshot, error) {
	return cloneValue(h.snapshot), nil
}
func (h *memoryHost) index(y Yard) int {
	for i, candidate := range h.snapshot.Yards {
		if candidate.Yard == y {
			return i
		}
	}
	panic("unknown yard")
}
func (h *memoryHost) WriteNetworkACL(_ context.Context, a ACL) error {
	if h.failACL {
		h.failACL = false
		return errors.New("injected ACL write failure")
	}
	for _, y := range h.snapshot.Yards {
		if y.ProfileDevices["eth0"]["security.acls"] == a.Name || (ACLName(y.Yard) == a.Name && y.InstanceInfo.Status != "Stopped") {
			return errors.New("ACL changed with running or attached NIC")
		}
	}
	for i := range h.snapshot.Yards {
		if ACLName(h.snapshot.Yards[i].Yard) == a.Name {
			a.Exists = true
			h.snapshot.Yards[i].ACL = cloneValue(a)
			h.events = append(h.events, "acl")
			return nil
		}
	}
	return errors.New("unknown ACL")
}
func (h *memoryHost) DeleteNetworkACL(_ context.Context, a ACL) error {
	for i := range h.snapshot.Yards {
		if h.snapshot.Yards[i].ACL.Name == a.Name {
			if h.snapshot.Yards[i].ProfileDevices["eth0"]["security.acls"] != "" {
				return errors.New("ACL still referenced")
			}
			h.snapshot.Yards[i].ACL = ACL{Name: a.Name}
			return nil
		}
	}
	return errors.New("unknown ACL")
}
func (h *memoryHost) WriteProfileNIC(_ context.Context, y ObservedYard, nic map[string]string) error {
	i := h.index(y.Yard)
	if h.snapshot.Yards[i].InstanceInfo.Status != "Stopped" {
		return errors.New("live NIC update")
	}
	h.snapshot.Yards[i].ProfileDevices["eth0"] = maps.Clone(nic)
	h.events = append(h.events, "nic")
	return nil
}
func (h *memoryHost) WriteProjectNetworks(_ context.Context, y ObservedYard, access string) error {
	h.snapshot.Yards[h.index(y.Yard)].ProjectConfig["restricted.networks.access"] = access
	return nil
}
func (h *memoryHost) NetworkPower(_ context.Context, y Yard, action string) error {
	i := h.index(y)
	if action == "start" {
		if h.stored.Policy.AppliedRevision != h.stored.Policy.Revision {
			return errors.New("started before verification")
		}
		h.snapshot.Yards[i].InstanceInfo.Status = "Running"
	} else {
		h.snapshot.Yards[i].InstanceInfo.Status = "Stopped"
	}
	h.events = append(h.events, action+":"+y.Name)
	return nil
}

type memoryLock struct{ held bool }

func (l *memoryLock) Acquire(context.Context) (func(), error) {
	if l.held {
		return nil, errors.New("recursive lock")
	}
	l.held = true
	return func() { l.held = false }, nil
}
func memoryService(h *memoryHost) Service {
	return Service{Host: h, Lock: &memoryLock{}, Guard: func(context.Context) error { return nil }}
}

type rollbackDeadlineHost struct {
	*memoryHost
	started         bool
	stopHadDeadline bool
}

func (h *rollbackDeadlineHost) NetworkPower(ctx context.Context, y Yard, action string) error {
	if action == "stop" && h.started {
		_, h.stopHadDeadline = ctx.Deadline()
	}
	if err := h.memoryHost.NetworkPower(ctx, y, action); err != nil {
		return err
	}
	if action == "start" {
		h.started = true
	}
	return nil
}

func fixtureYards(h *memoryHost) []Yard {
	var result []Yard
	for _, y := range h.snapshot.Yards {
		result = append(result, y.Yard)
	}
	return result
}

func TestApplyStopsDetachesVerifiesAndRestarts(t *testing.T) {
	h := newMemoryHost()
	s := memoryService(h)
	on := true
	plan, err := s.Prepare(context.Background(), fixtureYards(h), Change{Isolation: &on, Link: &Link{A: "a", B: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if !h.stored.Policy.AppliedIsolation || len(h.stored.Policy.PendingStart) != 0 {
		t.Fatalf("not applied: %+v", h.stored.Policy)
	}
	for _, y := range h.snapshot.Yards {
		if y.InstanceInfo.Status != "Running" {
			t.Fatalf("%s not resumed", y.Name)
		}
	}
	plan, err = s.Prepare(context.Background(), fixtureYards(h), Change{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Changed {
		t.Fatal("second apply is not idempotent")
	}
}

func TestPartialApplyPreventsStartAndReconcileRecovers(t *testing.T) {
	h := newMemoryHost()
	h.failACL = true
	s := memoryService(h)
	on := true
	plan, err := s.Prepare(context.Background(), fixtureYards(h), Change{Isolation: &on})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Apply(context.Background(), plan); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("error=%v", err)
	}
	called := false
	if err = s.WithStart(context.Background(), h.snapshot.Yards[0].Yard, func() error { called = true; return nil }); !errors.Is(err, ErrNotConverged) || called {
		t.Fatalf("incomplete policy admitted start: called=%t err=%v", called, err)
	}
	for _, y := range h.snapshot.Yards {
		if y.InstanceInfo.Status != "Stopped" {
			t.Fatalf("%s running after partial update", y.Name)
		}
	}
	plan, err = s.Prepare(context.Background(), fixtureYards(h), Change{})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	for _, y := range h.snapshot.Yards {
		if y.InstanceInfo.Status != "Running" {
			t.Fatalf("%s lost restart intent", y.Name)
		}
	}
}

func TestReconcileAcknowledgesAlreadyRunningYardAfterPolicyWriteFailure(t *testing.T) {
	h := newMemoryHost()
	h.failPolicyAfterStart = true
	s := memoryService(h)
	on := true
	plan, err := s.Prepare(context.Background(), fixtureYards(h), Change{Isolation: &on})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Apply(context.Background(), plan); err == nil || !strings.Contains(err.Error(), "acknowledgement") {
		t.Fatalf("restart acknowledgement failure = %v", err)
	}
	if h.snapshot.Yards[0].InstanceInfo.Status != "Running" || len(h.stored.Policy.PendingStart) != 3 {
		t.Fatalf("failed acknowledgement state = status %q policy %+v",
			h.snapshot.Yards[0].InstanceInfo.Status, h.stored.Policy)
	}

	plan, err = s.Prepare(context.Background(), fixtureYards(h), Change{})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	starts := map[string]int{}
	for _, event := range h.events {
		if name, found := strings.CutPrefix(event, "start:"); found {
			starts[name]++
		}
	}
	if starts["a"] != 1 || starts["b"] != 1 || starts["c"] != 1 || len(h.stored.Policy.PendingStart) != 0 {
		t.Fatalf("reconcile repeated or lost restart: starts=%v policy=%+v", starts, h.stored.Policy)
	}
}

func TestPostStartGuardFailureRollsBackWithDeadline(t *testing.T) {
	h := &rollbackDeadlineHost{memoryHost: newMemoryHost()}
	s := Service{
		Host: h,
		Lock: &memoryLock{},
		Guard: func(context.Context) error {
			if h.started {
				return errors.New("injected post-start guard failure")
			}
			return nil
		},
	}
	on := true
	plan, err := s.Prepare(context.Background(), fixtureYards(h.memoryHost), Change{Isolation: &on})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Apply(context.Background(), plan); err == nil || !strings.Contains(err.Error(), "post-start") {
		t.Fatalf("post-start guard failure = %v", err)
	}
	if !h.stopHadDeadline || h.snapshot.Yards[0].InstanceInfo.Status != "Stopped" {
		t.Fatalf("rollback was not bounded and completed: deadline=%t status=%q", h.stopHadDeadline, h.snapshot.Yards[0].InstanceInfo.Status)
	}
}

func TestDisableReleasesBindingsAndReenableCapturesCurrentSettings(t *testing.T) {
	h := newMemoryHost()
	s := memoryService(h)
	yards := fixtureYards(h)
	on := true
	plan, err := s.Prepare(context.Background(), yards, Change{Isolation: &on})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	off := false
	plan, err = s.Prepare(context.Background(), yards, Change{Isolation: &off})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if len(h.stored.Policy.Bindings) != 0 {
		t.Fatalf("disabled isolation retained stale bindings: %+v", h.stored.Policy.Bindings)
	}

	h.snapshot.Yards[0].ProfileDevices["eth0"]["mtu"] = "1400"
	h.snapshot.Yards[0].ProjectConfig["restricted.networks.access"] = "incusbr0,operator-net"
	plan, err = s.Prepare(context.Background(), yards, Change{Isolation: &on})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	var captured Binding
	for _, binding := range h.stored.Policy.Bindings {
		if binding.Yard.Name == "a" {
			captured = binding
		}
	}
	if captured.OriginalNIC["mtu"] != "1400" || captured.OriginalAccess != "incusbr0,operator-net" {
		t.Fatalf("re-enable reused stale originals: %+v", captured)
	}
}

func TestWithStartHoldsLockForEmptyPolicy(t *testing.T) {
	h := newMemoryHost()
	lock := &memoryLock{}
	s := Service{Host: h, Lock: lock}
	called := false
	err := s.WithStart(context.Background(), h.snapshot.Yards[0].Yard, func() error {
		called = true
		if !lock.held {
			return errors.New("start ran outside the host network lock")
		}
		return nil
	})
	if err != nil || !called || lock.held {
		t.Fatalf("empty-policy start lock: called=%t held-after=%t err=%v", called, lock.held, err)
	}
}

func TestWithRemovalRequiresLockBeforePhysicalTeardown(t *testing.T) {
	h := newMemoryHost()
	s := Service{Host: h}
	plan, err := s.PrepareRemoval(context.Background(), h.snapshot.Yards[0].Yard)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	err = s.WithRemoval(context.Background(), plan, func() error { called = true; return nil })
	if err == nil || !strings.Contains(err.Error(), "lock is required") || called {
		t.Fatalf("nil-lock removal: called=%t err=%v", called, err)
	}
}

func TestWithRemovalHoldsLockDuringPhysicalTeardown(t *testing.T) {
	h := newMemoryHost()
	lock := &memoryLock{}
	s := Service{Host: h, Lock: lock}
	plan, err := s.PrepareRemoval(context.Background(), h.snapshot.Yards[0].Yard)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	err = s.WithRemoval(context.Background(), plan, func() error {
		called = true
		if !lock.held {
			return errors.New("physical teardown ran outside the host network lock")
		}
		return nil
	})
	if err != nil || !called || lock.held {
		t.Fatalf("removal lock: called=%t held-after=%t err=%v", called, lock.held, err)
	}
}

func TestWithRemovalRejectsStalePolicyBeforePhysicalTeardown(t *testing.T) {
	h := newMemoryHost()
	s := memoryService(h)
	plan, err := s.PrepareRemoval(context.Background(), h.snapshot.Yards[0].Yard)
	if err != nil {
		t.Fatal(err)
	}
	h.stored.ETag = "changed"
	called := false
	err = s.WithRemoval(context.Background(), plan, func() error { called = true; return nil })
	if !errors.Is(err, domain.ErrPlanStale) || called {
		t.Fatalf("stale removal reached physical teardown: called=%t err=%v", called, err)
	}
}

func TestApplyRejectsChangedObservationBeforeAnyWrite(t *testing.T) {
	for _, on := range []bool{false, true} {
		h := newMemoryHost()
		s := memoryService(h)
		plan, err := s.Prepare(context.Background(), fixtureYards(h), Change{Isolation: &on})
		if err != nil {
			t.Fatal(err)
		}
		h.snapshot.Yards[0].ProfileETag = "changed"
		if err = s.Apply(context.Background(), plan); !errors.Is(err, domain.ErrPlanStale) {
			t.Fatalf("isolation=%t changed=%t err=%v", on, plan.Changed, err)
		}
		if len(h.events) != 0 {
			t.Fatalf("mutated before stale rejection: %v", h.events)
		}
	}
}

func TestTeardownRestoresRetainedProfileAndRemovesOnlyEndpointLinks(t *testing.T) {
	h := newMemoryHost()
	s := memoryService(h)
	on := true
	plan, err := s.Prepare(context.Background(), fixtureYards(h), Change{Isolation: &on, Link: &Link{A: "a", B: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	removal, err := s.PrepareRemoval(context.Background(), h.snapshot.Yards[0].Yard)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.WithRemoval(context.Background(), removal, func() error {
		h.snapshot.Yards[0].InstanceFound = false
		h.snapshot.Yards[0].InstanceInfo.Status = "Stopped"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if h.snapshot.Yards[0].ACL.Exists || h.snapshot.Yards[0].ProfileDevices["eth0"]["security.acls"] != "" {
		t.Fatal("retained profile still references deleted endpoint ACL")
	}
	if len(h.stored.Policy.Links) != 0 || len(h.stored.Policy.Bindings) != 2 || len(h.stored.Policy.Removing) != 0 {
		t.Fatalf("teardown not converged: %+v", h.stored.Policy)
	}
}

func TestRemovalDoesNotDeleteForeignACLAfterProjectDisappears(t *testing.T) {
	h := newMemoryHost()
	s := memoryService(h)
	on := true
	plan, err := s.Prepare(context.Background(), fixtureYards(h), Change{Isolation: &on})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	y := &h.snapshot.Yards[0]
	removal, err := s.PrepareRemoval(context.Background(), y.Yard)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.WithRemoval(context.Background(), removal, func() error {
		y.InstanceFound = false
		y.ProjectFound = false
		y.ProfileFound = false
		y.ACL.Config = map[string]string{"user.operator.owner": "foreign"}
		return nil
	}); err == nil {
		t.Fatal("accepted foreign ACL during removed-project cleanup")
	}
	if !y.ACL.Exists || y.ACL.Config["user.operator.owner"] != "foreign" {
		t.Fatal("foreign ACL was changed")
	}
}

func TestLinkChangeDoesNotRestartUnchangedYards(t *testing.T) {
	h := newMemoryHost()
	s := memoryService(h)
	on := true
	plan, err := s.Prepare(context.Background(), fixtureYards(h), Change{Isolation: &on})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	h.events = nil
	plan, err = s.Prepare(context.Background(), fixtureYards(h), Change{Link: &Link{A: "a", B: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Updates) != 2 {
		t.Fatalf("updates=%d, want linked endpoints only", len(plan.Updates))
	}
	if err = s.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	for _, event := range h.events {
		if event == "stop:c" || event == "start:c" {
			t.Fatalf("unrelated yard was interrupted: %v", h.events)
		}
	}
}

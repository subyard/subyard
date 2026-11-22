package testvmsruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/crypto/ssh"
)

const brokerFixtureBase = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type brokerPoolFixture struct {
	t      *testing.T
	ctx    context.Context
	rt     *Runtime
	store  LeaseStore
	incus  *fakeIncus
	now    time.Time
	facade Facade
}

func newBrokerPoolFixture(t *testing.T, count int) *brokerPoolFixture {
	t.Helper()
	f := &brokerPoolFixture{
		t: t, ctx: t.Context(), incus: newFakeIncus(t),
		now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	cfg := fixtureConfig(t)
	cfg.SlotCount = count
	f.rt = &Runtime{
		Config: cfg, Runner: f.incus, Now: func() time.Time { return f.now },
		slotAccountRoot: testkit.TempDir(t),
		lookupUser: func(name string) (*user.User, error) {
			home, exists := f.incus.accounts[name]
			if !exists {
				return nil, user.UnknownUserError(name)
			}
			return &user.User{Username: name, HomeDir: home, Gid: strconv.Itoa(os.Getegid())}, nil
		},
		AvailableBytes: func(string) (uint64, error) { return 100 << 30, nil },
		cacheProbe:     func(context.Context) (CacheUsage, error) { return CacheUsage{}, nil },
		diskUsageProbe: func(context.Context) (uint64, error) { return 0, nil },
	}
	f.rt.prepareDefaults()
	f.store = LeaseStore{Path: cfg.leaseState(), SlotCount: count, Now: f.rt.Now}
	f.facade = Facade{
		Context: f.ctx, Input: strings.NewReader(""), Store: f.store,
		Events: f.rt.Events, EnvironmentSpec: cfg.EnvironmentSpec,
		OnObserve: func(slots []LeaseSlot) []ResourceObservation { return f.rt.Observe(f.ctx, slots) },
		OnStatus:  func(pool LeasePool) ResourceStatus { return f.rt.ResourceStatus(f.ctx, pool) },
		OnAcquire: func(ctx context.Context, grant LeaseGrant, key string) (LeaseGrant, error) {
			index, err := slotNumber(grant.SlotID, f.rt.Config.SlotCount)
			if err != nil {
				return grant, err
			}
			child := f.prepareSlot(index, key)
			f.incus.accounts[child.Config.AgentUser] = child.Config.AgentHome
			// The fixture supplies a validated, already imported base. Exercise
			// the allocation markers without building an image or booting a VM.
			grant.BaseFingerprint = brokerFixtureBase
			err = f.store.mutateOwned(grant, func(slot *LeaseSlot, _ time.Time) error {
				slot.BaseFingerprint = grant.BaseFingerprint
				child.useLeaseSlot(*slot)
				child.Config.Image = slot.BaseFingerprint
				return nil
			})
			if err != nil {
				return grant, err
			}
			for indexVM := 1; indexVM <= child.Config.guestCount(); indexVM++ {
				name := child.Config.vm(indexVM)
				if err := child.initVM(ctx, name); err != nil {
					return grant, err
				}
				vm := f.incus.projects[child.Config.Project].instances[name]
				vm.status = "RUNNING"
				vm.ip = fmt.Sprintf("192.0.2.%d", 10*index+indexVM)
				if err := child.installManagedGuestKeys(ctx, name); err != nil {
					return grant, err
				}
			}
			if err := child.enableAgentAccess(ctx); err != nil {
				return grant, err
			}
			grant.ExpiresAt, err = f.store.MarkHeld(grant)
			return grant, err
		},
		OnRelease: func(ctx context.Context, grant LeaseGrant) error {
			return f.rt.ReleaseSlot(ctx, f.store, grant)
		},
		OnQuarantine: func(ctx context.Context, grant LeaseGrant, cause error) error {
			return f.rt.QuarantineSlot(ctx, f.store, grant, cause)
		},
	}
	return f
}

func (f *brokerPoolFixture) prepareSlot(index int, key string) *Runtime {
	f.t.Helper()
	child := f.rt.slotRuntime(index, key)
	for _, directory := range []string{child.Config.StateDir, child.Config.AgentHome} {
		brokerMust(f.t, os.MkdirAll(directory, 0o700))
		testkit.WriteFile(f.t, filepath.Join(directory, ".subyard-managed"), []byte(managedMarker), 0o600)
	}
	testkit.WriteFile(f.t, child.Config.keyPath()+".pub", []byte(f.rt.Config.AgentPublicKey), 0o600)
	brokerMust(f.t, child.ensureSlotNetwork(f.ctx))
	brokerMust(f.t, child.ensureProject(f.ctx))
	return child
}

func (f *brokerPoolFixture) request(command string, result any) []byte {
	f.t.Helper()
	facade := f.facade
	facade.Store = f.store
	var output bytes.Buffer
	facade.Output = &output
	brokerMust(f.t, facade.Run(command))
	brokerMust(f.t, json.Unmarshal(output.Bytes(), result))
	return output.Bytes()
}

func (f *brokerPoolFixture) acquire(index int, purpose string) LeaseGrant {
	f.t.Helper()
	return f.acquireWithKey(index, purpose, fixturePublicKey(f.t))
}

func (f *brokerPoolFixture) acquireWithKey(index int, purpose, key string) LeaseGrant {
	f.t.Helper()
	result := f.requestAcquire(index, purpose, key)
	if result.Status != "ok" || result.Grant == nil || result.Grant.SlotID != fmt.Sprintf("slot-%03d", index) {
		f.t.Fatalf("acquire slot %d: status=%s code=%s message=%s", index, result.Status, result.Code, result.Message)
	}
	return *result.Grant
}

func (f *brokerPoolFixture) requestAcquire(index int, purpose, key string) facadeResponse {
	f.t.Helper()
	var result facadeResponse
	f.request(fmt.Sprintf("acquire-v3 subyard-pair client-%d SHA256:controller test-yard Broker-Tests %08x %s %s slot-%03d",
		index, index, purpose, key, index), &result)
	return result
}

func (f *brokerPoolFixture) leaseAction(action string, grant LeaseGrant) facadeResponse {
	f.t.Helper()
	var result facadeResponse
	f.request(fmt.Sprintf("%s %s %s %d %s", action, grant.SlotID, grant.LeaseID, grant.LeaseEpoch,
		grant.Capability), &result)
	return result
}

func (f *brokerPoolFixture) slot(index int) LeaseSlot {
	f.t.Helper()
	slot, err := storeSlot(f.store, fmt.Sprintf("slot-%03d", index))
	brokerMust(f.t, err)
	return slot
}

func (f *brokerPoolFixture) keys(index int) []byte {
	f.t.Helper()
	return brokerRead(f.t, f.rt.slotRuntime(index, "").Config.AgentAuthorizedKeys)
}

func (f *brokerPoolFixture) assertSlotAccess(index int, key string, destinations ...string) {
	f.t.Helper()
	publicKey, _, options, rest, err := ssh.ParseAuthorizedKey(f.keys(index))
	brokerMust(f.t, err)
	if len(rest) != 0 || strings.TrimSpace(string(ssh.MarshalAuthorizedKey(publicKey))) != key {
		f.t.Errorf("slot %d did not publish exactly its lease key", index)
	}
	var allowed []string
	for _, option := range options {
		if destination, ok := strings.CutPrefix(option, `permitopen="`); ok {
			allowed = append(allowed, strings.TrimSuffix(destination, `"`))
		}
	}
	if !slices.Equal(allowed, destinations) {
		f.t.Errorf("slot %d permitopen = %v, want %v", index, allowed, destinations)
	}
	cfg := f.rt.slotRuntime(index, "").Config
	for selector := 1; selector <= len(destinations); selector++ {
		name := cfg.vm(selector)
		vm := f.incus.projects[cfg.Project].instances[name]
		if vm == nil || len(vm.agentKeyWrites) != 2 ||
			!slices.Contains(vm.agentKeyWrites, guestKeyWrite{account: "dev", key: key}) ||
			!slices.Contains(vm.agentKeyWrites, guestKeyWrite{account: "root", key: key}) {
			f.t.Errorf("slot %d %s must receive exactly one write of its lease key for dev and one for root", index, name)
		}
	}
}

func (f *brokerPoolFixture) failRelease(victim LeaseGrant) LeaseSlot {
	f.t.Helper()
	f.incus.calls = nil
	f.incus.fault = []string{"project", "list", "--format", "csv", "-c", "n"}
	result := f.leaseAction("release", victim)
	if result.Status != "error" || result.Code != "quarantined" || result.Message != "slot fencing or stop failed" {
		f.t.Fatalf("release: status=%s code=%s message=%s", result.Status, result.Code, result.Message)
	}
	slot := f.slot(1)
	if slot.State != SlotQuarantined || !brokerRecordID.MatchString(slot.IncidentID) ||
		!brokerRecordID.MatchString(slot.LastFailureEventID) || f.incus.fault != nil {
		f.t.Fatal("fault did not leave an attributed quarantined slot")
	}
	return slot
}

func (f *brokerPoolFixture) assertNoCallsToSlot(index int) {
	f.t.Helper()
	cfg := f.rt.slotRuntime(index, "").Config
	for _, call := range f.incus.calls {
		if call[0] == cfg.Incus && (incusCallProject(call[1:]) == cfg.Project || slices.Contains(call, cfg.Network)) {
			f.t.Errorf("unrelated slot received Incus call: %q", call)
		}
	}
}

func (f *brokerPoolFixture) assertEmptyProject(index int) {
	f.t.Helper()
	cfg := f.rt.slotRuntime(index, "").Config
	project := f.incus.projects[cfg.Project]
	if project == nil || project.config["user.subyard.managed"] != managedMarker {
		f.t.Fatalf("slot %d project marker was not preserved", index)
	}
	volumes, err := f.incus.incus([]string{"storage", "volume", "list", "default", "--project", cfg.Project})
	brokerMust(f.t, err)
	if len(project.instances) != 0 || volumes != "" {
		f.t.Errorf("slot %d has %d remaining instances and volumes %q", index, len(project.instances), volumes)
	}
}

func brokerMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func brokerRead(t *testing.T, path string) []byte {
	t.Helper()
	payload, err := os.ReadFile(path)
	brokerMust(t, err)
	return payload
}

func TestBrokerFailedReleaseQuarantinesOnlyItsSlot(t *testing.T) {
	f := newBrokerPoolFixture(t, 2)
	victim := f.acquire(1, "failed-release")
	f.acquire(2, "held-neighbor")
	neighbor, neighborKeys := f.slot(2), f.keys(2)
	if len(f.keys(1)) == 0 || len(neighborKeys) == 0 {
		t.Fatal("fixture did not publish forwarding access")
	}

	quarantined := f.failRelease(victim)
	if len(f.keys(1)) != 0 || !bytes.Equal(neighborKeys, f.keys(2)) || !reflect.DeepEqual(neighbor, f.slot(2)) {
		t.Error("release failed to fence its own key or changed the held neighbor")
	}
	f.assertNoCallsToSlot(2)
	if !slices.ContainsFunc(f.incus.calls, func(call []string) bool {
		return slices.Equal(call, []string{"systemctl", "start", "--no-block", "subyard-test-vms-lease-reaper.service"})
	}) {
		t.Error("quarantine did not wake the recovery service")
	}
	batch, err := f.rt.Events.Export()
	brokerMust(t, err)
	if len(batch.Incidents) != 1 {
		t.Fatalf("got %d incident artifacts", len(batch.Incidents))
	}
	incident := batch.Incidents[0]
	path := filepath.Join(f.rt.Events.incidentDirectory(), quarantined.IncidentID+".json")
	info, err := os.Lstat(path)
	brokerMust(t, err)
	if !info.Mode().IsRegular() || incident.IncidentID != quarantined.IncidentID ||
		incident.SlotID != victim.SlotID || incident.Command == nil ||
		incident.Command.Command != "incus project list --format csv -c n" ||
		incident.Command.ExitCode != 1 || !strings.Contains(incident.Command.Error, fakeIncusUnavailable) ||
		!strings.Contains(incident.FailureReason, fakeIncusUnavailable) || len(incident.Diagnostics) == 0 {
		t.Fatalf("incident does not describe the failed inventory: %+v", incident)
	}
	if incident.Context == nil || incident.Context.SchemaVersion != 2 ||
		incident.Context.Yard != victim.Context.Yard || incident.Context.Project != victim.Context.Project ||
		bytes.Contains(brokerRead(t, path), []byte(`"checkout"`)) {
		t.Errorf("incident context: %+v", incident.Context)
	}
}

func TestBrokerScheduledRecoveryDeletesOnlyTheQuarantinedAllocation(t *testing.T) {
	f := newBrokerPoolFixture(t, 2)
	victim := f.acquire(1, "failed-release")
	f.acquire(2, "held-neighbor")
	neighbor := f.slot(2)
	cfg := f.rt.slotRuntime(2, "").Config
	neighborBefore := fakeJSON(f.incus.projects[cfg.Project].instances[cfg.vm(1)].config) +
		fakeJSON(f.incus.projects[cfg.Project].instances[cfg.vm(2)].config)
	quarantined := f.failRelease(victim)
	f.incus.calls = nil
	f.now = quarantined.NextRecoveryAt.Add(time.Second)
	brokerMust(t, f.rt.ReapExpired(f.ctx, f.store))

	available := f.slot(1)
	if available.State != SlotAvailable || available.ResourceGeneration != victim.ResourceGeneration+1 ||
		available.LeaseEpoch != victim.LeaseEpoch || available.IncidentID != quarantined.IncidentID {
		t.Error("recovery did not preserve incident and epoch while advancing generation")
	}
	f.assertEmptyProject(1)
	f.assertDeletedPair(1, victim)
	f.assertNoCallsToSlot(2)
	project := f.incus.projects[cfg.Project]
	if !reflect.DeepEqual(neighbor, f.slot(2)) || len(project.instances) != 2 ||
		project.instances[cfg.vm(1)] == nil || project.instances[cfg.vm(2)] == nil ||
		project.instances[cfg.vm(1)].status != "RUNNING" || project.instances[cfg.vm(2)].status != "RUNNING" ||
		fakeJSON(project.instances[cfg.vm(1)].config)+fakeJSON(project.instances[cfg.vm(2)].config) != neighborBefore {
		t.Error("recovery changed the held neighbor or its VMs")
	}
	for _, call := range f.incus.calls {
		if call[0] == "incus" && (call[1] == "init" || call[1] == "start") {
			t.Errorf("recovery launched a replacement VM: %q", call)
		}
	}
	batch, err := f.rt.Events.Export()
	brokerMust(t, err)
	var timeline []string
	for _, event := range batch.Events {
		if event.IncidentID == quarantined.IncidentID && event.Kind != "diagnostics.saved" {
			timeline = append(timeline, event.Kind)
		}
		if event.SlotID == victim.SlotID && (event.Kind == "rebuild.create" || event.Kind == "recovery.failed") {
			t.Errorf("unexpected recovery event: %s", event.Kind)
		}
	}
	if want := []string{"slot.quarantined", "recovery.start", "recovery.available"}; !slices.Equal(timeline, want) {
		t.Errorf("incident timeline = %v, want %v", timeline, want)
	}
	if result := f.leaseAction("renew", victim); result.Code != "lease_lost" {
		t.Errorf("stale victim renew = %s, want lease_lost", result.Code)
	}
}

func TestBrokerHeldNeighborRenewsThroughPeerQuarantineAndRecovery(t *testing.T) {
	f := newBrokerPoolFixture(t, 2)
	victim := f.acquire(1, "failed-release")
	neighbor := f.acquire(2, "held-neighbor")
	quarantined := f.failRelease(victim)
	previous := f.slot(2)
	renew := func(victimState SlotState) {
		t.Helper()
		f.now = f.now.Add(time.Second)
		if state := f.slot(1).State; state != victimState {
			t.Fatalf("victim state = %s, want %s at neighbor renew", state, victimState)
		}
		result := f.leaseAction("renew", neighbor)
		current := f.slot(2)
		if result.Status != "ok" || result.ExpiresAt == nil ||
			!result.ExpiresAt.Equal(current.ExpiresAt) || !current.ExpiresAt.After(previous.ExpiresAt) ||
			!current.LastHeartbeatAt.After(previous.LastHeartbeatAt) {
			t.Fatalf("neighbor renewal did not advance during %s", victimState)
		}
		previous = current
	}
	renew(SlotQuarantined)
	f.rt.finishRecovery = func(ctx context.Context, store LeaseStore, slot LeaseSlot) error {
		renew(SlotRecovering)
		f.rt.finishRecovery = nil
		return f.rt.completeRecovery(ctx, store, slot)
	}
	f.now = quarantined.NextRecoveryAt.Add(time.Second)
	brokerMust(t, f.rt.ReapExpired(f.ctx, f.store))
	renew(SlotAvailable)
}

func TestBrokerProvisioningFailureRecoversAndAllowsReacquire(t *testing.T) {
	f := newBrokerPoolFixture(t, 1)
	cfg := f.rt.slotRuntime(1, "").Config
	f.incus.fault = []string{"exec", cfg.vm(1), "--project", cfg.Project, "--", "ip", "-4", "route", "show", "default"}
	result := f.requestAcquire(1, "failed-provisioning", fixturePublicKey(t))
	if result.Status != "error" || result.Code != "quarantined" || result.Message != "slot provisioning failed" ||
		result.Grant != nil || f.incus.fault != nil {
		t.Fatalf("provisioning fault: status=%s code=%s message=%s", result.Status, result.Code, result.Message)
	}
	failed := f.slot(1)
	if failed.State != SlotQuarantined || failed.IncidentID == "" || failed.NextRecoveryAt.IsZero() ||
		len(f.keys(1)) != 0 || len(f.incus.projects[cfg.Project].instances) != 2 {
		t.Fatal("failed provisioning did not fence and schedule recovery of the partial allocation")
	}
	f.now = failed.NextRecoveryAt.Add(time.Second)
	brokerMust(t, f.rt.ReapExpired(f.ctx, f.store))
	f.assertEmptyProject(1)
	if available := f.slot(1); available.State != SlotAvailable || available.ResourceGeneration != failed.ResourceGeneration+1 {
		t.Fatal("reaper did not return a new generation to the available pool")
	}
	freshKey := fixturePublicKey(t)
	fresh := f.acquireWithKey(1, "after-recovery", freshKey)
	if fresh.LeaseEpoch != failed.LeaseEpoch+1 || f.slot(1).State != SlotHeld {
		t.Fatal("reacquire did not hold a new lease after recovery")
	}
	f.assertSlotAccess(1, freshKey, "192.0.2.11:22", "192.0.2.12:22")
}

func TestBrokerReleaseDeletesDisposablePairAndNextGrantIsFresh(t *testing.T) {
	f := newBrokerPoolFixture(t, 2)
	f.acquire(1, "held-neighbor")
	neighbor := f.slot(1)
	old := f.acquire(2, "disposable")
	project := f.incus.projects[f.rt.slotRuntime(2, "").Config.Project]
	uuids := map[string]string{}
	for name, vm := range project.instances {
		uuids[name] = vm.config["volatile.uuid"]
	}
	if busy := f.facadeBusyRequest(2); busy.Code != "busy" || busy.Reason != "busy" || busy.State != SlotHeld {
		t.Fatalf("occupied exact slot: code=%s reason=%s", busy.Code, busy.Reason)
	}
	f.incus.calls = nil
	if result := f.leaseAction("release", old); result.Status != "ok" || result.Message != "released" {
		t.Fatalf("release: status=%s message=%s", result.Status, result.Message)
	}
	f.assertDeletedPair(2, old)
	f.assertEmptyProject(2)
	if state := f.slot(2); state.State != SlotAvailable || state.ResourceGeneration != old.ResourceGeneration {
		t.Error("release did not return the same generation to available")
	}
	if len(f.keys(2)) != 0 {
		t.Error("release kept forwarding access before reacquisition")
	}
	if result := f.leaseAction("renew", old); result.Code != "lease_lost" {
		t.Errorf("stale renew = %s, want lease_lost", result.Code)
	}
	freshKey := fixturePublicKey(t)
	fresh := f.acquireWithKey(2, "fresh", freshKey)
	f.assertSlotAccess(2, freshKey, "192.0.2.21:22", "192.0.2.22:22")
	if fresh.ResourceGeneration != old.ResourceGeneration+1 || fresh.LeaseEpoch != old.LeaseEpoch+1 ||
		fresh.BaseFingerprint == "" || fresh.BaseFingerprint != old.BaseFingerprint || fresh.LeaseID == old.LeaseID ||
		len(project.instances) != len(uuids) {
		t.Error("next grant did not advance allocation identity while retaining its base pin")
	}
	for name, vm := range project.instances {
		if vm.config["volatile.base_image"] != fresh.BaseFingerprint || vm.config["volatile.uuid"] == uuids[name] ||
			vm.config["user.subyard.generation"] != strconv.FormatUint(fresh.ResourceGeneration, 10) ||
			vm.config["user.subyard.lease-epoch"] != strconv.FormatUint(fresh.LeaseEpoch, 10) {
			t.Errorf("fresh %s did not receive the new identity and original base", name)
		}
	}
	if result := f.leaseAction("release", fresh); result.Status != "ok" || result.Message != "released" {
		t.Fatalf("fresh release: status=%s message=%s", result.Status, result.Message)
	}
	f.assertEmptyProject(2)
	if state := f.slot(2); state.State != SlotAvailable || state.ResourceGeneration != fresh.ResourceGeneration {
		t.Error("fresh release did not return the same generation to available")
	}
	if !reflect.DeepEqual(neighbor, f.slot(1)) {
		t.Error("busy request, release or reacquire changed the unselected lease")
	}
	f.assertNoCallsToSlot(1)
}

func (f *brokerPoolFixture) facadeBusyRequest(index int) facadeResponse {
	f.t.Helper()
	var result facadeResponse
	f.request(fmt.Sprintf("acquire-v3 subyard-pair busy-client SHA256:busy test-yard Broker-Tests 00000003 rejected %s slot-%03d",
		f.rt.Config.AgentPublicKey, index), &result)
	return result
}

func (f *brokerPoolFixture) assertDeletedPair(index int, grant LeaseGrant) {
	f.t.Helper()
	cfg := f.rt.slotRuntime(index, "").Config.withEnvironment(*grant.Environment)
	project := cfg.Project
	for _, call := range f.incus.calls {
		if call[0] == "incus" && call[1] == "delete" &&
			(incusCallProject(call[1:]) != project || (call[2] != cfg.vm(1) && call[2] != cfg.vm(2))) {
			f.t.Errorf("unexpected deletion: %q", call)
		}
	}
	for _, name := range []string{cfg.vm(1), cfg.vm(2)} {
		match := func(args ...string) int {
			return slices.IndexFunc(f.incus.calls, func(call []string) bool {
				return slices.Equal(call, append([]string{"incus"}, args...))
			})
		}
		stop := match("stop", name, "--project", project, "--timeout", "60")
		state := []string{"incus", "list", name, "--project", project, "-f", "csv", "-c", "ns"}
		deleted := match("delete", name, "--project", project)
		generation := match("config", "get", name, "user.subyard.generation", "--project", project)
		epoch := match("config", "get", name, "user.subyard.lease-epoch", "--project", project)
		if stop < 0 || deleted <= stop || generation < 0 || generation >= deleted || epoch < 0 ||
			epoch >= deleted || !slices.ContainsFunc(f.incus.calls[stop+1:deleted], func(call []string) bool {
			return slices.Equal(call, state)
		}) {
			f.t.Errorf("%s was not verified as stopped and marker-owned before deletion for generation %d epoch %d",
				name, grant.ResourceGeneration, grant.LeaseEpoch)
		}
	}
}

func TestSlotRuntimesUseDisjointProjectNetworkAccountAndKeys(t *testing.T) {
	f := newBrokerPoolFixture(t, 2)
	firstKey, secondKey := fixturePublicKey(t), fixturePublicKey(t)
	f.acquireWithKey(1, "held-neighbor", firstKey)
	oneKeys := f.keys(1)
	f.acquireWithKey(2, "own-network", secondKey)
	one, two := f.rt.slotRuntime(1, "").Config, f.rt.slotRuntime(2, "").Config
	if one.Project == two.Project || one.Network == two.Network || one.StateDir == two.StateDir ||
		one.AgentUser == two.AgentUser || one.AgentHome == two.AgentHome || one.AgentAuthorizedKeys == two.AgentAuthorizedKeys {
		t.Fatal("slot runtimes share project, network, state or data account paths")
	}
	for _, cfg := range []Config{one, two} {
		if allowed := f.incus.projects[cfg.Project].config["restricted.networks.access"]; allowed != cfg.Network {
			t.Errorf("project %s can access %q, want %q", cfg.Project, allowed, cfg.Network)
		}
	}
	f.assertSlotAccess(1, firstKey, "192.0.2.11:22", "192.0.2.12:22")
	f.assertSlotAccess(2, secondKey, "192.0.2.21:22", "192.0.2.22:22")
	if !bytes.Equal(oneKeys, f.keys(1)) {
		t.Error("acquiring slot 2 changed slot 1's key file")
	}
}

func TestReconcilePoolGrowAndShrinkPreservesHeldLowerSlot(t *testing.T) {
	t.Run("held lower slot", func(t *testing.T) {
		f := newBrokerPoolFixture(t, 1)
		f.acquire(1, "held-through-resize")
		neighbor := f.slot(1)
		brokerMust(t, f.resize(3))
		for _, index := range []int{2, 3} {
			if state := f.slot(index).State; state != SlotAvailable {
				t.Errorf("new slot %d state = %s, want available", index, state)
			}
			f.prepareSlot(index, "")
		}

		f.incus.calls = nil
		brokerMust(t, f.resize(2))
		f.assertRetired(3)
		f.assertNoCallsToSlot(1)
		f.assertNoCallsToSlot(2)
		f.assertEmptyProject(2)
		if !reflect.DeepEqual(neighbor, f.slot(1)) {
			t.Error("grow or shrink changed the held lower slot")
		}
	})

	t.Run("drain before broker recreation", func(t *testing.T) {
		f := newBrokerPoolFixture(t, 3)
		lower, retiring := f.acquire(1, "held-through-drain"), f.acquire(3, "retiring")
		before := brokerRead(t, f.store.Path)
		f.incus.calls = nil
		err := f.resize(2)
		if err == nil || !strings.Contains(err.Error(), "slot-003") || !bytes.Equal(before, brokerRead(t, f.store.Path)) ||
			len(f.incus.calls) != 0 {
			t.Fatalf("shrink touched a held retiring slot or its lease file: %v", err)
		}
		f.rt.Config.SlotCount, f.store.SlotCount = 3, 3
		brokerMust(t, f.rt.DrainAll(f.ctx, f.store, "broker service stopping"))
		for _, grant := range []LeaseGrant{lower, retiring} {
			if response := f.leaseAction("renew", grant); response.Code != "lease_lost" {
				t.Errorf("old grant for %s renew = %s, want lease_lost", grant.SlotID, response.Code)
			}
		}
		f.assertEmptyProject(1)
		f.assertEmptyProject(3)

		f.incus.calls = nil
		brokerMust(t, f.resize(2))
		f.assertRetired(3)
		one := f.rt.slotRuntime(1, "").Config
		three := f.rt.slotRuntime(3, "").Config
		var deletions [][]string
		for _, call := range f.incus.calls {
			if call[0] == "userdel" {
				deletions = append(deletions, call[1:])
			}
		}
		if len(deletions) != 1 || !slices.Equal(deletions[0], []string{three.AgentUser}) ||
			len(f.incus.accounts) != 1 || f.incus.accounts[one.AgentUser] != one.AgentHome {
			t.Errorf("retirement must delete only account %q, got userdel arguments %q", three.AgentUser, deletions)
		}
		if len(f.incus.projects) != 1 || len(f.incus.networks) != 1 ||
			f.incus.projects[one.Project] == nil || f.incus.networks[one.Network] != managedMarker {
			t.Error("broker recreation left projects or networks outside the configured allocation")
		}
		pool, err := f.store.Inspect()
		brokerMust(t, err)
		if len(pool.Slots) != 2 {
			t.Fatalf("reconciled pool has %d slots, want 2", len(pool.Slots))
		}
		for _, slot := range pool.Slots {
			if slot.State != SlotAvailable || slot.LeaseID != "" || slot.CapabilityHash != "" ||
				slot.ClientID != "" || slot.Reserved || slot.Environment != nil || !slot.ExpiresAt.IsZero() {
				t.Errorf("active lease record survived recreation in %s", slot.SlotID)
			}
		}
	})
}

func (f *brokerPoolFixture) resize(count int) error {
	f.rt.Config.SlotCount, f.store.SlotCount = count, count
	return f.rt.ReconcilePool(f.ctx, f.store)
}

func (f *brokerPoolFixture) assertRetired(index int) {
	f.t.Helper()
	cfg := f.rt.slotRuntime(index, "").Config
	if _, ok := f.incus.projects[cfg.Project]; ok {
		f.t.Error("retired project remains")
	}
	if _, ok := f.incus.networks[cfg.Network]; ok {
		f.t.Error("retired network remains")
	}
	for _, path := range []string{cfg.StateDir, cfg.AgentHome} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			f.t.Errorf("retired slot still has %s or could not inspect it: %v", path, err)
		}
	}
	pool, err := f.store.Inspect()
	brokerMust(f.t, err)
	for _, slot := range pool.Slots {
		if slot.SlotID == fmt.Sprintf("slot-%03d", index) {
			f.t.Errorf("retired lease record %s remains", slot.SlotID)
		}
	}
}

func TestBrokerStatusAfterQuarantineIsRedactedAndAttributed(t *testing.T) {
	f := newBrokerPoolFixture(t, 2)
	victim := f.acquire(1, "failed-release")
	neighbor := f.acquire(2, "held-neighbor")
	quarantined := f.failRelease(victim)
	var result facadeResponse
	raw := f.request("status", &result)
	if result.Status != "ok" || result.Pool == nil || len(result.Pool.Slots) != 2 {
		t.Fatal("status did not publish both slots")
	}
	for _, key := range []string{"client_id", "controller_fingerprint", "lease_id", "capability_hash",
		"targets", "address", "host_key_blob", "failure_reason", "checkout"} {
		if bytes.Contains(raw, []byte(`"`+key+`"`)) {
			t.Errorf("status disclosed field %s", key)
		}
	}
	one, two := result.Pool.Slots[0], result.Pool.Slots[1]
	if one.State != SlotQuarantined || one.IncidentID != quarantined.IncidentID ||
		one.LastFailureEventID != quarantined.LastFailureEventID {
		t.Error("public quarantine did not retain incident identifiers")
	}
	ctx := neighbor.Context
	if two.State != SlotHeld || two.DisplayLabel != ctx.Project+"#"+ctx.Run || two.Yard != ctx.Yard ||
		two.Project != ctx.Project || two.Run != ctx.Run || two.Purpose != ctx.Purpose ||
		two.AcquiredAt.IsZero() || two.ExpiresAt.IsZero() || two.LastHeartbeatAt.IsZero() {
		t.Error("held neighbor status lost public lease attribution or timestamps")
	}
}

// A refused release must name the failing step, keep the native stop error and say how
// long cleanup ran, so a lane's 65-second refusal is attributable from its own events.
func TestBrokerReleaseStopEventsNameStepAndTiming(t *testing.T) {
	for _, scenario := range []struct {
		name, faultVerb, wantKind, wantStep string
		wantText                            []string
	}{
		{"graceful stop timeout", "stop", "lease.stop_failed", "release step fence-and-stop: ",
			[]string{fakeIncusUnavailable, `remained in state "RUNNING" after stop`, "graceful stop (timeout 60s) returned after 1m1s"}},
		{"delete failure", "delete", "lease.stop_failed", "release step delete-allocation: ",
			[]string{fakeIncusUnavailable}},
		{"slow successful stop", "", "lease.stop_succeeded", "", nil},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newBrokerPoolFixture(t, 1)
			grant := f.acquire(1, "release-step")
			var project, vm string
			for name, candidate := range f.incus.projects {
				for instance := range candidate.instances {
					project, vm = name, instance
				}
			}
			if vm == "" {
				t.Fatal("fixture did not create the leased VM")
			}
			handler := f.incus.handler
			f.incus.handler = func(name string, args, env []string, input io.Reader) ([]byte, []byte, error) {
				if name == "incus" && len(args) > 0 && args[0] == "stop" {
					f.now = f.now.Add(61 * time.Second) // the graceful stop consumed its budget
				}
				return handler(name, args, env, input)
			}
			switch scenario.faultVerb {
			case "stop":
				f.incus.fault = []string{"stop", vm, "--project", project, "--timeout", "60"}
			case "delete":
				f.incus.fault = []string{"delete", vm, "--project", project}
			}
			result := f.leaseAction("release", grant)
			if (scenario.faultVerb == "") != (result.Status == "ok") {
				t.Fatalf("release: status=%s code=%s message=%s", result.Status, result.Code, result.Message)
			}
			batch, err := f.rt.Events.Export()
			brokerMust(t, err)
			var event *BrokerEvent
			for index := range batch.Events {
				if batch.Events[index].Kind == scenario.wantKind {
					event = &batch.Events[index]
				}
			}
			if event == nil || event.LeaseEpoch != grant.LeaseEpoch || event.DurationMS < 61000 {
				t.Fatalf("%s event lacks lease identity or cleanup duration: %+v", scenario.wantKind, event)
			}
			if !strings.HasPrefix(event.Error, scenario.wantStep) {
				t.Fatalf("event error %q does not name step %q", event.Error, scenario.wantStep)
			}
			for _, text := range scenario.wantText {
				if !strings.Contains(event.Error, text) {
					t.Errorf("event error lacks %q: %q", text, event.Error)
				}
			}
			if scenario.faultVerb != "" && f.slot(1).State != SlotQuarantined {
				t.Fatalf("failed release left slot %s", f.slot(1).State)
			}
		})
	}
}

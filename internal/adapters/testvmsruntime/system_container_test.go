package testvmsruntime

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSystemContainerPool(t *testing.T) {
	f := newBrokerPoolFixture(t, 2)
	var result facadeResponse
	acquire := func(count int) LeaseGrant {
		f.request(fmt.Sprintf("acquire-v3 system-container client-1 SHA256:controller test-yard Broker-Tests 00000001 nested %s slot-001 %d", fixturePublicKey(t), count), &result)
		if result.Status != "ok" || result.Grant == nil {
			t.Fatalf("container acquire: %s %s", result.Code, result.Message)
		}
		return *result.Grant
	}
	grant := acquire(2)
	cfg := f.rt.slotRuntime(1, "").Config.withEnvironment(*grant.Environment)
	project := f.incus.projects[cfg.Project]
	child := f.rt.slotRuntime(1, "")
	child.useLeaseSlot(f.slot(1))
	brokerMust(t, child.ensureProject(f.ctx))
	if project.config["limits.virtual-machines"] != "0" || project.config["restricted"] != "true" ||
		project.config["restricted.containers.privilege"] != "isolated" || project.config["limits.memory"] != "" {
		t.Fatal("container project policy missing")
	}
	for _, vm := range project.instances {
		for _, key := range []string{"limits.memory", "raw.qemu.conf"} {
			if _, ok := vm.config[key]; ok {
				t.Errorf("unexpected %s", key)
			}
		}
		if vm.config["limits.cpu"] == "" || vm.config["limits.processes"] != "4096" ||
			vm.config["boot.autostart"] != "false" || vm.config["security.nesting"] != "true" || vm.config["security.idmap.isolated"] != "true" {
			t.Fatal("missing container limits or isolation")
		}
	}
	for _, call := range f.incus.calls {
		if len(call) > 1 && call[1] == "init" && slices.Contains(call, "--vm") {
			t.Fatal("container initialized as VM")
		}
	}
	// A failed native deletion stays fenced for the next reap.
	f.now = grant.ExpiresAt.Add(time.Second)
	vm := project.instances[cfg.vm(2)]
	vm.kind = "virtual-machine"
	if err := f.rt.ReapExpired(f.ctx, f.store); err == nil || len(project.instances) != 2 || f.slot(1).State != SlotDraining {
		t.Fatal("foreign instance type did not fence the entire allocation before deletion")
	}
	vm.kind = "container"
	f.incus.fault = []string{"delete", cfg.vm(1), "--project", cfg.Project, "--force"}
	if err := f.rt.ReapExpired(f.ctx, f.store); err == nil || f.slot(1).State != SlotDraining {
		t.Fatal("failed deletion did not stay draining")
	}
	var status facadeResponse
	f.request("status", &status)
	if slot := status.Pool.Slots[0]; slot.State != SlotDraining || slot.FailureReason != containerCleanupFailure || !slot.NextRecoveryAt.IsZero() {
		t.Fatal("container cleanup failure is missing from draining slot status")
	}
	events, err := f.rt.eventRecorder().Export()
	brokerMust(t, err)
	if !slices.ContainsFunc(events.Events, func(event BrokerEvent) bool {
		return event.Kind == "lease.stop_failed" && event.SlotID == grant.SlotID && event.LeaseEpoch == grant.LeaseEpoch &&
			event.ResourceGeneration == grant.ResourceGeneration && event.ToState == SlotDraining && strings.Contains(event.Error, fakeIncusUnavailable)
	}) {
		t.Fatal("missing attributed cleanup failure event")
	}
	brokerMust(t, f.rt.ReapExpired(f.ctx, f.store))
	if f.slot(1).State != SlotAvailable || f.slot(1).FailureReason != "" || len(project.instances)+len(project.volumes) != 0 || project.config["restricted.containers.lowlevel"] != "" {
		t.Fatal("expired allocation survived cleanup")
	}
	ready := f.incus.projects[child.preparedRuntime().Config.Project]
	template := ready.instances[cfg.vm(1)]
	if len(ready.instances) != 1 || template.status != "STOPPED" || len(template.agentKeyWrites) != 0 ||
		template.config["user.subyard.lease-epoch"] != "" || template.config["volatile.base_image"] != grant.BaseFingerprint {
		t.Fatal("slot failed to retain a single pristine prepared root")
	}
	f.incus.calls = nil
	next := acquire(1)
	for _, call := range f.incus.calls {
		if len(call) >= 2 && call[0] == "incus" && call[1] == "init" {
			t.Fatal("warm acquire materialized the base again")
		}
	}
	if nextVM := project.instances[cfg.vm(1)]; nextVM.config["volatile.uuid"] == template.config["volatile.uuid"] ||
		nextVM.config["user.subyard.lease-epoch"] != fmt.Sprint(next.LeaseEpoch) || nextVM.config[preparedFor] != "" {
		t.Fatal("warm guest did not receive an independent root and the new lease identity")
	}
	if next.LeaseEpoch == grant.LeaseEpoch || len(project.instances) != 1 {
		t.Fatal("singleton lease reused expired allocation")
	}
	if r := f.leaseAction("release", next); r.Status != "ok" {
		t.Fatalf("release: %s", r.Code)
	}
}

func TestPreparedContainerRotationAndForeignState(t *testing.T) {
	f := newBrokerPoolFixture(t, 1)
	child := f.prepareSlot(1, fixturePublicKey(t))
	spec, err := child.Config.EnvironmentSpec(EnvironmentContainer)
	brokerMust(t, err)
	child.Config = child.Config.withEnvironment(spec)
	child.Config.Image = brokerFixtureBase
	child.allocation = &LeaseIdentity{SlotID: "slot-001", ResourceGeneration: 1, LeaseEpoch: 1}
	brokerMust(t, child.ensureProject(f.ctx))
	vm := child.Config.vm(1)
	brokerMust(t, child.initVM(f.ctx, vm))
	brokerMust(t, child.deleteAllocation(f.ctx))
	project := f.incus.projects[child.preparedRuntime().Config.Project]
	source := project.instances[vm]
	child.Config.Image = strings.Repeat("b", 64)
	// Even a stopped object with valid markers is unsafe once it has booted.
	source.lastUsedAt = f.now
	if err := child.initVM(f.ctx, vm); err == nil || child.retirePreparedContainer(f.ctx) == nil || project.instances[vm] != source {
		t.Fatal("used template was copied or deleted")
	}
	source.lastUsedAt = time.Time{}
	project.config[preparedFor] = "other-slot"
	if err := child.initVM(f.ctx, vm); err == nil || project.instances[vm] != source {
		t.Fatal("rotated a template in a foreign project")
	}
	project.config[preparedFor] = child.Config.Project
	brokerMust(t, child.initVM(f.ctx, vm))
	if project.instances[vm] == source || project.instances[vm].config["volatile.base_image"] != child.Config.Image {
		t.Fatal("slot did not rotate its prepared root to the pinned base")
	}
	brokerMust(t, child.retirePreparedContainer(f.ctx))
	if f.incus.projects[child.preparedRuntime().Config.Project] != nil {
		t.Fatal("retired slot kept its prepared project")
	}
}

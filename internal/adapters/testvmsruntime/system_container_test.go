package testvmsruntime

import (
	"fmt"
	"slices"
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
	f.incus.fault = []string{"delete", cfg.vm(1), "--project", cfg.Project, "--force"}
	if err := f.rt.ReapExpired(f.ctx, f.store); err == nil || f.slot(1).State != SlotDraining {
		t.Fatal("failed deletion did not stay draining")
	}
	brokerMust(t, f.rt.ReapExpired(f.ctx, f.store))
	if f.slot(1).State != SlotAvailable || len(project.instances)+len(project.volumes) != 0 || project.config["restricted.containers.lowlevel"] != "" {
		t.Fatal("expired allocation survived cleanup")
	}
	next := acquire(1)
	if next.LeaseEpoch == grant.LeaseEpoch || len(project.instances) != 1 {
		t.Fatal("singleton lease reused expired allocation")
	}
	if r := f.leaseAction("release", next); r.Status != "ok" {
		t.Fatalf("release: %s", r.Code)
	}
}

package testvmsruntime

import (
	"context"
	"fmt"
	"io"
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
	next := acquire(1)
	if next.LeaseEpoch == grant.LeaseEpoch || len(project.instances) != 1 {
		t.Fatal("singleton lease reused expired allocation")
	}
	if r := f.leaseAction("release", next); r.Status != "ok" {
		t.Fatalf("release: %s", r.Code)
	}
}

func TestSystemContainerGrantPinsSelectedImage(t *testing.T) {
	f := newBrokerPoolFixture(t, 2)
	spec, err := f.rt.Config.EnvironmentSpec(EnvironmentContainer)
	brokerMust(t, err)
	spec.Image = "ubuntu-24.04"
	grant, err := f.store.AcquireV3Slot(spec, "client", "SHA256:key", "yard", "Project", "run", "tests", "slot-001")
	brokerMust(t, err)
	f.rt.memoryProbe = func() (MemoryCapacity, error) { return MemoryCapacity{Available: 128 << 30}, nil }
	f.rt.Runner = &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
		switch strings.Join(args, " ") {
		case "query /1.0/storage-pools/default/resources":
			return []byte(`{"space":{"total":171798691840,"used":10737418240}}`), nil, nil
		case "image list images: ubuntu/24.04/cloud type=container --format json":
			return []byte(`[{"fingerprint":"` + brokerFixtureBase + `","type":"container","architecture":"` +
				imageArchitecture() + `","aliases":[{"name":"ubuntu/24.04/cloud"}]}]`), nil, nil
		}
		return nil, nil, fmt.Errorf("unexpected native command %q", args)
	}}
	child := f.rt.slotRuntime(1, "")
	child.Config = child.Config.withEnvironment(spec)
	grant, err = f.rt.prepareEnvironment(context.Background(), f.store, grant, child)
	brokerMust(t, err)
	slot := f.slot(1)
	if !slot.Reserved || slot.BaseFingerprint != brokerFixtureBase || grant.BaseFingerprint != slot.BaseFingerprint ||
		child.Config.Image != "images:"+slot.BaseFingerprint {
		t.Fatal("container grant lacks reserved, immutable source")
	}
}

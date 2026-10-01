package testvmsruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResourceStatusIsReadOnlyAndSeparatesPhysicalUsage(t *testing.T) {
	cfg := fixtureConfig(t)
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	runner := &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
		switch strings.Join(args, " ") {
		case "query /1.0/storage-pools/default/resources":
			return []byte(`{"space":{"total":107374182400,"used":1073741824}}`), nil, nil
		case "query /1.0/storage-pools/default":
			return []byte(`{"driver":"zfs"}`), nil, nil
		default:
			return nil, nil, fmt.Errorf("status attempted unexpected operation: %v", args)
		}
	}}
	rt := Runtime{Config: cfg, Runner: runner, Now: func() time.Time { return now }, cacheProbe: func(context.Context) (CacheUsage, error) {
		return CacheUsage{Driver: "zfs", ChargedBytes: 1234, Accounting: "conservative-shared-inclusive"}, nil
	}, memoryProbe: func() (MemoryCapacity, error) {
		return MemoryCapacity{Available: 16 << 30, scope: "synthetic-boundary"}, nil
	}, usageProbe: func(_ context.Context, _ LeaseSlot, _ string) allocationUsage {
		return allocationUsage{memory: 3 << 30, disk: 5 << 30, memoryKnown: true, diskKnown: true}
	}}
	empty := rt.ResourceStatus(context.Background(), LeasePool{})
	if len(empty.Bases) != 0 {
		t.Fatal("missing registry invented bases")
	}
	if _, err := os.Stat(rt.imageRegistryPath()); !os.IsNotExist(err) {
		t.Fatalf("status created registry: %v", err)
	}
	spec, _ := cfg.EnvironmentSpec(EnvironmentPair)
	registry := ImageRegistry{SchemaVersion: 1, Owner: strings.Repeat("a", 32), LastError: "private/path and secret detail",
		Bases: []BaseImage{
			{Key: strings.Repeat("b", 64), Fingerprint: strings.Repeat("c", 64), Environment: EnvironmentPair, CreatedAt: now.Add(-time.Hour), Size: 42},
			{Key: strings.Repeat("d", 64), Fingerprint: strings.Repeat("e", 64), Environment: EnvironmentPair, CreatedAt: now, Unusable: true},
		},
		Build: &BaseBuild{Environment: EnvironmentAndroid, Project: "private-builder-path", Memory: 8 << 30, Disk: 120 << 30},
	}
	if err := os.MkdirAll(cfg.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(rt.imageRegistryPath(), registry); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(rt.imageRegistryPath())
	value := rt.ResourceStatus(context.Background(), LeasePool{Slots: []LeaseSlot{{SlotID: "slot-001", State: SlotHeld, Environment: &spec, Reserved: true, LeaseID: "secret-lease"}}})
	if value.Storage == nil || value.Storage.Used != 1<<30 || value.Storage.BudgetUsed != 1<<30 || value.Storage.Free != 99<<30 || value.Storage.Driver != "zfs" {
		t.Fatalf("incorrect physical pool telemetry: %+v", value.Storage)
	}
	if value.Cache == nil || value.Cache.ChargedBytes != 1234 || value.Cache.PhysicalBytes != nil {
		t.Fatal("cache charge omitted or mislabeled physical")
	}
	if value.VirtualDiskCapacity != 2*budgetBytes(spec.Disk, "20GiB") || value.ReservedMemory == 0 || value.Builder.DiskPeak != 120<<30 {
		t.Fatal("reservations omitted or mixed with physical usage")
	}
	if len(value.Slots) != 1 || value.ConfirmedWorkingDisk != 5<<30 || value.WorkingDiskEvidence != "complete" ||
		value.Slots[0].RemainingDisk != value.VirtualDiskCapacity-5<<30 ||
		value.Slots[0].ConfirmedAnonMemory != 3<<30 || value.Slots[0].MemoryEvidence != "qemu_cgroup_anon_shmem" {
		t.Fatalf("measured working usage not separated from commitments: %+v", value)
	}
	rt.usageProbe = func(_ context.Context, _ LeaseSlot, _ string) allocationUsage {
		return allocationUsage{memoryReason: memoryIncusQueryDeadline}
	}
	unknown := rt.ResourceStatus(context.Background(), LeasePool{Slots: []LeaseSlot{{SlotID: "slot-001", State: SlotHeld, Environment: &spec, Reserved: true}}})
	if unknown.WorkingDiskEvidence != "unknown" || len(unknown.Slots) != 1 || unknown.Slots[0].RemainingDisk != unknown.VirtualDiskCapacity || unknown.Slots[0].DiskEvidence != "unknown" || unknown.Slots[0].MemoryEvidenceReason != "incus_query_deadline" ||
		unknown.Slots[0].ConfirmedAnonMemory != 0 || unknown.Slots[0].RemainingMemory != unknown.ReservedMemory {
		t.Fatalf("unknown usage received unsafe credit: %+v", unknown)
	}
	if value.OuterHostEvidence != "unavailable: allocation boundary" || !value.Bases[0].Current || value.Bases[0].AgeSeconds != 3600 {
		t.Fatal("missing scope/base age evidence")
	}
	if value.Bases[0].Unusable || !value.Bases[1].Unusable || value.Bases[1].Current {
		t.Fatal("unusable image concealed or selected as current")
	}
	body, _ := json.Marshal(value)
	if bytes.Contains(body, []byte("memory_evidence_reason")) {
		t.Fatal("known memory evidence included a failure reason")
	}
	unknownBody, _ := json.Marshal(unknown)
	if !bytes.Contains(unknownBody, []byte(`"memory_evidence_reason":"incus_query_deadline"`)) {
		t.Fatal("unknown memory evidence omitted its bounded reason")
	}
	rt.usageProbe = func(_ context.Context, _ LeaseSlot, _ string) allocationUsage {
		return allocationUsage{memory: 1024, memoryReason: memoryProcessesNotIsolated}
	}
	partial := rt.ResourceStatus(context.Background(), LeasePool{Slots: []LeaseSlot{{SlotID: "slot-001", State: SlotHeld, Environment: &spec, Reserved: true}}})
	if partial.Slots[0].MemoryEvidence != "partial_qemu_cgroup_anon_shmem" ||
		partial.Slots[0].MemoryEvidenceReason != "cgroup_processes_not_isolated" ||
		partial.ConfirmedAnonMemory != 1024 || partial.Slots[0].RemainingMemory != partial.ReservedMemory-1024 {
		t.Fatalf("partial memory semantics changed: %+v", partial.Slots[0])
	}
	for _, secret := range []string{"secret-lease", "private-builder-path", "private/path", registry.Owner} {
		if bytes.Contains(body, []byte(secret)) {
			t.Fatalf("status leaked %s", secret)
		}
	}
	after, _ := os.ReadFile(rt.imageRegistryPath())
	if !bytes.Equal(before, after) {
		t.Fatal("status rewrote registry")
	}
}

func TestResourceStatusScalesRequestedVMCount(t *testing.T) {
	for _, count := range []int{1, 2} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			cfg := fixtureConfig(t)
			cfg.Memory, cfg.Disk = "4GiB", "20GiB"
			spec, err := cfg.EnvironmentSpec(EnvironmentPair)
			if err != nil {
				t.Fatal(err)
			}
			spec.Count = count
			rt := Runtime{
				Config: cfg,
				memoryProbe: func() (MemoryCapacity, error) {
					return MemoryCapacity{Available: 16 << 30}, nil
				},
				diskUsageProbe: func(context.Context) (uint64, error) { return 1 << 30, nil },
				cacheProbe:     func(context.Context) (CacheUsage, error) { return CacheUsage{}, nil },
				usageProbe: func(context.Context, LeaseSlot, string) allocationUsage {
					return allocationUsage{memoryKnown: true, diskKnown: true}
				},
				Runner: &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
					switch strings.Join(args, " ") {
					case "query /1.0/storage-pools/default/resources":
						return []byte(`{"space":{"total":107374182400,"used":1073741824}}`), nil, nil
					case "query /1.0/storage-pools/default":
						return []byte(`{"driver":"zfs"}`), nil, nil
					default:
						return nil, nil, fmt.Errorf("unexpected status operation: %v", args)
					}
				}},
			}
			status := rt.ResourceStatus(context.Background(), LeasePool{Slots: []LeaseSlot{
				{SlotID: "slot-001", State: SlotHeld, Environment: &spec, Reserved: true},
			}})
			wantMemory, wantDisk := uint64(count)*(9<<29), uint64(count)*(20<<30)
			if len(status.Errors) != 0 || status.ReservedMemory != wantMemory || status.VirtualDiskCapacity != wantDisk || len(status.Slots) != 1 {
				t.Fatalf("wrong aggregate commitments: %+v", status)
			}
			slot := status.Slots[0]
			if slot.MemoryCommitment != wantMemory || slot.RemainingMemory != wantMemory || slot.VirtualDiskCapacity != wantDisk || slot.RemainingDisk != wantDisk {
				t.Fatalf("wrong slot commitments: %+v", slot)
			}
		})
	}
}

func TestRefreshFailurePreservesPreviousBaseAndLeasePool(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.RecipeRoot = t.TempDir()
	if err := os.WriteFile(filepath.Join(cfg.RecipeRoot, "manifest.sha256"), []byte("fixture recipe"), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	rt := Runtime{Config: cfg, Now: func() time.Time { return now }, Runner: &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
		if len(args) > 1 && args[0] == "image" && args[1] == "list" {
			return nil, nil, errors.New("upstream unavailable")
		}
		return nil, nil, fmt.Errorf("unexpected refresh mutation: %v", args)
	}}}
	registry := ImageRegistry{SchemaVersion: 1, Owner: strings.Repeat("a", 32), Bases: []BaseImage{{Key: strings.Repeat("b", 64), Fingerprint: strings.Repeat("c", 64), Environment: EnvironmentPair, CreatedAt: now.Add(-time.Hour)}}}
	if err := os.MkdirAll(cfg.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(rt.imageRegistryPath(), registry); err != nil {
		t.Fatal(err)
	}
	store := LeaseStore{Path: filepath.Join(cfg.StateDir, "leases.json"), SlotCount: 2}
	if err := rt.RefreshBase(context.Background(), store, EnvironmentPair); err == nil {
		t.Fatal("failed refresh reported success")
	}
	actual, err := rt.imageRegistry()
	if err != nil || len(actual.Bases) != 1 || actual.Bases[0].Fingerprint != registry.Bases[0].Fingerprint || actual.LastError != "base_build_failed" || !actual.RetryAfter.After(now) {
		t.Fatalf("failed refresh discarded current base or diagnostics: %+v %v", actual, err)
	}
	if _, err := os.Stat(store.Path); !os.IsNotExist(err) {
		t.Fatalf("refresh changed lease pool: %v", err)
	}
}

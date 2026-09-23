package testvmsruntime

import (
	"context"
	"encoding/json"
	"os"
	"time"
)

type ResourceStatus struct {
	Memory               *MemoryCapacity        `json:"memory,omitempty"`
	OuterHostEvidence    string                 `json:"outer_host_evidence"`
	Cache                *CacheUsage            `json:"cache,omitempty"`
	Storage              *PoolStorageStatus     `json:"storage,omitempty"`
	Budgets              map[string]uint64      `json:"budgets"`
	ReservedMemory       uint64                 `json:"reserved_vm_memory_bytes"`
	VirtualDiskCapacity  uint64                 `json:"reserved_vm_virtual_disk_bytes"`
	ConfirmedAnonMemory  uint64                 `json:"confirmed_anon_shmem_bytes"`
	ConfirmedWorkingDisk uint64                 `json:"confirmed_working_disk_physical_bytes"`
	WorkingDiskEvidence  string                 `json:"working_disk_evidence"`
	Slots                []SlotResourceStatus   `json:"slots"`
	Builder              *BuilderResourceStatus `json:"builder,omitempty"`
	Bases                []BaseResourceStatus   `json:"bases"`
	RetryAfter           time.Time              `json:"base_retry_after,omitempty"`
	LastBuildError       string                 `json:"last_build_error,omitempty"`
	Errors               []string               `json:"errors,omitempty"`
}

type PoolStorageStatus struct {
	Driver     string `json:"driver"`
	Total      uint64 `json:"physical_total_bytes"`
	Used       uint64 `json:"physical_used_bytes"`
	Free       uint64 `json:"physical_free_bytes"`
	BudgetUsed uint64 `json:"budget_used_bytes"`
}

type BuilderResourceStatus struct {
	Environment     string `json:"type"`
	Memory          uint64 `json:"reserved_memory_bytes"`
	DiskPeak        uint64 `json:"reserved_disk_peak_bytes"`
	PeakMeasurement string `json:"observed_peak"`
}

type SlotResourceStatus struct {
	SlotID                string `json:"slot_id"`
	Environment           string `json:"type"`
	MemoryCommitment      uint64 `json:"memory_commitment_bytes"`
	ConfirmedAnonMemory   uint64 `json:"confirmed_anon_shmem_bytes"`
	RemainingMemory       uint64 `json:"remaining_memory_growth_bytes"`
	MemoryEvidence        string `json:"memory_evidence"`
	VirtualDiskCapacity   uint64 `json:"virtual_disk_capacity_bytes"`
	ConfirmedPhysicalDisk uint64 `json:"confirmed_physical_disk_bytes"`
	RemainingDisk         uint64 `json:"remaining_disk_growth_bytes"`
	DiskEvidence          string `json:"disk_evidence"`
}

func usageEvidence(known bool, amount uint64, confirmed string) string {
	if known {
		return confirmed
	}
	if amount > 0 {
		return "partial_" + confirmed
	}
	return "unknown"
}

type BaseResourceStatus struct {
	Environment     string    `json:"type"`
	Fingerprint     string    `json:"fingerprint"`
	CreatedAt       time.Time `json:"created_at"`
	AgeSeconds      int64     `json:"age_seconds"`
	CompressedBytes uint64    `json:"compressed_bytes"`
	Current         bool      `json:"current"`
	Expired         bool      `json:"expired"`
	Unusable        bool      `json:"unusable"`
}

// ResourceStatus is a bounded read-only projection. Incus owns shared CoW block
// accounting; virtual commitments and image archive sizes are never added to its
// physical usage. Missing outer-host evidence is explicit even on a healthy yard.
func (rt *Runtime) ResourceStatus(ctx context.Context, pool LeasePool) ResourceStatus {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if rt.Runner == nil {
		rt.Runner = ProcessRunner{}
	}
	now := time.Now()
	if rt.Now != nil {
		now = rt.Now()
	}
	result := ResourceStatus{OuterHostEvidence: "unavailable: allocation boundary", WorkingDiskEvidence: "unknown", Slots: []SlotResourceStatus{}, Bases: []BaseResourceStatus{}, Budgets: map[string]uint64{
		"disk_bytes":           budgetBytes(rt.Config.DiskBudget, "160GiB"),
		"cache_bytes":          budgetBytes(rt.Config.CacheBudget, "24GiB"),
		"disk_reserve_bytes":   budgetBytes(rt.Config.DiskReserve, "5GiB"),
		"memory_reserve_bytes": budgetBytes(rt.Config.MemoryReserve, "2GiB"),
		"vm_overhead_bytes":    budgetBytes(rt.Config.VMOverhead, "512MiB"),
	}}
	memory, err := rt.readMemoryCapacity()
	if err == nil {
		result.Memory = &memory
	} else {
		result.Errors = append(result.Errors, "memory_telemetry_unavailable")
	}
	storage, err := rt.storageCapacity(ctx)
	if err == nil {
		result.Storage = &PoolStorageStatus{Driver: "unknown", Total: storage.Total, Used: storage.Used, Free: storage.Total - storage.Used, BudgetUsed: storage.BudgetUsed}
		body, driverErr := rt.incus(ctx, "query", "/1.0/storage-pools/default")
		var info struct {
			Driver string `json:"driver"`
		}
		if driverErr == nil && json.Unmarshal([]byte(body), &info) == nil && safeName.MatchString(info.Driver) {
			result.Storage.Driver = info.Driver
		} else {
			result.Errors = append(result.Errors, "storage_driver_unavailable")
		}
	} else {
		result.Errors = append(result.Errors, "storage_telemetry_unavailable")
	}
	if cache, err := rt.CacheUsage(ctx); err == nil {
		result.Cache = &cache
	} else {
		result.Errors = append(result.Errors, "cache_telemetry_unavailable")
	}
	var measuredSlots, confirmedDiskSlots int
	for _, slot := range pool.Slots {
		if slot.State == SlotAvailable || slot.State == SlotUnavailable {
			continue
		}
		spec := slot.Environment
		if spec == nil {
			legacy, err := rt.Config.EnvironmentSpec(EnvironmentPair)
			if err != nil {
				result.Errors = append(result.Errors, "legacy_reservation_unknown")
				continue
			}
			spec = &legacy
		} else if !slot.Reserved {
			continue
		}
		ram, disk := environmentCommitment(*spec, result.Budgets["vm_overhead_bytes"])
		result.ReservedMemory += ram
		result.VirtualDiskCapacity += disk
		measuredSlots++
		memoryScope := ""
		if result.Memory != nil {
			memoryScope = result.Memory.scope
		}
		usage := rt.allocationUsage(ctx, slot, memoryScope)
		if usage.diskKnown {
			confirmedDiskSlots++
		}
		result.ConfirmedAnonMemory += usage.memory
		result.ConfirmedWorkingDisk += usage.disk
		result.Slots = append(result.Slots, SlotResourceStatus{
			SlotID: slot.SlotID, Environment: spec.Name,
			MemoryCommitment: ram, ConfirmedAnonMemory: usage.memory,
			RemainingMemory:     ram - min(ram, usage.memory),
			MemoryEvidence:      usageEvidence(usage.memoryKnown, usage.memory, "qemu_cgroup_anon_shmem"),
			VirtualDiskCapacity: disk, ConfirmedPhysicalDisk: usage.disk,
			RemainingDisk: disk - min(disk, usage.disk),
			DiskEvidence:  usageEvidence(usage.diskKnown, usage.disk, "dir_ext4_allocated_blocks"),
		})
	}
	if measuredSlots > 0 {
		if confirmedDiskSlots == measuredSlots {
			result.WorkingDiskEvidence = "complete"
		} else if result.ConfirmedWorkingDisk > 0 {
			result.WorkingDiskEvidence = "partial"
		}
	}
	// Do not call any create/repair/GC path when the registry has not been installed.
	if _, err := os.Stat(rt.imageRegistryPath()); os.IsNotExist(err) {
		return result
	}
	registry, err := rt.imageRegistry()
	if err != nil {
		result.Errors = append(result.Errors, "image_registry_unavailable")
		return result
	}
	result.RetryAfter = registry.RetryAfter
	switch registry.LastError {
	case "", "base_build_failed", "capacity_memory", "capacity_disk", "upstream_lookup_failed":
		result.LastBuildError = registry.LastError
	default:
		result.LastBuildError = "base_build_failed"
	}
	if registry.Build != nil {
		result.Builder = &BuilderResourceStatus{Environment: registry.Build.Environment, Memory: registry.Build.Memory, DiskPeak: registry.Build.Disk, PeakMeasurement: "unavailable"}
	}
	current := map[string]BaseImage{}
	for _, base := range registry.Bases {
		if base.Unusable {
			continue
		}
		previous, found := current[base.Environment]
		if !found || base.CreatedAt.After(previous.CreatedAt) {
			current[base.Environment] = base
		}
	}
	for _, base := range registry.Bases {
		age := now.Sub(base.CreatedAt)
		result.Bases = append(result.Bases, BaseResourceStatus{Environment: base.Environment, Fingerprint: base.Fingerprint,
			CreatedAt: base.CreatedAt, AgeSeconds: max(0, int64(age/time.Second)), CompressedBytes: base.Size,
			Current: !base.Unusable && current[base.Environment].Fingerprint == base.Fingerprint,
			Expired: age < 0 || age >= baseMaxAge, Unusable: base.Unusable})
	}
	return result
}

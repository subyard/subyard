package testvmsruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// CapacityError is safe to retry only when no working allocation was created.
// It deliberately contains no host paths or ambient configuration.
type CapacityError struct{ Resource, Reason string }

func (err *CapacityError) Error() string {
	return "test environment capacity: " + err.Resource + ": " + err.Reason
}

type MemoryCapacity struct {
	Available         uint64 `json:"available_bytes"`
	Current           uint64 `json:"cgroup_current_bytes"`
	Limit             uint64 `json:"cgroup_limit_bytes"`
	Peak              uint64 `json:"cgroup_peak_bytes,omitempty"`
	OOM               uint64 `json:"cgroup_oom_events"`
	OOMKills          uint64 `json:"cgroup_oom_kills"`
	PeakAvailable     bool   `json:"peak_available"`
	EventsAvailable   bool   `json:"events_available"`
	LimitAvailable    bool   `json:"limit_available"`
	OuterHostEvidence string `json:"outer_host_evidence"`
	// scope is the cgroup whose free space bounds admission. VM usage is credited
	// only when its dedicated cgroup is inside this boundary.
	scope string
}

// The visible cgroup root bounds the broker and all its descendants. We also
// cap against host MemAvailable; a large cgroup limit cannot promise physical RAM.
func memoryCapacity(procRoot, cgroupRoot string) (MemoryCapacity, error) {
	return memoryCapacityForProcess(procRoot, cgroupRoot, "self")
}

func memoryCapacityForProcess(procRoot, cgroupRoot, process string) (MemoryCapacity, error) {
	var result MemoryCapacity
	result.OuterHostEvidence = "unavailable: allocation boundary"
	data, err := os.ReadFile(filepath.Join(procRoot, "meminfo"))
	if err != nil {
		return result, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "MemAvailable:" && fields[2] == "kB" {
			value, parseErr := strconv.ParseUint(fields[1], 10, 64)
			if parseErr != nil || value > ^uint64(0)/1024 {
				return result, errors.New("invalid available memory")
			}
			result.Available = value * 1024
		}
	}
	if result.Available == 0 {
		return result, errors.New("available memory unavailable")
	}
	// Inspect all visible ancestors. A namespaced root may hide outer limits;
	// report that boundary explicitly instead of claiming physical-host evidence.
	membership, err := os.ReadFile(filepath.Join(procRoot, process, "cgroup"))
	if err != nil {
		return result, errors.New("cgroup membership unavailable")
	}
	path := ""
	found := false
	for _, line := range strings.Split(string(membership), "\n") {
		if strings.HasPrefix(line, "0::/") {
			path = strings.TrimPrefix(line, "0::/")
			found = true
		}
	}
	if !found || strings.Contains(path, "..") || strings.Contains(path, " (deleted)") {
		return result, errors.New("invalid unified cgroup membership")
	}
	root := filepath.Clean(cgroupRoot)
	current := filepath.Join(root, path)
	tightest := ^uint64(0)
	result.scope = root
	rootMissingLimit := false
	for {
		read := func(name string) (uint64, error) {
			body, err := os.ReadFile(filepath.Join(current, name))
			if err != nil {
				return 0, err
			}
			return strconv.ParseUint(strings.TrimSpace(string(body)), 10, 64)
		}
		limit, limitErr := os.ReadFile(filepath.Join(current, "memory.max"))
		if limitErr != nil {
			// The real host root has no memory.max interface. A missing
			// non-root controller is never silently interpreted as unlimited.
			controllers, controllerErr := os.ReadFile(filepath.Join(current, "cgroup.controllers"))
			if current != root || !os.IsNotExist(limitErr) || controllerErr != nil || !strings.Contains(" "+strings.TrimSpace(string(controllers))+" ", " memory ") {
				return result, errors.New("cgroup v2 memory limit unavailable")
			}
			rootMissingLimit = true
		} else {
			used, err := read("memory.current")
			if err != nil {
				return result, err
			}
			if current == root && !result.LimitAvailable {
				result.Current = used
			}
			if strings.TrimSpace(string(limit)) != "max" {
				maximum, err := strconv.ParseUint(strings.TrimSpace(string(limit)), 10, 64)
				if err != nil {
					return result, err
				}
				free := uint64(0)
				if used < maximum {
					free = maximum - used
				}
				if !result.LimitAvailable || free < tightest {
					tightest = free
					result.Limit, result.Current = maximum, used
					result.scope = current
				}
				result.LimitAvailable = true
				result.Available = min(result.Available, free)
			}
		}
		if current == root {
			break
		}
		current = filepath.Dir(current)
	}
	// Peak and OOM counters must describe the selected limiting cgroup, not an
	// unrelated ancestor with larger historical numbers.
	if body, err := os.ReadFile(filepath.Join(result.scope, "memory.peak")); err == nil && !(rootMissingLimit && result.scope == root) {
		if result.Peak, err = strconv.ParseUint(strings.TrimSpace(string(body)), 10, 64); err != nil {
			return result, err
		}
		result.PeakAvailable = true
	}
	if body, err := os.ReadFile(filepath.Join(result.scope, "memory.events")); err == nil {
		result.EventsAvailable = true
		for _, line := range strings.Split(string(body), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 {
				continue
			}
			n, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return result, err
			}
			switch fields[0] {
			case "oom":
				result.OOM = n
			case "oom_kill":
				result.OOMKills = n
			}
		}
	}

	return result, nil
}

func budgetBytes(value, fallback string) uint64 {
	if value == "" {
		value = fallback
	}
	if value == "0GiB" { // An optional disk quota; physical reserves still apply.
		return 0
	}
	n, _ := sizeMiB(value)
	return uint64(n) * 1024 * 1024
}

type StorageCapacity struct {
	Total      uint64 `json:"total_bytes"`
	Used       uint64 `json:"used_bytes"`
	BudgetUsed uint64 `json:"budget_used_bytes"`
}

func (runtime *Runtime) storageCapacity(ctx context.Context) (StorageCapacity, error) {
	body, err := runtime.incus(ctx, "query", "/1.0/storage-pools/default/resources")
	if err != nil {
		return StorageCapacity{}, err
	}
	var response struct {
		Space struct {
			Total uint64 `json:"total"`
			Used  uint64 `json:"used"`
		} `json:"space"`
	}
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		return StorageCapacity{}, err
	}
	if response.Space.Total == 0 || response.Space.Used > response.Space.Total {
		return StorageCapacity{}, errors.New("invalid pool capacity")
	}
	charged, err := runtime.diskBudgetUsage(ctx, response.Space.Used)
	if err != nil {
		return StorageCapacity{}, err
	}
	return StorageCapacity{Total: response.Space.Total, Used: response.Space.Used, BudgetUsed: charged}, nil
}

func environmentCommitment(spec EnvironmentSpec, overhead uint64) (memory, disk uint64) {
	return uint64(spec.Count) * (budgetBytes(spec.Memory, "4GiB") + overhead),
		uint64(spec.Count) * budgetBytes(spec.Disk, "20GiB")
}

func (runtime *Runtime) outstandingCommitment(ctx context.Context, slot LeaseSlot, overhead uint64, memoryScope string) (uint64, uint64, error) {
	spec := slot.Environment
	if spec == nil {
		legacy, err := runtime.Config.EnvironmentSpec(EnvironmentPair)
		if err != nil {
			return 0, 0, err
		}
		spec = &legacy
	}
	memory, disk := environmentCommitment(*spec, overhead)
	used := runtime.allocationUsage(ctx, slot, memoryScope)
	return memory - min(memory, used.memory), disk - min(disk, used.disk), nil
}

// Full commitments intentionally remain conservative: actual usage is reported
// separately, and shared CoW blocks are counted only by Incus at the pool level.
func checkCapacity(memory MemoryCapacity, storage StorageCapacity, ram, disk, ramReserve, diskReserve, diskBudget uint64) error {
	if ram > memory.Available || ramReserve > memory.Available-ram {
		return &CapacityError{"memory", "insufficient confirmed memory reserve"}
	}
	if storage.Used > storage.Total {
		return &CapacityError{"disk", "invalid storage telemetry"}
	}
	free := storage.Total - storage.Used
	if disk > free || diskReserve > free-disk {
		return &CapacityError{"disk", "insufficient storage headroom"}
	}
	if diskBudget != 0 && (storage.BudgetUsed > diskBudget || disk > diskBudget-storage.BudgetUsed) {
		return &CapacityError{"disk", "broker disk budget exceeded"}
	}
	return nil
}

func (runtime *Runtime) reserveEnvironment(ctx context.Context, store LeaseStore, grant LeaseGrant) error {
	return store.withLock(true, func(pool *LeasePool) error {
		slot, err := findSlot(pool, grant.SlotID)
		if err != nil {
			return err
		}
		if slot.State != SlotProvisioning || slot.LeaseID != grant.LeaseID ||
			slot.LeaseEpoch != grant.LeaseEpoch || slot.CapabilityHash != capabilityDigest(grant.Capability) {
			return ErrLeaseLost
		}
		if slot.Environment == nil {
			return errors.New("environment missing from reservation")
		}
		memory, err := runtime.readMemoryCapacity()
		if err != nil {
			return &CapacityError{"memory", "memory telemetry unavailable"}
		}
		memoryScope := memory.scope
		storageBefore, err := runtime.storageCapacity(ctx)
		if err != nil {
			return &CapacityError{"disk", "storage telemetry unavailable"}
		}
		memoryBefore := memory.Available
		overhead := budgetBytes(runtime.Config.VMOverhead, "512MiB")
		var ram, disk uint64
		for _, current := range pool.Slots {
			if current.SlotID != grant.SlotID && !current.Reserved && current.Environment != nil {
				continue
			}
			if current.State == SlotAvailable || current.State == SlotUnavailable {
				continue
			}
			m, d, err := runtime.outstandingCommitment(ctx, current, overhead, memoryScope)
			if err != nil {
				return err
			}
			ram += m
			disk += d
		}
		// Bound free space on both sides of usage sampling. A guest can also
		// release credited RAM or disk blocks; the later free-space sample
		// alone would then count the same bytes twice.
		memory, err = runtime.readMemoryCapacity()
		if err != nil || memory.scope != memoryScope {
			return &CapacityError{"memory", "memory telemetry unavailable"}
		}
		storage, err := runtime.storageCapacity(ctx)
		if err != nil {
			return &CapacityError{"disk", "storage telemetry unavailable"}
		}
		memory.Available = min(memoryBefore, memory.Available)
		storage.Total = min(storageBefore.Total, storage.Total)
		storage.Used = max(storageBefore.Used, storage.Used)
		storage.BudgetUsed = max(storageBefore.BudgetUsed, storage.BudgetUsed)
		if err := checkCapacity(memory, storage, ram, disk,
			budgetBytes(runtime.Config.MemoryReserve, "2GiB"), budgetBytes(runtime.Config.DiskReserve, "5GiB"),
			budgetBytes(runtime.Config.DiskBudget, "0GiB")); err != nil {
			return err
		}
		slot.Reserved = true
		return nil
	})
}

func (runtime *Runtime) capacityEvidence() string {
	value, err := runtime.readMemoryCapacity()
	if err != nil {
		return "memory telemetry unavailable"
	}
	body, err := json.Marshal(value)
	if err != nil {
		return "memory telemetry unavailable"
	}
	return fmt.Sprintf("memory capacity %s", body)
}

func (runtime *Runtime) readMemoryCapacity() (MemoryCapacity, error) {
	if runtime.memoryProbe != nil {
		return runtime.memoryProbe()
	}
	return memoryCapacity("/proc", "/sys/fs/cgroup")
}

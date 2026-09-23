package testvmsruntime

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// allocationUsage is confirmed host usage already subtracted from live free
// space. Missing or ambiguous measurements give no credit to admission.
type allocationUsage struct {
	memory, disk           uint64
	memoryKnown, diskKnown bool
}

func (rt *Runtime) allocationUsage(ctx context.Context, slot LeaseSlot, memoryScope string) allocationUsage {
	if rt.usageProbe != nil {
		return rt.usageProbe(ctx, slot, memoryScope)
	}
	if slot.Environment == nil || !slot.Reserved {
		return allocationUsage{}
	}
	index, err := slotNumber(slot.SlotID, rt.Config.SlotCount)
	if err != nil {
		return allocationUsage{}
	}
	child := rt.slotRuntime(index, "")
	child.useLeaseSlot(slot)
	var pool struct {
		Driver string `json:"driver"`
	}
	if body, err := rt.incus(ctx, "query", "/1.0/storage-pools/default"); err == nil {
		_ = json.Unmarshal([]byte(body), &pool)
	}
	result := allocationUsage{memoryKnown: memoryScope != "", diskKnown: pool.Driver == "dir"}
	for i := 1; i <= slot.Environment.Count; i++ {
		vm := child.Config.vm(i)
		body, err := rt.incus(ctx, "query", "/1.0/instances/"+vm+"?project="+child.Config.Project)
		if err != nil {
			result.memoryKnown, result.diskKnown = false, false
			continue
		}
		var instance struct {
			Type   string            `json:"type"`
			Config map[string]string `json:"config"`
		}
		if json.Unmarshal([]byte(body), &instance) != nil || instance.Type != "virtual-machine" ||
			instance.Config["user.subyard.managed"] != managedMarker ||
			instance.Config["user.subyard.generation"] != strconv.FormatUint(slot.ResourceGeneration, 10) ||
			instance.Config["user.subyard.lease-epoch"] != strconv.FormatUint(slot.LeaseEpoch, 10) {
			result.memoryKnown, result.diskKnown = false, false
			continue
		}
		if memoryScope != "" {
			body, err = rt.incus(ctx, "query", "/1.0/instances/"+vm+"/state?project="+child.Config.Project)
			if err == nil {
				var state struct {
					Status string `json:"status"`
					PID    int    `json:"pid"`
				}
				if json.Unmarshal([]byte(body), &state) == nil {
					if state.Status == "Stopped" {
						// A stopped VM has no QEMU resident memory.
					} else if state.Status == "Running" && state.PID > 1 {
						if used, ok := qemuAnonymousBytes("/proc", "/sys/fs/cgroup", state.PID, memoryScope); ok && result.memory <= ^uint64(0)-used {
							result.memory += used
						} else {
							result.memoryKnown = false
						}
					} else {
						result.memoryKnown = false
					}
				} else {
					result.memoryKnown = false
				}
			} else {
				result.memoryKnown = false
			}
		}
		if pool.Driver == "dir" {
			// Incus dir stores each VM block image under its own volume path.
			// Count allocated blocks only on ext4, which cannot reflink them
			// into another volume. CoW filesystems cannot safely grant credit
			// from st_blocks because the same extent can be shared.
			volume := filepath.Join("/var/lib/incus/storage-pools/default/virtual-machines", child.Config.Project+"_"+vm)
			var statfs syscall.Statfs_t
			if syscall.Statfs(volume, &statfs) == nil && statfs.Type == 0xef53 {
				if used, ok := exclusiveBlocks(ctx, volume); ok && result.disk <= ^uint64(0)-used {
					result.disk += used
				} else {
					result.diskKnown = false
				}
			} else {
				result.diskKnown = false
			}
		}
	}
	return result
}

func qemuAnonymousBytes(procRoot, cgroupRoot string, pid int, boundary string) (uint64, bool) {
	name := strconv.Itoa(pid)
	body, err := os.ReadFile(filepath.Join(procRoot, name, "cgroup"))
	if err != nil {
		return 0, false
	}
	var relative string
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "0::/") {
			relative = strings.TrimPrefix(line, "0::/")
		}
	}
	if relative == "" || strings.Contains(relative, "..") || strings.Contains(relative, " (deleted)") {
		return 0, false
	}
	group := filepath.Join(cgroupRoot, relative)
	if group == boundary || !strings.HasPrefix(group, boundary+string(os.PathSeparator)) {
		return 0, false
	}
	// A shared service cgroup could contain unrelated processes. Give credit
	// only to a leaf containing this QEMU process alone.
	procs, err := os.ReadFile(filepath.Join(group, "cgroup.procs"))
	if err != nil || strings.TrimSpace(string(procs)) != name {
		return 0, false
	}
	// memory.stat includes descendants. Do not credit an unrelated process
	// hidden in a child cgroup of this QEMU cgroup.
	cgroupStats, err := os.ReadFile(filepath.Join(group, "cgroup.stat"))
	if err != nil {
		return 0, false
	}
	foundDescendants := false
	for _, line := range strings.Split(string(cgroupStats), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "nr_descendants" {
			descendants, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil || descendants != 0 {
				return 0, false
			}
			foundDescendants = true
		}
	}
	if !foundDescendants {
		return 0, false
	}
	stats, err := os.ReadFile(filepath.Join(group, "memory.stat"))
	if err != nil {
		return 0, false
	}
	var anon, shmem uint64
	var foundAnon, foundShmem bool
	for _, line := range strings.Split(string(stats), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, false
		}
		switch fields[0] {
		case "anon":
			anon, foundAnon = value, true
		case "shmem":
			shmem, foundShmem = value, true
		}
	}
	if !foundAnon || !foundShmem || anon > ^uint64(0)-shmem {
		return 0, false
	}
	return anon + shmem, true
}

func exclusiveBlocks(ctx context.Context, root string) (uint64, bool) {
	var blocks uint64
	var device uint64
	seen := map[[2]uint64]bool{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || (path != root && stat.Dev != device) {
			return fs.ErrInvalid
		}
		if path == root {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fs.ErrInvalid
			}
			device = stat.Dev
		}
		if info.Mode().IsRegular() && stat.Nlink == 1 {
			key := [2]uint64{stat.Dev, stat.Ino}
			if !seen[key] {
				seen[key] = true
				used := uint64(stat.Blocks) * 512
				if blocks > ^uint64(0)-used {
					return fs.ErrInvalid
				}
				blocks += used
			}
		}
		return nil
	})
	return blocks, err == nil
}

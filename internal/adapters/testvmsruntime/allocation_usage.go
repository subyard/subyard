package testvmsruntime

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// allocationUsage is confirmed disk usage already subtracted from live free
// space. Missing or ambiguous measurements give no credit to disk admission.
type allocationUsage struct {
	disk      uint64
	diskKnown bool
}

func (rt *Runtime) allocationUsage(ctx context.Context, slot LeaseSlot) allocationUsage {
	if rt.usageProbe != nil {
		return rt.usageProbe(ctx, slot)
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
	result := allocationUsage{diskKnown: pool.Driver == "dir"}
	for i := 1; i <= slot.Environment.Count; i++ {
		vm := child.Config.vm(i)
		body, err := rt.incus(ctx, "query", "/1.0/instances/"+vm+"?project="+child.Config.Project)
		if err != nil {
			result.diskKnown = false
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
			result.diskKnown = false
			continue
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

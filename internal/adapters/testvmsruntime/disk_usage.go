package testvmsruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

// diskBudgetUsage measures the pool-local charge used by the broker's disk
// budget. Physical pool headroom remains separately measured from Incus.
func (rt *Runtime) diskBudgetUsage(ctx context.Context, poolUsed uint64) (uint64, error) {
	rt.prepareDefaults()
	if rt.diskUsageProbe != nil {
		return rt.diskUsageProbe(ctx)
	}
	cache, err := rt.CacheUsage(ctx)
	if err != nil {
		return 0, err
	}
	return rt.combineDiskBudgetUsage(ctx, poolUsed, "/var/lib/incus", cache)
}

func (rt *Runtime) measureDiskBudgetUsage(ctx context.Context, poolUsed uint64, incusRoot string) (uint64, error) {
	cache, err := rt.measureCache(ctx, incusRoot)
	if err != nil {
		return 0, err
	}
	return rt.combineDiskBudgetUsage(ctx, poolUsed, incusRoot, cache)
}

func (rt *Runtime) combineDiskBudgetUsage(ctx context.Context, poolUsed uint64, incusRoot string, cache CacheUsage) (uint64, error) {
	switch cache.Driver {
	case "dir":
		working, err := measureDirVMUsage(ctx, incusRoot)
		if err != nil {
			return 0, err
		}
		return addCacheBytes(cache.ChargedBytes, working)
	case "btrfs", "zfs":
		// CoW block ownership cannot safely be derived from directory blocks.
		// Keep the established whole-pool quota until a driver-native scoped
		// measurement is available.
		return poolUsed, nil
	default:
		return 0, errors.New("unsupported disk budget storage driver")
	}
}

func measureDirVMUsage(ctx context.Context, incusRoot string) (uint64, error) {
	poolRoot := filepath.Join(incusRoot, "storage-pools", "default")
	if err := rejectSymlinkPath(poolRoot); err != nil {
		return 0, err
	}
	seen := map[cacheInode]bool{}
	var usage uint64
	for _, name := range []string{"virtual-machines", "virtual-machines-snapshots"} {
		root := filepath.Join(poolRoot, name)
		value, err := optionalCacheBlocks(ctx, root, seen)
		if err != nil {
			return 0, err
		}
		usage, err = addCacheBytes(usage, value)
		if err != nil {
			return 0, err
		}
	}
	return usage, nil
}

func optionalCacheBlocks(ctx context.Context, root string, seen map[cacheInode]bool) (uint64, error) {
	// The containing pool path must exist and be safe before a missing optional
	// child is treated as empty.
	if err := rejectSymlinkPath(filepath.Dir(root)); err != nil {
		return 0, err
	}
	info, err := os.Lstat(root)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return 0, errors.New("invalid virtual machine storage root")
	}
	return cacheBlocks(ctx, root, seen)
}

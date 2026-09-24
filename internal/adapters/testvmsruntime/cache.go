package testvmsruntime

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// CacheUsage includes the daemon's complete archive cache, including upstream
// and foreign images that the broker has no authority to delete. Charge bytes
// are an admission bound, not an assertion of exclusive CoW physical usage.
type CacheUsage struct {
	Driver             string  `json:"driver"`
	ArchiveChargeBytes uint64  `json:"archive_charge_bytes"`
	NativeChargeBytes  uint64  `json:"native_cache_charge_bytes"`
	ChargedBytes       uint64  `json:"charged_bytes"`
	Accounting         string  `json:"accounting"`
	PhysicalBytes      *uint64 `json:"physical_bytes,omitempty"`
}

func (runtime *Runtime) CacheUsage(ctx context.Context) (CacheUsage, error) {
	runtime.prepareDefaults()
	if runtime.cacheProbe != nil {
		return runtime.cacheProbe(ctx)
	}
	return runtime.measureCache(ctx, "/var/lib/incus")
}

func (runtime *Runtime) checkCacheBudget(ctx context.Context) error {
	usage, err := runtime.CacheUsage(ctx)
	if err != nil {
		return &CapacityError{Resource: "disk", Reason: "image cache telemetry unavailable"}
	}
	if usage.ChargedBytes > budgetBytes(runtime.Config.CacheBudget, "24GiB") {
		return &CapacityError{Resource: "disk", Reason: "image cache budget exceeded"}
	}
	return nil
}

func (runtime *Runtime) measureCache(ctx context.Context, incusRoot string) (CacheUsage, error) {
	var usage CacheUsage
	body, err := runtime.incus(ctx, "query", "/1.0/storage-pools/default")
	if err != nil {
		return usage, err
	}
	var pool struct {
		Driver string `json:"driver"`
	}
	if err := json.Unmarshal([]byte(body), &pool); err != nil {
		return usage, err
	}
	usage.Driver = pool.Driver
	switch pool.Driver {
	case "dir", "btrfs", "zfs":
	default:
		return usage, errors.New("unsupported image cache storage driver")
	}
	seen := map[cacheInode]bool{}
	archives := filepath.Join(incusRoot, "images")
	usage.ArchiveChargeBytes, err = cacheBlocks(ctx, archives, seen)
	if err != nil {
		return usage, errors.New("image archive accounting unavailable")
	}
	physical := exclusiveBlockFilesystem(archives)
	if pool.Driver == "dir" {
		native := filepath.Join(incusRoot, "storage-pools", "default", "images")
		if err := rejectSymlinkPath(native); err != nil && !os.IsNotExist(err) {
			return usage, errors.New("unsafe native image cache path")
		}
		if _, err := os.Lstat(native); err == nil {
			usage.NativeChargeBytes, err = cacheBlocks(ctx, native, seen)
			if err != nil {
				return usage, errors.New("native image cache accounting unavailable")
			}
			physical = physical && exclusiveBlockFilesystem(native)
		} else if !os.IsNotExist(err) {
			return usage, err
		}
	} else {
		physical = false
		usage.NativeChargeBytes, err = runtime.nativeImageCacheCharge(ctx)
		if err != nil {
			return usage, err
		}
	}
	usage.ChargedBytes, err = addCacheBytes(usage.ArchiveChargeBytes, usage.NativeChargeBytes)
	if err != nil {
		return usage, err
	}
	usage.Accounting = "conservative-shared-inclusive"
	if physical {
		usage.Accounting = "physical-blocks"
		value := usage.ChargedBytes
		usage.PhysicalBytes = &value
	}
	return usage, nil
}

func (runtime *Runtime) nativeImageCacheCharge(ctx context.Context) (uint64, error) {
	body, err := runtime.incus(ctx, "query", "/1.0/storage-pools/default/volumes?recursion=1&all-projects=true")
	if err != nil {
		return 0, errors.New("native image volume inventory unavailable")
	}
	var volumes []struct {
		Type    string `json:"type"`
		Name    string `json:"name"`
		Project string `json:"project"`
	}
	if err := json.Unmarshal([]byte(body), &volumes); err != nil || volumes == nil {
		return 0, errors.New("invalid native image volume inventory")
	}
	var charged uint64
	seen := map[string]bool{}
	for _, volume := range volumes {
		if volume.Type != "image" {
			continue
		}
		if !imageFingerprint.MatchString(volume.Name) {
			return 0, errors.New("invalid native image volume name")
		}
		project := volume.Project
		if project == "" {
			project = "default"
		}
		identity := project + "\x00" + volume.Name
		if seen[identity] {
			return 0, errors.New("duplicate native image volume")
		}
		seen[identity] = true
		path := "/1.0/storage-pools/default/volumes/image/" + url.PathEscape(volume.Name) + "/state?project=" + url.QueryEscape(project)
		body, err := runtime.incus(ctx, "query", path)
		if err != nil {
			return 0, errors.New("native image volume usage unavailable")
		}
		var state struct {
			Usage *struct {
				Used *uint64 `json:"used"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(body), &state); err != nil || state.Usage == nil || state.Usage.Used == nil {
			return 0, errors.New("invalid native image volume usage")
		}
		charged, err = addCacheBytes(charged, *state.Usage.Used)
		if err != nil {
			return 0, err
		}
	}
	return charged, nil
}

type cacheInode struct{ device, inode uint64 }

func cacheBlocks(ctx context.Context, root string, seen map[cacheInode]bool) (uint64, error) {
	return allocatedBlocks(ctx, root, seen, false)
}

func allocatedBlocks(ctx context.Context, root string, seen map[cacheInode]bool, allowVMMetadata bool) (uint64, error) {
	if err := rejectSymlinkPath(root); err != nil {
		return 0, err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return 0, errors.New("cache root is not a directory")
	}
	var total uint64
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := info.Mode()
		if !mode.IsDir() && !mode.IsRegular() && (!allowVMMetadata || mode&(os.ModeSymlink|os.ModeSocket) == 0) {
			if allowVMMetadata {
				return errors.New("unsupported virtual machine storage entry")
			}
			return errors.New("unsupported cache entry")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Blocks < 0 {
			return errors.New("cache block accounting unavailable")
		}
		identity := cacheInode{uint64(stat.Dev), uint64(stat.Ino)}
		if seen[identity] {
			return nil
		}
		seen[identity] = true
		if uint64(stat.Blocks) > ^uint64(0)/512 {
			return errors.New("cache block count overflow")
		}
		total, err = addCacheBytes(total, uint64(stat.Blocks)*512)
		return err
	})
	return total, err
}

func rejectSymlinkPath(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("cache path must be absolute")
	}
	current := string(filepath.Separator)
	for _, part := range strings.Split(filepath.Clean(path), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("cache path contains a symlink")
		}
	}
	return nil
}

func exclusiveBlockFilesystem(path string) bool {
	var stats syscall.Statfs_t
	if syscall.Statfs(path, &stats) != nil {
		return false
	}
	// Only the non-reflink ext4 block sum is called physical. Other backing
	// filesystems retain the conservative shared-inclusive accounting label.
	return uint64(stats.Type) == 0xef53
}

func addCacheBytes(left, right uint64) (uint64, error) {
	if right > ^uint64(0)-left {
		return 0, errors.New("cache size overflow")
	}
	return left + right, nil
}

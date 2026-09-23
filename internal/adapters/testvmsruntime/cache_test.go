package testvmsruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestCacheBlocksCountsSparseAllocationOnceAcrossHardlinks(t *testing.T) {
	root := t.TempDir()
	file, err := os.Create(filepath.Join(root, "unknown-upstream.rootfs"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(8 << 20); err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("x"), (8<<20)-1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, "unknown-upstream.rootfs"), filepath.Join(root, "alias.rootfs")); err != nil {
		t.Fatal(err)
	}
	blocks := func(path string) uint64 {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		return uint64(info.Sys().(*syscall.Stat_t).Blocks) * 512
	}
	got, err := cacheBlocks(context.Background(), root, map[cacheInode]bool{})
	if err != nil {
		t.Fatal(err)
	}
	want := blocks(root) + blocks(filepath.Join(root, "unknown-upstream.rootfs"))
	if got != want || got >= 8<<20 {
		t.Fatalf("sparse/hardlink accounting=%d want=%d", got, want)
	}
}

func TestCacheBlocksRejectsSymlinksAndMissingRoots(t *testing.T) {
	root := t.TempDir()
	actual := filepath.Join(root, "actual")
	if err := os.Mkdir(actual, 0700); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(root, "linked")
	if err := os.Symlink(actual, linked); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(actual, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{linked, filepath.Join(linked, "child"), filepath.Join(root, "missing")} {
		if _, err := cacheBlocks(context.Background(), path, map[cacheInode]bool{}); err == nil {
			t.Fatalf("accepted unsafe/missing path %s", path)
		}
	}
	if err := os.Symlink(filepath.Join(root, "outside"), filepath.Join(actual, "foreign-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := cacheBlocks(context.Background(), actual, map[cacheInode]bool{}); err == nil {
		t.Fatal("followed cache symlink")
	}
}

func TestCacheAccountingIncludesUnknownArchivesAndNativeDriverUsage(t *testing.T) {
	for _, driver := range []string{"dir", "btrfs", "zfs", "lvm"} {
		t.Run(driver, func(t *testing.T) {
			root := t.TempDir()
			archives := filepath.Join(root, "images")
			if err := os.Mkdir(archives, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(archives, "foreign-upstream.rootfs"), make([]byte, 8192), 0600); err != nil {
				t.Fatal(err)
			}
			nativeCalls := 0
			runtime := Runtime{Config: fixtureConfig(t), Runner: &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
				command := strings.Join(args, " ")
				switch command {
				case "query /1.0/storage-pools/default":
					return []byte(`{"driver":"` + driver + `"}`), nil, nil
				case "query /1.0/storage-pools/default/volumes?recursion=1&all-projects=true":
					return []byte(`[{"type":"image","name":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","project":"default"},{"type":"virtual-machine","name":"working"}]`), nil, nil
				case "query /1.0/storage-pools/default/volumes/image/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/state?project=default":
					nativeCalls++
					return []byte(`{"usage":{"used":4096}}`), nil, nil
				}
				return nil, nil, fmt.Errorf("unexpected call %s", command)
			}}}
			usage, err := runtime.measureCache(context.Background(), root)
			if driver == "lvm" {
				if err == nil {
					t.Fatal("unsupported driver accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if usage.ArchiveChargeBytes < 8192 || usage.ChargedBytes != usage.ArchiveChargeBytes+usage.NativeChargeBytes {
				t.Fatalf("unknown upstream archive omitted: %#v", usage)
			}
			if driver == "dir" {
				if nativeCalls != 0 || usage.NativeChargeBytes != 0 {
					t.Fatalf("absent dir native cache charged: %#v", usage)
				}
			} else {
				if nativeCalls != 1 || usage.NativeChargeBytes != 4096 || usage.PhysicalBytes != nil || usage.Accounting != "conservative-shared-inclusive" {
					t.Fatalf("shared blocks reported as physical: %#v", usage)
				}
			}
		})
	}
}

func TestCacheMeasurementFailsClosedAndBudgetIncludesNativeCache(t *testing.T) {
	root := t.TempDir()
	runtime := Runtime{Config: fixtureConfig(t), Runner: &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
		switch strings.Join(args, " ") {
		case "query /1.0/storage-pools/default":
			return []byte(`{"driver":"zfs"}`), nil, nil
		case "query /1.0/storage-pools/default/volumes?recursion=1&all-projects=true":
			return []byte(`[{"type":"image","name":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`), nil, nil
		default:
			return []byte(`{"usage":{}}`), nil, nil
		}
	}}}
	if _, err := runtime.measureCache(context.Background(), root); err == nil {
		t.Fatal("missing archive tree accepted")
	}
	if err := os.Mkdir(filepath.Join(root, "images"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.measureCache(context.Background(), root); err == nil {
		t.Fatal("missing native usage accepted")
	}
	runtime.Config.CacheBudget = "1MiB"
	runtime.cacheProbe = func(context.Context) (CacheUsage, error) {
		return CacheUsage{ArchiveChargeBytes: 1, NativeChargeBytes: 2 << 20, ChargedBytes: (2 << 20) + 1}, nil
	}
	var capacity *CapacityError
	if err := runtime.checkCacheBudget(context.Background()); !errors.As(err, &capacity) || capacity.Resource != "disk" {
		t.Fatalf("native cache escaped budget: %v", err)
	}
	runtime.cacheProbe = func(context.Context) (CacheUsage, error) { return CacheUsage{}, errors.New("unknown measurement") }
	if err := runtime.checkCacheBudget(context.Background()); !errors.As(err, &capacity) {
		t.Fatalf("unknown cache usage allowed: %v", err)
	}
}

func TestDirCacheCountsMaterializedNativeCacheWithoutHardlinkDoubleCharge(t *testing.T) {
	root := t.TempDir()
	archives := filepath.Join(root, "images")
	native := filepath.Join(root, "storage-pools", "default", "images")
	if err := os.MkdirAll(archives, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(native, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(archives, "upstream"), make([]byte, 8192), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(archives, "upstream"), filepath.Join(native, "same-image")); err != nil {
		t.Fatal(err)
	}
	runtime := Runtime{Config: fixtureConfig(t), Runner: &fakeRunner{handler: func(_ string, _ []string, _ []string, _ io.Reader) ([]byte, []byte, error) {
		return []byte(`{"driver":"dir"}`), nil, nil
	}}}
	usage, err := runtime.measureCache(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(native)
	if err != nil {
		t.Fatal(err)
	}
	if usage.NativeChargeBytes != uint64(info.Sys().(*syscall.Stat_t).Blocks)*512 {
		t.Fatalf("hardlinked native cache counted twice: %#v", usage)
	}
}

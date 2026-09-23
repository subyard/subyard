package testvmsruntime

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func diskUsageRuntime(t *testing.T, driver string) *Runtime {
	t.Helper()
	return &Runtime{Config: fixtureConfig(t), Runner: &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
		switch strings.Join(args, " ") {
		case "query /1.0/storage-pools/default":
			return []byte(`{"driver":"` + driver + `"}`), nil, nil
		case "query /1.0/storage-pools/default/volumes?recursion=1&all-projects=true":
			return []byte(`[]`), nil, nil
		default:
			return nil, nil, errors.New("unexpected Incus query")
		}
	}}}
}

func diskUsageRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, path := range []string{
		filepath.Join(root, "images"),
		filepath.Join(root, "storage-pools", "default"),
	} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestDiskBudgetUsageDirCountsOnlyCacheAndRetainedVMRoots(t *testing.T) {
	root := diskUsageRoot(t)
	cache := filepath.Join(root, "images", "foreign-image")
	vm := filepath.Join(root, "storage-pools", "default", "virtual-machines", "retained", "disk")
	snapshot := filepath.Join(root, "storage-pools", "default", "virtual-machines-snapshots", "retained", "snap", "disk")
	for _, path := range []string{filepath.Dir(vm), filepath.Dir(snapshot)} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(cache, make([]byte, 8192), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vm, make([]byte, 8192), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshot, make([]byte, 8192), 0600); err != nil {
		t.Fatal(err)
	}
	rt := diskUsageRuntime(t, "dir")
	got, err := rt.measureDiskBudgetUsage(context.Background(), 999<<30, root)
	if err != nil {
		t.Fatal(err)
	}
	cacheUsage, err := rt.measureCache(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	vmUsage, err := measureDirVMUsage(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	want, err := addCacheBytes(cacheUsage.ChargedBytes, vmUsage)
	if err != nil {
		t.Fatal(err)
	}
	if got != want || vmUsage == 0 {
		t.Fatalf("disk budget usage=%d want=%d vm=%d", got, want, vmUsage)
	}
	if err := os.WriteFile(filepath.Join(root, "unrelated-host-file"), make([]byte, 1<<20), 0600); err != nil {
		t.Fatal(err)
	}
	again, err := rt.measureDiskBudgetUsage(context.Background(), 999<<30, root)
	if err != nil || again != got {
		t.Fatalf("unrelated file changed scoped usage: %d %v", again, err)
	}
}

func TestMeasureDirVMUsageAllowsAbsentRootsOnlyBelowSafePool(t *testing.T) {
	root := diskUsageRoot(t)
	got, err := measureDirVMUsage(context.Background(), root)
	if err != nil || got != 0 {
		t.Fatalf("absent roots usage=%d err=%v", got, err)
	}
	if _, err := measureDirVMUsage(context.Background(), filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing pool parent accepted")
	}
}

func TestMeasureDirVMUsageRejectsUnsafeRoots(t *testing.T) {
	root := diskUsageRoot(t)
	pool := filepath.Join(root, "storage-pools", "default")
	if err := os.Symlink(filepath.Join(root, "outside"), filepath.Join(pool, "virtual-machines")); err != nil {
		t.Fatal(err)
	}
	if _, err := measureDirVMUsage(context.Background(), root); err == nil {
		t.Fatal("symlinked VM root accepted")
	}
}

func TestDiskBudgetUsageFailsClosedAndPreservesCoWFallback(t *testing.T) {
	root := diskUsageRoot(t)
	if _, err := diskUsageRuntime(t, "lvm").measureDiskBudgetUsage(context.Background(), 17, root); err == nil {
		t.Fatal("unknown driver accepted")
	}
	if got, err := diskUsageRuntime(t, "btrfs").measureDiskBudgetUsage(context.Background(), 17, root); err != nil || got != 17 {
		t.Fatalf("btrfs quota=%d err=%v", got, err)
	}
	if got, err := diskUsageRuntime(t, "zfs").measureDiskBudgetUsage(context.Background(), 23, root); err != nil || got != 23 {
		t.Fatalf("zfs quota=%d err=%v", got, err)
	}
	broken := Runtime{Config: fixtureConfig(t), Runner: &fakeRunner{handler: func(_ string, _ []string, _ []string, _ io.Reader) ([]byte, []byte, error) {
		return nil, nil, errors.New("telemetry unavailable")
	}}}
	if _, err := broken.measureDiskBudgetUsage(context.Background(), 0, root); err == nil {
		t.Fatal("cache telemetry failure accepted")
	}
	if _, err := addCacheBytes(^uint64(0), 1); err == nil {
		t.Fatal("disk budget addition overflow accepted")
	}
}

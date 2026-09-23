package testvmsruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func memoryFixture(t *testing.T) (string, string, func(string, string)) {
	t.Helper()
	root := t.TempDir()
	proc, group := filepath.Join(root, "proc"), filepath.Join(root, "cgroup")
	write := func(path, value string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(proc, "meminfo"), "MemAvailable: 16384 kB\n")
	write(filepath.Join(proc, "self/cgroup"), "0::/\n")
	write(filepath.Join(group, "memory.max"), "8388608\n")
	write(filepath.Join(group, "memory.current"), "2097152\n")
	return proc, group, write
}

func TestMemoryCapacityUsesTightestVisibleBoundary(t *testing.T) {
	proc, group, write := memoryFixture(t)
	write(filepath.Join(proc, "self/cgroup"), "0::/service/worker\n")
	write(filepath.Join(group, "service/memory.max"), "4194304\n")
	write(filepath.Join(group, "service/memory.current"), "3145728\n")
	write(filepath.Join(group, "service/worker/memory.max"), "max\n")
	write(filepath.Join(group, "service/worker/memory.current"), "1048576\n")
	write(filepath.Join(group, "memory.events"), "oom 3\noom_kill 2\n")
	write(filepath.Join(group, "service/memory.events"), "oom 2\noom_kill 1\n")
	got, err := memoryCapacity(proc, group)
	if err != nil {
		t.Fatal(err)
	}
	if got.Available != 1048576 || got.Limit != 4194304 || got.Current != 3145728 {
		t.Fatalf("ancestor ignored: %+v", got)
	}
	if !got.EventsAvailable || got.OOM != 2 || got.OOMKills != 1 || got.PeakAvailable || got.OuterHostEvidence == "" {
		t.Fatalf("missing telemetry hidden: %+v", got)
	}
}

func TestMemoryCapacityExhaustionAndMissingTelemetry(t *testing.T) {
	proc, group, write := memoryFixture(t)
	write(filepath.Join(group, "memory.current"), "9437184\n")
	got, err := memoryCapacity(proc, group)
	if err != nil || got.Available != 0 {
		t.Fatalf("over limit: %+v %v", got, err)
	}
	if err := os.Remove(filepath.Join(group, "memory.current")); err != nil {
		t.Fatal(err)
	}
	if _, err := memoryCapacity(proc, group); err == nil {
		t.Fatal("accepted unavailable telemetry")
	}
}

func TestMemoryCapacityRealRootWithoutLimit(t *testing.T) {
	proc, group, write := memoryFixture(t)
	if err := os.Remove(filepath.Join(group, "memory.max")); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(group, "cgroup.controllers"), "cpu memory io\n")
	write(filepath.Join(group, "memory.peak"), "12345\n")
	got, err := memoryCapacity(proc, group)
	if err != nil || got.Available != 16384*1024 || got.LimitAvailable || got.PeakAvailable {
		t.Fatalf("real root: %+v %v", got, err)
	}
	write(filepath.Join(proc, "self/cgroup"), "0::/../../outside\n")
	if _, err := memoryCapacity(proc, group); err == nil {
		t.Fatal("accepted escaping membership")
	}
}

func TestCapacityAdmissionKeepsReservesAndRejectsOverflow(t *testing.T) {
	for _, test := range []struct {
		name                      string
		memory                    MemoryCapacity
		storage                   StorageCapacity
		ram, disk, mr, dr, budget uint64
		resource                  string
	}{
		{"exact", MemoryCapacity{Available: 10}, StorageCapacity{Total: 30, Used: 10, BudgetUsed: 10}, 8, 15, 2, 5, 25, ""},
		{"memory reserve", MemoryCapacity{Available: 10}, StorageCapacity{Total: 30, Used: 10, BudgetUsed: 10}, 9, 15, 2, 5, 25, "memory"},
		{"filesystem reserve", MemoryCapacity{Available: 10}, StorageCapacity{Total: 30, Used: 10, BudgetUsed: 10}, 8, 16, 2, 5, 30, "disk"},
		{"budget", MemoryCapacity{Available: 10}, StorageCapacity{Total: 30, Used: 10, BudgetUsed: 10}, 8, 15, 2, 5, 24, "disk"},
		{"overflow", MemoryCapacity{Available: 10}, StorageCapacity{Total: 30, Used: 10, BudgetUsed: 10}, ^uint64(0), 15, 2, 5, 25, "memory"},
		{"unrelated backing usage", MemoryCapacity{Available: 10}, StorageCapacity{Total: 472, Used: 125, BudgetUsed: 1}, 8, 40, 2, 5, 160, ""},
		{"physical headroom still required", MemoryCapacity{Available: 10}, StorageCapacity{Total: 169, Used: 125, BudgetUsed: 1}, 8, 40, 2, 5, 160, "disk"},
		{"budget already exceeded", MemoryCapacity{Available: 10}, StorageCapacity{Total: 472, Used: 125, BudgetUsed: 161}, 8, 0, 2, 5, 160, "disk"},
		{"disk overflow", MemoryCapacity{Available: 10}, StorageCapacity{Total: ^uint64(0), BudgetUsed: 1}, 8, ^uint64(0), 2, 5, ^uint64(0), "disk"},
		{"invalid storage", MemoryCapacity{Available: 10}, StorageCapacity{Total: 9, Used: 10}, 8, 0, 2, 0, 25, "disk"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := checkCapacity(test.memory, test.storage, test.ram, test.disk, test.mr, test.dr, test.budget)
			if test.resource == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var capacity *CapacityError
			if !errors.As(err, &capacity) || capacity.Resource != test.resource {
				t.Fatalf("wrong rejection: %v", err)
			}
		})
	}
}

func TestSizeRejectsOverflow(t *testing.T) {
	for _, value := range []string{"99999999999999999999999GiB", "1073741825MiB", "0MiB"} {
		if _, err := sizeMiB(value); err == nil {
			t.Fatalf("accepted %s", value)
		}
	}
}

func TestConcurrentMixedAdmissionAndRetryAfterRelease(t *testing.T) {
	store := LeaseStore{Path: filepath.Join(t.TempDir(), "leases.json"), SlotCount: 2}
	cfg := fixtureConfig(t)
	cfg.Memory, cfg.Disk = "4GiB", "20GiB"
	grants := make([]LeaseGrant, 2)
	for i, name := range []string{EnvironmentPair, EnvironmentAndroid} {
		spec, err := cfg.environmentSpecForArch(name, "amd64")
		if err != nil {
			t.Fatal(err)
		}
		grants[i], err = store.AcquireV3Slot(spec, "client", "SHA256:key", "yard", "Project", fmt.Sprint(i), "capacity", fmt.Sprintf("slot-%03d", i+1))
		if err != nil {
			t.Fatal(err)
		}
	}
	runner := &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
		if strings.Join(args, " ") == "query /1.0/storage-pools/default/resources" {
			return []byte(`{"space":{"total":536870912000,"used":10737418240}}`), nil, nil
		}
		return nil, nil, fmt.Errorf("unexpected mutation: %v", args)
	}}
	rt := &Runtime{Config: cfg, Runner: runner, diskUsageProbe: func(context.Context) (uint64, error) { return 10 << 30, nil }, memoryProbe: func() (MemoryCapacity, error) { return MemoryCapacity{Available: 11 << 30}, nil }}
	var group sync.WaitGroup
	results := make([]error, 2)
	for i := range grants {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			results[i] = rt.reserveEnvironment(context.Background(), store, grants[i])
		}(i)
	}
	group.Wait()
	winner, loser := -1, -1
	for i, err := range results {
		if err == nil {
			if winner != -1 {
				t.Fatal("both allocations admitted beyond capacity")
			}
			winner = i
			continue
		}
		var capacity *CapacityError
		if !errors.As(err, &capacity) || capacity.Resource != "memory" {
			t.Fatalf("unexpected rejection: %v", err)
		}
		loser = i
	}
	if winner < 0 || loser < 0 {
		t.Fatalf("no single winner: %v", results)
	}
	if err := store.AbortProvisioning(grants[loser]); err != nil {
		t.Fatal(err)
	}
	// Confirmed cleanup releases the winner; the same untouched slot can retry.
	if err := store.BeginDrain(grants[winner]); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishDrain(grants[winner].SlotID, nil); err != nil {
		t.Fatal(err)
	}
	retried, err := store.AcquireV3Slot(*grants[loser].Environment, "client", "SHA256:key", "yard", "Project", "retry", "capacity", grants[loser].SlotID)
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.reserveEnvironment(context.Background(), store, retried); err != nil {
		t.Fatalf("capacity not reusable: %v", err)
	}
	if err := rt.reserveEnvironment(context.Background(), store, grants[loser]); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale grant accepted: %v", err)
	}
}

func TestAdmissionCreditsOnlyConfirmedExistingAllocation(t *testing.T) {
	store := LeaseStore{Path: filepath.Join(t.TempDir(), "leases.json"), SlotCount: 2}
	cfg := fixtureConfig(t)
	cfg.Memory, cfg.Disk = "4GiB", "20GiB"
	for i, name := range []string{EnvironmentPair, EnvironmentAndroid} {
		spec, err := cfg.environmentSpecForArch(name, "amd64")
		if err != nil {
			t.Fatal(err)
		}
		grant, err := store.AcquireV3Slot(spec, "client", "SHA256:key", "yard", "Project", fmt.Sprint(i), "capacity", fmt.Sprintf("slot-%03d", i+1))
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if err := store.withLock(true, func(pool *LeasePool) error {
				pool.Slots[0].Reserved = true
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.MarkHeld(grant); err != nil {
				t.Fatal(err)
			}
		} else {
			runner := &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
				if strings.Join(args, " ") == "query /1.0/storage-pools/default/resources" {
					return []byte(`{"space":{"total":107374182400,"used":42949672960}}`), nil, nil
				}
				return nil, nil, fmt.Errorf("unexpected command: %v", args)
			}}
			rt := &Runtime{Config: cfg, Runner: runner, diskUsageProbe: func(context.Context) (uint64, error) { return 40 << 30, nil }, memoryProbe: func() (MemoryCapacity, error) {
				return MemoryCapacity{Available: 16 << 30}, nil
			}}
			var capacity *CapacityError
			if err := rt.reserveEnvironment(context.Background(), store, grant); !errors.As(err, &capacity) || capacity.Resource != "memory" {
				t.Fatalf("missing usage credit did not reject conservatively: %v", err)
			}
			rt.usageProbe = func(_ context.Context, slot LeaseSlot, _ string) allocationUsage {
				if slot.SlotID == "slot-001" {
					return allocationUsage{memory: 7 << 30, disk: 30 << 30}
				}
				return allocationUsage{}
			}
			if err := rt.reserveEnvironment(context.Background(), store, grant); err != nil {
				t.Fatalf("confirmed usage was counted twice: %v", err)
			}
		}
	}
}

func TestAdmissionBoundsFreeSpaceWhenGuestReleasesCreditedUsage(t *testing.T) {
	for _, test := range []struct {
		name, resource string
		builder        bool
		memory         [2]uint64
		storageUsed    [2]uint64
		budgetUsed     [2]uint64
	}{
		{"reserve memory", "memory", false, [2]uint64{11 << 30, 18 << 30}, [2]uint64{20 << 30, 20 << 30}, [2]uint64{20 << 30, 20 << 30}},
		{"reserve disk", "disk", false, [2]uint64{30 << 30, 30 << 30}, [2]uint64{50 << 30, 20 << 30}, [2]uint64{20 << 30, 20 << 30}},
		{"builder memory", "memory", true, [2]uint64{11 << 30, 18 << 30}, [2]uint64{20 << 30, 20 << 30}, [2]uint64{20 << 30, 20 << 30}},
		{"builder disk", "disk", true, [2]uint64{30 << 30, 30 << 30}, [2]uint64{50 << 30, 20 << 30}, [2]uint64{20 << 30, 20 << 30}},
		{"reserve budget", "disk", false, [2]uint64{30 << 30, 30 << 30}, [2]uint64{20 << 30, 20 << 30}, [2]uint64{115 << 30, 85 << 30}},
		{"builder budget", "disk", true, [2]uint64{30 << 30, 30 << 30}, [2]uint64{20 << 30, 20 << 30}, [2]uint64{115 << 30, 85 << 30}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := fixtureConfig(t)
			cfg.Memory, cfg.Disk = "4GiB", "20GiB"
			store := LeaseStore{Path: filepath.Join(t.TempDir(), "leases.json"), SlotCount: 2}
			pair, _ := cfg.EnvironmentSpec(EnvironmentPair)
			first, err := store.AcquireV3Slot(pair, "client", "SHA256:key", "yard", "Project", "first", "capacity", "slot-001")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.mutateOwned(first, func(slot *LeaseSlot, _ time.Time) error { slot.Reserved = true; return nil }); err != nil {
				t.Fatal(err)
			}
			if _, err := store.MarkHeld(first); err != nil {
				t.Fatal(err)
			}
			var next LeaseGrant
			if !test.builder {
				android, _ := cfg.environmentSpecForArch(EnvironmentAndroid, "amd64")
				next, err = store.AcquireV3Slot(android, "client", "SHA256:key", "yard", "Project", "next", "capacity", "slot-002")
				if err != nil {
					t.Fatal(err)
				}
			}
			memoryReads, storageReads := 0, 0
			rt := &Runtime{Config: cfg, diskUsageProbe: func(context.Context) (uint64, error) { return test.budgetUsed[min(storageReads-1, 1)], nil }, cacheProbe: func(context.Context) (CacheUsage, error) { return CacheUsage{}, nil },
				memoryProbe: func() (MemoryCapacity, error) {
					index := min(memoryReads, 1)
					memoryReads++
					return MemoryCapacity{Available: test.memory[index]}, nil
				},
				usageProbe: func(_ context.Context, slot LeaseSlot, _ string) allocationUsage {
					if slot.SlotID == first.SlotID {
						return allocationUsage{memory: 7 << 30, disk: 30 << 30}
					}
					return allocationUsage{}
				},
				Runner: &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
					if strings.Join(args, " ") != "query /1.0/storage-pools/default/resources" {
						return nil, nil, fmt.Errorf("unexpected command: %v", args)
					}
					index := min(storageReads, 1)
					storageReads++
					return []byte(fmt.Sprintf(`{"space":{"total":%d,"used":%d}}`, uint64(100)<<30, test.storageUsed[index])), nil, nil
				}},
			}
			if test.builder {
				err = rt.admitBuild(context.Background(), store, LeaseGrant{}, 9<<30, 40<<30, &ImageRegistry{})
			} else {
				err = rt.reserveEnvironment(context.Background(), store, next)
			}
			var capacity *CapacityError
			if !errors.As(err, &capacity) || capacity.Resource != test.resource {
				t.Fatalf("released usage counted twice: %v", err)
			}
			if memoryReads != 2 || storageReads != 2 {
				t.Fatalf("capacity was not bracketed around usage: memory %d storage %d", memoryReads, storageReads)
			}
		})
	}
}

func TestPhysicalUsageRequiresDedicatedCgroupAndAllocatedBlocks(t *testing.T) {
	proc, group, write := memoryFixture(t)
	write(filepath.Join(proc, "123/cgroup"), "0::/vm/one\n")
	write(filepath.Join(group, "vm/one/cgroup.procs"), "123\n")
	write(filepath.Join(group, "vm/one/cgroup.stat"), "nr_descendants 0\n")
	write(filepath.Join(group, "vm/one/memory.stat"), "anon 1048576\nshmem 524288\nfile 99999999\n")
	if used, ok := qemuAnonymousBytes(proc, group, 123, group); !ok || used != 1572864 {
		t.Fatalf("dedicated cgroup: %d %v", used, ok)
	}
	write(filepath.Join(group, "vm/one/cgroup.procs"), "123\n456\n")
	if _, ok := qemuAnonymousBytes(proc, group, 123, group); ok {
		t.Fatal("credited shared cgroup")
	}
	write(filepath.Join(group, "vm/one/cgroup.procs"), "123\n")
	write(filepath.Join(group, "vm/one/cgroup.stat"), "nr_descendants 1\n")
	if _, ok := qemuAnonymousBytes(proc, group, 123, group); ok {
		t.Fatal("credited child cgroup usage")
	}
	volume := filepath.Join(t.TempDir(), "volume")
	if err := os.Mkdir(volume, 0700); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(volume, "root.img"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if used, ok := exclusiveBlocks(context.Background(), volume); !ok || used == 0 || used > 4096 {
		t.Fatalf("allocated root blocks: %d %v", used, ok)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := exclusiveBlocks(canceled, volume); ok {
		t.Fatal("ignored usage scan deadline")
	}
}

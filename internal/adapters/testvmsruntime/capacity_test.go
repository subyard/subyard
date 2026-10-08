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

	"github.com/Subyard/Subyard/internal/testkit"
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

func TestMemoryCapacityDistinguishesZeroFromInvalidAvailable(t *testing.T) {
	proc, group, write := memoryFixture(t)
	for _, available := range []string{"MemAvailable: 0 kB\n", "MemAvailable: 0 kB\nSwapFree: 1000000 kB\n"} {
		write(filepath.Join(proc, "meminfo"), available)
		value, err := memoryCapacity(proc, group)
		if err != nil || value.Available != 0 || value.VisibleAvailable != 0 {
			t.Fatalf("exhaustion misreported as missing or swap credited: %+v %v", value, err)
		}
	}
	for _, invalid := range []string{"MemFree: 1 kB\n", "MemAvailable: 1 MB\n", "MemAvailable: 1 kB\nMemAvailable: 2 kB\n"} {
		write(filepath.Join(proc, "meminfo"), invalid)
		if _, err := memoryCapacity(proc, group); err == nil {
			t.Fatalf("accepted malformed availability: %q", invalid)
		}
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
		{"unlimited quota", MemoryCapacity{Available: 10}, StorageCapacity{Total: 472, Used: 200, BudgetUsed: 200}, 8, 60, 2, 5, 0, ""},
		{"unlimited quota keeps disk reserve", MemoryCapacity{Available: 10}, StorageCapacity{Total: 264, Used: 200, BudgetUsed: 200}, 8, 60, 2, 5, 0, "disk"},
		{"unlimited quota keeps memory reserve", MemoryCapacity{Available: 9}, StorageCapacity{Total: 472, Used: 200, BudgetUsed: 200}, 8, 60, 2, 5, 0, "memory"},
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

func TestMemoryReserveAppliesToWorkingVMsAndSequentialBuilder(t *testing.T) {
	for _, builder := range []bool{false, true} {
		for _, test := range []struct {
			name, reserve string
			count         int
			available     uint64
			refused       bool
		}{
			{"available headroom", "", 2, 18 << 30, false},
			{"exact default reserve", "", 2, 13 << 30, false},
			{"below default reserve", "", 2, (13 << 30) - 1, true},
			{"insufficient pair headroom", "", 2, 11 << 30, true},
			{"explicit override", "2GiB", 2, 11 << 30, false},
			{"higher explicit override", "8GiB", 2, 17 << 30, false},
			{"below higher explicit override", "8GiB", 2, (17 << 30) - 1, true},
			{"single exact default reserve", "", 1, 17 << 29, false},
			{"single below default reserve", "", 1, (17 << 29) - 1, true},
			{"pair refused at single headroom", "", 2, 17 << 29, true},
		} {
			t.Run(fmt.Sprintf("builder=%t/%s", builder, test.name), func(t *testing.T) {
				cfg := fixtureConfig(t)
				cfg.Memory, cfg.Disk, cfg.MemoryReserve = "4GiB", "20GiB", test.reserve
				store := LeaseStore{Path: filepath.Join(testkit.TempDir(t), "leases.json"), SlotCount: 1}
				pair, err := cfg.EnvironmentSpec(EnvironmentPair)
				if err != nil {
					t.Fatal(err)
				}
				pair.Count = test.count
				grant, err := store.AcquireV3Slot(pair, "client", "SHA256:key", "yard", "Project", "run", "reserve", "slot-001")
				if err != nil {
					t.Fatal(err)
				}
				rt := &Runtime{
					Config: cfg,
					memoryProbe: func() (MemoryCapacity, error) {
						return capPhysicalMemory(MemoryCapacity{Available: 32 << 30, scope: "synthetic-boundary"}, test.available), nil
					},
					diskUsageProbe: func(context.Context) (uint64, error) { return 10 << 30, nil },
					cacheProbe:     func(context.Context) (CacheUsage, error) { return CacheUsage{}, nil },
					Runner: &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
						if strings.Join(args, " ") == "query /1.0/storage-pools/default/resources" {
							return []byte(`{"space":{"total":536870912000,"used":10737418240}}`), nil, nil
						}
						return nil, nil, fmt.Errorf("unexpected mutation: %v", args)
					}},
				}
				if builder {
					if err := store.mutateOwned(grant, func(slot *LeaseSlot, _ time.Time) error { slot.Reserved = true; return nil }); err != nil {
						t.Fatal(err)
					}
					// The builder uses one VM; admission must leave room for the
					// larger of the sequential builder and working environment.
					err = rt.admitBuild(context.Background(), store, grant, 9<<29, 20<<30, &ImageRegistry{})
				} else {
					err = rt.reserveEnvironment(context.Background(), store, grant)
				}
				if test.refused {
					var capacity *CapacityError
					if !errors.As(err, &capacity) || capacity.Resource != "memory" {
						t.Fatalf("memory reserve was not protected: %v", err)
					}
				} else if err != nil {
					t.Fatalf("confirmed headroom was not usable: %v", err)
				}
				if !builder {
					slot, slotErr := storeSlot(store, grant.SlotID)
					if slotErr != nil || slot.Reserved == test.refused {
						t.Fatalf("reservation after admission: %+v, %v", slot, slotErr)
					}
				}
			})
		}
	}
}

func TestConcurrentMixedAdmissionAndRetryAfterReady(t *testing.T) {
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
	rt := &Runtime{Config: cfg, Runner: runner, diskUsageProbe: func(context.Context) (uint64, error) { return 10 << 30, nil }, memoryProbe: func() (MemoryCapacity, error) { return MemoryCapacity{Available: 13 << 30}, nil }}
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
	// Native boot completion alone does not establish a crash-safe persisted
	// RAM phase. Transport/tool readiness may still be in progress; retain the
	// full admitted promise until MarkHeld even if physical pages are occupied.
	betweenBootAndHeld, err := store.AcquireV3Slot(*grants[loser].Environment, "client", "SHA256:key", "yard", "Project", "boot-ready", "capacity", grants[loser].SlotID)
	if err != nil {
		t.Fatal(err)
	}
	var refusal *CapacityError
	if err := rt.reserveEnvironment(context.Background(), store, betweenBootAndHeld); !errors.As(err, &refusal) || refusal.Admission == nil {
		t.Fatalf("unpersisted boot completion released a RAM promise: %v", err)
	}
	winnerRAM, _ := environmentCommitment(*grants[winner].Environment, 512<<20)
	if refusal.Admission.PendingVM != winnerRAM || refusal.Admission.Headroom >= refusal.Admission.Required {
		t.Fatalf("refusal concealed the provisioning promise: %+v", refusal.Admission)
	}
	if err := store.AbortProvisioning(betweenBootAndHeld); err != nil {
		t.Fatal(err)
	}
	// Once ready, the winner is accounted for by host available memory. Its
	// configured RAM must not be reserved again for a later allocation.
	if _, err := store.MarkHeld(grants[winner]); err != nil {
		t.Fatal(err)
	}
	retried, err := store.AcquireV3Slot(*grants[loser].Environment, "client", "SHA256:key", "yard", "Project", "retry", "capacity", grants[loser].SlotID)
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.reserveEnvironment(context.Background(), store, retried); err != nil {
		t.Fatalf("ready allocation retained its startup RAM promise: %v", err)
	}
	if err := rt.reserveEnvironment(context.Background(), store, grants[loser]); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale grant accepted: %v", err)
	}
}

func TestCanceledProvisioningRetainsMemoryUntilCleanup(t *testing.T) {
	for _, expire := range []bool{false, true} {
		t.Run(fmt.Sprintf("expire=%t", expire), func(t *testing.T) {
			cfg := fixtureConfig(t)
			cfg.Memory, cfg.Disk = "4GiB", "20GiB"
			store := LeaseStore{Path: filepath.Join(testkit.TempDir(t), "leases.json"), SlotCount: 2}
			spec, _ := cfg.EnvironmentSpec(EnvironmentPair)
			spec.Count = 1
			first, err := store.AcquireV3Slot(spec, "client", "SHA256:key", "yard", "Project", "first", "capacity", "slot-001")
			if err != nil {
				t.Fatal(err)
			}
			rt := Runtime{Config: cfg,
				memoryProbe:    func() (MemoryCapacity, error) { return MemoryCapacity{Available: 17 << 29}, nil },
				diskUsageProbe: func(context.Context) (uint64, error) { return 10 << 30, nil },
				usageProbe:     func(context.Context, LeaseSlot) allocationUsage { return allocationUsage{} },
				Runner: &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
					if strings.Join(args, " ") == "query /1.0/storage-pools/default/resources" {
						return []byte(`{"space":{"total":536870912000,"used":10737418240}}`), nil, nil
					}
					return nil, nil, fmt.Errorf("unexpected command: %v", args)
				}},
			}
			if err := rt.reserveEnvironment(context.Background(), store, first); err != nil {
				t.Fatal(err)
			}
			if expire {
				err = store.mutateOwned(first, func(slot *LeaseSlot, now time.Time) error { slot.ExpiresAt = now.Add(-time.Second); return nil })
			} else {
				err = store.BeginDrainAll("cancel provisioning")
			}
			if err != nil {
				t.Fatal(err)
			}
			next, err := store.AcquireV3Slot(spec, "client", "SHA256:key", "yard", "Project", "next", "capacity", "slot-002")
			if err != nil {
				t.Fatal(err)
			}
			for _, state := range []SlotState{SlotDraining, SlotQuarantined, SlotRecovering} {
				slot, err := storeSlot(store, first.SlotID)
				if err != nil || slot.State != state {
					t.Fatalf("expected %s: %+v %v", state, slot, err)
				}
				var capacity *CapacityError
				if err := rt.reserveEnvironment(context.Background(), store, next); !errors.As(err, &capacity) || capacity.Resource != "memory" {
					t.Fatalf("%s released an unfinished startup promise: %v", state, err)
				}
				if state == SlotDraining {
					err = store.FinishDrain(first.SlotID, errors.New("stop unconfirmed"))
				} else if state == SlotQuarantined {
					_, _, err = store.BeginScheduledRecovery(first.SlotID, true)
				} else {
					_, err = store.FinishRecovery(first.SlotID, nil, "", "")
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := rt.reserveEnvironment(context.Background(), store, next); err != nil {
				t.Fatalf("confirmed cleanup retained startup RAM: %v", err)
			}
		})
	}
}

func TestAdmissionCreditsOnlyConfirmedExistingDisk(t *testing.T) {
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
				return MemoryCapacity{Available: 20 << 30}, nil
			}}
			var capacity *CapacityError
			if err := rt.reserveEnvironment(context.Background(), store, grant); !errors.As(err, &capacity) || capacity.Resource != "disk" {
				t.Fatalf("missing disk usage credit did not reject conservatively: %v", err)
			}
			rt.usageProbe = func(_ context.Context, slot LeaseSlot) allocationUsage {
				if slot.SlotID == "slot-001" {
					return allocationUsage{disk: 30 << 30}
				}
				return allocationUsage{}
			}
			if err := rt.reserveEnvironment(context.Background(), store, grant); err != nil {
				t.Fatalf("confirmed usage was counted twice: %v", err)
			}
		}
	}
}

func TestAdmissionBoundsChangingHostCapacityAndDiskCredit(t *testing.T) {
	for _, test := range []struct {
		name, resource string
		builder        bool
		memory         [2]uint64
		storageUsed    [2]uint64
		budgetUsed     [2]uint64
		missingSecond  bool
	}{
		{"reserve memory", "memory", false, [2]uint64{11 << 30, 22 << 30}, [2]uint64{20 << 30, 20 << 30}, [2]uint64{20 << 30, 20 << 30}, false},
		{"reserve disk", "disk", false, [2]uint64{30 << 30, 30 << 30}, [2]uint64{50 << 30, 20 << 30}, [2]uint64{20 << 30, 20 << 30}, false},
		{"builder memory", "memory", true, [2]uint64{11 << 30, 22 << 30}, [2]uint64{20 << 30, 20 << 30}, [2]uint64{20 << 30, 20 << 30}, false},
		{"builder disk", "disk", true, [2]uint64{30 << 30, 30 << 30}, [2]uint64{50 << 30, 20 << 30}, [2]uint64{20 << 30, 20 << 30}, false},
		{"reserve budget", "disk", false, [2]uint64{30 << 30, 30 << 30}, [2]uint64{20 << 30, 20 << 30}, [2]uint64{115 << 30, 85 << 30}, false},
		{"builder budget", "disk", true, [2]uint64{30 << 30, 30 << 30}, [2]uint64{20 << 30, 20 << 30}, [2]uint64{115 << 30, 85 << 30}, false},
		{"reserve physical memory falls", "memory", false, [2]uint64{22 << 30, 11 << 30}, [2]uint64{20 << 30, 20 << 30}, [2]uint64{20 << 30, 20 << 30}, false},
		{"builder physical memory falls", "memory", true, [2]uint64{22 << 30, 11 << 30}, [2]uint64{20 << 30, 20 << 30}, [2]uint64{20 << 30, 20 << 30}, false},
		{"reserve second physical sample missing", "memory", false, [2]uint64{30 << 30, 30 << 30}, [2]uint64{20 << 30, 20 << 30}, [2]uint64{20 << 30, 20 << 30}, true},
		{"builder second physical sample missing", "memory", true, [2]uint64{30 << 30, 30 << 30}, [2]uint64{20 << 30, 20 << 30}, [2]uint64{20 << 30, 20 << 30}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := fixtureConfig(t)
			cfg.DiskBudget = "160GiB"
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
					if test.missingSecond && index == 1 {
						return MemoryCapacity{}, errors.New("physical memory source unavailable")
					}
					return capPhysicalMemory(MemoryCapacity{Available: 32 << 30, scope: "synthetic-boundary"}, test.memory[index]), nil
				},
				usageProbe: func(_ context.Context, slot LeaseSlot) allocationUsage {
					if slot.SlotID == first.SlotID {
						return allocationUsage{disk: 30 << 30}
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
			if !test.builder {
				slot, slotErr := storeSlot(store, next.SlotID)
				if slotErr != nil || slot.Reserved {
					t.Fatalf("refused admission reserved the slot: %+v, %v", slot, slotErr)
				}
			}
			wantStorageReads := 2
			if test.missingSecond {
				wantStorageReads = 1
			}
			if memoryReads != 2 || storageReads != wantStorageReads {
				t.Fatalf("capacity was not bracketed around usage: memory %d storage %d", memoryReads, storageReads)
			}
		})
	}
}

func TestDiskUsageRequiresAllocatedBlocks(t *testing.T) {
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

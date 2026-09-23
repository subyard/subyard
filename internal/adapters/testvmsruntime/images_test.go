package testvmsruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestBuilderProjectOwnershipCannotBeAdopted(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprint(existing), func(t *testing.T) {
			cfg := fixtureConfig(t)
			cfg.Project += "-builder"
			owner := strings.Repeat("a", 32)
			created := false
			runner := &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
				command := strings.Join(args, " ")
				switch command {
				case "project list --format csv -c n":
					if existing {
						return []byte(cfg.Project), nil, nil
					}
					return nil, nil, nil
				case "project get " + cfg.Project + " user.subyard.managed":
					return []byte(managedMarker), nil, nil
				case "project get " + cfg.Project + " user.subyard.base_owner":
					return []byte("foreign"), nil, nil
				}
				if strings.HasPrefix(command, "project create ") {
					if !strings.Contains(command, "user.subyard.base_owner="+owner) || !strings.Contains(command, "user.subyard.managed="+managedMarker) {
						t.Fatal("project created without atomic ownership")
					}
					created = true
					return nil, nil, nil
				}
				return nil, nil, fmt.Errorf("unexpected command %s", command)
			}}
			runtime := Runtime{Config: cfg, Runner: runner}
			err := runtime.ensureBuilderProject(context.Background(), owner)
			if existing && (err == nil || created) {
				t.Fatalf("foreign project adopted: err=%v created=%v", err, created)
			}
			if !existing && (err != nil || !created) {
				t.Fatalf("new project: err=%v created=%v", err, created)
			}
		})
	}
}

func TestBuilderCleanupRequiresPhysicalProofAndRecoversUncommittedImages(t *testing.T) {
	for _, failure := range []string{"", "other_task", "finished_publish", "marker", "publish", "operations", "stop", "delete", "reference"} {
		t.Run(failure, func(t *testing.T) {
			cfg := fixtureConfig(t)
			if err := os.MkdirAll(cfg.StateDir, 0700); err != nil {
				t.Fatal(err)
			}
			project := cfg.Project + "-builder"
			owner, key, fingerprint := strings.Repeat("a", 32), strings.Repeat("b", 64), strings.Repeat("c", 64)
			registry := ImageRegistry{SchemaVersion: 1, Owner: owner, Build: &BaseBuild{Key: key, Environment: EnvironmentPair, Project: project}}
			store := LeaseStore{Path: filepath.Join(t.TempDir(), "leases.json"), SlotCount: 1}
			vmDeleted, imageDeleted := false, false
			runner := &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
				command := strings.Join(args, " ")
				switch command {
				case "project list --format csv -c n":
					return []byte(project), nil, nil
				case "project get " + project + " user.subyard.managed":
					return []byte(managedMarker), nil, nil
				case "project get " + project + " user.subyard.base_owner":
					return []byte(owner), nil, nil
				case "operation list --project " + project + " --format json":
					if failure == "operations" {
						return nil, nil, errors.New("operation inventory failed")
					}
					if failure == "publish" {
						return []byte(`[{"class":"task","description":"Downloading image","status_code":103}]`), nil, nil
					}
					if failure == "finished_publish" {
						return []byte(`[{"class":"task","description":"Downloading image","status_code":200}]`), nil, nil
					}
					if failure == "other_task" {
						return []byte(`[{"class":"task","description":"Stopping instance","status_code":103}]`), nil, nil
					}
					return []byte("[]"), nil, nil
				case "list --project " + project + " -f csv -c n":
					if vmDeleted {
						return nil, nil, nil
					}
					return []byte("e2e-base-1"), nil, nil
				case "config get e2e-base-1 user.subyard.managed --project " + project:
					return []byte(managedMarker), nil, nil
				case "config get e2e-base-1 user.subyard.base_owner --project " + project:
					if failure == "marker" {
						return []byte("foreign"), nil, nil
					}
					return []byte(owner), nil, nil
				case "config get e2e-base-1 user.subyard.base_key --project " + project:
					return []byte(key), nil, nil
				case "stop e2e-base-1 --force --project " + project:
					return nil, nil, nil
				case "list e2e-base-1 --project " + project + " -f csv -c s":
					if failure == "stop" {
						return []byte("RUNNING"), nil, nil
					}
					return []byte("STOPPED"), nil, nil
				case "delete e2e-base-1 --project " + project:
					if failure == "delete" {
						return nil, nil, errors.New("delete failed")
					}
					vmDeleted = true
					return nil, nil, nil
				case "image list local: user.subyard.build_key=" + key + " --project default --format json":
					image := incusImage{Fingerprint: fingerprint, Properties: map[string]string{"user.subyard.build_key": key, "user.subyard.base_owner": owner}}
					payload, _ := json.Marshal([]incusImage{image})
					return payload, nil, nil
				case "list --all-projects --format json":
					if failure == "reference" {
						return []byte(`[{"config":{"volatile.base_image":"` + fingerprint + `"}}]`), nil, nil
					}
					return []byte("[]"), nil, nil
				case "image delete " + fingerprint + " --project default":
					imageDeleted = true
					return nil, nil, nil
				case "image list local: " + fingerprint + " --project default --format json":
					return []byte("[]"), nil, nil
				}
				return nil, nil, fmt.Errorf("unexpected command %s", command)
			}}
			runtime := Runtime{Config: cfg, Runner: runner}
			err := runtime.cleanupImageBuild(context.Background(), store, &registry)
			if failure == "" || failure == "other_task" || failure == "finished_publish" {
				if err != nil || registry.Build != nil || !vmDeleted || !imageDeleted {
					t.Fatalf("cleanup failed: err=%v build=%#v vm=%v image=%v", err, registry.Build, vmDeleted, imageDeleted)
				}
			} else {
				if err == nil || registry.Build == nil || imageDeleted {
					t.Fatalf("unsafe cleanup: err=%v build=%#v image=%v", err, registry.Build, imageDeleted)
				}
				if (failure == "marker" || failure == "publish" || failure == "operations" || failure == "stop") && vmDeleted {
					t.Fatal("unverified builder deleted")
				}
			}
		})
	}
}

func TestImagePruneRetainsCurrentExpiredAndUnknownReferences(t *testing.T) {
	for _, referenced := range []bool{false, true} {
		t.Run(fmt.Sprint(referenced), func(t *testing.T) {
			cfg := fixtureConfig(t)
			if err := os.MkdirAll(cfg.StateDir, 0700); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			owner := strings.Repeat("a", 32)
			old := BaseImage{Key: strings.Repeat("b", 64), Fingerprint: strings.Repeat("c", 64), Environment: EnvironmentPair, Recipe: strings.Repeat("d", 64), Architecture: "x86_64", CreatedAt: now.Add(-30 * 24 * time.Hour)}
			current := old
			current.Key = strings.Repeat("e", 64)
			current.Fingerprint = strings.Repeat("f", 64)
			current.CreatedAt = now.Add(-10 * 24 * time.Hour)
			registry := ImageRegistry{SchemaVersion: 1, Owner: owner, Bases: []BaseImage{old, current}}
			store := LeaseStore{Path: filepath.Join(t.TempDir(), "leases.json"), SlotCount: 1}
			deleted := false
			metadata := incusImage{Fingerprint: old.Fingerprint, Type: "virtual-machine", Architecture: old.Architecture, Properties: map[string]string{"user.subyard.base_owner": owner, "user.subyard.base_key": old.Key, "user.subyard.base_recipe": old.Recipe}}
			runner := &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
				command := strings.Join(args, " ")
				if strings.Contains(command, current.Fingerprint) {
					t.Fatal("expired current base was touched before replacement")
				}
				switch command {
				case "image list local: " + old.Fingerprint + " --project default --format json":
					if deleted {
						return []byte("[]"), nil, nil
					}
					p, _ := json.Marshal([]incusImage{metadata})
					return p, nil, nil
				case "query /1.0/images/" + old.Fingerprint + "?project=default":
					p, _ := json.Marshal(metadata)
					return p, nil, nil
				case "list --all-projects --format json":
					if referenced {
						return []byte(`[{"config":{"volatile.base_image":"` + old.Fingerprint + `"}}]`), nil, nil
					}
					return []byte("[]"), nil, nil
				case "image delete " + old.Fingerprint + " --project default":
					deleted = true
					return nil, nil, nil
				}
				return nil, nil, fmt.Errorf("unexpected command %s", command)
			}}
			runtime := Runtime{Config: cfg, Runner: runner, Now: func() time.Time { return now }}
			if err := runtime.pruneImages(context.Background(), store, &registry, true); err != nil {
				t.Fatal(err)
			}
			wantBases := 1
			if referenced {
				wantBases++
			}
			if deleted == referenced || len(registry.Bases) != wantBases {
				t.Fatalf("unsafe prune: deleted=%v bases=%d", deleted, len(registry.Bases))
			}
		})
	}
}

func TestImageReaperDoesNotWaitForActiveBuilder(t *testing.T) {
	cfg := fixtureConfig(t)
	if err := os.MkdirAll(cfg.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	runtime := Runtime{Config: cfg}
	registry := ImageRegistry{SchemaVersion: 1, Owner: strings.Repeat("a", 32)}
	if err := writeJSONAtomic(runtime.imageRegistryPath(), registry); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(runtime.imageRegistryPath()+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runtime.ReapImages(ctx, LeaseStore{Path: cfg.leaseState(), SlotCount: 1}); err != nil {
		t.Fatalf("busy builder blocked cache reap: %v", err)
	}
}

func TestRecipeRequestKeepsItsResolvedGeneration(t *testing.T) {
	root := t.TempDir()
	first, second := filepath.Join(root, "first"), filepath.Join(root, "second")
	for _, directory := range []string{first, second} {
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(first, "manifest.sha256"), []byte("first recipe"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "manifest.sha256"), []byte("second recipe"), 0600); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(root, "current")
	if err := os.Symlink(first, current); err != nil {
		t.Fatal(err)
	}
	cfg := fixtureConfig(t)
	cfg.RecipeRoot = current
	runtime := Runtime{Config: cfg}
	pinned, err := runtime.pinnedRecipeRuntime()
	if err != nil {
		t.Fatal(err)
	}
	before, err := pinned.recipeDigest(EnvironmentPair)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(root, "replacement")
	if err := os.Symlink(second, replacement); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, current); err != nil {
		t.Fatal(err)
	}
	after, err := pinned.recipeDigest(EnvironmentPair)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := runtime.recipeDigest(EnvironmentPair)
	if err != nil {
		t.Fatal(err)
	}
	if before != after || after == changed {
		t.Fatal("concurrent recipe switch changed pinned request")
	}
}

func TestBuilderAdmissionUsesSequentialPeakAndKeepsOtherReservations(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.DiskBudget = "160GiB"
	cfg.Memory, cfg.Disk = "4GiB", "20GiB"
	store := LeaseStore{Path: filepath.Join(t.TempDir(), "leases.json"), SlotCount: 2}
	spec, err := cfg.environmentSpecForArch(EnvironmentAndroid, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	grant, err := store.AcquireV3Slot(spec, "client", "SHA256:key", "yard", "Project", "run", "tests", "slot-001")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.mutateOwned(grant, func(slot *LeaseSlot, _ time.Time) error { slot.Reserved = true; return nil }); err != nil {
		t.Fatal(err)
	}
	runtime := Runtime{Config: cfg, diskUsageProbe: func(context.Context) (uint64, error) { return 10 << 30, nil }, cacheProbe: func(context.Context) (CacheUsage, error) { return CacheUsage{}, nil }, memoryProbe: func() (MemoryCapacity, error) { return MemoryCapacity{Available: 256 << 30}, nil }, Runner: &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
		if strings.Join(args, " ") == "query /1.0/storage-pools/default/resources" {
			return []byte(`{"space":{"total":507085107200,"used":133920153600}}`), nil, nil
		}
		return nil, nil, errors.New("unexpected command")
	}}}
	registry := ImageRegistry{}
	if err := runtime.admitBuild(context.Background(), store, grant, 9<<30, 120<<30, &registry); err != nil {
		t.Fatalf("sequential Android build double counted pending allocation: %v", err)
	}
	legacy, err := store.AcquireSlot("legacy", "SHA256:legacy", "", "tests", "slot-002")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkHeld(legacy); err != nil {
		t.Fatal(err)
	}
	var capacity *CapacityError
	if err := runtime.admitBuild(context.Background(), store, grant, 9<<30, 120<<30, &registry); !errors.As(err, &capacity) || capacity.Resource != "disk" {
		t.Fatalf("builder ignored another held legacy pair: %v", err)
	}
	runtime.Config.DiskBudget = "0GiB"
	if err := runtime.admitBuild(context.Background(), store, grant, 9<<30, 120<<30, &registry); err != nil {
		t.Fatalf("unlimited quota rejected sufficient physical space: %v", err)
	}
	if err := runtime.admitBuild(context.Background(), store, grant, 9<<30, 340<<30, &registry); !errors.As(err, &capacity) || capacity.Resource != "disk" {
		t.Fatalf("unlimited quota ignored held pair growth and physical reserve: %v", err)
	}
	stale := grant
	stale.LeaseEpoch++
	if err := runtime.admitBuild(context.Background(), store, stale, 9<<30, 120<<30, &registry); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale builder reservation excluded current allocation: %v", err)
	}
}

func TestCachedBaseMissingOrChangedIsExcludedWithoutDeletion(t *testing.T) {
	for _, mode := range []string{"missing", "changed", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			base := BaseImage{Fingerprint: strings.Repeat("a", 64), Key: strings.Repeat("b", 64), Recipe: strings.Repeat("c", 64), Architecture: "x86_64"}
			runtime := Runtime{Config: fixtureConfig(t), Runner: &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
				if args[0] == "image" {
					if mode == "missing" {
						return []byte("[]"), nil, nil
					}
					return []byte(`[{"fingerprint":"` + base.Fingerprint + `"}]`), nil, nil
				}
				if args[0] == "query" {
					if mode == "unavailable" {
						return nil, nil, errors.New("daemon unavailable")
					}
					return []byte(`{"fingerprint":"` + base.Fingerprint + `","type":"virtual-machine","architecture":"x86_64","properties":{"user.subyard.base_owner":"foreign"}}`), nil, nil
				}
				t.Fatalf("cache verification mutated image: %v", args)
				return nil, nil, nil
			}}}
			usable, err := runtime.cachedBaseUsable(context.Background(), "owner", base)
			if usable || (err != nil) != (mode == "unavailable") {
				t.Fatalf("cache decision=%v err=%v", usable, err)
			}
		})
	}
}

func TestWarmAcquireRechecksUpstreamFingerprint(t *testing.T) {
	for _, mode := range []string{"warm", "changed", "offline"} {
		t.Run(mode, func(t *testing.T) {
			changed := mode == "changed"
			cfg := fixtureConfig(t)
			cfg.RecipeRoot = t.TempDir()
			if err := os.WriteFile(filepath.Join(cfg.RecipeRoot, "manifest.sha256"), []byte("pinned recipe"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(cfg.StateDir, 0700); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			oldUpstream, newUpstream := strings.Repeat("a", 64), strings.Repeat("b", 64)
			runtime := Runtime{Config: cfg, diskUsageProbe: func(context.Context) (uint64, error) { return 10 << 30, nil }, Now: func() time.Time { return now }, memoryProbe: func() (MemoryCapacity, error) { return MemoryCapacity{Available: 256 << 30}, nil }, cacheProbe: func(context.Context) (CacheUsage, error) { return CacheUsage{}, nil }}
			recipe, err := runtime.recipeDigest(EnvironmentPair)
			if err != nil {
				t.Fatal(err)
			}
			base := BaseImage{Key: strings.Repeat("c", 64), Fingerprint: strings.Repeat("d", 64), Environment: EnvironmentPair, Recipe: recipe, Architecture: imageArchitecture(), Upstream: oldUpstream, CreatedAt: now.Add(-time.Hour)}
			registry := ImageRegistry{SchemaVersion: 1, Owner: strings.Repeat("e", 32), Bases: []BaseImage{base}}
			if err := writeJSONAtomic(runtime.imageRegistryPath(), registry); err != nil {
				t.Fatal(err)
			}
			store := LeaseStore{Path: cfg.leaseState(), SlotCount: 1}
			spec, err := cfg.EnvironmentSpec(EnvironmentPair)
			if err != nil {
				t.Fatal(err)
			}
			grant, err := store.AcquireV3Slot(spec, "client", "SHA256:key", "yard", "Project", "run", "tests", "slot-001")
			if err != nil {
				t.Fatal(err)
			}
			upstreamCalls := 0
			buildRequired := errors.New("new base build reached")
			metadata := incusImage{Fingerprint: base.Fingerprint, Type: "virtual-machine", Architecture: base.Architecture, Properties: map[string]string{"user.subyard.base_owner": registry.Owner, "user.subyard.base_key": base.Key, "user.subyard.base_recipe": base.Recipe}}
			runtime.Runner = &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
				command := strings.Join(args, " ")
				switch command {
				case "query /1.0/storage-pools/default/resources":
					return []byte(`{"space":{"total":171798691840,"used":10737418240}}`), nil, nil
				case "image list images: debian/13/cloud type=virtual-machine --format json":
					upstreamCalls++
					if mode == "offline" {
						return nil, nil, errors.New("upstream temporarily unavailable")
					}
					if upstreamCalls > 1 {
						return nil, nil, buildRequired
					}
					fingerprint := oldUpstream
					if changed {
						fingerprint = newUpstream
					}
					return []byte(`[{"fingerprint":"` + fingerprint + `","architecture":"` + imageArchitecture() + `","type":"virtual-machine","aliases":[{"name":"debian/13/cloud"}]}]`), nil, nil
				case "image list local: " + base.Fingerprint + " --project default --format json":
					payload, _ := json.Marshal([]incusImage{metadata})
					return payload, nil, nil
				case "query /1.0/images/" + base.Fingerprint + "?project=default":
					payload, _ := json.Marshal(metadata)
					return payload, nil, nil
				}
				return nil, nil, fmt.Errorf("unexpected command %s", command)
			}}
			child := runtime.slotRuntime(1, "")
			acquired, err := runtime.prepareEnvironment(context.Background(), store, grant, child)
			if changed {
				if !errors.Is(err, buildRequired) || upstreamCalls != 2 {
					t.Fatalf("changed alias reused stale base: calls=%d err=%v", upstreamCalls, err)
				}
			} else if err != nil || upstreamCalls != 1 || acquired.BaseFingerprint != base.Fingerprint {
				t.Fatalf("warm base failed: calls=%d err=%v grant=%#v", upstreamCalls, err, acquired)
			}
		})
	}
}

func TestPruneRetainsUnusableForeignImageWithoutBlockingHealthyBase(t *testing.T) {
	cfg := fixtureConfig(t)
	if err := os.MkdirAll(cfg.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	old := BaseImage{Key: strings.Repeat("a", 64), Fingerprint: strings.Repeat("b", 64), Environment: EnvironmentPair, Recipe: strings.Repeat("c", 64), Architecture: "x86_64", Unusable: true, CreatedAt: time.Now()}
	current := old
	current.Key = strings.Repeat("d", 64)
	current.Fingerprint = strings.Repeat("e", 64)
	current.Unusable = false
	current.CreatedAt = old.CreatedAt.Add(-time.Minute)
	registry := ImageRegistry{SchemaVersion: 1, Owner: strings.Repeat("f", 32), Bases: []BaseImage{old, current}}
	runtime := Runtime{Config: cfg, Runner: &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
		command := strings.Join(args, " ")
		if strings.Contains(command, current.Fingerprint) {
			t.Fatal("unusable image displaced healthy current retention")
		}
		switch command {
		case "image list local: " + old.Fingerprint + " --project default --format json":
			return []byte(`[{"fingerprint":"` + old.Fingerprint + `"}]`), nil, nil
		case "query /1.0/images/" + old.Fingerprint + "?project=default":
			return []byte(`{"fingerprint":"` + old.Fingerprint + `","type":"virtual-machine","architecture":"x86_64","properties":{"user.subyard.base_owner":"foreign"}}`), nil, nil
		}
		return nil, nil, fmt.Errorf("unsafe command %s", command)
	}}}
	store := LeaseStore{Path: cfg.leaseState(), SlotCount: 1}
	if err := runtime.pruneImages(context.Background(), store, &registry, false); err != nil {
		t.Fatalf("foreign image blocked healthy issuance: %v", err)
	}
	if len(registry.Bases) != 2 {
		t.Fatal("foreign image evidence was discarded")
	}
}

package testvmsruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConcurrentColdPrepareBuildsOnceAndPreservesBothPins(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.RecipeRoot = t.TempDir()
	if err := os.WriteFile(filepath.Join(cfg.RecipeRoot, "manifest.sha256"), []byte("fixture recipe"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	building, finishBuild := make(chan struct{}, 2), make(chan struct{})
	var release sync.Once
	var requests sync.WaitGroup
	defer func() {
		cancel()
		release.Do(func() { close(finishBuild) })
		requests.Wait()
	}()
	project := cfg.Project + "-builder"
	upstream, fingerprint := strings.Repeat("a", 64), strings.Repeat("b", 64)
	var mu sync.Mutex
	var projectExists, vmExists bool
	var owner, buildKey string
	var builds, publications, deletions int
	images := map[string]incusImage{}
	var deletedImages []string
	refreshedFingerprint := strings.Repeat("d", 64)
	// Each request has its own fakeRunner call log; only the simulated Incus
	// state is shared. No fixture lock serializes the blocked recipe execution.
	handler := func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
		command := strings.Join(args, " ")
		if strings.HasSuffix(command, "bash /opt/subyard-base-recipe/scripts/e2e-lab/base.sh") {
			select {
			case building <- struct{}{}:
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
			select {
			case <-finishBuild:
				return nil, nil, nil
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}
		mu.Lock()
		defer mu.Unlock()
		encode := func(value any) ([]byte, []byte, error) {
			body, err := json.Marshal(value)
			return body, nil, err
		}
		properties := func() map[string]string {
			values := map[string]string{}
			for _, arg := range args {
				if key, value, ok := strings.Cut(arg, "="); ok {
					values[key] = value
				}
			}
			return values
		}
		switch command {
		case "query /1.0/storage-pools/default/resources":
			return []byte(`{"space":{"total":171798691840,"used":10737418240}}`), nil, nil
		case "image list images: debian/13/cloud type=virtual-machine --format json":
			return []byte(`[{"fingerprint":"` + upstream + `","architecture":"` + imageArchitecture() + `","type":"virtual-machine","aliases":[{"name":"debian/13/cloud"}]}]`), nil, nil
		case "project list --format csv -c n":
			if projectExists {
				return []byte(project), nil, nil
			}
			return nil, nil, nil
		case "project get " + project + " user.subyard.managed", "config get e2e-base-1 user.subyard.managed --project " + project:
			return []byte(managedMarker), nil, nil
		case "project get " + project + " user.subyard.base_owner", "config get e2e-base-1 user.subyard.base_owner --project " + project:
			return []byte(owner), nil, nil
		case "config get e2e-base-1 user.subyard.base_key --project " + project:
			return []byte(buildKey), nil, nil
		case "project get " + project + " limits.cpu", "project get " + project + " limits.memory":
			return nil, nil, nil
		case "list --project " + project + " -f csv -c n":
			if vmExists {
				return []byte("e2e-base-1"), nil, nil
			}
			return nil, nil, nil
		case "info e2e-base-1 --project " + project:
			if !vmExists {
				return nil, nil, errors.New("instance absent")
			}
			return nil, nil, nil
		case "profile device list default --project " + project:
			return []byte("root\neth0"), nil, nil
		case "list e2e-base-1 --project " + project + " -f csv -c s":
			return []byte("STOPPED"), nil, nil
		case "operation list --project " + project + " --format json", "list --all-projects --format json":
			return []byte("[]"), nil, nil
		case "delete e2e-base-1 --project " + project:
			vmExists = false
			deletions++
			return nil, nil, nil
		}
		switch {
		case strings.HasPrefix(command, "project create "+project+" "):
			projectExists, owner = true, properties()["user.subyard.base_owner"]
		case strings.HasPrefix(command, "init images:"+upstream+" e2e-base-1 "):
			builds++
			vmExists, buildKey = true, properties()["user.subyard.base_key"]
		case strings.HasPrefix(command, "publish e2e-base-1 "):
			publications++
			published := fingerprint
			if publications > 1 {
				published = refreshedFingerprint
			}
			images[published] = incusImage{Fingerprint: published, Architecture: imageArchitecture(), Type: "virtual-machine", Properties: properties()}
		case strings.HasPrefix(command, "query /1.0/images/"):
			selected := strings.TrimSuffix(strings.TrimPrefix(args[1], "/1.0/images/"), "?project=default")
			return encode(images[selected])
		case strings.HasPrefix(command, "image list local: "):
			matching := []incusImage{}
			for _, image := range images {
				key, value, property := strings.Cut(args[3], "=")
				if image.Fingerprint == args[3] || (property && image.Properties[key] == value) {
					matching = append(matching, image)
				}
			}
			return encode(matching)
		case strings.HasPrefix(command, "image delete "):
			deletedImages = append(deletedImages, args[2])
			delete(images, args[2])
		case strings.HasPrefix(command, "exec e2e-base-1 "):
			if strings.HasSuffix(command, "-- cat /var/lib/subyard/base-manifest.sha256") {
				return []byte(strings.Repeat("c", 64)), nil, nil
			}
			if strings.HasSuffix(command, "-- passwd --status dev") {
				return []byte("dev P fixture"), nil, nil
			}
			if strings.HasSuffix(command, "-- sshd -T") {
				return []byte("passwordauthentication no"), nil, nil
			}
		case strings.HasPrefix(command, "project set "+project+" "),
			strings.HasPrefix(command, "profile device set default root "),
			strings.HasPrefix(command, "start e2e-base-1 "),
			strings.HasPrefix(command, "stop e2e-base-1 "),
			strings.HasPrefix(command, "file push "):
		default:
			return nil, nil, fmt.Errorf("unexpected command %s", command)
		}
		return nil, nil, nil
	}
	now := time.Now()
	newRuntime := func() *Runtime {
		rt := &Runtime{
			Config: cfg, Runner: &fakeRunner{handler: handler}, Stdout: io.Discard, Stderr: io.Discard,
			Now:            func() time.Time { return now },
			memoryProbe:    func() (MemoryCapacity, error) { return MemoryCapacity{Available: 64 << 30}, nil },
			diskUsageProbe: func(context.Context) (uint64, error) { return 10 << 30, nil },
			cacheProbe:     func(context.Context) (CacheUsage, error) { return CacheUsage{}, nil },
			usageProbe:     func(context.Context, LeaseSlot, string) allocationUsage { return allocationUsage{} },
		}
		rt.prepareDefaults()
		return rt
	}
	store := LeaseStore{Path: cfg.leaseState(), SlotCount: 2}
	spec, err := cfg.EnvironmentSpec(EnvironmentPair)
	if err != nil {
		t.Fatal(err)
	}
	grants := make([]LeaseGrant, 2)
	for i := range grants {
		grants[i], err = store.AcquireV3Slot(spec, "client", "SHA256:key", "yard", "Project", "run", "tests", fmt.Sprintf("slot-%03d", i+1))
		if err != nil {
			t.Fatal(err)
		}
	}
	type result struct {
		grant LeaseGrant
		image string
		err   error
	}
	results := make(chan result, 2)
	prepare := func(i int) {
		defer requests.Done()
		rt := newRuntime()
		child := rt.slotRuntime(i+1, "")
		grant, err := rt.prepareEnvironment(ctx, store, grants[i], child)
		results <- result{grant, child.Config.Image, err}
	}
	requests.Add(1)
	go prepare(0)
	select {
	case <-building:
	case result := <-results:
		t.Fatalf("first request failed before recipe execution: %v", result.err)
	case <-ctx.Done():
		t.Fatal("first request did not reach recipe execution")
	}
	requests.Add(1)
	secondStarted := make(chan struct{})
	go func() {
		close(secondStarted)
		prepare(1)
	}()
	<-secondStarted
	select {
	case <-building:
		t.Fatal("second request started a concurrent base build")
	case result := <-results:
		t.Fatalf("request completed while the only base was unbuilt: %v", result.err)
	case <-time.After(100 * time.Millisecond):
	}
	// The reaper must neither wait for nor clean the live, persisted builder.
	reaper := newRuntime()
	if err := reaper.ReapImages(ctx, store); err != nil {
		t.Fatal(err)
	}
	registry, err := reaper.imageRegistry()
	if err != nil || registry.Build == nil {
		t.Fatalf("active builder reservation lost: registry=%+v err=%v", registry, err)
	}
	release.Do(func() { close(finishBuild) })
	for range grants {
		select {
		case result := <-results:
			if result.err != nil || result.grant.BaseFingerprint != fingerprint || result.image != fingerprint {
				t.Fatalf("prepare failed or selected a different base: grant pin=%s image=%s err=%v", result.grant.BaseFingerprint, result.image, result.err)
			}
		case <-ctx.Done():
			t.Fatal("concurrent requests did not finish")
		}
	}
	mu.Lock()
	gotBuilds, gotPublications, gotDeletions := builds, publications, deletions
	mu.Unlock()
	if gotBuilds != 1 || gotPublications != 1 || gotDeletions != 1 {
		t.Fatalf("builds=%d publications=%d builder deletions=%d; want one each", gotBuilds, gotPublications, gotDeletions)
	}
	registry, err = reaper.imageRegistry()
	if err != nil || registry.Build != nil || len(registry.Bases) != 1 || registry.Bases[0].Fingerprint != fingerprint {
		t.Fatalf("unexpected durable image registry: %+v err=%v", registry, err)
	}
	pool, err := store.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	for _, slot := range pool.Slots {
		if slot.State != SlotProvisioning || !slot.Reserved || slot.BaseFingerprint != fingerprint {
			t.Fatalf("lost allocation reservation or base pin for %s: state=%s reserved=%v pin=%s", slot.SlotID, slot.State, slot.Reserved, slot.BaseFingerprint)
		}
	}
	t.Run("successful refresh retains old base until both leases drain", func(t *testing.T) {
		for _, grant := range grants {
			if _, err := store.MarkHeld(grant); err != nil {
				t.Fatal(err)
			}
		}
		refresh := newRuntime()
		refresh.Now = func() time.Time { return now.Add(time.Hour) }
		if err := refresh.RefreshBase(ctx, store, EnvironmentPair); err != nil {
			t.Fatal(err)
		}
		registry, err := refresh.imageRegistry()
		if err != nil || registry.Build != nil || len(registry.Bases) != 2 {
			t.Fatalf("refresh did not commit both generations: %+v err=%v", registry, err)
		}
		current := registry.Bases[1]
		if current.Fingerprint != refreshedFingerprint || !current.CreatedAt.After(registry.Bases[0].CreatedAt) {
			t.Fatalf("refresh did not publish a newer immutable generation: %+v", registry.Bases)
		}
		pool, err := store.Inspect()
		if err != nil {
			t.Fatal(err)
		}
		for _, slot := range pool.Slots {
			if slot.State != SlotHeld || slot.BaseFingerprint != fingerprint {
				t.Fatalf("refresh changed held lease %s: state=%s pin=%s", slot.SlotID, slot.State, slot.BaseFingerprint)
			}
		}
		for drained := 0; drained <= len(grants); drained++ {
			if drained > 0 {
				grant := grants[drained-1]
				if err := store.BeginDrain(grant); err != nil {
					t.Fatal(err)
				}
				if err := store.FinishDrain(grant.SlotID, nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := reaper.ReapImages(ctx, store); err != nil {
				t.Fatal(err)
			}
			registry, err := reaper.imageRegistry()
			if err != nil {
				t.Fatal(err)
			}
			wantBases := 2
			if drained == len(grants) {
				wantBases = 1
			}
			if len(registry.Bases) != wantBases || !slices.Contains(registry.Bases, current) {
				t.Fatalf("after %d drains, unexpected retained generations: %+v", drained, registry.Bases)
			}
			mu.Lock()
			_, oldPresent := images[fingerprint]
			_, newPresent := images[refreshedFingerprint]
			deleteCount := len(deletedImages)
			mu.Unlock()
			if oldPresent != (drained < len(grants)) || !newPresent || deleteCount != 2-wantBases {
				t.Fatalf("after %d drains: old=%v current=%v deletions=%d", drained, oldPresent, newPresent, deleteCount)
			}
		}
		mu.Lock()
		gotBuilds, gotPublications, gotDeletions = builds, publications, deletions
		mu.Unlock()
		if gotBuilds != 2 || gotPublications != 2 || gotDeletions != 2 {
			t.Fatalf("refresh totals: builds=%d publications=%d staging deletions=%d; want two each", gotBuilds, gotPublications, gotDeletions)
		}
	})
}

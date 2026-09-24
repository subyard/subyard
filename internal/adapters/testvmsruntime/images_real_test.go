//go:build realincus

package testvmsruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This deliberately boots only empty firmware, never an OS or the full broker.
// Run inside one allocated subyard-pair guest with SUBYARD_REAL_BROKER_STORAGE=1.
func TestRealBrokerStorageContract(t *testing.T) {
	if os.Getenv("SUBYARD_REAL_BROKER_STORAGE") != "1" {
		t.Skip("set SUBYARD_REAL_BROKER_STORAGE=1 inside an allocated E2E guest")
	}
	if os.Geteuid() != 0 {
		t.Fatal("requires guest root; use dev/e2e/broker-storage-contract.sh")
	}
	var lease struct {
		SchemaVersion int    `json:"schema_version"`
		Project       string `json:"project"`
		Run           string `json:"run"`
		Slot          string `json:"slot"`
	}
	body, err := os.ReadFile("/run/subyard-e2e-lease.json")
	if err != nil || json.Unmarshal(body, &lease) != nil ||
		lease.SchemaVersion < 1 || lease.Project == "" || lease.Run == "" || !brokerSlotID.MatchString(lease.Slot) ||
		os.Getenv("SUBYARD_E2E_TYPE") != EnvironmentPair {
		t.Fatal("requires root, an allocated guest lease context, and SUBYARD_E2E_TYPE=subyard-pair")
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	owner, err := randomToken(16)
	must(err)
	name := "subyard-storage-" + owner[:12]
	rt := &Runtime{Config: Config{Project: name, Prefix: "e2e-vm", StateDir: t.TempDir(), Incus: "incus"}, Runner: ProcessRunner{}, Now: time.Now}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	command := func(args ...string) string {
		t.Helper()
		value, err := rt.incus(ctx, args...)
		must(err)
		return strings.TrimSpace(value)
	}
	store := LeaseStore{Path: filepath.Join(rt.Config.StateDir, "leases.json"), SlotCount: 1}
	registry := ImageRegistry{SchemaVersion: 1, Owner: owner}
	projects := []string{rt.Config.Project, rt.Config.Project + "-builder"}
	createdProjects := map[string]bool{}
	imageKeys := map[string]string{}
	poolCreated := false
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		call := func(args ...string) (string, error) { return rt.incus(cleanupCtx, args...) }
		for _, project := range projects {
			if !createdProjects[project] {
				continue
			}
			marker, markerErr := call("project", "get", project, "user.subyard.base_owner")
			if markerErr != nil || strings.TrimSpace(marker) != owner {
				t.Errorf("cleanup refuses project without ownership: %s", project)
				continue
			}
			child := *rt
			child.Config.Project = project
			if err := child.requireProjectMarker(cleanupCtx); err != nil {
				t.Error(err)
				continue
			}
			names, err := child.projectInstances(cleanupCtx)
			if err != nil {
				t.Error(err)
				continue
			}
			for _, vm := range names {
				marker, markerErr := call("config", "get", vm, "user.subyard.base_owner", "--project", project)
				if (vm != "e2e-vm-1" && vm != "e2e-base-1") || markerErr != nil || strings.TrimSpace(marker) != owner || child.requireVMMarker(cleanupCtx, vm) != nil {
					t.Errorf("cleanup refuses unowned instance in %s", project)
					continue
				}
				_, _ = call("stop", vm, "--force", "--project", project)
				if err := child.stopRunningVM(cleanupCtx, vm); err != nil {
					t.Error(err)
					continue
				}
				if _, err := call("delete", vm, "--project", project); err != nil {
					t.Error(err)
				}
			}
			if _, err := call("project", "delete", project); err != nil {
				t.Error(err)
			}
		}
		images, err := rt.listImages(cleanupCtx, "local:", "user.subyard.base_owner="+owner, "--project", "default")
		if err != nil {
			t.Error(err)
		}
		for _, image := range images {
			key := image.Properties["user.subyard.base_key"]
			base := BaseImage{Key: key, Recipe: imageKeys[key], Fingerprint: image.Fingerprint, Architecture: imageArchitecture()}
			if base.Recipe == "" || rt.verifyBase(cleanupCtx, owner, base) != nil || len(image.Aliases) != 0 {
				t.Error("cleanup refuses image without exact ownership")
				continue
			}
			if err := rt.deleteImageAndVerify(cleanupCtx, image.Fingerprint); err != nil {
				t.Error(err)
			}
		}
		if poolCreated {
			marker, err := call("storage", "get", name, "user.subyard.base_owner")
			if err != nil || strings.TrimSpace(marker) != owner {
				t.Error("cleanup refuses pool without ownership")
			} else if _, err := call("storage", "delete", name); err != nil {
				t.Error(err)
			}
		}
	})
	command("storage", "create", name, "dir", "user.subyard.base_owner="+owner)
	poolCreated = true
	createVM := func(project, vm, key string, extra ...string) {
		command("project", "create", project, "-c", "features.images=false", "-c", "user.subyard.managed="+managedMarker,
			"-c", "user.subyard.base_owner="+owner)
		createdProjects[project] = true
		args := []string{"init", vm, "--empty", "--vm", "--no-profiles", "--project", project, "-s", name,
			"-c", "limits.cpu=1", "-c", "limits.memory=512MiB", "-d", "root,size=1GiB",
			"-c", "user.subyard.managed=" + managedMarker, "-c", "user.subyard.base_owner=" + owner, "-c", "user.subyard.base_key=" + key}
		command(append(args, extra...)...)
	}
	publish := func(project, vm, key, buildKey string) BaseImage {
		base := BaseImage{Key: key, Recipe: imageKey("recipe", key), Environment: EnvironmentPair, Architecture: imageArchitecture(), CreatedAt: time.Now()}
		imageKeys[key] = base.Recipe
		command("publish", vm, "--project", project, "user.subyard.base_owner="+owner, "user.subyard.base_key="+key,
			"user.subyard.base_recipe="+base.Recipe, "user.subyard.build_key="+buildKey)
		images, err := rt.listImages(ctx, "local:", "user.subyard.base_key="+key, "--project", "default")
		must(err)
		if len(images) != 1 {
			t.Fatalf("expected one published image, got %d", len(images))
		}
		base.Fingerprint = images[0].Fingerprint
		must(rt.verifyBase(ctx, owner, base))
		return base
	}
	assertImage := func(base BaseImage, present bool) {
		t.Helper()
		images, err := rt.listImages(ctx, "local:", base.Fingerprint, "--project", "default")
		must(err)
		if (len(images) == 1) != present || len(images) > 1 {
			t.Fatalf("image presence=%d, want present=%v", len(images), present)
		}
	}
	assertVolumes := func(project string, present bool) {
		t.Helper()
		var volumes []struct {
			Name string `json:"name"`
		}
		must(json.Unmarshal([]byte(command("storage", "volume", "list", name, "--project", project, "--format", "json")), &volumes))
		if volumes == nil || (len(volumes) > 0) != present {
			t.Fatalf("volume inventory=%v, want present=%v", volumes, present)
		}
	}
	// This local store exercises disposable pin/drain state. Physical fixtures
	// remain a single tiny firmware VM, not a provisioned broker environment.
	spec := EnvironmentSpec{Name: EnvironmentPair, Count: 2, CPU: 1, Memory: "512MiB", Disk: "10GiB", Lifecycle: DisposableLifecycle}
	grant, err := store.AcquireV3Slot(spec, "storage-contract", "SHA256:synthetic", "yard", "Project", "run", "storage-contract", "slot-001")
	must(err)
	rt.allocation = &LeaseIdentity{SlotID: grant.SlotID, ResourceGeneration: grant.ResourceGeneration, LeaseEpoch: grant.LeaseEpoch}
	vm := rt.Config.vm(1)
	createVM(rt.Config.Project, vm, imageKey(owner, "allocation"), "-c", fmt.Sprintf("user.subyard.generation=%d", grant.ResourceGeneration),
		"-c", fmt.Sprintf("user.subyard.lease-epoch=%d", grant.LeaseEpoch))
	assertVolumes(rt.Config.Project, true)
	command("start", vm, "--project", rt.Config.Project)
	if state := command("list", vm, "--project", rt.Config.Project, "-f", "csv", "-c", "s"); state != "RUNNING" {
		t.Fatalf("empty VM did not run: %s", state)
	}
	if err := rt.deleteAllocation(ctx); err == nil || !strings.Contains(err.Error(), "not stopped") || !rt.vmExists(ctx, vm) {
		t.Fatalf("running allocation was not preserved: %v", err)
	}
	// There is no OS agent or ACPI consumer in an empty firmware-only fixture.
	command("stop", vm, "--force", "--project", rt.Config.Project)
	must(rt.stopRunningVM(ctx, vm))
	rt.allocation.ResourceGeneration++
	if err := rt.deleteAllocation(ctx); err == nil || !strings.Contains(err.Error(), "allocation marker mismatch") || !rt.vmExists(ctx, vm) {
		t.Fatalf("mismatched allocation was not preserved: %v", err)
	}
	rt.allocation.ResourceGeneration--
	command("snapshot", "create", vm, "storage-contract", "--project", rt.Config.Project)
	var snapshots []json.RawMessage
	must(json.Unmarshal([]byte(command("query", "/1.0/instances/"+vm+"/snapshots?project="+rt.Config.Project)), &snapshots))
	if len(snapshots) != 1 {
		t.Fatalf("expected one physical snapshot, got %d", len(snapshots))
	}
	old := publish(rt.Config.Project, vm, imageKey(owner, "old"), imageKey(owner, "old-build"))
	current := publish(rt.Config.Project, vm, imageKey(owner, "current"), imageKey(owner, "current-build"))
	if old.Fingerprint == current.Fingerprint {
		t.Fatal("image generations are not distinct")
	}
	registry.Bases = []BaseImage{old, current}
	must(writeJSONAtomic(rt.imageRegistryPath(), registry))
	must(store.mutateOwned(grant, func(slot *LeaseSlot, _ time.Time) error {
		slot.BaseFingerprint, slot.Reserved = old.Fingerprint, true
		return nil
	}))
	_, err = store.MarkHeld(grant)
	must(err)
	must(rt.pruneImages(ctx, store, &registry, false))
	assertImage(old, true)
	assertImage(current, true)
	must(store.BeginDrain(grant))
	must(rt.deleteAllocation(ctx))
	assertVolumes(rt.Config.Project, false)
	must(store.FinishDrain(grant.SlotID, nil))
	pool, err := store.Inspect()
	must(err)
	if pool.Slots[0].State != SlotAvailable || pool.Slots[0].BaseFingerprint != "" {
		t.Fatal("verified drain did not clear the base pin")
	}
	must(rt.pruneImages(ctx, store, &registry, false))
	assertImage(old, false)
	assertImage(current, true)
	if err := rt.verifyBase(ctx, "foreign-owner", current); !errors.Is(err, errBaseMismatch) {
		t.Fatalf("wrong image ownership was accepted: %v", err)
	}
	t.Log("running/mismatched allocation retained; verified deletion removed root/snapshot; pin protected obsolete base until drain")

	buildKey := imageKey(owner, "interrupted-build")
	registry.Build = &BaseBuild{Key: buildKey, Environment: EnvironmentPair, Project: projects[1]}
	must(writeJSONAtomic(rt.imageRegistryPath(), registry))
	createVM(projects[1], "e2e-base-1", buildKey)
	orphan := publish(projects[1], "e2e-base-1", imageKey(owner, "uncommitted"), buildKey)
	restarted := &Runtime{Config: rt.Config, Runner: ProcessRunner{}, Now: time.Now}
	registry, err = restarted.imageRegistry() // Recover the persisted staging record.
	must(err)
	must(restarted.cleanupImageBuild(ctx, store, &registry))
	assertVolumes(projects[1], false)
	assertImage(orphan, false)
	assertImage(current, true)
	registry, err = rt.imageRegistry()
	must(err)
	if registry.Build != nil {
		t.Fatal("builder reservation remains after verified cleanup")
	}
	t.Log("persisted builder cleanup removed staging root and uncommitted image; current base preserved")
}

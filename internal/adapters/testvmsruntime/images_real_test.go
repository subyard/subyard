//go:build realincus

package testvmsruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
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
				if (vm != "e2e-vm-1" && vm != "e2e-vm-2" && vm != "e2e-base-1") || markerErr != nil || strings.TrimSpace(marker) != owner || child.requireVMMarker(cleanupCtx, vm) != nil {
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
		if !createdProjects[project] {
			command("project", "create", project, "-c", "features.images=false", "-c", "user.subyard.managed="+managedMarker,
				"-c", "user.subyard.base_owner="+owner)
			createdProjects[project] = true
		}
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
	spec := EnvironmentSpec{Name: EnvironmentPair, Count: 1, CPU: 1, Memory: "512MiB", Disk: "10GiB", Lifecycle: DisposableLifecycle}
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
	// This fixture has no OS agent, ACPI consumer, network or forwarding account.
	// Prove the native graceful timeout before exercising disposable recovery's
	// guarded fallback; disabling an agent would not prove a stalled stop.
	rt.prepareDefaults()
	slot, err := storeSlot(store, grant.SlotID)
	must(err)
	slot.State = SlotRecovering
	gracefulErr := rt.stopRunningVM(ctx, vm)
	if gracefulErr == nil {
		t.Fatal("firmware fixture unexpectedly stopped gracefully; stalled-stop reproduction incomplete")
	}
	// Normal query must preserve native missing-log failures instead of treating
	// the API error envelope as successful log content.
	missingCtx, cancelMissing := context.WithTimeout(ctx, time.Second)
	_, missingErr := rt.incus(missingCtx, "query", "/1.0/instances/"+url.PathEscape(vm)+
		"/logs/qemu.qmp.missing.log?project="+url.QueryEscape(rt.Config.Project))
	cancelMissing()
	var missingCommand *CommandError
	if !errors.As(missingErr, &missingCommand) || missingCommand.ExitCode <= 0 {
		t.Fatalf("native missing-log query did not return a nonzero CLI failure: %v", missingErr)
	}
	initial, err := rt.eventRecorder().SaveIncident(slot, gracefulErr, rt.recoveryDiagnostics(ctx, vm))
	must(err)
	slot.IncidentID = initial.IncidentID
	rt.recoverySlot = &slot
	must(rt.forceStopRecoveryVM(ctx, vm, gracefulErr))
	batch, err := rt.eventRecorder().Export()
	must(err)
	if len(batch.Incidents) != 2 || len(batch.Events) != 2 ||
		batch.Events[0].Kind != "vm.force_stop_planned" || batch.Events[1].Kind != "vm.force_stop_succeeded" {
		t.Fatalf("missing durable stalled-stop/fallback evidence: incidents=%d events=%#v", len(batch.Incidents), batch.Events)
	}
	for _, incident := range batch.Incidents {
		qmp := incident.Diagnostics["vm_1_qmp_log"]
		if !strings.Contains(qmp, "QUERY:") && !strings.Contains(qmp, "Event:") {
			t.Fatalf("native QMP record was not captured before force stop: incident=%s", incident.IncidentID)
		}
	}
	if state := command("list", vm, "--project", rt.Config.Project, "-f", "csv", "-c", "s"); state != "STOPPED" {
		t.Fatalf("force stop did not reach STOPPED: %s", state)
	}
	t.Log("native graceful stop failed; disposable recovery saved fresh evidence and force-stopped the exact allocation")
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

	// Exercise counted facade grants against real Incus using the same validated
	// firmware base. OS provisioning and SSH remain outside this storage contract.
	command("profile", "device", "add", "default", "root", "disk", "pool="+name, "path=/", "size=10GiB", "--project", rt.Config.Project)
	for _, count := range []int{1, 2, 1} {
		var output bytes.Buffer
		facade := Facade{Store: store, Output: &output, EnvironmentSpec: func(string) (EnvironmentSpec, error) { return spec, nil }}
		must(facade.Run(fmt.Sprintf("acquire-v3 %s storage-contract SHA256:synthetic yard Project run storage-contract %s slot-001 %d",
			EnvironmentPair, fixturePublicKey(t), count)))
		var response facadeResponse
		must(json.Unmarshal(output.Bytes(), &response))
		if response.Grant == nil || response.Grant.Environment.Count != count {
			t.Fatalf("counted facade grant: %s", output.String())
		}
		allocation := *response.Grant
		child := *rt
		child.Config = rt.Config.withEnvironment(*allocation.Environment)
		child.Config.Image = "local:" + current.Fingerprint
		child.allocation = &LeaseIdentity{SlotID: allocation.SlotID, ResourceGeneration: allocation.ResourceGeneration, LeaseEpoch: allocation.LeaseEpoch}
		command("project", "set", rt.Config.Project, "limits.instances", fmt.Sprint(count))
		command("project", "set", rt.Config.Project, "limits.virtual-machines", fmt.Sprint(count))
		for i := 1; i <= child.Config.guestCount(); i++ {
			must(child.initVM(ctx, child.Config.vm(i)))
			command("config", "set", child.Config.vm(i), "user.subyard.base_owner", owner, "--project", child.Config.Project)
			command("start", child.Config.vm(i), "--project", child.Config.Project)
		}
		names, err := child.projectInstances(ctx)
		must(err)
		if len(names) != count {
			t.Fatalf("requested %d VMs, Incus created %d", count, len(names))
		}
		for _, vm := range names {
			if command("config", "get", vm, "limits.memory", "--project", child.Config.Project) != spec.Memory ||
				command("list", vm, "--project", child.Config.Project, "-f", "csv", "-c", "s") != "RUNNING" {
				t.Fatal("counted allocation did not start with trusted memory limits")
			}
			command("stop", vm, "--force", "--project", child.Config.Project)
		}
		must(store.BeginDrain(allocation))
		must(child.deleteAllocation(ctx))
		assertVolumes(child.Config.Project, false)
		must(store.FinishDrain(allocation.SlotID, nil))
		assertImage(current, true)
		t.Logf("requested/created/started/deleted %d VMs from the same validated base", count)
	}
}

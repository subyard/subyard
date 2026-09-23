package testvmsruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"
)

const baseMaxAge = 7 * 24 * time.Hour

var imageFingerprint = regexp.MustCompile(`^[a-f0-9]{64}$`)

var errBaseMismatch = errors.New("image ownership or immutable recipe mismatch")

type BaseImage struct {
	Key          string    `json:"key"`
	Environment  string    `json:"environment"`
	Recipe       string    `json:"recipe"`
	Upstream     string    `json:"upstream"`
	Architecture string    `json:"architecture"`
	Manifest     string    `json:"manifest"`
	Fingerprint  string    `json:"fingerprint"`
	CreatedAt    time.Time `json:"created_at"`
	LastUsedAt   time.Time `json:"last_used_at"`
	Size         uint64    `json:"compressed_bytes"`
	Unusable     bool      `json:"unusable,omitempty"`
}

type BaseBuild struct {
	Key         string `json:"key"`
	Environment string `json:"environment"`
	Project     string `json:"project"`
	Memory      uint64 `json:"memory_bytes"`
	Disk        uint64 `json:"disk_peak_bytes"`
}

type ImageRegistry struct {
	SchemaVersion int         `json:"schema_version"`
	Owner         string      `json:"owner"`
	Bases         []BaseImage `json:"bases"`
	Build         *BaseBuild  `json:"build,omitempty"`
	RetryAfter    time.Time   `json:"retry_after,omitempty"`
	LastError     string      `json:"last_error,omitempty"`
	RetryAttempt  uint64      `json:"retry_attempt,omitempty"`
}

type incusImage struct {
	Fingerprint  string            `json:"fingerprint"`
	Architecture string            `json:"architecture"`
	Type         string            `json:"type"`
	Size         uint64            `json:"size"`
	Public       bool              `json:"public"`
	Properties   map[string]string `json:"properties"`
	Aliases      []struct {
		Name string `json:"name"`
	} `json:"aliases"`
}

type incusOperation struct {
	Class       string `json:"class"`
	Description string `json:"description"`
	StatusCode  int    `json:"status_code"`
}

func (rt *Runtime) imageRegistryPath() string {
	return filepath.Join(rt.Config.StateDir, "images.json")
}

func (rt *Runtime) imageRegistry() (ImageRegistry, error) {
	var registry ImageRegistry
	body, err := os.ReadFile(rt.imageRegistryPath())
	if os.IsNotExist(err) {
		owner, err := randomToken(16)
		return ImageRegistry{SchemaVersion: 1, Owner: owner}, err
	}
	if err != nil {
		return registry, err
	}
	if err := json.Unmarshal(body, &registry); err != nil {
		return registry, err
	}
	if registry.SchemaVersion != 1 || len(registry.Owner) != 32 {
		return registry, errors.New("invalid image ownership registry")
	}
	seen := map[string]bool{}
	for _, base := range registry.Bases {
		if !imageFingerprint.MatchString(base.Key) || !imageFingerprint.MatchString(base.Fingerprint) || seen[base.Key] {
			return registry, errors.New("invalid image registry entry")
		}
		seen[base.Key] = true
	}
	return registry, nil
}

// A single broker builder also serializes publish/prune. The nonblocking lock
// makes waiting for somebody else's build cancellable without killing its work.
func imageBuildLock(ctx context.Context, path string, operation func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			return err
		}
		if err := sleepContext(ctx, 250*time.Millisecond); err != nil {
			return err
		}
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return operation()
}

func imageKey(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:])
}

func imageArchitecture() string {
	if runtime.GOARCH == "amd64" {
		return "x86_64"
	}
	if runtime.GOARCH == "arm64" {
		return "aarch64"
	}
	return runtime.GOARCH
}

func (rt *Runtime) recipeDigest(name string) (string, error) {
	if rt.Config.RecipeRoot == "" {
		return "", errors.New("prepared environment recipes are not installed")
	}
	root := rt.Config.RecipeRoot
	// This manifest is installed from an explicit public-file allowlist. No
	// checkout or operator config is ever copied into a builder.
	body, err := os.ReadFile(filepath.Join(root, "manifest.sha256"))
	if err != nil {
		return "", err
	}
	return imageKey(name, rt.Config.Image, string(body), guestDependencyRevision), nil
}

func (rt *Runtime) prepareEnvironment(ctx context.Context, store LeaseStore, grant LeaseGrant, child *Runtime) (LeaseGrant, error) {
	if grant.Environment == nil {
		return grant, errors.New("named environment required")
	}
	if grant.Environment.Name == EnvironmentAndroid && runtime.GOARCH != "amd64" {
		return grant, errors.New("android-test requires x86_64")
	}
	pinned, err := rt.pinnedRecipeRuntime()
	if err != nil {
		return grant, err
	}
	rt = pinned
	ctx, cancel := context.WithTimeout(ctx, ProvisioningTTL-provisioningSafetyMargin)
	defer cancel()
	err = imageBuildLock(ctx, rt.imageRegistryPath()+".lock", func() error {
		registry, err := rt.imageRegistry()
		if err != nil {
			return err
		}
		if err := writeJSONAtomic(rt.imageRegistryPath(), registry); err != nil {
			return err
		}
		if registry.Build != nil {
			if rt.Now().Before(registry.RetryAfter) {
				return errors.New("builder cleanup retry is scheduled")
			}
			if err := rt.cleanupImageBuild(ctx, store, &registry); err != nil {
				return err
			}
		}
		if err := rt.pruneImages(ctx, store, &registry, false); err != nil {
			return err
		}
		if err := rt.checkCacheBudget(ctx); err != nil {
			return err
		}
		if err := rt.reserveEnvironment(ctx, store, grant); err != nil {
			return err
		}
		recipe, err := rt.recipeDigest(grant.Environment.Name)
		if err != nil {
			return err
		}
		upstream, _, upstreamErr := rt.upstreamImage(ctx)
		if upstreamErr != nil {
			registry.LastError = "upstream_lookup_failed"
			if err := writeJSONAtomic(rt.imageRegistryPath(), registry); err != nil {
				return errors.Join(upstreamErr, err)
			}
		} else if registry.LastError == "upstream_lookup_failed" {
			registry.LastError = ""
		}
		var base *BaseImage
		for i := range registry.Bases {
			candidate := &registry.Bases[i]
			if !candidate.Unusable && candidate.Environment == grant.Environment.Name && candidate.Recipe == recipe &&
				(upstreamErr != nil || candidate.Upstream == upstream.Fingerprint) &&
				candidate.Architecture == imageArchitecture() && !rt.Now().Before(candidate.CreatedAt) &&
				rt.Now().Sub(candidate.CreatedAt) < baseMaxAge && (base == nil || candidate.CreatedAt.After(base.CreatedAt)) {
				base = candidate
			}
		}
		if base != nil {
			usable, err := rt.cachedBaseUsable(ctx, registry.Owner, *base)
			if err != nil {
				return err
			}
			if !usable {
				base.Unusable = true
				if err := writeJSONAtomic(rt.imageRegistryPath(), registry); err != nil {
					return err
				}
				base = nil
			}
		}
		if base == nil {
			if upstreamErr != nil {
				return upstreamErr
			}
			if rt.Now().Before(registry.RetryAfter) {
				return errors.New("base build retry is scheduled; retry after backoff")
			}
			built, err := rt.buildBase(ctx, store, grant, &registry, recipe)
			if err != nil {
				registry.LastError = "base_build_failed"
				var capacity *CapacityError
				if errors.As(err, &capacity) {
					registry.LastError = "capacity_" + capacity.Resource
				} else {
					registry.RetryAttempt++
					registry.RetryAfter = rt.Now().Add(recoveryDelay(registry.RetryAttempt))
				}
				_ = writeJSONAtomic(rt.imageRegistryPath(), registry)
				return err
			}
			registry.LastError = ""
			registry.RetryAttempt = 0
			registry.Bases = append(registry.Bases, built)
			base = &registry.Bases[len(registry.Bases)-1]
		}
		base.LastUsedAt = rt.Now()
		if err := writeJSONAtomic(rt.imageRegistryPath(), registry); err != nil {
			return err
		}
		// Persist the pin before exposing the image to instance creation.
		if err := store.withLock(true, func(pool *LeasePool) error {
			slot, err := findSlot(pool, grant.SlotID)
			if err != nil {
				return err
			}
			if slot.State != SlotProvisioning || slot.LeaseID != grant.LeaseID || slot.LeaseEpoch != grant.LeaseEpoch {
				return ErrLeaseLost
			}
			slot.BaseFingerprint = base.Fingerprint
			return nil
		}); err != nil {
			return err
		}
		grant.BaseFingerprint = base.Fingerprint
		child.Config.Image = base.Fingerprint
		return nil
	})
	return grant, err
}

func (rt *Runtime) listImages(ctx context.Context, arguments ...string) ([]incusImage, error) {
	args := append([]string{"image", "list"}, arguments...)
	args = append(args, "--format", "json")
	body, err := rt.incus(ctx, args...)
	if err != nil {
		return nil, err
	}
	var images []incusImage
	if err := json.Unmarshal([]byte(body), &images); err != nil {
		return nil, err
	}
	if images == nil {
		return nil, errors.New("invalid image inventory")
	}
	return images, nil
}

func (rt *Runtime) upstreamImage(ctx context.Context) (incusImage, string, error) {
	remote, selector, hasRemote := strings.Cut(rt.Config.Image, ":")
	if !hasRemote {
		selector = remote
		remote = "local"
	}
	images, err := rt.listImages(ctx, remote+":", selector, "type=virtual-machine")
	if err != nil {
		return incusImage{}, "", err
	}
	var selected *incusImage
	for i := range images {
		candidate := &images[i]
		match := candidate.Fingerprint == selector
		for _, alias := range candidate.Aliases {
			match = match || alias.Name == selector
		}
		if !match || candidate.Type != "virtual-machine" || candidate.Architecture != imageArchitecture() {
			continue
		}
		if selected != nil {
			return incusImage{}, "", errors.New("ambiguous upstream image")
		}
		selected = candidate
	}
	if selected == nil || !imageFingerprint.MatchString(selected.Fingerprint) {
		return incusImage{}, "", errors.New("upstream VM image fingerprint unavailable")
	}
	return *selected, remote + ":" + selected.Fingerprint, nil
}

func (rt *Runtime) verifyBase(ctx context.Context, owner string, base BaseImage) error {
	body, err := rt.incus(ctx, "query", "/1.0/images/"+base.Fingerprint+"?project=default")
	if err != nil {
		return err
	}
	var image incusImage
	if err := json.Unmarshal([]byte(body), &image); err != nil {
		return err
	}
	if image.Fingerprint != base.Fingerprint || image.Type != "virtual-machine" || image.Public ||
		image.Architecture != base.Architecture || image.Properties["user.subyard.base_owner"] != owner ||
		image.Properties["user.subyard.base_key"] != base.Key || image.Properties["user.subyard.base_recipe"] != base.Recipe {
		return errBaseMismatch
	}
	return nil
}

func (rt *Runtime) cachedBaseUsable(ctx context.Context, owner string, base BaseImage) (bool, error) {
	images, err := rt.listImages(ctx, "local:", base.Fingerprint, "--project", "default")
	if err != nil {
		return false, err
	}
	if len(images) == 0 {
		return false, nil
	}
	if len(images) != 1 || images[0].Fingerprint != base.Fingerprint {
		return false, errors.New("ambiguous cached image inventory")
	}
	err = rt.verifyBase(ctx, owner, base)
	if errors.Is(err, errBaseMismatch) {
		return false, nil
	}
	return err == nil, err
}

func (rt *Runtime) buildBase(ctx context.Context, store LeaseStore, grant LeaseGrant, registry *ImageRegistry, recipe string) (BaseImage, error) {
	var result BaseImage
	upstream, source, err := rt.upstreamImage(ctx)
	if err != nil {
		return result, err
	}
	buildKey := imageKey(recipe, upstream.Fingerprint, rt.Now().UTC().Format(time.RFC3339Nano))
	spec := *grant.Environment
	spec.Count = 1 // Builder is private, never an allocation or grant.
	cfg := rt.Config.withEnvironment(spec)
	cfg.Project = rt.Config.Project + "-builder"
	cfg.Prefix = "e2e-base"
	cfg.Network = "incusbr0"
	cfg.StateDir = filepath.Join(rt.Config.StateDir, "builder")
	cfg.Image = source
	cfg.AgentPublicKey = ""
	builder := &Runtime{Config: cfg, Runner: rt.Runner, Stdout: rt.Stdout, Stderr: rt.Stderr, Now: rt.Now, Sleep: rt.Sleep}
	buildRAM, buildDisk := environmentCommitment(spec, budgetBytes(rt.Config.VMOverhead, "512MiB"))
	// Keep room for builder root plus publication/unpacked image cache.
	buildDisk *= 3
	if err := rt.admitBuild(ctx, store, grant, buildRAM, buildDisk, registry); err != nil {
		return result, err
	}
	registry.Build = &BaseBuild{Key: buildKey, Environment: spec.Name, Project: cfg.Project, Memory: buildRAM, Disk: buildDisk}
	if err := writeJSONAtomic(rt.imageRegistryPath(), registry); err != nil {
		return result, err
	}
	if err := builder.ensureBuilderProject(ctx, registry.Owner); err != nil {
		return result, err
	}
	if err := builder.ensureProject(ctx); err != nil {
		return result, err
	}
	vm := cfg.vm(1)
	if builder.vmExists(ctx, vm) {
		return result, errors.New("builder instance exists without current staging verification")
	}
	const cloud = `#cloud-config
users:
  - name: dev
    groups: [sudo]
    shell: /bin/bash
    sudo: ALL=(ALL) NOPASSWD:ALL
    lock_passwd: true
ssh_pwauth: false
`
	if _, err := builder.incus(ctx, "init", source, vm, "--vm", "--project", cfg.Project,
		"-c", "limits.cpu="+fmt.Sprint(spec.CPU), "-c", "limits.memory="+spec.Memory,
		"-c", "user.subyard.managed="+managedMarker, "-c", "user.subyard.base_owner="+registry.Owner,
		"-c", "user.subyard.base_key="+buildKey, "-c", "cloud-init.user-data="+cloud); err != nil {
		return result, err
	}
	if err := builder.startVM(ctx, vm); err != nil {
		return result, err
	}
	if err := builder.waitAgent(ctx, vm); err != nil {
		return result, err
	}
	if err := builder.ensureGuestTools(ctx, vm); err != nil {
		return result, err
	}
	archive := filepath.Join(rt.Config.RecipeRoot, "recipe.tar.gz")
	if _, err := builder.incus(ctx, "file", "push", archive, vm+"/tmp/subyard-base-recipe.tar.gz", "--project", cfg.Project); err != nil {
		return result, err
	}
	if _, err := builder.guest(ctx, vm, nil, "sh", "-eu", "-c", `install -d -m 0700 /opt/subyard-base-recipe
tar -xzf /tmp/subyard-base-recipe.tar.gz -C /opt/subyard-base-recipe
rm -f /tmp/subyard-base-recipe.tar.gz
cd /opt/subyard-base-recipe
sha256sum -c manifest.sha256 >/dev/null`); err != nil {
		return result, err
	}
	if _, err := builder.guest(ctx, vm, []string{"SUBYARD_BASE_ENVIRONMENT=" + spec.Name},
		"timeout", "--kill-after=10", "1800", "bash", "/opt/subyard-base-recipe/scripts/e2e-lab/base.sh"); err != nil {
		return result, err
	}
	manifest, err := builder.guest(ctx, vm, nil, "cat", "/var/lib/subyard/base-manifest.sha256")
	if err != nil || !imageFingerprint.MatchString(strings.TrimSpace(manifest)) {
		return result, errors.New("verified base dependency manifest missing")
	}
	result = BaseImage{Environment: spec.Name, Recipe: recipe, Upstream: upstream.Fingerprint, Architecture: upstream.Architecture,
		Manifest: strings.TrimSpace(manifest), CreatedAt: rt.Now(), LastUsedAt: rt.Now()}
	result.Key = imageKey(buildKey, result.Manifest)
	// The builder never had lease keys or a checkout. Remove generated machine
	// identities only after every dependency check, immediately before shutdown.
	const sanitize = `set -eu
cloud-init clean --logs --machine-id
rm -f /etc/ssh/ssh_host_* /var/lib/dbus/machine-id
find /root /home/dev -maxdepth 2 -type d -name .ssh -exec rm -rf -- {} +
rm -rf /opt/subyard-base-recipe
truncate -s 0 /etc/machine-id
sync`
	if _, err := builder.guest(ctx, vm, nil, "sh", "-c", sanitize); err != nil {
		return result, err
	}
	if err := builder.stopRunningVM(ctx, vm); err != nil {
		return result, err
	}
	if _, err := builder.incus(ctx, "publish", vm, "--project", cfg.Project,
		"user.subyard.base_owner="+registry.Owner, "user.subyard.base_key="+result.Key,
		"user.subyard.base_recipe="+recipe, "user.subyard.build_key="+buildKey); err != nil {
		return result, err
	}
	images, err := rt.listImages(ctx, "local:", "user.subyard.base_key="+result.Key, "--project", "default")
	if err != nil {
		return result, err
	}
	if len(images) != 1 {
		return result, errors.New("published image could not be uniquely verified")
	}
	result.Fingerprint, result.Size = images[0].Fingerprint, images[0].Size
	if err := rt.verifyBase(ctx, registry.Owner, result); err != nil {
		return result, err
	}
	// Physical work has happened; report an ordinary preparation failure so
	// the current allocation is quarantined, never a pre-mutation capacity abort.
	if err := rt.checkCacheBudget(ctx); err != nil {
		return result, errors.New("published image cache exceeds budget or cannot be measured")
	}
	// Commit before removing staging: a crash can recover either side safely.
	registry.Bases = append(registry.Bases, result)
	if err := writeJSONAtomic(rt.imageRegistryPath(), registry); err != nil {
		return result, err
	}
	if err := rt.cleanupImageBuild(ctx, store, registry); err != nil {
		return result, err
	}
	registry.Bases = registry.Bases[:len(registry.Bases)-1] // Caller publishes current selection.
	registry.RetryAfter = time.Time{}
	return result, nil
}

func (rt *Runtime) admitBuild(ctx context.Context, store LeaseStore, pending LeaseGrant, ram, disk uint64, registry *ImageRegistry) error {
	return store.withLock(true, func(pool *LeasePool) error {
		memory, err := rt.readMemoryCapacity()
		if err != nil {
			return &CapacityError{"memory", "builder memory telemetry unavailable"}
		}
		memoryScope := memory.scope
		storageBefore, err := rt.storageCapacity(ctx)
		if err != nil {
			return &CapacityError{"disk", "builder storage telemetry unavailable"}
		}
		memoryBefore := memory.Available
		overhead := budgetBytes(rt.Config.VMOverhead, "512MiB")
		if pending.SlotID != "" {
			slot, err := findSlot(pool, pending.SlotID)
			if err != nil {
				return err
			}
			if slot.State != SlotProvisioning || !slot.Reserved || slot.Environment == nil || slot.LeaseID != pending.LeaseID || slot.LeaseEpoch != pending.LeaseEpoch || slot.ResourceGeneration != pending.ResourceGeneration || slot.CapabilityHash != capabilityDigest(pending.Capability) {
				return ErrLeaseLost
			}
			futureRAM, futureDisk := environmentCommitment(*slot.Environment, overhead)
			// The builder and this allocation's working VMs are sequential under
			// the image lock. Reserve their larger peak, not both simultaneously.
			if futureRAM > ram {
				ram = futureRAM
			}
			if futureDisk > disk {
				disk = futureDisk
			}
		}
		for _, slot := range pool.Slots {
			if slot.SlotID == pending.SlotID || slot.State == SlotAvailable || slot.State == SlotUnavailable {
				continue
			}
			if slot.Environment != nil && !slot.Reserved {
				continue
			}
			m, d, err := rt.outstandingCommitment(ctx, slot, overhead, memoryScope)
			if err != nil {
				return err
			}
			ram += m
			disk += d
		}

		if err := rt.checkCacheBudget(ctx); err != nil {
			return err
		}
		memory, err = rt.readMemoryCapacity()
		if err != nil || memory.scope != memoryScope {
			return &CapacityError{"memory", "builder memory telemetry unavailable"}
		}
		storage, err := rt.storageCapacity(ctx)
		if err != nil {
			return &CapacityError{"disk", "builder storage telemetry unavailable"}
		}
		memory.Available = min(memoryBefore, memory.Available)
		storage.Total = min(storageBefore.Total, storage.Total)
		storage.Used = max(storageBefore.Used, storage.Used)
		storage.BudgetUsed = max(storageBefore.BudgetUsed, storage.BudgetUsed)
		return checkCapacity(memory, storage, ram, disk, budgetBytes(rt.Config.MemoryReserve, "2GiB"),
			budgetBytes(rt.Config.DiskReserve, "5GiB"), budgetBytes(rt.Config.DiskBudget, "160GiB"))
	})
}

func (rt *Runtime) cleanupImageBuild(ctx context.Context, store LeaseStore, registry *ImageRegistry) error {
	build := registry.Build
	if build == nil {
		return nil
	}
	if build.Project != rt.Config.Project+"-builder" || !imageFingerprint.MatchString(build.Key) {
		return errors.New("unrecognized builder staging")
	}
	cfg := rt.Config
	cfg.Project = build.Project
	cfg.Prefix = "e2e-base"
	builder := &Runtime{Config: cfg, Runner: rt.Runner, Stdout: rt.Stdout, Stderr: rt.Stderr, Now: rt.Now, Sleep: rt.Sleep}
	exists, err := builder.projectPresence(ctx)
	if err != nil {
		return err
	}
	if exists {
		if err := builder.requireProjectMarker(ctx); err != nil {
			return err
		}
		owner, err := builder.incus(ctx, "project", "get", build.Project, "user.subyard.base_owner")
		if err != nil {
			return err
		}
		if strings.TrimSpace(owner) != registry.Owner {
			return errors.New("foreign builder project")
		}
		// Incus publish creates a server-side task that can outlive a killed
		// client. The task has no source resource until publication completes,
		// so only the dedicated, marker-owned builder project scopes it safely.
		body, err := builder.incus(ctx, "operation", "list", "--project", build.Project, "--format", "json")
		if err != nil {
			return err
		}
		var operations []incusOperation
		if err := json.Unmarshal([]byte(body), &operations); err != nil || operations == nil {
			return errors.New("builder operation inventory unavailable")
		}
		for _, operation := range operations {
			if operation.Class == "task" && operation.Description == "Downloading image" && operation.StatusCode < 200 {
				return errors.New("builder image publication still in progress")
			}
		}
		names, err := builder.projectInstances(ctx)
		if err != nil {
			return err
		}
		for _, name := range names {
			if name != cfg.vm(1) {
				return errors.New("foreign builder instance")
			}
			for property, want := range map[string]string{"user.subyard.managed": managedMarker, "user.subyard.base_owner": registry.Owner, "user.subyard.base_key": build.Key} {
				got, err := builder.incus(ctx, "config", "get", name, property, "--project", cfg.Project)
				if err != nil {
					return err
				}
				if strings.TrimSpace(got) != want {
					return errors.New("builder ownership mismatch")
				}
			}
			_, stopErr := builder.incus(ctx, "stop", name, "--force", "--project", cfg.Project)
			state, stateErr := builder.incus(ctx, "list", name, "--project", cfg.Project, "-f", "csv", "-c", "s")
			if stateErr != nil {
				return errors.Join(stopErr, stateErr)
			}
			if strings.TrimSpace(state) != "STOPPED" {
				return errors.Join(stopErr, errors.New("builder did not stop"))
			}

			if _, err := builder.incus(ctx, "delete", name, "--project", cfg.Project); err != nil {
				return err
			}
		}
		remaining, err := builder.projectInstances(ctx)
		if err != nil {
			return err
		}
		if len(remaining) != 0 {
			return errors.New("builder instances remain after cleanup")
		}
	}
	// A crash after publish but before the registry commit can leave a complete
	// image. Only images carrying this exact staging identity are ours to remove.
	published, err := rt.listImages(ctx, "local:", "user.subyard.build_key="+build.Key, "--project", "default")
	if err != nil {
		return err
	}
	pins, err := rt.imageReferences(ctx, store)
	if err != nil {
		return err
	}
	for _, image := range published {
		if image.Properties["user.subyard.build_key"] != build.Key || image.Properties["user.subyard.base_owner"] != registry.Owner || !imageFingerprint.MatchString(image.Fingerprint) {
			return errors.New("unrecognized image in builder staging")
		}
		committed := false
		for _, base := range registry.Bases {
			committed = committed || base.Fingerprint == image.Fingerprint
		}
		if committed {
			continue
		}
		if pins[image.Fingerprint] || len(image.Aliases) != 0 {
			return errors.New("uncommitted builder image has external references")
		}
		if err := rt.deleteImageAndVerify(ctx, image.Fingerprint); err != nil {
			return err
		}
	}
	registry.Build = nil
	return writeJSONAtomic(rt.imageRegistryPath(), registry)
}

func (rt *Runtime) pruneImages(ctx context.Context, store LeaseStore, registry *ImageRegistry, _ bool) error {
	pool, err := store.Inspect()
	if err != nil {
		return err
	}
	pins := map[string]bool{}
	for _, slot := range pool.Slots {
		if slot.BaseFingerprint != "" {
			pins[slot.BaseFingerprint] = true
		}
	}
	latest := map[string]string{}
	sort.Slice(registry.Bases, func(i, j int) bool { return registry.Bases[i].CreatedAt.After(registry.Bases[j].CreatedAt) })
	for _, base := range registry.Bases {
		if !base.Unusable && latest[base.Environment] == "" {
			latest[base.Environment] = base.Key
		}
	}
	var references map[string]bool
	for i := len(registry.Bases) - 1; i >= 0; i-- {
		base := registry.Bases[i]
		// Keep the current image even after expiry. A failed refresh must not
		// remove it; eligibility for issuing an image is checked separately.
		if pins[base.Fingerprint] || latest[base.Environment] == base.Key {
			continue
		}
		images, err := rt.listImages(ctx, "local:", base.Fingerprint, "--project", "default")
		if err != nil {
			return err
		}
		if len(images) > 1 || (len(images) == 1 && images[0].Fingerprint != base.Fingerprint) {
			return errors.New("ambiguous image inventory")
		}
		if len(images) == 1 {
			if err := rt.verifyBase(ctx, registry.Owner, base); err != nil {
				if errors.Is(err, errBaseMismatch) {
					// Changed objects are no longer deletion candidates. Keep the
					// evidence without blocking another healthy generation.
					continue
				}
				return err
			}
			if len(images[0].Aliases) != 0 {
				continue
			}
			if references == nil {
				references, err = rt.imageReferences(ctx, store)
				if err != nil {
					return err
				}
			}
			if references[base.Fingerprint] {
				continue
			}
			if err := rt.deleteImageAndVerify(ctx, base.Fingerprint); err != nil {
				return err
			}
		}
		// Absence also reconciles a crash after deletion but before this write.
		registry.Bases = append(registry.Bases[:i], registry.Bases[i+1:]...)
		if err := writeJSONAtomic(rt.imageRegistryPath(), registry); err != nil {
			return err
		}
	}
	return nil
}

func (rt *Runtime) ensureBuilderProject(ctx context.Context, owner string) error {
	exists, err := rt.projectPresence(ctx)
	if err != nil {
		return err
	}
	if exists {
		if err := rt.requireProjectMarker(ctx); err != nil {
			return err
		}
		marker, err := rt.incus(ctx, "project", "get", rt.Config.Project, "user.subyard.base_owner")
		if err != nil {
			return err
		}
		if strings.TrimSpace(marker) != owner {
			return errors.New("foreign builder project")
		}
		names, err := rt.projectInstances(ctx)
		if err != nil {
			return err
		}
		if len(names) != 0 {
			return errors.New("builder staging must be cleaned before reuse")
		}
		return nil
	}
	// Ownership is present from the first durable physical object, so restart
	// cleanup never has to adopt an unmarked project after an interrupted build.
	_, err = rt.incus(ctx, "project", "create", rt.Config.Project,
		"-c", "features.images=false", "-c", "user.subyard.managed="+managedMarker,
		"-c", "user.subyard.base_owner="+owner, "-c", "restricted=true",
		"-c", "limits.cpu="+fmt.Sprint(rt.Config.CPU), "-c", "limits.memory="+rt.Config.Memory)
	return err
}

func (rt *Runtime) imageReferences(ctx context.Context, store LeaseStore) (map[string]bool, error) {
	pool, err := store.Inspect()
	if err != nil {
		return nil, err
	}
	references := map[string]bool{}
	for _, slot := range pool.Slots {
		if slot.BaseFingerprint != "" {
			references[slot.BaseFingerprint] = true
		}
	}
	body, err := rt.incus(ctx, "list", "--all-projects", "--format", "json")
	if err != nil {
		return nil, err
	}
	var instances []struct {
		Config         map[string]string `json:"config"`
		ExpandedConfig map[string]string `json:"expanded_config"`
	}
	if err := json.Unmarshal([]byte(body), &instances); err != nil {
		return nil, err
	}
	if instances == nil {
		return nil, errors.New("invalid instance reference inventory")
	}
	for _, instance := range instances {
		for _, cfg := range []map[string]string{instance.Config, instance.ExpandedConfig} {
			if fingerprint := cfg["volatile.base_image"]; fingerprint != "" {
				references[fingerprint] = true
			}
		}
	}
	return references, nil
}

func (rt *Runtime) deleteImageAndVerify(ctx context.Context, fingerprint string) error {
	if _, err := rt.incus(ctx, "image", "delete", fingerprint, "--project", "default"); err != nil {
		return err
	}
	remaining, err := rt.listImages(ctx, "local:", fingerprint, "--project", "default")
	if err != nil {
		return err
	}
	if len(remaining) != 0 {
		return errors.New("image remains after deletion")
	}
	return nil
}

// Resolve a versioned recipe symlink once per build request. A concurrent setup
// may switch the public link but cannot change the recipe this request hashes.
func (rt *Runtime) pinnedRecipeRuntime() (*Runtime, error) {
	if rt.Config.RecipeRoot == "" {
		return nil, errors.New("prepared environment recipes are not installed")
	}
	resolved, err := filepath.EvalSymlinks(rt.Config.RecipeRoot)
	if err != nil {
		return nil, err
	}
	pinned := *rt
	pinned.Config.RecipeRoot = resolved
	return &pinned, nil
}

// ReapImages never waits for an active builder. Lease expiry remains the first
// reaper duty; idle cache maintenance has its own short physical-operation bound.
func (rt *Runtime) ReapImages(ctx context.Context, store LeaseStore) error {
	rt.prepareDefaults()
	if _, err := os.Stat(rt.imageRegistryPath()); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	lock, err := os.OpenFile(rt.imageRegistryPath()+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return nil
		}
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	registry, err := rt.imageRegistry()
	if err != nil {
		return err
	}
	if registry.Build != nil {
		if rt.Now().Before(registry.RetryAfter) {
			return nil
		}
		if err := rt.cleanupImageBuild(ctx, store, &registry); err != nil {
			registry.LastError = "base_cleanup_failed"
			registry.RetryAttempt++
			registry.RetryAfter = rt.Now().Add(recoveryDelay(registry.RetryAttempt))
			return errors.Join(err, writeJSONAtomic(rt.imageRegistryPath(), registry))
		}
	}
	return rt.pruneImages(ctx, store, &registry, false)
}

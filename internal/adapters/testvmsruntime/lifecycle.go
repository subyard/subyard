package testvmsruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// VM disks are thin-provisioned; preserve real host headroom instead of summing virtual maxima.
	HostReserveBytes = uint64(5 * 1024 * 1024 * 1024)

	// Keep best-effort trim below the facade request lifetime so verified stop retains its budget.
	retainedGuestTrimTimeout      = 15 * time.Second
	retainedGuestTrimTotalTimeout = 30 * time.Second
)

func (runtime *Runtime) AcquireSlot(
	ctx context.Context, store LeaseStore, grant LeaseGrant, publicKey string,
) (LeaseGrant, error) {
	runtime.prepareDefaults()
	slot, err := slotNumber(grant.SlotID, runtime.Config.SlotCount)
	if err != nil {
		return grant, err
	}
	acquired := grant
	err = withFileLock(runtime.slotLifecycleLock(grant.SlotID), func() error {
		var acquireErr error
		acquired, acquireErr = runtime.acquireSlotLocked(ctx, store, grant, publicKey, slot)
		return acquireErr
	})
	return acquired, err
}

func (runtime *Runtime) acquireSlotLocked(
	ctx context.Context,
	store LeaseStore,
	grant LeaseGrant,
	publicKey string,
	slot int,
) (LeaseGrant, error) {
	child := runtime.slotRuntime(slot, publicKey)
	if grant.Environment == nil {
		return grant, errors.New("explicit disposable environment is required")
	}
	child.Config = child.Config.withEnvironment(*grant.Environment)
	child.allocation = &LeaseIdentity{SlotID: grant.SlotID, ResourceGeneration: grant.ResourceGeneration, LeaseEpoch: grant.LeaseEpoch}
	stored, err := storeSlot(store, grant.SlotID)
	if err != nil {
		return grant, err
	}
	if stored.LegacyRetained {
		exists, err := child.projectPresence(ctx)
		if err != nil {
			return grant, err
		}
		if exists {
			if err := child.requireProjectMarker(ctx); err != nil {
				return grant, err
			}
			names, err := child.projectInstances(ctx)
			if err != nil {
				return grant, err
			}
			if len(names) != 0 {
				return grant, ErrLegacyRetained
			}
		}
		if err := store.mutateOwned(grant, func(slot *LeaseSlot, _ time.Time) error { slot.LegacyRetained = false; return nil }); err != nil {
			return grant, err
		}
	}
	grant, err = runtime.prepareEnvironment(ctx, store, grant, child)
	if err != nil {
		return grant, err
	}
	if err := child.ensureSlotNetwork(ctx); err != nil {
		return grant, err
	}
	if err := child.provisionPair(ctx); err != nil {
		return grant, err
	}
	// Optimized drivers may materialize their native image cache during init.
	// This check occurs before opening forwarding or returning a held grant.
	if err := runtime.checkCacheBudget(ctx); err != nil {
		return grant, errors.New("working image cache exceeds budget or cannot be measured")
	}
	for selector := 1; selector <= child.Config.guestCount(); selector++ {
		if err := child.installLeaseContext(ctx, child.Config.vm(selector), grant); err != nil {
			return grant, err
		}
	}
	grant.DataUser = child.Config.AgentUser
	for selector := 1; selector <= child.Config.guestCount(); selector++ {
		vm := child.Config.vm(selector)
		address, err := child.vmIP(ctx, vm)
		if err != nil {
			return grant, err
		}
		key, err := child.guestHostKey(ctx, vm)
		if err != nil {
			return grant, err
		}
		keyType, keyBlob, ok := strings.Cut(key, " ")
		if !ok {
			return grant, errors.New("invalid guest host key")
		}
		grant.Targets = append(grant.Targets, LeaseTarget{
			Selector: selector, Name: vm, Address: address,
			HostKeyType: keyType, HostKeyBlob: keyBlob,
		})
	}
	if err := child.enableAgentAccess(ctx); err != nil {
		return grant, err
	}
	expires, err := store.MarkHeld(grant)
	if err != nil {
		_ = child.stopRetained(ctx)
		return grant, err
	}
	grant.ExpiresAt = expires
	return grant, nil
}

func (runtime *Runtime) ensureSlotNetwork(ctx context.Context) error {
	cfg := runtime.Config
	if _, err := runtime.incus(ctx, "network", "show", cfg.Network, "--project", "default"); err != nil {
		if _, createErr := runtime.incus(ctx, "network", "create", cfg.Network,
			"ipv4.address=auto", "ipv6.address=none",
			"user.subyard.managed="+managedMarker, "--project", "default"); createErr != nil {
			return createErr
		}
	}
	marker, err := runtime.incus(ctx, "network", "get", cfg.Network,
		"user.subyard.managed", "--project", "default")
	if err != nil {
		return err
	}
	if strings.TrimSpace(marker) != managedMarker {
		return fmt.Errorf("network %q exists without the Subyard marker", cfg.Network)
	}
	return nil
}

func (runtime *Runtime) ReleaseSlot(
	ctx context.Context,
	store LeaseStore,
	grant LeaseGrant,
) error {
	runtime.prepareDefaults()
	if _, err := slotNumber(grant.SlotID, runtime.Config.SlotCount); err != nil {
		return err
	}
	return withFileLock(
		runtime.slotLifecycleLock(grant.SlotID),
		func() error {
			if err := store.BeginDrain(grant); err != nil {
				return err
			}
			slot, err := storeSlot(store, grant.SlotID)
			if err != nil {
				return err
			}
			_, _ = runtime.eventRecorder().Record(BrokerEvent{
				Kind:               "lease.release",
				SlotID:             slot.SlotID,
				ResourceGeneration: slot.ResourceGeneration,
				LeaseEpoch:         slot.LeaseEpoch,
				FromState:          SlotHeld,
				ToState:            SlotDraining,
				Context:            leaseContextFromSlot(slot),
			})
			return runtime.completeDrain(ctx, store, slot)
		},
	)
}

func (runtime *Runtime) ReapExpired(ctx context.Context, store LeaseStore) error {
	runtime.prepareDefaults()
	pool, err := store.Status()
	if err != nil {
		return err
	}
	var result error
	for _, slot := range pool.Slots {
		if slot.State != SlotDraining {
			continue
		}
		if drainErr := runtime.drainSlot(ctx, store, slot.SlotID); drainErr != nil {
			result = errors.Join(result, drainErr)
		}
	}
	pool, err = store.Status()
	if err != nil {
		return errors.Join(result, err)
	}
	now := runtime.Now().UTC()
	for _, slot := range pool.Slots {
		if !dueForRecovery(slot, now) {
			continue
		}
		if recoveryErr := runtime.RecoverScheduled(
			ctx,
			store,
			slot.SlotID,
			false,
		); recoveryErr != nil {
			result = errors.Join(result, recoveryErr)
		}
	}
	return result
}

func (runtime *Runtime) drainSlot(
	ctx context.Context,
	store LeaseStore,
	slotID string,
) error {
	runtime.prepareDefaults()
	if _, err := slotNumber(slotID, runtime.Config.SlotCount); err != nil {
		return err
	}
	return withFileLock(
		runtime.slotLifecycleLock(slotID),
		func() error {
			slot, err := storeSlot(store, slotID)
			if err != nil {
				return err
			}
			if slot.State != SlotDraining {
				return nil
			}
			return runtime.completeDrain(ctx, store, slot)
		},
	)
}

func (runtime *Runtime) completeDrain(
	ctx context.Context,
	store LeaseStore,
	slot LeaseSlot,
) error {
	if runtime.finishDrain != nil {
		return runtime.finishDrain(ctx, store, slot)
	}
	return runtime.finishDrainingSlot(ctx, store, slot)
}

func (runtime *Runtime) finishDrainingSlot(
	ctx context.Context,
	store LeaseStore,
	slot LeaseSlot,
) error {
	kind := "lease.fence"
	fromState := SlotHeld
	if slot.ReadyAt.IsZero() {
		fromState = SlotProvisioning
	}
	if slot.FailureReason == heartbeatExpiredReason ||
		slot.FailureReason == provisioningExpiredReason {
		kind = "lease.expired"
	}
	_, _ = runtime.eventRecorder().Record(BrokerEvent{
		Kind:               kind,
		SlotID:             slot.SlotID,
		ResourceGeneration: slot.ResourceGeneration,
		LeaseEpoch:         slot.LeaseEpoch,
		FromState:          fromState,
		ToState:            SlotDraining,
		Context:            leaseContextFromSlot(slot),
	})
	number, err := slotNumber(slot.SlotID, runtime.Config.SlotCount)
	if err != nil {
		return err
	}
	child := runtime.slotRuntime(number, "")
	child.useLeaseSlot(slot)
	evidence, stopErr := child.stopRetainedWithEvidence(ctx)
	if stopErr == nil && slot.Environment != nil {
		stopErr = child.deleteAllocation(ctx)
	}
	runtime.recordStopOutcome(slot, evidence, stopErr)
	finishErr := store.FinishDrain(slot.SlotID, stopErr)
	if stopErr == nil {
		if finishErr != nil {
			return finishErr
		}
		_, _ = runtime.eventRecorder().Record(BrokerEvent{
			Kind:               "lease.available",
			SlotID:             slot.SlotID,
			ResourceGeneration: slot.ResourceGeneration,
			LeaseEpoch:         slot.LeaseEpoch,
			FromState:          SlotDraining,
			ToState:            SlotAvailable,
			Context:            leaseContextFromSlot(slot),
		})
		return nil
	}
	_, _ = runtime.eventRecorder().Record(BrokerEvent{
		Kind:               "lease.stop_failed",
		SlotID:             slot.SlotID,
		ResourceGeneration: slot.ResourceGeneration,
		LeaseEpoch:         slot.LeaseEpoch,
		FromState:          SlotDraining,
		ToState:            SlotQuarantined,
		Error:              stopErr.Error(),
		Context:            leaseContextFromSlot(slot),
	})
	incidentErr := runtime.handleQuarantineLocked(ctx, store, slot.SlotID, stopErr)
	return errors.Join(finishErr, incidentErr, stopErr)
}

func (runtime *Runtime) DrainAll(ctx context.Context, store LeaseStore, reason string) error {
	if err := store.BeginDrainAll(reason); err != nil {
		return err
	}
	return runtime.ReapExpired(ctx, store)
}

func (runtime *Runtime) RevokeSlot(ctx context.Context, store LeaseStore, slotID string) error {
	if err := store.BeginDrainSlot(slotID, "operator revoke"); err != nil {
		return err
	}
	return runtime.ReapExpired(ctx, store)
}

func (runtime *Runtime) RevokeExpectedSlot(
	ctx context.Context,
	store LeaseStore,
	expected LeaseIdentity,
) error {
	runtime.prepareDefaults()
	if err := expected.validate(); err != nil {
		return err
	}
	if _, err := slotNumber(expected.SlotID, runtime.Config.SlotCount); err != nil {
		return err
	}
	return withFileLock(
		runtime.slotLifecycleLock(expected.SlotID),
		func() error {
			shouldDrain, err := store.BeginExpectedDrain(expected, "operator revoke")
			if err != nil || !shouldDrain {
				return err
			}
			slot, err := storeSlot(store, expected.SlotID)
			if err != nil {
				return err
			}
			if !slotMatchesLeaseIdentity(slot, expected) || slot.State != SlotDraining {
				return fmt.Errorf("%w: slot %s identity changed", ErrLeaseTargetStale, expected.SlotID)
			}
			return runtime.completeDrain(ctx, store, slot)
		},
	)
}

func (runtime *Runtime) slotLifecycleLock(slotID string) string {
	return filepath.Join(runtime.Config.StateDir, "lifecycle", slotID+".lock")
}

func (runtime *Runtime) RecoverSlot(ctx context.Context, store LeaseStore, slotID string) error {
	if _, err := storeSlot(store, slotID); errors.Is(err, ErrCorruptLeaseState) {
		if rebuildErr := store.rebuildCorruptPoolForRecovery(slotID); rebuildErr != nil {
			return rebuildErr
		}
	}
	return runtime.RecoverScheduled(ctx, store, slotID, true)
}

func (runtime *Runtime) ReconcilePool(ctx context.Context, store LeaseStore) error {
	runtime.prepareDefaults()
	current, plannedRetiring, err := store.ResizePlan()
	if err != nil {
		return err
	}
	path := runtime.capacityPath()
	available := runtime.AvailableBytes
	if available == nil {
		available = filesystemAvailableBytes
	}
	free, capacityErr := available(path)
	if capacityErr != nil {
		return fmt.Errorf("inspect test-vms pool capacity: %w", capacityErr)
	}
	fmt.Fprintf(runtime.Stdout,
		"Reconcile test-vms pool: slots %d -> %d, maximum VMs %d, per-VM cpu=%d memory=%s disk=%s, available=%d MiB, host reserve=%d MiB.\n",
		current, runtime.Config.SlotCount, runtime.Config.SlotCount*2,
		runtime.Config.CPU, runtime.Config.Memory, runtime.Config.Disk,
		free/(1024*1024), HostReserveBytes/(1024*1024))
	for _, slot := range plannedRetiring {
		fmt.Fprintf(runtime.Stdout, "  retire %s: delete only its marker-owned stopped pair, project, network, data account and state\n",
			slot.SlotID)
	}
	retiring, err := store.PrepareResize()
	if err != nil {
		return err
	}
	for _, slot := range retiring {
		number, err := slotNumberUnbounded(slot.SlotID)
		if err != nil {
			return err
		}
		if err := runtime.cleanupRetiringSlot(ctx, number); err != nil {
			return fmt.Errorf("cleanup retiring %s: %w", slot.SlotID, err)
		}
	}
	return store.CommitResize()
}

func (runtime *Runtime) cleanupRetiringSlot(ctx context.Context, slot int) error {
	child := runtime.slotRuntime(slot, "")
	child.prepareDefaults()
	exists, err := child.projectPresence(ctx)
	if err != nil {
		return err
	}
	if exists {
		names, err := child.projectInstances(ctx)
		if err != nil {
			return err
		}
		if len(names) != 0 {
			return errors.New("idle slot contains untracked instances; explicit recovery is required before pool shrink")
		}
	}
	if err := child.cleanupManaged(ctx, true); err != nil {
		return err
	}
	if _, err := child.incus(ctx, "network", "show", child.Config.Network,
		"--project", "default"); err == nil {
		marker, err := child.incus(ctx, "network", "get", child.Config.Network,
			"user.subyard.managed", "--project", "default")
		if err != nil {
			return err
		}
		if strings.TrimSpace(marker) != managedMarker {
			return fmt.Errorf("network %q is not Subyard-managed", child.Config.Network)
		}
		if _, err := child.incus(ctx, "network", "delete", child.Config.Network,
			"--project", "default"); err != nil {
			return err
		}
	}
	if _, err := os.Stat(child.Config.StateDir); err == nil {
		marker, err := os.ReadFile(child.Config.stateMarker())
		if err != nil || strings.TrimSpace(string(marker)) != managedMarker {
			return fmt.Errorf("slot state %q is not exactly Subyard-managed",
				child.Config.StateDir)
		}
		if err := os.RemoveAll(child.Config.StateDir); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Stat(child.Config.AgentHome); err == nil {
		marker, markerErr := os.ReadFile(filepath.Join(child.Config.AgentHome,
			".subyard-managed"))
		if markerErr != nil || strings.TrimSpace(string(marker)) != managedMarker {
			return fmt.Errorf("slot account home %q is not exactly Subyard-managed",
				child.Config.AgentHome)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if account, err := user.Lookup(child.Config.AgentUser); err == nil {
		if account.HomeDir != child.Config.AgentHome {
			return fmt.Errorf("slot account %q has unexpected home %q",
				child.Config.AgentUser, account.HomeDir)
		}
		child.killAgentSessions(ctx)
		if _, _, err := child.Runner.Run(ctx, "userdel",
			[]string{child.Config.AgentUser}, nil, nil); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(child.Config.AgentHome); err != nil {
		return err
	}
	return nil
}

func (runtime *Runtime) prepareDefaults() {
	if runtime.Runner == nil {
		runtime.Runner = ProcessRunner{}
	}
	if runtime.Stdout == nil {
		runtime.Stdout = io.Discard
	}
	if runtime.Stderr == nil {
		runtime.Stderr = io.Discard
	}
	if runtime.Now == nil {
		runtime.Now = time.Now
	}
	if runtime.Sleep == nil {
		runtime.Sleep = sleepContext
	}
	if runtime.Events == nil {
		recorder := EventRecorder{
			StateDir: runtime.Config.StateDir,
			Source:   runtime.Config.BrokerSource,
			Now:      runtime.Now,
		}
		runtime.Events = &recorder
	}
}

func (runtime *Runtime) slotRuntime(slot int, publicKey string) *Runtime {
	cfg := runtime.Config
	suffix := strconv.Itoa(slot)
	cfg.Project = cfg.Project + "-slot-" + suffix
	cfg.Network = cfg.Prefix + "-net-" + suffix
	cfg.StateDir = filepath.Join(cfg.StateDir, "slots", suffix)
	cfg.AgentUser = "subyard-e2e-slot-" + suffix
	cfg.AgentHome = filepath.Join("/var/lib/subyard/e2e-slots", suffix)
	cfg.AgentAuthorizedKeys = filepath.Join(cfg.AgentHome, ".ssh", "authorized_keys")
	cfg.AgentPublicKey = publicKey
	return &Runtime{
		Config: cfg, ConfigPath: runtime.ConfigPath, Runner: runtime.Runner,
		Stdout: runtime.Stdout, Stderr: runtime.Stderr, Now: runtime.Now, Sleep: runtime.Sleep,
		AvailableBytes: runtime.AvailableBytes, ExecutablePath: runtime.ExecutablePath,
		Events: runtime.Events,
	}
}

func (runtime *Runtime) capacityPath() string {
	path := runtime.Config.StateDir
	for {
		if _, err := os.Stat(path); err == nil {
			break
		}
		parent := filepath.Dir(path)
		if parent == path {
			path = "/"
			break
		}
		path = parent
	}
	return path
}

func filesystemAvailableBytes(path string) (uint64, error) {
	var stats syscall.Statfs_t
	if err := syscall.Statfs(path, &stats); err != nil {
		return 0, err
	}
	return stats.Bavail * uint64(stats.Bsize), nil
}

func slotNumber(slotID string, maximum int) (int, error) {
	value, err := slotNumberUnbounded(slotID)
	if err != nil || value > maximum {
		return 0, fmt.Errorf("invalid slot id %q", slotID)
	}
	return value, nil
}

func slotNumberUnbounded(slotID string) (int, error) {
	const prefix = "slot-"
	if !strings.HasPrefix(slotID, prefix) {
		return 0, fmt.Errorf("invalid slot id %q", slotID)
	}
	value, err := strconv.Atoi(strings.TrimPrefix(slotID, prefix))
	if err != nil || value < 1 {
		return 0, fmt.Errorf("invalid slot id %q", slotID)
	}
	return value, nil
}

func (runtime *Runtime) stopRetained(ctx context.Context) error {
	_, err := runtime.stopRetainedWithEvidence(ctx)
	return err
}

type stopEvidence struct {
	guestKeyCleanupAttempts int
	guestKeyCleanupDeferred int
}

func (runtime *Runtime) stopRetainedWithEvidence(ctx context.Context) (stopEvidence, error) {
	var evidence stopEvidence
	if err := runtime.restrictAgentAccess("released"); err != nil {
		return evidence, err
	}
	exists, err := runtime.projectPresence(ctx)
	if err != nil {
		return evidence, fmt.Errorf("inventory retained slot project: %w", err)
	}
	if !exists {
		return evidence, nil
	}
	if err := runtime.requireProjectMarker(ctx); err != nil {
		return evidence, err
	}
	var names []string
	if runtime.allocation != nil {
		names, err = runtime.projectInstances(ctx)
		if err != nil {
			return evidence, err
		}
		if err := runtime.Config.validateManagedNames(names); err != nil {
			return evidence, err
		}
		for _, name := range names {
			if err := runtime.requireAllocationMarker(ctx, name); err != nil {
				return evidence, err
			}
		}
	} else {
		// Preserve retained lease stop semantics during the explicit migration window.
		for selector := 1; selector <= runtime.Config.guestCount(); selector++ {
			vm := runtime.Config.vm(selector)
			if runtime.vmExists(ctx, vm) {
				names = append(names, vm)
			}
		}
	}
	trimCtx, cancelTrim := context.WithTimeout(ctx, retainedGuestTrimTotalTimeout)
	defer cancelTrim()
	for _, vm := range names {
		state, err := runtime.incus(ctx, "list", vm, "--project", runtime.Config.Project,
			"-f", "csv", "-c", "s")
		if err != nil {
			return evidence, err
		}
		if strings.TrimSpace(state) == "RUNNING" {
			evidence.guestKeyCleanupAttempts++
			keyCleanupErr := errors.Join(
				runtime.installManagedGuestKeys(ctx, vm),
				runtime.removeLeaseContext(ctx, vm),
			)
			if runtime.allocation == nil {
				runtime.trimRetainedGuest(trimCtx, vm)
			}
			stopErr := runtime.stopRunningVM(ctx, vm)
			if stopErr != nil {
				return evidence, errors.Join(keyCleanupErr, stopErr)
			}
			if keyCleanupErr != nil {
				evidence.guestKeyCleanupDeferred++
				// A rebooting guest can temporarily lose its Incus agent. The data-account
				// forwarding key was already fenced above, so a verified stop closes access.
				// The next acquire replaces guest lease keys before publishing forwarding.
				fmt.Fprintf(runtime.Stderr,
					"test-vms: %s guest lease cleanup deferred until the next acquire; VM stopped\n",
					vm)
			}
		} else if strings.TrimSpace(state) != "STOPPED" {
			return evidence, fmt.Errorf(
				"%s cannot be fenced from state %q",
				vm,
				strings.TrimSpace(state),
			)
		}
	}
	return evidence, nil
}

func (runtime *Runtime) trimRetainedGuest(ctx context.Context, vm string) {
	_, err := runtime.guest(ctx, vm, nil,
		"timeout", "--signal=TERM", "--kill-after=10",
		fmt.Sprintf("%.0f", retainedGuestTrimTimeout.Seconds()),
		"sh", "-eu", "-c", "sync\nfstrim -av")
	if err != nil {
		fmt.Fprintln(runtime.Stderr, "test-vms: retained guest disk trim failed; continuing to stop")
	}
}

func (runtime *Runtime) recordStopOutcome(
	slot LeaseSlot,
	evidence stopEvidence,
	stopErr error,
) {
	if slot.SlotID == "" {
		return
	}
	if evidence.guestKeyCleanupAttempts != 0 {
		cause := error(nil)
		if evidence.guestKeyCleanupDeferred != 0 {
			cause = fmt.Errorf(
				"guest lease cleanup deferred for %d of %d running VMs after verified stop",
				evidence.guestKeyCleanupDeferred,
				evidence.guestKeyCleanupAttempts,
			)
		}
		_, _ = runtime.eventRecorder().Record(BrokerEvent{
			Kind:               "guest_key.cleanup",
			SlotID:             slot.SlotID,
			ResourceGeneration: slot.ResourceGeneration,
			LeaseEpoch:         slot.LeaseEpoch,
			Error:              errorStringOrEmpty(cause),
			Context:            leaseContextFromSlot(slot),
		})
	}
	if stopErr == nil {
		_, _ = runtime.eventRecorder().Record(BrokerEvent{
			Kind:               "lease.stop_succeeded",
			SlotID:             slot.SlotID,
			ResourceGeneration: slot.ResourceGeneration,
			LeaseEpoch:         slot.LeaseEpoch,
			Context:            leaseContextFromSlot(slot),
		})
	}
}

func (runtime *Runtime) stopRunningVM(ctx context.Context, vm string) error {
	_, stopErr := runtime.incus(ctx, "stop", vm, "--project", runtime.Config.Project,
		"--timeout", "60")
	state, stateErr := runtime.incus(ctx, "list", vm, "--project",
		runtime.Config.Project, "-f", "csv", "-c", "s")
	if stateErr != nil {
		return errors.Join(stopErr, stateErr)
	}
	if strings.TrimSpace(state) != "STOPPED" {
		return errors.Join(stopErr,
			fmt.Errorf("%s remained in state %q after stop", vm, strings.TrimSpace(state)))
	}
	return nil
}

func (runtime *Runtime) useLeaseSlot(slot LeaseSlot) {
	if slot.Environment == nil {
		return
	}
	runtime.Config = runtime.Config.withEnvironment(*slot.Environment)
	runtime.allocation = &LeaseIdentity{SlotID: slot.SlotID, ResourceGeneration: slot.ResourceGeneration, LeaseEpoch: slot.LeaseEpoch}
}

func (runtime *Runtime) requireAllocationMarker(ctx context.Context, vm string) error {
	if err := runtime.requireVMMarker(ctx, vm); err != nil {
		return err
	}
	if runtime.allocation == nil {
		return nil
	}
	for _, marker := range []struct {
		key   string
		value uint64
	}{
		{"user.subyard.generation", runtime.allocation.ResourceGeneration},
		{"user.subyard.lease-epoch", runtime.allocation.LeaseEpoch},
	} {
		value, err := runtime.incus(ctx, "config", "get", vm, marker.key, "--project", runtime.Config.Project)
		if err != nil {
			return err
		}
		if strings.TrimSpace(value) != strconv.FormatUint(marker.value, 10) {
			return fmt.Errorf("VM %q allocation marker mismatch", vm)
		}
	}
	return nil
}

// Incus instance deletion removes its root volume and snapshots. Every existing
// instance must match this exact allocation, and every stop is verified first.
func (runtime *Runtime) deleteAllocation(ctx context.Context) error {
	if runtime.allocation == nil {
		return errors.New("disposable cleanup requires allocation identity")
	}
	exists, err := runtime.projectPresence(ctx)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if err := runtime.requireProjectMarker(ctx); err != nil {
		return err
	}
	names, err := runtime.projectInstances(ctx)
	if err != nil {
		return err
	}
	if err := runtime.Config.validateManagedNames(names); err != nil {
		return err
	}
	for _, vm := range names {
		if err := runtime.requireAllocationMarker(ctx, vm); err != nil {
			return err
		}
		state, err := runtime.incus(ctx, "list", vm, "--project", runtime.Config.Project, "-f", "csv", "-c", "s")
		if err != nil {
			return err
		}
		if strings.TrimSpace(state) != "STOPPED" {
			return fmt.Errorf("VM %q is not stopped before deletion", vm)
		}
	}
	for _, vm := range names {
		if _, err := runtime.incus(ctx, "delete", vm, "--project", runtime.Config.Project); err != nil {
			return err
		}
	}
	remaining, err := runtime.projectInstances(ctx)
	if err != nil {
		return err
	}
	if len(remaining) != 0 {
		return errors.New("allocation instances remain after deletion")
	}
	return nil
}

// RetireLegacySlot is the explicit destructive migration from retained guest
// data. The operator's observed counters fence the whole operation, including
// never-leased slots whose epoch is zero.
func (runtime *Runtime) RetireLegacySlot(ctx context.Context, store LeaseStore, expected LeaseIdentity) error {
	runtime.prepareDefaults()
	number, err := slotNumber(expected.SlotID, runtime.Config.SlotCount)
	if err != nil || expected.ResourceGeneration == 0 {
		return errors.New("complete legacy retirement identity is required")
	}
	return withFileLock(runtime.slotLifecycleLock(expected.SlotID), func() error {
		if err := store.withLock(true, func(pool *LeasePool) error {
			slot, err := findSlot(pool, expected.SlotID)
			if err != nil {
				return err
			}
			if !slotMatchesLeaseIdentity(*slot, expected) {
				return ErrLeaseTargetStale
			}
			if !slot.LegacyRetained {
				return errors.New("slot has no legacy retained data")
			}
			if slot.State != SlotAvailable && slot.State != SlotQuarantined {
				return fmt.Errorf("cannot retire legacy slot while %s", slot.State)
			}
			slot.State = SlotDraining
			slot.FailureReason = "explicit legacy retirement"
			return nil
		}); err != nil {
			return err
		}
		child := runtime.slotRuntime(number, "")
		// Legacy instances do not have allocation counters. Verify every managed
		// name and marker before stopping anything under the operator's fence.
		cleanupErr := child.verifyLegacyRetirementOwnership(ctx)
		if cleanupErr == nil {
			cleanupErr = child.stopRetained(ctx)
		}
		if cleanupErr == nil {
			cleanupErr = child.deleteStoppedLegacyPair(ctx)
		}
		if cleanupErr != nil {
			return errors.Join(cleanupErr, store.FinishDrain(expected.SlotID, cleanupErr))
		}
		return store.withLock(true, func(pool *LeasePool) error {
			slot, err := findSlot(pool, expected.SlotID)
			if err != nil {
				return err
			}
			if !slotMatchesLeaseIdentity(*slot, expected) || slot.State != SlotDraining {
				return ErrLeaseTargetStale
			}
			clearLease(slot)
			slot.LegacyRetained = false
			slot.State = SlotAvailable
			return nil
		})
	})
}

func (runtime *Runtime) deleteStoppedLegacyPair(ctx context.Context) error {
	exists, err := runtime.projectPresence(ctx)
	if err != nil || !exists {
		return err
	}
	if err := runtime.requireProjectMarker(ctx); err != nil {
		return err
	}
	names, err := runtime.projectInstances(ctx)
	if err != nil {
		return err
	}
	if err := runtime.Config.validateManagedNames(names); err != nil {
		return err
	}
	for _, vm := range names {
		if err := runtime.requireVMMarker(ctx, vm); err != nil {
			return err
		}
		state, err := runtime.incus(ctx, "list", vm, "--project", runtime.Config.Project, "-f", "csv", "-c", "s")
		if err != nil {
			return err
		}
		if strings.TrimSpace(state) != "STOPPED" {
			return fmt.Errorf("legacy VM %q is not stopped", vm)
		}
	}
	for _, vm := range names {
		if _, err := runtime.incus(ctx, "delete", vm, "--project", runtime.Config.Project); err != nil {
			return err
		}
	}
	names, err = runtime.projectInstances(ctx)
	if err != nil {
		return err
	}
	if len(names) != 0 {
		return errors.New("legacy instances remain after retirement")
	}
	return nil
}

func (runtime *Runtime) verifyLegacyRetirementOwnership(ctx context.Context) error {
	exists, err := runtime.projectPresence(ctx)
	if err != nil || !exists {
		return err
	}
	if err := runtime.requireProjectMarker(ctx); err != nil {
		return err
	}
	names, err := runtime.projectInstances(ctx)
	if err != nil {
		return err
	}
	if err := runtime.Config.validateManagedNames(names); err != nil {
		return err
	}
	for _, vm := range names {
		if err := runtime.requireVMMarker(ctx, vm); err != nil {
			return err
		}
	}
	return nil
}

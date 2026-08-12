package testvmsruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

func (runtime *Runtime) HandleQuarantine(
	ctx context.Context,
	store LeaseStore,
	slotID string,
	cause error,
) error {
	runtime.prepareDefaults()
	if _, err := slotNumber(slotID, runtime.Config.SlotCount); err != nil {
		return err
	}
	return withFileLock(runtime.slotLifecycleLock(slotID), func() error {
		return runtime.handleQuarantineLocked(ctx, store, slotID, cause)
	})
}

func (runtime *Runtime) QuarantineSlot(
	ctx context.Context,
	store LeaseStore,
	grant LeaseGrant,
	cause error,
) error {
	runtime.prepareDefaults()
	if _, err := slotNumber(grant.SlotID, runtime.Config.SlotCount); err != nil {
		return err
	}
	return withFileLock(runtime.slotLifecycleLock(grant.SlotID), func() error {
		if err := store.Quarantine(grant, cause); err != nil {
			return err
		}
		return runtime.handleQuarantineLocked(ctx, store, grant.SlotID, cause)
	})
}

func (runtime *Runtime) handleQuarantineLocked(
	ctx context.Context,
	store LeaseStore,
	slotID string,
	cause error,
) error {
	slot, err := storeSlot(store, slotID)
	if err != nil {
		return err
	}
	if slot.State != SlotQuarantined && slot.State != SlotRecovering {
		return fmt.Errorf("slot %s is %s, not quarantined", slotID, slot.State)
	}
	if runtime.finishQuarantine != nil {
		return runtime.finishQuarantine(ctx, store, slot, cause)
	}
	number, err := slotNumber(slotID, runtime.Config.SlotCount)
	if err != nil {
		return err
	}
	child := runtime.slotRuntime(number, "")
	if fenceErr := child.restrictAgentAccess("quarantined"); fenceErr != nil {
		cause = errors.Join(
			cause,
			fmt.Errorf("fence quarantined slot access: %w", fenceErr),
		)
	}
	if guestKeyErr := child.removeQuarantinedGuestKeys(ctx); guestKeyErr != nil {
		cause = errors.Join(
			cause,
			fmt.Errorf("remove quarantined guest keys: %w", guestKeyErr),
		)
	}
	diagnostics := child.recoveryDiagnostics(ctx)
	artifact, err := runtime.eventRecorder().SaveIncident(slot, cause, diagnostics)
	if err != nil {
		return fmt.Errorf("persist emergency incident before recovery: %w", err)
	}
	event, err := runtime.eventRecorder().Record(BrokerEvent{
		Kind:               "slot.quarantined",
		SlotID:             slot.SlotID,
		ResourceGeneration: slot.ResourceGeneration,
		LeaseEpoch:         slot.LeaseEpoch,
		FromState:          slot.State,
		ToState:            SlotQuarantined,
		Error:              errorString(cause),
		IncidentID:         artifact.IncidentID,
		Context:            leaseContextFromSlot(slot),
	})
	if err != nil {
		return fmt.Errorf("persist quarantine event: %w", err)
	}
	if err := store.SetQuarantineIncident(slotID, event.EventID, artifact.IncidentID); err != nil {
		return err
	}
	_, _ = runtime.eventRecorder().Record(BrokerEvent{
		Kind:               "diagnostics.saved",
		SlotID:             slot.SlotID,
		ResourceGeneration: slot.ResourceGeneration,
		LeaseEpoch:         slot.LeaseEpoch,
		IncidentID:         artifact.IncidentID,
		Context:            leaseContextFromSlot(slot),
	})
	runtime.startRecoveryService(ctx)
	return nil
}

func (runtime *Runtime) RecoverScheduled(
	ctx context.Context,
	store LeaseStore,
	slotID string,
	force bool,
) error {
	runtime.prepareDefaults()
	return withFileLock(
		runtime.slotLifecycleLock(slotID),
		func() error {
			return runtime.recoverScheduledLocked(ctx, store, slotID, force)
		},
	)
}

func (runtime *Runtime) RecoverExpectedSlot(
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
			startedSlot, started, err := store.BeginExpectedRecovery(expected)
			if err != nil || !started {
				return err
			}
			if startedSlot.IncidentID == "" {
				cause := errors.New(startedSlot.FailureReason)
				if strings.TrimSpace(startedSlot.FailureReason) == "" {
					cause = errors.New("quarantined slot requires recovery")
				}
				if err := runtime.handleQuarantineLocked(
					ctx, store, startedSlot.SlotID, cause,
				); err != nil {
					_, finishErr := store.FinishRecovery(startedSlot.SlotID, err, "", "")
					return errors.Join(err, finishErr)
				}
				startedSlot, err = storeSlot(store, startedSlot.SlotID)
				if err != nil {
					return err
				}
			}
			return runtime.completeRecovery(ctx, store, startedSlot)
		},
	)
}

func (runtime *Runtime) recoverScheduledLocked(
	ctx context.Context,
	store LeaseStore,
	slotID string,
	force bool,
) error {
	slot, err := storeSlot(store, slotID)
	if err != nil {
		return err
	}
	if slot.State == SlotAvailable {
		if force {
			return fmt.Errorf("slot %s is available, not quarantined", slotID)
		}
		return nil
	}
	if slot.State == SlotRecovering {
		interrupted := errors.New("previous recovery was interrupted before completion")
		if err := store.InterruptRecovery(slotID, interrupted); err != nil {
			return err
		}
		if err := runtime.handleQuarantineLocked(ctx, store, slotID, interrupted); err != nil {
			return err
		}
		slot, err = storeSlot(store, slotID)
		if err != nil {
			return err
		}
	}
	if slot.State == SlotQuarantined && slot.IncidentID == "" {
		cause := errors.New(slot.FailureReason)
		if strings.TrimSpace(slot.FailureReason) == "" {
			cause = errors.New("quarantined slot requires recovery")
		}
		if err := runtime.handleQuarantineLocked(ctx, store, slotID, cause); err != nil {
			return err
		}
	}
	startedSlot, started, err := store.BeginScheduledRecovery(slotID, force)
	if err != nil {
		return err
	}
	if !started {
		return nil
	}
	return runtime.completeRecovery(ctx, store, startedSlot)
}

func (runtime *Runtime) completeRecovery(
	ctx context.Context,
	store LeaseStore,
	startedSlot LeaseSlot,
) error {
	if runtime.finishRecovery != nil {
		return runtime.finishRecovery(ctx, store, startedSlot)
	}
	slotID := startedSlot.SlotID
	startedAt := runtime.Now().UTC()
	if _, err := runtime.eventRecorder().Record(BrokerEvent{
		Kind:               "recovery.start",
		SlotID:             startedSlot.SlotID,
		ResourceGeneration: startedSlot.ResourceGeneration,
		LeaseEpoch:         startedSlot.LeaseEpoch,
		FromState:          SlotQuarantined,
		ToState:            SlotRecovering,
		RecoveryAttempt:    startedSlot.RecoveryAttempt,
		IncidentID:         startedSlot.IncidentID,
		Context:            leaseContextFromSlot(startedSlot),
	}); err != nil {
		_, finishErr := store.FinishRecovery(slotID, err, "", "")
		return errors.Join(fmt.Errorf("persist recovery start: %w", err), finishErr)
	}
	rebuildErr := runtime.rebuildSlotPair(ctx, startedSlot)
	duration := runtime.Now().UTC().Sub(startedAt)
	if duration < 0 {
		duration = 0
	}
	if rebuildErr == nil {
		finished, finishErr := store.FinishRecovery(slotID, nil, "", "")
		if finishErr != nil {
			rebuildErr = finishErr
		} else {
			_, eventErr := runtime.eventRecorder().Record(BrokerEvent{
				Kind:               "recovery.available",
				SlotID:             finished.SlotID,
				ResourceGeneration: finished.ResourceGeneration,
				LeaseEpoch:         finished.LeaseEpoch,
				FromState:          SlotRecovering,
				ToState:            SlotAvailable,
				RecoveryAttempt:    finished.RecoveryAttempt,
				DurationMS:         duration.Milliseconds(),
				IncidentID:         finished.IncidentID,
				Context:            leaseContextFromSlot(startedSlot),
			})
			return eventErr
		}
	}
	current, statusErr := storeSlot(store, slotID)
	if statusErr != nil {
		return errors.Join(rebuildErr, statusErr)
	}
	failureEvent, eventErr := runtime.eventRecorder().Record(BrokerEvent{
		Kind:               "recovery.failed",
		SlotID:             current.SlotID,
		ResourceGeneration: current.ResourceGeneration,
		LeaseEpoch:         current.LeaseEpoch,
		FromState:          SlotRecovering,
		ToState:            SlotQuarantined,
		RecoveryAttempt:    current.RecoveryAttempt,
		DurationMS:         duration.Milliseconds(),
		Error:              errorString(rebuildErr),
		IncidentID:         current.IncidentID,
		Context:            leaseContextFromSlot(current),
	})
	if eventErr != nil {
		_, finishErr := store.FinishRecovery(slotID, rebuildErr, "", "")
		return errors.Join(rebuildErr, eventErr, finishErr)
	}
	finished, finishErr := store.FinishRecovery(
		slotID,
		rebuildErr,
		failureEvent.EventID,
		"",
	)
	if finishErr != nil {
		return errors.Join(rebuildErr, finishErr)
	}
	_, scheduleErr := runtime.eventRecorder().Record(BrokerEvent{
		Kind:               "recovery.scheduled",
		SlotID:             finished.SlotID,
		ResourceGeneration: finished.ResourceGeneration,
		LeaseEpoch:         finished.LeaseEpoch,
		FromState:          SlotRecovering,
		ToState:            SlotQuarantined,
		RecoveryAttempt:    finished.RecoveryAttempt,
		Error:              errorString(rebuildErr),
		IncidentID:         finished.IncidentID,
		Context:            leaseContextFromSlot(finished),
	})
	return errors.Join(rebuildErr, scheduleErr)
}

func (runtime *Runtime) rebuildSlotPair(ctx context.Context, slot LeaseSlot) error {
	number, err := slotNumber(slot.SlotID, runtime.Config.SlotCount)
	if err != nil {
		return err
	}
	child := runtime.slotRuntime(number, "")
	child.prepareDefaults()
	if err := child.restrictAgentAccess("quarantine-rebuild"); err != nil {
		return err
	}
	exists, err := child.projectPresence(ctx)
	if err != nil {
		return fmt.Errorf("inventory quarantined slot project: %w", err)
	}
	if exists {
		if _, err := runtime.eventRecorder().Record(BrokerEvent{
			Kind:               "rebuild.delete",
			SlotID:             slot.SlotID,
			ResourceGeneration: slot.ResourceGeneration,
			LeaseEpoch:         slot.LeaseEpoch,
			RecoveryAttempt:    slot.RecoveryAttempt,
			IncidentID:         slot.IncidentID,
			Context:            leaseContextFromSlot(slot),
		}); err != nil {
			return fmt.Errorf("persist rebuild delete event: %w", err)
		}
		if err := child.deleteManagedPairForRebuild(ctx); err != nil {
			return err
		}
	}
	if err := child.preflightSlotCapacity(ctx); err != nil {
		return err
	}
	for _, path := range []string{
		child.Config.knownHosts(),
		child.Config.revokedKey(),
		child.Config.failureLog(),
	} {
		_ = os.Remove(path)
	}
	if _, err := runtime.eventRecorder().Record(BrokerEvent{
		Kind:               "rebuild.create",
		SlotID:             slot.SlotID,
		ResourceGeneration: slot.ResourceGeneration,
		LeaseEpoch:         slot.LeaseEpoch,
		RecoveryAttempt:    slot.RecoveryAttempt,
		IncidentID:         slot.IncidentID,
		Context:            leaseContextFromSlot(slot),
	}); err != nil {
		return fmt.Errorf("persist rebuild create event: %w", err)
	}
	if err := child.provisionPair(ctx); err != nil {
		return err
	}
	if err := child.stopRetained(ctx); err != nil {
		return err
	}
	_, err = runtime.eventRecorder().Record(BrokerEvent{
		Kind:               "rebuild.verified",
		SlotID:             slot.SlotID,
		ResourceGeneration: slot.ResourceGeneration + 1,
		LeaseEpoch:         slot.LeaseEpoch,
		RecoveryAttempt:    slot.RecoveryAttempt,
		IncidentID:         slot.IncidentID,
		Context:            leaseContextFromSlot(slot),
	})
	return err
}

func (runtime *Runtime) removeQuarantinedGuestKeys(ctx context.Context) error {
	exists, err := runtime.projectPresence(ctx)
	if err != nil {
		return fmt.Errorf("inventory quarantined slot project before guest-key removal: %w", err)
	}
	if !exists {
		return nil
	}
	if err := runtime.requireProjectMarker(ctx); err != nil {
		return err
	}
	var result error
	for selector := 1; selector <= 2; selector++ {
		vm := runtime.Config.vm(selector)
		if !runtime.vmExists(ctx, vm) {
			continue
		}
		if err := runtime.requireVMMarker(ctx, vm); err != nil {
			result = errors.Join(result, err)
			continue
		}
		state, err := runtime.incus(
			ctx,
			"list",
			vm,
			"--project",
			runtime.Config.Project,
			"-f",
			"csv",
			"-c",
			"s",
		)
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		if strings.TrimSpace(state) != "RUNNING" {
			continue
		}
		if err := runtime.installManagedGuestKeys(ctx, vm); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}

func (runtime *Runtime) deleteManagedPairForRebuild(ctx context.Context) error {
	exists, err := runtime.projectPresence(ctx)
	if err != nil {
		return fmt.Errorf("inventory quarantined slot project before delete: %w", err)
	}
	if !exists {
		return nil
	}
	if err := runtime.requireProjectMarker(ctx); err != nil {
		return err
	}
	names, err := runtime.projectInstances(ctx)
	if err != nil {
		return fmt.Errorf("inventory quarantined slot project: %w", err)
	}
	if err := runtime.Config.validateManagedNames(names); err != nil {
		return err
	}
	for _, name := range names {
		if err := runtime.requireVMMarker(ctx, name); err != nil {
			return err
		}
	}
	for _, name := range names {
		if _, err := runtime.incus(
			ctx,
			"delete",
			"--force",
			name,
			"--project",
			runtime.Config.Project,
		); err != nil {
			return err
		}
	}
	return nil
}

func (runtime *Runtime) recoveryDiagnostics(ctx context.Context) map[string]string {
	cfg := runtime.Config
	diagnostics := map[string]string{}
	if payload, err := os.ReadFile(cfg.failureLog()); err == nil {
		diagnostics["legacy_failure_log"] = string(payload)
	}
	if runtime.projectExists(ctx) {
		if value, err := runtime.incus(ctx, "project", "show", cfg.Project); err == nil {
			diagnostics["project"] = value
		}
		for selector := 1; selector <= 2; selector++ {
			vm := cfg.vm(selector)
			if !runtime.vmExists(ctx, vm) {
				continue
			}
			if value, err := runtime.incus(ctx, "info", "--show-log", vm,
				"--project", cfg.Project); err == nil {
				diagnostics[fmt.Sprintf("vm_%d_info_log", selector)] = value
			}
		}
	} else {
		diagnostics["project"] = "absent"
	}
	if stdout, stderr, err := runtime.Runner.Run(
		ctx,
		"journalctl",
		[]string{
			"--no-pager", "-n", "400",
			"-u", "incus.service",
			"-u", "subyard-test-vms-broker.service",
			"-u", "subyard-test-vms-lease-reaper.service",
		},
		nil,
		nil,
	); err == nil {
		diagnostics["service_journal"] = string(stdout)
	} else if len(stderr) != 0 {
		diagnostics["service_journal"] = string(stderr)
	}
	return diagnostics
}

func (runtime *Runtime) startRecoveryService(ctx context.Context) {
	if _, err := runtime.Runner.LookPath("systemctl"); err != nil {
		return
	}
	_, _, _ = runtime.Runner.Run(
		ctx,
		"systemctl",
		[]string{"start", "--no-block", "subyard-test-vms-lease-reaper.service"},
		nil,
		nil,
	)
}

func storeSlot(store LeaseStore, slotID string) (LeaseSlot, error) {
	pool, err := store.Status()
	if err != nil {
		return LeaseSlot{}, err
	}
	for _, slot := range pool.Slots {
		if slot.SlotID == slotID {
			return slot, nil
		}
	}
	return LeaseSlot{}, fmt.Errorf("unknown slot %q", slotID)
}

func dueForRecovery(slot LeaseSlot, now time.Time) bool {
	return slot.State == SlotRecovering ||
		(slot.State == SlotQuarantined &&
			(slot.NextRecoveryAt.IsZero() || !now.Before(slot.NextRecoveryAt)))
}

package application

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/ports"
)

type BootPowerResult struct {
	Started        []string
	Stopped        []string
	AlreadyRunning []string
}

type BootPowerReconciler struct {
	Inventory ports.InstanceInventory
	Instances ports.Incus
	Power     ports.InstancePowerManager
	Network   ports.HostNetworkGuard
	Clock     ports.Clock
	IncusWait time.Duration
}

func (reconciler BootPowerReconciler) HasManaged(ctx context.Context) (bool, error) {
	instances, err := reconciler.listManaged(ctx, false)
	return len(instances) != 0, err
}

func (reconciler BootPowerReconciler) Run(ctx context.Context) (BootPowerResult, error) {
	result := BootPowerResult{}
	if reconciler.Instances == nil || reconciler.Power == nil || reconciler.Network == nil {
		return result, errors.New("instance reader, power manager and host network guard are required")
	}
	instances, err := reconciler.waitForManaged(ctx, true)
	if err != nil || len(instances) == 0 {
		return result, err
	}
	bridges := managedBridges(instances)
	for _, instance := range instances {
		desired, _ := instance.EffectiveConfig("user.subyard.desired_power")
		if desired != PowerStopped {
			continue
		}
		reference := instanceReference(instance)
		switch strings.ToLower(instance.Status) {
		case "stopped":
		case "running":
			if err := reconciler.Power.SetInstancePower(ctx, instance.Project, instance.Name, "stop", false); err != nil {
				return result, fmt.Errorf("stop %s: %w", reference, err)
			}
			result.Stopped = append(result.Stopped, reference)
		default:
			return result, fmt.Errorf("cannot restore %s from state %q", reference, instance.Status)
		}
	}
	if err := reconciler.Network.Check(ctx, bridges); err != nil {
		return result, reconciler.stopRunningFailClosed(ctx, err)
	}
	for _, observed := range instances {
		instance, err := reconciler.Instances.Instance(ctx, observed.Project, observed.Name)
		if err != nil {
			return result, fmt.Errorf("re-read %s: %w", instanceReference(observed), err)
		}
		if err := validateManagedPower(instance); err != nil {
			return result, err
		}
		desired, _ := instance.EffectiveConfig("user.subyard.desired_power")
		if desired != PowerRunning {
			continue
		}
		reference := instanceReference(instance)
		switch strings.ToLower(instance.Status) {
		case "running":
			result.AlreadyRunning = append(result.AlreadyRunning, reference)
		case "stopped":
			if err := reconciler.Power.SetInstancePower(ctx, instance.Project, instance.Name, "start", false); err != nil {
				return result, fmt.Errorf("start %s: %w", reference, err)
			}
			result.Started = append(result.Started, reference)
		default:
			return result, fmt.Errorf("cannot restore %s from state %q", reference, instance.Status)
		}
		if err := reconciler.Network.Check(ctx, bridges); err != nil {
			return result, reconciler.stopRunningFailClosed(ctx, err)
		}
	}
	return result, nil
}

func (reconciler BootPowerReconciler) listManaged(
	ctx context.Context,
	validate bool,
) ([]ports.InstanceInfo, error) {
	if reconciler.Inventory == nil {
		return nil, errors.New("instance inventory is required")
	}
	instances, err := reconciler.Inventory.ListInstances(ctx)
	if err != nil {
		return nil, err
	}
	return filterManagedInstances(instances, validate)
}

func (reconciler BootPowerReconciler) waitForManaged(
	ctx context.Context,
	validate bool,
) ([]ports.InstanceInfo, error) {
	if reconciler.Inventory == nil {
		return nil, errors.New("instance inventory is required")
	}
	clock := reconciler.Clock
	if clock == nil {
		clock = bootWallClock{}
	}
	timeout := reconciler.IncusWait
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	readinessContext, cancelReadiness := context.WithTimeout(ctx, timeout)
	defer cancelReadiness()
	deadline := clock.Now().Add(timeout)
	var lastErr error
	for {
		instances, err := reconciler.Inventory.ListInstances(readinessContext)
		if err == nil {
			return filterManagedInstances(instances, validate)
		}
		lastErr = err
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		remaining := deadline.Sub(clock.Now())
		if remaining <= 0 || errors.Is(readinessContext.Err(), context.DeadlineExceeded) {
			return nil, incusReadinessTimeout(timeout, lastErr)
		}
		delay := time.Second
		if remaining < delay {
			delay = remaining
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-readinessContext.Done():
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return nil, incusReadinessTimeout(timeout, lastErr)
		case <-clock.After(delay):
		}
	}
}

func incusReadinessTimeout(timeout time.Duration, lastErr error) error {
	return fmt.Errorf(
		"Incus did not become ready within %s: %w: %v",
		timeout,
		ports.ErrIncusUnavailable,
		lastErr,
	)
}

type bootWallClock struct{}

func (bootWallClock) Now() time.Time                             { return time.Now() }
func (bootWallClock) After(delay time.Duration) <-chan time.Time { return time.After(delay) }

func filterManagedInstances(instances []ports.InstanceInfo, validate bool) ([]ports.InstanceInfo, error) {
	managed := make([]ports.InstanceInfo, 0, len(instances))
	for _, instance := range instances {
		managedValue, _ := instance.EffectiveConfig("user.subyard.managed")
		switch managedValue {
		case "", "false":
			continue
		case "true":
		default:
			return nil, fmt.Errorf("%s has invalid managed power metadata", instanceReference(instance))
		}
		if validate {
			if err := validateManagedPower(instance); err != nil {
				return nil, err
			}
		}
		managed = append(managed, instance)
	}
	sort.Slice(managed, func(left, right int) bool {
		return instanceReference(managed[left]) < instanceReference(managed[right])
	})
	return managed, nil
}

func (reconciler BootPowerReconciler) stopRunningFailClosed(ctx context.Context, reason error) error {
	instances, err := reconciler.listManaged(ctx, false)
	if err != nil {
		return errors.Join(reason, fmt.Errorf("list yards for fail-closed stop: %w", err))
	}
	var failures []error
	for _, observed := range instances {
		instance, readErr := reconciler.Instances.Instance(ctx, observed.Project, observed.Name)
		if readErr != nil {
			failures = append(failures, readErr)
			continue
		}
		if !strings.EqualFold(instance.Status, "running") {
			continue
		}
		if stopErr := reconciler.Power.SetInstancePower(
			ctx, instance.Project, instance.Name, "stop", true,
		); stopErr != nil {
			failures = append(failures, fmt.Errorf("stop %s: %w", instanceReference(instance), stopErr))
		}
	}
	if len(failures) != 0 {
		return errors.Join(append([]error{reason, errors.New("failed to stop unsafe yards")}, failures...)...)
	}
	return fmt.Errorf("%w; running yards stopped fail-closed", reason)
}

func validateManagedPower(instance ports.InstanceInfo) error {
	reference := instanceReference(instance)
	if instance.Project == "" || instance.Name == "" {
		return errors.New("managed instance has incomplete identity")
	}
	initialized, _ := instance.EffectiveConfig("user.subyard.initialized")
	if initialized != "true" {
		return fmt.Errorf("%s is not fully initialized", reference)
	}
	autostart, _ := instance.EffectiveConfig("boot.autostart")
	if autostart != "false" {
		return fmt.Errorf("%s must set boot.autostart=false", reference)
	}
	desired, _ := instance.EffectiveConfig("user.subyard.desired_power")
	if desired != PowerRunning && desired != PowerStopped {
		return fmt.Errorf("%s has invalid desired power %q", reference, desired)
	}
	bridge, _ := instance.EffectiveConfig("user.subyard.bridge")
	if bridge == "" {
		return fmt.Errorf("%s has no managed bridge", reference)
	}
	return nil
}

func managedBridges(instances []ports.InstanceInfo) []string {
	seen := make(map[string]struct{}, len(instances))
	for _, instance := range instances {
		bridge, _ := instance.EffectiveConfig("user.subyard.bridge")
		seen[bridge] = struct{}{}
	}
	bridges := make([]string, 0, len(seen))
	for bridge := range seen {
		bridges = append(bridges, bridge)
	}
	sort.Strings(bridges)
	return bridges
}

func instanceReference(instance ports.InstanceInfo) string {
	return instance.Project + "/" + instance.Name
}

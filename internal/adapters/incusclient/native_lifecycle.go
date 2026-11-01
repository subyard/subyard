package incusclient

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

// OriginalLifecycle contains only this invocation's captured identity and fixed
// owning checks. Callers retain source authority and their existing start lock.
type OriginalLifecycle struct {
	Project, Name, UUID string
	Kind                domain.YardKind
	Weight              string
	Inspect             func(context.Context) (ports.InstanceInfo, error)
	HostSafe            func(context.Context) error
	Publications        func(context.Context) error
}

func checkOriginalKindUUID(instance ports.InstanceInfo, project, name, uuid string, kind domain.YardKind) error {
	if uuid == "" || kind != domain.YardVM && kind != domain.YardContainer || instance.Project != project || instance.Name != name || instance.Type != kind || instance.Config["volatile.uuid"] != uuid || instance.LocalConfig["volatile.uuid"] != uuid {
		return domain.ErrPlanStale
	}
	return nil
}
func (client *Client) checkOriginalKindUUID(ctx context.Context, project, name, uuid string, kind domain.YardKind) error {
	instance, err := client.Instance(ctx, project, name)
	if err != nil {
		return err
	}
	return checkOriginalKindUUID(instance, project, name, uuid, kind)
}

// StartOriginalInstance is the one native physical start sequence for CLI and
// init. It never selects a new target, commits intent or schedules background work.
func StartOriginalInstance(ctx context.Context, reader ports.Incus, original OriginalLifecycle) error {
	host, ok := reader.(interface {
		SetOriginalInstancePowerIf(context.Context, string, string, string, bool, string, domain.YardKind) error
		VMCPUConvergedIf(context.Context, string, string, bool, string) (bool, error)
		ProbeOriginalInstanceAgentIf(context.Context, string, string, string, domain.YardKind) (bool, error)
	})
	if !ok || original.Inspect == nil || original.HostSafe == nil || original.Publications == nil || ports.NativeOperationObserverFrom(ctx) == nil || original.UUID == "" || original.Kind != domain.YardVM && original.Kind != domain.YardContainer {
		return domain.ErrPlanStale
	}
	weighted := original.Weight != ""
	if weighted {
		weight, err := strconv.Atoi(original.Weight)
		if original.Kind != domain.YardVM || err != nil || weight < 1 || weight > 10000 || strconv.Itoa(weight) != original.Weight {
			return domain.ErrPlanStale
		}
	}
	inspect := func(attempt context.Context) (ports.InstanceInfo, error) {
		instance, err := original.Inspect(attempt)
		if err != nil {
			return instance, err
		}
		if err := checkOriginalKindUUID(instance, original.Project, original.Name, original.UUID, original.Kind); err != nil {
			return instance, err
		}
		if instance.Config[vmCPUWeightKey] != original.Weight || instance.LocalConfig[vmCPUWeightKey] != original.Weight {
			return instance, domain.ErrPlanStale
		}
		return instance, nil
	}
	fail := func(cause error, stop bool) error {
		if !stop {
			return cause
		}
		stopped, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
		defer cancel()
		return errors.Join(cause, host.SetOriginalInstancePowerIf(stopped, original.Project, original.Name, "stop", true, original.UUID, original.Kind))
	}
	instance, err := inspect(ctx)
	if err != nil {
		return err
	}
	if !strings.EqualFold(instance.Status, "running") && !strings.EqualFold(instance.Status, "stopped") {
		return errors.New("original lifecycle target has unknown power state")
	}
	if err := original.HostSafe(ctx); err != nil {
		return err
	}
	if err := original.Publications(ctx); err != nil {
		return err
	}
	if !strings.EqualFold(instance.Status, "running") {
		if err := host.SetOriginalInstancePowerIf(ctx, original.Project, original.Name, "start", false, original.UUID, original.Kind); err != nil {
			return err
		}
	}
	check := func() error {
		instance, err := inspect(ctx)
		if err != nil {
			return err
		}
		if !strings.EqualFold(instance.Status, "running") {
			return errors.New("original lifecycle instance is not running")
		}
		return original.HostSafe(ctx)
	}
	cpu := func() error {
		ready, err := host.VMCPUConvergedIf(ctx, original.Project, original.Name, true, original.UUID)
		if err == nil && !ready {
			return errors.New("VM host CPU weight did not converge")
		}
		return err
	}
	if err := check(); err != nil {
		return fail(err, true)
	}
	if weighted {
		if err := cpu(); err != nil {
			return fail(err, true)
		}
	}
	budget := 120 * time.Second
	if original.Kind == domain.YardVM {
		budget = 600 * time.Second
	}
	agentContext, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	for {
		if _, err := inspect(agentContext); err != nil {
			return fail(err, weighted)
		}
		probe, stopProbe := context.WithTimeout(ports.WithoutNativeOperationObserver(agentContext), 5*time.Second)
		ready, probeErr := host.ProbeOriginalInstanceAgentIf(probe, original.Project, original.Name, original.UUID, original.Kind)
		stopProbe()
		if probeErr == nil && ready && agentContext.Err() == nil {
			break
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-agentContext.Done():
			timer.Stop()
			return fail(fmt.Errorf("original lifecycle instance agent did not become ready: %w", context.Cause(agentContext)), weighted)
		case <-timer.C:
		}
	}
	if weighted {
		// Image seeding may replace QEMU after the first scheduling application.
		if err := check(); err != nil {
			return fail(err, true)
		}
		if err := original.Publications(ctx); err != nil {
			return fail(err, true)
		}
		if err := check(); err != nil {
			return fail(err, true)
		}
		if err := cpu(); err != nil {
			return fail(err, true)
		}
	}
	if err := check(); err != nil {
		return fail(err, true)
	}
	return nil
}

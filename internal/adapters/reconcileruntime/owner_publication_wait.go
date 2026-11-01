package reconcileruntime

import (
	"context"
	"errors"
	"maps"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/hostruntime"
	"github.com/Subyard/Subyard/internal/adapters/incusclient"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerpublication"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

// The original create result or prepared existing-instance guard supplies UUID;
// finalization must never authorize a newly discovered same-name instance.
func (runtime Runtime) ownerPowerUUID(ctx context.Context) (string, error) {
	if runtime.ownerSources == nil || ports.NativeOperationObserverFrom(ctx) == nil || runtime.RepositoryRoot != runtime.ownerSources.root || runtime.OwnerEnvironmentBinding() != runtime.ownerSources.environment {
		return "", domain.ErrPlanStale
	}
	if err := runtime.ownerSources.check(); err != nil {
		return "", err
	}
	uuid := ""
	if runtime.ownerInstance != nil {
		original := runtime.ownerInstance.original
		if runtime.ownerInstance.resolved != nil {
			original = *runtime.ownerInstance.resolved
		}
		if original.Project != runtime.Yard.IncusProject || original.Instance != runtime.Yard.YardInstanceName || original.Scope != "current" && original.Scope != "legacy" && original.Scope != "recreate" || runtime.ownerInstance.created == nil {
			return "", domain.ErrPlanStale
		}
		if err := runtime.checkCreatedOwnerInstance(ctx, original, *runtime.ownerInstance.created); err != nil {
			return "", err
		}
		uuid = runtime.ownerInstance.created.InstanceUUID
	} else if runtime.ownerVolume != nil {
		original := runtime.ownerVolume.original
		if original.Project != runtime.Yard.IncusProject || original.Instance != runtime.Yard.YardInstanceName || original.Scope != "current" && original.Scope != "legacy" && original.Scope != "recreate" {
			return "", domain.ErrPlanStale
		}
		uuid = original.InstanceUUID
	}
	if !domain.SafeID(uuid) {
		return "", domain.ErrPlanStale
	}
	instance, err := runtime.Incus.Instance(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName)
	if err != nil {
		return "", err
	}
	if instance.Project != runtime.Yard.IncusProject || instance.Name != runtime.Yard.YardInstanceName || (runtime.Yard.YardKind != domain.YardVM && runtime.Yard.YardKind != domain.YardContainer) || instance.Type != runtime.Yard.YardKind || instance.Config["volatile.uuid"] != uuid || instance.LocalConfig["volatile.uuid"] != uuid {
		return "", domain.ErrPlanStale
	}
	return uuid, nil
}

// Existing-instance native power metadata keeps the prepared UUID, including
// Ensure and Commit. Created instances retain their stronger causal writer.
type ownerExistingPowerWriter struct{ runtime Runtime }

func (writer ownerExistingPowerWriter) SetInstanceConfig(ctx context.Context, project, name string, values map[string]string) error {
	runtime := writer.runtime
	if project != runtime.Yard.IncusProject || name != runtime.Yard.YardInstanceName {
		return domain.ErrPlanStale
	}
	uuid, err := runtime.ownerPowerUUID(ctx)
	if err != nil {
		return err
	}
	client, ok := runtime.Incus.(interface {
		SetOriginalInstanceConfigIf(context.Context, string, string, string, domain.YardKind, map[string]string, map[string]string) error
	})
	if !ok {
		return domain.ErrPlanStale
	}
	instance, err := runtime.Incus.Instance(ctx, project, name)
	if err != nil {
		return err
	}
	if desired := instance.LocalConfig["user.subyard.desired_power"]; desired != "" && desired != values["user.subyard.desired_power"] && values["user.subyard.desired_power"] != "" {
		return domain.ErrPlanStale
	}
	before := map[string]string{}
	for _, key := range []string{"user.subyard.managed", "user.subyard.initialized", "user.subyard.desired_power", "user.subyard.name", "user.subyard.bridge", "boot.autostart"} {
		before[key] = instance.LocalConfig[key]
	}
	return client.SetOriginalInstanceConfigIf(ctx, project, name, uuid, runtime.Yard.YardKind, before, values)
}

func (runtime Runtime) waitOwnerPublications(ctx context.Context, desired domain.DesiredPower) error {
	uuid, err := runtime.ownerPowerUUID(ctx)
	if err != nil {
		return err
	}
	contracts, err := ownerpublication.Load(runtime.RepositoryRoot)
	if err != nil {
		return err
	}
	read := func(call context.Context) (ports.InstanceInfo, error) {
		if runtime.ownerSources == nil || runtime.RepositoryRoot != runtime.ownerSources.root || runtime.OwnerEnvironmentBinding() != runtime.ownerSources.environment {
			return ports.InstanceInfo{}, domain.ErrPlanStale
		}
		if err := runtime.ownerSources.check(); err != nil {
			return ports.InstanceInfo{}, err
		}
		return runtime.Incus.Instance(call, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName)
	}
	instance, err := read(ctx)
	if err != nil {
		return err
	}
	scope := ""
	if runtime.ownerInstance != nil {
		scope = runtime.ownerInstance.original.Scope
	} else if runtime.ownerVolume != nil {
		scope = runtime.ownerVolume.original.Scope
	}
	target := ownerpublication.OriginalTarget{Project: runtime.Yard.IncusProject, Name: runtime.Yard.YardInstanceName, UUID: uuid, Kind: runtime.Yard.YardKind, Desired: desired, Scope: scope}
	if err := target.Check(instance); err != nil {
		return err
	}
	if _, _, _, err := ownerpublication.Plan(instance, contracts, nil); err != nil {
		return err
	}
	if desired == domain.PowerStopped {
		return nil
	}
	var devices []string
	for _, c := range contracts {
		if instance.LocalConfig[ownerpublication.Prefix+c.Device] != "" {
			devices = append(devices, c.Device)
		}
	}
	if len(devices) == 0 {
		var original *ports.InstanceInfo
		if runtime.ownerPublication != nil {
			original = runtime.ownerPublication.observed
		}
		return ownerpublication.CheckRestoredOriginal(instance, original, contracts)
	}
	if runtime.ownerPublication == nil {
		return domain.ErrPlanStale
	}
	consumer, err := runtime.ownerPublicationConsumer(ctx)
	if err != nil {
		return err
	}
	checked, ok := ports.NativeOperationObserverFrom(ctx).(ports.NativeCheckedInvocation)
	if !ok {
		return domain.ErrPlanStale
	}
	intent, recipe, err := checked.CheckedInvocation()
	if err != nil {
		return err
	}
	requests := []ownerpublication.Handoff{}
	for _, device := range devices {
		h, e := ownerpublication.NewHandoff(target, device, ports.NativeInvocationKey(ctx), intent, recipe, consumer, instance, contracts)
		if e != nil {
			return e
		}
		requests = append(requests, h)
	}
	exchange, ok := runtime.Incus.(interface {
		ExchangePublicationHandoff(context.Context, ownerpublication.Handoff, []ownerpublication.Contract, string, string, func() error) error
	})
	if !ok {
		return domain.ErrPlanStale
	}
	guard := func(call context.Context) error {
		if e := runtime.ownerSources.check(); e != nil {
			return e
		}
		if e := ownerpublication.RequireReader(runtime.RepositoryRoot, runtime.Yard.Paths.ConfigHome); e != nil {
			return e
		}
		return runtime.ownerPublication.original.CheckPublicationConsumer(call, consumer)
	}
	inspect := func(call context.Context) (ports.InstanceInfo, error) {
		i, e := read(call)
		if e != nil {
			return i, e
		}
		for _, h := range requests {
			if e := h.Check(i, contracts, true); e != nil {
				return i, e
			}
		}
		return i, nil
	}
	return ownerpublication.Deliver(ctx, requests, inspect, guard, func(call context.Context, h ownerpublication.Handoff, before, after string) error {
		return exchange.ExchangePublicationHandoff(call, h, contracts, before, after, func() error { return guard(call) })
	}, func(call context.Context) error {
		return runtime.ownerPublication.original.StartPublicationConsumer(call, consumer)
	})
}

func (runtime Runtime) ownerPublicationHostSafe(ctx context.Context) error {
	if service, ok := runtime.NetworkPolicy.(*yardnetwork.Service); ok && service.Guard != nil {
		return service.Guard(ctx)
	}
	guard := hostruntime.NetworkGuard{Run: func(ctx context.Context, path string, args ...string) ([]byte, error) {
		command := exec.CommandContext(ctx, path, args...)
		if filepath.Base(path) == "NetworkManager" && os.Geteuid() != 0 {
			if runtime.environmentValue("SUBYARD_SUDO_PREAUTHORIZED") != "1" {
				return nil, errors.New("original init NetworkManager reader is not authorized")
			}
			command = exec.CommandContext(ctx, "/usr/bin/sudo", append([]string{"-n", "--", path}, args...)...)
		}
		command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8", "LC_ALL=C.UTF-8"}
		return command.CombinedOutput()
	}}
	return guard.Check(ctx, []string{runtime.Yard.IncusBridge})
}

// Native finalization retains the original source and UUID proof. Blank sealed
// weight preserves the existing explicit inherit behavior on that same identity.
func (runtime Runtime) ownerPhysicalLifecycle(ctx context.Context, desired domain.DesiredPower) error {
	uuid, err := runtime.ownerPowerUUID(ctx)
	if err != nil {
		return err
	}
	inspect := func(attempt context.Context) (ports.InstanceInfo, error) {
		checked, err := runtime.ownerPowerUUID(attempt)
		if err != nil {
			return ports.InstanceInfo{}, err
		}
		if checked != uuid {
			return ports.InstanceInfo{}, domain.ErrPlanStale
		}
		return runtime.Incus.Instance(attempt, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName)
	}
	if desired == domain.PowerStopped {
		host, ok := runtime.Incus.(interface {
			SetOriginalInstancePowerIf(context.Context, string, string, string, bool, string, domain.YardKind) error
		})
		if !ok {
			return domain.ErrPlanStale
		}
		instance, err := inspect(ctx)
		if err != nil {
			return err
		}
		if !strings.EqualFold(instance.Status, "running") && !strings.EqualFold(instance.Status, "stopped") {
			return domain.ErrPlanStale
		}
		if !strings.EqualFold(instance.Status, "stopped") {
			if err := host.SetOriginalInstancePowerIf(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, "stop", false, uuid, runtime.Yard.YardKind); err != nil {
				return err
			}
		}
		instance, err = inspect(ctx)
		if err != nil {
			return err
		}
		if !strings.EqualFold(instance.Status, "stopped") {
			return domain.ErrPlanStale
		}
		return nil
	}
	if desired != domain.PowerRunning {
		return domain.ErrPlanStale
	}
	instance, err := inspect(ctx)
	if err != nil {
		return err
	}
	if runtime.ownerPublication != nil {
		observed := instance
		observed.Config = maps.Clone(instance.Config)
		observed.LocalConfig = maps.Clone(instance.LocalConfig)
		observed.Devices = maps.Clone(instance.Devices)
		observed.LocalDevices = maps.Clone(instance.LocalDevices)
		for key, value := range observed.Devices {
			observed.Devices[key] = maps.Clone(value)
		}
		for key, value := range observed.LocalDevices {
			observed.LocalDevices[key] = maps.Clone(value)
		}
		runtime.ownerPublication.observed = &observed
	}
	weight := runtime.environmentValue("VM_CPU_WEIGHT")
	if instance.Config["user.subyard.vm_cpu_weight"] != instance.LocalConfig["user.subyard.vm_cpu_weight"] {
		return domain.ErrPlanStale
	}
	if weight == "" {
		weight = instance.LocalConfig["user.subyard.vm_cpu_weight"]
	} else if instance.LocalConfig["user.subyard.vm_cpu_weight"] != weight {
		return domain.ErrPlanStale
	}
	if runtime.ownerInstance != nil && runtime.environmentValue("VM_CPU_WEIGHT") == "" && weight != "" {
		return domain.ErrPlanStale
	}
	contracts, err := ownerpublication.Load(runtime.RepositoryRoot)
	if err != nil {
		return err
	}
	publisher, ok := runtime.Incus.(interface {
		ReconcileOriginalPublicationsIf(context.Context, string, string, string, domain.YardKind, []ownerpublication.Contract, []netip.Addr, ...func() error) ([]string, error)
	})
	if !ok {
		return domain.ErrPlanStale
	}
	publications := func(attempt context.Context) error {
		_, err := ownerpublication.ReconcileAddresses(hostruntime.LocalAddresses, func(addresses []netip.Addr) ([]string, error) {
			return publisher.ReconcileOriginalPublicationsIf(attempt, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, uuid, runtime.Yard.YardKind, contracts, addresses, func() error {
				if err := runtime.ownerSources.check(); err != nil {
					return err
				}
				return ownerpublication.RequireReader(runtime.RepositoryRoot, runtime.Yard.Paths.ConfigHome)
			})
		})
		return err
	}
	return incusclient.StartOriginalInstance(ctx, runtime.Incus, incusclient.OriginalLifecycle{Project: runtime.Yard.IncusProject, Name: runtime.Yard.YardInstanceName, UUID: uuid, Kind: runtime.Yard.YardKind, Weight: weight, Inspect: inspect, HostSafe: runtime.ownerPublicationHostSafe, Publications: publications})
}

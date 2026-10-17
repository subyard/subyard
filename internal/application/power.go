package application

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

var ErrPowerUnmanaged = errors.New("power intent is unmanaged")

type PowerIntent struct {
	Desired  domain.DesiredPower
	Imported bool
}

type PowerService struct {
	Instances ports.Incus
	Config    ports.InstanceConfigWriter
}

func (service PowerService) Converged(
	ctx context.Context,
	yard domain.Context,
) (bool, error) {
	if service.Instances == nil {
		return false, errors.New("Incus instance reader is required")
	}
	instance, err := service.Instances.Instance(ctx, yard.IncusProject, yard.YardInstanceName)
	if err != nil {
		return false, err
	}
	return powerMetadataConverged(instance, yard)
}

func (service PowerService) Ensure(
	ctx context.Context,
	yard domain.Context,
) (PowerIntent, error) {
	if service.Instances == nil || service.Config == nil {
		return PowerIntent{}, errors.New("Incus instance reader and config writer are required")
	}
	instance, err := service.Instances.Instance(ctx, yard.IncusProject, yard.YardInstanceName)
	if err != nil {
		return PowerIntent{}, err
	}
	values, intent, err := powerMetadataUpdate(instance, yard)
	if err != nil {
		return PowerIntent{}, err
	}
	if err := service.Config.SetInstanceConfig(
		ctx, yard.IncusProject, yard.YardInstanceName, values,
	); err != nil {
		return PowerIntent{}, err
	}
	return intent, nil
}

func (service PowerService) Intent(
	ctx context.Context,
	yard domain.Context,
) (PowerIntent, error) {
	if service.Instances == nil {
		return PowerIntent{}, errors.New("Incus instance reader is required")
	}
	instance, err := service.Instances.Instance(ctx, yard.IncusProject, yard.YardInstanceName)
	if err != nil {
		return PowerIntent{}, err
	}
	metadata := parsePowerMetadata(instance)
	converged, err := managedPowerMetadataConverged(metadata, yard)
	if err != nil {
		return PowerIntent{}, err
	}
	if !converged {
		return PowerIntent{}, fmt.Errorf("%w: %s/%s", ErrPowerUnmanaged,
			yard.IncusProject, yard.YardInstanceName)
	}
	return PowerIntent{Desired: metadata.Desired.Value}, nil
}

func InitialPower(yard domain.Context) domain.DesiredPower {
	if yard.YardName == "" || yard.YardName == "default" || yard.NestedE2EVMs {
		return domain.PowerRunning
	}
	return domain.PowerStopped
}

func (service PowerService) Set(
	ctx context.Context,
	yard domain.Context,
	desired domain.DesiredPower,
	initialized bool,
) error {
	if service.Config == nil {
		return errors.New("Incus instance config writer is required")
	}
	if desired != domain.PowerRunning && desired != domain.PowerStopped {
		return fmt.Errorf("invalid desired power %q", desired)
	}
	name, bridge := powerIdentity(yard)
	return service.Config.SetInstanceConfig(ctx, yard.IncusProject, yard.YardInstanceName, map[string]string{
		powerAutostartKey:   "false",
		powerManagedKey:     "true",
		powerYardNameKey:    name,
		powerBridgeKey:      bridge,
		powerDesiredKey:     string(desired),
		powerInitializedKey: fmt.Sprint(initialized),
	})
}

func (service PowerService) Commit(
	ctx context.Context,
	yard domain.Context,
	desired domain.DesiredPower,
) error {
	return service.Set(ctx, yard, desired, true)
}

type LifecycleRunner struct {
	Power       PowerService
	Physical    ports.AdapterRunner
	Yard        domain.Context
	AfterCommit func(context.Context, domain.Context) error
}

func (runner LifecycleRunner) Run(
	ctx context.Context,
	request domain.AdapterRequest,
	protected io.Reader,
) (domain.AdapterResult, string, error) {
	if request.Adapter != "lifecycle" || (request.Action != "start" && request.Action != "stop") {
		return domain.AdapterResult{}, "", errors.New("invalid lifecycle adapter request")
	}
	if runner.Physical == nil {
		return domain.AdapterResult{}, "", errors.New("physical lifecycle adapter is required")
	}
	if _, err := runner.Power.Ensure(ctx, runner.Yard); err != nil {
		return domain.AdapterResult{}, "", fmt.Errorf("prepare power metadata: %w", err)
	}
	physical := request
	physical.Arguments = append([]string{request.Action}, request.Arguments...)
	result, diagnostics, err := runner.Physical.Run(ctx, physical, protected)
	if err != nil || result.Status != "ok" {
		return result, diagnostics, err
	}
	desired := domain.PowerStopped
	if request.Action == "start" {
		desired = domain.PowerRunning
	}
	observed, verifyErr := runner.Power.Instances.Instance(ctx, runner.Yard.IncusProject, runner.Yard.YardInstanceName)
	if verifyErr != nil {
		return result, diagnostics, fmt.Errorf("verify physical power: %w", verifyErr)
	}
	if !strings.EqualFold(observed.Status, string(desired)) {
		return result, diagnostics, fmt.Errorf("physical %s did not establish %s before desired power commit", request.Action, desired)
	}
	if err := runner.Power.Commit(ctx, runner.Yard, desired); err != nil {
		return result, diagnostics, fmt.Errorf("physical %s succeeded but desired power was not committed: %w",
			request.Action, err)
	}
	if request.Action == "start" && runner.AfterCommit != nil {
		if err := runner.AfterCommit(ctx, runner.Yard); err != nil {
			return result, diagnostics, fmt.Errorf("desired power committed but publication retry failed: %w", err)
		}
	}
	if result.Output == nil {
		result.Output = make(map[string]any)
	}
	result.Output["desiredPower"] = string(desired)
	return result, diagnostics, nil
}

func powerMetadataConverged(instance ports.InstanceInfo, yard domain.Context) (bool, error) {
	metadata := parsePowerMetadata(instance)
	return managedPowerMetadataConverged(metadata, yard)
}

func managedPowerMetadataConverged(metadata powerMetadata, yard domain.Context) (bool, error) {
	if metadata.Managed.State == fieldAbsent || metadata.Managed.State == fieldEmpty {
		return false, nil
	}
	if metadata.Managed.State != fieldValid || !metadata.Managed.Value {
		return false, fmt.Errorf("%s/%s has invalid managed power metadata %q",
			yard.IncusProject, yard.YardInstanceName, metadata.Managed.Raw)
	}
	if metadata.Desired.State != fieldValid {
		return false, fmt.Errorf("%s/%s has invalid desired power %q",
			yard.IncusProject, yard.YardInstanceName, metadata.Desired.Raw)
	}
	if metadata.Initialized.State != fieldValid {
		return false, fmt.Errorf("%s/%s has invalid initialized power metadata %q",
			yard.IncusProject, yard.YardInstanceName, metadata.Initialized.Raw)
	}
	name, bridge := powerIdentity(yard)
	return metadata.YardName.Raw == name && metadata.Bridge.Raw == bridge &&
		metadata.Autostart.State == fieldValid && !metadata.Autostart.Value, nil
}

func powerMetadataUpdate(
	instance ports.InstanceInfo,
	yard domain.Context,
) (map[string]string, PowerIntent, error) {
	name, bridge := powerIdentity(yard)
	values := map[string]string{
		powerYardNameKey: name, powerBridgeKey: bridge, powerAutostartKey: "false",
	}
	metadata := parsePowerMetadata(instance)
	if metadata.Managed.State == fieldValid && metadata.Managed.Value {
		if _, err := managedPowerMetadataConverged(metadata, yard); err != nil {
			return nil, PowerIntent{}, err
		}
		return values, PowerIntent{Desired: metadata.Desired.Value}, nil
	}
	if metadata.Managed.State != fieldAbsent && metadata.Managed.State != fieldEmpty {
		return nil, PowerIntent{}, fmt.Errorf("%s/%s has invalid managed power metadata %q",
			yard.IncusProject, yard.YardInstanceName, metadata.Managed.Raw)
	}
	var desired domain.DesiredPower
	switch strings.ToLower(instance.Status) {
	case "running":
		desired = domain.PowerRunning
	case "stopped":
		desired = domain.PowerStopped
	default:
		return nil, PowerIntent{}, fmt.Errorf("cannot import %s/%s power state from %q",
			yard.IncusProject, yard.YardInstanceName, instance.Status)
	}
	values[powerManagedKey] = "true"
	values[powerDesiredKey] = string(desired)
	values[powerInitializedKey] = "true"
	return values, PowerIntent{Desired: desired, Imported: true}, nil
}

func powerIdentity(yard domain.Context) (string, string) {
	name := yard.YardName
	if name == "" {
		name = "default"
	}
	bridge := yard.IncusBridge
	if bridge == "" {
		bridge = "incusbr0"
	}
	return name, bridge
}

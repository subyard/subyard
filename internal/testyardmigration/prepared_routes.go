package testyardmigration

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

func ValidatePreparedRouteDevices(options Options, before string, p *PreparedRouteDevices) error {
	if p == nil || p.Before != before || len(p.Devices) > 256 {
		return domain.ErrPlanStale
	}
	state, err := decodeRouteConsumersState(before)
	if err != nil {
		return err
	}
	allowed := map[string]bool{consumerKey("subyard-"+CurrentYard, "yard-"+CurrentYard): true}
	for _, c := range state.Consumers {
		allowed[consumerKey(c.Project, c.Instance)] = true
	}
	previous := ""
	seen := map[string]bool{}
	for _, d := range p.Devices {
		key := consumerKey(d.Project, d.Instance)
		desired := map[string]string{"type": "disk", "source": routeSource(options), "path": routeGuestPath, "readonly": "true"}
		if key <= previous || !allowed[key] || d.UUID == "" || d.Device != routeDeviceName || d.Source != routeSource(options) || d.Destination != routeGuestPath || d.Before != nil && !maps.Equal(d.Before, desired) {
			return domain.ErrPlanStale
		}
		previous = key
		seen[key] = true
	}
	for _, c := range state.Consumers {
		if !seen[consumerKey(c.Project, c.Instance)] {
			return domain.ErrPlanStale
		}
	}
	return nil
}

// PreparedRouteDevices binds the original owning route snapshot to the exact
// instance UUID and local device state captured by the official SDK adapter.
// It is private recipe metadata, independently of frozen migration syntax.
type PreparedRouteDevices struct {
	Before  string
	Devices []ports.PreparedReadOnlyDisk
	Broker  bool
}

type RouteDevicePreparer interface {
	PrepareReadOnlyDisk(context.Context, string, string, string, string, string) (ports.PreparedReadOnlyDisk, error)
}

func PrepareRouteDevices(ctx context.Context, options Options, before string, native RouteDevicePreparer) (*PreparedRouteDevices, error) {
	prepared, err := decodeRouteConsumersState(before)
	if err != nil || native == nil {
		return nil, errors.New("original route device preparation unavailable")
	}
	result := &PreparedRouteDevices{Before: before, Devices: []ports.PreparedReadOnlyDisk{}}
	if !prepared.Active {
		return result, nil
	}
	yards, err := validateCommitInventory(ctx, options, prepared, false, true)
	if err != nil {
		return nil, err
	}
	ready, err := inspectCurrentRoute(options)
	if err != nil {
		return nil, err
	}
	result.Broker = !ready || !hasManagedYard(yards, CurrentYard) || managedYardRunning(yards, CurrentYard)
	for _, yard := range yards {
		if yard.Yard == LegacyYard || !yard.AllowsHostAccess {
			continue
		}
		device, err := native.PrepareReadOnlyDisk(ctx, yard.Project, yard.Instance, routeDeviceName, routeSource(options), routeGuestPath)
		if err != nil {
			return nil, err
		}
		result.Devices = append(result.Devices, device)
	}
	slices.SortFunc(result.Devices, func(a, b ports.PreparedReadOnlyDisk) int {
		return strings.Compare(consumerKey(a.Project, a.Instance), consumerKey(b.Project, b.Instance))
	})
	return result, nil
}

func applyOriginalRouteDevice(ctx context.Context, options Options, before string, yard managedYard) error {
	if options.RouteDevices.Before != before || options.ApplyRouteDevice == nil {
		return domain.ErrPlanStale
	}
	var original *ports.PreparedReadOnlyDisk
	for index := range options.RouteDevices.Devices {
		device := &options.RouteDevices.Devices[index]
		if device.Project != yard.Project || device.Instance != yard.Instance {
			continue
		}
		if original != nil {
			return domain.ErrPlanStale
		}
		original = device
	}
	if original == nil || original.Device != routeDeviceName || original.Source != routeSource(options) || original.Destination != routeGuestPath || original.UUID == "" {
		return domain.ErrPlanStale
	}
	return options.ApplyRouteDevice(ctx, *original)
}

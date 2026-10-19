package testyardmigration

import (
	"context"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

// PrepareOwnerRouteDevices freezes the ordinary non-owner inventory and one
// explicit canonical owner slot before that owner's original init executes.
// The marker is inert; only an authenticated own-create result may replace it.
func PrepareOwnerRouteDevices(ctx context.Context, options Options, before string, native RouteDevicePreparer) (*PreparedRouteDevices, error) {
	p, err := PrepareRouteDevices(ctx, options, before, native)
	if err != nil {
		return nil, err
	}
	state, err := decodeRouteConsumersState(before)
	if err != nil || !state.Active {
		return nil, domain.ErrPlanStale
	}
	project, instance := "subyard-"+CurrentYard, "yard-"+CurrentYard
	found := false
	for _, d := range p.Devices {
		if d.Project == project && d.Instance == instance {
			found = true
		}
	}
	if !found {
		p.Devices = append(p.Devices, ports.PreparedReadOnlyDisk{Project: project, Instance: instance, UUID: "<after-owner>", Device: routeDeviceName, Source: routeSource(options), Destination: routeGuestPath})
	}
	p.Broker = true
	slices.SortFunc(p.Devices, func(a, b ports.PreparedReadOnlyDisk) int {
		return strings.Compare(consumerKey(a.Project, a.Instance), consumerKey(b.Project, b.Instance))
	})
	if err = ValidatePreparedRouteDevices(options, before, p); err != nil {
		return nil, err
	}
	return p, nil
}

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
	state, err := decodeRouteConsumersState(before)
	if err != nil || !state.Active || native == nil {
		return nil, domain.ErrPlanStale
	}
	registration, err := inspectRegistration(options)
	if err != nil {
		return nil, err
	}
	if registration.state != StateCurrent {
		return nil, domain.ErrPlanStale
	}
	// Validate every actual device before excluding the owner being replaced.
	// The ordinary commit validator requires that replacement to be complete.
	yards, err := inspectManagedYards(ctx, options)
	if err != nil {
		return nil, err
	}
	if err := validatePreparedSet(state.Consumers, nonOwnerYards(yards), false); err != nil {
		return nil, err
	}
	yards = slices.DeleteFunc(yards, func(yard managedYard) bool { return yard.Yard == LegacyYard || yard.Yard == CurrentYard })
	if err := inspectRouteConsumerEligibility(options, yards); err != nil {
		return nil, err
	}
	state.Consumers = nil
	for _, yard := range yards {
		if yard.AllowsHostAccess {
			state.Consumers = append(state.Consumers, routeConsumerSnapshot{yard.Project, yard.Instance, yard.Yard, yard.Mounted})
		}
	}
	before, err = encodeRouteConsumersState(state)
	if err != nil {
		return nil, err
	}
	p, err := prepareRouteDeviceSet(ctx, options, before, native, yards, true)
	if err != nil {
		return nil, err
	}
	project, instance := "subyard-"+CurrentYard, "yard-"+CurrentYard
	p.Devices = append(p.Devices, ports.PreparedReadOnlyDisk{Project: project, Instance: instance, UUID: "<after-owner>", Device: routeDeviceName, Source: routeSource(options), Destination: routeGuestPath})
	slices.SortFunc(p.Devices, func(a, b ports.PreparedReadOnlyDisk) int {
		return strings.Compare(consumerKey(a.Project, a.Instance), consumerKey(b.Project, b.Instance))
	})
	if err = ValidatePreparedRouteDevices(options, before, p); err != nil {
		return nil, err
	}
	return p, nil
}

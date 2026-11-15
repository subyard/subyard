package testyardmigration

import (
	"maps"

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

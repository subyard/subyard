package reconcileruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testyardmigration"
)

type afterOwnerRoutePhysical struct {
	Version  int    `json:"version"`
	Instance []byte `json:"instance"`
	Mounted  bool   `json:"mounted"`
	Broker   []byte `json:"broker"`
}

func AfterOwnerRoutePhysicalBound() int {
	raw, _ := json.Marshal(afterOwnerRoutePhysical{1, []byte(strings.Repeat("x", 128)), true, make([]byte, AfterOwnerBrokerPhysicalBound())})
	return len(raw)
}
func (runtime Runtime) PrepareAfterOwnerRoutes(ctx context.Context, routes *testyardmigration.PreparedRouteDevices, template []byte) (*PreparedInstalledRuntime, error) {
	broker, err := runtime.prepareTestVMBrokerRuntime(ctx, false)
	if err != nil {
		return nil, err
	}
	defer broker.Close()
	p, err := runtime.PrepareRouteConsumerRecipe(ctx, routes, broker)
	if err != nil {
		return nil, err
	}
	p.broker = nil
	return p.reserveAfterOwnerMetadata(template)
}
func (runtime Runtime) BindAfterOwnerRoutes(ctx context.Context, raw, physical []byte) (*PreparedInstalledRuntime, error) {
	value := afterOwnerRoutePhysical{Version: 1}
	if physical != nil {
		d := json.NewDecoder(bytes.NewReader(physical))
		d.DisallowUnknownFields()
		if d.Decode(&value) != nil {
			return nil, domain.ErrPlanStale
		}
		canonical, _ := json.Marshal(value)
		if !bytes.Equal(canonical, physical) || value.Version != 1 || !domain.SafeID(string(value.Instance)) || len(value.Instance) > 128 || string(value.Instance) == "absent" {
			return nil, domain.ErrPlanStale
		}
		var broker afterOwnerBrokerPhysical
		d = json.NewDecoder(bytes.NewReader(value.Broker))
		d.DisallowUnknownFields()
		if d.Decode(&broker) != nil {
			return nil, domain.ErrPlanStale
		}
		canonical, _ = json.Marshal(broker)
		if !bytes.Equal(canonical, value.Broker) || broker.Version != 1 {
			return nil, domain.ErrPlanStale
		}
		runtime.afterOwnerBroker = &broker
	}
	runtime.afterOwnerRoutes = &value
	return runtime.BindInstalledRuntimeContext(ctx, raw)
}
func (runtime Runtime) ResolveAfterOwnerRoutes(ctx context.Context, raw []byte, current ports.PreparedReadOnlyDisk) (*PreparedInstalledRuntime, []byte, error) {
	p, err := runtime.BindAfterOwnerRoutes(ctx, raw, nil)
	if err != nil {
		return nil, nil, err
	}
	defer p.Close()
	found := false
	for _, d := range p.routes.Devices {
		if d.Project != "subyard-"+testyardmigration.CurrentYard || d.Instance != "yard-"+testyardmigration.CurrentYard {
			continue
		}
		if found || current.Project != d.Project || current.Instance != d.Instance || current.Device != d.Device || current.Source != d.Source || current.Destination != d.Destination || d.UUID != "<after-owner>" && d.UUID != current.UUID {
			return nil, nil, domain.ErrPlanStale
		}
		desired := map[string]string{"type": "disk", "source": d.Source, "path": d.Destination, "readonly": "true"}
		if current.Before != nil && !maps.Equal(current.Before, desired) {
			return nil, nil, domain.ErrPlanStale
		}
		found = true
	}
	if !found {
		return nil, nil, domain.ErrPlanStale
	}
	broker, err := runtime.afterOwnerBrokerObservation(ctx, p.broker)
	if err != nil {
		return nil, nil, err
	}
	physical, _ := json.Marshal(afterOwnerRoutePhysical{1, []byte(current.UUID), current.Before != nil, broker})
	bound, err := runtime.BindAfterOwnerRoutes(ctx, raw, physical)
	return bound, physical, err
}

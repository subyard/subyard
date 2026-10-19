package reconcileruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testyardmigration"
)

const routeMetadataPath = "prepared/routes-original.json"

type routeRecipe struct {
	Version                                                     int
	Routes                                                      *testyardmigration.PreparedRouteDevices
	BrokerSource, BrokerRuntime, BrokerResources, BrokerBinding string
}

func (p *PreparedInstalledRuntime) PreparedRoutes() *testyardmigration.PreparedRouteDevices {
	return p.routes
}

func (p *PreparedInstalledRuntime) RouteSourceBinding() string {
	if p == nil || p.routes == nil {
		return ""
	}
	type device struct{ Project, Instance, UUID, Device, Source, Destination string }
	devices := make([]device, 0, len(p.routes.Devices))
	for _, d := range p.routes.Devices {
		devices = append(devices, device{d.Project, d.Instance, d.UUID, d.Device, d.Source, d.Destination})
	}
	broker := ""
	if p.broker != nil {
		broker = brokerSourceBinding(p.files[brokerMetadataPath])
	}
	return installedRuntimeDigest(struct {
		Source, Broker string
		Devices        []device
	}{p.source, broker, devices})
}

func (runtime Runtime) PrepareRouteConsumerRecipe(ctx context.Context, routes *testyardmigration.PreparedRouteDevices, broker *PreparedTestVMBroker) (*PreparedInstalledRuntime, error) {
	if routes == nil || len(routes.Devices) > 256 || routes.Broker && broker == nil {
		return nil, domain.ErrPlanStale
	}
	var p *PreparedInstalledRuntime
	var err error
	d := routeRecipe{Version: 1, Routes: routes}
	if broker != nil {
		p, err = runtime.PrepareTestVMBrokerRecipe(ctx, broker)
		if err != nil {
			return nil, err
		}
		d.BrokerSource, d.BrokerRuntime, d.BrokerResources, d.BrokerBinding = p.source, p.runtime, p.resources, p.binding
	} else {
		source, err := runtime.SelectedRuntimeInputs("core-guest-runtime")
		if err != nil {
			return nil, err
		}
		p = &PreparedInstalledRuntime{source: source, runtime: runtime.configRuntimeBinding(), resources: runtime.installedResourceBinding(), files: map[string][]byte{}, executionFiles: map[string][]byte{}}
	}
	p.owner = "route-consumers"
	p.routes = routes
	p.files[routeMetadataPath], err = json.Marshal(d)
	if err != nil {
		return nil, err
	}
	if err := checkMaterializedRecipeSize(p); err != nil {
		return nil, err
	}
	p.binding = installedRuntimeDigest(json.RawMessage(p.Recipe()))
	return p, nil
}

func (runtime Runtime) bindRouteRecipe(ctx context.Context, r installedRecipe, raw []byte) (*PreparedInstalledRuntime, error) {
	if r.Profile != "" || r.Handler != "" || r.Launcher != "" || r.HookBefore != "" || len(r.Leaves) != 0 || len(r.Native) != 0 || r.Runtime != runtime.configRuntimeBinding() || r.Resources != runtime.installedResourceBinding() {
		return nil, domain.ErrPlanStale
	}
	files := map[string][]byte{}
	previous := ""
	for _, f := range r.Files {
		if f.Path <= previous {
			return nil, errors.New("route recipe file order")
		}
		previous = f.Path
		files[f.Path] = f.Content
	}
	var d routeRecipe
	decoder := json.NewDecoder(bytes.NewReader(files[routeMetadataPath]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&d); err != nil {
		return nil, err
	}
	canonical, _ := json.Marshal(d)
	if !bytes.Equal(canonical, files[routeMetadataPath]) || d.Version != 1 || d.Routes == nil || len(d.Routes.Devices) > 256 {
		return nil, domain.ErrPlanStale
	}
	if err := testyardmigration.ValidatePreparedRouteDevices(testyardmigration.Options{DataHome: runtime.Yard.Paths.DataHome}, d.Routes.Before, d.Routes); err != nil {
		return nil, err
	}
	var p *PreparedInstalledRuntime
	if d.BrokerBinding != "" {
		if d.BrokerSource != r.Source || d.BrokerRuntime != r.Runtime || d.BrokerResources != r.Resources {
			return nil, domain.ErrPlanStale
		}
		broker := r
		broker.Owner = "test-vm-broker"
		broker.Files = nil
		for _, f := range r.Files {
			if f.Path != routeMetadataPath {
				broker.Files = append(broker.Files, f)
			}
		}
		original, err := json.Marshal(broker)
		if err != nil {
			return nil, err
		}
		if installedRuntimeDigest(json.RawMessage(original)) != d.BrokerBinding {
			return nil, domain.ErrPlanStale
		}
		p, err = runtime.bindBrokerRecipe(ctx, broker, original)
		if err != nil {
			return nil, err
		}
	} else {
		if len(files) != 1 || d.BrokerBinding != "" || d.BrokerSource != "" || d.BrokerRuntime != "" || d.BrokerResources != "" {
			return nil, domain.ErrPlanStale
		}
		if source, err := runtime.SelectedRuntimeInputs("core-guest-runtime"); err != nil || source != r.Source {
			return nil, domain.ErrPlanStale
		}
		p = &PreparedInstalledRuntime{source: r.Source, runtime: r.Runtime, resources: r.Resources, files: map[string][]byte{}, executionFiles: map[string][]byte{}}
	}
	p.owner = "route-consumers"
	p.routes = d.Routes
	p.files = maps.Clone(files)
	p.binding = installedRuntimeDigest(json.RawMessage(raw))
	return p, nil
}

func (runtime Runtime) ApplyPreparedRouteBroker(ctx context.Context, p *PreparedInstalledRuntime) error {
	if p == nil || p.owner != "route-consumers" || p.routes == nil || !p.routes.Broker || p.broker == nil {
		return domain.ErrPlanStale
	}
	// Retain one original cache/lifetime; the broker leaf performs only its
	// original prepared work and never invokes a whole yard command.
	if p.runtime != runtime.configRuntimeBinding() || p.resources != runtime.installedResourceBinding() {
		return domain.ErrPlanStale
	}
	if source, err := runtime.SelectedRuntimeInputs("core-guest-runtime"); err != nil || source != p.source {
		return domain.ErrPlanStale
	}
	execution, err := runtime.preparedSourceExecution(ctx, p)
	if err != nil {
		return err
	}
	defer p.Close()
	err = runtime.applyPreparedTestVMBrokerRuntime(ctx, p.broker, execution)
	if err != nil && p.stageBinding != "" {
		p.retain.Store(true)
	}
	return err
}

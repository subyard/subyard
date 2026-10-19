package reconcileruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releaseowner/nativejson"
	"github.com/Subyard/Subyard/internal/testyardmigration"
)

const routeMetadataPath = "prepared/routes-original.json"

type routeRecipe struct {
	Version                                                     int
	Routes                                                      *routeDeviceRecord
	BrokerSource, BrokerRuntime, BrokerResources, BrokerBinding string
}

type routeDiskRecord struct {
	Project, Instance, UUID, Device, Source, Destination string
	Before                                               ownerPlanMap[string]
}
type routeDeviceRecord struct {
	Before  string
	Devices ownerPlanSlice[routeDiskRecord]
	Broker  bool
}

func recordRouteDevices(p *testyardmigration.PreparedRouteDevices) *routeDeviceRecord {
	if p == nil {
		return nil
	}
	d := &routeDeviceRecord{Before: p.Before, Broker: p.Broker, Devices: ownerPlanSlice[routeDiskRecord]{Present: p.Devices != nil}}
	for _, v := range p.Devices {
		d.Devices.Values = append(d.Devices.Values, routeDiskRecord{v.Project, v.Instance, v.UUID, v.Device, v.Source, v.Destination, ownerMap(v.Before)})
	}
	return d
}
func (d *routeDeviceRecord) restore() *testyardmigration.PreparedRouteDevices {
	if d == nil {
		return nil
	}
	p := &testyardmigration.PreparedRouteDevices{Before: d.Before, Broker: d.Broker}
	if d.Devices.Present {
		p.Devices = []ports.PreparedReadOnlyDisk{}
	}
	for _, v := range d.Devices.Values {
		p.Devices = append(p.Devices, ports.PreparedReadOnlyDisk{Project: v.Project, Instance: v.Instance, UUID: v.UUID, Device: v.Device, Source: v.Source, Destination: v.Destination, Before: v.Before.restore()})
	}
	return p
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
	if p.files[brokerMetadataPath] != nil {
		broker = brokerSourceBinding(p.files[brokerMetadataPath])
	}
	raw, err := nativejson.EncodeLosslessNativeJSON(struct {
		Source, Broker string
		Devices        []device
	}{p.source, broker, devices}, 32<<20)
	if err != nil {
		return ""
	}
	return installedRuntimeDigest(json.RawMessage(raw))
}

func (runtime Runtime) PrepareRouteConsumerRecipe(ctx context.Context, routes *testyardmigration.PreparedRouteDevices, broker *PreparedTestVMBroker) (*PreparedInstalledRuntime, error) {
	if routes == nil || len(routes.Devices) > 256 || routes.Broker && broker == nil {
		return nil, domain.ErrPlanStale
	}
	if err := testyardmigration.ValidatePreparedRouteDevices(testyardmigration.Options{DataHome: runtime.Yard.Paths.DataHome}, routes.Before, routes); err != nil {
		return nil, err
	}
	var p *PreparedInstalledRuntime
	var err error
	d := routeRecipe{Version: 1, Routes: recordRouteDevices(routes)}
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
	p.routes = d.Routes.restore()
	p.files[routeMetadataPath], err = nativejson.EncodeLosslessNativeJSON(d, 32<<20)
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
	if err := nativejson.DecodeLosslessNativeJSON(files[routeMetadataPath], &d, 32<<20); err != nil {
		return nil, err
	}
	routes := d.Routes.restore()
	canonical, _ := nativejson.EncodeLosslessNativeJSON(routeRecipe{Version: d.Version, Routes: recordRouteDevices(routes), BrokerSource: d.BrokerSource, BrokerRuntime: d.BrokerRuntime, BrokerResources: d.BrokerResources, BrokerBinding: d.BrokerBinding}, 32<<20)
	if !bytes.Equal(canonical, files[routeMetadataPath]) || d.Version != 1 || routes == nil || len(routes.Devices) > 256 {
		return nil, domain.ErrPlanStale
	}
	if runtime.afterOwnerRoutes != nil && len(runtime.afterOwnerRoutes.Instance) != 0 {
		found := false
		for i := range routes.Devices {
			v := &routes.Devices[i]
			if v.Project != "subyard-"+testyardmigration.CurrentYard || v.Instance != "yard-"+testyardmigration.CurrentYard {
				continue
			}
			if found || v.UUID != "<after-owner>" && v.UUID != string(runtime.afterOwnerRoutes.Instance) {
				return nil, domain.ErrPlanStale
			}
			found = true
			v.UUID = string(runtime.afterOwnerRoutes.Instance)
			v.Before = nil
			if runtime.afterOwnerRoutes.Mounted {
				v.Before = map[string]string{"type": "disk", "source": v.Source, "path": v.Destination, "readonly": "true"}
			}
		}
		if !found {
			return nil, domain.ErrPlanStale
		}
	}
	if err := testyardmigration.ValidatePreparedRouteDevices(testyardmigration.Options{DataHome: runtime.Yard.Paths.DataHome}, routes.Before, routes); err != nil {
		return nil, err
	}
	var p *PreparedInstalledRuntime
	if d.BrokerBinding != "" {
		if d.BrokerSource != r.Source || d.BrokerRuntime != r.Runtime || d.BrokerResources != r.Resources {
			return nil, domain.ErrPlanStale
		}
		broker := r
		broker.Owner = "test-vm-broker"
		broker.AfterOwner = nil
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
	p.routes = routes
	p.files = maps.Clone(files)
	p.afterOwner = bytes.Clone(r.AfterOwner)
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

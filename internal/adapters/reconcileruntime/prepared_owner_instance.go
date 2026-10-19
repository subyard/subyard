package reconcileruntime

import (
	"bytes"
	"context"
	"reflect"
	"slices"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releaseowner/nativejson"
)

// OwnerInitProjection is only an original pre-consent planning read view. It
// neither authorizes removal nor turns later resource names into create results.
type OwnerInitProjection struct{ record ownerInitProjectionRecord }
type ownerProjectionContext domain.Context
type ownerProjectionResourceOwner struct {
	UID, DataHome, ConfigHome string
	Context                   *ownerProjectionContext
}
type ownerProjectionResource struct {
	Kind, Name, Pool, Binding, Path string
	SnapshotCount                   int
	UsedBy, Bindings                ownerPlanSlice[string]
	Owner                           *ownerProjectionResourceOwner
}
type ownerProjectionState struct {
	ProjectConfig, ProfileConfig                                                            ownerPlanMap[string]
	ProfileDevices                                                                          ownerPlanDevices
	Instance                                                                                ownerInstancePublic
	Config, LocalConfig                                                                     ownerPlanMap[string]
	Devices, LocalDevices                                                                   ownerPlanDevices
	HostPoolFound, HostNetworkFound, ProjectFound, ProfileFound, InstanceFound, VolumeFound bool
}
type ownerInitProjectionRecord struct {
	Version                                 int
	Context                                 ownerProjectionContext
	Environment                             string
	Project, Instance, Pool, Volume, Bridge string
	State                                   ownerProjectionState
	Targets                                 []ownerProjectionResource
}

func describeOwnerProjectionState(s ports.ReconcileState) ownerProjectionState {
	i := s.Instance
	return ownerProjectionState{ownerMap(s.ProjectConfig), ownerMap(s.ProfileConfig), ownerDevices(s.ProfileDevices), ownerInstancePublic{i.Name, i.Project, i.Type, i.Status}, ownerMap(i.Config), ownerMap(i.LocalConfig), ownerDevices(i.Devices), ownerDevices(i.LocalDevices), s.HostPoolFound, s.HostNetworkFound, s.ProjectFound, s.ProfileFound, s.InstanceFound, s.VolumeFound}
}
func (d ownerProjectionState) restore() ports.ReconcileState {
	i := d.Instance
	return ports.ReconcileState{ProjectConfig: d.ProjectConfig.restore(), ProfileConfig: d.ProfileConfig.restore(), ProfileDevices: restoreOwnerDevices(d.ProfileDevices), Instance: ports.InstanceInfo{Name: i.Name, Project: i.Project, Type: i.Type, Status: i.Status, Config: d.Config.restore(), LocalConfig: d.LocalConfig.restore(), Devices: restoreOwnerDevices(d.Devices), LocalDevices: restoreOwnerDevices(d.LocalDevices)}, HostPoolFound: d.HostPoolFound, HostNetworkFound: d.HostNetworkFound, ProjectFound: d.ProjectFound, ProfileFound: d.ProfileFound, InstanceFound: d.InstanceFound, VolumeFound: d.VolumeFound}
}
func ownerProjectionTargets(targets []ports.TeardownResource) []ownerProjectionResource {
	if targets == nil {
		return nil
	}
	result := make([]ownerProjectionResource, 0, len(targets))
	for _, t := range targets {
		r := ownerProjectionResource{Kind: t.Kind, Name: t.Name, Pool: t.Pool, Binding: t.Binding, Path: t.Path, SnapshotCount: t.SnapshotCount, UsedBy: ownerSlice(slices.Clone(t.UsedBy)), Bindings: ownerSlice(slices.Clone(t.Bindings))}
		if t.Owner != nil {
			r.Owner = &ownerProjectionResourceOwner{UID: t.Owner.UID, DataHome: t.Owner.DataHome, ConfigHome: t.Owner.ConfigHome}
			if t.Owner.Context != nil {
				value := ownerProjectionContext(*t.Owner.Context)
				r.Owner.Context = &value
			}
		}
		result = append(result, r)
	}
	return result
}
func (runtime Runtime) ownerProjectionScope() ownerInitProjectionRecord {
	pool, volume := runtime.volumeNames()
	bridge := runtime.Yard.IncusBridge
	if bridge == "" {
		bridge = runtime.environmentDefault("INCUS_BRIDGE", "incusbr0")
	}
	yard := runtime.Yard
	yard.Paths.RepositoryRoot = runtime.bindingPath(yard.Paths.RepositoryRoot)
	yard.Paths.ConfigDir = runtime.bindingPath(yard.Paths.ConfigDir)
	return ownerInitProjectionRecord{Version: 1, Context: ownerProjectionContext(yard), Environment: runtime.OwnerEnvironmentBinding(), Project: runtime.Yard.IncusProject, Instance: runtime.Yard.YardInstanceName, Pool: pool, Volume: volume, Bridge: bridge}
}
func (p OwnerInitProjection) valid(runtime Runtime) bool {
	d := p.record
	s := runtime.ownerProjectionScope()
	if d.Version != 1 || !reflect.DeepEqual(d.Context, s.Context) || d.Environment != s.Environment || d.Project != s.Project || d.Instance != s.Instance || d.Pool != s.Pool || d.Volume != s.Volume || d.Bridge != s.Bridge || len(d.Targets) > 256 || !domain.SafeID(d.Project) || d.Project == "default" || !domain.SafeID(d.Instance) {
		return false
	}
	state := d.State.restore()
	if !reflect.DeepEqual(describeOwnerProjectionState(state), d.State) || state.InstanceFound && (state.Instance.Project != d.Project || state.Instance.Name != d.Instance || state.Instance.Config["volatile.uuid"] == "") {
		return false
	}
	seen := map[string]bool{}
	for _, target := range d.Targets {
		key := target.Kind + "/" + target.Pool + "/" + target.Name
		if seen[key] || target.Binding == "" {
			return false
		}
		seen[key] = true
		if target.Kind == "project" && target.Name != d.Project || target.Kind == "instance" && target.Name != d.Instance {
			return false
		}
	}
	if state.ProjectFound && !seen["project//"+d.Project] || state.InstanceFound && !seen["instance//"+d.Instance] {
		return false
	}
	if !state.ProjectFound && seen["project//"+d.Project] || !state.InstanceFound && seen["instance//"+d.Instance] {
		return false
	}
	return true
}
func (runtime Runtime) PrepareOwnerInitProjection(ctx context.Context, originalState ports.ReconcileState, originalTargets []ports.TeardownResource) (OwnerInitProjection, error) {
	state, err := runtime.reconcileState(ctx)
	if err != nil {
		return OwnerInitProjection{}, err
	}
	if !reflect.DeepEqual(state, originalState) {
		return OwnerInitProjection{}, domain.ErrPlanStale
	}
	actual, err := runtime.Incus.TeardownInventory(ctx, runtime.Yard.IncusProject)
	if err != nil {
		return OwnerInitProjection{}, err
	}
	// Teardown's shared pool/network entries remain actual. Only the exact
	// scoped resources already observed by its original handle are projected.
	for _, target := range actual {
		if !slices.ContainsFunc(originalTargets, func(original ports.TeardownResource) bool { return reflect.DeepEqual(original, target) }) {
			return OwnerInitProjection{}, domain.ErrPlanStale
		}
	}
	d := runtime.ownerProjectionScope()
	d.State = describeOwnerProjectionState(state)
	d.Targets = ownerProjectionTargets(actual)
	p := OwnerInitProjection{d}
	if !p.valid(runtime) {
		return OwnerInitProjection{}, domain.ErrPlanStale
	}
	return p, nil
}

// CheckOwnerInitRemoval is a guard on an already selected conditional child.
// Its caller must separately hold the successful original teardown result;
// observed absence alone never chooses the child or confers cleanup authority.
func (runtime Runtime) CheckOwnerInitRemoval(ctx context.Context, p OwnerInitProjection) error {
	if !p.valid(runtime) {
		return domain.ErrPlanStale
	}
	state, err := runtime.reconcileState(ctx)
	if err != nil {
		return err
	}
	if state.ProjectFound || state.InstanceFound || state.HostPoolFound != p.record.State.HostPoolFound || state.HostNetworkFound != p.record.State.HostNetworkFound {
		return domain.ErrPlanStale
	}
	for _, target := range p.record.Targets {
		if target.Kind == "volume" && target.Pool == p.record.Pool && target.Name == p.record.Volume && state.VolumeFound {
			return domain.ErrPlanStale
		}
	}
	return nil
}

// CheckOwnerInitCreatedProject admits only the exact project returned by this
// original invocation's known successful create. It retains every original
// instance/volume absence and shared-resource guard; it does not plan a child.
func (runtime Runtime) CheckOwnerInitCreatedProject(ctx context.Context, p OwnerInitProjection, original ports.PreparedImageProject, created ports.NativeOwnerCreated) error {
	if !p.valid(runtime) || original.Project != p.record.Project || !original.Absent || !original.SharedImages || len(original.InitialConfig) == 0 {
		return domain.ErrPlanStale
	}
	state, err := runtime.reconcileState(ctx)
	if err != nil {
		return err
	}
	if !state.ProjectFound || state.InstanceFound || !state.ProfileFound || len(state.ProfileConfig) != 0 || len(state.ProfileDevices) != 0 || state.HostPoolFound != p.record.State.HostPoolFound || state.HostNetworkFound != p.record.State.HostNetworkFound {
		return domain.ErrPlanStale
	}
	for _, target := range p.record.Targets {
		if target.Kind == "volume" && target.Pool == p.record.Pool && target.Name == p.record.Volume && state.VolumeFound {
			return domain.ErrPlanStale
		}
	}
	reader, ok := runtime.Incus.(interface {
		CheckOwnerCreatedImageProject(context.Context, ports.PreparedImageProject, ports.NativeOwnerCreated) error
	})
	if !ok {
		return domain.ErrPlanStale
	}
	return reader.CheckOwnerCreatedImageProject(ctx, original, created)
}
func (p OwnerInitProjection) Record() ([]byte, error) {
	return nativejson.EncodeLosslessNativeJSON(p.record, 32<<20)
}
func (runtime Runtime) RestoreOwnerInitProjection(raw []byte) (OwnerInitProjection, error) {
	var d ownerInitProjectionRecord
	if err := nativejson.DecodeLosslessNativeJSON(raw, &d, 32<<20); err != nil {
		return OwnerInitProjection{}, err
	}
	p := OwnerInitProjection{d}
	if !p.valid(runtime) {
		return OwnerInitProjection{}, domain.ErrPlanStale
	}
	canonical, err := p.Record()
	if err != nil || !bytes.Equal(raw, canonical) {
		return OwnerInitProjection{}, domain.ErrPlanStale
	}
	return p, nil
}
func (runtime Runtime) OwnerInitPlanningView(p OwnerInitProjection) (Runtime, error) {
	if !p.valid(runtime) {
		return Runtime{}, domain.ErrPlanStale
	}
	runtime.Incus = ownerInitPlanningReader{IncusReader: runtime.Incus, projection: p}
	runtime.ownerInitPlanning = true
	runtime.ConfigWriter = nil
	runtime.Executor = nil
	return runtime, nil
}

type ownerInitPlanningReader struct {
	IncusReader
	projection OwnerInitProjection
}

func (r ownerInitPlanningReader) Instance(_ context.Context, project, instance string) (ports.InstanceInfo, error) {
	d := r.projection.record
	if project != d.Project || instance != d.Instance {
		return ports.InstanceInfo{}, domain.ErrPlanStale
	}
	return ports.InstanceInfo{}, ports.ErrInstanceNotFound
}
func (r ownerInitPlanningReader) ReconcileState(_ context.Context, project, instance, pool, volume, bridge string) (ports.ReconcileState, error) {
	d := r.projection.record
	if project != d.Project || instance != d.Instance || pool != d.Pool || volume != d.Volume || bridge != d.Bridge {
		return ports.ReconcileState{}, domain.ErrPlanStale
	}
	s := d.State.restore()
	s.ProjectFound = false
	s.ProfileFound = false
	s.InstanceFound = false
	for _, target := range d.Targets {
		if target.Kind == "volume" && target.Pool == d.Pool && target.Name == d.Volume {
			s.VolumeFound = false
		}
	}
	s.ProjectConfig = nil
	s.ProfileConfig = nil
	s.ProfileDevices = nil
	s.Instance = ports.InstanceInfo{}
	return s, nil
}
func (r ownerInitPlanningReader) TeardownInventory(_ context.Context, project string) ([]ports.TeardownResource, error) {
	if project != r.projection.record.Project {
		return nil, domain.ErrPlanStale
	}
	return nil, nil
}
func (r ownerInitPlanningReader) ListInstances(ctx context.Context) ([]ports.InstanceInfo, error) {
	values, err := r.IncusReader.ListInstances(ctx)
	if err != nil {
		return nil, err
	}
	d := r.projection.record
	return slices.DeleteFunc(slices.Clone(values), func(v ports.InstanceInfo) bool { return v.Project == d.Project && v.Name == d.Instance }), nil
}

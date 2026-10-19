package cli

import (
	"bytes"
	"errors"
	"reflect"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/sshagentruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releaseowner/nativejson"
	"github.com/Subyard/Subyard/internal/uninstall"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

const ownerTeardownRecordLimit = 96 << 20

// A distinct concrete type excludes Context's compatibility JSON decoder.
type ownerTeardownContext domain.Context

type ownerTeardownRow[T any] struct {
	Key   string
	Value T
}
type ownerTeardownMap[T any] struct {
	Present bool
	Rows    []ownerTeardownRow[T]
}

func teardownMap[T any](values map[string]T) ownerTeardownMap[T] {
	d := ownerTeardownMap[T]{Present: values != nil}
	for key, value := range values {
		d.Rows = append(d.Rows, ownerTeardownRow[T]{key, value})
	}
	slices.SortFunc(d.Rows, func(a, b ownerTeardownRow[T]) int { return strings.Compare(a.Key, b.Key) })
	return d
}
func (d ownerTeardownMap[T]) restore() map[string]T {
	if !d.Present {
		return nil
	}
	values := make(map[string]T, len(d.Rows))
	for _, row := range d.Rows {
		values[row.Key] = row.Value
	}
	return values
}
func teardownSlice[T, U any](values []T, convert func(T) U) []U {
	if values == nil {
		return nil
	}
	result := make([]U, len(values))
	for index, value := range values {
		result[index] = convert(value)
	}
	return result
}

type ownerTeardownDevices = ownerTeardownMap[ownerTeardownMap[string]]

func teardownDevices(values map[string]map[string]string) ownerTeardownDevices {
	if values == nil {
		return ownerTeardownDevices{}
	}
	rows := make(map[string]ownerTeardownMap[string], len(values))
	for key, value := range values {
		rows[key] = teardownMap(value)
	}
	return teardownMap(rows)
}
func restoreTeardownDevices(d ownerTeardownDevices) map[string]map[string]string {
	if !d.Present {
		return nil
	}
	values := make(map[string]map[string]string, len(d.Rows))
	for _, row := range d.Rows {
		values[row.Key] = row.Value.restore()
	}
	return values
}

type ownerTeardownResourceOwner struct {
	UID, DataHome, ConfigHome string
	Context                   *ownerTeardownContext
}
type ownerTeardownResource struct {
	Kind, Name, Pool, Binding, Path string
	SnapshotCount                   int
	UsedBy, Bindings                []string
	Owner                           *ownerTeardownResourceOwner
}

func describeTeardownResource(p ports.TeardownResource) ownerTeardownResource {
	d := ownerTeardownResource{Kind: p.Kind, Name: p.Name, Pool: p.Pool, Binding: p.Binding, Path: p.Path, SnapshotCount: p.SnapshotCount, UsedBy: p.UsedBy, Bindings: p.Bindings}
	if p.Owner != nil {
		d.Owner = &ownerTeardownResourceOwner{UID: p.Owner.UID, DataHome: p.Owner.DataHome, ConfigHome: p.Owner.ConfigHome}
		if p.Owner.Context != nil {
			value := ownerTeardownContext(*p.Owner.Context)
			d.Owner.Context = &value
		}
	}
	return d
}
func (d ownerTeardownResource) restore() ports.TeardownResource {
	p := ports.TeardownResource{Kind: d.Kind, Name: d.Name, Pool: d.Pool, Binding: d.Binding, Path: d.Path, SnapshotCount: d.SnapshotCount, UsedBy: d.UsedBy, Bindings: d.Bindings}
	if d.Owner != nil {
		p.Owner = &ports.InstallationResourceOwner{UID: d.Owner.UID, DataHome: d.Owner.DataHome, ConfigHome: d.Owner.ConfigHome}
		if d.Owner.Context != nil {
			value := domain.Context(*d.Owner.Context)
			p.Owner.Context = &value
		}
	}
	return p
}

type ownerTeardownArtifact struct {
	Path, Binding, Category           string
	Entries, Namespaces               []ports.ArtifactEntry
	Opaque, Scoped, Mutable           bool
	Exclusions, Preserved, OwnedPaths []string
	Resource                          *ownerTeardownResource
}

func describeTeardownArtifact(p ports.TeardownArtifact) ownerTeardownArtifact {
	d := ownerTeardownArtifact{Path: p.Path, Binding: p.Binding, Category: p.Category, Entries: p.Entries, Namespaces: p.Namespaces, Opaque: p.Opaque, Scoped: p.Scoped, Mutable: p.Mutable, Exclusions: p.Exclusions, Preserved: p.Preserved, OwnedPaths: p.OwnedPaths}
	if p.Resource != nil {
		value := describeTeardownResource(*p.Resource)
		d.Resource = &value
	}
	return d
}
func (d ownerTeardownArtifact) restore() ports.TeardownArtifact {
	p := ports.TeardownArtifact{Path: d.Path, Binding: d.Binding, Category: d.Category, Entries: d.Entries, Namespaces: d.Namespaces, Opaque: d.Opaque, Scoped: d.Scoped, Mutable: d.Mutable, Exclusions: d.Exclusions, Preserved: d.Preserved, OwnedPaths: d.OwnedPaths}
	if d.Resource != nil {
		value := d.Resource.restore()
		p.Resource = &value
	}
	return p
}
func describeTeardownTree(p uninstall.Tree) ownerTeardownArtifact {
	return describeTeardownArtifact(ports.TeardownArtifact(p))
}
func restoreTeardownTree(d ownerTeardownArtifact) uninstall.Tree { return uninstall.Tree(d.restore()) }

type ownerTeardownService struct {
	Scope     string
	Units     []string
	Artifacts []ownerTeardownArtifact
}

func describeTeardownService(p uninstall.RecoveryService) ownerTeardownService {
	return ownerTeardownService{p.Scope, p.Units, teardownSlice(p.Artifacts, describeTeardownArtifact)}
}
func (d ownerTeardownService) restore() uninstall.RecoveryService {
	return uninstall.RecoveryService{Scope: d.Scope, Units: d.Units, Artifacts: teardownSlice(d.Artifacts, ownerTeardownArtifact.restore)}
}

type ownerTeardownInstance struct {
	Name, Project, Status string
	Type                  domain.YardKind
	Config, LocalConfig   ownerTeardownMap[string]
	Devices, LocalDevices ownerTeardownDevices
}
type ownerTeardownState struct {
	ProjectConfig, ProfileConfig                                                            ownerTeardownMap[string]
	ProfileDevices                                                                          ownerTeardownDevices
	Instance                                                                                ownerTeardownInstance
	HostPoolFound, HostNetworkFound, ProjectFound, ProfileFound, InstanceFound, VolumeFound bool
}

func describeTeardownState(p ports.ReconcileState) ownerTeardownState {
	i := p.Instance
	return ownerTeardownState{teardownMap(p.ProjectConfig), teardownMap(p.ProfileConfig), teardownDevices(p.ProfileDevices), ownerTeardownInstance{i.Name, i.Project, i.Status, i.Type, teardownMap(i.Config), teardownMap(i.LocalConfig), teardownDevices(i.Devices), teardownDevices(i.LocalDevices)}, p.HostPoolFound, p.HostNetworkFound, p.ProjectFound, p.ProfileFound, p.InstanceFound, p.VolumeFound}
}
func (d ownerTeardownState) restore() ports.ReconcileState {
	i := d.Instance
	return ports.ReconcileState{ProjectConfig: d.ProjectConfig.restore(), ProfileConfig: d.ProfileConfig.restore(), ProfileDevices: restoreTeardownDevices(d.ProfileDevices), Instance: ports.InstanceInfo{Name: i.Name, Project: i.Project, Status: i.Status, Type: i.Type, Config: i.Config.restore(), LocalConfig: i.LocalConfig.restore(), Devices: restoreTeardownDevices(i.Devices), LocalDevices: restoreTeardownDevices(i.LocalDevices)}, HostPoolFound: d.HostPoolFound, HostNetworkFound: d.HostNetworkFound, ProjectFound: d.ProjectFound, ProfileFound: d.ProfileFound, InstanceFound: d.InstanceFound, VolumeFound: d.VolumeFound}
}

type ownerTeardownSnapshot struct {
	Resources                []ownerTeardownResource
	Artifacts                []ownerTeardownArtifact
	Registered               []ownerTeardownContext
	Services                 []ownerTeardownService
	State                    ownerTeardownState
	Agent                    []byte
	AgentBinding, AgentState string
}
type ownerTeardownIngress struct {
	Device, Protocol, Listen string
	GuestPort                int
}
type ownerTeardownNetworkBinding struct {
	Yard                      yardnetwork.Yard
	IPv4, MAC, OriginalAccess string
	OriginalNIC               ownerTeardownMap[string]
	ApprovedIngress           []ownerTeardownIngress
}
type ownerTeardownNetwork struct {
	Yard                        yardnetwork.Yard
	Schema                      int
	Isolation, AppliedIsolation bool
	Revision, AppliedRevision   uint64
	Links                       []yardnetwork.Link
	Bindings                    []ownerTeardownNetworkBinding
	PendingStart, Removing      []string
	Content, ETag               string
	Cleanup                     bool
	Consequences                []string
}

func describeTeardownNetwork(p *yardnetwork.RemovalPlan) (*ownerTeardownNetwork, error) {
	if p == nil {
		return nil, nil
	}
	v := p.Stored.Policy
	d := &ownerTeardownNetwork{Yard: p.Yard, Schema: v.Schema, Isolation: v.Isolation, AppliedIsolation: v.AppliedIsolation, Revision: v.Revision, AppliedRevision: v.AppliedRevision, Links: v.Links, PendingStart: v.PendingStart, Removing: v.Removing, Content: p.Stored.Content, ETag: p.Stored.ETag, Cleanup: p.Cleanup, Consequences: p.Consequences}
	if v.Bindings != nil {
		d.Bindings = []ownerTeardownNetworkBinding{}
	}
	for _, binding := range v.Bindings {
		row := ownerTeardownNetworkBinding{Yard: binding.Yard, IPv4: binding.IPv4, MAC: binding.MAC, OriginalAccess: binding.OriginalAccess, OriginalNIC: teardownMap(binding.OriginalNIC)}
		if binding.ApprovedIngress != nil {
			row.ApprovedIngress = []ownerTeardownIngress{}
		}
		for _, ingress := range binding.ApprovedIngress {
			// Decode normalizes away legacy-only transient validation fields.
			plain := yardnetwork.ApprovedIngress{Device: ingress.Device, Protocol: ingress.Protocol, Listen: ingress.Listen, GuestPort: ingress.GuestPort}
			if !reflect.DeepEqual(ingress, plain) {
				return nil, errors.New("teardown network policy is not normalized")
			}
			row.ApprovedIngress = append(row.ApprovedIngress, ownerTeardownIngress{ingress.Device, ingress.Protocol, ingress.Listen, ingress.GuestPort})
		}
		d.Bindings = append(d.Bindings, row)
	}
	return d, nil
}
func (d *ownerTeardownNetwork) restore() *yardnetwork.RemovalPlan {
	if d == nil {
		return nil
	}
	v := yardnetwork.Policy{Schema: d.Schema, Isolation: d.Isolation, AppliedIsolation: d.AppliedIsolation, Revision: d.Revision, AppliedRevision: d.AppliedRevision, Links: d.Links, PendingStart: d.PendingStart, Removing: d.Removing}
	if d.Bindings != nil {
		v.Bindings = []yardnetwork.Binding{}
	}
	for _, row := range d.Bindings {
		binding := yardnetwork.Binding{Yard: row.Yard, IPv4: row.IPv4, MAC: row.MAC, OriginalAccess: row.OriginalAccess, OriginalNIC: row.OriginalNIC.restore()}
		if row.ApprovedIngress != nil {
			binding.ApprovedIngress = []yardnetwork.ApprovedIngress{}
		}
		for _, ingress := range row.ApprovedIngress {
			binding.ApprovedIngress = append(binding.ApprovedIngress, yardnetwork.ApprovedIngress{Device: ingress.Device, Protocol: ingress.Protocol, Listen: ingress.Listen, GuestPort: ingress.GuestPort})
		}
		v.Bindings = append(v.Bindings, binding)
	}
	return &yardnetwork.RemovalPlan{Yard: d.Yard, Stored: yardnetwork.StoredPolicy{Policy: v, Content: d.Content, ETag: d.ETag}, Cleanup: d.Cleanup, Consequences: d.Consequences}
}

type ownerTeardownYard struct {
	Context     ownerTeardownContext
	Environment ownerTeardownMap[string]
	Snapshot    int
	Network     *ownerTeardownNetwork
}
type ownerTeardownStartup struct {
	Before    ownerTeardownArtifact
	Blocks    []string
	Pending   *ownerTeardownArtifact
	Temporary string
}
type ownerTeardownProfileCheck struct {
	Profile, Hook string
	Environment   ownerTeardownMap[string]
}
type ownerTeardownRecord struct {
	Version                                                  int
	Schema                                                   int
	Installation                                             uninstall.Installation
	Stage                                                    uninstall.RecoveryStage
	Yards                                                    []ownerTeardownYard
	Snapshots                                                []ownerTeardownSnapshot
	Trees, Launchers                                         []ownerTeardownArtifact
	Startup                                                  []ownerTeardownStartup
	OwnedGitDirectories, Preserved                           []string
	Services                                                 []ownerTeardownService
	SourceRoot                                               string
	SourceAssets                                             []ownerTeardownArtifact
	AssetsReady                                              bool
	Shared                                                   []ownerTeardownResource
	OwnerLock, RecoveryBinding                               *ownerTeardownArtifact
	EpochBefore                                              uint64
	ProfileChecks                                            []ownerTeardownProfileCheck
	Full, HoldShared, KeepData, ResetConfig, CompletedReset  bool
	Changed, PhysicalChanged, RevokeAgent, Assessed, Resumed bool
	ActiveYard, LiveSnapshot                                 int
	ManagedRoots, PreservedRoots, MutableRoots               []string
}

// captureOwnerTeardownRecord retains the original normal teardown graph. The
// surrounding owner recipe pins its source and configuration-root descriptors.
func captureOwnerTeardownRecord(p *teardownExecution) ([]byte, error) {
	if p == nil {
		return nil, nil
	}
	if p.full || p.keepData || p.resetConfig || p.completedReset || p.configSnapshot != nil {
		return nil, errors.New("owner teardown snapshot requires normal teardown")
	}
	d := ownerTeardownRecord{Version: 1, Schema: p.Schema, Installation: p.Installation, Stage: p.Stage, Trees: teardownSlice(p.Trees, describeTeardownTree), Launchers: teardownSlice(p.Launchers, describeTeardownTree), OwnedGitDirectories: p.OwnedGitDirectories, Preserved: p.Preserved, Services: teardownSlice(p.Services, describeTeardownService), SourceRoot: p.SourceRoot, SourceAssets: teardownSlice(p.SourceAssets, describeTeardownTree), AssetsReady: p.AssetsReady, Shared: teardownSlice(p.Shared, describeTeardownResource), EpochBefore: p.EpochBefore, Full: p.full, HoldShared: p.holdShared, KeepData: p.keepData, ResetConfig: p.resetConfig, CompletedReset: p.completedReset, Changed: p.changed, PhysicalChanged: p.physicalChanged, RevokeAgent: p.revokeAgent, Assessed: p.Assessed, Resumed: p.resumed, ActiveYard: -1, LiveSnapshot: -1, ManagedRoots: p.ManagedRoots, PreservedRoots: p.PreservedRoots, MutableRoots: p.MutableRoots}
	if p.OwnerLock != nil {
		value := describeTeardownTree(*p.OwnerLock)
		d.OwnerLock = &value
	}
	if p.RecoveryBinding != nil {
		value := describeTeardownTree(*p.RecoveryBinding)
		d.RecoveryBinding = &value
	}
	if p.Startup != nil {
		d.Startup = []ownerTeardownStartup{}
	}
	for _, edit := range p.Startup {
		row := ownerTeardownStartup{Before: describeTeardownTree(edit.Before), Blocks: edit.Blocks, Temporary: edit.Temporary}
		if edit.Pending != nil {
			value := describeTeardownTree(*edit.Pending)
			row.Pending = &value
		}
		d.Startup = append(d.Startup, row)
	}
	if p.ProfileChecks != nil {
		d.ProfileChecks = []ownerTeardownProfileCheck{}
	}
	for _, check := range p.ProfileChecks {
		d.ProfileChecks = append(d.ProfileChecks, ownerTeardownProfileCheck{check.Profile, check.Hook, teardownMap(check.Environment)})
	}
	snapshotIDs := map[*teardownSnapshot]int{}
	addSnapshot := func(snapshot *teardownSnapshot) (int, error) {
		if snapshot == nil {
			return -1, nil
		}
		if index, exists := snapshotIDs[snapshot]; exists {
			return index, nil
		}
		agent, err := snapshot.Agent.CaptureRecord()
		if err != nil {
			return -1, err
		}
		if snapshot.Agent != nil && (snapshot.Agent.Binding() != snapshot.AgentBinding || snapshot.Agent.State() != snapshot.AgentState) || snapshot.Agent == nil && (snapshot.AgentBinding != "" || snapshot.AgentState != "") {
			return -1, domain.ErrPlanStale
		}
		index := len(d.Snapshots)
		snapshotIDs[snapshot] = index
		d.Snapshots = append(d.Snapshots, ownerTeardownSnapshot{teardownSlice(snapshot.Resources, describeTeardownResource), teardownSlice(snapshot.Artifacts, describeTeardownArtifact), teardownSlice(snapshot.Registered, func(c domain.Context) ownerTeardownContext { return ownerTeardownContext(c) }), teardownSlice(snapshot.Services, describeTeardownService), describeTeardownState(snapshot.State), agent, snapshot.AgentBinding, snapshot.AgentState})
		return index, nil
	}
	if p.Yards != nil {
		d.Yards = []ownerTeardownYard{}
	}
	for index := range p.Yards {
		yard := &p.Yards[index]
		if yard.Snapshot == nil {
			return nil, errors.New("original owner teardown snapshot is required")
		}
		if yard == p.activeYard {
			d.ActiveYard = index
		}
		snapshot, err := addSnapshot(yard.Snapshot)
		if err != nil {
			return nil, err
		}
		network, err := describeTeardownNetwork(yard.Network)
		if err != nil {
			return nil, err
		}
		d.Yards = append(d.Yards, ownerTeardownYard{ownerTeardownContext(yard.Context), teardownMap(yard.Environment), snapshot, network})
	}
	if p.activeYard != nil && d.ActiveYard == -1 {
		return nil, errors.New("owner teardown active yard is outside original plan")
	}
	var err error
	d.LiveSnapshot, err = addSnapshot(p.liveSnapshot)
	if err != nil {
		return nil, err
	}
	return nativejson.EncodeLosslessNativeJSON(d, ownerTeardownRecordLimit)
}

// Restore uses the checked caller's original logical context. Physical source
// aliases belong to the owner's cached runner, and are never reopened here.
func restoreOwnerTeardownRecord(raw []byte, loaded config.Loaded, cli *CLI) (*teardownExecution, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var d ownerTeardownRecord
	if err := nativejson.DecodeLosslessNativeJSON(raw, &d, ownerTeardownRecordLimit); err != nil {
		return nil, err
	}
	if d.Version != 1 || d.Full || d.KeepData || d.ResetConfig || d.CompletedReset {
		return nil, errors.New("invalid normal owner teardown descriptor")
	}
	if d.Schema != 0 && d.Schema != 1 {
		return nil, errors.New("invalid owner teardown recovery schema")
	}
	switch d.Stage {
	case "", uninstall.RecoveryPrepared, uninstall.RecoveryProducersStopped, uninstall.RecoveryYardsRemoved, uninstall.RecoveryFilesRemoved:
	default:
		return nil, errors.New("invalid owner teardown recovery stage")
	}
	if cli == nil || d.ActiveYard < 0 || d.ActiveYard >= len(d.Yards) || len(d.Yards) > 128 {
		return nil, domain.ErrPlanStale
	}
	if len(d.Snapshots) > len(d.Yards)+1 {
		return nil, errors.New("invalid owner teardown snapshot graph")
	}
	if d.Installation.DataHome != "" && d.Installation.DataHome != loaded.Context.Paths.DataHome || d.Installation.ConfigHome != "" && d.Installation.ConfigHome != loaded.Context.Paths.ConfigHome {
		return nil, domain.ErrPlanStale
	}
	active := d.Yards[d.ActiveYard]
	if domain.Context(active.Context) != loaded.Context || !reflect.DeepEqual(active.Environment.restore(), structuredCommandContext(loaded)) {
		return nil, domain.ErrPlanStale
	}
	p := &teardownExecution{RecoveryPlan: uninstall.RecoveryPlan{Schema: d.Schema, Installation: d.Installation, Stage: d.Stage, Trees: teardownSlice(d.Trees, restoreTeardownTree), Launchers: teardownSlice(d.Launchers, restoreTeardownTree), OwnedGitDirectories: d.OwnedGitDirectories, Preserved: d.Preserved, Services: teardownSlice(d.Services, ownerTeardownService.restore), SourceRoot: d.SourceRoot, SourceAssets: teardownSlice(d.SourceAssets, restoreTeardownTree), AssetsReady: d.AssetsReady, Shared: teardownSlice(d.Shared, ownerTeardownResource.restore), EpochBefore: d.EpochBefore}, holdShared: d.HoldShared, changed: d.Changed, physicalChanged: d.PhysicalChanged, revokeAgent: d.RevokeAgent, Assessed: d.Assessed, resumed: d.Resumed, ManagedRoots: d.ManagedRoots, PreservedRoots: d.PreservedRoots, MutableRoots: d.MutableRoots}
	if d.OwnerLock != nil {
		value := restoreTeardownTree(*d.OwnerLock)
		p.OwnerLock = &value
	}
	if d.RecoveryBinding != nil {
		value := restoreTeardownTree(*d.RecoveryBinding)
		p.RecoveryBinding = &value
	}
	if d.Startup != nil {
		p.Startup = []uninstall.StartupEdit{}
	}
	for _, edit := range d.Startup {
		row := uninstall.StartupEdit{Before: restoreTeardownTree(edit.Before), Blocks: edit.Blocks, Temporary: edit.Temporary}
		if edit.Pending != nil {
			value := restoreTeardownTree(*edit.Pending)
			row.Pending = &value
		}
		p.Startup = append(p.Startup, row)
	}
	if d.ProfileChecks != nil {
		p.ProfileChecks = []uninstall.RecoveryProfileCheck{}
	}
	for _, check := range d.ProfileChecks {
		p.ProfileChecks = append(p.ProfileChecks, uninstall.RecoveryProfileCheck{Profile: check.Profile, Hook: check.Hook, Environment: check.Environment.restore()})
	}
	snapshots := make([]*teardownSnapshot, len(d.Snapshots))
	for index, row := range d.Snapshots {
		snapshots[index] = &teardownSnapshot{Resources: teardownSlice(row.Resources, ownerTeardownResource.restore), Artifacts: teardownSlice(row.Artifacts, ownerTeardownArtifact.restore), Registered: teardownSlice(row.Registered, func(c ownerTeardownContext) domain.Context { return domain.Context(c) }), Services: teardownSlice(row.Services, ownerTeardownService.restore), State: row.State.restore(), AgentBinding: row.AgentBinding, AgentState: row.AgentState}
	}
	resolveSnapshot := func(index int) (*teardownSnapshot, error) {
		if index == -1 {
			return nil, nil
		}
		if index < 0 || index >= len(snapshots) {
			return nil, errors.New("invalid owner teardown snapshot reference")
		}
		return snapshots[index], nil
	}
	if d.Yards != nil {
		p.Yards = []uninstall.RecoveryYard{}
	}
	restoredAgents := make([]bool, len(snapshots))
	yardIdentities := map[[2]string]bool{}
	for _, row := range d.Yards {
		yard := uninstall.RecoveryYard{Context: domain.Context(row.Context), Environment: row.Environment.restore(), Network: row.Network.restore()}
		if yard.Context.AccessKind != domain.AccessLocal || yard.Context.Paths.DataHome != loaded.Context.Paths.DataHome || yard.Context.Paths.ConfigHome != loaded.Context.Paths.ConfigHome {
			return nil, domain.ErrPlanStale
		}
		if err := yard.Context.Validate(); err != nil {
			return nil, err
		}
		identity := [2]string{yard.Context.IncusProject, yard.Context.YardInstanceName}
		if yardIdentities[identity] {
			return nil, errors.New("duplicate owner teardown yard identity")
		}
		yardIdentities[identity] = true
		if yard.Network != nil && yard.Network.Yard != networkYard(yard.Context) {
			return nil, domain.ErrPlanStale
		}
		var err error
		yard.Snapshot, err = resolveSnapshot(row.Snapshot)
		if err != nil {
			return nil, err
		}
		if yard.Snapshot == nil {
			return nil, errors.New("original owner teardown snapshot is required")
		}
		agentRow := d.Snapshots[row.Snapshot]
		manager := sshagentruntime.Manager{Config: sshagentruntime.Config{Directory: sshagentruntime.Directory(yard.Context.Paths.DataHome, yard.Context.YardName)}}
		yard.Snapshot.Agent, err = manager.RestorePreparedLockRecord(agentRow.Agent, agentRow.AgentBinding, agentRow.AgentState)
		if err != nil {
			return nil, err
		}
		restoredAgents[row.Snapshot] = true
		p.Yards = append(p.Yards, yard)
	}
	p.activeYard = &p.Yards[d.ActiveYard]
	var err error
	p.liveSnapshot, err = resolveSnapshot(d.LiveSnapshot)
	if err != nil {
		return nil, err
	}
	if p.liveSnapshot != nil && !restoredAgents[d.LiveSnapshot] {
		row := d.Snapshots[d.LiveSnapshot]
		manager := sshagentruntime.Manager{Config: sshagentruntime.Config{Directory: sshagentruntime.Directory(loaded.Context.Paths.DataHome, loaded.Context.YardName)}}
		p.liveSnapshot.Agent, err = manager.RestorePreparedLockRecord(row.Agent, row.AgentBinding, row.AgentState)
		if err != nil {
			return nil, err
		}
	}
	canonical, err := captureOwnerTeardownRecord(p)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(raw, canonical) {
		return nil, errors.New("noncanonical owner teardown references or map rows")
	}
	return p, nil
}

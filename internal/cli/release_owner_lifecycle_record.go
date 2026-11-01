package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/adapters/testvmsruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releaseowner/nativejson"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

type ownerLifecycleRecord struct {
	Action                     string
	Force, Changed, VMCPUReady bool
	Status, StartupScope       string
	Observed                   ownerTeardownState
	Steps                      []domain.OperationStep
	Startup                    []byte                                    `json:",omitempty"`
	Publication                *reconcileruntime.PublicationConsumerPlan `json:",omitempty"`
}

func captureOwnerStart(ctx context.Context, operation *CLI, loaded config.Loaded, logicalRoot string) (*ownerLifecycleRecord, error) {
	definition, ok := operation.manifest.Lookup("start")
	if !ok {
		return nil, domain.ErrPlanStale
	}
	execution, err := prepareLifecycleExecution(definition, []string{"--yes"})
	if err != nil {
		return nil, err
	}
	if err := operation.observeLifecycleExecution(ctx, loaded.Context, execution); err != nil {
		return nil, err
	}
	scope, err := operation.startupScopeBinding(ctx, loaded)
	if err != nil {
		return nil, err
	}
	startup, err := operation.prepareResourceStartup(ctx, loaded)
	if err != nil {
		return nil, err
	}
	if startup != nil {
		startup.intent.instanceUUID = execution.observed.Config["volatile.uuid"]
	}
	raw, err := captureOwnerResourceStartup(startup, operation, logicalRoot)
	if err != nil {
		return nil, err
	}
	return &ownerLifecycleRecord{Action: execution.action, Force: execution.force, Changed: execution.changed, VMCPUReady: execution.vmCPUReady, Status: execution.status, StartupScope: scope, Observed: describeTeardownState(ports.ReconcileState{Instance: *execution.observed}), Steps: domain.CloneOperationSteps(execution.approvedSteps), Startup: raw}, nil
}

func (record *ownerLifecycleRecord) restore(loaded config.Loaded) (*lifecycleExecution, error) {
	if record == nil || record.Action != "start" || record.Force || record.Observed.Instance.Name != loaded.Context.YardInstanceName || record.Observed.Instance.Project != loaded.Context.IncusProject || record.Observed.Instance.Config.restore()["volatile.uuid"] == "" {
		return nil, domain.ErrPlanStale
	}
	instance := record.Observed.restore().Instance
	return &lifecycleExecution{action: record.Action, force: record.Force, changed: record.Changed, vmCPUReady: record.VMCPUReady, status: record.Status, observed: &instance, approvedSteps: domain.CloneOperationSteps(record.Steps)}, nil
}

func (p *preparedOwnerChildren) runStart(ctx context.Context, yard string, output io.Writer) error {
	child, ok := p.inits[yard]
	if !ok || child.start == nil {
		return domain.ErrPlanStale
	}
	execution, err := child.start.restore(child.loaded)
	if err != nil {
		return err
	}
	scope, err := p.operation.startupScopeBinding(ctx, child.loaded)
	if err != nil || scope != child.start.StartupScope {
		return domain.ErrPlanStale
	}
	sources, err := p.restoreChildSources(ctx, yard)
	if err != nil {
		return err
	}
	runtime, err := child.runtime.OwnerSourceRuntime(ctx, sources)
	if err != nil {
		return errors.Join(err, sources.Close())
	}
	// Resource definitions carry a private executable path. Rebuild their sealed
	// registry from the retained execution tree, never the live source registry.
	physical, err := ownerResourceSourceCLI(p.operation, child.runtime, runtime)
	if err != nil {
		return errors.Join(err, sources.FinishOwnerSources(err))
	}
	startup, err := restoreOwnerResourceStartup(child.start.Startup, physical, child.loaded, child.runtime.LogicalRepositoryRoot)
	if err != nil {
		return errors.Join(err, sources.FinishOwnerSources(err))
	}
	if startup != nil && startup.intent.instanceUUID != execution.observed.Config["volatile.uuid"] {
		return errors.Join(domain.ErrPlanStale, sources.FinishOwnerSources(domain.ErrPlanStale))
	}
	orchestrator, plan, err := ownerChildOrchestrator(ctx, physical, child.loaded, "start", runtime.RepositoryRoot)
	if err != nil {
		return errors.Join(err, sources.FinishOwnerSources(err))
	}
	if startup != nil {
		unlock, lockErr := lockIntegrationYard(ctx, child.loaded)
		if lockErr != nil {
			return errors.Join(lockErr, sources.FinishOwnerSources(lockErr))
		}
		defer unlock()
		if err := startup.refresh(ctx, physical); err != nil {
			return errors.Join(err, sources.FinishOwnerSources(err))
		}
		if physical.options.NetworkPolicy == nil {
			if err := physical.prepareSudoPrivileges(ctx, output, physical.effectiveUID(), execution.action); err != nil {
				return errors.Join(err, sources.FinishOwnerSources(err))
			}
		}
	}
	if ports.NativeOperationObserverFrom(ctx) != nil && (execution.observed.Type == domain.YardVM || execution.observed.Type == domain.YardContainer) {
		execution.ownerPhysical = ownerStartPhysical{
			cli: physical, yard: child.loaded.Context, uuid: execution.observed.Config["volatile.uuid"], weight: execution.observed.Config["user.subyard.vm_cpu_weight"],
			guard:    func(context.Context) error { _, err := runtime.SelectedRuntimeInputs("core-guest-runtime"); return err },
			original: execution.observed, consumer: child.publicationConsumer, publicationRuntime: &child.runtime,
		}
		execution.ownerPowerConfig = ownerStartPowerConfig{cli: physical, yard: child.loaded.Context, uuid: execution.observed.Config["volatile.uuid"], guard: func(context.Context) error { _, err := runtime.SelectedRuntimeInputs("core-guest-runtime"); return err }}
	}
	result, err := physical.executeLifecycle(ctx, orchestrator, child.loaded.Context, plan, execution, output)
	if err == nil && result.Status != "ok" {
		err = errors.New("original owner temporary start did not complete")
	}
	if err == nil {
		err = startup.apply(ctx, physical, plan.OperationID)
	}
	return errors.Join(err, sources.FinishOwnerSources(err))
}

// Reuse the same retained registry and cached environment binding for original
// resource leaves. It never loads executable definitions from a live root.
func ownerResourceSourceCLI(operation *CLI, original, retained reconcileruntime.Runtime) (*CLI, error) {
	physical := *operation
	physical.ownerResourceContext = operation
	physical.options.RepositoryRoot = retained.RepositoryRoot
	physical.options.WorkingDir = ownerRepositoryValue(physical.options.WorkingDir, operation.options.RepositoryRoot, retained.RepositoryRoot)
	physical.resourceSourceEnvironment = map[string]string{}
	before := environmentMap(original.Environment)
	for key, value := range environmentMap(retained.Environment) {
		if before[key] != value {
			physical.resourceSourceEnvironment[key] = value
		}
	}
	physical.resourceSourceEnvironment["SUBYARD_PROFILE_GUEST_ENVIRONMENT_HOOKS"] = ownerRepositoryValue(before["SUBYARD_PROFILE_GUEST_ENVIRONMENT_HOOKS"], original.RepositoryRoot, retained.RepositoryRoot)
	var err error
	physical.resources, err = resource.Load(retained.RepositoryRoot)
	return &physical, err
}

// This is the original resource leaf inside the existing lifecycle capsule.
// The source recipe owns executable bytes; this leaf owns the approved native
// plan, selected configuration, ingress preview and desired intent.
type ownerResourceStartup struct {
	Loaded                  ownerLoadedSnapshot
	Definition              ownerOrphanDefinition
	Handler, Scope          string
	Schema, Binding, Action string
	Assessment              *ownerOrphanAssessment
	Steps                   []ownerResourceStep
	Consequences            []string
	Before, After           string
	InstanceUUID            string
	Ingress                 ownerResourceIngress
}

// Public steps omit empty containers in JSON. Preserve the approved native
// values here without those projection tags.
type ownerResourceStep struct {
	ID, Target, Observed, Desired string
	Decision                      domain.StepDecision
	Preconditions, DependsOn      []string
	Verify, Consequence           string
}

func describeResourceStep(v domain.OperationStep) ownerResourceStep {
	return ownerResourceStep{v.ID, v.Target, v.Observed, v.Desired, v.Decision, v.Preconditions, v.DependsOn, v.Verify, v.Consequence}
}
func (v ownerResourceStep) restore() domain.OperationStep {
	return domain.OperationStep{ID: v.ID, Target: v.Target, Observed: v.Observed, Desired: v.Desired, Decision: v.Decision, Preconditions: v.Preconditions, DependsOn: v.DependsOn, Verify: v.Verify, Consequence: v.Consequence}
}

type ownerResourceIngress struct {
	Yards         []yardnetwork.Yard
	Contexts      []ownerInitContext
	Target        yardnetwork.Yard
	Before, After ownerResourceNetworkPlan
	Protocol      string
	GuestPort     int
	Up            bool
	Contract      resource.ProxyContract
	Address       string
	Port          int
}

// Existing teardown rows preserve native maps and non-UTF8 byte strings. No
// general executable grammar or independent resource registry is introduced.
type ownerResourceACL struct {
	Name, ETag      string
	Exists          bool
	Config          ownerTeardownMap[string]
	Ingress, Egress []yardnetwork.Rule
}
type ownerResourceObserved struct {
	Yard                     yardnetwork.Yard
	Contracts                []resource.ProxyContract
	State                    ownerTeardownState
	ProjectETag, ProfileETag string
	ProfileUsedBy            []string
	IPv4, MAC                string
	ACL                      ownerResourceACL
}
type ownerResourceUpdate struct {
	Yard      ownerResourceObserved
	NIC       ownerTeardownMap[string]
	Access    string
	ACL       ownerResourceACL
	RemoveACL bool
}
type ownerResourceNetwork struct {
	Name, Type string
	Managed    bool
	Config     ownerTeardownMap[string]
}
type ownerResourceNetworkPlan struct {
	Before, Policy    *ownerTeardownNetwork
	Updates           []ownerResourceUpdate
	Changed, Physical bool
	Fingerprint       string
	Change            yardnetwork.Change
	Yards             []yardnetwork.Yard
	Firewall          string
	Clustered         bool
	Observed          []ownerResourceObserved
	Networks          ownerTeardownMap[ownerResourceNetwork]
	Claims            ownerTeardownMap[ownerTeardownMap[[]yardnetwork.ReservationClaim]]
}

func describeResourceACL(v yardnetwork.ACL) ownerResourceACL {
	return ownerResourceACL{v.Name, v.ETag, v.Exists, teardownMap(v.Config), v.Ingress, v.Egress}
}
func (v ownerResourceACL) restore() yardnetwork.ACL {
	return yardnetwork.ACL{Name: v.Name, ETag: v.ETag, Exists: v.Exists, Config: v.Config.restore(), Ingress: v.Ingress, Egress: v.Egress}
}
func describeResourceObserved(v yardnetwork.ObservedYard) ownerResourceObserved {
	s := ports.ReconcileState{ProjectFound: v.ProjectFound, ProjectConfig: v.ProjectConfig, ProfileFound: v.ProfileFound, ProfileDevices: v.ProfileDevices, InstanceFound: v.InstanceFound, Instance: v.InstanceInfo}
	return ownerResourceObserved{v.Yard, v.IngressContracts, describeTeardownState(s), v.ProjectETag, v.ProfileETag, v.ProfileUsedBy, v.IPv4, v.MAC, describeResourceACL(v.ACL)}
}
func (v ownerResourceObserved) restore() yardnetwork.ObservedYard {
	s := v.State.restore()
	return yardnetwork.ObservedYard{Yard: v.Yard, IngressContracts: v.Contracts, ProjectFound: s.ProjectFound, ProjectConfig: s.ProjectConfig, ProfileFound: s.ProfileFound, ProfileDevices: s.ProfileDevices, InstanceFound: s.InstanceFound, InstanceInfo: s.Instance, ProjectETag: v.ProjectETag, ProfileETag: v.ProfileETag, ProfileUsedBy: v.ProfileUsedBy, IPv4: v.IPv4, MAC: v.MAC, ACL: v.ACL.restore()}
}
func describeResourceNetworkPlan(v yardnetwork.Plan) (ownerResourceNetworkPlan, error) {
	before, err := describeTeardownNetwork(&yardnetwork.RemovalPlan{Stored: v.Before})
	if err != nil {
		return ownerResourceNetworkPlan{}, err
	}
	policy, err := describeTeardownNetwork(&yardnetwork.RemovalPlan{Stored: yardnetwork.StoredPolicy{Policy: v.Policy}})
	if err != nil {
		return ownerResourceNetworkPlan{}, err
	}
	r := ownerResourceNetworkPlan{Before: before, Policy: policy, Changed: v.Changed, Physical: v.Physical, Fingerprint: v.Fingerprint, Change: v.Change, Yards: v.Yards, Firewall: v.Snapshot.Firewall, Clustered: v.Snapshot.Clustered, Observed: teardownSlice(v.Snapshot.Yards, describeResourceObserved)}
	if v.Updates != nil {
		r.Updates = []ownerResourceUpdate{}
	}
	for _, u := range v.Updates {
		r.Updates = append(r.Updates, ownerResourceUpdate{describeResourceObserved(u.Yard), teardownMap(u.NIC), u.Access, describeResourceACL(u.ACL), u.RemoveACL})
	}
	var networks map[string]ownerResourceNetwork
	if v.Snapshot.Networks != nil {
		networks = map[string]ownerResourceNetwork{}
	}
	for k, n := range v.Snapshot.Networks {
		networks[k] = ownerResourceNetwork{n.Name, n.Type, n.Managed, teardownMap(n.Config)}
	}
	r.Networks = teardownMap(networks)
	var claims map[string]ownerTeardownMap[[]yardnetwork.ReservationClaim]
	if v.Snapshot.ReservationClaims != nil {
		claims = map[string]ownerTeardownMap[[]yardnetwork.ReservationClaim]{}
	}
	for k, c := range v.Snapshot.ReservationClaims {
		claims[k] = teardownMap(c)
	}
	r.Claims = teardownMap(claims)
	return r, nil
}
func (v ownerResourceNetworkPlan) restore() yardnetwork.Plan {
	r := yardnetwork.Plan{Before: v.Before.restore().Stored, Policy: v.Policy.restore().Stored.Policy, Changed: v.Changed, Physical: v.Physical, Fingerprint: v.Fingerprint, Change: v.Change, Yards: v.Yards, Snapshot: yardnetwork.Snapshot{Firewall: v.Firewall, Clustered: v.Clustered, Yards: teardownSlice(v.Observed, ownerResourceObserved.restore)}}
	if v.Updates != nil {
		r.Updates = []yardnetwork.Update{}
	}
	for _, u := range v.Updates {
		r.Updates = append(r.Updates, yardnetwork.Update{Yard: u.Yard.restore(), NIC: u.NIC.restore(), Access: u.Access, ACL: u.ACL.restore(), RemoveACL: u.RemoveACL})
	}
	if v.Networks.Present {
		r.Snapshot.Networks = map[string]yardnetwork.Network{}
	}
	for k, n := range v.Networks.restore() {
		r.Snapshot.Networks[k] = yardnetwork.Network{Name: n.Name, Type: n.Type, Managed: n.Managed, Config: n.Config.restore()}
	}
	if v.Claims.Present {
		r.Snapshot.ReservationClaims = map[string]map[string][]yardnetwork.ReservationClaim{}
	}
	for k, c := range v.Claims.restore() {
		r.Snapshot.ReservationClaims[k] = c.restore()
	}
	return r
}

func captureOwnerResourceStartup(startup *resourceStartup, cli *CLI, logicalRoot string) ([]byte, error) {
	if startup == nil {
		return nil, nil
	}
	if startup.ingress == nil || startup.intent == nil {
		return nil, domain.ErrPlanStale
	}
	definition, err := describeOwnerResourceDefinition(startup.definition, cli, logicalRoot)
	if err != nil {
		return nil, err
	}
	handler, err := os.ReadFile(startup.definition.HandlerPath())
	if err != nil {
		return nil, err
	}
	i := startup.ingress
	before, err := describeResourceNetworkPlan(i.preview.Before)
	if err != nil {
		return nil, err
	}
	after, err := describeResourceNetworkPlan(i.preview.After)
	if err != nil {
		return nil, err
	}
	var contexts []ownerInitContext
	if i.contexts != nil {
		contexts = []ownerInitContext{}
	}
	for _, value := range i.contexts {
		value = ownerLoadedLabels(config.Loaded{Context: value}, value.Paths.RepositoryRoot, logicalRoot).Context
		contexts = append(contexts, ownerInitContext(value))
	}
	r := ownerResourceStartup{Loaded: captureOwnerLoaded(ownerLoadedLabels(startup.loaded, startup.loaded.Context.Paths.RepositoryRoot, logicalRoot)), Definition: definition, Handler: operationStateDigest(handler), Scope: startup.scope, Schema: startup.assessment.Schema, Binding: startup.assessment.Binding, Action: startup.localAction, Assessment: describeOwnerOrphanAssessment(&startup.assessment.Assessment), Steps: teardownSlice(startup.assessment.Steps, describeResourceStep), Consequences: startup.consequences, Before: startup.intent.before, After: startup.intent.after, InstanceUUID: startup.intent.instanceUUID, Ingress: ownerResourceIngress{Yards: i.yards, Contexts: contexts, Target: i.preview.Target, Before: before, After: after, Protocol: i.preview.Protocol, GuestPort: i.preview.GuestPort, Up: i.up, Contract: i.contract, Address: i.address, Port: i.port}}
	return nativejson.EncodeLosslessNativeJSON(r, ownerTeardownRecordLimit)
}

func restoreOwnerResourceStartup(raw []byte, cli *CLI, loaded config.Loaded, logicalRoot string) (*resourceStartup, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var r ownerResourceStartup
	if err := nativejson.DecodeLosslessNativeJSON(raw, &r, ownerTeardownRecordLimit); err != nil {
		return nil, err
	}
	definition, ok := cli.resources.Lookup(r.Definition.Command)
	if !ok || definition.Command != r.Definition.Command || r.Assessment == nil || r.Ingress.Before.Before == nil || r.Ingress.Before.Policy == nil || r.Ingress.After.Before == nil || r.Ingress.After.Policy == nil {
		return nil, domain.ErrPlanStale
	}
	description, err := describeOwnerResourceDefinition(definition, cli, logicalRoot)
	if err != nil || !reflect.DeepEqual(description, r.Definition) {
		return nil, domain.ErrPlanStale
	}
	saved, err := r.Loaded.restore(loaded.Catalog)
	if err != nil {
		return nil, err
	}
	// Context paths retain the original owner's labels even though executable
	// reads now use the checked source tree. Compare the original config graph.
	actual := ownerLoadedLabels(loaded, loaded.Context.Paths.RepositoryRoot, logicalRoot)
	if !reflect.DeepEqual(captureOwnerLoaded(saved), captureOwnerLoaded(actual)) {
		return nil, domain.ErrPlanStale
	}
	handler, err := os.ReadFile(definition.HandlerPath())
	if err != nil || operationStateDigest(handler) != r.Handler {
		return nil, domain.ErrPlanStale
	}
	i := r.Ingress
	var contexts []domain.Context
	if i.Contexts != nil {
		contexts = []domain.Context{}
	}
	for _, value := range i.Contexts {
		context := ownerLoadedLabels(config.Loaded{Context: domain.Context(value)}, logicalRoot, loaded.Context.Paths.RepositoryRoot).Context
		contexts = append(contexts, context)
	}
	service := cli.resourceIngressService(loaded, contexts)
	if service == nil {
		return nil, domain.ErrPlanStale
	}
	ingress := &resourceIngress{loaded: loaded, service: service, contexts: contexts, yards: i.Yards, preview: yardnetwork.IngressPreview{Before: i.Before.restore(), After: i.After.restore(), Target: i.Target, Protocol: i.Protocol, GuestPort: i.GuestPort}, up: i.Up, contract: i.Contract, address: i.Address, port: i.Port}
	startup := &resourceStartup{loaded: loaded, definition: definition, consequences: r.Consequences, ingress: ingress, intent: &resourceStartupIntent{loaded: loaded, definition: definition, before: r.Before, after: r.After, instanceUUID: r.InstanceUUID}, localAction: r.Action, assessment: resource.PrepareResult{Schema: r.Schema, Binding: r.Binding, Assessment: *r.Assessment.restore(), Steps: teardownSlice(r.Steps, ownerResourceStep.restore)}, scope: r.Scope}
	canonical, err := captureOwnerResourceStartup(startup, cli, logicalRoot)
	if err != nil || !bytes.Equal(canonical, raw) {
		return nil, domain.ErrPlanStale
	}
	return startup, nil
}

func (p *preparedOwnerChildren) runPower(ctx context.Context, yard string) error {
	child, ok := p.inits[yard]
	if !ok || child.power == nil {
		return domain.ErrPlanStale
	}
	if p.publicationInstallation != nil && child.runtime.OwnerPublicationRecord() != nil {
		var err error
		child.runtime, err = child.runtime.WithOwnerPublicationInstallation(ctx, p.publicationInstallation)
		if err != nil {
			return err
		}
	}
	prepared, err := child.runtime.RestoreOwnerHostPower(ctx, *child.power)
	if err != nil {
		return err
	}
	defer prepared.Close()
	sources, err := p.restoreChildSources(ctx, yard)
	if err != nil {
		return err
	}
	var publicationPlan *reconcileruntime.PublicationConsumerPlan
	if child.start != nil {
		publicationPlan = child.start.Publication
	}
	if err := child.runtime.ApplyOwnerPublicationPower(ctx, prepared, sources, publicationPlan); err != nil {
		return err
	}
	if result := child.runtime.OwnerPublicationInstallation(); result != nil {
		p.publicationInstallation = result
	}
	if child.start != nil && child.start.Publication != nil {
		consumer, err := child.runtime.OwnerPublicationConsumer(ctx)
		if err != nil {
			return err
		}
		child.publicationConsumer = &consumer
		p.inits[yard] = child
	}
	return nil
}

func (p *preparedOwnerChildren) readBrokerStatus(ctx context.Context, yard string, output io.Writer) error {
	child, ok := p.inits[yard]
	if !ok || output == nil {
		return domain.ErrPlanStale
	}
	return p.operation.readTestVMBrokerStatus(ctx, child.loaded, output)
}

func (cli *CLI) readTestVMBrokerStatus(ctx context.Context, loaded config.Loaded, output io.Writer) error {
	if output == nil {
		return domain.ErrPlanStale
	}
	pool, err := cli.probeTestVMPool(ports.WithoutNativeOperationObserver(ctx), loaded)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(struct {
		Pool *testvmsruntime.LeasePool `json:"pool"`
	}{pool})
}

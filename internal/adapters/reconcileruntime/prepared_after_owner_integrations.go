package reconcileruntime

import (
	"context"
	"crypto/sha256"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releaseowner/nativejson"
)

// Keys, immutable Desired values, route declarations and hook authority stay in
// the original recipe. The physical rows are indexed by that closed scope.
type afterOwnerIntegrationFact struct {
	Desired, Observed string
	AtDesired         bool
}
type afterOwnerIntegrationRoute struct {
	Marker, LocalMarker string
	Device, LocalDevice ownerPlanMap[string]
}
type afterOwnerIntegrationPhysical struct {
	Version                           int
	Configs                           afterOwnerConfigPhysical
	Stopped                           bool
	Fingerprint, Scope, InstanceScope string
	Changed                           bool
	Facts                             []afterOwnerIntegrationFact
	Routes                            []afterOwnerIntegrationRoute
	Hooks                             ownerHookObservation
}

func (runtime Runtime) PrepareAfterOwnerIntegrationSources(ctx context.Context) (*PreparedInstalledRuntime, error) {
	configs, err := runtime.prepareConfigPayloads(afterOwnerConfigInstance)
	if err != nil {
		return nil, err
	}
	entries, err := runtime.integrationArtifactsPhase("apply")
	if err != nil {
		return nil, err
	}
	plan := IntegrationPlan{}
	plan.Scope, _, err = runtime.IntegrationScope()
	if err != nil {
		return nil, err
	}
	plan.sources, plan.packageSources, err = runtime.integrationSourceBindingsPhase("inspect")
	if err != nil {
		return nil, err
	}
	keys := map[string]string{}
	for _, entry := range entries {
		name := entry.Path
		if name == "" {
			name = entry.ID
		}
		keys[entry.Kind+":"+name] = ""
	}
	for _, file := range configs.files {
		if file.ownedFormat != "" {
			if payload, ok := configs.payloads[file.destination]; ok {
				keys["structured:"+file.destination] = fmt.Sprintf("%x", sha256.Sum256(payload))
			}
		}
	}
	definitions, err := runtime.integrationDefinitions()
	if err != nil {
		return nil, err
	}
	skipped := []string{}
	for _, definition := range definitions {
		if definition.Lifecycle != nil {
			skipped = append(skipped, definition.Name)
			desired := ""
			if !slices.Contains(strings.Fields(runtime.environmentValue("CODING_TOOL_INTEGRATIONS")), definition.Name) {
				desired = "absent"
			}
			keys["lifecycle:"+definition.Name] = desired
		}
	}
	commands, err := runtime.provisionAgentCommands()
	if err != nil {
		return nil, err
	}
	for _, command := range commands {
		keys["command:"+command] = "ready"
	}
	checks, err := runtime.provisionAgentChecks()
	if err != nil {
		return nil, err
	}
	for _, check := range checks {
		keys["check:"+check] = "ready"
	}
	keys["hooks"] = "ready"
	keys["services"] = "deselected managed services inactive; role-forbidden utility absent"
	for key, desired := range keys {
		plan.facts = append(plan.facts, integrationFact{Key: key, Desired: desired})
	}
	slices.SortFunc(plan.facts, func(a, b integrationFact) int { return strings.Compare(a.Key, b.Key) })
	plan.profileRoutes = runtime.integrationProfileRoutes(ctx, ports.InstanceInfo{})
	if runtime.HookPlan == nil {
		runtime.HookPlan, err = runtime.PrepareProjectHooks(ctx, true)
		if err != nil {
			return nil, err
		}
	}
	p := &PreparedIntegrations{configs: configs, entries: entries, plan: plan, runtime: runtime.configRuntimeBinding(), skipped: strings.Join(skipped, " "), hooks: runtime.HookPlan}
	p.binding = p.calculateBinding()
	return runtime.PrepareMaterializedIntegrations(ctx, p)
}

func (p *PreparedInstalledRuntime) AfterOwnerIntegrationPhysicalBound() (int, error) {
	original, _, err := decodeMaterializedMetadata(p)
	if err != nil || original.Integrations == nil {
		return 0, domain.ErrPlanStale
	}
	d := original.Integrations
	f := strings.Repeat("f", 64)
	value := afterOwnerIntegrationPhysical{Version: 1, Configs: afterOwnerConfigPhysical{1, []byte(strings.Repeat("x", 128)), f}, Fingerprint: f, Scope: f, InstanceScope: f, Changed: true}
	for _, fact := range d.Plan.Facts {
		desired := ""
		if fact.Desired == "" {
			desired = f
		}
		observed := f
		if strings.HasPrefix(fact.Key, "lifecycle:") && fact.Desired == "absent" {
			observed = f + ":" + f
		}
		value.Facts = append(value.Facts, afterOwnerIntegrationFact{desired, observed, false})
	}
	for _, route := range d.Plan.ProfileRoutes {
		value.Routes = append(value.Routes, afterOwnerIntegrationRoute{strings.Repeat("x", 128), strings.Repeat("x", 128), ownerMap(route.Device), ownerMap(route.Device)})
	}
	hooks := d.Hooks.restore()
	observation := hookObservation{Projects: f, Wiring: f, Hooks: slices.Clone(hooks.allowedHooks), Facts: map[string]string{}, Resolved: map[string]string{}}
	for _, hook := range hooks.allowedHooks {
		observation.Facts[hook] = f
		observation.Resolved[hook] = strings.Repeat("x", 4096)
	}
	for _, record := range hooks.ownerProjects {
		observation.Roots = append(observation.Roots, filepath.Dir(record.YardPath))
	}
	observation.Roots = append(observation.Roots, hooks.approved.Roots...)
	value.Hooks = ownerHook(observation)
	raw, err := nativejson.EncodeLosslessNativeJSON(value, 32<<20)
	return len(raw), err
}

func (runtime Runtime) BindAfterOwnerIntegrations(ctx context.Context, raw, physical []byte) (*PreparedInstalledRuntime, error) {
	runtime.afterOwnerConfigSource = physical == nil
	if physical != nil {
		var value afterOwnerIntegrationPhysical
		if nativejson.DecodeLosslessNativeJSON(physical, &value, 32<<20) != nil || value.Version != 1 {
			return nil, domain.ErrPlanStale
		}
		runtime.afterOwnerConfigs = &value.Configs
		runtime.afterOwnerIntegrations = &value
	}
	runtime.afterOwnerIntegrationSource = true
	return runtime.BindInstalledRuntimeContext(ctx, raw)
}

func (runtime Runtime) ResolveAfterOwnerIntegrations(ctx context.Context, raw []byte) (*PreparedInstalledRuntime, []byte, error) {
	p, err := runtime.BindAfterOwnerIntegrations(ctx, raw, nil)
	if err != nil {
		return nil, nil, err
	}
	defer p.Close()
	configs, err := runtime.afterOwnerPhysicalConfigs(ctx)
	if err != nil {
		return nil, nil, err
	}
	value := afterOwnerIntegrationPhysical{Version: 1, Configs: afterOwnerConfigPhysical{1, []byte(configs.instance), configs.binding}}
	instance, err := runtime.Incus.Instance(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName)
	if err != nil {
		return nil, nil, err
	}
	if !strings.EqualFold(instance.Status, "running") {
		if !strings.EqualFold(instance.Status, "stopped") {
			return nil, nil, domain.ErrPlanStale
		}
		value.Stopped = true
	} else {
		execution, err := runtime.materializedIntegrationExecution(ctx, p)
		if err != nil {
			return nil, nil, err
		}
		if err = execution.CheckProjectHookPlan(ctx, true); err != nil {
			return nil, nil, err
		}
		plan, err := execution.IntegrationPlan(ctx)
		if err != nil {
			return nil, nil, err
		}
		if plan.sources != p.integrations.plan.sources || !reflect.DeepEqual(plan.packageSources, p.integrations.plan.packageSources) || len(plan.Adoption) != 0 || plan.AdoptionFingerprint != "" || !reflect.DeepEqual(plan.profileRoutes, p.integrations.plan.profileRoutes) {
			return nil, nil, domain.ErrPlanStale
		}
		value.Fingerprint, value.Scope, value.InstanceScope, value.Changed = plan.Fingerprint, plan.Scope, plan.instanceScope, plan.Changed
		facts := map[string]integrationFact{}
		for _, fact := range plan.facts {
			if _, exists := facts[fact.Key]; exists {
				return nil, nil, domain.ErrPlanStale
			}
			facts[fact.Key] = fact
		}
		for _, original := range p.integrations.plan.facts {
			fact, ok := facts[original.Key]
			if !ok || original.Desired != "" && original.Desired != fact.Desired || len(fact.Observed) > 129 {
				return nil, nil, domain.ErrPlanStale
			}
			desired := ""
			if original.Desired == "" {
				desired = fact.Desired
			}
			value.Facts = append(value.Facts, afterOwnerIntegrationFact{desired, fact.Observed, fact.AtDesired})
			delete(facts, original.Key)
		}
		if len(facts) != 0 {
			return nil, nil, domain.ErrPlanStale
		}
		for _, route := range plan.profileRoutes {
			device, local := plan.instance.Devices[route.DeviceName], plan.instance.LocalDevices[route.DeviceName]
			if device != nil && !maps.Equal(device, route.Device) || local != nil && !maps.Equal(local, route.Device) {
				return nil, nil, domain.ErrPlanStale
			}
			value.Routes = append(value.Routes, afterOwnerIntegrationRoute{plan.instance.Config[route.MarkerKey], plan.instance.LocalConfig[route.MarkerKey], ownerMap(device), ownerMap(local)})
		}
		value.Hooks = ownerHook(execution.HookPlan.current)
	}
	physical, err := nativejson.EncodeLosslessNativeJSON(value, 32<<20)
	if err != nil {
		return nil, nil, err
	}
	bound, err := runtime.BindAfterOwnerIntegrations(ctx, raw, physical)
	return bound, physical, err
}

func (p *PreparedInstalledRuntime) overlayAfterOwnerIntegrations(d *materializedIntegrations) (IntegrationPlan, *ProjectHookPlan, error) {
	plan, hooks := d.Plan.restore(), d.Hooks.restore()
	v := p.afterOwnerIntegrations
	if v == nil {
		return plan, hooks, nil
	}
	if v.Version != 1 || v.Configs.Version != 1 || !domain.SafeID(string(v.Configs.Instance)) || len(v.Configs.Instance) > 128 || !profileRuntimeDigest.MatchString(v.Configs.Binding) {
		return plan, nil, domain.ErrPlanStale
	}
	if v.Stopped {
		if len(v.Facts) != 0 || len(v.Routes) != 0 {
			return plan, nil, domain.ErrPlanStale
		}
		return plan, hooks, nil
	}
	if !profileRuntimeDigest.MatchString(v.Fingerprint) || !profileRuntimeDigest.MatchString(v.Scope) || v.InstanceScope != d.Plan.Scope || len(v.Facts) != len(plan.facts) || len(v.Routes) != len(plan.profileRoutes) {
		return plan, nil, domain.ErrPlanStale
	}
	plan.Fingerprint, plan.Scope, plan.instanceScope, plan.Changed = v.Fingerprint, v.Scope, v.InstanceScope, v.Changed
	for i, fact := range v.Facts {
		if len(fact.Observed) > 129 || plan.facts[i].Desired != "" && fact.Desired != "" {
			return plan, nil, domain.ErrPlanStale
		}
		if plan.facts[i].Desired == "" {
			if !profileRuntimeDigest.MatchString(fact.Desired) {
				return plan, nil, domain.ErrPlanStale
			}
			plan.facts[i].Desired = fact.Desired
		}
		plan.facts[i].Observed, plan.facts[i].AtDesired = fact.Observed, fact.AtDesired
	}
	plan.instance = ports.InstanceInfo{Config: map[string]string{}, LocalConfig: map[string]string{}, Devices: map[string]map[string]string{}, LocalDevices: map[string]map[string]string{}}
	for i, row := range v.Routes {
		route := plan.profileRoutes[i]
		device, local := row.Device.restore(), row.LocalDevice.restore()
		if len(row.Marker) > 128 || len(row.LocalMarker) > 128 || device != nil && !maps.Equal(device, route.Device) || local != nil && !maps.Equal(local, route.Device) {
			return plan, nil, domain.ErrPlanStale
		}
		plan.instance.Config[route.MarkerKey], plan.instance.LocalConfig[route.MarkerKey] = row.Marker, row.LocalMarker
		plan.instance.Devices[route.DeviceName], plan.instance.LocalDevices[route.DeviceName] = device, local
	}
	observation := v.Hooks.restore()
	for _, hook := range observation.Hooks {
		if !slices.Contains(hooks.allowedHooks, hook) {
			return plan, nil, domain.ErrPlanStale
		}
	}
	hooks.approved, hooks.current = observation, observation
	hooks.conditional = false
	hooks.resolvedProjects, hooks.executionWiring = observation.Projects, observation.Wiring
	return plan, hooks, nil
}

func (p *PreparedInstalledRuntime) PreparedIntegrations() *PreparedIntegrations {
	return p.integrations
}
func (p *PreparedIntegrations) Plan() IntegrationPlan { return p.plan }

func (p *PreparedInstalledRuntime) ReserveAfterOwnerIntegrationMetadata(template []byte) (*PreparedInstalledRuntime, error) {
	return p.reserveAfterOwnerMetadata(template)
}
func (runtime Runtime) ObserveAfterOwnerIntegrations(ctx context.Context, p *PreparedInstalledRuntime) (IntegrationPlan, bool, error) {
	if p == nil || p.afterOwnerIntegrations == nil || p.integrations == nil {
		return IntegrationPlan{}, false, domain.ErrPlanStale
	}
	instance, err := runtime.Incus.Instance(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName)
	if err != nil || instance.Config["volatile.uuid"] != string(p.afterOwnerIntegrations.Configs.Instance) {
		return IntegrationPlan{}, false, domain.ErrPlanStale
	}
	if strings.EqualFold(instance.Status, "stopped") {
		return p.integrations.plan, true, nil
	}
	if p.afterOwnerIntegrations.Stopped || !strings.EqualFold(instance.Status, "running") {
		return IntegrationPlan{}, false, domain.ErrPlanStale
	}
	execution, err := runtime.materializedIntegrationExecution(ctx, p)
	if err != nil {
		return IntegrationPlan{}, false, err
	}
	if err = execution.CheckProjectHookPlan(ctx, true); err != nil {
		return IntegrationPlan{}, false, err
	}
	fresh, err := execution.IntegrationPlan(ctx)
	if err != nil {
		return fresh, false, err
	}
	compared, err := execution.ProjectIntegrationPlanAfterProfileActivation(ctx, p.integrations.plan, fresh)
	if err != nil {
		return fresh, false, err
	}
	if err = CheckIntegrationPlan(p.integrations.plan, compared); err != nil {
		return fresh, false, err
	}
	return compared, false, nil
}

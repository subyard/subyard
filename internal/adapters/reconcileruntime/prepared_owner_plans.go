package reconcileruntime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/releaseowner/nativejson"
)

// These closed descriptors contain no interfaces or executable definitions.
// Wire strings preserve arbitrary original Go string bytes. Maps are sorted rows;
// presence bits retain the distinction between an empty and an absent container.
type ownerPlanSlice[T any] struct {
	Present bool
	Values  []T
}

func ownerSlice[T any](values []T) ownerPlanSlice[T] {
	return ownerPlanSlice[T]{values != nil, values}
}

func (values ownerPlanSlice[T]) restore() []T {
	if !values.Present {
		return nil
	}
	return append([]T{}, values.Values...)
}

type ownerPlanEntry[T any] struct {
	Key   string
	Value T
}

type ownerPlanMap[T any] struct {
	Present bool
	Entries []ownerPlanEntry[T]
}

func ownerMap[T any](values map[string]T) ownerPlanMap[T] {
	result := ownerPlanMap[T]{Present: values != nil}
	for key, value := range values {
		result.Entries = append(result.Entries, ownerPlanEntry[T]{key, value})
	}
	slices.SortFunc(result.Entries, func(a, b ownerPlanEntry[T]) int { return bytes.Compare([]byte(a.Key), []byte(b.Key)) })
	return result
}

func (values ownerPlanMap[T]) restore() map[string]T {
	if !values.Present {
		return nil
	}
	result := make(map[string]T, len(values.Entries))
	for _, entry := range values.Entries {
		result[entry.Key] = entry.Value
	}
	return result
}

type ownerPlanDefinition struct{ Name, Root, Activation string }
type ownerRuntimePlan struct {
	Definitions                 ownerPlanSlice[ownerPlanDefinition]
	Approved, Current, Resolved ownerPlanMap[ports.RuntimeObservation]
	Conditional                 ownerPlanMap[bool]
	Inputs                      string
	Unavailable                 bool
}

type ownerHookObservation struct {
	Projects, Wiring string
	Hooks, Roots     ownerPlanSlice[string]
	Facts, Resolved  ownerPlanMap[string]
}

func ownerHook(observation hookObservation) ownerHookObservation {
	return ownerHookObservation{observation.Projects, observation.Wiring, ownerSlice(observation.Hooks), ownerSlice(observation.Roots), ownerMap(observation.Facts), ownerMap(observation.Resolved)}
}

func (d ownerHookObservation) restore() hookObservation {
	return hookObservation{d.Projects, d.Wiring, d.Hooks.restore(), d.Roots.restore(), d.Facts.restore(), d.Resolved.restore()}
}

type ownerMetadataPlan struct {
	Targets           ownerPlanSlice[projectMetadataTarget]
	Expected          ownerPlanSlice[byte]
	Projects          string
	ProjectedBindings ownerPlanMap[string]
}

type ownerHookPlan struct {
	Plan                           ownerHookScalars
	Approved, Current, Provisioned ownerHookObservation
	AllowedHooks                   ownerPlanSlice[string]
	OwnerProjects                  ownerPlanSlice[domain.ProjectRecord]
	HookBindings                   ownerPlanMap[string]
	Metadata                       *ownerMetadataPlan
}

type ownerHookScalars struct {
	Conditional, Unavailable, Completed, Stopped                bool
	Source, ResolvedProjects, ExecutionWiring, IntegrationScope string
	MetadataCompleted                                           bool
}

func describeOwnerHooks(plan *ProjectHookPlan) *ownerHookPlan {
	d := describeHookPlan(plan)
	if d == nil {
		return nil
	}
	result := &ownerHookPlan{Plan: ownerHookScalars{d.Conditional, d.Unavailable, d.Completed, d.Stopped, d.Source, d.ResolvedProjects, d.ExecutionWiring, d.IntegrationScope, d.MetadataCompleted}, Approved: ownerHook(d.Approved), Current: ownerHook(d.Current), Provisioned: ownerHook(d.Provisioned), AllowedHooks: ownerSlice(d.AllowedHooks), OwnerProjects: ownerSlice(d.OwnerProjects), HookBindings: ownerMap(d.HookBindings)}
	if d.Metadata != nil {
		result.Metadata = &ownerMetadataPlan{ownerSlice(d.Metadata.Targets), ownerSlice([]byte(d.Metadata.Expected)), d.Metadata.Projects, ownerMap(d.Metadata.ProjectedBindings)}
	}
	return result
}

func (d *ownerHookPlan) restore() *ProjectHookPlan {
	if d == nil {
		return nil
	}
	p := materializedHookPlan{Conditional: d.Plan.Conditional, Unavailable: d.Plan.Unavailable, Completed: d.Plan.Completed, Stopped: d.Plan.Stopped, Source: d.Plan.Source, ResolvedProjects: d.Plan.ResolvedProjects, ExecutionWiring: d.Plan.ExecutionWiring, IntegrationScope: d.Plan.IntegrationScope, MetadataCompleted: d.Plan.MetadataCompleted}
	p.Approved, p.Current, p.Provisioned = d.Approved.restore(), d.Current.restore(), d.Provisioned.restore()
	p.AllowedHooks, p.OwnerProjects, p.HookBindings = d.AllowedHooks.restore(), d.OwnerProjects.restore(), d.HookBindings.restore()
	if d.Metadata != nil {
		p.Metadata = &projectMetadataPlan{Targets: d.Metadata.Targets.restore(), projectMetadataObservation: projectMetadataObservation{Expected: d.Metadata.Expected.restore(), Projects: d.Metadata.Projects}, ProjectedBindings: d.Metadata.ProjectedBindings.restore()}
	}
	return p.restore()
}

type ownerPlanDevices = ownerPlanMap[ownerPlanMap[string]]

func ownerDevices(devices map[string]map[string]string) ownerPlanDevices {
	if devices == nil {
		return ownerPlanDevices{}
	}
	values := make(map[string]ownerPlanMap[string], len(devices))
	for key, value := range devices {
		values[key] = ownerMap(value)
	}
	return ownerMap(values)
}

func restoreOwnerDevices(d ownerPlanDevices) map[string]map[string]string {
	if !d.Present {
		return nil
	}
	values := make(map[string]map[string]string, len(d.Entries))
	for _, entry := range d.Entries {
		values[entry.Key] = entry.Value.restore()
	}
	return values
}

type ownerIntegrationRoute struct {
	Owner, MarkerKey, DeviceName, Marker string
	Device                               ownerPlanMap[string]
}

type ownerIntegrationPlan struct {
	Plan                                ownerIntegrationScalars
	Steps, Adoption, Fingerprints       ownerPlanSlice[string]
	Facts                               ownerPlanSlice[integrationFact]
	PackageSources, Config, LocalConfig ownerPlanMap[string]
	Devices, LocalDevices               ownerPlanDevices
	Routes                              ownerPlanSlice[ownerIntegrationRoute]
}

type ownerIntegrationPublic struct {
	Fingerprint         string
	Changed             bool
	AdoptionFingerprint string
}

type ownerInstancePublic struct {
	Name, Project string
	Type          domain.YardKind
	Status        string
}

type ownerIntegrationScalars struct {
	Plan           ownerIntegrationPublic
	Scope, Sources string
	Instance       ownerInstancePublic
	InstanceScope  string
}

func describeOwnerIntegration(plan IntegrationPlan) ownerIntegrationPlan {
	d := describeIntegrationPlan(plan)
	result := ownerIntegrationPlan{Plan: ownerIntegrationScalars{ownerIntegrationPublic{d.Plan.Fingerprint, d.Plan.Changed, d.Plan.AdoptionFingerprint}, d.Scope, d.Sources, ownerInstancePublic{d.Instance.Name, d.Instance.Project, d.Instance.Type, d.Instance.Status}, d.InstanceScope}, Steps: ownerSlice(d.Plan.Steps), Adoption: ownerSlice(d.Plan.Adoption), Fingerprints: ownerSlice(d.Fingerprints), Facts: ownerSlice(d.Facts), PackageSources: ownerMap(d.PackageSources), Config: ownerMap(d.Instance.Config), LocalConfig: ownerMap(d.Instance.LocalConfig), Devices: ownerDevices(d.Instance.Devices), LocalDevices: ownerDevices(d.Instance.LocalDevices), Routes: ownerPlanSlice[ownerIntegrationRoute]{Present: d.ProfileRoutes != nil}}
	for _, route := range d.ProfileRoutes {
		result.Routes.Values = append(result.Routes.Values, ownerIntegrationRoute{route.Owner, route.MarkerKey, route.DeviceName, route.Marker, ownerMap(route.Device)})
	}
	return result
}

func (d ownerIntegrationPlan) restore() IntegrationPlan {
	p := materializedIntegrationPlan{Plan: IntegrationPlan{Fingerprint: d.Plan.Plan.Fingerprint, Changed: d.Plan.Plan.Changed, AdoptionFingerprint: d.Plan.Plan.AdoptionFingerprint}, Scope: d.Plan.Scope, Sources: d.Plan.Sources, Instance: ports.InstanceInfo{Name: d.Plan.Instance.Name, Project: d.Plan.Instance.Project, Type: d.Plan.Instance.Type, Status: d.Plan.Instance.Status}, InstanceScope: d.Plan.InstanceScope}
	p.Plan.Steps, p.Plan.Adoption, p.Fingerprints, p.Facts, p.PackageSources = d.Steps.restore(), d.Adoption.restore(), d.Fingerprints.restore(), d.Facts.restore(), d.PackageSources.restore()
	p.Instance.Config, p.Instance.LocalConfig = d.Config.restore(), d.LocalConfig.restore()
	p.Instance.Devices, p.Instance.LocalDevices = restoreOwnerDevices(d.Devices), restoreOwnerDevices(d.LocalDevices)
	if d.Routes.Present {
		p.ProfileRoutes = []integrationProfileRoute{}
	}
	for _, route := range d.Routes.Values {
		p.ProfileRoutes = append(p.ProfileRoutes, integrationProfileRoute{route.Owner, route.MarkerKey, route.DeviceName, route.Marker, route.Device.restore()})
	}
	return p.restore()
}

type ownerNativePlans struct {
	Version                      uint32
	SourceInputs                 string
	Environment                  ownerPlanSlice[string]
	Runtime                      *ownerRuntimePlan
	Hooks                        *ownerHookPlan
	Adoption                     ownerIntegrationPlan
	MetadataProjection           ownerPlanSlice[byte]
	AdoptLegacyIntegrations      bool
	LegacyIntegrationFingerprint string
}

func ownerDefinitionRows(runtime Runtime, definitions []profile.Definition) []ownerPlanDefinition {
	if definitions == nil {
		return nil
	}
	result := make([]ownerPlanDefinition, 0, len(definitions))
	for _, definition := range definitions {
		result = append(result, ownerPlanDefinition{definition.Name, runtime.bindingPath(definition.Root), definition.Runtime.ActivationID})
	}
	return result
}

func encodeOwnerPlans(d ownerNativePlans) ([]byte, error) {
	return nativejson.EncodeLosslessNativeJSON(d, MaxInstalledRecipe)
}

func ownerPlansHaveInputs(d ownerNativePlans) bool {
	return d.Runtime != nil && (d.Runtime.Inputs != "" || len(d.Runtime.Definitions.Values) != 0) || d.Hooks != nil && d.Hooks.Plan.Source != "" || d.Adoption.Plan.Sources != "" || d.Adoption.Plan.Plan.Fingerprint != ""
}

func ownerNativeEnvironment(runtime Runtime) []string {
	values := runtime.bindingEnvironment(runtime.Environment)
	values = slices.DeleteFunc(values, func(value string) bool {
		return strings.HasPrefix(value, "SUBYARD_OPERATION_ID=") || strings.HasPrefix(value, "SUBYARD_SUDO_PREAUTHORIZED=")
	})
	slices.Sort(values)
	return values
}

// Rebuilding only the closed containers rejects duplicate/unsorted map rows,
// contradictory presence bits, and data smuggled into unused descriptor fields.
func normalizeOwnerPlans(d ownerNativePlans) ownerNativePlans {
	d.Hooks = describeOwnerHooks(d.Hooks.restore())
	d.Adoption = describeOwnerIntegration(d.Adoption.restore())
	d.MetadataProjection = ownerSlice(d.MetadataProjection.restore())
	d.Environment = ownerSlice(d.Environment.restore())
	if p := d.Runtime; p != nil {
		copy := *p
		copy.Definitions = ownerSlice(p.Definitions.restore())
		copy.Approved, copy.Current, copy.Resolved = ownerMap(p.Approved.restore()), ownerMap(p.Current.restore()), ownerMap(p.Resolved.restore())
		copy.Conditional = ownerMap(p.Conditional.restore())
		d.Runtime = &copy
	}
	return d
}

func validateOwnerRuntime(p *ownerRuntimePlan) error {
	ids := map[string]bool{}
	for _, definition := range p.Definitions.Values {
		if definition.Activation == "" || ids[definition.Activation] {
			return errors.New("invalid owner native definition descriptor")
		}
		ids[definition.Activation] = true
	}
	for _, observations := range []ownerPlanMap[ports.RuntimeObservation]{p.Approved, p.Current, p.Resolved} {
		for _, entry := range observations.Entries {
			if !ids[entry.Key] {
				return errors.New("unknown owner native observation")
			}
			payload, err := json.Marshal(entry.Value)
			if err != nil {
				return err
			}
			if _, err := decodeProfileRuntimeObservation(payload); err != nil {
				return err
			}
		}
	}
	for _, entry := range p.Conditional.Entries {
		if !ids[entry.Key] {
			return errors.New("unknown conditional owner native observation")
		}
	}
	return nil
}

// CaptureOwnerNativePlans retains original owner observations and progress. It
// never prepares work or observes native state.
func (runtime Runtime) CaptureOwnerNativePlans(adoption IntegrationPlan) ([]byte, error) {
	d := ownerNativePlans{Version: 1, Hooks: describeOwnerHooks(runtime.HookPlan), Adoption: describeOwnerIntegration(adoption), MetadataProjection: ownerSlice(runtime.projectMetadataProjection), AdoptLegacyIntegrations: runtime.AdoptLegacyIntegrations, LegacyIntegrationFingerprint: runtime.LegacyIntegrationFingerprint}
	if p := runtime.RuntimePlan; p != nil {
		d.Runtime = &ownerRuntimePlan{ownerSlice(ownerDefinitionRows(runtime, p.definitions)), ownerMap(p.approved), ownerMap(p.current), ownerMap(p.resolved), ownerMap(p.conditional), p.inputs, p.unavailable}
	}
	if ownerPlansHaveInputs(d) {
		d.Environment = ownerSlice(ownerNativeEnvironment(runtime))
		var err error
		d.SourceInputs, err = runtime.profileRuntimeInputs(nil)
		if err != nil {
			return nil, err
		}
	}
	return encodeOwnerPlans(d)
}

// RestoreOwnerNativePlans validates source and input identity before attaching
// the original approved/current/conditional/resolved owner maps.
func (runtime Runtime) RestoreOwnerNativePlans(raw []byte) (Runtime, IntegrationPlan, error) {
	fail := func(err error) (Runtime, IntegrationPlan, error) { return Runtime{}, IntegrationPlan{}, err }
	if len(raw) == 0 || len(raw) > MaxInstalledRecipe {
		return fail(errors.New("invalid owner native plan size"))
	}
	var d ownerNativePlans
	if err := nativejson.DecodeLosslessNativeJSON(raw, &d, MaxInstalledRecipe); err != nil {
		return fail(err)
	}
	if d.Version != 1 {
		return fail(errors.New("unsupported owner native plan descriptor"))
	}
	canonical, err := encodeOwnerPlans(normalizeOwnerPlans(d))
	if err != nil || !bytes.Equal(raw, canonical) {
		return fail(errors.New("noncanonical owner native plan descriptor"))
	}
	if ownerPlansHaveInputs(d) {
		if !slices.Equal(ownerNativeEnvironment(runtime), d.Environment.Values) {
			return fail(fmt.Errorf("%w: owner native environment changed", domain.ErrPlanStale))
		}
		inputs, err := runtime.profileRuntimeInputs(nil)
		if err != nil {
			return fail(err)
		}
		if inputs != d.SourceInputs {
			return fail(fmt.Errorf("%w: owner native sources changed", domain.ErrPlanStale))
		}
	} else if d.SourceInputs != "" || d.Environment.Present || len(d.Environment.Values) != 0 {
		return fail(errors.New("unexpected owner native source descriptor"))
	}
	var plan *ProfileRuntimePlan
	if p := d.Runtime; p != nil {
		if err := validateOwnerRuntime(p); err != nil {
			return fail(err)
		}
		var definitions []profile.Definition
		if p.Inputs != "" || len(p.Definitions.Values) != 0 {
			definitions, err = runtime.profileRuntimeDefinitions()
			if err != nil {
				return fail(err)
			}
			if !slices.Equal(ownerDefinitionRows(runtime, definitions), p.Definitions.Values) {
				return fail(domain.ErrPlanStale)
			}
			inputs, err := runtime.profileRuntimeInputs(definitions)
			if err != nil {
				return fail(err)
			}
			if inputs != p.Inputs {
				return fail(domain.ErrPlanStale)
			}
		}
		if !p.Definitions.Present {
			definitions = nil
		} else if definitions == nil {
			definitions = []profile.Definition{}
		}
		plan = &ProfileRuntimePlan{definitions: definitions, approved: p.Approved.restore(), current: p.Current.restore(), conditional: p.Conditional.restore(), inputs: p.Inputs, resolved: p.Resolved.restore(), unavailable: p.Unavailable}
	}
	runtime.RuntimePlan, runtime.HookPlan = plan, d.Hooks.restore()
	runtime.projectMetadataProjection = d.MetadataProjection.restore()
	runtime.AdoptLegacyIntegrations, runtime.LegacyIntegrationFingerprint = d.AdoptLegacyIntegrations, d.LegacyIntegrationFingerprint
	return runtime, d.Adoption.restore(), nil
}

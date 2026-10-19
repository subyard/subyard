package reconcileruntime

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

// BindAfterOwnerRuntimeSources delegates the source proof to the existing
// fixed runtime binder; the reserved inert metadata never becomes a script.
func (runtime Runtime) BindAfterOwnerRuntimeSources(ctx context.Context, raw []byte) (*PreparedInstalledRuntime, error) {
	var r installedRecipe
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&r) != nil {
		return nil, domain.ErrPlanStale
	}
	canonical, _ := json.Marshal(r)
	if !bytes.Equal(raw, canonical) {
		return nil, domain.ErrPlanStale
	}
	metadata := bytes.Clone(r.AfterOwner)
	if metadata == nil || r.Owner == "materialized-files" || r.Owner == "materialized-integrations" || r.Owner == "test-vm-broker" || r.Owner == "route-consumers" {
		return nil, domain.ErrPlanStale
	}
	r.AfterOwner = nil
	base, _ := json.Marshal(r)
	p, err := runtime.BindInstalledRuntimeContext(ctx, base)
	if err != nil {
		return nil, err
	}
	p.afterOwner = metadata
	p.afterOwnerRuntime = true
	if err := checkMaterializedRecipeSize(p); err != nil {
		return nil, err
	}
	p.binding = installedRuntimeDigest(json.RawMessage(raw))
	return p, nil
}
func (p *PreparedInstalledRuntime) ReserveAfterOwnerMetadata(template []byte) (*PreparedInstalledRuntime, error) {
	copy, err := p.reserveAfterOwnerMetadata(template)
	if err != nil {
		return nil, err
	}
	copy.afterOwnerRuntime = true
	return copy, nil
}
func (p *PreparedInstalledRuntime) reserveAfterOwnerMetadata(template []byte) (*PreparedInstalledRuntime, error) {
	if p == nil || p.closed.Load() || p.afterOwner != nil {
		return nil, domain.ErrPlanStale
	}
	var capacity struct {
		Version         int    `json:"version"`
		Phase           string `json:"phase"`
		MetadataBytes   int    `json:"metadataBytes"`
		DescriptorBytes int    `json:"descriptorBytes"`
	}
	d := json.NewDecoder(bytes.NewReader(template))
	d.DisallowUnknownFields()
	if d.Decode(&capacity) != nil {
		return nil, domain.ErrPlanStale
	}
	canonical, _ := json.Marshal(capacity)
	if !bytes.Equal(canonical, template) || capacity.Version != 1 || capacity.Phase != "template" || capacity.MetadataBytes < len(template) || capacity.MetadataBytes > 32<<20 || capacity.DescriptorBytes < 1 || capacity.DescriptorBytes > 32<<20 {
		return nil, domain.ErrPlanStale
	}
	copy := p.afterOwnerSourceCopy()
	copy.afterOwner = make([]byte, capacity.MetadataBytes)
	if err := checkMaterializedRecipeSize(copy); err != nil {
		return nil, err
	}
	copy.afterOwner = bytes.Clone(template)
	copy.binding = installedRuntimeDigest(json.RawMessage(copy.Recipe()))
	return copy, nil
}
func (p *PreparedInstalledRuntime) afterOwnerSourceCopy() *PreparedInstalledRuntime {
	copy := &PreparedInstalledRuntime{owner: p.owner, source: p.source, runtime: p.runtime, resources: p.resources, keyLauncher: p.keyLauncher, hookPath: p.hookPath, hookBefore: p.hookBefore, leaves: p.leaves, profile: p.profile, handler: p.handler, native: p.native, configs: p.configs, afterOwnerConfigs: p.afterOwnerConfigs, afterOwnerConfigSource: p.afterOwnerConfigSource, afterOwnerIntegrations: p.afterOwnerIntegrations, ownerInputs: p.ownerInputs, executionFiles: p.executionFiles, files: map[string][]byte{}}
	copy.routes = recordRouteDevices(p.routes).restore()
	for path, payload := range p.files {
		copy.files[path] = payload
	}
	return copy
}

// ObservePreparedAfterOwnerRuntime runs the existing read-only component on the
// captured executable tree. It selects no new handler or source after effects.
func (runtime Runtime) ObservePreparedAfterOwnerRuntime(ctx context.Context, p *PreparedInstalledRuntime) (ports.RuntimeObservation, string, bool, error) {
	if err := runtime.validateInstalledRecipe(p); err != nil {
		return ports.RuntimeObservation{}, "", false, err
	}
	var execution Runtime
	var err error
	if p.profile != "" {
		execution, err = runtime.preparedProfileExecution(ctx, p)
	} else {
		execution, err = runtime.preparedSourceExecution(ctx, p)
	}
	if err != nil {
		return ports.RuntimeObservation{}, "", false, err
	}
	if p.profile == "" {
		observed, err := execution.ObserveInstalledRuntime(ctx, p.owner)
		return observed, "", false, err
	}
	observed, err := execution.ObserveProfileRuntime(ctx, p.owner)
	if err != nil {
		return observed, "", false, err
	}
	if observed.State != ports.RuntimeStateCurrent && observed.State != ports.RuntimeStateStale {
		return observed, "", false, nil
	}
	generation, capable, err := execution.CaptureProfileCustody(ctx, p.owner)
	return observed, generation, capable, err
}

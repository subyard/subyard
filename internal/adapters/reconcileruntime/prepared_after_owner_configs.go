package reconcileruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
)

const afterOwnerConfigInstance = "after-owner"

type afterOwnerConfigPhysical struct {
	Version  int    `json:"version"`
	Instance []byte `json:"instance"`
	Binding  string `json:"binding"`
}

// PrepareAfterOwnerMaterializedConfigs captures only the owning file selector
// and source bytes. The future instance is deliberately not a physical guard.
func (runtime Runtime) PrepareAfterOwnerMaterializedConfigs(ctx context.Context, template []byte) (*PreparedInstalledRuntime, error) {
	configs, err := runtime.prepareConfigPayloads(afterOwnerConfigInstance)
	if err != nil {
		return nil, err
	}
	p, err := runtime.PrepareMaterializedConfigs(ctx, configs)
	if err != nil {
		return nil, err
	}
	return p.reserveAfterOwnerMetadata(template)
}

// BindAfterOwnerMaterializedConfigSource validates original source and scope
// without requiring or inventing a physical instance.
func (runtime Runtime) BindAfterOwnerMaterializedConfigSource(ctx context.Context, raw []byte) (*PreparedInstalledRuntime, error) {
	runtime.afterOwnerConfigSource = true
	return runtime.BindInstalledRuntimeContext(ctx, raw)
}

// ResolveAfterOwnerMaterializedConfigs measures the instance only after the
// caller proves the original owner completed. The original selector/payloads
// remain the source authority checked by the ordinary owning restore.
func (runtime Runtime) ResolveAfterOwnerMaterializedConfigs(ctx context.Context, original []byte) (*PreparedInstalledRuntime, []byte, error) {
	current, err := runtime.afterOwnerPhysicalConfigs(ctx)
	if err != nil {
		return nil, nil, err
	}
	physical := afterOwnerConfigPhysical{1, []byte(current.instance), current.binding}
	raw, _ := json.Marshal(physical)
	p, err := runtime.BindAfterOwnerMaterializedConfigs(ctx, original, raw)
	return p, raw, err
}

// BindAfterOwnerMaterializedConfigs restores the same two measured fields.
// Source/selector guards still run; partial file writes cannot replace them.
func (runtime Runtime) BindAfterOwnerMaterializedConfigs(ctx context.Context, raw, physical []byte) (*PreparedInstalledRuntime, error) {
	var value afterOwnerConfigPhysical
	decoder := json.NewDecoder(bytes.NewReader(physical))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil {
		return nil, domain.ErrPlanStale
	}
	canonical, _ := json.Marshal(value)
	if !bytes.Equal(canonical, physical) || value.Version != 1 || !domain.SafeID(string(value.Instance)) || len(value.Instance) > 128 || !profileRuntimeDigest.MatchString(value.Binding) {
		return nil, domain.ErrPlanStale
	}
	runtime.afterOwnerConfigs = &value
	return runtime.BindInstalledRuntimeContext(ctx, raw)
}

// WithAfterOwnerMetadata replaces the already reserved metadata header. It neither
// reads source files nor changes any other recipe header or payload byte.
func (p *PreparedInstalledRuntime) WithAfterOwnerMetadata(metadata []byte) (*PreparedInstalledRuntime, error) {
	if p == nil || p.closed.Load() || p.afterOwner == nil || len(metadata) == 0 || len(metadata) > 32<<20 {
		return nil, domain.ErrPlanStale
	}
	copy := p.afterOwnerSourceCopy()
	copy.afterOwnerRuntime = p.afterOwnerRuntime
	copy.afterOwner = bytes.Clone(metadata)
	if err := checkMaterializedRecipeSize(copy); err != nil {
		return nil, err
	}
	copy.binding = installedRuntimeDigest(json.RawMessage(copy.Recipe()))
	return copy, nil
}

func (p *PreparedInstalledRuntime) overlayAfterOwnerConfigs(original *materializedRecipe) error {
	if p.afterOwnerConfigs == nil {
		return nil
	}
	if (p.owner != "materialized-files" && p.owner != "materialized-integrations") || p.owner == "materialized-files" && original.Integrations != nil || original.Configs.Instance != afterOwnerConfigInstance || p.afterOwner == nil {
		return domain.ErrPlanStale
	}
	if p.owner == "materialized-files" && p.source != original.Configs.sourceBinding() {
		return domain.ErrPlanStale
	}
	original.Configs.Instance = string(p.afterOwnerConfigs.Instance)
	original.Configs.Binding = p.afterOwnerConfigs.Binding
	return nil
}

// The two materialized source reads per file plus its rendered payload produce
// at most 6144 blobs for the existing 2048-file selector; metadata uses <=2
// chunks. Reserving this leaf cannot reach the existing 8192-member ceiling.
func afterOwnerConfigCapacity(instance string) int {
	raw, _ := json.Marshal(afterOwnerConfigPhysical{1, []byte(strings.Repeat("x", max(128, len(instance)))), strings.Repeat("f", 64)})
	return len(raw)
}

func (p *PreparedInstalledRuntime) PreparedConfigs() *PreparedConfigs {
	if p == nil {
		return nil
	}
	return p.configs
}
func AfterOwnerMaterializedPhysicalBound() int { return afterOwnerConfigCapacity("") }

func (runtime Runtime) afterOwnerPhysicalConfigs(ctx context.Context) (*PreparedConfigs, error) {
	state, err := runtime.reconcileState(ctx)
	if err != nil {
		return nil, err
	}
	if !state.InstanceFound || !domain.SafeID(state.Instance.Config["volatile.uuid"]) {
		return nil, domain.ErrPlanStale
	}
	return runtime.prepareConfigPayloads(state.Instance.Config["volatile.uuid"])
}

package reconcileruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/configmaterial"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

const afterOwnerConfigInstance = "after-owner"

type afterOwnerConfigPhysical struct {
	Version  int                            `json:"version"`
	Instance []byte                         `json:"instance"`
	Binding  string                         `json:"binding"`
	Files    []afterOwnerConfigFilePhysical `json:"files,omitempty"`
	Stopped  bool                           `json:"stopped,omitempty"`
}

type afterOwnerConfigState struct {
	Exists bool   `json:"exists"`
	Digest string `json:"digest"`
	UID    uint32 `json:"uid"`
	GID    uint32 `json:"gid"`
	Mode   uint32 `json:"mode"`
}
type afterOwnerConfigFilePhysical struct {
	Destination     []byte
	Payload         string
	Assessment      config.MaterializedArtifactAssessment
	State           afterOwnerConfigState
	Materialization *configmaterial.ProtectedState `json:"materialization,omitempty"`
	ApprovedDesired bool                           `json:"-"`
}

func (runtime Runtime) PrepareAfterOwnerMaterializedConfigSources(ctx context.Context) (*PreparedInstalledRuntime, error) {
	configs, err := runtime.prepareConfigPayloads(afterOwnerConfigInstance)
	if err != nil {
		return nil, err
	}
	p, err := runtime.PrepareMaterializedConfigs(ctx, configs)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (p *PreparedInstalledRuntime) ReserveAfterOwnerMaterializedConfigMetadata(template []byte) (*PreparedInstalledRuntime, error) {
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
	state, err := runtime.reconcileState(ctx)
	if err != nil || !state.InstanceFound || state.Instance.Config["volatile.uuid"] != current.instance {
		return nil, nil, domain.ErrPlanStale
	}
	physical := afterOwnerConfigPhysical{Version: 1, Instance: []byte(current.instance), Binding: current.binding}
	if strings.EqualFold(state.Instance.Status, "stopped") {
		physical.Stopped = true
	} else {
		if !strings.EqualFold(state.Instance.Status, "running") {
			return nil, nil, domain.ErrPlanStale
		}
		physical.Files, err = runtime.observeAfterOwnerConfigFiles(ctx, current)
		if err != nil {
			return nil, nil, err
		}
	}
	raw, _ := json.Marshal(physical)
	p, err := runtime.BindAfterOwnerMaterializedConfigs(ctx, original, raw)
	return p, raw, err
}

// BindAfterOwnerMaterializedConfigs restores the original physical observations.
// Source/selector guards still run; partial file writes cannot replace them.
func (runtime Runtime) BindAfterOwnerMaterializedConfigs(ctx context.Context, raw, physical []byte) (*PreparedInstalledRuntime, error) {
	if len(physical) > 32<<20 {
		return nil, domain.ErrPlanStale
	}
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
	if value.Stopped && len(value.Files) != 0 {
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

func (p *PreparedInstalledRuntime) PreparedConfigs() *PreparedConfigs {
	if p == nil {
		return nil
	}
	return p.configs
}
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

func configPhysicalName(file guestConfigFile) string {
	if file.label == file.integration+" config" {
		return file.integration + ".config"
	}
	if file.label == file.integration+" rules" {
		return file.integration + ".rules"
	}
	return file.integration + ".instructions"
}

func (runtime Runtime) afterOwnerConfigRequest(ctx context.Context, mode string, entries []integrationArtifact, before *afterOwnerConfigState) ([]afterOwnerConfigState, error) {
	uid := runtime.Yard.DevUID
	if uid <= 0 {
		uid = 1000
	}
	payload, err := json.Marshal(struct {
		Home    string                 `json:"home"`
		UID     int                    `json:"uid"`
		Entries []integrationArtifact  `json:"entries"`
		Before  *afterOwnerConfigState `json:"before,omitempty"`
	}{"/home/" + runtime.devUser(), uid, entries, before})
	if err != nil {
		return nil, err
	}
	program := strings.NewReplacer("@STATE_ROOT@", "/var/lib/subyard/integrations", "@STATE_UID@", "0").Replace(integrationInventoryProgram)
	if current, _ := ctx.Value(currentMaterializedConfigCASKey{}).(bool); current && mode == "configs-write" {
		ctx = ports.WithNativeSynchronousConfigCAS(ctx)
	}
	result, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, ports.InstanceExecRequest{Command: []string{"python3", "-B", "-c", program, mode}, Stdin: payload})
	var failure *ports.NativeOperationFailure
	if errors.As(err, &failure) {
		// Preserve the original chain: only the invocation observer can prove
		// its exact failure terminal and exclude additional opaque branches.
		return nil, err
	}
	if err != nil || result.ExitCode != 0 {
		return nil, domain.ErrPlanStale
	}
	if mode == "configs-write" {
		return nil, nil
	}
	var states []afterOwnerConfigState
	decoder := json.NewDecoder(bytes.NewReader(result.Stdout))
	decoder.DisallowUnknownFields()
	if len(result.Stdout) > len(entries)*256+3 || decoder.Decode(&states) != nil || len(states) != len(entries) {
		return nil, domain.ErrPlanStale
	}
	for _, state := range states {
		if state.Exists && (!profileRuntimeDigest.MatchString(state.Digest) || state.Mode > 07777) || !state.Exists && state != (afterOwnerConfigState{}) {
			return nil, domain.ErrPlanStale
		}
	}
	return states, nil
}

func (runtime Runtime) observeAfterOwnerConfigFiles(ctx context.Context, p *PreparedConfigs) ([]afterOwnerConfigFilePhysical, error) {
	states, err := runtime.afterOwnerConfigRequest(ctx, "configs-observe", p.entries, nil)
	if err != nil {
		return nil, err
	}
	rows := make([]afterOwnerConfigFilePhysical, 0, len(p.entries))
	for _, file := range p.files {
		payload, exists := p.payloads[file.destination]
		if !exists {
			continue
		}
		state := states[len(rows)]
		desired := config.DesiredMaterializedArtifact{Name: configPhysicalName(file), Destination: file.destination, OwnedFormat: file.ownedFormat, DesiredHash: fmt.Sprintf("%x", sha256.Sum256(payload))}
		observation := config.MaterializationObservation{ExitCode: 1}
		if state.Exists {
			observation.ExitCode = 0
			observation.Output = []byte(state.Digest)
		}
		var materialization *configmaterial.ProtectedState
		approvedDesired := false
		if file.ownedFormat != "" {
			desired.DesiredHash, err = configmaterial.DesiredDigestFor(file.ownedFormat, payload)
			if err != nil {
				return nil, err
			}
			request, e := configmaterial.Request(file.ownedFormat, configmaterial.ModeObserve, runtime.devUser(), file.destination, runtime.Yard.DevUID, payload)
			if e != nil {
				return nil, e
			}
			var expected *configmaterial.ApplyExpectedState
			if p.afterOwnerFiles != nil {
				old := p.afterOwnerFiles[len(rows)]
				expected = &configmaterial.ApplyExpectedState{Fingerprint: old.Assessment.Snapshot.GuestHash, State: old.Materialization, Exists: old.State.Exists, UID: old.State.UID, GID: old.State.GID, Mode: old.State.Mode}
			}
			request, e = configmaterial.WithAfterOwnerObservation(request, expected)
			if e != nil {
				return nil, e
			}
			result, e := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, request)
			if e != nil || result.ExitCode != 0 {
				return nil, domain.ErrPlanStale
			}
			observed, e := configmaterial.ParseObservation(result.Stdout)
			if e != nil {
				return nil, e
			}
			observation.Fingerprint, observation.OwnedConverged = observed.Fingerprint, observed.Converged
			if !observed.State.Valid() {
				return nil, domain.ErrPlanStale
			}
			if observed.Destination == nil {
				return nil, domain.ErrPlanStale
			}
			destination := observed.Destination
			state = afterOwnerConfigState{destination.Exists, destination.Digest, destination.UID, destination.GID, destination.Mode}
			materialization, approvedDesired = observed.State, observed.ApprovedDesired
		}
		assessed, e := config.AssessMaterializedArtifact(desired, observation)
		if e != nil {
			return nil, e
		}
		rows = append(rows, afterOwnerConfigFilePhysical{Destination: []byte(file.destination), Payload: materializedBlobPath(payload), Assessment: assessed, State: state, Materialization: materialization, ApprovedDesired: approvedDesired})
	}
	return rows, nil
}

// Only this AfterOwner handle accepts movement from each retained observation
// to its original approved desired state. The generic config guard is unchanged.
func (runtime Runtime) CheckAfterOwnerMaterializedConfigs(ctx context.Context, p *PreparedInstalledRuntime) error {
	if p == nil || p.afterOwnerConfigs == nil || p.owner != "materialized-files" {
		return domain.ErrPlanStale
	}
	prepared, err := runtime.restoreMaterializedConfigs(ctx, p)
	if err != nil {
		return err
	}
	if p.afterOwnerConfigs.Stopped {
		state, err := runtime.reconcileState(ctx)
		if err != nil || !state.InstanceFound || state.Instance.Config["volatile.uuid"] != string(p.afterOwnerConfigs.Instance) || !strings.EqualFold(state.Instance.Status, "stopped") {
			return domain.ErrPlanStale
		}
		return nil
	}
	_, err = runtime.checkAfterOwnerConfigFiles(ctx, prepared)
	return err
}

func validateAfterOwnerConfigFiles(p *PreparedConfigs) error {
	if len(p.afterOwnerFiles) != len(p.entries) {
		return domain.ErrPlanStale
	}
	index := 0
	for _, file := range p.files {
		payload, ok := p.payloads[file.destination]
		if !ok {
			continue
		}
		row := p.afterOwnerFiles[index]
		index++
		if !bytes.Equal(row.Destination, []byte(file.destination)) || row.Payload != materializedBlobPath(payload) || row.Assessment.Snapshot.Name != configPhysicalName(file) {
			return domain.ErrPlanStale
		}
		state := row.State
		if state.Exists && (!profileRuntimeDigest.MatchString(state.Digest) || state.Mode > 07777) || !state.Exists && state != (afterOwnerConfigState{}) {
			return domain.ErrPlanStale
		}
		if file.ownedFormat == "" {
			observation := config.MaterializationObservation{ExitCode: 1}
			if state.Exists {
				observation.ExitCode = 0
				observation.Output = []byte(state.Digest)
			}
			expected, _ := config.AssessMaterializedArtifact(config.DesiredMaterializedArtifact{Name: configPhysicalName(file), DesiredHash: fmt.Sprintf("%x", sha256.Sum256(payload))}, observation)
			if expected != row.Assessment {
				return domain.ErrPlanStale
			}
		} else if !row.Materialization.Valid() || row.Assessment.Snapshot.GuestExit != 0 || row.Assessment.Snapshot.GuestOutput != "owned-"+file.ownedFormat || !profileRuntimeDigest.MatchString(row.Assessment.Snapshot.GuestHash) || row.Assessment.Converged && !state.Exists {
			return domain.ErrPlanStale
		}
	}
	return nil
}
func (runtime Runtime) checkAfterOwnerConfigFiles(ctx context.Context, p *PreparedConfigs) (map[string]bool, error) {
	current, err := runtime.observeAfterOwnerConfigFiles(ctx, p)
	if err != nil {
		return nil, err
	}
	return checkAfterOwnerConfigRows(p, current, runtime.Yard.DevUID)
}

func checkAfterOwnerConfigRows(p *PreparedConfigs, current []afterOwnerConfigFilePhysical, uid int) (map[string]bool, error) {
	if len(current) != len(p.afterOwnerFiles) {
		return nil, domain.ErrPlanStale
	}
	desired := map[string]bool{}
	for i, fresh := range current {
		old := p.afterOwnerFiles[i]
		if !bytes.Equal(old.Destination, fresh.Destination) || old.Payload != fresh.Payload || old.Assessment.Snapshot.Name != fresh.Assessment.Snapshot.Name {
			return nil, domain.ErrPlanStale
		}
		if !fresh.Assessment.Converged && !fresh.ApprovedDesired && (old.Assessment.Converged || old.Assessment.Snapshot != fresh.Assessment.Snapshot) {
			return nil, domain.ErrPlanStale
		}
		metadata := fresh.State
		metadata.Digest = ""
		original := old.State
		original.Digest = ""
		if uid <= 0 {
			uid = 1000
		}
		wanted := afterOwnerConfigState{Exists: true, UID: uint32(uid), GID: uint32(uid), Mode: 0644}
		if p.entries[i].Kind == "file" {
			wantedState := wanted
			wantedState.Digest = p.entries[i].Digest
			if fresh.State != old.State && fresh.State != wantedState {
				return nil, domain.ErrPlanStale
			}
		}
		if metadata != original && metadata != wanted {
			return nil, domain.ErrPlanStale
		}
		desired[string(fresh.Destination)] = fresh.Assessment.Converged && metadata == wanted
	}
	return desired, nil
}

func (runtime Runtime) CheckAfterOwnerConfigAssessments(ctx context.Context, p *PreparedInstalledRuntime, fresh []config.MaterializedArtifactAssessment) error {
	if p == nil || p.afterOwnerConfigs == nil {
		return domain.ErrPlanStale
	}
	if p.afterOwnerConfigs.Stopped {
		if len(fresh) != 0 {
			return domain.ErrPlanStale
		}
		return runtime.CheckAfterOwnerMaterializedConfigs(ctx, p)
	}
	return runtime.checkPhysicalConfigAssessments(ctx, p, fresh)
}

func (runtime Runtime) CheckCurrentConfigAssessments(ctx context.Context, p *PreparedInstalledRuntime, fresh []config.MaterializedArtifactAssessment) error {
	if !p.HasCurrentMaterializedCustody() {
		return domain.ErrPlanStale
	}
	return runtime.checkPhysicalConfigAssessments(ctx, p, fresh)
}

func (runtime Runtime) checkPhysicalConfigAssessments(ctx context.Context, p *PreparedInstalledRuntime, fresh []config.MaterializedArtifactAssessment) error {
	prepared, err := runtime.restoreMaterializedConfigs(ctx, p)
	if err != nil {
		return err
	}
	current, err := runtime.observeAfterOwnerConfigFiles(ctx, prepared)
	if err != nil {
		return err
	}
	if _, err := checkAfterOwnerConfigRows(prepared, current, runtime.Yard.DevUID); err != nil {
		return err
	}
	// Runtime owns instruction artifacts regardless of the legacy composer;
	// CLI config assessments exclude them. Every physical row was checked above.
	definitions, err := runtime.integrationDefinitions()
	if err != nil {
		return err
	}
	sources, err := config.SelectMaterializationSources(runtimeEnvironment(runtime.Environment), runtime.devUser(), config.MaterializationRuntime, definitions...)
	if err != nil {
		return err
	}
	for _, source := range sources {
		if _, included := prepared.payloads[source.Asset.Destination]; source.PreviewInstructions && included {
			current = slices.DeleteFunc(current, func(row afterOwnerConfigFilePhysical) bool {
				return string(row.Destination) == source.Asset.Destination
			})
		}
	}
	if len(fresh) != len(current) {
		return domain.ErrPlanStale
	}
	seen := map[string]bool{}
	for _, fact := range fresh {
		index := slices.IndexFunc(current, func(row afterOwnerConfigFilePhysical) bool { return row.Assessment.Snapshot.Name == fact.Snapshot.Name })
		if index < 0 || seen[fact.Snapshot.Name] || current[index].Assessment != fact {
			return domain.ErrPlanStale
		}
		seen[fact.Snapshot.Name] = true
	}
	return nil
}

func (runtime Runtime) writeAfterOwnerConfigFile(ctx context.Context, file guestConfigFile, payload []byte, original afterOwnerConfigFilePhysical) error {
	if file.ownedFormat != "" {
		request, err := configmaterial.Request(file.ownedFormat, configmaterial.ModeApply, runtime.devUser(), file.destination, runtime.Yard.DevUID, payload)
		if err != nil {
			return err
		}
		state := original.State
		request, err = configmaterial.WithExpectedApply(request, configmaterial.ApplyExpectedState{Fingerprint: original.Assessment.Snapshot.GuestHash, State: original.Materialization, Exists: state.Exists, UID: state.UID, GID: state.GID, Mode: state.Mode})
		if err != nil {
			return err
		}
		result, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, request)
		if err != nil || result.ExitCode != 0 {
			return domain.ErrPlanStale
		}
		return nil
	}
	entry := integrationArtifact{ID: file.integration, Kind: "file", Path: file.destination, Digest: fmt.Sprintf("%x", sha256.Sum256(payload)), Content: base64.StdEncoding.EncodeToString(payload)}
	_, err := runtime.afterOwnerConfigRequest(ctx, "configs-write", []integrationArtifact{entry}, &original.State)
	return err
}

func (p *PreparedInstalledRuntime) AfterOwnerMaterializedPhysicalBound() (int, error) {
	if p == nil || p.configs == nil {
		return 0, domain.ErrPlanStale
	}
	value := maxAfterOwnerConfigPhysical(p.configs)
	raw, err := json.Marshal(value)
	return len(raw), err
}

func maxAfterOwnerConfigPhysical(configs *PreparedConfigs) afterOwnerConfigPhysical {
	f := strings.Repeat("f", 64)
	value := afterOwnerConfigPhysical{Version: 1, Instance: []byte(strings.Repeat("x", 128)), Binding: f, Stopped: true}
	for _, file := range configs.files {
		if payload, ok := configs.payloads[file.destination]; ok {
			row := afterOwnerConfigFilePhysical{Destination: []byte(file.destination), Payload: materializedBlobPath(payload), Assessment: config.MaterializedArtifactAssessment{Snapshot: config.MaterializedArtifactSnapshot{Name: configPhysicalName(file), GuestExit: 255, GuestHash: f, GuestOutput: "unavailable"}}, State: afterOwnerConfigState{true, f, ^uint32(0), ^uint32(0), 07777}}
			if file.ownedFormat != "" {
				row.Materialization = &configmaterial.ProtectedState{Baseline: f, Receipt: f}
			}
			value.Files = append(value.Files, row)
		}
	}
	return value
}

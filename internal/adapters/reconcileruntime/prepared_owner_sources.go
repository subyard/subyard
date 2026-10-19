package reconcileruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/profile"
)

const ownerSourceBindingsPath = "prepared/owner-source-bindings.json"

type ownerSourceBindingRecord struct {
	Environment string
	Inputs      []ownerSourceInputBinding
}
type ownerSourceInputBinding struct{ Definitions, Inputs string }
type preparedOwnerSourceExecution struct {
	files       map[string]string
	payloads    map[string][]byte
	root        string
	environment string
	bindings    []ownerSourceInputBinding
}

func (runtime Runtime) ownerInputBindings() ([]ownerSourceInputBinding, error) {
	definitions, err := runtime.profileRuntimeDefinitions()
	if err != nil {
		return nil, err
	}
	scopes := [][]profile.Definition{nil, definitions}
	for _, definition := range definitions {
		scopes = append(scopes, []profile.Definition{definition})
	}
	result := []ownerSourceInputBinding{}
	for _, scope := range scopes {
		key := installedRuntimeDigest(scope)
		if slices.ContainsFunc(result, func(b ownerSourceInputBinding) bool { return b.Definitions == key }) {
			continue
		}
		value, err := runtime.profileRuntimeInputs(scope)
		if err != nil {
			return nil, err
		}
		result = append(result, ownerSourceInputBinding{key, value})
	}
	slices.SortFunc(result, func(a, b ownerSourceInputBinding) int {
		return bytes.Compare([]byte(a.Definitions), []byte(b.Definitions))
	})
	return result, nil
}

// PrepareOwnerRuntimeSources captures the existing init/lifecycle public source
// scope and selected external materialized sources. It starts no guest work.
func (runtime Runtime) PrepareOwnerRuntimeSources(ctx context.Context) (*PreparedInstalledRuntime, error) {
	configs, err := runtime.prepareConfigPayloads("owner-conditional")
	if err != nil {
		return nil, err
	}
	p, err := runtime.PrepareMaterializedConfigs(ctx, configs)
	if err != nil {
		return nil, err
	}
	p.owner = "owner-native-runtime"
	d := &materializedIntegrations{Hooks: describeHookPlan(runtime.HookPlan)}
	if err := runtime.captureMaterializedIntegrationSources(p, d); err != nil {
		return nil, err
	}
	description, err := runtime.materializedConfigDescription(configs)
	if err != nil {
		return nil, err
	}
	if err := writeMaterializedMetadata(p, materializedRecipe{Version: 1, Configs: description, Integrations: d}); err != nil {
		return nil, err
	}
	p.ownerInputs, err = runtime.ownerInputBindings()
	if err != nil {
		return nil, err
	}
	p.files[ownerSourceBindingsPath], err = json.Marshal(ownerSourceBindingRecord{runtime.OwnerEnvironmentBinding(), p.ownerInputs})
	if err != nil {
		return nil, err
	}
	p.source = installedRuntimeDigest(struct {
		Config, Environment string
		Inputs              []ownerSourceInputBinding
	}{d.sourceBinding(description), runtime.OwnerEnvironmentBinding(), p.ownerInputs})
	p.integrations = &PreparedIntegrations{configs: configs, hooks: runtime.HookPlan}
	p.executionFiles = map[string][]byte{}
	for _, source := range d.Sources {
		p.executionFiles[source.Layout] = p.files[source.Payload]
	}
	for _, source := range d.Environment {
		p.executionFiles[source.Payload] = p.files[source.Payload]
	}
	if err := checkMaterializedRecipeSize(p); err != nil {
		return nil, err
	}
	if _, _, err := p.stageUsage(); err != nil {
		return nil, err
	}
	p.binding = installedRuntimeDigest(json.RawMessage(p.Recipe()))
	return p, nil
}

func (runtime Runtime) bindOwnerRuntimeSources(ctx context.Context, r installedRecipe, raw []byte) (*PreparedInstalledRuntime, error) {
	current, err := runtime.PrepareOwnerRuntimeSources(ctx)
	if err != nil {
		return nil, err
	}
	// Reconstructing a current descriptor is solely a guard. Its byte payloads
	// never replace the original approved table supplied by the Rust bundle.
	original := &PreparedInstalledRuntime{owner: r.Owner, source: r.Source, runtime: r.Runtime, resources: r.Resources, files: map[string][]byte{}}
	previous := ""
	for _, file := range r.Files {
		if file.Path <= previous {
			return nil, domain.ErrPlanStale
		}
		previous = file.Path
		original.files[file.Path] = file.Content
	}
	old, _, err := decodeMaterializedMetadata(original)
	if err != nil {
		return nil, err
	}
	fresh, _, err := decodeMaterializedMetadata(current)
	if err != nil {
		return nil, err
	}
	if old.Integrations == nil || fresh.Integrations == nil {
		return nil, domain.ErrPlanStale
	}
	if err := runtime.checkMaterializedIntegrationDescription(old.Integrations, fresh.Integrations, original.files); err != nil {
		return nil, err
	}
	if len(old.Configs.Sources) != len(fresh.Configs.Sources) {
		return nil, domain.ErrPlanStale
	}
	for i, guard := range old.Configs.Sources {
		value := &fresh.Configs.Sources[i]
		if !bytes.Equal(guard.Path, value.Path) || guard.Follow != value.Follow || guard.Exists != value.Exists || guard.Payload != value.Payload || guard.Projected != value.Projected {
			return nil, domain.ErrPlanStale
		}
		if err := runtime.checkProjectedMaterializedGuard(guard.Path, guard.Identity, value.Identity, guard.Projected, original.files[guard.Payload]); err != nil {
			return nil, err
		}
		value.Identity = guard.Identity
	}
	// Publish the original source guards into the otherwise exactly equal fixed
	// component shape. Unprojected source identity never gains a byte-only guard.
	if err := writeMaterializedMetadata(current, fresh); err != nil {
		return nil, err
	}
	expected := current.Recipe()
	if !bytes.Equal(expected, raw) {
		return nil, domain.ErrPlanStale
	}
	current.files = original.files
	current.binding = installedRuntimeDigest(json.RawMessage(raw))
	return current, nil
}

func (runtime Runtime) OwnerSourceRuntime(ctx context.Context, p *PreparedInstalledRuntime) (Runtime, error) {
	if p == nil || p.owner != "owner-native-runtime" || p.closed.Load() || p.runtime != runtime.configRuntimeBinding() || p.resources != runtime.installedResourceBinding() {
		return Runtime{}, domain.ErrPlanStale
	}
	original, _, err := decodeMaterializedMetadata(p)
	if err != nil {
		return Runtime{}, err
	}
	guard, err := runtime.PrepareOwnerRuntimeSources(ctx)
	if err != nil {
		return Runtime{}, err
	}
	if guard.source != p.source {
		return Runtime{}, domain.ErrPlanStale
	}
	// The source guard above ends before materialization. All execution reads
	// below use the retained original files, even if live paths then change.
	execution, err := runtime.materializedIntegrationExecution(ctx, p)
	if err != nil {
		return Runtime{}, err
	}
	proof := &preparedOwnerSourceExecution{root: p.directory, environment: execution.OwnerEnvironmentBinding(), bindings: p.ownerInputs, files: map[string]string{}, payloads: p.executionFiles}
	for relative := range p.executionFiles {
		identity, err := installedRuntimeFile(filepath.Join(p.directory, relative))
		if err != nil {
			return Runtime{}, err
		}
		proof.files[relative] = installedRuntimeDigest(identity)
	}
	// Every selected original source was physically read back before the first
	// child dispatch; a hash supplied by a caller cannot construct this proof.
	if err := proof.check(); err != nil {
		return Runtime{}, err
	}
	execution.ownerSources = proof
	execution.HookPlan = original.Integrations.Hooks.restore()
	return execution, nil
}

func (p *PreparedInstalledRuntime) FinishOwnerSources(err error) error {
	if p == nil || p.owner != "owner-native-runtime" {
		return domain.ErrPlanStale
	}
	if err != nil && p.stageBinding != "" {
		p.retain.Store(true)
	}
	return p.Close()
}
func (proof *preparedOwnerSourceExecution) check() error {
	info, err := os.Lstat(proof.root)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0500 {
		return domain.ErrPlanStale
	}
	count := 0
	err = filepath.WalkDir(proof.root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(proof.root, path)
		if err != nil {
			return err
		}
		if relative == ".stage.json" {
			return nil
		}
		expected, ok := proof.files[relative]
		if !ok {
			return domain.ErrPlanStale
		}
		actual, err := installedRuntimeFile(path)
		if err != nil || installedRuntimeDigest(actual) != expected {
			return domain.ErrPlanStale
		}
		body, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(body, proof.payloads[relative]) {
			return domain.ErrPlanStale
		}
		count++
		return nil
	})
	if err != nil {
		return err
	}
	if count != len(proof.files) {
		return domain.ErrPlanStale
	}
	return nil
}
func (proof *preparedOwnerSourceExecution) inputs(runtime Runtime, definitions []profile.Definition) (string, error) {
	if runtime.RepositoryRoot != proof.root || runtime.OwnerEnvironmentBinding() != proof.environment {
		return "", domain.ErrPlanStale
	}
	if err := proof.check(); err != nil {
		return "", err
	}
	key := installedRuntimeDigest(definitions)
	for _, binding := range proof.bindings {
		if binding.Definitions == key {
			return binding.Inputs, nil
		}
	}
	return "", errors.New("owner runtime source scope was not prepared")
}

func (runtime Runtime) OwnerEnvironmentBinding() string {
	environment := runtime.bindingEnvironment(runtime.Environment)
	values := make([][]byte, len(environment))
	for i, value := range environment {
		values[i] = []byte(value)
	}
	return installedRuntimeDigest(values)
}

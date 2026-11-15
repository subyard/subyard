package reconcileruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"

	"github.com/Subyard/Subyard/internal/profile"
)

const ownerSourceBindingsPath = "prepared/owner-source-bindings.json"

type ownerSourceBindingRecord struct {
	Environment string
	Inputs      []ownerSourceInputBinding
}
type ownerSourceInputBinding struct{ Definitions, Inputs string }

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
	return p, nil
}

func (runtime Runtime) OwnerEnvironmentBinding() string {
	environment := runtime.bindingEnvironment(runtime.Environment)
	values := make([][]byte, len(environment))
	for i, value := range environment {
		values[i] = []byte(value)
	}
	return installedRuntimeDigest(values)
}

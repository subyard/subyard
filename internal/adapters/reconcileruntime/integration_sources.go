package reconcileruntime

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/profile"
)

func (runtime Runtime) integrationDefinitions() ([]profile.IntegrationDefinition, error) {
	if runtime.Integrations != nil {
		return slices.Clone(runtime.Integrations), nil
	}
	return profile.LoadIntegrations(runtime.RepositoryRoot)
}

func (runtime Runtime) integrationPackageBindingWithSourceReader(definition profile.IntegrationDefinition, reader func(config.MaterializedAsset) ([]byte, error)) (string, error) {
	source, err := definition.SourceBinding()
	if err != nil {
		return "", err
	}
	values := map[string]string{}
	for _, setting := range definition.Settings {
		values[setting.Name] = runtime.bindingPath(runtime.environmentValue(setting.Name))
	}
	for _, input := range definition.Inputs {
		values[input.Name] = runtime.bindingPath(input.Value)
	}
	if path := runtime.environmentValue("AGENT_" + definition.Name + "_PROVISION"); path != "" {
		var data []byte
		var err error
		if reader == nil {
			data, err = (config.MaterializedAsset{Source: path}).ReadSource()
		} else {
			data, err = reader(config.MaterializedAsset{Source: path})
		}
		if err != nil {
			return "", err
		}
		values["AGENT_"+definition.Name+"_PROVISION"] = fmt.Sprintf("%x", sha256.Sum256(data))
	}
	payload, _ := json.Marshal(struct {
		Source string
		Values map[string]string
	}{source, values})
	return fmt.Sprintf("%x", sha256.Sum256(payload)), nil
}
func (runtime Runtime) integrationSourceBindingsWithSourceReader(phase string, reader func(config.MaterializedAsset) ([]byte, error)) (string, map[string]string, error) {
	definitions, err := runtime.integrationDefinitions()
	if err != nil {
		return "", nil, err
	}
	if len(definitions) == 0 {
		// Retain the original scope grammar when no package contract is declared.
		return "", nil, nil
	}
	sources := map[string]string{}
	for _, definition := range definitions {
		if definition.Retired {
			continue
		}
		binding, err := runtime.integrationPackageBindingWithSourceReader(definition, reader)
		if err != nil {
			return "", nil, runtime.integrationDiagnostic(err, definition, phase, "native-unavailable")
		}
		sources[definition.Name] = binding
	}
	payload, _ := json.Marshal(sources)
	return fmt.Sprintf("%x", sha256.Sum256(payload)), sources, nil
}

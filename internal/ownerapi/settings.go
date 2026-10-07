package ownerapi

import (
	"slices"
	"sort"

	"github.com/Subyard/Subyard/internal/config"
)

type Setting struct {
	Name           string                    `json:"name"`
	Kind           config.SettingKind        `json:"kind"`
	Type           config.SettingValueType   `json:"type"`
	Default        *string                   `json:"default,omitempty"`
	Value          *string                   `json:"value,omitempty"`
	ValueAvailable bool                      `json:"valueAvailable"`
	Aliases        []string                  `json:"aliases"`
	Scopes         []config.SettingScope     `json:"scopes"`
	Syncable       bool                      `json:"syncable"`
	Merge          string                    `json:"merge"`
	Application    config.SettingApplication `json:"application"`
	Owner          string                    `json:"owner"`
	Enum           []string                  `json:"enum"`
	Minimum        int                       `json:"minimum"`
	Maximum        int                       `json:"maximum"`
	Optional       bool                      `json:"optional"`
	Editable       bool                      `json:"editable"`
	Provenance     []SettingProvenance       `json:"provenance"`
}

type SettingsList struct {
	SchemaVersion int       `json:"schemaVersion"`
	YardName      string    `json:"yardName"`
	Settings      []Setting `json:"settings"`
}

func publicSettingValue(definition config.SettingDefinition, value string) bool {
	if definition.Sensitive || definition.Kind != config.SettingScalar || len(value) > 8192 {
		return false
	}
	switch definition.Type {
	case config.SettingAbsolutePath, config.SettingRegularFilePath, config.SettingExecutable:
		return false
	}
	return config.ValidateNonSecretContent(definition.Name, value) == nil
}

func (service Service) SettingsList() (SettingsList, error) {
	loaded := service.Loaded
	result := SettingsList{SchemaVersion: 1, YardName: loaded.Context.YardName, Settings: []Setting{}}
	definitions := config.ResolvedSettingCatalog(loaded)
	known := map[string]bool{}
	for _, definition := range definitions {
		known[definition.Name] = true
	}
	for _, name := range config.SettingNames(loaded.Settings) {
		if known[name] {
			continue
		}
		if definition, ok := loaded.Catalog.LookupSetting(name); ok {
			definitions = append(definitions, config.ResolvedSettingDefinition(loaded, definition))
		}
	}
	sort.Slice(definitions, func(left, right int) bool { return definitions[left].Name < definitions[right].Name })
	for _, definition := range definitions {
		if definition.Sensitive {
			continue
		}
		trace := loaded.Settings[definition.Name]
		entry := Setting{Name: definition.Name, Kind: definition.Kind, Type: definition.Type,
			Aliases: append([]string{}, definition.Aliases...), Scopes: append([]config.SettingScope{}, definition.Scopes...),
			Syncable: definition.Syncable, Merge: definition.Merge, Application: definition.Application,
			Owner: definition.Owner, Enum: append([]string{}, definition.Enum...), Minimum: definition.Minimum,
			Maximum: definition.Maximum, Optional: definition.Optional}
		value, present := loaded.Environment[definition.Name]
		entry.ValueAvailable = present && publicSettingValue(definition, value)
		if entry.ValueAvailable {
			entry.Value = &value
		}
		if definition.HasDefault && publicSettingValue(definition, definition.Default) {
			value := definition.Default
			entry.Default = &value
		}
		entry.Provenance = settingProvenance(trace, entry.ValueAvailable)
		entry.Editable = definition.Kind == config.SettingScalar && slices.ContainsFunc(definition.Scopes,
			func(scope config.SettingScope) bool {
				return scope == config.ScopeShared || scope == config.ScopeHost || scope == config.ScopeYard
			})
		result.Settings = append(result.Settings, entry)
	}
	return result, nil
}

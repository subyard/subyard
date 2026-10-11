package config

import (
	"fmt"
	"slices"

	"github.com/Subyard/Subyard/internal/profile"
)

// Catalog is an operation-local view of core and immutable shipped settings.
// Its zero value provides only core settings; no operation registers global state.
type Catalog struct {
	profiles     map[string]SettingDefinition
	declarations []profile.Definition
	integrations []profile.IntegrationDefinition
}

func LoadCatalog(root string) (Catalog, error) {
	definitions, err := profile.Load(root)
	if err != nil {
		return Catalog{}, err
	}
	integrations, err := profile.LoadIntegrations(root)
	if err != nil {
		return Catalog{}, err
	}
	result := Catalog{profiles: make(map[string]SettingDefinition), declarations: definitions, integrations: integrations}
	type settingsOwner struct {
		Name     string
		Settings []profile.Setting
	}
	owners := []settingsOwner{}
	for _, owner := range definitions {
		owners = append(owners, settingsOwner{"profile:" + owner.Name, owner.Settings})
	}
	for _, owner := range integrations {
		settings := slices.Clone(owner.Settings)
		for _, input := range owner.Inputs {
			value := input.Value
			settings = append(settings, profile.Setting{Name: input.Name, Type: input.Type, Scopes: []string{"shipped"}, Application: "yard-init", Default: &value})
		}
		owners = append(owners, settingsOwner{"integration:" + owner.Name, settings})
	}
	for _, owner := range owners {
		for _, setting := range owner.Settings {
			if _, exists := result.LookupSetting(setting.Name); exists {
				return Catalog{}, fmt.Errorf("profile %s setting %s collides with an existing setting", owner.Name, setting.Name)
			}
			definition := SettingDefinition{
				Name: setting.Name, Kind: SettingScalar, Type: SettingValueType(setting.Type),
				Syncable: setting.Syncable, Merge: "replace", Owner: owner.Name,
				Optional: setting.Optional, Minimum: setting.Minimum, Maximum: setting.Maximum,
				HostListener: setting.HostListener, DefaultFrom: setting.DefaultFrom, Enum: slices.Clone(setting.Enum),
			}
			for _, scope := range setting.Scopes {
				definition.Scopes = append(definition.Scopes, SettingScope(scope))
			}
			switch setting.Application {
			case "next-command":
				definition.Application = SettingNextCommand
			case "yard-init":
				definition.Application = SettingYardInit
			case "config-apply":
				definition.Application = SettingConfigApply
			default:
				return Catalog{}, fmt.Errorf("profile %s setting %s has invalid application", owner.Name, setting.Name)
			}
			if setting.Default != nil {
				definition.HasDefault, definition.Default = true, *setting.Default
				if !definition.allows(ScopeShipped) {
					return Catalog{}, fmt.Errorf("profile setting %s default requires shipped scope", setting.Name)
				}
				if err := validateSettingValue(definition, definition.Default); err != nil {
					return Catalog{}, fmt.Errorf("profile setting %s default: %w", setting.Name, err)
				}
				if err := ValidateNonSecretContent(setting.Name, definition.Default); err != nil {
					return Catalog{}, err
				}
			}
			result.profiles[setting.Name] = definition
		}
	}
	for _, definition := range result.SettingCatalog() {
		if definition.DefaultFrom == nil {
			continue
		}
		seen := map[string]bool{definition.Name: true}
		current := definition
		for current.DefaultFrom != nil {
			if current.Type != SettingPort || current.Minimum < 1 || current.Maximum > 65535 || current.Maximum < current.Minimum || current.HasDefault {
				return Catalog{}, fmt.Errorf("invalid derived port default for %s", current.Name)
			}
			source, ok := result.LookupSetting(current.DefaultFrom.Setting)
			if !ok || source.Type != SettingPort || seen[source.Name] {
				return Catalog{}, fmt.Errorf("invalid derived port reference for %s", current.Name)
			}
			seen[source.Name] = true
			current = source
		}
	}
	for _, owner := range integrations {
		if owner.Dashboard != nil {
			field, ok := result.LookupSetting(owner.Dashboard.PortSetting)
			if !ok || field.Type != SettingPort || field.Owner != "integration:"+owner.Name {
				return Catalog{}, fmt.Errorf("integration dashboard requires its owned port setting")
			}
		}
	}
	return result, nil
}

func (settings Catalog) Profiles() []profile.Definition {
	return append([]profile.Definition{}, settings.declarations...)
}

func (settings Catalog) Integrations() []profile.IntegrationDefinition {
	return append([]profile.IntegrationDefinition{}, settings.integrations...)
}

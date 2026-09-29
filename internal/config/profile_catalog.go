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
}

func LoadCatalog(root string) (Catalog, error) {
	definitions, err := profile.Load(root)
	if err != nil {
		return Catalog{}, err
	}
	result := Catalog{profiles: make(map[string]SettingDefinition), declarations: definitions}
	for _, owner := range definitions {
		for _, setting := range owner.Settings {
			if _, exists := result.LookupSetting(setting.Name); exists {
				return Catalog{}, fmt.Errorf("profile %s setting %s collides with an existing setting", owner.Name, setting.Name)
			}
			definition := SettingDefinition{
				Name: setting.Name, Kind: SettingScalar, Type: SettingValueType(setting.Type),
				Syncable: setting.Syncable, Merge: "replace", Owner: "profile:" + owner.Name,
				Optional: setting.Optional, Minimum: setting.Minimum, Maximum: setting.Maximum,
				HostListener: setting.HostListener, Enum: slices.Clone(setting.Enum),
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
	return result, nil
}

func (settings Catalog) Profiles() []profile.Definition {
	return append([]profile.Definition{}, settings.declarations...)
}

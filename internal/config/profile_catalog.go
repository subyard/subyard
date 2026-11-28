package config

import (
	"fmt"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/profile"
)

// Catalog is an operation-local view of core and immutable shipped settings.
// Its zero value provides only core settings; no operation registers global state.
type Catalog struct {
	profiles     map[string]SettingDefinition
	declarations []profile.Definition
	integrations []profile.IntegrationDefinition
	retired      []profile.RetiredComponent
	// root is the repository root LoadCatalog read; it also ships the
	// profile packages that carry no declaration.
	root string
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
	profiles := make([]profile.Metadata, len(definitions))
	for i, definition := range definitions {
		profiles[i], err = definition.Metadata()
		if err != nil {
			return Catalog{}, err
		}
	}
	agents := make([]profile.IntegrationMetadata, len(integrations))
	for i, definition := range integrations {
		agents[i], err = definition.Metadata()
		if err != nil {
			return Catalog{}, err
		}
	}
	result, err := BuildCatalog(profiles, agents)
	if err != nil {
		return Catalog{}, err
	}
	retired, err := profile.LoadRetired(root)
	if err != nil {
		return Catalog{}, err
	}
	for _, component := range retired {
		if component.Kind == "profile" && slices.ContainsFunc(definitions, func(item profile.Definition) bool { return item.Name == component.ID }) {
			return Catalog{}, fmt.Errorf("retired profile collides with active package %s", component.ID)
		}
		if component.Kind == "integration" {
			if slices.ContainsFunc(integrations, func(item profile.IntegrationDefinition) bool { return item.Name == component.ID }) {
				return Catalog{}, fmt.Errorf("retired integration collides with active package %s", component.ID)
			}
			integrations = append(integrations, profile.IntegrationDefinition{SchemaVersion: 1, Name: component.ID, Retired: true, RetiredSettings: slices.Clone(component.Settings)})
		}
	}
	result.declarations, result.integrations, result.retired, result.root = definitions, integrations, retired, root
	return result, nil
}

// BuildCatalog evaluates declarations without opening packages or resolving hooks.
func BuildCatalog(profiles []profile.Metadata, agents []profile.IntegrationMetadata) (Catalog, error) {
	if err := profile.ValidateMetadata(profiles, agents); err != nil {
		return Catalog{}, err
	}
	definitions := make([]profile.Definition, 0, len(profiles))
	integrations := make([]profile.IntegrationDefinition, 0, len(agents))
	for _, metadata := range profiles {
		data, err := metadata.Data()
		if err != nil {
			return Catalog{}, err
		}
		definitions = append(definitions, profile.Definition{SchemaVersion: data.SchemaVersion, Name: data.Name, DefaultYards: data.DefaultYards, Disabled: data.Disabled, DisabledWhen: data.DisabledWhen, SelectedProvisionOnly: data.SelectedProvisionOnly, OwnerService: data.OwnerService, Native: data.Native, Consumers: data.Consumers, CredentialImportExclusions: data.CredentialImportExclusions, ManagedPaths: data.ManagedPaths, Setup: data.Setup, Settings: data.Settings, Runtime: data.Runtime, GuestEnvironment: data.GuestEnvironment, Dashboard: data.Dashboard, UnmanagedIntegrations: data.UnmanagedIntegrations})
	}
	for _, metadata := range agents {
		data, err := metadata.Data()
		if err != nil {
			return Catalog{}, err
		}
		integrations = append(integrations, profile.IntegrationDefinition{SchemaVersion: data.SchemaVersion, Retired: data.Retired, RetiredRoutes: data.RetiredRoutes, RetiredSettings: data.RetiredSettings, Name: data.Name, Inputs: data.Inputs, Settings: data.Settings, Provision: data.Provision, Command: data.Command, Check: data.Check, Health: data.Health, Depends: data.Depends, Persist: data.Persist, Lifecycle: data.Lifecycle, Dashboard: data.Dashboard, Native: data.Native, Artifacts: data.Artifacts})
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
	var destinations []string
	var links strings.Builder
	for _, owner := range integrations {
		links.WriteString(owner.Persist)
		links.WriteByte(' ')
		for _, artifact := range owner.Artifacts {
			destinations = append(destinations, artifact.Destination)
		}
		if owner.Dashboard != nil {
			field, ok := result.LookupSetting(owner.Dashboard.PortSetting)
			if !ok || field.Type != SettingPort || field.Owner != "integration:"+owner.Name {
				return Catalog{}, fmt.Errorf("integration dashboard requires its owned port setting")
			}
		}
	}
	if err := validateManagedDestinations(destinations, links.String(), ""); err != nil {
		return Catalog{}, err
	}
	return result, nil
}

func (settings Catalog) Profiles() []profile.Definition {
	result := make([]profile.Definition, len(settings.declarations))
	for i := range result {
		result[i] = settings.declarations[i].Clone()
	}
	return result
}

func (settings Catalog) Integrations() []profile.IntegrationDefinition {
	result := make([]profile.IntegrationDefinition, len(settings.integrations))
	for i := range result {
		result[i] = settings.integrations[i].Clone()
	}
	return result
}

// ShippedIntegration reports whether id names an integration package in this catalog.
func (settings Catalog) ShippedIntegration(id string) bool {
	return slices.ContainsFunc(settings.integrations, func(definition profile.IntegrationDefinition) bool { return definition.Name == id })
}

// RetiredIntegration reports a shipped package kept only for migration.
func (settings Catalog) RetiredIntegration(id string) bool {
	return slices.ContainsFunc(settings.integrations, func(definition profile.IntegrationDefinition) bool {
		return definition.Retired && definition.Name == id
	})
}

// CheckSelectableIntegrations refuses every retired ID in an explicit new selection.
func (settings Catalog) CheckSelectableIntegrations(requested []string) error {
	// Retired IDs are shipped package names, so each refusal may name its ID.
	var refusals []error
	for _, id := range requested {
		if settings.RetiredIntegration(id) {
			refusals = append(refusals, namedRefusal(domain.ConfigIntegrationRetired, domain.ConfigIntegrationName, id))
		}
	}
	return domain.CollectRefusals(refusals...)
}

// RetiredProfile recognizes only declared removed profiles, never arbitrary names.
func (settings Catalog) RetiredProfile(name string) bool {
	return slices.ContainsFunc(settings.retired, func(component profile.RetiredComponent) bool {
		return component.Kind == "profile" && component.ID == name
	})
}

// RetiredSetting reports whether the catalog discards a removed setting.
func (settings Catalog) RetiredSetting(name string) bool {
	if isRetiredSetting(name) {
		return true
	}
	for _, component := range settings.retired {
		if slices.Contains(component.Settings, name) {
			return true
		}
	}
	for _, definition := range settings.integrations {
		if definition.Retired && slices.Contains(definition.RetiredSettings, name) {
			return true
		}
	}
	return false
}

// IsLegacySetting includes removed declarations when filtering inherited contexts.
func (settings Catalog) IsLegacySetting(name string) bool {
	_, alias := legacySettingNames[name]
	return alias || settings.RetiredSetting(name)
}

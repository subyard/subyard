package profile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
)

// Metadata and IntegrationMetadata contain validated declarations without native
// roots or executable bindings. Data returns an independent semantic snapshot.
type MetadataData struct {
	SchemaVersion              int                   `json:"schema_version"`
	Name                       string                `json:"-"`
	DefaultYards               []string              `json:"default_yards,omitempty"`
	DisabledWhen               map[string]string     `json:"disabled_when,omitempty"`
	SelectedProvisionOnly      bool                  `json:"selected_provision_only,omitempty"`
	OwnerService               string                `json:"owner_service,omitempty"`
	Native                     []Native              `json:"native,omitempty"`
	Consumers                  []Consumer            `json:"consumers,omitempty"`
	CredentialImportExclusions [][]string            `json:"credential_import_exclusions,omitempty"`
	ManagedPaths               []ManagedPath         `json:"managed_paths,omitempty"`
	Setup                      *Setup                `json:"setup,omitempty"`
	Settings                   []Setting             `json:"settings,omitempty"`
	Runtime                    *RuntimeHook          `json:"runtime,omitempty"`
	GuestEnvironment           *GuestEnvironmentHook `json:"guest_environment,omitempty"`
	Dashboard                  *IntegrationDashboard `json:"dashboard,omitempty"`
}
type Metadata struct {
	data  MetadataData
	valid bool
}

func Decode(name string, content []byte) (Metadata, error) {
	var declaration Definition
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&declaration); err != nil {
		return Metadata{}, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return Metadata{}, errors.New("profile declaration has trailing data")
	}
	declaration.Name = name
	return declaration.Metadata()
}
func (declaration Definition) Metadata() (Metadata, error) {
	if err := declaration.validateSemantic(); err != nil {
		return Metadata{}, err
	}
	return Metadata{data: cloneMetadataData(MetadataData{SchemaVersion: declaration.SchemaVersion, Name: declaration.Name, DefaultYards: declaration.DefaultYards, DisabledWhen: declaration.DisabledWhen, SelectedProvisionOnly: declaration.SelectedProvisionOnly, OwnerService: declaration.OwnerService, Native: declaration.Native, Consumers: declaration.Consumers, CredentialImportExclusions: declaration.CredentialImportExclusions, ManagedPaths: declaration.ManagedPaths, Setup: declaration.Setup, Settings: declaration.Settings, Runtime: declaration.Runtime, GuestEnvironment: declaration.GuestEnvironment, Dashboard: declaration.Dashboard}), valid: true}, nil
}
func (metadata Metadata) Data() (MetadataData, error) {
	if !metadata.valid {
		return MetadataData{}, errors.New("missing validated Metadata")
	}
	return cloneMetadataData(metadata.data), nil
}
func (metadata Metadata) definition() Definition {
	data := cloneMetadataData(metadata.data)
	return Definition{SchemaVersion: data.SchemaVersion, Name: data.Name, DefaultYards: data.DefaultYards, DisabledWhen: data.DisabledWhen, SelectedProvisionOnly: data.SelectedProvisionOnly, OwnerService: data.OwnerService, Native: data.Native, Consumers: data.Consumers, CredentialImportExclusions: data.CredentialImportExclusions, ManagedPaths: data.ManagedPaths, Setup: data.Setup, Settings: data.Settings, Runtime: data.Runtime, GuestEnvironment: data.GuestEnvironment, Dashboard: data.Dashboard}
}
func cloneMetadataData(data MetadataData) MetadataData {
	data.Dashboard = cloneIntegrationMetadataData(IntegrationMetadataData{Dashboard: data.Dashboard}).Dashboard
	data.Native = slices.Clone(data.Native)
	data.Settings = cloneSettings(data.Settings)
	data.DefaultYards = slices.Clone(data.DefaultYards)
	data.DisabledWhen = maps.Clone(data.DisabledWhen)
	data.Consumers = slices.Clone(data.Consumers)
	data.ManagedPaths = slices.Clone(data.ManagedPaths)
	data.CredentialImportExclusions = slices.Clone(data.CredentialImportExclusions)
	for i, fragments := range data.CredentialImportExclusions {
		data.CredentialImportExclusions[i] = slices.Clone(fragments)
	}
	if data.Setup != nil {
		value := *data.Setup
		value.Instructions = slices.Clone(value.Instructions)
		value.Fields = slices.Clone(value.Fields)
		data.Setup = &value
	}
	if data.Runtime != nil {
		value := *data.Runtime
		value.ProjectsChangedHooks = slices.Clone(value.ProjectsChangedHooks)
		data.Runtime = &value
	}
	if data.GuestEnvironment != nil {
		value := *data.GuestEnvironment
		data.GuestEnvironment = &value
	}
	return data
}

type IntegrationMetadataData struct {
	SchemaVersion   int                   `json:"schema_version"`
	Retired         bool                  `json:"retired,omitempty"`
	RetiredRoutes   []RetiredRoute        `json:"retired_routes,omitempty"`
	RetiredSettings []string              `json:"retired_settings,omitempty"`
	Name            string                `json:"-"`
	Inputs          []IntegrationInput    `json:"inputs,omitempty"`
	Settings        []Setting             `json:"settings,omitempty"`
	Provision       string                `json:"provision,omitempty"`
	Command         string                `json:"command,omitempty"`
	Check           string                `json:"check,omitempty"`
	Health          string                `json:"health,omitempty"`
	Cleanup         string                `json:"cleanup,omitempty"`
	Depends         []string              `json:"depends,omitempty"`
	Persist         string                `json:"persist,omitempty"`
	Lifecycle       *IntegrationLifecycle `json:"lifecycle,omitempty"`
	Dashboard       *IntegrationDashboard `json:"dashboard,omitempty"`
	Native          []Native              `json:"native,omitempty"`
}
type IntegrationMetadata struct {
	data  IntegrationMetadataData
	valid bool
}

func DecodeIntegration(name string, content []byte) (IntegrationMetadata, error) {
	var declaration IntegrationDefinition
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&declaration); err != nil {
		return IntegrationMetadata{}, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return IntegrationMetadata{}, errors.New("integration declaration has trailing data")
	}
	declaration.Name = name
	return declaration.Metadata()
}
func (declaration IntegrationDefinition) Metadata() (IntegrationMetadata, error) {
	if err := declaration.validateSemantic(); err != nil {
		return IntegrationMetadata{}, err
	}
	return IntegrationMetadata{data: cloneIntegrationMetadataData(IntegrationMetadataData{SchemaVersion: declaration.SchemaVersion, Retired: declaration.Retired, RetiredRoutes: declaration.RetiredRoutes, RetiredSettings: declaration.RetiredSettings, Name: declaration.Name, Inputs: declaration.Inputs, Settings: declaration.Settings, Provision: declaration.Provision, Command: declaration.Command, Check: declaration.Check, Health: declaration.Health, Cleanup: declaration.Cleanup, Depends: declaration.Depends, Persist: declaration.Persist, Lifecycle: declaration.Lifecycle, Dashboard: declaration.Dashboard, Native: declaration.Native}), valid: true}, nil
}
func (metadata IntegrationMetadata) Data() (IntegrationMetadataData, error) {
	if !metadata.valid {
		return IntegrationMetadataData{}, errors.New("missing validated IntegrationMetadata")
	}
	return cloneIntegrationMetadataData(metadata.data), nil
}
func (metadata IntegrationMetadata) definition() IntegrationDefinition {
	data := cloneIntegrationMetadataData(metadata.data)
	return IntegrationDefinition{SchemaVersion: data.SchemaVersion, Retired: data.Retired, RetiredRoutes: data.RetiredRoutes, RetiredSettings: data.RetiredSettings, Name: data.Name, Inputs: data.Inputs, Settings: data.Settings, Provision: data.Provision, Command: data.Command, Check: data.Check, Health: data.Health, Cleanup: data.Cleanup, Depends: data.Depends, Persist: data.Persist, Lifecycle: data.Lifecycle, Dashboard: data.Dashboard, Native: data.Native}
}
func cloneIntegrationMetadataData(data IntegrationMetadataData) IntegrationMetadataData {
	data.RetiredRoutes = slices.Clone(data.RetiredRoutes)
	data.RetiredSettings = slices.Clone(data.RetiredSettings)
	data.Native = slices.Clone(data.Native)
	data.Settings = cloneSettings(data.Settings)
	data.Inputs = slices.Clone(data.Inputs)
	data.Depends = slices.Clone(data.Depends)
	if data.Lifecycle != nil {
		value := *data.Lifecycle
		value.SourceFiles = slices.Clone(value.SourceFiles)
		value.InstanceConfigKeys = slices.Clone(value.InstanceConfigKeys)
		value.InstanceDevices = slices.Clone(value.InstanceDevices)
		data.Lifecycle = &value
	}
	if data.Dashboard != nil {
		value := *data.Dashboard
		if value.ContainerRoute != nil {
			route := *value.ContainerRoute
			value.ContainerRoute = &route
		}
		data.Dashboard = &value
	}
	return data
}
func cloneSettings(settings []Setting) []Setting {
	settings = slices.Clone(settings)
	for i := range settings {
		settings[i].Scopes = slices.Clone(settings[i].Scopes)
		settings[i].Enum = slices.Clone(settings[i].Enum)
		if settings[i].Default != nil {
			value := *settings[i].Default
			settings[i].Default = &value
		}
		if settings[i].DefaultFrom != nil {
			value := *settings[i].DefaultFrom
			settings[i].DefaultFrom = &value
		}
	}
	return settings
}

// ValidateMetadata checks operation-local ownership collisions independently of
// package files. Zero metadata and invalid snapshots fail closed.
func ValidateMetadata(profiles []Metadata, integrations []IntegrationMetadata) error {
	consumers, setupPaths, settings, activations := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, metadata := range profiles {
		definition, err := metadata.Data()
		if err != nil {
			return err
		}
		for _, consumer := range definition.Consumers {
			if consumers[consumer.ID] {
				return fmt.Errorf("duplicate profile credential consumer %s", consumer.ID)
			}
			consumers[consumer.ID] = true
		}
		if definition.Setup != nil {
			if setupPaths[definition.Setup.ConfigFile] {
				return errors.New("profile setup configuration path collision")
			}
			setupPaths[definition.Setup.ConfigFile] = true
		}
		for _, setting := range definition.Settings {
			if settings[setting.Name] {
				return fmt.Errorf("duplicate profile setting %s", setting.Name)
			}
			settings[setting.Name] = true
		}
		if definition.Runtime != nil {
			if activations[definition.Runtime.ActivationID] {
				return fmt.Errorf("duplicate profile runtime activation ID %s", definition.Runtime.ActivationID)
			}
			activations[definition.Runtime.ActivationID] = true
		}
	}
	keys, devices, names := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, metadata := range integrations {
		definition, err := metadata.Data()
		if err != nil {
			return err
		}
		if names[definition.Name] {
			return errors.New("integration name collision")
		}
		names[definition.Name] = true
		if definition.Lifecycle != nil {
			for _, key := range definition.Lifecycle.InstanceConfigKeys {
				if keys[key] {
					return errors.New("integration native key collision")
				}
				keys[key] = true
			}
			for _, name := range definition.Lifecycle.InstanceDevices {
				if devices[name] {
					return errors.New("integration device collision")
				}
				devices[name] = true
			}
		}
	}
	return nil
}

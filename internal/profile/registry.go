// Package profile reads optional, shipped profile extension declarations.
package profile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type Native struct {
	Package string `json:"package"`
	Path    string `json:"path"`
}
type Consumer struct {
	ID          string `json:"id"`
	Zone        string `json:"zone"`
	Path        string `json:"path"`
	Format      string `json:"format"`
	LegacyPath  string `json:"legacy_path,omitempty"`
	StopHandler string `json:"stop_handler,omitempty"`
}
type ManagedPath struct {
	Root string `json:"root"`
	Path string `json:"path"`
}
type Field struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Kind  string `json:"kind"`
}
type Setup struct {
	Title            string   `json:"title"`
	Instructions     []string `json:"instructions"`
	ConfigFile       string   `json:"config_file"`
	Fields           []Field  `json:"fields"`
	Consumer         string   `json:"consumer"`
	Zone             string   `json:"zone"`
	Label            string   `json:"label"`
	KeyOverrideField string   `json:"key_override_field,omitempty"`
	Followup         string   `json:"followup"`
	SyncFields       bool     `json:"sync_fields,omitempty"`
}
type Definition struct {
	SchemaVersion              int                   `json:"schema_version"`
	Name                       string                `json:"-"`
	Root                       string                `json:"-"`
	DefaultYards               []string              `json:"default_yards,omitempty"`
	DisabledWhen               map[string]string     `json:"disabled_when,omitempty"`
	SelectedProvisionOnly      bool                  `json:"selected_provision_only,omitempty"`
	OwnerService               string                `json:"owner_service,omitempty"`
	LifecycleOnly              bool                  `json:"lifecycle_only,omitempty"`
	FullCleanup                string                `json:"full_cleanup,omitempty"`
	FullCleanupEnvironment     []string              `json:"full_cleanup_environment,omitempty"`
	Native                     []Native              `json:"native,omitempty"`
	Consumers                  []Consumer            `json:"consumers,omitempty"`
	CredentialImportExclusions [][]string            `json:"credential_import_exclusions,omitempty"`
	ManagedPaths               []ManagedPath         `json:"managed_paths,omitempty"`
	Setup                      *Setup                `json:"setup,omitempty"`
	Dashboard                  *IntegrationDashboard `json:"dashboard,omitempty"`
	Settings                   []Setting             `json:"settings,omitempty"`
	Runtime                    *RuntimeHook          `json:"runtime,omitempty"`
	GuestEnvironment           *GuestEnvironmentHook `json:"guest_environment,omitempty"`
	validationInputs           []string
}

func (definition Definition) Selected(yard string, environment map[string]string) bool {
	if definition.LifecycleOnly {
		return false
	}
	for key, value := range definition.DisabledWhen {
		if actual, ok := environment[key]; ok && actual == value {
			return false
		}
	}
	if profiles, explicit := environment["ENVIRONMENT_PROFILES"]; explicit {
		return slices.Contains(strings.Fields(profiles), definition.Name)
	}
	return slices.Contains(definition.DefaultYards, yard)
}

// Load uses only immutable shipped packages. No external registry or operator
// code is loaded, and absence of a descriptor preserves existing profile hooks.
func Load(root string) ([]Definition, error) {
	packages, err := loadDeclarations(root, "profiles", "profile.json")
	if err != nil {
		return nil, err
	}
	result := make([]Definition, 0, len(packages))
	metadataInputs := make([]Metadata, 0, len(packages))
	for _, shipped := range packages {
		path, data := shipped.path, shipped.data
		metadata, err := Decode(shippedName(path), data)
		if err != nil {
			return nil, fmt.Errorf("profile declaration %s: %w", path, err)
		}
		definition := metadata.definition()
		definition.Root = filepath.Dir(path)
		definition.validationInputs = []string{path}
		if err := definition.validatePackage(); err != nil {
			return nil, fmt.Errorf("profile %s: %w", definition.Name, err)
		}
		metadataInputs = append(metadataInputs, metadata)
		result = append(result, definition)
	}
	if err := ValidateMetadata(metadataInputs, nil); err != nil {
		return nil, err
	}
	return result, nil
}

// ValidationInputs returns the package files actually inspected by Load.
// Consumers can bind metadata and hook identity without duplicating validation.
func (definition Definition) ValidationInputs() []string {
	return slices.Clone(definition.validationInputs)
}

func shippedName(path string) string { return filepath.Base(filepath.Dir(path)) }

func (definition *Definition) validatePackage() error {
	if definition.OwnerService != "" {
		info, err := os.Lstat(filepath.Join(definition.Root, definition.OwnerService))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return errors.New("owner service must be an executable regular file")
		}
		resolved, err := filepath.EvalSymlinks(filepath.Join(definition.Root, definition.OwnerService))
		base, baseErr := filepath.EvalSymlinks(definition.Root)
		if err != nil || baseErr != nil || resolved != filepath.Join(base, definition.OwnerService) {
			return errors.New("owner service must not traverse symlinks")
		}
		definition.validationInputs = append(definition.validationInputs, resolved)
	}
	if definition.FullCleanup != "" {
		path := filepath.Join(definition.Root, definition.FullCleanup)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return errors.New("full cleanup assessment must be an executable regular file")
		}
		resolved, err := filepath.EvalSymlinks(path)
		base, baseErr := filepath.EvalSymlinks(definition.Root)
		if err != nil || baseErr != nil || resolved != filepath.Join(base, definition.FullCleanup) {
			return errors.New("full cleanup assessment must not traverse symlinks")
		}
		definition.validationInputs = append(definition.validationInputs, resolved)
	}
	for _, consumer := range definition.Consumers {
		if consumer.StopHandler != "" {
			if err := definition.validateExecutable(consumer.StopHandler); err != nil {
				return fmt.Errorf("credential stop handler: %w", err)
			}
		}
	}
	if definition.Runtime != nil {
		if err := definition.validateExecutable(definition.Runtime.Handler); err != nil {
			return fmt.Errorf("runtime handler: %w", err)
		}
	}
	if definition.GuestEnvironment != nil {
		if err := definition.validateExecutable(definition.GuestEnvironment.Handler); err != nil {
			return fmt.Errorf("guest environment handler: %w", err)
		}
	}
	return nil
}

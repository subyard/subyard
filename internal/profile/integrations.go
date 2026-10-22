package profile

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

type PortDefault struct {
	Setting string `json:"setting"`
	Offset  int    `json:"offset"`
}
type IntegrationInput struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
}
type IntegrationLifecycle struct {
	Handler            string   `json:"handler"`
	SourceFiles        []string `json:"source_files"`
	InstanceConfigKeys []string `json:"instance_config_keys"`
	InstanceDevices    []string `json:"instance_devices"`
}
type IntegrationRoute struct {
	MarkerKey  string `json:"marker_key"`
	DeviceName string `json:"device_name"`
}
type IntegrationDashboard struct {
	Scheme         string            `json:"scheme"`
	Path           string            `json:"path"`
	PortSetting    string            `json:"port_setting"`
	GuestPort      int               `json:"guest_port"`
	ContainerRoute *IntegrationRoute `json:"container_route,omitempty"`
}
type RetiredRoute struct {
	MarkerKey  string `json:"marker_key"`
	DeviceName string `json:"device_name"`
	GuestPort  int    `json:"guest_port"`
}

type IntegrationDefinition struct {
	SchemaVersion   int                   `json:"schema_version"`
	Retired         bool                  `json:"retired,omitempty"`
	RetiredRoutes   []RetiredRoute        `json:"retired_routes,omitempty"`
	RetiredSettings []string              `json:"retired_settings,omitempty"`
	Name            string                `json:"-"`
	Root            string                `json:"-"`
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
	Artifacts       []IntegrationArtifact `json:"artifacts,omitempty"`

	validationInputs []string
}

func LoadIntegrations(root string) ([]IntegrationDefinition, error) {
	packages, err := loadDeclarations(root, "agents", "integration.json")
	if err != nil {
		return nil, err
	}
	retired, err := loadDeclarations(root, "retired-integrations", "integration.json")
	if err != nil {
		return nil, err
	}
	packages = append(packages, retired...)
	result := make([]IntegrationDefinition, 0, len(packages))
	metadataInputs := make([]IntegrationMetadata, 0, len(packages))
	names := map[string]bool{}
	for _, shipped := range packages {
		metadata, err := DecodeIntegration(shippedName(shipped.path), shipped.data)
		if err != nil {
			return nil, fmt.Errorf("integration declaration: %w", err)
		}
		definition := metadata.definition()
		definition.Root = filepath.Dir(shipped.path)
		if names[definition.Name] || (filepath.Base(filepath.Dir(definition.Root)) == "retired-integrations") != definition.Retired {
			return nil, errors.New("integration name collision or invalid retirement package")
		}
		names[definition.Name] = true
		definition.validationInputs = []string{shipped.path}
		if err := definition.validatePackage(); err != nil {
			return nil, fmt.Errorf("integration %s: %w", definition.Name, err)
		}
		metadataInputs = append(metadataInputs, metadata)
		result = append(result, definition)
	}
	if err := ValidateMetadata(nil, metadataInputs); err != nil {
		return nil, err
	}
	return result, nil
}

func (definition IntegrationDefinition) packageDefinition() Definition {
	return Definition{Root: definition.Root, Name: definition.Name, SchemaVersion: 1, Settings: definition.Settings}
}
func (definition IntegrationDefinition) ExecutablePath(path string) (string, error) {
	return definition.packageDefinition().ExecutablePath(path)
}
func (definition IntegrationDefinition) ReadSource(path string) ([]byte, error) {
	validated, err := definition.packageDefinition().sourcePath(path, false)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(validated)
	if err != nil || info.Size() > 4<<20 || len(definition.Artifacts) != 0 && !safeArtifactFile(info) {
		return nil, errors.New("integration source is unbounded")
	}
	return os.ReadFile(validated)
}
func (definition IntegrationDefinition) SourceBinding() (string, error) {
	files := []string{"integration.json"}
	for _, artifact := range definition.Artifacts {
		if artifact.Source != "" {
			files = append(files, artifact.Source)
		}
	}
	for _, path := range []string{definition.Provision, definition.Cleanup} {
		if path != "" {
			files = append(files, path)
		}
	}
	if definition.Lifecycle != nil {
		files = append(files, definition.Lifecycle.Handler)
		files = append(files, definition.Lifecycle.SourceFiles...)
	}
	sources := map[string][]byte{}
	for _, path := range files {
		data, err := definition.ReadSource(path)
		if err != nil {
			return "", err
		}
		sources[path] = data
	}
	// Native producer bytes are part of the approved package, including source
	// development. Never substitute source hashes for a missing executable.
	for _, native := range definition.Native {
		path, err := definition.NativeExecutablePath(native.Path)
		if err != nil {
			return "", err
		}
		info, err := os.Stat(path)
		if err != nil || info.Size() > 16<<20 {
			return "", errors.New("integration native artifact is unbounded")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		sources[native.Path] = data
	}
	data, _ := json.Marshal(struct {
		Definition IntegrationDefinition
		Sources    map[string][]byte
	}{definition, sources})
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

// NativeExecutablePath prefers package-contained assets in installed/sealed
// runtimes. Plain source copies may use the development build namespace; Git
// metadata is neither required nor inspected. A present invalid package asset
// must never be hidden by a development fallback.
func (definition IntegrationDefinition) NativeExecutablePath(path string) (string, error) {
	declared := false
	for _, native := range definition.Native {
		declared = declared || native.Path == path
	}
	if !declared {
		return "", errors.New("undeclared integration native artifact")
	}
	if _, err := os.Lstat(filepath.Join(definition.Root, path)); !os.IsNotExist(err) {
		return definition.ExecutablePath(path)
	}
	root := filepath.Clean(filepath.Join(definition.Root, "..", "..", ".."))
	// Installed runtimes do not include the source module. Validate it through
	// the same symlink-free source-path guard before allowing .build resolution.
	development := Definition{Root: root}
	if _, err := development.sourcePath("go.mod", false); err != nil {
		return "", errors.New("integration native artifact is missing")
	}
	return development.ExecutablePath(filepath.Join(".build", "agents", definition.Name, path))
}

// ValidationInputs returns the package files actually inspected by LoadIntegrations.
func (definition IntegrationDefinition) ValidationInputs() []string {
	return slices.Clone(definition.validationInputs)
}

func (definition *IntegrationDefinition) validatePackage() error {
	if len(definition.Artifacts) != 0 {
		if _, err := definition.ReadSource("integration.json"); err != nil {
			return err
		}
	}
	for _, artifact := range definition.Artifacts {
		if artifact.Source != "" {
			path, err := definition.ArtifactSourcePath(artifact.Source)
			if err != nil {
				return err
			}
			definition.validationInputs = append(definition.validationInputs, path)
		}
	}
	for _, path := range []string{definition.Provision, definition.Cleanup} {
		if path != "" {
			resolved, err := definition.ExecutablePath(path)
			if err != nil {
				return err
			}
			definition.validationInputs = append(definition.validationInputs, resolved)
		}
	}
	if lifecycle := definition.Lifecycle; lifecycle != nil {
		resolved, err := definition.ExecutablePath(lifecycle.Handler)
		if err != nil {
			return err
		}
		definition.validationInputs = append(definition.validationInputs, resolved)
		for _, path := range lifecycle.SourceFiles {
			if _, err := definition.ReadSource(path); err != nil {
				return err
			}
			definition.validationInputs = append(definition.validationInputs, filepath.Join(definition.Root, path))
		}
	}
	return nil
}

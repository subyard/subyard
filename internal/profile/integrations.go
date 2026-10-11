package profile

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
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
type IntegrationDefinition struct {
	SchemaVersion int                   `json:"schema_version"`
	Name          string                `json:"-"`
	Root          string                `json:"-"`
	Inputs        []IntegrationInput    `json:"inputs,omitempty"`
	Settings      []Setting             `json:"settings,omitempty"`
	Provision     string                `json:"provision,omitempty"`
	Command       string                `json:"command,omitempty"`
	Check         string                `json:"check,omitempty"`
	Health        string                `json:"health,omitempty"`
	Cleanup       string                `json:"cleanup,omitempty"`
	Depends       []string              `json:"depends,omitempty"`
	Persist       string                `json:"persist,omitempty"`
	Lifecycle     *IntegrationLifecycle `json:"lifecycle,omitempty"`
	Dashboard     *IntegrationDashboard `json:"dashboard,omitempty"`

	validationInputs []string
}

func LoadIntegrations(root string) ([]IntegrationDefinition, error) {
	packages, err := loadDeclarations(root, "agents", "integration.json")
	if err != nil {
		return nil, err
	}
	result := make([]IntegrationDefinition, 0, len(packages))
	keys, devices := map[string]bool{}, map[string]bool{}
	for _, shipped := range packages {
		var definition IntegrationDefinition
		decoder := json.NewDecoder(bytes.NewReader(shipped.data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&definition); err != nil {
			return nil, fmt.Errorf("integration declaration: %w", err)
		}
		if decoder.Decode(new(any)) != io.EOF {
			return nil, errors.New("integration declaration has trailing data")
		}
		definition.Root, definition.Name = filepath.Dir(shipped.path), filepath.Base(filepath.Dir(shipped.path))
		definition.validationInputs = []string{shipped.path}
		if err := definition.validate(); err != nil {
			return nil, fmt.Errorf("integration %s: %w", definition.Name, err)
		}
		if definition.Lifecycle != nil {
			for _, key := range definition.Lifecycle.InstanceConfigKeys {
				if keys[key] {
					return nil, errors.New("integration native key collision")
				}
				keys[key] = true
			}
			for _, name := range definition.Lifecycle.InstanceDevices {
				if devices[name] {
					return nil, errors.New("integration device collision")
				}
				devices[name] = true
			}
		}
		result = append(result, definition)
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
	if err != nil || info.Size() > 4<<20 {
		return nil, errors.New("integration source is unbounded")
	}
	return os.ReadFile(validated)
}
func (definition IntegrationDefinition) SourceBinding() (string, error) {
	files := []string{"integration.json"}
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
	data, _ := json.Marshal(struct {
		Definition IntegrationDefinition
		Sources    map[string][]byte
	}{definition, sources})
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

// ValidationInputs returns the package files actually inspected by LoadIntegrations.
func (definition IntegrationDefinition) ValidationInputs() []string {
	return slices.Clone(definition.validationInputs)
}

func (definition *IntegrationDefinition) validate() error {
	if definition.SchemaVersion != 1 || !domain.SafeName(definition.Name) {
		return errors.New("unsupported integration schema or name")
	}
	packaged := definition.packageDefinition()
	if err := packaged.validateExtensions(); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, input := range definition.Inputs {
		if !settingNamePattern.MatchString(input.Name) || seen[input.Name] || !textSafe(input.Value) {
			return errors.New("invalid immutable input")
		}
		seen[input.Name] = true
		if input.Type != "version" && input.Type != "image-reference" && input.Type != "sha256" && input.Type != "string" {
			return errors.New("unsupported immutable input type")
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
	for _, command := range []string{definition.Command, definition.Check, definition.Health} {
		if command != "" && strings.IndexFunc(command, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._/-", r))
		}) >= 0 {
			return errors.New("invalid integration command")
		}
	}
	for _, dependency := range definition.Depends {
		if !domain.SafeName(dependency) || seen[dependency] {
			return errors.New("invalid duplicate dependency")
		}
		seen[dependency] = true
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
		seen := map[string]bool{}
		for _, key := range lifecycle.InstanceConfigKeys {
			if !strings.HasPrefix(key, "user.subyard.") || !textSafe(key) || seen[key] {
				return errors.New("invalid integration native key")
			}
			seen[key] = true
		}
		for _, name := range lifecycle.InstanceDevices {
			if !domain.SafeName(name) || seen[name] {
				return errors.New("invalid integration native device")
			}
			seen[name] = true
		}
	}
	if dashboard := definition.Dashboard; dashboard != nil {
		if dashboard.Scheme != "http" && dashboard.Scheme != "https" || !strings.HasPrefix(dashboard.Path, "/") || !textSafe(dashboard.Path) || dashboard.GuestPort < 1 || dashboard.GuestPort > 65535 || !settingNamePattern.MatchString(dashboard.PortSetting) {
			return errors.New("invalid integration dashboard")
		}
		if route := dashboard.ContainerRoute; route != nil {
			if definition.Lifecycle == nil {
				return errors.New("dashboard route requires lifecycle ownership")
			}
			ownsKey, ownsDevice := false, false
			for _, key := range definition.Lifecycle.InstanceConfigKeys {
				ownsKey = ownsKey || key == route.MarkerKey
			}
			for _, name := range definition.Lifecycle.InstanceDevices {
				ownsDevice = ownsDevice || name == route.DeviceName
			}
			if !ownsKey || !ownsDevice {
				return errors.New("dashboard route is outside lifecycle ownership")
			}
		}
	}
	return nil
}

// Package profile reads optional, shipped profile extension declarations.
package profile

import (
	"bytes"
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

type Native struct {
	Package string `json:"package"`
	Path    string `json:"path"`
}
type Consumer struct {
	ID     string `json:"id"`
	Zone   string `json:"zone"`
	Path   string `json:"path"`
	Format string `json:"format"`
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
}
type Definition struct {
	SchemaVersion         int               `json:"schema_version"`
	Name                  string            `json:"-"`
	Root                  string            `json:"-"`
	DefaultYards          []string          `json:"default_yards,omitempty"`
	DisabledWhen          map[string]string `json:"disabled_when,omitempty"`
	SelectedProvisionOnly bool              `json:"selected_provision_only,omitempty"`
	OwnerService          string            `json:"owner_service,omitempty"`
	Native                []Native          `json:"native,omitempty"`
	Consumers             []Consumer        `json:"consumers,omitempty"`
	ManagedPaths          []ManagedPath     `json:"managed_paths,omitempty"`
	Setup                 *Setup            `json:"setup,omitempty"`
}

func (definition Definition) Selected(yard string, environment map[string]string) bool {
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
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	paths, err := filepath.Glob(filepath.Join(root, "config", "profiles", "*", "profile.json"))
	if err != nil {
		return nil, err
	}
	result := make([]Definition, 0, len(paths))
	consumers := map[string]bool{}
	settings := map[string]bool{}
	for _, path := range paths {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		parent, err := os.Lstat(filepath.Dir(path))
		if err != nil || !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("profile package must be a real directory")
		}
		if !info.Mode().IsRegular() || info.Size() > 64<<10 {
			return nil, errors.New("profile declaration must be a bounded regular file")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var definition Definition
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&definition); err != nil {
			return nil, fmt.Errorf("profile declaration %s: %w", path, err)
		}
		if decoder.Decode(new(any)) != io.EOF {
			return nil, errors.New("profile declaration has trailing data")
		}
		definition.Root = filepath.Dir(path)
		definition.Name = filepath.Base(definition.Root)
		if err := definition.validate(); err != nil {
			return nil, fmt.Errorf("profile %s: %w", definition.Name, err)
		}
		for _, consumer := range definition.Consumers {
			if consumers[consumer.ID] {
				return nil, fmt.Errorf("duplicate profile credential consumer %s", consumer.ID)
			}
			consumers[consumer.ID] = true
		}
		if definition.Setup != nil {
			if settings[definition.Setup.ConfigFile] {
				return nil, errors.New("profile setup configuration path collision")
			}
			settings[definition.Setup.ConfigFile] = true
		}
		result = append(result, definition)
	}
	return result, nil
}

func relative(path string) bool {
	return path != "" && path != "." && !filepath.IsAbs(path) && filepath.Clean(path) == path &&
		path != ".." && !strings.HasPrefix(path, "../") && !strings.ContainsAny(path, "\\\r\n\t")
}
func textSafe(value string) bool {
	return len(value) <= 4096 && !strings.ContainsAny(value, "\x00\x1b\r\n")
}
func (definition Definition) validate() error {
	if definition.SchemaVersion != 1 || !domain.SafeName(definition.Name) {
		return errors.New("unsupported profile schema or name")
	}
	for _, yard := range definition.DefaultYards {
		if !domain.SafeName(yard) {
			return errors.New("invalid default yard")
		}
	}
	for key, value := range definition.DisabledWhen {
		if key == "" || strings.IndexFunc(key, func(r rune) bool { return !(r == '_' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') }) >= 0 || !textSafe(value) {
			return errors.New("invalid selection condition")
		}
	}
	if definition.OwnerService != "" {
		if !relative(definition.OwnerService) {
			return errors.New("owner service path escapes profile")
		}
		info, err := os.Lstat(filepath.Join(definition.Root, definition.OwnerService))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return errors.New("owner service must be an executable regular file")
		}
		resolved, err := filepath.EvalSymlinks(filepath.Join(definition.Root, definition.OwnerService))
		base, baseErr := filepath.EvalSymlinks(definition.Root)
		if err != nil || baseErr != nil || resolved != filepath.Join(base, definition.OwnerService) {
			return errors.New("owner service must not traverse symlinks")
		}
	}
	nativePaths := map[string]bool{}
	for _, native := range definition.Native {
		if !relative(native.Path) || !relative(native.Package) || nativePaths[native.Path] {
			return errors.New("invalid or duplicate native artifact path")
		}
		nativePaths[native.Path] = true
	}
	for _, consumer := range definition.Consumers {
		if !domain.SafeName(consumer.ID) || !domain.SafeName(consumer.Zone) || !relative(consumer.Path) {
			return errors.New("invalid profile credential declaration")
		}
		if consumer.Format != "file" && consumer.Format != "rsa-private-key" {
			return errors.New("unsupported profile credential format")
		}
	}
	for _, path := range definition.ManagedPaths {
		if path.Root != "data" && path.Root != "operator" {
			return errors.New("invalid managed path root")
		}
		if !relative(strings.ReplaceAll(path.Path, "{yard}", "yard")) || strings.ContainsAny(strings.ReplaceAll(path.Path, "{yard}", "yard"), "{}") {
			return errors.New("invalid managed profile path")
		}
	}
	if setup := definition.Setup; setup != nil {
		if !relative(setup.ConfigFile) || setup.Title == "" || !textSafe(setup.Title) || !textSafe(setup.Followup) || !domain.SafeName(setup.Label) {
			return errors.New("invalid setup declaration")
		}
		for _, line := range setup.Instructions {
			if !textSafe(line) {
				return errors.New("invalid setup instruction")
			}
		}
		if len(setup.Fields) == 0 || len(setup.Fields) > 16 {
			return errors.New("setup requires bounded fields")
		}
		names := map[string]bool{}
		for _, field := range setup.Fields {
			if !domain.SafeName(field.Name) || names[field.Name] || field.Label == "" || !textSafe(field.Label) {
				return errors.New("invalid setup field")
			}
			names[field.Name] = true
			if field.Kind != "numeric-string" && field.Kind != "positive-integer" {
				return errors.New("unsupported setup field type")
			}
		}
		if setup.KeyOverrideField != "" && (!domain.SafeName(setup.KeyOverrideField) || names[setup.KeyOverrideField]) {
			return errors.New("invalid credential override field")
		}
		found := false
		for _, consumer := range definition.Consumers {
			found = found || consumer.ID == setup.Consumer && consumer.Zone == setup.Zone
		}
		if !found {
			return errors.New("setup references undeclared credential consumer")
		}
	}
	return nil
}

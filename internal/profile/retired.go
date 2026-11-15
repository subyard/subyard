package profile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/observation"
)

// RetiredComponent declares stored-input compatibility without executable code.
type RetiredComponent struct {
	ID           string                 `json:"id"`
	Kind         string                 `json:"kind"`
	Settings     []string               `json:"settings,omitempty"`
	AdoptionKeys []string               `json:"adoption_keys,omitempty"`
	Route        *RetiredComponentRoute `json:"route,omitempty"`
}

type RetiredComponentRoute struct {
	MarkerKey  string `json:"marker_key"`
	DeviceName string `json:"device_name"`
	GuestPort  int    `json:"guest_port"`
	Guard      bool   `json:"guard,omitempty"`
	Defer      bool   `json:"defer,omitempty"`
}

// LoadRetired reads the owning data declaration. Fixture roots may omit it.
func LoadRetired(root string) ([]RetiredComponent, error) {
	content, err := os.ReadFile(filepath.Join(root, "config", "retired-components.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(content) > 1<<20 {
		return nil, errors.New("retired component declaration exceeds size limit")
	}
	var manifest struct {
		SchemaVersion int                `json:"schema_version"`
		Components    []RetiredComponent `json:"components"`
	}
	if err := observation.ValidateJSON(content); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, err
	}
	if decoder.Decode(new(any)) != io.EOF || manifest.SchemaVersion != 1 {
		return nil, errors.New("invalid retired component schema")
	}
	seen := map[string]bool{}
	for _, component := range manifest.Components {
		identity := component.Kind + ":" + component.ID
		if !domain.SafeName(component.ID) || seen[identity] || (component.Kind != "integration" && component.Kind != "profile") {
			return nil, errors.New("invalid retired component identity")
		}
		seen[identity] = true
		if component.Kind == "integration" {
			if _, err := (IntegrationDefinition{SchemaVersion: 1, Name: component.ID, Retired: true, RetiredSettings: component.Settings}).Metadata(); err != nil {
				return nil, err
			}
		} else {
			for _, setting := range component.Settings {
				if !settingNamePattern.MatchString(setting) {
					return nil, fmt.Errorf("invalid retired setting %q", setting)
				}
			}
		}
		for _, key := range component.AdoptionKeys {
			if !strings.HasPrefix(key, "user.subyard.") || !textSafe(key) || strings.ContainsAny(key, " \t\r\n") {
				return nil, errors.New("invalid retired adoption key")
			}
		}
		if route := component.Route; route != nil {
			if !domain.SafeName(route.DeviceName) || !strings.HasPrefix(route.MarkerKey, "user.subyard.") || !textSafe(route.MarkerKey) || strings.ContainsAny(route.MarkerKey, " \t\r\n") || route.GuestPort < 1 || route.GuestPort > 65535 {
				return nil, errors.New("invalid retired route")
			}
		}
	}
	return manifest.Components, nil
}

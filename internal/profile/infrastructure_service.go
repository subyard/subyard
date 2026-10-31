package profile

import (
	"errors"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
)

// InfrastructureService declares a profile-owned runtime role. The ordinary
// yard reconciler still owns creation, power, upgrades and removal.
type InfrastructureService struct {
	ID               string            `json:"id"`
	DefaultYard      string            `json:"default_yard"`
	RequiredSettings map[string]string `json:"required_settings"`
	RouteRoot        string            `json:"route_root"`
	BindingHandler   string            `json:"binding_handler,omitempty"`
}

func (service InfrastructureService) validate() error {
	if !domain.SafeName(service.ID) || !domain.SafeName(service.DefaultYard) || service.DefaultYard == "default" || !relative(service.RouteRoot) || len(service.RequiredSettings) == 0 || len(service.RequiredSettings) > 32 {
		return errors.New("invalid infrastructure service declaration")
	}
	if service.BindingHandler != "" && !relative(service.BindingHandler) {
		return errors.New("invalid infrastructure service binding handler")
	}
	for key, value := range service.RequiredSettings {
		if key == "" || strings.IndexFunc(key, func(r rune) bool { return !(r == '_' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') }) >= 0 || !textSafe(value) {
			return errors.New("invalid infrastructure service required setting")
		}
	}
	return nil
}

func (service InfrastructureService) Matches(environment map[string]string) bool {
	for key, value := range service.RequiredSettings {
		if environment[key] != value {
			return false
		}
	}
	return true
}

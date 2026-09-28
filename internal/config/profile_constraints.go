package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
)

// ProfileProvisionScope is metadata on the existing environment profile, not a
// second registry. Dedicated hooks require the corresponding exclusive yard role.
func ProfileProvisionScope(configDir, name string) (string, error) {
	if !domain.SafeName(name) {
		return "", fmt.Errorf("invalid environment profile %q", name)
	}
	values, err := ReadAssignments(filepath.Join(configDir, "profiles", name, "profile.conf"))
	if errors.Is(err, os.ErrNotExist) {
		return "shared", nil
	}
	if err != nil {
		return "", err
	}
	switch values["PROVISION_SCOPE"] {
	case "", "shared":
		return "shared", nil
	case "dedicated":
		return "dedicated", nil
	default:
		return "", fmt.Errorf("profile %q has an unsupported PROVISION_SCOPE", name)
	}
}

func validateProfileConstraints(configDir, yard string, values environment) error {
	if required := values["REQUIRED_YARD_KIND"]; required != "" && values["YARD_KIND"] != required {
		return fmt.Errorf("selected yard role requires YARD_KIND=%s", required)
	}
	exclusive := values["EXCLUSIVE_ENVIRONMENT_PROFILE"]
	if exclusive != "" && yard == "default" {
		return fmt.Errorf("exclusive environment profiles require a dedicated named yard")
	}
	for _, profile := range strings.Fields(values["ENVIRONMENT_PROFILES"]) {
		if exclusive != "" && profile != exclusive {
			return fmt.Errorf("selected yard role permits only environment profile %q", exclusive)
		}
		scope, err := ProfileProvisionScope(configDir, profile)
		if err != nil {
			return err
		}
		if scope == "dedicated" && exclusive != profile {
			return fmt.Errorf("profile %q requires its dedicated named-yard preset", profile)
		}
	}
	if values["ALLOWS_HOST_ACCESS"] == "false" {
		for _, name := range []string{
			"HOST_MOUNTS", "HOST_LINKS", "HOST_CLAUDE_MD", "HOST_CODEX_AGENTS_MD",
			"HOST_OPENCODE_AGENTS_MD", "YARD_CAPABILITIES", "YARD_CAPS", "YARD_DEVICES", "YARD_MOUNTS",
		} {
			if strings.TrimSpace(values[name]) != "" {
				return fmt.Errorf("selected yard role forbids non-empty %s", name)
			}
		}
		for _, name := range []string{"FORWARD_SSH_AGENT", "NESTED_E2E_VMS"} {
			if value := values[name]; value != "" && value != "0" {
				return fmt.Errorf("selected yard role requires %s=0", name)
			}
		}
	}
	if values["VM_FREE_PAGE_REPORTING"] == "1" || values["VM_PIN_IPV4"] == "1" || values["SRV_VOLUME_TYPE"] == "block" {
		if values["YARD_KIND"] != "vm" {
			return fmt.Errorf("VM page reporting, IPv4 pinning and block storage require YARD_KIND=vm")
		}
	}
	if values["SRV_VOLUME_TYPE"] == "block" && values["SRV_VOLUME_SIZE"] == "" {
		return fmt.Errorf("block storage requires an explicit SRV_VOLUME_SIZE")
	}
	return nil
}

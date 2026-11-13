package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/profile"
)

// ProfileProvisionScope is metadata on the existing environment profile, not a
// second registry. Dedicated hooks require the corresponding exclusive yard role.
func ProfileProvisionScope(configDir, name string) (string, error) {
	declaration, err := profileDeclaration(configDir, name)
	return declaration.scope, err
}

type profileConstraintDeclaration struct {
	scope              string
	requiresHostAccess bool
}

// profileDeclaration reads a profile's shipped placement metadata from profile.conf.
func profileDeclaration(configDir, name string) (profileConstraintDeclaration, error) {
	declaration := profileConstraintDeclaration{scope: "shared"}
	if !domain.SafeName(name) {
		return declaration, fmt.Errorf("invalid environment profile %q", name)
	}
	pkg, err := profile.ResolvePackageInConfig(configDir, name)
	if errors.Is(err, os.ErrNotExist) {
		return declaration, nil
	}
	if err != nil {
		return declaration, err
	}
	values, err := ProfileValues(pkg.Config, nil)
	if err != nil {
		return declaration, err
	}
	var problems []error
	switch values["PROVISION_SCOPE"] {
	case "", "shared":
	case "dedicated":
		declaration.scope = "dedicated"
	default:
		problems = append(problems, fmt.Errorf("profile %q has an unsupported PROVISION_SCOPE", name))
	}
	// A profile whose runtime needs an owner-host route declares it, so a yard role
	// without host access refuses the selection instead of provisioning a dead client.
	switch values["REQUIRES_HOST_ACCESS"] {
	case "", "0":
	case "1":
		declaration.requiresHostAccess = true
	default:
		problems = append(problems, fmt.Errorf("profile %q has an unsupported REQUIRES_HOST_ACCESS", name))
	}
	return declaration, domain.CollectRefusals(problems...)
}

func validateProfileConstraints(configDir, yard string, values environment) error {
	var refusals []error
	if required := values["REQUIRED_YARD_KIND"]; required != "" && values["YARD_KIND"] != required {
		refusals = append(refusals, fmt.Errorf("selected yard role requires YARD_KIND=%s", required))
	}
	exclusive := values["EXCLUSIVE_ENVIRONMENT_PROFILE"]
	if exclusive != "" && yard == "default" {
		refusals = append(refusals, fmt.Errorf("exclusive environment profiles require a dedicated named yard"))
	}
	noHostAccess := values["ALLOWS_HOST_ACCESS"] == "false"
	for _, profile := range strings.Fields(values["ENVIRONMENT_PROFILES"]) {
		if exclusive != "" && profile != exclusive {
			refusals = append(refusals, fmt.Errorf("selected yard role permits only environment profile %q", exclusive))
		}
		declaration, err := profileDeclaration(configDir, profile)
		if err != nil {
			refusals = append(refusals, err)
			continue
		}
		if declaration.scope == "dedicated" && exclusive != profile {
			refusals = append(refusals, fmt.Errorf("profile %q requires its dedicated named-yard preset", profile))
		}
		if declaration.requiresHostAccess && noHostAccess {
			refusals = append(refusals, fmt.Errorf(
				"profile %q requires host access; this yard role sets ALLOWS_HOST_ACCESS=false, so deselect the profile or use a yard that allows host access", profile))
		}
	}
	if noHostAccess {
		for _, name := range []string{
			"HOST_MOUNTS", "HOST_LINKS", "HOST_CLAUDE_MD", "HOST_CODEX_AGENTS_MD",
			"HOST_OPENCODE_AGENTS_MD", "YARD_CAPABILITIES", "YARD_CAPS", "YARD_DEVICES", "YARD_MOUNTS",
		} {
			if strings.TrimSpace(values[name]) != "" {
				refusals = append(refusals, fmt.Errorf("selected yard role forbids non-empty %s", name))
			}
		}
		for _, name := range []string{"FORWARD_SSH_AGENT", "NESTED_E2E_VMS"} {
			if value := values[name]; value != "" && value != "0" {
				refusals = append(refusals, fmt.Errorf("selected yard role requires %s=0", name))
			}
		}
	}
	if values["VM_FREE_PAGE_REPORTING"] == "1" || values["VM_PIN_IPV4"] == "1" || values["SRV_VOLUME_TYPE"] == "block" || values["VM_CPU_WEIGHT"] != "" {
		if values["YARD_KIND"] != "vm" {
			refusals = append(refusals, fmt.Errorf("VM page reporting, CPU weight, IPv4 pinning and block storage require YARD_KIND=vm"))
		}
	}
	if values["SRV_VOLUME_TYPE"] == "block" && values["SRV_VOLUME_SIZE"] == "" {
		refusals = append(refusals, fmt.Errorf("block storage requires an explicit SRV_VOLUME_SIZE"))
	}
	return domain.CollectRefusals(refusals...)
}

// WithEnvironmentProfiles validates a planned selection without writing settings.
func WithEnvironmentProfiles(loaded Loaded, profiles []string) (Loaded, error) {
	loaded.Environment = cloneEnvironment(loaded.Environment)
	loaded.Environment["ENVIRONMENT_PROFILES"] = strings.Join(profiles, " ")
	if err := validateProfileConstraints(loaded.Context.Paths.ConfigDir, loaded.Context.YardName, loaded.Environment); err != nil {
		return Loaded{}, err
	}
	selection, err := resolveEnvironmentProfiles(loaded.Context.Paths.ConfigDir, loaded.Context.YardName, loaded.Environment, loaded.Catalog)
	if err != nil {
		return Loaded{}, err
	}
	loaded.EnvironmentProfiles = selection
	loaded.Environment["ENVIRONMENT_PROFILES"] = strings.Join(selection.Effective, " ")
	if err := validateHostListenerCollisions(loaded.Environment, loaded.Catalog); err != nil {
		return Loaded{}, err
	}
	return loaded, nil
}

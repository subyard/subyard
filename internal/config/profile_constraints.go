package config

import (
	"errors"
	"fmt"
	"os"
	"slices"
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
		problems = append(problems, namedRefusal(domain.ConfigProfileUnsupportedScope, domain.ConfigProfileName, name))
	}
	// A profile whose runtime needs an owner-host route declares it, so a yard role
	// without host access refuses the selection instead of provisioning a dead client.
	switch values["REQUIRES_HOST_ACCESS"] {
	case "", "0":
	case "1":
		declaration.requiresHostAccess = true
	default:
		problems = append(problems, namedRefusal(domain.ConfigProfileUnsupportedHostAccess, domain.ConfigProfileName, name))
	}
	return declaration, domain.CollectRefusals(problems...)
}

// namedRefusal types a refusal when its name validates; otherwise the same text
// stays unclassified, so an operation boundary does not render the value.
func namedRefusal(code domain.ConfigRefusalCode, validate func(string) (domain.ConfigName, bool), value string) error {
	if name, ok := validate(value); ok {
		return domain.NewConfigRefusal(code, name)
	}
	return domain.UnclassifiedConfigRefusal(code, value)
}

// validateProfileConstraints returns every role and profile placement refusal;
// prior lists refusals found earlier in the same load.
func validateProfileConstraints(configDir, yard string, values environment, prior ...error) error {
	refusals := slices.Clone(prior)
	if required := values["REQUIRED_YARD_KIND"]; required != "" && values["YARD_KIND"] != required {
		refusals = append(refusals, namedRefusal(domain.ConfigRoleRequiresYardKind, domain.ConfigYardKindName, required))
	}
	exclusive := values["EXCLUSIVE_ENVIRONMENT_PROFILE"]
	if exclusive != "" && yard == "default" {
		refusals = append(refusals, domain.NewConfigRefusal(domain.ConfigExclusiveRequiresNamedYard))
	}
	noHostAccess := values["ALLOWS_HOST_ACCESS"] == "false"
	for _, profile := range strings.Fields(values["ENVIRONMENT_PROFILES"]) {
		if exclusive != "" && profile != exclusive {
			refusals = append(refusals, namedRefusal(domain.ConfigRolePermitsOnlyProfile, domain.ConfigProfileName, exclusive))
		}
		declaration, err := profileDeclaration(configDir, profile)
		if err != nil {
			refusals = append(refusals, err)
			continue
		}
		if declaration.scope == "dedicated" && exclusive != profile {
			refusals = append(refusals, namedRefusal(domain.ConfigProfileRequiresDedicatedPreset, domain.ConfigProfileName, profile))
		}
		if declaration.requiresHostAccess && noHostAccess {
			refusals = append(refusals, namedRefusal(domain.ConfigProfileRequiresHostAccess, domain.ConfigProfileName, profile))
		}
	}
	if noHostAccess {
		for _, name := range []string{
			"HOST_MOUNTS", "HOST_LINKS", "HOST_CLAUDE_MD", "HOST_CODEX_AGENTS_MD",
			"HOST_OPENCODE_AGENTS_MD", "YARD_CAPABILITIES",
		} {
			if strings.TrimSpace(values[name]) != "" {
				refusals = append(refusals, namedRefusal(domain.ConfigRoleForbidsSetting, domain.ConfigSettingName, name))
			}
		}
		for _, name := range []string{"FORWARD_SSH_AGENT", "NESTED_E2E_VMS"} {
			if value := values[name]; value != "" && value != "0" {
				refusals = append(refusals, namedRefusal(domain.ConfigRoleRequiresDisabled, domain.ConfigSettingName, name))
			}
		}
	}
	if values["VM_FREE_PAGE_REPORTING"] == "1" || values["VM_PIN_IPV4"] == "1" || values["SRV_VOLUME_TYPE"] == "block" || values["VM_CPU_WEIGHT"] != "" {
		if values["YARD_KIND"] != "vm" {
			refusals = append(refusals, domain.NewConfigRefusal(domain.ConfigVMFeaturesRequireVM))
		}
	}
	if values["SRV_VOLUME_TYPE"] == "block" && values["SRV_VOLUME_SIZE"] == "" {
		refusals = append(refusals, domain.NewConfigRefusal(domain.ConfigBlockStorageRequiresSize))
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

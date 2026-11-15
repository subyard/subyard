package domain

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ConfigRefusalCode is the closed set of configuration refusals that an
// operation boundary may render. Each text is a fixed template that names only
// validated profile, setting or yard-kind names, never a setting value.
type ConfigRefusalCode uint8

const (
	ConfigRoleRequiresYardKind ConfigRefusalCode = iota + 1
	ConfigExclusiveRequiresNamedYard
	ConfigRolePermitsOnlyProfile
	ConfigProfileRequiresDedicatedPreset
	ConfigProfileRequiresHostAccess
	ConfigProfileUnsupportedScope
	ConfigProfileUnsupportedHostAccess
	ConfigRoleForbidsSetting
	ConfigRoleRequiresDisabled
	ConfigVMFeaturesRequireVM
	ConfigBlockStorageRequiresSize
	ConfigRoleForbidsCodingTools
	ConfigTemplateForbidsCodingTools
	ConfigShippedPackageUnavailable
	ConfigShippedPackageSymlink
	ConfigOperatorHomeRequired
	ConfigHomeNotAbsolute
	ConfigRootNotDirectory
	ConfigTransactionInterrupted
)

const roleNext = "deselect the named profile or clear the named setting, or use a yard role that permits it"

var configRefusalTexts = map[ConfigRefusalCode]struct{ template, next string }{
	ConfigRoleRequiresYardKind:           {"selected yard role requires YARD_KIND=%s", "use a yard of the required kind or another yard role"},
	ConfigExclusiveRequiresNamedYard:     {"exclusive environment profiles require a dedicated named yard", "create a named yard from the profile's preset"},
	ConfigRolePermitsOnlyProfile:         {"selected yard role permits only environment profile %q", roleNext},
	ConfigProfileRequiresDedicatedPreset: {"profile %q requires its dedicated named-yard preset", "create a named yard from the profile's preset"},
	ConfigProfileRequiresHostAccess:      {"profile %q requires host access; this yard role sets ALLOWS_HOST_ACCESS=false, so deselect the profile or use a yard that allows host access", roleNext},
	ConfigProfileUnsupportedScope:        {"profile %q has an unsupported PROVISION_SCOPE", "restore the shipped profile package"},
	ConfigProfileUnsupportedHostAccess:   {"profile %q has an unsupported REQUIRES_HOST_ACCESS", "restore the shipped profile package"},
	ConfigRoleForbidsSetting:             {"selected yard role forbids non-empty %s", roleNext},
	ConfigRoleRequiresDisabled:           {"selected yard role requires %s=0", roleNext},
	ConfigVMFeaturesRequireVM:            {"VM page reporting, CPU weight, IPv4 pinning and block storage require YARD_KIND=vm", "clear those settings or use a VM yard"},
	ConfigBlockStorageRequiresSize:       {"block storage requires an explicit SRV_VOLUME_SIZE", "set SRV_VOLUME_SIZE for the yard"},
	ConfigRoleForbidsCodingTools:         {"yard role forbids non-empty CODING_TOOL_INTEGRATIONS", roleNext},
	ConfigTemplateForbidsCodingTools:     {"selected yard template forbids coding tool integrations; clear the yard's CODING_TOOL_INTEGRATIONS and remove temporary selection overrides before changing YARD_TEMPLATE", "clear the yard's CODING_TOOL_INTEGRATIONS"},
	ConfigShippedPackageUnavailable:      {"shipped package directory is unavailable", "restore the runtime's shipped config directory"},
	ConfigShippedPackageSymlink:          {"shipped package directory must not traverse symlinks", "use a real directory path for the shipped config directory"},
	ConfigOperatorHomeRequired:           {"operator home is required", "set HOME or SUBYARD_OPERATOR_HOME"},
	ConfigHomeNotAbsolute:                {"SUBYARD_CONFIG_HOME must be absolute", "set SUBYARD_CONFIG_HOME to an absolute path"},
	ConfigRootNotDirectory:               {"configuration root must be a real directory", "replace the configuration root with an operator-owned directory"},
	ConfigTransactionInterrupted:         {"an interrupted configuration transaction requires recovery with yard config sync", "run yard config sync"},
}

// ConfigName is a profile, setting or yard-kind name that passed validation. Its
// zero value is not a name; values are built only by the constructors below.
type ConfigName struct{ value string }

// ConfigProfileName accepts only a safe environment profile name.
func ConfigProfileName(value string) (ConfigName, bool) { return configName(value, SafeName(value)) }

// ConfigSettingName accepts only an upper-case shell variable name.
func ConfigSettingName(value string) (ConfigName, bool) {
	valid := value != "" && len(value) <= 128 && !(value[0] >= '0' && value[0] <= '9') &&
		strings.Trim(value, "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_") == ""
	return configName(value, valid)
}

// ConfigYardKindName accepts only a supported yard kind.
func ConfigYardKindName(value string) (ConfigName, bool) {
	return configName(value, value == string(YardContainer) || value == string(YardVM))
}

func configName(value string, valid bool) (ConfigName, bool) {
	if !valid {
		return ConfigName{}, false
	}
	return ConfigName{value}, true
}

// ConfigRefusal is a typed, value-free configuration refusal. Its Error text is
// the established diagnostic, so operations that already print it are unchanged.
type ConfigRefusal struct {
	code  ConfigRefusalCode
	names []string
}

// NewConfigRefusal builds a refusal from validated names only.
func NewConfigRefusal(code ConfigRefusalCode, names ...ConfigName) *ConfigRefusal {
	refusal := &ConfigRefusal{code: code}
	for _, name := range names {
		refusal.names = append(refusal.names, name.value)
	}
	return refusal
}

func (refusal *ConfigRefusal) Code() ConfigRefusalCode { return refusal.code }

func (refusal *ConfigRefusal) Error() string {
	text, ok := configRefusalTexts[refusal.code]
	if !ok || strings.Count(text.template, "%") != len(refusal.names) || slices.Contains(refusal.names, "") {
		return "configuration refusal"
	}
	arguments := make([]any, len(refusal.names))
	for index, name := range refusal.names {
		arguments[index] = name
	}
	return fmt.Sprintf(text.template, arguments...)
}

// Next is the fixed next step for the refusal.
func (refusal *ConfigRefusal) Next() string { return configRefusalTexts[refusal.code].next }

// UnclassifiedConfigRefusal keeps the established text when a name failed
// validation. It is deliberately not a ConfigRefusal, so boundaries do not render it.
func UnclassifiedConfigRefusal(code ConfigRefusalCode, values ...string) error {
	arguments := make([]any, len(values))
	for index, value := range values {
		arguments[index] = value
	}
	return errors.New(fmt.Sprintf(configRefusalTexts[code].template, arguments...))
}

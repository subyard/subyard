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
	ConfigSettingCanonicalPort
	ConfigPortCollision
	ConfigDerivedPortSource
	ConfigHostBaseMismatch
	ConfigSettingNumeric
	ConfigSettingZeroOne
	ConfigE2EVMCPU
	ConfigHostCPUUnavailable
	ConfigSettingPositiveInteger
	ConfigSettingPositiveSize
	ConfigE2EVMDiskMinimum
	ConfigE2EVMSlotCountRange
	ConfigE2EVMBootTimeoutRange
	ConfigE2EVMImageUnsafe
	ConfigDependencyDirectoryUnsafe
	ConfigDependencyDirectoryNotOwned
	ConfigPathNotAbsolute
	ConfigPathNotNormalized
	ConfigPathTooBroad
	ConfigYardNameRequired
	ConfigAccessKindInvalid
	ConfigYardKindInvalid
	ConfigNestedE2EVMsRequireContainer
	ConfigContextIdentityRequired
	ConfigShiftModeInvalid
	ConfigDevUIDNegative
	ConfigSSHPortRange
	ConfigOwnerEndpointRequired
	ConfigOwnerYardNameRequired
	ConfigYardNameInvalid
	ConfigUnknownYard
	ConfigYardTemplateInvalid
	ConfigUnknownYardTemplate
	ConfigProfileNameInvalid
	ConfigUnknownIntegration
	ConfigDuplicateIntegration
	ConfigConflictingCommandSettings
)

const (
	roleNext    = "deselect the named profile or clear the named setting, or use a yard role that permits it"
	settingNext = "set the named setting to a supported value, then run config validate"
	pathNext    = "set the named path to a normalized absolute path that is not a broad host directory"
)

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
	ConfigSettingCanonicalPort:           {"%s must be a canonical port in range 1024..65535", settingNext},
	ConfigPortCollision:                  {"%s collides with %s; choose another port", "choose another port for one of the named settings"},
	ConfigDerivedPortSource:              {"derived port %s has invalid source", "set the named port explicitly or fix the port it derives from"},
	ConfigHostBaseMismatch:               {"HOST_BASE must equal RESTRICTED_DISK_PATHS", "set HOST_BASE and RESTRICTED_DISK_PATHS to the same path"},
	ConfigSettingNumeric:                 {"%s must be numeric", settingNext},
	ConfigSettingZeroOne:                 {"%s must be 0 or 1", settingNext},
	ConfigE2EVMCPU:                       {"E2E_VM_CPU must be auto or a positive integer", settingNext},
	ConfigHostCPUUnavailable:             {"cannot resolve E2E_VM_CPU: host CPU count is unavailable", "set E2E_VM_CPU to a positive integer"},
	ConfigSettingPositiveInteger:         {"%s must be a positive integer", settingNext},
	ConfigSettingPositiveSize:            {"%s must use a positive MiB or GiB value", settingNext},
	ConfigE2EVMDiskMinimum:               {"E2E_VM_DISK must be at least 10GiB", settingNext},
	ConfigE2EVMSlotCountRange:            {"E2E_VM_SLOT_COUNT must be an integer from 1 to 1048575", settingNext},
	ConfigE2EVMBootTimeoutRange:          {"E2E_VM_BOOT_TIMEOUT must be an integer from 30 to 1800", settingNext},
	ConfigE2EVMImageUnsafe:               {"E2E_VM_IMAGE contains unsafe characters", settingNext},
	ConfigDependencyDirectoryUnsafe:      {"configuration dependency directory has unsafe type or permissions", "make configuration directories real directories without group or other write access"},
	ConfigDependencyDirectoryNotOwned:    {"configuration dependency directory is not operator-owned", "make configuration directories owned by the operator"},
	ConfigPathNotAbsolute:                {"%s must be an absolute path", pathNext},
	ConfigPathNotNormalized:              {"%s must be normalized", pathNext},
	ConfigPathTooBroad:                   {"%s is too broad", pathNext},
	ConfigYardNameRequired:               {"yard name is required", "select a yard with -Y NAME"},
	ConfigAccessKindInvalid:              {`access kind must be "local" or "remote"`, settingNext},
	ConfigYardKindInvalid:                {`yard kind must be "container" or "vm"`, settingNext},
	ConfigNestedE2EVMsRequireContainer:   {"nested E2E VMs currently require a container yard", "set NESTED_E2E_VMS=0 or use a container yard"},
	ConfigContextIdentityRequired:        {"instance name, Incus project, SSH host and dev user are required", "restore YARD_INSTANCE_NAME, INCUS_PROJECT, SSH_HOST and DEV_USER"},
	ConfigShiftModeInvalid:               {"shift mode must be shift or acl", "set SHIFT_MODE to shift or acl"},
	ConfigDevUIDNegative:                 {"dev UID must be non-negative", "set DEV_UID to a non-negative number"},
	ConfigSSHPortRange:                   {"SSH port must be an integer from 1 to 65535", "set SSH_PORT to a port from 1 to 65535"},
	ConfigOwnerEndpointRequired:          {"remote yard context requires an owner endpoint", "set OWNER_ENDPOINT for the remote yard"},
	ConfigOwnerYardNameRequired:          {"remote yard context requires a canonical owner yard name", "set OWNER_YARD_NAME to a canonical yard name"},
	// Producers append the unvalidated value after these texts; boundaries omit it.
	ConfigYardNameInvalid:            {"invalid yard name", "use a lower-case yard name of letters, digits, '-' or '_'"},
	ConfigUnknownYard:                {"unknown yard", "run yard yards to list registered yards"},
	ConfigYardTemplateInvalid:        {"invalid YARD_TEMPLATE", "set YARD_TEMPLATE to a shipped yard template name"},
	ConfigUnknownYardTemplate:        {"unknown YARD_TEMPLATE", "set YARD_TEMPLATE to a shipped yard template name"},
	ConfigProfileNameInvalid:         {"invalid environment profile", "select environment profiles by their shipped names"},
	ConfigUnknownIntegration:         {"unknown integration %q", "select coding tool integrations by their shipped names"},
	ConfigDuplicateIntegration:       {"duplicate agent %q", "list each coding tool integration once"},
	ConfigConflictingCommandSettings: {"conflicting command environment settings %s and %s", "pass only the canonical setting in the command environment"},
}

// ConfigName is a profile, setting or yard-kind name that passed validation. Its
// zero value is not a name; values are built only by the constructors below.
type ConfigName struct{ value string }

// ConfigProfileName accepts only a safe environment profile name.
func ConfigProfileName(value string) (ConfigName, bool) { return configName(value, SafeName(value)) }

// ConfigSettingName accepts only a shell variable name, as catalog settings are.
func ConfigSettingName(value string) (ConfigName, bool) {
	valid := value != "" && len(value) <= 128 && !(value[0] >= '0' && value[0] <= '9') &&
		strings.Trim(value, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_") == ""
	return configName(value, valid)
}

// ConfigIntegrationName accepts only a safe coding tool integration name.
func ConfigIntegrationName(value string) (ConfigName, bool) {
	return configName(value, SafeName(value))
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

// SettingRefusal types a refusal over setting names. An invalid name keeps the
// same text unclassified, so a boundary never renders an unvalidated value.
func SettingRefusal(code ConfigRefusalCode, settings ...string) error {
	names := make([]ConfigName, len(settings))
	for index, setting := range settings {
		name, ok := ConfigSettingName(setting)
		if !ok {
			return UnclassifiedConfigRefusal(code, settings...)
		}
		names[index] = name
	}
	return NewConfigRefusal(code, names...)
}

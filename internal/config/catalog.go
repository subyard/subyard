package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/Subyard/Subyard/internal/domain"
)

type SettingKind string

const (
	SettingScalar SettingKind = "scalar"
	SettingFile   SettingKind = "file"
)

type SettingValueType string

const (
	SettingString          SettingValueType = "string"
	SettingBoolean         SettingValueType = "boolean"
	SettingInteger         SettingValueType = "integer"
	SettingPort            SettingValueType = "port"
	SettingSize            SettingValueType = "size"
	SettingName            SettingValueType = "name"
	SettingNameList        SettingValueType = "name-list"
	SettingAbsolutePath    SettingValueType = "absolute-path"
	SettingRelativePath    SettingValueType = "relative-path"
	SettingImageReference  SettingValueType = "image-reference"
	SettingMultiline       SettingValueType = "multiline"
	SettingMountList       SettingValueType = "mount-list"
	SettingLinkList        SettingValueType = "link-list"
	SettingExecutable      SettingValueType = "executable"
	SettingRegularFilePath SettingValueType = "regular-file"
	SettingSHA256          SettingValueType = "sha256"
	SettingVersion         SettingValueType = "version"
)

type SettingScope string

const (
	ScopeShipped SettingScope = "shipped"
	ScopeShared  SettingScope = "shared"
	ScopeHost    SettingScope = "host"
	ScopeYard    SettingScope = "yard"
	ScopeCommand SettingScope = "command"
)

type SettingDefinition struct {
	Name        string
	Kind        SettingKind
	Type        SettingValueType
	Aliases     []string
	Scopes      []SettingScope
	Syncable    bool
	Sensitive   bool
	Merge       string
	Application SettingApplication
	Owner       string
	Enum        []string
	Minimum     int
	Maximum     int
	Optional    bool
	Default     string
	HasDefault  bool
}

var catalog = map[string]SettingDefinition{
	"ADB_CONSOLE_EMULATOR_PORT": scalar("port", SettingPort, SettingNextCommand, true,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand), optionalRange(1, 65535)),
	"ADB_CONSOLE_PROXY_PORT": scalar("port", SettingPort, SettingNextCommand, true,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand), optionalRange(1, 65535)),
	"ADB_EMULATOR_PORT": scalar("port", SettingPort, SettingNextCommand, true,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand), numberRange(1, 65535)),
	"ADB_PROXY_PORT": scalar("port", SettingPort, SettingNextCommand, true,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand), numberRange(1, 65535)),
	"AGENTS": scalar("agent-integration", SettingNameList, SettingYardInit, true,
		scopes(ScopeShipped, ScopeShared, ScopeHost, ScopeYard, ScopeCommand)),
	"BASE_IMAGE": scalar("yard-runtime", SettingImageReference, SettingYardInit, true,
		scopes(ScopeShipped, ScopeShared, ScopeHost, ScopeYard, ScopeCommand)),
	"BASE_IMAGE_FALLBACK": scalar("yard-runtime", SettingImageReference, SettingYardInit, true,
		scopes(ScopeShipped, ScopeShared, ScopeHost, ScopeYard, ScopeCommand)),
	"CCUSAGE_PROVISION": scalar("agent-integration", SettingRegularFilePath, SettingYardInit, false,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand)),
	"CCUSAGE_SHA256_AMD64": scalar("agent-integration", SettingSHA256, SettingYardInit, true,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand)),
	"CCUSAGE_SHA256_ARM64": scalar("agent-integration", SettingSHA256, SettingYardInit, true,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand)),
	"CCUSAGE_VERSION": scalar("agent-integration", SettingVersion, SettingYardInit, true,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand)),
	"CODEX_SHA256_AMD64": scalar("agent-integration", SettingSHA256, SettingYardInit, true,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand)),
	"CODEX_SHA256_ARM64": scalar("agent-integration", SettingSHA256, SettingYardInit, true,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand)),
	"CODEX_VERSION": scalar("agent-integration", SettingVersion, SettingYardInit, true,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand)),
	"DEV_SUDO": scalar("yard-security", SettingBoolean, SettingYardInit, true,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand)),
	"DEV_UID": scalar("yard-runtime", SettingInteger, SettingYardInit, true,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand), numberRange(0, 1<<31-1)),
	"DEV_USER": scalar("yard-runtime", SettingName, SettingYardInit, true,
		scopes(ScopeShipped, ScopeShared, ScopeHost, ScopeYard, ScopeCommand)),
	"E2E_VM_BOOT_TIMEOUT": scalar("test-vms", SettingInteger, SettingNextCommand, true,
		scopes(ScopeShipped, ScopeShared, ScopeHost, ScopeYard, ScopeCommand), numberRange(30, 1800)),
	"E2E_VM_CPU": scalar("test-vms", SettingInteger, SettingNextCommand, true,
		scopes(ScopeShipped, ScopeShared, ScopeHost, ScopeYard, ScopeCommand), numberRange(1, 1<<20-1)),
	"E2E_VM_DISK": scalar("test-vms", SettingSize, SettingNextCommand, true,
		scopes(ScopeShipped, ScopeShared, ScopeHost, ScopeYard, ScopeCommand)),
	"E2E_VM_IMAGE": scalar("test-vms", SettingImageReference, SettingNextCommand, true,
		scopes(ScopeShipped, ScopeShared, ScopeHost, ScopeYard, ScopeCommand)),
	"E2E_VM_MEMORY": scalar("test-vms", SettingSize, SettingNextCommand, true,
		scopes(ScopeShipped, ScopeShared, ScopeHost, ScopeYard, ScopeCommand)),
	"E2E_VM_SLOT_COUNT": scalar("test-vms", SettingInteger, SettingYardInit, true,
		scopes(ScopeShipped, ScopeShared, ScopeHost, ScopeYard, ScopeCommand), numberRange(1, 1<<20-1)),
	"FORWARD_SSH_AGENT": scalar("yard-security", SettingBoolean, SettingYardInit, true,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand)),
	"HOST_BASE": scalar("host-storage", SettingAbsolutePath, SettingNextCommand, false,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand)),
	"HOST_CLAUDE_MD": scalar("host-files", SettingAbsolutePath, SettingYardInit, true,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand), optional()),
	"HOST_CODEX_AGENTS_MD": scalar("host-files", SettingAbsolutePath, SettingYardInit, true,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand), optional()),
	"HOST_LINKS": scalar("yard-storage", SettingLinkList, SettingYardInit, true,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand), optional()),
	"HOST_MOUNTS": scalar("host-storage", SettingMountList, SettingYardInit, true,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand), optional()),
	"HOST_OPENCODE_AGENTS_MD": scalar("host-files", SettingAbsolutePath, SettingYardInit, true,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand), optional()),
	"INCUS_BRIDGE": scalar("host-network", SettingName, SettingYardInit, false,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand)),
	"INCUS_PROJECT": scalar("yard-identity", SettingName, SettingNextCommand, false,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand)),
	"INSTANCE_NAME": scalar("yard-identity", SettingName, SettingNextCommand, false,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand)),
	"INSTANCE_TYPE": scalar("yard-security", SettingString, SettingYardInit, true,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand), enum("container", "vm")),
	"LIMITS_CPU": scalar("yard-resources", SettingInteger, SettingYardInit, true,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand), optionalRange(1, 1<<20-1)),
	"LIMITS_MEMORY": scalar("yard-resources", SettingSize, SettingYardInit, true,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand), optional()),
	"NESTED_E2E_VMS": scalar("yard-security", SettingBoolean, SettingYardInit, true,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand)),
	"ORCA_ADVERTISE_HOST": scalar("orca-resource", SettingString, SettingNextCommand, true,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand), optional()),
	"ORCA_HOST_PORT": scalar("orca-resource", SettingPort, SettingNextCommand, true,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand), optionalRange(1, 65535)),
	"REMOTE_DEST": scalar("remote-connection", SettingString, SettingNextCommand, false,
		scopes(ScopeHost, ScopeYard, ScopeCommand)),
	"REMOTE_DEV_USER": scalar("remote-connection", SettingName, SettingNextCommand, false,
		scopes(ScopeHost, ScopeYard, ScopeCommand), optional()),
	"REMOTE_SSH_PORT": scalar("remote-connection", SettingPort, SettingNextCommand, false,
		scopes(ScopeHost, ScopeYard, ScopeCommand), optionalRange(1, 65535)),
	"REMOTE_YARD": scalar("remote-connection", SettingName, SettingNextCommand, false,
		scopes(ScopeHost, ScopeYard, ScopeCommand), optional()),
	"RESTRICTED_DISK_PATHS": scalar("host-security", SettingAbsolutePath, SettingYardInit, false,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand)),
	"SHIFT_MODE": scalar("host-storage", SettingString, SettingYardInit, true,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand), enum("shift", "acl")),
	"SRV_POOL": scalar("host-storage", SettingName, SettingYardInit, true,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand)),
	"SRV_VOLUME": scalar("yard-storage", SettingName, SettingYardInit, false,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand)),
	"SSH_HOST": scalar("yard-identity", SettingName, SettingNextCommand, false,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand)),
	"SSH_PORT": scalar("host-network", SettingPort, SettingNextCommand, true,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand), numberRange(1, 65535)),
	"STORAGE_PATH": scalar("host-storage", SettingAbsolutePath, SettingYardInit, false,
		scopes(ScopeShipped, ScopeHost, ScopeCommand)),
	"SUBYARD_AGE_SHA256_AMD64": scalar("credential-tools", SettingSHA256, SettingYardInit, true,
		scopes(ScopeShipped, ScopeHost, ScopeCommand)),
	"SUBYARD_AGE_SHA256_ARM64": scalar("credential-tools", SettingSHA256, SettingYardInit, true,
		scopes(ScopeShipped, ScopeHost, ScopeCommand)),
	"SUBYARD_AGE_VERSION": scalar("credential-tools", SettingVersion, SettingYardInit, true,
		scopes(ScopeShipped, ScopeHost, ScopeCommand)),
	"SUBYARD_KEYS_CONSUMER_ROOT": scalar("credential-materialization", SettingAbsolutePath, SettingNextCommand, false,
		scopes(ScopeHost, ScopeYard, ScopeCommand)),
	"SUBYARD_KEYS_ROOT": scalar("credential-ledger", SettingAbsolutePath, SettingNextCommand, false,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand)),
	"SUBYARD_KEYS_SYSTEMD_DIR": scalar("credential-ledger", SettingAbsolutePath, SettingYardInit, false,
		scopes(ScopeShipped, ScopeCommand)),
	"SUBYARD_KEYS_TOOLS_DIR": scalar("credential-tools", SettingAbsolutePath, SettingYardInit, false,
		scopes(ScopeShipped, ScopeCommand)),
	"SUBYARD_POWER_LIBEXEC_DIR": scalar("boot-power", SettingAbsolutePath, SettingYardInit, false,
		scopes(ScopeShipped, ScopeCommand)),
	"SUBYARD_POWER_RECONCILER_PATH": scalar("boot-power", SettingAbsolutePath, SettingYardInit, false,
		scopes(ScopeShipped, ScopeCommand)),
	"SUBYARD_POWER_UNIT_PATH": scalar("boot-power", SettingAbsolutePath, SettingYardInit, false,
		scopes(ScopeShipped, ScopeCommand)),
	"SUBYARD_SOPS_SHA256_AMD64": scalar("credential-tools", SettingSHA256, SettingYardInit, true,
		scopes(ScopeShipped, ScopeHost, ScopeCommand)),
	"SUBYARD_SOPS_SHA256_ARM64": scalar("credential-tools", SettingSHA256, SettingYardInit, true,
		scopes(ScopeShipped, ScopeHost, ScopeCommand)),
	"SUBYARD_SOPS_VERSION": scalar("credential-tools", SettingVersion, SettingYardInit, true,
		scopes(ScopeShipped, ScopeHost, ScopeCommand)),
	"YARD_CAPABILITIES": scalar("environment-profile", SettingNameList, SettingYardInit, true,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand), optional()),
	"YARD_CAPS": scalar("environment-profile", SettingNameList, SettingYardInit, true,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand), optional()),
	"YARD_DEVICES": scalar("environment-profile", SettingNameList, SettingYardInit, true,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand), optional()),
	"YARD_MOUNTS": scalar("environment-profile", SettingMountList, SettingYardInit, true,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand), optional()),
	"YARD_PROFILES": scalar("environment-profile", SettingNameList, SettingYardInit, true,
		scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand), optional()),
	"YARD_TEMPLATE": scalar("environment-profile", SettingName, SettingYardInit, true,
		scopes(ScopeYard, ScopeCommand), optional()),
	"YARD_TYPE": scalar("remote-connection", SettingString, SettingNextCommand, false,
		scopes(ScopeHost, ScopeYard, ScopeCommand), enum("local", "remote")),
}

type definitionOption func(*SettingDefinition)

func scalar(
	owner string,
	valueType SettingValueType,
	application SettingApplication,
	syncable bool,
	allowed []SettingScope,
	options ...definitionOption,
) SettingDefinition {
	definition := SettingDefinition{
		Kind: SettingScalar, Type: valueType, Application: application,
		Syncable: syncable, Merge: "replace", Owner: owner, Scopes: allowed,
	}
	for _, apply := range options {
		apply(&definition)
	}
	return definition
}

func scopes(values ...SettingScope) []SettingScope {
	return values
}

func optional() definitionOption {
	return func(definition *SettingDefinition) {
		definition.Optional = true
	}
}

func numberRange(minimum, maximum int) definitionOption {
	return func(definition *SettingDefinition) {
		definition.Minimum = minimum
		definition.Maximum = maximum
	}
}

func optionalRange(minimum, maximum int) definitionOption {
	return func(definition *SettingDefinition) {
		definition.Minimum = minimum
		definition.Maximum = maximum
		definition.Optional = true
	}
}

func enum(values ...string) definitionOption {
	return func(definition *SettingDefinition) {
		definition.Enum = append([]string(nil), values...)
	}
}

func LookupSetting(name string) (SettingDefinition, bool) {
	if definition, ok := catalog[name]; ok {
		definition.Name = name
		return definition, true
	}
	definition, ok := agentSettingDefinition(name)
	if ok {
		definition.Name = name
	}
	return definition, ok
}

func SettingCatalog() []SettingDefinition {
	result := make([]SettingDefinition, 0, len(catalog))
	for name, definition := range catalog {
		definition.Name = name
		result = append(result, definition)
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].Name < result[right].Name
	})
	return result
}

func agentSettingDefinition(name string) (SettingDefinition, bool) {
	if !strings.HasPrefix(name, "AGENT_") {
		return SettingDefinition{}, false
	}
	for _, suffix := range []string{
		"_CONFIG_DEST", "_RULES_DEST", "_COMMAND", "_CONFIG", "_PERSIST",
		"_PROJECTS_CHANGED", "_PROVISION", "_RULES", "_CHECK",
	} {
		agent, found := strings.CutSuffix(strings.TrimPrefix(name, "AGENT_"), suffix)
		if !found || !domain.SafeName(agent) {
			continue
		}
		switch suffix {
		case "_CONFIG", "_RULES":
			return SettingDefinition{
				Kind: SettingFile, Type: SettingRegularFilePath,
				Scopes:   scopes(ScopeShipped, ScopeShared, ScopeHost, ScopeYard, ScopeCommand),
				Syncable: true, Merge: "replace", Application: SettingConfigApply, Owner: "agent-integration",
			}, true
		case "_CONFIG_DEST", "_RULES_DEST":
			return SettingDefinition{
				Kind: SettingScalar, Type: SettingRelativePath,
				Scopes:   scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand),
				Syncable: false, Merge: "replace", Application: SettingConfigApply, Owner: "agent-integration",
			}, true
		case "_PROVISION":
			return SettingDefinition{
				Kind: SettingFile, Type: SettingRegularFilePath,
				Scopes:   scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand),
				Syncable: false, Merge: "replace", Application: SettingYardInit, Owner: "agent-integration",
			}, true
		case "_COMMAND", "_CHECK", "_PROJECTS_CHANGED":
			return SettingDefinition{
				Kind: SettingScalar, Type: SettingExecutable,
				Scopes:   scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand),
				Syncable: false, Merge: "replace", Application: SettingYardInit, Owner: "agent-integration",
			}, true
		case "_DEPENDS":
			return SettingDefinition{
				Kind: SettingScalar, Type: SettingNameList,
				Scopes:   scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand),
				Syncable: false, Merge: "replace", Application: SettingYardInit, Owner: "agent-integration",
			}, true
		case "_PERSIST":
			return SettingDefinition{
				Kind: SettingScalar, Type: SettingMultiline,
				Scopes:   scopes(ScopeShipped, ScopeHost, ScopeYard, ScopeCommand),
				Syncable: false, Merge: "replace", Application: SettingYardInit, Owner: "agent-integration",
			}, true
		}
	}
	return SettingDefinition{}, false
}

func ValidateSetting(scope SettingScope, name, value string, requireSyncable bool) error {
	definition, err := ValidateSettingName(scope, name, requireSyncable)
	if err != nil {
		return err
	}
	if definition.Sensitive {
		return fmt.Errorf("setting %s appears to contain secret material; use the secret or credential store", name)
	}
	if err := ValidateNonSecretContent(name, value); err != nil {
		return err
	}
	if err := validateSettingValue(definition, value); err != nil {
		return fmt.Errorf("setting %s: %w", name, err)
	}
	return nil
}

func ValidateNonSecretContent(name, value string) error {
	if secretLike(name, value) {
		return fmt.Errorf(
			"setting %s appears to contain secret material; use the secret or credential store",
			name,
		)
	}
	return nil
}

func ValidateSettingName(
	scope SettingScope,
	name string,
	requireSyncable bool,
) (SettingDefinition, error) {
	definition, ok := LookupSetting(name)
	if !ok {
		return SettingDefinition{}, fmt.Errorf("unknown setting %q", name)
	}
	if !definition.allows(scope) {
		return SettingDefinition{}, fmt.Errorf("setting %s is not allowed in %s scope", name, scope)
	}
	if requireSyncable && !definition.Syncable {
		return SettingDefinition{}, fmt.Errorf(
			"setting %s is local-only and cannot be imported from versioned configuration", name,
		)
	}
	return definition, nil
}

func (definition SettingDefinition) allows(scope SettingScope) bool {
	for _, allowed := range definition.Scopes {
		if allowed == scope {
			return true
		}
	}
	return false
}

func validateSettingValue(definition SettingDefinition, value string) error {
	if value == "" && definition.Optional {
		return nil
	}
	if strings.ContainsRune(value, '\x00') {
		return errors.New("contains a NUL byte")
	}
	for _, allowed := range definition.Enum {
		if value == allowed {
			return nil
		}
	}
	if len(definition.Enum) != 0 {
		return fmt.Errorf("must be one of %s", strings.Join(definition.Enum, ", "))
	}
	switch definition.Type {
	case SettingString, SettingMultiline:
		if controlCharacter(value, definition.Type == SettingMultiline) {
			return errors.New("contains unsafe control characters")
		}
	case SettingMountList:
		if err := validateMountList(value); err != nil {
			return err
		}
	case SettingLinkList:
		if err := validateLinkList(value); err != nil {
			return err
		}
	case SettingBoolean:
		if value != "0" && value != "1" {
			return errors.New("must be 0 or 1")
		}
	case SettingInteger, SettingPort:
		if definition.Name == "E2E_VM_CPU" && value == "auto" {
			return nil
		}
		number, err := strconv.Atoi(value)
		if err != nil {
			return errors.New("must be an integer")
		}
		if number < definition.Minimum ||
			(definition.Maximum != 0 && number > definition.Maximum) {
			return fmt.Errorf("must be in range %d..%d", definition.Minimum, definition.Maximum)
		}
	case SettingSize:
		if err := validateSize(value); err != nil {
			return err
		}
	case SettingName:
		if !domain.SafeName(value) {
			return errors.New("must be a safe lowercase name")
		}
	case SettingNameList:
		for _, name := range strings.Fields(value) {
			if !domain.SafeName(name) {
				return fmt.Errorf("contains unsafe name %q", name)
			}
		}
	case SettingAbsolutePath:
		if !filepath.IsAbs(value) {
			return errors.New("must be an absolute path")
		}
	case SettingRelativePath:
		clean := filepath.Clean(value)
		if value == "" || filepath.IsAbs(value) || clean == ".." ||
			strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return errors.New("must be a safe relative path")
		}
	case SettingImageReference:
		if value == "" || strings.HasPrefix(value, "-") ||
			strings.ContainsFunc(value, func(char rune) bool {
				return !strings.ContainsRune(
					"abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._:/@+-", char,
				)
			}) {
			return errors.New("contains unsafe image-reference characters")
		}
	case SettingExecutable:
		if value == "" || strings.ContainsAny(value, "\x00\n\r\t ") {
			return errors.New("must be one executable name or path")
		}
	case SettingRegularFilePath:
		if value == "" || !filepath.IsAbs(value) {
			return errors.New("must be an absolute path")
		}
	case SettingSHA256:
		if len(value) != 64 || strings.ContainsFunc(value, func(char rune) bool {
			return (char < '0' || char > '9') && (char < 'a' || char > 'f')
		}) {
			return errors.New("must be a lowercase SHA-256 digest")
		}
	case SettingVersion:
		if value == "" || strings.HasPrefix(value, "-") ||
			strings.ContainsFunc(value, func(char rune) bool {
				return (char < '0' || char > '9') && char != '.' && char != '-' &&
					(char < 'a' || char > 'z')
			}) {
			return errors.New("must be a safe version")
		}
	default:
		return fmt.Errorf("has unsupported catalog type %q", definition.Type)
	}
	return nil
}

func validateSize(value string) error {
	raw := strings.TrimSuffix(value, "MiB")
	if raw == value {
		raw = strings.TrimSuffix(value, "GiB")
		if raw == value {
			return errors.New("must be a positive MiB or GiB value")
		}
	}
	number, err := strconv.Atoi(raw)
	if err != nil || number < 1 {
		return errors.New("must be a positive MiB or GiB value")
	}
	return nil
}

func validateMountList(value string) error {
	for _, entry := range strings.Fields(value) {
		parts := strings.Split(entry, ":")
		if len(parts) != 4 || !safeRelativeValue(parts[0]) ||
			!filepath.IsAbs(parts[1]) || filepath.Clean(parts[1]) != parts[1] ||
			(parts[2] != "ro" && parts[2] != "rw") {
			return fmt.Errorf("contains invalid mount entry %q", entry)
		}
		mode, err := strconv.ParseUint(parts[3], 8, 12)
		if err != nil || mode > 0o777 {
			return fmt.Errorf("contains invalid mount mode in %q", entry)
		}
	}
	return nil
}

func validateLinkList(value string) error {
	for _, entry := range strings.Fields(value) {
		parts := strings.Split(entry, ":")
		if (len(parts) != 2 && len(parts) != 3) ||
			!safeRelativeValue(parts[0]) || !filepath.IsAbs(parts[1]) ||
			filepath.Clean(parts[1]) != parts[1] ||
			(len(parts) == 3 && parts[2] != "file") {
			return fmt.Errorf("contains invalid persistent-link entry %q", entry)
		}
	}
	return nil
}

func safeRelativeValue(value string) bool {
	clean := filepath.Clean(value)
	return value != "" && !filepath.IsAbs(value) && clean == value &&
		clean != "." && clean != ".." &&
		!strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

func controlCharacter(value string, allowNewline bool) bool {
	return strings.ContainsFunc(value, func(char rune) bool {
		if allowNewline && char == '\n' {
			return false
		}
		return unicode.IsControl(char)
	})
}

func secretLike(name, value string) bool {
	upperName := strings.ToUpper(name)
	for _, marker := range []string{"PASSWORD", "PASSPHRASE", "PRIVATE_KEY", "ACCESS_TOKEN", "API_KEY", "SECRET"} {
		if strings.Contains(upperName, marker) {
			return true
		}
	}
	lower := strings.ToLower(value)
	for _, marker := range []string{
		"-----begin private key-----",
		"-----begin openssh private key-----",
		"password=",
		`"password":`,
		"passwd=",
		"access_token=",
		`"access_token":`,
		"api_key=",
		`"api_key":`,
		`"apikey":`,
		"client_secret=",
		`"client_secret":`,
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

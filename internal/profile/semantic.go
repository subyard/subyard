package profile

import (
	"errors"
	"fmt"
	"github.com/Subyard/Subyard/internal/domain"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

func relative(path string) bool {
	return path != "" && path != "." && !filepath.IsAbs(path) && filepath.Clean(path) == path &&
		path != ".." && !strings.HasPrefix(path, "../") && !strings.ContainsAny(path, "\\\r\n\t")
}
func textSafe(value string) bool {
	return len(value) <= 4096 && !strings.ContainsAny(value, "\x00\x1b\r\n")
}
func (definition *Definition) validateSemantic() error {
	if definition.SchemaVersion != 1 || !domain.SafeName(definition.Name) {
		return errors.New("unsupported profile schema or name")
	}
	if definition.InstancePublication != nil {
		if err := definition.InstancePublication.validate(definition.Name); err != nil {
			return err
		}
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

	}
	if definition.LifecycleOnly && (definition.FullCleanup == "" || len(definition.DefaultYards) > 0 || definition.OwnerService != "" || definition.Setup != nil || definition.Runtime != nil || len(definition.Settings) > 0 || len(definition.Native) > 0 || len(definition.Consumers) > 0 || definition.GuestEnvironment != nil || definition.Dashboard != nil) {
		return errors.New("lifecycle-only profiles may declare only full cleanup hooks, managed paths and native publication metadata")
	}
	if definition.FullCleanup != "" && !relative(definition.FullCleanup) {
		return errors.New("full cleanup path escapes profile")
	}
	if len(definition.FullCleanupEnvironment) > 32 || len(definition.FullCleanupEnvironment) > 0 && definition.FullCleanup == "" {
		return errors.New("invalid full cleanup environment declaration")
	}
	cleanupNames := map[string]bool{}
	for _, name := range definition.FullCleanupEnvironment {
		if name == "" || cleanupNames[name] || strings.IndexFunc(name, func(r rune) bool { return !(r == '_' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') }) >= 0 {
			return errors.New("invalid full cleanup environment name")
		}
		cleanupNames[name] = true
	}
	nativePaths := map[string]bool{}
	for _, native := range definition.Native {
		if !relative(native.Path) || !relative(native.Package) || nativePaths[native.Path] {
			return errors.New("invalid or duplicate native artifact path")
		}
		nativePaths[native.Path] = true
	}
	for _, consumer := range definition.Consumers {
		path := strings.ReplaceAll(consumer.Path, "{zone}", "zone")
		if !domain.SafeName(consumer.ID) || consumer.ID == "none" ||
			consumer.Zone != "*" && !domain.SafeName(consumer.Zone) || !relative(path) || strings.ContainsAny(path, "{}") {
			return errors.New("invalid profile credential declaration")
		}
		if strings.Contains(consumer.Path, "{zone}") && (consumer.Zone != "*" || strings.Count(consumer.Path, "{zone}") != 1 || strings.Contains(filepath.Dir(consumer.Path), "{zone}")) {
			return errors.New("credential zone placeholder requires a wildcard zone and a filename")
		}
		if consumer.Format != "file" && consumer.Format != "rsa-private-key" && consumer.Format != "rsa-private-key-with-settings" {
			return errors.New("unsupported profile credential format")
		}
		if consumer.Format == "rsa-private-key-with-settings" && (definition.Setup == nil || !definition.Setup.SyncFields || definition.Setup.Consumer != consumer.ID) {
			return errors.New("credential settings require a matching synchronized setup")
		}
		if consumer.LegacyPath != "" && (consumer.Format != "rsa-private-key-with-settings" || !relative(consumer.LegacyPath) || strings.ContainsAny(consumer.LegacyPath, "{}") || consumer.LegacyPath == consumer.Path) {
			return errors.New("invalid legacy credential path")
		}
		if consumer.StopHandler != "" {
			if err := validateExecutableName(consumer.StopHandler); err != nil {
				return fmt.Errorf("credential stop handler: %w", err)
			}
		}
	}
	if len(definition.CredentialImportExclusions) > 32 {
		return errors.New("too many profile credential import exclusions")
	}
	for _, fragments := range definition.CredentialImportExclusions {
		if len(fragments) == 0 || len(fragments) > 8 {
			return errors.New("credential import exclusion requires bounded directory fragments")
		}
		for _, fragment := range fragments {
			if len(fragment) < 3 || len(fragment) > 256 || !strings.HasPrefix(fragment, "/") || !strings.HasSuffix(fragment, "/") {
				return errors.New("credential import exclusion requires slash-delimited directory fragments")
			}
			for _, part := range strings.Split(fragment[1:len(fragment)-1], "/") {
				if !domain.SafeID(part) {
					return errors.New("invalid credential import exclusion directory fragment")
				}
			}
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
			if setup.SyncFields && consumer.ID == setup.Consumer && consumer.Format != "rsa-private-key-with-settings" {
				return errors.New("synchronized setup requires a credential settings consumer")
			}
		}
		if !found {
			return errors.New("setup references undeclared credential consumer")
		}
	}
	if err := definition.validateExtensions(); err != nil {
		return err
	}
	return nil
}

func (definition *IntegrationDefinition) validateSemantic() error {
	if (definition.SchemaVersion != 1 && definition.SchemaVersion != 2) || !domain.SafeName(definition.Name) {
		return errors.New("unsupported integration schema or name")
	}
	if err := definition.validateArtifacts(); err != nil {
		return err
	}
	if definition.Retired {
		if definition.Lifecycle == nil || definition.Provision != "" || definition.Cleanup != "" || definition.Persist != "" || len(definition.Depends) != 0 || definition.Command != "" || definition.Check != "" || definition.Health != "" || len(definition.Native) != 0 || len(definition.Inputs) != 0 || len(definition.Settings) != 0 || definition.Dashboard != nil {
			return errors.New("retired integration permits only cleanup lifecycle metadata")
		}
		for _, name := range definition.RetiredSettings {
			if !settingNamePattern.MatchString(name) {
				return errors.New("invalid retired setting name")
			}
		}
	} else if len(definition.RetiredSettings) != 0 || len(definition.RetiredRoutes) != 0 {
		return errors.New("retired settings require retired integration")
	}
	packaged := Definition{Name: definition.Name, SchemaVersion: 1, Settings: definition.Settings}
	if err := packaged.validateExtensions(); err != nil {
		return err
	}
	nativePaths := map[string]bool{}
	for _, native := range definition.Native {
		if !relative(native.Package) || !relative(native.Path) || nativePaths[native.Path] {
			return errors.New("invalid or duplicate integration native artifact path")
		}
		nativePaths[native.Path] = true
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
			if err := validateExecutableName(path); err != nil {
				return err
			}
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
		if err := validateExecutableName(lifecycle.Handler); err != nil {
			return err
		}
		for _, path := range lifecycle.SourceFiles {
			if err := validateExecutableName(path); err != nil {
				return err
			}
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
	for _, route := range definition.RetiredRoutes {
		if definition.Lifecycle == nil || route.GuestPort < 1 || route.GuestPort > 65535 ||
			!slices.Contains(definition.Lifecycle.InstanceConfigKeys, route.MarkerKey) || !slices.Contains(definition.Lifecycle.InstanceDevices, route.DeviceName) {
			return errors.New("retired route is outside cleanup ownership")
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

var settingNamePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

var allowedScopes = map[string]bool{
	"shipped": true, "shared": true, "host": true, "yard": true, "command": true,
}

func (definition *Definition) validateExtensions() error {
	if dashboard := definition.Dashboard; dashboard != nil {
		if definition.OwnerService == "" || definition.Runtime == nil || dashboard.ContainerRoute == nil ||
			(dashboard.Scheme != "http" && dashboard.Scheme != "https") || !strings.HasPrefix(dashboard.Path, "/") || !textSafe(dashboard.Path) ||
			dashboard.GuestPort < 1 || dashboard.GuestPort > 65535 || !settingNamePattern.MatchString(dashboard.PortSetting) ||
			!strings.HasPrefix(dashboard.ContainerRoute.MarkerKey, "user.subyard.") || !textSafe(dashboard.ContainerRoute.MarkerKey) || !domain.SafeName(dashboard.ContainerRoute.DeviceName) {
			return errors.New("invalid profile dashboard ownership")
		}
		owned := false
		for _, field := range definition.Settings {
			owned = owned || field.Name == dashboard.PortSetting && field.Type == "port" && field.HostListener
		}
		if !owned {
			return errors.New("profile dashboard requires its owned listener port")
		}
	}
	seen := map[string]bool{}
	for _, setting := range definition.Settings {
		if !settingNamePattern.MatchString(setting.Name) || seen[setting.Name] {
			return errors.New("invalid or duplicate profile setting name")
		}
		seen[setting.Name] = true
		if setting.Application != "next-command" && setting.Application != "yard-init" && setting.Application != "config-apply" {
			return errors.New("invalid profile setting application")
		}
		if len(setting.Scopes) == 0 {
			return errors.New("profile setting requires scopes")
		}
		scopes := map[string]bool{}
		for _, scope := range setting.Scopes {
			if !allowedScopes[scope] || scopes[scope] {
				return errors.New("invalid or duplicate profile setting scope")
			}
			scopes[scope] = true
		}
		if setting.Type != "string" && setting.Type != "boolean" && setting.Type != "integer" && setting.Type != "port" && setting.Type != "size" && setting.Type != "name" && setting.Type != "version" && setting.Type != "image-reference" && setting.Type != "sha256" && setting.Type != "absolute-path" {
			return errors.New("unsupported profile setting type")
		}
		if len(setting.Enum) != 0 {
			enums := map[string]bool{}
			for _, value := range setting.Enum {
				if value == "" || !textSafe(value) || enums[value] {
					return errors.New("invalid or duplicate profile setting enum value")
				}
				enums[value] = true
			}
		}
		if setting.Minimum < 0 || setting.Maximum < 0 || setting.Minimum > 0 && setting.Maximum > 0 && setting.Minimum > setting.Maximum {
			return errors.New("invalid profile setting bounds")
		}
		if (setting.Minimum != 0 || setting.Maximum != 0) && setting.Type != "integer" && setting.Type != "port" {
			return errors.New("bounds require numeric profile setting type")
		}
		if setting.Type == "port" && (setting.Minimum < 1 || setting.Maximum < 1 || setting.Maximum > 65535 || setting.Minimum > setting.Maximum) {
			return errors.New("port profile setting requires bounds within 1..65535")
		}
		if setting.HostListener && setting.Type != "port" {
			return errors.New("host listener profile setting must be a port")
		}
		if setting.DefaultFrom != nil && (setting.Type != "port" || setting.Default != nil || setting.DefaultFrom.Offset < -65535 || setting.DefaultFrom.Offset > 65535 || !settingNamePattern.MatchString(setting.DefaultFrom.Setting)) {
			return errors.New("invalid derived port default")
		}
		if setting.Default != nil {
			if err := setting.validateDefault(*setting.Default); err != nil {
				return err
			}
		}
	}
	if definition.Runtime != nil {
		if !safeIdentifier(definition.Runtime.ActivationID) {
			return errors.New("invalid runtime activation ID")
		}
		if err := validateExecutableName(definition.Runtime.Handler); err != nil {
			return fmt.Errorf("runtime handler: %w", err)
		}
		seen := map[string]bool{}
		for _, path := range definition.Runtime.ProjectsChangedHooks {
			if !strings.HasPrefix(path, "/usr/local/libexec/subyard/projects-changed.d/") || filepath.Clean(path) != path || filepath.Dir(path) != "/usr/local/libexec/subyard/projects-changed.d" || strings.ContainsAny(path, "\r\n\x00") || seen[path] {
				return errors.New("invalid runtime project hook path")
			}
			seen[path] = true
		}
	}
	if definition.GuestEnvironment != nil {
		if err := validateExecutableName(definition.GuestEnvironment.Handler); err != nil {
			return fmt.Errorf("guest environment handler: %w", err)
		}
	}
	return nil
}

func safeIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '-' && r != '_' {
			return false
		}
	}
	return value[0] >= 'a' && value[0] <= 'z'
}

func (setting Setting) validateDefault(value string) error {
	if value == "" && setting.Optional {
		return nil
	}
	if !textSafe(value) {
		return errors.New("invalid profile setting default")
	}
	valid := true
	switch setting.Type {
	case "boolean":
		valid = value == "0" || value == "1"
	case "integer", "port":
		n, err := strconv.Atoi(value)
		valid = err == nil && n >= 0
		if valid && setting.Minimum > 0 {
			valid = n >= setting.Minimum
		}
		if valid && setting.Maximum > 0 {
			valid = n <= setting.Maximum
		}
		if valid && setting.Type == "port" {
			valid = n >= 1 && n <= 65535
		}
	case "size":
		raw := strings.TrimSuffix(strings.TrimSuffix(value, "MiB"), "GiB")
		n, err := strconv.Atoi(raw)
		valid = raw != value && err == nil && n > 0
	case "name":
		valid = domain.SafeName(value)
	case "absolute-path":
		valid = filepath.IsAbs(value) && filepath.Clean(value) == value
	}
	if len(setting.Enum) != 0 {
		match := false
		for _, candidate := range setting.Enum {
			match = match || value == candidate
		}
		valid = valid && match
	}
	if !valid {
		return errors.New("profile setting default violates its type or bounds")
	}
	return nil
}

func validateExecutableName(path string) error {
	if !relative(path) {
		return errors.New("handler path escapes profile")
	}
	return nil
}

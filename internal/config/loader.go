package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
)

type LoadOptions struct {
	RepositoryRoot          string
	OperatorHome            string
	YardName                string
	YardSettingsFile        string
	Environment             map[string]string
	DisablePrivate          bool
	SyncSource              bool
	ConfigLocked            bool
	AllowPendingTransaction bool
	LayerPaths              *LayerPaths
	YardDirs                []string
}

type LayerPaths struct {
	SharedSettings string
	SharedAssets   string
	HostSettings   string
	HostAssets     string
	YardSettings   map[string]string
	YardAssets     map[string]string
}

type Loaded struct {
	Context             domain.Context
	Environment         map[string]string
	Settings            map[string]SettingTrace
	ConfigurationLayers []ConfigurationLayer
}

type retiredYardTemplateError struct {
	diagnostic string
}

var ErrUnknownYard = errors.New("unknown yard")

func (err *retiredYardTemplateError) Error() string {
	return err.diagnostic
}

func IsRetiredYardTemplate(err error) bool {
	var retired *retiredYardTemplateError
	return errors.As(err, &retired)
}

func Load(options LoadOptions) (Loaded, error) {
	tracker := newSettingTracker()
	ctx, values, err := load(options, tracker)
	if err != nil {
		return Loaded{}, err
	}
	environment := make(map[string]string, len(values))
	for name, value := range values {
		environment[name] = value
	}
	return Loaded{
		Context: ctx, Environment: environment, Settings: tracker.traces(values),
		ConfigurationLayers: tracker.configurationLayers(),
	}, nil
}

func load(
	options LoadOptions,
	tracker *settingTracker,
) (domain.Context, environment, error) {
	root, err := filepath.Abs(options.RepositoryRoot)
	if err != nil {
		return domain.Context{}, nil, fmt.Errorf("resolve repository root: %w", err)
	}
	values := environmentFrom(options.Environment)
	if err := normalizeLegacyEnvironment(values); err != nil {
		return domain.Context{}, nil, err
	}
	if values["SUBYARD_ENGINE_CONTEXT"] == "1" && values["SUBYARD_CONFIG_LOADED"] != "1" {
		resetInheritedContext(values)
	}
	commandEnvironment := cloneEnvironment(values)
	if values["SUBYARD_CONFIG_DIR"] == "" {
		values["SUBYARD_CONFIG_DIR"] = filepath.Join(root, "config")
	}
	configDir := filepath.Clean(values["SUBYARD_CONFIG_DIR"])
	if options.OperatorHome != "" {
		values["SUBYARD_OPERATOR_HOME"] = options.OperatorHome
		commandEnvironment["SUBYARD_OPERATOR_HOME"] = options.OperatorHome
	}
	if values["SUBYARD_OPERATOR_HOME"] == "" {
		return domain.Context{}, nil, errors.New("operator home is required")
	}
	configHome, err := bootstrapConfigHome(values)
	if err != nil {
		return domain.Context{}, nil, err
	}
	values["SUBYARD_CONFIG_HOME"] = configHome
	if !options.ConfigLocked {
		unlock, err := LockRoot(configHome, false)
		if err != nil {
			return domain.Context{}, nil, err
		}
		defer unlock()
	}
	pending, err := PendingConfigurationTransaction(configHome)
	if err != nil {
		return domain.Context{}, nil, err
	}
	if pending && !options.AllowPendingTransaction {
		return domain.Context{}, nil, errors.New(
			"an interrupted configuration transaction requires recovery with yard config sync",
		)
	}
	dataHome := values["SUBYARD_HOME"]
	if dataHome == "" {
		dataHome = filepath.Join(values["SUBYARD_OPERATOR_HOME"], ".subyard")
	}
	values["SUBYARD_HOME"] = filepath.Clean(dataHome)
	setConfigurationRolePaths(values, configHome, "default")

	// Immutable shipped defaults are the lowest-precedence layer.
	defaultLayer := tracker.addLayer(
		"default", "shipped defaults", configDir, pathPresent(configDir), settingAny,
	)
	for _, name := range []string{"incus.project.env", "subyard.env", "host.env", "agents.env", "ports.env"} {
		path := filepath.Join(configDir, name)
		if err := applyOptionalTracked(path, values, tracker, defaultLayer); err != nil {
			return domain.Context{}, nil, err
		}
	}
	sharedSettingsFile := filepath.Join(configHome, "overrides", "shared", "config.env")
	if options.LayerPaths != nil {
		sharedSettingsFile = options.LayerPaths.SharedSettings
	}
	sharedSettingsExist, err := regularFileExists(sharedSettingsFile)
	if err != nil {
		return domain.Context{}, nil, err
	}
	sharedSettingsLayer := tracker.addLayer(
		"shared", "scalar settings", sharedSettingsFile, sharedSettingsExist, settingScalar,
	)
	if sharedSettingsExist {
		if err := applyEnvFileTrackedValidated(
			sharedSettingsFile, values, tracker, sharedSettingsLayer,
			ScopeShared, options.SyncSource,
		); err != nil {
			return domain.Context{}, nil, err
		}
	}
	logicalAssets := agentAssetMappings(values, configDir)
	sharedAssets := filepath.Join(configHome, "overrides", "shared", "agents")
	if options.LayerPaths != nil {
		sharedAssets = options.LayerPaths.SharedAssets
	}
	sharedLayer := tracker.addLayer(
		"shared", "file settings", sharedAssets, pathPresent(sharedAssets), settingFile,
	)
	if err := applyAgentAssetLayer(values, logicalAssets,
		sharedAssets, tracker, sharedLayer); err != nil {
		return domain.Context{}, nil, err
	}

	hostSettingsFile := filepath.Join(configHome, "config.env")
	if options.LayerPaths != nil {
		hostSettingsFile = options.LayerPaths.HostSettings
	}
	hostSettingsExist, err := regularFileExists(hostSettingsFile)
	if err != nil {
		return domain.Context{}, nil, err
	}
	hostSettingsPath := hostSettingsFile
	hostSettingsPresent := hostSettingsExist
	if !hostSettingsExist && !options.DisablePrivate && options.LayerPaths == nil {
		privateConfig := filepath.Join(configDir, "..", "private", "config.env")
		if pathPresent(privateConfig) {
			hostSettingsPath = privateConfig
			hostSettingsPresent = true
		}
	}
	hostSettingsLayer := tracker.addLayer(
		"host", "scalar settings", hostSettingsPath, hostSettingsPresent, settingAny,
	)
	if hostSettingsExist {
		if err := applyEnvFileTrackedValidated(
			hostSettingsFile, values, tracker, hostSettingsLayer,
			ScopeHost, options.SyncSource,
		); err != nil {
			return domain.Context{}, nil, err
		}
		if err := validateBootstrapConfigHome(values, configHome, hostSettingsFile); err != nil {
			return domain.Context{}, nil, err
		}
	} else if !options.DisablePrivate && options.LayerPaths == nil {
		// Source checkouts retain a read-only compatibility input until the
		// installer moves it into configHome/config.env.
		privateConfig := filepath.Join(configDir, "..", "private", "config.env")
		if pathPresent(privateConfig) {
			if err := applyEnvFileTrackedValidated(
				privateConfig, values, tracker, hostSettingsLayer, ScopeHost, false,
			); err != nil {
				return domain.Context{}, nil, err
			}
		}
		if err := validateBootstrapConfigHome(values, configHome, privateConfig); err != nil {
			return domain.Context{}, nil, err
		}
	}
	values["SUBYARD_CONFIG_DIR"] = configDir
	extendAgentAssetMappings(logicalAssets, values)
	hostAssets := filepath.Join(configHome, "overrides", "host", "agents")
	if options.LayerPaths != nil {
		hostAssets = options.LayerPaths.HostAssets
	}
	hostFileLayer := tracker.addLayer(
		"host", "file settings", hostAssets, pathPresent(hostAssets), settingFile,
	)
	if err := applyAgentAssetLayer(values, logicalAssets,
		hostAssets, tracker, hostFileLayer); err != nil {
		return domain.Context{}, nil, err
	}

	yardName := options.YardName
	if yardName == "" {
		yardName = values["SUBYARD_YARD"]
	}
	if explicit := commandEnvironment["SUBYARD_YARD"]; explicit != "" && options.YardName == "" {
		yardName = explicit
	}
	if yardName == "" || yardName == "default" {
		yardName = "default"
	} else {
		if !domain.SafeName(yardName) {
			return domain.Context{}, nil, fmt.Errorf("invalid yard name %q", yardName)
		}
		yardDerivationLayer := tracker.addLayer(
			"yard", "derived", "yard name "+yardName, true, settingScalar,
			"HOST_BASE", "INCUS_PROJECT", "YARD_INSTANCE_NAME", "RESTRICTED_DISK_PATHS",
			"SRV_VOLUME", "SSH_HOST",
		)
		applyYardDerivations(yardName, values, tracker, yardDerivationLayer)
		yardFile := ""
		if options.YardSettingsFile != "" {
			yardFile = options.YardSettingsFile
		} else if options.LayerPaths != nil {
			yardFile = options.LayerPaths.YardSettings[yardName]
			if yardFile == "" {
				return domain.Context{}, nil, fmt.Errorf("unknown yard %q", yardName)
			}
		} else {
			yardFile, err = findYardFile(root, yardName, values, options.YardDirs)
			if err != nil {
				return domain.Context{}, nil, err
			}
		}
		if err := applyYardConfigTracked(
			configDir, yardName, yardFile, values, tracker, options.SyncSource,
		); err != nil {
			return domain.Context{}, nil, err
		}
		if err := validateBootstrapConfigHome(values, configHome, yardFile); err != nil {
			return domain.Context{}, nil, err
		}
		extendAgentAssetMappings(logicalAssets, values)
		yardAssets := filepath.Join(configHome, "yards", yardName, "overrides", "agents")
		if options.LayerPaths != nil {
			yardAssets = options.LayerPaths.YardAssets[yardName]
		}
		yardFileLayer := tracker.addLayer(
			"yard", "file settings", yardAssets, pathPresent(yardAssets), settingFile,
		)
		if err := applyAgentAssetLayer(values, logicalAssets,
			yardAssets, tracker, yardFileLayer); err != nil {
			return domain.Context{}, nil, err
		}
	}

	// The process environment is the final layer. Engine-forwarded contexts
	// have already had inherited yard fields removed above.
	commandLayer := tracker.addLayer(
		"command", "command override", "environment", true, settingAny,
	)
	for name, value := range commandEnvironment {
		if _, ok := LookupSetting(name); ok {
			if err := ValidateSetting(ScopeCommand, name, value, false); err != nil {
				return domain.Context{}, nil, fmt.Errorf("environment: %w", err)
			}
		}
		values[name] = value
		tracker.record(commandLayer, name, value, "environment", 0, "")
	}
	values["SUBYARD_CONFIG_DIR"] = configDir
	values["SUBYARD_CONFIG_HOME"] = configHome
	setConfigurationRolePaths(values, configHome, yardName)
	if yardName != "default" {
		values["YARD_NAME"] = yardName
	}
	normalizationLayer := tracker.addLayer(
		"derived", "derived", "resolver normalization", true, settingScalar, "E2E_VM_CPU",
	)
	if err := normalizeAgentAssetPaths(values, tracker); err != nil {
		return domain.Context{}, nil, err
	}
	if err := resolveAgentDependencies(values); err != nil {
		return domain.Context{}, nil, err
	}
	tracker.normalize("CODING_TOOL_INTEGRATIONS", values["CODING_TOOL_INTEGRATIONS"], "resolved agent dependencies")
	normalizeAgentPersistLinks(values, tracker, defaultLayer)
	ctx, err := contextFrom(root, yardName, values, tracker, defaultLayer, normalizationLayer)
	return ctx, values, err
}

func resolveAgentDependencies(values environment) error {
	requested := strings.Fields(values["CODING_TOOL_INTEGRATIONS"])
	selected := make(map[string]bool, len(requested))
	for _, agent := range requested {
		if selected[agent] {
			return fmt.Errorf("duplicate agent %q", agent)
		}
		selected[agent] = true
	}

	knownAgent := func(agent string) bool {
		for _, suffix := range []string{
			"COMMAND", "CHECK", "CONFIG", "CONFIG_DEST", "DEPENDS", "PERSIST",
			"PROJECTS_CHANGED", "PROVISION", "RULES", "RULES_DEST",
		} {
			if _, found := values["AGENT_"+agent+"_"+suffix]; found {
				return true
			}
		}
		return false
	}
	visiting := make(map[string]bool)
	visited := make(map[string]bool)
	resolved := make([]string, 0, len(requested))
	var visit func(string) error
	visit = func(agent string) error {
		if visited[agent] {
			return nil
		}
		if visiting[agent] {
			return fmt.Errorf("agent dependency cycle at %q", agent)
		}
		visiting[agent] = true
		dependencies := strings.Fields(values["AGENT_"+agent+"_DEPENDS"])
		seenDependencies := make(map[string]bool, len(dependencies))
		for _, dependency := range dependencies {
			if seenDependencies[dependency] {
				return fmt.Errorf("duplicate dependency %q for agent %q", dependency, agent)
			}
			seenDependencies[dependency] = true
			if !domain.SafeName(dependency) || !knownAgent(dependency) {
				return fmt.Errorf("unknown dependency %q for agent %q", dependency, agent)
			}
			if err := visit(dependency); err != nil {
				return err
			}
		}
		delete(visiting, agent)
		visited[agent] = true
		resolved = append(resolved, agent)
		return nil
	}
	for _, agent := range requested {
		if err := visit(agent); err != nil {
			return err
		}
	}
	values["CODING_TOOL_INTEGRATIONS"] = strings.Join(resolved, " ")
	return nil
}

func normalizeAgentPersistLinks(
	values environment,
	tracker *settingTracker,
	defaultLayer settingLayerID,
) {
	assignments := tracker.assignments["HOST_LINKS"]
	if len(assignments) == 0 || assignments[len(assignments)-1].Layer != defaultLayer {
		return
	}
	var selected strings.Builder
	for _, agent := range strings.Fields(values["CODING_TOOL_INTEGRATIONS"]) {
		selected.WriteString(values["AGENT_"+agent+"_PERSIST"])
	}
	values["HOST_LINKS"] = selected.String()
	tracker.normalize("HOST_LINKS", values["HOST_LINKS"], "derived from selected CODING_TOOL_INTEGRATIONS")
}

func cloneEnvironment(values environment) environment {
	result := make(environment, len(values))
	for name, value := range values {
		result[name] = value
	}
	return result
}

func bootstrapConfigHome(values environment) (string, error) {
	configHome := values["SUBYARD_CONFIG_HOME"]
	if configHome == "" {
		if xdg := values["XDG_CONFIG_HOME"]; xdg != "" {
			configHome = filepath.Join(xdg, "subyard")
		} else {
			configHome = filepath.Join(values["SUBYARD_OPERATOR_HOME"], ".config", "subyard")
		}
	}
	if !filepath.IsAbs(configHome) {
		return "", errors.New("SUBYARD_CONFIG_HOME must be absolute")
	}
	return filepath.Clean(configHome), nil
}

func ResolveConfigHome(operatorHome string, source map[string]string) (string, error) {
	values := environmentFrom(source)
	if operatorHome != "" {
		values["SUBYARD_OPERATOR_HOME"] = operatorHome
	}
	if values["SUBYARD_OPERATOR_HOME"] == "" {
		return "", errors.New("operator home is required")
	}
	return bootstrapConfigHome(values)
}

func PendingConfigurationTransaction(configHome string) (bool, error) {
	path := filepath.Join(configHome, ".sync", "transaction.json")
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func validateBootstrapConfigHome(values environment, expected, source string) error {
	if actual := filepath.Clean(values["SUBYARD_CONFIG_HOME"]); actual != expected {
		return fmt.Errorf("%s cannot relocate its config root from %s to %s; set SUBYARD_CONFIG_HOME before launch",
			source, expected, actual)
	}
	return nil
}

func setConfigurationRolePaths(values environment, configHome, yardName string) {
	values["SUBYARD_CONFIG_SHARED_DIR"] = filepath.Join(configHome, "overrides", "shared")
	values["SUBYARD_CONFIG_HOST_DIR"] = filepath.Join(configHome, "overrides", "host")
	values["SUBYARD_CONFIG_SECRETS_DIR"] = filepath.Join(configHome, "secrets")
	values["SUBYARD_CONFIG_GENERATED_DIR"] = filepath.Join(configHome, "generated")
	if yardName != "" && yardName != "default" {
		values["SUBYARD_CONFIG_YARD_DIR"] = filepath.Join(configHome, "yards", yardName)
	} else {
		values["SUBYARD_CONFIG_YARD_DIR"] = ""
	}
	if values["SUBYARD_KEYS_CONSUMER_ROOT"] == "" {
		values["SUBYARD_KEYS_CONSUMER_ROOT"] = values["SUBYARD_CONFIG_GENERATED_DIR"]
	}
	if values["SUBYARD_KEYS_PROD_FINGERPRINTS"] == "" {
		values["SUBYARD_KEYS_PROD_FINGERPRINTS"] =
			filepath.Join(values["SUBYARD_CONFIG_HOST_DIR"], "prod-fingerprints")
	}
}

func agentAssetMappings(values environment, configDir string) map[string]string {
	result := make(map[string]string)
	for name, value := range values {
		if !sourceValuedAgentSetting(name) || value == "" {
			continue
		}
		path := filepath.Clean(value)
		relative, err := filepath.Rel(configDir, path)
		if err == nil && safeAgentAssetRelative(relative) &&
			(strings.HasPrefix(relative, "agents"+string(filepath.Separator)) || relative == "agents") {
			result[name] = strings.TrimPrefix(relative, "agents"+string(filepath.Separator))
		}
	}
	return result
}

func extendAgentAssetMappings(mappings map[string]string, values environment) {
	for name, value := range values {
		if !sourceValuedAgentSetting(name) || value == "" {
			continue
		}
		path := filepath.ToSlash(filepath.Clean(value))
		for _, marker := range []string{"/private/agents/", "/overrides/host/agents/",
			"/overrides/shared/agents/"} {
			if index := strings.LastIndex(path, marker); index >= 0 {
				relative := filepath.FromSlash(path[index+len(marker):])
				if safeAgentAssetRelative(relative) {
					mappings[name] = relative
				}
				break
			}
		}
	}
}

func applyAgentAssetLayer(
	values environment,
	mappings map[string]string,
	agentsRoot string,
	tracker *settingTracker,
	layer settingLayerID,
) error {
	if agentsRoot == "" {
		return nil
	}
	for name, relative := range mappings {
		candidate := filepath.Join(agentsRoot, relative)
		exists, err := regularFileExists(candidate)
		if err != nil {
			return fmt.Errorf("%s override: %w", name, err)
		}
		if exists {
			values[name] = candidate
			tracker.record(layer, name, candidate, candidate, 0, "effective file source")
		}
	}
	return nil
}

func safeAgentAssetRelative(relative string) bool {
	if relative == "" || relative == "." || filepath.IsAbs(relative) ||
		relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) ||
		strings.ContainsAny(relative, "\x00\n\r\t") {
		return false
	}
	return true
}

func regularFileExists(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s must be a regular non-symlink file", path)
	}
	return true, nil
}

func normalizeAgentAssetPaths(
	values environment,
	tracker *settingTracker,
) error {
	for name, value := range values {
		if !sourceValuedAgentSetting(name) || value == "" {
			continue
		}
		if !filepath.IsAbs(value) {
			return fmt.Errorf("%s must name an absolute regular file", name)
		}
		path := filepath.Clean(value)
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("%s is not openable: %w", name, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("%s must name a regular non-symlink file", name)
		}
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("%s is not openable: %w", name, err)
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		values[name] = path
		if path != value {
			tracker.normalize(name, path, "normalized absolute path")
		}
	}
	return nil
}

func sourceValuedAgentSetting(name string) bool {
	if !strings.HasPrefix(name, "AGENT_") {
		return false
	}
	for _, suffix := range []string{"_CONFIG", "_RULES", "_PROVISION"} {
		agent, found := strings.CutSuffix(strings.TrimPrefix(name, "AGENT_"), suffix)
		if found && domain.SafeName(agent) {
			return true
		}
	}
	return false
}

func resetInheritedContext(values environment) {
	for _, name := range []string{
		"YARD_NAME", "ACCESS_KIND", "ENVIRONMENT_PROFILES", "YARD_KIND", "YARD_INSTANCE_NAME", "INCUS_PROJECT",
		"INCUS_BRIDGE", "SSH_HOST", "SSH_PORT", "OWNER_ENDPOINT", "OWNER_YARD_NAME", "SHIFT_MODE",
		"FORWARD_SSH_AGENT", "DEV_SUDO", "DEV_UID", "DEV_USER", "YARD_TEMPLATE", "NESTED_E2E_VMS",
		"E2E_VM_IMAGE", "E2E_VM_CPU", "E2E_VM_MEMORY", "E2E_VM_DISK", "E2E_VM_SLOT_COUNT", "E2E_VM_BOOT_TIMEOUT",
		"SUBYARD_STATE_DIR", "RESTRICTED_DISK_PATHS",
		"HOST_BASE", "SRV_VOLUME",
	} {
		delete(values, name)
	}
}

func applyYardConfig(configDir, yardName, yardFile string, values environment) error {
	return applyYardConfigTracked(configDir, yardName, yardFile, values, nil, false)
}

func applyYardConfigTracked(
	configDir, yardName, yardFile string,
	values environment,
	tracker *settingTracker,
	syncSource bool,
) error {
	// A named-yard settings file selects one public profile. The profile is
	// applied first and the named-yard settings file wins last.
	// Probe a copy because env files are declarative but may contain defaults that
	// depend on the existing normalized environment.
	probe := make(environment, len(values))
	for name, value := range values {
		probe[name] = value
	}
	delete(probe, "YARD_TEMPLATE")
	if err := applyEnvFileValidated(yardFile, probe, ScopeYard, syncSource, nil); err != nil {
		return err
	}
	if template := probe["YARD_TEMPLATE"]; template != "" {
		if !domain.SafeName(template) {
			return fmt.Errorf("invalid YARD_TEMPLATE %q in %s", template, yardFile)
		}
		if template == "e2e-vms" {
			return retiredE2EVMTemplateError(yardName, yardFile)
		}
		templateFile := filepath.Join(configDir, "yards", "profiles", template+".env")
		info, err := os.Stat(templateFile)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("unknown YARD_TEMPLATE %q in %s", template, yardFile)
			}
			return err
		}
		if info.IsDir() {
			return fmt.Errorf("unknown YARD_TEMPLATE %q in %s", template, yardFile)
		}
		if tracker == nil {
			if err := applyEnvFileValidated(
				templateFile, values, ScopeShipped, false, nil,
			); err != nil {
				return err
			}
		} else {
			profileLayer := tracker.addLayer(
				"yard", "shipped profile", templateFile, true, settingScalar,
			)
			if err := applyEnvFileTrackedValidated(
				templateFile, values, tracker, profileLayer, ScopeShipped, false,
			); err != nil {
				return err
			}
		}
	}
	if tracker == nil {
		return applyEnvFileValidated(yardFile, values, ScopeYard, syncSource, nil)
	}
	yardLayer := tracker.addLayer("yard", "scalar settings", yardFile, true, settingAny)
	return applyEnvFileTrackedValidated(
		yardFile, values, tracker, yardLayer, ScopeYard, syncSource,
	)
}

func applyEnvFileTracked(
	path string,
	values environment,
	tracker *settingTracker,
	layer settingLayerID,
) error {
	return applyEnvFileObserved(path, values, func(name, value string, line int) {
		tracker.record(layer, name, value, path, line, "")
	})
}

type observedAssignment struct {
	name  string
	value string
	line  int
}

func applyEnvFileTrackedValidated(
	path string,
	values environment,
	tracker *settingTracker,
	layer settingLayerID,
	scope SettingScope,
	requireSyncable bool,
) error {
	return applyEnvFileValidated(path, values, scope, requireSyncable,
		func(name, value string, line int) {
			tracker.record(layer, name, value, path, line, "")
		})
}

func applyEnvFileValidated(
	path string,
	values environment,
	scope SettingScope,
	requireSyncable bool,
	observer assignmentObserver,
) error {
	probe := cloneEnvironment(values)
	var assignments []observedAssignment
	if err := applyEnvFileObserved(path, probe, func(name, value string, line int) {
		assignments = append(assignments, observedAssignment{name: name, value: value, line: line})
	}); err != nil {
		return err
	}
	normalized := make([]observedAssignment, 0, len(assignments))
	type layerValue struct {
		input string
		value string
		line  int
	}
	seen := make(map[string]layerValue)
	for _, assignment := range assignments {
		canonical := canonicalSettingName(assignment.name)
		if previous, exists := seen[canonical]; exists && previous.input != assignment.name &&
			previous.value != assignment.value {
			return fmt.Errorf(
				"%s:%d: conflicting settings %s=%q and %s=%q",
				path, assignment.line, previous.input, previous.value, assignment.name, assignment.value,
			)
		}
		seen[canonical] = layerValue{input: assignment.name, value: assignment.value, line: assignment.line}
		probe[canonical] = assignment.value
		if canonical != assignment.name {
			delete(probe, assignment.name)
		}
		assignment.name = canonical
		normalized = append(normalized, assignment)
	}
	assignments = normalized
	for legacy := range legacySettingNames {
		delete(probe, legacy)
	}
	for _, assignment := range assignments {
		if err := ValidateSetting(scope, assignment.name, assignment.value, requireSyncable); err != nil {
			return fmt.Errorf("%s:%d: %w", path, assignment.line, err)
		}
	}
	for name := range values {
		delete(values, name)
	}
	for name, value := range probe {
		values[name] = value
	}
	if observer != nil {
		for _, assignment := range assignments {
			observer(assignment.name, assignment.value, assignment.line)
		}
	}
	return nil
}

var legacySettingNames = map[string]string{
	"YARD_TYPE":           "ACCESS_KIND",
	"INSTANCE_TYPE":       "YARD_KIND",
	"INSTANCE_NAME":       "YARD_INSTANCE_NAME",
	"REMOTE_DEST":         "OWNER_ENDPOINT",
	"REMOTE_YARD":         "OWNER_YARD_NAME",
	"BASE_IMAGE":          "YARD_IMAGE",
	"BASE_IMAGE_FALLBACK": "YARD_IMAGE_FALLBACK",
	"YARD_PROFILES":       "ENVIRONMENT_PROFILES",
	"AGENTS":              "CODING_TOOL_INTEGRATIONS",
}

func IsLegacySetting(name string) bool {
	_, legacy := legacySettingNames[name]
	return legacy
}

func AddLegacySettingAliases(values map[string]string) {
	for legacy, canonical := range legacySettingNames {
		values[legacy] = values[canonical]
	}
}

func canonicalSettingName(name string) string {
	if canonical, legacy := legacySettingNames[name]; legacy {
		return canonical
	}
	return name
}

func normalizeLegacyEnvironment(values environment) error {
	for legacy, canonical := range legacySettingNames {
		legacyValue, hasLegacy := values[legacy]
		if !hasLegacy {
			continue
		}
		if canonicalValue, hasCanonical := values[canonical]; hasCanonical && canonicalValue != legacyValue {
			return fmt.Errorf(
				"conflicting command environment settings %s=%q and %s=%q",
				canonical, canonicalValue, legacy, legacyValue,
			)
		}
		values[canonical] = legacyValue
		delete(values, legacy)
	}
	return nil
}

func applyOptionalTracked(
	path string,
	values environment,
	tracker *settingTracker,
	layer settingLayerID,
) error {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return applyEnvFileTracked(path, values, tracker, layer)
}

func pathPresent(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func retiredE2EVMTemplateError(yardName, yardFile string) error {
	return &retiredYardTemplateError{diagnostic: fmt.Sprintf(`YARD_TEMPLATE "e2e-vms" is retired in %s
replace its YARD_TEMPLATE assignment with:
  YARD_TEMPLATE=test-vms
then verify the unchanged yard identity:
  yard -Y %s check
  yard -Y %s status
to retire that yard after the config migration:
  yard -Y %s test-vms status
  yard -Y %s teardown`, yardFile, yardName, yardName, yardName, yardName)}
}

func environmentFrom(explicit map[string]string) environment {
	values := make(environment)
	if explicit == nil {
		for _, pair := range os.Environ() {
			name, value, ok := strings.Cut(pair, "=")
			if ok {
				values[name] = value
			}
		}
		return values
	}
	for name, value := range explicit {
		values[name] = value
	}
	return values
}

// FindYardSettingsFile returns the highest-precedence supported definition for
// a named yard.
func FindYardSettingsFile(root, name, configHome string) (string, error) {
	return findYardFileAt(root, name, configHome, nil)
}

func findYardFile(root, name string, values environment, explicit []string) (string, error) {
	return findYardFileAt(root, name, values["SUBYARD_CONFIG_HOME"], explicit)
}

func findYardFileAt(root, name, configHome string, explicit []string) (string, error) {
	var candidates []string
	if len(explicit) != 0 {
		for _, directory := range explicit {
			candidates = append(candidates,
				filepath.Join(directory, name, "config.env"),
				filepath.Join(directory, name+".env"),
			)
		}
	} else {
		privateYards := filepath.Join(root, "private", "yards")
		candidates = []string{
			filepath.Join(configHome, "yards", name, "config.env"),
			privateYards + string(filepath.Separator) + name + ".env",
			filepath.Join(configHome, "yards", name+".env"),
		}
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%w %q", ErrUnknownYard, name)
}

func applyYardDerivations(
	name string,
	values environment,
	tracker *settingTracker,
	layer settingLayerID,
) {
	values["YARD_NAME"] = name
	setYardDefault(values, "YARD_INSTANCE_NAME", "yard", "yard-"+name, tracker, layer)
	setYardDefault(values, "INCUS_PROJECT", "subyard", "subyard-"+name, tracker, layer)
	setYardDefault(values, "SSH_HOST", "yard", "yard-"+name, tracker, layer)
	setYardDefault(values, "SRV_VOLUME", "yard-srv", "yard-srv-"+name, tracker, layer)
	setYardDefault(
		values, "RESTRICTED_DISK_PATHS", "/srv/subyard", "/srv/subyard-"+name, tracker, layer,
	)
	setYardDefault(values, "HOST_BASE", "/srv/subyard", "/srv/subyard-"+name, tracker, layer)
	configHome := values["SUBYARD_CONFIG_HOME"]
	if configHome == "" {
		configHome = filepath.Join(values["SUBYARD_OPERATOR_HOME"], ".config", "subyard")
	}
	setYardDefault(values, "SUBYARD_STATE_DIR", filepath.Join(configHome, "projects"),
		filepath.Join(configHome, "yards", name, "projects"), tracker, layer)
}

func setYardDefault(
	values environment,
	name, generic, derived string,
	tracker *settingTracker,
	layer settingLayerID,
) {
	if values[name] == "" || values[name] == generic {
		values[name] = derived
		tracker.record(layer, name, derived, "", 0, "derived from yard name")
	}
}

func setDefault(
	values environment,
	name, value string,
	tracker *settingTracker,
	layer settingLayerID,
) {
	if values[name] == "" {
		values[name] = value
		tracker.record(layer, name, value, "built-in resolver default", 0, "")
	}
}

func contextFrom(
	root, yardName string,
	values environment,
	tracker *settingTracker,
	defaultLayer, normalizationLayer settingLayerID,
) (domain.Context, error) {
	setDefault(values, "ACCESS_KIND", "local", tracker, defaultLayer)
	setDefault(values, "YARD_KIND", "container", tracker, defaultLayer)
	setDefault(values, "YARD_INSTANCE_NAME", "yard", tracker, defaultLayer)
	setDefault(values, "INCUS_PROJECT", "subyard", tracker, defaultLayer)
	setDefault(values, "INCUS_BRIDGE", "incusbr0", tracker, defaultLayer)
	setDefault(values, "SSH_HOST", "yard", tracker, defaultLayer)
	setDefault(values, "DEV_USER", "dev", tracker, defaultLayer)
	setDefault(values, "NESTED_E2E_VMS", "0", tracker, defaultLayer)
	setDefault(values, "E2E_VM_IMAGE", "images:debian/13/cloud", tracker, defaultLayer)
	setDefault(values, "E2E_VM_CPU", "2", tracker, defaultLayer)
	setDefault(values, "E2E_VM_MEMORY", "4GiB", tracker, defaultLayer)
	setDefault(values, "E2E_VM_DISK", "20GiB", tracker, defaultLayer)
	setDefault(values, "E2E_VM_SLOT_COUNT", "2", tracker, defaultLayer)
	setDefault(values, "E2E_VM_BOOT_TIMEOUT", "300", tracker, defaultLayer)
	for _, name := range []string{"STORAGE_PATH", "HOST_BASE", "RESTRICTED_DISK_PATHS"} {
		raw := values[name]
		clean := filepath.Clean(raw)
		values[name] = clean
		if clean != raw {
			tracker.normalize(name, clean, "normalized path")
		}
	}
	configHome := values["SUBYARD_CONFIG_HOME"]
	dataHome := values["SUBYARD_HOME"]
	hostBase := values["HOST_BASE"]
	restrictedBase := values["RESTRICTED_DISK_PATHS"]
	if hostBase != restrictedBase {
		return domain.Context{}, errors.New("HOST_BASE must equal RESTRICTED_DISK_PATHS")
	}
	stateDir := values["SUBYARD_STATE_DIR"]
	if stateDir == "" {
		stateDir = filepath.Join(configHome, "projects")
	}
	sshPort, err := optionalInt(values["SSH_PORT"])
	if err != nil {
		return domain.Context{}, fmt.Errorf("SSH_PORT: %w", err)
	}
	devUID, err := strconv.Atoi(values["DEV_UID"])
	if err != nil {
		return domain.Context{}, fmt.Errorf("DEV_UID must be numeric")
	}
	forwardAgent, err := zeroOne(values["FORWARD_SSH_AGENT"], "FORWARD_SSH_AGENT")
	if err != nil {
		return domain.Context{}, err
	}
	devSudo, err := zeroOne(values["DEV_SUDO"], "DEV_SUDO")
	if err != nil {
		return domain.Context{}, err
	}
	nestedE2EVMs, err := zeroOne(values["NESTED_E2E_VMS"], "NESTED_E2E_VMS")
	if err != nil {
		return domain.Context{}, err
	}
	rawE2EVMCPU := values["E2E_VM_CPU"]
	e2eVMCPU, err := resolveE2EVMCPU(rawE2EVMCPU, runtime.NumCPU())
	if err != nil {
		return domain.Context{}, err
	}
	values["E2E_VM_CPU"] = e2eVMCPU
	if e2eVMCPU != rawE2EVMCPU {
		tracker.record(
			normalizationLayer, "E2E_VM_CPU", e2eVMCPU, "", 0,
			"resolved from "+rawE2EVMCPU,
		)
	}
	if err := validateE2EConfig(values); err != nil {
		return domain.Context{}, err
	}
	ctx := domain.Context{
		YardName:         yardName,
		AccessKind:       domain.AccessKind(values["ACCESS_KIND"]),
		YardKind:         domain.YardKind(values["YARD_KIND"]),
		YardInstanceName: values["YARD_INSTANCE_NAME"],
		IncusProject:     values["INCUS_PROJECT"],
		IncusBridge:      values["INCUS_BRIDGE"],
		SSHHost:          values["SSH_HOST"],
		DevUser:          values["DEV_USER"],
		SSHPort:          sshPort,
		OwnerEndpoint:    values["OWNER_ENDPOINT"],
		OwnerYardName:    values["OWNER_YARD_NAME"],
		YardImageRef:     domain.YardImageRef(values["YARD_IMAGE"]),
		ShiftMode:        values["SHIFT_MODE"],
		ForwardSSHAgent:  forwardAgent,
		DevSudo:          devSudo,
		NestedE2EVMs:     nestedE2EVMs,
		DevUID:           devUID,
		Paths: domain.RuntimePaths{
			RepositoryRoot: root,
			ConfigDir:      filepath.Clean(values["SUBYARD_CONFIG_DIR"]),
			OperatorHome:   values["SUBYARD_OPERATOR_HOME"],
			ConfigHome:     configHome,
			DataHome:       dataHome,
			StoragePath:    values["STORAGE_PATH"],
			HostBase:       hostBase,
			StateDir:       stateDir,
		},
	}
	if ctx.AccessKind == domain.AccessRemote && ctx.OwnerYardName == "" {
		ctx.OwnerYardName = "default"
	}
	return domain.NormalizeContext(ctx)
}

func resolveE2EVMCPU(value string, hostCPUs int) (string, error) {
	if value != "" && value != "auto" {
		if strings.Trim(value, "0123456789") != "" {
			return "", errors.New("E2E_VM_CPU must be auto or a positive integer")
		}
		count, err := strconv.Atoi(value)
		if err != nil || count < 1 {
			return "", errors.New("E2E_VM_CPU must be auto or a positive integer")
		}
		return strconv.Itoa(count), nil
	}
	if hostCPUs < 1 {
		return "", errors.New("cannot resolve E2E_VM_CPU: host CPU count is unavailable")
	}
	count := hostCPUs * 2 / 3
	if count < 1 {
		count = 1
	}
	if count > 4 {
		count = 4
	}
	return strconv.Itoa(count), nil
}

func optionalInt(value string) (int, error) {
	if value == "" {
		return 0, nil
	}
	return strconv.Atoi(value)
}

func zeroOne(value, name string) (bool, error) {
	switch value {
	case "0":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, fmt.Errorf("%s must be 0 or 1", name)
	}
}

func validateE2EConfig(values environment) error {
	positive := func(name string) (int, error) {
		value, err := strconv.Atoi(values[name])
		if err != nil || value < 1 {
			return 0, fmt.Errorf("%s must be a positive integer", name)
		}
		return value, nil
	}
	if _, err := positive("E2E_VM_CPU"); err != nil {
		return err
	}
	sizeMiB := func(name string) (int, error) {
		value := values[name]
		factor, raw := 1, strings.TrimSuffix(value, "MiB")
		if strings.HasSuffix(value, "GiB") {
			factor, raw = 1024, strings.TrimSuffix(value, "GiB")
		} else if raw == value {
			return 0, fmt.Errorf("%s must use a positive MiB or GiB value", name)
		}
		amount, err := strconv.Atoi(raw)
		if err != nil || amount < 1 {
			return 0, fmt.Errorf("%s must use a positive MiB or GiB value", name)
		}
		return amount * factor, nil
	}
	if _, err := sizeMiB("E2E_VM_MEMORY"); err != nil {
		return err
	}
	if disk, err := sizeMiB("E2E_VM_DISK"); err != nil {
		return err
	} else if disk < 10*1024 {
		return errors.New("E2E_VM_DISK must be at least 10GiB")
	}
	for name, bounds := range map[string][2]int{
		"E2E_VM_SLOT_COUNT": {1, 1<<20 - 1}, "E2E_VM_BOOT_TIMEOUT": {30, 1800},
	} {
		value, err := strconv.Atoi(values[name])
		if err != nil || value < bounds[0] || value > bounds[1] {
			return fmt.Errorf("%s must be an integer from %d to %d", name, bounds[0], bounds[1])
		}
	}
	image := values["E2E_VM_IMAGE"]
	if image == "" || strings.HasPrefix(image, "-") || strings.ContainsFunc(image, func(char rune) bool {
		return !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._:/@+-", char)
	}) {
		return errors.New("E2E_VM_IMAGE contains unsafe characters")
	}
	return nil
}

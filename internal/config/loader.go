package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/resource"
)

type LoadOptions struct {
	// NativeTargetOnly resolves scalar address/paths for registry assessment. Its
	// result must never be used for guest access, provisioning or activation.
	NativeTargetOnly bool
	Catalog          *Catalog
	// Resources supplies operation-scoped declarations on demand.
	// Saved endpoint state is always read fresh. Standalone callers may omit it.
	Resources               func(string) (resource.Registry, error)
	RepositoryRoot          string
	OperatorHome            string
	YardName                string
	YardSettingsFile        string
	Environment             map[string]string
	DisablePrivate          bool
	SyncSource              bool
	ConfigLocked            bool
	AllowPendingTransaction bool
	AllowResetYard          bool // teardown retry only; requires a validated ownership marker
	LayerPaths              *LayerPaths
	// FileOverrides substitutes retained draft files while keeping logical paths
	// and normal registration, precedence and resource-endpoint resolution.
	// An empty source marks a logical file absent.
	ProspectiveFiles *ProspectiveFiles
	FileOverrides    map[string]string
	YardDirs         []string
}

const GitSettingsRelativePath = ".sync/settings"

type LayerPaths struct {
	ExcludedLocalPaths map[string]bool
	GitSharedSettings  string
	GitHostSettings    string
	GitYardSettings    map[string]string
	GitSharedAssets    string
	GitHostAssets      string
	GitYardAssets      map[string]string
	SharedSettings     string
	SharedAssets       string
	HostSettings       string
	HostAssets         string
	YardSettings       map[string]string
	YardAssets         map[string]string
}

type Loaded struct {
	Catalog               Catalog
	Integrations          IntegrationSelection
	EnvironmentProfiles   EnvironmentProfileSelection
	Context               domain.Context
	Environment           map[string]string
	Settings              map[string]SettingTrace
	ConfigurationLayers   []ConfigurationLayer
	assetValidationInputs []string
	ProspectiveFiles      *ProspectiveFiles
	ignored               []IgnoredAssignment
}

// AssetValidationInputs returns the logical file sources opened while validating
// agent assets. It includes explicit sources outside the configuration layers.
func (loaded Loaded) AssetValidationInputs() []string {
	return slices.Clone(loaded.assetValidationInputs)
}

type retiredYardTemplateError struct {
	yardName, yardFile string
}

var ErrUnknownYard = errors.New("unknown yard")

func (err *retiredYardTemplateError) Error() string { return err.render(err.yardFile) }

func (err *retiredYardTemplateError) render(source string) string {
	return fmt.Sprintf(`YARD_TEMPLATE "e2e-vms" is retired in %s
replace its YARD_TEMPLATE assignment with:
  YARD_TEMPLATE=test-vms
then verify the unchanged yard identity:
  yard -Y %s check
  yard -Y %s status
to retire that yard after the config migration:
  yard -Y %s test-vms status
  yard -Y %s teardown`, source, err.yardName, err.yardName, err.yardName, err.yardName)
}

func IsRetiredYardTemplate(err error) bool {
	var retired *retiredYardTemplateError
	return errors.As(err, &retired)
}

func Load(options LoadOptions) (Loaded, error) {
	tracker := newSettingTracker()
	tracker.fileOverrides = options.FileOverrides
	tracker.prospectiveFiles = options.ProspectiveFiles
	if options.Catalog != nil {
		tracker.catalog = *options.Catalog
	} else {
		var err error
		tracker.catalog, err = LoadCatalog(options.RepositoryRoot)
		if err != nil {
			return Loaded{}, err
		}
	}
	ctx, values, err := load(options, tracker)
	if err != nil {
		return Loaded{}, err
	}
	environment := make(map[string]string, len(values))
	for name, value := range values {
		environment[name] = value
	}
	return Loaded{
		Catalog: tracker.catalog, Context: ctx, Environment: environment, Settings: tracker.traces(values),
		ConfigurationLayers: tracker.configurationLayers(), Integrations: tracker.integrations,
		EnvironmentProfiles:   tracker.environmentProfiles,
		assetValidationInputs: tracker.assetValidationInputs,
		ProspectiveFiles:      options.ProspectiveFiles,
		ignored:               tracker.ignoredAssignments(ctx.YardName, ctx.Paths.ConfigHome),
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
	for name := range values {
		if tracker.catalog.retiredSetting(name) {
			delete(values, name)
		}
	}
	if err := normalizeLegacyEnvironment(values); err != nil {
		return domain.Context{}, nil, err
	}
	if values["SUBYARD_ENGINE_CONTEXT"] == "1" && values["SUBYARD_CONFIG_LOADED"] != "1" {
		resetInheritedContext(values)
	}
	commandEnvironment := cloneEnvironment(values)
	// A command-environment profile-only value never takes part in evaluation;
	// the command layer below only reports it.
	for _, name := range profileOnlySettings {
		delete(values, name)
	}
	if values["SUBYARD_CONFIG_DIR"] == "" {
		values["SUBYARD_CONFIG_DIR"] = filepath.Join(root, "config")
	}
	configDir := filepath.Clean(values["SUBYARD_CONFIG_DIR"])
	if options.OperatorHome != "" {
		values["SUBYARD_OPERATOR_HOME"] = options.OperatorHome
		commandEnvironment["SUBYARD_OPERATOR_HOME"] = options.OperatorHome
	}
	if values["SUBYARD_OPERATOR_HOME"] == "" {
		return domain.Context{}, nil, domain.NewConfigRefusal(domain.ConfigOperatorHomeRequired)
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
	pending, err := options.ProspectiveFiles.pendingTransaction(configHome)
	if err != nil {
		return domain.Context{}, nil, err
	}
	if pending && !options.AllowPendingTransaction {
		return domain.Context{}, nil, domain.NewConfigRefusal(domain.ConfigTransactionInterrupted)
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
	integrationDefaultSources := map[string]string{}
	for _, owner := range tracker.catalog.Integrations() {
		source := filepath.Join(owner.Root, "integration.json")
		integrationDefaultSources["integration:"+owner.Name] = source
		// Immutable inputs never inherit operator or ambient values.
		for _, input := range owner.Inputs {
			delete(values, input.Name)
			delete(commandEnvironment, input.Name)
		}
		metadata := map[string]string{"PROVISION": owner.Provision, "COMMAND": owner.Command, "CHECK": owner.Check, "HEALTH": owner.Health, "DEPENDS": strings.Join(owner.Depends, " "), "PERSIST": owner.Persist}
		for _, suffix := range []string{"PROVISION", "COMMAND", "CHECK", "HEALTH", "DEPENDS", "PERSIST"} {
			value := metadata[suffix]
			if value == "" {
				continue
			}
			if suffix == "PROVISION" {
				value = filepath.Join(owner.Root, value)
			}
			name := "AGENT_" + owner.Name + "_" + suffix
			if values[name] == "" {
				values[name] = value
			}
			tracker.record(defaultLayer, name, values[name], source, 0, "package mechanics default")
		}
		for _, artifact := range owner.Artifacts {
			if artifact.Kind != "config" {
				continue
			}
			key := "AGENT_" + owner.Name + "_" + strings.ToUpper(artifact.ID)
			if _, present := values[key]; !present {
				values[key] = filepath.Join(owner.Root, artifact.Source)
			}
			if _, present := values[key+"_DEST"]; !present {
				values[key+"_DEST"] = artifact.Destination
			}
			for _, name := range []string{key, key + "_DEST"} {
				tracker.record(defaultLayer, name, values[name], source, 0, "package artifact default")
			}
		}
	}
	for _, definition := range tracker.catalog.SettingCatalog() {
		if !definition.HasDefault && (definition.DefaultFrom == nil || integrationDefaultSources[definition.Owner] == "") {
			continue
		}
		if values[definition.Name] == "" {
			values[definition.Name] = definition.Default
		}
		tracker.record(defaultLayer, definition.Name, values[definition.Name], integrationDefaultSources[definition.Owner], 0, "profile default")
	}
	for _, name := range []string{"incus.project.env", "subyard.env", "host.env", "agents.env", "ports.env"} {
		path := filepath.Join(configDir, name)
		if err := applyOptionalTracked(path, values, tracker, defaultLayer); err != nil {
			return domain.Context{}, nil, err
		}
	}
	yardName := options.YardName
	if yardName == "" {
		yardName = values["SUBYARD_YARD"]
	}
	if explicit := commandEnvironment["SUBYARD_YARD"]; explicit != "" && options.YardName == "" {
		yardName = explicit
	}
	if yardName == "" {
		yardName = "default"
	}
	if !domain.SafeName(yardName) {
		return domain.Context{}, nil, fmt.Errorf("%w %q", domain.NewConfigRefusal(domain.ConfigYardNameInvalid), yardName)
	}
	resetFallback, err := options.ProspectiveFiles.fallbackReset(configHome, yardName)
	if err != nil {
		return domain.Context{}, nil, err
	}
	gitRoot := filepath.Join(configHome, GitSettingsRelativePath)
	gitShared := filepath.Join(gitRoot, "overrides", "shared", "config.env")
	gitHost := filepath.Join(gitRoot, "config.env")
	gitYard := filepath.Join(gitRoot, "yards", yardName, "config.env")
	localShared := filepath.Join(configHome, "overrides", "shared", "config.env")
	localHost := filepath.Join(configHome, "config.env")
	localYard := filepath.Join(configHome, "yards", yardName, "config.env")
	if options.LayerPaths != nil {
		gitShared, gitHost, gitYard = options.LayerPaths.GitSharedSettings, options.LayerPaths.GitHostSettings, options.LayerPaths.GitYardSettings[yardName]
		localShared, localHost, localYard = options.LayerPaths.SharedSettings, options.LayerPaths.HostSettings, options.LayerPaths.YardSettings[yardName]
	} else {
		hostExists, err := tracker.fileExists(localHost)
		if err != nil {
			return domain.Context{}, nil, err
		}
		if !hostExists && !options.DisablePrivate {
			privateConfig := filepath.Join(configDir, "..", "private", "config.env")
			if pathPresent(privateConfig) {
				localHost = privateConfig
			}
		}
		if yardName != "default" && options.YardSettingsFile == "" {
			if (len(options.FileOverrides) != 0 || options.ProspectiveFiles != nil) && len(options.YardDirs) == 0 {
				candidates := append(YardFileCandidates(configDir, configHome, yardName), gitYard)
				if resetFallback {
					candidates = []string{localYard, filepath.Join(configHome, "yards", yardName+".env")}
				}
				localYard, err = tracker.findYardFile(candidates, yardName)
			} else {
				localYard, err = findYardFile(root, yardName, values, options.YardDirs)
			}
			if errors.Is(err, ErrUnknownYard) && options.AllowResetYard && resetFallback {
				localYard, err = "", nil
			}
			if err != nil {
				return domain.Context{}, nil, err
			}
			if localYard == gitYard {
				localYard = ""
			}
		}
	}
	if options.YardSettingsFile != "" && yardName != "default" {
		localYard = options.YardSettingsFile
	}
	if resetFallback && !options.SyncSource {
		gitYard = ""
	}
	if yardName != "default" && localYard == "" && gitYard == "" && !(options.AllowResetYard && resetFallback) {
		return domain.Context{}, nil, fmt.Errorf("%w %q", ErrUnknownYard, yardName)
	}
	if yardName != "default" {
		yardDerivationLayer := tracker.addLayer("yard", "derived", "yard name "+yardName, true, settingScalar,
			"HOST_BASE", "INCUS_PROJECT", "YARD_INSTANCE_NAME", "RESTRICTED_DISK_PATHS", "SRV_VOLUME", "SSH_HOST")
		applyYardDerivations(yardName, values, tracker, yardDerivationLayer)
	}
	// Select the profile from both yard layers before applying it once. Local
	// assignments, including an explicit empty template, override the Git cache.
	probe := cloneEnvironment(values)
	delete(probe, "YARD_TEMPLATE")
	yardPresent := false
	templateSource := gitYard
	for _, path := range []string{gitYard, localYard} {
		if options.LayerPaths != nil && path == localYard && options.LayerPaths.ExcludedLocalPaths[path] {
			continue
		}
		if path == "" {
			continue
		}
		exists, err := tracker.fileExists(path)
		if err != nil {
			return domain.Context{}, nil, err
		}
		if exists {
			yardPresent = true
			templateSource = path
			source := tracker.fileSource(path)
			if err := tracker.catalog.applyEnvFileValidatedFrom(path, source, probe, ScopeYard, options.SyncSource || path == gitYard, nil, nil, tracker.prospectiveReader()); err != nil {
				return domain.Context{}, nil, err
			}
		}
	}
	if yardName != "default" && !yardPresent && !(options.AllowResetYard && resetFallback) {
		return domain.Context{}, nil, fmt.Errorf("%w %q", ErrUnknownYard, yardName)
	}
	if err := applyYardTemplateTracked(configDir, yardName, probe["YARD_TEMPLATE"], templateSource, values, tracker); err != nil {
		return domain.Context{}, nil, err
	}
	logicalAssets := agentAssetMappings(values, configDir)
	// Package defaults use canonical immutable roots, even when configDir is an
	// allowed source alias. Preserve the same file-setting overlay identities.
	for _, owner := range tracker.catalog.Integrations() {
		for _, artifact := range owner.Artifacts {
			if artifact.Kind != "config" {
				continue
			}
			key := "AGENT_" + owner.Name + "_" + strings.ToUpper(artifact.ID)
			if values[key] == filepath.Join(owner.Root, artifact.Source) {
				logicalAssets[key] = filepath.Join(owner.Name, artifact.Source)
			}
		}
	}
	gitSharedAssets := filepath.Join(gitRoot, "overrides", "shared", "agents")
	gitHostAssets := filepath.Join(gitRoot, "overrides", "host", "agents")
	gitYardAssets := filepath.Join(gitRoot, "yards", yardName, "overrides", "agents")
	sharedAssets := filepath.Join(configHome, "overrides", "shared", "agents")
	hostAssets := filepath.Join(configHome, "overrides", "host", "agents")
	yardAssets := filepath.Join(configHome, "yards", yardName, "overrides", "agents")
	if options.LayerPaths != nil {
		gitSharedAssets, gitHostAssets, gitYardAssets = options.LayerPaths.GitSharedAssets, options.LayerPaths.GitHostAssets, options.LayerPaths.GitYardAssets[yardName]
		sharedAssets, hostAssets, yardAssets = options.LayerPaths.SharedAssets, options.LayerPaths.HostAssets, options.LayerPaths.YardAssets[yardName]
	}
	if resetFallback && !options.SyncSource {
		gitYardAssets = ""
	}
	for _, layer := range []struct {
		path, assets, scope, role, assetRole string
		syncable                             bool
	}{
		{gitShared, gitSharedAssets, "shared", "Git scalar settings", "Git file settings", true},
		{gitHost, gitHostAssets, "host", "Git scalar settings", "Git file settings", true},
		{gitYard, gitYardAssets, "yard", "Git scalar settings", "Git file settings", true},
		{localShared, sharedAssets, "shared", "scalar settings", "file settings", options.SyncSource},
		{localHost, hostAssets, "host", "scalar settings", "file settings", options.SyncSource},
		{localYard, yardAssets, "yard", "scalar settings", "file settings", options.SyncSource},
	} {
		if yardName != "default" && layer.scope == "yard" {
			derivation := tracker.addLayer("yard", "derived", "yard name "+yardName, true, settingScalar,
				"HOST_BASE", "INCUS_PROJECT", "YARD_INSTANCE_NAME", "RESTRICTED_DISK_PATHS", "SRV_VOLUME", "SSH_HOST")
			applyYardDerivations(yardName, values, tracker, derivation)
		}
		exists := false
		if layer.path != "" && !(options.LayerPaths != nil && layer.role == "scalar settings" && options.LayerPaths.ExcludedLocalPaths[layer.path]) {
			exists, err = tracker.fileExists(layer.path)
			if err != nil {
				return domain.Context{}, nil, err
			}
		}
		id := tracker.addLayer(layer.scope, layer.role, layer.path, exists, settingScalar)
		if exists {
			if err := applyEnvFileTrackedValidated(layer.path, values, tracker, id, SettingScope(layer.scope), layer.syncable); err != nil {
				return domain.Context{}, nil, err
			}
			if err := validateBootstrapConfigHome(values, configHome, layer.path); err != nil {
				return domain.Context{}, nil, tracker.sourceError("SUBYARD_CONFIG_HOME", "cannot relocate config root; set SUBYARD_CONFIG_HOME before launch", err)
			}
		}
		extendAgentAssetMappings(logicalAssets, values)
		assetLayer := tracker.addLayer(layer.scope, layer.assetRole, layer.assets, tracker.pathPresent(layer.assets), settingFile)
		var excluded map[string]bool
		if options.LayerPaths != nil && layer.assetRole == "file settings" {
			excluded = options.LayerPaths.ExcludedLocalPaths
		}
		if err := applyAgentAssetLayer(values, logicalAssets, layer.assets, tracker, assetLayer, excluded); err != nil {
			return domain.Context{}, nil, err
		}
	}
	values["SUBYARD_CONFIG_DIR"] = configDir

	// The process environment is the final layer. Engine-forwarded contexts
	// have already had inherited yard fields removed above.
	commandLayer := tracker.addLayer(
		"command", "command override", "environment", true, settingAny,
	)
	var commandRefusals []error
	for _, name := range slices.Sorted(maps.Keys(commandEnvironment)) {
		value := commandEnvironment[name]
		if IsProfileOnlySetting(name) {
			tracker.observeProfileOnly(commandLayer, name, value, "", 0)
			continue
		}
		if _, ok := tracker.catalog.LookupSetting(name); ok {
			if err := tracker.catalog.ValidateSetting(ScopeCommand, name, value, false); err != nil {
				commandRefusals = append(commandRefusals, sourceContext("", 0, recognizedSettingKey(tracker.catalog, name), "command override", err))
				continue
			}
		}
		values[name] = value
		tracker.record(commandLayer, name, value, "environment", 0, "")
	}
	if err := domain.CollectRefusals(commandRefusals...); err != nil {
		return domain.Context{}, nil, err
	}
	values["SUBYARD_CONFIG_DIR"] = configDir
	// Config-directory env files are compatibility defaults, not authority for
	// immutable package inputs. Report the same canonical value hooks receive.
	for _, owner := range tracker.catalog.Integrations() {
		for _, input := range owner.Inputs {
			values[input.Name] = input.Value
			tracker.record(defaultLayer, input.Name, input.Value, filepath.Join(owner.Root, "integration.json"), 0, "immutable shipped input")
		}
	}
	values["SUBYARD_CONFIG_HOME"] = configHome
	setConfigurationRolePaths(values, configHome, yardName)
	if yardName != "default" {
		values["YARD_NAME"] = yardName
	}
	normalizationLayer := tracker.addLayer(
		"derived", "derived", "resolver normalization", true, settingScalar, "E2E_VM_CPU",
	)
	if options.NativeTargetOnly {
		// Address discovery does not require guest-account, port or provisioner
		// readiness. Scalar source parsing and registration checks above still apply.
		for _, entry := range []struct{ key, value string }{{"ACCESS_KIND", "local"}, {"INCUS_PROJECT", "subyard"}, {"YARD_INSTANCE_NAME", "yard"}, {"SSH_HOST", "yard"}} {
			setDefault(values, entry.key, entry.value, tracker, defaultLayer)
		}
		return domain.Context{YardName: yardName, AccessKind: domain.AccessKind(values["ACCESS_KIND"]), IncusProject: values["INCUS_PROJECT"], YardInstanceName: values["YARD_INSTANCE_NAME"], SSHHost: values["SSH_HOST"], Paths: domain.RuntimePaths{RepositoryRoot: root, ConfigDir: configDir, ConfigHome: configHome, OperatorHome: values["SUBYARD_OPERATOR_HOME"], DataHome: values["SUBYARD_HOME"], HostBase: values["HOST_BASE"]}}, values, nil
	}
	if err := normalizeAgentAssetPaths(values, tracker); err != nil {
		return domain.Context{}, nil, err
	}
	integrationRefusal, err := resolveLoadedIntegrations(values, tracker)
	if err != nil {
		return domain.Context{}, nil, err
	}
	normalizeAgentPersistLinks(values, tracker, defaultLayer)
	roleRefusals := []error{integrationRefusal}
	if values["ALLOWS_HOST_ACCESS"] == "false" {
		for _, owner := range tracker.catalog.Integrations() {
			for _, artifact := range owner.Artifacts {
				if artifact.SourceSetting != "" && strings.TrimSpace(values[artifact.SourceSetting]) != "" {
					roleRefusals = append(roleRefusals, namedRefusal(domain.ConfigRoleForbidsSetting, domain.ConfigSettingName, artifact.SourceSetting))
				}
			}
		}
	}
	// Role, profile and context checks read independent settings, so each stage
	// runs and every refusal is reported. Profile resolution needs valid constraints.
	var refusals []error
	if err := validateProfileConstraints(configDir, yardName, values, roleRefusals...); err != nil {
		refusals = append(refusals, refusalCauses(err)...)
	} else if err := resolveLoadedEnvironmentProfiles(configDir, yardName, values, tracker); err != nil {
		refusals = append(refusals, err)
	}
	ctx, err := contextFrom(root, yardName, values, tracker, defaultLayer, normalizationLayer)
	if err := domain.CollectRefusals(append(refusals, refusalCauses(err)...)...); err != nil {
		return domain.Context{}, nil, err
	}
	if err := applySavedResourceEndpoints(root, options, ctx, values, tracker); err != nil {
		return ctx, values, err
	}
	// Derived ports need their source ports; collisions are collected once per pair.
	collisions := newPortCollisions()
	if err := normalizeDerivedPorts(values, tracker, collisions); err != nil {
		return ctx, values, err
	}
	webPreview := normalizeWebPreviewPort(values, tracker, ctx.SSHPort, collisions)
	return ctx, values, domain.CollectRefusals(append(collisions.refusals, webPreview)...)
}

// refusalCauses lets independent stages merge into one stable collection.
func refusalCauses(err error) []error {
	if refusals, ok := err.(*domain.Refusals); ok {
		return refusals.Unwrap()
	}
	return []error{err}
}

// portCollisions reports each colliding listener pair once, whichever check finds it.
type portCollisions struct {
	seen     map[[2]string]bool
	refusals []error
}

func newPortCollisions() *portCollisions { return &portCollisions{seen: map[[2]string]bool{}} }

func (collisions *portCollisions) add(name, other string) {
	pair := [2]string{min(name, other), max(name, other)}
	if !collisions.seen[pair] {
		collisions.seen[pair] = true
		collisions.refusals = append(collisions.refusals, domain.SettingRefusal(domain.ConfigPortCollision, name, other))
	}
}

func normalizeWebPreviewPort(values environment, tracker *settingTracker, sshPort int, collisions *portCollisions) error {
	const name = "WEB_PREVIEW_HOST_PORT"
	if values[name] == "" {
		values[name] = strconv.Itoa(1024 + (sshPort+30000-1024)%64512)
		tracker.normalize(name, values[name], "derived from SSH_PORT")
	}
	port, err := strconv.Atoi(values[name])
	if err != nil || port < 1024 || port > 65535 || strconv.Itoa(port) != values[name] {
		return domain.SettingRefusal(domain.ConfigSettingCanonicalPort, name)
	}
	profiles := strings.Fields(values["ENVIRONMENT_PROFILES"])
	for _, definition := range tracker.catalog.SettingCatalog() {
		if !definition.HostListener || definition.Name == name {
			continue
		}
		if owner, integration := strings.CutPrefix(definition.Owner, "integration:"); integration && !slices.Contains(strings.Fields(values["CODING_TOOL_INTEGRATIONS"]), owner) {
			continue
		}
		if owner, profile := strings.CutPrefix(definition.Owner, "profile:"); profile && !slices.Contains(profiles, owner) {
			continue
		}
		if listenerPort, err := strconv.Atoi(values[definition.Name]); err == nil && listenerPort == port {
			collisions.add(name, definition.Name)
		}
	}
	return nil
}

// normalizeDerivedPorts returns only a derived-source failure; collisions are
// recorded in collisions.
func normalizeDerivedPorts(values environment, tracker *settingTracker, collisions *portCollisions) error {
	var derive func(string) error
	derive = func(name string) error {
		definition, ok := tracker.catalog.LookupSetting(name)
		if !ok || definition.DefaultFrom == nil || values[name] != "" {
			return nil
		}
		if err := derive(definition.DefaultFrom.Setting); err != nil {
			return err
		}
		source, err := strconv.Atoi(values[definition.DefaultFrom.Setting])
		if err != nil {
			return domain.SettingRefusal(domain.ConfigDerivedPortSource, name)
		}
		width := definition.Maximum - definition.Minimum + 1
		value := (source + definition.DefaultFrom.Offset - definition.Minimum) % width
		if value < 0 {
			value += width
		}
		values[name] = strconv.Itoa(definition.Minimum + value)
		tracker.normalize(name, values[name], "derived from "+definition.DefaultFrom.Setting)
		return nil
	}
	var sources []error
	for _, definition := range tracker.catalog.SettingCatalog() {
		sources = append(sources, derive(definition.Name))
	}
	if err := domain.CollectRefusals(sources...); err != nil {
		return err
	}
	// Integration listeners preserve reservations against configured peer listeners,
	// including packages whose service is stopped or not yet initialized.
	for _, definition := range tracker.catalog.SettingCatalog() {
		owner, integration := strings.CutPrefix(definition.Owner, "integration:")
		if !integration || !definition.HostListener || !slices.Contains(strings.Fields(values["CODING_TOOL_INTEGRATIONS"]), owner) {
			continue
		}
		for _, peer := range tracker.catalog.SettingCatalog() {
			if !peer.HostListener || peer.Name == definition.Name || values[definition.Name] == "" {
				continue
			}
			if other, ok := strings.CutPrefix(peer.Owner, "integration:"); ok && !slices.Contains(strings.Fields(values["CODING_TOOL_INTEGRATIONS"]), other) {
				continue
			}
			if values[peer.Name] == values[definition.Name] {
				collisions.add(definition.Name, peer.Name)
			}
		}
	}
	addHostListenerCollisions(values, tracker.catalog, collisions)
	return nil
}

func validateHostListenerCollisions(values map[string]string, settings Catalog) error {
	collisions := newPortCollisions()
	addHostListenerCollisions(values, settings, collisions)
	return domain.CollectRefusals(collisions.refusals...)
}

func addHostListenerCollisions(values map[string]string, settings Catalog, collisions *portCollisions) {
	ports := map[string]string{}
	for _, definition := range settings.SettingCatalog() {
		if !definition.HostListener {
			continue
		}
		if owner, ok := strings.CutPrefix(definition.Owner, "integration:"); ok && !slices.Contains(strings.Fields(values["CODING_TOOL_INTEGRATIONS"]), owner) {
			continue
		}
		if owner, ok := strings.CutPrefix(definition.Owner, "profile:"); ok && !slices.Contains(strings.Fields(values["ENVIRONMENT_PROFILES"]), owner) {
			continue
		}
		value := values[definition.Name]
		if value == "" {
			continue
		}
		if previous := ports[value]; previous != "" {
			collisions.add(definition.Name, previous)
			continue
		}
		ports[value] = definition.Name
	}
}

func normalizeAgentPersistLinks(
	values environment,
	tracker *settingTracker,
	defaultLayer settingLayerID,
) {
	values["INTEGRATION_HOST_LINKS"] = ""
	assignments := tracker.assignments["HOST_LINKS"]
	if len(assignments) == 0 || assignments[len(assignments)-1].Layer != defaultLayer {
		return
	}
	var selected strings.Builder
	for _, agent := range strings.Fields(values["CODING_TOOL_INTEGRATIONS"]) {
		selected.WriteString(values["AGENT_"+agent+"_PERSIST"])
	}
	values["HOST_LINKS"] = selected.String()
	values["INTEGRATION_HOST_LINKS"] = values["HOST_LINKS"]
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
		return "", domain.NewConfigRefusal(domain.ConfigHomeNotAbsolute)
	}
	return filepath.Clean(configHome), nil
}

func ResolveConfigHome(operatorHome string, source map[string]string) (string, error) {
	values := environmentFrom(source)
	if operatorHome != "" {
		values["SUBYARD_OPERATOR_HOME"] = operatorHome
	}
	if values["SUBYARD_OPERATOR_HOME"] == "" {
		return "", domain.NewConfigRefusal(domain.ConfigOperatorHomeRequired)
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
		return sourceError(source, 0, "SUBYARD_CONFIG_HOME", "scalar settings", "cannot relocate config root; set SUBYARD_CONFIG_HOME before launch", nil)
	}
	return nil
}

func setConfigurationRolePaths(values environment, configHome, yardName string) {
	values["SUBYARD_CONFIG_SHARED_DIR"] = filepath.Join(configHome, "overrides", "shared")
	values["SUBYARD_CONFIG_HOST_DIR"] = filepath.Join(configHome, "overrides", "host")
	values["SUBYARD_CONFIG_SECRETS_DIR"] = filepath.Join(configHome, "secrets")
	values["SUBYARD_CONFIG_GENERATED_DIR"] = filepath.Join(configHome, "generated")
	if yardName == "" {
		yardName = "default"
	}
	values["SUBYARD_CONFIG_YARD_DIR"] = filepath.Join(configHome, "yards", yardName)
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
	excluded map[string]bool,
) error {
	if agentsRoot == "" {
		return nil
	}
	for name, relative := range mappings {
		candidate := filepath.Join(agentsRoot, relative)
		if excluded[candidate] {
			continue
		}
		exists, err := tracker.fileExists(candidate)
		if err != nil {
			return sourceContext(candidate, 0, recognizedSettingKey(tracker.catalog, name), "file settings", err)
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
		return false, sourceAccessError(path, "configuration source", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, sourceError(path, 0, "", "configuration source", "must be a regular non-symlink file", nil)
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
			return tracker.sourceError(name, "must name an absolute regular file", nil)
		}
		path := filepath.Clean(value)
		if snapshot, handled, err := tracker.prospectiveFiles.snapshot(path); handled || err != nil {
			if err != nil || !snapshot.Exists {
				return tracker.sourceError(name, "file setting source is not openable", err)
			}
			tracker.assetValidationInputs = append(tracker.assetValidationInputs, path)
			values[name] = path
			if path != value {
				tracker.normalize(name, path, "normalized absolute path")
			}
			continue
		}
		source := tracker.fileSource(path)
		info, err := os.Lstat(source)
		if err != nil {
			return tracker.sourceError(name, "file setting source is not openable", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return tracker.sourceError(name, "must name a regular non-symlink file", nil)
		}
		file, err := os.Open(source)
		if err != nil {
			return tracker.sourceError(name, "file setting source is not openable", err)
		}
		if err := file.Close(); err != nil {
			return tracker.sourceError(name, "cannot close file setting source", err)
		}
		tracker.assetValidationInputs = append(tracker.assetValidationInputs, path)
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
		"E2E_DISK_BUDGET", "E2E_CACHE_BUDGET", "E2E_DISK_RESERVE", "E2E_MEMORY_RESERVE", "E2E_VM_OVERHEAD",
		"E2E_VM_IMAGE", "E2E_VM_CPU", "E2E_VM_MEMORY", "E2E_VM_DISK", "E2E_VM_SLOT_COUNT", "E2E_VM_BOOT_TIMEOUT",
		"SUBYARD_STATE_DIR", "RESTRICTED_DISK_PATHS",
		"HOST_BASE", "SRV_VOLUME", "ALLOWS_CODING_TOOLS",
		"CODING_TOOL_INTEGRATIONS", "INTEGRATION_HOST_LINKS",
		"VM_FREE_PAGE_REPORTING", "VM_PIN_IPV4", "VM_CPU_WEIGHT", "ALLOWS_HOST_ACCESS", "ALLOWS_PROJECTS",
		"EXCLUSIVE_ENVIRONMENT_PROFILE", "REQUIRED_YARD_KIND", "ROOT_DISK_SIZE", "SRV_VOLUME_TYPE", "SRV_VOLUME_SIZE",
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
	settings := Catalog{}
	if tracker != nil {
		settings = tracker.catalog
	} else {
		var err error
		settings, err = LoadCatalog(filepath.Dir(configDir))
		if err != nil {
			return err
		}
	}
	probe := make(environment, len(values))
	for name, value := range values {
		probe[name] = value
	}
	delete(probe, "YARD_TEMPLATE")
	if err := settings.applyEnvFileValidated(yardFile, probe, ScopeYard, syncSource, nil); err != nil {
		return err
	}
	if err := applyYardTemplateTracked(configDir, yardName, probe["YARD_TEMPLATE"], yardFile, values, tracker); err != nil {
		return err
	}
	if tracker == nil {
		return settings.applyEnvFileValidated(yardFile, values, ScopeYard, syncSource, nil)
	}
	yardLayer := tracker.addLayer("yard", "scalar settings", yardFile, true, settingAny)
	return applyEnvFileTrackedValidated(
		yardFile, values, tracker, yardLayer, ScopeYard, syncSource,
	)
}

func applyYardTemplateTracked(configDir, yardName, template, yardFile string, values environment, tracker *settingTracker) error {
	if template != "" {
		if !domain.SafeName(template) {
			return fmt.Errorf("%w %q in %s", domain.NewConfigRefusal(domain.ConfigYardTemplateInvalid), template, yardFile)
		}
		if template == "e2e-vms" {
			return retiredE2EVMTemplateError(yardName, yardFile)
		}
		templateFile := filepath.Join(configDir, "yards", "profiles", template+".env")
		info, err := os.Stat(templateFile)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("%w %q in %s", domain.NewConfigRefusal(domain.ConfigUnknownYardTemplate), template, yardFile)
			}
			return err
		}
		if info.IsDir() {
			return fmt.Errorf("%w %q in %s", domain.NewConfigRefusal(domain.ConfigUnknownYardTemplate), template, yardFile)
		}
		settings := Catalog{}
		if tracker != nil {
			settings = tracker.catalog
		} else {
			var err error
			settings, err = LoadCatalog(filepath.Dir(configDir))
			if err != nil {
				return err
			}
		}
		if tracker == nil {
			if err := settings.applyEnvFileValidated(
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
	return nil
}

func applyEnvFileTracked(
	path string,
	values environment,
	tracker *settingTracker,
	layer settingLayerID,
) error {
	return applyEnvFileObserved(path, values, func(name, value string, line int) {
		tracker.record(layer, name, value, path, line, "")
	}, tracker.prospectiveReader())
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
	source := tracker.fileSource(path)
	return tracker.catalog.applyEnvFileValidatedFrom(path, source, values, scope, requireSyncable,
		func(name, value string, line int) {
			tracker.record(layer, name, value, path, line, "")
		}, func(name, value string, line int) {
			tracker.observeProfileOnly(layer, name, value, path, line)
		}, tracker.prospectiveReader())
}

func (settings Catalog) applyEnvFileValidated(
	path string,
	values environment,
	scope SettingScope,
	requireSyncable bool,
	observer assignmentObserver,
) error {
	return settings.applyEnvFileValidatedFrom(path, path, values, scope, requireSyncable, observer, nil)
}

func (settings Catalog) applyEnvFileValidatedFrom(path, source string, values environment, scope SettingScope, requireSyncable bool, observer assignmentObserver, ignored ignoredObserver, readers ...func(string) ([]byte, error)) error {
	if len(readers) != 0 && readers[0] != nil {
		payload, err := readers[0](source)
		if err != nil {
			return sourceAccessError(path, "scalar settings", err)
		}
		return settings.applyEnvReaderValidatedIgnoring(bytes.NewReader(payload), path, values, scope, requireSyncable, observer, ignored)
	}
	file, err := os.Open(source)
	if err != nil {
		return sourceAccessError(path, "scalar settings", err)
	}
	defer file.Close()
	return settings.applyEnvReaderValidatedIgnoring(file, path, values, scope, requireSyncable, observer, ignored)
}

func (settings Catalog) applyEnvReaderValidated(reader io.Reader, path string, values environment, scope SettingScope, requireSyncable bool, observer assignmentObserver) error {
	return settings.applyEnvReaderValidatedIgnoring(reader, path, values, scope, requireSyncable, observer, nil)
}

// ignoredObserver receives an ignored profile-only assignment and its line.
type ignoredObserver func(name, value string, line int)

// applyEnvReaderValidatedIgnoring evaluates and validates one scalar layer.
// Outside shipped scope, a profile-only assignment is ignored like a retired
// setting while the file is read (see discardPolicy). It is reported to
// ignored, never validated, applied or observed as a setting.
func (settings Catalog) applyEnvReaderValidatedIgnoring(reader io.Reader, path string, values environment, scope SettingScope, requireSyncable bool, observer assignmentObserver, ignored ignoredObserver) error {
	probe := cloneEnvironment(values)
	var assignments []observedAssignment
	var discarded []observedAssignment
	var content bytes.Buffer
	policy := discardPolicy{retired: settings.retiredSetting}
	if scope != ScopeShipped {
		policy.ignored = IsProfileOnlySetting
	}
	if err := applyEnvReaderObservedRetired(io.TeeReader(reader, &content), path, probe, func(name, value string, line int) {
		assignments = append(assignments, observedAssignment{name: name, value: value, line: line})
	}, policy, func(name, value string, line int) {
		if policy.ignores(name) {
			discarded = append(discarded, observedAssignment{name: name, value: value, line: line})
		}
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
		if settings.retiredSetting(assignment.name) {
			delete(probe, assignment.name)
			continue
		}
		canonical := canonicalSettingName(assignment.name)
		assignment.value = normalizeLegacySelection(assignment.name, assignment.value)
		if previous, exists := seen[canonical]; exists && previous.input != assignment.name &&
			previous.value != assignment.value {
			return sourceError(path, assignment.line, recognizedSettingKey(settings, canonical), "scalar settings", "conflicting legacy and canonical settings", nil)
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
		syncable := requireSyncable
		if syncable && assignment.name == "SSH_PORT" {
			if err := LegacyGitSSHPort(path, content.Bytes()); err != nil {
				return sourceContext(path, assignment.line, "SSH_PORT", "scalar settings", err)
			}
			syncable = false
		}
		if err := settings.ValidateSetting(scope, assignment.name, assignment.value, syncable); err != nil {
			return sourceContext(path, assignment.line, recognizedSettingKey(settings, assignment.name), "scalar settings", err)
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
	if ignored != nil {
		for _, assignment := range discarded {
			ignored(assignment.name, assignment.value, assignment.line)
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

// Retired inputs may remain in persisted files from older releases. They are
// parsed but discarded, never exposed as writable settings or effective values.
func isRetiredSetting(name string) bool {
	switch name {
	case "CODEX_VERSION", "CODEX_SHA256_AMD64", "CODEX_SHA256_ARM64":
		return true
	default:
		// The explicit integration cleanup hook no longer exists.
		agent, cleanup := strings.CutSuffix(strings.TrimPrefix(name, "AGENT_"), "_CLEANUP")
		return cleanup && strings.HasPrefix(name, "AGENT_") && domain.SafeName(agent)
	}
}

func discardRetiredSettings(values environment) {
	for name := range values {
		if isRetiredSetting(name) {
			delete(values, name)
		}
	}
}

func IsLegacySetting(name string) bool {
	_, legacy := legacySettingNames[name]
	return legacy || isRetiredSetting(name)
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
	discardRetiredSettings(values)
	for legacy, canonical := range legacySettingNames {
		legacyValue, hasLegacy := values[legacy]
		if !hasLegacy {
			continue
		}
		legacyValue = normalizeLegacySelection(legacy, legacyValue)
		if canonicalValue, hasCanonical := values[canonical]; hasCanonical && canonicalValue != legacyValue {
			// Command values may be secrets, so even the error text names only settings.
			return domain.SettingRefusal(domain.ConfigConflictingCommandSettings, canonical, legacy)
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
	return &retiredYardTemplateError{yardName: yardName, yardFile: yardFile}
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
	return FindYardRegistrationFile(filepath.Join(root, "config"), configHome, name)
}

func findYardFile(root, name string, values environment, explicit []string) (string, error) {
	if len(explicit) == 0 {
		return FindYardRegistrationFile(
			values["SUBYARD_CONFIG_DIR"], values["SUBYARD_CONFIG_HOME"], name,
		)
	}
	return findYardFileAt(root, name, values["SUBYARD_CONFIG_HOME"], explicit)
}

func findYardFileAt(root, name, configHome string, explicit []string) (string, error) {
	if len(explicit) == 0 {
		return FindYardRegistrationFile(filepath.Join(root, "config"), configHome, name)
	}
	var candidates []string
	for _, directory := range explicit {
		candidates = append(candidates,
			filepath.Join(directory, name, "config.env"),
			filepath.Join(directory, name+".env"),
		)
	}
	return findFirstYardFile(candidates, name)
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
	if tracker != nil {
		assignments := tracker.assignments[name]
		if len(assignments) > 0 {
			previous := tracker.layers[assignments[len(assignments)-1].Layer]
			if previous.Scope == "yard" && (previous.Role == "scalar settings" || previous.Role == "Git scalar settings") {
				return
			}
		}
	}
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
	setDefault(values, "E2E_DISK_BUDGET", DefaultTestVMDiskBudget, tracker, defaultLayer)
	setDefault(values, "E2E_CACHE_BUDGET", DefaultTestVMCacheBudget, tracker, defaultLayer)
	setDefault(values, "E2E_DISK_RESERVE", DefaultTestVMDiskReserve, tracker, defaultLayer)
	setDefault(values, "E2E_MEMORY_RESERVE", DefaultTestVMMemoryReserve, tracker, defaultLayer)
	setDefault(values, "E2E_VM_OVERHEAD", DefaultTestVMOverhead, tracker, defaultLayer)
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
	// Each check reads only its own settings, so every refusal is reported.
	var refusals []error
	if hostBase != restrictedBase {
		refusals = append(refusals, domain.NewConfigRefusal(domain.ConfigHostBaseMismatch))
	}
	stateDir := values["SUBYARD_STATE_DIR"]
	if stateDir == "" {
		stateDir = filepath.Join(configHome, "projects")
	}
	sshPort, err := optionalInt(values["SSH_PORT"])
	if err != nil {
		refusals = append(refusals, fmt.Errorf("%w: %w", domain.SettingRefusal(domain.ConfigSettingNumeric, "SSH_PORT"), err))
	}
	devUID, err := strconv.Atoi(values["DEV_UID"])
	if err != nil {
		refusals = append(refusals, domain.SettingRefusal(domain.ConfigSettingNumeric, "DEV_UID"))
	}
	forwardAgent, devSudo, nestedE2EVMs := zeroOne(values, "FORWARD_SSH_AGENT", &refusals), zeroOne(values, "DEV_SUDO", &refusals), zeroOne(values, "NESTED_E2E_VMS", &refusals)
	rawE2EVMCPU := values["E2E_VM_CPU"]
	e2eVMCPU, err := resolveE2EVMCPU(rawE2EVMCPU, runtime.NumCPU())
	if err != nil {
		refusals = append(refusals, err)
	} else {
		values["E2E_VM_CPU"] = e2eVMCPU
		if e2eVMCPU != rawE2EVMCPU {
			tracker.record(
				normalizationLayer, "E2E_VM_CPU", e2eVMCPU, "", 0,
				"resolved from "+rawE2EVMCPU,
			)
		}
	}
	refusals = append(refusals, validateE2EConfig(values, err == nil))
	if err := domain.CollectRefusals(refusals...); err != nil {
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
			return "", domain.NewConfigRefusal(domain.ConfigE2EVMCPU)
		}
		count, err := strconv.Atoi(value)
		if err != nil || count < 1 {
			return "", domain.NewConfigRefusal(domain.ConfigE2EVMCPU)
		}
		return strconv.Itoa(count), nil
	}
	if hostCPUs < 1 {
		return "", domain.NewConfigRefusal(domain.ConfigHostCPUUnavailable)
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

func zeroOne(values environment, name string, refusals *[]error) bool {
	switch values[name] {
	case "0":
		return false
	case "1":
		return true
	default:
		*refusals = append(*refusals, domain.SettingRefusal(domain.ConfigSettingZeroOne, name))
		return false
	}
}

// validateE2EConfig reports every independent E2E setting refusal; checkCPU is
// false when E2E_VM_CPU already failed to resolve.
func validateE2EConfig(values environment, checkCPU bool) error {
	var refusals []error
	if value, err := strconv.Atoi(values["E2E_VM_CPU"]); checkCPU && (err != nil || value < 1) {
		refusals = append(refusals, domain.SettingRefusal(domain.ConfigSettingPositiveInteger, "E2E_VM_CPU"))
	}
	sizeMiB := func(name string) (int, bool) {
		value := values[name]
		factor, raw := 1, strings.TrimSuffix(value, "MiB")
		if strings.HasSuffix(value, "GiB") {
			factor, raw = 1024, strings.TrimSuffix(value, "GiB")
		}
		amount, err := strconv.Atoi(raw)
		if raw == value || err != nil || amount < 1 {
			refusals = append(refusals, domain.SettingRefusal(domain.ConfigSettingPositiveSize, name))
			return 0, false
		}
		return amount * factor, true
	}
	for _, name := range []string{"E2E_DISK_BUDGET", "E2E_CACHE_BUDGET", "E2E_DISK_RESERVE", "E2E_MEMORY_RESERVE", "E2E_VM_OVERHEAD", "E2E_VM_MEMORY"} {
		if name != "E2E_DISK_BUDGET" || values[name] != "0GiB" {
			sizeMiB(name)
		}
	}
	if disk, ok := sizeMiB("E2E_VM_DISK"); ok && disk < 10*1024 {
		refusals = append(refusals, domain.NewConfigRefusal(domain.ConfigE2EVMDiskMinimum))
	}
	for _, bound := range []struct {
		name     string
		min, max int
		code     domain.ConfigRefusalCode
	}{{"E2E_VM_SLOT_COUNT", 1, 1<<20 - 1, domain.ConfigE2EVMSlotCountRange}, {"E2E_VM_BOOT_TIMEOUT", 30, 1800, domain.ConfigE2EVMBootTimeoutRange}} {
		if value, err := strconv.Atoi(values[bound.name]); err != nil || value < bound.min || value > bound.max {
			refusals = append(refusals, domain.NewConfigRefusal(bound.code))
		}
	}
	image := values["E2E_VM_IMAGE"]
	if image == "" || strings.HasPrefix(image, "-") || strings.ContainsFunc(image, func(char rune) bool {
		return !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._:/@+-", char)
	}) {
		refusals = append(refusals, domain.NewConfigRefusal(domain.ConfigE2EVMImageUnsafe))
	}
	return domain.CollectRefusals(refusals...)
}

func applyEnvFileValidated(path string, values environment, scope SettingScope, syncable bool, observer assignmentObserver) error {
	return (Catalog{}).applyEnvFileValidated(path, values, scope, syncable, observer)
}

// LayerAssignment retains the source line for every evaluated assignment,
// including effects from parameter defaults.
type LayerAssignment struct {
	Name, Value string
	Line        int
}
type LayerEvaluation struct {
	Values      map[string]string
	Assignments []LayerAssignment
}

// EvaluateLayer evaluates and validates one scalar layer atomically, preserving
// assignment order, scopes and legacy conflict checks without opening source.
func (settings Catalog) EvaluateLayer(reader io.Reader, source string, base map[string]string, scope SettingScope, requireSyncable bool) (LayerEvaluation, error) {
	result := LayerEvaluation{Values: cloneEnvironment(base)}
	err := settings.applyEnvReaderValidated(reader, source, result.Values, scope, requireSyncable, func(name, value string, line int) {
		result.Assignments = append(result.Assignments, LayerAssignment{Name: name, Value: value, Line: line})
	})
	if err != nil {
		return LayerEvaluation{}, err
	}
	return result, nil
}

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

type initMode uint8

const (
	initReconcile initMode = iota
	initConfigs
	initReset
)

const forwardedSSHAgentConsequence = "SSH agent forwarding enables git push and other host access from " +
	"inside the yard with the forwarded, write-enabled credential while the SSH session is active. " +
	"No private key is copied into the yard, but any process that can reach the forwarded agent can " +
	"exercise it; agent ask-rules are a UX safeguard, not a security boundary"

type initArguments struct {
	mode    initMode
	profile string
}

type initBootstrap struct {
	profile    string
	sourcePath string
	targetPath string
	content    []byte
}

type initExecution struct {
	resourceCommand       string
	resourceArguments     []string
	provisionProfile      string
	requestedProfile      string
	profileProvision      *provisionExecution
	profileSetup          *initProfileSet
	operationID           string
	integrationSelection  *initIntegrationSelection
	integrationBaseline   *initIntegrationBaseline
	integrationAdoption   reconcileruntime.IntegrationPlan
	loaded                config.Loaded
	mode                  initMode
	bootstrap             *initBootstrap
	plan                  application.ReconcilePlan
	approvedPlan          application.ReconcilePlan
	approvedFinalize      application.ReconcilePlan
	inputBaseline         *ownerInputBaseline
	teardownResources     []ports.TeardownResource
	resetBaseline         *resetTeardownBaseline
	platform              ports.InitPlatform
	powerYards            []domain.Context
	hostID                string
	hostIDPending         bool
	configsChanged        bool
	hooksApplicable       bool
	orphanIngress         *orphanIngressPlan
	orphanIngressDeferred bool
	runtimePlan           *reconcileruntime.ProfileRuntimePlan
	hookPlan              *reconcileruntime.ProjectHookPlan
	hookProjects          []domain.ProjectRecord
}

func (execution *initExecution) refreshOrphanIngress(ctx context.Context, cli *CLI) error {
	err := execution.orphanIngress.refresh(ctx, cli, execution.loaded)
	if execution.orphanIngressDeferred && errors.Is(err, errOrphanIngressAccessDeferred) {
		return nil
	}
	return err
}

func (execution *initExecution) finishDeferredOrphanIngress(ctx context.Context, cli *CLI, output io.Writer) error {
	if !execution.orphanIngressDeferred {
		return nil
	}
	if err := execution.orphanIngress.refresh(ctx, cli, execution.loaded); err != nil {
		if errors.Is(err, domain.ErrPlanStale) {
			return fmt.Errorf("%w: deselected public ingress was discovered after owner Incus access was restored; rerun init for a new assessment and approval", domain.ErrPlanStale)
		}
		return fmt.Errorf("inspect deselected public ingress after restoring owner Incus access: %w", err)
	}
	return nil
}

// approvedStages projects only authorization captured during read-only prepare.
func (execution *initExecution) approvedStages(stage application.ReconcileStage) application.ReconcilePlan {
	for _, step := range execution.approvedPlan.Steps {
		if step.Stage == stage {
			return application.ReconcilePlan{Steps: []application.ReconcileStep{step}}
		}
	}
	return application.ReconcilePlan{}
}

type initReporter struct{ output io.Writer }

func (reporter initReporter) StageSkipped(stage application.ReconcileStage) {
	fmt.Fprintf(reporter.output, "  [ .. ] %s (already converged)\n", stage.ID)
}

func (reporter initReporter) StageStarted(stage application.ReconcileStage) {
	fmt.Fprintf(reporter.output, "  [ .. ] %s\n", stage.ID)
}

func parseInitArguments(arguments []string) (initArguments, error) {
	request := initArguments{mode: initReconcile}
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		switch argument {
		case "-y", "--yes":
		case "--configs":
			if request.mode == initReset || request.profile != "" {
				return initArguments{}, errors.New("--configs, --reset and --profile cannot be used together")
			}
			request.mode = initConfigs
		case "--reset":
			if request.mode == initConfigs || request.profile != "" {
				return initArguments{}, errors.New("--configs, --reset and --profile cannot be used together")
			}
			request.mode = initReset
		case "--profile":
			if index+1 >= len(arguments) {
				return initArguments{}, errors.New("--profile needs a value")
			}
			index++
			if request.profile != "" {
				return initArguments{}, errors.New("--profile may be specified only once")
			}
			if request.mode != initReconcile {
				return initArguments{}, errors.New("--configs, --reset and --profile cannot be used together")
			}
			request.profile = arguments[index]
			if !domain.SafeName(request.profile) {
				return initArguments{}, fmt.Errorf("invalid profile %q", request.profile)
			}
		default:
			return initArguments{}, fmt.Errorf("unknown option %q", argument)
		}
	}
	return request, nil
}

func (cli *CLI) loadInitContext(
	yard string,
	explicit bool,
	arguments []string,
) (config.Loaded, *initBootstrap, error) {
	request, err := parseInitArguments(arguments)
	if err != nil {
		return config.Loaded{}, nil, err
	}
	if request.profile == "" {
		loaded, err := cli.loadContext(yard)
		return loaded, nil, err
	}
	if !explicit || yard == "" || yard == "default" {
		return config.Loaded{}, nil, errors.New(
			"--profile requires selecting a non-default yard with -Y",
		)
	}
	source := filepath.Join(
		cli.options.RepositoryRoot, "config", "profiles", request.profile, "yard.env",
	)
	info, err := os.Lstat(source)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return config.Loaded{}, nil, fmt.Errorf(
				"profile %q has no named-yard preset", request.profile,
			)
		}
		return config.Loaded{}, nil, fmt.Errorf("inspect profile preset: %w", err)
	}
	if !info.Mode().IsRegular() {
		return config.Loaded{}, nil, fmt.Errorf(
			"profile %q named-yard preset is not a regular file", request.profile,
		)
	}
	content, err := os.ReadFile(source)
	if err != nil {
		return config.Loaded{}, nil, fmt.Errorf("read profile preset: %w", err)
	}
	operatorHome := cli.env["SUBYARD_OPERATOR_HOME"]
	if operatorHome == "" {
		operatorHome = cli.env["HOME"]
	}
	configHome, err := config.ResolveConfigHome(operatorHome, cli.env)
	if err != nil {
		return config.Loaded{}, nil, err
	}
	target := filepath.Join(configHome, "yards", yard, "config.env")
	configDir := cli.env["SUBYARD_CONFIG_DIR"]
	if configDir == "" {
		configDir = filepath.Join(cli.options.RepositoryRoot, "config")
	}
	existingPath, existingErr := config.FindYardRegistrationFile(configDir, configHome, yard)
	if existingErr == nil {
		existing, err := cli.resolveContextWithYardSettings(yard, existingPath)
		if err != nil {
			return config.Loaded{}, nil, err
		}
		if existing.Context.AccessKind != domain.AccessLocal {
			return config.Loaded{}, nil, errors.New("--profile is only supported for local yards")
		}
		preset, err := cli.resolveContextWithYardSettings(yard, source)
		if err != nil {
			return config.Loaded{}, nil, err
		}
		if preset.Context.AccessKind != domain.AccessLocal {
			return config.Loaded{}, nil, errors.New("--profile is only supported for local yards")
		}
		if name := cli.firstInitProfileConflict(existing, existingPath, preset, source); name != "" {
			return config.Loaded{}, nil, fmt.Errorf(
				"named yard %q conflicts with profile %q at setting %s; use plain init for an intentionally customized yard",
				yard, request.profile, name,
			)
		}
		if name := cli.firstInitProfileOverride(existing, preset, source); name != "" {
			return config.Loaded{}, nil, fmt.Errorf(
				"command environment overrides profile %q at setting %s",
				request.profile, name,
			)
		}
		cli.adoptContext(existing)
		return existing, nil, nil
	} else if !errors.Is(existingErr, config.ErrUnknownYard) {
		return config.Loaded{}, nil, fmt.Errorf("inspect named-yard definition: %w", existingErr)
	}
	loaded, err := cli.loadContextWithYardSettings(yard, source)
	if err != nil {
		return config.Loaded{}, nil, err
	}
	if loaded.Context.AccessKind != domain.AccessLocal {
		return config.Loaded{}, nil, errors.New("--profile is only supported for local yards")
	}
	if name := cli.firstInitProfileOverride(loaded, loaded, source); name != "" {
		return config.Loaded{}, nil, fmt.Errorf(
			"command environment overrides profile %q at setting %s",
			request.profile, name,
		)
	}
	return loaded, &initBootstrap{
		profile: request.profile, sourcePath: source, targetPath: target, content: content,
	}, nil
}

func (cli *CLI) firstInitProfileOverride(
	loaded config.Loaded,
	preset config.Loaded,
	presetPath string,
) string {
	presetValues := yardAssignments(preset, presetPath)
	names := make([]string, 0, len(presetValues))
	for name := range presetValues {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if cli.initProfileEndpointSetting(preset, name) {
			for _, resolution := range loaded.Settings[name].Resolutions {
				if resolution.Scope == string(config.ScopeYard) && resolution.Status != "unset" {
					presetValues[name] = resolution.Value
				}
			}
		}
		if loaded.Environment[name] != presetValues[name] {
			return name
		}
	}
	return ""
}

func (cli *CLI) firstInitProfileConflict(
	existing config.Loaded,
	existingPath string,
	preset config.Loaded,
	presetPath string,
) string {
	existingValues := yardAssignments(existing, existingPath)
	presetValues := yardAssignments(preset, presetPath)
	names := make([]string, 0, len(presetValues))
	for name := range presetValues {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if cli.initProfileEndpointSetting(preset, name) {
			continue
		}
		if value, ok := existingValues[name]; !ok || value != presetValues[name] {
			return name
		}
	}
	return ""
}

func yardAssignments(loaded config.Loaded, path string) map[string]string {
	values := map[string]string{}
	path = filepath.Clean(path)
	for name, trace := range loaded.Settings {
		for _, resolution := range trace.Resolutions {
			if resolution.Scope == string(config.ScopeYard) &&
				resolution.Status != "unset" && filepath.Clean(resolution.Path) == path {
				values[name] = resolution.Value
			}
		}
	}
	return values
}

func (cli *CLI) initPlatform(loaded config.Loaded, powerYards []domain.Context) ports.InitPlatform {
	return cli.initPlatformWithDispatcher(loaded, powerYards, cli.options.DispatcherPath)
}

func (cli *CLI) initPlatformWithDispatcher(
	loaded config.Loaded,
	powerYards []domain.Context,
	dispatcherPath string,
) ports.InitPlatform {
	if cli.options.InitPlatform != nil {
		return cli.options.InitPlatform
	}
	environment := structuredCommandContext(loaded)
	if cli.retainedAdapterCompatibility {
		config.AddLegacySettingAliases(environment)
	}
	environment["SUBYARD_DISPATCHER_PATH"] = dispatcherPath
	environment["SUBYARD_POWER_ENGINE_SOURCE"] = dispatcherPath
	incusPort, executor := cli.statusPorts()
	configWriter, _ := incusPort.(ports.InstanceConfigWriter)
	return reconcileruntime.Runtime{
		Profiles:          loaded.Catalog.Profiles(),
		RepositoryRoot:    cli.options.RepositoryRoot,
		Environment:       environmentList(cli.env, environment),
		LaunchEnvironment: environmentList(cli.baseEnv, nil),
		Stdin:             cli.options.Stdin,
		Stdout:            cli.options.Stderr,
		Stderr:            cli.options.Stderr,
		Incus:             incusPort,
		NetworkPolicy:     cli.networkService(powerYards),
		ConfigWriter:      configWriter,
		Executor:          executor,
		Yard:              loaded.Context,
		PowerYards:        powerYards,
		SRVPool:           loaded.Environment["SRV_POOL"],
		SRVVolume:         loaded.Environment["SRV_VOLUME"],
	}
}

func (cli *CLI) powerYardContexts(current config.Loaded) ([]domain.Context, error) {
	names, err := config.YardNames(
		current.Context.Paths.ConfigDir, current.Context.Paths.ConfigHome,
	)
	if err != nil {
		return nil, err
	}
	operatorHome := current.Context.Paths.OperatorHome
	result := make([]domain.Context, 0, len(names))
	for _, name := range names {
		environment := make(map[string]string, len(cli.baseEnv))
		for key, value := range cli.baseEnv {
			environment[key] = value
		}
		environment["SUBYARD_OPERATOR_HOME"] = operatorHome
		environment["SUBYARD_CONFIG_HOME"] = current.Context.Paths.ConfigHome
		environment["SUBYARD_HOME"] = current.Context.Paths.DataHome
		loaded, err := config.Load(config.LoadOptions{
			Catalog:        &cli.catalog,
			RepositoryRoot: cli.options.RepositoryRoot,
			OperatorHome:   operatorHome,
			YardName:       name,
			Environment:    environment,
		})
		if err != nil {
			return nil, fmt.Errorf("load power context %q: %w", name, err)
		}
		if loaded.Context.AccessKind != domain.AccessRemote {
			result = append(result, loaded.Context)
		}
	}
	return result, nil
}

func (cli *CLI) prepareInitExecution(
	ctx context.Context,
	loaded config.Loaded,
	arguments []string,
	bootstrap *initBootstrap,
) (*initExecution, error) {
	request, err := parseInitArguments(arguments)
	if err != nil {
		return nil, err
	}
	mode := request.mode
	baseline, err := captureInitIntegrationBaseline(loaded)
	if err != nil {
		return nil, err
	}
	var selection *initIntegrationSelection
	// The durable release transition owns restricted-role registration bytes and
	// paths until verification. Public init can adopt them after it completes.
	transitionOwnsRegistration := cli.releaseTransitionChild && !loaded.Integrations.AllowsCodingTools
	if mode != initConfigs && !transitionOwnsRegistration {
		loaded, selection, err = cli.prepareInitIntegrationSelection(ctx, loaded, bootstrap)
		if err != nil {
			return nil, err
		}
	}
	var platform ports.InitPlatform
	var powerYards []domain.Context
	if cli.options.InitPlatform != nil {
		platform = cli.options.InitPlatform
	} else {
		powerYards, err = cli.powerYardContexts(loaded)
		if err != nil {
			return nil, err
		}
		platform = cli.initPlatform(loaded, powerYards)
	}
	execution := &initExecution{
		requestedProfile: request.profile, loaded: loaded, mode: mode, bootstrap: bootstrap, platform: platform, powerYards: powerYards,
		integrationSelection: selection, integrationBaseline: baseline,
	}
	if runtime, ok := execution.platform.(reconcileruntime.Runtime); ok {
		runtime.InitProfile = request.profile
		if mode == initReset {
			execution.resetBaseline, err = cli.prepareResetTeardownBaseline(ctx, loaded)
			if err != nil {
				return nil, err
			}
			execution.teardownResources = execution.resetBaseline.resources()
			runtime.TeardownResources = slices.Clone(execution.teardownResources)
			runtime.TeardownArtifacts = execution.resetBaseline.artifacts()
		}
		execution.platform = runtime
	}
	if mode == initReconcile && cli.options.InitPlatform == nil {
		execution.orphanIngress, err = cli.prepareOrphanIngress(ctx, loaded)
		if errors.Is(err, errOrphanIngressAccessDeferred) {
			execution.orphanIngressDeferred = true
		} else if err != nil {
			return nil, fmt.Errorf("inspect deselected public ingress: %w", err)
		}

	}
	if bootstrap == nil && mode == initReconcile && slices.Equal(baseline.Selection.Requested, loaded.Integrations.Requested) {
		execution.platform, execution.integrationAdoption, err = prepareLegacyIntegrationAdoption(ctx, baseline.Selection, execution.platform)
		if err != nil {
			return nil, err
		}
	}
	execution.hostID, execution.hostIDPending, err = configsync.ResolveHostID(
		loaded.Context.Paths.ConfigHome, loaded.Environment,
	)
	if err != nil {
		return nil, err
	}
	if mode == initConfigs {
		if err := execution.checkReleaseConfigOwnership(ctx, cli); err != nil {
			return nil, err
		}
		converged, err := execution.platform.ConfigsConverged(ctx)
		if err != nil {
			return nil, fmt.Errorf("inspect agent configuration: %w", err)
		}
		execution.configsChanged = !converged
		return execution, nil
	}
	stages := application.InitStages(loaded.Context)
	if mode == initReset {
		execution.plan.Steps = make([]application.ReconcileStep, 0, len(stages))
		for _, stage := range stages {
			execution.plan.Steps = append(execution.plan.Steps, application.ReconcileStep{Stage: stage, Conditional: true})
		}
	} else {
		execution.plan, err = (application.Reconciler{Stages: stages, Runner: execution.platform}).Plan(ctx)
		if err != nil {
			return nil, err
		}
	}
	execution.approvedPlan = application.ReconcilePlan{Steps: slices.Clone(execution.plan.Steps)}
	execution.approvedFinalize = application.ReconcilePlan{Steps: []application.ReconcileStep{{Stage: application.FinalizeStage(), Conditional: true}}}
	if runtime, ok := execution.platform.(reconcileruntime.Runtime); ok {
		unavailable := len(execution.plan.Steps) != 0 && !execution.plan.Steps[0].Converged
		dependent := slices.ContainsFunc(execution.approvedPlan.Steps, func(step application.ReconcileStep) bool {
			return step.Stage.ID == ports.ReconcileStageProvision && !step.Converged
		})
		execution.runtimePlan, err = runtime.PrepareProfileRuntimes(ctx, unavailable, mode == initReset, dependent)
		if err != nil {
			return nil, err
		}
		runtime.RuntimePlan = execution.runtimePlan
		// An approved instance repair can temporarily boot an intentionally stopped
		// yard. Capture bounded conditional hooks before that prerequisite runs.
		temporaryPower := slices.ContainsFunc(execution.approvedPlan.Steps, func(step application.ReconcileStep) bool {
			return step.Stage.ID == ports.ReconcileStageInstance && !step.Converged
		})
		execution.hookPlan, err = runtime.PrepareProjectHooks(ctx, unavailable || mode == initReset, temporaryPower)
		if err != nil {
			return nil, err
		}
		store, err := openProjectStoreReadOnly(loaded.Context.Paths.StateDir)
		if err != nil {
			return nil, err
		}
		execution.hookProjects, err = store.List(ctx)
		if err != nil {
			return nil, err
		}
		execution.hookPlan.CaptureOwnerProjects(execution.hookProjects)
		runtime.HookPlan = execution.hookPlan
		execution.platform = runtime
	}
	if execution.plan.Pending() != 0 || bootstrap != nil {
		if err := execution.platform.Preflight(ctx, mode == initReset); err != nil {
			return nil, fmt.Errorf("host preflight failed: %w", err)
		}
	}
	if execution.hooksOnly() {
		execution.hooksApplicable, err = execution.platform.ProjectHooksApplicable(ctx)
		if err != nil {
			return nil, err
		}
	}
	return execution, nil
}

func (execution *initExecution) consequences() []string {
	if execution.hooksOnly() {
		return []string{"retry installed project hooks once for active resources"}
	}
	hostIDConsequences := execution.profileProvisionConsequences()
	hostIDConsequences = append(hostIDConsequences, integrationAdoptionConsequences(execution.loaded.Context.YardName, execution.integrationAdoption)...)
	hostIDConsequences = append(hostIDConsequences, execution.profileSetup.consequences()...)
	if execution.integrationSelection != nil {
		hostIDConsequences = append(hostIDConsequences, "record the selected yard's requested integration set")
	}
	if execution.bootstrap != nil {
		hostIDConsequences = append(hostIDConsequences,
			"create named yard definition from profile "+execution.bootstrap.profile)
	}
	if execution.hostIDPending {
		hostIDConsequences = append(hostIDConsequences, "record owner HostID "+execution.hostID)
	}
	hostIDConsequences = append(hostIDConsequences, execution.orphanIngress.consequences()...)
	if execution.orphanIngressDeferred {
		hostIDConsequences = append(hostIDConsequences,
			"restore owner Incus access and inspect deselected public ingress; any discovered route requires a new init approval")
	}
	switch execution.mode {
	case initConfigs:
		return append(hostIDConsequences, "refresh in-yard agent instructions and default configs")
	case initReset:
		result := []string{"delete the yard instance and its disk data"}
		for _, step := range execution.plan.Steps {
			result = execution.appendStageConsequences(result, step)
		}
		return append(hostIDConsequences, result...)
	default:
		result := make([]string, 0, execution.plan.Pending())
		for _, step := range execution.plan.Steps {
			if !step.Converged {
				result = execution.appendStageConsequences(result, step)
			}
		}
		return append(hostIDConsequences, result...)
	}
}

func (execution *initExecution) appendStageConsequences(
	result []string,
	step application.ReconcileStep,
) []string {
	result = append(result, step.Stage.Label)
	if step.Stage.ID == ports.ReconcileStageSSH && execution.loaded.Context.ForwardSSHAgent {
		result = append(result, forwardedSSHAgentConsequence)
	}
	return result
}

func (execution *initExecution) actionPlan() (domain.ActionID, domain.ActionDelta, error) {
	if execution == nil {
		return "", domain.ActionDelta{}, errors.New("init execution is required")
	}
	action := domain.ActionID("yard.init.reconcile")
	changed := execution.profileSetup != nil || execution.profileProvisionChanged() || execution.plan.Pending() != 0 || execution.bootstrap != nil || execution.hostIDPending || execution.integrationSelection != nil || execution.orphanIngress != nil || execution.orphanIngressDeferred
	switch execution.mode {
	case initReconcile:
		if execution.hooksOnly() {
			action, changed = "yard.init.project-hooks", execution.hooksApplicable
		}
	case initConfigs:
		action = "yard.init.configs"
		changed = execution.configsChanged || execution.bootstrap != nil || execution.hostIDPending
	case initReset:
		action = "yard.init.reset"
		changed = true
	default:
		return "", domain.ActionDelta{}, errors.New("invalid init mode")
	}
	delta := domain.ActionDelta{Changed: changed}
	if changed {
		delta.Consequences = execution.consequences()
	}
	return action, delta, nil
}

func (execution *initExecution) hooksOnly() bool {
	return execution.profileSetup == nil && !execution.profileProvisionChanged() && execution.mode == initReconcile && execution.plan.Pending() == 0 &&
		execution.bootstrap == nil && !execution.hostIDPending && execution.integrationSelection == nil && execution.orphanIngress == nil && !execution.orphanIngressDeferred
}

func (execution *initExecution) validateProfileRepair(ctx context.Context, cli *CLI) error {
	if cli.profileInitRepair == nil {
		return nil
	}
	if execution.mode != initReconcile || execution.bootstrap != nil || execution.hostIDPending {
		return errors.New("release repair requires ordinary init of an existing yard; run yard update first")
	}
	assessment, err := cli.assessConfigTarget(ctx,
		configTarget{Name: execution.loaded.Context.YardName, Loaded: execution.loaded}, true)
	if err != nil {
		return err
	}
	if !cli.profileInitRepair.matchesRequestedConfigs([]configTargetAssessment{assessment}) {
		return errors.New("init release repair requires the persisted yard configuration without overrides")
	}
	return nil
}

func (execution *initExecution) refreshAssessment(ctx context.Context) error {
	if execution == nil {
		return errors.New("init execution is required")
	}
	if err := execution.checkHookProjects(ctx); err != nil {
		return err
	}
	if err := execution.checkNativePlans(ctx, true); err != nil {
		return err
	}
	if err := execution.profileSetup.check(); err != nil {
		return err
	}
	if err := execution.checkIntegrationAdoption(ctx); err != nil {
		return err
	}
	hostID, pending, err := configsync.ResolveHostID(
		execution.loaded.Context.Paths.ConfigHome, execution.loaded.Environment,
	)
	if err != nil {
		return err
	}
	if hostID != execution.hostID {
		return fmt.Errorf("%w: owner HostID changed after planning", domain.ErrPlanStale)
	}
	execution.hostID = hostID
	execution.hostIDPending = pending
	switch execution.mode {
	case initConfigs:
		converged, err := execution.platform.ConfigsConverged(ctx)
		if err != nil {
			return fmt.Errorf("inspect agent configuration: %w", err)
		}
		execution.configsChanged = !converged
	case initReconcile:
		if err := (application.Reconciler{Stages: application.InitStages(execution.loaded.Context), Runner: execution.platform}).CheckApproved(ctx, execution.approvedPlan); err != nil {
			return err
		}
		plan, err := (application.Reconciler{
			Stages: application.InitStages(execution.loaded.Context), Runner: execution.platform,
		}).Plan(ctx)
		if err != nil {
			return err
		}
		execution.plan = plan
		if execution.hooksOnly() {
			execution.hooksApplicable, err = execution.platform.ProjectHooksApplicable(ctx)
			if err != nil {
				return err
			}
		}
	case initReset:
	default:
		return errors.New("invalid init mode")
	}
	return nil
}

func (cli *CLI) printInitPlan(execution *initExecution) {
	if execution.mode == initConfigs {
		fmt.Fprintln(cli.options.Stdout, "init --configs: refresh agent configuration")
		return
	}
	fmt.Fprintln(cli.options.Stdout, "\nSubyard init")
	for _, consequence := range integrationAdoptionConsequences(execution.loaded.Context.YardName, execution.integrationAdoption) {
		fmt.Fprintf(cli.options.Stdout, "  [do  ] %s\n", consequence)
	}
	for _, consequence := range execution.orphanIngress.consequences() {
		fmt.Fprintf(cli.options.Stdout, "  [do  ] %s\n", consequence)
	}
	if execution.orphanIngressDeferred {
		fmt.Fprintln(cli.options.Stdout, "  [do  ] restore owner Incus access and inspect deselected public ingress before continuing")
	}
	for _, step := range execution.plan.Steps {
		state := "do"
		if step.Converged {
			state = "skip"
		}
		fmt.Fprintf(cli.options.Stdout, "  [%-4s] %s\n", state, step.Stage.Label)
	}
}

func (execution *initExecution) run(ctx context.Context, cli *CLI, output io.Writer) error {
	unlock, err := lockIntegrationYard(ctx, execution.loaded)
	if err != nil {
		return err
	}
	defer unlock()
	if err := execution.checkBeforeInitWrites(ctx, cli); err != nil {
		return err
	}
	if cli.options.InitPlatform == nil && execution.mode == initReconcile {
		if err := execution.refreshOrphanIngress(ctx, cli); err != nil {
			return err
		}
	}
	if execution.hooksOnly() {
		if err := execution.retryProjectHooks(ctx); err != nil {
			return err
		}
		return nil
	}
	if err := execution.integrationSelection.check(ctx, cli, execution); err != nil {
		return err
	}
	if execution.mode == initReconcile {
		incusStage := application.InitStages(execution.loaded.Context)[0]
		if err := (application.Reconciler{Stages: []application.ReconcileStage{incusStage},
			Runner: execution.platform, Reporter: initReporter{output: output}}).Apply(ctx, execution.approvedStages(incusStage)); err != nil {
			return err
		}
		// The installer activates access in this process. Keep the approved
		// inputs and native plans, and reject drift before publishing yard state.
		if err := execution.checkBeforeInitWrites(ctx, cli); err != nil {
			return err
		}
		if err := (application.Reconciler{Stages: application.InitStages(execution.loaded.Context), Runner: execution.platform}).CheckApproved(ctx, execution.approvedPlan); err != nil {
			return err
		}
		if err := execution.integrationSelection.check(ctx, cli, execution); err != nil {
			return err
		}
		if err := execution.finishDeferredOrphanIngress(ctx, cli, output); err != nil {
			return err
		}
	}
	if !execution.orphanIngressDeferred {
		if err := execution.orphanIngress.apply(ctx, cli, execution.loaded, execution.operationID); err != nil {
			return fmt.Errorf("close deselected public ingress: %w", err)
		}
	}
	if err := execution.integrationSelection.apply(execution); err != nil {
		return err
	}
	if execution.bootstrap != nil {
		if err := config.CreatePersistentFile(
			execution.loaded.Context.Paths.ConfigHome,
			execution.bootstrap.targetPath,
			execution.bootstrap.content,
		); err != nil {
			return fmt.Errorf("create named-yard definition: %w", err)
		}
	}
	hostID, err := configsync.EnsureHostID(
		execution.loaded.Context.Paths.ConfigHome, execution.loaded.Environment,
	)
	if err != nil {
		return fmt.Errorf("initialize owner HostID: %w", err)
	}
	fmt.Fprintf(output, "  [ ok ] owner HostID: %s\n", hostID)
	if hostID != execution.hostID {
		return fmt.Errorf("%w: owner HostID changed during initialization", domain.ErrPlanStale)
	}
	if err := execution.profileSetup.apply(ctx, execution, output); err != nil {
		return err
	}
	if execution.mode == initConfigs {
		if err := execution.checkReleaseConfigOwnership(ctx, cli); err != nil {
			return err
		}
		return execution.platform.RefreshConfigs(ctx)
	}
	if execution.mode == initReset {
		if execution.resetBaseline != nil {
			if err := execution.resetBaseline.check(ctx, cli); err != nil {
				return err
			}
		}
		if err := execution.platform.Teardown(ctx); err != nil {
			return fmt.Errorf("teardown before reset: %w", err)
		}
		if execution.resetBaseline != nil {
			if err := execution.resetBaseline.verify(ctx, cli); err != nil {
				return fmt.Errorf("verify teardown before reset: %w", err)
			}
		}
	}
	reconciler := application.Reconciler{
		Stages: application.InitStages(execution.loaded.Context), Runner: execution.platform,
		Reporter: initReporter{output: output},
	}
	approved := execution.approvedPlan
	if execution.mode == initReconcile {
		reconciler.Stages = reconciler.Stages[1:]
		approved.Steps = approved.Steps[1:]
	}
	if err := reconciler.Apply(ctx, approved); err != nil {
		return err
	}
	if err := execution.retryProjectHooks(ctx); err != nil {
		return err
	}
	if execution.profileProvision == nil {
		if err := cli.printInitProvisionHint(ctx, execution, output); err != nil {
			return err
		}
	}
	finalizer := application.Reconciler{
		Stages: []application.ReconcileStage{application.FinalizeStage()},
		Runner: execution.platform, Reporter: initReporter{output: output},
	}
	if err := finalizer.Apply(ctx, execution.approvedFinalize); err != nil {
		return err
	}
	if execution.profileProvision == nil {
		fmt.Fprintln(output, "  [ ok ] Subyard initialized")
	}
	return nil
}

func (execution *initExecution) checkBeforeInitWrites(ctx context.Context, cli *CLI) error {
	if err := execution.checkNativePlans(ctx, true); err != nil {
		return err
	}
	if err := execution.checkHookProjects(ctx); err != nil {
		return err
	}
	if err := execution.inputBaseline.check(ctx, cli); err != nil {
		return err
	}
	if err := execution.checkIntegrationBaseline(cli); err != nil {
		return err
	}
	if err := execution.profileSetup.check(); err != nil {
		return err
	}
	if err := execution.checkIntegrationAdoption(ctx); err != nil {
		return err
	}
	return nil
}

func (execution *initExecution) retryProjectHooks(ctx context.Context) error {
	if err := execution.platform.RunProjectHooks(ctx); err != nil {
		return fmt.Errorf("project hook verification failed; retry init: %w", err)
	}
	return nil
}

func (execution *initExecution) checkNativePlans(ctx context.Context, beforeWrites bool) error {
	runtime, ok := execution.platform.(reconcileruntime.Runtime)
	if !ok {
		return nil
	}

	if err := runtime.CheckProfileRuntimePlan(ctx); err != nil {
		return err
	}
	return runtime.CheckProjectHookPlan(ctx, beforeWrites)
}

func reconcileMigrationTestVMs(ctx context.Context, platform ports.InitPlatform) error {
	converged, err := platform.CheckStage(ctx, ports.ReconcileStageTestVMs)
	if err != nil {
		return err
	}
	if !converged {
		if err := platform.ApplyStage(ctx, ports.ReconcileStageTestVMs); err != nil {
			return err
		}
	}
	converged, err = platform.VerifyStage(ctx, ports.ReconcileStageTestVMs)
	if err != nil {
		return err
	}
	if !converged {
		return errors.New("test VM backend did not converge")
	}
	return nil
}

func (cli *CLI) printInitProvisionHint(
	ctx context.Context,
	execution *initExecution,
	output io.Writer,
) error {
	project, err := cli.prepareProjectInventory(ctx, execution.loaded, nil)
	if err != nil {
		return err
	}
	provision, err := cli.prepareProvisionExecution(execution.loaded, nil, project)
	if err != nil {
		return err
	}
	profiles := provision.profiles
	hint := cli.yardHint(execution.loaded.Context)
	if len(profiles) == 0 {
		fmt.Fprintf(output, "  %s provision -l\n", hint)
		return nil
	}
	fmt.Fprintf(output, "  %s provision    # %s\n", hint, strings.Join(profiles, " "))
	return nil
}

type initAdapter struct {
	execution *initExecution
	cli       *CLI
	output    io.Writer
}

func (adapter initAdapter) Run(
	ctx context.Context,
	request domain.AdapterRequest,
	_ io.Reader,
) (domain.AdapterResult, string, error) {
	if request.Adapter != "init" || request.Action != "reconcile" || adapter.execution == nil {
		return domain.AdapterResult{}, "", errors.New("invalid init adapter request")
	}
	adapter.execution.operationID = request.OperationID
	if err := adapter.execution.run(ctx, adapter.cli, adapter.output); err != nil {
		return domain.AdapterResult{}, "", err
	}
	return domain.AdapterResult{
		Schema: 1, OperationID: request.OperationID, Status: "ok",
		Output: map[string]any{"pending": adapter.execution.plan.Pending()},
	}, "", nil
}

// Only a persistent, ordinary yard selection can authorize first-inventory adoption.
func prepareLegacyIntegrationAdoption(ctx context.Context, selection config.IntegrationSelection, platform ports.InitPlatform) (ports.InitPlatform, reconcileruntime.IntegrationPlan, error) {
	runtime, ok := platform.(reconcileruntime.Runtime)
	if !ok || !selection.Present || !selection.AllowsCodingTools || selection.Provenance.Scope == "command" {
		return platform, reconcileruntime.IntegrationPlan{}, nil
	}
	return runtime.PrepareLegacyIntegrationAdoption(ctx)
}

func integrationAdoptionConsequences(yard string, plan reconcileruntime.IntegrationPlan) []string {
	result := make([]string, 0, len(plan.Adoption))
	for _, path := range plan.Adoption {
		result = append(result, fmt.Sprintf("yard %s: take matching legacy path under integration management: %s", yard, path))
	}
	return result
}

func (execution *initExecution) checkIntegrationAdoption(ctx context.Context) error {
	if execution.integrationAdoption.AdoptionFingerprint == "" {
		return nil
	}
	runtime, ok := execution.platform.(reconcileruntime.Runtime)
	if !ok {
		return errors.New("legacy integration adoption runtime is unavailable")
	}
	plan, err := runtime.IntegrationPlan(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrPlanStale, err)
	}
	if plan.AdoptionFingerprint != execution.integrationAdoption.AdoptionFingerprint {
		return fmt.Errorf("%w: legacy integration adoption changed after planning", domain.ErrPlanStale)
	}
	return nil
}

// Release activation must enroll legacy wiring before its config-only child can
// replace files. Ordinary explicit config refresh keeps its existing contract.
func (execution *initExecution) checkReleaseConfigOwnership(ctx context.Context, cli *CLI) error {
	if !cli.releaseTransitionChild {
		return nil
	}
	if runtime, ok := execution.platform.(reconcileruntime.Runtime); ok {
		if _, err := runtime.IntegrationPlan(ctx); err != nil {
			return fmt.Errorf("cannot refresh release configs before integration ownership is established: %w", err)
		}
	}
	return nil
}

func (execution *initExecution) rebuildPlatform(cli *CLI) {
	execution.platform = cli.initPlatform(execution.loaded, execution.powerYards)
	if runtime, ok := execution.platform.(reconcileruntime.Runtime); ok {
		runtime.RuntimePlan = execution.runtimePlan
		runtime.HookPlan = execution.hookPlan
		runtime.InitProfile = execution.requestedProfile
		if execution.mode == initReset {
			runtime.TeardownResources = slices.Clone(execution.teardownResources)
			runtime.TeardownArtifacts = execution.resetBaseline.artifacts()
		}
		runtime.ProvisionProfile = execution.provisionProfile
		runtime.ResourceCommand = execution.resourceCommand
		runtime.ResourceArguments = slices.Clone(execution.resourceArguments)
		if execution.integrationAdoption.AdoptionFingerprint != "" {
			runtime.AdoptLegacyIntegrations = true
			runtime.LegacyIntegrationFingerprint = execution.integrationAdoption.AdoptionFingerprint
		}
		execution.platform = runtime
	}
}

func (execution *initExecution) checkHookProjects(ctx context.Context) error {
	if execution.hookPlan == nil {
		return nil
	}
	store, err := openProjectStoreReadOnly(execution.loaded.Context.Paths.StateDir)
	if err != nil {
		return err
	}
	records, err := store.List(ctx)
	if err != nil {
		return err
	}
	if operationStateDigest(records) != operationStateDigest(execution.hookProjects) {
		return fmt.Errorf("%w: captured project hook owner records changed", domain.ErrPlanStale)
	}
	return nil
}

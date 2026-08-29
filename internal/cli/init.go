package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	loaded         config.Loaded
	mode           initMode
	bootstrap      *initBootstrap
	plan           application.ReconcilePlan
	platform       ports.InitPlatform
	powerYards     []domain.Context
	hostID         string
	hostIDPending  bool
	configsChanged bool
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
	existingPath, existingErr := config.FindYardSettingsFile(
		cli.options.RepositoryRoot, yard, configHome,
	)
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
		if name := firstInitProfileConflict(existing, existingPath, preset, source); name != "" {
			return config.Loaded{}, nil, fmt.Errorf(
				"named yard %q conflicts with profile %q at setting %s; use plain init for an intentionally customized yard",
				yard, request.profile, name,
			)
		}
		if name := firstInitProfileOverride(existing, preset, source); name != "" {
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
	if name := firstInitProfileOverride(loaded, loaded, source); name != "" {
		return config.Loaded{}, nil, fmt.Errorf(
			"command environment overrides profile %q at setting %s",
			request.profile, name,
		)
	}
	return loaded, &initBootstrap{
		profile: request.profile, sourcePath: source, targetPath: target, content: content,
	}, nil
}

func firstInitProfileOverride(
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
		if loaded.Environment[name] != presetValues[name] {
			return name
		}
	}
	return ""
}

func firstInitProfileConflict(
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
		RepositoryRoot: cli.options.RepositoryRoot,
		Environment:    environmentList(cli.env, environment),
		Stdin:          cli.options.Stdin,
		Stdout:         cli.options.Stderr,
		Stderr:         cli.options.Stderr,
		Incus:          incusPort,
		ConfigWriter:   configWriter,
		Executor:       executor,
		Yard:           loaded.Context,
		PowerYards:     powerYards,
		SRVPool:        loaded.Environment["SRV_POOL"],
		SRVVolume:      loaded.Environment["SRV_VOLUME"],
	}
}

func (cli *CLI) powerYardContexts(current config.Loaded) ([]domain.Context, error) {
	directories := config.RegistryDirectories(
		current.Context.Paths.ConfigDir, current.Context.Paths.ConfigHome,
	)
	names, err := config.YardNames(directories...)
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
		loaded: loaded, mode: mode, bootstrap: bootstrap, platform: platform, powerYards: powerYards,
	}
	execution.hostID, execution.hostIDPending, err = configsync.ResolveHostID(
		loaded.Context.Paths.ConfigHome, loaded.Environment,
	)
	if err != nil {
		return nil, err
	}
	if mode == initConfigs {
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
			execution.plan.Steps = append(execution.plan.Steps, application.ReconcileStep{Stage: stage})
		}
	} else {
		execution.plan, err = (application.Reconciler{Stages: stages, Runner: execution.platform}).Plan(ctx)
		if err != nil {
			return nil, err
		}
	}
	if execution.plan.Pending() != 0 || bootstrap != nil {
		if err := execution.platform.Preflight(ctx, mode == initReset); err != nil {
			return nil, fmt.Errorf("host preflight failed: %w", err)
		}
	}
	return execution, nil
}

func (execution *initExecution) consequences() []string {
	hostIDConsequences := []string{}
	if execution.bootstrap != nil {
		hostIDConsequences = append(hostIDConsequences,
			"create named yard definition from profile "+execution.bootstrap.profile)
	}
	if execution.hostIDPending {
		hostIDConsequences = append(hostIDConsequences, "record owner HostID "+execution.hostID)
	}
	switch execution.mode {
	case initConfigs:
		return append(hostIDConsequences, "refresh in-yard agent instructions and default configs")
	case initReset:
		result := []string{"delete the yard instance and its disk data"}
		for _, step := range execution.plan.Steps {
			result = append(result, step.Stage.Label)
		}
		return append(hostIDConsequences, result...)
	default:
		result := make([]string, 0, execution.plan.Pending())
		for _, step := range execution.plan.Steps {
			if !step.Converged {
				result = append(result, step.Stage.Label)
			}
		}
		return append(hostIDConsequences, result...)
	}
}

func (execution *initExecution) actionPlan() (domain.ActionID, domain.ActionDelta, error) {
	if execution == nil {
		return "", domain.ActionDelta{}, errors.New("init execution is required")
	}
	action := domain.ActionID("yard.init.reconcile")
	changed := execution.plan.Pending() != 0 || execution.bootstrap != nil || execution.hostIDPending
	switch execution.mode {
	case initReconcile:
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

func (execution *initExecution) refreshAssessment(ctx context.Context) error {
	if execution == nil {
		return errors.New("init execution is required")
	}
	hostID, pending, err := configsync.ResolveHostID(
		execution.loaded.Context.Paths.ConfigHome, execution.loaded.Environment,
	)
	if err != nil {
		return err
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
		plan, err := (application.Reconciler{
			Stages: application.InitStages(execution.loaded.Context), Runner: execution.platform,
		}).Plan(ctx)
		if err != nil {
			return err
		}
		execution.plan = plan
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
	for _, step := range execution.plan.Steps {
		state := "do"
		if step.Converged {
			state = "skip"
		}
		fmt.Fprintf(cli.options.Stdout, "  [%-4s] %s\n", state, step.Stage.Label)
	}
}

func (execution *initExecution) run(ctx context.Context, cli *CLI, output io.Writer) error {
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
	if execution.mode == initConfigs {
		return execution.platform.RefreshConfigs(ctx)
	}
	if execution.mode == initReset {
		if err := execution.platform.Teardown(ctx); err != nil {
			return fmt.Errorf("teardown before reset: %w", err)
		}
	}
	reconciler := application.Reconciler{
		Stages: application.InitStages(execution.loaded.Context), Runner: execution.platform,
		Reporter: initReporter{output: output},
	}
	if err := reconciler.Apply(ctx); err != nil {
		return err
	}
	if err := cli.printInitProvisionHint(ctx, execution, output); err != nil {
		return err
	}
	finalizer := application.Reconciler{
		Stages: []application.ReconcileStage{application.FinalizeStage()},
		Runner: execution.platform, Reporter: initReporter{output: output},
	}
	if err := finalizer.Apply(ctx); err != nil {
		return err
	}
	fmt.Fprintln(output, "  [ ok ] Subyard initialized")
	return nil
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
	if err := adapter.execution.run(ctx, adapter.cli, adapter.output); err != nil {
		return domain.AdapterResult{}, "", err
	}
	return domain.AdapterResult{
		Schema: 1, OperationID: request.OperationID, Status: "ok",
		Output: map[string]any{"pending": adapter.execution.plan.Pending()},
	}, "", nil
}

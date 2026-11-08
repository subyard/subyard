package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

// An in-place repair retains installed configuration until it is ready. The
// same confirmation then covers ordinary init for the disclosed pending yards.

type pendingInitSetting = domain.ConfigurationDifference

type pendingInitYard struct {
	Name string
	// State describes why a yard was not inspected; empty for a running yard.
	State       string
	Reason      string
	Unavailable string
	Baseline    bool
	Pending     bool
	Settings    []pendingInitSetting
}

type pendingInitPlan struct {
	Command string
	Release string
	Yards   []pendingInitYard
}

func (plan pendingInitPlan) pendingYards() []string {
	var names []string
	for _, yard := range plan.Yards {
		if yard.Pending {
			names = append(names, yard.Name)
		}
	}
	return names
}

// preparePostRepairInit extends only in-place repair plans. Execution through RPC
// prints the commands; the direct CLI runs them after releasing the repair locks.
func (prepared *preparedCommand) preparePostRepairInit(ctx context.Context, execution *releaseExecution) error {
	if !activeReleaseRepair(execution.prepared.Request) {
		return nil
	}
	plan, err := prepared.CLI.inspectPendingInit(ctx, execution.configHome)
	if err != nil {
		return fmt.Errorf("inspect integration settings awaiting yard init: %w", err)
	}
	pending := len(plan.pendingYards()) != 0
	if !pending && !slices.ContainsFunc(plan.Yards, func(yard pendingInitYard) bool { return yard.State != "" }) {
		return nil
	}
	plan.Command, plan.Release = prepared.Definition.Name, execution.prepared.TargetRelease
	prepared.pendingInit = &plan
	steps := prepared.steps
	prepared.steps = func() []domain.OperationStep { return plan.steps(steps()) }
	prepared.exactState = operationStateDigest(struct {
		Release string
		Pending pendingInitPlan
	}{prepared.exactState, plan})
	if pending {
		assess := prepared.assess
		prepared.assess = func(ctx context.Context) (domain.ActionID, domain.ActionDelta, error) {
			action, delta, err := assess(ctx)
			delta.Changed = true // Pending inits add effects even when resume needs no new release grant.
			return action, delta, err
		}
	}
	execute := prepared.execute
	prepared.execute = func(ctx context.Context, orchestrator *application.Orchestrator, output io.Writer) (domain.AdapterResult, error) {
		result, err := execute(ctx, orchestrator, output)
		if err == nil && result.Status == "ok" {
			if pending {
				fmt.Fprintf(output, "Release %s is ready; pending integration settings use ordinary yard init:\n", plan.Release)
			}
			for _, line := range plan.lines() {
				fmt.Fprintln(output, line)
			}
			for _, name := range plan.pendingYards() {
				fmt.Fprintf(output, "  yard -Y %s init\n", name)
			}
		}
		return result, err
	}
	return nil
}

// inspectPendingInit compares the installed and desired init-owned settings of
// every local yard, in stable order. Stopped, absent or unavailable yards are
// not inspected, matching the repair's ordinary installed-view assessment.
func (cli *CLI) inspectPendingInit(ctx context.Context, configHome string) (pendingInitPlan, error) {
	operation := cli.releaseActivationOperation()
	configDir := firstNonempty(operation.env["SUBYARD_CONFIG_DIR"], filepath.Join(operation.options.RepositoryRoot, "config"))
	names, err := config.YardNames(configDir, configHome)
	if err != nil {
		return pendingInitPlan{}, err
	}
	names = append(names, "default")
	slices.Sort(names)
	names = slices.Compact(names)
	var plan pendingInitPlan
	var failures []error
	for _, name := range names {
		yard, err := operation.inspectPendingInitYard(ctx, configHome, name)
		if err != nil {
			var refusals *domain.Refusals
			if errors.As(err, &refusals) {
				failures = append(failures, refusals.Unwrap()...)
			} else {
				failures = append(failures, err)
			}
			continue
		}
		if yard.State != "remote" {
			plan.Yards = append(plan.Yards, yard)
		}
	}
	return plan, domain.CollectRefusals(failures...)
}

// Recheck only the selected yard before its ordinary init, so another yard's
// failure cannot suppress an independent init or its refusal.
func (cli *CLI) inspectPendingInitYard(ctx context.Context, configHome, name string) (pendingInitYard, error) {
	operation := cli.releaseActivationOperation()
	loaded, err := operation.resolveReleaseTransitionContext(ctx, name, configHome)
	if err != nil {
		return pendingInitYard{Name: name, State: "unavailable", Reason: "desired integration sources cannot be read"}, nil
	}
	targets, err := operation.releaseConfigTargets(ctx, loaded, false)
	if err != nil {
		return pendingInitYard{Name: name}, err
	}
	if loaded.Context.AccessKind == domain.AccessRemote {
		return pendingInitYard{Name: name, State: "remote"}, nil
	}
	if len(targets) == 0 {
		return pendingInitYard{Name: name, State: "unavailable", Reason: "local yard is no longer available"}, nil
	}
	return operation.inspectPendingInitTarget(ctx, targets[0])
}

func (operation *CLI) inspectPendingInitTarget(ctx context.Context, target configTarget) (pendingInitYard, error) {
	yard := pendingInitYard{Name: target.Name}
	incus, _ := operation.statusPorts()
	instance, err := incus.Instance(ctx, target.Loaded.Context.IncusProject, target.Loaded.Context.YardInstanceName)
	switch {
	case errors.Is(err, ports.ErrInstanceNotFound):
		yard.State = "stopped"
	case err != nil:
		// The ordinary release assessment owns transport and read failures.
		yard.State = "unavailable"
	case !strings.EqualFold(instance.Status, "running"):
		yard.State = strings.ToLower(firstNonempty(instance.Status, "not running"))
	default:
		applied, baseline, err := operation.installedReleaseTarget(ctx, target)
		if err != nil {
			return yard, err
		}
		yard.Baseline = baseline
		diagnostic, err := operation.compareInstalledConfiguration(target, applied, baseline)
		if err != nil {
			var unavailable pendingInitUnavailableError
			if errors.As(err, &unavailable) {
				yard.State, yard.Reason = "unavailable", unavailable.Error()
				return yard, nil
			}
			return yard, fmt.Errorf("yard %s: inspect init integration settings: %w", target.Name, err)
		}
		yard.Settings = slices.DeleteFunc(diagnostic.Differences, func(row pendingInitSetting) bool {
			definition, _ := target.Loaded.Catalog.LookupSetting(row.Name)
			return definition.Application == config.SettingConfigApply
		})
		yard.Unavailable = diagnostic.Unavailable
		yard.Pending = slices.ContainsFunc(yard.Settings, func(row pendingInitSetting) bool {
			return row.Kind != domain.ConfigurationContentUnknown && row.Kind != domain.ConfigurationStateUnknown
		})
	}
	return yard, nil
}

type pendingInitUnavailableError struct{}

func (pendingInitUnavailableError) Error() string {
	return "desired integration sources cannot be read"
}

// Settings for unselected consumers do not await init. Compare the native input
// scopes so irrelevant host files and integration settings cannot remain pending
// forever when ordinary init already reports convergence.
func (cli *CLI) pendingEffectiveSettings(desired, installed config.Loaded, settings []pendingInitSetting) ([]pendingInitSetting, error) {
	integrationOnly := func(name string) bool {
		// The core usage helper consumes its hook even with no selected coding
		// integration, so an unchanged integration scope cannot suppress it.
		if strings.HasPrefix(name, "CCUSAGE_") && desired.Integrations.AllowsCodingTools {
			return false
		}
		return name == "INTEGRATION_HOST_LINKS" || desired.Catalog.IntegrationInitSetting(name) &&
			name != "CODING_TOOL_INTEGRATIONS" && name != "DEV_USER" && name != "DEV_UID" && name != "HOST_LINKS" &&
			name != "ALLOWS_HOST_ACCESS" && name != "ALLOWS_CODING_TOOLS"
	}
	if !slices.ContainsFunc(settings, func(setting pendingInitSetting) bool { return integrationOnly(setting.Name) }) {
		return settings, nil
	}
	desiredScope, _, err := cli.materializedIntegrationScope(desired, nil)
	if err != nil {
		return settings, pendingInitUnavailableError{}
	}
	installedScope, _, err := cli.materializedIntegrationScope(installed, nil)
	if err != nil {
		return settings, err
	}
	if desiredScope == installedScope {
		settings = slices.DeleteFunc(settings, func(setting pendingInitSetting) bool { return integrationOnly(setting.Name) })
	}
	return settings, nil
}

// pendingInitValues are the init-owned values the integration fingerprint binds,
// normalized as installed state records them. Other settings are the desired
// values in both views, as the installed projection keeps them. The selection is
// compared separately as its effective set, which yard init installs and records.
func pendingInitValues(loaded config.Loaded) map[string]string {
	values := map[string]string{"ALLOWS_CODING_TOOLS": strconv.FormatBool(loaded.Integrations.AllowsCodingTools), "ENVIRONMENT_PROFILES": strings.Join(loaded.EnvironmentProfiles.Effective, " ")}
	for name, value := range loaded.Environment {
		owned := loaded.Catalog.IntegrationInitSetting(name) || name == "INTEGRATION_HOST_LINKS" || name == "ENVIRONMENT_PROFILES" || name == "WEB_PREVIEW_HOST_PORT"
		if owned && name != "CODING_TOOL_INTEGRATIONS" {
			switch name {
			case "ENVIRONMENT_PROFILES":
				value = strings.Join(loaded.EnvironmentProfiles.Effective, " ")
			case "HOST_LINKS", "INTEGRATION_HOST_LINKS":
				value = strings.Join(strings.Fields(value), " ")
			case "ALLOWS_CODING_TOOLS":
				value = strconv.FormatBool(loaded.Integrations.AllowsCodingTools)
			}
			values[name] = value
		}
	}
	return values
}

// pendingInitSettings lists the settings whose desired value differs from the
// installed one, sorted by name, before removing unchanged consumer inputs.
func pendingInitSettings(desired, installed config.Loaded) []pendingInitSetting {
	catalog := desired.Catalog
	desiredValues, installedValues := pendingInitValues(desired), pendingInitValues(installed)
	const amd64, arm64 = "CCUSAGE_SHA256_AMD64", "CCUSAGE_SHA256_ARM64"
	if archive, ok := installedValues[amd64]; ok && installedValues[arm64] == archive && (archive == desiredValues[amd64] || archive == desiredValues[arm64]) {
		installedValues[amd64], installedValues[arm64] = desiredValues[amd64], desiredValues[arm64]
	}
	names := map[string]bool{}
	for _, values := range []map[string]string{desiredValues, installedValues} {
		for name := range values {
			names[name] = true
		}
	}
	var result []pendingInitSetting
	for name := range names {
		installedValue, installedSet := installedValues[name]
		desiredValue, desiredSet := desiredValues[name]
		if installedSet == desiredSet && installedValue == desiredValue {
			continue
		}
		definition, _ := catalog.LookupSetting(name)
		kind := domain.ConfigurationValues
		if definition.Sensitive {
			kind = domain.ConfigurationSensitive
		}
		result = append(result, pendingInitSetting{Name: name, Installed: installedValue, InstalledSet: installedSet,
			Desired: desiredValue, DesiredSet: desiredSet, Kind: kind})
	}
	if !slices.Equal(installed.Integrations.Effective, desired.Integrations.Effective) {
		result = append(result, pendingInitSetting{Name: "CODING_TOOL_INTEGRATIONS",
			Installed: strings.Join(installed.Integrations.Effective, " "), InstalledSet: true,
			Desired: strings.Join(desired.Integrations.Effective, " "), DesiredSet: true})
	}
	listed := slices.ContainsFunc(result, func(setting pendingInitSetting) bool { return setting.Name == "ALLOWS_CODING_TOOLS" })
	if installed.Integrations.AllowsCodingTools != desired.Integrations.AllowsCodingTools && !listed {
		result = append(result, pendingInitSetting{Name: "ALLOWS_CODING_TOOLS",
			Installed: strconv.FormatBool(installed.Integrations.AllowsCodingTools), Desired: strconv.FormatBool(desired.Integrations.AllowsCodingTools), InstalledSet: true, DesiredSet: true})
	}
	slices.SortStableFunc(result, func(left, right pendingInitSetting) int { return strings.Compare(left.Name, right.Name) })
	return result
}

const pendingInitHeading = "Init-owned integration settings awaiting yard init (installed -> desired); yard init runs after the release repair is ready:"

// lines renders the listing printed before the operation's confirmation.
func (plan pendingInitPlan) lines() []string { return plan.listing(pendingInitHeading) }

func (plan pendingInitPlan) listing(heading string) []string {
	lines := []string{heading}
	for _, yard := range plan.Yards {
		switch {
		case yard.State == "unavailable":
			reason := "unavailable"
			if yard.Reason != "" {
				reason += ": " + yard.Reason
			}
			lines = append(lines, "  yard "+yard.Name+": not inspected ("+reason+")")
			continue
		case yard.State != "":
			lines = append(lines, "  yard "+yard.Name+": "+yard.State+"; not inspected")
			continue
		case !yard.Baseline:
			lines = append(lines, "  yard "+yard.Name+": installed integration settings unknown (no applied baseline)")
		case yard.Unavailable != "":
			lines = append(lines, "  yard "+yard.Name+": "+yard.Unavailable)
		case !yard.Pending:
			lines = append(lines, "  yard "+yard.Name+": integration settings current")
			continue
		default:
			lines = append(lines, "  yard "+yard.Name+":")
		}
		for _, setting := range yard.Settings {
			lines = append(lines, "    "+setting.Name+"  "+setting.Value())
		}
	}
	return lines
}

func (cli *CLI) printPendingInit(plan pendingInitPlan) {
	for _, line := range plan.lines() {
		fmt.Fprintln(cli.options.Stdout, line)
	}
}

// steps preserves the release plan and appends each ordinary init after it.
func (plan pendingInitPlan) steps(release []domain.OperationStep) []domain.OperationStep {
	steps := domain.CloneOperationSteps(release)
	for _, name := range plan.pendingYards() {
		step := domain.OperationStep{ID: "release.init." + name, Target: "yard " + name,
			Observed: "init-owned integration settings differ from installed settings",
			Desired:  "yard init applies desired configuration after the release is ready", Decision: domain.StepConditional,
			Preconditions: []string{"release repair is ready", "ordinary yard init admission and guards"},
			Verify:        "ordinary yard init verifies convergence and release readiness",
			Consequence:   "run yard -Y " + name + " init after the release repair"}
		if len(release) != 0 {
			step.DependsOn = []string{release[len(release)-1].ID}
		}
		steps = append(steps, step)
	}
	return steps
}

// runPostRepairInit uses fresh ordinary commands with the single confirmation's
// consent. Every disclosed pending yard runs; no release admission is bypassed.
func (cli *CLI) runPostRepairInit(ctx context.Context, plan pendingInitPlan, assumeYes bool) int {
	var refusals []error
	for _, approved := range plan.Yards {
		if !approved.Pending {
			continue
		}
		name := approved.Name
		fresh, err := cli.inspectPendingInitYard(ctx, cli.env["SUBYARD_CONFIG_HOME"], name)
		if err != nil {
			cli.printPendingInit(pendingInitPlan{Yards: []pendingInitYard{{Name: name, State: "unavailable", Reason: "integration settings cannot be inspected"}}})
			refusals = append(refusals, fmt.Errorf("yard %s: cannot recheck integration settings; next: run yard -Y %s init", name, name))
			continue
		}
		approvedListing := (pendingInitPlan{Yards: []pendingInitYard{approved}}).lines()
		freshPlan := pendingInitPlan{Yards: []pendingInitYard{fresh}}
		changed := !slices.Equal(approvedListing, freshPlan.lines())
		if changed {
			cli.printPendingInit(freshPlan)
		}
		if fresh.State == "unavailable" || changed && !assumeYes && (cli.promptInputTerminal == nil || !cli.promptInputTerminal()) {
			refusals = append(refusals, fmt.Errorf("yard %s: integration settings need fresh confirmation; next: run yard -Y %s init", name, name))
			continue
		}
		arguments := []string{"-Y", name, "init"}
		if assumeYes || !changed {
			arguments = append(arguments, "--yes")
		}
		fmt.Fprintf(cli.options.Stdout, "\nRunning yard init for yard %s after the release repair\n", name)
		var diagnostics bytes.Buffer
		if code := cli.runNestedCommand(ctx, arguments, &diagnostics); code != 0 {
			refusals = append(refusals, fmt.Errorf("yard %s: %s; next: run yard -Y %s init", name, lastDiagnostic(diagnostics.String(), code), name))
		}
	}
	if err := domain.CollectRefusals(refusals...); err != nil {
		cli.errorf("%s: release %s is ready; pending yard init did not complete: %v", plan.Command, plan.Release, err)
		return 1
	}
	return 0
}

// lastDiagnostic is the final diagnostic line of a refused nested command.
func lastDiagnostic(output string, code int) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		if line := strings.TrimSpace(lines[index]); line != "" {
			return line
		}
	}
	return fmt.Sprintf("exit status %d", code)
}

// runNestedCommand runs one same-host command with freshly loaded persisted
// settings, like the activation repair follow-up. A non-nil diagnostics writer
// also receives the command's standard error.
func (cli *CLI) runNestedCommand(ctx context.Context, arguments []string, diagnostics io.Writer) int {
	if cli.nestedCommand != nil {
		return cli.nestedCommand(ctx, arguments, diagnostics)
	}
	options := cli.options
	environment := cli.freshMigrationEnvironment(cli.baseEnv, options.RepositoryRoot)
	environment["SUBYARD_OPERATION_ID"] = newOperationID()
	options.Environment = environmentList(environment, nil)
	options.Arguments = slices.Clone(arguments)
	if diagnostics != nil {
		options.Stderr = io.MultiWriter(cli.options.Stderr, diagnostics)
	}
	operation, err := New(options)
	if err != nil {
		cli.errorf("prepare %s: %v", strings.Join(arguments, " "), err)
		if diagnostics != nil {
			fmt.Fprintf(diagnostics, "%v\n", err)
		}
		return 1
	}
	operation.promptInputTerminal = cli.promptInputTerminal
	return operation.Run(ctx)
}

// pendingInitIntegrationRefusal inspects, read-only like integration status,
// the desired integration configuration that pending yard init applies after an
// installed-view repair. A prospective view supplies the desired files a planned
// source publication would leave. Desired inputs that cannot be read remain
// uninspected, as in the pending-init listing.
func (cli *CLI) pendingInitIntegrationRefusal(ctx context.Context, installed configTarget, configHome string, files *config.ProspectiveFiles) error {
	desiredView := *cli
	desiredView.activation = nil
	var prospective []*config.ProspectiveFiles
	if files != nil {
		prospective = append(prospective, files)
	}
	loaded, err := desiredView.loadReleaseTransitionContext(installed.Name, configHome, nil, prospective...)
	if err != nil {
		return nil
	}
	targets, err := desiredView.releaseConfigTargets(ctx, loaded, false)
	if err != nil || len(targets) == 0 {
		return nil
	}
	desired := targets[0]
	if integrationConfigurationFingerprint(desired.Loaded) == integrationConfigurationFingerprint(installed.Loaded) {
		return nil
	}
	platform := desiredView.initPlatform(desired.Loaded, nil)
	runtime, ok := platform.(reconcileruntime.Runtime)
	if !ok {
		return nil
	}
	if files != nil {
		runtime.SourceReader = files.ReadAsset
	}
	refusal := func(err error) error {
		return pendingInitConflictError{yard: installed.Name, conflict: reconcileruntime.IntegrationOwnershipConflict(err), cause: err}
	}
	platform, _, err = prepareLegacyIntegrationAdoption(ctx, desired.Loaded.Integrations, runtime)
	if err != nil {
		return refusal(err)
	}
	if runtime, ok = platform.(reconcileruntime.Runtime); !ok {
		return refusal(errors.New("prepared integration runtime is unavailable"))
	}
	if _, err = runtime.IntegrationPlan(ctx); err != nil {
		return refusal(err)
	}
	return nil
}

// pendingInitConflictError refuses an installed-view repair whose integration
// effects could overwrite owned state that the following yard init cannot
// retire. An inspection failure refuses too, but is not named a conflict.
type pendingInitConflictError struct {
	yard     string
	conflict bool
	cause    error
}

func (err pendingInitConflictError) Error() string {
	return fmt.Sprintf("yard %s: inspect integration configuration awaiting yard init: %v", err.yard, err.cause)
}

func (err pendingInitConflictError) Unwrap() error { return err.cause }

func (err pendingInitConflictError) ActivationDiagnostic() (string, string) {
	retry := "run yard -Y " + err.yard + " integration status"
	if err.conflict {
		return "yard " + err.yard + ": integration configuration awaiting yard init has an ownership conflict; the release repair would overwrite the changed owned configuration; fix the conflict that yard integration status reports, then run yard migrate", retry
	}
	return "yard " + err.yard + ": integration configuration awaiting yard init cannot be inspected; the release repair stops rather than overwrite owned configuration that init may be unable to retire; check yard integration status, then run yard migrate", retry
}

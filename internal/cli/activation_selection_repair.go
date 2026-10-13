package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

func (cli *CLI) runSelectionSourceRepair(ctx context.Context, yard string, arguments []string, yes bool) int {
	request, err := parseConfigAliasRepair(arguments)
	if err != nil || len(request.choices) != 1 || !slices.Contains([]string{"CODING_TOOL_INTEGRATIONS", "ENVIRONMENT_PROFILES"}, request.choices[0].name) {
		cli.errorf("config reduce-selection requires one optional selection, explicit --scope and --local [--check]")
		return 2
	}
	request.selection = true
	if err := cli.checkPersistentSelectionInputs(); err != nil {
		cli.errorf("%v", err)
		return 2
	}
	return cli.runConfigSourceRepair(ctx, yard, request, yes)
}

func (cli *CLI) checkPersistentSelectionInputs() error {
	for _, key := range []string{"CODING_TOOL_INTEGRATIONS", "AGENTS", "ENVIRONMENT_PROFILES", "YARD_PROFILES"} {
		if _, present := cli.baseEnv[key]; present {
			return errors.New("selection source repair requires persistent selection; remove temporary selection overrides")
		}
	}
	return nil
}

// A failed optional hook must not lock its selection in forever. This edits only
// desired selection; the original sealed owner separately authorizes recovery.
func (cli *CLI) runActivationSelectionRepair(ctx context.Context, yard, handler string, arguments []string, yes bool, outcome releasetransition.Outcome) (bool, int) {
	request := configAliasRepairRequest{scope: config.ScopeYard, local: true, selection: true,
		yes: yes || slices.Contains(arguments, "--yes") || slices.Contains(arguments, "-y")}
	switch handler {
	case "@config":
		arguments = configArguments(arguments)
		if len(arguments) == 0 || arguments[0] != "set" {
			return false, 0
		}
		write, err := parseConfigAuthoringRequest("set", arguments[1:], yes)
		if err != nil || write.git || !slices.Contains([]string{"CODING_TOOL_INTEGRATIONS", "ENVIRONMENT_PROFILES"}, write.name) {
			return false, 0
		}
		request.scope, request.yes = write.scope, write.assumeYes
		request.choices = []configAliasChoice{{write.name, write.value}}
	case "@integration", "@profile":
		verb, id := "", ""
		if handler == "@integration" {
			parsed, err := parseIntegrationArguments(arguments)
			if err != nil {
				return false, 0
			}
			verb, id = parsed.verb, parsed.id
		} else {
			parsed, err := parseProfileArguments(arguments)
			if err != nil {
				return false, 0
			}
			verb, id = parsed.verb, parsed.id
		}
		if verb != "disable" {
			return false, 0
		}
		loaded, err := cli.loadSelectionRepair(yard)
		if err != nil {
			cli.errorf("selection source repair: configuration is unavailable or invalid")
			return true, 1
		}
		name, selected := "CODING_TOOL_INTEGRATIONS", loaded.Integrations.Requested
		if handler == "@profile" {
			name = "ENVIRONMENT_PROFILES"
			definitions, loadErr := profile.Load(cli.options.RepositoryRoot)
			if loadErr == nil {
				selected, loadErr = cli.currentProfileSelection(loaded, definitions)
			}
			if loadErr == nil && !slices.ContainsFunc(definitions, func(d profile.Definition) bool { return d.Name == id }) {
				loadErr = errors.New("unknown environment profile")
			}
			if loadErr != nil {
				cli.errorf("selection source repair: profile selection is unavailable")
				return true, 1
			}
		} else if _, err := config.ResolveIntegrationSelection(loaded.Environment, []string{id}); err != nil {
			cli.errorf("selection source repair: %v", err)
			return true, 2
		}
		selected = slices.DeleteFunc(slices.Clone(selected), func(value string) bool { return value == id })
		if handler == "@integration" {
			selection, err := config.ResolveIntegrationSelection(loaded.Environment, selected)
			if err != nil || len(selection.DependencyReasons[id]) != 0 {
				cli.errorf("cannot disable %s: disable its dependents explicitly first", id)
				return true, 2
			}
		}
		request.choices = []configAliasChoice{{name, strings.Join(selected, " ")}}
	default:
		return false, 0
	}
	if err := cli.checkPersistentSelectionInputs(); err != nil {
		cli.errorf("%v", err)
		return true, 2
	}
	if handled, code := cli.runCompletedSelectionAuthoring(ctx, yard, request, outcome); handled {
		return true, code
	}

	return true, cli.runConfigSourceRepair(ctx, yard, request, yes)
}

// Completed history confers no activation authority. Admit only an ordinary
// desired-source reduction while all other activation resources remain current.
func (cli *CLI) runCompletedSelectionAuthoring(ctx context.Context, yard string, selection configAliasRepairRequest, outcome releasetransition.Outcome) (bool, int) {
	options, available, err := cli.mutationGateReleaseOptions()
	if err != nil || !available {
		return false, 0
	}
	store, err := releasetransition.NewPOSIXV2Store(options.ConfigHome)
	if err != nil {
		return false, 0
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil || !snapshot.Exists {
		return false, 0
	}
	journal, err := releasetransition.ParseJournal(snapshot.Payload)
	if err != nil || journal.Checkpoint != releasetransition.JournalComplete {
		return false, 0
	}
	fail := func(err error) (bool, int) { cli.errorf("config set: %v", err); return true, 1 }
	// Integration provisioning or services can drift while config files remain
	// current. A source-only reduction does not apply either kind of drift.
	permit, err := cli.prepareConfigApplyRepairMode(ctx, yard, true, outcome, false, nil)
	if err != nil || permit == nil {
		return fail(errors.New("source reduction requires completed history with only materialized configuration drift"))
	}
	runtime := releaseruntime.NewSourceInspector(cli.options.RepositoryRoot, cli.options.Stderr)
	defer runtime.Close()
	configDir, tooling, err := runtime.InspectSourceTooling(ctx, options.RuntimeRoot, cli.options.RepositoryRoot)
	if err != nil {
		return fail(err)
	}
	proof, err := cli.inspectConfigSourceAdmission(ctx, runtime)
	if err != nil {
		return fail(err)
	}
	guard := func() error {
		dir, binding, err := runtime.InspectSourceTooling(ctx, options.RuntimeRoot, cli.options.RepositoryRoot)
		if err != nil || dir != configDir || binding != tooling {
			return domain.ErrPlanStale
		}
		after, err := cli.inspectConfigSourceAdmission(ctx, runtime)
		if err != nil || after.Binding != proof.Binding {
			return domain.ErrPlanStale
		}
		return nil
	}
	loaded, err := cli.loadSelectionRepair(yard)
	if err != nil {
		return fail(err)
	}
	if loaded.Context.AccessKind != domain.AccessLocal {
		return fail(errors.New("source reduction requires the local source owner"))
	}
	choice := selection.choices[0]
	request := configAuthoringRequest{action: "set", name: choice.name, value: choice.value, scope: selection.scope,
		sourceBinding: operationStateDigest([]string{proof.Binding, tooling, permit.factsFingerprint}), sourceGuard: guard}
	arguments := []string{"set", choice.name, choice.value, "--scope", string(selection.scope), "--local"}
	code := cli.runPreparedConfigMutation(ctx, loaded, arguments, selection.yes, func(prepared *preparedCommand) error {
		path, err := configScalarAuthoringPath(loaded, selection.scope)
		if err != nil {
			return err
		}
		baseline, err := readConfigAuthoringTarget(path)
		if err != nil {
			return err
		}
		if _, err := cli.selectionRepairContent(yard, path, baseline.Content, selection); err != nil {
			return err
		}
		if err := prepared.prepareConfigScalarRequest(ctx, "set", arguments[1:], request); err != nil {
			return err
		}
		execute := prepared.execute
		prepared.execute = func(ctx context.Context, orchestrator *application.Orchestrator, output io.Writer) (domain.AdapterResult, error) {
			unlock, err := cli.lockConfigApplyRepair(ctx, permit)
			if err != nil {
				return domain.AdapterResult{}, domain.ErrPlanStale
			}
			defer unlock()
			return execute(ctx, orchestrator, output)
		}
		return nil
	})
	return true, code
}

func (cli *CLI) loadSelectionRepair(yard string) (config.Loaded, error) {
	values := cli.configSourceEnvironment()
	return config.Load(config.LoadOptions{Catalog: &cli.catalog, RepositoryRoot: cli.options.RepositoryRoot,
		OperatorHome: values["SUBYARD_OPERATOR_HOME"], YardName: yard, Environment: values})
}

func (cli *CLI) selectionRepairContent(yard, path string, content []byte, request configAliasRepairRequest) ([]byte, error) {
	loaded, err := cli.loadSelectionRepair(yard)
	if err != nil {
		return nil, err
	}
	choice := request.choices[0]
	assignments, err := config.ParsePersistentAssignments(path, content)
	if err != nil {
		return nil, err
	}
	definition, _ := cli.catalog.LookupSetting(choice.name)
	for _, assignment := range assignments {
		if (assignment.Name == choice.name || slices.Contains(definition.Aliases, assignment.Name)) && (!assignment.Direct || assignment.Dynamic) {
			return nil, errors.New("activation source repair requires a literal persistent selection")
		}
	}
	if err := cli.catalog.ValidateSetting(request.scope, choice.name, choice.value, false); err != nil {
		return nil, err
	}
	selected := loaded.Integrations.Requested
	if choice.name == "ENVIRONMENT_PROFILES" {
		definitions, loadErr := profile.Load(cli.options.RepositoryRoot)
		if loadErr != nil {
			return nil, loadErr
		}
		selected, err = cli.currentProfileSelection(loaded, definitions)
		if err != nil {
			return nil, err
		}
	}
	requested := strings.Fields(choice.value)
	for _, id := range requested {
		if !slices.Contains(selected, id) {
			return nil, errors.New("activation source repair may only reduce persistent selection")
		}
	}
	if choice.name == "CODING_TOOL_INTEGRATIONS" {
		selection, err := config.ResolveIntegrationSelection(loaded.Environment, requested)
		if err != nil {
			return nil, err
		}
		for _, removed := range selected {
			if !slices.Contains(requested, removed) && slices.Contains(selection.Effective, removed) {
				return nil, fmt.Errorf("cannot disable %s: required by %s", removed, strings.Join(selection.DependencyReasons[removed], ", "))
			}
		}
	}
	return config.EditPersistentAssignmentContent(path, content, choice.name, &choice.value)
}

func (cli *CLI) checkSelectionReduction(before, after config.Loaded, name string) error {
	previous, next := before.Integrations.Effective, after.Integrations.Effective
	if name == "ENVIRONMENT_PROFILES" {
		definitions, err := profile.Load(cli.options.RepositoryRoot)
		if err != nil {
			return err
		}
		selection := func(loaded config.Loaded) ([]string, error) {
			roots, err := cli.currentProfileSelection(loaded, definitions)
			if err != nil {
				return nil, err
			}
			provision, err := cli.prepareProvisionExecution(loaded, nil, nil)
			if err != nil {
				return nil, err
			}
			// Explicit empty roots still run eligible legacy provision hooks.
			// Keep descriptor-only roots in the same affected-yard guard.
			for _, id := range provision.profiles {
				if !slices.Contains(roots, id) {
					roots = append(roots, id)
				}
			}
			return roots, nil
		}
		previous, err = selection(before)
		if err != nil {
			return err
		}
		next, err = selection(after)
		if err != nil {
			return err
		}
	}
	for _, id := range next {
		if !slices.Contains(previous, id) {
			return fmt.Errorf("yard %s: activation source repair may only reduce persistent selection", before.Context.YardName)
		}
	}
	return nil
}

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

type configTarget struct {
	Name   string
	Loaded config.Loaded
}

type configAsset struct {
	Name        string
	Source      string
	Destination string
	Scope       string
	Role        string
}

func (cli *CLI) runConfig(ctx context.Context, loaded config.Loaded, arguments []string) int {
	assumeYes := cli.env["ASSUME_YES"] == "1"
	if len(arguments) != 0 && (arguments[0] == "-y" || arguments[0] == "--yes") {
		assumeYes = true
		arguments = arguments[1:]
	}
	if len(arguments) == 0 || commandHelpRequested(arguments) {
		fmt.Fprintf(cli.options.Stdout,
			"Usage: %s config fields [SETTING] | show [SETTING] | paths | set|unset|import|edit ... | status [--all-local] | apply [--all-local] [--yes] | sync <command>\n"+
				"  fields  list the typed public settings contract (read-only)\n"+
				"  show    explain effective Subyard settings and their sources (read-only)\n"+
				"  paths   list configuration sources and storage roles (read-only)\n"+
				"  set     write a typed persistent scalar setting\n"+
				"  unset   remove a persistent scalar setting\n"+
				"  import  replace a typed persistent file setting from a file\n"+
				"  edit    edit a typed persistent file setting with VISUAL or EDITOR\n"+
				"  status  check materialized file settings in running local yards (read-only)\n"+
				"  apply   refresh materialized file settings in running local yards\n"+
				"  sync    connect, inspect, pull, push or import versioned non-secret settings\n",
			cli.options.Program)
		return 0
	}
	action := arguments[0]
	switch action {
	case "fields":
		if len(arguments) > 2 {
			cli.errorf("config fields accepts at most one setting name")
			return 2
		}
		name := ""
		if len(arguments) == 2 {
			name = arguments[1]
		}
		return cli.writeConfigFields(loaded, name)
	case "show":
		if len(arguments) > 2 {
			cli.errorf("config show accepts at most one setting name")
			return 2
		}
		name := ""
		if len(arguments) == 2 {
			name = arguments[1]
		}
		return cli.writeConfigShow(loaded, name)
	case "paths":
		if len(arguments) != 1 {
			cli.errorf("config paths accepts no options")
			return 2
		}
		return cli.writeConfigPaths(loaded)
	case "sync":
		return cli.runConfigSync(ctx, loaded, arguments[1:], assumeYes)
	case "set", "unset", "import", "edit":
		return cli.runConfigAuthoring(
			ctx, loaded, action, arguments[1:], assumeYes,
		)
	}
	allLocal := false
	for _, argument := range arguments[1:] {
		switch argument {
		case "--all-local":
			allLocal = true
		case "-y", "--yes":
			assumeYes = true
		default:
			cli.errorf("config %s: unknown argument %q", action, argument)
			return 2
		}
	}
	switch action {
	case "status", "apply":
	default:
		cli.errorf(
			"config expects fields, show, paths, set, unset, import, edit, status, apply or sync",
		)
		return 2
	}
	targets, err := cli.localConfigTargets(loaded, allLocal)
	if err != nil {
		cli.errorf("config %s: %v", action, err)
		return 1
	}
	if action == "status" {
		if err := cli.configStatus(ctx, targets, true); err != nil {
			cli.errorf("config status: %v", err)
			return 1
		}
		return 0
	}
	return cli.applyConfig(ctx, targets, assumeYes)
}

func (cli *CLI) writeConfigFields(loaded config.Loaded, requested string) int {
	if requested != "" {
		definition, ok := config.LookupSetting(requested)
		if !ok {
			cli.errorf("config fields: unknown setting %q", requested)
			return 2
		}
		writeSettingDefinition(
			cli.options.Stdout, config.ResolvedSettingDefinition(loaded, definition),
		)
		return 0
	}
	writer := tabwriter.NewWriter(cli.options.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "SETTING\tKIND\tTYPE\tDEFAULT\tSCOPES\tSYNCABLE\tMERGE\tAPPLIES\tOWNER")
	for _, definition := range config.ResolvedSettingCatalog(loaded) {
		defaultValue := "<none>"
		if definition.HasDefault {
			defaultValue = settingDisplayValue(
				definition.Default, definition.Sensitive, true,
			)
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			definition.Name, definition.Kind, definition.Type,
			defaultValue, settingScopeList(definition.Scopes), yesNo(definition.Syncable),
			definition.Merge, definition.Application, definition.Owner)
	}
	_ = writer.Flush()
	fmt.Fprintln(cli.options.Stdout,
		"AGENT_<name>_{CONFIG,RULES,CONFIG_DEST,RULES_DEST,PROVISION,COMMAND,CHECK,DEPENDS,PERSIST} are typed catalog patterns.")
	return 0
}

func (cli *CLI) runConfigSync(
	ctx context.Context,
	loaded config.Loaded,
	arguments []string,
	assumeYes bool,
) int {
	if len(arguments) != 0 {
		switch arguments[0] {
		case "help":
			if len(arguments) != 1 {
				cli.errorf("config sync help accepts no arguments")
				return 2
			}
			cli.writeConfigSyncHelp()
			return 0
		case "connect":
			return cli.runConfigSyncConnect(ctx, loaded, arguments[1:], assumeYes)
		case "path":
			return cli.runConfigSyncPath(ctx, loaded, arguments[1:])
		case "status":
			return cli.runConfigSyncStatus(ctx, loaded, arguments[1:])
		case "pull":
			return cli.runConfigSyncPull(ctx, loaded, arguments[1:], assumeYes)
		case "push":
			return cli.runConfigSyncPush(ctx, loaded, arguments[1:], assumeYes)
		}
	}
	if commandHelpRequested(arguments) {
		cli.writeConfigSyncHelp()
		return 0
	}
	if loaded.Context.YardType == domain.YardRemote {
		forwarded := append([]string{"sync"}, arguments...)
		if assumeYes {
			forwarded = append(forwarded, "--yes")
		}
		return cli.forwardRemote(ctx, loaded.Context, "config", forwarded)
	}
	source, check, adopt, materialize := "", false, false, false
	for _, argument := range arguments {
		switch argument {
		case "--check":
			check = true
		case "--adopt":
			adopt = true
		case "--apply":
			materialize = true
		case "-y", "--yes":
			assumeYes = true
		default:
			if strings.HasPrefix(argument, "-") {
				cli.errorf("config sync: unknown option %q", argument)
				return 2
			}
			if source != "" {
				cli.errorf("config sync accepts exactly one checkout path")
				return 2
			}
			source = argument
		}
	}
	if check && materialize {
		cli.errorf("config sync: --check and --apply cannot be used together")
		return 2
	}
	if source == "" {
		record, exists, err := configsync.ReadSourceRecord(
			loaded.Context.Paths.ConfigHome,
		)
		if err != nil {
			cli.errorf("config sync: read registered source: %v", err)
			return 1
		}
		if !exists {
			cli.errorf(
				"config sync: no checkout was provided or registered; run %s config sync connect <git-url>",
				cli.options.Program,
			)
			return 2
		}
		source = record.Checkout
	}
	if !check {
		if err := configsync.Recover(loaded.Context.Paths.ConfigHome); err != nil {
			cli.errorf("config sync recovery: %v", err)
			return 1
		}
	}
	options := configsync.Options{
		SourceRoot: source, ConfigHome: loaded.Context.Paths.ConfigHome,
		RepositoryRoot: cli.options.RepositoryRoot,
		OperatorHome:   loaded.Context.Paths.OperatorHome,
		Environment:    cli.baseEnv, FileSettings: config.SyncableFileMappings(loaded),
		Adopt: adopt, YardInUse: cli.configSyncYardInUse(loaded),
	}
	plan, err := configsync.BuildPlan(options)
	if err != nil {
		if errors.Is(err, configsync.ErrRecoveryPending) && check {
			cli.errorf("config sync --check: interrupted transaction requires recovery by a normal config sync")
		} else {
			cli.errorf("config sync: %v", err)
		}
		return 1
	}
	writeConfigSyncPlan(cli.options.Stdout, plan)
	if check {
		if plan.NeedsApply() {
			fmt.Fprintln(cli.options.Stdout, "config sync check: changes required")
			return 1
		}
		fmt.Fprintln(cli.options.Stdout, "config sync check: converged")
		return 0
	}
	if plan.NeedsConfirmation() {
		consequences := make([]string, 0, len(plan.Changes)+1)
		if plan.InitializeHostID {
			consequences = append(consequences, "record owner host ID "+plan.HostID)
		}
		for _, change := range plan.Changes {
			consequences = append(consequences, change.Action+" "+change.Path)
		}
		if materialize && configSyncPlanNeedsMaterialization(plan) {
			consequences = append(consequences,
				"refresh affected file settings in running local yards")
		}
		orchestrator := cli.operationOrchestrator(
			cli.env["SUBYARD_OPERATION_ID"], loaded, nil, nil,
		)
		operation, err := orchestrator.Prepare(loaded.Context, domain.CommandPolicy{
			Name: "config sync", Effect: domain.CommandMutate, Confirmation: domain.ConfirmationRequired,
			RemotePolicy: domain.RemoteOnOwner, Consequences: consequences,
		})
		if err != nil {
			cli.errorf("config sync: %v", err)
			return 1
		}
		operation, err = orchestrator.Confirm(ctx, operation, assumeYes)
		if errors.Is(err, application.ErrDeclined) {
			cli.errorf("config sync: operation declined")
			return 1
		}
		if err != nil {
			cli.errorf("config sync: %v", err)
			return 1
		}
		orchestrator.Runner = configSyncAdapter{plan: plan}
		if _, _, err := orchestrator.RunAdapter(ctx, operation, domain.AdapterRequest{
			OperationID: operation.OperationID, Adapter: "config-sync", Action: "apply",
		}, nil); err != nil {
			cli.errorf("config sync: %v", err)
			return 1
		}
	} else if err := configsync.Apply(plan); err != nil {
		cli.errorf("config sync: %v", err)
		return 1
	}
	if !plan.NeedsApply() {
		fmt.Fprintln(cli.options.Stdout, "config sync: already converged")
		return 0
	}
	fmt.Fprintf(cli.options.Stdout, "config sync: applied generation %d\n", plan.Generation)
	if materialize {
		if err := cli.materializeConfigSyncPlan(ctx, loaded, plan, true); err != nil {
			cli.errorf("config sync --apply: %v", err)
			return 1
		}
	}
	cli.writeConfigSyncFollowups(loaded, plan, materialize)
	return 0
}

func (cli *CLI) writeConfigSyncHelp() {
	fmt.Fprintf(cli.options.Stdout,
		"Usage: %s config sync [checkout] [--check] [--adopt] [--apply] [--yes]\n"+
			"       %s config sync connect <git-url> [--host-id ID] [--checkout PATH] [--init] [--apply] [--yes]\n"+
			"       %s config sync path\n"+
			"       %s config sync status [--offline]\n"+
			"       %s config sync pull [--apply] [--yes]\n"+
			"       %s config sync push -m <message> [--apply] [--yes]\n\n"+
			"Versioned sync is manual; no background pull or push runs automatically.\n"+
			"  connect  clone, register and import a private Git configuration source\n"+
			"  path     print the registered owner-host checkout path\n"+
			"  status   show registration, Git relation, conflicts and applied generation\n"+
			"  pull     fetch, fast-forward and transactionally import remote settings\n"+
			"  push     export persistent syncable settings, commit and push upstream\n"+
			"  --apply  also refresh affected file settings in running local yards\n\n"+
			"Examples:\n"+
			"  %s config sync connect <git-url> --apply\n"+
			"  %s config sync status\n"+
			"  %s config sync pull --apply\n"+
			"  %s config sync push -m \"Update host configuration\" --apply\n\n"+
			"push uses the current operator account's configured Git author. It exports only\n"+
			"explicit catalog-known, syncable, non-secret persistent settings. It never reads\n"+
			"configuration back from running containers and never exports keys, secrets,\n"+
			"project data, generated state or arbitrary runtime files.\n",
		cli.options.Program, cli.options.Program, cli.options.Program,
		cli.options.Program, cli.options.Program, cli.options.Program,
		cli.options.Program, cli.options.Program, cli.options.Program,
		cli.options.Program)
}

type configSyncAdapter struct {
	plan configsync.Plan
}

func (adapter configSyncAdapter) Run(
	_ context.Context,
	request domain.AdapterRequest,
	_ io.Reader,
) (domain.AdapterResult, string, error) {
	if request.Adapter != "config-sync" || request.Action != "apply" {
		return domain.AdapterResult{}, "", errors.New("invalid config sync adapter request")
	}
	if err := configsync.Apply(adapter.plan); err != nil {
		return domain.AdapterResult{}, "", err
	}
	return domain.AdapterResult{
		Schema: 1, OperationID: request.OperationID, Status: "ok",
		Output: map[string]any{"generation": adapter.plan.Generation},
	}, "", nil
}

func writeConfigSyncPlan(output io.Writer, plan configsync.Plan) {
	fmt.Fprintln(output, "Versioned configuration sync")
	fmt.Fprintf(output, "  source-commit: %s\n", plan.SourceCommit)
	fmt.Fprintf(output, "  source-digest: %s\n", plan.SourceDigest)
	fmt.Fprintf(output, "  owner-host: %s\n", plan.HostID)
	fmt.Fprintf(output, "  generation: %d -> %d\n",
		plan.PreviousGeneration, plan.Generation)
	if plan.InitializeHostID {
		fmt.Fprintln(output, "  initialize: host-id")
	}
	if len(plan.Changes) == 0 {
		if plan.ManifestChanged {
			fmt.Fprintln(output, "  managed paths: unchanged (manifest metadata update)")
		} else {
			fmt.Fprintln(output, "  managed paths: unchanged")
		}
		return
	}
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "  ACTION\tPATH\tDIGEST\tAPPLIES\tDETAIL")
	for _, change := range plan.Changes {
		applications := make([]string, 0, len(change.Applications))
		for _, application := range change.Applications {
			applications = append(applications, string(application))
		}
		detail := change.Detail
		if detail == "" {
			detail = "-"
		}
		fmt.Fprintf(writer, "  %s\t%s\t%s\t%s\t%s\n",
			change.Action, change.Path, configSyncDigestTransition(change),
			strings.Join(applications, ","), detail)
	}
	_ = writer.Flush()
}

func configSyncDigestTransition(change configsync.Change) string {
	short := func(value string) string {
		if value == "" {
			return "-"
		}
		if len(value) > 12 {
			return value[:12]
		}
		return value
	}
	return short(change.BeforeDigest) + "->" + short(change.AfterDigest)
}

func (cli *CLI) configSyncYardInUse(loaded config.Loaded) configsync.YardUseProbe {
	return func(name string) (string, bool, error) {
		projectRoot := filepath.Join(
			loaded.Context.Paths.ConfigHome, "yards", name, "projects",
		)
		entries, err := os.ReadDir(projectRoot)
		if err == nil && len(entries) != 0 {
			return "project state exists", true, nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", false, err
		}
		target, err := cli.loadInventoryLoaded(name, loaded)
		if err != nil {
			return "", false, err
		}
		incus, _ := cli.statusPorts()
		_, err = incus.Instance(
			context.Background(), target.Context.IncusProject, target.Context.InstanceName,
		)
		if err == nil {
			return "managed Incus yard exists", true, nil
		}
		if errors.Is(err, ports.ErrInstanceNotFound) {
			return "", false, nil
		}
		return "", false, err
	}
}

func (cli *CLI) writeConfigSyncFollowups(
	loaded config.Loaded,
	plan configsync.Plan,
	materialized bool,
) {
	needsApply, needsInit, nextCommand := false, false, false
	initAll, applyAll := false, false
	initYards, applyYards := map[string]struct{}{}, map[string]struct{}{}
	for _, change := range plan.Changes {
		yard, scoped := configSyncPathYard(change.Path)
		for _, application := range change.Applications {
			switch application {
			case config.SettingNextCommand:
				nextCommand = true
			case config.SettingConfigApply:
				needsApply = true
				if scoped {
					applyYards[yard] = struct{}{}
				} else {
					applyAll = true
				}
			case config.SettingYardInit:
				needsInit = true
				if scoped {
					initYards[yard] = struct{}{}
				} else {
					initAll = true
				}
			}
		}
	}
	if !nextCommand && !needsApply && !needsInit {
		return
	}
	fmt.Fprintln(cli.options.Stdout, "Next actions")
	if nextCommand {
		fmt.Fprintln(cli.options.Stdout, "  next command: effective automatically")
	}
	if needsApply && !materialized {
		if applyAll {
			fmt.Fprintf(cli.options.Stdout, "  config apply: %s config apply --all-local\n",
				cli.options.Program)
		} else {
			for _, name := range sortedSet(applyYards) {
				fmt.Fprintf(cli.options.Stdout, "  config apply: %s -Y %s config apply\n",
					cli.options.Program, name)
			}
		}
	}
	if needsInit {
		names := sortedSet(initYards)
		if initAll {
			names = cli.configSyncLocalYards(loaded)
		}
		for _, name := range names {
			if name == "default" {
				fmt.Fprintf(cli.options.Stdout, "  yard init: %s init\n", cli.options.Program)
			} else {
				fmt.Fprintf(cli.options.Stdout, "  yard init: %s -Y %s init\n",
					cli.options.Program, name)
			}
		}
	}
}

func (cli *CLI) configSyncLocalYards(loaded config.Loaded) []string {
	directories := config.RegistryDirectories(
		loaded.Context.Paths.ConfigDir, loaded.Context.Paths.ConfigHome,
	)
	names, err := config.YardNames(directories...)
	if err != nil {
		return []string{"default"}
	}
	var result []string
	for _, name := range names {
		target, err := cli.loadInventoryLoaded(name, loaded)
		if err == nil && target.Context.YardType != domain.YardRemote {
			result = append(result, name)
		}
	}
	return result
}

func (cli *CLI) materializeConfigSyncPlan(
	ctx context.Context,
	loaded config.Loaded,
	plan configsync.Plan,
	assumeYes bool,
) error {
	all := false
	names := map[string]struct{}{}
	for _, change := range plan.Changes {
		applies := false
		for _, application := range change.Applications {
			if application == config.SettingConfigApply {
				applies = true
				break
			}
		}
		if !applies {
			continue
		}
		if name, scoped := configSyncPathYard(change.Path); scoped {
			names[name] = struct{}{}
		} else {
			all = true
		}
	}
	if !all && len(names) == 0 {
		fmt.Fprintln(cli.options.Stdout,
			"config sync --apply: no materialized file settings changed")
		return nil
	}
	var targets []configTarget
	var err error
	if all {
		targets, err = cli.localConfigTargets(loaded, true)
	} else {
		for _, name := range sortedSet(names) {
			target, loadErr := cli.loadInventoryLoaded(name, loaded)
			if loadErr != nil {
				return fmt.Errorf("yard %s: %w", name, loadErr)
			}
			if target.Context.YardType != domain.YardRemote {
				targets = append(targets, configTarget{Name: name, Loaded: target})
			}
		}
	}
	if err != nil {
		return err
	}
	if code := cli.applyConfig(ctx, targets, assumeYes); code != 0 {
		return errors.New("materialized configuration refresh failed")
	}
	return nil
}

func configSyncPathYard(path string) (string, bool) {
	parts := strings.Split(filepath.ToSlash(path), "/")
	if len(parts) >= 3 && parts[0] == "yards" && domain.SafeName(parts[1]) {
		return parts[1], true
	}
	return "", false
}

func sortedSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func writeSettingDefinition(output io.Writer, definition config.SettingDefinition) {
	fmt.Fprintf(output, "setting: %s\n", definition.Name)
	fmt.Fprintf(output, "kind: %s\n", definition.Kind)
	fmt.Fprintf(output, "type: %s\n", definition.Type)
	defaultValue := "<none>"
	if definition.HasDefault {
		defaultValue = settingDisplayValue(definition.Default, definition.Sensitive, false)
	}
	fmt.Fprintf(output, "default: %s\n", defaultValue)
	fmt.Fprintf(output, "scopes: %s\n", settingScopeList(definition.Scopes))
	fmt.Fprintf(output, "syncable: %s\n", yesNo(definition.Syncable))
	fmt.Fprintf(output, "sensitive: %s\n", yesNo(definition.Sensitive))
	fmt.Fprintf(output, "merge: %s\n", definition.Merge)
	fmt.Fprintf(output, "applies: %s\n", definition.Application)
	fmt.Fprintf(output, "owner: %s\n", definition.Owner)
	if len(definition.Aliases) != 0 {
		fmt.Fprintf(output, "deprecated-aliases: %s\n", strings.Join(definition.Aliases, ", "))
	}
	if len(definition.Enum) != 0 {
		fmt.Fprintf(output, "values: %s\n", strings.Join(definition.Enum, ", "))
	}
	if definition.Minimum != 0 || definition.Maximum != 0 {
		fmt.Fprintf(output, "range: %d..%d\n", definition.Minimum, definition.Maximum)
	}
}

func settingScopeList(scopes []config.SettingScope) string {
	values := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		values = append(values, string(scope))
	}
	return strings.Join(values, ",")
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func (cli *CLI) writeConfigShow(loaded config.Loaded, requested string) int {
	if requested != "" {
		name, trace, ok := findSettingTrace(loaded.Settings, requested)
		if !ok {
			cli.errorf("config show: unknown setting %q", requested)
			return 2
		}
		writeSettingTrace(cli.options.Stdout, name, trace)
		return 0
	}

	writer := tabwriter.NewWriter(cli.options.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "SETTING\tEFFECTIVE\tSCOPE\tSOURCE\tAPPLIES")
	for _, name := range config.SettingNames(loaded.Settings) {
		trace := loaded.Settings[name]
		resolution, configured := effectiveSettingResolution(trace)
		if !configured && trace.EffectiveValue == "" {
			continue
		}
		scope, source := "-", "-"
		if configured {
			scope = resolution.Scope
			source = settingResolutionSource(resolution)
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n",
			name, settingDisplayValue(trace.EffectiveValue, trace.Sensitive, true),
			scope, source, trace.Application)
	}
	_ = writer.Flush()
	return 0
}

func findSettingTrace(
	settings map[string]config.SettingTrace,
	requested string,
) (string, config.SettingTrace, bool) {
	if trace, ok := settings[requested]; ok {
		return requested, trace, true
	}
	for name, trace := range settings {
		if strings.EqualFold(name, requested) {
			return name, trace, true
		}
	}
	return "", config.SettingTrace{}, false
}

func writeSettingTrace(output io.Writer, name string, trace config.SettingTrace) {
	fmt.Fprintf(output, "setting: %s\n", name)
	fmt.Fprintf(output, "effective: %s\n",
		settingDisplayValue(trace.EffectiveValue, trace.Sensitive, false))
	fmt.Fprintf(output, "type: %s %s\n", trace.Kind, trace.Type)
	fmt.Fprintf(output, "allowed-scopes: %s\n", settingScopeList(trace.Scopes))
	fmt.Fprintf(output, "syncable: %s\n", yesNo(trace.Syncable))
	fmt.Fprintf(output, "merge: %s\n", trace.Merge)
	fmt.Fprintf(output, "applies: %s\n", trace.Application)
	fmt.Fprintf(output, "owner: %s\n", trace.Owner)
	if len(trace.Aliases) != 0 {
		fmt.Fprintf(output, "deprecated-aliases: %s\n", strings.Join(trace.Aliases, ", "))
	}
	fmt.Fprintln(output, "precedence:")
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "  SCOPE\tROLE\tSOURCE\tVALUE\tSTATUS")
	for _, resolution := range trace.Resolutions {
		value := "<unset>"
		if resolution.Status != "unset" {
			value = settingDisplayValue(resolution.Value, trace.Sensitive, false)
		}
		source := settingResolutionSource(resolution)
		if resolution.Detail != "" {
			source += " (" + resolution.Detail + ")"
		}
		fmt.Fprintf(writer, "  %s\t%s\t%s\t%s\t%s\n",
			resolution.Scope, resolution.Role, source, value, resolution.Status)
	}
	_ = writer.Flush()
}

func effectiveSettingResolution(
	trace config.SettingTrace,
) (config.SettingResolution, bool) {
	for _, resolution := range trace.Resolutions {
		if resolution.Status == "effective" {
			return resolution, true
		}
	}
	return config.SettingResolution{}, false
}

func settingResolutionSource(resolution config.SettingResolution) string {
	source := resolution.Path
	if source == "" {
		source = resolution.Role
	}
	if resolution.Line > 0 {
		source += fmt.Sprintf(":%d", resolution.Line)
	}
	return source
}

func settingDisplayValue(value string, sensitive, compact bool) string {
	if sensitive {
		return "[redacted]"
	}
	if value == "" {
		return "<unset>"
	}
	value = strings.NewReplacer(
		"\\", "\\\\", "\n", "\\n", "\r", "\\r", "\t", "\\t",
	).Replace(value)
	characters := []rune(value)
	if compact && len(characters) > 72 {
		return string(characters[:69]) + "..."
	}
	return value
}

func (cli *CLI) writeConfigPaths(loaded config.Loaded) int {
	values := loaded.Environment
	fmt.Fprintf(cli.options.Stdout, "shipped-defaults: %s\n", loaded.Context.Paths.ConfigDir)
	fmt.Fprintf(cli.options.Stdout, "configuration-root: %s\n", loaded.Context.Paths.ConfigHome)
	for _, layer := range loaded.ConfigurationLayers {
		state := "missing"
		if layer.Present {
			state = "present"
		}
		label := strings.ReplaceAll(layer.Scope+"-"+layer.Role, " ", "-")
		fmt.Fprintf(cli.options.Stdout, "source %s: %s (%s)\n", label, layer.Path, state)
	}
	for _, layer := range []struct {
		name, key string
	}{
		{"shared-file-settings", "SUBYARD_CONFIG_SHARED_DIR"},
		{"host-file-settings", "SUBYARD_CONFIG_HOST_DIR"},
		{"yard-configuration", "SUBYARD_CONFIG_YARD_DIR"},
		{"secret-inputs", "SUBYARD_CONFIG_SECRETS_DIR"},
		{"generated-consumers", "SUBYARD_CONFIG_GENERATED_DIR"},
	} {
		value := values[layer.key]
		if value == "" {
			value = "-"
		}
		fmt.Fprintf(cli.options.Stdout, "%s: %s\n", layer.name, value)
	}
	fmt.Fprintf(cli.options.Stdout, "credential-ledger: %s\n",
		filepath.Join(loaded.Context.Paths.ConfigHome, "keys"))
	fmt.Fprintf(cli.options.Stdout, "project-state: %s\n", loaded.Context.Paths.StateDir)
	fmt.Fprintf(cli.options.Stdout, "support-tools: %s\n",
		filepath.Join(loaded.Context.Paths.ConfigHome, "tools"))
	syncStatus, err := configsync.ReadStatus(
		loaded.Context.Paths.ConfigHome, cli.baseEnv,
	)
	if err != nil {
		cli.errorf("config paths: %v", err)
		return 1
	}
	hostIDState := "saved"
	if syncStatus.HostIDPending {
		hostIDState = "proposed; saved by first config sync"
	}
	fmt.Fprintf(cli.options.Stdout, "owner-host-id: %s (%s)\n",
		syncStatus.HostID, hostIDState)
	fmt.Fprintf(cli.options.Stdout, "versioned-config-manifest: %s\n",
		syncStatus.ManifestPath)
	sourceRecord, sourceRegistered, err := configsync.ReadSourceRecord(
		loaded.Context.Paths.ConfigHome,
	)
	if err != nil {
		cli.errorf("config paths: %v", err)
		return 1
	}
	if sourceRegistered {
		fmt.Fprintf(cli.options.Stdout, "versioned-config-checkout: %s\n",
			sourceRecord.Checkout)
	} else {
		fmt.Fprintln(cli.options.Stdout, "versioned-config-checkout: <not registered>")
	}
	if syncStatus.ManifestPresent {
		fmt.Fprintf(cli.options.Stdout, "versioned-config-source-commit: %s\n",
			syncStatus.SourceCommit)
		fmt.Fprintf(cli.options.Stdout, "versioned-config-generation: %d\n",
			syncStatus.Generation)
	}
	if syncStatus.RecoveryRequired {
		fmt.Fprintln(cli.options.Stdout, "versioned-config-recovery: required")
	}
	assets, err := effectiveConfigAssets(loaded)
	if err != nil {
		cli.errorf("config paths: %v", err)
		return 1
	}
	for _, asset := range assets {
		scope, role := asset.Scope, asset.Role
		if scope == "" {
			scope = configPathScope(asset.Source, values)
		}
		if role == "" {
			role = "file setting"
		}
		fmt.Fprintf(cli.options.Stdout,
			"file-setting %s: %s (scope=%s, role=%s, consumer=%s)\n",
			asset.Name, asset.Source, scope, role, asset.Destination)
	}
	return 0
}

func configPathScope(path string, values map[string]string) string {
	for _, layer := range []struct {
		key, name string
	}{
		{"SUBYARD_CONFIG_YARD_DIR", "yard"},
		{"SUBYARD_CONFIG_HOST_DIR", "host"},
		{"SUBYARD_CONFIG_SHARED_DIR", "shared"},
		{"SUBYARD_CONFIG_GENERATED_DIR", "generated"},
		{"SUBYARD_CONFIG_SECRETS_DIR", "secret"},
		{"SUBYARD_CONFIG_DIR", "default"},
	} {
		if root := values[layer.key]; root != "" && pathWithinCLI(path, root) {
			return layer.name
		}
	}
	return "environment-or-external"
}

func (cli *CLI) localConfigTargets(loaded config.Loaded, allLocal bool) ([]configTarget, error) {
	if !allLocal {
		return []configTarget{{Name: loaded.Context.YardName, Loaded: loaded}}, nil
	}
	names := map[string]struct{}{"default": {}}
	yardsRoot := filepath.Join(loaded.Context.Paths.ConfigHome, "yards")
	entries, err := os.ReadDir(yardsRoot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			if domain.SafeName(name) {
				if info, statErr := os.Lstat(filepath.Join(yardsRoot, name, "config.env")); statErr == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
					names[name] = struct{}{}
				}
			}
			continue
		}
		if filepath.Ext(name) == ".env" {
			legacy := strings.TrimSuffix(name, ".env")
			if domain.SafeName(legacy) {
				names[legacy] = struct{}{}
			}
		}
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	targets := make([]configTarget, 0, len(sorted))
	for _, name := range sorted {
		targetLoaded, err := cli.loadInventoryLoaded(name, loaded)
		if err != nil {
			return nil, fmt.Errorf("yard %s: %w", name, err)
		}
		if targetLoaded.Context.YardType == domain.YardRemote {
			continue
		}
		targets = append(targets, configTarget{Name: name, Loaded: targetLoaded})
	}
	return targets, nil
}

func (cli *CLI) configStatus(
	ctx context.Context,
	targets []configTarget,
	checkDrift bool,
) error {
	if len(targets) == 0 {
		return errors.New("no local yards selected")
	}
	configHome := targets[0].Loaded.Context.Paths.ConfigHome
	if err := validateManagedConfigTree(configHome); err != nil {
		return err
	}
	legacy, err := legacyConfigInputs(targets[0].Loaded)
	if err != nil {
		return err
	}
	for _, path := range legacy {
		fmt.Fprintf(cli.options.Stdout, "attention: legacy input requires review/import: %s\n", path)
	}
	var drifted []string
	for _, target := range targets {
		state, drift, err := cli.configTargetDrift(ctx, target, checkDrift)
		if err != nil {
			return fmt.Errorf("yard %s: %w", target.Name, err)
		}
		fmt.Fprintf(cli.options.Stdout, "yard %s materialized-config: %s\n", target.Name, state)
		if drift {
			drifted = append(drifted, target.Name)
		}
	}
	if len(drifted) != 0 {
		return fmt.Errorf(
			"materialized agent config drift in yards: %s", strings.Join(drifted, ", "),
		)
	}
	return nil
}

func (cli *CLI) applyConfig(ctx context.Context, targets []configTarget, assumeYes bool) int {
	if len(targets) == 0 {
		cli.errorf("config apply: no local yards selected")
		return 1
	}
	if err := validateManagedConfigTree(targets[0].Loaded.Context.Paths.ConfigHome); err != nil {
		cli.errorf("config apply: %v", err)
		return 1
	}
	running := make([]configTarget, 0, len(targets))
	for _, target := range targets {
		if target.Loaded.Context.YardType == domain.YardRemote {
			cli.errorf("config apply does not implicitly operate on remote yard %s", target.Name)
			return 1
		}
		state, _, err := cli.configTargetDrift(ctx, target, false)
		if err != nil {
			cli.errorf("config apply: yard %s: %v", target.Name, err)
			return 1
		}
		if state == "running" || state == "drift" {
			running = append(running, target)
		} else {
			fmt.Fprintf(cli.options.Stdout,
				"yard %s materialized-config: %s; skipped\n", target.Name, state)
		}
	}
	if len(running) == 0 {
		fmt.Fprintln(cli.options.Stdout, "config apply: no running local yards to refresh")
		return 0
	}
	if !assumeYes {
		prompt := cli.options.Prompt
		if prompt == nil {
			prompt = streamPrompt{input: cli.options.Stdin, output: cli.options.Stderr}
		}
		names := make([]string, 0, len(running))
		for _, target := range running {
			names = append(names, target.Name)
		}
		accepted, err := prompt.Confirm(ctx, "Apply Subyard file settings",
			[]string{
				"refresh materialized agent configs in local running yards: " +
					strings.Join(names, ", "),
			})
		if err != nil {
			cli.errorf("config apply: %v", err)
			return 1
		}
		if !accepted {
			cli.errorf("config apply: operation declined")
			return 1
		}
	}
	applier := cli.options.Config
	if applier == nil {
		applier = dispatcherConfigApplier{
			path: cli.options.DispatcherPath, environment: cli.baseEnv,
			stdout: cli.options.Stdout, stderr: cli.options.Stderr,
		}
	}
	for _, target := range running {
		if err := applier.ApplyConfig(ctx, target.Name); err != nil {
			cli.errorf("config apply: yard %s: %v", target.Name, err)
			return 1
		}
	}
	if err := cli.configStatus(ctx, running, true); err != nil {
		cli.errorf("config apply verification: %v", err)
		return 1
	}
	return 0
}

type dispatcherConfigApplier struct {
	path        string
	environment map[string]string
	stdout      io.Writer
	stderr      io.Writer
	applyDrift  bool
}

func (applier dispatcherConfigApplier) ApplyConfig(ctx context.Context, yard string) error {
	arguments := []string{}
	if yard != "" && yard != "default" {
		arguments = append(arguments, "-Y", yard)
	}
	if applier.applyDrift {
		arguments = append(arguments, "config", "apply", "--yes")
	} else {
		arguments = append(arguments, "init", "--configs", "--yes")
	}
	command := exec.CommandContext(ctx, applier.path, arguments...)
	command.Env = environmentList(applier.environment, map[string]string{"ASSUME_YES": "1"})
	command.Stdin = strings.NewReader("")
	command.Stdout = applier.stdout
	command.Stderr = applier.stderr
	return command.Run()
}

func (cli *CLI) configTargetDrift(
	ctx context.Context,
	target configTarget,
	check bool,
) (string, bool, error) {
	if target.Loaded.Context.YardType == domain.YardRemote {
		return "remote (settings only; consumers not checked)", false, nil
	}
	incus, executor := cli.statusPorts()
	info, err := incus.Instance(ctx, target.Loaded.Context.IncusProject,
		target.Loaded.Context.InstanceName)
	if errors.Is(err, ports.ErrInstanceNotFound) {
		return "absent", false, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return "absent", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !strings.EqualFold(info.Status, "running") {
		return "stopped", false, nil
	}
	if !check {
		return "running", false, nil
	}
	assets, err := effectiveConfigAssets(target.Loaded)
	if err != nil {
		return "", false, err
	}
	for _, asset := range assets {
		hostHash, err := hashRegularFile(asset.Source)
		if err != nil {
			return "", false, fmt.Errorf("%s: %w", asset.Name, err)
		}
		result, err := executor.Exec(ctx, target.Loaded.Context.IncusProject,
			target.Loaded.Context.InstanceName, ports.InstanceExecRequest{
				Command: []string{"sha256sum", "--", asset.Destination},
				User:    uint32(target.Loaded.Context.DevUID),
				Group:   uint32(target.Loaded.Context.DevUID),
			})
		if err != nil || result.ExitCode != 0 {
			return "drift", true, nil
		}
		fields := strings.Fields(string(result.Stdout))
		if len(fields) == 0 || fields[0] != hostHash {
			return "drift", true, nil
		}
	}
	return "converged", false, nil
}

func effectiveConfigAssets(loaded config.Loaded) ([]configAsset, error) {
	values := loaded.Environment
	var result []configAsset
	for _, agent := range strings.Fields(values["AGENTS"]) {
		if !domain.SafeName(agent) {
			return nil, fmt.Errorf("invalid agent name %q", agent)
		}
		for _, kind := range []string{"CONFIG", "RULES"} {
			settingName := "AGENT_" + agent + "_" + kind
			source := values[settingName]
			destination := values["AGENT_"+agent+"_"+kind+"_DEST"]
			if source == "" || destination == "" {
				continue
			}
			if filepath.IsAbs(destination) || destination == ".." ||
				strings.HasPrefix(filepath.Clean(destination), ".."+string(filepath.Separator)) {
				return nil, fmt.Errorf("invalid %s %s destination", agent, kind)
			}
			asset := configAsset{
				Name: agent + "." + strings.ToLower(kind), Source: source,
				Destination: filepath.Join("/home", loaded.Context.DevUser, destination),
			}
			if trace, ok := loaded.Settings[settingName]; ok {
				if resolution, found := effectiveSettingResolution(trace); found {
					asset.Scope, asset.Role = resolution.Scope, resolution.Role
				}
			}
			result = append(result, asset)
		}
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Name < result[right].Name })
	return result, nil
}

func hashRegularFile(path string) (string, error) {
	return config.HashRegularFile(path)
}

func validateManagedConfigTree(root string) error {
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("config root must be a real directory")
	}
	uid := uint32(os.Getuid())
	if err := validateConfigOwnerMode(root, info, true, uid); err != nil {
		return err
	}
	for _, relative := range []string{"config.env", "overrides", "yards", "secrets", "generated"} {
		path := filepath.Join(root, relative)
		if err := filepath.WalkDir(path, func(path string, entry os.DirEntry, walkErr error) error {
			if errors.Is(walkErr, os.ErrNotExist) {
				return filepath.SkipDir
			}
			if walkErr != nil {
				return walkErr
			}
			info, err := os.Lstat(path)
			if err != nil {
				return err
			}
			return validateConfigOwnerMode(path, info, entry.IsDir(), uid)
		}); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func validateConfigOwnerMode(path string, info os.FileInfo, directory bool, uid uint32) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uid {
		return fmt.Errorf("config path is not operator-owned: %s", path)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("config path is a symlink: %s", path)
	}
	if directory {
		if !info.IsDir() {
			return fmt.Errorf("config path is not a directory: %s", path)
		}
		if info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("config directory is group/world writable: %s", path)
		}
		return nil
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("config path is not a regular file: %s", path)
	}
	sensitive := filepath.Base(path) == "config.env" ||
		strings.Contains(path, string(filepath.Separator)+"secrets"+string(filepath.Separator)) ||
		strings.Contains(path, string(filepath.Separator)+"generated"+string(filepath.Separator))
	if sensitive && info.Mode().Perm() != 0o600 {
		return fmt.Errorf("sensitive config file mode must be 0600: %s", path)
	}
	if !sensitive && info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("config file is group/world writable: %s", path)
	}
	return nil
}

func legacyConfigInputs(loaded config.Loaded) ([]string, error) {
	var result []string
	legacyRoot := filepath.Join(loaded.Context.Paths.ConfigHome, "secrets", "legacy")
	if err := filepath.WalkDir(legacyRoot, func(path string, entry os.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return filepath.SkipDir
		}
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			relative, _ := filepath.Rel(loaded.Context.Paths.ConfigHome, path)
			result = append(result, relative)
		}
		return nil
	}); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	flat, _ := filepath.Glob(filepath.Join(loaded.Context.Paths.ConfigHome, "yards", "*.env"))
	for _, path := range flat {
		relative, _ := filepath.Rel(loaded.Context.Paths.ConfigHome, path)
		result = append(result, relative)
	}
	if info, err := os.Lstat(filepath.Join(loaded.Context.Paths.DataHome, "operator-overlay")); err == nil && info.IsDir() {
		result = append(result, "legacy-data:operator-overlay (ignored)")
	}
	sort.Strings(result)
	return result, nil
}

func pathWithinCLI(path, root string) bool {
	path, root = filepath.Clean(path), filepath.Clean(root)
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

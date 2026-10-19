package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

func configEarlyInvocation(arguments []string) (string, []string) {
	yes := false
	if len(arguments) != 0 && (arguments[0] == "--yes" || arguments[0] == "-y") {
		yes = true
		arguments = arguments[1:]
	}
	if len(arguments) == 0 {
		return "", nil
	}
	rest := arguments[1:]
	if yes && (arguments[0] == "repair-alias" || arguments[0] == "reduce-selection") {
		rest = append(slices.Clone(rest), "--yes")
	}
	return arguments[0], rest
}

func (cli *CLI) configSourceEnvironment() map[string]string {
	values := map[string]string{}
	for name, value := range cli.baseEnv {
		if _, setting := cli.catalog.LookupSetting(name); setting || config.IsLegacySetting(name) {
			continue
		}
		values[name] = value
	}
	if values["SUBYARD_OPERATOR_HOME"] == "" {
		values["SUBYARD_OPERATOR_HOME"] = values["HOME"]
	}
	for _, name := range []string{"SUBYARD_CONFIG_HOME", "SUBYARD_CONFIG_DIR", "SUBYARD_HOME"} {
		if value := cli.baseEnv[name]; value != "" {
			values[name] = value
		}
	}
	delete(values, "SUBYARD_YARD")
	return values
}

type configAliasChoice struct{ name, value string }
type configAliasRepairRequest struct {
	scope             config.SettingScope
	choices           []configAliasChoice
	check, yes, local bool
	selection         bool
}

func parseConfigAliasRepair(arguments []string) (configAliasRepairRequest, error) {
	request := configAliasRepairRequest{}
	var positional []string
	for index := 0; index < len(arguments); index++ {
		switch argument := arguments[index]; argument {
		case "--scope":
			if request.scope != "" || index+1 == len(arguments) {
				return request, errors.New("one explicit --scope host, shared or yard is required")
			}
			index++
			request.scope = config.SettingScope(arguments[index])
		case "--local":
			request.local = true
		case "--check":
			request.check = true
		case "--yes", "-y":
			request.yes = true
		default:
			if strings.HasPrefix(argument, "--") {
				return request, errors.New("repair-alias accepts only explicit desired pairs, --scope, --local, --check and --yes")
			}
			positional = append(positional, argument)
		}
	}
	if !request.local || request.scope != config.ScopeHost && request.scope != config.ScopeShared && request.scope != config.ScopeYard {
		return request, errors.New("repair-alias requires --local and one explicit --scope host, shared or yard")
	}
	if len(positional) == 0 || len(positional)%2 != 0 || len(positional) > 8 {
		return request, errors.New("repair-alias requires one to four explicit canonical SETTING VALUE pairs")
	}
	seen := map[string]bool{}
	for index := 0; index < len(positional); index += 2 {
		name := positional[index]
		if !slices.Contains([]string{"YARD_IMAGE", "YARD_IMAGE_FALLBACK", "ENVIRONMENT_PROFILES", "CODING_TOOL_INTEGRATIONS"}, name) || seen[name] {
			return request, errors.New("repair-alias accepts each supported non-sensitive canonical alias family at most once")
		}
		seen[name] = true
		request.choices = append(request.choices, configAliasChoice{name, positional[index+1]})
	}
	return request, nil
}

func (cli *CLI) aliasRepairContent(path string, content []byte, request configAliasRepairRequest) ([]byte, error) {
	assignments, err := config.ParsePersistentAssignments(path, content)
	if err != nil {
		return nil, err
	}
	draft := bytes.Clone(content)
	for _, choice := range request.choices {
		definition, err := cli.catalog.ValidateSettingName(request.scope, choice.name, false)
		if err != nil || definition.Sensitive || len(definition.Aliases) == 0 {
			return nil, errors.New("requested alias family is not supported in this local scope")
		}
		if err := cli.catalog.ValidateSetting(request.scope, choice.name, choice.value, false); err != nil {
			return nil, errors.New("explicit desired value does not satisfy the selected setting schema")
		}
		legacy, canonicalExact := false, false
		for _, assignment := range assignments {
			if assignment.Name != choice.name && !slices.Contains(definition.Aliases, assignment.Name) {
				continue
			}
			if !assignment.Direct || assignment.Dynamic {
				return nil, &config.SourceError{Path: path, Line: assignment.Line, Key: choice.name, Role: "alias repair", Reason: "targeted assignment must be literal and direct"}
			}
			if assignment.Name != choice.name {
				legacy = true
			} else if assignment.Value == choice.value {
				canonicalExact = true
			}
		}
		if !legacy {
			if !canonicalExact {
				return nil, errors.New("actual repair requires a related literal legacy alias; canonical-only inputs permit exact desired repetition only")
			}
			for _, assignment := range assignments {
				if assignment.Name == choice.name && assignment.Value != choice.value {
					return nil, errors.New("canonical-only repair cannot change an existing desired value")
				}
			}
			continue
		}
		draft, err = config.EditPersistentAssignmentContent(path, draft, choice.name, &choice.value)
		if err != nil {
			return nil, err
		}
	}
	return draft, nil
}

func (cli *CLI) inspectConfigSourceAdmission(ctx context.Context, runtime *releaseruntime.SourceInspector) (releaseruntime.SourceAdmission, error) {
	options, available, err := cli.mutationGateReleaseOptions()
	if err != nil || !available {
		return releaseruntime.SourceAdmission{}, errors.New("source repair requires verified installed release metadata")
	}
	return runtime.InspectSourceAdmission(ctx, options.RuntimeRoot, options.ConfigHome)
}

func (cli *CLI) runConfigAliasRepair(ctx context.Context, yard string, arguments []string, assumeYes bool) int {
	request, err := parseConfigAliasRepair(arguments)
	if err != nil {
		cli.errorf("config repair-alias: %v", err)
		return 2
	}
	return cli.runConfigSourceRepair(ctx, yard, request, assumeYes)
}

func (cli *CLI) runConfigSourceRepair(ctx context.Context, yard string, request configAliasRepairRequest, assumeYes bool) int {
	label := "config repair-alias"
	if request.selection {
		label = "config reduce-selection"
	}
	if !domain.SafeName(yard) {
		cli.errorf("%s", label+" requires a plain local yard name")
		return 2
	}
	values := cli.configSourceEnvironment()
	runtime := releaseruntime.NewSourceInspector(cli.options.RepositoryRoot, cli.options.Stderr)
	defer runtime.Close()
	options, available, optionsErr := cli.mutationGateReleaseOptions()
	if optionsErr != nil || !available {
		cli.errorf("%s", label+" requires verified installed release metadata")
		return 1
	}
	shipped, toolingBinding, err := runtime.InspectSourceTooling(ctx, options.RuntimeRoot, cli.options.RepositoryRoot)
	if err != nil {
		cli.errorf(label+": %v", err)
		return 1
	}
	if selected := values["SUBYARD_CONFIG_DIR"]; selected != "" && filepath.Clean(selected) != shipped {
		cli.errorf("%s", label+" requires the sealed executing candidate's shipped configuration assets")
		return 2
	}
	values["SUBYARD_CONFIG_DIR"] = shipped
	configHome, err := config.ResolveConfigHome(values["SUBYARD_OPERATOR_HOME"], values)
	if err != nil {
		cli.errorf("%s", label+": local configuration root is unavailable")
		return 1
	}
	loaded := config.Loaded{Context: domain.Context{YardName: yard, Paths: domain.RuntimePaths{ConfigHome: configHome}}}
	path, err := configScalarAuthoringPath(loaded, request.scope)
	if err != nil {
		cli.errorf("%s", label+": explicit local scope is required")
		return 2
	}
	baseline, err := config.ReadPersistentFileSnapshot(configHome, path)
	if err != nil || !baseline.Exists && !request.selection {
		cli.errorf("%s", label+": selected local scalar source is unavailable or unsafe")
		return 1
	}
	var content []byte
	if request.selection {
		content, err = cli.selectionRepairContent(yard, path, baseline.Content, request)
	} else {
		content, err = cli.aliasRepairContent(path, baseline.Content, request)
	}
	if err != nil {
		cli.errorf(label+": %v", configProspectiveError{configHome, err})
		return 1
	}
	file, err := os.CreateTemp("/tmp", "subyard-alias-repair-*")
	if err != nil {
		cli.errorf("%s", label+": retained draft is unavailable")
		return 1
	}
	draft := file.Name()
	defer os.Remove(draft)
	err = file.Chmod(0o600)
	if err == nil {
		_, err = file.Write(content)
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		loaded, err = config.Load(config.LoadOptions{RepositoryRoot: cli.options.RepositoryRoot, OperatorHome: values["SUBYARD_OPERATOR_HOME"], YardName: yard, Environment: values, FileOverrides: map[string]string{path: draft}})
	}
	if err != nil {
		cli.errorf(label+": %v", configProspectiveError{configHome, err})
		return 1
	}
	if loaded.Context.AccessKind != domain.AccessLocal {
		cli.errorf("%s", label+" must run on the local source owner")
		return 2
	}
	owner, err := captureOwnerWriteAdmission(loaded, cli.options.RepositoryRoot)
	if err != nil {
		cli.errorf("%s: owner admission unavailable", label)
		return 1
	}
	action := ""
	if request.selection {
		action = "selection-repair"
	}
	prospective, err := cli.prepareConfigProspective(loaded, configAuthoringRequest{action: action, scope: request.scope, name: request.choices[0].name}, path, config.PersistentFileSnapshot{Exists: true, Content: content})
	if err != nil {
		cli.errorf(label+": %v", err)
		return 1
	}
	defer prospective.close()
	before, err := cli.inspectConfigSourceAdmission(ctx, runtime)
	if err != nil {
		cli.errorf(label+": %v", err)
		return 1
	}
	if request.selection && (before.Metadata.Journal == nil || before.Metadata.Journal.Checkpoint != releasetransition.JournalReconciling) {
		cli.errorf("selection source repair requires an interrupted activation-only transaction")
		return 1
	}
	var keys []string
	for _, choice := range request.choices {
		keys = append(keys, choice.name)
	}
	consequence := fmt.Sprintf("Canonicalize explicitly selected %s settings in %s scope; preserve release history.", strings.Join(keys, ", "), request.scope)
	if request.selection {
		consequence = fmt.Sprintf("Store reduced %s selection in local %s settings; preserve interrupted release history. Review a fresh activation recovery plan with yard migrate.", strings.Join(keys, ", "), request.scope)
	}
	fmt.Fprintln(cli.options.Stdout, consequence)
	if request.check {
		fmt.Fprintln(cli.options.Stdout, "Prospective scoped configuration is valid; source repair has not been applied.")
		return 0
	}
	changed := !bytes.Equal(baseline.Content, content)
	cli.ensureOperationID()
	operation := "repair-alias"
	if request.selection {
		operation = "reduce-selection"
	}
	if !cli.planConfigAction(ctx, loaded, operation, assumeYes || request.yes || cli.env["ASSUME_YES"] == "1", !changed, consequence) {
		return 1
	}
	releaseOwner, err := cli.acquireOwnerMutation(ctx, owner, false, true)
	if err != nil {
		cli.errorf("%s: %v", label, err)
		return 1
	}
	defer releaseOwner()
	store, _ := releasetransition.NewPOSIXV2Store(configHome)
	unlock, err := store.Lock()
	if err != nil {
		cli.errorf("%s", label+": protected release lock is unavailable")
		return 1
	}
	defer unlock()
	guard := func() error {
		configDir, currentTooling, err := runtime.InspectSourceTooling(ctx, options.RuntimeRoot, cli.options.RepositoryRoot)
		if err != nil || configDir != shipped || currentTooling != toolingBinding {
			return domain.ErrPlanStale
		}
		after, err := cli.inspectConfigSourceAdmission(ctx, runtime)
		if err != nil || before.Binding != after.Binding {
			return domain.ErrPlanStale
		}
		return prospective.checkLocked(true)
	}
	if changed {
		err = config.WritePersistentFileIfUnchanged(configHome, path, baseline, content, guard)
	} else {
		var release func()
		release, err = config.LockRoot(configHome, false)
		if err == nil {
			current, readErr := config.ReadPersistentFileSnapshot(configHome, path)
			err = readErr
			if err == nil && (!sameConfigAuthoringSnapshot(current, baseline) || current.Identity != baseline.Identity) {
				err = domain.ErrPlanStale
			}
			if err == nil {
				err = guard()
			}
			release()
		}
	}
	if err == nil {
		err = prospective.verify()
	}
	if err != nil {
		if errors.Is(err, config.ErrPersistentTargetStale) {
			err = domain.ErrPlanStale
		}
		cli.errorf(label+": %v", err)
		return 1
	}
	if request.selection {
		fmt.Fprintln(cli.options.Stdout, "Reduced persistent selection stored. The interrupted activation remains pending; inspect a fresh sealed-owner migrate plan and consent before recovery.")
	} else {
		fmt.Fprintln(cli.options.Stdout, "Configuration source is canonical and schema-valid. Assess runtime readiness separately with the sealed owner.")
	}
	return 0
}

func (cli *CLI) runConfigValidate(yard string, arguments []string) int {
	all, structured := false, false
	for _, argument := range arguments {
		switch argument {
		case "--all-local":
			all = true
		case "--json":
			structured = true
		default:
			cli.errorf("config validate accepts --all-local and --json")
			return 2
		}
	}
	if !domain.SafeName(yard) {
		cli.errorf("config validate requires a plain local yard name")
		return 2
	}
	values := cli.configSourceEnvironment()
	home, err := config.ResolveConfigHome(values["SUBYARD_OPERATOR_HOME"], values)
	if err != nil {
		cli.errorf("config validate: local configuration root is unavailable")
		return 1
	}
	configDir := values["SUBYARD_CONFIG_DIR"]
	if configDir == "" {
		configDir = filepath.Join(cli.options.RepositoryRoot, "config")
	}
	names := []string{yard}
	if all {
		names, err = config.YardNames(configDir, home)
	}
	type row struct {
		Yard       string `json:"yard"`
		Valid      bool   `json:"valid"`
		Diagnostic string `json:"diagnostic,omitempty"`
		Retry      string `json:"retry,omitempty"`
	}
	var rows []row
	failed := err != nil
	if err != nil {
		rows = append(rows, row{Yard: yard, Diagnostic: "local registration inventory is invalid or unavailable"})
	}
	for _, name := range names {
		loaded, err := config.Load(config.LoadOptions{RepositoryRoot: cli.options.RepositoryRoot, OperatorHome: values["SUBYARD_OPERATOR_HOME"], YardName: name, Environment: values})
		if err == nil && all && loaded.Context.AccessKind != domain.AccessLocal {
			continue
		}
		result := row{Yard: name, Valid: err == nil}
		if err != nil {
			failed = true
			result.Diagnostic = (configProspectiveError{home, err}).Error()
			result.Retry = config.SourceAliasRepairRetry(home, name, err)
			if result.Retry == "" {
				result.Retry = "restore the identified source or use its supported schema migration, then run config validate"
			}
		}
		rows = append(rows, result)
	}
	if structured {
		if err := json.NewEncoder(cli.options.Stdout).Encode(struct {
			SchemaVersion  int   `json:"schemaVersion"`
			Configurations []row `json:"configurations"`
		}{1, rows}); err != nil {
			return 1
		}
	} else {
		for _, result := range rows {
			if result.Valid {
				fmt.Fprintf(cli.options.Stdout, "yard %s: configuration schema valid\n", result.Yard)
			} else {
				fmt.Fprintf(cli.options.Stdout, "yard %s: %s\nRetry: %s\n", result.Yard, result.Diagnostic, result.Retry)
			}
		}
	}
	if failed {
		return 1
	}
	return 0
}

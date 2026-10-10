package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
)

// ProfileServiceRuntime applies only the named profile's declared owner hook.
type ProfileServiceRuntime interface {
	ProfileServiceConverged(context.Context, string) (bool, error)
	ApplyProfileService(context.Context, string, []ports.TeardownArtifact) error
}

type profileRequest struct {
	verb, id string
	json     bool
}

func parseProfileArguments(arguments []string) (profileRequest, error) {
	var request profileRequest
	var positional []string
	for _, argument := range arguments {
		switch argument {
		case "--yes", "-y":
		case "--json":
			request.json = true
		default:
			positional = append(positional, argument)
		}
	}
	if len(positional) != 2 || !slices.Contains([]string{"enable", "disable", "setup", "status"}, positional[0]) || !domain.SafeName(positional[1]) || request.json && positional[0] != "status" {
		return request, errors.New("usage: profile enable|disable|setup|status <id> [--yes] [--json]")
	}
	request.verb, request.id = positional[0], positional[1]
	return request, nil
}

func profileReadOnlyInvocation(arguments []string) bool {
	request, err := parseProfileArguments(arguments)
	return err == nil && request.verb == "status"
}

func (cli *CLI) profileRuntime(loaded config.Loaded) ProfileServiceRuntime {
	if cli.options.ProfileRuntime != nil {
		return cli.options.ProfileRuntime(loaded)
	}
	return cli.initPlatform(loaded, nil).(ProfileServiceRuntime)
}

func (cli *CLI) requireRunningProfileYard(ctx context.Context, loaded config.Loaded) error {
	_, err := cli.observeProfileYard(ctx, loaded)
	return err
}

func (cli *CLI) observeProfileYard(ctx context.Context, loaded config.Loaded) (ports.InstanceInfo, error) {
	incus, _ := cli.statusPorts()
	instance, err := incus.Instance(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName)
	if err != nil {
		return instance, fmt.Errorf("profile changes require an existing running yard: %w", err)
	}
	if !strings.EqualFold(instance.Status, "running") {
		return instance, fmt.Errorf("yard %q is %s; profile changes require a running yard (start it explicitly)", loaded.Context.YardName, instance.Status)
	}
	metadata := instance.LocalConfig
	if metadata == nil {
		metadata = instance.Config
	}
	if metadata["user.subyard.managed"] != "true" || metadata["user.subyard.initialized"] != "true" || metadata["user.subyard.name"] != loaded.Context.YardName {
		return instance, errors.New("profile changes require an initialized managed instance belonging to the selected yard")
	}
	return instance, nil
}

func profileYardBinding(instance ports.InstanceInfo) string {
	return operationStateDigest(struct {
		Name, Project string
		Type          domain.YardKind
		UUID          string
	}{instance.Name, instance.Project, instance.Type, instance.Config["volatile.uuid"]})
}

// Preserve implicit provision defaults when authoring the first explicit list.
func (cli *CLI) currentProfileSelection(loaded config.Loaded, definitions []profile.Definition) ([]string, error) {
	if value, explicit := loaded.Environment["ENVIRONMENT_PROFILES"]; explicit {
		return strings.Fields(value), nil
	}
	provision, err := cli.prepareProvisionExecution(loaded, nil, nil)
	if err != nil {
		return nil, err
	}
	result := slices.Clone(provision.profiles)
	for _, definition := range definitions {
		if definition.Selected(loaded.Context.YardName, loaded.Environment) && !slices.Contains(result, definition.Name) {
			result = append(result, definition.Name)
		}
	}
	return result, nil
}

// Validate an existing connection without printing its contents. The binding
// detects rotation while an operation waits for confirmation.
func (cli *CLI) profileConnection(loaded config.Loaded, definition profile.Definition) (string, error) {
	schema := definition.Setup
	if schema == nil {
		return "not-required", nil
	}
	runtime, err := cli.credentialRuntime(loaded)
	if err != nil {
		return "", err
	}
	key, err := runtime.ConsumerPath(schema.Consumer, schema.Zone)
	if err != nil {
		return "", err
	}
	path := filepath.Join(loaded.Context.Paths.ConfigHome, schema.ConfigFile)
	before, err := readInitSelectionSnapshot(loaded.Context.Paths.ConfigHome, path)
	if err != nil {
		return "", err
	}
	var values map[string]any
	if before.Exists {
		if before.Identity.Mode&0o777 != 0o600 && before.Identity.Mode&0o777 != 0o400 {
			return "", errors.New("profile settings are unprotected")
		}
		values, err = schema.Decode(before.Content)
		if err != nil {
			return "", errors.New("profile settings are invalid")
		}
		if override, _ := values[schema.KeyOverrideField].(string); override != "" {
			key = override
		}
	} else if !schema.SyncFields {
		return "", errors.New("profile settings are missing")
	}
	if err := runtime.ValidateConsumerFile(schema.Consumer, schema.Zone, key); err != nil {
		return "", err
	}
	if schema.SyncFields {
		fields, err := runtime.ConsumerSettings(schema.Consumer, schema.Zone, key)
		if err != nil || fields == nil && (values == nil || values["use_credential_settings"] == true) {
			return "", errors.New("complete synchronized profile connection is missing")
		}
		if fields != nil && values != nil && values["use_credential_settings"] != true {
			want, err := schema.SharedSettings(values)
			if err != nil || !reflect.DeepEqual(fields, want) {
				return "", errors.New("local profile settings conflict with the synchronized connection")
			}
		}
	}
	binding, err := runtime.ConsumerFileBinding(key)
	return operationStateDigest(struct {
		Settings config.PersistentFileSnapshot
		Key      string
	}{before, binding}), err
}

type profileStatus struct {
	Yard       string `json:"yard"`
	ID         string `json:"id"`
	Enabled    bool   `json:"enabled"`
	Configured bool   `json:"configured"`
	Runtime    string `json:"runtime"`
	Ready      bool   `json:"ready"`
}

func (prepared *preparedCommand) prepareProfile(ctx context.Context, _ *initBootstrap) error {
	cli, loaded := prepared.CLI, prepared.Loaded
	request, err := parseProfileArguments(prepared.Arguments)
	if err != nil {
		return err
	}
	if loaded.Context.AccessKind != domain.AccessLocal {
		return errors.New("profile commands require the owner host; setup uses a protected owner-local credential file")
	}
	definitions, err := profile.Load(cli.options.RepositoryRoot)
	if err != nil {
		return err
	}
	index := slices.IndexFunc(definitions, func(d profile.Definition) bool { return d.Name == request.id })
	if index < 0 || definitions[index].OwnerService == "" {
		return fmt.Errorf("profile %q does not declare a scoped owner service", request.id)
	}
	definition := definitions[index]
	connection, connectionErr := cli.profileConnection(loaded, definition)
	if request.verb == "status" {
		status := profileStatus{Yard: loaded.Context.YardName, ID: request.id, Enabled: definition.Selected(loaded.Context.YardName, loaded.Environment), Configured: connectionErr == nil, Runtime: "unavailable"}
		if cli.requireRunningProfileYard(ctx, loaded) == nil {
			converged, err := cli.profileRuntime(loaded).ProfileServiceConverged(ctx, request.id)
			if err != nil {
				return err
			}
			status.Runtime = "pending"
			if converged {
				status.Runtime = "removed"
				if status.Enabled {
					status.Runtime = "ready"
				}
			}
			status.Ready = status.Enabled && status.Configured && converged
		}
		prepared.displayOnly = func() {
			if request.json {
				_ = json.NewEncoder(cli.options.Stdout).Encode(status)
				return
			}
			fmt.Fprintf(cli.options.Stdout, "yard: %s\nprofile: %s\nenabled: %t\nconfigured: %t\nruntime: %s\nready: %t\n", status.Yard, status.ID, status.Enabled, status.Configured, status.Runtime, status.Ready)
		}
		return nil
	}
	for _, resolution := range loaded.Settings["ENVIRONMENT_PROFILES"].Resolutions {
		if resolution.Status == "effective" && resolution.Scope == "command" {
			return errors.New("remove the temporary ENVIRONMENT_PROFILES override before managing a profile")
		}
	}
	yardBinding := ""
	if request.verb != "setup" {
		instance, err := cli.observeProfileYard(ctx, loaded)
		if err != nil {
			return err
		}
		yardBinding = profileYardBinding(instance)
	}
	if err := config.CheckLocalSettingsWritable(loaded.Context.Paths.ConfigHome); err != nil {
		return err
	}
	inputs, err := cli.captureOwnerInputs(loaded, "", nil)
	if err != nil {
		return err
	}
	var artifacts []ports.TeardownArtifact
	if request.verb == "disable" {
		artifacts = []ports.TeardownArtifact{}
		for _, managed := range definition.ManagedPaths {
			root := loaded.Context.Paths.DataHome
			if managed.Root == "operator" {
				root = loaded.Context.Paths.OperatorHome
			}
			path := filepath.Join(root, strings.ReplaceAll(managed.Path, "{yard}", loaded.Context.YardName))
			binding, err := teardownArtifactBinding(path)
			if err != nil {
				return err
			}
			artifacts = append(artifacts, ports.TeardownArtifact{Path: path, Binding: binding})
		}
	}
	selection, err := cli.currentProfileSelection(loaded, definitions)
	if err != nil {
		return err
	}
	requested := slices.Clone(selection)
	if request.verb == "enable" && !slices.Contains(requested, request.id) {
		requested = append(requested, request.id)
	}
	if request.verb == "disable" {
		requested = slices.DeleteFunc(requested, func(id string) bool { return id == request.id })
	}
	selectionChanged := request.verb != "setup" && !slices.Equal(selection, requested)
	candidate := loaded
	if selectionChanged {
		candidate, err = config.WithEnvironmentProfiles(loaded, requested)
		if err != nil {
			return err
		}
	}
	if request.verb == "enable" && !definition.Selected(candidate.Context.YardName, candidate.Environment) {
		return errors.New("profile is disabled by the yard's capability constraints")
	}
	path, err := configScalarAuthoringPath(loaded, config.ScopeYard)
	if err != nil {
		return err
	}
	before, err := readConfigAuthoringTarget(path)
	if err != nil {
		return err
	}
	value := strings.Join(requested, " ")
	var setups *initProfileSet
	if request.verb != "disable" && definition.Setup != nil && prepared.interactiveSetup {
		item, err := cli.prepareInitProfile(ctx, &initExecution{loaded: candidate}, prepared.Arguments, definition)
		if err != nil {
			return err
		}
		if item != nil {
			setups = &initProfileSet{root: cli.options.RepositoryRoot, definitions: definitions, items: []*initProfileSetup{item}}
		}
	}
	if request.verb != "disable" && connectionErr != nil && setups == nil {
		return fmt.Errorf("profile connection is unavailable; run yard -Y %s profile setup %s interactively on the owner host", loaded.Context.YardName, request.id)
	}
	stage := application.ReconcileStage{ID: ports.ReconcileStageKeys, Label: "Initialize the encrypted credential ledger and sync timer"}
	reconciler := application.Reconciler{Stages: []application.ReconcileStage{stage}, Runner: cli.initPlatform(candidate, nil), Reporter: initReporter{output: cli.options.Stderr}}
	var keysPlan application.ReconcilePlan
	if setups != nil && setups.items[0].key != nil {
		keysPlan, err = reconciler.Plan(ctx)
		if err != nil {
			return err
		}
	}
	var runtime ProfileServiceRuntime
	converged := true
	if request.verb != "setup" {
		runtime = cli.profileRuntime(candidate)
		converged, err = runtime.ProfileServiceConverged(ctx, request.id)
		if err != nil {
			return err
		}
	}
	consequences := setups.consequences()
	if keysPlan.Pending() != 0 {
		consequences = append(consequences, stage.Label)
	}
	if selectionChanged {
		consequences = append(consequences, "save profiles for yard "+loaded.Context.YardName+": "+value)
	}
	if !converged {
		consequences = append(consequences, request.verb+" profile "+request.id+" in yard "+loaded.Context.YardName)
	}
	changed := selectionChanged || !converged || setups != nil
	action := domain.ActionID("profile.reconcile")
	if request.verb == "setup" {
		action = "profile.setup"
	}
	prepared.exactState = operationStateDigest(struct {
		Inputs, Connection, Setup, Yard string
		Selection                       config.PersistentFileSnapshot
		Keys                            application.ReconcilePlan
		Artifacts                       []ports.TeardownArtifact
	}{inputs.binding(), connection, (&initExecution{profileSetup: setups}).stateBinding(), yardBinding, before, keysPlan, artifacts})
	prepared.stepsComplete, prepared.executeNoOp = true, true
	prepared.steps = func() []domain.OperationStep {
		var steps []domain.OperationStep
		add := func(id, target, desired, verify string, apply bool) {
			decision, observed := domain.StepSkip, desired
			if apply {
				decision, observed = domain.StepApply, "pending"
			}
			step := domain.OperationStep{ID: id, Target: target, Observed: observed, Desired: desired, Decision: decision, Preconditions: []string{"captured owner inputs and profile declaration are unchanged"}, Verify: verify, Consequence: desired}
			if len(steps) != 0 {
				step.DependsOn = []string{steps[len(steps)-1].ID}
			}
			steps = append(steps, step)
		}
		if len(keysPlan.Steps) != 0 {
			add("profile.keys", loaded.Context.Paths.ConfigHome, stage.Label, "verify credential owner initialization", keysPlan.Pending() != 0)
		}
		if setups != nil {
			add("profile.setup", loaded.Context.Paths.ConfigHome, "protected connection for "+request.id, "validate protected consumer connection", true)
		}
		if request.verb != "setup" {
			add("profile.selection", path, "profiles ["+value+"]", "read effective yard profile selection", selectionChanged)
			add("profile.runtime", loaded.Context.IncusProject+"/"+loaded.Context.YardInstanceName+":"+request.id, "profile "+request.verb+" converged", "selected profile owner hook --check", !converged)
		} else if setups == nil {
			add("profile.setup", loaded.Context.Paths.ConfigHome, "protected connection for "+request.id, "validate protected consumer connection", false)
		}
		return steps
	}
	prepared.assess = func(context.Context) (domain.ActionID, domain.ActionDelta, error) {
		return action, domain.ActionDelta{Changed: changed, Consequences: consequences}, nil
	}
	check := func(ctx context.Context) error {
		if err := inputs.check(ctx, cli); err != nil {
			return err
		}
		if err := setups.check(); err != nil {
			return err
		}
		for _, artifact := range artifacts {
			binding, err := teardownArtifactBinding(artifact.Path)
			if err != nil {
				return err
			}
			if binding != artifact.Binding {
				return fmt.Errorf("%w: profile artifact changed", domain.ErrPlanStale)
			}
		}
		if request.verb != "setup" {
			instance, err := cli.observeProfileYard(ctx, loaded)
			if err != nil {
				return err
			}
			if profileYardBinding(instance) != yardBinding {
				return fmt.Errorf("%w: yard instance identity changed", domain.ErrPlanStale)
			}
		}
		if connectionErr == nil {
			fresh, err := cli.profileConnection(loaded, definition)
			if err != nil || fresh != connection {
				return fmt.Errorf("%w: profile connection changed", domain.ErrPlanStale)
			}
		}
		if len(keysPlan.Steps) != 0 {
			if err := reconciler.CheckApproved(ctx, keysPlan); err != nil {
				return err
			}
		}
		if runtime != nil && converged {
			ready, err := runtime.ProfileServiceConverged(ctx, request.id)
			if err != nil {
				return err
			}
			if !ready {
				return fmt.Errorf("%w: profile runtime now requires work", domain.ErrPlanStale)
			}
		}
		return nil
	}
	prepared.execute = func(ctx context.Context, _ *application.Orchestrator, output io.Writer) (domain.AdapterResult, error) {
		result := domain.AdapterResult{Schema: 1, OperationID: prepared.Plan.OperationID, Status: "ok"}
		unlock, err := lockIntegrationYard(ctx, loaded)
		if err != nil {
			return result, err
		}
		defer unlock()
		if err := check(ctx); err != nil {
			return result, err
		}
		if err := setups.applyWith(ctx, output, func(ctx context.Context) error { return reconciler.Apply(ctx, keysPlan) }); err != nil {
			return result, err
		}
		if request.verb != "disable" {
			if _, err := cli.profileConnection(candidate, definition); err != nil {
				return result, fmt.Errorf("verify profile connection: %w", err)
			}
		}
		if selectionChanged {
			if err := config.WritePersistentAssignmentIfUnchanged(loaded.Context.Paths.ConfigHome, path, "ENVIRONMENT_PROFILES", &value, before); err != nil {
				return result, err
			}
		}
		if runtime != nil && !converged {
			if err := runtime.ApplyProfileService(ctx, request.id, artifacts); err != nil {
				return result, fmt.Errorf("profile selection saved; runtime remains pending, retry profile %s %s: %w", request.verb, request.id, err)
			}
		}
		if runtime != nil {
			ready, err := runtime.ProfileServiceConverged(ctx, request.id)
			if err != nil {
				return result, err
			}
			if !ready {
				return result, errors.New("profile runtime did not converge; desired selection retained for retry")
			}
		}
		fresh, err := config.Load(config.LoadOptions{Catalog: &cli.catalog, RepositoryRoot: cli.options.RepositoryRoot,
			OperatorHome: loaded.Context.Paths.OperatorHome, YardName: loaded.Context.YardName, Environment: cli.baseEnv})
		if err != nil {
			return result, err
		}
		if integrationConfigurationFingerprint(fresh) != integrationConfigurationFingerprint(candidate) {
			return result, fmt.Errorf("%w: profile configuration changed during apply; inspect status and retry", domain.ErrPlanStale)
		}
		return result, nil
	}
	return nil
}

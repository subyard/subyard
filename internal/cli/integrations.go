package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
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

// IntegrationRuntime is shared by prepared mutations and the read-only query.
type IntegrationRuntime interface {
	IntegrationPlan(context.Context) (reconcileruntime.IntegrationPlan, error)
	ApplyIntegrations(context.Context, reconcileruntime.IntegrationPlan) error
}

type IntegrationCleanupRuntime interface {
	IntegrationCleanupPlan(context.Context, string) (reconcileruntime.IntegrationCleanupPlan, error)
	ApplyIntegrationCleanup(context.Context, string, reconcileruntime.IntegrationCleanupPlan) error
}

type integrationRequest struct {
	verb, id string
	json     bool
	check    bool
}

func parseIntegrationArguments(arguments []string) (integrationRequest, error) {
	var request integrationRequest
	var positional []string
	for _, argument := range arguments {
		switch argument {
		case "--yes", "-y":
		case "--json":
			request.json = true
		case "--check":
			request.check = true
		default:
			positional = append(positional, argument)
		}
	}
	if len(positional) == 0 {
		return request, errors.New("usage: integration enable|disable <id> | cleanup <id> [--check] | status [id] [--json]")
	}
	request.verb = positional[0]
	if len(positional) > 1 {
		request.id = positional[1]
	}
	if len(positional) > 2 || (request.verb != "status" && request.verb != "enable" && request.verb != "disable" && request.verb != "cleanup") || (request.verb != "status" && (request.id == "" || request.json)) || (request.id != "" && !domain.SafeName(request.id)) || (request.check && request.verb != "cleanup") {
		return request, errors.New("usage: integration enable|disable <id> | cleanup <id> [--check] | status [id] [--json]")
	}
	return request, nil
}

func integrationReadOnlyInvocation(arguments []string) bool {
	request, err := parseIntegrationArguments(arguments)
	return err == nil && (request.verb == "status" || request.check)
}

func (cli *CLI) integrationRuntime(loaded config.Loaded) IntegrationRuntime {
	if cli.options.IntegrationRuntime != nil {
		return cli.options.IntegrationRuntime(loaded)
	}
	runtime := cli.initPlatform(loaded, nil)
	return runtime.(IntegrationRuntime)
}

func (cli *CLI) requireRunningIntegrationYard(ctx context.Context, loaded config.Loaded) error {
	incus, _ := cli.statusPorts()
	instance, err := incus.Instance(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName)
	if err != nil {
		return fmt.Errorf("integration requires an existing running yard: %w", err)
	}
	if !strings.EqualFold(instance.Status, "running") {
		return fmt.Errorf("yard %q is %s; integration changes require a running yard (start it explicitly)", loaded.Context.YardName, instance.Status)
	}
	return nil
}

func (cli *CLI) integrationSelectionContext(loaded config.Loaded, requested []string) (config.Loaded, error) {
	environment := maps.Clone(cli.baseEnv)
	delete(environment, "SUBYARD_ENGINE_CONTEXT")
	delete(environment, "SUBYARD_CONFIG_LOADED")
	delete(environment, "AGENTS")
	environment["CODING_TOOL_INTEGRATIONS"] = strings.Join(requested, " ")
	environment["SUBYARD_CONFIG_HOME"] = loaded.Context.Paths.ConfigHome
	environment["SUBYARD_HOME"] = loaded.Context.Paths.DataHome
	return config.Load(config.LoadOptions{RepositoryRoot: cli.options.RepositoryRoot, OperatorHome: loaded.Context.Paths.OperatorHome, YardName: loaded.Context.YardName, Environment: environment})
}

func (cli *CLI) persistentIntegrationContext(loaded config.Loaded) (config.Loaded, error) {
	environment := maps.Clone(cli.baseEnv)
	delete(environment, "AGENTS")
	delete(environment, "CODING_TOOL_INTEGRATIONS")
	// The command environment must not be mistaken for an authored yard request.
	delete(environment, "SUBYARD_CONFIG_LOADED")
	delete(environment, "SUBYARD_ENGINE_CONTEXT")
	environment["SUBYARD_CONFIG_HOME"] = loaded.Context.Paths.ConfigHome
	environment["SUBYARD_HOME"] = loaded.Context.Paths.DataHome
	persistent, err := config.Load(config.LoadOptions{RepositoryRoot: cli.options.RepositoryRoot, OperatorHome: loaded.Context.Paths.OperatorHome, YardName: loaded.Context.YardName, Environment: environment})
	if err != nil {
		return persistent, err
	}
	if !slices.Equal(loaded.Integrations.Requested, persistent.Integrations.Requested) || loaded.Integrations.AllowsCodingTools != persistent.Integrations.AllowsCodingTools {
		return persistent, errors.New("temporary integration selection differs from persistent settings; remove the command override before changing integrations")
	}
	return persistent, nil
}

func integrationSourceGuard(loaded config.Loaded) error {
	_, registered, err := configsync.ReadSourceRecord(loaded.Context.Paths.ConfigHome)
	if err != nil {
		return err
	}
	if registered {
		return errors.New("configuration is source-managed; edit the yard CODING_TOOL_INTEGRATIONS in the registered source, run config sync, then init to reconcile")
	}
	return nil
}

func (prepared *preparedCommand) prepareIntegration(ctx context.Context, _ *initBootstrap) error {
	cli := prepared.CLI
	request, err := parseIntegrationArguments(prepared.Arguments)
	if err != nil {
		return err
	}
	if prepared.Loaded.Context.AccessKind == domain.AccessRemote {
		return errors.New("remote integration requires an owner-built operation plan")
	}
	if request.verb == "status" {
		status, err := cli.queryIntegrationStatus(ctx, prepared.Loaded, request.id)
		if err != nil {
			return err
		}
		prepared.displayOnly = func() { cli.printIntegrationStatus(status, request.json) }
		return nil
	}
	// This precondition deliberately also precedes no-op selection and source checks.
	if err = cli.requireRunningIntegrationYard(ctx, prepared.Loaded); err != nil {
		return err
	}
	loaded, err := cli.persistentIntegrationContext(prepared.Loaded)
	if err != nil {
		return err
	}
	if request.verb == "cleanup" {
		return prepared.prepareIntegrationCleanup(ctx, loaded, request)
	}
	if err = integrationSourceGuard(loaded); err != nil {
		return err
	}
	if _, err = config.ResolveIntegrationSelection(loaded.Environment, []string{request.id}); err != nil {
		return err
	}
	if !loaded.Integrations.Present {
		return errors.New("existing yard has no trustworthy requested integration set; explicitly set CODING_TOOL_INTEGRATIONS with config set --scope yard first")
	}
	requested := slices.Clone(loaded.Integrations.Requested)
	if !loaded.Integrations.AllowsCodingTools {
		requested = []string{}
	}
	if request.verb == "enable" {
		if !loaded.Integrations.AllowsCodingTools {
			return errors.New("yard role forbids coding tool integrations")
		}
		if !slices.Contains(requested, request.id) {
			requested = append(requested, request.id)
		}
	} else {
		requested = slices.DeleteFunc(requested, func(id string) bool { return id == request.id })
		selection, err := config.ResolveIntegrationSelection(loaded.Environment, requested)
		if err != nil {
			return err
		}
		if dependents := selection.DependencyReasons[request.id]; len(dependents) > 0 {
			return fmt.Errorf("cannot disable %s: required by %s; disable dependents explicitly first", request.id, strings.Join(dependents, ", "))
		}
	}
	candidate, err := cli.integrationSelectionContext(loaded, requested)
	if err != nil {
		return err
	}
	write, err := config.PlanYardIntegrationWrite(loaded, requested)
	if err != nil {
		return err
	}
	value := strings.Join(requested, " ")
	desiredChanged := write.Changed()
	runtime := cli.integrationRuntime(candidate)
	plan, err := runtime.IntegrationPlan(ctx)
	if err != nil {
		return err
	}
	prepared.exactState = operationStateDigest(struct {
		Runtime string
		Write   *config.YardIntegrationWrite
	}{plan.Fingerprint, write})
	consequences := slices.Clone(plan.Steps)
	if write.SourcePath != "" || write.FlatBefore.Exists {
		consequences = append(consequences, "Preserve and migrate the complete legacy yard registration to canonical config")
	}
	if desiredChanged {
		consequences = append([]string{"Store requested integrations for yard " + loaded.Context.YardName + ": " + value}, consequences...)
	}
	if len(consequences) == 0 && plan.Changed {
		consequences = []string{"Reconcile integrations in yard " + loaded.Context.YardName}
	}
	prepared.assess = func(context.Context) (domain.ActionID, domain.ActionDelta, error) {
		return "integration.reconcile", domain.ActionDelta{Changed: desiredChanged || plan.Changed, Consequences: consequences}, nil
	}
	prepared.executeNoOp = true // Running/stale checks also apply to already-converged commands.
	prepared.preview = func() {
		for _, step := range consequences {
			fmt.Fprintln(cli.options.Stdout, "  "+step)
		}
	}
	prepared.execute = func(ctx context.Context, _ *application.Orchestrator, _ io.Writer) (domain.AdapterResult, error) {
		result := domain.AdapterResult{Schema: 1, OperationID: prepared.Plan.OperationID, Status: "ok"}
		unlock, err := lockIntegrationYard(ctx, loaded)
		if err != nil {
			return result, err
		}
		defer unlock()
		if err = cli.requireRunningIntegrationYard(ctx, loaded); err != nil {
			return result, err
		}
		if err = integrationSourceGuard(loaded); err != nil {
			return result, err
		}
		current, err := cli.persistentIntegrationContext(loaded)
		if err != nil {
			return result, err
		}
		if !reflect.DeepEqual(current.Environment, loaded.Environment) {
			return result, fmt.Errorf("%w: integration settings changed", domain.ErrPlanStale)
		}
		observed, err := runtime.IntegrationPlan(ctx)
		if err != nil {
			return result, err
		}
		if observed.Fingerprint != plan.Fingerprint {
			return result, fmt.Errorf("%w: integration runtime changed", domain.ErrPlanStale)
		}
		if err = write.Apply(); err != nil {
			return result, fmt.Errorf("%w: %v", domain.ErrPlanStale, err)
		}
		if err = runtime.ApplyIntegrations(ctx, plan); err != nil {
			return result, fmt.Errorf("desired integrations saved; runtime remains pending: %w", err)
		}
		final, err := cli.persistentIntegrationContext(candidate)
		if err != nil || integrationConfigurationFingerprint(final) != integrationConfigurationFingerprint(candidate) {
			return result, fmt.Errorf("%w: configuration changed during integration reconciliation; inspect status and retry", domain.ErrPlanStale)
		}
		return result, nil
	}
	return nil
}

// Sync and host-wide authoring can change inputs outside the per-yard lock.
// Compare public settings after apply before claiming that the saved intent is ready.
func integrationConfigurationFingerprint(loaded config.Loaded) string {
	values := map[string]string{}
	for name, value := range loaded.Environment {
		if _, known := config.LookupSetting(name); known || name == "INTEGRATION_HOST_LINKS" {
			values[name] = value
		}
	}
	return operationStateDigest(struct {
		Values    map[string]string
		Requested []string
		Allowed   bool
	}{values, loaded.Integrations.Requested, loaded.Integrations.AllowsCodingTools})
}

type integrationStatus struct {
	Yard          string                      `json:"yard"`
	ID            string                      `json:"id,omitempty"`
	Selection     config.IntegrationSelection `json:"selection"`
	Observed      string                      `json:"observed"`
	ObservedScope string                      `json:"observed_scope"`
	Detail        string                      `json:"detail,omitempty"`
}

func (cli *CLI) queryIntegrationStatus(ctx context.Context, loaded config.Loaded, id string) (integrationStatus, error) {
	if loaded.Context.AccessKind == domain.AccessRemote {
		session, err := cli.openOwnerRPC(ctx, loaded.Context)
		if err != nil {
			return integrationStatus{}, err
		}
		defer session.close()
		if err := session.negotiate(ctx); err != nil {
			return integrationStatus{}, err
		}
		return session.integrationStatus(ctx, loaded.Context, cli.ensureOperationID(), id)
	}
	status := integrationStatus{Yard: loaded.Context.YardName, ID: id, Selection: loaded.Integrations, Observed: "unknown", ObservedScope: "yard"}
	if id != "" {
		if _, err := config.ResolveIntegrationSelection(loaded.Environment, []string{id}); err != nil {
			return status, err
		}
	}
	incus, _ := cli.statusPorts()
	instance, err := incus.Instance(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName)
	if errors.Is(err, ports.ErrInstanceNotFound) {
		status.Observed = "missing"
		return status, nil
	}
	if err != nil {
		status.Detail = err.Error()
		return status, nil
	}
	if !strings.EqualFold(instance.Status, "running") {
		status.Observed = "stopped"
		return status, nil
	}
	plan, err := cli.integrationRuntime(loaded).IntegrationPlan(ctx)
	if err != nil {
		status.Observed = "conflict"
		status.Detail = err.Error()
		return status, nil
	}
	status.Observed = "ready"
	if plan.Changed {
		status.Observed = "pending"
		status.Detail = strings.Join(plan.Steps, "; ")
	}
	return status, nil
}
func (cli *CLI) printIntegrationStatus(status integrationStatus, jsonOutput bool) {
	if jsonOutput {
		_ = json.NewEncoder(cli.options.Stdout).Encode(status)
		return
	}
	requested := strings.Join(status.Selection.Requested, " ")
	if !status.Selection.Present {
		requested = "<unset>"
	} else if requested == "" {
		requested = "<empty>"
	}
	fmt.Fprintf(cli.options.Stdout, "Yard: %s\nRequested: %s\nEffective: %s\nSource: %s (%s)\nObserved (yard): %s\n", status.Yard, requested, strings.Join(status.Selection.Effective, " "), status.Selection.Provenance.Path, status.Selection.Provenance.Scope, status.Observed)
	if status.ID != "" {
		fmt.Fprintf(cli.options.Stdout, "%s: requested=%t effective=%t required-by=%s\n", status.ID, slices.Contains(status.Selection.Requested, status.ID), slices.Contains(status.Selection.Effective, status.ID), strings.Join(status.Selection.DependencyReasons[status.ID], ", "))
	}
	if !status.Selection.AllowsCodingTools {
		fmt.Fprintln(cli.options.Stdout, "Coding tools: forbidden by yard role")
	}
	if status.ID == "" {
		ids := make([]string, 0, len(status.Selection.DependencyReasons))
		for id := range status.Selection.DependencyReasons {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			fmt.Fprintf(cli.options.Stdout, "%s required by: %s\n", id, strings.Join(status.Selection.DependencyReasons[id], ", "))
		}
	}
	if status.Detail != "" {
		fmt.Fprintln(cli.options.Stdout, status.Detail)
	}
}

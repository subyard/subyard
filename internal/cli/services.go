package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/Subyard/Subyard/internal/adapters/shelladapter"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerinventory"
	"github.com/Subyard/Subyard/internal/ownerservice"
	"github.com/Subyard/Subyard/internal/profile"
)

type serviceSelection struct {
	definition  profile.Definition
	record      ownerservice.Registration
	registry    ownerservice.Registry
	publication string
}

type serviceArguments struct{ action, id, owner, pool, yard, expectedOwner string }

func serviceOptions(arguments []string) (serviceArguments, []string, error) {
	var request serviceArguments
	var rest []string
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if slices.Contains([]string{"--owner", "--pool", "--yard", "--expect-owner"}, argument) {
			index++
			if index == len(arguments) {
				return request, nil, fmt.Errorf("%s requires a value", argument)
			}
			value := arguments[index]
			if argument != "--owner" && argument != "--expect-owner" && !domain.SafeName(value) || (argument == "--owner" || argument == "--expect-owner") && !domain.SafeID(value) {
				return request, nil, fmt.Errorf("invalid %s selector", argument)
			}
			var field *string
			switch argument {
			case "--owner":
				field = &request.owner
			case "--pool":
				field = &request.pool
			case "--yard":
				field = &request.yard
			case "--expect-owner":
				field = &request.expectedOwner
			}
			if *field != "" {
				return request, nil, fmt.Errorf("%s may be specified only once", argument)
			}
			*field = value
			continue
		}
		rest = append(rest, argument)
	}
	return request, rest, nil
}

func parseServiceArguments(arguments []string) (serviceArguments, error) {
	request, rest, err := serviceOptions(arguments)
	if err != nil {
		return request, err
	}
	rest = slices.DeleteFunc(rest, func(value string) bool { return value == "--yes" || value == "-y" })
	if len(rest) != 2 || !slices.Contains([]string{"register", "install", "start", "stop", "update", "remove"}, rest[0]) || !domain.SafeName(rest[1]) {
		return request, errors.New("service requires register|install|start|stop|update|remove and a service ID")
	}
	request.action, request.id = rest[0], rest[1]
	if request.action != "register" && request.action != "install" && request.yard != "" {
		return request, errors.New("--yard is supported only by service register or install")
	}
	return request, nil
}

func (cli *CLI) servicePaths() (string, string, string, error) {
	operator := firstNonempty(cli.env["SUBYARD_OPERATOR_HOME"], cli.env["HOME"])
	configHome, err := config.ResolveConfigHome(operator, cli.env)
	if err != nil {
		return "", "", "", err
	}
	dataHome := firstNonempty(cli.env["SUBYARD_HOME"], filepath.Join(operator, ".subyard"))
	if !filepath.IsAbs(dataHome) || filepath.Clean(dataHome) != dataHome {
		return "", "", "", errors.New("invalid owner data home")
	}
	owner, _, err := configsync.ResolveHostID(configHome, cli.baseEnv)
	return dataHome, configHome, owner, err
}

// Owner transport needs trusted installation paths, not a workspace's settings.
// Native operations must replace this locator with their resolved service yard.
func (cli *CLI) ownerServiceContext() (config.Loaded, error) {
	home, configHome, _, err := cli.servicePaths()
	if err != nil {
		return config.Loaded{}, err
	}
	return config.Loaded{Catalog: cli.catalog, Environment: maps.Clone(cli.baseEnv), Context: domain.Context{
		YardName: "default", AccessKind: domain.AccessLocal,
		Paths: domain.RuntimePaths{RepositoryRoot: cli.options.RepositoryRoot, ConfigDir: filepath.Join(cli.options.RepositoryRoot, "config"), OperatorHome: firstNonempty(cli.baseEnv["SUBYARD_OPERATOR_HOME"], cli.baseEnv["HOME"]), ConfigHome: configHome, DataHome: home},
	}}, nil
}

func (cli *CLI) declaredServiceCommand(definition command.Definition) bool {
	if definition.Handler == "@service" {
		return true
	}
	_, err := cli.serviceDefinition(definition.Name)
	return err == nil
}

func (cli *CLI) serviceDefinition(id string) (profile.Definition, error) {
	definitions, err := profile.Load(cli.options.RepositoryRoot)
	if err != nil {
		return profile.Definition{}, err
	}
	for _, definition := range definitions {
		if definition.InfrastructureService != nil && definition.InfrastructureService.ID == id {
			return definition, nil
		}
	}
	return profile.Definition{}, fmt.Errorf("infrastructure service %q is not declared by a shipped profile", id)
}

func (cli *CLI) serviceRuntimeRole(yard domain.Context, uuid string) (string, string, string, error) {
	definitions, err := profile.Load(cli.options.RepositoryRoot)
	if err != nil {
		return "", "", "", err
	}
	registry := ownerservice.Registry{Root: filepath.Join(yard.Paths.DataHome, "services")}
	for _, definition := range definitions {
		if definition.InfrastructureService == nil {
			continue
		}
		records, err := registry.List(definition.InfrastructureService.ID)
		if err != nil {
			return "", "", "", err
		}
		for _, record := range records {
			if record.Yard == yard.YardName && record.Project == yard.IncusProject && record.Instance == yard.YardInstanceName && record.UUID == uuid {
				return "infrastructure-service", record.Service, record.Pool, nil
			}
		}
	}
	return "", "", "", nil
}

// Yard teardown retires the same captured service metadata after native cleanup.
// This also covers ordinary teardown and prevents orphan routes during fixtures
// or when the operator deliberately removes the runtime through its yard name.
func (prepared *preparedCommand) bindServiceTeardown(ctx context.Context) error {
	cli := prepared.CLI
	definitions, err := profile.Load(cli.options.RepositoryRoot)
	if err != nil {
		return err
	}
	home, _, owner, err := cli.servicePaths()
	if err != nil {
		return err
	}
	registry := ownerservice.Registry{Root: filepath.Join(home, "services")}
	var selected []*serviceSelection
	for _, definition := range definitions {
		if definition.InfrastructureService == nil {
			continue
		}
		records, err := registry.List(definition.InfrastructureService.ID)
		if err != nil {
			return err
		}
		for _, record := range records {
			if record.Owner == owner && record.Yard == prepared.Loaded.Context.YardName && record.Project == prepared.Loaded.Context.IncusProject && record.Instance == prepared.Loaded.Context.YardInstanceName {
				selection := &serviceSelection{definition: definition, record: record, registry: registry, publication: filepath.Join(home, definition.InfrastructureService.RouteRoot, "services")}
				if err := cli.checkServiceSelection(ctx, prepared.Loaded, selection); err != nil {
					return err
				}
				selected = append(selected, selection)
			}
		}
	}
	if len(selected) == 0 {
		return nil
	}
	steps, execute, refresh := prepared.steps, prepared.execute, prepared.refresh
	guard := func(ctx context.Context) error {
		for _, selection := range selected {
			if err := cli.checkServiceSelection(ctx, prepared.Loaded, selection); err != nil {
				return err
			}
		}
		return nil
	}
	prepared.refresh = func(ctx context.Context) (domain.ActionID, domain.ActionDelta, error) {
		if err := guard(ctx); err != nil {
			return "", domain.ActionDelta{}, err
		}
		return refresh(ctx)
	}
	prepared.steps = func() []domain.OperationStep {
		result := steps()
		for index, selection := range selected {
			step := selection.step()
			step.ID = fmt.Sprintf("service-registration-%d", index)
			step.Decision = domain.StepApply
			step.Desired = "exact service registration and publication absent after native cleanup"
			result = append(result, step)
		}
		return result
	}
	prepared.execute = func(ctx context.Context, orchestrator *application.Orchestrator, diagnostics io.Writer) (domain.AdapterResult, error) {
		if err := guard(ctx); err != nil {
			return domain.AdapterResult{}, err
		}
		result, err := execute(ctx, orchestrator, diagnostics)
		if err != nil || result.Status != "ok" {
			return result, err
		}
		for _, selection := range selected {
			if err := registry.Remove(selection.record, selection.publication); err != nil {
				return result, err
			}
		}
		return result, nil
	}
	for _, selection := range selected {
		if err := prepared.bindServiceOperation(ctx, selection); err != nil {
			return err
		}
	}
	return nil
}

func (cli *CLI) resolveServiceCommand(id string, arguments []string) (*serviceSelection, error) {
	return cli.resolveServiceBinding(id, arguments, true)
}
func (cli *CLI) resolveServiceBinding(id string, arguments []string, requirePublication bool) (*serviceSelection, error) {
	options, _, err := serviceOptions(arguments)
	if err != nil {
		return nil, err
	}
	definition, err := cli.serviceDefinition(id)
	if err != nil {
		return nil, err
	}
	home, _, owner, err := cli.servicePaths()
	if err != nil {
		return nil, err
	}
	if options.expectedOwner != "" && options.expectedOwner != owner {
		return nil, fmt.Errorf("%w: selected service owner changed", domain.ErrPlanStale)
	}
	registry := ownerservice.Registry{Root: filepath.Join(home, "services")}
	record, err := registry.Resolve(owner, id, options.pool)
	if err != nil {
		var failure *ownerservice.ResolutionError
		if errors.As(err, &failure) {
			return nil, err
		}
		return nil, &ownerservice.ResolutionError{Code: "service_unavailable", Owner: owner, Service: id, Pool: options.pool}
	}
	publication := filepath.Join(home, definition.InfrastructureService.RouteRoot, "services")
	if err := registry.Published(record, publication); requirePublication && err != nil {
		return nil, &ownerservice.ResolutionError{Code: "service_unavailable", Owner: owner, Service: id, Pool: record.Pool, Candidates: []ownerservice.Public{record.Public}}
	}
	return &serviceSelection{definition: definition, record: record, registry: registry, publication: publication}, nil
}

// Resolve the installation owner before inspecting any selected workspace.
func (cli *CLI) selectServiceInvocation(ctx context.Context, definition command.Definition, arguments []string) (string, []string, error) {
	options, _, err := serviceOptions(arguments)
	if err != nil {
		return "", nil, err
	}
	home, _, owner, err := cli.servicePaths()
	if err != nil {
		return "", nil, err
	}
	forwarded := make([]string, 0, len(arguments))
	for index := 0; index < len(arguments); index++ {
		if arguments[index] == "--owner" {
			index++
			continue
		}
		forwarded = append(forwarded, arguments[index])
	}
	if options.owner != "" && options.owner != "local" && options.owner != owner {
		connections, err := (ownerinventory.Connections{Root: filepath.Join(home, "owner-inventory")}).List()
		if err != nil {
			return "", nil, err
		}
		for _, connection := range connections {
			if connection.HostID == options.owner {
				if _, err := connection.RequireTrust(); err != nil {
					return "", nil, err
				}
				base, err := cli.ownerServiceContext()
				if err != nil {
					return "", nil, err
				}
				base.Context.AccessKind = domain.AccessRemote
				base.Context.OwnerEndpoint = connection.Destination
				base.Context.OwnerYardName = "default"
				cli.serviceRemote = &base
				cli.serviceExpectedOwner = connection.HostID
				return "default", forwarded, nil
			}
		}
		return "", nil, fmt.Errorf("service_owner_absent: owner=%s", options.owner)
	}
	if options.expectedOwner != "" && options.expectedOwner != owner {
		return "", nil, fmt.Errorf("%w: selected service owner changed", domain.ErrPlanStale)
	}
	if definition.Handler == "@service" {
		request, err := parseServiceArguments(forwarded)
		if err != nil {
			return "", nil, err
		}
		declared, err := cli.serviceDefinition(request.id)
		if err != nil {
			return "", nil, err
		}
		if request.action == "register" {
			if request.yard == "" {
				return "", nil, errors.New("service register requires --yard NAME")
			}
			return request.yard, forwarded, nil
		}
		if request.action == "install" {
			record, err := (ownerservice.Registry{Root: filepath.Join(home, "services")}).Resolve(owner, request.id, request.pool)
			if err == nil {
				if request.yard != "" && request.yard != record.Yard {
					return "", nil, domain.ErrPlanStale
				}
				return record.Yard, forwarded, nil
			}
			var absent *ownerservice.ResolutionError
			if !errors.As(err, &absent) || absent.Code != "service_absent" {
				return "", nil, err
			}
			yard := request.yard
			if yard == "" {
				if request.pool != "" && request.pool != "default" {
					return "", nil, errors.New("installing a named pool requires --yard NAME")
				}
				yard = declared.InfrastructureService.DefaultYard
			}
			return yard, forwarded, nil
		}
		selection, err := cli.resolveServiceBinding(request.id, forwarded, request.action != "remove")
		if err != nil {
			return "", nil, err
		}
		cli.serviceSelection = selection
		return selection.record.Yard, forwarded, nil
	}
	selection, err := cli.resolveServiceCommand(definition.Name, forwarded)
	if err != nil {
		return "", nil, err
	}
	cli.serviceSelection = selection
	return selection.record.Yard, forwarded, nil
}

// A prepared service operation may read its context several times. Previously
// adopted profile settings are resolved values, not new command overrides.
func (cli *CLI) serviceContextReader() *CLI {
	reader := *cli
	reader.env = maps.Clone(cli.baseEnv)
	if operation := cli.env["SUBYARD_OPERATION_ID"]; operation != "" {
		reader.env["SUBYARD_OPERATION_ID"] = operation
	}
	return &reader
}

func (cli *CLI) loadServiceYard(yard string) (config.Loaded, error) {
	loaded, err := cli.serviceContextReader().loadContext(yard)
	if err == nil {
		cli.adoptContext(loaded)
	}
	return loaded, err
}

func (cli *CLI) loadServiceContext(yard string, arguments []string) (config.Loaded, *initBootstrap, error) {
	loaded, bootstrap, err := cli.serviceContextReader().resolveServiceContext(yard, arguments)
	if err == nil {
		cli.adoptContext(loaded)
	}
	return loaded, bootstrap, err
}

func (cli *CLI) resolveServiceContext(yard string, arguments []string) (config.Loaded, *initBootstrap, error) {
	request, err := parseServiceArguments(arguments)
	if err != nil {
		return config.Loaded{}, nil, err
	}
	if request.action == "install" {
		definition, err := cli.serviceDefinition(request.id)
		if err != nil {
			return config.Loaded{}, nil, err
		}
		if existing, existingErr := cli.loadContext(yard); existingErr == nil {
			if !definition.InfrastructureService.Matches(existing.Environment) {
				return config.Loaded{}, nil, errors.New("existing runtime does not match the declared service")
			}
			return existing, nil, nil
		} else if !errors.Is(existingErr, config.ErrUnknownYard) {
			return config.Loaded{}, nil, existingErr
		}
		return cli.loadInitContext(yard, true, []string{"--profile", definition.Name})
	}
	loaded, err := cli.loadContext(yard)
	if request.action == "remove" && errors.Is(err, config.ErrUnknownYard) {
		definition, definitionErr := cli.serviceDefinition(request.id)
		if definitionErr != nil {
			return config.Loaded{}, nil, definitionErr
		}
		cli.allowResetYard = true
		loaded, _, err = cli.loadInitContext(yard, true, []string{"--profile", definition.Name})
	}
	return loaded, nil, err
}

func (cli *CLI) reportServiceError(arguments []string, err error) int {
	var failure *ownerservice.ResolutionError
	if slices.Contains(arguments, "--json") && errors.As(err, &failure) {
		_ = json.NewEncoder(cli.options.Stdout).Encode(struct {
			Schema int    `json:"schema_version"`
			Status string `json:"status"`
			*ownerservice.ResolutionError
		}{1, "error", failure})
	} else {
		cli.errorf("%v", err)
	}
	return 1
}

// serviceRuntime checks native installation ownership and UUID; a matching name,
// enabled setting, profile or reachable broker cannot authorize registration.
func (cli *CLI) serviceRuntime(ctx context.Context, loaded config.Loaded, definition profile.Definition) (string, string, error) {
	if loaded.Context.AccessKind != domain.AccessLocal || !definition.InfrastructureService.Matches(loaded.Environment) {
		return "", "", errors.New("runtime does not match its declared infrastructure service")
	}
	incus, _ := cli.statusPorts()
	state, err := incus.ReconcileState(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName, loaded.Context.IncusBridge, loaded.Environment["SRV_POOL"], loaded.Environment["SRV_VOLUME"])
	if err != nil {
		return "", "", err
	}
	project := state.ProjectConfig
	if !state.ProjectFound || !state.InstanceFound || project["user.subyard.installation"] != "1" && project["user.subyard.installation"] != domain.AdoptedInstallation || project["user.subyard.operator_uid"] != strconv.Itoa(os.Getuid()) || project["user.subyard.data_home"] != loaded.Context.Paths.DataHome || project["user.subyard.config_home"] != loaded.Context.Paths.ConfigHome || project["user.subyard.yard_name"] != loaded.Context.YardName || project["user.subyard.instance_name"] != loaded.Context.YardInstanceName || state.Instance.Config["user.subyard.managed"] != "true" || state.Instance.Config["user.subyard.name"] != loaded.Context.YardName {
		return "", "", errors.New("service runtime ownership is unproven")
	}
	uuid := state.Instance.Config["volatile.uuid"]
	if uuid == "" || project["user.subyard.installation"] == domain.AdoptedInstallation && project["user.subyard.adopted_uuid"] != uuid {
		return "", "", errors.New("service runtime UUID is unproven")
	}
	ownership := map[string]string{}
	for _, key := range []string{"user.subyard.installation", "user.subyard.adopted_uuid", "user.subyard.operator_uid", "user.subyard.data_home", "user.subyard.config_home", "user.subyard.yard_name", "user.subyard.instance_name", "user.subyard.bridge"} {
		ownership[key] = project[key]
	}
	return uuid, operationStateDigest(ownership), nil
}

func (selection *serviceSelection) step() domain.OperationStep {
	record := selection.record
	target := fmt.Sprintf("owner=%s service=%s pool=%s project=%s instance=%s runtime_uuid=%s registration_generation=%s", record.Owner, record.Service, record.Pool, record.Project, record.Instance, record.UUID, record.Generation)
	return domain.OperationStep{ID: "service", Target: target, Observed: "captured infrastructure service binding", Desired: "same owner runtime and registration", Decision: domain.StepConditional, Preconditions: []string{"registration generation, native UUID and installation ownership are unchanged"}, Verify: "verify the exact owner service registration before effects", Consequence: target}
}

func (cli *CLI) checkServiceSelection(ctx context.Context, loaded config.Loaded, selection *serviceSelection) error {
	_, _, owner, ownerErr := cli.servicePaths()
	if ownerErr != nil {
		return ownerErr
	}
	if owner != selection.record.Owner {
		return fmt.Errorf("%w: service owner identity changed", domain.ErrPlanStale)
	}
	current, err := selection.registry.Resolve(selection.record.Owner, selection.record.Service, selection.record.Pool)
	if err != nil || current != selection.record {
		return fmt.Errorf("%w: infrastructure service registration changed", domain.ErrPlanStale)
	}
	uuid, ownership, err := cli.serviceRuntime(ctx, loaded, selection.definition)
	if err != nil {
		return err
	}
	if uuid != current.UUID || ownership != current.Ownership || loaded.Context.IncusProject != current.Project || loaded.Context.YardInstanceName != current.Instance {
		return fmt.Errorf("%w: infrastructure service runtime changed", domain.ErrPlanStale)
	}
	return nil
}

func (cli *CLI) checkServiceOwner(expected string) error {
	_, _, actual, err := cli.servicePaths()
	if err != nil {
		return err
	}
	if actual != expected {
		return domain.ErrPlanStale
	}
	return nil
}

func (cli *CLI) serviceRuntimeAbsent(ctx context.Context, loaded config.Loaded, selection *serviceSelection) (bool, error) {
	incus, _ := cli.statusPorts()
	state, err := incus.ReconcileState(ctx, selection.record.Project, selection.record.Instance, loaded.Context.IncusBridge, loaded.Environment["SRV_POOL"], loaded.Environment["SRV_VOLUME"])
	return !state.ProjectFound && !state.InstanceFound, err
}

func (prepared *preparedCommand) prepareServiceUnregister(selection *serviceSelection) error {
	prepared.stepsComplete = true
	prepared.executeNoOp = true
	prepared.exactState = operationStateDigest(selection.record)
	prepared.assess = func(context.Context) (domain.ActionID, domain.ActionDelta, error) {
		return "service.unregister", domain.ActionDelta{Changed: true}, nil
	}
	prepared.steps = func() []domain.OperationStep {
		step := selection.step()
		step.Decision = domain.StepApply
		step.Desired = "registration and publication absent; native runtime already absent"
		return []domain.OperationStep{step}
	}
	prepared.execute = func(ctx context.Context, _ *application.Orchestrator, _ io.Writer) (domain.AdapterResult, error) {
		if err := prepared.CLI.checkServiceOwner(selection.record.Owner); err != nil {
			return domain.AdapterResult{}, err
		}
		absent, err := prepared.CLI.serviceRuntimeAbsent(ctx, prepared.Loaded, selection)
		if err != nil {
			return domain.AdapterResult{}, err
		}
		if !absent {
			return domain.AdapterResult{}, domain.ErrPlanStale
		}
		if err := selection.registry.Remove(selection.record, selection.publication); err != nil {
			return domain.AdapterResult{}, err
		}
		return domain.AdapterResult{Schema: shelladapter.ProtocolSchema, OperationID: prepared.Plan.OperationID, Status: "ok"}, nil
	}
	return nil
}

func (prepared *preparedCommand) prepareService(ctx context.Context, bootstrap *initBootstrap) error {
	cli := prepared.CLI
	request, err := parseServiceArguments(prepared.Arguments)
	if err != nil {
		return err
	}
	definition, err := cli.serviceDefinition(request.id)
	if err != nil {
		return err
	}
	home, _, owner, err := cli.servicePaths()
	if err != nil {
		return err
	}
	registry := ownerservice.Registry{Root: filepath.Join(home, "services")}
	publication := filepath.Join(home, definition.InfrastructureService.RouteRoot, "services")
	pool := request.pool
	var selection *serviceSelection
	var ownerRepairBefore *ownerservice.Registration
	if request.action == "register" || request.action == "install" {
		var record ownerservice.Registration
		before, resolveErr := registry.Resolve(owner, request.id, pool)
		if request.action == "register" {
			records, listErr := registry.List(request.id)
			if listErr != nil {
				return listErr
			}
			for _, candidate := range records {
				if candidate.Pool == pool || pool == "" && len(records) == 1 {
					before, resolveErr = candidate, nil
					break
				}
			}
		}

		var absent *ownerservice.ResolutionError
		if resolveErr != nil && (!errors.As(resolveErr, &absent) || absent.Code != "service_absent") {
			return resolveErr
		}
		if resolveErr == nil {
			record = before
			pool = record.Pool
			if record.Yard != prepared.Loaded.Context.YardName {
				return domain.ErrPlanStale
			}
		} else if pool == "" {
			pool = "default"
		}
		if record.Generation == "" {
			generation, err := ownerservice.NewGeneration()
			if err != nil {
				return err
			}
			record = ownerservice.Registration{Public: ownerservice.Public{SchemaVersion: 1, Owner: owner, Service: request.id, Pool: pool, Yard: prepared.Loaded.Context.YardName, Project: prepared.Loaded.Context.IncusProject, Instance: prepared.Loaded.Context.YardInstanceName, Generation: generation}}
		}
		selection = &serviceSelection{definition: definition, record: record, registry: registry, publication: publication}
		if request.action == "install" && record.UUID == "" {
			incus, _ := cli.statusPorts()
			native, err := incus.ReconcileState(ctx, record.Project, record.Instance, prepared.Loaded.Context.IncusBridge, prepared.Loaded.Environment["SRV_POOL"], prepared.Loaded.Environment["SRV_VOLUME"])
			if err != nil {
				return err
			}
			if native.InstanceFound {
				if _, _, err := cli.serviceRuntime(ctx, prepared.Loaded, definition); err != nil {
					return err
				}
			}
		}
		if request.action == "register" {
			uuid, ownership, err := cli.serviceRuntime(ctx, prepared.Loaded, definition)
			if err != nil {
				return err
			}
			if resolveErr == nil && (uuid != record.UUID || ownership != record.Ownership) {
				return domain.ErrPlanStale
			}
			selection.record.UUID, selection.record.Ownership = uuid, ownership
			if record.Owner != owner {
				ownerRepairBefore = &before
				generation, err := ownerservice.NewGeneration()
				if err != nil {
					return err
				}
				selection.record.Owner, selection.record.Generation = owner, generation
			}
		}
	} else {
		selection, err = cli.resolveServiceBinding(request.id, prepared.Arguments, request.action != "remove")
		if err != nil {
			return err
		}
		prepared.Loaded, _, err = cli.loadServiceContext(selection.record.Yard, prepared.Arguments)
		if err != nil {
			return err
		}
		if request.action == "remove" {
			absent, err := cli.serviceRuntimeAbsent(ctx, prepared.Loaded, selection)
			if err != nil {
				return err
			}
			if absent {
				return prepared.prepareServiceUnregister(selection)
			}
		}
		if err := cli.checkServiceSelection(ctx, prepared.Loaded, selection); err != nil {
			return err
		}
	}
	if request.action == "register" {
		prepared.stepsComplete = true
		prepared.executeNoOp = true
		prepared.exactState = operationStateDigest(selection.record)
		prepared.assess = func(context.Context) (domain.ActionID, domain.ActionDelta, error) {
			return "service.register", domain.ActionDelta{Changed: true}, nil
		}
		prepared.steps = func() []domain.OperationStep {
			step := selection.step()
			step.Decision = domain.StepApply
			step.Desired = "registered infrastructure service; existing runtime and leases retained"
			if ownerRepairBefore != nil {
				step.Observed = fmt.Sprintf("previous owner=%s registration_generation=%s; native installation and UUID verified", ownerRepairBefore.Owner, ownerRepairBefore.Generation)
			}
			return []domain.OperationStep{step}
		}
		prepared.execute = func(ctx context.Context, _ *application.Orchestrator, _ io.Writer) (domain.AdapterResult, error) {
			if err := cli.checkServiceOwner(selection.record.Owner); err != nil {
				return domain.AdapterResult{}, err
			}
			uuid, ownership, err := cli.serviceRuntime(ctx, prepared.Loaded, definition)
			if err != nil {
				return domain.AdapterResult{}, err
			}
			if uuid != selection.record.UUID || ownership != selection.record.Ownership {
				return domain.AdapterResult{}, domain.ErrPlanStale
			}
			var registrationErr error
			if ownerRepairBefore != nil {
				registrationErr = registry.RepairOwner(*ownerRepairBefore, selection.record)
			} else {
				registrationErr = registry.Save(selection.record)
			}
			if registrationErr != nil {
				return domain.AdapterResult{}, registrationErr
			}
			if err := registry.Publish(selection.record, publication); err != nil {
				return domain.AdapterResult{}, err
			}
			return domain.AdapterResult{Schema: shelladapter.ProtocolSchema, OperationID: prepared.Plan.OperationID, Status: "ok"}, nil
		}
		return nil
	}
	resumeSource := ""
	if request.action == "start" && definition.InfrastructureService.BindingHandler != "" {
		source, err := os.ReadFile(filepath.Join(definition.Root, definition.InfrastructureService.BindingHandler))
		if err != nil {
			return err
		}
		resumeSource = operationStateDigest(source)
	}
	innerName := request.action
	if innerName == "install" || innerName == "update" {
		innerName = "init"
	}
	if innerName == "remove" {
		innerName = "teardown"
	}
	innerDefinition, ok := cli.manifest.Lookup(innerName)
	if !ok {
		return errors.New("service lifecycle command unavailable")
	}
	innerArguments := []string{}
	if request.action == "install" && bootstrap != nil {
		innerArguments = []string{"--profile", definition.Name}
	}
	if request.action == "remove" {
		innerArguments = []string{"--reset-config"}
	}
	child := &preparedCommand{CLI: cli, Definition: innerDefinition, Arguments: innerArguments, Loaded: prepared.Loaded, policy: commandPolicy(innerDefinition, prepared.Loaded.Context, innerArguments, nil), interactiveSetup: prepared.interactiveSetup}
	behavior, err := resolveCoreCommand(innerDefinition)
	if err != nil {
		return err
	}
	if err := behavior.prepare(child, ctx, bootstrap); err != nil {
		return err
	}
	if child.assess == nil {
		child.assess = func(context.Context) (domain.ActionID, domain.ActionDelta, error) {
			return "yard.start", domain.ActionDelta{Changed: true, Consequences: child.policy.Consequences}, nil
		}
	}
	prepared.Loaded = child.Loaded
	registrationPending := request.action == "remove" || request.action == "install" && (selection.record.UUID == "" || registry.Published(selection.record, publication) != nil)
	prepared.assess = func(ctx context.Context) (domain.ActionID, domain.ActionDelta, error) {
		action, delta, err := child.assess(ctx)
		prepared.initFacts = child.initFacts
		delta.Changed = delta.Changed || registrationPending
		return action, delta, err
	}
	if child.refresh != nil {
		prepared.refresh = func(ctx context.Context) (domain.ActionID, domain.ActionDelta, error) {
			child.Plan = prepared.Plan
			if request.action != "install" {
				if err := cli.checkServiceSelection(ctx, prepared.Loaded, selection); err != nil {
					return "", domain.ActionDelta{}, err
				}
			}
			action, delta, err := child.refresh(ctx)
			delta.Changed = delta.Changed || registrationPending
			return action, delta, err
		}
	}
	prepared.initExecution = child.initExecution
	prepared.initFacts = child.initFacts
	prepared.stepsComplete = child.stepsComplete
	prepared.executeNoOp = true
	prepared.exactState = operationStateDigest(struct {
		Service      ownerservice.Registration
		Lifecycle    string
		ResumeSource string
	}{selection.record, child.exactState, resumeSource})
	prepared.closeResource = child.closeResource
	prepared.preview = child.preview
	prepared.steps = func() []domain.OperationStep {
		step := selection.step()
		if registrationPending {
			step.Decision = domain.StepApply
			step.Desired = "registered exact infrastructure runtime"
		}
		if request.action == "remove" {
			step.Desired = "exact service registration and publication absent"
		}
		steps := []domain.OperationStep{step}
		if child.steps != nil {
			steps = append(steps, child.steps()...)
		}
		return steps
	}
	prepared.execute = func(ctx context.Context, orchestrator *application.Orchestrator, diagnostics io.Writer) (domain.AdapterResult, error) {
		if err := cli.checkServiceOwner(owner); err != nil {
			return domain.AdapterResult{}, err
		}
		if request.action == "install" {
			current, resolveErr := registry.Resolve(owner, request.id, pool)
			if selection.record.UUID == "" {
				var absent *ownerservice.ResolutionError
				if !errors.As(resolveErr, &absent) || absent.Code != "service_absent" {
					return domain.AdapterResult{}, domain.ErrPlanStale
				}
			} else {
				if resolveErr != nil || current != selection.record {
					return domain.AdapterResult{}, domain.ErrPlanStale
				}
				uuid, ownership, err := cli.serviceRuntime(ctx, prepared.Loaded, definition)
				if err != nil {
					return domain.AdapterResult{}, err
				}
				if uuid != current.UUID || ownership != current.Ownership {
					return domain.AdapterResult{}, domain.ErrPlanStale
				}
			}
		} else {
			if err := cli.checkServiceSelection(ctx, prepared.Loaded, selection); err != nil {
				return domain.AdapterResult{}, err
			}
		}
		child.Plan = prepared.Plan
		// Keep the exact confirmed plan's authorization and audit scope while
		// selecting the existing lifecycle's bounded physical adapter.
		orchestrator.Runner = cli.operationOrchestrator(prepared.Plan.OperationID, child.Loaded, nil, &innerDefinition).Runner
		result, err := child.execute(ctx, orchestrator, diagnostics)
		if err != nil || result.Status != "ok" {
			return result, err
		}
		if request.action == "install" {
			loaded, err := cli.loadServiceYard(selection.record.Yard)
			if err != nil {
				return result, err
			}
			uuid, ownership, err := cli.serviceRuntime(ctx, loaded, definition)
			if err != nil {
				return result, err
			}
			if selection.record.UUID != "" && (selection.record.UUID != uuid || selection.record.Ownership != ownership) {
				return result, domain.ErrPlanStale
			}
			selection.record.UUID, selection.record.Ownership = uuid, ownership
			if err := registry.Save(selection.record); err != nil {
				return result, err
			}
			err = registry.Publish(selection.record, publication)
			return result, err
		}
		if request.action == "start" {
			if err := cli.applyServiceBinding(ctx, selection, resumeSource, "", true); err != nil {
				return result, err
			}
		}
		if request.action == "remove" {
			_, readErr := registry.Resolve(owner, request.id, pool)
			var absent *ownerservice.ResolutionError
			if !errors.As(readErr, &absent) || absent.Code != "service_absent" {
				err = registry.Remove(selection.record, publication)
			}
		}
		return result, err
	}
	if request.action == "stop" {
		return prepared.bindServiceOperation(ctx, selection)
	}
	return nil
}

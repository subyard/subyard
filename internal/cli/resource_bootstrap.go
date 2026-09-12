package cli

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/resourceendpoint"
)

// A resource bootstrap composes desired configuration, init and the resource's
// own assessment. Preparation remains read-only; the normal resource action
// owns the single confirmation before any of these changes are applied.
type resourceBootstrap struct {
	loaded        config.Loaded
	initial       config.Loaded
	definition    resource.Definition
	init          *initExecution
	selectionPath string
	selection     config.PersistentFileSnapshot
	profiles      string
	endpoint      *resourceendpoint.Plan
	request       resourceendpoint.Request
	manager       resourceendpoint.Manager
}

func (cli *CLI) prepareResourceBootstrap(ctx context.Context, loaded config.Loaded, definition resource.Definition, verb string) (*resourceBootstrap, error) {
	if !definition.Bootstrap || verb != definition.BringUp {
		return nil, nil
	}
	bootstrap := &resourceBootstrap{loaded: loaded, initial: loaded, definition: definition}
	bootstrap.loaded.Environment = maps.Clone(loaded.Environment)
	profiles := strings.Fields(loaded.Environment["ENVIRONMENT_PROFILES"])
	if !slices.Contains(profiles, definition.Profile) {
		profiles = append(profiles, definition.Profile)
		bootstrap.profiles = strings.Join(profiles, " ")
		bootstrap.loaded.Environment["ENVIRONMENT_PROFILES"] = bootstrap.profiles
		path, err := configScalarAuthoringPath(loaded, config.ScopeYard)
		if err != nil {
			return nil, err
		}
		bootstrap.selectionPath = path
		bootstrap.selection, err = readConfigAuthoringTarget(path)
		if err != nil {
			return nil, err
		}
	}
	if definition.Endpoint != nil {
		host, port := config.ResourceEndpointOverrides(loaded, definition)
		bootstrap.request = resourceendpoint.Request{
			Directory: filepath.Join(loaded.Context.Paths.DataHome, "resource-endpoints"),
			Yard:      loaded.Context.YardName, Resource: definition.Profile + "." + definition.Name,
			Host: host, Port: port, PreferredPort: definition.Endpoint.PreferredPort,
		}
		reserved, err := cli.resourceReservedPorts(loaded, definition)
		if err != nil {
			return nil, err
		}
		bootstrap.request.ReservedPorts = reserved
		bootstrap.manager = cli.resourceEndpointManager()
		endpoint, err := bootstrap.previewEndpoint(ctx)
		if err != nil {
			return nil, err
		}
		bootstrap.endpoint = &endpoint
		bootstrap.loaded.Environment[definition.Proxy.AdvertiseHostSetting] = endpoint.Host
		bootstrap.loaded.Environment[definition.Proxy.HostPortSetting] = strconv.Itoa(endpoint.Port)
	}
	execution, err := cli.prepareInitExecution(ctx, bootstrap.loaded, nil, nil)
	if err != nil {
		return nil, err
	}
	// An already initialized yard does not need the unconditional init hook retry:
	// the resource performs its own project reconciliation during bring-up.
	if !execution.hooksOnly() {
		bootstrap.init = execution
	}
	return bootstrap, nil
}

// Reserve configured owner ports even when another yard is stopped and its
// service has never acquired an endpoint record. Remote yards belong to a
// different owner and cannot consume ports here.
func (cli *CLI) resourceReservedPorts(current config.Loaded, definition resource.Definition) ([]int, error) {
	names, err := config.YardNames(current.Context.Paths.ConfigDir, current.Context.Paths.ConfigHome)
	if err != nil {
		return nil, err
	}
	settings := []string{"SSH_PORT", "ADB_PROXY_PORT", "ADB_CONSOLE_PROXY_PORT", "AI_OBSERVER_HOST_PORT"}
	for _, item := range cli.resources.Definitions() {
		if item.Proxy != nil && !slices.Contains(settings, item.Proxy.HostPortSetting) {
			settings = append(settings, item.Proxy.HostPortSetting)
		}
	}
	ports := []int{}
	for _, name := range names {
		loaded := current
		if name != current.Context.YardName {
			environment := maps.Clone(cli.baseEnv)
			// Command-scoped ports select this invocation's yard, not its peers.
			for _, setting := range settings {
				delete(environment, setting)
			}
			environment["SUBYARD_CONFIG_HOME"] = current.Context.Paths.ConfigHome
			environment["SUBYARD_HOME"] = current.Context.Paths.DataHome
			loaded, err = config.Load(config.LoadOptions{RepositoryRoot: cli.options.RepositoryRoot,
				OperatorHome: current.Context.Paths.OperatorHome, YardName: name, Environment: environment})
			if err != nil {
				return nil, fmt.Errorf("inspect owner ports for yard %s: %w", name, err)
			}
		}
		if loaded.Context.AccessKind == domain.AccessRemote {
			continue
		}
		for _, setting := range settings {
			if name == current.Context.YardName && setting == definition.Proxy.HostPortSetting {
				continue
			}
			if port, err := strconv.Atoi(loaded.Environment[setting]); err == nil && port > 0 && !slices.Contains(ports, port) {
				ports = append(ports, port)
			}
		}
	}
	slices.Sort(ports)
	return ports, nil
}

func (bootstrap *resourceBootstrap) previewEndpoint(ctx context.Context) (resourceendpoint.Plan, error) {
	bounded, cancel := context.WithTimeout(ctx, resourcePrepareTimeout)
	defer cancel()
	return bootstrap.manager.Preview(bounded, bootstrap.request)
}

func (bootstrap *resourceBootstrap) augment(assessment domain.ActionAssessment) domain.ActionAssessment {
	if bootstrap == nil {
		return assessment
	}
	consequences := []string{}
	if bootstrap.selectionPath != "" {
		consequences = append(consequences, "enable profile "+bootstrap.definition.Profile+" for yard "+bootstrap.loaded.Context.YardName+" while preserving existing profiles")
	}
	if bootstrap.endpoint != nil && (bootstrap.endpoint.HostSource != "saved" || bootstrap.endpoint.PortSource != "saved") {
		// Explicit overrides can already be recorded; an idempotent commit does not
		// change the route or existing client grants.
		host, port, exists, err := resourceendpoint.ReadSaved(bootstrap.request.Directory, bootstrap.request.Yard, bootstrap.request.Resource)
		if err != nil || !exists || host != bootstrap.endpoint.Host || port != bootstrap.endpoint.Port {
			consequences = append(consequences, fmt.Sprintf("reserve owner endpoint %s:%d for this yard", bootstrap.endpoint.Host, bootstrap.endpoint.Port))
		}
	}
	if bootstrap.init != nil {
		consequences = append(consequences, bootstrap.init.consequences()...)
	}
	if len(consequences) != 0 {
		assessment.Changed = true
		assessment.Consequences = append(consequences, assessment.Consequences...)
	}
	return assessment
}

func (bootstrap *resourceBootstrap) refresh(ctx context.Context, cli *CLI) error {
	if bootstrap == nil {
		return nil
	}
	fresh, err := config.Load(config.LoadOptions{
		RepositoryRoot: cli.options.RepositoryRoot,
		OperatorHome:   bootstrap.initial.Environment["SUBYARD_OPERATOR_HOME"],
		YardName:       bootstrap.initial.Context.YardName,
		Environment:    cli.baseEnv,
	})
	if err != nil {
		return err
	}
	for name, trace := range bootstrap.initial.Settings {
		if fresh.Settings[name].EffectiveValue != trace.EffectiveValue {
			return fmt.Errorf("%w: %s changed after confirmation", domain.ErrPlanStale, name)
		}
	}
	if bootstrap.selectionPath != "" {
		current, err := readConfigAuthoringTarget(bootstrap.selectionPath)
		if err != nil {
			return err
		}
		if !sameConfigAuthoringSnapshot(bootstrap.selection, current) {
			return fmt.Errorf("%w: profile configuration changed after confirmation", domain.ErrPlanStale)
		}
	}
	if bootstrap.endpoint != nil {
		reserved, err := cli.resourceReservedPorts(fresh, bootstrap.definition)
		if err != nil {
			return err
		}
		if !slices.Equal(reserved, bootstrap.request.ReservedPorts) {
			return fmt.Errorf("%w: configured owner ports changed after confirmation", domain.ErrPlanStale)
		}
		current, err := bootstrap.previewEndpoint(ctx)
		if err != nil {
			return err
		}
		if current != *bootstrap.endpoint {
			return fmt.Errorf("%w: endpoint selection changed after confirmation", domain.ErrPlanStale)
		}
	}
	if bootstrap.init != nil {
		before := bootstrap.init.consequences()
		if err := bootstrap.init.refreshAssessment(ctx); err != nil {
			return err
		}
		for _, consequence := range bootstrap.init.consequences() {
			if !slices.Contains(before, consequence) && !bootstrap.init.hooksOnly() {
				return fmt.Errorf("%w: initialization consequences changed after confirmation", domain.ErrPlanStale)
			}
		}
	}
	return nil
}

func (bootstrap *resourceBootstrap) apply(ctx context.Context, cli *CLI) error {
	if bootstrap == nil {
		return nil
	}
	if err := bootstrap.refresh(ctx, cli); err != nil {
		return err
	}
	writeSelection := func() error {
		if bootstrap.selectionPath == "" {
			return nil
		}
		return config.WritePersistentAssignmentIfUnchanged(bootstrap.loaded.Context.Paths.ConfigHome,
			bootstrap.selectionPath, "ENVIRONMENT_PROFILES", &bootstrap.profiles, bootstrap.selection)
	}
	if bootstrap.endpoint != nil {
		bounded, cancel := context.WithTimeout(ctx, resourcePrepareTimeout)
		err := bootstrap.manager.CommitWith(bounded, bootstrap.request, *bootstrap.endpoint, writeSelection)
		cancel()
		if err != nil {
			return err
		}
	} else if err := writeSelection(); err != nil {
		return err
	}
	if bootstrap.init != nil {
		if cli.options.InitPlatform == nil && !bootstrap.init.hooksOnly() {
			if err := cli.prepareSudoPrivileges(ctx, cli.options.Stderr, cli.effectiveUID(), bootstrap.definition.Command); err != nil {
				return err
			}
			bootstrap.init.platform = cli.initPlatform(bootstrap.loaded, bootstrap.init.powerYards)
		}
		cli.printInitPlan(bootstrap.init)
		if err := bootstrap.init.run(ctx, cli, cli.options.Stdout); err != nil {
			return err
		}
	}
	return nil
}

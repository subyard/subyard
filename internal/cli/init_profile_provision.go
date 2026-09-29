package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/hostruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/resource"
)

func (cli *CLI) initProfileEndpointSetting(preset config.Loaded, name string) bool {
	for _, contract := range cli.resources.ProxyContracts(strings.Fields(preset.Environment["ENVIRONMENT_PROFILES"])) {
		if contract.AddressPolicy == resource.ProxyAddressOwnerIPv4UDP && (name == contract.AdvertiseHostSetting || name == contract.OwnerInterfaceSetting || name == contract.HostPortSetting) {
			return true
		}
	}
	return false
}

func (execution *initExecution) profileProvisionChanged() bool {
	p := execution.profileProvision
	return p != nil && (p.endpoint != nil || p.requiresPowerCycle || len(p.changedProfiles) != 0 || len(p.startupSeeds) != 0)
}

func (execution *initExecution) profileProvisionConsequences() []string {
	if !execution.profileProvisionChanged() {
		return nil
	}
	return execution.profileProvision.policy(command.Definition{}, execution.loaded.Context).Consequences
}

func (cli *CLI) prepareInitProfileProvision(ctx context.Context, initial config.Loaded, execution *initExecution, arguments []string) error {
	request, err := parseInitArguments(arguments)
	if err != nil || request.profile == "" {
		return err
	}
	available, err := provisionableProfiles(cli.options.RepositoryRoot)
	if err != nil {
		return err
	}
	// A preset may configure an environment without owning an installation hook.
	if !slices.Contains(available, request.profile) {
		return nil
	}
	execution.profileProvision, err = cli.prepareProvisionExecution(execution.loaded, []string{request.profile}, nil)
	if err != nil {
		return err
	}
	readAddresses := cli.provisionEndpointAddresses
	if readAddresses == nil {
		readAddresses = hostruntime.OwnerIPv4Addresses
	}
	var notes []string
	execution.profileProvision.endpoint, notes, err = cli.prepareProvisionEndpointWithBootstrap(initial, execution.profileProvision.profiles, readAddresses, execution.bootstrap)
	if err != nil {
		return err
	}
	for _, note := range notes {
		fmt.Fprintln(cli.options.Stderr, note)
	}
	if endpoint := execution.profileProvision.endpoint; endpoint != nil && execution.bootstrap != nil {
		execution.bootstrap.content = endpoint.content
	}
	return cli.observeInitProfileProvision(ctx, execution)
}

func (cli *CLI) observeInitProfileProvision(ctx context.Context, execution *initExecution) error {
	p := execution.profileProvision
	if p == nil {
		return nil
	}
	pending := execution.bootstrap != nil
	for _, step := range execution.plan.Steps {
		if !step.Converged && (step.Stage.ID == ports.ReconcileStageIncus || step.Stage.ID == ports.ReconcileStageProject || step.Stage.ID == ports.ReconcileStageInstance || step.Stage.ID == ports.ReconcileStageProvision) {
			pending = true
		}
	}
	// A partially initialized host may not have Incus access yet. Settings and
	// addresses remain exact; instance ownership is checked after reconciliation
	// and before provisioning or updating an existing registration's endpoint.
	if err := p.endpoint.checkWithInstance(ctx, cli, !pending); err != nil {
		return err
	}
	if pending {
		p.changedProfiles = slices.Clone(p.profiles)
		p.requiresPowerCycle = true
		p.startupSeeds = nil
		definitions := cli.selectedStartupResources(execution.loaded)
		if len(definitions) == 0 {
			return nil
		}
		deferInspection := execution.bootstrap != nil
		for _, step := range execution.plan.Steps {
			if !step.Converged && step.Stage.ID == ports.ReconcileStageIncus {
				deferInspection = true
			}
		}
		var instance ports.InstanceInfo
		var instanceErr error
		if !deferInspection {
			incus, _ := cli.statusPorts()
			instance, instanceErr = incus.Instance(ctx, execution.loaded.Context.IncusProject, execution.loaded.Context.YardInstanceName)
			if instanceErr != nil && !errors.Is(instanceErr, ports.ErrInstanceNotFound) && !errors.Is(instanceErr, ports.ErrIncusUnavailable) {
				return instanceErr
			}
		}
		p.startupSeedDeferred = deferInspection || instanceErr != nil
		for _, definition := range definitions {
			if slices.Contains(p.profiles, definition.Profile) {
				if instanceErr == nil {
					eligible, err := startupSeedEligible(execution.loaded, instance, definition)
					if err != nil {
						return err
					}
					if !eligible {
						continue
					}
				}
				p.startupSeeds = append(p.startupSeeds, definition)
			}
		}
		return nil
	}
	return cli.observeProvisionExecution(ctx, execution.loaded, command.Definition{Handler: "@provision"}, p)
}

func (cli *CLI) executeInitProfileProvision(ctx context.Context, execution *initExecution, orchestrator *application.Orchestrator, plan domain.OperationPlan, output io.Writer) (domain.AdapterResult, error) {
	p := execution.profileProvision
	endpoint := p.endpoint
	// Init already wrote the approved bootstrap, or canonicalized its integration
	// selection. Accept only those exact bytes before updating the endpoint baseline.
	if endpoint != nil {
		expected := endpoint.before.Content
		if execution.bootstrap != nil {
			expected = execution.bootstrap.content
		} else if selection := execution.integrationSelection; selection != nil && selection.write != nil {
			expected = selection.write.Content
		}
		current, err := config.ReadPersistentFileSnapshot(endpoint.initial.Context.Paths.ConfigHome, endpoint.path)
		if err != nil {
			return domain.AdapterResult{}, err
		}
		if !bytes.Equal(current.Content, expected) {
			return domain.AdapterResult{}, domain.ErrPlanStale
		}
		fresh, err := config.Load(config.LoadOptions{RepositoryRoot: cli.options.RepositoryRoot, OperatorHome: execution.loaded.Context.Paths.OperatorHome, YardName: execution.loaded.Context.YardName, Environment: cli.baseEnv})
		if err != nil {
			return domain.AdapterResult{}, err
		}
		for name, trace := range endpoint.initial.Settings {
			value := trace.EffectiveValue
			if name == "CODING_TOOL_INTEGRATIONS" || name == "AGENTS" {
				continue
			}
			if execution.bootstrap != nil && endpoint.values[name] != "" {
				value = endpoint.values[name]
			}
			if fresh.Settings[name].EffectiveValue != value {
				return domain.AdapterResult{}, fmt.Errorf("%w: setting %s changed during init", domain.ErrPlanStale, name)
			}
		}
		endpoint.initial, endpoint.before, endpoint.bootstrap = fresh, current, nil
		if execution.bootstrap != nil {
			endpoint.content = current.Content
		}
		if execution.integrationSelection != nil && execution.bootstrap == nil {
			endpoint.content = current.Content
			for name, value := range endpoint.values {
				endpoint.content, err = config.EditPersistentAssignmentContent(endpoint.path, endpoint.content, name, &value)
				if err != nil {
					return domain.AdapterResult{}, err
				}
			}
		}
		if err := endpoint.check(ctx, cli); err != nil {
			return domain.AdapterResult{}, err
		}
		execution.loaded = fresh
	}
	definition := command.Definition{Handler: "@provision"}
	orchestrator.Runner = cli.operationOrchestrator(plan.OperationID, execution.loaded, nil, &definition).Runner
	return cli.executeProvision(ctx, orchestrator, execution.loaded, plan, p, output)
}

package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/hostruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/resource"
)

type provisionEndpoint struct {
	initial       config.Loaded
	path          string
	before        config.PersistentFileSnapshot
	content       []byte
	values        map[string]string
	contracts     []resource.ProxyContract
	readAddresses func() ([]hostruntime.OwnerIPv4, error)
	bootstrap     *initBootstrap
}

func selectProvisionEndpoint(addresses []hostruntime.OwnerIPv4, address, iface string) (hostruntime.OwnerIPv4, bool) {
	var candidates []hostruntime.OwnerIPv4
	for _, candidate := range addresses {
		if (address != "" && candidate.Address.String() != address) || (iface != "" && candidate.Interface != iface) {
			continue
		}
		if address == "" && !hostruntime.PublicEndpointIPv4(candidate.Address) {
			continue
		}
		if resource.ExplicitOwnerIPv4(candidate.Address) && !slices.Contains(candidates, candidate) {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) != 1 {
		return hostruntime.OwnerIPv4{}, false
	}
	return candidates[0], true
}

func (cli *CLI) prepareProvisionEndpoint(loaded config.Loaded, profiles []string, readAddresses func() ([]hostruntime.OwnerIPv4, error)) (*provisionEndpoint, []string, error) {
	return cli.prepareProvisionEndpointWithBootstrap(loaded, profiles, readAddresses, nil)
}

func (cli *CLI) prepareProvisionEndpointWithBootstrap(loaded config.Loaded, profiles []string, readAddresses func() ([]hostruntime.OwnerIPv4, error), bootstrap *initBootstrap) (*provisionEndpoint, []string, error) {
	if loaded.Context.AccessKind != domain.AccessLocal || loaded.Context.YardKind != domain.YardVM || loaded.Context.YardName == "" || loaded.Context.YardName == "default" {
		return nil, nil, nil
	}
	plan := &provisionEndpoint{bootstrap: bootstrap, initial: loaded, values: map[string]string{}, readAddresses: readAddresses}
	var addresses []hostruntime.OwnerIPv4
	var notes []string
	observed := false
	for _, contract := range cli.resources.ProxyContracts(strings.Fields(loaded.Environment["ENVIRONMENT_PROFILES"])) {
		if contract.AddressPolicy != resource.ProxyAddressOwnerIPv4UDP || !slices.Contains(profiles, contract.Profile) {
			continue
		}
		address, iface := loaded.Environment[contract.AdvertiseHostSetting], loaded.Environment[contract.OwnerInterfaceSetting]
		if address != "" && iface != "" {
			continue
		}
		if !observed {
			var err error
			addresses, err = readAddresses()
			if err != nil {
				return nil, nil, fmt.Errorf("inspect owner IPv4 addresses: %w", err)
			}
			observed = true
		}
		candidate, ok := selectProvisionEndpoint(addresses, address, iface)
		if !ok {
			notes = append(notes, fmt.Sprintf("%s endpoint needs an explicit choice: set %s and %s in yard settings before enabling it (no unique public owner IPv4/interface)", contract.Resource, contract.AdvertiseHostSetting, contract.OwnerInterfaceSetting))
			continue
		}
		if address == "" {
			plan.values[contract.AdvertiseHostSetting] = candidate.Address.String()
		}
		if iface == "" {
			plan.values[contract.OwnerInterfaceSetting] = candidate.Interface
		}
		plan.contracts = append(plan.contracts, contract)
	}
	if len(plan.values) == 0 {
		return nil, notes, nil
	}
	var err error
	plan.path, err = configScalarAuthoringPath(loaded, config.ScopeYard)
	if err != nil {
		return nil, nil, err
	}
	plan.before, err = readInitSelectionSnapshot(loaded.Context.Paths.ConfigHome, plan.path)
	if err != nil {
		return nil, nil, err
	}
	if !plan.before.Exists && bootstrap == nil {
		return nil, nil, errors.New("initialize the named yard before automatically configuring its owner endpoint")
	}
	plan.content = plan.before.Content
	if bootstrap != nil {
		plan.content = bootstrap.content
	}
	for _, name := range slices.Sorted(maps.Keys(plan.values)) {
		value := plan.values[name]
		if err := config.ValidateSetting(config.ScopeYard, name, value, false); err != nil {
			return nil, nil, err
		}
		plan.content, err = config.EditPersistentAssignmentContent(plan.path, plan.content, name, &value)
		if err != nil {
			return nil, nil, err
		}
	}
	if err := plan.checkSource(); err != nil {
		return nil, nil, err
	}
	return plan, notes, nil
}

func (plan *provisionEndpoint) checkSource() error {
	_, err := os.Lstat(filepath.Join(plan.initial.Context.Paths.ConfigHome, config.SourceRecordRelativePath))
	if err == nil {
		return errors.New("configuration is source-managed; set the resource endpoint through the registered source")
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (plan *provisionEndpoint) check(ctx context.Context, cli *CLI) error {
	return plan.checkWithInstance(ctx, cli, true)
}

func (plan *provisionEndpoint) checkWithInstance(ctx context.Context, cli *CLI, inspectInstance bool) error {
	if plan == nil {
		return nil
	}
	if err := plan.checkSource(); err != nil {
		return err
	}
	options := config.LoadOptions{RepositoryRoot: cli.options.RepositoryRoot, OperatorHome: plan.initial.Context.Paths.OperatorHome, YardName: plan.initial.Context.YardName, Environment: cli.baseEnv}
	if plan.bootstrap != nil {
		options.YardSettingsFile = plan.bootstrap.sourcePath
	}
	fresh, err := config.Load(options)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(fresh.Context, plan.initial.Context) {
		return domain.ErrPlanStale
	}
	for name, trace := range plan.initial.Settings {
		if fresh.Settings[name].EffectiveValue != trace.EffectiveValue {
			return domain.ErrPlanStale
		}
	}
	current, err := readInitSelectionSnapshot(plan.initial.Context.Paths.ConfigHome, plan.path)
	if err != nil {
		return err
	}
	if !sameConfigAuthoringSnapshot(current, plan.before) {
		return domain.ErrPlanStale
	}
	addresses, err := plan.readAddresses()
	if err != nil {
		return err
	}
	var instance ports.InstanceInfo
	if inspectInstance && plan.bootstrap == nil {
		incus, _ := cli.statusPorts()
		instance, err = incus.Instance(ctx, fresh.Context.IncusProject, fresh.Context.YardInstanceName)
		if err != nil && !errors.Is(err, ports.ErrInstanceNotFound) {
			return err
		}
	}
	for _, contract := range plan.contracts {
		candidate, ok := selectProvisionEndpoint(addresses, fresh.Environment[contract.AdvertiseHostSetting], fresh.Environment[contract.OwnerInterfaceSetting])
		if !ok || (plan.values[contract.AdvertiseHostSetting] != "" && plan.values[contract.AdvertiseHostSetting] != candidate.Address.String()) || (plan.values[contract.OwnerInterfaceSetting] != "" && plan.values[contract.OwnerInterfaceSetting] != candidate.Interface) {
			return domain.ErrPlanStale
		}
		if instance.Config[contract.OwnershipKey()] != "" || instance.LocalConfig[contract.OwnershipKey()] != "" || instance.Devices[contract.Device] != nil || instance.LocalDevices[contract.Device] != nil {
			return fmt.Errorf("stop resource %s before configuring its owner endpoint", contract.Resource)
		}
	}
	return nil
}

func (plan *provisionEndpoint) consequences() []string {
	if plan == nil {
		return nil
	}
	var result []string
	for _, name := range slices.Sorted(maps.Keys(plan.values)) {
		result = append(result, fmt.Sprintf("save %s=%s in yard settings (service enablement unchanged)", name, plan.values[name]))
	}
	return result
}

func (plan *provisionEndpoint) apply(ctx context.Context, cli *CLI) error {
	if plan == nil {
		return nil
	}
	unlock, err := lockIntegrationYard(ctx, plan.initial)
	if err != nil {
		return err
	}
	defer unlock()
	if err := plan.check(ctx, cli); err != nil {
		return err
	}
	return config.CompareAndSwapPersistentFile(plan.initial.Context.Paths.ConfigHome, plan.path, plan.before, plan.content)
}

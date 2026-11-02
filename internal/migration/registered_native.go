package migration

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/adapters/securityruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerpublication"
	"github.com/Subyard/Subyard/internal/ownerroute"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/resource"
)

// InspectRegisteredYardMigration recognizes only the selected profiles' exact
// native resources. The returned projection is an ownership assessment, never a
// substitute for the subsequent ordinary desired-state reconciliation.
func inspectRegisteredYardMigration(ctx context.Context, runtime reconcileruntime.Runtime, contracts []resource.ProxyContract) (ports.ReconcileState, []ports.TeardownResource, map[string]string, map[string]string, map[string]string, error) {
	state, err := runtime.Incus.ReconcileState(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, runtime.SRVPool, runtime.SRVVolume, runtime.Yard.IncusBridge)
	if err != nil {
		return state, nil, nil, nil, nil, err
	}
	original := state.Instance
	state.Instance.Config = maps.Clone(original.Config)
	state.Instance.LocalConfig = maps.Clone(original.LocalConfig)
	state.Instance.Devices = maps.Clone(original.Devices)
	state.Instance.LocalDevices = maps.Clone(original.LocalDevices)
	receipts := map[string]string{}
	var refusals []error
	for _, contract := range contracts {
		device, present := original.LocalDevices[contract.Device]
		if !present {
			continue
		}
		var findings []error
		owned := !contract.IsOwnerIPv4Ingress() && contract.OwnershipMetadata && maps.Equal(device, original.Devices[contract.Device])
		if !owned {
			findings = append(findings, errors.New("has unsupported or inherited ownership"))
		}
		key, value := contract.OwnershipKey(), contract.OwnershipValue(device)
		listen, ok := strings.CutPrefix(device["listen"], "tcp:")
		host, port, splitErr := net.SplitHostPort(listen)
		if !ok || splitErr != nil || !(ownerroute.TailscaleAddress(host) || host == "127.0.0.1" && contract.AddressPolicy == resource.ProxyAddressLoopbackOrTailscale) {
			findings = append(findings, errors.New("does not match its declared native proxy recipe"))
		} else if owned {
			proposed := original
			proposed.Config = maps.Clone(original.Config)
			proposed.Config[key] = value
			checker := securityruntime.Runtime{Yard: runtime.Yard, Environment: map[string]string{contract.AdvertiseHostSetting: host, contract.HostPortSetting: port}}
			findings = append(findings, checker.CheckResourceProxy(ctx, contract, proposed))
		}
		if marker := original.LocalConfig[key]; marker != "" && marker != value || original.Config[key] != original.LocalConfig[key] {
			findings = append(findings, errors.New("has foreign or inherited receipt"))
		}
		receipts[key] = value
		delete(state.Instance.LocalDevices, contract.Device)
		delete(state.Instance.Devices, contract.Device)
		delete(state.Instance.LocalConfig, key)
		delete(state.Instance.Config, key)
		deferred := ownerpublication.Prefix + contract.Device
		if original.LocalConfig[deferred] != "" || original.Config[deferred] != "" {
			findings = append(findings, errors.New("has unresolved deferred publication"))
		}
		delete(state.Instance.LocalConfig, deferred)
		delete(state.Instance.Config, deferred)
		if err := domain.CollectRefusals(findings...); err != nil {
			refusals = append(refusals, fmt.Errorf("registered resource %q: %w", contract.Device, err))
		}
	}
	if state.ProjectConfig["user.subyard.installation"] == "1" {
		if state.ProjectConfig["user.subyard.operator_uid"] != strconv.Itoa(os.Getuid()) || state.ProjectConfig["user.subyard.config_home"] != runtime.Yard.Paths.ConfigHome || state.ProjectConfig["user.subyard.data_home"] != runtime.Yard.Paths.DataHome || state.ProjectConfig["user.subyard.adopted_uuid"] != "" {
			refusals = append(refusals, errors.New("registered project owner differs from protected operator identity"))
		}
		state.Instance = original
		return state, nil, nil, state.ProjectConfig, receipts, domain.CollectRefusals(refusals...)
	}
	// Declared publications are observed historical recipes, not today's defaults.
	catalog, err := config.LoadCatalog(runtime.RepositoryRoot)
	if err != nil {
		return state, nil, nil, nil, nil, domain.CollectRefusals(append(refusals, err)...)
	}
	definitions, err := profile.Load(runtime.RepositoryRoot)
	if err != nil {
		return state, nil, nil, nil, nil, domain.CollectRefusals(append(refusals, err)...)
	}
	environment := map[string]string{}
	for _, row := range runtime.Environment {
		key, value, _ := strings.Cut(row, "=")
		environment[key] = value
	}
	environment["YARD_NAME"] = runtime.Yard.YardName
	for _, definition := range definitions {
		projection, err := definition.ObserveInstancePublication(original.LocalConfig, original.Config, environment,
			func(name, value string) error { return catalog.ValidateSetting(config.ScopeYard, name, value, false) })
		if err != nil {
			refusals = append(refusals, fmt.Errorf("profile %s: %w", definition.Name, err))
			// The refused publication still owns its keys; they are not foreign too.
			projection = map[string]string{definition.InstancePublication.SchemaKey: "", definition.InstancePublication.RevisionKey: ""}
		}
		for key := range projection {
			delete(state.Instance.Config, key)
			delete(state.Instance.LocalConfig, key)
		}
	}
	// Limits are ordinary mutable configuration, not a new privilege or path.
	runtime.Environment = slices.Clone(runtime.Environment)
	for key, setting := range map[string]string{"limits.cpu": "LIMITS_CPU", "limits.memory": "LIMITS_MEMORY"} {
		value := original.Config[key]
		if value != "" {
			if err := catalog.ValidateSetting(config.ScopeYard, setting, value, false); err != nil {
				refusals = append(refusals, err)
				// An invalid limit is not imported and not reported again as unconverged.
				value = ""
				delete(state.Instance.Config, key)
				delete(state.Instance.LocalConfig, key)
			}
			for index, row := range runtime.Environment {
				if strings.HasPrefix(row, setting+"=") {
					runtime.Environment[index] = setting + "=" + value
				}
			}
		}
	}
	runtime.Incus = registeredStateReader{IncusReader: runtime.Incus, state: state}
	projected, resources, metadata, before, err := runtime.InspectYardAdoption(ctx)
	projected.Instance = original
	return projected, resources, metadata, before, receipts, domain.CollectRefusals(append(refusals, err)...)
}

type registeredStateReader struct {
	reconcileruntime.IncusReader
	state ports.ReconcileState
}

func (reader registeredStateReader) ReconcileState(context.Context, string, string, string, string, string) (ports.ReconcileState, error) {
	return reader.state, nil
}

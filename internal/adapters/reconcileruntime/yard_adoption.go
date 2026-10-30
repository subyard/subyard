package reconcileruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/state"
)

// InspectYardAdoption shares the configured native capability validators with
// ordinary reconciliation. Adoption itself only publishes project metadata.
func (runtime Runtime) InspectYardAdoption(ctx context.Context) (ports.ReconcileState, []ports.TeardownResource, map[string]string, map[string]string, error) {
	state, err := runtime.reconcileState(ctx)
	if err != nil {
		return state, nil, nil, nil, err
	}
	before := maps.Clone(state.ProjectConfig)
	// Incus initializes these flags true; an unset flag has default false and
	// inherits the default project's namespace instead of proving dedication.
	if before["features.profiles"] != "true" || before["features.storage.volumes"] != "true" {
		return state, nil, nil, nil, errors.New("yard adoption requires explicitly dedicated project profile and volume namespaces")
	}
	asset := config.MaterializedAsset{Source: filepath.Join(runtime.RepositoryRoot, ownerProjectRenderer)}
	helper, err := asset.ReadSource()
	if runtime.SourceReader != nil {
		helper, err = runtime.SourceReader(asset)
	}
	if err != nil {
		return state, nil, nil, nil, err
	}
	identity, err := json.Marshal(runtime.Yard)
	if err != nil {
		return state, nil, nil, nil, err
	}
	runtime.Environment = ownerRenderEnvironment(runtime.Environment, "SUBYARD_INSTALLATION_CONTEXT", string(identity))
	poolName, volumeName := runtime.volumeNames()
	for key, value := range map[string]string{"SRV_POOL": poolName, "SRV_VOLUME": volumeName, "SUBYARD_USER": "", "YARD_INSTANCE_NAME": runtime.Yard.YardInstanceName, "YARD_NAME": runtime.Yard.YardName, "HOST_BASE": runtime.Yard.Paths.HostBase, "STORAGE_PATH": runtime.Yard.Paths.StoragePath, "SUBYARD_HOME": runtime.Yard.Paths.DataHome, "SUBYARD_CONFIG_HOME": runtime.Yard.Paths.ConfigHome} {
		runtime.Environment = ownerRenderEnvironment(runtime.Environment, key, value)
	}
	rendered, err := runtime.renderProjectConfig(ctx, helper)
	if err != nil {
		return state, nil, nil, nil, err
	}
	metadata := map[string]string{}
	for _, row := range bytes.Split(bytes.TrimSuffix(rendered, []byte{0}), []byte{0}) {
		key, value, ok := strings.Cut(string(row), "=")
		if !ok {
			return state, nil, nil, nil, errors.New("invalid project ownership rendering")
		}
		if strings.HasPrefix(key, "user.subyard.") {
			metadata[key] = value
		}
	}
	metadata["user.subyard.installation"] = domain.AdoptedInstallation
	metadata["user.subyard.adopted_uuid"] = state.Instance.Config["volatile.uuid"]
	state.ProjectConfig = maps.Clone(before)
	stamped := before["user.subyard.installation"] == domain.AdoptedInstallation
	for key, value := range before {
		if strings.HasPrefix(key, "user.subyard.") {
			if !stamped || value != metadata[key] {
				return state, nil, nil, nil, errors.New("yard adoption refuses partial, foreign or conflicting project ownership")
			}
			delete(state.ProjectConfig, key)
		}
	}
	if stamped {
		for key, value := range metadata {
			if before[key] != value {
				return state, nil, nil, nil, errors.New("yard adoption refuses incomplete adopted project ownership")
			}
		}
	}
	// These namespaces are not represented by the bounded teardown inventory.
	for key, value := range state.ProjectConfig {
		if strings.HasPrefix(key, "features.") && value != "true" && value != "false" {
			return state, nil, nil, nil, errors.New("yard adoption requires canonical project feature booleans")
		}
		if strings.HasPrefix(key, "features.") && value != "false" && key != "features.profiles" && key != "features.storage.volumes" && key != "features.storage.buckets" && key != "features.images" {
			return state, nil, nil, nil, errors.New("yard adoption refuses an uninventoried project feature namespace")
		}
	}
	pool, _ := runtime.volumeNames()
	root := map[string]string{"type": "disk", "path": "/", "pool": pool}
	if size := runtime.environmentValue("ROOT_DISK_SIZE"); size != "" {
		root["size"] = size
	}
	if !maps.Equal(root, state.ProfileDevices["root"]) || !maps.Equal(map[string]string{"type": "nic", "network": runtime.Yard.IncusBridge}, state.ProfileDevices["eth0"]) {
		return state, nil, nil, nil, errors.New("yard adoption refuses noncanonical project root or NIC properties")
	}
	state, resources, err := runtime.inspectLegacyYard(ctx, state, true)
	if err != nil {
		return state, nil, nil, nil, err
	}
	for _, resource := range resources {
		if resource.SnapshotCount != 0 {
			return state, nil, nil, nil, errors.New("yard adoption refuses unproven snapshots")
		}
	}
	return state, resources, metadata, before, nil
}

// YardAdoptionProjects proves existing workspace devices from the selected
// operator's registry. Callers bind these records into the pre-consent snapshot.
// It never imports guest observations or repairs registry permissions.
func (runtime Runtime) YardAdoptionProjects(ctx context.Context, instance ports.InstanceInfo) ([]domain.ProjectRecord, error) {
	present := false
	for name := range instance.LocalDevices {
		present = present || strings.HasPrefix(name, "ws-")
	}
	if !present {
		return nil, nil
	}
	store, err := state.NewFileStore(runtime.Yard.Paths.StateDir)
	if err != nil {
		return nil, err
	}
	records, err := store.ListReadOnly(ctx)
	if err != nil {
		return nil, fmt.Errorf("yard adoption project registry: %w", err)
	}
	byDevice := map[string]domain.ProjectRecord{}
	for _, record := range records {
		if record.SSHHost != runtime.Yard.SSHHost || record.Mode != domain.ProjectBind {
			continue
		}
		name := state.WorkspaceDeviceFor(record)
		if _, duplicate := byDevice[name]; duplicate {
			return nil, fmt.Errorf("yard adoption project registry has ambiguous device %q", name)
		}
		byDevice[name] = record
	}
	selected := []domain.ProjectRecord{}
	// Preserve registry order so the consent binding is deterministic.
	for _, record := range records {
		name := state.WorkspaceDeviceFor(record)
		device, exists := instance.LocalDevices[name]
		if !exists || byDevice[name].ProjectID != record.ProjectID {
			continue
		}
		if !filepath.IsAbs(record.HostPath) || filepath.Clean(record.HostPath) != record.HostPath {
			return nil, fmt.Errorf("yard adoption refuses noncanonical registered bind source for %q", name)
		}
		desired := map[string]string{"type": "disk", "source": record.HostPath, "path": record.YardPath, "shift": "true"}
		if !maps.Equal(device, desired) || !maps.Equal(instance.Devices[name], desired) {
			return nil, fmt.Errorf("yard adoption refuses changed registered bind device %q", name)
		}
		selected = append(selected, record)
	}
	for name := range instance.LocalDevices {
		if strings.HasPrefix(name, "ws-") {
			if _, registered := byDevice[name]; !registered {
				return nil, fmt.Errorf("yard adoption refuses unregistered local device %q", name)
			}
		}
	}
	return selected, nil
}

func (runtime Runtime) adoptionKindPolicy(state ports.ReconcileState, configuration map[string]string) error {
	if runtime.Yard.YardKind != domain.YardContainer && runtime.Yard.YardKind != domain.YardVM {
		return errors.New("unsupported adoption instance kind")
	}
	if bounded, err := runtime.vmDeviceBoundary(state); err != nil || !bounded {
		return errors.New("yard adoption requires converged configured native device boundaries")
	}
	if runtime.Yard.NestedE2EVMs {
		configuration["security.syscalls.intercept.bpf"] = "true"
		configuration["security.syscalls.intercept.bpf.devices"] = "true"
	}
	for _, key := range []string{"security.syscalls.intercept.bpf", "security.syscalls.intercept.bpf.devices"} {
		if state.Instance.Config[key] != configuration[key] || state.Instance.LocalConfig[key] != configuration[key] {
			return errors.New("yard adoption requires converged nested device privileges")
		}
	}
	if runtime.Yard.YardKind == domain.YardVM {
		if runtime.environmentValue("VM_FREE_PAGE_REPORTING") == "1" {
			if ready, err := vmRawQEMUConverged(state.Instance); err != nil || !ready {
				return errors.New("yard adoption requires converged VM raw policy")
			}
			configuration["raw.qemu.conf"] = vmPageReportingConfig
		}
		configuration["user.subyard.vm_cpu_weight"] = runtime.environmentValue("VM_CPU_WEIGHT")
		if state.Instance.Config["user.subyard.vm_cpu_weight"] != configuration["user.subyard.vm_cpu_weight"] {
			return errors.New("yard adoption requires converged configured VM CPU policy")
		}
		if state.Instance.Config["security.nesting"] != "" {
			return errors.New("yard adoption refuses container privileges on a VM")
		}
		if local := state.Instance.LocalDevices["eth0"]; local != nil {
			copy := maps.Clone(local)
			delete(copy, "ipv4.address")
			delete(copy, "hwaddr")
			if !maps.Equal(copy, state.ProfileDevices["eth0"]) || local["ipv4.address"] == "" {
				return errors.New("yard adoption refuses noncanonical VM NIC override")
			}
		}
	}
	return nil
}

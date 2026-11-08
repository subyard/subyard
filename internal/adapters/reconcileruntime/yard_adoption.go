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

// InspectYardAdoption checks capability configuration keys; ordinary reconciliation
// validates their devices and sources. Adoption itself only publishes project metadata.
func (runtime Runtime) InspectYardAdoption(ctx context.Context) (ports.ReconcileState, []ports.TeardownResource, map[string]string, map[string]string, error) {
	state, err := runtime.reconcileState(ctx)
	if err != nil {
		return state, nil, nil, nil, err
	}
	before := maps.Clone(state.ProjectConfig)
	asset := config.MaterializedAsset{Source: filepath.Join(runtime.RepositoryRoot, ownerProjectRenderer)}
	helper, err := asset.ReadSource()
	if runtime.SourceReader != nil {
		helper, err = runtime.SourceReader(asset)
	}
	if err != nil {
		return state, nil, nil, nil, err
	}
	// Verified asset handles are process-local labels; publish only persisted identity.
	identity, err := json.Marshal(domain.ContinuationContext(runtime.Yard))
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
	var refusals []error
	// Incus initializes these flags true; an unset flag has default false and
	// inherits the default project's namespace instead of proving dedication.
	if before["features.profiles"] != "true" || before["features.storage.volumes"] != "true" {
		refusals = append(refusals, runtime.adoptionRefusal("yard adoption requires explicitly dedicated project profile and volume namespaces"))
	}
	state.ProjectConfig = maps.Clone(before)
	stamped := before["user.subyard.installation"] == domain.AdoptedInstallation
	// A project stamped by an earlier release keeps its label as found: no consumer
	// reads it, and retaining it keeps the ownership binding stable across processes.
	if stamped && adoptedContextLabelsOnly(before[adoptedContextKey], metadata[adoptedContextKey]) {
		metadata[adoptedContextKey] = before[adoptedContextKey]
	}
	for key, value := range before {
		if strings.HasPrefix(key, "user.subyard.") {
			if !stamped || value != metadata[key] {
				refusals = append(refusals, runtime.adoptionRefusal("yard adoption refuses partial, foreign or conflicting project ownership"))
			}
			delete(state.ProjectConfig, key)
		}
	}
	if stamped {
		for key, value := range metadata {
			if before[key] != value {
				refusals = append(refusals, runtime.adoptionRefusal("yard adoption refuses incomplete adopted project ownership"))
			}
		}
	}
	// These namespaces are not represented by the bounded teardown inventory.
	for key, value := range state.ProjectConfig {
		if strings.HasPrefix(key, "features.") && value != "true" && value != "false" {
			refusals = append(refusals, runtime.adoptionRefusal("yard adoption requires canonical project feature booleans"))
		}
		if strings.HasPrefix(key, "features.") && value != "false" && key != "features.profiles" && key != "features.storage.volumes" && key != "features.storage.buckets" && key != "features.images" {
			refusals = append(refusals, runtime.adoptionRefusal("yard adoption refuses an uninventoried project feature namespace"))
		}
	}
	// Only device identity refuses. A pinned address, hardware address or differing
	// size enters management as found; ordinary init owns any later convergence.
	pool, _ := runtime.volumeNames()
	root, nic := state.ProfileDevices["root"], state.ProfileDevices["eth0"]
	var parameters []domain.MigrationParameter
	for _, field := range []struct{ name, installed, desired string }{
		{"root.type", root["type"], "disk"}, {"root.path", root["path"], "/"}, {"root.pool", root["pool"], pool},
		{"eth0.type", nic["type"], "nic"}, {"eth0.network", nic["network"], runtime.Yard.IncusBridge},
	} {
		if field.installed != field.desired {
			parameter := domain.MigrationParameter{Name: field.name, Installed: domain.MigrationDiagnosticValue(field.installed), Desired: domain.MigrationDiagnosticValue(field.desired)}
			parameters = append(parameters, parameter)
		}
	}
	if len(parameters) != 0 {
		refusals = append(refusals, domain.WithMigrationParameters(runtime.adoptionRefusal("yard adoption refuses noncanonical project root or NIC properties"), parameters...))
	}
	// A verified prior publication may be followed by an interrupted ordinary
	// init, which marks the instance uninitialized until finalization. Only the
	// complete matching adopted metadata above authorizes this retry state.
	state, resources, err := runtime.inspectLegacyYard(ctx, state, true, stamped)
	refusals = append(refusals, err)
	for _, resource := range resources {
		if resource.SnapshotCount != 0 {
			refusals = append(refusals, runtime.adoptionRefusal("yard adoption refuses unproven snapshots"))
		}
	}
	if err := domain.CollectRefusals(refusals...); err != nil {
		return state, nil, nil, nil, err
	}
	return state, resources, metadata, before, nil
}

const adoptedContextKey = "user.subyard.context"

// adoptedContextLabelsOnly accepts exactly a canonical stamped context whose only
// differences from the rendered one are the released asset path roles.
func adoptedContextLabelsOnly(stamped, rendered string) bool {
	decoder := json.NewDecoder(strings.NewReader(stamped))
	decoder.DisallowUnknownFields()
	var value domain.Context
	if err := decoder.Decode(&value); err != nil || decoder.More() {
		return false
	}
	canonical, err := json.Marshal(value)
	if err != nil || string(canonical) != stamped {
		return false
	}
	normalized, err := json.Marshal(domain.ContinuationContext(value))
	return err == nil && string(normalized) == rendered
}

// YardAdoptionProjects selects the existing workspace devices the selected
// operator's registry proves. Callers bind these records into the pre-consent
// snapshot. Other ws-* devices are retained without gaining project ownership.
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
	var refusals []error
	byDevice := map[string]domain.ProjectRecord{}
	for _, record := range records {
		if record.SSHHost != runtime.Yard.SSHHost || record.Mode != domain.ProjectBind {
			continue
		}
		name := state.WorkspaceDeviceFor(record)
		if _, duplicate := byDevice[name]; duplicate {
			refusals = append(refusals, domain.MigrationStateRefusal(runtime.Yard.YardName, fmt.Errorf("yard adoption project registry has ambiguous device %q", name), "yard adoption project registry has ambiguous device identity"))
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
			refusals = append(refusals, domain.MigrationStateRefusal(runtime.Yard.YardName, fmt.Errorf("yard adoption refuses noncanonical registered bind source for %q", name), "yard adoption refuses noncanonical registered bind source"))
			continue
		}
		desired := map[string]string{"type": "disk", "source": record.HostPath, "path": record.YardPath, "shift": "true"}
		if maps.Equal(device, desired) && maps.Equal(instance.Devices[name], desired) {
			selected = append(selected, record)
		}
	}
	return selected, domain.CollectRefusals(refusals...)
}

func (runtime Runtime) adoptionKindPolicy(state ports.ReconcileState, configuration map[string]string) error {
	if runtime.Yard.YardKind != domain.YardContainer && runtime.Yard.YardKind != domain.YardVM {
		return runtime.adoptionRefusal("unsupported adoption instance kind")
	}
	var refusals []error
	// Only a restricted role still forbids devices; its profile size is checked by the caller.
	if runtime.environmentValue("ALLOWS_HOST_ACCESS") == "false" {
		if _, err := runtime.vmDeviceBoundary(state); err != nil {
			refusals = append(refusals, domain.MigrationStateRefusal(runtime.Yard.YardName, fmt.Errorf("yard adoption requires converged configured native device boundaries: %w", err), "yard adoption requires converged configured native device boundaries"))
		}
	}
	if runtime.Yard.NestedE2EVMs {
		configuration["security.syscalls.intercept.bpf"] = "true"
		configuration["security.syscalls.intercept.bpf.devices"] = "true"
	}
	var parameters []domain.MigrationParameter
	for _, key := range []string{"security.syscalls.intercept.bpf", "security.syscalls.intercept.bpf.devices"} {
		for _, observed := range []struct{ label, value string }{{key, state.Instance.Config[key]}, {key + ".local", state.Instance.LocalConfig[key]}} {
			if observed.value != configuration[key] {
				parameter := domain.MigrationParameter{Name: observed.label, Installed: domain.MigrationDiagnosticValue(observed.value), Desired: domain.MigrationDiagnosticValue(configuration[key])}
				parameters = append(parameters, parameter)
			}
		}
	}
	if len(parameters) != 0 {
		refusals = append(refusals, domain.WithMigrationParameters(runtime.adoptionRefusal("yard adoption requires converged nested device privileges"), parameters...))
	}
	if runtime.Yard.YardKind == domain.YardVM {
		if runtime.environmentValue("VM_FREE_PAGE_REPORTING") == "1" {
			if ready, err := vmRawQEMUConverged(state.Instance); err != nil || !ready {
				refusals = append(refusals, runtime.adoptionRefusal("yard adoption requires converged VM raw policy"))
			}
			configuration["raw.qemu.conf"] = vmPageReportingConfig
		}
		configuration["user.subyard.vm_cpu_weight"] = runtime.environmentValue("VM_CPU_WEIGHT")
		if state.Instance.Config["user.subyard.vm_cpu_weight"] != configuration["user.subyard.vm_cpu_weight"] {
			reason := "yard adoption requires converged configured VM CPU policy"
			parameter := domain.MigrationParameter{Name: "VM_CPU_WEIGHT", Installed: domain.MigrationDiagnosticValue(state.Instance.Config["user.subyard.vm_cpu_weight"]), Desired: domain.MigrationDiagnosticValue(configuration["user.subyard.vm_cpu_weight"])}
			refusals = append(refusals, domain.WithMigrationParameters(runtime.adoptionRefusal(reason), parameter))
		}
		if state.Instance.Config["security.nesting"] != "" {
			refusals = append(refusals, runtime.adoptionRefusal("yard adoption refuses container privileges on a VM"))
		}
	}
	return domain.CollectRefusals(refusals...)
}

func (runtime Runtime) adoptionRefusal(reason string) error {
	return domain.MigrationStateRefusal(runtime.Yard.YardName, errors.New(reason), reason)
}

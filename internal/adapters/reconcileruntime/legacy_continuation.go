package reconcileruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/testvmsruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerpublication"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/previewroute"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

// InspectLegacyContinuation observes a bounded dedicated legacy yard without
// changing project policy or manufacturing installation ownership markers.
func (runtime Runtime) InspectLegacyContinuation(ctx context.Context) (ports.ReconcileState, []ports.TeardownResource, error) {
	state, err := runtime.reconcileState(ctx)
	if err != nil {
		return state, nil, err
	}
	return runtime.inspectLegacyYard(ctx, state, false, false)
}

func (runtime Runtime) inspectLegacyYard(ctx context.Context, state ports.ReconcileState, adopt, registered bool) (ports.ReconcileState, []ports.TeardownResource, error) {
	if runtime.Yard.AccessKind != domain.AccessLocal || runtime.Yard.IncusProject == "default" ||
		!state.ProjectFound || !state.ProfileFound || !state.InstanceFound || !state.VolumeFound {
		return state, nil, runtime.adoptionRefusal("legacy continuation requires an existing dedicated local project, profile, instance and volume")
	}
	var refusals []error
	for _, key := range []string{"installation", "operator_uid", "data_home", "config_home", "context", "yard_name", "instance_name", "host_base", "srv_pool", "srv_volume", "storage_pool", "storage_path", "bridge"} {
		if _, present := state.ProjectConfig["user.subyard."+key]; present {
			refusals = append(refusals, runtime.adoptionRefusal("legacy continuation refuses partial or conflicting project ownership"))
		}
	}

	// Unknown permission keys cannot be approved merely because they exist.
	allowed := map[string]bool{"restricted": true, "restricted.containers.nesting": true, "restricted.containers.privilege": true, "restricted.containers.interception": true, "restricted.devices.disk": true, "restricted.devices.disk.paths": true, "restricted.devices.unix-char": true, "restricted.devices.proxy": true, "restricted.networks.access": true}
	if runtime.environmentValue("VM_FREE_PAGE_REPORTING") == "1" || runtime.Yard.YardKind == domain.YardVM {
		allowed["restricted.virtual-machines.lowlevel"] = true
	}
	for key := range state.ProjectConfig {
		if (key == "restricted" || strings.HasPrefix(key, "restricted.")) && !allowed[key] {
			refusals = append(refusals, runtime.adoptionRefusal("legacy continuation refuses unknown project permissions"))
		}
	}
	if state.ProjectConfig["features.profiles"] == "false" || state.ProjectConfig["features.storage.volumes"] == "false" {
		refusals = append(refusals, runtime.adoptionRefusal("legacy continuation requires dedicated project profiles and volumes"))
	}
	// The supported historical default profile has no privileged config. Do not
	// infer that arbitrary profile config was authorized by its resource name.
	if len(state.ProfileConfig) != 0 {
		refusals = append(refusals, runtime.adoptionRefusal("legacy continuation refuses unsupported default-profile configuration"))
	}

	instance := state.Instance
	var identityDifferences, identityFields []string
	var identityParameters []domain.MigrationParameter
	for _, field := range []struct{ name, observed, required string }{
		{"project", instance.Project, runtime.Yard.IncusProject},
		{"instance", instance.Name, runtime.Yard.YardInstanceName},
		{"type", string(instance.Type), string(runtime.Yard.YardKind)},
		{"user.subyard.managed", instance.Config["user.subyard.managed"], "true"},
		{"user.subyard.name", instance.Config["user.subyard.name"], runtime.Yard.YardName},
		{"user.subyard.bridge", instance.Config["user.subyard.bridge"], runtime.Yard.IncusBridge},
		{"boot.autostart", instance.Config["boot.autostart"], "false"},
	} {
		if field.observed != field.required {
			identityFields = append(identityFields, field.name)
			identityDifferences = append(identityDifferences, field.name+": observed="+legacyIdentityValue(field.observed)+", required="+legacyIdentityValue(field.required))
			parameter := domain.MigrationParameter{Name: field.name, Installed: domain.MigrationDiagnosticValue(field.observed), Desired: domain.MigrationDiagnosticValue(field.required)}
			identityParameters = append(identityParameters, parameter)
		}
	}
	if instance.Config["volatile.uuid"] == "" {
		identityFields = append(identityFields, "volatile.uuid")
		identityDifferences = append(identityDifferences, "volatile.uuid: observed empty, required nonempty")
	}
	if runtime.LegacyContinuation == nil && !registered && instance.Config["user.subyard.initialized"] != "true" {
		identityFields = append(identityFields, "user.subyard.initialized")
		identityDifferences = append(identityDifferences, "user.subyard.initialized: observed="+legacyIdentityValue(instance.Config["user.subyard.initialized"])+", required=true")
	}
	if power := instance.Config["user.subyard.desired_power"]; power != "running" && power != "stopped" {
		identityFields = append(identityFields, "user.subyard.desired_power")
		identityDifferences = append(identityDifferences, "user.subyard.desired_power: observed="+legacyIdentityValue(power)+", required running or stopped")
	}
	if len(identityDifferences) != 0 {
		refusals = append(refusals, domain.WithMigrationParameters(domain.MigrationStateRefusal(runtime.Yard.YardName, config.OwnerMetadataError("", "legacy continuation native identity", "requires a complete matching managed native identity and UUID", strings.Join(identityDifferences, "; "), nil), "legacy continuation requires a complete matching managed native identity and UUID", identityFields...), identityParameters...))
	}

	// Policy findings about a different instance or kind would be meaningless.
	if instance.Project == runtime.Yard.IncusProject && instance.Name == runtime.Yard.YardInstanceName && instance.Type == runtime.Yard.YardKind {
		refusals = append(refusals, runtime.legacyInstancePolicy(ctx, state, adopt))
	}
	reporting, err := runtime.vmPageReporting()
	refusals = append(refusals, err)
	pool, volume := runtime.volumeNames()
	if err == nil && (len(state.ProfileDevices) != 2 || state.ProfileDevices["root"]["type"] != "disk" || state.ProfileDevices["root"]["path"] != "/" || state.ProfileDevices["root"]["pool"] != pool || state.ProfileDevices["root"]["source"] != "" || state.ProfileDevices["eth0"]["parent"] != "" || state.ProfileDevices["eth0"]["type"] != "nic" || state.ProfileDevices["eth0"]["network"] != runtime.Yard.IncusBridge || !runtime.projectPolicyConverged(state, reporting)) {
		refusals = append(refusals, runtime.adoptionRefusal("legacy project policy or profile is not already converged; continuation cannot create or repair project resources"))
	}
	refusals = append(refusals, runtime.legacyNetworkPolicy(ctx))

	resources, err := runtime.Incus.TeardownInventory(ctx, runtime.Yard.IncusProject)
	if err != nil {
		return state, nil, domain.CollectRefusals(append(refusals, err)...)
	}
	expected := map[string]bool{"project//" + runtime.Yard.IncusProject: false, "profile//default": false, "instance//" + runtime.Yard.YardInstanceName: false, "volume/" + pool + "/" + volume: false}
	for _, entry := range resources {
		key := entry.Kind + "/" + entry.Pool + "/" + entry.Name
		// An isolated project may retain the exact native root image cache.
		// Association with the instance proves this one image, never neighbors.
		if adopt && state.ProjectConfig["features.images"] == "true" && entry.Kind == "image" && entry.Pool == "" && entry.Name == instance.Config["volatile.base_image"] && len(entry.Name) == 64 {
			if _, err := hex.DecodeString(entry.Name); err == nil {
				if _, exists := expected[key]; !exists {
					expected[key] = false
				}
			}
		}
		seen, ok := expected[key]
		if !ok || seen || entry.Binding == "" {
			refusals = append(refusals, domain.MigrationStateRefusal(runtime.Yard.YardName, fmt.Errorf("legacy continuation refuses unknown or incomplete native inventory: %q", key), "legacy continuation refuses unknown or incomplete native inventory"))
			continue
		}
		expected[key] = true
	}
	for key, seen := range expected {
		if !seen {
			refusals = append(refusals, domain.MigrationStateRefusal(runtime.Yard.YardName, fmt.Errorf("legacy continuation native inventory is incomplete: %q", key), "legacy continuation native inventory is incomplete"))
		}
	}
	return state, resources, domain.CollectRefusals(refusals...)
}

func (runtime Runtime) legacyNetworkPolicy(ctx context.Context) error {
	planner, ok := runtime.NetworkPolicy.(interface {
		Prepare(context.Context, []yardnetwork.Yard, yardnetwork.Change) (yardnetwork.Plan, error)
	})
	if !ok {
		return errors.New("legacy continuation requires read-only native network policy assessment")
	}
	if service, ok := runtime.NetworkPolicy.(*yardnetwork.Service); ok && service == nil {
		return errors.New("legacy continuation requires native network policy assessment")
	}
	network, err := planner.Prepare(ctx, []yardnetwork.Yard{runtime.networkPolicyYard()}, yardnetwork.Change{})
	if err != nil {
		return err
	}
	if network.Physical && len(network.Updates) == 0 {
		return errors.New("legacy network policy observation is incomplete")
	}
	for _, update := range network.Updates {
		if update.Yard.Project == runtime.Yard.IncusProject {
			return runtime.adoptionRefusal("legacy project network policy must already be converged; run yard network reconcile for its separate assessed operation before continuing")
		}
	}
	return nil
}

func LegacyContinuationResources(resources []ports.TeardownResource) []string {
	result := make([]string, 0, len(resources))
	for _, entry := range resources {
		result = append(result, entry.Kind+"/"+entry.Pool+"/"+entry.Name)
	}
	sort.Strings(result)
	return result
}

func (runtime Runtime) checkLegacyContinuation(ctx context.Context) error {
	grant := runtime.LegacyContinuation
	if grant == nil {
		return nil
	}
	state, resources, err := runtime.InspectLegacyContinuation(ctx)
	if err != nil {
		return fmt.Errorf("%w: legacy continuation native identity is no longer eligible: %w", domain.ErrPlanStale, err)
	}
	binding, err := runtime.LegacyContinuationSecurityBinding(state)
	if err != nil {
		return fmt.Errorf("%w: native publication policy changed: %w", domain.ErrPlanStale, err)
	}
	var differences []string
	if grant.Scope != "normal-reconcile" {
		differences = append(differences, "scope: stored="+legacyIdentityValue(grant.Scope)+", required=normal-reconcile")
	}
	if current := domain.ContinuationContext(runtime.Yard); grant.Context != current {
		differences = append(differences, "context changed; configHome: stored="+config.QuoteMetadataPath(grant.Context.Paths.ConfigHome)+", current="+config.QuoteMetadataPath(current.Paths.ConfigHome)+"; dataHome: stored="+config.QuoteMetadataPath(grant.Context.Paths.DataHome)+", current="+config.QuoteMetadataPath(current.Paths.DataHome))
	}
	if current := state.Instance.Config["volatile.uuid"]; grant.InstanceUUID != current {
		differences = append(differences, "instanceUuid: stored="+legacyIdentityValue(grant.InstanceUUID)+", current="+legacyIdentityValue(current))
	}
	if grant.SecurityPolicyBinding != binding {
		differences = append(differences, "securityPolicyBinding: stored and current protected policy differ")
	}
	if current := LegacyContinuationResources(resources); !reflect.DeepEqual(grant.Resources, current) {
		differences = append(differences, fmt.Sprintf("resources: stored and current inventory differ (stored count=%d, current count=%d)", len(grant.Resources), len(current)))
	}
	if len(differences) != 0 {
		return config.OwnerMetadataError("", "legacy continuation", "legacy continuation identity changed", strings.Join(differences, "; "), domain.ErrPlanStale)
	}
	return nil
}

// LegacyContinuationSecurityBinding covers stable native policy, not power,
// observed addresses, volatile runtime state, release links or history bytes.
func (runtime Runtime) LegacyContinuationSecurityBinding(state ports.ReconcileState) (string, error) {
	publication, err := runtime.legacyPublicationPolicy(state.Instance)
	if err != nil {
		return "", err
	}
	effectiveConfig, localConfig := legacyStableConfig(state.Instance.Config), legacyStableConfig(state.Instance.LocalConfig)
	for key, value := range publication {
		effectiveConfig[key] = value
		localConfig[key] = value
	}
	project := maps.Clone(state.ProjectConfig)
	if project == nil {
		project = map[string]string{}
	}
	profile := maps.Clone(state.ProfileConfig)
	if profile == nil {
		profile = map[string]string{}
	}
	payload, _ := json.Marshal(struct {
		Project, Profile               map[string]string
		Devices                        map[string]map[string]string
		Config, LocalConfig            map[string]string
		EffectiveDevices, LocalDevices map[string]map[string]string
	}{project, profile, state.ProfileDevices, effectiveConfig, localConfig, legacyStableDevices(state.Instance.Devices), legacyStableDevices(state.Instance.LocalDevices)})
	return fmt.Sprintf("%x", sha256.Sum256(payload)), nil
}

// Continuation admits only the historical default container shape; the opt-in is
// not authority to approve arbitrary devices or raw instance privileges. Adoption
// cannot be rolled back, so it takes existing devices as found and ordinary init
// owns convergence. Here only srv refuses: reattaching it would change persistent
// data. The host access boundary, resource receipts and consistency check also apply.
func (runtime Runtime) legacyInstancePolicy(ctx context.Context, state ports.ReconcileState, adopt bool) error {
	instance := state.Instance
	var refusals []error
	if !adopt && (runtime.Yard.YardKind != domain.YardContainer || runtime.environmentValue("NESTED_E2E_VMS") == "1" || runtime.environmentValue("ALLOWS_HOST_ACCESS") == "false") {
		refusals = append(refusals, runtime.adoptionRefusal("legacy continuation supports only the canonical non-nested container policy"))
	}
	extras, extrasErr := runtime.extrasContext()
	refusals = append(refusals, extrasErr)
	extraConfig := map[string]string{}
	var extraDevices map[string]map[string]string
	if adopt {
		if extrasErr == nil {
			extraConfig = extrasCapabilityConfiguration(extras)
			var parameters []domain.MigrationParameter
			for key, value := range extraConfig {
				for _, observed := range []struct{ name, value string }{{key, instance.Config[key]}, {key + ".local", instance.LocalConfig[key]}} {
					if observed.value != value {
						parameter := domain.MigrationParameter{Name: observed.name, Installed: domain.MigrationDiagnosticValue(observed.value), Desired: domain.MigrationDiagnosticValue(value)}
						parameters = append(parameters, parameter)
					}
				}
			}
			if len(parameters) != 0 {
				refusals = append(refusals, domain.WithMigrationParameters(runtime.adoptionRefusal("legacy continuation native capability configuration is not converged"), parameters...))
			}
		}
		refusals = append(refusals, runtime.adoptionKindPolicy(state, extraConfig))
	} else if extrasErr == nil {
		extraDevices, extraConfig, extrasErr = runtime.extrasNativePolicy(state, extras, true, nil)
		if extrasErr != nil {
			refusals = append(refusals, fmt.Errorf("legacy continuation native capability assessment: %w", extrasErr))
		}
	}
	hostMemory := runtime.Yard.NestedE2EVMs || extras["SUBYARD_EXTRAS_HOST_MEMORY"] == "1"
	if !adopt && extrasErr == nil {
		refusals = append(refusals, testvmsruntime.HostMemoryPolicy(instance.LocalConfig, instance.LocalDevices, instance.Devices, hostMemory))
	}
	var limitParameters []domain.MigrationParameter
	for key, setting := range map[string]string{"limits.cpu": "LIMITS_CPU", "limits.memory": "LIMITS_MEMORY"} {
		if want := runtime.environmentValue(setting); want != "" && instance.Config[key] != want {
			parameter := domain.MigrationParameter{Name: setting, Installed: domain.MigrationDiagnosticValue(instance.Config[key]), Desired: domain.MigrationDiagnosticValue(want)}
			limitParameters = append(limitParameters, parameter)
		}
	}
	if len(limitParameters) != 0 {
		refusals = append(refusals, domain.WithMigrationParameters(runtime.adoptionRefusal("legacy continuation requires already-converged configured instance limits"), limitParameters...))
	}
	if shift := runtime.environmentDefault("SHIFT_MODE", "shift"); shift != "shift" && shift != "acl" {
		refusals = append(refusals, runtime.adoptionRefusal("legacy continuation requires a supported host mount policy"))
	}
	if !maps.Equal(instance.Config, instance.LocalConfig) {
		failure := runtime.adoptionRefusal("legacy continuation refuses inconsistent local and expanded instance configuration")
		for _, key := range []string{"security.nesting", "security.privileged", "limits.cpu", "limits.memory", "user.subyard.managed", "user.subyard.name", "user.subyard.bridge", "user.subyard.desired_power", "user.subyard.disk_tmp"} {
			if instance.LocalConfig[key] != instance.Config[key] {
				parameter := domain.MigrationParameter{Name: fmt.Sprintf("%s.local", key), Installed: domain.MigrationDiagnosticValue(instance.LocalConfig[key]), Desired: domain.MigrationDiagnosticValue(instance.Config[key])}
				failure = domain.WithMigrationParameters(failure, parameter)
			}
		}
		refusals = append(refusals, failure)
	}
	publication, publicationErr := runtime.legacyPublicationPolicy(instance)
	refusals = append(refusals, publicationErr)
	retiredKeys := map[string]bool{}
	if adopt {
		// The retired integration's own handler removes these after activation.
		for _, definition := range runtime.Integrations {
			if definition.Retired && definition.Lifecycle != nil {
				for _, key := range definition.Lifecycle.InstanceConfigKeys {
					retiredKeys[key] = true
				}
			}
		}
	}
	configurations := []map[string]string{instance.Config, instance.LocalConfig}
	if extrasErr != nil || publicationErr != nil {
		configurations = nil // Without these inputs every allowed key would look foreign.
	}
	for _, values := range configurations {
		for key, value := range values {
			// Volatile state of an accepted device describes that device.
			if adopt && slices.ContainsFunc(slices.Collect(maps.Keys(instance.LocalDevices)), func(name string) bool { return strings.HasPrefix(key, "volatile."+name+".") }) {
				continue
			}
			// Incus records the resolved bus of a profile disk here too.
			if adopt && runtime.Yard.YardKind == domain.YardVM && strings.HasPrefix(key, "volatile.") && strings.HasSuffix(key, ".io.bus") {
				name := strings.TrimSuffix(strings.TrimPrefix(key, "volatile."), ".io.bus")
				if instance.Devices[name]["type"] == "disk" {
					switch value {
					case "9p", "virtiofs", "virtio-scsi", "virtio-blk", "nvme", "usb":
						continue
					}
				}
			}
			if expected, ok := extraConfig[key]; ok && expected != "" && value == expected {
				continue
			}
			if key == "user.subyard.host_memory" && (adopt || hostMemory) {
				continue
			}
			if retiredKeys[key] {
				continue
			}
			switch key {
			case "security.nesting":
				if value == "true" {
					continue
				}
			case "user.subyard.disk_tmp":
				if value == "v1" {
					continue
				}
			case "limits.cpu", "limits.memory":
				setting := map[string]string{"limits.cpu": "LIMITS_CPU", "limits.memory": "LIMITS_MEMORY"}[key]
				if value != "" && value == runtime.environmentValue(setting) {
					continue
				}
			case "volatile.vsock_id", "volatile.vm.definition":
				if adopt && runtime.Yard.YardKind == domain.YardVM {
					continue
				}
			case "volatile.vm.rtc_adjustment", "volatile.vm.rtc_offset":
				if adopt && runtime.Yard.YardKind == domain.YardVM {
					parsed, err := strconv.ParseInt(value, 10, 64)
					if err == nil && strconv.FormatInt(parsed, 10) == value {
						continue
					}
				}
			case "volatile.vm.needs_reset":
				if adopt && runtime.Yard.YardKind == domain.YardVM && value == "true" {
					continue
				}
			case "boot.autostart", "user.subyard.managed", "user.subyard.initialized", "user.subyard.name", "user.subyard.bridge", "user.subyard.desired_power",
				"user.subyard.ccusage_version", "user.subyard.codex_version", "user.subyard.preview_sha256", "user.subyard.preview_endpoint_sha256",
				"volatile.uuid", "volatile.uuid.generation", "volatile.base_image", "volatile.cloud-init.instance-id", "volatile.idmap.base", "volatile.idmap.current", "volatile.idmap.next", "volatile.last_state.idmap", "volatile.apply_template", "volatile.last_state.power", "volatile.last_state.ready", "volatile.eth0.name", "volatile.eth0.hwaddr", "volatile.eth0.host_name", "volatile.eth0.last_state.ip_addresses":
				continue
			case previewroute.Key, ownerpublication.Prefix + previewroute.DeviceName:
				if adopt || runtime.previewRouteConverged(ctx, instance) {
					continue
				}
			default:
				if _, owned := publication[key]; owned {
					continue
				}
				// Image descriptive metadata is generated by Incus, not instance policy.
				if strings.HasPrefix(key, "image.") || strings.HasPrefix(key, retiredIntegrationMarkerPrefix) {
					continue
				}
			}
			refusals = append(refusals, domain.MigrationStateRefusal(runtime.Yard.YardName, fmt.Errorf("legacy continuation refuses unsupported instance configuration key %q", key), "legacy continuation refuses unsupported instance configuration key", key))
		}
	}
	if runtime.Yard.YardKind == domain.YardContainer && instance.Config["security.nesting"] != "true" {
		reason := "legacy continuation requires canonical local container configuration without inherited overrides"
		parameter := domain.MigrationParameter{Name: "security.nesting", Installed: domain.MigrationDiagnosticValue(instance.Config["security.nesting"]), Desired: "true"}
		refusals = append(refusals, domain.WithMigrationParameters(runtime.adoptionRefusal(reason), parameter))
	}
	if runtime.Yard.YardKind == domain.YardContainer {
		if instance.Config["volatile.idmap.base"] != "0" {
			reason := "legacy continuation requires the unprivileged default ID-map base"
			parameter := domain.MigrationParameter{Name: "volatile.idmap.base", Installed: domain.MigrationDiagnosticValue(instance.Config["volatile.idmap.base"]), Desired: "0"}
			refusals = append(refusals, domain.WithMigrationParameters(runtime.adoptionRefusal(reason), parameter))
		}
		current, err := legacyIDMap(instance.Config["volatile.idmap.current"])
		refusals = append(refusals, domain.MigrationStateRefusal(runtime.Yard.YardName, err, "legacy continuation requires canonical unprivileged UID/GID maps"))
		if err == nil {
			next, err := legacyIDMap(instance.Config["volatile.idmap.next"])
			if err != nil || !reflect.DeepEqual(current, next) {
				refusals = append(refusals, runtime.adoptionRefusal("legacy continuation requires matching canonical current and next unprivileged ID maps"))
			}
		}
		if stored := instance.Config["volatile.last_state.idmap"]; stored != "" && stored != "[]" {
			_, err := legacyIDMap(stored)
			refusals = append(refusals, domain.MigrationStateRefusal(runtime.Yard.YardName, err, "legacy continuation requires canonical stored unprivileged UID/GID maps"))
		}
	}
	if initialized := instance.Config["user.subyard.initialized"]; initialized != "true" && initialized != "false" {
		refusals = append(refusals, runtime.adoptionRefusal("legacy continuation refuses invalid lifecycle state"))
	}
	pool, volume := runtime.volumeNames()
	desired := map[string]map[string]string{
		"srv":                {"type": "disk", "pool": pool, "source": volume, "path": "/srv"},
		"subyard-e2e-routes": {"type": "disk", "source": filepath.Join(runtime.Yard.Paths.DataHome, "e2e", "routes"), "path": "/var/lib/subyard/e2e-routes", "readonly": "true"},
		"ssh":                {"type": "proxy", "bind": "host", "listen": "tcp:127.0.0.1:" + strconv.Itoa(runtime.Yard.SSHPort), "connect": "tcp:127.0.0.1:22"},
	}
	if adopt {
		if runtime.environmentValue("SRV_VOLUME_TYPE") == "block" {
			delete(desired["srv"], "path")
			desired["srv"]["io.bus"] = "virtio-scsi"
		}
		if !maps.Equal(instance.LocalDevices["srv"], desired["srv"]) {
			failure := domain.MigrationStateRefusal(runtime.Yard.YardName, fmt.Errorf("yard adoption refuses srv device that is missing or differs from persistent volume %s/%s: reattaching it would change the data mounted in the yard", pool, volume), "yard adoption refuses a missing or different srv device: reattaching it would change persistent data")
			for _, key := range []string{"type", "pool", "source", "path", "io.bus"} {
				if instance.LocalDevices["srv"][key] != desired["srv"][key] {
					installed := domain.MigrationDiagnosticValue(instance.LocalDevices["srv"][key])
					if (key == "pool" || key == "source") && instance.LocalDevices["srv"][key] != "" {
						installed = "differs"
					}
					parameter := domain.MigrationParameter{Name: fmt.Sprintf("srv.%s", key), Installed: installed, Desired: domain.MigrationDiagnosticValue(desired["srv"][key])}
					failure = domain.WithMigrationParameters(failure, parameter)
				}
			}
			refusals = append(refusals, failure)
		}
		_, err := runtime.YardAdoptionProjects(ctx, instance)
		refusals = append(refusals, err)
	} else if extrasErr == nil {
		for name, device := range extraDevices {
			desired[name] = device
		}
		for _, entry := range strings.Fields(runtime.environmentValue("HOST_MOUNTS")) {
			parts := strings.Split(entry, ":")
			if len(parts) != 4 || parts[0] == "" || (parts[2] != "ro" && parts[2] != "rw") {
				refusals = append(refusals, runtime.adoptionRefusal("legacy continuation requires canonical host mount declarations"))
				continue
			}
			device := map[string]string{"type": "disk", "source": filepath.Join(runtime.Yard.Paths.HostBase, parts[0]), "path": parts[1]}
			if parts[2] == "ro" {
				device["readonly"] = "true"
			}
			if runtime.environmentDefault("SHIFT_MODE", "shift") == "shift" {
				device["shift"] = "true"
			}
			desired[parts[0]] = device
		}
		for name, device := range instance.LocalDevices {
			switch name {
			case "subyard-host-memory":
				// The owning host-memory validator checked marker, shape and effective identity.
				continue
			case "subyard-docker-apparmor":
				if runtime.Yard.YardKind != domain.YardContainer || !maps.Equal(device, legacyAppArmorDevice()) {
					refusals = append(refusals, runtime.adoptionRefusal("legacy continuation refuses noncanonical AppArmor compatibility device"))
				}
				continue
			case "kvm":
				// Historical mode omission is supported. The actual GID is checked
				// against the guest once, then remains immutable without guest access.
				copy := maps.Clone(device)
				gid, hasGID := copy["gid"]
				if hasGID {
					value, err := strconv.ParseUint(gid, 10, 32)
					if err != nil || value == 0 || value == 4294967295 || strconv.FormatUint(value, 10) != gid {
						refusals = append(refusals, runtime.adoptionRefusal("legacy continuation refuses unsupported KVM group mapping"))
						continue
					}
					delete(copy, "gid")
				}
				if copy["mode"] == "0660" {
					delete(copy, "mode")
				}
				if !maps.Equal(copy, map[string]string{"type": "unix-char", "source": "/dev/kvm", "path": "/dev/kvm"}) {
					refusals = append(refusals, runtime.adoptionRefusal("legacy continuation refuses noncanonical KVM device"))
					continue
				}
				if runtime.LegacyContinuation == nil {
					if runtime.Executor == nil || !strings.EqualFold(instance.Status, "running") {
						refusals = append(refusals, errors.New("legacy continuation requires read-only guest KVM group verification"))
						continue
					}
					result, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, ports.InstanceExecRequest{Command: []string{"getent", "group", "kvm"}})
					if err != nil {
						refusals = append(refusals, err)
						continue
					}
					parts := strings.Split(strings.TrimSpace(string(result.Stdout)), ":")
					if hasGID {
						if result.ExitCode != 0 || len(parts) != 4 || parts[0] != "kvm" || parts[2] != gid {
							refusals = append(refusals, runtime.adoptionRefusal("legacy continuation refuses an unverified KVM group mapping"))
						}
					} else if result.ExitCode != 2 {
						refusals = append(refusals, runtime.adoptionRefusal("legacy continuation requires an already-converged or verifiably absent guest KVM group"))
					}
				}
				desired[name] = device
			case previewroute.DeviceName:
				if !runtime.previewRouteConverged(ctx, instance) {
					refusals = append(refusals, runtime.adoptionRefusal("legacy continuation refuses a noncanonical preview route"))
				}
				continue
			}
			if !maps.Equal(device, desired[name]) {
				refusals = append(refusals, domain.MigrationStateRefusal(runtime.Yard.YardName, fmt.Errorf("legacy continuation refuses unsupported local device %q", name), "legacy continuation refuses unsupported local device", name))
			}
		}
		for name, device := range desired {
			if !maps.Equal(instance.LocalDevices[name], device) {
				failure := domain.MigrationStateRefusal(runtime.Yard.YardName, fmt.Errorf("legacy continuation requires already-converged managed device %q", name), "legacy continuation requires already-converged managed device", name)
				if domain.SafeName(name) {
					for _, key := range []string{"type", "pool", "source", "path", "bind", "listen", "connect", "readonly", "shift", "io.bus"} {
						want, declared := device[key]
						if !declared {
							continue
						}
						actual := instance.LocalDevices[name][key]
						if actual != want {
							installed := domain.MigrationDiagnosticValue(actual)
							if (key == "pool" || key == "source") && actual != "" {
								installed = "differs"
							}
							parameter := domain.MigrationParameter{Name: fmt.Sprintf("%s.%s", name, key), Installed: installed, Desired: domain.MigrationDiagnosticValue(want)}
							failure = domain.WithMigrationParameters(failure, parameter)
						}
					}
				}
				refusals = append(refusals, failure)
			}
		}
	}
	effective := make(map[string]map[string]string, len(instance.LocalDevices)+len(state.ProfileDevices))
	for name, device := range state.ProfileDevices {
		effective[name] = device
	}
	for name, device := range instance.LocalDevices {
		if _, inherited := state.ProfileDevices[name]; inherited && !adopt {
			refusals = append(refusals, domain.MigrationStateRefusal(runtime.Yard.YardName, fmt.Errorf("legacy continuation refuses local override of profile device %q", name), "legacy continuation refuses local override of profile device"))
		}
		effective[name] = device
	}
	if !reflect.DeepEqual(effective, instance.Devices) {
		failure := runtime.adoptionRefusal("legacy continuation refuses inconsistent expanded native devices")
		for _, name := range []string{"root", "eth0", "srv", "ssh", "subyard-e2e-routes"} {
			for _, key := range []string{"type", "pool", "source", "path", "bind", "listen", "connect", "readonly", "shift", "io.bus"} {
				observed, expected := instance.Devices[name][key], effective[name][key]
				if observed == expected {
					continue
				}
				installed, desired := domain.MigrationDiagnosticValue(observed), domain.MigrationDiagnosticValue(expected)
				if key == "source" || key == "pool" {
					if observed != "" {
						installed = "differs"
					}
					if expected != "" {
						desired = "differs"
					}
				}
				parameter := domain.MigrationParameter{Name: fmt.Sprintf("%s.%s", name, key), Installed: installed, Desired: desired}
				failure = domain.WithMigrationParameters(failure, parameter)
			}
		}
		refusals = append(refusals, failure)
	}
	return domain.CollectRefusals(refusals...)
}

func legacyAppArmorDevice() map[string]string {
	return map[string]string{"type": "disk", "source": "/dev/null", "path": "/sys/module/apparmor/parameters/enabled", "readonly": "true"}
}

func legacyStableConfig(values map[string]string) map[string]string {
	result := maps.Clone(values)
	if result == nil {
		result = map[string]string{}
	}
	// Exact state/publication fields written by the existing power and guest
	// publication owners. ID maps, MACs and every other property remain bound.
	for _, key := range []string{"user.subyard.initialized", "user.subyard.desired_power", "user.subyard.ccusage_version", "user.subyard.preview_sha256", "user.subyard.preview_endpoint_sha256", "volatile.last_state.power", "volatile.last_state.ready", "volatile.eth0.host_name", "volatile.eth0.last_state.ip_addresses", previewroute.Key, ownerpublication.Prefix + previewroute.DeviceName} {
		delete(result, key)
	}
	// Integration apply publishes retirement proof after consent.
	maps.DeleteFunc(result, func(key, _ string) bool { return strings.HasPrefix(key, retiredIntegrationMarkerPrefix) })
	// Base provisioning can publish this canonical policy after consent.
	if result["user.subyard.disk_tmp"] == "v1" {
		delete(result, "user.subyard.disk_tmp")
	}
	return result
}

func legacyStableDevices(values map[string]map[string]string) map[string]map[string]string {
	result := maps.Clone(values)
	if result == nil {
		result = map[string]map[string]string{}
	}
	// Continuation shape-checks these exact managed additions on every observation,
	// including the first normal init after an old release. Adoption accepts them as
	// found; its native digest, not this binding, pins their exact bytes before consent.
	result["subyard-docker-apparmor"] = legacyAppArmorDevice()
	delete(result, previewroute.DeviceName)
	return result
}

// Incus authors these two contiguous unprivileged maps for its default
// non-isolated container. This validates supported shape, not historical creator
// identity or server-global subordinate-ID provenance. Every value stays bound.
func legacyIDMap(value string) ([2]legacyIDMapEntry, error) {
	var result [2]legacyIDMapEntry
	var entries []legacyIDMapEntry
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&entries); err != nil || len(entries) != 2 {
		return result, errors.New("legacy continuation requires canonical unprivileged UID/GID maps")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return result, errors.New("legacy continuation refuses trailing ID-map data")
	}
	seen := map[bool]bool{}
	for _, entry := range entries {
		if entry.IsUID == entry.IsGID || seen[entry.IsUID] || entry.NSID != 0 || entry.HostID < 65536 || entry.MapRange < 65536 || entry.MapRange > 4294967295 || entry.HostID > 4294967295-entry.MapRange {
			return result, errors.New("legacy continuation refuses noncanonical or privileged ID mapping")
		}
		seen[entry.IsUID] = true
		if entry.IsUID {
			result[0] = entry
		} else {
			result[1] = entry
		}
	}
	return result, nil
}

type legacyIDMapEntry struct {
	IsUID    bool  `json:"Isuid"`
	IsGID    bool  `json:"Isgid"`
	HostID   int64 `json:"Hostid"`
	NSID     int64 `json:"Nsid"`
	MapRange int64 `json:"Maprange"`
}

func (runtime Runtime) legacyPublicationPolicy(instance ports.InstanceInfo) (map[string]string, error) {
	definitions, err := runtime.profileDefinitions()
	if err != nil {
		return nil, err
	}
	result := map[string]string{}
	// Owner identity comes from the admitted context, never ambient YARD_NAME.
	environment := runtimeEnvironment(runtime.Environment)
	environment["YARD_NAME"] = runtime.Yard.YardName
	claimed := map[string]bool{}
	for _, definition := range definitions {
		if declaration := definition.InstancePublication; declaration != nil {
			for _, key := range []string{declaration.SchemaKey, declaration.RevisionKey} {
				if claimed[key] {
					return nil, errors.New("native publication ownership collision")
				}
				claimed[key] = true
			}
		}
		projection, err := definition.ProjectInstancePublication(instance.LocalConfig, instance.Config, environment)
		if err != nil {
			return nil, fmt.Errorf("legacy continuation native publication: %w", err)
		}
		for key, value := range projection {
			result[key] = value
		}
	}
	return result, nil
}

func legacyIdentityValue(value string) string {
	if !domain.SafeID(value) {
		return "unavailable (empty or invalid native identifier)"
	}
	return config.QuoteMetadataPath(value)
}

package reconcileruntime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

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
	if runtime.Yard.AccessKind != domain.AccessLocal || runtime.Yard.IncusProject == "default" ||
		!state.ProjectFound || !state.ProfileFound || !state.InstanceFound || !state.VolumeFound {
		return state, nil, errors.New("legacy continuation requires an existing dedicated local project, profile, instance and volume")
	}
	for _, key := range []string{"installation", "operator_uid", "data_home", "config_home", "context", "yard_name", "instance_name", "host_base", "srv_pool", "srv_volume", "storage_pool", "storage_path", "bridge"} {
		if _, present := state.ProjectConfig["user.subyard."+key]; present {
			return state, nil, errors.New("legacy continuation refuses partial or conflicting project ownership")
		}
	}

	// Unknown permission keys cannot be approved merely because they exist.
	allowed := map[string]bool{"restricted": true, "restricted.containers.nesting": true, "restricted.containers.privilege": true, "restricted.containers.interception": true, "restricted.devices.disk": true, "restricted.devices.disk.paths": true, "restricted.devices.unix-char": true, "restricted.devices.proxy": true, "restricted.networks.access": true}
	if runtime.environmentValue("VM_FREE_PAGE_REPORTING") == "1" {
		allowed["restricted.virtual-machines.lowlevel"] = true
	}
	for key := range state.ProjectConfig {
		if (key == "restricted" || strings.HasPrefix(key, "restricted.")) && !allowed[key] {
			return state, nil, errors.New("legacy continuation refuses unknown project permissions")
		}
	}
	if state.ProjectConfig["features.profiles"] == "false" || state.ProjectConfig["features.storage.volumes"] == "false" {
		return state, nil, errors.New("legacy continuation requires dedicated project profiles and volumes")
	}
	// The supported historical default profile has no privileged config. Do not
	// infer that arbitrary profile config was authorized by its resource name.
	if len(state.ProfileConfig) != 0 {
		return state, nil, errors.New("legacy continuation refuses unsupported default-profile configuration")
	}

	instance := state.Instance
	if instance.Project != runtime.Yard.IncusProject || instance.Name != runtime.Yard.YardInstanceName || instance.Type != runtime.Yard.YardKind ||
		instance.Config["volatile.uuid"] == "" || instance.Config["user.subyard.managed"] != "true" ||
		(runtime.LegacyContinuation == nil && instance.Config["user.subyard.initialized"] != "true") || instance.Config["user.subyard.name"] != runtime.Yard.YardName ||
		instance.Config["user.subyard.bridge"] != runtime.Yard.IncusBridge ||
		(instance.Config["user.subyard.desired_power"] != "running" && instance.Config["user.subyard.desired_power"] != "stopped") ||
		instance.Config["boot.autostart"] != "false" {
		return state, nil, errors.New("legacy continuation requires a complete matching managed native identity and UUID")
	}
	if err := runtime.legacyInstancePolicy(ctx, state); err != nil {
		return state, nil, err
	}
	reporting, err := runtime.vmPageReporting(ctx)
	if err != nil {
		return state, nil, err
	}
	pool, volume := runtime.volumeNames()
	if len(state.ProfileDevices) != 2 || state.ProfileDevices["root"]["type"] != "disk" || state.ProfileDevices["root"]["path"] != "/" || state.ProfileDevices["root"]["pool"] != pool || state.ProfileDevices["root"]["source"] != "" || state.ProfileDevices["eth0"]["parent"] != "" || state.ProfileDevices["eth0"]["type"] != "nic" || state.ProfileDevices["eth0"]["network"] != runtime.Yard.IncusBridge || !runtime.projectPolicyConverged(state, reporting) {
		return state, nil, errors.New("legacy project policy or profile is not already converged; continuation cannot create or repair project resources")
	}
	planner, ok := runtime.NetworkPolicy.(interface {
		Prepare(context.Context, []yardnetwork.Yard, yardnetwork.Change) (yardnetwork.Plan, error)
	})
	if !ok {
		return state, nil, errors.New("legacy continuation requires read-only native network policy assessment")
	}
	if service, ok := runtime.NetworkPolicy.(*yardnetwork.Service); ok && service == nil {
		return state, nil, errors.New("legacy continuation requires native network policy assessment")
	}
	network, err := planner.Prepare(ctx, []yardnetwork.Yard{runtime.networkPolicyYard()}, yardnetwork.Change{})
	if err != nil {
		return state, nil, err
	}
	if network.Physical && len(network.Updates) == 0 {
		return state, nil, errors.New("legacy network policy observation is incomplete")
	}
	for _, update := range network.Updates {
		if update.Yard.Project == runtime.Yard.IncusProject {
			return state, nil, errors.New("legacy project network policy must already be converged; run yard network reconcile for its separate assessed operation before continuing")
		}
	}

	resources, err := runtime.Incus.TeardownInventory(ctx, runtime.Yard.IncusProject)
	if err != nil {
		return state, nil, err
	}
	expected := map[string]bool{"project//" + runtime.Yard.IncusProject: false, "profile//default": false, "instance//" + runtime.Yard.YardInstanceName: false, "volume/" + pool + "/" + volume: false}
	for _, entry := range resources {
		key := entry.Kind + "/" + entry.Pool + "/" + entry.Name
		seen, ok := expected[key]
		if !ok || seen || entry.Binding == "" {
			return state, nil, errors.New("legacy continuation refuses unknown or incomplete native inventory")
		}
		expected[key] = true
	}
	for _, seen := range expected {
		if !seen {
			return state, nil, errors.New("legacy continuation native inventory is incomplete")
		}
	}
	return state, resources, nil
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
		return fmt.Errorf("%w: legacy continuation native identity is no longer eligible: %v", domain.ErrPlanStale, err)
	}
	binding, err := runtime.LegacyContinuationSecurityBinding(state)
	if err != nil {
		return fmt.Errorf("%w: native publication policy changed: %v", domain.ErrPlanStale, err)
	}
	if grant.Scope != "normal-reconcile" || grant.Context != domain.ContinuationContext(runtime.Yard) ||
		grant.InstanceUUID != state.Instance.Config["volatile.uuid"] || grant.SecurityPolicyBinding != binding || !reflect.DeepEqual(grant.Resources, LegacyContinuationResources(resources)) {
		return fmt.Errorf("%w: legacy continuation identity changed", domain.ErrPlanStale)
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

// Only the historical default container shape is eligible. The opt-in is not
// authority to approve arbitrary devices or raw instance privileges.
func (runtime Runtime) legacyInstancePolicy(ctx context.Context, state ports.ReconcileState) error {
	instance := state.Instance
	if runtime.Yard.YardKind != domain.YardContainer || runtime.environmentValue("NESTED_E2E_VMS") == "1" || runtime.environmentValue("ALLOWS_HOST_ACCESS") == "false" {
		return errors.New("legacy continuation supports only the canonical non-nested container policy")
	}
	extras, err := runtime.extrasContext()
	if err != nil {
		return err
	}
	if extras["SUBYARD_EXTRAS_MOUNTS"] != "" || extras["SUBYARD_EXTRAS_CAPABILITIES"] != "" || extras["SUBYARD_EXTRAS_DEVICES"] != "" || extras["SUBYARD_EXTRAS_HOST_MEMORY"] != "0" {
		return errors.New("legacy continuation refuses unsupported additional native profile capabilities")
	}
	for key, setting := range map[string]string{"limits.cpu": "LIMITS_CPU", "limits.memory": "LIMITS_MEMORY"} {
		if want := runtime.environmentValue(setting); want != "" && instance.Config[key] != want {
			return errors.New("legacy continuation requires already-converged configured instance limits")
		}
	}
	if shift := runtime.environmentDefault("SHIFT_MODE", "shift"); shift != "shift" && shift != "acl" {
		return errors.New("legacy continuation requires a supported host mount policy")
	}
	if !maps.Equal(instance.Config, instance.LocalConfig) {
		return errors.New("legacy continuation refuses inconsistent local and expanded instance configuration")
	}
	publication, err := runtime.legacyPublicationPolicy(instance)
	if err != nil {
		return err
	}
	for _, values := range []map[string]string{instance.Config, instance.LocalConfig} {
		for key, value := range values {
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
			case "boot.autostart", "user.subyard.managed", "user.subyard.initialized", "user.subyard.name", "user.subyard.bridge", "user.subyard.desired_power",
				"user.subyard.ccusage_version", "user.subyard.codex_version", "user.subyard.preview_sha256", "user.subyard.preview_endpoint_sha256",
				"volatile.uuid", "volatile.uuid.generation", "volatile.base_image", "volatile.cloud-init.instance-id", "volatile.idmap.base", "volatile.idmap.current", "volatile.idmap.next", "volatile.last_state.idmap", "volatile.apply_template", "volatile.last_state.power", "volatile.last_state.ready", "volatile.eth0.name", "volatile.eth0.hwaddr", "volatile.eth0.host_name", "volatile.eth0.last_state.ip_addresses":
				continue
			case previewroute.Key, ownerpublication.Prefix + previewroute.DeviceName:
				if runtime.previewRouteConverged(ctx, instance) {
					continue
				}
			default:
				if _, owned := publication[key]; owned {
					continue
				}
				// Image descriptive metadata is generated by Incus, not instance policy.
				if strings.HasPrefix(key, "image.") {
					continue
				}
			}
			return fmt.Errorf("legacy continuation refuses unsupported instance configuration key %q", key)
		}
	}
	if instance.Config["security.nesting"] != "true" || !maps.Equal(instance.Config, instance.LocalConfig) {
		return errors.New("legacy continuation requires canonical local container configuration without inherited overrides")
	}
	if instance.Config["volatile.idmap.base"] != "0" {
		return errors.New("legacy continuation requires the unprivileged default ID-map base")
	}
	current, err := legacyIDMap(instance.Config["volatile.idmap.current"])
	if err != nil {
		return err
	}
	next, err := legacyIDMap(instance.Config["volatile.idmap.next"])
	if err != nil || !reflect.DeepEqual(current, next) {
		return errors.New("legacy continuation requires matching canonical current and next unprivileged ID maps")
	}
	if stored := instance.Config["volatile.last_state.idmap"]; stored != "" && stored != "[]" {
		if _, err := legacyIDMap(stored); err != nil {
			return err
		}
	}
	if initialized := instance.Config["user.subyard.initialized"]; initialized != "true" && initialized != "false" {
		return errors.New("legacy continuation refuses invalid lifecycle state")
	}
	pool, volume := runtime.volumeNames()
	desired := map[string]map[string]string{
		"srv":                {"type": "disk", "pool": pool, "source": volume, "path": "/srv"},
		"subyard-e2e-routes": {"type": "disk", "source": filepath.Join(runtime.Yard.Paths.DataHome, "e2e", "routes"), "path": "/var/lib/subyard/e2e-routes", "readonly": "true"},
		"ssh":                {"type": "proxy", "bind": "host", "listen": "tcp:127.0.0.1:" + strconv.Itoa(runtime.Yard.SSHPort), "connect": "tcp:127.0.0.1:22"},
	}
	for _, entry := range strings.Fields(runtime.environmentValue("HOST_MOUNTS")) {
		parts := strings.Split(entry, ":")
		if len(parts) != 4 || parts[0] == "" || (parts[2] != "ro" && parts[2] != "rw") {
			return errors.New("legacy continuation requires canonical host mount declarations")
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
		case "subyard-docker-apparmor":
			if !maps.Equal(device, legacyAppArmorDevice()) {
				return errors.New("legacy continuation refuses noncanonical AppArmor compatibility device")
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
					return errors.New("legacy continuation refuses unsupported KVM group mapping")
				}
				delete(copy, "gid")
			}
			if copy["mode"] == "0660" {
				delete(copy, "mode")
			}
			if !maps.Equal(copy, map[string]string{"type": "unix-char", "source": "/dev/kvm", "path": "/dev/kvm"}) {
				return errors.New("legacy continuation refuses noncanonical KVM device")
			}
			if runtime.LegacyContinuation == nil {
				if runtime.Executor == nil || !strings.EqualFold(instance.Status, "running") {
					return errors.New("legacy continuation requires read-only guest KVM group verification")
				}
				result, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, ports.InstanceExecRequest{Command: []string{"getent", "group", "kvm"}})
				parts := strings.Split(strings.TrimSpace(string(result.Stdout)), ":")
				if hasGID {
					if err != nil || result.ExitCode != 0 || len(parts) != 4 || parts[0] != "kvm" || parts[2] != gid {
						return errors.New("legacy continuation refuses an unverified KVM group mapping")
					}
				} else if result.ExitCode != 2 {
					return errors.New("legacy continuation requires an already-converged or verifiably absent guest KVM group")
				}
			}
			desired[name] = device
		case previewroute.DeviceName:
			if !runtime.previewRouteConverged(ctx, instance) {
				return errors.New("legacy continuation refuses a noncanonical preview route")
			}
			continue
		}
		if !maps.Equal(device, desired[name]) {
			return fmt.Errorf("legacy continuation refuses unsupported local device %q", name)
		}
	}
	for name, device := range desired {
		if !maps.Equal(instance.LocalDevices[name], device) {
			return fmt.Errorf("legacy continuation requires already-converged managed device %q", name)
		}
	}
	effective := make(map[string]map[string]string, len(instance.LocalDevices)+len(state.ProfileDevices))
	for name, device := range state.ProfileDevices {
		effective[name] = device
	}
	for name, device := range instance.LocalDevices {
		if _, inherited := state.ProfileDevices[name]; inherited {
			return fmt.Errorf("legacy continuation refuses local override of profile device %q", name)
		}
		effective[name] = device
	}
	if !reflect.DeepEqual(effective, instance.Devices) {
		return errors.New("legacy continuation refuses inconsistent expanded native devices")
	}
	return nil
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
	// These exact current managed additions are independently shape-checked on
	// every observation, including the first normal init after an old release.
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

package cli

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/uninstall"
)

// Legacy workloads predate project ownership labels. Their native route mount
// binds the exact data root; the persisted registration and sealed original
// runtime supply the remaining provenance. Every project resource is checked:
// proving one instance never approves unrelated neighbors or custom volumes.
func (cli *CLI) inspectLegacyFullYardProvenance(ctx context.Context, loaded config.Loaded, installation uninstall.Installation) error {
	if err := releaseruntime.VerifyLegacyUninstallSource(ctx, installation.RuntimeRoot); err != nil {
		return err
	}
	return readLegacyFullRegistration(installation.ConfigHome, loaded.Context.YardName)
}

func readLegacyFullRegistration(configHome, yardName string) error {
	registered := false
	paths := []string{
		filepath.Join(configHome, "yards", yardName, "config.env"),
		filepath.Join(configHome, "yards", yardName+".env"),
	}
	// Historical default yards read the host-wide layer, not yards/default.
	if yardName == "default" {
		paths = append(paths, filepath.Join(configHome, "config.env"))
	}
	for _, path := range paths {
		file, err := config.ReadPersistentFileSnapshot(configHome, path)
		if err != nil {
			return err
		}
		registered = registered || file.Exists
	}
	if !registered {
		return errors.New("legacy yard has no protected local registration")
	}
	return nil
}

func validateLegacyFullYard(loaded config.Loaded, installation uninstall.Installation, snapshot *teardownSnapshot) error {
	state := snapshot.State
	if state.ProjectConfig["features.storage.volumes"] == "false" || state.ProjectConfig["features.profiles"] == "false" {
		return errors.New("legacy project shares unproven storage or profiles")
	}
	if loaded.Context.IncusProject == "default" || !state.ProjectFound || !state.InstanceFound {
		return errors.New("legacy workload has no exact dedicated project and instance")
	}
	for name := range state.ProjectConfig {
		if strings.HasPrefix(name, "user.subyard.") {
			return errors.New("legacy project has conflicting or partial installation ownership")
		}
	}
	instance := state.Instance
	for _, name := range []string{"installation", "operator_uid", "context", "data_home", "config_home"} {
		if _, exists := instance.Config["user.subyard."+name]; exists {
			return errors.New("legacy instance has conflicting or partial installation ownership")
		}
	}
	if instance.Project != loaded.Context.IncusProject || instance.Name != loaded.Context.YardInstanceName ||
		instance.Config["user.subyard.managed"] != "true" || instance.Config["user.subyard.name"] != loaded.Context.YardName ||
		instance.Config["user.subyard.bridge"] != loaded.Context.IncusBridge {
		return errors.New("legacy instance identity does not match its persisted registration")
	}
	route := instance.LocalDevices["subyard-e2e-routes"]
	if route == nil {
		route = instance.Devices["subyard-e2e-routes"]
	}
	if route["type"] != "disk" || route["source"] != filepath.Join(installation.DataHome, "e2e", "routes") ||
		route["path"] != "/var/lib/subyard/e2e-routes" || route["readonly"] != "true" {
		return errors.New("legacy workload has no exact native installation route binding")
	}
	if len(state.ProfileConfig) != 0 || len(state.ProfileDevices) != 2 {
		return errors.New("legacy default profile has unrelated configuration or devices")
	}
	rootPool := firstNonempty(loaded.Environment["ROOT_POOL"], loaded.Environment["SRV_POOL"])
	expected := map[string]map[string]string{
		"root":                    {"type": "disk", "path": "/", "pool": rootPool},
		"eth0":                    {"type": "nic", "network": loaded.Context.IncusBridge},
		"srv":                     {"type": "disk", "path": "/srv", "pool": loaded.Environment["SRV_POOL"], "source": loaded.Environment["SRV_VOLUME"]},
		"subyard-e2e-routes":      {"type": "disk", "source": filepath.Join(installation.DataHome, "e2e", "routes"), "path": "/var/lib/subyard/e2e-routes", "readonly": "true"},
		"kvm":                     {"type": "unix-char", "source": "/dev/kvm", "path": "/dev/kvm"},
		"subyard-docker-apparmor": {"type": "disk", "source": "/dev/null", "path": "/sys/module/apparmor/parameters/enabled", "readonly": "true"},
	}
	if loaded.Context.YardKind == "container" && loaded.Context.SSHPort > 0 && loaded.Context.SSHPort <= 65535 {
		expected["ssh"] = map[string]string{"type": "proxy", "bind": "host", "listen": "tcp:127.0.0.1:" + strconv.Itoa(loaded.Context.SSHPort), "connect": "tcp:127.0.0.1:22"}
	}
	for _, line := range strings.Fields(loaded.Environment["HOST_MOUNTS"]) {
		parts := strings.Split(line, ":")
		if len(parts) != 4 || !strings.HasPrefix(parts[0], "host-") || !filepath.IsAbs(parts[1]) {
			return errors.New("legacy host mount declaration is unsupported")
		}
		device := map[string]string{"type": "disk", "source": filepath.Join(loaded.Context.Paths.HostBase, parts[0]), "path": parts[1]}
		if parts[2] == "ro" {
			device["readonly"] = "true"
		}
		if loaded.Environment["SHIFT_MODE"] == "shift" {
			device["shift"] = "true"
		}
		expected[parts[0]] = device
	}
	if mode := instance.Devices["kvm"]["mode"]; mode != "" {
		if mode != "0660" {
			return errors.New("legacy KVM device has an unexpected mode")
		}
		expected["kvm"]["mode"] = mode
	}
	// Historical provisioning sets this to the guest kvm group's numeric ID.
	// It changes device access only; source/path/type remain exactly /dev/kvm.
	if gid := instance.Devices["kvm"]["gid"]; gid != "" {
		value, err := strconv.ParseUint(gid, 10, 32)
		if err != nil || strconv.FormatUint(value, 10) != gid {
			return errors.New("legacy KVM device has an invalid group identity")
		}
		expected["kvm"]["gid"] = gid
	}
	for name, device := range state.ProfileDevices {
		if !legacyDeviceMatches(device, expected[name]) {
			return errors.New("legacy default profile does not match its persisted devices")
		}
	}
	for name, device := range instance.Devices {
		if !legacyDeviceMatches(device, expected[name]) {
			return errors.New("legacy instance contains an unexpected or changed device")
		}
	}
	if !legacyDeviceMatches(instance.Devices["srv"], expected["srv"]) {
		return errors.New("legacy persistent volume does not match its native attachment")
	}
	project, yard := false, false
	for _, resource := range snapshot.Resources {
		if resource.SnapshotCount != 0 {
			return errors.New("legacy project contains unproven snapshots")
		}
		switch resource.Kind {
		case "pool", "network", "host-profile":
			continue // Existing shared-consumer guards assess these separately.
		case "project":
			if resource.Name != loaded.Context.IncusProject {
				return errors.New("legacy inventory contains another project")
			}
			project = true
		case "instance":
			if resource.Name != instance.Name {
				return errors.New("legacy project contains an unrelated instance")
			}
			yard = true
		case "profile":
			if resource.Name != "default" {
				return errors.New("legacy project contains an unrelated profile")
			}
		case "volume":
			if resource.Name != loaded.Environment["SRV_VOLUME"] || resource.Pool != loaded.Environment["SRV_POOL"] {
				return errors.New("legacy project contains an unrelated custom volume")
			}
		case "image":
			if resource.Name == "" || resource.Name != instance.Config["volatile.base_image"] {
				return errors.New("legacy project contains an unrelated image")
			}
		default:
			return errors.New("legacy project inventory contains an unsupported resource")
		}
	}
	if !project || !yard {
		return errors.New("legacy project inventory is incomplete")
	}
	return nil
}

func legacyDeviceMatches(actual, expected map[string]string) bool {
	if len(expected) == 0 || len(actual) != len(expected) {
		return false
	}
	for name, value := range expected {
		if actual[name] != value {
			return false
		}
	}
	return true
}

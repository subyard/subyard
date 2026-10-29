package reconcileruntime

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/adapters/testvmsruntime"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/uninstall"
)

// extrasNativePolicy is the read-only counterpart of the extras leaf. It
// evaluates declarations against observed native devices, never the reverse.
func (runtime Runtime) extrasNativePolicy(state ports.ReconcileState, extras map[string]string, complete bool, sources *[]any) (map[string]map[string]string, map[string]string, error) {
	devices := map[string]map[string]string{}
	configuration := map[string]string{"security.idmap.size": "", "security.syscalls.intercept.mknod": "", "security.syscalls.intercept.setxattr": ""}
	for _, declaration := range strings.Fields(extras["SUBYARD_EXTRAS_MOUNTS"]) {
		parts := strings.Split(declaration, ":") // extrasContext validated the declaration.
		path := filepath.Join(runtime.Yard.Paths.HostBase, parts[0])
		tree, err := uninstall.CaptureRoot(path)
		if sources != nil {
			*sources = append(*sources, tree)
		}
		mode, _ := strconv.ParseUint(parts[3], 8, 32)
		uid, uidErr := strconv.ParseUint(runtime.environmentDefault("DEV_UID", "1000"), 10, 32)
		if err != nil || uidErr != nil || len(tree.Entries) != 1 || tree.Entries[0].Mode&syscall.S_IFMT != syscall.S_IFDIR || uint64(tree.Entries[0].Mode&07777) != mode {
			return nil, nil, fmt.Errorf("profile mount %q has unverified host directory permissions", parts[0])
		}
		if uint64(tree.Entries[0].UID) != uid || uint64(tree.Entries[0].GID) != uid {
			return nil, nil, fmt.Errorf("profile mount %q has unverified owner identity", parts[0])
		}
		device := map[string]string{"type": "disk", "source": path, "path": parts[1]}
		if parts[2] == "ro" {
			device["readonly"] = "true"
		}
		if runtime.environmentDefault("SHIFT_MODE", "shift") == "shift" {
			device["shift"] = "true"
		}
		devices["yx-"+parts[0]] = device
	}
	root := runtime.HostDeviceRoot
	if root == "" {
		root = "/dev"
	}
	addCharacter := func(name, source string) error {
		path := filepath.Join(root, strings.TrimPrefix(source, "/dev/"))
		info, err := os.Stat(path)
		if sources != nil && (err == nil || os.IsNotExist(err)) {
			var stat any
			if err == nil {
				stat = info.Sys()
			}
			*sources = append(*sources, struct {
				Path string
				Stat any
			}{path, stat})
		}
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeCharDevice == 0 {
			return fmt.Errorf("profile device source %q is not a character device", source)
		}
		device := map[string]string{"type": "unix-char", "source": source, "path": source}
		if state.Instance.LocalDevices[name]["mode"] != "" {
			device["mode"] = "0666"
		}
		devices[name] = device
		return nil
	}
	needFuse := false
	for _, capability := range strings.Fields(extras["SUBYARD_EXTRAS_CAPABILITIES"]) {
		if capability == "nesting" || capability == "rootless-docker" {
			configuration["security.idmap.size"] = "1000000"
			configuration["security.syscalls.intercept.mknod"] = "true"
			configuration["security.syscalls.intercept.setxattr"] = "true"
		}
		needFuse = true
	}
	for _, device := range strings.Fields(extras["SUBYARD_EXTRAS_DEVICES"]) {
		switch device {
		case "kvm":
			if err := addCharacter("yx-dev-kvm", "/dev/kvm"); err != nil {
				return nil, nil, err
			}
		case "fuse":
			needFuse = true
		case "gpu":
			nodes, err := filepath.Glob(filepath.Join(root, "dri/renderD*"))
			if err != nil {
				return nil, nil, err
			}
			for _, node := range nodes {
				if err := addCharacter("yx-dev-dri-"+filepath.Base(node), "/dev/dri/"+filepath.Base(node)); err != nil {
					return nil, nil, err
				}
			}
		}
	}
	if needFuse {
		if err := addCharacter("yx-dev-fuse", "/dev/fuse"); err != nil {
			return nil, nil, err
		}
	}
	for key, value := range configuration {
		if !complete && value == "" {
			continue
		}
		if state.Instance.Config[key] != value || state.Instance.LocalConfig[key] != value {
			return nil, nil, fmt.Errorf("profile capability configuration %q is not converged", key)
		}
	}
	for name, device := range devices {
		if !maps.Equal(state.Instance.LocalDevices[name], device) || !maps.Equal(state.Instance.Devices[name], device) {
			return nil, nil, fmt.Errorf("profile device %q is not converged", name)
		}
	}
	for name := range state.Instance.Devices {
		if complete && strings.HasPrefix(name, "yx-") && devices[name] == nil {
			return nil, nil, fmt.Errorf("unselected profile device %q remains", name)
		}
	}
	return devices, configuration, nil
}

// ProfileProvisionReady uses the same native policy for the requested hook's
// prerequisites. Other selected profiles do not expand this operation's work.
func (runtime Runtime) ProfileProvisionReady(ctx context.Context, name string) (bool, any, error) {
	runtime.Environment = append([]string(nil), runtime.Environment...)
	for index, entry := range runtime.Environment {
		if strings.HasPrefix(entry, "ENVIRONMENT_PROFILES=") {
			runtime.Environment[index] = "ENVIRONMENT_PROFILES=" + name
		}
	}
	extras, err := runtime.extrasContext()
	if err != nil {
		return false, ports.ReconcileState{}, err
	}
	state, err := runtime.reconcileState(ctx)
	if err != nil {
		return false, state, err
	}
	if !state.InstanceFound || state.Instance.Config["user.subyard.initialized"] != "true" {
		return false, state, nil
	}
	observation := struct {
		State   ports.ReconcileState
		Sources []any
	}{State: state}
	if _, _, err := runtime.extrasNativePolicy(state, extras, false, &observation.Sources); err != nil {
		return false, state, nil
	}
	if extras["SUBYARD_EXTRAS_HOST_MEMORY"] == "1" {
		if err := testvmsruntime.HostMemoryPolicy(state.Instance.LocalConfig, state.Instance.LocalDevices, state.Instance.Devices, true); err != nil {
			return false, state, nil
		}
	}
	return true, observation, nil
}

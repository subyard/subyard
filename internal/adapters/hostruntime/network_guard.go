package hostruntime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type NetworkGuard struct {
	Lookup func(string) (string, bool)
	Run    func(context.Context, string, ...string) ([]byte, error)
}

func (guard NetworkGuard) Check(ctx context.Context, bridges []string) error {
	if len(bridges) == 0 {
		return errors.New("host network guard needs at least one managed bridge")
	}
	for _, bridge := range bridges {
		if !safeInterfaceName(bridge) {
			return fmt.Errorf("unsafe managed bridge %q", bridge)
		}
	}
	active, err := guard.networkManagerActive(ctx)
	if err != nil {
		return err
	}
	if active {
		binary, ok := guard.lookup("NetworkManager")
		if !ok {
			return errors.New("NetworkManager is active but its binary is unavailable")
		}
		output, err := guard.run(ctx, binary, "--print-config")
		if err != nil {
			return fmt.Errorf("read NetworkManager effective configuration: %w", err)
		}
		if err := validateNetworkManagerConfig(string(output), bridges); err != nil {
			return err
		}
	}
	return guard.routesSafe(ctx, bridges)
}

func (guard NetworkGuard) networkManagerActive(ctx context.Context) (bool, error) {
	systemctl, systemctlOK := guard.lookup("systemctl")
	_, networkManagerOK := guard.lookup("NetworkManager")
	if !systemctlOK {
		if networkManagerOK {
			return false, errors.New("cannot inspect NetworkManager service state")
		}
		return false, nil
	}
	output, _ := guard.run(ctx, systemctl, "is-active", "NetworkManager")
	switch strings.TrimSpace(string(output)) {
	case "active", "activating", "reloading", "deactivating":
		return true, nil
	case "inactive", "failed", "unknown":
		return false, nil
	default:
		return false, errors.New("cannot inspect NetworkManager service state")
	}
}

func (guard NetworkGuard) routesSafe(ctx context.Context, bridges []string) error {
	ip, ok := guard.lookup("ip")
	if !ok {
		return errors.New("ip is required for the host route guard")
	}
	output, err := guard.run(ctx, ip, "-j", "-4", "route", "show", "default")
	if err != nil {
		return fmt.Errorf("inspect host IPv4 default routes: %w", err)
	}
	var routes []struct {
		Device string `json:"dev"`
	}
	if err := json.Unmarshal(output, &routes); err != nil {
		return fmt.Errorf("parse host IPv4 default routes: %w", err)
	}
	for _, route := range routes {
		if route.Device == "" {
			continue
		}
		if unsafeDeviceName(route.Device, bridges) {
			return fmt.Errorf("unsafe host default route uses %q", route.Device)
		}
		link, err := guard.run(ctx, ip, "-j", "-d", "link", "show", "dev", route.Device)
		if err != nil {
			return fmt.Errorf("inspect route device %q: %w", route.Device, err)
		}
		var links []struct {
			LinkInfo struct {
				Kind string `json:"info_kind"`
			} `json:"linkinfo"`
		}
		if err := json.Unmarshal(link, &links); err != nil {
			return fmt.Errorf("parse route device %q: %w", route.Device, err)
		}
		if len(links) != 0 && links[0].LinkInfo.Kind == "veth" {
			return fmt.Errorf("unsafe host default route uses veth %q", route.Device)
		}
	}
	return nil
}

func (guard NetworkGuard) lookup(name string) (string, bool) {
	if guard.Lookup != nil {
		return guard.Lookup(name)
	}
	candidates := map[string][]string{
		"systemctl":      {"/usr/bin/systemctl", "/bin/systemctl"},
		"NetworkManager": {"/usr/sbin/NetworkManager", "/usr/bin/NetworkManager", "/sbin/NetworkManager"},
		"ip":             {"/usr/sbin/ip", "/usr/bin/ip", "/sbin/ip", "/bin/ip"},
	}
	for _, candidate := range candidates[name] {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate, true
		}
	}
	return "", false
}

func (guard NetworkGuard) run(ctx context.Context, name string, arguments ...string) ([]byte, error) {
	if guard.Run != nil {
		return guard.Run(ctx, name, arguments...)
	}
	return exec.CommandContext(ctx, name, arguments...).CombinedOutput()
}

// validateNetworkManagerConfig checks the effective [keyfile] unmanaged-devices list. Values
// merged through "+=" are comma-joined by NetworkManager, so both separators are accepted.
func validateNetworkManagerConfig(contents string, bridges []string) error {
	section, value := "", ""
	scanner := bufio.NewScanner(strings.NewReader(contents))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "[") {
			section = strings.ReplaceAll(line, " ", "")
			continue
		}
		name, entry, found := strings.Cut(line, "=")
		if found && section == "[keyfile]" && strings.TrimSpace(name) == "unmanaged-devices" {
			value = entry
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	unmanaged := make(map[string]bool)
	for _, entry := range strings.FieldsFunc(value, func(character rune) bool { return character == ';' || character == ',' }) {
		if entry = strings.TrimSpace(entry); entry != "" {
			unmanaged[entry] = true
		}
	}
	var missing []string
	for _, required := range []string{"type:veth", "driver:veth"} {
		if !unmanaged[required] {
			missing = append(missing, required)
		}
	}
	for _, bridge := range bridges {
		if !unmanaged["interface-name:"+bridge] {
			missing = append(missing, "interface-name:"+bridge)
		}
	}
	if len(missing) != 0 {
		return fmt.Errorf("NetworkManager effective [keyfile] unmanaged-devices lacks %s; a NetworkManager conf.d file that sets unmanaged-devices= replaces the Subyard guard list — use unmanaged-devices+= there, then rerun yard init", strings.Join(missing, ","))
	}
	return nil
}

func safeInterfaceName(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("._:-", character) {
			continue
		}
		return false
	}
	return true
}

func unsafeDeviceName(device string, bridges []string) bool {
	for _, bridge := range bridges {
		if device == bridge {
			return true
		}
	}
	for _, prefix := range []string{"veth", "tap", "macvtap", "vnet", "docker", "virbr"} {
		if strings.HasPrefix(device, prefix) {
			return true
		}
	}
	return dockerBridgeName(device)
}

// dockerBridgeName matches Docker's user-defined bridges, "br-" plus 12 network-ID hex
// characters; an admin bridge such as br-lan may carry the host uplink.
func dockerBridgeName(device string) bool {
	suffix, found := strings.CutPrefix(device, "br-")
	if !found || len(suffix) != 12 {
		return false
	}
	for _, character := range suffix {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

// NetworkManagerGuardPath is the Subyard guard drop-in that scripts/lib/host.sh owns.
const NetworkManagerGuardPath = "/etc/NetworkManager/conf.d/zz-subyard-unmanaged.conf"

// NetworkManagerGuardCurrent reports whether no released guard format remains: the guard is
// absent or holds only its appending [keyfile] key, and the legacy 99- file is gone.
func NetworkManagerGuardCurrent(conf string) (bool, error) {
	if _, err := os.Lstat(filepath.Join(filepath.Dir(conf), "99-subyard-unmanaged.conf")); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	contents, err := os.ReadFile(conf)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	sections, keys := 0, 0
	for _, line := range strings.Split(string(contents), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case line == "[keyfile]":
			sections++
		case strings.HasPrefix(line, "unmanaged-devices+=") && sections == 1:
			keys++
		default:
			return false, nil
		}
	}
	return sections == 1 && keys == 1, nil
}

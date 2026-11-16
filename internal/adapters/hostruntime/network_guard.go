package hostruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
)

type NetworkGuard struct {
	Lookup func(string) (string, bool)
	Run    func(context.Context, string, ...string) ([]byte, error)
	// RebootPending reports a migrated guard waiting for a host reboot; nil means none.
	RebootPending func() bool
}

// Check reports every independent refusal: NetworkManager's effective guard, the runtime state
// of existing yard bridges, and host default routes always all run.
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
		return domain.CollectRefusals(err, guard.routesSafe(ctx, bridges))
	}
	var guardErr error
	if active {
		guardErr = guard.networkManagerGuard(ctx, bridges)
	}
	return domain.CollectRefusals(guardErr, guard.routesSafe(ctx, bridges))
}

func (guard NetworkGuard) networkManagerGuard(ctx context.Context, bridges []string) error {
	binary, ok := guard.lookup("NetworkManager")
	if !ok {
		return errors.New("NetworkManager is active but its binary is unavailable")
	}
	output, err := guard.run(ctx, binary, "--print-config")
	if err != nil {
		return fmt.Errorf("read NetworkManager effective configuration: %w", err)
	}
	violations := networkManagerConfigViolations(string(output), bridges)
	nmcli, ok := guard.lookup("nmcli")
	if !ok {
		violations = append(violations, "cannot read NetworkManager's devices with nmcli")
	} else if devices, err := guard.run(ctx, nmcli, "-t", "-f", "DEVICE,STATE", "device"); err != nil || strings.TrimSpace(string(devices)) == "" {
		violations = append(violations, "cannot read NetworkManager's devices with nmcli")
	} else {
		for _, line := range strings.Split(string(devices), "\n") {
			device, state, _ := strings.Cut(line, ":")
			if slices.Contains(bridges, device) && state != "unmanaged" {
				violations = append(violations, "NetworkManager manages yard bridge "+device)
			}
		}
	}
	if len(violations) == 0 {
		return nil
	}
	if guard.RebootPending != nil && guard.RebootPending() {
		return fmt.Errorf("NetworkManager guard is not effective: %s — a migrated NetworkManager guard waits for a host reboot: reboot the host once, then rerun yard start", strings.Join(violations, "; "))
	}
	return fmt.Errorf("NetworkManager guard is not effective: %s — correct the NetworkManager configuration, then rerun yard start", strings.Join(violations, "; "))
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
		"nmcli":          {"/usr/bin/nmcli", "/bin/nmcli"},
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

// networkManagerConfigViolations applies the guard rule shared with scripts/lib-power.sh and
// tests/fixtures/nm-guard-vectors. NetworkManager prints [device*] sections in priority order,
// except the plain [device] section, which it always considers last. [device-subyard] must set
// managed=0 for type:veth, driver:veth and every bridge without except: entries, and no section
// printed before it may set managed=1 or stop-match for devices the guard covers.
func networkManagerConfigViolations(contents string, bridges []string) []string {
	type section struct {
		name   string
		values map[string]string
	}
	var sections []section
	for _, line := range strings.Split(contents, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			sections = append(sections, section{name: strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"), values: map[string]string{}})
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if found && len(sections) != 0 {
			sections[len(sections)-1].values[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	guardIndex := slices.IndexFunc(sections, func(item section) bool { return item.name == "device-subyard" })
	if guardIndex < 0 {
		return []string{"NetworkManager has no [device-subyard] guard section"}
	}
	var violations []string
	guard := sections[guardIndex]
	if !nmFalse(guard.values["managed"]) {
		violations = append(violations, "[device-subyard] does not set managed=0")
	}
	listed := map[string]bool{}
	for _, entry := range nmListEntries(guard.values["match-device"]) {
		listed[entry] = true
		if strings.HasPrefix(entry, "except:") {
			violations = append(violations, "[device-subyard] match-device has the exception "+entry)
		}
	}
	required := []string{"type:veth", "driver:veth"}
	for _, bridge := range bridges {
		required = append(required, "interface-name:"+bridge)
	}
	for _, entry := range required {
		if !listed[entry] {
			violations = append(violations, "[device-subyard] match-device lacks "+entry)
		}
	}
	for _, item := range sections[:guardIndex] {
		if !strings.HasPrefix(item.name, "device") || item.name == "device" {
			continue
		}
		managed, stop := nmTrue(item.values["managed"]), nmTrue(item.values["stop-match"])
		if !managed && !stop || nmUnrelatedOnly(item.values["match-device"], bridges) {
			continue
		}
		setting := "stop-match"
		if managed {
			setting = "managed=1"
		}
		violations = append(violations, "["+item.name+"] has priority over [device-subyard] and sets "+setting+" for devices the guard covers")
	}
	return violations
}

func nmListEntries(value string) []string {
	var entries []string
	for _, entry := range strings.FieldsFunc(value, func(character rune) bool { return character == ',' || character == ';' }) {
		if entry = strings.TrimSpace(entry); entry != "" {
			entries = append(entries, entry)
		}
	}
	return entries
}

func nmTrue(value string) bool {
	return slices.Contains([]string{"1", "true", "yes", "on"}, strings.ToLower(value))
}

func nmFalse(value string) bool {
	return slices.Contains([]string{"0", "false", "no", "off"}, strings.ToLower(value))
}

// nmUnrelatedOnly reports whether every entry is an exact interface name outside the guard.
func nmUnrelatedOnly(value string, bridges []string) bool {
	entries := nmListEntries(value)
	if strings.TrimSpace(value) == "" {
		return false
	}
	for _, entry := range entries {
		name, literal := entry, false
		switch {
		case strings.HasPrefix(entry, "interface-name:="):
			name, literal = strings.TrimPrefix(entry, "interface-name:="), true
		case strings.HasPrefix(entry, "interface-name:~"):
			name = strings.TrimPrefix(entry, "interface-name:~")
		case strings.HasPrefix(entry, "interface-name:"):
			name = strings.TrimPrefix(entry, "interface-name:")
		case strings.Contains(entry, ":"):
			return false
		}
		if !literal && strings.ContainsAny(name, "*?") || name == "" || ownDeviceName(name, bridges) {
			return false
		}
	}
	return true
}

func ownDeviceName(name string, bridges []string) bool {
	return strings.HasPrefix(name, "veth") || incusTapName(name) || slices.Contains(bridges, name)
}

// incusTapName matches Incus's tap devices, "tap" plus 8 characters.
func incusTapName(name string) bool {
	return len(name) == len("tap")+8 && strings.HasPrefix(name, "tap")
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

// unsafeDeviceName matches Subyard's own devices: yard bridges, veths and Incus tap devices.
func unsafeDeviceName(device string, bridges []string) bool {
	return ownDeviceName(device, bridges)
}

// NetworkManagerGuardPath is the Subyard guard drop-in that scripts/lib/host.sh owns.
const NetworkManagerGuardPath = "/etc/NetworkManager/conf.d/zz-subyard-unmanaged.conf"

// NetworkManagerRebootPendingPath holds the boot ID during which a released guard was
// rewritten without reloading NetworkManager (scripts/lib/host.sh).
const NetworkManagerRebootPendingPath = "/run/subyard/nm-guard-reboot-pending"

const bootIDPath = "/proc/sys/kernel/random/boot_id"

// NetworkManagerRebootReminder is the one next step after a guard migration.
const NetworkManagerRebootReminder = "reboot the host once so NetworkManager applies the migrated Subyard guard safely; NetworkManager keeps its previous configuration until then"

// NetworkManagerRebootPending reports whether the migrated guard still waits: the record names
// this boot and, when it names a NetworkManager invocation, NetworkManager has not restarted
// since (a restart reads every configuration file again). It matches scripts/lib-power.sh.
func NetworkManagerRebootPending(path, bootID string, invocation func() string) bool {
	recorded, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	fields := strings.Fields(string(recorded))
	current, err := os.ReadFile(bootID)
	if err != nil || len(fields) == 0 || fields[0] != strings.TrimSpace(string(current)) {
		return false
	}
	if len(fields) < 2 || fields[1] == "unknown" || invocation == nil {
		return true
	}
	now := invocation()
	return now == "" || now == fields[1]
}

// LocalNetworkManagerRebootPending reports the pending reboot on this host.
func LocalNetworkManagerRebootPending() bool {
	return NetworkManagerRebootPending(NetworkManagerRebootPendingPath, bootIDPath, func() string {
		output, err := exec.Command("systemctl", "show", "-p", "InvocationID", "--value", "NetworkManager").Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(output))
	})
}

// NetworkManagerGuardCurrent reports whether no released guard remains: the guard is absent or
// is the [device-subyard] section alone, and the legacy 99- file, even a dangling link, is gone.
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
	sections, keys := 0, map[string]int{}
	for _, line := range strings.Split(string(contents), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case line == "[device-subyard]":
			sections++
		case strings.HasPrefix(line, "["):
			return false, nil
		default:
			key, _, found := strings.Cut(line, "=")
			if !found || sections != 1 {
				return false, nil
			}
			keys[strings.TrimSpace(key)]++
		}
	}
	return sections == 1 && len(keys) == 2 && keys["match-device"] == 1 && keys["managed"] == 1, nil
}

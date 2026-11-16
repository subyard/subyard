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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
)

type NetworkGuard struct {
	Lookup func(string) (string, bool)
	Run    func(context.Context, string, ...string) ([]byte, error)
	// State reports the guard's read state (see NetworkManagerGuardState); nil means loaded.
	State func() string
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
	remedy := "correct the NetworkManager configuration, then restart NetworkManager or reboot so it drops what it still holds, then rerun yard start"
	for _, violation := range violations {
		if strings.Contains(violation, "no [device-subyard]") || strings.Contains(violation, "match-device lacks") {
			remedy = "run yard init"
		}
	}
	state := "loaded"
	if guard.State != nil {
		state = guard.State()
	}
	switch state {
	case "unread-init":
		violations = append(violations, "NetworkManager has not read the Subyard guard since it was written")
		remedy = "reboot the host or restart NetworkManager ('sudo systemctl restart NetworkManager'), then rerun yard start"
	case "unread-migration":
		violations = append(violations, "NetworkManager has not read the migrated Subyard guard")
		remedy = "reboot the host once, then rerun yard start"
	}
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
	return fmt.Errorf("NetworkManager guard is not effective: %s — %s", strings.Join(violations, "; "), remedy)
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
	if listed["interface-name:tap*"] {
		listed["interface-name:tap????????"] = true
	}
	required := slices.Clone(NetworkManagerGuardStaticEntries)
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
		if nmTrue(item.values["managed"]) {
			violations = append(violations, "["+item.name+"] has priority over [device-subyard] and sets managed=1")
		} else if nmTrue(item.values["stop-match"]) {
			violations = append(violations, "["+item.name+"] has priority over [device-subyard] and sets stop-match")
		}
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

// NetworkManagerGuardStaticEntries are listed by every guard besides the Incus-managed bridges,
// as POWER_NM_GUARD_STATIC in scripts/lib-power.sh.
var NetworkManagerGuardStaticEntries = []string{"type:veth", "driver:veth", "interface-name:tap????????"}

// NetworkManagerGuardStatePath records which guard NetworkManager has read (scripts/lib/host.sh).
const NetworkManagerGuardStatePath = "/run/subyard/nm-guard-state"

const bootIDPath = "/proc/sys/kernel/random/boot_id"

// NetworkManagerRebootReminder is the one next step after a guard migration.
const NetworkManagerRebootReminder = "reboot the host once so NetworkManager applies the migrated Subyard guard safely; NetworkManager keeps its previous configuration until then"

// NetworkManagerUnreadReminder names the start NetworkManager needs before any yard starts.
const NetworkManagerUnreadReminder = "NetworkManager has not read the Subyard guard yet: reboot the host or restart NetworkManager before starting a yard"

// NetworkManagerGuardState is the pure read-state rule shared with power_nm_guard_state_rule in
// scripts/lib-power.sh and checked on tests/fixtures/nm-guard-state.txt. The record applies only
// with five fields, the same boot, the same guard file ID and a readable invocation: "held", or
// another NetworkManager invocation since, is loaded; "unread" is not. Anything else falls back
// to the age test. It returns loaded, loaded-reboot, unread-init or unread-migration.
func NetworkManagerGuardState(record, boot, fileID, invocation string, ageLoaded bool) string {
	fields := strings.Fields(record)
	if len(fields) == 5 && fields[0] == boot && fileID != "" && fields[1] == fileID && invocation != "" &&
		(fields[3] == "held" || fields[3] == "unread") && (fields[4] == "init" || fields[4] == "migration") {
		switch {
		case fields[2] != invocation:
			return "loaded"
		case fields[3] == "held" && fields[4] == "migration":
			return "loaded-reboot"
		case fields[3] == "held":
			return "loaded"
		default:
			return "unread-" + fields[4]
		}
	}
	if ageLoaded {
		return "loaded"
	}
	return "unread-init"
}

func localNetworkManagerShow(property string) string {
	output, err := exec.Command("systemctl", "show", "-p", property, "--value", "NetworkManager").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// networkManagerFileID matches `stat -c '%i:%.9Z'`.
func networkManagerFileID(path string) string {
	var stat syscall.Stat_t
	if err := syscall.Stat(path, &stat); err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d.%09d", stat.Ino, stat.Ctim.Sec, stat.Ctim.Nsec)
}

// networkManagerGuardAgeLoaded: NetworkManager's main process started at least 2 s after the
// guard file's ctime. Any failed read means not loaded.
func networkManagerGuardAgeLoaded(path string) bool {
	var stat syscall.Stat_t
	if syscall.Stat(path, &stat) != nil {
		return false
	}
	pid := localNetworkManagerShow("MainPID")
	if pid == "" || pid == "0" {
		return false
	}
	processStat, err := os.ReadFile(filepath.Join("/proc", pid, "stat"))
	if err != nil {
		return false
	}
	_, rest, found := strings.Cut(string(processStat), ") ")
	fields := strings.Fields(rest)
	if !found || len(fields) < 20 {
		return false
	}
	startTicks, err := strconv.ParseFloat(fields[19], 64)
	if err != nil {
		return false
	}
	uptimeBytes, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return false
	}
	uptimeFields := strings.Fields(string(uptimeBytes))
	if len(uptimeFields) == 0 {
		return false
	}
	uptime, err := strconv.ParseFloat(uptimeFields[0], 64)
	if err != nil {
		return false
	}
	const ticksPerSecond = 100 // USER_HZ on Linux, as getconf CLK_TCK reports
	now := float64(time.Now().UnixNano()) / 1e9
	ctime := float64(stat.Ctim.Sec) + float64(stat.Ctim.Nsec)/1e9
	return now-ctime >= uptime-startTicks/ticksPerSecond+2
}

// LocalNetworkManagerGuardState reports this host's guard read state.
func LocalNetworkManagerGuardState() string {
	return networkManagerGuardStateAt(NetworkManagerGuardStatePath, NetworkManagerGuardPath, bootIDPath,
		localNetworkManagerShow("InvocationID"), networkManagerGuardAgeLoaded)
}

// networkManagerGuardStateAt reads the record and facts; an unreadable record or boot ID never
// yields loaded.
func networkManagerGuardStateAt(statePath, guardPath, bootPath, invocation string, ageLoaded func(string) bool) string {
	record, err := os.ReadFile(statePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "unread-init"
	}
	boot, err := os.ReadFile(bootPath)
	if err != nil {
		return "unread-init"
	}
	firstLine, _, _ := strings.Cut(string(record), "\n")
	return NetworkManagerGuardState(firstLine, strings.TrimSpace(string(boot)), networkManagerFileID(guardPath), invocation, ageLoaded(guardPath))
}

func networkManagerActiveLocally() bool {
	switch localNetworkManagerShow("ActiveState") {
	case "active", "activating", "reloading", "deactivating":
		return true
	}
	return false
}

// LocalNetworkManagerGuardRelevant: a guard exists and NetworkManager runs, so its read state
// matters for reminders.
func LocalNetworkManagerGuardRelevant() bool {
	_, err := os.Lstat(NetworkManagerGuardPath)
	return err == nil && networkManagerActiveLocally()
}

// LocalNetworkManagerRebootPending reports a migrated guard waiting for the host reboot.
func LocalNetworkManagerRebootPending() bool {
	state := LocalNetworkManagerGuardState()
	return state == "loaded-reboot" || state == "unread-migration"
}

// NetworkManagerGuardCurrent reports whether no released guard remains: the guard is absent or
// is the [device-subyard] section alone, and the legacy 99- file, even a dangling link, is gone.
func NetworkManagerGuardCurrent(conf string) (bool, error) {
	if _, err := os.Lstat(filepath.Join(filepath.Dir(conf), "99-subyard-unmanaged.conf")); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if _, err := os.Lstat(conf); errors.Is(err, os.ErrNotExist) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	// A dangling link is no current guard, as in scripts/lib/host.sh.
	contents, err := os.ReadFile(conf)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
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

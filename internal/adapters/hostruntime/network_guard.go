package hostruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

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
	// Each read is independent: a failed --print-config never hides the read state or the
	// bridges nmcli reports. The next steps are joined in order.
	var violations, remedies []string
	reboot := false
	if output, err := guard.run(ctx, binary, "--print-config"); err != nil {
		violations = append(violations, fmt.Sprintf("cannot read NetworkManager's effective configuration (%v)", err))
		remedies = append(remedies, "fix 'NetworkManager --print-config' (run 'sudo -v' when it needs root)")
	} else if violations = NetworkManagerConfigViolations(string(output), bridges); len(violations) > 0 {
		remedies = append(remedies, "correct the NetworkManager configuration")
		reboot = true
		for _, violation := range violations {
			if strings.Contains(violation, "no [device-subyard]") || strings.Contains(violation, "match-device lacks") {
				remedies[len(remedies)-1], reboot = "run yard init", false
				break
			}
		}
	}
	state := "loaded"
	if guard.State != nil {
		state = guard.State()
	}
	switch state {
	case "unread-init", "unread-migration":
		violations = append(violations, "NetworkManager has not read the Subyard guard on disk since this boot began")
		reboot = true
	case "unbound":
		// Only init can tell what NetworkManager holds: it records the guard as unread.
		if _, err := os.Lstat(NetworkManagerGuardPath); err == nil && networkManagerFileID(NetworkManagerGuardPath) == "" {
			violations = append(violations, "the Subyard guard "+NetworkManagerGuardPath+" is not a regular file (a link, for example); init replaces it with a file")
		} else {
			violations = append(violations, "no Subyard record binds the guard on disk to what NetworkManager read")
		}
		if !slices.Contains(remedies, "run yard init") {
			remedies = append(remedies, "run yard init")
		}
		reboot = true
	}
	if reboot {
		remedies = append(remedies, "reboot the host once")
	}
	nextStep := "yard start"
	nmcli, ok := guard.lookup("nmcli")
	if !ok {
		violations = append(violations, "cannot read NetworkManager's devices with nmcli")
	} else if devices, err := guard.run(ctx, nmcli, "-t", "-f", "DEVICE,STATE", "device"); err != nil || strings.TrimSpace(string(devices)) == "" {
		violations = append(violations, "cannot read NetworkManager's devices with nmcli")
	} else {
		for _, line := range strings.Split(string(devices), "\n") {
			device, state, _ := strings.Cut(line, ":")
			if slices.Contains(bridges, device) && state != "unmanaged" {
				if len(violations) == 0 {
					nextStep = "yard init"
				}
				violations = append(violations, "NetworkManager manages yard bridge "+device)
			}
		}
	}
	if len(violations) == 0 {
		return nil
	}
	return fmt.Errorf("NetworkManager guard is not effective: %s — %s", strings.Join(violations, "; "),
		strings.Join(append(remedies, "rerun "+nextStep), ", then "))
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
			if route.Device == "eth0" {
				if detect, ok := guard.lookup("systemd-detect-virt"); ok {
					kind, err := guard.run(ctx, detect, "--container")
					if err == nil && strings.TrimSpace(string(kind)) == "lxc" {
						continue
					}
				}
			}
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

// NetworkManagerConfigViolations checks effective or proposed NetworkManager configuration.
// NetworkManager prints [device*] sections in priority order,
// except the plain [device] section, which it always considers last. [device-subyard] must set
// managed=0 for type:veth, driver:veth and every bridge without except: entries, and no section
// printed before it may set managed=1 or stop-match for devices the guard covers.
func NetworkManagerConfigViolations(contents string, bridges []string) []string {
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

// NetworkManagerGuardStatePath records which guard NetworkManager has read (scripts/lib/host.sh):
// boot ID, the guard file's identity (inode:ctime, compared only for equality), sha256 of its
// static content, held|unread and init|migration. It persists across boots, so losing it can only
// refuse starts.
const NetworkManagerGuardStatePath = "/var/lib/subyard/nm-guard-state"

const bootIDPath = "/proc/sys/kernel/random/boot_id"

// NetworkManagerRebootReminder is the one next step after a guard migration.
const NetworkManagerRebootReminder = "reboot the host once so NetworkManager applies the migrated Subyard guard safely; NetworkManager keeps its previous configuration until then"

// NetworkManagerUnreadReminder names the reboot NetworkManager needs before any yard starts.
const NetworkManagerUnreadReminder = "NetworkManager has not read the Subyard guard yet: reboot the host once before starting a yard"

// NetworkManagerUnboundReminder: no record binds the guard on disk, so init must record it first.
const NetworkManagerUnboundReminder = "no Subyard record binds the NetworkManager guard on disk: run yard init, then reboot the host once before starting a yard"

var exactInterfaceEntry = regexp.MustCompile(`^interface-name:[A-Za-z0-9_.:-]+$`)

// NetworkManagerGuardStatic is the guard's static content, as power_nm_guard_static_text in
// scripts/lib-power.sh (shared vectors in tests/fixtures/nm-guard-static): trimmed lines without
// comments or blank lines, each ending in a newline, and match-device without exact interface
// names (the Incus bridges, which need no reboot because each one is pinned unmanaged).
func NetworkManagerGuardStatic(contents string) string {
	var static strings.Builder
	for _, line := range strings.Split(contents, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if key, value, found := strings.Cut(line, "="); found && strings.TrimSpace(key) == "match-device" {
			var kept []string
			for _, entry := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ';' }) {
				entry = strings.TrimSpace(entry)
				if entry != "" && !exactInterfaceEntry.MatchString(entry) {
					kept = append(kept, entry)
				}
			}
			line = "match-device=" + strings.Join(kept, ",")
		}
		static.WriteString(line + "\n")
	}
	return static.String()
}

// NetworkManagerGuardStaticHash is the record's hash of the guard's static content.
func NetworkManagerGuardStaticHash(contents string) string {
	sum := sha256.Sum256([]byte(NetworkManagerGuardStatic(contents)))
	return hex.EncodeToString(sum[:])
}

// NetworkManagerGuardState is the pure read-state rule shared with power_nm_guard_state_rule in
// scripts/lib-power.sh and checked on tests/fixtures/nm-guard-state.txt. A record binds one guard
// file (its identity) and its static content; without that binding the state is "unbound", which
// init repairs by recording the file as unread. A bound record is loaded when it is "held" or
// names another boot (NetworkManager read the file when it started). It returns loaded,
// loaded-reboot (a held migration waiting for the reboot), unread-init, unread-migration or
// unbound.
func NetworkManagerGuardState(record, boot, fileID, hash string) string {
	if boot == "" {
		return "unread-init"
	}
	fields := strings.Fields(record)
	if len(fields) != 5 || fileID == "" || fields[1] != fileID || hash == "" || fields[2] != hash ||
		(fields[3] != "held" && fields[3] != "unread") || (fields[4] != "init" && fields[4] != "migration") {
		return "unbound"
	}
	switch {
	case fields[0] != boot:
		return "loaded"
	case fields[3] == "held" && fields[4] == "migration":
		return "loaded-reboot"
	case fields[3] == "held":
		return "loaded"
	default:
		return "unread-" + fields[4]
	}
}

// networkManagerFileID matches power_nm_file_id: one lstat of the path itself, which must be a
// regular file (never a link, FIFO or anything a read could block on).
func networkManagerFileID(path string) string {
	var stat syscall.Stat_t
	if err := syscall.Lstat(path, &stat); err != nil || stat.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return ""
	}
	return fmt.Sprintf("%d:%d.%09d", stat.Ino, stat.Ctim.Sec, stat.Ctim.Nsec)
}

// readRegularGuard opens the path without following a link or blocking, and reads it only when
// the opened file is the regular file the identity named.
func readRegularGuard(path, fileID string) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var stat syscall.Stat_t
	if err := syscall.Fstat(int(file.Fd()), &stat); err != nil || stat.Mode&syscall.S_IFMT != syscall.S_IFREG ||
		fmt.Sprintf("%d:%d.%09d", stat.Ino, stat.Ctim.Sec, stat.Ctim.Nsec) != fileID {
		return nil, errors.New("the guard is not the regular file it was")
	}
	return io.ReadAll(io.LimitReader(file, 1<<20))
}

func localNetworkManagerShow(property string) string {
	output, err := exec.Command("systemctl", "show", "-p", property, "--value", "NetworkManager").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// LocalNetworkManagerGuardState reports this host's guard read state.
func LocalNetworkManagerGuardState() string {
	return networkManagerGuardStateAt(NetworkManagerGuardStatePath, NetworkManagerGuardPath, bootIDPath)
}

// networkManagerGuardStateAt reads the boot ID, the guard's identity and content through any
// symlink, and the record; any failed read never yields loaded.
func networkManagerGuardStateAt(statePath, guardPath, bootPath string) string {
	boot, err := os.ReadFile(bootPath)
	if err != nil || strings.TrimSpace(string(boot)) == "" {
		return "unread-init"
	}
	// Subyard writes a regular file; a link could be retargeted and restored behind the record,
	// and the type is part of the identity read, so nothing else is ever opened.
	fileID := networkManagerFileID(guardPath)
	if fileID == "" {
		return "unbound"
	}
	guard, err := readRegularGuard(guardPath, fileID)
	// The identity is read again after the content: a concurrent rewrite changes it.
	if fileID == "" || err != nil || networkManagerFileID(guardPath) != fileID {
		return "unbound"
	}
	record, err := os.ReadFile(statePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "unbound"
	}
	firstLine, _, _ := strings.Cut(string(record), "\n")
	return NetworkManagerGuardState(firstLine, strings.TrimSpace(string(boot)), fileID, NetworkManagerGuardStaticHash(string(guard)))
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

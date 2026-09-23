package testvmsruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func (runtime *Runtime) killAgentSessions(ctx context.Context) {
	if _, err := user.Lookup(runtime.Config.AgentUser); err != nil {
		return
	}
	if _, err := runtime.Runner.LookPath("pkill"); err != nil {
		return
	}
	_, _, _ = runtime.Runner.Run(ctx, "pkill",
		[]string{"-KILL", "-u", runtime.Config.AgentUser}, nil, nil)
}

func (runtime *Runtime) writeAgentAuthorizedKeys(ip1, ip2 string) error {
	cfg := runtime.Config
	if _, err := user.Lookup(cfg.AgentUser); err != nil {
		return errors.New("agent bastion account is missing; re-run yard init")
	}
	key := ""
	if cfg.AgentPublicKey != "" {
		var err error
		key, err = normalizedPublicKey(cfg.AgentPublicKey)
		if err != nil {
			return err
		}
	}
	directory := filepath.Dir(cfg.AgentAuthorizedKeys)
	mode := os.FileMode(0o600)
	if os.Geteuid() == 0 {
		group, err := userGroup(cfg.AgentUser)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(directory, 0o750); err != nil {
			return err
		}
		if err := os.Chmod(directory, 0o750); err != nil {
			return err
		}
		if err := os.Chown(directory, 0, group); err != nil {
			return err
		}
		mode = 0o640
	} else if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	options := `restrict,command="` + cfg.StatusCommand + `"`
	if ip1 != "" || ip2 != "" {
		if !safeIPv4(ip1) || (cfg.guestCount() == 2 && !safeIPv4(ip2)) || (cfg.guestCount() == 1 && ip2 != "") {
			return errors.New("cannot publish non-IPv4 VM targets")
		}
		options = `restrict,port-forwarding,permitopen="` + ip1 + `:22"`
		if cfg.guestCount() == 2 {
			options += `,permitopen="` + ip2 + `:22"`
		}
		options += `,command="` + cfg.StatusCommand + `"`
	}
	payload := []byte(nil)
	if key != "" {
		payload = []byte(options + " " + key + " " + agentKeyMarker + "\n")
	}
	if err := writeAtomic(cfg.AgentAuthorizedKeys, payload, mode); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		group, _ := userGroup(cfg.AgentUser)
		return os.Chown(cfg.AgentAuthorizedKeys, 0, group)
	}
	return nil
}

func (runtime *Runtime) restrictAgentAccess(_ string) error {
	runtime.killAgentSessions(context.Background())
	return runtime.writeAgentAuthorizedKeys("", "")
}

func (runtime *Runtime) enableAgentAccess(ctx context.Context) error {
	cfg := runtime.Config
	ip1, err := runtime.vmIP(ctx, cfg.vm(1))
	if err != nil {
		return err
	}
	ip2 := ""
	if cfg.guestCount() == 2 {
		ip2, err = runtime.vmIP(ctx, cfg.vm(2))
		if err != nil {
			return err
		}
	}
	runtime.killAgentSessions(ctx)
	return runtime.writeAgentAuthorizedKeys(ip1, ip2)
}

func (runtime *Runtime) collectFailureDiagnostics(ctx context.Context, cause error) error {
	cfg := runtime.Config
	if err := runtime.ensureStateDir(); err != nil {
		return err
	}
	var payload strings.Builder
	fmt.Fprintf(&payload, "timestamp_utc=%s\n", runtime.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&payload, "worker_error=%s\nproject=%s\n", oneLine(cause.Error()), cfg.Project)
	if runtime.projectExists(ctx) {
		fmt.Fprintln(&payload, "\n== project ==")
		if value, err := runtime.incus(ctx, "project", "show", cfg.Project); err == nil {
			payload.WriteString(value)
		}
		for index := 1; index <= cfg.guestCount(); index++ {
			vm := cfg.vm(index)
			if !runtime.vmExists(ctx, vm) {
				continue
			}
			fmt.Fprintf(&payload, "\n== %s info/log ==\n", vm)
			if value, err := runtime.incus(ctx, "info", "--show-log", vm,
				"--project", cfg.Project); err == nil {
				payload.WriteString(value)
			}
		}
	} else {
		fmt.Fprintln(&payload, "project_state=absent")
	}
	if err := writeAtomic(cfg.failureLog(), []byte(payload.String()), 0o640); err != nil {
		return err
	}
	fmt.Fprintf(runtime.Stderr, "test-vms: failure diagnostics saved to %s\n%s",
		cfg.failureLog(), payload.String())
	return nil
}

func (runtime *Runtime) doctor(ctx context.Context, want map[string]string) error {
	cfg := runtime.Config
	wantEnabled := want["WANT_ENABLED"]
	actualEnabled := "0"
	if cfg.Enabled {
		actualEnabled = "1"
	}
	if wantEnabled != actualEnabled {
		return errors.New("backend enabled state differs")
	}
	if _, err := os.Stat(runtime.ConfigPath); err != nil {
		return errors.New("backend config is missing")
	}
	executable := runtime.ExecutablePath
	if executable == "" {
		var err error
		executable, err = os.Executable()
		if err != nil {
			return err
		}
	}
	hash, err := fileSHA256(executable)
	if err != nil {
		return err
	}
	if hash != want["WANT_ENGINE_HASH"] {
		return errors.New("installed test-vms engine hash differs")
	}
	if !cfg.Enabled {
		if runtime.commandOK(ctx, "systemctl", "is-active", "--quiet",
			"subyard-test-vms-lease-reaper.timer") {
			return errors.New("lease reaper remains active")
		}
		if runtime.commandOK(ctx, "systemctl", "is-active", "--quiet",
			"subyard-test-vms-firewall.service") {
			return errors.New("firewall remains active")
		}
		for _, path := range []string{
			"/etc/systemd/system/incus.service.d/subyard-nested-e2e.conf",
			"/etc/ssh/sshd_config.d/90-subyard-e2e-agent.conf",
		} {
			if _, err := os.Stat(path); err == nil {
				return fmt.Errorf("disabled backend artifact remains: %s", path)
			}
		}
		if _, err := user.Lookup(cfg.AgentUser); err == nil {
			return errors.New("agent account remains")
		}
		return nil
	}
	for _, command := range []string{cfg.Incus, "qemu-system-x86_64", "nft"} {
		if _, err := runtime.Runner.LookPath(command); err != nil {
			return fmt.Errorf("required command is missing: %s", command)
		}
	}
	version, _, err := runtime.Runner.Run(ctx, cfg.Incus, []string{"--version"}, nil, nil)
	if err != nil {
		return err
	}
	if !runtime.commandOK(ctx, "dpkg", "--compare-versions",
		strings.TrimSpace(string(version)), "ge", "6.0.6") {
		return errors.New("inner Incus is too old")
	}
	if !runtime.commandOK(ctx, "systemctl", "is-active", "--quiet", "incus.service") {
		return errors.New("inner Incus is inactive")
	}
	if !fileContains("/etc/systemd/system/incus.service.d/subyard-nested-e2e.conf",
		"Environment=INCUS_SECURITY_APPARMOR=false") {
		return errors.New("Incus drop-in differs")
	}
	if !runtime.commandOK(ctx, "systemctl", "is-enabled", "--quiet",
		"subyard-test-vms-lease-reaper.timer") {
		return errors.New("lease reaper is disabled")
	}
	if !runtime.commandOK(ctx, "systemctl", "is-active", "--quiet",
		"subyard-test-vms-firewall.service") {
		return errors.New("firewall is inactive")
	}
	if !runtime.commandOK(ctx, "nft", "list", "table", "inet", "subyard_e2e") {
		return errors.New("firewall table is missing")
	}
	sshd, _, err := runtime.Runner.Run(ctx, "sshd", []string{"-T"}, nil, nil)
	if err != nil {
		return errors.New("cannot render sshd config")
	}
	if !linePresent(string(sshd), "passwordauthentication no") ||
		!linePresent(string(sshd), "kbdinteractiveauthentication no") {
		return errors.New("SSH password login is enabled")
	}
	for _, node := range []string{"/dev/kvm", "/dev/vsock", "/dev/vhost-vsock", "/dev/net/tun"} {
		info, err := os.Stat(node)
		if err != nil || info.Mode()&os.ModeCharDevice == 0 {
			return fmt.Errorf("required device is missing: %s", node)
		}
	}
	if err := requireRootMode(cfg.StateDir, 0o700); err != nil {
		return err
	}
	devGroups, err := runtime.output(ctx, "id", "-nG", cfg.DevUser)
	if err != nil {
		return errors.New("yard developer account is missing")
	}
	for _, group := range strings.Fields(devGroups) {
		if group == "incus-admin" || group == "yard" {
			return errors.New("yard developer has inner privileges")
		}
	}
	if _, err := user.Lookup(cfg.AgentUser); err != nil {
		return errors.New("agent account is missing")
	}
	account, err := runtime.output(ctx, "passwd", "--status", cfg.AgentUser)
	if err != nil || !strings.HasPrefix(account, cfg.AgentUser+" P ") {
		return errors.New("agent account cannot use key login")
	}
	groups, err := runtime.output(ctx, "id", "-nG", cfg.AgentUser)
	if err != nil || len(strings.Fields(groups)) != 1 {
		return errors.New("agent has supplementary groups")
	}
	groupName, err := runtime.output(ctx, "id", "-gn", cfg.AgentUser)
	if err != nil {
		return errors.New("cannot inspect agent group")
	}
	if err := requireOwnerMode(filepath.Join(cfg.AgentHome, ".ssh"),
		"root", strings.TrimSpace(groupName), 0o750); err != nil {
		return err
	}
	if err := requireOwnerMode(cfg.AgentAuthorizedKeys,
		"root", strings.TrimSpace(groupName), 0o640); err != nil {
		return err
	}
	authorized, err := os.ReadFile(cfg.AgentAuthorizedKeys)
	if err != nil {
		return errors.New("cannot inspect controller authorized_keys")
	}
	if len(authorized) != 0 {
		return errors.New("static controller key remains active")
	}
	const command = "/usr/local/libexec/subyard/test-vms-authorized-key"
	if err := requireRootMode(command, 0o755); err != nil {
		return err
	}
	commandPayload, err := os.ReadFile(command)
	if err != nil || !strings.Contains(string(commandPayload),
		`restrict,command="sudo -n /usr/local/libexec/subyard/test-vms-inner _test-vms-facade"`) {
		return errors.New("default-open forced facade differs")
	}
	if !fileContains("/etc/ssh/sshd_config.d/90-subyard-e2e-agent.conf",
		"AuthorizedKeysCommand "+command+" %u %t %k") {
		return errors.New("controller AuthorizedKeysCommand differs")
	}
	if !fileContains("/etc/sudoers.d/subyard-test-vms-facade",
		`subyard-e2e-agent ALL=(root) NOPASSWD: /usr/local/libexec/subyard/test-vms-inner _test-vms-facade`) {
		return errors.New("bounded facade sudo policy differs")
	}
	return nil
}

func (runtime *Runtime) commandOK(ctx context.Context, name string, arguments ...string) bool {
	_, _, err := runtime.Runner.Run(ctx, name, arguments, nil, nil)
	return err == nil
}

func (runtime *Runtime) output(ctx context.Context, name string, arguments ...string) (string, error) {
	stdout, _, err := runtime.Runner.Run(ctx, name, arguments, nil, nil)
	return string(stdout), err
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func fileContains(path, value string) bool {
	payload, err := os.ReadFile(path)
	return err == nil && linePresent(string(payload), value)
}

func requireRootMode(path string, mode os.FileMode) error {
	return requireOwnerMode(path, "root", "root", mode)
}

func requireOwnerMode(path, ownerName, groupName string, mode os.FileMode) error {
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != mode {
		return fmt.Errorf("%s mode differs", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot inspect %s ownership", path)
	}
	owner, ownerErr := user.LookupId(strconv.Itoa(int(stat.Uid)))
	group, groupErr := user.LookupGroupId(strconv.Itoa(int(stat.Gid)))
	if ownerErr != nil || groupErr != nil || owner.Username != ownerName || group.Name != groupName {
		return fmt.Errorf("%s ownership differs", path)
	}
	return nil
}

func safeIPv4(value string) bool {
	ip := netParseIP(value)
	return ip != nil
}

var netParseIP = func(value string) []byte {
	parts := strings.Split(value, ".")
	if len(parts) != 4 {
		return nil
	}
	result := make([]byte, 4)
	for index, part := range parts {
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 || number > 255 || strconv.Itoa(number) != part {
			return nil
		}
		result[index] = byte(number)
	}
	return result
}

func oneLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

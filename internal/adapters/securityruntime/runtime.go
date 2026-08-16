package securityruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/resource"
)

var ErrContract = errors.New("security contract failed")

type Runtime struct {
	RepositoryRoot      string
	Environment         map[string]string
	Yard                domain.Context
	Incus               ports.Incus
	Stdout              io.Writer
	Stderr              io.Writer
	State               func(context.Context, Runtime) (ports.ReconcileState, bool, error)
	ProxyContracts      []resource.ProxyContract
	ResolveOwnerAddress func(context.Context, string) (string, error)
}

type finding struct {
	level   string
	message string
}

func (runtime Runtime) CheckSecurity(
	ctx context.Context,
	requireLive bool,
	quiet bool,
) (string, error) {
	findings := runtime.staticFindings()
	state, live, err := runtime.liveState(ctx)
	if err != nil {
		return "FAIL", err
	}
	if !live {
		if requireLive {
			findings = append(findings, finding{"fail",
				fmt.Sprintf("live Incus project %q is not reachable", runtime.Yard.IncusProject)})
		}
		findings = append(findings, finding{"warn", "live Incus state unavailable; static contract checked only"})
	} else {
		findings = append(findings, runtime.liveFindings(ctx, state, requireLive)...)
	}
	failures, warnings := render(runtime.Stdout, runtime.Stderr, findings, quiet)
	if failures != 0 {
		return "FAIL", fmt.Errorf("%w: %d failure(s), %d warning(s)",
			ErrContract, failures, warnings)
	}
	if !quiet {
		fmt.Fprintf(writer(runtime.Stdout), "Subyard security contract passed (%d warning(s))\n", warnings)
	}
	if live {
		return "live", nil
	}
	return "static-only", nil
}

func (runtime Runtime) staticFindings() []finding {
	var result []finding
	for _, entry := range strings.Fields(runtime.Environment["HOST_MOUNTS"]) {
		result = append(result, mountFindings("HOST_MOUNTS", entry)...)
	}
	profiles := filepath.Join(runtime.RepositoryRoot, "config", "profiles")
	if configured := runtime.Environment["SUBYARD_PROFILES_DIR"]; configured != "" {
		profiles = configured
	}
	entries, err := os.ReadDir(profiles)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		result = append(result, finding{"fail", "read profiles: " + err.Error()})
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(profiles, entry.Name(), "profile.conf")
		values, err := config.ReadAssignments(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			result = append(result, finding{"fail", fmt.Sprintf("read profile %s: %v", entry.Name(), err)})
			continue
		}
		for _, value := range strings.Fields(values["YARD_MOUNTS"]) {
			result = append(result, mountFindings(entry.Name()+" YARD_MOUNTS", value)...)
		}
		for _, value := range strings.Fields(values["ENV_MOUNTS"]) {
			if hostSocket(value) {
				result = append(result, finding{"fail",
					fmt.Sprintf("%s ENV_MOUNTS exposes a host-control socket: %s", entry.Name(), value)})
			}
		}
	}
	if runtime.Yard.ForwardSSHAgent {
		result = append(result, finding{"warn",
			"SSH agent forwarding is enabled; this is operator opt-in, not a credential boundary"})
	}
	return append(result, runtime.keyFindings()...)
}

func (runtime Runtime) keyFindings() []finding {
	root := runtime.Environment["SUBYARD_KEYS_ROOT"]
	if root == "" {
		root = filepath.Join(runtime.Yard.Paths.ConfigHome, "keys")
	}
	root = filepath.Clean(root)
	repository := filepath.Clean(runtime.RepositoryRoot)
	hostBase := filepath.Clean(runtime.Yard.Paths.HostBase)
	var result []finding
	if pathWithin(root, repository) {
		result = append(result, finding{"fail",
			"SUBYARD_KEYS_ROOT is inside the public/private checkout: " + root})
	}
	if pathWithin(root, hostBase) {
		result = append(result, finding{"fail",
			"SUBYARD_KEYS_ROOT is beneath HOST_BASE and could become a yard mount: " + root})
	}
	info, err := os.Stat(root)
	if errors.Is(err, os.ErrNotExist) {
		return result
	}
	if err != nil {
		return append(result, finding{"fail", "inspect credential ledger root: " + err.Error()})
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		result = append(result, finding{"fail",
			fmt.Sprintf("credential ledger root must have mode 0700: %s (mode %04o)",
				root, info.Mode().Perm())})
	}
	for _, name := range []string{"age.txt", "signing_ed25519"} {
		path := filepath.Join(root, "identity", name)
		identity, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			result = append(result, finding{"fail", "inspect key identity: " + err.Error()})
			continue
		}
		if identity.Mode()&os.ModeSymlink != 0 {
			result = append(result, finding{"fail", "key identity must not be a symlink: " + path})
			continue
		}
		if identity.Mode().Perm() != 0o600 {
			result = append(result, finding{"fail", "key identity must have mode 0600: " + path})
		}
	}
	return result
}

func (runtime Runtime) liveState(ctx context.Context) (ports.ReconcileState, bool, error) {
	if runtime.State != nil {
		return runtime.State(ctx, runtime)
	}
	if runtime.Environment["SUBYARD_SECURITY_SKIP_LIVE"] == "1" || runtime.Incus == nil {
		return ports.ReconcileState{}, false, nil
	}
	if _, err := runtime.Incus.Server(ctx); err != nil {
		return ports.ReconcileState{}, false, nil
	}
	pool := environmentDefault(runtime.Environment, "SRV_POOL", "default")
	volume := environmentDefault(runtime.Environment, "SRV_VOLUME", "yard-srv")
	state, err := runtime.Incus.ReconcileState(ctx, runtime.Yard.IncusProject,
		runtime.Yard.YardInstanceName, pool, volume, runtime.Yard.IncusBridge)
	if err != nil {
		return ports.ReconcileState{}, false, fmt.Errorf("inspect live Incus policy: %w", err)
	}
	return state, state.ProjectFound, nil
}

func (runtime Runtime) liveFindings(ctx context.Context, state ports.ReconcileState, requireLive bool) []finding {
	var result []finding
	project := state.ProjectConfig
	if project["restricted"] != "true" {
		result = append(result, finding{"fail",
			fmt.Sprintf("Incus project %q is not restricted", runtime.Yard.IncusProject)})
	}
	if project["restricted.containers.privilege"] != "unprivileged" {
		result = append(result, finding{"fail",
			fmt.Sprintf("Incus project %q does not require unprivileged containers",
				runtime.Yard.IncusProject)})
	}
	interception := "block"
	if runtime.Yard.NestedE2EVMs {
		interception = "allow"
	}
	if project["restricted.containers.interception"] != interception {
		result = append(result, finding{"fail",
			fmt.Sprintf("Incus project %q syscall interception policy does not match NESTED_E2E_VMS",
				runtime.Yard.IncusProject)})
	}
	if !state.InstanceFound {
		if requireLive {
			result = append(result, finding{"fail",
				fmt.Sprintf("instance %q is missing from project %q",
					runtime.Yard.YardInstanceName, runtime.Yard.IncusProject)})
		}
		return append(result, finding{"warn",
			fmt.Sprintf("instance %q is absent; project policy checked only", runtime.Yard.YardInstanceName)})
	}
	instance := state.Instance
	if instance.Config["security.privileged"] == "true" {
		result = append(result, finding{"fail",
			fmt.Sprintf("instance %q is privileged", runtime.Yard.YardInstanceName)})
	}
	for name, device := range instance.Devices {
		deviceType := device["type"]
		source := device["source"]
		path := device["path"]
		if hostSocket(source + " " + path) {
			result = append(result, finding{"fail",
				fmt.Sprintf("device %q exposes a host-control socket", name)})
		}
		if deviceType == "disk" && filepath.IsAbs(source) && !pathWithin(source, runtime.Yard.Paths.HostBase) {
			if strings.HasPrefix(name, "host-") || strings.HasPrefix(name, "yx-") {
				result = append(result, finding{"fail",
					fmt.Sprintf("managed disk device %q is outside HOST_BASE: %s", name, source)})
			} else {
				result = append(result, finding{"warn",
					fmt.Sprintf("explicit disk device %q exposes host path %q; encapsulation is reduced",
						name, source)})
			}
		}
		if deviceType == "unix-char" {
			switch {
			case source == "/dev/kvm", source == "/dev/fuse", renderDevice.MatchString(source):
			case source == "/dev/vsock", source == "/dev/vhost-vsock", source == "/dev/net/tun":
				if !runtime.Yard.NestedE2EVMs {
					result = append(result, finding{"fail",
						fmt.Sprintf("nested VM device %q is attached while NESTED_E2E_VMS is disabled", name)})
				}
			default:
				result = append(result, finding{"fail",
					fmt.Sprintf("unix-char device %q is outside the supported allowlist: %s", name, source)})
			}
		}
		if runtime.proxyContract(name) != nil {
			if err := runtime.checkOwnedProxy(ctx, name, device, instance.LocalConfig); err != nil {
				result = append(result, finding{"fail", err.Error()})
			}
		} else if deviceType == "proxy" && !loopbackProxy(device["listen"]) {
			result = append(result, finding{"fail",
				fmt.Sprintf("proxy device %q is not loopback-only or declared by an active resource: %s", name, device["listen"])})
		}
	}
	bpf := instance.LocalConfig["security.syscalls.intercept.bpf"]
	bpfDevices := instance.LocalConfig["security.syscalls.intercept.bpf.devices"]
	if runtime.Yard.NestedE2EVMs {
		if bpf != "true" || bpfDevices != "true" {
			result = append(result, finding{"fail",
				"nested E2E VMs require both device-cgroup BPF interception flags"})
		}
	} else if bpf != "" || bpfDevices != "" {
		result = append(result, finding{"fail",
			"device-cgroup BPF interception is enabled while NESTED_E2E_VMS is disabled"})
	}
	return result
}

func (runtime Runtime) checkOwnedProxy(
	ctx context.Context,
	name string,
	device map[string]string,
	instanceConfig map[string]string,
) error {
	contract := runtime.proxyContract(name)
	if contract == nil {
		return fmt.Errorf("proxy device %q is not loopback-only or declared by an active resource: %s", name, device["listen"])
	}
	if device["type"] != "proxy" {
		return fmt.Errorf("device %q does not have the proxy type required by its typed resource contract", name)
	}
	if !exactProxyOptions(device) {
		return fmt.Errorf("proxy device %q contains options outside its typed resource contract", name)
	}
	if contract.OwnershipMetadata &&
		instanceConfig[contract.OwnershipKey()] != contract.OwnershipValue(device) {
		return fmt.Errorf("proxy device %q is missing matching owner-side resource metadata", name)
	}
	host := runtime.Environment[contract.AdvertiseHostSetting]
	portText := runtime.Environment[contract.HostPortSetting]
	port, err := strconv.Atoi(portText)
	if host == "" || err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != portText {
		return fmt.Errorf("proxy device %q has incomplete typed owner endpoint settings", name)
	}
	resolver := runtime.ResolveOwnerAddress
	if resolver == nil {
		resolver = resolveOwnerAddress
	}
	address, err := resolver(ctx, host)
	if err != nil {
		return fmt.Errorf("proxy device %q owner address is not trusted: %v", name, err)
	}
	parsed := net.ParseIP(address)
	if parsed == nil || parsed.To4() == nil || parsed.IsUnspecified() || parsed.IsMulticast() {
		return fmt.Errorf("proxy device %q resolved to an unsafe owner address", name)
	}
	switch contract.AddressPolicy {
	case resource.ProxyAddressTailscaleOnly:
		if parsed.IsLoopback() {
			return fmt.Errorf("proxy device %q requires a non-loopback Tailscale owner address", name)
		}
	case resource.ProxyAddressLoopbackOrTailscale:
	default:
		return fmt.Errorf("proxy device %q has no valid typed owner-address policy", name)
	}
	wantListen := "tcp:" + address + ":" + portText
	if device["listen"] != wantListen || device["connect"] != contract.Connect || device["bind"] != "host" {
		return fmt.Errorf("proxy device %q does not match its typed resource contract", name)
	}
	return nil
}

func exactProxyOptions(device map[string]string) bool {
	if len(device) != 4 {
		return false
	}
	for _, key := range []string{"type", "listen", "connect", "bind"} {
		if _, ok := device[key]; !ok {
			return false
		}
	}
	return true
}

func (runtime Runtime) proxyContract(name string) *resource.ProxyContract {
	for index := range runtime.ProxyContracts {
		if runtime.ProxyContracts[index].Device == name {
			return &runtime.ProxyContracts[index]
		}
	}
	return nil
}

func resolveOwnerAddress(ctx context.Context, host string) (string, error) {
	if host == "127.0.0.1" || host == "localhost" {
		return "127.0.0.1", nil
	}
	if !safeOwnerHost(host) {
		return "", errors.New("advertise host must be a hostname or IPv4 address without scheme, path, or port")
	}
	command := exec.CommandContext(ctx, "tailscale", "ip", "-4")
	output, err := command.Output()
	if err != nil {
		return "", errors.New("tailscale CLI did not report an active IPv4 address")
	}
	tailscale := make(map[string]struct{})
	for _, value := range strings.Fields(string(output)) {
		ip := net.ParseIP(value)
		if ip != nil && ip.To4() != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
			tailscale[ip.String()] = struct{}{}
		}
	}
	if len(tailscale) == 0 {
		return "", errors.New("owner host has no active Tailscale IPv4 address")
	}
	resolved, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		return "", errors.New("advertise host did not resolve")
	}
	active, err := activeIPv4Addresses()
	if err != nil {
		return "", err
	}
	return selectOwnerAddress(resolved, tailscale, active)
}

func selectOwnerAddress(
	resolved []string,
	tailscale map[string]struct{},
	active map[string]struct{},
) (string, error) {
	unique := make(map[string]struct{})
	for _, value := range resolved {
		ip := net.ParseIP(value)
		if ip == nil || ip.To4() == nil {
			continue
		}
		unique[ip.String()] = struct{}{}
	}
	addresses := make([]string, 0, len(unique))
	for address := range unique {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	if len(addresses) != 1 {
		return "", errors.New("advertise host must have exactly one IPv4 DNS answer")
	}
	candidate := addresses[0]
	if _, ok := tailscale[candidate]; !ok {
		return "", errors.New("advertise host is not an active Tailscale IPv4 address")
	}
	if _, ok := active[candidate]; !ok {
		return "", errors.New("advertise host is not active on the owner host")
	}
	return candidate, nil
}

func activeIPv4Addresses() (map[string]struct{}, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, errors.New("could not inspect active owner-host addresses")
	}
	result := make(map[string]struct{})
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			value := address.String()
			if host, _, ok := strings.Cut(value, "/"); ok {
				value = host
			}
			ip := net.ParseIP(value)
			if ip != nil && ip.To4() != nil {
				result[ip.String()] = struct{}{}
			}
		}
	}
	return result, nil
}

func safeOwnerHost(value string) bool {
	if value == "" || len(value) > 253 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '.' || character == '-' {
			continue
		}
		return false
	}
	return value[0] != '-' && value[len(value)-1] != '-'
}

func mountFindings(kind, entry string) []finding {
	parts := strings.Split(entry, ":")
	if len(parts) != 4 {
		return []finding{{"fail", fmt.Sprintf("%s mount must use name:path:ro|rw:mode: %s", kind, entry)}}
	}
	name, path, access, mode := parts[0], parts[1], parts[2], parts[3]
	var result []finding
	if !safeMountName(name) {
		result = append(result, finding{"fail", fmt.Sprintf("%s mount has unsafe name: %s", kind, name)})
	}
	if !filepath.IsAbs(path) {
		result = append(result, finding{"fail", fmt.Sprintf("%s mount target must be absolute: %s", kind, path)})
	}
	if access != "" && access != "ro" && access != "rw" {
		result = append(result, finding{"fail",
			fmt.Sprintf("%s mount access must be ro or rw: %s", kind, entry)})
	}
	if mode != "" && !octalMode.MatchString(mode) {
		result = append(result, finding{"fail", fmt.Sprintf("%s mount mode must be octal: %s", kind, entry)})
	}
	if path == "/" || hostSocket(path) {
		result = append(result, finding{"fail",
			fmt.Sprintf("%s mount targets a forbidden host-control path: %s", kind, path)})
	}
	return result
}

var (
	octalMode    = regexp.MustCompile(`^0[0-7]{3}$`)
	renderDevice = regexp.MustCompile(`^/dev/dri/renderD[0-9]+$`)
)

func safeMountName(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("._-", character) {
			continue
		}
		return false
	}
	return true
}

func hostSocket(value string) bool {
	value = strings.ToLower(value)
	return strings.Contains(value, "docker.sock") ||
		strings.Contains(value, "incus.sock") ||
		strings.Contains(value, "lxd.sock")
}

func pathWithin(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

func loopbackProxy(value string) bool {
	for _, prefix := range []string{
		"tcp:127.0.0.1:", "udp:127.0.0.1:", "tcp:[::1]:", "udp:[::1]:",
	} {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func environmentDefault(environment map[string]string, name, fallback string) string {
	if environment[name] != "" {
		return environment[name]
	}
	return fallback
}

func render(stdout, stderr io.Writer, findings []finding, quiet bool) (int, int) {
	failures := 0
	warnings := 0
	for _, item := range findings {
		switch item.level {
		case "fail":
			failures++
			fmt.Fprintf(writer(stderr), "  [fail] %s\n", item.message)
		case "warn":
			warnings++
			if !quiet {
				fmt.Fprintf(writer(stderr), "  [warn] %s\n", item.message)
			}
		}
	}
	return failures, warnings
}

func writer(value io.Writer) io.Writer {
	if value == nil {
		return io.Discard
	}
	return value
}

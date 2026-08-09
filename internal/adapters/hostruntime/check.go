package hostruntime

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

var ErrHostNotReady = errors.New("host is not ready")

type HostFacts struct {
	OSName        string
	Kernel        string
	CPUFlags      []string
	KVM           bool
	NestedDevices map[string]bool
	CPUs          int
	MemoryBytes   uint64
	StoragePath   string
	StorageFree   uint64
	StorageFS     string
	StorageKnown  bool
	IncusPresent  bool
	IncusVersion  string
	DockerPresent bool
	PortListening bool
}

type CheckOptions struct {
	Strict      bool
	BasePresent bool
}

type HostCheck struct {
	Yard        domain.Context
	Yards       []domain.Context
	Environment map[string]string
	Incus       ports.Incus
	Output      io.Writer
	Probe       func(context.Context, HostCheck) (HostFacts, error)
}

type finding struct {
	section string
	level   string
	message string
}

func (check HostCheck) Run(ctx context.Context, options CheckOptions) error {
	probe := check.Probe
	if probe == nil {
		probe = collectHostFacts
	}
	facts, err := probe(ctx, check)
	if err != nil {
		return fmt.Errorf("collect host facts: %w", err)
	}
	findings := check.evaluate(facts, options)
	failures, warnings := renderHostCheck(check.Output, findings)
	if failures != 0 {
		return fmt.Errorf("%w: %d hard requirement(s) failed, %d warning(s)",
			ErrHostNotReady, failures, warnings)
	}
	return nil
}

func (check HostCheck) evaluate(facts HostFacts, options CheckOptions) []finding {
	var result []finding
	add := func(section, level, message string) {
		result = append(result, finding{section: section, level: level, message: message})
	}
	if facts.OSName == "" {
		add("OS", "fail", "cannot read /etc/os-release")
	} else {
		add("OS", "ok", fmt.Sprintf("%s (kernel %s)", facts.OSName, facts.Kernel))
	}
	if len(facts.CPUFlags) == 0 {
		add("CPU virtualization", "warn",
			"no vmx/svm flags — VM mode and the hardware-accelerated emulator will not work")
	} else {
		add("CPU virtualization", "ok",
			"hardware virtualization flags present ("+strings.Join(facts.CPUFlags, " ")+")")
	}
	if facts.KVM {
		add("KVM device", "ok", "/dev/kvm present")
	} else {
		add("KVM device", "warn", "/dev/kvm missing — needed for VM mode and the Android emulator")
	}
	if check.Yard.NestedE2EVMs {
		for _, path := range []string{"/dev/kvm", "/dev/vsock", "/dev/vhost-vsock", "/dev/net/tun"} {
			if facts.NestedDevices[path] {
				add("Nested E2E VM devices", "ok", path+" present")
			} else {
				add("Nested E2E VM devices", "fail",
					path+" missing — nested E2E VMs cannot work on this host")
			}
		}
	}
	if facts.CPUs > 0 {
		add("Resources", "ok", fmt.Sprintf("%d CPU(s)", facts.CPUs))
	} else {
		add("Resources", "warn", "CPU count is unavailable")
	}
	if facts.MemoryBytes == 0 {
		add("Resources", "warn", "cannot read /proc/meminfo")
	} else {
		add("Resources", "ok", fmt.Sprintf("%.1f GiB RAM",
			float64(facts.MemoryBytes)/(1024*1024*1024)))
	}

	minimum := uint64(environmentInt(check.Environment, "MIN_DISK_GIB", 2))
	resumeMinimum := uint64(environmentInt(check.Environment, "RESUME_MIN_DISK_GIB", 1))
	recommended := uint64(environmentInt(check.Environment, "REC_DISK_GIB", 50))
	floor := minimum
	resume := options.BasePresent && resumeMinimum < minimum
	if resume {
		floor = resumeMinimum
	}
	free := facts.StorageFree / (1024 * 1024 * 1024)
	switch {
	case !facts.StorageKnown:
		add("Storage ("+check.Yard.Paths.StoragePath+")", "warn",
			"cannot determine free space for "+facts.StoragePath)
	case free >= recommended:
		add("Storage ("+check.Yard.Paths.StoragePath+")", "ok",
			fmt.Sprintf("%d GiB free on %s (fs: %s)", free, facts.StoragePath, facts.StorageFS))
	case free >= floor && resume && free < minimum:
		add("Storage ("+check.Yard.Paths.StoragePath+")", "warn",
			fmt.Sprintf("%d GiB free on %s — enough to repair this managed yard; a new yard needs >= %d GiB (fs: %s)",
				free, facts.StoragePath, minimum, facts.StorageFS))
	case free >= floor:
		add("Storage ("+check.Yard.Paths.StoragePath+")", "warn",
			fmt.Sprintf("%d GiB free on %s — enough for a base yard; the android profile wants >= %d GiB (fs: %s)",
				free, facts.StoragePath, recommended, facts.StorageFS))
	default:
		add("Storage ("+check.Yard.Paths.StoragePath+")", "fail",
			fmt.Sprintf("only %d GiB free on %s; need >= %d GiB (fs: %s)",
				free, facts.StoragePath, floor, facts.StorageFS))
	}

	if facts.IncusPresent {
		version := facts.IncusVersion
		if version == "" {
			version = "?"
		}
		add("Existing tools", "ok", "incus present ("+version+")")
		minimumVersion := environmentValue(check.Environment, "MIN_INCUS_VER", "6.0.6")
		if version != "?" && !versionAtLeast(version, minimumVersion) {
			add("Existing tools", "warn",
				fmt.Sprintf("incus %s < %s — nested Docker requires an Incus upgrade", version, minimumVersion))
		}
	} else {
		add("Existing tools", "warn", "incus not installed — yard init will install it")
	}
	if facts.DockerPresent {
		add("Existing tools", "warn", "docker present on host — Subyard runs Docker inside the yard")
	} else {
		add("Existing tools", "ok", "no host Docker (expected; Docker lives inside the yard)")
	}

	var duplicates []string
	for _, yard := range check.Yards {
		if yard.YardType == domain.YardRemote || yard.YardName == check.Yard.YardName {
			continue
		}
		if yard.SSHPort == check.Yard.SSHPort {
			duplicates = append(duplicates, yard.YardName)
		}
	}
	sort.Strings(duplicates)
	if len(duplicates) == 0 {
		add("Yard SSH port", "ok",
			fmt.Sprintf("SSH_PORT %d is unique across configured yards", check.Yard.SSHPort))
	} else {
		level := "warn"
		if options.Strict {
			level = "fail"
		}
		add("Yard SSH port", level,
			fmt.Sprintf("SSH_PORT %d also used by yard(s): %s", check.Yard.SSHPort,
				strings.Join(duplicates, " ")))
	}
	if facts.PortListening {
		level := "warn"
		if options.Strict && !options.BasePresent {
			level = "fail"
		}
		add("Yard SSH port", level,
			fmt.Sprintf("host port %d is already listening (another service, or this yard is running)",
				check.Yard.SSHPort))
	} else {
		add("Yard SSH port", "ok",
			fmt.Sprintf("host loopback port %d is free", check.Yard.SSHPort))
	}
	return result
}

func renderHostCheck(output io.Writer, findings []finding) (int, int) {
	if output == nil {
		output = io.Discard
	}
	fmt.Fprintln(output, "Subyard host check")
	section := ""
	failures := 0
	warnings := 0
	for _, item := range findings {
		if item.section != section {
			fmt.Fprintf(output, "\n%s:\n", item.section)
			section = item.section
		}
		fmt.Fprintf(output, "  [%s] %s\n", map[string]string{
			"ok": " ok ", "warn": "warn", "fail": "fail",
		}[item.level], item.message)
		switch item.level {
		case "fail":
			failures++
		case "warn":
			warnings++
		}
	}
	fmt.Fprintln(output)
	if failures == 0 {
		fmt.Fprint(output, "Host is ready.")
		if warnings != 0 {
			fmt.Fprintf(output, " (%d warning(s))", warnings)
		}
		fmt.Fprintln(output)
	} else {
		fmt.Fprintf(output, "Host is not ready: %d hard requirement(s) failed, %d warning(s).\n",
			failures, warnings)
	}
	return failures, warnings
}

func collectHostFacts(ctx context.Context, check HostCheck) (HostFacts, error) {
	facts := HostFacts{
		CPUs:          runtime.NumCPU(),
		NestedDevices: make(map[string]bool),
		StoragePath:   check.Yard.Paths.StoragePath,
	}
	facts.OSName = osReleaseName("/etc/os-release")
	var uts syscall.Utsname
	if syscall.Uname(&uts) == nil {
		facts.Kernel = utsString(uts.Release[:])
	}
	cpuInfo, _ := os.ReadFile("/proc/cpuinfo")
	for _, flag := range []string{"svm", "vmx"} {
		if fieldPresent(string(cpuInfo), flag) {
			facts.CPUFlags = append(facts.CPUFlags, flag)
		}
	}
	facts.KVM = pathExists("/dev/kvm")
	for _, path := range []string{"/dev/kvm", "/dev/vsock", "/dev/vhost-vsock", "/dev/net/tun"} {
		facts.NestedDevices[path] = charDevice(path)
	}
	facts.MemoryBytes = memoryBytes("/proc/meminfo")
	probe := facts.StoragePath
	for {
		if info, err := os.Stat(probe); err == nil && info.IsDir() {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	facts.StoragePath = probe
	var stat syscall.Statfs_t
	if syscall.Statfs(probe, &stat) == nil {
		facts.StorageFree = stat.Bavail * uint64(stat.Bsize)
		facts.StorageFS = fmt.Sprintf("0x%x", stat.Type)
		facts.StorageKnown = true
	}
	if binary, err := exec.LookPath("incus"); err == nil {
		facts.IncusPresent = true
		if check.Incus != nil {
			if server, serverErr := check.Incus.Server(ctx); serverErr == nil {
				facts.IncusVersion = server.Version
			}
		}
		if facts.IncusVersion == "" {
			version, _ := exec.CommandContext(ctx, binary, "--version").Output()
			facts.IncusVersion = strings.TrimSpace(string(version))
		}
	}
	_, dockerErr := exec.LookPath("docker")
	facts.DockerPresent = dockerErr == nil
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(check.Yard.SSHPort))
	connection, err := (&net.Dialer{Timeout: 200 * time.Millisecond}).DialContext(ctx, "tcp", address)
	if err == nil {
		facts.PortListening = true
		_ = connection.Close()
	}
	return facts, nil
}

func osReleaseName(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		name, value, found := strings.Cut(scanner.Text(), "=")
		if found && name == "PRETTY_NAME" {
			if decoded, err := strconv.Unquote(value); err == nil {
				return decoded
			}
			return strings.Trim(value, `"'`)
		}
	}
	return ""
}

func memoryBytes(path string) uint64 {
	file, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var kib uint64
		if _, err := fmt.Sscanf(scanner.Text(), "MemTotal: %d kB", &kib); err == nil {
			return kib * 1024
		}
	}
	return 0
}

func fieldPresent(contents, wanted string) bool {
	for _, field := range strings.Fields(contents) {
		if field == wanted {
			return true
		}
	}
	return false
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func charDevice(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func utsString(value []int8) string {
	var result strings.Builder
	for _, character := range value {
		if character == 0 {
			break
		}
		result.WriteByte(byte(character))
	}
	return result.String()
}

func environmentInt(environment map[string]string, name string, fallback int) int {
	value, err := strconv.Atoi(environment[name])
	if err != nil || value < 0 {
		return fallback
	}
	return value
}

func environmentValue(environment map[string]string, name, fallback string) string {
	if environment[name] != "" {
		return environment[name]
	}
	return fallback
}

func versionAtLeast(actual, minimum string) bool {
	actualParts := versionParts(actual)
	minimumParts := versionParts(minimum)
	for index := 0; index < len(actualParts) || index < len(minimumParts); index++ {
		var left, right int
		if index < len(actualParts) {
			left = actualParts[index]
		}
		if index < len(minimumParts) {
			right = minimumParts[index]
		}
		if left != right {
			return left > right
		}
	}
	return true
}

func versionParts(value string) []int {
	var result []int
	for _, part := range strings.FieldsFunc(value, func(character rune) bool {
		return character < '0' || character > '9'
	}) {
		number, err := strconv.Atoi(part)
		if err == nil {
			result = append(result, number)
		}
	}
	return result
}

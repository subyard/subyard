package testvmsruntime

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const brokerMemorySlice = "subyardtestvms.slice"
const brokerRAMBytes = uint64(2 << 30)
const brokerSwapBytes = uint64(2 << 30)
const brokerInputRoot = "/run/subyard-test-vms-workers"
const brokerWorkerPrefix = "subyard-test-vms-worker-"

// Incus supplies system credentials to container exec callers as well as PID1.
// They are ambient launcher metadata, never our private request handoff.
const incusSystemCredentialDirectory = "/dev/.incus-systemd-credentials"

var restoredBrokerInputs *brokerInputs

type brokerInputs struct {
	BridgePID      string            `json:"bridge_pid"`
	BridgeStart    string            `json:"bridge_start"`
	RequesterPID   string            `json:"requester_pid"`
	RequesterStart string            `json:"requester_start"`
	Environment    map[string]string `json:"environment"`
}

var brokerInputNames = []string{"SSH_ORIGINAL_COMMAND", "SUBYARD_TEST_VMS_CONFIG", "WANT_ENABLED", "WANT_ENGINE_HASH", "WANT_DIAGNOSTIC", "SUBYARD_SPOOL_ACK"}

type BrokerMemoryCapacity struct {
	Current                 uint64 `json:"current_bytes"`
	RAMLimit                uint64 `json:"ram_limit_bytes"`
	SwapCurrent             uint64 `json:"swap_current_bytes"`
	SwapLimit               uint64 `json:"swap_limit_bytes"`
	EffectiveRAMLimit       uint64 `json:"effective_ram_limit_bytes"`
	AllocationScopeVerified bool   `json:"allocation_scope_verified"`
}

func cgroupCounter(root, name string) (uint64, error) {
	body, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(body)), 10, 64)
}

func brokerMemoryCapacity(root string) (BrokerMemoryCapacity, error) {
	var value BrokerMemoryCapacity
	slice := filepath.Join(root, brokerMemorySlice)
	var err error
	value.RAMLimit, err = cgroupCounter(slice, "memory.max")
	if err != nil || value.RAMLimit != brokerRAMBytes {
		return value, errors.New("broker RAM limit is unavailable or differs")
	}
	value.SwapLimit, err = cgroupCounter(slice, "memory.swap.max")
	if err != nil || value.SwapLimit != brokerSwapBytes {
		return value, errors.New("broker swap limit is unavailable or differs")
	}
	value.Current, err = cgroupCounter(slice, "memory.current")
	if err != nil {
		return value, errors.New("broker RAM usage is unavailable")
	}
	value.SwapCurrent, err = cgroupCounter(slice, "memory.swap.current")
	if err != nil {
		return value, errors.New("broker swap usage is unavailable")
	}
	value.EffectiveRAMLimit = value.RAMLimit
	// The slice is deliberately top-level. Its visible allocation root is the
	// sole ancestor; a hidden outer-host ceiling remains unavailable evidence.
	body, err := os.ReadFile(filepath.Join(root, "memory.max"))
	if err == nil && strings.TrimSpace(string(body)) != "max" {
		maximum, parseErr := strconv.ParseUint(strings.TrimSpace(string(body)), 10, 64)
		if parseErr != nil || maximum < brokerRAMBytes {
			return value, errors.New("allocation RAM limit is below the broker budget")
		}
		value.EffectiveRAMLimit = min(value.EffectiveRAMLimit, maximum)
	} else if err != nil {
		controllers, controllerErr := os.ReadFile(filepath.Join(root, "cgroup.controllers"))
		if !os.IsNotExist(err) || controllerErr != nil || !strings.Contains(" "+strings.TrimSpace(string(controllers))+" ", " memory ") {
			return value, errors.New("allocation memory controller is unavailable")
		}
	}
	value.AllocationScopeVerified = true
	return value, nil
}

func brokerCgroupPath(membership string) (string, error) {
	path := ""
	for _, line := range strings.Split(membership, "\n") {
		if strings.HasPrefix(line, "0::/") {
			if path != "" {
				return "", errors.New("broker cgroup membership is ambiguous")
			}
			path = strings.TrimPrefix(line, "0::")
		}
	}
	if path == "" || strings.Contains(path, "..") || strings.Contains(path, " (deleted)") {
		return "", errors.New("broker cgroup membership is invalid")
	}
	return path, nil
}

func brokerProcessStart(data []byte) string {
	closing := strings.LastIndex(string(data), ") ")
	if closing < 0 {
		return ""
	}
	fields := strings.Fields(string(data[closing+2:]))
	if len(fields) < 20 || fields[0] == "Z" {
		return ""
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || start == 0 {
		return ""
	}
	return fields[19]
}

func brokerBridgeAlive(pid, start string) bool {
	body, err := os.ReadFile(filepath.Join("/proc", pid, "stat"))
	return err == nil && brokerProcessStart(body) == start
}

func brokerMemoryCommand(executable string, arguments []string, pid, start string) []string {
	command := []string{"systemd-run", "--quiet", "--wait", "--pipe", "--collect", "--service-type=exec",
		"--expand-environment=no", "--slice=" + brokerMemorySlice,
		"--unit=" + brokerWorkerPrefix + pid + "-" + start + ".service",
		"--property=KillMode=control-group", "--property=TimeoutStopSec=10s"}
	// Unit properties contain only a protected file path; request capabilities
	// must not enter unit Environment, which is exposed through systemd D-Bus.
	command = append(command, "--property=LoadCredential=subyard-broker-request:"+brokerInputPath(pid, start))
	command = append(command, "--", executable)
	return append(command, arguments...)
}

func brokerNeedsNamespaceWaiter(path, directory string, pid, parent int) bool {
	own := "/" + brokerMemorySlice
	return pid > 1 && parent == 0 && directory == incusSystemCredentialDirectory &&
		path != own && !strings.HasPrefix(path, own+"/")
}

func brokerNamespaceWaiterCommand(executable string, arguments []string) []string {
	// Incus attaches a command whose parent is outside its PID namespace. Keep
	// that attached PID as a transport waiter and give its child a visible parent.
	// Save stdin before creating the async job: /bin/sh can replace descriptor 0
	// with /dev/null before applying that job's redirections.
	command := []string{"/bin/sh", "-c", `trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
exec 3<&0
"$@" <&3 3<&- &
exec 3<&-
wait "$!"`, "subyard-broker-transport", executable}
	return append(command, arguments...)
}

// EnterBrokerMemoryScope execs a native stdio transport. PID1 creates the actual
// worker in our bounded slice; the transport retains all original caller limits.
// It never moves a process out of an operator's CPU, memory, PID or I/O ceilings.
// The child verifies its exact unit/membership before accepting inherited inputs.
func EnterBrokerMemoryScope(configPath string) error {
	const root = "/sys/fs/cgroup"
	var stat unix.Statfs_t
	if err := unix.Statfs(root, &stat); err != nil || stat.Type != unix.CGROUP2_SUPER_MAGIC {
		return errors.New("broker requires cgroup v2 memory accounting")
	}
	if _, err := brokerMemoryCapacity(root); err != nil {
		return err
	}
	body, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return errors.New("broker cgroup membership is unavailable")
	}
	path, err := brokerCgroupPath(string(body))
	if err != nil {
		return err
	}
	// Installed lifecycle/reaper units are already born inside this budget.
	// In particular ExecStop must not launch a new dependency while the slice
	// is stopping; systemd owns these units' cancellation and child cleanup.
	if path == "/"+brokerMemorySlice+"/subyard-test-vms-broker.service" ||
		path == "/"+brokerMemorySlice+"/subyard-test-vms-lease-reaper.service" {
		return nil
	}
	if strings.HasPrefix(path, "/"+brokerMemorySlice+"/") {
		inputs := restoredBrokerInputs
		if inputs == nil {
			return errors.New("broker worker request credential is unavailable")
		}
		pid, start := inputs.BridgePID, inputs.BridgeStart
		if path != "/"+brokerMemorySlice+"/"+brokerWorkerPrefix+pid+"-"+start+".service" || !inputs.alive() {
			return errors.New("broker worker transport identity is unavailable or stale")
		}
		// systemd-run --pipe does not bind service lifetime to the waiting
		// transport. A dead/disconnected transport must not leave work running.
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for range ticker.C {
				if !inputs.alive() {
					// Let the existing operation context cancel native commands and
					// persist fencing/quarantine, then bound any unresponsive cleanup.
					_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
					time.Sleep(10 * time.Second)
					os.Exit(1)
				}
			}
		}()
		return nil
	}
	if path == "/"+brokerMemorySlice {
		return errors.New("broker slice cannot host the transport directly")
	}
	executable, err := os.Executable()
	if err != nil {
		return errors.New("broker executable is unavailable")
	}
	if brokerNeedsNamespaceWaiter(path, os.Getenv("CREDENTIALS_DIRECTORY"), os.Getpid(), os.Getppid()) {
		command := brokerNamespaceWaiterCommand(executable, os.Args[1:])
		return syscall.Exec(command[0], command, os.Environ())
	}
	body, err = os.ReadFile("/proc/self/stat")
	start := brokerProcessStart(body)
	if err != nil || start == "" {
		return errors.New("broker transport identity is unavailable")
	}
	pid := strconv.Itoa(os.Getpid())
	if os.Getpid() <= 1 || os.Getppid() <= 1 {
		return errors.New("broker requester identity is unavailable")
	}
	requester := strconv.Itoa(os.Getppid())
	body, err = os.ReadFile(filepath.Join("/proc", requester, "stat"))
	requesterStart := brokerProcessStart(body)
	if err != nil || requesterStart == "" {
		return errors.New("broker requester identity is unavailable")
	}
	inputs := captureBrokerInputs(pid, start, requester, requesterStart, configPath)
	if err := writeBrokerInputs(inputs); err != nil {
		return err
	}
	defer os.Remove(brokerInputPath(pid, start)) // Exec success leaves cleanup to the child/reaper.
	command := brokerMemoryCommand(executable, os.Args[1:], pid, start)
	binary, err := exec.LookPath(command[0])
	if err != nil {
		return errors.New("native broker process transport is unavailable")
	}
	return syscall.Exec(binary, command, os.Environ())
}

// The caller has already loaded this exact config before native launch. Carry
// its path explicitly in the private handoff; unit environment/argv remain safe.
func captureBrokerInputs(pid, start, requester, requesterStart, configPath string) brokerInputs {
	inputs := brokerInputs{BridgePID: pid, BridgeStart: start, RequesterPID: requester, RequesterStart: requesterStart,
		Environment: map[string]string{"SUBYARD_TEST_VMS_CONFIG": configPath}}
	for _, name := range brokerInputNames {
		if name != "SUBYARD_TEST_VMS_CONFIG" {
			if value, present := os.LookupEnv(name); present {
				inputs.Environment[name] = value
			}
		}
	}
	return inputs
}

func restoreBrokerInputEnvironment(inputs brokerInputs) error {
	if inputs.Environment["SUBYARD_TEST_VMS_CONFIG"] == "" {
		return errors.New("broker request config input is unavailable")
	}
	for name, value := range inputs.Environment {
		if err := os.Setenv(name, value); err != nil {
			return errors.New("broker request input cannot be restored")
		}
	}
	return nil
}

func brokerInputPath(pid, start string) string {
	// The scoped native drop-in resolves %N to the unit name without .service.
	return filepath.Join(brokerInputRoot, brokerWorkerPrefix+pid+"-"+start+".json")
}

func (inputs brokerInputs) alive() bool {
	return brokerBridgeAlive(inputs.BridgePID, inputs.BridgeStart) && brokerBridgeAlive(inputs.RequesterPID, inputs.RequesterStart)
}

func decodeBrokerInputs(data []byte) (brokerInputs, error) {
	var inputs brokerInputs
	if len(data) > 80<<10 {
		return inputs, errors.New("broker request credential exceeds its bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&inputs) != nil || decoder.Decode(new(any)) != io.EOF {
		return inputs, errors.New("broker request credential is invalid")
	}
	for _, value := range []string{inputs.BridgePID, inputs.BridgeStart, inputs.RequesterPID, inputs.RequesterStart} {
		number, err := strconv.ParseUint(value, 10, 64)
		if err != nil || number == 0 || strconv.FormatUint(number, 10) != value {
			return inputs, errors.New("broker requester identity is invalid")
		}
	}
	if inputs.BridgePID == "1" || inputs.RequesterPID == "1" {
		return inputs, errors.New("broker requester identity is invalid")
	}
	for name, value := range inputs.Environment {
		allowed := false
		for _, known := range brokerInputNames {
			if name == known {
				allowed = true
			}
		}
		if !allowed || strings.ContainsRune(value, 0) || (name != "SUBYARD_SPOOL_ACK" && len(value) > 8<<10) || len(value) > 64<<10 {
			return inputs, errors.New("broker request input is invalid")
		}
	}
	return inputs, nil
}

func brokerInputDirectory() error {
	return privateBrokerInputDirectory(brokerInputRoot, 0)
}

func privateBrokerInputDirectory(root string, owner uint32) error {
	// Only the final leaf may be absent: /run itself must already be safe.
	if err := rejectSymlinkPath(filepath.Dir(root)); err != nil {
		return errors.New("broker request directory is unsafe")
	}
	if err := os.Mkdir(root, 0700); err != nil {
		if !os.IsExist(err) {
			return errors.New("broker request directory is unavailable")
		}
	} else if os.Chmod(root, 0700) != nil {
		return errors.New("broker request directory permissions cannot be set")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return errors.New("broker request directory permissions are unsafe")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != owner {
		return errors.New("broker request directory ownership is unsafe")
	}
	return nil
}

func readBrokerInputFile(path string) ([]byte, error) {
	return readPrivateBrokerInputFile(path, 0)
}

func readPrivateBrokerInputFile(path string, owner uint32) ([]byte, error) {
	if err := rejectSymlinkPath(path); err != nil {
		return nil, errors.Join(errors.New("broker request credential path is unsafe"), err)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.Join(errors.New("broker request credential is unavailable"), err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	var stat unix.Stat_t
	if unix.Fstat(fd, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != owner || stat.Nlink != 1 || stat.Mode&0077 != 0 {
		return nil, errors.New("broker request credential permissions are unsafe")
	}
	body, err := io.ReadAll(io.LimitReader(file, (80<<10)+1))
	if err != nil || len(body) > 80<<10 {
		return nil, errors.New("broker request credential exceeds its bound")
	}
	return body, nil
}

func writeBrokerInputs(inputs brokerInputs) error {
	if err := brokerInputDirectory(); err != nil {
		return err
	}
	body, err := json.Marshal(inputs)
	if err != nil {
		return errors.New("broker request cannot be encoded")
	}
	if _, err := decodeBrokerInputs(body); err != nil {
		return err
	}
	path := brokerInputPath(inputs.BridgePID, inputs.BridgeStart)
	return publishBrokerInputs(path, body)
}

func publishBrokerInputs(path string, body []byte) error {
	staging := path + ".tmp"
	file, err := os.OpenFile(staging, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("broker request handoff cannot be created")
	}
	defer os.Remove(staging)
	defer file.Close()
	if file.Chmod(0600) != nil {
		return errors.New("broker request handoff permissions cannot be set")
	}
	if _, err := file.Write(body); err != nil {
		return errors.New("broker request handoff cannot be written")
	}
	if file.Sync() != nil || file.Close() != nil {
		return errors.New("broker request handoff cannot be completed")
	}
	if unix.Renameat2(unix.AT_FDCWD, staging, unix.AT_FDCWD, path, unix.RENAME_NOREPLACE) != nil {
		return errors.New("broker request handoff cannot be published")
	}
	return nil
}

// A native worker must use its own credential directory. The exact Incus
// system context is permitted only for launchers outside our entire slice;
// its contents grant no authority and are never read or forwarded.
func brokerRequestCredentialScope(path, directory string) (bool, error) {
	own := "/" + brokerMemorySlice
	if path != own && !strings.HasPrefix(path, own+"/") {
		if directory == "" || directory == incusSystemCredentialDirectory {
			return false, nil
		}
		return false, errors.New("broker request credential launcher context is invalid")
	}
	unit := strings.TrimPrefix(path, own+"/")
	if directory == "" && (unit == "subyard-test-vms-broker.service" || unit == "subyard-test-vms-lease-reaper.service") {
		return false, nil // Installed units use their direct lifecycle, without a request handoff.
	}
	if !strings.HasPrefix(unit, brokerWorkerPrefix) || !strings.HasSuffix(unit, ".service") || strings.Contains(unit, "/") {
		return false, errors.New("broker request credential worker membership is invalid")
	}
	if directory != "" && directory != filepath.Join("/run/credentials", unit) {
		return false, errors.New("broker request credential worker directory is invalid")
	}
	return true, nil
}

// RestoreBrokerMemoryInputs runs before config loading because setup supplies a
// staged config path. Native credentials are root-private and excluded from D-Bus
// Environment/ExecStart properties; membership is verified before using them.
func RestoreBrokerMemoryInputs() error {
	directory := os.Getenv("CREDENTIALS_DIRECTORY")
	body, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return errors.New("broker credential membership is unavailable")
	}
	path, err := brokerCgroupPath(string(body))
	if err != nil {
		return err
	}
	request, err := brokerRequestCredentialScope(path, directory)
	if err != nil {
		return err
	}
	if !request {
		return nil
	}
	// Membership fixes the native path even when the manager omits its hint.
	// Every nonempty, foreign hint was rejected above; file/identity guards stay mandatory.
	if directory == "" {
		directory = filepath.Join("/run/credentials", filepath.Base(path))
	}
	body, err = readBrokerInputFile(filepath.Join(directory, "subyard-broker-request"))
	if err != nil {
		return err
	}
	inputs, err := decodeBrokerInputs(body)
	if err != nil {
		return err
	}
	if path != "/"+brokerMemorySlice+"/"+brokerWorkerPrefix+inputs.BridgePID+"-"+inputs.BridgeStart+".service" || !inputs.alive() {
		return errors.New("broker request credential identity is stale")
	}
	if _, err := brokerMemoryCapacity("/sys/fs/cgroup"); err != nil {
		return err
	}
	if inputs.Environment["SUBYARD_TEST_VMS_CONFIG"] == "" {
		return errors.New("broker request config input is unavailable")
	}
	if err := brokerInputDirectory(); err != nil {
		return err
	}
	source := brokerInputPath(inputs.BridgePID, inputs.BridgeStart)
	if _, err := readBrokerInputFile(source); err != nil {
		return err
	}
	if err := os.Remove(source); err != nil {
		return errors.New("broker request handoff cannot be removed")
	}
	if err := restoreBrokerInputEnvironment(inputs); err != nil {
		return err
	}
	restoredBrokerInputs = &inputs
	return nil
}

// The existing minute reaper bounds crash leftovers from failed native launches.
func CleanupBrokerMemoryInputs() error {
	return cleanupBrokerMemoryInputs(brokerInputRoot, 0)
}

func cleanupBrokerMemoryInputs(root string, owner uint32) error {
	if _, err := os.Lstat(root); os.IsNotExist(err) {
		return nil
	}
	if err := privateBrokerInputDirectory(root, owner); err != nil {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return errors.New("broker handoff cleanup is unavailable")
	}
	var failures error
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		pid, start, staging, err := brokerInputFilename(entry.Name())
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		body, err := readPrivateBrokerInputFile(path, owner)
		if errors.Is(err, os.ErrNotExist) {
			continue // The credential consumer has removed its source already.
		}
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		remove := !brokerBridgeAlive(pid, start)
		if !staging && !remove {
			inputs, decodeErr := decodeBrokerInputs(body)
			if decodeErr != nil || inputs.BridgePID != pid || inputs.BridgeStart != start {
				failures = errors.Join(failures, errors.New("broker handoff cleanup ownership is invalid"))
				continue
			}
			remove = !inputs.alive()
		}
		// A dead writer's partial staging/final body need not decode. Its
		// protected filename binds the orphan to a verified dead PID/start.
		if remove {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				failures = errors.Join(failures, errors.New("broker handoff cleanup failed"))
			}
		}
	}
	return failures
}

func brokerInputFilename(name string) (pid, start string, staging bool, err error) {
	staging = strings.HasSuffix(name, ".tmp")
	name = strings.TrimSuffix(name, ".tmp")
	if !strings.HasPrefix(name, brokerWorkerPrefix) || !strings.HasSuffix(name, ".json") {
		return "", "", false, errors.New("broker handoff cleanup ownership is invalid")
	}
	parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(name, brokerWorkerPrefix), ".json"), "-")
	if len(parts) != 2 {
		return "", "", false, errors.New("broker handoff cleanup ownership is invalid")
	}
	for index, value := range parts {
		number, parseErr := strconv.ParseUint(value, 10, 64)
		if parseErr != nil || number == 0 || (index == 0 && number <= 1) || strconv.FormatUint(number, 10) != value {
			return "", "", false, errors.New("broker handoff cleanup ownership is invalid")
		}
	}
	return parts[0], parts[1], staging, nil
}

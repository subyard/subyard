package hostruntime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// VMCPUWeight moves only the verified QEMU process into a root-slice scope.
// A task nice value cannot raise its share against sibling container cgroups.
// Leaving an ancestor with aggregate limits would bypass those limits, so such
// hosts fail closed instead of copying a collective ceiling onto one VM.
func VMCPUWeight(ctx context.Context, pid int64, uuid string, weight int, apply bool) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	source, err := exec.CommandContext(ctx, "systemctl", "show", "incus.service", "--property=ControlGroup", "--value").Output()
	if err != nil {
		return false, errors.New("cannot inspect the Incus service cgroup")
	}
	return vmCPUWeight(ctx, "/proc", "/sys/fs/cgroup", strings.TrimSpace(string(source)), pid, uuid, weight, apply,
		func(ctx context.Context, name string, args ...string) error {
			output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
			if err != nil {
				if len(output) > 2048 {
					output = output[:2048]
				}
				return fmt.Errorf("VM CPU scheduling %s failed: %s: %w", name, strings.TrimSpace(string(output)), err)
			}
			return nil
		})
}

func vmCPUWeight(ctx context.Context, procRoot, cgroupRoot, sourceCgroup string, pid int64, uuid string, weight int, apply bool,
	run func(context.Context, string, ...string) error) (bool, error) {
	if pid <= 1 || uuid == "" || weight < 1 || weight > 10000 {
		return false, errors.New("invalid VM CPU scheduling identity or weight")
	}
	process := filepath.Join(procRoot, strconv.FormatInt(pid, 10))
	stat, err := os.ReadFile(filepath.Join(process, "stat"))
	if err != nil {
		return false, errors.New("cannot inspect the VM host process")
	}
	args, err := os.ReadFile(filepath.Join(process, "cmdline"))
	if err != nil || !qemuIdentity(args, uuid) {
		return false, errors.New("VM host PID does not identify the owned QEMU process")
	}
	cgroup, err := os.ReadFile(filepath.Join(process, "cgroup"))
	if err != nil {
		return false, errors.New("cannot inspect the VM host cgroup")
	}
	path := strings.TrimSuffix(string(cgroup), "\n")
	if !strings.HasPrefix(path, "0::/") || strings.Contains(path, "\n") {
		return false, errors.New("VM CPU weight requires unified cgroup v2")
	}
	path = strings.TrimPrefix(path, "0::")
	if filepath.Clean(path) != path {
		return false, errors.New("invalid VM host cgroup path")
	}
	if !strings.HasPrefix(sourceCgroup, "/") || filepath.Clean(sourceCgroup) != sourceCgroup || filepath.Base(sourceCgroup) != "incus.service" {
		return false, errors.New("invalid Incus service cgroup identity")
	}
	if err := vmCPUAncestorsUnrestricted(cgroupRoot, sourceCgroup); err != nil {
		return false, err
	}
	digest := sha256.Sum256([]byte(uuid + ":" + strconv.FormatInt(pid, 10)))
	scope := fmt.Sprintf("subyardvm-%x.scope", digest[:12])
	target := "/" + scope
	current := filepath.Join(cgroupRoot, path)
	if path == target {
		members, err := os.ReadFile(filepath.Join(current, "cgroup.procs"))
		if err != nil || strings.TrimSpace(string(members)) != strconv.FormatInt(pid, 10) {
			return false, errors.New("VM CPU scope contains an unexpected process")
		}
		value, err := os.ReadFile(filepath.Join(current, "cpu.weight"))
		if err != nil {
			return false, errors.New("cannot inspect the effective VM CPU weight")
		}
		if strings.TrimSpace(string(value)) == strconv.Itoa(weight) {
			return true, nil
		}
	} else {
		if path != sourceCgroup {
			return false, errors.New("VM CPU weight requires QEMU in incus.service or its owned Subyard scope")
		}
	}
	if !apply {
		return false, nil
	}
	if os.Geteuid() != 0 {
		return false, errors.New("applying VM host CPU weight requires root")
	}
	executable, err := os.Readlink(filepath.Join(process, "exe"))
	if err != nil || !strings.HasPrefix(filepath.Base(executable), "qemu-system-") {
		return false, errors.New("VM host executable is not QEMU")
	}
	// Recheck immediately before the privileged boundary. A stopped/replaced
	// QEMU or changed process identity must never move an unrelated host process.
	latest, err := os.ReadFile(filepath.Join(process, "stat"))
	if err != nil || processStart(latest) == "" || processStart(latest) != processStart(stat) {
		return false, errors.New("VM host process changed before CPU scheduling apply")
	}
	latestGroup, err := os.ReadFile(filepath.Join(process, "cgroup"))
	if err != nil || string(latestGroup) != string(cgroup) {
		return false, errors.New("VM host cgroup changed before CPU scheduling apply")
	}
	if path == target {
		err = run(ctx, "systemctl", "set-property", "--runtime", scope, "CPUWeight="+strconv.Itoa(weight))
	} else {
		err = run(ctx, "busctl", "--system", "--", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
			"org.freedesktop.systemd1.Manager", "StartTransientUnit", "ssa(sv)a(sa(sv))", scope, "fail", "6",
			"PIDs", "au", "1", strconv.FormatInt(pid, 10), "CPUWeight", "t", strconv.Itoa(weight),
			"Slice", "s", "-.slice", "Before", "as", "1", "incus.service",
			"Description", "s", "Subyard VM CPU scheduling", "CollectMode", "s", "inactive-or-failed", "0")
		if err == nil {
			// StartTransientUnit enqueues a job. Join it before checking the kernel
			// state so a normal systemd scheduling delay is not reported as drift.
			err = run(ctx, "systemctl", "start", scope)
		}
	}
	if err != nil {
		return false, err
	}
	return vmCPUWeight(ctx, procRoot, cgroupRoot, sourceCgroup, pid, uuid, weight, false, run)
}

func qemuIdentity(raw []byte, uuid string) bool {
	args := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
	if len(args) == 0 || !strings.HasPrefix(filepath.Base(args[0]), "qemu-system-") {
		return false
	}
	for i := 1; i+1 < len(args); i++ {
		if args[i] == "-uuid" && args[i+1] == uuid {
			return true
		}
	}
	return false
}

func processStart(raw []byte) string {
	_, rest, ok := strings.Cut(string(raw), ") ")
	if !ok {
		return ""
	}
	fields := strings.Fields(rest)
	if len(fields) < 20 {
		return ""
	}
	return fields[19]
}

func vmCPUAncestorsUnrestricted(root, path string) error {
	for path != "/" {
		directory := filepath.Join(root, path)
		entries, err := os.ReadDir(directory)
		if err != nil {
			return errors.New("cannot verify the VM source cgroup resource ceilings")
		}
		for _, entry := range entries {
			name := entry.Name()
			if name == "cpu.uclamp.max" || name == "cpuset.cpus.partition" {
				value, err := os.ReadFile(filepath.Join(directory, name))
				want := "member"
				if name == "cpu.uclamp.max" {
					want = "max"
				}
				actual := strings.TrimSpace(string(value))
				if err != nil || (actual != want && !(name == "cpu.uclamp.max" && actual == "100.00")) {
					return fmt.Errorf("VM CPU scope would bypass an ancestor %s restriction", name)
				}
			}
			if name == "cpu.max" || name == "memory.max" || name == "memory.high" ||
				name == "memory.swap.max" || name == "memory.zswap.max" || name == "pids.max" ||
				name == "io.max" || name == "rdma.max" || name == "misc.max" ||
				(strings.HasPrefix(name, "hugetlb.") && strings.HasSuffix(name, ".max")) {
				value, err := os.ReadFile(filepath.Join(directory, name))
				if err != nil || !unlimitedCgroupValue(name, string(value)) {
					return fmt.Errorf("VM CPU scope would bypass an ancestor %s ceiling; retain the VM in its existing group", name)
				}
			}
			if name == "cpuset.cpus.effective" || name == "cpuset.mems.effective" {
				value, err := os.ReadFile(filepath.Join(directory, name))
				outer, outerErr := os.ReadFile(filepath.Join(root, name))
				if err != nil || outerErr != nil || strings.TrimSpace(string(value)) != strings.TrimSpace(string(outer)) {
					return fmt.Errorf("VM CPU scope would bypass an ancestor %s restriction", name)
				}
			}
		}
		path = filepath.Dir(path)
	}
	return nil
}

func unlimitedCgroupValue(name, value string) bool {
	fields := strings.Fields(value)
	if name == "cpu.max" {
		return len(fields) == 2 && fields[0] == "max"
	}
	if name == "io.max" || name == "rdma.max" || name == "misc.max" {
		for _, line := range strings.Split(strings.TrimSpace(value), "\n") {
			parts := strings.Fields(line)
			if len(parts) == 0 {
				continue
			}
			for _, field := range parts[1:] {
				_, limit, found := strings.Cut(field, "=")
				if !found {
					limit = field
				}
				if limit != "max" {
					return false
				}
			}
		}
		return true
	}
	return len(fields) == 1 && fields[0] == "max"
}

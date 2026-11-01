package hostruntime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
)

func TestVMCPUWeightReadinessAndOwnership(t *testing.T) {
	const uuid = "12345678-1234-1234-1234-123456789abc"
	for _, scenario := range []string{"drift", "converged", "wrong weight", "foreign member", "wrong uuid", "wrong binary", "foreign cgroup", "v1"} {
		t.Run(scenario, func(t *testing.T) {
			root := testkit.TempDir(t)
			proc := filepath.Join(root, "proc")
			cgroups := filepath.Join(root, "cgroup")
			write := func(path, content string) { writeVMCPUFixture(t, path, content) }
			write(filepath.Join(proc, "123", "stat"), "123 (qemu-system-x86) S "+strings.Repeat("0 ", 18)+"321\n")
			cmd := "/usr/bin/qemu-system-x86_64\x00-uuid\x00" + uuid + "\x00"
			if scenario == "wrong uuid" {
				cmd = "/usr/bin/qemu-system-x86_64\x00-uuid\x00other\x00"
			}
			if scenario == "wrong binary" {
				cmd = "/usr/bin/other\x00-uuid\x00" + uuid + "\x00"
			}
			write(filepath.Join(proc, "123", "cmdline"), cmd)
			path := "/system.slice/incus.service"
			write(filepath.Join(cgroups, "system.slice", "cpu.max"), "max 100000\n")
			write(filepath.Join(cgroups, "system.slice", "incus.service", "memory.max"), "max\n")
			if scenario == "converged" || scenario == "wrong weight" || scenario == "foreign member" {
				digest := sha256.Sum256([]byte(uuid + ":123"))
				path = "/subyardvm-" + fmt.Sprintf("%x", digest[:12]) + ".scope"
				value := "1000\n"
				if scenario == "wrong weight" {
					value = "100\n"
				}
				write(filepath.Join(cgroups, path, "cpu.weight"), value)
				members := "123\n"
				if scenario == "foreign member" {
					members += "456\n"
				}
				write(filepath.Join(cgroups, path, "cgroup.procs"), members)
			}
			if scenario == "foreign cgroup" {
				path = "/user.slice/foreign.scope"
			}
			group := "0::" + path + "\n"
			if scenario == "v1" {
				group = "1:cpu:/incus.service\n"
			}
			write(filepath.Join(proc, "123", "cgroup"), group)
			ready, err := vmCPUWeight(context.Background(), proc, cgroups, "/system.slice/incus.service", 123, uuid, 1000, false,
				func(context.Context, string, ...string) error { t.Fatal("readiness mutated host"); return nil })
			wantError := scenario != "drift" && scenario != "converged" && scenario != "wrong weight"
			if (err != nil) != wantError || ready != (scenario == "converged") {
				t.Fatalf("readiness=%t error=%v", ready, err)
			}
		})
	}
}

func TestVMCPUScopeRefusesAncestorResourceCeilings(t *testing.T) {
	for name, value := range map[string]string{
		"cpu.max": "200000 100000", "memory.max": "1073741824", "memory.high": "536870912",
		"memory.swap.max": "0", "pids.max": "2048", "io.max": "8:0 rbps=1000 wbps=max",
		"hugetlb.2MB.max": "0", "rdma.max": "mlx5_0 hca_handle=100 hca_object=max",
		"misc.max": "sev 10", "cpuset.cpus.effective": "0", "cpuset.mems.effective": "0",
	} {
		t.Run(name, func(t *testing.T) {
			root := testkit.TempDir(t)
			writeVMCPUFixture(t, filepath.Join(root, "system.slice", "incus.service", name), value)
			if strings.HasPrefix(name, "cpuset.") {
				writeVMCPUFixture(t, filepath.Join(root, name), "0-3")
			}
			if err := vmCPUAncestorsUnrestricted(root, "/system.slice/incus.service"); err == nil {
				t.Fatal("bypassed an aggregate ancestor ceiling")
			}
		})
	}
	root := testkit.TempDir(t)
	for _, path := range []string{"system.slice/cpu.max", "system.slice/incus.service/cpu.max"} {
		writeVMCPUFixture(t, filepath.Join(root, path), "max 100000")
	}
	if err := vmCPUAncestorsUnrestricted(root, "/system.slice/incus.service"); err != nil {
		t.Fatal(err)
	}
}

func writeVMCPUFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, []byte(content), 0o644)
}

func TestVMCPUMutatorInheritsValidatedStdinAndEnclosingProcessGroup(t *testing.T) {
	home := testkit.TempDir(t)
	_, release, err := uninstall.OwnerLock(context.Background(), home, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	owner, err := uninstall.BorrowOwnerLock(home)
	if err != nil || owner == nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err := uninstall.ValidateOwnerLockDescriptor(home, uint32(os.Getuid()), owner); err != nil {
		t.Fatal(err)
	}
	ctx := WithVMCPUOwnerStdin(context.Background(), owner)
	command := vmCPUCommand(ctx, "python3", "-B", "-c", `import json,os; s=os.fstat(0);print(json.dumps([s.st_dev,s.st_ino,os.getpgrp()]))`)
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	var actual []uint64
	if err := json.Unmarshal(output, &actual); err != nil {
		t.Fatal(err)
	}
	info, err := owner.Stat()
	if err != nil {
		t.Fatal(err)
	}
	expected := info.Sys().(*syscall.Stat_t)
	if len(actual) != 3 || actual[0] != uint64(expected.Dev) || actual[1] != expected.Ino || actual[2] != uint64(syscall.Getpgrp()) {
		t.Fatal("mutator changed original OFD or escaped enclosing group", actual)
	}
}

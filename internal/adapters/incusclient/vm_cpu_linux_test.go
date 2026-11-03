package incusclient

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Subyard/Subyard/internal/testkit"
)

// The fake keeps the part of sudo this contract depends on: it resolves the
// command by path and reports an unusable one itself, exiting 1.
const fakeResolvingSudo = `#!/bin/sh
[ "$1" = -n ] && [ "$2" = -- ] || exit 9
[ -x "$3" ] || { echo "sudo: $3: command not found" >&2; exit 1; }
cmp -s "$3" "$EXPECTED_ENGINE" || { echo "sudo fixture: $3 is not the running engine" >&2; exit 8; }
printf '%s\n' "$*" > "$SUDO_LOG"
`

// A release transition runs its engine from a sealed memfd, whose reported
// executable path does not exist for sudo to resolve.
func TestVMCPUHelperReachesSealedEngineThroughSudo(t *testing.T) {
	if os.Getenv("SUBYARD_TEST_VM_CPU_SEALED") == "1" {
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(self, "/memfd:") {
			t.Fatalf("fixture is not running the memfd image: %s", self)
		}
		if err := runVMCPUHelper(context.Background(), "subyard", "yard", "/run/incus.sock"); err != nil {
			t.Fatal(err)
		}
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	flags := unix.MFD_CLOEXEC | unix.MFD_ALLOW_SEALING
	fd, err := unix.MemfdCreate("subyard-vm-cpu-test", flags|unix.MFD_EXEC)
	if err == unix.EINVAL {
		fd, err = unix.MemfdCreate("subyard-vm-cpu-test", flags)
	}
	if err != nil {
		t.Fatal(err)
	}
	engine := os.NewFile(uintptr(fd), "subyard-vm-cpu-test")
	defer engine.Close()
	if _, err := io.Copy(engine, source); err != nil {
		t.Fatal(err)
	}
	if err := engine.Chmod(0o500); err != nil {
		t.Fatal(err)
	}
	root := testkit.TempDir(t)
	testkit.WriteFile(t, filepath.Join(root, "sudo"), []byte(fakeResolvingSudo), 0o700)
	log := filepath.Join(root, "sudo.log")
	command := exec.Command("/proc/self/fd/3", "-test.run=^TestVMCPUHelperReachesSealedEngineThroughSudo$", "-test.v")
	command.ExtraFiles = []*os.File{engine}
	command.Env = append(os.Environ(), "SUBYARD_TEST_VM_CPU_SEALED=1", "EXPECTED_ENGINE="+executable, "SUDO_LOG="+log,
		"PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("sealed engine could not reach its privileged helper: %v\n%s", err, output)
	}
	invocation, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if want := " _vm-cpu apply subyard yard /run/incus.sock\n"; !strings.HasSuffix(string(invocation), want) {
		t.Fatalf("helper invocation = %q", invocation)
	}
}

func TestVMCPUHelperPathKeepsOrdinaryEngine(t *testing.T) {
	want, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	got, err := vmCPUHelperPath()
	if err != nil || got != want {
		t.Fatalf("vmCPUHelperPath() = %q, %v, want %q", got, err, want)
	}
}

func TestVMCPUHelperFailureWithoutHelperIsNotConvergence(t *testing.T) {
	t.Run("sudo is missing", func(t *testing.T) {
		t.Setenv("PATH", testkit.TempDir(t))
		err := runVMCPUHelper(context.Background(), "subyard", "yard", "")
		var missing *exec.Error
		if !errors.As(err, &missing) || strings.Contains(err.Error(), "did not converge") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("deadline stays identifiable", func(t *testing.T) {
		root := testkit.TempDir(t)
		testkit.WriteFile(t, filepath.Join(root, "sudo"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o700)
		t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		err := runVMCPUHelper(ctx, "subyard", "yard", "")
		if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "did not converge") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestVMCPUHelperFailureNamesTheActualCause(t *testing.T) {
	for _, scenario := range []struct {
		name, script, want, reject string
	}{
		{"sudo refusal stays visible", "echo 'sudo: a password is required' >&2; exit 1", "apply VM CPU weight as root: sudo: a password is required: exit status 1", ""},
		{"unusable command is not an authorization claim", "echo 'sudo: /x: command not found' >&2; exit 1", "sudo: /x: command not found", "authorization"},
		{"helper diagnostic passes through", "echo 'VM CPU scheduling: cannot inspect the VM host cgroup' >&2; exit 2", "VM CPU scheduling: cannot inspect the VM host cgroup: exit status 2", "as root"},
		{"silent helper exit means no convergence", "exit 1", "VM host CPU weight did not converge: exit status 1", "authorization"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root := testkit.TempDir(t)
			testkit.WriteFile(t, filepath.Join(root, "sudo"), []byte("#!/bin/sh\n"+scenario.script+"\n"), 0o700)
			t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
			err := runVMCPUHelper(context.Background(), "subyard", "yard", "")
			if err == nil || !strings.Contains(err.Error(), scenario.want) {
				t.Fatalf("err = %v, want %q", err, scenario.want)
			}
			if scenario.reject != "" && strings.Contains(err.Error(), scenario.reject) {
				t.Fatalf("err = %v must not claim %q", err, scenario.reject)
			}
		})
	}
}

package shelladapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/uninstall"
)

// NativeVMCPU is a fixed CLI leaf, not a generic executable/argument transport.
type NativeVMCPU struct{ Project, Instance, Socket, UUID string }

func RunNativeVMCPU(ctx context.Context, request NativeVMCPU, diagnostics io.Writer) error {
	return runNativeVMCPU(ctx, request, diagnostics, "/usr/bin/sudo")
}

func runNativeVMCPU(ctx context.Context, request NativeVMCPU, diagnostics io.Writer, sudo string) error {
	capability := ports.NativeSelfExecutableFrom(ctx)
	home := ports.NativeOwnerHome(ctx)
	if ports.NativeOperationObserverFrom(ctx) == nil || capability == nil || home == "" || request.UUID == "" || !domain.SafeName(request.Project) || !domain.SafeName(request.Instance) || request.Socket != "" && !filepath.IsAbs(request.Socket) {
		return errors.New("native VM CPU executable custody is unavailable")
	}
	image, digest, err := capability.Borrow()
	if err != nil {
		return err
	}
	defer image.Close()
	owner, err := uninstall.BorrowOwnerLock(home)
	if err != nil {
		return err
	}
	if owner == nil {
		return errors.New("native VM CPU owner barrier is unavailable")
	}
	defer owner.Close()
	if err := uninstall.ValidateOwnerLockDescriptor(home, uint32(os.Getuid()), owner); err != nil {
		return err
	}
	// The parent holds this exact image FD until foreground drain. sudo may close
	// inherited descriptors, so it opens our retained image through the parent.
	executable := fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), image.Fd())
	args := []string{"_vm-cpu", "native-apply", request.Project, request.Instance, request.Socket, request.UUID, strconv.Itoa(os.Getuid()), home}
	command := exec.CommandContext(ctx, executable, args...)
	if os.Geteuid() != 0 {
		command = exec.CommandContext(ctx, sudo, append([]string{"-n", "--", executable}, args...)...)
	}
	command.Dir, err = os.Getwd()
	if err != nil {
		return err
	}
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8", "LC_ALL=C.UTF-8"}
	command.Stdin = owner
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	command.WaitDelay = 2 * time.Second
	stderr := &limitedBuffer{limit: 2048}
	command.Stderr = stderr
	raw, err := json.Marshal(request)
	if err != nil {
		return err
	}
	binding, err := json.Marshal(struct {
		Kind, Image, Invocation, Directory string
		Arguments, Environment             []string
		Request                            []byte
	}{"vm-cpu-self", digest, ports.NativeInvocationKey(ctx), command.Dir, command.Args, command.Env, raw})
	if err != nil {
		return err
	}
	err = runNativeForeground(ctx, command, "vm-cpu-self", binding, true, nil, func(waitErr error) error {
		if stderr.exceeded {
			return errors.Join(waitErr, errors.New("native VM CPU diagnostic exceeded limit"))
		}
		return waitErr
	})
	if diagnostics != nil {
		_, _ = io.WriteString(diagnostics, stderr.String())
	}
	return err
}

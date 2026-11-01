package shelladapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/uninstall"
)

// RunNativeOwnerSource launches only the closed source-policy leaf. The image
// proof comes from Serve; request bytes are private and only their binding is
// durable. The caller owns the bounded framed exchange and its final result.
func RunNativeOwnerSource(ctx context.Context, mode string, environment []string, directory string, peer *os.File, request []byte, exchange func() error) error {
	if mode != "capture" && mode != "check" && mode != "catalog" && mode != "apply" && mode != "drain" {
		return errors.New("native source leaf kind")
	}
	imageProof := ports.NativeSelfExecutableFrom(ctx)
	if imageProof == nil {
		imageProof = ports.NativePreparationExecutableFrom(ctx)
	}
	if imageProof == nil || directory == "" || peer == nil {
		return errors.New("native source image unavailable")
	}
	observer := ports.NativeOperationObserverFrom(ctx)
	if mode == "apply" && (observer == nil || ports.NativeInvocationKey(ctx) == "" || ports.NativeOwnerHome(ctx) == "") {
		return errors.New("native source admission unavailable")
	}
	image, digest, err := imageProof.Borrow()
	if err != nil {
		return err
	}
	defer image.Close()
	executable := fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), image.Fd())
	command := exec.CommandContext(ctx, executable, "_owner-source-v1", mode)
	command.Dir = directory
	command.Env = environment
	command.ExtraFiles = []*os.File{peer}
	var owner *os.File
	if observer != nil || mode == "drain" {
		owner, err = uninstall.BorrowOwnerLock(ports.NativeOwnerHome(ctx))
		if err != nil {
			return err
		}
		if owner == nil {
			return errors.New("native source owner OFD unavailable")
		}
		defer owner.Close()
		command.Stdin = owner
		command.ExtraFiles = append(command.ExtraFiles, owner)
	}
	command.Stdout = io.Discard
	if f := ports.NativeInteractionFrom(ctx); f != nil {
		command.Stdout = f
	}
	command.Stderr = io.Discard
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	command.WaitDelay = 2 * time.Second
	binding, err := json.Marshal(struct {
		Kind, Image, Invocation, Directory string
		Arguments, Environment             []string
		Request                            []byte
	}{"owner-source-" + mode, digest, ports.NativeInvocationKey(ctx), directory, command.Args, command.Env, request})
	if err != nil {
		return err
	}
	return runNativeForeground(ctx, command, "owner-source-"+mode, binding, true, func() error { _ = peer.Close(); return exchange() }, nil)
}

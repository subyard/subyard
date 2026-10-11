package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// A session owns its process group for the lifetime of its handler.
// Ctrl-C reaches that foreground group instead of the caller's cancellation
// context, and background shell children may ignore it. Observe handler exit
// without reaping it, so its PID cannot be reused before group cleanup.
func runResourceSession(command *exec.Cmd, grace time.Duration) error {
	exited := make(chan struct{})
	var fence sync.Mutex
	signalGroup := func(signal unix.Signal) error {
		fence.Lock()
		defer fence.Unlock()
		select {
		case <-exited:
			return nil
		default:
			return unix.Kill(-command.Process.Pid, signal)
		}
	}
	command.Cancel = func() error {
		if err := signalGroup(unix.SIGTERM); err != nil {
			return err
		}
		// Keep the handler's identity waitable until cleanup completes. A
		// cooperative handler can release owned resources before escalation.
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-exited:
			return nil
		case <-timer.C:
			return signalGroup(unix.SIGKILL)
		}
	}
	if err := command.Start(); err != nil {
		return err
	}
	var info unix.Siginfo
	var observeErr error
	for {
		observeErr = unix.Waitid(unix.P_PID, command.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(observeErr, unix.EINTR) {
			break
		}
	}
	// Finish any cancellation signal before allowing Wait to reap the leader.
	fence.Lock()
	close(exited)
	fence.Unlock()
	var cleanupErr error
	if observeErr == nil {
		cleanupErr = unix.Kill(-command.Process.Pid, unix.SIGKILL)
		if errors.Is(cleanupErr, unix.ESRCH) {
			cleanupErr = nil
		}
	}
	waitErr := command.Wait()
	if observeErr != nil {
		return fmt.Errorf("observe resource session exit before group cleanup: %w", observeErr)
	}
	if cleanupErr != nil {
		return fmt.Errorf("clean up resource session process group: %w", cleanupErr)
	}
	return waitErr
}

func configureResourceSessionForeground(command *exec.Cmd, input io.Reader) (func() error, error) {
	terminal, ok := input.(*os.File)
	if !ok || !term.IsTerminal(int(terminal.Fd())) {
		return nil, nil
	}
	fd := int(terminal.Fd())
	foreground, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
	if err != nil {
		return nil, fmt.Errorf("read resource session terminal foreground group: %w", err)
	}
	if foreground != unix.Getpgrp() {
		return nil, fmt.Errorf("resource session terminal is not owned by the caller process group")
	}
	command.SysProcAttr.Foreground = true
	command.SysProcAttr.Ctty = fd
	return func() error {
		if err := restoreTerminalForeground(fd, foreground); err != nil {
			return fmt.Errorf("restore resource session terminal foreground group: %w", err)
		}
		return nil
	}, nil
}

func restoreTerminalForeground(fd, processGroup int) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	var blocked, previous unix.Sigset_t
	signal := int(unix.SIGTTOU) - 1
	blocked.Val[signal/64] |= uint64(1) << uint(signal%64)
	if err := unix.PthreadSigmask(unix.SIG_BLOCK, &blocked, &previous); err != nil {
		return err
	}
	restoreErr := unix.IoctlSetPointerInt(fd, unix.TIOCSPGRP, processGroup)
	maskErr := unix.PthreadSigmask(unix.SIG_SETMASK, &previous, nil)
	if restoreErr != nil {
		return restoreErr
	}
	return maskErr
}

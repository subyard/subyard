package shelladapter

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"

	"github.com/Subyard/Subyard/internal/ports"
)

// RunNativeCommand shares foreground custody between fixed adapter scripts and
// resource handlers. Callers install the original owner descriptor first. Read
// only probes use their ordinary runner and cannot reserve mutation authority.
// afterWait may recognize a caller-owned typed postcondition before deciding
// completion; it must retain every unproven wait/stream failure.
func RunNativeCommand(ctx context.Context, command *exec.Cmd, request []byte, afterStart func() error, afterWait func(error) error) error {
	observer := ports.NativeOperationObserverFrom(ctx)
	var binding []byte
	if observer != nil {
		file, err := os.Open(command.Path)
		if err != nil {
			return err
		}
		body, readErr := io.ReadAll(io.LimitReader(file, (32<<20)+1))
		if err := errors.Join(readErr, file.Close()); err != nil {
			return err
		}
		if len(body) > 32<<20 {
			return errors.New("native adapter source bound")
		}
		binding, err = json.Marshal(struct {
			Invocation, Directory  string
			Source, Request        []byte
			Arguments, Environment []string
		}{ports.NativeInvocationKey(ctx), command.Dir, body, request, command.Args, command.Env})
		if err != nil {
			return err
		}
	}
	return runNativeForeground(ctx, command, "adapter-script", binding, observer != nil, afterStart, afterWait)
}

// runNativeForeground is shared by bounded scripts and the fixed retained-self
// leaf. Only the owning constructors decide which executable proof is valid.
func runNativeForeground(ctx context.Context, command *exec.Cmd, kind string, binding []byte, requireGroupDrain bool, afterStart func() error, afterWait func(error) error) error {
	observer := ports.NativeOperationObserverFrom(ctx)
	var ordinal uint64
	if observer != nil {
		var err error
		ordinal, err = observer.CommandStarted(kind, fmt.Sprintf("%x", sha256.Sum256(binding)))
		if err != nil {
			return err
		}
		defer func() {
			if observer != nil {
				observer.Uncertain()
			}
		}()
	}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start adapter: %w", err)
	}
	if afterStart != nil {
		if err := afterStart(); err != nil {
			_ = command.Cancel()
			return errors.Join(err, command.Wait())
		}
	}
	err := command.Wait()
	if afterWait != nil {
		err = afterWait(err)
	}
	if ctx.Err() != nil {
		err = errors.Join(err, context.Cause(ctx))
	}
	if err != nil {
		return err
	}
	// Fixed retained leaves require drain even in closed preparation. Ordinary
	// scripts may intentionally leave background work after their leader exits.
	if requireGroupDrain && command.SysProcAttr != nil && command.SysProcAttr.Setpgid {
		if err := syscall.Kill(-command.Process.Pid, 0); err != syscall.ESRCH {
			return errors.New("native adapter foreground is not drained")
		}
	}
	if observer != nil {
		// Wait includes foreground stream copies. Surviving group members keep
		// the dispatch UNKNOWN even when the leader exited successfully.
		if err := syscall.Kill(-command.Process.Pid, 0); err != syscall.ESRCH {
			return errors.New("native adapter foreground is not drained")
		}
		if err := observer.CommandTerminal(ordinal); err != nil {
			return err
		}
		observer = nil
	}
	return nil
}

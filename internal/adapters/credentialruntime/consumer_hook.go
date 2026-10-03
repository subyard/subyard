package credentialruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func (runtime *Runtime) consumerStopHandler(consumer, zone string) (string, error) {
	if _, err := runtime.ConsumerPath(consumer, zone); err != nil {
		return "", err
	}
	for _, declaration := range runtime.consumers {
		if declaration.ID == consumer && declaration.StopHandler != "" {
			return runtime.consumerOwners[consumer].ExecutablePath(declaration.StopHandler)
		}
	}
	return "", errors.New("credential consumer has no verified stop handler")
}

func (runtime *Runtime) runConsumerStopHandler(ctx context.Context, consumer, zone string) error {
	path, err := runtime.consumerStopHandler(consumer, zone)
	if err != nil {
		return err
	}
	callContext, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	dispatcher := runtime.config.Dispatcher
	if strings.HasPrefix(dispatcher, "/proc/self/fd/") {
		dispatcher = strings.Replace(dispatcher, "/proc/self/", fmt.Sprintf("/proc/%d/", os.Getpid()), 1)
	}
	command := exec.CommandContext(callContext, path, dispatcher, runtime.config.Context, zone)
	command.Dir = filepath.Dir(path)
	command.Env = make([]string, 0, len(runtime.config.TargetEnvironment))
	for _, entry := range runtime.config.TargetEnvironment {
		name, _, _ := strings.Cut(entry, "=")
		// Retain target routing/config inputs, but prevent ambient shell or
		// dynamic-loader startup code from replacing the shipped hook.
		switch name {
		case "BASH_ENV", "ENV", "SHELLOPTS", "BASHOPTS", "LD_PRELOAD", "LD_LIBRARY_PATH":
			continue
		}
		if !strings.HasPrefix(name, "BASH_FUNC_") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Stdin = nil
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	command.WaitDelay = 2 * time.Second
	stdout, stderr := &limitedBuffer{limit: 64 << 10}, &limitedBuffer{limit: 64 << 10}
	command.Stdout, command.Stderr = stdout, stderr
	err = command.Run()
	// Hooks receive metadata only; never relay arbitrary process output.
	if err != nil || callContext.Err() != nil || stdout.exceeded || stderr.exceeded {
		return errors.New("credential consumer stop could not be verified")
	}
	return nil
}

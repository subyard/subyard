package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/shellquote"
)

const defaultLimit = 4 * 1024 * 1024

type Process struct {
	Program   string
	Arguments []string
	Directory string
	Env       []string
	Timeout   time.Duration
	MaxBytes  int64
}

type ProcessError struct {
	ExitCode int
	Message  string
}

func (err *ProcessError) Error() string { return "transport failed: " + err.Message }

func Local(engine, repositoryRoot string) Process {
	return Process{
		Program: engine, Arguments: []string{"rpc", "--stdio"}, Directory: repositoryRoot,
		Env: append(os.Environ(), "SUBYARD_REPOSITORY_ROOT="+repositoryRoot),
	}
}

func SSH(program, target string, connectTimeout time.Duration) (Process, error) {
	return SSHYard(program, target, "", connectTimeout)
}

func SSHPinned(program, target, knownHostsPath string, connectTimeout time.Duration) (Process, error) {
	if !filepath.IsAbs(knownHostsPath) || strings.ContainsAny(knownHostsPath, "\r\n") {
		return Process{}, errors.New("managed known_hosts path must be absolute")
	}
	return sshProcess(program, target, "", connectTimeout, []string{
		"StrictHostKeyChecking=yes",
		"UserKnownHostsFile=" + knownHostsPath,
		"GlobalKnownHostsFile=/dev/null",
		"UpdateHostKeys=no",
	})
}

// SSHHostKeyAssessment asks OpenSSH itself to resolve the configured target
// (including aliases and proxies) and record the one server key it negotiates
// in an isolated known_hosts file. Authentication is disabled deliberately;
// callers inspect the captured key, then perform the authoritative RPC over a
// separate SSHPinned transport.
func SSHHostKeyAssessment(
	program, target, knownHostsPath string, connectTimeout time.Duration,
) (Process, error) {
	if !filepath.IsAbs(knownHostsPath) || strings.ContainsAny(knownHostsPath, "\r\n") {
		return Process{}, errors.New("assessment known_hosts path must be absolute")
	}
	if program == "" {
		program = "ssh"
	}
	if !domain.SafeSSHTarget(target) {
		return Process{}, fmt.Errorf("invalid SSH target %q", target)
	}
	seconds := int(connectTimeout.Round(time.Second) / time.Second)
	if seconds < 1 {
		seconds = 2
	}
	options := []string{
		"BatchMode=yes", "ConnectTimeout=" + strconv.Itoa(seconds),
		"StrictHostKeyChecking=accept-new", "UserKnownHostsFile=" + knownHostsPath,
		"GlobalKnownHostsFile=/dev/null", "HashKnownHosts=no", "UpdateHostKeys=no",
		"CheckHostIP=no", "ControlMaster=no", "ControlPath=none",
		"PreferredAuthentications=none",
	}
	arguments := []string{"-T"}
	for _, option := range options {
		arguments = append(arguments, "-o", option)
	}
	arguments = append(arguments, target, "--", "true")
	return Process{Program: program, Arguments: arguments}, nil
}

func SSHYard(program, target, yard string, connectTimeout time.Duration) (Process, error) {
	return sshProcess(program, target, yard, connectTimeout, nil)
}

func sshProcess(
	program, target, yard string, connectTimeout time.Duration, options []string,
) (Process, error) {
	if program == "" {
		program = "ssh"
	}
	if !domain.SafeSSHTarget(target) {
		return Process{}, fmt.Errorf("invalid SSH target %q", target)
	}
	if yard != "" && !domain.SafeName(yard) {
		return Process{}, fmt.Errorf("invalid remote yard %q", yard)
	}
	seconds := int(connectTimeout.Round(time.Second) / time.Second)
	if seconds < 1 {
		seconds = 2
	}
	remote := []string{"yard"}
	if yard != "" && yard != "default" {
		remote = append(remote, "-Y", yard)
	}
	remote = append(remote, "rpc", "--stdio")
	command := "exec " + shellquote.Command(remote)
	arguments := []string{"-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=" + strconv.Itoa(seconds)}
	for _, option := range options {
		arguments = append(arguments, "-o", option)
	}
	arguments = append(arguments, target, "--", "bash", "-lc", shellquote.Word(command))
	return Process{Program: program, Arguments: arguments}, nil
}

func (transport Process) Call(ctx context.Context, _ string, request []byte) ([]byte, error) {
	return transport.CallReader(ctx, bytes.NewReader(request))
}

func (transport Process) Run(ctx context.Context, arguments ...string) ([]byte, error) {
	transport.Arguments = append([]string(nil), arguments...)
	return transport.CallReader(ctx, bytes.NewReader(nil))
}

func (transport Process) CallReader(ctx context.Context, request io.Reader) ([]byte, error) {
	if transport.Program == "" {
		return nil, errors.New("transport program is required")
	}
	callContext := ctx
	cancel := func() {}
	if transport.Timeout > 0 {
		callContext, cancel = context.WithTimeout(ctx, transport.Timeout)
	}
	defer cancel()
	command := exec.CommandContext(callContext, transport.Program, transport.Arguments...)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	command.WaitDelay = 2 * time.Second
	command.Dir = transport.Directory
	if transport.Env != nil {
		command.Env = transport.Env
	}
	command.Stdin = request
	limit := transport.MaxBytes
	if limit <= 0 {
		limit = defaultLimit
	}
	stdout := &limitedBuffer{limit: limit}
	stderr := &limitedBuffer{limit: limit}
	command.Stdout = stdout
	command.Stderr = stderr
	err := command.Run()
	if stdout.exceeded || stderr.exceeded {
		return nil, errors.New("transport output exceeded limit")
	}
	if callContext.Err() != nil {
		return nil, fmt.Errorf("transport cancelled: %w", context.Cause(callContext))
	}
	if err != nil {
		message := strings.TrimSpace(stderr.buffer.String())
		if message == "" {
			message = err.Error()
		}
		exitCode := 1
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			exitCode = exitError.ExitCode()
		}
		return bytes.Clone(stdout.buffer.Bytes()), &ProcessError{ExitCode: exitCode, Message: message}
	}
	return bytes.Clone(stdout.buffer.Bytes()), nil
}

type limitedBuffer struct {
	buffer   bytes.Buffer
	limit    int64
	exceeded bool
}

func (buffer *limitedBuffer) Write(value []byte) (int, error) {
	remaining := buffer.limit - int64(buffer.buffer.Len())
	if remaining <= 0 {
		buffer.exceeded = true
		return len(value), nil
	}
	write := value
	if int64(len(write)) > remaining {
		write = write[:remaining]
		buffer.exceeded = true
	}
	_, _ = buffer.buffer.Write(write)
	return len(value), nil
}

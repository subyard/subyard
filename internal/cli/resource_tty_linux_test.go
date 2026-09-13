//go:build linux

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/shellquote"
	"golang.org/x/sys/unix"
)

const resourceTTYHelper = "SUBYARD_RESOURCE_TTY_TEST_HELPER"

func TestResourceMutationDoesNotReadFromOperatorTerminal(t *testing.T) {
	if os.Getenv(resourceTTYHelper) != "1" {
		runResourceTTYHelper(t)
		return
	}

	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open controlling terminal: %v", err)
	}
	defer tty.Close()

	root, environment, _ := resourceCommandFixture(t)
	handlerPath := filepath.Join(root, "config", "profiles", "fixture", "resources", "demo", "handler.sh")
	handler := `#!/bin/sh
set -eu
case "${SUBYARD_RESOURCE_MODE:-}" in
  prepare)
    printf '%s\n' '{"schema":"yard.resource-action-assessment.v1","action":"run","changed":true,"consequences":["start fixture runtime"]}'
    ;;
  apply)
    printf '%s\n' "$$" >"$SUBYARD_REPOSITORY_ROOT/resource-tty.pid"
    IFS= read -r input || :
    printf 'completed\n' >"$SUBYARD_REPOSITORY_ROOT/resource-tty.completed"
    ;;
esac
`
	writeCLIFile(t, handlerPath, handler, 0o700)

	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root,
		Program:        "yard",
		Arguments:      []string{"demo", "run", "--yes"},
		Environment:    environment,
		WorkingDir:     root,
		Stdin:          tty,
		Stdout:         tty,
		Stderr:         &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- program.Run(ctx) }()

	pidPath := filepath.Join(root, "resource-tty.pid")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case code := <-done:
			if code != 0 {
				t.Fatalf("resource command exited with code %d: %s", code, stderr.String())
			}
			if _, err := os.Stat(filepath.Join(root, "resource-tty.completed")); err != nil {
				t.Fatalf("resource handler did not complete: %v", err)
			}
			return
		default:
		}

		pidBytes, err := os.ReadFile(pidPath)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
			if parseErr != nil {
				t.Fatalf("parse resource handler pid: %v", parseErr)
			}
			status, statusErr := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
			if statusErr == nil && strings.Contains(string(status), "\nState:\tT") {
				cancel()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("cancel did not reap stopped resource handler group")
				}
				if _, statErr := os.Stat("/proc/" + strconv.Itoa(pid)); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("stopped resource handler remained after cancellation: %v", statErr)
				}
				t.Fatal("non-session resource handler stopped while reading inherited operator terminal")
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not terminate resource command after diagnostic timeout")
	}
	t.Fatal("resource command neither completed nor entered the expected stopped state")
}

func TestResourceSessionPreservesOperatorInput(t *testing.T) {
	if os.Getenv(resourceTTYHelper) != t.Name() {
		runResourceTTYHelper(t, t.Name(), "operator input\n")
		return
	}

	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open controlling terminal: %v", err)
	}
	defer tty.Close()
	callerForeground, err := testForegroundProcessGroup(int(tty.Fd()))
	if err != nil {
		t.Fatalf("read caller foreground process group: %v", err)
	}

	root, environment, _ := resourceCommandFixture(t)
	handlerPath := filepath.Join(root, "config", "profiles", "fixture", "resources", "demo", "handler.sh")
	handler := `#!/bin/sh
set -eu
case "${SUBYARD_RESOURCE_MODE:-}" in
  prepare)
    printf '%s\n' '{"schema":"yard.resource-action-assessment.v1","action":"view","changed":true,"consequences":["open fixture session"]}'
    ;;
  apply)
    IFS= read -r input
    printf '%s\n' "$input" >"$SUBYARD_REPOSITORY_ROOT/resource-session-input.log"
    ;;
esac
`
	writeCLIFile(t, handlerPath, handler, 0o700)

	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root,
		Program:        "yard",
		Arguments:      []string{"demo", "view"},
		Environment:    environment,
		WorkingDir:     root,
		Stdin:          tty,
		Stdout:         tty,
		Stderr:         &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- program.Run(ctx) }()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("resource session exited with code %d: %s", code, stderr.String())
		}
	case <-time.After(3 * time.Second):
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("cancel did not reap stopped resource session")
		}
		t.Fatal("resource session did not read input from its controlling terminal")
	}
	input, err := os.ReadFile(filepath.Join(root, "resource-session-input.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(input) != "operator input\n" {
		t.Fatalf("resource session input = %q", input)
	}
	restoredForeground, err := testForegroundProcessGroup(int(tty.Fd()))
	if err != nil {
		t.Fatalf("read restored foreground process group: %v", err)
	}
	if restoredForeground != callerForeground {
		t.Fatalf("foreground process group = %d, want caller group %d", restoredForeground, callerForeground)
	}
}

func testForegroundProcessGroup(fd int) (int, error) {
	return unix.IoctlGetInt(fd, unix.TIOCGPGRP)
}

func TestResourceMutationCancellationReapsProcessGroup(t *testing.T) {
	root, environment, _ := resourceCommandFixture(t)
	handlerPath := filepath.Join(root, "config", "profiles", "fixture", "resources", "demo", "handler.sh")
	handler := `#!/bin/sh
set -eu
case "${SUBYARD_RESOURCE_MODE:-}" in
  prepare)
    printf '%s\n' '{"schema":"yard.resource-action-assessment.v1","action":"run","changed":true,"consequences":["start fixture runtime"]}'
    ;;
  apply)
    sleep 300 &
    child=$!
    printf '%s %s\n' "$$" "$child" >"$SUBYARD_REPOSITORY_ROOT/resource-cancel.pids"
    wait "$child"
    ;;
esac
`
	writeCLIFile(t, handlerPath, handler, 0o700)

	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root,
		Program:        "yard",
		Arguments:      []string{"demo", "run", "--yes"},
		Environment:    environment,
		WorkingDir:     root,
		Stderr:         &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- program.Run(ctx) }()
	pidsPath := filepath.Join(root, "resource-cancel.pids")
	deadline := time.Now().Add(3 * time.Second)
	var pids []string
	for time.Now().Before(deadline) {
		value, readErr := os.ReadFile(pidsPath)
		if readErr == nil {
			pids = strings.Fields(string(value))
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(pids) != 2 {
		cancel()
		t.Fatalf("resource handler did not publish parent and child pids: %q", pids)
	}

	cancel()
	select {
	case code := <-done:
		if code != 1 || !strings.Contains(stderr.String(), context.Canceled.Error()) {
			t.Fatalf("cancelled resource command code=%d stderr=%q", code, stderr.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled resource command did not return")
	}

	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		allStopped := true
		for _, pid := range pids {
			status, statusErr := os.ReadFile("/proc/" + pid + "/status")
			if statusErr == nil && !strings.Contains(string(status), "\nState:\tZ") {
				allStopped = false
			}
		}
		if allStopped {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("resource process group still has a live member: pids %s", strings.Join(pids, ", "))
}

func TestResourceSessionCancellationRestoresTerminalAndReapsGroup(t *testing.T) {
	if os.Getenv(resourceTTYHelper) != t.Name() {
		runResourceTTYHelper(t, t.Name(), "begin session\n")
		return
	}

	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open controlling terminal: %v", err)
	}
	defer tty.Close()
	callerForeground, err := testForegroundProcessGroup(int(tty.Fd()))
	if err != nil {
		t.Fatalf("read caller foreground process group: %v", err)
	}

	root, environment, _ := resourceCommandFixture(t)
	handlerPath := filepath.Join(root, "config", "profiles", "fixture", "resources", "demo", "handler.sh")
	handler := `#!/bin/sh
set -eu
case "${SUBYARD_RESOURCE_MODE:-}" in
  prepare)
    printf '%s\n' '{"schema":"yard.resource-action-assessment.v1","action":"view","changed":true,"consequences":["open fixture session"]}'
    ;;
  apply)
    IFS= read -r input
    sleep 300 &
    child=$!
    printf '%s %s %s\n' "$input" "$$" "$child" >"$SUBYARD_REPOSITORY_ROOT/resource-session-cancel"
    wait "$child"
    ;;
esac
`
	writeCLIFile(t, handlerPath, handler, 0o700)

	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root,
		Program:        "yard",
		Arguments:      []string{"demo", "view"},
		Environment:    environment,
		WorkingDir:     root,
		Stdin:          tty,
		Stdout:         tty,
		Stderr:         &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- program.Run(ctx) }()
	statePath := filepath.Join(root, "resource-session-cancel")
	deadline := time.Now().Add(3 * time.Second)
	var state []string
	for time.Now().Before(deadline) {
		value, readErr := os.ReadFile(statePath)
		if readErr == nil {
			state = strings.Fields(string(value))
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(state) != 4 || strings.Join(state[:2], " ") != "begin session" {
		cancel()
		t.Fatalf("session did not read input and publish child state: %q", state)
	}
	pids := state[2:]

	cancel()
	select {
	case code := <-done:
		if code != 1 || !strings.Contains(stderr.String(), context.Canceled.Error()) {
			t.Fatalf("cancelled resource session code=%d stderr=%q", code, stderr.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled resource session did not return")
	}
	restoredForeground, err := testForegroundProcessGroup(int(tty.Fd()))
	if err != nil {
		t.Fatalf("read restored foreground process group: %v", err)
	}
	if restoredForeground != callerForeground {
		t.Fatalf("foreground process group = %d, want caller group %d", restoredForeground, callerForeground)
	}
	assertNoLiveProcesses(t, pids)
}

func assertNoLiveProcesses(t *testing.T, pids []string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		allStopped := true
		for _, pid := range pids {
			status, statusErr := os.ReadFile("/proc/" + pid + "/status")
			if statusErr == nil && !strings.Contains(string(status), "\nState:\tZ") {
				allStopped = false
			}
		}
		if allStopped {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("resource process group still has a live member: pids %s", strings.Join(pids, ", "))
}

func TestResourceSessionTerminalInterruptReapsProcessGroup(t *testing.T) {
	for _, trap := range []string{"signal", "exit130", "exit0"} {
		t.Run(trap, func(t *testing.T) {
			if os.Getenv(resourceTTYHelper) != t.Name() {
				runResourceTTYHelper(t, t.Name(), "terminal-interrupt")
				return
			}
			tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer tty.Close()
			foreground, err := testForegroundProcessGroup(int(tty.Fd()))
			if err != nil {
				t.Fatal(err)
			}
			root, environment, _ := resourceCommandFixture(t)
			handler := `#!/bin/sh
set -eu
case "${SUBYARD_RESOURCE_MODE:-}" in
  prepare)
    printf '%s\n' '{"schema":"yard.resource-action-assessment.v1","action":"view","changed":true,"consequences":["open fixture session"]}'
    ;;
  apply)
    INTERRUPT_TRAP
    (trap '' INT; exec sleep 300) &
    child=$!
    printf '%s %s\n' "$$" "$child" >"$SUBYARD_REPOSITORY_ROOT/resource-interrupt.pids"
    wait "$child"
    ;;
esac
`
			trapCommand := ":"
			wantCode := 1
			if trap != "signal" {
				trapCommand = "trap 'exit " + strings.TrimPrefix(trap, "exit") + "' INT"
				if trap == "exit0" {
					wantCode = 0
				}
			}
			handler = strings.Replace(handler, "INTERRUPT_TRAP", trapCommand, 1)
			writeCLIFile(t, filepath.Join(root, "config", "profiles", "fixture", "resources", "demo", "handler.sh"), handler, 0o700)
			var stderr bytes.Buffer
			program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"demo", "view"},
				Environment: environment, WorkingDir: root, Stdin: tty, Stdout: tty, Stderr: &stderr})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan int, 1)
			go func() { done <- program.Run(ctx) }()
			var pids []string
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				value, err := os.ReadFile(filepath.Join(root, "resource-interrupt.pids"))
				if err == nil {
					pids = strings.Fields(string(value))
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if len(pids) != 2 {
				t.Fatalf("session did not publish process IDs: %q", pids)
			}
			child, _ := strconv.Atoi(pids[1])
			childProcess, err := os.FindProcess(child)
			if err != nil {
				t.Fatal(err)
			}
			defer childProcess.Release()
			defer childProcess.Kill()
			fmt.Fprintln(tty, "RESOURCE-INTERRUPT-READY")
			select {
			case code := <-done:
				if code != wantCode {
					t.Fatalf("interrupted session code=%d stderr=%q", code, stderr.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("terminal interrupt did not stop session")
			}
			if ctx.Err() != nil {
				t.Fatalf("terminal interrupt unexpectedly cancelled caller context: %v", ctx.Err())
			}
			if restored, err := testForegroundProcessGroup(int(tty.Fd())); err != nil || restored != foreground {
				t.Fatalf("restored foreground=%d, want=%d, err=%v", restored, foreground, err)
			}
			assertNoLiveProcesses(t, pids)
		})
	}
}

func TestResourceSessionNormalExitReapsOnlyItsProcessGroup(t *testing.T) {
	if os.Getenv(resourceTTYHelper) != t.Name() {
		runResourceTTYHelper(t, t.Name())
		return
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer tty.Close()
	root, environment, _ := resourceCommandFixture(t)
	handler := `#!/bin/sh
set -eu
case "${SUBYARD_RESOURCE_MODE:-}" in
  prepare)
    printf '%s\n' '{"schema":"yard.resource-action-assessment.v1","action":"view","changed":true,"consequences":["open fixture session"]}'
    ;;
  apply)
    sleep 300 >/dev/null 2>&1 &
    printf '%s\n' "$!" >"$SUBYARD_REPOSITORY_ROOT/session-child.pid"
    setsid sh -c 'printf "%s\n" "$$" >"$SUBYARD_REPOSITORY_ROOT/detached-child.pid"; exec sleep 300' >/dev/null 2>&1 &
    while [ ! -s "$SUBYARD_REPOSITORY_ROOT/detached-child.pid" ]; do sleep 0.01; done
    ;;
esac
`
	writeCLIFile(t, filepath.Join(root, "config", "profiles", "fixture", "resources", "demo", "handler.sh"), handler, 0o700)
	var stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"demo", "view"},
		Environment: environment, WorkingDir: root, Stdin: tty, Stdout: tty, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	code := program.Run(ctx)
	child := resourceTestChildProcess(t, filepath.Join(root, "session-child.pid"))
	detached := resourceTestChildProcess(t, filepath.Join(root, "detached-child.pid"))
	if code != 0 {
		t.Fatalf("session normal exit code=%d stderr=%q", code, stderr.String())
	}
	assertNoLiveProcesses(t, []string{strconv.Itoa(child.Pid)})
	assertResourceProcessAlive(t, detached)
}

func TestResourceNonterminalActionsPreserveBackgroundProcess(t *testing.T) {
	for _, verb := range []string{"view", "run"} {
		t.Run(verb, func(t *testing.T) {
			root, environment, _ := resourceCommandFixture(t)
			handler := `#!/bin/sh
set -eu
case "${SUBYARD_RESOURCE_MODE:-}" in
  prepare)
    printf '{"schema":"yard.resource-action-assessment.v1","action":"%s","changed":true,"consequences":["start fixture child"]}\n' "$1"
    ;;
  apply)
    sleep 300 >/dev/null 2>&1 &
    printf '%s\n' "$!" >"$SUBYARD_REPOSITORY_ROOT/session-child.pid"
    ;;
esac
`
			writeCLIFile(t, filepath.Join(root, "config", "profiles", "fixture", "resources", "demo", "handler.sh"), handler, 0o700)
			var stderr bytes.Buffer
			program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"demo", verb, "--yes"},
				Environment: environment, WorkingDir: root, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: &stderr})
			if err != nil {
				t.Fatal(err)
			}
			code := program.Run(context.Background())
			child := resourceTestChildProcess(t, filepath.Join(root, "session-child.pid"))
			if code != 0 {
				t.Fatalf("nonterminal session code=%d stderr=%q", code, stderr.String())
			}
			assertResourceProcessAlive(t, child)
		})
	}
}

func TestResourceSessionUnwaitableChildDoesNotSignalGroup(t *testing.T) {
	if os.Getenv(resourceTTYHelper) != t.Name() {
		runResourceTTYHelper(t, t.Name())
		return
	}
	// Auto-reaping removes the leader before waitid can retain its identity.
	// Keep this process-wide signal setting isolated in the helper subprocess.
	signal.Ignore(syscall.SIGCHLD)
	defer signal.Reset(syscall.SIGCHLD)
	pidPath := filepath.Join(t.TempDir(), "child.pid")
	command := exec.CommandContext(context.Background(), "sh", "-c",
		`sleep 300 >/dev/null 2>&1 & printf '%s\n' "$!" >"$1"`, "sh", pidPath)
	configureResourceProcess(command)
	err := runResourceTerminalSession(command)
	child := resourceTestChildProcess(t, pidPath)
	if !errors.Is(err, unix.ECHILD) {
		t.Fatalf("unwaitable session error=%v, want ECHILD", err)
	}
	assertResourceProcessAlive(t, child)
}

func TestResourceSessionStartFailureRestoresForeground(t *testing.T) {
	if os.Getenv(resourceTTYHelper) != t.Name() {
		runResourceTTYHelper(t, t.Name())
		return
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer tty.Close()
	foreground, err := testForegroundProcessGroup(int(tty.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	root, environment, _ := resourceCommandFixture(t)
	handler := `#!/bin/sh
set -eu
case "${SUBYARD_RESOURCE_MODE:-}" in
  prepare)
    printf '%s\n' '#!/missing-resource-interpreter' >"$0"
    printf '%s\n' '{"schema":"yard.resource-action-assessment.v1","action":"view","changed":true,"consequences":["open fixture session"]}'
    ;;
esac
`
	writeCLIFile(t, filepath.Join(root, "config", "profiles", "fixture", "resources", "demo", "handler.sh"), handler, 0o700)
	var stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"demo", "view"},
		Environment: environment, WorkingDir: root, Stdin: tty, Stdout: tty, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 || !strings.Contains(stderr.String(), "no such file") {
		t.Fatalf("session start failure code=%d stderr=%q", code, stderr.String())
	}
	if restored, err := testForegroundProcessGroup(int(tty.Fd())); err != nil || restored != foreground {
		t.Fatalf("restored foreground=%d, want=%d, err=%v", restored, foreground, err)
	}
}

func resourceTestChildProcess(t *testing.T, path string) *os.Process {
	t.Helper()
	value, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(value)))
	if err != nil {
		t.Fatal(err)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { process.Kill(); process.Release() })
	return process
}

func assertResourceProcessAlive(t *testing.T, process *os.Process) {
	t.Helper()
	status, err := os.ReadFile("/proc/" + strconv.Itoa(process.Pid) + "/status")
	if err != nil || strings.Contains(string(status), "\nState:\tZ") {
		t.Fatalf("background process %d did not survive: %v", process.Pid, err)
	}
}

type resourceInterruptOutput struct {
	buffer bytes.Buffer
	ready  chan struct{}
	once   sync.Once
}

func (output *resourceInterruptOutput) Write(value []byte) (int, error) {
	n, err := output.buffer.Write(value)
	if bytes.Contains(output.buffer.Bytes(), []byte("RESOURCE-INTERRUPT-READY")) {
		output.once.Do(func() { close(output.ready) })
	}
	return n, err
}

func (output *resourceInterruptOutput) String() string { return output.buffer.String() }

func runResourceTTYHelper(t *testing.T, helper ...string) {
	t.Helper()
	scriptPath, err := exec.LookPath("script")
	if err != nil {
		t.Fatal("util-linux script is required for the resource terminal regression")
	}
	testName := "TestResourceMutationDoesNotReadFromOperatorTerminal"
	helperValue := "1"
	input := ""
	if len(helper) != 0 {
		testName = helper[0]
		helperValue = helper[0]
	}
	if len(helper) > 1 {
		input = helper[1]
	}
	arguments := []string{os.Args[0], "-test.run", "^" + testName + "$", "-test.v"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, scriptPath, "-qefc", shellquote.Command(arguments), "/dev/null")
	command.WaitDelay = 2 * time.Second
	command.Env = append(os.Environ(), resourceTTYHelper+"="+helperValue)
	if input == "terminal-interrupt" {
		reader, writer := io.Pipe()
		defer reader.Close()
		defer writer.Close()
		output := &resourceInterruptOutput{ready: make(chan struct{})}
		command.Stdin, command.Stdout, command.Stderr = reader, output, output
		done := make(chan error, 1)
		go func() { done <- command.Run() }()
		select {
		case <-output.ready:
			if _, err := writer.Write([]byte{3}); err != nil {
				t.Fatal(err)
			}
			writer.Close()
		case err := <-done:
			t.Fatalf("resource terminal helper exited before interrupt: %v\n%s", err, output.String())
		case <-ctx.Done():
			t.Fatal("resource terminal helper never became ready")
		}
		if err := <-done; err != nil {
			t.Fatalf("resource terminal helper failed: %v\n%s", err, output.String())
		}
		return
	}
	command.Stdin = strings.NewReader(input)
	output, err := command.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			t.Fatalf("resource terminal helper exceeded its deadline: %v", ctx.Err())
		}
		t.Fatalf("resource terminal helper failed: %v\n%s", err, output)
	}
}

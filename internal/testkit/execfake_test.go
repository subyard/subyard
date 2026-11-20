package testkit

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// cleanupRecorder captures failures and cleanups of the code under test.
type cleanupRecorder struct {
	testing.TB
	mu       sync.Mutex
	cleanups []func()
	failures []string
}

func (recorder *cleanupRecorder) Cleanup(cleanup func()) {
	recorder.cleanups = append(recorder.cleanups, cleanup)
}

func (recorder *cleanupRecorder) Error(arguments ...any) { recorder.fail(fmt.Sprint(arguments...)) }

func (recorder *cleanupRecorder) Errorf(format string, arguments ...any) {
	recorder.fail(fmt.Sprintf(format, arguments...))
}

func (recorder *cleanupRecorder) Fatal(arguments ...any) { recorder.fail(fmt.Sprint(arguments...)) }

func (recorder *cleanupRecorder) Fatalf(format string, arguments ...any) {
	recorder.fail(fmt.Sprintf(format, arguments...))
}

func (recorder *cleanupRecorder) fail(message string) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.failures = append(recorder.failures, message)
}

func (recorder *cleanupRecorder) runCleanups() {
	for index := len(recorder.cleanups) - 1; index >= 0; index-- {
		recorder.cleanups[index]()
	}
}

func runShim(t *testing.T, path string, arguments ...string) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	command := exec.Command(path, arguments...)
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return stdout.String(), stderr.String(), 0
	case errors.As(err, &exit):
		return stdout.String(), stderr.String(), exit.ExitCode()
	default:
		t.Fatal(err)
		return "", "", 0
	}
}

func TestExecFakeRecordsExactArgumentsAndAnswersFirstMatchingRule(t *testing.T) {
	bin := TempDir(t)
	fake := NewExecFake(t, bin, "ssh", "git")
	fake.On(Match{Tool: "ssh", Prefix: []string{"-G"}}, Reply{Stdout: "hostname owner\n"}).
		On(Match{Tool: "ssh", Contains: []string{"-o", "BatchMode=yes"}}, Reply{Stderr: "denied\n", Exit: 255}).
		On(Match{Tool: "ssh", Args: []string{}}, Reply{Exit: 3}).
		On(Match{Tool: "ssh"}, Reply{Script: `printf '%s|' "$#" "$@"; exit 9`}).
		On(Match{Args: []string{"status", "--porcelain"}}, Reply{Stdout: " M file\n"})
	awkward := []string{"", "two words", "it's", "new\nline", "$HOME", "*", "-G"}
	for _, test := range []struct {
		tool      string
		arguments []string
		stdout    string
		stderr    string
		code      int
	}{
		{tool: "ssh", arguments: []string{"-G", "owner"}, stdout: "hostname owner\n"},
		{tool: "ssh", arguments: []string{"-p", "22", "-o", "BatchMode=yes", "owner"}, stderr: "denied\n", code: 255},
		{tool: "ssh", arguments: []string{"-o", "BatchMode=no"}, stdout: "2|-o|BatchMode=no|", code: 9},
		{tool: "ssh", code: 3},
		{tool: "ssh", arguments: awkward, stdout: "7||two words|it's|new\nline|$HOME|*|-G|", code: 9},
		{tool: "git", arguments: []string{"status", "--porcelain"}, stdout: " M file\n"},
	} {
		stdout, stderr, code := runShim(t, filepath.Join(bin, test.tool), test.arguments...)
		if stdout != test.stdout || stderr != test.stderr || code != test.code {
			t.Errorf("%s %q: stdout=%q stderr=%q code=%d", test.tool, test.arguments, stdout, stderr, code)
		}
	}
	fake.AssertCalls(
		Call{Tool: "ssh", Args: []string{"-G", "owner"}},
		Call{Tool: "ssh", Args: []string{"-p", "22", "-o", "BatchMode=yes", "owner"}},
		Call{Tool: "ssh", Args: []string{"-o", "BatchMode=no"}},
		Call{Tool: "ssh", Args: []string{}},
		Call{Tool: "ssh", Args: awkward},
		Call{Tool: "git", Args: []string{"status", "--porcelain"}},
	)
}

func TestExecFakeUnexpectedCallExits127AndFailsAtCleanup(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		t.Run(fmt.Sprintf("allowed=%t", allowed), func(t *testing.T) {
			recorder := &cleanupRecorder{TB: t}
			bin := TempDir(t)
			fake := NewExecFake(recorder, bin, "incus")
			fake.On(Match{Args: []string{"list"}}, Reply{})
			if allowed {
				fake.AllowUnexpected()
			}
			if _, stderr, code := runShim(t, filepath.Join(bin, "incus"), "delete", "yard"); code != 127 ||
				!strings.Contains(stderr, "unexpected call to incus") {
				t.Fatalf("unexpected call: code=%d stderr=%q", code, stderr)
			}
			if _, _, code := runShim(t, filepath.Join(bin, "incus"), "list"); code != 0 {
				t.Fatalf("expected call: code=%d", code)
			}
			if len(recorder.failures) != 0 {
				t.Fatalf("failed before cleanup: %q", recorder.failures)
			}
			recorder.runCleanups()
			if allowed && len(recorder.failures) != 0 ||
				!allowed && !slices.Equal(recorder.failures, []string{`execfake: unexpected call [incus["delete" "yard"]]`}) {
				t.Fatalf("cleanup failures: %q", recorder.failures)
			}
		})
	}
}

func TestExecFakeAssertCallsReportsMismatch(t *testing.T) {
	recorder := &cleanupRecorder{TB: t}
	bin := TempDir(t)
	fake := NewExecFake(recorder, bin, "yard")
	fake.On(Match{}, Reply{})
	runShim(t, filepath.Join(bin, "yard"), "status")
	fake.AssertCalls(Call{Tool: "yard", Args: []string{"status"}})
	if len(recorder.failures) != 0 {
		t.Fatalf("matching calls failed: %q", recorder.failures)
	}
	fake.AssertCalls(Call{Tool: "yard", Args: []string{"status", "--all"}})
	fake.AssertCalls()
	if len(recorder.failures) != 2 || !strings.Contains(recorder.failures[0], `want [yard["status" "--all"]]`) {
		t.Fatalf("mismatch failures: %q", recorder.failures)
	}
}

func TestExecFakeInstallsShimAtPathAndRefusesUnknownTool(t *testing.T) {
	recorder := &cleanupRecorder{TB: t}
	root := TempDir(t)
	fake := NewExecFake(recorder, TempDir(t))
	script := fake.Install("scripts/invoke.sh", filepath.Join(root, "scripts", "invoke.sh"))
	fake.On(Match{Tool: "scripts/invoke.sh", Args: []string{"status"}}, Reply{Stdout: "ok\n"})
	if stdout, _, code := runShim(t, script, "status"); stdout != "ok\n" || code != 0 {
		t.Fatalf("installed shim: stdout=%q code=%d", stdout, code)
	}
	info, err := os.Stat(script)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("shim mode: %v %v", info, err)
	}
	if fake.Path("scripts/invoke.sh") != script {
		t.Fatal("shim path differs")
	}
	fake.AssertCalls(Call{Tool: "scripts/invoke.sh", Args: []string{"status"}})
	fake.On(Match{Tool: "missing"}, Reply{})
	fake.On(Match{}, Reply{Script: "exit 0", Exit: 1})
	if len(recorder.failures) != 2 {
		t.Fatalf("invalid rules accepted: %q", recorder.failures)
	}
}

func TestExecFakeRecordsConcurrentCallsWhole(t *testing.T) {
	bin := TempDir(t)
	fake := NewExecFake(t, bin, "worker")
	fake.On(Match{Tool: "worker"}, Reply{})
	// Each record is about 500 KiB, far above what one shell write holds.
	const workers, calls, arguments = 24, 2, 100
	padding := strings.Repeat("x", 5000)
	var group sync.WaitGroup
	for worker := range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			for call := range calls {
				argv := []string{fmt.Sprint(worker), fmt.Sprint(call)}
				for range arguments {
					argv = append(argv, padding)
				}
				if output, err := exec.Command(filepath.Join(bin, "worker"), argv...).CombinedOutput(); err != nil {
					t.Errorf("worker %d call %d: %v %s", worker, call, err, output)
				}
			}
		}()
	}
	group.Wait()
	recorded := fake.Calls()
	seen := map[string]bool{}
	for _, call := range recorded {
		if call.Tool != "worker" || len(call.Args) != 2+arguments || slices.ContainsFunc(call.Args[2:], func(argument string) bool { return argument != padding }) {
			t.Fatalf("torn record: tool=%q arguments=%d", call.Tool, len(call.Args))
		}
		seen[call.Args[0]+"/"+call.Args[1]] = true
	}
	if len(recorded) != workers*calls || len(seen) != workers*calls {
		t.Fatalf("recorded %d calls, %d distinct", len(recorded), len(seen))
	}
}

func TestExecFakeKeepsOrderOfSequentialCalls(t *testing.T) {
	bin := TempDir(t)
	fake := NewExecFake(t, bin, "first", "second")
	fake.On(Match{}, Reply{})
	var want []Call
	for index := range 30 {
		tool := []string{"first", "second"}[index%2]
		runShim(t, filepath.Join(bin, tool), fmt.Sprint(index))
		want = append(want, Call{Tool: tool, Args: []string{fmt.Sprint(index)}})
	}
	fake.AssertCalls(want...)
}

func TestExecFakeChangesRulesWhileCallsRun(t *testing.T) {
	bin := TempDir(t)
	fake := NewExecFake(t, bin, "worker")
	fake.On(Match{}, Reply{Stdout: "ok\n"})
	stop := make(chan struct{})
	var group sync.WaitGroup
	var mu sync.Mutex
	runs := 0
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				output, err := exec.Command(filepath.Join(bin, "worker"), "run").CombinedOutput()
				if err != nil || string(output) != "ok\n" {
					t.Errorf("call during rule change: %v %q", err, output)
					return
				}
				mu.Lock()
				runs++
				mu.Unlock()
			}
		}()
	}
	for index := range 200 {
		fake.On(Match{Args: []string{"never", fmt.Sprint(index)}}, Reply{Exit: 1})
	}
	close(stop)
	group.Wait()
	if calls := fake.Calls(); len(calls) != runs || runs == 0 {
		t.Fatalf("recorded %d of %d calls", len(calls), runs)
	}
}

func TestExecFakeRecordsWithShellBuiltinsOnly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the read-only record directory")
	}
	recorder := &cleanupRecorder{TB: t}
	bin := TempDir(t)
	fake := NewExecFake(recorder, bin, "tool")
	fake.On(Match{Args: []string{"script"}}, Reply{Stdout: "out 'quoted'\n", Stderr: "err $HOME\n", Script: `printf '%s|' "$@"; exit 4`})
	run := func(arguments ...string) (string, string, int) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		command := exec.Command(filepath.Join(bin, "tool"), arguments...)
		command.Env, command.Stdout, command.Stderr = []string{"PATH=/nonexistent"}, &stdout, &stderr
		err := command.Run()
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Fatal(err)
		}
		return stdout.String(), stderr.String(), command.ProcessState.ExitCode()
	}
	if stdout, stderr, code := run("script"); stdout != "out 'quoted'\nscript|" || stderr != "err $HOME\n" || code != 4 {
		t.Fatalf("builtin reply: stdout=%q stderr=%q code=%d", stdout, stderr, code)
	}
	records := filepath.Join(fake.state, "records")
	if err := os.Chmod(records, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(records, 0o700) })
	if stdout, stderr, code := run("script"); code != 126 || stdout != "" || !strings.Contains(stderr, "cannot record a call to tool") {
		t.Fatalf("unrecorded call: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if err := os.Chmod(records, 0o700); err != nil {
		t.Fatal(err)
	}
	fake.AssertCalls(Call{Tool: "tool", Args: []string{"script"}})
	// A record whose write failed after its claim is reported, not dropped.
	WriteFile(t, filepath.Join(records, "1"), nil, 0o600)
	recorder.runCleanups()
	if len(recorder.failures) != 1 || !strings.Contains(recorder.failures[0], "execfake: call 1: record does not end with NUL") {
		t.Fatalf("cleanup failures: %q", recorder.failures)
	}
}

func TestExecFakeCopiesRulesAndRefusesNUL(t *testing.T) {
	recorder := &cleanupRecorder{TB: t}
	bin := TempDir(t)
	fake := NewExecFake(recorder, bin, "tool")
	arguments := []string{"status"}
	fake.On(Match{Args: arguments}, Reply{Stdout: "status\n"})
	arguments[0] = "changed"
	fake.On(Match{Args: []string{"other"}}, Reply{})
	if stdout, _, code := runShim(t, filepath.Join(bin, "tool"), "status"); stdout != "status\n" || code != 0 {
		t.Fatalf("registered rule changed with its caller's slice: stdout=%q code=%d", stdout, code)
	}
	fake.On(Match{Contains: []string{"a\x00b"}}, Reply{})
	fake.On(Match{}, Reply{Stdout: "a\x00b"})
	if len(recorder.failures) != 2 {
		t.Fatalf("NUL rules accepted: %q", recorder.failures)
	}
}

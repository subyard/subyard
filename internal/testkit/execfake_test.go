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
	const workers, calls = 8, 25
	padding := strings.Repeat("x", 512)
	var group sync.WaitGroup
	for worker := range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			for call := range calls {
				command := exec.Command(filepath.Join(bin, "worker"), fmt.Sprint(worker), fmt.Sprint(call), padding)
				if output, err := command.CombinedOutput(); err != nil {
					t.Errorf("worker %d call %d: %v %s", worker, call, err, output)
				}
			}
		}()
	}
	group.Wait()
	recorded := fake.Calls()
	seen := map[string]bool{}
	for _, call := range recorded {
		if call.Tool != "worker" || len(call.Args) != 3 || call.Args[2] != padding {
			t.Fatalf("torn record: %#v", call)
		}
		seen[call.Args[0]+"/"+call.Args[1]] = true
	}
	if len(recorded) != workers*calls || len(seen) != workers*calls {
		t.Fatalf("recorded %d calls, %d distinct", len(recorded), len(seen))
	}
}

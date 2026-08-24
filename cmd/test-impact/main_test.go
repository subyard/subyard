package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/Subyard/Subyard/internal/testimpact"
)

type commandResult struct {
	SchemaVersion  int              `json:"schema_version"`
	Status         string           `json:"status"`
	Changes        []map[string]any `json:"changes"`
	CheckSets      []string         `json:"check_sets"`
	RiskDomains    []string         `json:"risk_domains"`
	HostFreeChecks []struct {
		ID string `json:"id"`
	} `json:"host_free_checks"`
	E2EChecks []map[string]any `json:"e2e_checks"`
	FullP0    struct {
		Required bool `json:"required"`
	} `json:"full_p0"`
	Errors []struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
}

func TestRunEquivalentSourcesReturnSameSelectedJSON(t *testing.T) {
	repo, base, head := newCommandRepository(t)
	changeDocument := `{"schema_version":1,"changes":[{"status":"A","similarity":null,"old_path":null,"new_path":"internal/configsync/sync.go","old_mode":"000000","new_mode":"100644"}]}`
	changeFile := filepath.Join(t.TempDir(), "changes.json")
	if err := os.WriteFile(changeFile, []byte(changeDocument), 0o600); err != nil {
		t.Fatalf("write change document: %v", err)
	}

	tests := []struct {
		name  string
		args  []string
		stdin io.Reader
	}{
		{"current", []string{"--format", "json", "--current-base", base}, strings.NewReader("")},
		{"commit", []string{"--head", head, "--format", "json", "--base", base}, strings.NewReader("")},
		{"file", []string{"--changes-from", changeFile, "--format", "json"}, strings.NewReader("")},
		{"stdin", []string{"--format", "json", "--changes-from", "-"}, strings.NewReader(changeDocument)},
	}

	var reference string
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status, stdout, stderr := invokeRun(test.args, test.stdin, repo)
			if status != 0 {
				t.Fatalf("run() status = %d, want 0; stderr=%q", status, stderr)
			}
			if stderr != "" {
				t.Fatalf("run() stderr = %q, want empty", stderr)
			}
			result := decodeSingleResult(t, stdout)
			if result.Status != "selected" || len(result.Changes) != 1 {
				t.Fatalf("result = %#v, want one selected change", result)
			}
			if reference == "" {
				reference = stdout
			} else if stdout != reference {
				t.Fatalf("equivalent source output differs:\n%s\n%s", reference, stdout)
			}
		})
	}
}

func TestRunRejectsCLIMisuseWithExitTwoAndNoPlan(t *testing.T) {
	repo, base, head := newCommandRepository(t)
	tests := []struct {
		name     string
		args     []string
		wantJSON bool
	}{
		{"missing source", []string{"--format", "json"}, true},
		{"unknown flag", []string{"--format", "json", "--unknown"}, true},
		{"unknown flag before format", []string{"--unknown", "--format", "json"}, true},
		{"duplicate flag", []string{"--format", "json", "--base", base, "--base", base, "--head", head}, true},
		{"conflicting sources", []string{"--format", "json", "--current-base", base, "--changes-from", "-"}, true},
		{"missing value", []string{"--format", "json", "--base", "--head", head}, true},
		{"base without head", []string{"--format", "json", "--base", base}, true},
		{"head without base", []string{"--format", "json", "--head", head}, true},
		{"duplicate format", []string{"--format", "json", "--format", "json", "--current-base", base}, true},
		{"invalid format", []string{"--format", "yaml", "--current-base", base}, false},
		{"positional argument", []string{"--format", "json", "--current-base", base, "extra"}, true},
	}

	wantJSON := `{"schema_version":1,"status":"error","errors":[{"code":"CLI_MISUSE","message":"invalid command line"}]}` + "\n"
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status, stdout, stderr := invokeRun(test.args, strings.NewReader(""), repo)
			if status != 2 {
				t.Fatalf("run() status = %d, want 2", status)
			}
			if !test.wantJSON {
				if !strings.Contains(stdout, `status: "error"`) {
					t.Fatalf("run() human stdout = %q, want error status", stdout)
				}
				return
			}
			if stdout != wantJSON {
				t.Fatalf("run() stdout = %q, want minimal error %q", stdout, wantJSON)
			}
			if stderr != "test-impact: CLI_MISUSE: invalid command line\n" {
				t.Fatalf("run() stderr = %q", stderr)
			}
		})
	}
}

func TestRunAnalysisFailuresReturnUniversalFallback(t *testing.T) {
	repo, _, _ := newCommandRepository(t)
	invalidFile := filepath.Join(t.TempDir(), "invalid.json")
	if err := os.WriteFile(invalidFile, []byte(`{"schema_version":1,"changes":[`), 0o600); err != nil {
		t.Fatalf("write invalid document: %v", err)
	}
	unpairedSurrogateFile := filepath.Join(t.TempDir(), "unpaired-surrogate.json")
	unpairedSurrogateDocument := `{"schema_version":1,"changes":[{"status":"A","similarity":null,"old_path":null,"new_path":"docs/\ud800.md","old_mode":"000000","new_mode":"100644"}]}`
	if err := os.WriteFile(unpairedSurrogateFile, []byte(unpairedSurrogateDocument), 0o600); err != nil {
		t.Fatalf("write unpaired surrogate document: %v", err)
	}
	unmatchedFile := filepath.Join(t.TempDir(), "unmatched.json")
	unmatchedDocument := `{"schema_version":1,"changes":[{"status":"A","similarity":null,"old_path":null,"new_path":"product/unmapped.go","old_mode":"000000","new_mode":"100644"}]}`
	if err := os.WriteFile(unmatchedFile, []byte(unmatchedDocument), 0o600); err != nil {
		t.Fatalf("write unmatched document: %v", err)
	}

	tests := []struct {
		name        string
		args        []string
		wantCode    string
		wantChanges int
	}{
		{"invalid change JSON", []string{"--changes-from", invalidFile, "--format", "json"}, "CHANGE_SOURCE_INVALID", 0},
		{"unpaired Unicode surrogate", []string{"--changes-from", unpairedSurrogateFile, "--format", "json"}, "CHANGE_SOURCE_INVALID", 0},
		{"missing current ref", []string{"--current-base", "refs/heads/missing", "--format", "json"}, "CHANGE_SOURCE_INVALID", 0},
		{"unmatched path", []string{"--changes-from", unmatchedFile, "--format", "json"}, "UNMATCHED_PATH", 1},
	}

	wantLeaves := []string{"host-free:core", "veranda:build", "veranda:check", "veranda:rust-test", "veranda:test"}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status, stdout, stderr := invokeRun(test.args, strings.NewReader(""), repo)
			if status != 0 {
				t.Fatalf("run() status = %d, want 0", status)
			}
			result := decodeSingleResult(t, stdout)
			if result.Status != "fallback" || !result.FullP0.Required {
				t.Fatalf("fallback status/full P0 = %q/%t", result.Status, result.FullP0.Required)
			}
			if !reflect.DeepEqual(result.CheckSets, []string{"host-free:all"}) || len(result.RiskDomains) != 0 || len(result.E2EChecks) != 0 {
				t.Fatalf("fallback plan = checks %v domains %v e2e %v", result.CheckSets, result.RiskDomains, result.E2EChecks)
			}
			if got := hostFreeIDs(result); !reflect.DeepEqual(got, wantLeaves) {
				t.Fatalf("fallback leaves = %v, want %v", got, wantLeaves)
			}
			if len(result.Errors) != 1 || result.Errors[0].Code != test.wantCode {
				t.Fatalf("fallback errors = %#v, want code %s", result.Errors, test.wantCode)
			}
			if len(result.Changes) != test.wantChanges {
				t.Fatalf("fallback changes = %v, want %d", result.Changes, test.wantChanges)
			}
			if stderr != fmt.Sprintf("test-impact: %s: %s\n", result.Errors[0].Code, result.Errors[0].Message) {
				t.Fatalf("stderr = %q, want sanitized structured diagnostic", stderr)
			}
		})
	}
}

func TestRunRegistryValidationFailureReturnsOneUniversalFallbackDocument(t *testing.T) {
	repo, _, _ := newCommandRepository(t)
	var stdout, stderr bytes.Buffer
	status := runWithRegistryLoader(
		[]string{"--changes-from", "missing.json", "--format", "json"},
		strings.NewReader(""),
		&stdout,
		&stderr,
		repo,
		func() (testimpact.Registry, error) {
			return testimpact.Registry{}, errors.New("invalid embedded registry")
		},
	)
	if status != 0 {
		t.Fatalf("runWithRegistryLoader() status = %d, want 0", status)
	}
	result := decodeSingleResult(t, stdout.String())
	if result.Status != "fallback" || !result.FullP0.Required {
		t.Fatalf("result status/full P0 = %q/%t, want fallback/true", result.Status, result.FullP0.Required)
	}
	wantLeaves := []string{"host-free:core", "veranda:build", "veranda:check", "veranda:rust-test", "veranda:test"}
	if got := hostFreeIDs(result); !reflect.DeepEqual(got, wantLeaves) {
		t.Fatalf("fallback leaves = %v, want %v", got, wantLeaves)
	}
	if len(result.Errors) != 1 || result.Errors[0].Code != "REGISTRY_INVALID" {
		t.Fatalf("fallback errors = %#v, want REGISTRY_INVALID", result.Errors)
	}
	wantStderr := "test-impact: REGISTRY_INVALID: embedded check registry could not be validated\n"
	if stderr.String() != wantStderr {
		t.Fatalf("stderr = %q, want %q", stderr.String(), wantStderr)
	}
}

func TestEmergencyHostFreeChecksMatchBuiltInComposite(t *testing.T) {
	registry, err := testimpact.BuiltInRegistry()
	if err != nil {
		t.Fatalf("BuiltInRegistry() error = %v", err)
	}
	leaves, err := registry.Expand([]string{"host-free:all"})
	if err != nil {
		t.Fatalf("Expand(host-free:all) error = %v", err)
	}

	got := make([]string, 0, len(emergencyHostFreeChecks()))
	for _, check := range emergencyHostFreeChecks() {
		got = append(got, check.ID)
	}
	want := make([]string, 0, len(leaves))
	for _, leaf := range leaves {
		want = append(want, leaf.ID)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("emergency fallback IDs = %v, built-in composite IDs = %v", got, want)
	}
}

func TestRunValidatesPolicyBeforeReadingChangeSource(t *testing.T) {
	repo, _, _ := newCommandRepository(t)
	if err := os.WriteFile(filepath.Join(repo, "tests/impact-map.json"), []byte(`{"schema_version":2}`), 0o644); err != nil {
		t.Fatalf("replace impact map: %v", err)
	}
	unsafeMissing := filepath.Join(repo, "missing\n\x1b[2J.json")

	status, stdout, stderr := invokeRun([]string{"--changes-from", unsafeMissing, "--format", "json"}, strings.NewReader(""), repo)
	if status != 0 {
		t.Fatalf("run() status = %d, want 0", status)
	}
	result := decodeSingleResult(t, stdout)
	if len(result.Errors) != 1 || result.Errors[0].Code != "POLICY_INVALID" {
		t.Fatalf("errors = %#v, want policy validation failure before source read", result.Errors)
	}
	if strings.Contains(stdout, unsafeMissing) || strings.Contains(stderr, unsafeMissing) || strings.ContainsRune(stderr, '\x1b') {
		t.Fatalf("unsafe path leaked: stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestRunCommitModeRejectsNonCurrentHeadAndDirtySelectorFiles(t *testing.T) {
	t.Run("non-current head", func(t *testing.T) {
		repo, base, _ := newCommandRepository(t)
		status, stdout, _ := invokeRun([]string{"--base", base, "--head", base, "--format", "json"}, strings.NewReader(""), repo)
		if status != 0 {
			t.Fatalf("run() status = %d, want 0", status)
		}
		result := decodeSingleResult(t, stdout)
		if result.Status != "fallback" || len(result.Errors) != 1 || result.Errors[0].Code != "COMMIT_GUARD_FAILED" {
			t.Fatalf("result = %#v, want guarded fallback", result)
		}
	})

	t.Run("dirty selector path", func(t *testing.T) {
		repo, base, head := newCommandRepository(t)
		path := filepath.Join(repo, "dev/test-impact.sh")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir dev: %v", err)
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatalf("write dirty selector path: %v", err)
		}
		status, stdout, _ := invokeRun([]string{"--base", base, "--head", head, "--format", "json"}, strings.NewReader(""), repo)
		if status != 0 {
			t.Fatalf("run() status = %d, want 0", status)
		}
		result := decodeSingleResult(t, stdout)
		if result.Status != "fallback" || len(result.Errors) != 1 || result.Errors[0].Code != "COMMIT_GUARD_FAILED" {
			t.Fatalf("result = %#v, want guarded fallback", result)
		}
	})

	t.Run("unrelated unsupported untracked path remains allowed", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("FIFO fixture is Unix-specific")
		}
		repo, base, head := newCommandRepository(t)
		if err := syscall.Mkfifo(filepath.Join(repo, "unrelated-fifo"), 0o600); err != nil {
			t.Fatalf("create unrelated FIFO: %v", err)
		}
		status, stdout, stderr := invokeRun([]string{"--base", base, "--head", head, "--format", "json"}, strings.NewReader(""), repo)
		if status != 0 {
			t.Fatalf("run() status = %d, want 0", status)
		}
		result := decodeSingleResult(t, stdout)
		if result.Status != "selected" || stderr != "" {
			t.Fatalf("unrelated untracked path changed commit result: result=%#v stderr=%q", result, stderr)
		}
	})

	t.Run("unrelated unmerged path remains allowed", func(t *testing.T) {
		repo, _, originalHead := newCommandRepository(t)
		gitCommand(t, repo, "checkout", "--quiet", "-b", "left")
		writeCommandFile(t, repo, "docs/conflict.md", []byte("left\n"))
		gitCommand(t, repo, "add", "--all")
		gitCommand(t, repo, "commit", "--quiet", "-m", "left")
		gitCommand(t, repo, "checkout", "--quiet", "-b", "right", originalHead)
		writeCommandFile(t, repo, "docs/conflict.md", []byte("right\n"))
		gitCommand(t, repo, "add", "--all")
		gitCommand(t, repo, "commit", "--quiet", "-m", "right")
		rightHead := gitCommandOutput(t, repo, "rev-parse", "HEAD")
		merge := exec.Command("git", "-C", repo, "merge", "left")
		if output, err := merge.CombinedOutput(); err == nil {
			t.Fatalf("git merge unexpectedly succeeded: %s", output)
		}

		status, stdout, stderr := invokeRun([]string{"--base", originalHead, "--head", rightHead, "--format", "json"}, strings.NewReader(""), repo)
		if status != 0 {
			t.Fatalf("run() status = %d, want 0", status)
		}
		result := decodeSingleResult(t, stdout)
		if result.Status != "selected" || stderr != "" {
			t.Fatalf("unrelated unmerged path changed commit result: result=%#v stderr=%q", result, stderr)
		}
	})
}

func TestReadChangeSourceFreezesResolvedCommitIDsBeforeRefsMove(t *testing.T) {
	const (
		baseRef = "refs/heads/base"
		headRef = "refs/heads/head"
		baseOID = "1111111111111111111111111111111111111111"
		headOID = "2222222222222222222222222222222222222222"
	)

	refsMoved := false
	var analyzedBase, analyzedHead string
	operations := commitSourceOperations{
		resolveCommit: func(_ context.Context, _, ref string) (string, error) {
			if refsMoved {
				return "3333333333333333333333333333333333333333", nil
			}
			switch ref {
			case baseRef:
				return baseOID, nil
			case headRef, "HEAD":
				return headOID, nil
			default:
				return "", errors.New("unexpected ref")
			}
		},
		selectorPathsDirty: func(context.Context, string) (bool, error) {
			refsMoved = true
			return false, nil
		},
		readCommitChanges: func(_ context.Context, _, base, head string) (testimpact.ChangeSet, error) {
			analyzedBase, analyzedHead = base, head
			if base != baseOID || head != headOID {
				return testimpact.ChangeSet{}, errors.New("symbolic ref reached diff reader")
			}
			return testimpact.ChangeSet{SchemaVersion: 1}, nil
		},
	}

	changeSet, resultError := readChangeSourceWithCommitOperations(
		context.Background(),
		"/trusted/repository",
		commandOptions{base: baseRef, head: headRef},
		strings.NewReader(""),
		operations,
	)
	if resultError != nil {
		t.Fatalf("readChangeSourceWithCommitOperations() error = %#v", resultError)
	}
	if changeSet.SchemaVersion != 1 {
		t.Fatalf("change set = %#v, want fake reader result", changeSet)
	}
	if analyzedBase != baseOID || analyzedHead != headOID {
		t.Fatalf("analyzed IDs = %q..%q, want %q..%q", analyzedBase, analyzedHead, baseOID, headOID)
	}
}

func TestRunHumanOutputAndDiagnosticsEscapeUnsafePathsDeterministically(t *testing.T) {
	repo, _, _ := newCommandRepository(t)
	unsafePath := "product/line\nnext\t\x1b[31m.go"
	encodedPath, err := json.Marshal(unsafePath)
	if err != nil {
		t.Fatalf("marshal unsafe path fixture: %v", err)
	}
	document := fmt.Sprintf(`{"schema_version":1,"changes":[{"status":"A","similarity":null,"old_path":null,"new_path":%s,"old_mode":"000000","new_mode":"100644"}]}`, encodedPath)
	changeFile := filepath.Join(t.TempDir(), "controls.json")
	if err := os.WriteFile(changeFile, []byte(document), 0o600); err != nil {
		t.Fatalf("write control fixture: %v", err)
	}

	firstStatus, firstStdout, firstStderr := invokeRun([]string{"--changes-from", changeFile}, strings.NewReader(""), repo)
	secondStatus, secondStdout, secondStderr := invokeRun([]string{"--changes-from", changeFile}, strings.NewReader(""), repo)
	if firstStatus != 0 || secondStatus != 0 || firstStdout != secondStdout || firstStderr != secondStderr {
		t.Fatalf("repeated output differs: status=%d/%d stdout=%q/%q stderr=%q/%q",
			firstStatus, secondStatus, firstStdout, secondStdout, firstStderr, secondStderr)
	}
	if strings.ContainsRune(firstStdout, '\x1b') || strings.ContainsRune(firstStderr, '\x1b') || strings.Contains(firstStdout, "line\nnext") {
		t.Fatalf("raw controls leaked: stdout=%q stderr=%q", firstStdout, firstStderr)
	}
	if !strings.Contains(firstStdout, `"product/line\nnext\t\x1b[31m.go"`) {
		t.Fatalf("human output did not visibly escape path: %q", firstStdout)
	}
}

func invokeRun(args []string, stdin io.Reader, root string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	status := run(args, stdin, &stdout, &stderr, root)
	return status, stdout.String(), stderr.String()
}

func decodeSingleResult(t *testing.T, output string) commandResult {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(output))
	var result commandResult
	if err := decoder.Decode(&result); err != nil {
		t.Fatalf("decode stdout JSON %q: %v", output, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("stdout contains more than one JSON document: %q", output)
	}
	return result
}

func hostFreeIDs(result commandResult) []string {
	ids := make([]string, 0, len(result.HostFreeChecks))
	for _, check := range result.HostFreeChecks {
		ids = append(ids, check.ID)
	}
	return ids
}

func newCommandRepository(t *testing.T) (repo, base, head string) {
	t.Helper()
	repo = t.TempDir()
	gitCommand(t, repo, "init", "--quiet")
	gitCommand(t, repo, "config", "user.name", "Test User")
	gitCommand(t, repo, "config", "user.email", "test@example.com")

	mapSource, err := os.ReadFile(filepath.Join(publicRepositoryRoot(t), "tests/impact-map.json"))
	if err != nil {
		t.Fatalf("read repository impact map: %v", err)
	}
	writeCommandFile(t, repo, "tests/impact-map.json", mapSource)
	writeCommandFile(t, repo, "README.md", []byte("fixture\n"))
	gitCommand(t, repo, "add", "--all")
	gitCommand(t, repo, "commit", "--quiet", "-m", "base")
	base = gitCommandOutput(t, repo, "rev-parse", "HEAD")

	writeCommandFile(t, repo, "internal/configsync/sync.go", []byte("package configsync\n"))
	gitCommand(t, repo, "add", "--all")
	gitCommand(t, repo, "commit", "--quiet", "-m", "head")
	head = gitCommandOutput(t, repo, "rev-parse", "HEAD")
	return repo, base, head
}

func publicRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller() failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(current), "../.."))
}

func writeCommandFile(t *testing.T, repo, name string, contents []byte) {
	t.Helper()
	path := filepath.Join(repo, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", name, err)
	}
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func gitCommand(t *testing.T, repo string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repo}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func gitCommandOutput(t *testing.T, repo string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repo}, args...)...)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(output))
}

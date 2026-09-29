package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

func TestSSHAgentStatusDoesNotCreateState(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	var out, stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"ssh-agent", "status", "--json"}, Environment: environment, WorkingDir: root, Stdout: &out, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 || !strings.Contains(out.String(), `"state":"locked"`) {
		t.Fatalf("code=%d output=%s stderr=%s", code, &out, &stderr)
	}
	if _, err := os.Stat(filepath.Join(root, "data")); !os.IsNotExist(err) {
		t.Fatalf("status created data: %v", err)
	}
}

type sshAgentDispatchExecutor struct {
	requests []ports.InstanceExecRequest
	failAt   int
}

func (executor *sshAgentDispatchExecutor) Exec(
	_ context.Context, _, _ string, request ports.InstanceExecRequest,
) (ports.InstanceExecResult, error) {
	request.Command = append([]string(nil), request.Command...)
	request.Stdin = append([]byte(nil), request.Stdin...)
	executor.requests = append(executor.requests, request)
	if executor.failAt == len(executor.requests) {
		return ports.InstanceExecResult{ExitCode: 9}, nil
	}
	return ports.InstanceExecResult{}, nil
}

func TestSSHAgentEnvironmentIncludesDeselectedProfileHooksAndStopsOnFailure(t *testing.T) {
	root, _, _ := nativeFixture(t)
	writeCLIFile(t, filepath.Join(root, "scripts", "ssh-agent-environment.sh"), "generic", 0o700)
	for _, item := range []struct{ name, body string }{
		{"selected", "selected hook"}, {"deselected", "deselected hook"},
	} {
		directory := filepath.Join(root, "config", "profiles", item.name)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		writeCLIFile(t, filepath.Join(directory, "profile.json"), `{"schema_version":1,"guest_environment":{"handler":"guest.sh"}}`, 0o600)
		writeCLIFile(t, filepath.Join(directory, "guest.sh"), item.body, 0o700)
	}
	catalog, err := config.LoadCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	scripts, err := sshAgentEnvironmentScripts(root, catalog.Profiles())
	if err != nil {
		t.Fatal(err)
	}
	if len(scripts) != 3 || string(scripts[0]) != "generic" || string(scripts[1]) != "deselected hook" || string(scripts[2]) != "selected hook" {
		t.Fatalf("dispatch order/payloads: %q", scripts)
	}
	yard := domain.Context{IncusProject: "subyard", YardInstanceName: "yard", DevUser: "dev"}
	executor := &sshAgentDispatchExecutor{}
	if err := applySSHAgentEnvironment(context.Background(), executor, yard, scripts); err != nil {
		t.Fatal(err)
	}
	if len(executor.requests) != 3 {
		t.Fatalf("executed %d setup scripts, want generic plus both profile hooks", len(executor.requests))
	}
	for _, request := range executor.requests {
		if strings.Join(request.Command, " ") != "sh -eu -s -- ensure dev" {
			t.Fatalf("unexpected guest setup command %q", request.Command)
		}
	}
	executor = &sshAgentDispatchExecutor{failAt: 2}
	if err := applySSHAgentEnvironment(context.Background(), executor, yard, scripts); err == nil || len(executor.requests) != 2 {
		t.Fatalf("failed hook was ignored or dispatch continued: requests=%d err=%v", len(executor.requests), err)
	}
	if err := os.Remove(filepath.Join(root, "config", "profiles", "deselected", "guest.sh")); err != nil {
		t.Fatal(err)
	}
	if _, err := sshAgentEnvironmentScripts(root, catalog.Profiles()); err == nil {
		t.Fatal("missing snapshotted hook was accepted")
	}
}

func TestSSHAgentArgumentBoundaries(t *testing.T) {
	for _, args := range [][]string{
		{"unlock"}, {"unlock", "--key", "key"}, {"unlock", "--ttl", "2h"},
		{"unlock", "--key", "key", "--ttl", "0"},
		{"unlock", "--key", "key", "--ttl", "-1s"},
		{"unlock", "--key", "key", "--ttl", "1.5s"},
		{"unlock", "--key", "key", "--ttl", "0d"},
		{"unlock", "--key", "key", "--ttl", "-1d"},
		{"unlock", "--key", "key", "--ttl", "1.5d"},
		{"unlock", "--key", "key", "--ttl", "999999999999999999999d"},
		{"unlock", "--key", "key", "--ttl", "24856d"},
		{"unlock", "--key", "key", "--ttl", "1d2h"},
		{"unlock", "--key", "key", "--ttl", "2147483648s"},
		{"unlock", "--key", "key", "--ttl", "2h", "--ttl", "3h"},
		{"status", "unlock"}, {"status", "--key", "key"}, {"lock", "--ttl", "1h"},
		{"lock", "--json"}, {"unlock", "--key", "--ttl", "2h"},
	} {
		if _, err := parseSSHAgentArguments(args); err == nil {
			t.Fatalf("accepted invalid arguments %q", args)
		}
	}
	got, err := parseSSHAgentArguments([]string{"--yes", "unlock", "--key", "key with spaces", "--ttl", "2h"})
	if err != nil || got.key != "key with spaces" || got.ttl != 2*time.Hour || !got.yes {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	for value, want := range map[string]time.Duration{
		"25h": 25 * time.Hour, "336h": 14 * 24 * time.Hour,
		"14d": 14 * 24 * time.Hour, "365d": 365 * 24 * time.Hour,
		"8760h": 365 * 24 * time.Hour, "2147483647s": 2147483647 * time.Second,
		"24855d": 24855 * 24 * time.Hour,
	} {
		got, err := parseSSHAgentArguments([]string{"unlock", "--key", "key", "--ttl", value})
		if err != nil || got.ttl != want {
			t.Errorf("TTL %q: got=%s want=%s err=%v", value, got.ttl, want, err)
		}
	}
}

func TestSSHAgentUnlockNeedsTerminalEvenWithConsent(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	var out bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"ssh-agent", "unlock", "--key", "absent", "--ttl", "2h", "--yes"}, Environment: environment, WorkingDir: root, Stderr: &out})
	if err != nil {
		t.Fatal(err)
	}
	program.operatorTerminal = func() bool { return false }
	if code := program.Run(context.Background()); code != 1 || !strings.Contains(out.String(), "requires a terminal") {
		t.Fatalf("code=%d err=%s", code, &out)
	}
}

func TestSSHAgentLockIsPromptFreeAndUnlockAssessesAccess(t *testing.T) {
	registry, err := application.NewCoreActionRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		action domain.ActionID
		policy domain.ActionConfirmationPolicy
	}{
		{"ssh-agent.lock", domain.ActionConfirmationNever},
		{"ssh-agent.unlock", domain.ActionConfirmationPromptDefaultYes},
	} {
		assessment, err := registry.Assess(item.action, domain.ActionDelta{Changed: true, Consequences: []string{"change temporary key access"}})
		if err != nil {
			t.Fatal(err)
		}
		policy, _, err := registry.Resolve(assessment)
		if err != nil || policy != item.policy {
			t.Fatalf("%s: %s %v", item.action, policy, err)
		}
	}
}

func TestSSHAgentRejectsInvalidKeyBeforeGuestPreparation(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	key := filepath.Join(root, "invalid-key")
	writeCLIFile(t, key, "invalid private key", 0600)
	var out bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"ssh-agent", "unlock", "--key", key, "--ttl", "2h", "--yes"}, Environment: environment, WorkingDir: root, Stderr: &out})
	if err != nil {
		t.Fatal(err)
	}
	program.operatorTerminal = func() bool { return true }
	if code := program.Run(context.Background()); code != 1 || !strings.Contains(out.String(), "encrypted private key") {
		t.Fatalf("invalid key reached transport preparation: code=%d err=%s", code, &out)
	}
}

func TestSSHAgentRevocationRemainsAvailableDuringReleaseRecovery(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	runtimeRoot := filepath.Join(root, "runtime-agent-recovery")
	environment = append(environment, "YARD_RUNTIME_ROOT="+runtimeRoot, "V2_GATE_CAPTURE="+filepath.Join(root, "capture"))
	journal, _ := installUnfinishedV2MutationGateFixture(t, root, environment, runtimeRoot)
	before, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"status", "lock"} {
		var stderr bytes.Buffer
		program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"ssh-agent", verb}, Environment: environment, WorkingDir: root, Stderr: &stderr})
		if err != nil {
			t.Fatal(err)
		}
		if code := program.Run(context.Background()); code != 0 {
			t.Fatalf("%s: code=%d %s", verb, code, &stderr)
		}
	}
	after, err := os.ReadFile(journal)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("SSH agent inspection/revocation changed release journal")
	}
}

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
	"github.com/Subyard/Subyard/internal/domain"
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

func TestSSHAgentArgumentBoundaries(t *testing.T) {
	for _, args := range [][]string{
		{"unlock"}, {"unlock", "--key", "key"}, {"unlock", "--ttl", "2h"},
		{"unlock", "--key", "key", "--ttl", "0"},
		{"unlock", "--key", "key", "--ttl", "-1s"},
		{"unlock", "--key", "key", "--ttl", "1.5s"},
		{"unlock", "--key", "key", "--ttl", "25h"},
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

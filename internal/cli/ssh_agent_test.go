package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/domain"
)

func TestSSHAgentCLIHelpPrintsWithoutRuntimeEffects(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	var stdout, stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Program: "yard",
		Arguments: []string{"ssh-agent", "--help"}, Environment: environment,
		WorkingDir: root, Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 || !strings.Contains(stdout.String(), "Usage: yard [-Y NAME] ssh-agent") {
		t.Fatalf("help code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(root, "data", "ssh-agent")); !os.IsNotExist(err) {
		t.Fatalf("help created runtime state: err=%v", err)
	}
}

func TestSSHAgentCLIStatusJSONIsReadOnly(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	dataHome := filepath.Join(root, "data")
	if err := os.MkdirAll(dataHome, 0o700); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Program: "yard",
		Arguments: []string{"ssh-agent", "status", "--json"}, Environment: environment,
		WorkingDir: root, Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("status code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var status struct {
		Yard, State string
	}
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil || status.Yard != "default" || status.State != "stopped" {
		t.Fatalf("status=%#v err=%v stdout=%q", status, err, stdout.String())
	}
	if _, err := os.Stat(filepath.Join(dataHome, "ssh-agent")); !os.IsNotExist(err) {
		t.Fatalf("status created runtime state: err=%v", err)
	}
}

func TestSSHAgentCLIRemoteSelectionIsRejected(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	environment = append(environment, "ACCESS_KIND=remote", "OWNER_ENDPOINT=owner.example", "OWNER_YARD_NAME=default")
	var stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Program: "yard",
		Arguments: []string{"ssh-agent", "status"}, Environment: environment,
		WorkingDir: root, Stdout: io.Discard, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 || !strings.Contains(stderr.String(), "owner-host only") {
		t.Fatalf("remote code=%d stderr=%q", code, stderr.String())
	}
}

func TestSSHAgentAuditArgumentsRedactKey(t *testing.T) {
	got := sshAgentAuditArguments([]string{"start", "--key", "/secret/id_ed25519", "--ttl", "14d"})
	if want := []string{"start", "--key", "<redacted>", "--ttl", "14d"}; !slices.Equal(got, want) {
		t.Fatalf("redacted arguments = %#v, want %#v", got, want)
	}
	got = sshAgentAuditArguments([]string{"start", "--key=/secret/id_ed25519"})
	if want := []string{"start", "--key=<redacted>"}; !slices.Equal(got, want) {
		t.Fatalf("equals redacted arguments = %#v, want %#v", got, want)
	}
}

func TestSSHAgentActionsRegistered(t *testing.T) {
	registry, err := application.NewCoreActionRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []domain.ActionID{"ssh-agent.help", "ssh-agent.status"} {
		assessment, err := registry.Assess(action, domain.ActionDelta{})
		if err != nil || assessment.Effect != domain.ActionRead {
			t.Fatalf("read action %q = %#v, err=%v", action, assessment, err)
		}
	}
	for _, action := range []domain.ActionID{"ssh-agent.start", "ssh-agent.stop"} {
		assessment, err := registry.Assess(action, domain.ActionDelta{Changed: true, Consequences: []string{"change SSH-agent access"}})
		if err != nil || assessment.Effect != domain.ActionMutation {
			t.Fatalf("mutation action %q = %#v, err=%v", action, assessment, err)
		}
		policy, request, err := registry.Resolve(assessment)
		if err != nil || policy != domain.ActionConfirmationPromptDefaultYes || request == nil {
			t.Fatalf("mutation policy %q = %q, %#v, err=%v", action, policy, request, err)
		}
	}
	assessment, _ := registry.Assess("ssh-agent.stop", domain.ActionDelta{})
	policy, request, err := registry.Resolve(assessment)
	if err != nil || policy != domain.ActionConfirmationNever || request != nil {
		t.Fatalf("unchanged stop policy = %q, %#v, err=%v", policy, request, err)
	}
}

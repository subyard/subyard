package reconcileruntime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

type grantRecorder struct{ calls []ports.InstanceExecRequest }

func (recorder *grantRecorder) Exec(_ context.Context, _, _ string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	recorder.calls = append(recorder.calls, request)
	return ports.InstanceExecResult{}, nil
}

// Under prompt, also when unset, the grant follows the agent policies: an interrupted
// policy installation publishes nothing, and yes and no never publish from this step.
func TestPromptSudoGrantFollowsAgentPolicies(t *testing.T) {
	interrupted := errors.New("policy installation interrupted")
	for _, test := range []struct {
		mode    string
		install error
		grants  int
	}{
		{"prompt", interrupted, 0}, {"prompt", nil, 1}, {"", interrupted, 0}, {"", nil, 1}, {"yes", nil, 0}, {"no", nil, 0}, {"yes", interrupted, 0},
	} {
		recorder := &grantRecorder{}
		environment := []string{"DEV_USER=dev"}
		if test.mode != "" {
			environment = append(environment, "DEV_SUDO="+test.mode)
		}
		runtime := Runtime{Executor: recorder, Yard: domain.Context{DevUser: "dev"}, Environment: environment}
		installed := false
		err := runtime.withPromptSudoGrant(context.Background(), func() error { installed = true; return test.install })
		if !installed || !errors.Is(err, test.install) {
			t.Fatalf("%s: install ran=%t error=%v", test.mode, installed, err)
		}
		if len(recorder.calls) != test.grants {
			t.Fatalf("%s with install error %v: %d grant calls, want %d", test.mode, test.install, len(recorder.calls), test.grants)
		}
		if test.grants == 1 && (recorder.calls[0].Command[len(recorder.calls[0].Command)-1] != "dev" || !strings.Contains(recorder.calls[0].Command[2], "NOPASSWD:ALL")) {
			t.Fatalf("grant call: %#v", recorder.calls[0].Command)
		}
	}
}

// Base provisioning keeps an existing prompt grant but never creates one.
func TestBaseProvisionDefersNewPromptSudoGrant(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "..", "scripts", "04-provision-subyard.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	start := strings.Index(text, "reconcile_dev_sudo() {")
	end := strings.Index(text, "\nreconcile_dev_sudo \"")
	if start < 0 || end < start {
		t.Fatal("reconcile_dev_sudo is missing")
	}
	root := testkit.TempDir(t)
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(bin, "visudo"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	for _, test := range []struct {
		mode           string
		existing, want bool
	}{
		{"prompt", false, false}, {"prompt", true, true}, {"yes", false, true}, {"no", true, false},
	} {
		sudoers := filepath.Join(root, "90-subyard-dev")
		_ = os.Remove(sudoers)
		if test.existing {
			testkit.WriteFile(t, sudoers, []byte("dev ALL=(ALL) NOPASSWD:ALL\n"), 0o440)
		}
		command := exec.Command("bash", "-euc", text[start:end]+"\nreconcile_dev_sudo \"$1\"\n", "test", sudoers)
		command.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "DEV_USER=dev", "DEV_SUDO=" + test.mode}
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v %s", test.mode, err, output)
		}
		if _, err := os.Stat(sudoers); (err == nil) != test.want {
			t.Fatalf("%s with existing=%t: grant present=%t, want %t", test.mode, test.existing, err == nil, test.want)
		}
	}
}

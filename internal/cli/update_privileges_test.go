package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

// Keep the real CLI consent, draft publication and native inspection recheck;
// substitute only the sealed engine and sudo executable.
func TestUpdateAuthorizesRootBeforePostConsentInspection(t *testing.T) {
	for _, scenario := range []struct {
		name, resource, order string
		refuse                bool
	}{
		{name: "authorize before inspection", resource: "host-power", order: "sudo -n true\ninspect\n"},
		{name: "refusal stops before inspection", resource: "host-power", refuse: true, order: "sudo -n true\nsudo -v\n"},
		{name: "broker may be unprivileged", resource: "test-vm-broker", order: "inspect\n"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root, environment, runtimeRoot := updateReleaseFixture(t)
			installer := filepath.Join(root, "scripts", "install-runtime-release.sh")
			payload, err := os.ReadFile(installer)
			if err != nil {
				t.Fatal(err)
			}
			before := `"consequences":["activate verified runtime 1.2.3"]},"outcome"`
			after := `"consequences":["activate verified runtime 1.2.3"]},"decisions":[{"resource":"activation.` + scenario.resource + `","scope":"activation","decision":"canonicalize","result":"converged"}],"outcome"`
			if strings.Count(string(payload), before) != 1 {
				t.Fatal("fixture inspection boundary changed")
			}
			testkit.WriteFile(t, installer, []byte(strings.Replace(string(payload), before, after, 1)), 0o700)
			log := filepath.Join(root, "order.log")
			bin := filepath.Join(root, "bin")
			if err := os.MkdirAll(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(bin, "sudo"), []byte("#!/bin/sh\nprintf 'sudo %s\\n' \"$*\" >> \"$UPDATE_TRANSITION_CAPTURE\"\nexit \"$UPDATE_SUDO_RC\"\n"), 0o700)
			path := bin + ":" + os.Getenv("PATH")
			t.Setenv("PATH", path)
			prompt := &callbackPrompt{callback: func() {
				initial, err := os.ReadFile(log)
				if err != nil || strings.Contains(string(initial), "sudo") {
					t.Fatalf("preparation requested privileges: log=%q err=%v", initial, err)
				}
				testkit.WriteFile(t, log, nil, 0o600)
			}}
			sudoRC, wantCode := "0", 0
			if scenario.refuse {
				sudoRC, wantCode = "1", 1
			}
			var stdout, stderr bytes.Buffer
			program, err := New(Options{RepositoryRoot: root, WorkingDir: root, Program: "yard",
				Arguments:   []string{"update", "--version", "1.2.3", "--runtime-root", runtimeRoot},
				Environment: append(environment, "PATH="+path, "UPDATE_TRANSITION_CAPTURE="+log, "UPDATE_SUDO_RC="+sudoRC),
				Prompt:      prompt, Config: &recordingConfigApplier{}, Stdout: &stdout, Stderr: &stderr})
			if err != nil {
				t.Fatal(err)
			}
			program.effectiveUID = func() int { return 1000 }
			program.operatorTerminal = func() bool { return true }
			if code := program.Run(context.Background()); code != wantCode || len(prompt.requests) != 1 {
				t.Fatalf("code=%d confirmations=%d stderr=%q", code, len(prompt.requests), stderr.String())
			}
			order, err := os.ReadFile(log)
			if err != nil || !strings.HasPrefix(string(order), scenario.order) {
				t.Fatalf("unexpected post-consent order=%q err=%v", order, err)
			}
			if scenario.refuse {
				if string(order) != scenario.order {
					t.Fatalf("refused authorization reached inspection or mutation: %q", order)
				}
				if _, err := os.Stat(filepath.Join(runtimeRoot, "releases", "1.2.3-f16d05ec6b29")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("refused authorization published candidate: %v", err)
				}
			}
			if scenario.resource != "host-power" && strings.Contains(string(order), "sudo") {
				t.Fatalf("unprivileged update requested sudo: %q", order)
			}
		})
	}
}

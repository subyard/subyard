package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestConfigRegistrationRepairPreservesBothFilesUntilAuthorized(t *testing.T) {
	for _, mode := range []string{"check", "decline", "approve", "stale", "archive-conflict"} {
		t.Run(mode, func(t *testing.T) {
			root, _, configHome, environment := configCommandFixture(t)
			nested := filepath.Join(configHome, "yards", "named", "config.env")
			flat := filepath.Join(configHome, "yards", "named.env")
			archive := filepath.Join(configHome, "recovery", "yard-registrations", "named.env")
			const canonical = "YARD_TEMPLATE=test-vms\nSSH_PORT=2224\n"
			const shadowed = "YARD_TEMPLATE=e2e-vms\n# retained-secret-canary\nSSH_PORT=2234\n"
			writeConfigCommandFile(t, nested, canonical, 0o600)
			writeConfigCommandFile(t, flat, shadowed, 0o600)
			if mode == "archive-conflict" {
				writeConfigCommandFile(t, archive, "previous recovery\n", 0o600)
			}
			prompt := &testkit.Prompt{Answers: []bool{mode == "approve"}}
			arguments := []string{"config", "repair-registration", "named"}
			if mode == "check" {
				arguments = append(arguments, "--check")
			}
			var stdout, stderr bytes.Buffer
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard", Arguments: arguments,
				Environment: environment, WorkingDir: root, Prompt: prompt,
				Stdout: &stdout, Stderr: &stderr,
			})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "stale" {
				program.options.Prompt = &callbackPrompt{callback: func() {
					writeConfigCommandFile(t, nested, canonical+"# concurrent\n", 0o600)
				}}
			}
			code := program.Run(context.Background())
			wantSuccess := mode == "check" || mode == "approve"
			if (code == 0) != wantSuccess {
				t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			if strings.Contains(stdout.String()+stderr.String(), "retained-secret-canary") {
				t.Fatal("repair exposed configuration content")
			}
			if mode == "check" || mode == "approve" {
				for _, path := range []string{"yards/named/config.env", "yards/named.env", "recovery/yard-registrations/named.env"} {
					if !strings.Contains(stdout.String(), path) {
						t.Fatalf("repair plan omitted %s: %s", path, stdout.String())
					}
				}
			}
			if mode == "approve" {
				if len(prompt.Requests) != 1 || prompt.Requests[0].Default != domain.ConfirmationDefaultYes {
					t.Fatalf("repair confirmation = %#v", prompt.Requests)
				}
				if payload, err := os.ReadFile(archive); err != nil || string(payload) != shadowed {
					t.Fatalf("recovery copy not preserved: %v", err)
				}
				if _, err := os.Lstat(flat); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("shadowed registration still present: %v", err)
				}
			} else if payload, err := os.ReadFile(flat); err != nil || string(payload) != shadowed {
				t.Fatalf("unapproved/stale source changed: %v", err)
			}
			if mode == "stale" && !strings.Contains(stderr.String(), domain.ErrPlanStale.Error()) {
				t.Fatalf("missing stale diagnostic: %s", stderr.String())
			}
			if mode == "check" || mode == "archive-conflict" {
				if len(prompt.Requests) != 0 {
					t.Fatal("read-only or blocked repair prompted")
				}
			}
		})
	}
}

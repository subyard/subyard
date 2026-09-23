package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestTestVMRefreshUsesNormalConfirmationWithoutLeaseMutation(t *testing.T) {
	for _, accept := range []bool{false, true} {
		t.Run(map[bool]string{false: "decline", true: "accept"}[accept], func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			environment = append(environment, "NESTED_E2E_VMS=1", "SUBYARD_OPERATION_ID=refresh-base")
			incus := lifecycleIncus()
			instance := incus.Instances["subyard/yard"]
			instance.Status = "Running"
			incus.Instances["subyard/yard"] = instance
			prompt := &testkit.Prompt{Answers: []bool{accept}}
			if err := os.MkdirAll(filepath.Join(root, "scripts/e2e-lab"), 0o700); err != nil {
				t.Fatal(err)
			}
			writeCLIFile(t, filepath.Join(root, "scripts/e2e-lab/invoke.sh"), "#!/bin/sh\nprintf '%s\\n' \"$@\" > refresh-arguments\n", 0o700)
			var stderr bytes.Buffer
			probe := &testVMStatusProbe{}
			program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"test-vms", "refresh", "android-test"}, Environment: environment, WorkingDir: root, Incus: incus, ProjectData: probe, Prompt: prompt, Stderr: &stderr})
			if err != nil {
				t.Fatal(err)
			}
			code := program.Run(context.Background())
			if (accept && code != 0) || (!accept && code != 1) {
				t.Fatalf("exit=%d: %s", code, stderr.String())
			}
			if len(prompt.Requests) != 1 || prompt.Requests[0].Default != domain.ConfirmationDefaultYes {
				t.Fatalf("refresh confirmation=%+v", prompt.Requests)
			}
			if len(probe.requests) != 0 {
				t.Fatal("refresh probed or acquired a lease slot")
			}
			arguments, err := os.ReadFile(filepath.Join(root, "refresh-arguments"))
			if !accept && !errors.Is(err, os.ErrNotExist) {
				t.Fatal("decline mutated broker")
			}
			if accept && (err != nil || string(arguments) != "refresh-android-test\n--yes\n") {
				t.Fatalf("refresh arguments: %q, error: %v", arguments, err)
			}
		})
	}
}

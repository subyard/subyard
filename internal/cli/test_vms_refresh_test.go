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
	for _, image := range []string{"", "debian-13", "ubuntu-24.04", "ubuntu-26.04"} {
		for _, accept := range []bool{false, true} {
			t.Run(image+"/"+map[bool]string{false: "decline", true: "accept"}[accept], func(t *testing.T) {
				cliFixture := newFixture(t, withCommands("scripts/e2e-lab/invoke.sh"))
				root, environment := cliFixture.Root, cliFixture.Environment
				environment = append(environment, "NESTED_E2E_VMS=1", "SUBYARD_OPERATION_ID=refresh-base")
				incus := lifecycleIncus()
				instance := incus.Instances["subyard/yard"]
				instance.Status = "Running"
				incus.Instances["subyard/yard"] = instance
				prompt := &testkit.Prompt{Answers: []bool{accept}}
				if err := os.MkdirAll(filepath.Join(root, "scripts/e2e-lab"), 0o700); err != nil {
					t.Fatal(err)
				}
				var stderr bytes.Buffer
				probe := &testVMStatusProbe{output: []byte(`{"schema_version":1,"status":"ok","pool":{"schema_version":2,"resource_type":"agent-e2e","resource_id":"test-vms","slots":[{"slot_id":"slot-001","resource_generation":7,"lease_epoch":0,"state":"available"}]}}`)}
				arguments := []string{"test-vms", "refresh", "android-test"}
				expected := "refresh-android-test\n--yes\n"
				expectedArgs := []string{"refresh-android-test", "--yes"}
				if image != "" {
					arguments = append(arguments, "--image", image)
					expected += "--image\n" + image + "\n"
					expectedArgs = append(expectedArgs, "--image", image)
				}
				cliFixture.Exec().On(testkit.Match{Tool: "scripts/e2e-lab/invoke.sh", Args: expectedArgs},
					testkit.Reply{Script: "printf '%s\\n' \"$@\" > refresh-arguments"})
				program := cliFixture.New(Options{RepositoryRoot: root, Program: "yard", Arguments: arguments, Environment: environment, WorkingDir: root, Incus: incus, ProjectData: probe, Prompt: prompt, Stderr: &stderr})
				infrastructureFixture(t, program, incus, "test-vms")
				code := program.Run(context.Background())
				if (accept && code != 0) || (!accept && code != 1) {
					t.Fatalf("exit=%d: %s", code, stderr.String())
				}
				if len(prompt.Requests) != 1 || prompt.Requests[0].Default != domain.ConfirmationDefaultYes {
					t.Fatalf("refresh confirmation=%+v", prompt.Requests)
				}
				if len(probe.requests) != map[bool]int{false: 1, true: 2}[accept] {
					t.Fatal("refresh did not capture and recheck its read-only pool snapshot")
				}
				for _, request := range probe.requests {
					if request.Command[len(request.Command)-1] != "status" {
						t.Fatal("refresh acquired or mutated a lease")
					}
				}
				actual, err := os.ReadFile(filepath.Join(root, "refresh-arguments"))
				if !accept && !errors.Is(err, os.ErrNotExist) {
					t.Fatal("decline mutated broker")
				}
				if accept && (err != nil || string(actual) != expected) {
					t.Fatalf("refresh arguments: %q, error: %v", actual, err)
				}
				var calls []testkit.Call
				if accept {
					calls = append(calls, testkit.Call{Tool: "scripts/e2e-lab/invoke.sh", Args: expectedArgs})
				}
				cliFixture.Exec().AssertCalls(calls...)
			})
		}
	}

}

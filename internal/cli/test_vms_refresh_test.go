package cli

import (
	"context"
	"slices"
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
			runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{Schema: 1, OperationID: "refresh-base", Status: "ok"}}}}
			probe := &testVMStatusProbe{}
			program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"test-vms", "refresh", "android-test"}, Environment: environment, WorkingDir: root, Incus: incus, ProjectData: probe, AdapterRunner: runner, Prompt: prompt})
			if err != nil {
				t.Fatal(err)
			}
			code := program.Run(context.Background())
			if (accept && code != 0) || (!accept && code != 1) {
				t.Fatalf("exit=%d", code)
			}
			if len(prompt.Requests) != 1 || prompt.Requests[0].Default != domain.ConfirmationDefaultYes {
				t.Fatalf("refresh confirmation=%+v", prompt.Requests)
			}
			if len(probe.requests) != 0 {
				t.Fatal("refresh probed or acquired a lease slot")
			}
			if !accept && len(runner.Requests) != 0 {
				t.Fatal("decline mutated broker")
			}
			if accept && (len(runner.Requests) != 1 || !slices.Equal(runner.Requests[0].Arguments, []string{"refresh-android-test", "--yes"})) {
				t.Fatalf("unexpected refresh transport: %+v", runner.Requests)
			}
		})
	}
}

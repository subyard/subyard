package reconcileruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

// ownershipReleaseExecutor reports one retired structured document whose owned
// fields changed after apply, as the guest writer reports it.
type ownershipReleaseExecutor struct {
	*retryableIntegrationExecutor
	path     string
	released bool
	retires  int
}

func (fixture *ownershipReleaseExecutor) Exec(ctx context.Context, project, instance string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	command := request.Command
	if len(command) >= 5 && command[0] == "python3" && strings.Contains(command[3], "Version 1 records observed ownership only") && command[4] == "observe" {
		payload, _ := json.Marshal(integrationObservation{Fingerprint: strings.Repeat("a", 64), Changed: fixture.pending,
			Retired: []integrationArtifact{{ID: "sample", Kind: "structured", Format: "json", Path: fixture.path, Digest: strings.Repeat("b", 64)}}})
		return ports.InstanceExecResult{Stdout: payload}, nil
	}
	if len(command) >= 5 && command[0] == "python3" && strings.Contains(command[3], "def retire(") {
		fixture.requests = append(fixture.requests, request)
		observation := map[string]any{"converged": fixture.released, "fingerprint": strings.Repeat("c", 64)}
		switch command[4] {
		case "assess-retire":
			if !fixture.released {
				observation["released"] = []string{"/edited/mode", "/kept"}
			}
		case "retire":
			fixture.retires++
			if !fixture.released {
				observation["released"] = []string{"/edited/mode", "/kept"}
			}
			observation["converged"], fixture.released = true, true
		default:
			return ports.InstanceExecResult{ExitCode: 2}, nil
		}
		payload, _ := json.Marshal(observation)
		return ports.InstanceExecResult{Stdout: payload}, nil
	}
	return fixture.retryableIntegrationExecutor.Exec(ctx, project, instance, request)
}

// Retiring a document whose owned fields changed is previewed as a warning,
// completes instead of refusing, and reports the completed release exactly once.
func TestRetirementReleasesChangedOwnedFieldsWithNotice(t *testing.T) {
	root := testkit.TempDir(t)
	for _, directory := range []string{"config", "scripts"} {
		if err := os.Mkdir(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	testkit.WriteFile(t, filepath.Join(root, "config/projects-changed.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o644)
	testkit.WriteFile(t, filepath.Join(root, "scripts/reconcile-integrations.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	path := "/home/dev/.sample/settings.json"
	executor := &ownershipReleaseExecutor{retryableIntegrationExecutor: &retryableIntegrationExecutor{}, path: path}
	var output bytes.Buffer
	runtime := Runtime{RepositoryRoot: root, Executor: executor, Stdout: &output,
		Incus: &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{Status: "Running"}}},
		Yard:  domain.Context{YardName: "example", IncusProject: "test", YardInstanceName: "yard", DevUser: "dev", DevUID: os.Getuid()}}
	plan, err := runtime.IntegrationPlan(context.Background())
	if err != nil {
		t.Fatalf("changed owned fields blocked the plan: %v", err)
	}
	notice := "owned fields in " + path + " changed after Subyard applied them; retiring sample leaves them in place and releases their ownership: /edited/mode, /kept"
	if !plan.Changed || len(plan.Warnings) != 1 || plan.Warnings[0] != notice || len(plan.ownershipReleases) != 1 {
		t.Fatalf("release preview: changed=%t warnings=%q releases=%v", plan.Changed, plan.Warnings, plan.ownershipReleases)
	}
	if output.Len() != 0 {
		t.Fatalf("assessment reported a release: %q", output.String())
	}
	if err := runtime.ApplyIntegrations(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	result := "  [ ok ] yard example: released ownership of changed fields in " + path + "; left in place and no longer managed by Subyard: /edited/mode, /kept\n"
	if executor.retires != 1 || strings.Count(output.String(), "released ownership") != 1 || !strings.Contains(output.String(), result) {
		t.Fatalf("release result: retires=%d output=%q", executor.retires, output.String())
	}
	output.Reset()
	after, err := runtime.IntegrationPlan(context.Background())
	if err != nil || len(after.Warnings) != 0 || len(after.ownershipReleases) != 0 || output.Len() != 0 {
		t.Fatalf("completed release is still reported: %v %q %q", err, after.Warnings, output.String())
	}
}

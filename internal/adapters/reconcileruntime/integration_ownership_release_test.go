package reconcileruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Subyard/Subyard/internal/adapters/configmaterial"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

// ownershipReleaseExecutor reports one retired structured document whose owned
// fields changed after apply, as the guest writer reports it.
type ownershipReleaseExecutor struct {
	*retryableIntegrationExecutor
	path     string
	pointers []string
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
		pending := !fixture.released
		switch command[4] {
		case "assess-retire":
		case "retire":
			fixture.retires++
			observation["converged"], fixture.released = true, true
		default:
			return ports.InstanceExecResult{ExitCode: 2}, nil
		}
		if pending {
			observation["release"] = "changed"
			if len(fixture.pointers) != 0 {
				observation["released"] = fixture.pointers
			}
		}
		payload, _ := json.Marshal(observation)
		return ports.InstanceExecResult{Stdout: payload}, nil
	}
	return fixture.retryableIntegrationExecutor.Exec(ctx, project, instance, request)
}

func ownershipReleaseRuntime(t *testing.T, executor *ownershipReleaseExecutor, output *bytes.Buffer) Runtime {
	t.Helper()
	root := testkit.TempDir(t)
	for _, directory := range []string{"config", "scripts"} {
		if err := os.Mkdir(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	testkit.WriteFile(t, filepath.Join(root, "config/projects-changed.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o644)
	testkit.WriteFile(t, filepath.Join(root, "scripts/reconcile-integrations.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	return Runtime{RepositoryRoot: root, Executor: executor, Stdout: output,
		Incus: &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{Status: "Running"}}},
		Yard:  domain.Context{YardName: "example", IncusProject: "test", YardInstanceName: "yard", DevUser: "dev", DevUID: os.Getuid()}}
}

// Retiring a document whose owned fields changed is previewed as a warning,
// completes instead of refusing, and reports the completed release exactly once.
// The release stays detectable when no owned path remains to name.
func TestRetirementReleasesChangedOwnedFieldsWithNotice(t *testing.T) {
	path := "/home/dev/.sample/settings.json"
	for _, scenario := range []struct {
		pointers []string
		fields   string
	}{
		{[]string{"/edited/mode", "/kept"}, "fields left in place: /edited/mode, /kept"},
		{nil, "no owned fields remain"},
	} {
		t.Run(scenario.fields, func(t *testing.T) {
			executor := &ownershipReleaseExecutor{retryableIntegrationExecutor: &retryableIntegrationExecutor{}, path: path, pointers: scenario.pointers}
			var output bytes.Buffer
			runtime := ownershipReleaseRuntime(t, executor, &output)
			plan, err := runtime.IntegrationPlan(context.Background())
			if err != nil {
				t.Fatalf("changed owned fields blocked the plan: %v", err)
			}
			notice := "retiring sample releases ownership of " + path + ": owned fields changed after Subyard applied them; " + scenario.fields
			if !plan.Changed || len(plan.Warnings) != 1 || plan.Warnings[0] != notice || !slices.Equal(plan.OwnershipReleases(), []string{path}) {
				t.Fatalf("release preview: changed=%t warnings=%q releases=%v", plan.Changed, plan.Warnings, plan.OwnershipReleases())
			}
			if output.Len() != 0 {
				t.Fatalf("assessment reported a release: %q", output.String())
			}
			if err := runtime.ApplyIntegrations(context.Background(), plan); err != nil {
				t.Fatal(err)
			}
			result := "  [ ok ] yard example: released ownership of " + path + ": owned fields changed after Subyard applied them; " + scenario.fields + "; Subyard no longer manages it\n"
			if executor.retires != 1 || strings.Count(output.String(), "released ownership") != 1 || !strings.Contains(output.String(), result) {
				t.Fatalf("release result: retires=%d output=%q", executor.retires, output.String())
			}
			output.Reset()
			after, err := runtime.IntegrationPlan(context.Background())
			if err != nil || len(after.Warnings) != 0 || len(after.OwnershipReleases()) != 0 || output.Len() != 0 {
				t.Fatalf("completed release is still reported: %v %q %q", err, after.Warnings, output.String())
			}
		})
	}
}

// Activation warnings reject text above the release diagnostic bound; a long
// field list is shortened and counts what it omitted.
func TestOwnershipReleaseNoticeStaysWithinDiagnosticBound(t *testing.T) {
	pointers := make([]string, 256)
	for index := range pointers {
		pointers[index] = fmt.Sprintf("/%03d/%s", index, strings.Repeat("p", 500))
	}
	pointers[0] = "/000"
	slices.Sort(pointers)
	observation := configmaterial.Observation{Release: configmaterial.ReleaseChanged, Released: pointers}
	for _, notice := range []string{
		ownershipReleaseNotice("sample", "/home/dev/.sample/settings.json", observation),
		boundedNotice("  [ ok ] yard "+strings.Repeat("y", 63)+": released ownership of /home/dev/.sample/settings.json: ", observation, "; Subyard no longer manages it"),
		ownershipReleaseNotice("sample", "/home/dev/"+strings.Repeat("d", 600), observation),
	} {
		if len(notice) > ownershipNoticeLimit || !utf8.ValidString(notice) {
			t.Fatalf("notice exceeds the bound: %d %q", len(notice), notice)
		}
	}
	short := ownershipReleaseNotice("sample", "/home/dev/.sample/settings.json", observation)
	if !strings.Contains(short, "fields left in place: /000 and 255 more") {
		t.Fatalf("omitted pointers are not counted: %q", short)
	}
}

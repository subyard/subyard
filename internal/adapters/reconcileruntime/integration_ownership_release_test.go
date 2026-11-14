package reconcileruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/Subyard/Subyard/internal/shellquote"
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
		observation := integrationObservation{Fingerprint: strings.Repeat("a", 64), Changed: fixture.pending}
		if fixture.commits == 0 {
			// The inventory forgets a retired record only at its commit.
			observation.Retired = []integrationArtifact{{ID: "sample", Kind: "structured", Format: "json", Path: fixture.path, Digest: strings.Repeat("b", 64)}}
		}
		payload, _ := json.Marshal(observation)
		return ports.InstanceExecResult{Stdout: payload}, nil
	}
	if len(command) >= 5 && command[0] == "python3" && strings.Contains(command[3], "def retire(") {
		fixture.requests = append(fixture.requests, request)
		observation := map[string]any{"converged": fixture.released, "fingerprint": strings.Repeat("c", 64)}
		// The guest keeps reporting a completed release until fresh ownership.
		switch command[4] {
		case "assess-retire":
		case "retire":
			fixture.retires++
			observation["converged"], fixture.released = true, true
		default:
			return ports.InstanceExecResult{ExitCode: 2}, nil
		}
		{
			observation["release"] = "changed"
			if len(fixture.pointers) != 0 {
				observation["released"], observation["released_count"] = fixture.pointers, len(fixture.pointers)
			}
		}
		payload, _ := json.Marshal(observation)
		return ports.InstanceExecResult{Stdout: payload}, nil
	}
	return fixture.retryableIntegrationExecutor.Exec(ctx, project, instance, request)
}

func ownershipReleaseRuntime(t *testing.T, executor ports.InstanceExecutor, output *bytes.Buffer) Runtime {
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

// Between the guest release and the inventory commit, the completed release
// still marks the plan, so the installed-view repair guard keeps blocking.
func TestCompletedReleaseStillMarksThePlanBeforeInventoryCommit(t *testing.T) {
	path := "/home/dev/.sample/settings.json"
	executor := &ownershipReleaseExecutor{retryableIntegrationExecutor: &retryableIntegrationExecutor{}, path: path, pointers: []string{"/kept"}, released: true}
	var output bytes.Buffer
	plan, err := ownershipReleaseRuntime(t, executor, &output).IntegrationPlan(context.Background())
	if err != nil || !slices.Equal(plan.OwnershipReleases(), []string{path}) || len(plan.Warnings) != 1 {
		t.Fatalf("completed release lost its plan signal: %v releases=%v warnings=%q", err, plan.OwnershipReleases(), plan.Warnings)
	}
}

// Activation warnings reject text above the release diagnostic bound: the
// notice lists every retained field when they fit, and otherwise counts them.
func TestOwnershipReleaseNoticeStaysWithinDiagnosticBound(t *testing.T) {
	long := make([]string, 40)
	for index := range long {
		long[index] = fmt.Sprintf("/field-%02d", index)
	}
	for _, scenario := range []struct {
		observation configmaterial.Observation
		fields      string
	}{
		{configmaterial.Observation{Release: configmaterial.ReleaseChanged, Released: []string{"/a", "/b"}, ReleasedCount: 2}, "fields left in place: /a, /b"},
		{configmaterial.Observation{Release: configmaterial.ReleaseChanged, Released: long, ReleasedCount: len(long)}, "40 fields left in place"},
		{configmaterial.Observation{Release: configmaterial.ReleaseChanged, ReleasedCount: 300}, "300 fields left in place"},
		{configmaterial.Observation{Release: configmaterial.ReleaseChanged}, "no owned fields remain"},
		{configmaterial.Observation{Release: configmaterial.ReleaseMissingDocument}, "nothing to remove"},
	} {
		for _, notice := range []string{
			ownershipReleaseNotice("sample", "/home/dev/.sample/settings.json", scenario.observation),
			boundedNotice("  [ ok ] yard "+strings.Repeat("y", 63)+": released ownership of /home/dev/.sample/settings.json: ", scenario.observation, "; Subyard no longer manages it"),
			ownershipReleaseNotice("sample", "/home/dev/"+strings.Repeat("d", 600), scenario.observation),
		} {
			if len(notice) > ownershipNoticeLimit || !utf8.ValidString(notice) || !strings.Contains(notice, "; "+scenario.fields) {
				t.Fatalf("notice=%q (%d bytes), want fields %q", notice, len(notice), scenario.fields)
			}
		}
	}
}

// The init plan previews a pending ownership release before consent.
func TestRetiredIntegrationNoticesPreviewOwnershipRelease(t *testing.T) {
	path := "/home/dev/.sample/settings.json"
	executor := &ownershipReleaseExecutor{retryableIntegrationExecutor: &retryableIntegrationExecutor{}, path: path, pointers: []string{"/kept"}}
	var output bytes.Buffer
	runtime := ownershipReleaseRuntime(t, executor, &output)
	retirements, warnings, err := runtime.RetiredIntegrationNotices(context.Background())
	notice := "retiring sample releases ownership of " + path + ": owned fields changed after Subyard applied them; fields left in place: /kept"
	if err != nil || len(retirements) != 0 || !slices.Equal(warnings, []string{notice}) {
		t.Fatalf("init preview: retirements=%q warnings=%q err=%v", retirements, warnings, err)
	}
	if executor.retires != 0 || output.Len() != 0 {
		t.Fatalf("preview performed or reported the release: retires=%d output=%q", executor.retires, output.String())
	}
}

// releasedInstalledExecutor reports a selected document whose ownership was
// released, as after an init interrupted between its inventory commit and the
// store of its applied settings.
type releasedInstalledExecutor struct {
	*retryableIntegrationExecutor
	writes int
}

func (fixture *releasedInstalledExecutor) Exec(ctx context.Context, project, instance string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	command := request.Command
	if len(command) >= 5 && command[0] == "python3" && strings.Contains(command[3], "Version 1 records observed ownership only") &&
		(command[4] == "observe" || command[4] == "observe-applied") {
		payload, _ := json.Marshal(integrationObservation{Fingerprint: strings.Repeat("a", 64)})
		return ports.InstanceExecResult{Stdout: payload}, nil
	}
	if len(command) >= 5 && command[0] == "python3" && strings.Contains(command[3], "def retire(") {
		if command[4] != "observe" {
			fixture.writes++
			return ports.InstanceExecResult{ExitCode: 2}, &ports.GuestExitError{Code: 2}
		}
		payload, _ := json.Marshal(map[string]any{"converged": false, "fingerprint": strings.Repeat("c", 64), "release": "changed"})
		return ports.InstanceExecResult{Stdout: payload}, nil
	}
	return fixture.retryableIntegrationExecutor.Exec(ctx, project, instance, request)
}

// A release repair of the installed view must not overlay a document whose
// ownership was released: it refuses with a pending-configuration finding that
// names plain init. The desired view, a deliberate enable, may take ownership.
func TestInstalledViewRefusesReleasedDocument(t *testing.T) {
	executor := &releasedInstalledExecutor{retryableIntegrationExecutor: &retryableIntegrationExecutor{}}
	var output bytes.Buffer
	runtime := ownershipReleaseRuntime(t, executor, &output)
	asset := filepath.Join(runtime.RepositoryRoot, "settings.json")
	testkit.WriteFile(t, asset, []byte(`{"managed":{"mode":"subyard"}}`), 0o600)
	runtime.Environment = []string{"CODING_TOOL_INTEGRATIONS=sample", "AGENT_sample_CONFIG=" + asset, "AGENT_sample_CONFIG_DEST=.sample/settings.json"}
	if _, err := runtime.IntegrationPlan(context.Background()); err != nil {
		t.Fatalf("desired view refused a deliberate enable: %v", err)
	}
	installed := runtime.WithInstalledConfiguration()
	_, err := installed.IntegrationPlan(context.Background())
	var finding integrationOwnershipError
	if !errors.As(err, &finding) || !finding.PendingIntegrationConfiguration() {
		t.Fatalf("installed view did not refuse the released document: %v", err)
	}
	message, retry := finding.ActivationDiagnostic()
	if retry != shellquote.Command([]string{"yard", "-Y", "example", "init"}) || !strings.Contains(message, "/home/dev/.sample/settings.json") || len(message) > 512 {
		t.Fatalf("diagnostic=%q retry=%q", message, retry)
	}
	if err := installed.ApplyIntegrations(context.Background(), IntegrationPlan{}); err == nil || executor.writes != 0 {
		t.Fatalf("installed-view apply reached the released document: %v writes=%d", err, executor.writes)
	}
}

// The installed view requests the guest's refusing apply mode; the desired view,
// a deliberate enable, requests ordinary apply.
func TestInstalledViewAppliesWithRefusingMode(t *testing.T) {
	executor := &retryableIntegrationExecutor{}
	runtime := Runtime{Executor: executor, Yard: domain.Context{YardName: "example", IncusProject: "test", YardInstanceName: "yard", DevUser: "dev", DevUID: os.Getuid()}}
	file := guestConfigFile{destination: "/home/dev/.sample/settings.json", ownedFormat: "json"}
	for _, scenario := range []struct {
		runtime Runtime
		mode    string
	}{{runtime, configmaterial.ModeApply}, {runtime.WithInstalledConfiguration(), configmaterial.ModeApplyInstalled}} {
		executor.requests = nil
		if err := scenario.runtime.applyGuestConfig(context.Background(), file, []byte(`{"managed":1}`)); err != nil {
			t.Fatal(err)
		}
		if len(executor.requests) != 1 || executor.requests[0].Command[4] != scenario.mode {
			t.Fatalf("apply mode: %v, want %s", executor.requests, scenario.mode)
		}
	}
}

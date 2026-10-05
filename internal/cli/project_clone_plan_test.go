package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func emptyCloneGit(t *testing.T, arguments ...string) string {
	t.Helper()
	output, err := exec.Command("git", arguments...).CombinedOutput()
	if err != nil {
		t.Fatalf("native clone source fixture: %v\n%s", err, output)
	}
	return strings.TrimSpace(string(output))
}

func emptyCloneAddRef(t *testing.T, source, ref string) {
	t.Helper()
	tree := emptyCloneGit(t, "-C", source, "mktree")
	commit := emptyCloneGit(t, "-C", source, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit-tree", tree, "-m", "fixture")
	emptyCloneGit(t, "-C", source, "update-ref", ref, commit)
}

type emptyClonePlanData struct {
	yard          domain.Context
	requests      []ports.InstanceExecRequest
	workspaceRoot string
	beforeClone   func()
}

func (data *emptyClonePlanData) Execute(ctx context.Context, yard domain.Context, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	data.requests = append(data.requests, request)
	if data.workspaceRoot != "" {
		if request.Command[0] == "git" && len(request.Command) > 1 && request.Command[1] == "clone" && data.beforeClone != nil {
			data.beforeClone()
		}
		// This native guest-port fixture executes the approved workspace commands
		// against a private local root, while retaining canonical owner metadata.
		request.Command = slices.Clone(request.Command)
		if request.Command[0] == "install" {
			uid := strconv.Itoa(yard.DevUID)
			if len(request.Command) != 8 || !slices.Equal(request.Command[:7], []string{"install", "-d", "-o", uid, "-g", uid, "--"}) {
				return ports.InstanceExecResult{}, errors.New("unexpected clone workspace ownership request")
			}
			// Map the validated guest ownership onto this unprivileged host user.
			request.Command[3], request.Command[5] = strconv.Itoa(os.Geteuid()), strconv.Itoa(os.Getegid())
		}
		for index := range request.Command {
			if strings.Contains(request.Command[index], "/usr/local/libexec/subyard/projects-changed") {
				return ports.InstanceExecResult{}, nil
			}
			request.Command[index] = strings.ReplaceAll(request.Command[index], "/srv/workspaces", data.workspaceRoot)
		}
		return (exportRouteNativeData{yard: data.yard}).Execute(ctx, yard, request)
	}
	if request.Command[0] == "git" {
		return (exportRouteNativeData{yard: data.yard}).Execute(ctx, yard, request)
	}
	// No guest workspaces exist in this source/admission contract fixture.
	return ports.InstanceExecResult{}, nil
}

func TestProjectEmptyCloneNativeExecutionCommitsOnlyVerifiedCopy(t *testing.T) {
	for _, change := range []string{"none", "ref-during"} {
		t.Run(change, func(t *testing.T) {
			program, execution, source, data := emptyClonePlanFixture(t)
			data.workspaceRoot = testkit.TempDir(t)
			definition, _ := program.manifest.Lookup("clone")
			prepared := &preparedCommand{CLI: program, Definition: definition, Loaded: execution.Loaded, Project: execution}
			prepared.policy = commandPolicy(definition, execution.Loaded.Context, nil, execution)
			if err := prepared.prepareProject(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			if err := prepared.preparePlan(context.Background()); err != nil {
				t.Fatal(err)
			}
			orchestrator := program.operationOrchestrator(prepared.Plan.OperationID, prepared.Loaded, nil, nil)
			confirmed, err := orchestrator.Confirm(context.Background(), prepared.Plan, true)
			if err != nil {
				t.Fatal(err)
			}
			prepared.Plan = confirmed
			if change == "ref-during" {
				data.beforeClone = func() { emptyCloneAddRef(t, source, "refs/heads/main") }
			}
			result, runErr := prepared.Execute(context.Background(), orchestrator, io.Discard)
			metadataPath := filepath.Join(data.workspaceRoot, execution.Record.ProjectID, ".subyard-meta.json")
			if change == "none" {
				if runErr != nil || result.Status != "ok" {
					t.Fatalf("native empty clone failed: %v", runErr)
				}
				if _, err := execution.Store.GetReadOnly(context.Background(), execution.Record.ProjectID); err != nil {
					t.Fatalf("verified empty clone not committed: %v", err)
				}
				if _, err := os.Stat(metadataPath); err != nil {
					t.Fatal(err)
				}
			} else {
				if !errors.Is(runErr, domain.ErrPlanStale) {
					t.Fatalf("source drift during native clone was accepted: %v", runErr)
				}
				if _, err := execution.Store.GetReadOnly(context.Background(), execution.Record.ProjectID); err == nil {
					t.Fatal("unverified clone finalized owner registry")
				}
				if _, err := os.Stat(metadataPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("unverified clone published metadata")
				}
			}
			if execution.Reservation != nil {
				t.Fatal("clone retained admission after execution")
			}
		})
	}
}

func (data *emptyClonePlanData) Stream(context.Context, domain.Context, ports.InstanceExecRequest, io.Reader) (ports.InstanceExecResult, error) {
	panic("empty clone source check must not stream")
}

func emptyClonePlanFixture(t *testing.T) (*CLI, *projectExecution, string, *emptyClonePlanData) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git is required")
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	root, environment, _ := nativeFixture(t)
	// Keep the synthetic guest owner distinct from the unprivileged test user.
	environment = append(environment, "DEV_UID="+strconv.Itoa(os.Geteuid()+1))
	source := filepath.Join(testkit.TempDir(t), "Empty.git")
	emptyCloneGit(t, "init", "--bare", "--initial-branch=main", source)
	data := &emptyClonePlanData{}
	program, err := New(Options{RepositoryRoot: root, Environment: environment, WorkingDir: root, ProjectData: data})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	data.yard = loaded.Context
	execution, err := program.prepareProjectClone(context.Background(), loaded, []string{source})
	if err != nil {
		t.Fatal(err)
	}
	return program, execution, source, data
}

func TestProjectEmptyCloneRetainsStateAndRefusesNewRefsBeforeAdmission(t *testing.T) {
	program, execution, source, data := emptyClonePlanFixture(t)
	definition, ok := program.manifest.Lookup("clone")
	if !ok {
		t.Fatal("clone definition missing")
	}
	prepared := &preparedCommand{CLI: program, Definition: definition, Loaded: execution.Loaded, Project: execution}
	prepared.policy = commandPolicy(definition, execution.Loaded.Context, nil, execution)
	if err := prepared.prepareProject(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if err := prepared.preparePlan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !execution.cloneUnborn || execution.cloneRevision != "" || !strings.Contains(prepared.Plan.Steps[0].Desired, "unborn HEAD") {
		t.Fatal("empty source did not become an explicit retained unborn plan")
	}
	binding := execution.sourceBinding()
	execution.cloneUnborn = false
	if execution.sourceBinding() == binding {
		t.Fatal("empty clone state was not bound into the retained plan")
	}
	execution.cloneUnborn = true
	before := nativeTreeSnapshot(t, execution.Loaded.Context.Paths.StateDir)
	emptyCloneAddRef(t, source, "refs/heads/main")
	if err := execution.prepareSource(context.Background(), program); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("cached empty plan accepted newly populated source: %v", err)
	}
	if !execution.cloneUnborn || execution.cloneRevision != "" || execution.sourceBinding() != binding {
		t.Fatal("cached preparation widened the approved empty source")
	}
	prepared.Plan.Confirmed = true
	data.requests = nil
	if _, err := prepared.Execute(context.Background(), &application.Orchestrator{}, nil); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("stale empty source reached application: %v", err)
	}
	if execution.Reservation != nil || !slices.Equal(before, nativeTreeSnapshot(t, execution.Loaded.Context.Paths.StateDir)) {
		t.Fatal("source drift changed owner admission/registry before rejection")
	}
	if len(data.requests) != 1 || !slices.Contains(data.requests[0].Command, "ls-remote") {
		t.Fatal("stale empty source reached workspace operations")
	}
	if err := program.reserveProjectExecution(context.Background(), execution); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("native admission guard accepted populated source: %v", err)
	}
	if !slices.Equal(before, nativeTreeSnapshot(t, execution.Loaded.Context.Paths.StateDir)) {
		t.Fatal("native admission guard wrote stale reservation")
	}
}

func TestProjectCloneMissingHEADWithOtherRefsIsNotEmpty(t *testing.T) {
	program, execution, source, _ := emptyClonePlanFixture(t)
	emptyCloneAddRef(t, source, "refs/heads/other")
	if err := execution.prepareSource(context.Background(), program); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("nonempty source without HEAD accepted as empty: %v", err)
	}
	if execution.cloneUnborn || execution.cloneRevision != "" || execution.Reservation != nil {
		t.Fatal("failed empty source probe retained clone authority")
	}
}

func TestProjectCloneMalformedHEADDoesNotBecomeEmpty(t *testing.T) {
	for _, output := range []string{"\n", "invalid HEAD\n", strings.Repeat("a", 40) + " refs/heads/main\n"} {
		program, execution, _, _ := emptyClonePlanFixture(t)
		program.options.ProjectData = projectActionObservationProbe{execute: func(ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
			return ports.InstanceExecResult{Stdout: []byte(output)}, nil
		}}
		if err := execution.prepareSource(context.Background(), program); err == nil || execution.cloneUnborn {
			t.Fatal("malformed HEAD probe became an empty clone plan")
		}
	}
}

package reconcileruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

// yardActivityFixture runs the real hook observer, metadata publisher and installed
// dispatcher against a private guest tree that it reports under canonical guest paths.
type yardActivityFixture struct {
	t                *testing.T
	options          yardActivityOptions
	root, workspaces string
	hook, effects    string
	record           domain.ProjectRecord
	payload          []byte
	toFixture        *strings.Replacer
	toGuest          *strings.Replacer
	beforeDispatch   func(attempt int)
	dispatches       int
}

type yardActivityOptions struct {
	metadata, hook bool
	// stopped plans conditionally for an intentionally stopped yard that init starts.
	stopped bool
}

func newYardActivityFixture(t *testing.T, options yardActivityOptions) *yardActivityFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git is required")
	}
	testkit.UseHostTempDir(t)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	root := testkit.TempDir(t)
	fixture := &yardActivityFixture{t: t, options: options, root: root, workspaces: filepath.Join(root, "workspaces"), effects: filepath.Join(root, "effects")}
	libexec, etc := filepath.Join(root, "libexec"), filepath.Join(root, "etc")
	fixture.toFixture = strings.NewReplacer("/srv/workspaces", fixture.workspaces, "/usr/local/libexec/subyard", libexec, "/etc/subyard", etc)
	fixture.toGuest = strings.NewReplacer(fixture.workspaces, "/srv/workspaces", libexec, "/usr/local/libexec/subyard", etc, "/etc/subyard")
	source := fixture.path("sample", "src")
	for _, path := range []string{source, filepath.Join(libexec, "projects-changed.d"), etc} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	dispatcher, err := os.ReadFile("../../../config/projects-changed.sh")
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(libexec, "projects-changed"), []byte(fixture.toFixture.Replace(string(dispatcher))), 0o700)
	testkit.WriteFile(t, filepath.Join(etc, "agent-project-hooks"), nil, 0o600)
	if options.hook {
		fixture.hook = filepath.Join(libexec, "projects-changed.d", "owned")
		fixture.writeHook("")
	}
	testkit.WriteFile(t, filepath.Join(source, "main.go"), []byte("package main\n"), 0o600)
	fixture.git("init", "-q", "--initial-branch=main")
	fixture.git("add", "-A")
	fixture.git("-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "initial")
	fixture.record = domain.ProjectRecord{Schema: 1, IdentityVersion: 2, ProjectID: "sample", Name: "sample", YardPath: "/srv/workspaces/sample/src", Mode: domain.ProjectGit, Target: "yard", SSHHost: "yard"}
	fixture.payload, err = application.ProjectMetadata(fixture.record, "selected")
	if err != nil {
		t.Fatal(err)
	}
	if options.metadata {
		testkit.WriteFile(t, fixture.path("sample", ".subyard-meta.json"), fixture.payload, 0o600)
	}
	return fixture
}

func (fixture *yardActivityFixture) path(parts ...string) string {
	return filepath.Join(append([]string{fixture.workspaces}, parts...)...)
}

func (fixture *yardActivityFixture) writeHook(extra string) {
	body := "#!/bin/sh\n[ -e '" + fixture.root + "/fail' ] && exit 1\necho invoked >> '" + fixture.effects + "'\n" + extra
	testkit.WriteFile(fixture.t, fixture.hook, []byte(body), 0o700)
}

func (fixture *yardActivityFixture) git(arguments ...string) {
	fixture.t.Helper()
	command := exec.Command("git", append([]string{"-C", fixture.path("sample", "src")}, arguments...)...)
	if output, err := command.CombinedOutput(); err != nil {
		fixture.t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
}

func (fixture *yardActivityFixture) invocations() int {
	data, err := os.ReadFile(fixture.effects)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		fixture.t.Fatal(err)
	}
	return strings.Count(string(data), "invoked\n")
}

func (fixture *yardActivityFixture) Exec(ctx context.Context, project, instance string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	switch {
	case request.Command[0] == "python3":
		return metadataFileExecutor{fixture.workspaces}.Exec(ctx, project, instance, request)
	case request.Command[0] == "bash":
		command := exec.CommandContext(ctx, "bash", "-s", "--", "--observe")
		command.Stdin = strings.NewReader(fixture.toFixture.Replace(string(request.Stdin)))
		output, err := command.Output()
		return ports.InstanceExecResult{Stdout: []byte(fixture.toGuest.Replace(string(output)))}, err
	case request.Command[1] == "-eu":
		// Installed dispatcher convergence needs root-owned guest files.
		return ports.InstanceExecResult{}, nil
	}
	fixture.dispatches++
	if fixture.beforeDispatch != nil {
		fixture.beforeDispatch(fixture.dispatches)
	}
	arguments := []string{}
	for _, argument := range request.Command[1:] {
		arguments = append(arguments, fixture.toFixture.Replace(argument))
	}
	command := exec.CommandContext(ctx, request.Command[0], arguments...)
	command.Env = os.Environ()
	for name, value := range request.Environment {
		if name != "HOME" {
			command.Env = append(command.Env, name+"="+fixture.toFixture.Replace(value))
		}
	}
	output, err := command.Output()
	result := ports.InstanceExecResult{Stdout: []byte(fixture.toGuest.Replace(string(output)))}
	if exit := new(exec.ExitError); errors.As(err, &exit) {
		result.ExitCode, err = exit.ExitCode(), nil
	}
	return result, err
}

func (fixture *yardActivityFixture) prepare() Runtime {
	fixture.t.Helper()
	ctx := context.Background()
	instance := ports.InstanceInfo{Status: "running"}
	if fixture.options.stopped {
		instance = ports.InstanceInfo{Status: "stopped", Config: map[string]string{"user.subyard.managed": "true", "user.subyard.initialized": "true", "user.subyard.desired_power": "stopped"}}
	}
	runtime := Runtime{RepositoryRoot: "../../..", Profiles: []profile.Definition{}, Integrations: []profile.IntegrationDefinition{}, Executor: fixture,
		Incus: &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true, Instance: instance}},
		Yard:  domain.Context{IncusProject: "subyard", YardInstanceName: "yard", YardName: "selected", DevUser: "dev", DevUID: os.Getuid()}}
	var err error
	if runtime.HookPlan, err = runtime.PrepareProjectHooks(ctx, false, fixture.options.stopped); err != nil {
		fixture.t.Fatal(err)
	}
	runtime.HookPlan.CaptureOwnerProjects([]domain.ProjectRecord{fixture.record})
	if err := runtime.PrepareProjectMetadata(ctx); err != nil {
		fixture.t.Fatal(err)
	}
	return runtime
}

// start runs the approved temporary start of a stopped yard; its desired power stays stopped.
func (fixture *yardActivityFixture) start(runtime Runtime) {
	if fixture.options.stopped {
		runtime.Incus.(*testkit.Incus).Reconcile.Instance.Status = "running"
	}
}

func projectHookSteps(plan *ProjectHookPlan) []domain.OperationStep {
	return []domain.OperationStep{plan.OperationStep("subyard/yard"), *plan.MetadataOperationStep("subyard/yard")}
}

// Ordinary agent work in project trees and unregistered siblings changes neither the
// approved hook guard nor its step decisions, with or without a metadata repair.
func TestProjectHookGuardIgnoresOrdinaryYardActivity(t *testing.T) {
	for _, variant := range []struct {
		name    string
		options yardActivityOptions
	}{
		{"ready metadata with hook", yardActivityOptions{metadata: true, hook: true}},
		{"ready metadata without hooks", yardActivityOptions{metadata: true}},
		{"metadata repair with hook", yardActivityOptions{hook: true}},
		// The conditional plan of a stopped yard first observes the guest after its start.
		{"stopped yard", yardActivityOptions{metadata: true, stopped: true}},
		{"stopped yard metadata repair", yardActivityOptions{stopped: true}},
	} {
		for _, activity := range []struct {
			name         string
			setup, agent func(*yardActivityFixture)
		}{
			{"commit", nil, func(f *yardActivityFixture) {
				testkit.WriteFile(f.t, f.path("sample", "src", "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o600)
				f.git("-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qam", "change")
			}},
			{"worktree add", nil, func(f *yardActivityFixture) { f.git("worktree", "add", "-q", "-b", "task", "../wt-task") }},
			{"worktree remove", func(f *yardActivityFixture) { f.git("worktree", "add", "-q", "-b", "old", "../wt-old") },
				func(f *yardActivityFixture) { f.git("worktree", "remove", "../wt-old") }},
			{"top-level file in source", nil, func(f *yardActivityFixture) {
				testkit.WriteFile(f.t, f.path("sample", "src", "build.log"), []byte("build"), 0o600)
			}},
			{"top-level file in worktree", func(f *yardActivityFixture) { f.git("worktree", "add", "-q", "-b", "old", "../wt-old") },
				func(f *yardActivityFixture) {
					testkit.WriteFile(f.t, f.path("sample", "wt-old", "build.log"), []byte("build"), 0o600)
				}},
			{"unregistered sibling directory", nil, func(f *yardActivityFixture) {
				if err := os.MkdirAll(f.path("scratch", "build"), 0o700); err != nil {
					f.t.Fatal(err)
				}
			}},
		} {
			t.Run(variant.name+"/"+activity.name, func(t *testing.T) {
				ctx := context.Background()
				fixture := newYardActivityFixture(t, variant.options)
				if activity.setup != nil {
					activity.setup(fixture)
				}
				runtime := fixture.prepare()
				approved := projectHookSteps(runtime.HookPlan)
				activity.agent(fixture)
				if err := runtime.CheckProjectHookPlan(ctx, true); err != nil {
					t.Fatalf("guard after consent: %v", err)
				}
				if steps := projectHookSteps(runtime.HookPlan); !reflect.DeepEqual(steps, approved) {
					t.Fatalf("checked steps changed:\napproved=%+v\ncurrent=%+v", approved, steps)
				}
				for index, step := range projectHookSteps(fixture.prepare().HookPlan) {
					if step.Decision != approved[index].Decision {
						t.Fatalf("fresh %s decision=%s, approved %s", step.ID, step.Decision, approved[index].Decision)
					}
				}
				fixture.start(runtime)
				if err := runtime.RunProjectHooks(ctx); err != nil {
					t.Fatalf("hooks after activity: %v", err)
				}
				want := 0
				if variant.options.hook {
					want = 1
				}
				if got := fixture.invocations(); got != want || fixture.dispatches != 1 {
					t.Fatalf("hook invocations=%d dispatches=%d, want %d/1", got, fixture.dispatches, want)
				}
				if got, err := os.ReadFile(fixture.path("sample", ".subyard-meta.json")); err != nil || !bytes.Equal(got, fixture.payload) {
					t.Fatalf("metadata after hooks: %q %v", got, err)
				}
			})
		}
	}
}

// Changes to what the approved hooks read refuse before any hook runs.
func TestProjectHookGuardRefusesChangedScope(t *testing.T) {
	replaceRoot := func(f *yardActivityFixture) {
		if err := os.Rename(f.path("sample"), f.path("sample-old")); err != nil {
			f.t.Fatal(err)
		}
		if err := os.MkdirAll(f.path("sample", "src"), 0o700); err != nil {
			f.t.Fatal(err)
		}
	}
	for _, variant := range []struct {
		name    string
		options yardActivityOptions
	}{
		{"ready metadata", yardActivityOptions{metadata: true, hook: true}},
		{"metadata repair", yardActivityOptions{hook: true}},
		{"stopped yard", yardActivityOptions{metadata: true, stopped: true}},
		{"stopped yard metadata repair", yardActivityOptions{stopped: true}},
	} {
		metadata := variant.options.metadata
		for _, change := range []struct {
			name   string
			mutate func(*yardActivityFixture)
		}{
			{"registered root removed", func(f *yardActivityFixture) {
				if err := os.Rename(f.path("sample"), f.path("sample-old")); err != nil {
					f.t.Fatal(err)
				}
			}},
			{"registered root replaced", func(f *yardActivityFixture) {
				replaceRoot(f)
				if metadata {
					testkit.WriteFile(f.t, f.path("sample", ".subyard-meta.json"), f.payload, 0o600)
				}
			}},
			{"hook source changed", func(f *yardActivityFixture) { f.writeHook("# changed\n") }},
			{"hook added", func(f *yardActivityFixture) {
				testkit.WriteFile(f.t, filepath.Join(f.root, "libexec", "projects-changed.d", "added"), []byte("#!/bin/sh\necho invoked >> '"+f.effects+"'\n"), 0o700)
			}},
			{"metadata changed", func(f *yardActivityFixture) {
				testkit.WriteFile(f.t, f.path("sample", ".subyard-meta.json"), append(bytes.TrimSpace(f.payload), ' ', '\n'), 0o600)
			}},
		} {
			// A stopped yard's plan has no guest observation to bind before its start,
			// so only removal of a registered root is a change against its approval.
			if variant.options.stopped && change.name != "registered root removed" {
				continue
			}
			t.Run(variant.name+"/"+change.name, func(t *testing.T) {
				ctx := context.Background()
				fixture := newYardActivityFixture(t, variant.options)
				runtime := fixture.prepare()
				if err := runtime.CheckProjectHookPlan(ctx, true); err != nil {
					t.Fatal(err)
				}
				change.mutate(fixture)
				fixture.start(runtime)
				err := runtime.RunProjectHooks(ctx)
				if !errors.Is(err, domain.ErrPlanStale) || fixture.dispatches != 0 || fixture.invocations() != 0 {
					t.Fatalf("changed scope reached hooks: dispatches=%d invocations=%d err=%v", fixture.dispatches, fixture.invocations(), err)
				}
				if !metadata && change.name != "metadata changed" {
					if _, err := os.Lstat(fixture.path("sample", ".subyard-meta.json")); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("refused repair published metadata: %v", err)
					}
				}
			})
		}
	}
}

// A registered root replaced after the last host check reaches only the dispatcher's own
// refusal; the next observation binds the published metadata and refuses before any hook.
func TestProjectHookGuardRefusesRootReplacedAtDispatch(t *testing.T) {
	for _, options := range []yardActivityOptions{{metadata: true, hook: true}, {hook: true}, {metadata: true, stopped: true}} {
		t.Run(fmt.Sprintf("metadata=%t stopped=%t", options.metadata, options.stopped), func(t *testing.T) {
			fixture := newYardActivityFixture(t, options)
			runtime := fixture.prepare()
			fixture.beforeDispatch = func(attempt int) {
				if attempt != 1 {
					return
				}
				if err := os.Rename(fixture.path("sample"), fixture.path("sample-old")); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(fixture.path("sample", "src"), 0o700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, fixture.path("sample", ".subyard-meta.json"), fixture.payload, 0o600)
			}
			fixture.start(runtime)
			err := runtime.RunProjectHooks(context.Background())
			if !errors.Is(err, domain.ErrPlanStale) || fixture.dispatches != 1 || fixture.invocations() != 0 {
				t.Fatalf("replaced root reached hooks: dispatches=%d invocations=%d err=%v", fixture.dispatches, fixture.invocations(), err)
			}
		})
	}
}

// A continuation after an approved prerequisite binds the registered roots, not the
// agent-writable workspace listing.
func TestProjectHookContinuationBindsRegisteredRoots(t *testing.T) {
	for _, scenario := range []string{"worktree added", "unregistered sibling added", "unregistered sibling removed", "registered root removed"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newYardActivityFixture(t, yardActivityOptions{metadata: true})
			if err := os.Mkdir(fixture.path("old-scratch"), 0o700); err != nil {
				t.Fatal(err)
			}
			before := fixture.prepare()
			switch scenario {
			case "worktree added":
				fixture.git("worktree", "add", "-q", "-b", "task", "../wt-task")
			case "unregistered sibling added":
				if err := os.Mkdir(fixture.path("scratch"), 0o700); err != nil {
					t.Fatal(err)
				}
			case "unregistered sibling removed":
				if err := os.Remove(fixture.path("old-scratch")); err != nil {
					t.Fatal(err)
				}
			case "registered root removed":
				if err := os.Rename(fixture.path("sample"), fixture.path("sample-old")); err != nil {
					t.Fatal(err)
				}
			}
			// The fresh continuation plan observes the guest again with the same owner records.
			current := before
			var err error
			if current.HookPlan, err = current.PrepareProjectHooks(context.Background(), false); err != nil {
				t.Fatal(err)
			}
			current.HookPlan.CaptureOwnerProjects([]domain.ProjectRecord{fixture.record})
			err = before.HookPlan.CheckContinuationScope(current.HookPlan)
			if refused := scenario == "registered root removed"; refused != errors.Is(err, domain.ErrPlanStale) || !refused && err != nil {
				t.Fatalf("continuation scope: %v", err)
			}
		})
	}
}

// The installed dispatcher refuses a project digest that moved after the host
// observation. Init observes again and redispatches a bounded number of times.
func TestProjectHookDispatchRetriesGuestScopeRace(t *testing.T) {
	for _, scenario := range []string{"one race", "persistent churn", "hook failure"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newYardActivityFixture(t, yardActivityOptions{metadata: true, hook: true})
			runtime := fixture.prepare()
			fixture.beforeDispatch = func(attempt int) {
				if scenario == "persistent churn" || scenario == "one race" && attempt == 1 {
					if err := os.MkdirAll(fixture.path("sample", "wt-"+string(rune('a'+attempt))), 0o700); err != nil {
						t.Fatal(err)
					}
				}
			}
			if scenario == "hook failure" {
				testkit.WriteFile(t, filepath.Join(fixture.root, "fail"), nil, 0o600)
			}
			err := runtime.RunProjectHooks(context.Background())
			var failure *application.ProjectHookFailure
			switch scenario {
			case "one race":
				if err != nil || fixture.dispatches != 2 || fixture.invocations() != 1 {
					t.Fatalf("race was not redispatched: dispatches=%d invocations=%d err=%v", fixture.dispatches, fixture.invocations(), err)
				}
			case "persistent churn":
				if err == nil || errors.As(err, &failure) || !strings.Contains(err.Error(), "kept changing") ||
					fixture.dispatches != projectHookDispatchAttempts || fixture.invocations() != 0 {
					t.Fatalf("persistent churn: dispatches=%d invocations=%d err=%v", fixture.dispatches, fixture.invocations(), err)
				}
			case "hook failure":
				if !errors.As(err, &failure) || len(failure.Hooks) != 1 || fixture.dispatches != 1 {
					t.Fatalf("hook failure was redispatched: dispatches=%d err=%v", fixture.dispatches, err)
				}
			}
		})
	}
}

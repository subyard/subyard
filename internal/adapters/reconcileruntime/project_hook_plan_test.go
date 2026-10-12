package reconcileruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

type hookPlanExecutor struct {
	observation hookObservation
	calls       int
	fail        bool
	observeFail bool
}

type changingNativeHookExecutor struct {
	hookPlanExecutor
	binding      string
	observations int
}

func (executor *changingNativeHookExecutor) Exec(ctx context.Context, project, name string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	if len(request.Command) > 1 && request.Command[0] == "bash" && request.Command[1] == "-s" {
		executor.observations++
	}
	if len(request.Command) > 1 && request.Command[0] == "sh" && request.Command[1] == "-c" {
		var scope struct {
			Bindings map[string]string `json:"bindings"`
		}
		if err := json.Unmarshal([]byte(request.Environment["SUBYARD_PROJECT_HOOK_SCOPE"]), &scope); err != nil {
			return ports.InstanceExecResult{}, err
		}
		if scope.Bindings["/usr/local/libexec/subyard/projects-changed.d/owned"] != executor.binding {
			executor.calls++
			return ports.InstanceExecResult{ExitCode: 1}, nil
		}
		result, err := executor.hookPlanExecutor.Exec(ctx, project, name, request)
		if err == nil && result.ExitCode == 0 {
			executor.binding = testRuntimeNewDigest
		}
		return result, err
	}
	return executor.hookPlanExecutor.Exec(ctx, project, name, request)
}

func TestPreparedProjectHooksRunOnceAfterSuccessfulNativeCatalogChange(t *testing.T) {
	for _, scenario := range []string{"repeat", "project changed", "source changed", "failed first invocation"} {
		t.Run(scenario, func(t *testing.T) {
			hook := "/usr/local/libexec/subyard/projects-changed.d/owned"
			executor := &changingNativeHookExecutor{hookPlanExecutor: hookPlanExecutor{observation: hookObservation{
				Projects: testRuntimeOldDigest, Wiring: testRuntimeNewDigest, Hooks: []string{hook},
				Facts: map[string]string{hook: testRuntimeOldDigest}, Resolved: map[string]string{hook: hook},
			}}, binding: testRuntimeOldDigest}
			incus := &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{Status: "running"}}}
			runtime := Runtime{RepositoryRoot: "../../..", Profiles: []profile.Definition{}, Integrations: []profile.IntegrationDefinition{}, Incus: incus, Executor: executor,
				Yard: domain.Context{IncusProject: "subyard", YardInstanceName: "yard", DevUser: "dev", DevUID: 1000}}
			prepare := func() {
				t.Helper()
				plan, err := runtime.PrepareProjectHooks(context.Background(), false)
				if err != nil {
					t.Fatal(err)
				}
				plan.hookBindings[hook] = executor.binding
				runtime.HookPlan = plan
			}
			prepare()
			executor.fail = scenario == "failed first invocation"
			err := runtime.RunProjectHooks(context.Background())
			if (err != nil) != executor.fail || executor.calls != 1 {
				t.Fatalf("first hook: calls=%d err=%v", executor.calls, err)
			}
			if executor.fail {
				executor.fail = false
				prepare()
				if err := runtime.RunProjectHooks(context.Background()); err != nil || executor.calls != 2 {
					t.Fatalf("fresh plan did not retry failed hook: calls=%d err=%v", executor.calls, err)
				}
				return
			}
			switch scenario {
			case "project changed":
				executor.observation.Projects = testRuntimeNewDigest
			case "source changed":
				executor.observation.Facts[hook] = testRuntimeNewDigest
			}
			observations := executor.observations
			err = runtime.RunProjectHooks(context.Background())
			if scenario == "repeat" && err != nil || scenario != "repeat" && !errors.Is(err, domain.ErrPlanStale) {
				t.Errorf("repeated plan guard: scenario=%s err=%v", scenario, err)
			}
			if executor.calls != 1 || executor.observations <= observations {
				t.Errorf("successful plan must recheck guards without repeating effects: calls=%d observations=%d->%d", executor.calls, observations, executor.observations)
			}
			if scenario == "repeat" {
				prepare()
				if err := runtime.RunProjectHooks(context.Background()); err != nil || executor.calls != 2 {
					t.Fatalf("fresh plan did not retry successful hook: calls=%d err=%v", executor.calls, err)
				}
			}
		})
	}
}

func (executor *hookPlanExecutor) Exec(_ context.Context, _ string, _ string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	if len(request.Command) > 1 && request.Command[0] == "bash" && request.Command[1] == "-s" {
		if executor.observeFail {
			return ports.InstanceExecResult{}, errors.New("observation unavailable")
		}
		payload, _ := json.Marshal(executor.observation)
		return ports.InstanceExecResult{Stdout: payload}, nil
	}
	if len(request.Command) > 2 && request.Command[0] == "sh" && request.Command[1] == "-c" {
		executor.calls++
		if request.Environment["SUBYARD_PROJECT_HOOK_SCOPE"] == "" {
			return ports.InstanceExecResult{}, errors.New("missing native scope")
		}
		if executor.fail {
			return ports.InstanceExecResult{ExitCode: 1}, nil
		}
	}
	return ports.InstanceExecResult{}, nil
}

func TestPreparedProjectHooksRejectScopeExpansionBeforeInvocation(t *testing.T) {
	for _, scenario := range []string{"projects", "hook", "wiring", "source after prerequisite", "failed hook", "success"} {
		t.Run(scenario, func(t *testing.T) {
			executor := &hookPlanExecutor{observation: hookObservation{Projects: testRuntimeOldDigest, Wiring: testRuntimeNewDigest, Hooks: []string{"/usr/local/libexec/subyard/projects-changed.d/owned"}, Facts: map[string]string{"/usr/local/libexec/subyard/projects-changed.d/owned": testRuntimeOldDigest}}}
			incus := &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{Status: "running"}}}
			runtime := Runtime{RepositoryRoot: "../../..", Profiles: []profile.Definition{}, Integrations: []profile.IntegrationDefinition{}, Incus: incus, Executor: executor, Yard: domain.Context{IncusProject: "subyard", YardInstanceName: "yard", DevUser: "dev", DevUID: 1000}}
			plan, err := runtime.PrepareProjectHooks(context.Background(), false)
			if err != nil {
				t.Fatal(err)
			}
			runtime.HookPlan = plan
			switch scenario {
			case "projects":
				executor.observation.Projects = testRuntimeNewDigest
			case "hook":
				executor.observation.Hooks = append(executor.observation.Hooks, "/usr/local/libexec/subyard/projects-changed.d/new")
			case "wiring":
				executor.observation.Wiring = testRuntimeOldDigest
			case "failed hook":
				executor.fail = true
			}
			err = runtime.CheckProjectHookPlan(context.Background(), true)
			if scenario == "projects" || scenario == "hook" || scenario == "wiring" {
				if !errors.Is(err, domain.ErrPlanStale) || executor.calls != 0 {
					t.Fatalf("new scope reached effects: calls=%d err=%v", executor.calls, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "source after prerequisite" {
				executor.observation.Facts = map[string]string{"/usr/local/libexec/subyard/projects-changed.d/owned": testRuntimeNewDigest}
				if err := runtime.RunProjectHooks(context.Background()); !errors.Is(err, domain.ErrPlanStale) || executor.calls != 0 {
					t.Fatalf("same-path source tamper reached hook: calls=%d err=%v", executor.calls, err)
				}
				return
			}
			err = runtime.RunProjectHooks(context.Background())
			if (err != nil) != (scenario == "failed hook") || executor.calls != 1 {
				t.Fatalf("native outcome swallowed: calls=%d err=%v", executor.calls, err)
			}
		})
	}
}

func TestProjectHookDispatcherEnforcesManifestBeforeAnyHook(t *testing.T) {
	for _, scenario := range []string{"project added", "hook added", "source swapped", "large unrelated metadata", "command name", "command source swapped", "PATH shadow", "success", "hook failure"} {
		t.Run(scenario, func(t *testing.T) {
			root := testkit.TempDir(t)
			source, err := os.ReadFile("../../../config/projects-changed.sh")
			if err != nil {
				t.Fatal(err)
			}
			// Execute the actual dispatcher and observer against an isolated guest filesystem.
			source = []byte(strings.NewReplacer("/srv/workspaces", filepath.Join(root, "workspaces"), "/usr/local/libexec/subyard", filepath.Join(root, "libexec"), "/etc/subyard", filepath.Join(root, "etc"), "/usr/local/bin", filepath.Join(root, "bin"), "/usr/bin", filepath.Join(root, "usrbin"), "/bin", filepath.Join(root, "systembin")).Replace(string(source)))
			dispatcher := filepath.Join(root, "libexec", "projects-changed")
			hookdir := dispatcher + ".d"
			for _, dir := range []string{hookdir, filepath.Join(root, "etc"), filepath.Join(root, "workspaces")} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			testkit.WriteFile(t, dispatcher, source, 0o700)
			testkit.WriteFile(t, filepath.Join(root, "etc", "agent-project-hooks"), []byte("\n"), 0o600)
			log := filepath.Join(root, "effects")
			hook := filepath.Join(hookdir, "owned")
			body := "#!/bin/sh\nprintf invoked > '" + log + "'\n"
			if scenario == "hook failure" {
				body += "exit 1\n"
			}
			testkit.WriteFile(t, hook, []byte(body), 0o700)
			if scenario == "large unrelated metadata" {
				workspace := filepath.Join(root, "workspaces", "large")
				if err := os.Mkdir(workspace, 0o700); err != nil {
					t.Fatal(err)
				}
				file, err := os.OpenFile(filepath.Join(workspace, "private.env"), os.O_CREATE|os.O_WRONLY, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				if err := file.Truncate(2 << 20); err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "command name" || scenario == "command source swapped" || scenario == "PATH shadow" {
				if err := os.Mkdir(filepath.Join(root, "bin"), 0o700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(root, "bin", "fixture-command"), []byte(body), 0o700)
				testkit.WriteFile(t, filepath.Join(root, "etc", "agent-project-hooks"), []byte("fixture-command\n"), 0o600)
			}
			observed, err := exec.Command("bash", dispatcher, "--observe").Output()
			if err != nil {
				t.Fatal(err)
			}
			var observation hookObservation
			if err := json.Unmarshal(observed, &observation); err != nil {
				t.Fatal(err)
			}
			scope, _ := json.Marshal(map[string]any{"projects": observation.Projects, "roots": observation.Roots, "hooks": observation.Hooks, "facts": observation.Facts, "resolved": observation.Resolved})
			if scenario == "project added" {
				if err := os.Mkdir(filepath.Join(root, "workspaces", "new"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "source swapped" {
				testkit.WriteFile(t, hook, []byte(body+"# changed\n"), 0o700)
			}
			if scenario == "hook added" {
				testkit.WriteFile(t, filepath.Join(hookdir, "new"), []byte(body), 0o700)
			}
			if scenario == "command source swapped" {
				testkit.WriteFile(t, filepath.Join(root, "bin", "fixture-command"), []byte(body+"#tamper\n"), 0o700)
			}
			command := exec.Command("bash", dispatcher)
			command.Env = append(os.Environ(), "SUBYARD_PROJECT_HOOK_SCOPE="+string(scope))
			if scenario == "PATH shadow" {
				attacker := filepath.Join(root, "unapproved")
				if err := os.Mkdir(attacker, 0o700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(attacker, "fixture-command"), []byte("#!/bin/sh\nprintf unapproved > '"+log+"'\n"), 0o700)
				command.Env = append(command.Env, "PATH="+attacker+":"+os.Getenv("PATH"))
			}
			output, err := command.CombinedOutput()
			if (err != nil) != (scenario != "success" && scenario != "large unrelated metadata" && scenario != "command name" && scenario != "PATH shadow") {
				t.Fatalf("native dispatcher outcome: err=%v output=%s", err, output)
			}
			_, statErr := os.Stat(log)
			if scenario == "project added" || scenario == "hook added" || scenario == "source swapped" || scenario == "command source swapped" {
				if !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("unapproved target reached a hook: %v", statErr)
				}
			} else if statErr != nil {
				t.Fatal(statErr)
			}
			if scenario == "PATH shadow" {
				payload, err := os.ReadFile(log)
				if err != nil || string(payload) != "invoked" {
					t.Fatalf("unapproved PATH hook executed: %q %v", payload, err)
				}
			}
		})
	}
}

func TestProjectHookObservationBoundsCapturedInputs(t *testing.T) {
	// Working-tree contents stay outside the core scope, regardless of tree size.
	t.Setenv("TMPDIR", "/tmp")
	for _, scenario := range []struct {
		name                         string
		directories, metadata, hooks int
		largeHook                    bool
		limitError                   string
	}{
		{"large cache tree", 4097, 0, 1, false, ""},
		{"metadata overflow", 0, 4097, 1, false, "project hook scope exceeds observation limit"},
		{"hook overflow", 0, 0, 4097, false, "project hook scope exceeds observation limit"},
		{"shared metadata and hook bound", 0, 4088, 8, false, "project hook scope exceeds observation limit"},
		{"hook content overflow", 0, 0, 1, true, "project hook input exceeds observation limit"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root := testkit.TempDir(t)
			source, err := os.ReadFile("../../../config/projects-changed.sh")
			if err != nil {
				t.Fatal(err)
			}
			source = []byte(strings.NewReplacer("/srv/workspaces", filepath.Join(root, "workspaces"), "/usr/local/libexec/subyard", filepath.Join(root, "libexec"), "/etc/subyard", filepath.Join(root, "etc")).Replace(string(source)))
			dispatcher := filepath.Join(root, "libexec", "projects-changed")
			workspace := filepath.Join(root, "workspaces", "project")
			cache := filepath.Join(workspace, "src", ".build", "cache")
			for _, path := range []string{dispatcher + ".d", filepath.Join(root, "etc"), cache} {
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			for index := range scenario.directories {
				if err := os.Mkdir(filepath.Join(cache, fmt.Sprint(index)), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			for index := range scenario.metadata {
				testkit.WriteFile(t, filepath.Join(workspace, fmt.Sprint(index)), nil, 0o600)
			}
			testkit.WriteFile(t, dispatcher, source, 0o700)
			testkit.WriteFile(t, filepath.Join(root, "etc", "agent-project-hooks"), []byte("\n"), 0o600)
			log := filepath.Join(root, "effects")
			body := []byte("#!/bin/sh\nprintf invoked > '" + log + "'\n")
			if scenario.largeHook {
				body = append(body, make([]byte, 1<<20)...)
			}
			for index := range scenario.hooks {
				testkit.WriteFile(t, filepath.Join(dispatcher+".d", fmt.Sprint(index)), body, 0o700)
			}
			observe := func() ([]byte, error) { return exec.Command("bash", dispatcher, "--observe").Output() }
			payload, err := observe()
			if scenario.limitError != "" {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || !strings.Contains(string(exit.Stderr), scenario.limitError) {
					t.Fatalf("native bound: err=%v", err)
				}
				return
			}
			if err != nil || len(payload) > 64<<10 {
				t.Fatalf("native cache-tree observation: err=%v bytes=%d", err, len(payload))
			}
			var observation hookObservation
			if err := json.Unmarshal(payload, &observation); err != nil {
				t.Fatal(err)
			}
			// Cache writes must neither require another discovery nor stale the plan.
			testkit.WriteFile(t, filepath.Join(cache, "new-cache-file"), []byte("cache"), 0o600)
			current, err := observe()
			if err != nil || string(payload) != string(current) {
				t.Fatalf("cache churn changed captured scope: %v", err)
			}
			run := func() error {
				command := exec.Command("bash", dispatcher)
				command.Env = append(os.Environ(), "SUBYARD_PROJECT_HOOK_SCOPE="+string(payload))
				return command.Run()
			}
			if err := run(); err != nil {
				t.Fatal(err)
			}
			if content, err := os.ReadFile(log); err != nil || string(content) != "invoked" {
				t.Fatalf("captured hook was not invoked: %v", err)
			}
			if err := os.Remove(log); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(workspace, "src"), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := run(); err == nil {
				t.Fatal("canonical project-root metadata drift was approved")
			}
			if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("project-root drift reached a hook")
			}
		})
	}
}

func TestStoppedProjectHooksSurviveApprovedTemporaryStart(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(fmt.Sprintf("reconstruction=%t", unavailable), func(t *testing.T) {
			executor := &hookPlanExecutor{observation: hookObservation{Projects: testRuntimeOldDigest, Wiring: testRuntimeNewDigest}}
			incus := &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{Status: "stopped", Config: map[string]string{
				"user.subyard.managed": "true", "user.subyard.initialized": "true", "user.subyard.desired_power": "stopped",
			}}}}
			runtime := Runtime{RepositoryRoot: "../../..", Profiles: []profile.Definition{}, Integrations: []profile.IntegrationDefinition{}, Incus: incus, Executor: executor, Yard: domain.Context{IncusProject: "subyard", YardInstanceName: "yard", DevUser: "dev", DevUID: 1000}}
			plan, err := runtime.PrepareProjectHooks(context.Background(), unavailable, true)
			if err != nil {
				t.Fatal(err)
			}
			if plan.OperationStep("subyard/yard").Decision != domain.StepConditional {
				t.Fatal("approved temporary boot was omitted from hook assessment")
			}
			runtime.HookPlan = plan
			if err := runtime.CheckProjectHookPlan(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			// An approved instance repair temporarily boots the stopped yard; finalization
			// still owns restoring desired=stopped after native runtime and hook verification.
			incus.Reconcile.Instance.Status = "running"
			incus.Reconcile.Instance.Config["user.subyard.initialized"] = "false"
			if unavailable {
				incus.Reconcile.Instance.Config["user.subyard.desired_power"] = "running"
			}
			if err := runtime.RunProjectHooks(context.Background()); err != nil {
				t.Fatalf("approved temporary start acquired unplanned hook work: %v", err)
			}
			if executor.calls != 1 {
				t.Fatalf("hook dispatcher calls=%d, want 1", executor.calls)
			}
		})
	}
}

func TestTemporaryStoppedProjectHooksRetainScopeAndIntentGuards(t *testing.T) {
	for _, scenario := range []string{"success", "new hook", "new project root", "desired power changed", "unexpected pre-write start", "observation unavailable", "native source changed", "projects changed after resolution", "skip without prerequisite"} {
		t.Run(scenario, func(t *testing.T) {
			root := testkit.TempDir(t)
			runtime := profileRuntimeFixtureAt(t, root, "#!/bin/sh\nprintf '{\"state\":\"current\",\"actual\":\""+testRuntimeNewDigest+"\",\"desired\":\""+testRuntimeNewDigest+"\"}\\n'\n")
			hook := "/usr/local/libexec/subyard/projects-changed.d/owned"
			profileFile := filepath.Join(root, "config", "profiles", "synthetic", "profile.json")
			testkit.WriteFile(t, profileFile, []byte(`{"schema_version":1,"runtime":{"activation_id":"synthetic-runtime","handler":"runtime.sh","projects_changed_hooks":["`+hook+`"]}}`), 0o600)
			source, err := os.ReadFile("../../../config/projects-changed.sh")
			if err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(root, "config", "projects-changed.sh"), source, 0o644)
			executor := &hookPlanExecutor{observation: hookObservation{Projects: testRuntimeOldDigest, Wiring: testRuntimeNewDigest, Hooks: []string{hook}, Facts: map[string]string{hook: testRuntimeOldDigest}}}
			runtime.Executor = executor
			incus := runtime.Incus.(*testkit.Incus)
			instance := ports.InstanceInfo{Status: "stopped", Config: map[string]string{"user.subyard.managed": "true", "user.subyard.initialized": "true", "user.subyard.desired_power": "stopped"}}
			incus.Instances["subyard/yard"] = instance
			incus.Reconcile = ports.ReconcileState{InstanceFound: true, Instance: instance}
			runtime.RuntimePlan, err = runtime.PrepareProfileRuntimes(context.Background(), false, false, true)
			if err != nil {
				t.Fatal(err)
			}
			runtime.HookPlan, err = runtime.PrepareProjectHooks(context.Background(), false, scenario != "skip without prerequisite")
			if err != nil {
				t.Fatal(err)
			}
			if err := runtime.CheckProjectHookPlan(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			if scenario == "skip without prerequisite" {
				if runtime.HookPlan.OperationStep("subyard/yard").Decision != domain.StepSkip {
					t.Fatal("stopped no-op acquired hook work")
				}
				if err := runtime.RunProjectHooks(context.Background()); err != nil || executor.calls != 0 {
					t.Fatalf("stopped no-op ran guest hooks: calls=%d err=%v", executor.calls, err)
				}
			}
			incus.Reconcile.Instance.Status = "running"
			incus.Reconcile.Instance.Config["user.subyard.initialized"] = "false"
			instance.Status = "running"
			incus.Instances["subyard/yard"] = instance
			switch scenario {
			case "new hook":
				executor.observation.Hooks = append(executor.observation.Hooks, hook+"-new")
			case "new project root":
				executor.observation.Roots = []string{"/srv/workspaces/new"}
			case "desired power changed":
				incus.Reconcile.Instance.Config["user.subyard.desired_power"] = "running"
			case "unexpected pre-write start":
				if err := runtime.CheckProjectHookPlan(context.Background(), true); !errors.Is(err, domain.ErrPlanStale) || executor.calls != 0 {
					t.Fatalf("unannounced start was approved: %v", err)
				}
				return
			case "observation unavailable":
				executor.observeFail = true
			case "native source changed":
				testkit.WriteFile(t, filepath.Join(root, "config", "profiles", "synthetic", "runtime.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o700)
			case "projects changed after resolution":
				if err := runtime.RunProjectHooks(context.Background()); err != nil {
					t.Fatal(err)
				}
				executor.calls = 0
				executor.observation.Projects = testRuntimeNewDigest
			}
			err = runtime.RunProjectHooks(context.Background())
			if scenario == "success" {
				if err != nil || executor.calls != 1 {
					t.Fatalf("approved conditional hook failed: calls=%d err=%v", executor.calls, err)
				}
			} else if err == nil || executor.calls != 0 {
				t.Fatalf("unapproved change reached hooks: calls=%d err=%v", executor.calls, err)
			}
		})
	}
}

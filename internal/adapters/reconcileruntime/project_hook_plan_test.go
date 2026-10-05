package reconcileruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
}

func (executor *hookPlanExecutor) Exec(_ context.Context, _ string, _ string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	if len(request.Command) > 1 && request.Command[0] == "bash" && request.Command[1] == "-s" {
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
			runtime := Runtime{RepositoryRoot: "../../..", Profiles: []profile.Definition{}, Incus: incus, Executor: executor, Yard: domain.Context{IncusProject: "subyard", YardInstanceName: "yard", DevUser: "dev", DevUID: 1000}}
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

func TestProjectHookObservationBoundsUniquePaths(t *testing.T) {
	for _, scenario := range []struct {
		name                  string
		gitRoots, directories int
		limitError            string
	}{
		{"thousand roots and retained directories", 1000, 120, ""},
		{"directory overflow", 0, 4097, "project discovery exceeds observation limit"},
		{"metadata overflow", 1400, 0, "project hook scope exceeds observation limit"},
		{"mixed union overflow", 500, 1500, "project hook scope exceeds observation limit"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root := testkit.TempDir(t)
			source, err := os.ReadFile("../../../config/projects-changed.sh")
			if err != nil {
				t.Fatal(err)
			}
			source = []byte(strings.NewReplacer("/srv/workspaces", filepath.Join(root, "workspaces"), "/usr/local/libexec/subyard", filepath.Join(root, "libexec"), "/etc/subyard", filepath.Join(root, "etc")).Replace(string(source)))
			dispatcher := filepath.Join(root, "libexec", "projects-changed")
			hook := dispatcher + ".d/owned"
			workspace := filepath.Join(root, "workspaces", "project", "src")
			seed := filepath.Join(root, "seed")
			for _, path := range []string{filepath.Dir(hook), filepath.Join(root, "etc"), workspace, seed} {
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if output, err := exec.Command("git", "init", "-q", "--template=", seed).CombinedOutput(); err != nil {
				t.Fatalf("native Git fixture: %v %s", err, output)
			}
			for index := range scenario.gitRoots {
				checkout := filepath.Join(workspace, ".build", "capacity", fmt.Sprint(index), ".git")
				if err := os.CopyFS(checkout, os.DirFS(filepath.Join(seed, ".git"))); err != nil {
					t.Fatal(err)
				}
			}
			for index := range scenario.directories {
				if err := os.MkdirAll(filepath.Join(workspace, "retained", fmt.Sprint(index), "nested"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			// Native Git creates its own modes; normalize the isolated fixture explicitly.
			if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				mode := os.FileMode(0o600)
				if entry.IsDir() {
					mode = 0o700
				}
				return os.Chmod(path, mode)
			}); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, dispatcher, source, 0o700)
			testkit.WriteFile(t, filepath.Join(root, "etc", "agent-project-hooks"), []byte("\n"), 0o600)
			log := filepath.Join(root, "effects")
			body := "#!/bin/sh\nprintf invoked > '" + log + "'\nchmod 0600 '" + log + "'\n"
			testkit.WriteFile(t, hook, []byte(body), 0o700)
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
				t.Fatalf("native thousand-root observation: err=%v bytes=%d", err, len(payload))
			}
			var observation hookObservation
			if err := json.Unmarshal(payload, &observation); err != nil {
				t.Fatal(err)
			}
			approved, err := json.Marshal(observation)
			if err != nil {
				t.Fatal(err)
			}
			run := func() error {
				command := exec.Command("bash", dispatcher)
				command.Env = append(os.Environ(), "SUBYARD_PROJECT_HOOK_SCOPE="+string(approved))
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
			gitConfig := filepath.Join(workspace, ".build", "capacity", "0", ".git", "config")
			content, err := os.ReadFile(gitConfig)
			if err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, gitConfig, append(content, []byte("\n# fixture metadata changed\n")...), 0o600)
			if err := run(); err == nil {
				t.Fatal("traversal suppressed Git metadata hashing")
			}
			if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("metadata drift reached a hook")
			}
			payload, err = observe()
			if err != nil || json.Unmarshal(payload, &observation) != nil {
				t.Fatal("cannot capture refreshed fixture scope")
			}
			approved, err = json.Marshal(observation)
			if err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, hook, []byte(body+"# source changed\n"), 0o700)
			if err := run(); err == nil {
				t.Fatal("same-path hook source change was approved")
			}
			if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("hook source drift reached a hook")
			}
		})
	}
}

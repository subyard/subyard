package reconcileruntime

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func writeAgentSkillFixture(t *testing.T, root string) []string {
	t.Helper()
	files := map[string][]byte{
		"config/skills/core/sample/SKILL.md":              []byte("generic core skill\n"),
		"config/profiles/synthetic/skills/local/SKILL.md": []byte("generic profile skill\n"),
		"config/profiles/synthetic/skills.json":           []byte(`{"declared":{"SKILL.md":"payload.txt","duplicate":"payload.txt"}}`),
		"config/profiles/synthetic/payload.txt":           []byte("declared skill\n"),
	}
	for _, name := range []string{"scripts/refresh-agent-skills.sh", "config/skills/package.py", "config/skills/refresh.sh", "config/skills/deliver.py", "config/skills/probe.py"} {
		payload, err := os.ReadFile(filepath.Join("../../..", name))
		if err != nil {
			t.Fatal(err)
		}
		files[name] = payload
	}
	names := []string{}
	for name, payload := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, payload, 0o600)
		names = append(names, name)
	}
	for _, name := range []string{"config/skills/tests/ignored", "config/skills/__pycache__/ignored", "config/profiles/synthetic/skills/tests/ignored", "config/profiles/synthetic/skills/local/cache.pyc"} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, []byte("ignored\n"), 0o600)
	}
	slices.Sort(names)
	return names
}

func TestAgentSkillSourcesParityWithCapturedReadiness(t *testing.T) {
	root := testkit.TempDir(t)
	want := writeAgentSkillFixture(t, root)
	incus := &testkit.Incus{ExecSteps: []testkit.IncusExecStep{{Result: ports.InstanceExecResult{Stdout: []byte("skills-current\n")}}}}
	runtime := Runtime{RepositoryRoot: root, Executor: incus, Yard: domain.Context{DevUser: "dev"}}
	paths, err := runtime.agentSkillSources()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(paths, want) {
		t.Fatalf("native inventory=%v want=%v", paths, want)
	}
	if ready, err := runtime.agentSkillsConverged(context.Background(), paths); err != nil || !ready {
		t.Fatal(ready, err)
	}
	var input struct{ Files map[string][]byte }
	if err := json.Unmarshal(incus.ExecCalls[0].Request.Stdin, &input); err != nil {
		t.Fatal(err)
	}
	captured := []string{}
	for name := range input.Files {
		captured = append(captured, name)
	}
	slices.Sort(captured)
	output, err := exec.Command("python3", "-B", filepath.Join(root, "config/skills/package.py"), "files", root).Output()
	if err != nil {
		t.Fatal(err)
	}
	var python []string
	if err := json.Unmarshal(output, &python); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(captured, python) || !slices.Equal(python, want) {
		t.Fatalf("captured=%v python=%v want=%v", captured, python, want)
	}
}

func TestAgentSkillSourcesRefuseUnsafeInputs(t *testing.T) {
	for _, mutation := range []string{"parent", "absolute", "symlink", "ancestor-symlink", "directory", "missing-wrapper-link", "profiles-link"} {
		t.Run(mutation, func(t *testing.T) {
			root := testkit.TempDir(t)
			writeAgentSkillFixture(t, root)
			manifest := filepath.Join(root, "config/profiles/synthetic/skills.json")
			switch mutation {
			case "parent":
				testkit.WriteFile(t, manifest, []byte(`{"unsafe":{"SKILL.md":"../outside"}}`), 0o600)
			case "absolute":
				testkit.WriteFile(t, manifest, []byte(`{"unsafe":{"SKILL.md":"/outside"}}`), 0o600)
			case "directory":
				testkit.WriteFile(t, manifest, []byte(`{"unsafe":{"SKILL.md":"skills"}}`), 0o600)
			case "symlink":
				path := filepath.Join(root, "config/profiles/synthetic/payload.txt")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("/etc/passwd", path); err != nil {
					t.Fatal(err)
				}
			case "ancestor-symlink":
				path := filepath.Join(root, "config/skills/core")
				if err := os.Rename(path, path+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("core-original", path); err != nil {
					t.Fatal(err)
				}
			case "missing-wrapper-link":
				path := filepath.Join(root, "scripts/refresh-agent-skills.sh")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("missing", path); err != nil {
					t.Fatal(err)
				}
			case "profiles-link":
				path := filepath.Join(root, "config/profiles")
				if err := os.Rename(path, path+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("missing", path); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := (Runtime{RepositoryRoot: root}).agentSkillSources(); err == nil {
				t.Fatal("unsafe native source admitted")
			}
			if err := exec.Command("python3", "-B", filepath.Join(root, "config/skills/package.py"), "files", root).Run(); err == nil {
				t.Fatal("unsafe Python source admitted")
			}
		})
	}
}

func TestAgentSkillsReadinessUsesCapturedSourcesAndGuestProbe(t *testing.T) {
	for _, test := range []struct {
		output     string
		want, fail bool
	}{{"skills-current\n", true, false}, {"skills-stale\n", false, false}, {"unknown\n", false, true}} {
		incus := &testkit.Incus{ExecSteps: []testkit.IncusExecStep{{Result: ports.InstanceExecResult{Stdout: []byte(test.output)}}}}
		runtime := Runtime{RepositoryRoot: "../../..", Executor: incus, Yard: domain.Context{IncusProject: "p", YardInstanceName: "i", DevUser: "fixture-user"}, Environment: []string{"ENVIRONMENT_PROFILES=synthetic"}}
		paths, err := runtime.agentSkillSources()
		if err != nil {
			t.Fatal(err)
		}
		current, err := runtime.agentSkillsConverged(context.Background(), paths)
		if current != test.want || (err != nil) != test.fail {
			t.Fatalf("%q current=%v err=%v", test.output, current, err)
		}
		request := incus.ExecCalls[0].Request
		if len(request.Command) != 4 || request.Command[0] != "python3" || !strings.Contains(request.Command[3], "skills-current") {
			t.Fatalf("wrong native probe: %v", request.Command)
		}
		var input struct {
			Files          map[string][]byte
			User, Profiles string
			Coding         bool
		}
		if err := json.Unmarshal(request.Stdin, &input); err != nil {
			t.Fatal(err)
		}
		if input.User != "fixture-user" || input.Profiles != "synthetic" || !input.Coding || len(input.Files["config/skills/deliver.py"]) == 0 || len(input.Files["config/skills/core/subyard-preview/SKILL.md"]) == 0 {
			t.Fatal("probe omitted selected source or runtime inputs")
		}
	}
}

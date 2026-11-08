package reconcileruntime

import (
	"context"
	"encoding/json"
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

type appliedIdentityExecutor struct{ root string }

func (fixture appliedIdentityExecutor) Exec(ctx context.Context, _, _ string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	if len(request.Command) == 5 { // No integration inventory, including no tools selected.
		return ports.InstanceExecResult{Stdout: []byte(`{"installed":false}`)}, nil
	}
	program := request.Command[3]
	program = strings.ReplaceAll(program, "'/etc/", "'"+fixture.root+"/etc/")
	program = strings.ReplaceAll(program, "'/var/", "'"+fixture.root+"/var/")
	program = strings.ReplaceAll(program, "        info = parent.lstat()", "        if parent == Path('/tmp'): break\n        info = parent.lstat()")
	// The fixture owns the protected root itself: it is the ordinary root-owned
	// case, not the Codex sandbox whose root maps to an overflow ID.
	program = strings.ReplaceAll(program, "if root.st_uid != 0:", "if False:")
	program = strings.ReplaceAll(program, "owner = (0, 0)", fmt.Sprintf("owner = (%d, %d)", os.Getuid(), os.Getgid()))
	program = strings.ReplaceAll(program, "owner[0] != 0", fmt.Sprintf("owner[0] != %d", os.Getuid()))
	program = strings.ReplaceAll(program, "st_uid != 0", fmt.Sprintf("st_uid != %d", os.Getuid()))
	program = strings.ReplaceAll(program, "st_gid != 0", fmt.Sprintf("st_gid != %d", os.Getgid()))
	program = "import types\nimport sys\nsys.modules['pwd'] = types.SimpleNamespace(getpwall=lambda: [types.SimpleNamespace(pw_name='dev', pw_uid=1001, pw_dir='/home/dev')])\n" + program
	command := exec.CommandContext(ctx, "python3", "-B", "-c", program)
	command.Stdin = strings.NewReader(string(request.Stdin))
	output, err := command.CombinedOutput()
	result := ports.InstanceExecResult{Stdout: output}
	if exit, ok := err.(*exec.ExitError); ok {
		result.ExitCode = exit.ExitCode()
	}
	return result, err
}

func TestAppliedGuestIdentityWithoutIntegrationInventory(t *testing.T) {
	for _, scenario := range []string{"current", "missing", "unsafe-mode", "symlink", "foreign-yard", "ambiguous-home", "incomplete"} {
		t.Run(scenario, func(t *testing.T) {
			root := testkit.TempDir(t)
			for _, path := range []string{"etc/subyard", "var/lib/subyard/agent-skills", filepath.Dir(guestFactsSource)} {
				if err := os.MkdirAll(filepath.Join(root, path), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			program, err := os.ReadFile(filepath.Join("../../..", guestFactsSource))
			if err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(root, guestFactsSource), program, 0o600)
			testkit.WriteFile(t, filepath.Join(root, "etc/machine-id"), []byte(strings.Repeat("a", 32)), 0o644)
			runtime := Runtime{RepositoryRoot: root, Executor: appliedIdentityExecutor{root}, Integrations: []profile.IntegrationDefinition{}, Yard: domain.Context{YardName: "default", IncusProject: "subyard", YardInstanceName: "yard", DevUser: "other", DevUID: 1002}, Environment: []string{"YARD_VERSION=1.2.3"}}
			var target any
			if err := json.Unmarshal(runtime.guestRuntimeFacts(), &target); err != nil {
				t.Fatal(err)
			}
			facts := map[string]any{"schema": 1, "machine_id": strings.Repeat("a", 32), "status": "current", "target": target, "last_success": target, "capabilities": "unknown", "readiness": "unknown"}
			if scenario == "foreign-yard" {
				target.(map[string]any)["yard"].(map[string]any)["name"] = "foreign"
			}
			if scenario == "incomplete" {
				facts["status"] = "incomplete"
			}
			payload, _ := json.Marshal(facts)
			testkit.WriteFile(t, filepath.Join(root, "etc/subyard/guest-runtime.json"), payload, 0o644)
			home := "/home/dev"
			if scenario == "ambiguous-home" {
				home = "/home/foreign"
			}
			receipt, _ := json.Marshal(map[string]any{"schema": 1, "home": home, "extra": false, "skills": map[string]any{"sample": map[string]string{"SKILL.md": strings.Repeat("b", 64)}}})
			path := filepath.Join(root, "var/lib/subyard/agent-skills/core.json")
			if scenario != "missing" {
				testkit.WriteFile(t, path, receipt, 0o600)
			}
			if scenario == "unsafe-mode" {
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "symlink" {
				if err := os.Rename(path, path+".target"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".target", path); err != nil {
					t.Fatal(err)
				}
			}
			applied, err := runtime.AppliedIntegrationConfiguration(context.Background())
			if scenario == "current" {
				if err != nil || applied.Installed || applied.Settings != nil || applied.Identity["DEV_USER"] != "dev" || applied.Identity["DEV_UID"] != "1001" {
					t.Fatalf("no-inventory identity: %#v %v", applied, err)
				}
				if err := runtime.CheckAppliedGuestIdentity(context.Background()); err != nil {
					t.Fatal(err)
				}
			} else if scenario == "missing" {
				if err != nil || len(applied.Identity) != 0 {
					t.Fatalf("missing evidence: %#v %v", applied, err)
				}
				if err := runtime.CheckAppliedGuestIdentity(context.Background()); err == nil {
					t.Fatal("unverifiable identity authoring accepted")
				}
			} else if err == nil {
				t.Fatal("invalid owner evidence accepted")
			}
		})
	}
}

func TestAppliedEnvironmentProfilesUseProtectedSuccessfulSelection(t *testing.T) {
	for _, scenario := range []string{"empty", "retained", "missing", "foreign", "unsafe", "incomplete"} {
		t.Run(scenario, func(t *testing.T) {
			root := testkit.TempDir(t)
			for _, path := range []string{"etc/subyard", filepath.Dir(guestFactsSource)} {
				if err := os.MkdirAll(filepath.Join(root, path), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			program, err := os.ReadFile(filepath.Join("../../..", guestFactsSource))
			if err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(root, guestFactsSource), program, 0o600)
			testkit.WriteFile(t, filepath.Join(root, "etc/machine-id"), []byte(strings.Repeat("a", 32)), 0o644)
			runtime := Runtime{RepositoryRoot: root, Executor: appliedIdentityExecutor{root}, Yard: domain.Context{YardName: "default", IncusProject: "subyard", YardInstanceName: "yard", DevUser: "dev", DevUID: 1001}, Environment: []string{"YARD_VERSION=1.2.3", "ENVIRONMENT_PROFILES=newly-desired"}}
			var target map[string]any
			if err := json.Unmarshal(runtime.guestRuntimeFacts(), &target); err != nil {
				t.Fatal(err)
			}
			selected := []string{}
			if scenario == "retained" {
				selected = []string{"retained"}
			}
			target["selected_profiles"] = selected
			if scenario == "foreign" {
				target["yard"].(map[string]any)["name"] = "foreign"
			}
			facts := map[string]any{"schema": 1, "machine_id": strings.Repeat("a", 32), "status": "current", "target": target, "last_success": target, "capabilities": "unknown", "readiness": "unknown"}
			if scenario == "incomplete" {
				facts["status"] = "incomplete"
			}
			content, _ := json.Marshal(facts)
			mode := os.FileMode(0o644)
			if scenario == "unsafe" {
				mode = 0o666
			}
			if scenario != "missing" {
				testkit.WriteFile(t, filepath.Join(root, "etc/subyard/guest-runtime.json"), content, mode)
			}
			actual, known, err := runtime.AppliedEnvironmentProfiles(context.Background())
			switch scenario {
			case "empty", "retained":
				if err != nil || !known || strings.Join(actual, " ") != strings.Join(selected, " ") {
					t.Fatalf("selection=%v known=%v: %v", actual, known, err)
				}
			case "missing":
				if err != nil || known {
					t.Fatalf("missing proof accepted: %v", err)
				}
			default:
				if err == nil {
					t.Fatal("foreign or incomplete proof accepted")
				}
			}
		})
	}
}

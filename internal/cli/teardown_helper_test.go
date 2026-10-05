package cli

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestTeardownVolumeHelperFeedsNativeDeletionLoop(t *testing.T) {
	root := testkit.TempDir(t)
	helper, err := filepath.Abs("../../scripts/lib/teardown-plan.py")
	if err != nil {
		t.Fatal(err)
	}
	resources := []ports.TeardownResource{
		{Kind: "volume", Pool: "approved-pool", Name: "yard-srv", Binding: strings.Repeat("a", 64)},
		{Kind: "volume", Pool: "other-pool", Name: "second", Binding: strings.Repeat("b", 64)},
		{Kind: "profile", Name: "default", Binding: strings.Repeat("c", 64)},
	}
	payload, err := json.Marshal(resources)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("python3", helper, "volumes")
	command.Env = append(os.Environ(), "SUBYARD_TEARDOWN_INVENTORY="+string(payload))
	output, err := command.CombinedOutput()
	if err != nil || string(output) != "approved-pool\tyard-srv\nother-pool\tsecond\n" {
		t.Fatalf("native no-argument volumes command: %q, %v", output, err)
	}
	// Exercise the shipped loop, rather than a second implementation of its
	// argument parsing. The fixture Incus deletes real file-backed volumes.
	script, err := os.ReadFile("../../scripts/teardown-physical.sh")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(script), "    while IFS=$'\\t' read -r approved_pool approved_volume; do")
	endText := "done < <(python3 \"$TEARDOWN_GUARD\" volumes)"
	end := strings.Index(string(script), endText)
	if start < 0 || end < start {
		t.Fatal("native volume deletion loop not found")
	}
	for _, path := range []string{"approved-pool/yard-srv", "other-pool/second", "approved-pool/unapproved"} {
		directory := filepath.Join(root, filepath.Dir(path))
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(directory, 0700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(root, path), []byte("native volume"), 0600)
	}
	setup := `set -euo pipefail
PROJ=(--project fixture)
guard_resource() { [ "$1" = volume ]; }
ok() { :; }
die() { exit 9; }
incus() {
  [ "$1" = storage ] && [ "$2" = volume ] && [ "$3" = delete ]
  rm -- "$FIXTURE_ROOT/$4/$5"
}

`
	command = exec.Command("bash", "-c", setup+string(script)[start:end+len(endText)])
	command.Env = append(os.Environ(), "SUBYARD_TEARDOWN_INVENTORY="+string(payload), "TEARDOWN_GUARD="+helper, "FIXTURE_ROOT="+root)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("native deletion loop: %s, %v", output, err)
	}
	for _, path := range []string{"approved-pool/yard-srv", "other-pool/second"} {
		if _, err := os.Stat(filepath.Join(root, path)); !os.IsNotExist(err) {
			t.Fatalf("approved volume remains: %s (%v)", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "approved-pool/unapproved")); err != nil {
		t.Fatalf("unapproved volume was not retained: %v", err)
	}
}

func TestTeardownArtifactHelperGuardsActualDeletion(t *testing.T) {
	for _, scenario := range []string{"unchanged", "tree", "replaced", "symlink", "absent", "disappeared", "added", "added-child", "during-delete", "owner-during-delete", "symlink-parent"} {
		t.Run(scenario, func(t *testing.T) {
			root := testkit.TempDir(t)
			path := filepath.Join(root, "owned")
			foreign := filepath.Join(root, "foreign")
			testkit.WriteFile(t, foreign, []byte("foreign"), 0600)
			if scenario == "tree" || scenario == "added-child" {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(path, "child"), []byte("before"), 0600)
				if err := os.Symlink(foreign, filepath.Join(path, "link")); err != nil {
					t.Fatal(err)
				}
			} else if scenario == "symlink" {
				if err := os.Symlink("before", path); err != nil {
					t.Fatal(err)
				}
			} else if scenario != "absent" && scenario != "added" {
				testkit.WriteFile(t, path, []byte("before"), 0600)
			}
			binding, err := teardownArtifactBinding(path)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := json.Marshal([]teardownArtifact{{Path: path, Binding: binding}})
			if err != nil {
				t.Fatal(err)
			}
			helper, err := filepath.Abs("../../scripts/lib/teardown-plan.py")
			if err != nil {
				t.Fatal(err)
			}
			command := exec.Command("python3", "-B", helper, "remove-artifact", path)
			switch scenario {
			case "replaced":
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				replacement := filepath.Join(root, "replacement")
				testkit.WriteFile(t, replacement, []byte("after!"), 0600)
				if err := os.Chtimes(replacement, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(replacement, path); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("after!", path); err != nil {
					t.Fatal(err)
				}
			case "disappeared":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "added":
				testkit.WriteFile(t, path, []byte("after!"), 0600)
			case "added-child":
				testkit.WriteFile(t, filepath.Join(path, "new"), []byte("after!"), 0600)
			case "symlink-parent":
				moved := root + "-moved"
				if err := os.Rename(root, moved); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Remove(root); _ = os.Rename(moved, root) })
				if err := os.Symlink(moved, root); err != nil {
					t.Fatal(err)
				}
			case "during-delete", "owner-during-delete":
				// Inject drift after the helper's tree guard, before its unlink.
				// Ownership uses synthetic stat metadata so no root is needed.
				program := `import importlib.util, json, os, sys
spec = importlib.util.spec_from_file_location("guard", sys.argv[1])
guard = importlib.util.module_from_spec(spec)
spec.loader.exec_module(guard)
original = guard.remove_entries
def changed(fd, name, entries):
    if sys.argv[3] == "during-delete":
        replacement = sys.argv[2] + ".new"
        info = os.stat(sys.argv[2])
        with open(replacement, "w") as file:
            file.write("after!")
        os.chmod(replacement, info.st_mode & 0o777)
        os.utime(replacement, ns=(info.st_atime_ns, info.st_mtime_ns))
        os.replace(replacement, sys.argv[2])
    else:
        fact = guard.artifact_fact
        def owner_drift(*args):
            result = fact(*args)
            result["UID"] += 1
            result["GID"] += 1
            return result
        guard.artifact_fact = owner_drift
    original(fd, name, entries)
guard.remove_entries = changed
try:
    guard.guard_artifact(json.loads(os.environ["SUBYARD_TEARDOWN_ARTIFACTS"])[0], remove=True)
except (ValueError, OSError):
    sys.exit(75)
`
				command = exec.Command("python3", "-B", "-c", program, helper, path, scenario)
			}
			command.Env = append(os.Environ(), "SUBYARD_TEARDOWN_ARTIFACTS="+string(payload))
			output, err := command.CombinedOutput()
			allowed := scenario == "unchanged" || scenario == "tree" || scenario == "absent" || scenario == "disappeared"
			if allowed {
				if err != nil {
					t.Fatalf("approved deletion failed: %s (%v)", output, err)
				}
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("approved artifact remains: %v", err)
				}
			} else {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != ports.TeardownPlanStaleExitCode {
					t.Fatalf("drift did not produce typed stale refusal: %s (%v)", output, err)
				}
				if _, err := os.Lstat(path); err != nil {
					t.Fatalf("changed artifact was deleted: %v", err)
				}
			}
			if _, err := os.Stat(foreign); err != nil {
				t.Fatalf("foreign artifact was deleted: %v", err)
			}
		})
	}
}

func TestTeardownArtifactBindingIncludesOwnership(t *testing.T) {
	path := filepath.Join(testkit.TempDir(t), "owned")
	testkit.WriteFile(t, path, []byte("protected"), 0600)
	current, err := teardownArtifactBinding(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"UID", "GID"} {
		command := exec.Command("python3", "-B", "-c", `import importlib.util, os, sys
spec = importlib.util.spec_from_file_location("guard", sys.argv[1])
guard = importlib.util.module_from_spec(spec)
spec.loader.exec_module(guard)
with guard.artifact_parent(sys.argv[2]) as (fd, name):
    entries = guard.artifact_entries(fd, name)
entries[0][sys.argv[3]] += 1
print(guard.artifact_digest(entries))
`, "../../scripts/lib/teardown-plan.py", path, field)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("ownership fixture: %s (%v)", output, err)
		}
		approved := &teardownSnapshot{Artifacts: []teardownArtifact{{Path: path, Binding: strings.TrimSpace(string(output))}}}
		after := &teardownSnapshot{Artifacts: []teardownArtifact{{Path: path, Binding: current}}}
		if err := checkTeardownSnapshot(approved, after); !errors.Is(err, domain.ErrPlanStale) {
			t.Fatalf("%s drift accepted: %v", field, err)
		}
	}
}

func TestTeardownStateHelperPreservesCanonicalBoundary(t *testing.T) {
	for _, scenario := range []string{"canonical", "custom", "symlink", "stale"} {
		t.Run(scenario, func(t *testing.T) {
			configHome := testkit.TempDir(t)
			path := filepath.Join(configHome, "projects")
			if scenario == "custom" {
				path = filepath.Join(configHome, "custom")
			}
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(path, "owned.json"), []byte("before"), 0600)
			if scenario == "symlink" {
				target := filepath.Join(configHome, "target")
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			binding, err := teardownArtifactBinding(path)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "stale" {
				testkit.WriteFile(t, filepath.Join(path, "new.json"), []byte("unapproved"), 0600)
			}
			payload, err := json.Marshal([]teardownArtifact{{Path: path, Binding: binding}})
			if err != nil {
				t.Fatal(err)
			}
			command := exec.Command("bash", "-c", `. "${1%/*}/engine-context.sh"
. "$1"
subyard_state_remove_canonical "$2" "$3" ''
`, "fixture", "../../scripts/lib/host.sh", path, configHome)
			command.Env = append(os.Environ(), "SUBYARD_TEARDOWN_ARTIFACTS="+string(payload))
			output, err := command.CombinedOutput()
			if scenario == "stale" {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != ports.TeardownPlanStaleExitCode {
					t.Fatalf("canonical drift was not rejected: %s (%v)", output, err)
				}
			} else if err != nil {
				t.Fatalf("canonical cleanup failed: %s (%v)", output, err)
			}
			_, err = os.Lstat(path)
			if scenario == "canonical" {
				if !os.IsNotExist(err) || strings.TrimSpace(string(output)) != "removed" {
					t.Fatalf("unchanged canonical state remains: %s (%v)", output, err)
				}
			} else if err != nil {
				t.Fatalf("protected state was removed: %v", err)
			}
		})
	}
}

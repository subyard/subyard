package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
)

func TestTeardownVolumeHelperFeedsNativeDeletionLoop(t *testing.T) {
	root := testkit.TempDir(t)
	engine, err := os.Executable()
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
	command := exec.Command(engine, "_teardown-guard", "volumes")
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
	endText := "done < <(teardown_guard volumes)"
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
teardown_guard() { "$SUBYARD_DISPATCHER_PATH" _teardown-guard "$@"; }
ok() { :; }
die() { exit 9; }
incus() {
  [ "$1" = storage ] && [ "$2" = volume ] && [ "$3" = delete ]
  rm -- "$FIXTURE_ROOT/$4/$5"
}

`
	command = exec.Command("bash", "-c", setup+string(script)[start:end+len(endText)])
	command.Env = append(os.Environ(), "SUBYARD_TEARDOWN_INVENTORY="+string(payload), "SUBYARD_DISPATCHER_PATH="+engine, "FIXTURE_ROOT="+root)
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

func TestTeardownArtifactBindingIncludesOwnership(t *testing.T) {
	path := filepath.Join(testkit.TempDir(t), "owned")
	testkit.WriteFile(t, path, []byte("protected"), 0600)
	current, err := teardownArtifactBinding(path)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := uninstall.CaptureArtifact(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"UID", "GID"} {
		entries := slices.Clone(artifact.Entries)
		if field == "UID" {
			entries[0].UID++
		} else {
			entries[0].GID++
		}
		body, err := json.Marshal(entries)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		output := hex.EncodeToString(sum[:])
		approved := &teardownSnapshot{Artifacts: []teardownArtifact{{Path: path, Binding: output}}}
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
			engine, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			command.Env = append(os.Environ(), "SUBYARD_TEARDOWN_ARTIFACTS="+string(payload), "SUBYARD_DISPATCHER_PATH="+engine)
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

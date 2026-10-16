package releaseruntime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestOptionalRuntimeQueryRequiresIndependentRollbackProof(t *testing.T) {
	for _, scenario := range []string{"failed-query", "malformed-query", "invalid-inspection", "invalid-payload", "history", "profile-owner-loss", "changed-core", "execute-recheck"} {
		t.Run(scenario, func(t *testing.T) {
			runtime, active, target, home, installed := retainedRuntimeFixture(t)
			defer runtime.Close()
			root := filepath.Dir(active)
			runtimeRoot := filepath.Join(root, "runtime")
			if err := os.MkdirAll(filepath.Join(runtimeRoot, "releases"), 0o700); err != nil {
				t.Fatal(err)
			}
			inspection := `{"schemaVersion":1,"inspection":{"plan":"plan-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible","consequences":["apply the inspected rollback"]},"outcome":{"status":"migration-required","reachedGoal":false,"active":"0.19.1-active","previous":"0.11.2-target","target":"0.11.2-target","code":"transition-required","message":"confirmation required","retry":"confirm the inspected rollback"}}}`
			if scenario == "invalid-inspection" {
				inspection = `{}`
			}
			probe := "exit 1"
			if scenario == "malformed-query" {
				probe = "printf 'malformed\\n'"
			}
			for _, candidate := range []struct{ path, id, version string }{{active, "0.19.1-active", "0.19.1"}, {target, "0.11.2-target", "0.11.2"}} {
				for _, directory := range []string{"bin", "config/preview"} {
					if err := os.MkdirAll(filepath.Join(candidate.path, directory), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				engine := fmt.Sprintf("#!/bin/sh\ncase \"${1:-}\" in\n --version) printf 'yard-engine %s\\n' ;;\n _release-transition) request=$(cat); case \"$request\" in\n *runtime-activation-capabilities*) %s ;;\n *) printf '%%s\\n' %q ;;\n esac ;;\n *) exit 64 ;;\nesac\n", candidate.version, probe, inspection)
				testkit.WriteFile(t, filepath.Join(candidate.path, "bin/yard-engine"), []byte(engine), 0o700)
				testkit.WriteFile(t, filepath.Join(candidate.path, "config/release-transition.json"), []byte("{}\n"), 0o600)
				helper := "identical public core helper"
				if scenario == "changed-core" && candidate.path == target {
					helper = "different public core helper"
				}
				testkit.WriteFile(t, filepath.Join(candidate.path, "config/preview/subyard-preview"), []byte(helper), 0o700)
				var manifest strings.Builder
				if err := filepath.WalkDir(candidate.path, func(path string, entry os.DirEntry, err error) error {
					if err != nil || entry.IsDir() {
						return err
					}
					payload, err := os.ReadFile(path)
					if err == nil {
						relative, _ := filepath.Rel(candidate.path, path)
						fmt.Fprintf(&manifest, "%x  ./%s\n", sha256.Sum256(payload), relative)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(candidate.path, "runtime-files.sha256"), []byte(manifest.String()), 0o600)
				if scenario == "invalid-payload" && candidate.path == target {
					testkit.WriteFile(t, filepath.Join(candidate.path, "bin/yard-engine"), []byte(engine+"\n"), 0o700)
				}
				if err := os.Rename(candidate.path, filepath.Join(runtimeRoot, "releases", candidate.id)); err != nil {
					t.Fatal(err)
				}
			}
			for name, id := range map[string]string{"current": "0.19.1-active", "previous": "0.11.2-target"} {
				if err := os.Symlink("releases/"+id, filepath.Join(runtimeRoot, name)); err != nil {
					t.Fatal(err)
				}
			}
			runtime.config.InstalledRuntimePresence = func(context.Context, string, config.Loaded, string) (ports.RuntimePresence, error) {
				return ports.RuntimePresence{Actual: "installed", Preview: true}, nil
			}
			if scenario == "profile-owner-loss" {
				testkit.WriteFile(t, installed, []byte("installed profile runtime"), 0o700)
			}
			if scenario == "history" {
				path := filepath.Join(home, "release-transition/v2/history-checkpoint.json")
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, []byte("{invalid"), 0o600)
			}
			prepared, err := runtime.PrepareTransition(context.Background(), []string{"--rollback", "--runtime-root", runtimeRoot}, home, "default")
			want := scenario == "failed-query" || scenario == "malformed-query" || scenario == "execute-recheck"
			if (err == nil) != want {
				t.Fatalf("independent rollback proof: prepared=%v err=%v", prepared.Changed, err)
			}
			if scenario == "execute-recheck" {
				testkit.WriteFile(t, installed, []byte("new installed profile runtime"), 0o700)
				if err := prepared.Execute(context.Background()); !errors.Is(err, domain.ErrPlanStale) {
					t.Fatalf("execution did not recheck runtime-owner loss: %v", err)
				}
			}
			for name, expected := range map[string]string{"current": "releases/0.19.1-active", "previous": "releases/0.11.2-target"} {
				if got, err := os.Readlink(filepath.Join(runtimeRoot, name)); err != nil || got != expected {
					t.Fatalf("inspection changed %s: %q, %v", name, got, err)
				}
			}
		})
	}
}

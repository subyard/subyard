package github_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

// Reconcile semantics belong to git-wiring-test.py. This regression exercises
// the real loader, binding, shell adapter and producer up to the Incus boundary.
func TestProvisionTransportsSelectedGitMode(t *testing.T) {
	t.Setenv("TMPDIR", "/tmp")
	root, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	shim, err := os.ReadFile("provision-incus.py")
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"shipped", "yard", "command"} {
		for _, mode := range []string{"off", "broker-first", "broker-last"} {
			if scope == "shipped" && mode != "off" {
				continue
			}
			t.Run(scope+"/"+mode, func(t *testing.T) {
				home := testkit.TempDir(t)
				configHome := filepath.Join(home, "settings")
				yardFile := filepath.Join(configHome, "yards/default/config.env")
				if err := os.MkdirAll(filepath.Dir(yardFile), 0o700); err != nil {
					t.Fatal(err)
				}
				settings := "ENVIRONMENT_PROFILES=github\nCODING_TOOL_INTEGRATIONS=\n"
				if scope == "yard" {
					settings += "GITHUB_GIT_MODE=" + mode + "\n"
				}
				if scope == "command" {
					persisted := "broker-last"
					if mode == "broker-last" {
						persisted = "off"
					}
					settings += "GITHUB_GIT_MODE=" + persisted + "\n"
				}
				testkit.WriteFile(t, yardFile, []byte(settings), 0o600)
				environment := map[string]string{
					"SUBYARD_CONFIG_HOME": configHome,
					"SUBYARD_HOME":        filepath.Join(home, "data"),
					"UNDECLARED_SECRET":   "must-not-reach-guest",
				}
				if scope == "command" {
					environment["GITHUB_GIT_MODE"] = mode
				}
				loaded, err := config.Load(config.LoadOptions{
					RepositoryRoot: root, OperatorHome: home, DisablePrivate: true, Environment: environment,
				})
				if err != nil {
					t.Fatal(err)
				}
				if got := loaded.Environment["GITHUB_GIT_MODE"]; got != mode {
					t.Fatalf("loaded mode=%q want=%q", got, mode)
				}
				pkg, err := profile.ResolvePackage(root, "github")
				if err != nil {
					t.Fatal(err)
				}
				binding, err := config.ProfileProvisionBinding(pkg, loaded.Environment)
				if err != nil {
					t.Fatal(err)
				}
				metadata, err := json.Marshal(map[string]any{"input": map[string]any{"profileBinding": binding}})
				if err != nil {
					t.Fatal(err)
				}
				metadataPath := filepath.Join(home, "metadata.json")
				testkit.WriteFile(t, metadataPath, metadata, 0o600)
				bin := filepath.Join(home, "bin")
				if err := os.Mkdir(bin, 0o700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(bin, "incus"), shim, 0o700)
				for _, action := range []string{"check", "apply"} {
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					args := []string{filepath.Join(root, "scripts/provision-profile.sh")}
					if action == "check" {
						args = append(args, "--check")
					}
					cmd := exec.CommandContext(ctx, "bash", append(args, "github")...)
					// The selected mode must come from metadata, never ambient env.
					for name, value := range loaded.Environment {
						if name != "GITHUB_GIT_MODE" && name != "PATH" {
							cmd.Env = append(cmd.Env, name+"="+value)
						}
					}
					cmd.Env = append(cmd.Env, "PATH="+bin+":"+os.Getenv("PATH"), "TMPDIR=/tmp",
						"SUBYARD_ENGINE_CONTEXT=1", "SUBYARD_ENGINE_CONTEXT_SCHEMA=1",
						"SUBYARD_METADATA_FD=3", "EXPECTED_MODE="+mode, "EXPECTED_ACTION="+action)
					fd, err := os.Open(metadataPath)
					if err != nil {
						t.Fatal(err)
					}
					cmd.ExtraFiles = []*os.File{fd}
					output, runErr := cmd.CombinedOutput()
					fd.Close()
					if runErr != nil {
						t.Fatalf("%s: %v\n%s", action, runErr, output)
					}
					if action == "check" && strings.TrimSpace(string(output)) != "converged" {
						t.Fatalf("check output=%q", output)
					}
				}
			})
		}
	}
}

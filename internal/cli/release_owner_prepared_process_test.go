package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releaseowner"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
	"golang.org/x/sys/unix"
)

// This is a local process fixture, not physical Incus/systemd acceptance. The
// actual Rust owner and production helper execute one fixed installed component
// from the immutable recipe; only its owning wrapper payload is synthetic.
func TestRustPreparedComponentOwnerAndHelperLoss(t *testing.T) {
	owner, helper, corpusPath := os.Getenv("SUBYARD_RUST_OWNER_EXECUTABLE"), os.Getenv("SUBYARD_RUST_NATIVE_HELPER"), os.Getenv("SUBYARD_RUST_RUNTIME_CORPUS")
	if owner == "" || helper == "" || corpusPath == "" {
		t.Skip("paired actual Rust/native local process fixture")
	}
	var corpus struct {
		Registry string                                   `json:"registry"`
		Catalog  []releasetransition.CapabilityDescriptor `json:"catalog"`
	}
	raw, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"normal", "owner-loss", "helper-loss"} {
		t.Run(mode, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			home, runtimeRoot, dataHome := filepath.Join(root, "state"), filepath.Join(root, "runtime"), filepath.Join(root, "data")
			if artifact := os.Getenv("SUBYARD_NATIVE_PROCESS_ARTIFACT"); artifact != "" {
				t.Cleanup(func() {
					names, _ := filepath.Glob(filepath.Join(home, "migrations/native-custody/v1/*/*.json"))
					for _, path := range names {
						raw, err := os.ReadFile(path)
						if err != nil {
							t.Error(err)
							continue
						}
						name := filepath.Join(artifact, mode, filepath.Base(filepath.Dir(path))+"-"+filepath.Base(path))
						if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
							t.Error(err)
							continue
						}
						if err := os.WriteFile(name, raw, 0600); err != nil {
							t.Error(err)
						}
					}
				})
			}
			tools := filepath.Join(dataHome, "tools")
			for _, path := range []string{home, dataHome, filepath.Join(tools, "bin"), filepath.Join(runtimeRoot, "releases/old"), filepath.Join(runtimeRoot, "releases/new"), filepath.Join(home, ".sync/settings")} {
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink("releases/old", filepath.Join(runtimeRoot, "current")); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(home, ".sync/settings/config.env"), []byte("SSH_PORT=2240\n"), 0600)
			testkit.WriteFile(t, filepath.Join(root, "config/projects-changed.sh"), []byte("#!/bin/sh\nexit 0\n"), 0644)
			public := repositoryRoot(t)
			paths := []string{"scripts/install-keys-auto-sync.sh", "config/systemd/subyard-keys-sync.service.in", "config/systemd/subyard-keys-sync.timer.in", "scripts/lib-power.sh"}
			entries, err := os.ReadDir(filepath.Join(public, "scripts/lib"))
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if !entry.IsDir() && !strings.HasSuffix(entry.Name(), ".pyc") {
					paths = append(paths, "scripts/lib/"+entry.Name())
				}
			}
			for _, relative := range paths {
				body, err := os.ReadFile(filepath.Join(public, relative))
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(root, relative)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, body, 0755)
			}
			script := `#!/bin/bash
set -euo pipefail
if [ "${1:-}" = --check ]; then [ -f "$SUBYARD_HOME/effect" ]; exit; fi
[ "${SUBYARD_NATIVE_OWNER_FD3:-}" = 1 ] && [ -f /proc/self/fd/3 ]
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
. "$SCRIPT_DIR/lib/native-installed-stage.sh"
printf '%s\n' "$BASHPID" >"$SUBYARD_HOME/child.pid"
: >"$SUBYARD_HOME/ready"
while [ ! -f "$SUBYARD_HOME/release" ]; do sleep .01; done
printf approved >"$SUBYARD_HOME/effect"
printf x >>"$SUBYARD_HOME/count"
`
			testkit.WriteFile(t, filepath.Join(root, "scripts/install-key-tools.sh"), []byte(script), 0755)
			ready, release, effect, count, childPID := filepath.Join(dataHome, "ready"), filepath.Join(dataHome, "release"), filepath.Join(dataHome, "effect"), filepath.Join(dataHome, "count"), filepath.Join(dataHome, "child.pid")
			server, err := testkit.NewIncusServer(testkit.TempDir(t))
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			environment = append(environment, "PATH=/usr/local/bin:/usr/bin:/bin", "SUBYARD_INCUS_SOCKET="+server.SocketPath, "SUBYARD_POWER_RECONCILER_PATH="+filepath.Join(dataHome, "absent-power"), "SUBYARD_POWER_UNIT_PATH="+filepath.Join(dataHome, "absent-unit"), "SUBYARD_KEYS_TOOLS_DIR="+tools, "READY="+ready, "RELEASE="+release, "EFFECT="+effect, "COUNT="+count, "CHILD_PID="+childPID)
			direct, err := New(Options{RepositoryRoot: root, Environment: environment})
			if err != nil {
				t.Fatal(err)
			}
			configFD, err := os.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			defer configFD.Close()
			c := &releaseOwnerComponents{cli: direct, context: releaseowner.Context{Session: strings.Repeat("b", 64), DataHome: dataHome, ConfigHome: home, RuntimeRoot: runtimeRoot, RepositoryRoot: root, PhysicalRepositoryRoot: root, Target: "new", Artifact: strings.Repeat("a", 64), Yard: "default", Releases: releasetransition.ReleasePair{From: "old", Target: "new"}, Direction: releasetransition.DirectionActivateTarget}, root: configFD}
			if _, _, err := c.CaptureInventory(context.Background()); err != nil {
				t.Fatal("owning inventory", err)
			}
			targets, err := c.CaptureActivation(context.Background(), "key-tools-runtime", false)
			if err != nil || len(targets) != 1 {
				t.Fatal("owning prepared key component", err)
			}
			target := targets[0]
			recipeRaw, err := c.InstalledRuntimeRecipe(target.PreparedRecipe)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.BindInstalledRuntimeRecipe(context.Background(), strings.Repeat("f", 64), recipeRaw); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatal("wrong recipe SHA admitted", err)
			}
			key := target.Owner + ":" + target.Yard + ":" + target.Class
			original := c.activation[key]
			mismatch := original
			mismatch.value.PreparedRecipe = strings.Repeat("f", 64)
			c.activation[key] = mismatch
			if err := c.BindInstalledRuntimeRecipe(context.Background(), target.PreparedRecipe, recipeRaw); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatal("unapproved target/recipe binding admitted", err)
			}
			c.activation[key] = original
			if err := c.BindInstalledRuntimeRecipe(context.Background(), target.PreparedRecipe, recipeRaw); err != nil {
				t.Fatal("original native bind", err)
			}
			server.SetInstance(target.Project, target.Instance, map[string]any{"name": target.Instance, "status": "Running", "config": map[string]any{"volatile.uuid": "replacement-uuid"}})
			if err := c.ApplyActivation(context.Background(), target); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatal("absent original guest replaced before command", err)
			}
			if _, err := os.Stat(ready); !os.IsNotExist(err) {
				t.Fatal("UUID replacement reached command", err)
			}
			server.ReplaceInstances(map[string]map[string]any{})
			if destination := os.Getenv("SUBYARD_RUST_INSTALLED_RECIPE_CORPUS"); destination != "" && mode == "normal" {
				appendCapturedRecipeCorpus(t, destination, target, recipeRaw)
			}
			c.Close()
			options := map[string]any{"dataHome": dataHome, "configHome": home, "runtimeRoot": runtimeRoot, "repositoryRoot": root, "yard": "default", "releases": map[string]string{"from": "old", "target": "new"}, "artifactDigest": strings.Repeat("a", 64), "registryRaw": []byte(corpus.Registry), "catalog": corpus.Catalog, "inheritedSettingIds": []string{}, "rollbackTarget": nil, "migrationCheckpoint": false, "sourceIngress": nil}
			child := startRustOwnerWithBudget(t, owner, helper, environment, root, false, 90*time.Second)
			defer child.close()
			if reply := child.exchange(map[string]any{"op": "open", "options": options}); reply.Error != "" {
				t.Fatal("open", reply.Error)
			}
			inspected := child.exchange(map[string]any{"op": "inspect", "goal": map[string]string{"target": "new", "direction": "activate-target"}})
			if inspected.Error != "" || inspected.Inspection == nil || len(inspected.Inspection.Blockers) != 0 {
				t.Fatalf("inspection refused: %#v blockers=%+v", inspected, inspected.Inspection.Blockers)
			}
			plan := inspected.Inspection.Plan
			if err := child.encoder.Encode(map[string]any{"op": "execute", "plan": plan, "authorization": "fixture-private-consent"}); err != nil {
				t.Fatal(err)
			}
			child.admit(plan, "")
			type terminalReply struct {
				reply rustOwnerReply
				err   error
			}
			finished := make(chan terminalReply, 1)
			go func() {
				var reply rustOwnerReply
				err := child.decoder.Decode(&reply)
				finished <- terminalReply{reply, err}
			}()
			waitPreparedProcess(t, func() bool {
				select {
				case terminal := <-finished:
					t.Fatalf("owner returned before active component: reply=%+v outcome=%+v err=%v", terminal.reply, terminal.reply.Outcome, terminal.err)
				default:
				}
				_, err := os.Stat(ready)
				return err == nil
			}, "actual cached native wrapper")
			pidRaw, err := os.ReadFile(childPID)
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(pidRaw)))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { syscall.Kill(-pid, syscall.SIGKILL) })
			facts, err := filepath.Glob(filepath.Join(home, "migrations/native-custody/v1/*/*command-001-intent.json"))
			if err != nil || len(facts) == 0 {
				t.Fatal("missing durable native command intent", err)
			}
			if mode != "normal" {
				if mode == "helper-loss" {
					raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", child.cmd.Process.Pid, child.cmd.Process.Pid))
					if err != nil {
						t.Fatal(err)
					}
					pids := strings.Fields(string(raw))
					if len(pids) != 1 {
						t.Fatal("unexpected helper process set", len(pids))
					}
					helperPID, err := strconv.Atoi(pids[0])
					if err != nil {
						t.Fatal(err)
					}
					if err := syscall.Kill(helperPID, syscall.SIGKILL); err != nil {
						t.Fatal(err)
					}
				}
				child.cmd.Process.Kill()
				child.cmd.Wait()
				child.socket.Close()
				child.custody.Close()
				if !preparedOwnerBusy(t, dataHome) {
					t.Fatal("surviving native work lost original account OFD")
				}
			}
			testkit.WriteFile(t, release, nil, 0600)
			if mode == "normal" {
				terminal := <-finished
				reply := terminal.reply
				if terminal.err != nil {
					t.Fatal(terminal.err)
				}
				if reply.Error != "" || reply.Outcome == nil || reply.Outcome.Status != releasetransition.StatusReady {
					t.Fatalf("normal active component: %#v outcome=%+v", reply, reply.Outcome)
				}
				// The live Rust session legitimately retains its account OFD
				// until the outer operation closes after consuming Ready.
				child.close()
			}
			waitPreparedProcess(t, func() bool { return !preparedOwnerBusy(t, dataHome) }, "native account custody drain")
			if body, err := os.ReadFile(effect); err != nil || string(body) != "approved" {
				t.Fatal("approved payload did not finish", err)
			}
			if mode == "normal" {
				return
			}
			fresh := startRustOwnerWithBudget(t, owner, helper, environment, root, false, 90*time.Second)
			defer fresh.close()
			if reply := fresh.exchange(map[string]any{"op": "open", "options": options}); reply.Error != "" {
				t.Fatal("fresh open", reply.Error)
			}
			reply := fresh.exchange(map[string]any{"op": "resume"})
			if reply.Error != "" || reply.Outcome == nil {
				t.Fatalf("fresh reply: %#v", reply)
			}
			if mode == "owner-loss" && reply.Outcome.Status != releasetransition.StatusReady {
				t.Fatalf("closed native resume: %#v", reply)
			}
			if mode == "helper-loss" && reply.Outcome.Status == releasetransition.StatusReady {
				t.Fatal("lost opaque command became Ready")
			}
			if body, err := os.ReadFile(count); err != nil || string(body) != "x" {
				t.Fatal("native command replayed", err)
			}
		})
	}
}

func waitPreparedProcess(t *testing.T, condition func() bool, label string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("deadline: " + label)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func preparedOwnerBusy(t *testing.T, home string) bool {
	t.Helper()
	fd, err := unix.Open(uninstall.OwnerLockPath(home), unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		return false
	}
	if !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatal(err)
	}
	return true
}

func appendCapturedRecipeCorpus(t *testing.T, destination string, target releaseowner.Target, raw []byte) {
	t.Helper()
	type sample struct {
		Name   string               `json:"name"`
		Raw    []byte               `json:"raw"`
		SHA    string               `json:"sha256"`
		Target *releaseowner.Target `json:"target,omitempty"`
	}
	rows := []sample{}
	if original, err := os.ReadFile(destination); err == nil {
		if err := json.Unmarshal(original, &rows); err != nil {
			t.Fatal(err)
		}
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	rows = append(rows, sample{"actual-captured-" + target.Owner + "-" + target.Class + "-target", raw, target.PreparedRecipe, &target})
	encoded, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, destination, encoded, 0600)
}

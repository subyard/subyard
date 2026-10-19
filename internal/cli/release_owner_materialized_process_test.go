package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

// Real Rust/helper processes and SDK transport, with a fixed canned Incus
// server and marked wrapper. This proves no physical guest or host lifecycle.
func TestRustMaterializedOriginalBundleAndOpaqueLoss(t *testing.T) {
	owner, helper, corpusPath := os.Getenv("SUBYARD_RUST_OWNER_EXECUTABLE"), os.Getenv("SUBYARD_RUST_NATIVE_HELPER"), os.Getenv("SUBYARD_RUST_RUNTIME_CORPUS")
	if owner == "" || helper == "" || corpusPath == "" {
		t.Skip("paired original materialized payload fixture")
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
	for _, mode := range []string{"fresh-bundle", "source-drift", "helper-loss"} {
		t.Run(mode, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			home, data, runtimeRoot := filepath.Join(root, "state"), filepath.Join(root, "data"), filepath.Join(root, "runtime")
			for _, path := range []string{home, data, filepath.Join(runtimeRoot, "releases/old"), filepath.Join(runtimeRoot, "releases/new"), filepath.Join(home, ".sync/settings")} {
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink("releases/old", filepath.Join(runtimeRoot, "current")); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(home, ".sync/settings/config.env"), []byte("SSH_PORT=2240\n"), 0o600)
			nativeMaterializedSourceFiles(t, root)
			for _, relative := range []string{"scripts/lib/native-installed-stage.sh", "scripts/lib/native-installed-stage.py", "scripts/lib/native-owner-barrier.py"} {
				body, err := os.ReadFile(filepath.Join(repositoryRoot(t), relative))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(filepath.Join(root, relative)), 0o700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(root, relative), body, 0o755)
			}
			wrapper := `#!/bin/bash
set -euo pipefail
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
. "$SCRIPT_DIR/lib/native-installed-stage.sh"
printf '%s\n' "$BASHPID" >"$SUBYARD_HOME/child.pid"
: >"$SUBYARD_HOME/ready"
while [ ! -f "$SUBYARD_HOME/release" ]; do sleep .01; done
printf approved >"$SUBYARD_HOME/integration-effect"
printf x >>"$SUBYARD_HOME/integration-count"
`
			testkit.WriteFile(t, filepath.Join(root, "scripts/reconcile-integrations.sh"), []byte(wrapper), 0o755)
			server, err := testkit.NewIncusServer(testkit.TempDir(t))
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			server.SetExtensions("projects", "instances", "instance_all_projects", "container_exec_user_group_cwd", "storage", "network")
			server.SetProject("subyard", map[string]any{"name": "subyard", "config": map[string]string{}})
			server.SetInstance("subyard", "yard", map[string]any{"name": "yard", "status": "Running", "config": map[string]string{"volatile.uuid": "synthetic-instance-uuid", "user.subyard.managed": "true", "user.subyard.initialized": "true", "user.subyard.name": "default"}, "expanded_config": map[string]string{"volatile.uuid": "synthetic-instance-uuid", "user.subyard.managed": "true", "user.subyard.initialized": "true", "user.subyard.name": "default"}})
			state := &nativeMaterializedState{files: map[string][]byte{}}
			server.SetExecResponder(state.step)
			environment = append(environment, "PATH=/usr/local/bin:/usr/bin:/bin", "SUBYARD_INCUS_SOCKET="+server.SocketPath, "SUBYARD_POWER_RECONCILER_PATH="+filepath.Join(data, "absent-power"), "SUBYARD_POWER_UNIT_PATH="+filepath.Join(data, "absent-unit"))
			program, err := New(Options{RepositoryRoot: root, Environment: environment})
			if err != nil {
				t.Fatal(err)
			}
			fd, err := os.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			defer fd.Close()
			probe := &releaseOwnerComponents{cli: program, root: fd, context: releaseowner.Context{Session: strings.Repeat("b", 64), Artifact: strings.Repeat("a", 64), Releases: releasetransition.ReleasePair{From: "old", Target: "new"}, Direction: releasetransition.DirectionActivateTarget, Target: "new", RepositoryRoot: root, PhysicalRepositoryRoot: root, ConfigHome: home, DataHome: data, RuntimeRoot: runtimeRoot, Yard: "default"}}
			if _, _, err := probe.CaptureInventory(t.Context()); err != nil {
				t.Fatal("owning inventory", err)
			}
			for _, owner := range []string{"materialized-config", "core-guest-runtime", "key-tools-runtime"} {
				if _, err := probe.CaptureActivation(t.Context(), owner, false); err != nil {
					t.Fatalf("controlled owning fixture preflight %s: %T %v", owner, err, err)
				}
			}
			probe.Close()
			options := map[string]any{"dataHome": data, "configHome": home, "runtimeRoot": runtimeRoot, "repositoryRoot": root, "yard": "default", "releases": map[string]string{"from": "old", "target": "new"}, "artifactDigest": strings.Repeat("a", 64), "registryRaw": []byte(corpus.Registry), "catalog": corpus.Catalog, "inheritedSettingIds": []string{}, "rollbackTarget": nil, "migrationCheckpoint": false, "sourceIngress": nil}
			first := startRustOwnerWithBudget(t, owner, helper, environment, root, mode != "helper-loss", 120*time.Second)
			defer first.close()
			if reply := first.exchange(map[string]any{"op": "open", "options": options}); reply.Error != "" {
				t.Fatal("open", reply.Error)
			}
			inspection := first.exchange(map[string]any{"op": "inspect", "goal": map[string]string{"target": "new", "direction": "activate-target"}}).Inspection
			if inspection == nil || len(inspection.Blockers) != 0 {
				t.Fatalf("inspection unavailable: %+v", inspection)
			}
			if err := first.encoder.Encode(map[string]any{"op": "execute", "plan": inspection.Plan, "authorization": "fixture-private-consent"}); err != nil {
				t.Fatal(err)
			}
			first.admit(inspection.Plan, "")
			if mode != "helper-loss" {
				reader := bufio.NewReader(first.control)
				for n := 0; n < 128; n++ {
					first.control.SetReadDeadline(time.Now().Add(20 * time.Second))
					phase, err := reader.ReadString('\n')
					if err != nil {
						t.Fatalf("original bundle cut: %v (%s)", err, first.stderr.String())
					}
					if strings.TrimSpace(phase) == "after-authorized-journal" {
						break
					}
					if _, err := first.control.Write([]byte{'R'}); err != nil {
						t.Fatal(err)
					}
					if n == 127 {
						t.Fatal("cut bound")
					}
				}
			} else {
				waitPreparedProcess(t, func() bool { _, err := os.Stat(filepath.Join(data, "ready")); return err == nil }, "original integration transport")
				pids, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", first.cmd.Process.Pid, first.cmd.Process.Pid))
				if err != nil {
					t.Fatal(err)
				}
				children := strings.Fields(string(pids))
				if len(children) != 1 {
					t.Fatal("unexpected helper set")
				}
				pid, err := strconv.Atoi(children[0])
				if err != nil {
					t.Fatal(err)
				}
				if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
				if !preparedOwnerBusy(t, data) {
					t.Fatal("opaque foreground lost original owner OFD")
				}
			}
			first.cmd.Process.Kill()
			first.cmd.Wait()
			first.socket.Close()
			first.custody.Close()
			if first.control != nil {
				first.control.Close()
			}
			if mode == "source-drift" {
				testkit.WriteFile(t, filepath.Join(root, "config/agents/sample/rules"), []byte("external changed source\n"), 0o600)
			}
			testkit.WriteFile(t, filepath.Join(data, "release"), nil, 0o600)
			waitPreparedProcess(t, func() bool { return !preparedOwnerBusy(t, data) }, "original custody release")
			fresh := startRustOwnerWithBudget(t, owner, helper, environment, root, false, 120*time.Second)
			defer fresh.close()
			if reply := fresh.exchange(map[string]any{"op": "open", "options": options}); reply.Error != "" {
				t.Fatal("fresh open", reply.Error)
			}
			reply := fresh.exchange(map[string]any{"op": "resume"})
			if mode == "fresh-bundle" {
				if reply.Error != "" || reply.Outcome == nil || reply.Outcome.Status != releasetransition.StatusReady {
					t.Fatalf("fresh original bundle: %+v outcome=%+v", reply, reply.Outcome)
				}
				if body, err := os.ReadFile(filepath.Join(data, "integration-count")); err != nil || string(body) != "x" {
					t.Fatal("original integration did not run once", err)
				}
			} else if reply.Outcome != nil && reply.Outcome.Status == releasetransition.StatusReady {
				t.Fatal("unproved source/lost command became Ready")
			}
			if mode == "source-drift" {
				state.mu.Lock()
				writes, committed := state.writes, state.committed
				state.mu.Unlock()
				if writes != 0 || committed {
					t.Fatal("source drift reached guest mutation")
				}
				if _, err := os.Stat(filepath.Join(data, "integration-count")); !os.IsNotExist(err) {
					t.Fatal("source drift ran integration")
				}
			}
			if mode == "helper-loss" {
				if body, err := os.ReadFile(filepath.Join(data, "integration-count")); err != nil || string(body) != "x" {
					t.Fatal("unknown command replayed", err)
				}
			}
		})
	}
}

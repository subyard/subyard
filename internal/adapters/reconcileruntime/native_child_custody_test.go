package reconcileruntime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
)

type custodyTransportObserver struct{}

func (custodyTransportObserver) Before(ports.NativeOperation) error            { return nil }
func (custodyTransportObserver) Accepted(ports.NativeOperation) error          { return nil }
func (custodyTransportObserver) Terminal(ports.NativeOperation) error          { return nil }
func (custodyTransportObserver) Uncertain()                                    {}
func (custodyTransportObserver) CommandStarted(string, string) (uint64, error) { return 1, nil }
func (custodyTransportObserver) CommandTerminal(uint64) error                  { return nil }

func TestNativeFixedTransportRetainsOriginalOwnerAfterHelperDeath(t *testing.T) {
	if mode := os.Getenv("SUBYARD_NATIVE_CHILD_CUSTODY"); mode != "" {
		home := os.Getenv("SUBYARD_NATIVE_CHILD_HOME")
		_, release, err := uninstall.OwnerLock(context.Background(), home, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		runtime := Runtime{RepositoryRoot: home, Environment: append(os.Environ(), "SUBYARD_HOME="+home), Stderr: io.Discard, Yard: domain.Context{Paths: domain.RuntimePaths{DataHome: home}}}
		ctx := ports.WithNativeOperationObserver(context.Background(), custodyTransportObserver{})
		if mode == "component" {
			err = runtime.runPath(ctx, io.Discard, filepath.Join(home, "handler.sh"))
		} else {
			_, _, err = runtime.runShippedHandler(ctx, profile.Definition{Root: home}, "handler.sh", runtime.Environment, time.Minute, "apply")
		}
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	helper, err := filepath.Abs("../../../scripts/lib/native-owner-barrier.py")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"component", "profile"} {
		t.Run(mode, func(t *testing.T) {
			home := testkit.TempDir(t)
			script := `#!/bin/sh
set -eu
python3 -B -c 'import importlib.util,sys;s=importlib.util.spec_from_file_location("barrier",sys.argv[1]);m=importlib.util.module_from_spec(s);s.loader.exec_module(m);assert m.inherited_owner_descriptors()==(3,)' "$BARRIER_HELPER"
: >"$READY"
while [ ! -f "$RELEASE" ]; do sleep .01; done
`
			testkit.WriteFile(t, filepath.Join(home, "handler.sh"), []byte(script), 0o755)
			ready, release := filepath.Join(home, "ready"), filepath.Join(home, "release")
			child := exec.Command(os.Args[0], "-test.run=^TestNativeFixedTransportRetainsOriginalOwnerAfterHelperDeath$")
			child.Env = append(os.Environ(), "SUBYARD_NATIVE_CHILD_CUSTODY="+mode, "SUBYARD_NATIVE_CHILD_HOME="+home, "BARRIER_HELPER="+helper, "READY="+ready, "RELEASE="+release)
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { os.WriteFile(release, nil, 0o600); child.Process.Kill(); child.Wait() }()
			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, err := os.Stat(ready); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("fixed child did not start")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := child.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			child.Wait()
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			_, unlock, err := uninstall.OwnerLock(ctx, home, nil)
			cancel()
			if unlock != nil {
				unlock()
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("surviving native child lost original owner OFD", err)
			}
			testkit.WriteFile(t, release, nil, 0o600)
			ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, unlock, err = uninstall.OwnerLock(ctx, home, nil)
			if err != nil {
				t.Fatal("child did not release original owner OFD", err)
			}
			unlock()
		})
	}
}

func TestPreparedInstalledStageHelperAndGroupLoss(t *testing.T) {
	if mode := os.Getenv("SUBYARD_INSTALLED_STAGE_CHILD"); mode != "" {
		root := os.Getenv("SUBYARD_INSTALLED_STAGE_ROOT")
		home := os.Getenv("SUBYARD_INSTALLED_STAGE_HOME")
		runtime := Runtime{RepositoryRoot: root, Yard: domain.Context{Paths: domain.RuntimePaths{DataHome: home, ConfigHome: home, OperatorHome: home}}, Environment: []string{"SUBYARD_KEYS_TOOLS_DIR=" + filepath.Join(home, "tools")}}
		p, err := runtime.PrepareInstalledRuntime(context.Background(), "key-tools-runtime")
		if err != nil {
			t.Fatal(err)
		}
		p.stageBinding = os.Getenv("SUBYARD_INSTALLED_STAGE_KEY")
		if err := p.materialize(); err != nil {
			t.Fatal(err)
		}
		child := exec.Command("bash", filepath.Join(p.directory, "scripts/install-key-tools.sh"))
		child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		child.Env = append(os.Environ(), "SUBYARD_PREPARED_INSTALLED_CLEANUP=1", "SUBYARD_PREPARED_INSTALLED_STAGE="+p.directory, "SUBYARD_PREPARED_INSTALLED_BINDING="+p.stageBinding, "SUBYARD_PREPARED_INSTALLED_RECIPE="+p.binding, "SUBYARD_PREPARED_INSTALLED_OWNER="+p.owner)
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(struct {
			PID   int    `json:"pid"`
			Stage string `json:"stage"`
		}{child.Process.Pid, p.directory})
		testkit.WriteFile(t, filepath.Join(home, "child.json"), raw, 0o600)
		child.Wait()
		return
	}
	for _, mode := range []string{"helper-only", "group-loss", "nonzero"} {
		t.Run(mode, func(t *testing.T) {
			runtime := preparedInstalledFixture(t)
			root, home := runtime.RepositoryRoot, runtime.Yard.Paths.DataHome
			script := `#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "$SCRIPT_DIR/lib/native-installed-stage.sh"
: >"$READY"
while [ ! -f "$RELEASE" ]; do sleep .01; done
exit "$RESULT"
`
			testkit.WriteFile(t, filepath.Join(root, "scripts/install-key-tools.sh"), []byte(script), 0o755)
			key := installedRuntimeDigest(home)
			ready, release := filepath.Join(home, "ready"), filepath.Join(home, "release")
			result := 0
			if mode == "nonzero" {
				result = 2
			}
			child := exec.Command(os.Args[0], "-test.run=^TestPreparedInstalledStageHelperAndGroupLoss$")
			child.Env = append(os.Environ(), "SUBYARD_INSTALLED_STAGE_CHILD="+mode, "SUBYARD_INSTALLED_STAGE_ROOT="+root, "SUBYARD_INSTALLED_STAGE_HOME="+home, "SUBYARD_INSTALLED_STAGE_KEY="+key, "READY="+ready, "RELEASE="+release, "RESULT="+strconv.Itoa(result))
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			var info struct {
				PID   int    `json:"pid"`
				Stage string `json:"stage"`
			}
			deadline := time.Now().Add(10 * time.Second)
			for {
				raw, _ := os.ReadFile(filepath.Join(home, "child.json"))
				if json.Unmarshal(raw, &info) == nil {
					if _, err := os.Stat(ready); err == nil {
						break
					}
				}
				if time.Now().After(deadline) {
					t.Fatal("prepared child did not start")
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Cleanup(func() {
				syscall.Kill(-info.PID, syscall.SIGKILL)
				child.Process.Kill()
				child.Wait()
				filepath.WalkDir(info.Stage, func(path string, e os.DirEntry, err error) error {
					if err == nil && e.IsDir() {
						os.Chmod(path, 0o700)
					}
					return nil
				})
				os.RemoveAll(info.Stage)
			})
			child.Process.Kill()
			child.Wait()
			if mode == "group-loss" {
				syscall.Kill(-info.PID, syscall.SIGKILL)
			} else {
				testkit.WriteFile(t, release, nil, 0o600)
			}
			if mode == "helper-only" {
				deadline = time.Now().Add(5 * time.Second)
				for {
					if _, err := os.Stat(info.Stage); os.IsNotExist(err) {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("drained surviving wrapper retained its stage")
					}
					time.Sleep(10 * time.Millisecond)
				}
			} else {
				time.Sleep(100 * time.Millisecond)
				if _, err := os.Stat(info.Stage); err != nil {
					t.Fatal("unknown/nonzero command stage was removed", err)
				}
			}
		})
	}
}

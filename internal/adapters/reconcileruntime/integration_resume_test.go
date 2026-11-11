package reconcileruntime

import (
	"bytes"
	"context"
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

// Keep the real inventory protocol and bytes; only the guest transport and
// opaque lifecycle commands are substituted. All writes stay in /tmp.
type interruptedInventoryExecutor struct {
	configInventoryFixture
	retryableIntegrationExecutor
	cut       string
	committed bool
}

func (f *interruptedInventoryExecutor) Exec(ctx context.Context, project, instance string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	point := ""
	var result ports.InstanceExecResult
	var err error
	if len(request.Command) >= 5 && request.Command[0] == "python3" && request.Command[3] == IntegrationInventoryReader() {
		point = request.Command[4]
		if point == "observe-applied" && f.committed {
			point = "verify"
		}
		mode := request.Command[4]
		// Relocate source-owned absolute destinations without altering digests.
		request.Stdin = bytes.ReplaceAll(request.Stdin, []byte("/usr/local/libexec/subyard/"), []byte(f.home+"/"))
		request.Stdin = bytes.ReplaceAll(request.Stdin, []byte("/etc/subyard/"), []byte(f.home+"/"))
		var input map[string]any
		_ = json.Unmarshal(request.Stdin, &input)
		input["home"], input["uid"] = f.home, os.Getuid()
		request.Stdin, _ = json.Marshal(input)
		command := exec.CommandContext(ctx, "python3", "-B", "-c", f.program, mode)
		command.Stdin = bytes.NewReader(request.Stdin)
		result.Stdout, err = command.Output()
		var failure *exec.ExitError
		if errors.As(err, &failure) {
			result.ExitCode, result.Stderr = failure.ExitCode(), failure.Stderr
		}
	} else {
		result, err = f.retryableIntegrationExecutor.Exec(ctx, project, instance, request)
		if strings.Contains(strings.Join(request.Command, " "), "exec timeout --kill-after=5s 120s") {
			point = "hooks"
		}
	}
	if point == "commit" {
		f.committed = true
	}
	if err == nil && f.cut != "" && point == f.cut {
		f.cut = ""
		return ports.InstanceExecResult{ExitCode: 1}, errors.New("fixture interruption after " + point)
	}
	return result, err
}

func TestIntegrationInterruptedInstalledApplyFinishesInventoryTail(t *testing.T) {
	for _, cut := range []string{"apply", "lifecycle", "hooks", "commit", "verify", "capture-settings"} {
		t.Run(cut, func(t *testing.T) {
			root := testkit.TempDir(t)
			cutPath := filepath.Join(root, "cut")
			for _, name := range []string{"config/projects-changed.sh", "scripts/reconcile-integrations.sh"} {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(root, name), []byte("#!/bin/sh\nif [ -s \"$CUT\" ]; then : > \"$CUT\"; exit 1; fi\nexit 0\n"), 0755)
			}
			executor := &interruptedInventoryExecutor{configInventoryFixture: newConfigInventoryFixture(t)}
			// Model the fixture account's actual primary group. The transport
			// has no authority to chown to a guest-only numeric group.
			executor.program = strings.NewReplacer(
				"os.fchown(stream.fileno(), uid, uid)", "os.fchown(stream.fileno(), uid, os.getgid())",
				"os.chown(directory, uid, uid if gid is None else gid)", "os.chown(directory, uid, os.getgid() if gid is None else gid)",
				"group = entry.get('gid', owner) if entry['kind'] == 'link' else owner", "group = entry.get('gid', os.getgid()) if entry['kind'] == 'link' else os.getgid()",
			).Replace(executor.program)
			runtime := Runtime{RepositoryRoot: root, Executor: executor, Environment: []string{"CUT=" + cutPath},
				Incus: &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{Status: "Running"}}},
				Yard:  domain.Context{YardName: "sample", IncusProject: "sample", YardInstanceName: "sample", DevUser: "dev", DevUID: os.Getuid()}}
			apply := func() error {
				plan, err := runtime.IntegrationPlan(context.Background())
				if err != nil {
					return err
				}
				return runtime.ApplyIntegrations(context.Background(), plan)
			}
			if err := apply(); err != nil {
				t.Fatal(err)
			}
			// Simulate drift repair in the installed view, then interruption after
			// the inventory intent and desired artifacts have been published.
			runtime.appliedIntegrationInspection = true
			executor.serviceChanged, executor.cut, executor.committed = true, cut, false
			if cut == "lifecycle" {
				testkit.WriteFile(t, cutPath, []byte("1"), 0600)
				executor.cut = ""
			}
			if err := apply(); err == nil {
				t.Fatal("interruption did not fire")
			}
			if cut == "apply" {
				entry := executor.read()["entries"].([]any)[0].(map[string]any)
				path := entry["path"].(string)
				owned, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, []byte("foreign bytes"), info.Mode().Perm())
				if err := apply(); err == nil {
					t.Fatal("pending intent accepted arbitrary bytes")
				}
				foreign, _ := os.ReadFile(path)
				if string(foreign) != "foreign bytes" {
					t.Fatal("foreign artifact was changed")
				}
				testkit.WriteFile(t, path, owned, info.Mode().Perm())
			}
			if err := apply(); err != nil {
				t.Fatalf("resume: %v", err)
			}
			for _, value := range executor.read()["entries"].([]any) {
				entry := value.(map[string]any)
				if entry["pending_digest"] != nil || entry["uncommitted"] != nil {
					t.Fatal("resume left inventory pending", entry)
				}
			}
			before := executor.hookAttempts
			if err := apply(); err != nil {
				t.Fatal(err)
			}
			if executor.hookAttempts != before {
				t.Fatal("repeat reran hooks")
			}
		})
	}
}

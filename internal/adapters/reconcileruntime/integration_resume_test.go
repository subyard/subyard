package reconcileruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
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

// Keep the real inventory protocol and bytes; only the guest transport and
// opaque lifecycle commands are substituted. All writes stay in /tmp.
type interruptedInventoryExecutor struct {
	configInventoryFixture
	retryableIntegrationExecutor
	cut       string
	committed bool
}

func newInterruptedInventoryExecutor(t *testing.T) *interruptedInventoryExecutor {
	t.Helper()
	f := &interruptedInventoryExecutor{configInventoryFixture: newConfigInventoryFixture(t)}
	// Model the fixture account's actual primary group, not a guest-only group.
	f.program = strings.NewReplacer(
		"os.fchown(stream.fileno(), uid, uid)", "os.fchown(stream.fileno(), uid, os.getgid())",
		"os.chown(directory, uid, uid if gid is None else gid)", "os.chown(directory, uid, os.getgid() if gid is None else gid)",
		"group = entry.get('gid', owner) if entry['kind'] == 'link' else owner", "group = entry.get('gid', os.getgid()) if entry['kind'] == 'link' else os.getgid()",
	).Replace(f.program)
	return f
}

func TestInstalledReadinessPreservesFailedInitIntent(t *testing.T) {
	for _, labelled := range []bool{false, true} {
		t.Run(fmt.Sprintf("leftover-pending=%t", labelled), func(t *testing.T) {
			executor := newInterruptedInventoryExecutor(t)
			root := testkit.TempDir(t)
			if err := os.Mkdir(filepath.Join(root, "config"), 0700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(root, "config/projects-changed.sh"), []byte("#!/bin/sh\nexit 0\n"), 0755)
			runtime := Runtime{RepositoryRoot: root, Executor: executor, Integrations: []profile.IntegrationDefinition{},
				Incus: &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{Status: "Running"}}},
				Yard:  domain.Context{DevUser: "dev", DevUID: os.Getuid()}}
			ctx := context.Background()
			installed, err := runtime.integrationArtifactsPhase("apply")
			if err != nil {
				t.Fatal(err)
			}
			inventory := func(mode string, entries []integrationArtifact) {
				t.Helper()
				if _, err := runtime.integrationInventory(ctx, mode, entries, ""); err != nil {
					t.Fatal(err)
				}
			}
			inventory("apply", installed)
			inventory("commit", installed)
			runtime = runtime.WithInstalledConfiguration()
			if labelled {
				inventory("apply", installed)
			}
			before, err := runtime.IntegrationPlan(ctx)
			if err != nil || before.InstalledChanged() {
				t.Fatalf("installed baseline: %#v %v", before, err)
			}
			if !labelled {
				// Labels appearing between inspect and converge change the bound
				// state. Observation itself must never cause this write.
				inventory("apply", installed)
				pending, err := runtime.IntegrationPlan(ctx)
				if err != nil || !pending.Changed || pending.Fingerprint == before.Fingerprint ||
					!errors.Is(CheckIntegrationPlan(before, pending), domain.ErrPlanStale) {
					t.Fatalf("intervening apply was not guarded as stale: %#v %v", pending, err)
				}
				inventory("commit", installed)
				committed, err := runtime.IntegrationPlan(ctx)
				if err != nil || committed.StateBinding() != before.StateBinding() || CheckIntegrationPlan(pending, committed) != nil {
					t.Fatalf("finishing redundant intent did not restore the original plan: %#v %v", committed, err)
				}
			}
			// Init selects a new integration. It labels the shared installed files
			// and writes the new config, then native provisioning fails before commit.
			payload := []byte("pending integration config\n")
			desired := append(append([]integrationArtifact{}, installed...), integrationArtifact{
				ID: "sample", Kind: "file", Path: executor.home + "/new-config", Mode: 0644,
				Digest: fmt.Sprintf("%x", sha256.Sum256(payload)), Content: base64.StdEncoding.EncodeToString(payload)})
			inventory("apply", desired)
			path := executor.state + "/inventory.json"
			saved := mustReadIntegrationFile(t, path)
			first, err := runtime.IntegrationPlan(ctx)
			if err != nil || !first.Changed || first.InstalledChanged() {
				t.Fatalf("failed init revoked installed readiness or lost apply tail: %#v %v", first, err)
			}
			second, err := runtime.IntegrationPlan(ctx)
			if err != nil || first.StateBinding() != second.StateBinding() || !bytes.Equal(saved, mustReadIntegrationFile(t, path)) {
				t.Fatalf("unchanged observation moved the plan or wrote inventory: %v", err)
			}
			executor.serviceChanged = true
			drift, err := runtime.IntegrationPlan(ctx)
			if err != nil || !drift.InstalledChanged() {
				t.Fatalf("pending init hid installed service drift: %#v %v", drift, err)
			}
			executor.serviceChanged = false
			// Real installed drift must still require repair and retain its refusal.
			file := executor.home + "/projects-changed"
			testkit.WriteFile(t, file, []byte("foreign bytes"), 0755)
			if _, err := runtime.IntegrationPlan(ctx); err == nil {
				t.Fatal("pending init hid foreign installed bytes")
			}
		})
	}
}

func mustReadIntegrationFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
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
			executor := newInterruptedInventoryExecutor(t)
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

package reconcileruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/incusclient"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestCurrentMaterializedConfigFailureKeepsOriginalCustodyCause(t *testing.T) {
	known := &ports.NativeOperationFailure{Operation: ports.NativeOperation{ID: "original", Status: 400}, Cause: errors.New("closed remote failure")}
	exitCode := 7
	completed := &ports.NativeOperationFailure{Operation: ports.NativeOperation{Kind: "instance-exec", ID: "original", Status: 200}, ExecExitCode: &exitCode, Cause: errors.New("completed SDK command failure")}
	for _, input := range []error{known, fmt.Errorf("wrapped: %w", known), errors.Join(known, errors.New("additional unproved branch")), completed, fmt.Errorf("wrapped: %w", completed), errors.Join(completed, errors.New("additional unproved branch")), errors.New("transport"), nil} {
		executor := &testkit.Incus{ExecSteps: []testkit.IncusExecStep{{Result: ports.InstanceExecResult{ExitCode: 1}, Err: input}}}
		runtime := Runtime{Executor: executor, Yard: domain.Context{DevUser: "dev", DevUID: 1001}}
		_, err := runtime.afterOwnerConfigRequest(context.Background(), "configs-write", nil, nil)
		var failure *ports.NativeOperationFailure
		if errors.As(input, &failure) {
			if err != input {
				t.Fatal("original closed/unproved error chain was replaced", err)
			}
		} else if !errors.Is(err, domain.ErrPlanStale) || errors.As(err, &failure) {
			t.Fatal("generic/nonzero exec became known failure", err)
		}
	}
}

func TestCurrentMaterializedFilesOriginalCapsuleFreshProcess(t *testing.T) {
	ctx := context.Background()
	if root := os.Getenv("SUBYARD_CURRENT_CONFIG_CHILD"); root != "" {
		runtime, executor := afterOwnerFilesFixture(t, root)
		raw, err := os.ReadFile(filepath.Join(root, "current.recipe"))
		if err != nil {
			t.Fatal(err)
		}
		a := filepath.Join(root, "home/.sample/a.conf")
		before := afterOwnerFileStat(t, a)
		writes, _ := os.ReadFile(filepath.Join(root, "writes"))
		scenario := os.Getenv("SUBYARD_CURRENT_CONFIG_SCENARIO")
		switch scenario {
		case "source":
			testkit.WriteFile(t, filepath.Join(root, "sample.RULES"), []byte("foreign source\n"), 0600)
		case "mode":
			os.Chmod(filepath.Join(root, "home/.sample/b.rules"), 0600)
		case "third-state":
			testkit.WriteFile(t, filepath.Join(root, "home/.sample/b.rules"), []byte("foreign B\n"), 0644)
		case "mutation-path":
			executor.mutate = scenario
		}
		p, err := runtime.BindInstalledRuntimeContext(ctx, raw)
		if err == nil {
			defer p.Close()
			if !p.HasCurrentMaterializedCustody() {
				t.Fatal("missing original current custody")
			}
			err = runtime.ApplyPreparedMaterializedConfigs(ctx, p)
		}
		after, _ := os.ReadFile(filepath.Join(root, "writes"))
		if scenario != "partial" {
			if err == nil || !bytes.Equal(writes, after) {
				t.Fatal("drift reached a write", scenario, err)
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(p.Recipe(), raw) || string(after[len(writes):]) != "/home/dev/.sample/b.rules\n" {
				t.Fatal("original remaining-B scope changed")
			}
			body, err := os.ReadFile(filepath.Join(root, "home/.sample/b.rules"))
			if err != nil || string(body) != "approved B\n" {
				t.Fatal("original B not applied", err)
			}
		}
		if before != afterOwnerFileStat(t, a) {
			t.Fatal("already desired A identity changed")
		}
		return
	}
	for _, scenario := range []string{"partial", "source", "mode", "third-state", "mutation-path"} {
		t.Run(scenario, func(t *testing.T) {
			root := testkit.TempDir(t)
			runtime, executor := afterOwnerFilesFixture(t, root)
			if err := os.MkdirAll(filepath.Join(root, "home/.sample"), 0700); err != nil {
				t.Fatal(err)
			}
			for name, content := range map[string]string{"sample.CONFIG": "approved A\n", "sample.RULES": "approved B\n", "home/.sample/a.conf": "old A\n", "home/.sample/b.rules": "old B\n"} {
				testkit.WriteFile(t, filepath.Join(root, name), []byte(content), 0644)
			}
			inventory := configInventoryFixture{t: t, home: filepath.Join(root, "home"), state: filepath.Join(root, "inventory")}
			entries := []map[string]any{}
			for _, leaf := range []string{"a.conf", "b.rules"} {
				letter := "A"
				if leaf == "b.rules" {
					letter = "B"
				}
				entries = append(entries, map[string]any{"id": "sample", "kind": "file", "path": filepath.Join(root, "home/.sample", leaf), "digest": fmt.Sprintf("%x", sha256.Sum256([]byte("old "+letter+"\n")))})
			}
			inventory.save(entries, []string{})
			prepared, err := runtime.PrepareConfigs(ctx)
			if err != nil {
				t.Fatal(err)
			}
			p, err := runtime.PrepareCurrentMaterializedConfigs(ctx, prepared)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			testkit.WriteFile(t, filepath.Join(root, "current.recipe"), p.Recipe(), 0600)
			executor.cut = true
			if err := runtime.ApplyPreparedMaterializedConfigs(ctx, p); err == nil || !executor.cutSeen {
				t.Fatal("no real A/remaining-B partial state", err)
			}
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			child := exec.Command(binary, "-test.run=^TestCurrentMaterializedFilesOriginalCapsuleFreshProcess$", "-test.v")
			child.Env = append(os.Environ(), "SUBYARD_CURRENT_CONFIG_CHILD="+root, "SUBYARD_CURRENT_CONFIG_SCENARIO="+scenario)
			if output, err := child.CombinedOutput(); err != nil {
				t.Fatalf("fresh process: %v\n%s", err, output)
			}
		})
	}
}

// The prepared source/custody path selects the opt-in; the real SDK fixture
// supplies a closed status200/exit7 operation, without simulating an ENOSPC VM.
type currentConfigScopeExecutor struct {
	t          *testing.T
	base       ports.InstanceExecutor
	sdk        ports.InstanceExecutor
	authorized bool
	failures   int
	calls      int
}

func (e *currentConfigScopeExecutor) Exec(ctx context.Context, project, name string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	e.calls++
	cas := len(request.Command) == 5 && request.Command[0] == "python3" && request.Command[4] == "configs-write"
	if got := ports.NativeSynchronousConfigCASFrom(ctx); got != (cas && e.authorized) {
		e.t.Fatalf("completed-exec scope escaped original-current fixed CAS: %v %v", got, request.Command[:1])
	}
	rawWrite := len(request.Command) > 3 && request.Command[0] == "sh" && strings.Contains(request.Command[3], ".subyard-config.")
	if e.sdk != nil && (cas || rawWrite) {
		e.failures++
		return e.sdk.Exec(ctx, project, name, request)
	}
	return e.base.Exec(ctx, project, name, request)
}

type currentConfigScopeObserver struct {
	terminal  []ports.NativeOperation
	uncertain bool
}

func (*currentConfigScopeObserver) Before(ports.NativeOperation) error   { return nil }
func (*currentConfigScopeObserver) Accepted(ports.NativeOperation) error { return nil }
func (o *currentConfigScopeObserver) Terminal(op ports.NativeOperation) error {
	o.terminal = append(o.terminal, op)
	return nil
}
func (o *currentConfigScopeObserver) Uncertain()                                  { o.uncertain = true }
func (*currentConfigScopeObserver) CommandStarted(string, string) (uint64, error) { return 1, nil }
func (*currentConfigScopeObserver) CommandTerminal(uint64) error                  { return nil }

func TestCurrentMaterializedConfigCompletedExecScope(t *testing.T) {
	for _, kind := range []string{"current", "no-current", "after-owner", "shared-integrations-helper"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			root := testkit.TempDir(t)
			runtime, base := afterOwnerFilesFixture(t, root)
			if err := os.MkdirAll(filepath.Join(root, "home/.sample"), 0700); err != nil {
				t.Fatal(err)
			}
			for name, body := range map[string]string{"sample.CONFIG": "approved A\n", "sample.RULES": "approved B\n", "home/.sample/a.conf": "old A\n", "home/.sample/b.rules": "old B\n"} {
				testkit.WriteFile(t, filepath.Join(root, name), []byte(body), 0644)
			}
			inventory := configInventoryFixture{t: t, home: filepath.Join(root, "home"), state: filepath.Join(root, "inventory")}
			entries := []map[string]any{}
			for _, leaf := range []string{"a.conf", "b.rules"} {
				letter := "A"
				if leaf == "b.rules" {
					letter = "B"
				}
				entries = append(entries, map[string]any{"id": "sample", "kind": "file", "path": filepath.Join(root, "home/.sample", leaf), "digest": fmt.Sprintf("%x", sha256.Sum256([]byte("old "+letter+"\n")))})
			}
			inventory.save(entries, []string{})
			var p *PreparedInstalledRuntime
			var err error
			if kind == "after-owner" {
				original, e := runtime.PrepareAfterOwnerMaterializedConfigs(ctx, []byte(`{"version":1,"phase":"template","metadataBytes":8192,"descriptorBytes":8192}`))
				if e != nil {
					t.Fatal(e)
				}
				defer original.Close()
				resolved, physical, e := runtime.ResolveAfterOwnerMaterializedConfigs(ctx, original.Recipe())
				if e != nil {
					t.Fatal(e)
				}
				defer resolved.Close()
				p, err = runtime.BindAfterOwnerMaterializedConfigs(ctx, original.Recipe(), physical)
			} else if kind != "shared-integrations-helper" {
				prepared, e := runtime.PrepareConfigs(ctx)
				if e != nil {
					t.Fatal(e)
				}
				if kind == "current" {
					p, err = runtime.PrepareCurrentMaterializedConfigs(ctx, prepared)
				} else {
					p, err = runtime.PrepareMaterializedConfigs(ctx, prepared)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if p != nil {
				defer p.Close()
			}
			server, e := testkit.NewIncusServer(testkit.TempDir(t))
			if e != nil {
				t.Fatal(e)
			}
			defer server.Close()
			server.QueueExec(testkit.IncusServerExecStep{ExitCode: 7, Stdout: []byte("drained")})
			executor := &currentConfigScopeExecutor{t: t, base: base, sdk: incusclient.New(server.SocketPath), authorized: kind == "current"}
			runtime.Executor = executor
			observer := &currentConfigScopeObserver{}
			ctx = ports.WithNativeOperationObserver(ctx, observer)
			if kind == "shared-integrations-helper" {
				_, err = runtime.afterOwnerConfigRequest(ctx, "configs-write", nil, nil)
			} else {
				err = runtime.ApplyPreparedMaterializedConfigs(ctx, p)
			}
			var failure *ports.NativeOperationFailure
			known := errors.As(err, &failure) && failure.ExecExitCode != nil
			if err == nil || known != (kind == "current") || executor.failures != 1 || len(observer.terminal) != 1 || observer.terminal[0].Status != 200 || observer.uncertain {
				t.Fatalf("owning prepared CAS failure scope: known=%v calls=%d terminals=%v err=%v", known, executor.failures, observer.terminal, err)
			}
			if known && (*failure.ExecExitCode != 7 || failure.Operation != observer.terminal[0]) {
				t.Fatal("original SDK failure tuple lost")
			}
		})
	}
	t.Run("materialized-integrations", func(t *testing.T) {
		root := testkit.TempDir(t)
		runtime, base := materializedIntegrationFixture(t, root, true)
		p := prepareMaterializedIntegrationFixture(t, runtime)
		defer p.Close()
		executor := &currentConfigScopeExecutor{t: t, base: base}
		runtime.Executor = executor
		if err := runtime.ApplyPreparedMaterializedIntegrations(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		if executor.calls == 0 {
			t.Fatal("integration path was not exercised")
		}
	})
}

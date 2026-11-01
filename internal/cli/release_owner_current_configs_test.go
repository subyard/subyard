package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestCurrentMaterializedOriginalTargetFreshBind(t *testing.T) {
	for _, scenario := range []string{"partial", "old-no-custody", "foreign-file", "before", "missing-before", "uuid", "apply-failed"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			c, _ := nativeActivationSourceFixture(t)
			nativeMaterializedSourceFiles(t, c.context.RepositoryRoot)
			settings := filepath.Join(c.context.RepositoryRoot, "config/agents.env")
			content, err := os.ReadFile(settings)
			if err != nil {
				t.Fatal(err)
			}
			content = bytes.ReplaceAll(content, []byte("AGENT_sample_RULES_DEST=.sample/rules"), []byte("AGENT_sample_RULES_DEST=.sample/b.rules"))
			content = append(content, []byte("AGENT_sample_CONFIG='"+filepath.Join(c.context.RepositoryRoot, "config/agents/sample/CONFIG")+"'\nAGENT_sample_CONFIG_DEST=.sample/a.conf\n")...)
			testkit.WriteFile(t, settings, content, 0755)
			testkit.WriteFile(t, filepath.Join(c.context.RepositoryRoot, "config/agents/sample/CONFIG"), []byte("approved A\n"), 0755)
			incus := c.cli.options.Incus.(*testkit.Incus)
			instance := ports.InstanceInfo{Name: "yard", Project: "subyard", Status: "Running", Config: map[string]string{"volatile.uuid": "current-original-uuid"}}
			incus.Instances["subyard/yard"] = instance
			incus.Reconcile = ports.ReconcileState{InstanceFound: true, Instance: instance}
			state := &nativeMaterializedState{files: map[string][]byte{"/home/dev/.sample/a.conf": []byte("old A\n"), "/home/dev/.sample/b.rules": []byte("old B\n")}}
			exit := 37
			cause := &ports.NativeOperationFailure{Operation: ports.NativeOperation{Kind: "instance-exec", ID: "original", Status: 200}, ExecExitCode: &exit, Cause: errors.New("private native apply failure")}
			c.cli.options.Executor = state
			if scenario == "apply-failed" {
				c.cli.options.Executor = currentConfigFailureExecutor{state, cause}
			}
			if _, _, err := c.CaptureInventory(ctx); err != nil {
				t.Fatal(err)
			}
			values, err := c.CaptureActivation(ctx, "materialized-config", false)
			if err != nil {
				t.Fatal(err)
			}
			var original releaseowner.Target
			for _, v := range values {
				if v.Class == "materialized-files" {
					original = v
				}
			}
			if original.PreparedRecipe == "" || original.AfterOwner != "" {
				t.Fatal("not an original current target")
			}
			raw, err := c.InstalledRuntimeRecipe(original.PreparedRecipe)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "old-no-custody" {
				prepared := c.activation[activationKey(original)]
				runtime := prepared.platform.(reconcileruntime.Runtime)
				old, err := runtime.PrepareMaterializedConfigs(ctx, prepared.nativeConfigs)
				if err != nil {
					t.Fatal(err)
				}
				defer old.Close()
				original.PreparedRecipe = old.Binding()
				raw = old.Recipe()
			}
			state.files["/home/dev/.sample/a.conf"] = []byte("approved A\n")
			if scenario == "foreign-file" {
				state.files["/home/dev/.sample/b.rules"] = []byte("foreign B\n")
			}
			fresh := &releaseOwnerComponents{cli: c.cli, root: c.root, context: c.context, inputs: map[string]config.PersistentFileSnapshot{}, overlays: map[string]config.PersistentFileSnapshot{}, loaded: map[string]config.Loaded{}, activation: map[string]releaseActivationPrepared{}, installedBound: map[string]bool{}}
			t.Cleanup(func() { fresh.Close() })
			if _, _, err := fresh.CaptureInventory(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := fresh.CaptureActivation(ctx, original.Owner, true); err != nil {
				t.Fatal(err)
			}
			if scenario == "before" {
				original.Before = "foreign"
			}
			if scenario == "missing-before" {
				original.Before = ""
			}
			if scenario == "uuid" {
				original.InstanceUUID = "foreign"
			}
			err = fresh.BindInstalledRuntimeRecipeForTarget(ctx, original.PreparedRecipe, raw, original)
			if scenario != "partial" && scenario != "apply-failed" {
				if err == nil || state.writes != 0 {
					t.Fatal("unproved original target accepted", scenario, err)
				}
				return
			}
			if err != nil {
				t.Fatal("fresh original current bind", err)
			}
			if scenario == "apply-failed" {
				err := fresh.ApplyActivation(ctx, original)
				var diagnostic interface{ ActivationDiagnostic() (string, string) }
				if !errors.Is(err, cause) || !errors.As(err, &diagnostic) {
					t.Fatal("native apply lost its public component diagnostic")
				}
				message, retry := diagnostic.ActivationDiagnostic()
				if message != fmt.Sprintf("yard %s: cannot apply materialized config for release activation", original.Yard) || retry != fmt.Sprintf("run yard -Y %s config status", original.Yard) || strings.Contains(message, "private") || state.writes != 0 {
					t.Fatal("native apply diagnostic changed its scope or leaked private cause")
				}
				return
			}
			if err := fresh.ApplyActivation(ctx, original); err != nil {
				t.Fatal("remaining original B", err)
			}
			if state.writes != 1 || string(state.files["/home/dev/.sample/a.conf"]) != "approved A\n" || string(state.files["/home/dev/.sample/b.rules"]) != "approved original materialized rules\n" {
				t.Fatal("current recovery expanded writes", state.writes)
			}
		})
	}
}

type currentConfigFailureExecutor struct {
	ports.InstanceExecutor
	cause error
}

func (e currentConfigFailureExecutor) Exec(ctx context.Context, project, name string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	if len(request.Command) > 4 && request.Command[4] == "configs-write" {
		return ports.InstanceExecResult{}, e.cause
	}
	return e.InstanceExecutor.Exec(ctx, project, name, request)
}

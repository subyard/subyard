package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestInitProfileRuntimePreparationObservesOnceAndRetainsFreshGuard(t *testing.T) {
	for _, scenario := range []string{"current", "stale", "absent", "deferred", "pending", "reset", "failed", "reordered"} {
		t.Run(scenario, func(t *testing.T) {
			root, _, _ := nativeFixture(t)
			writeRuntimeProfileFixture(t, root)
			state, calls := filepath.Join(root, "runtime-state"), filepath.Join(root, "runtime-calls")
			current := fmt.Sprintf(`{"state":"current","actual":%q,"desired":%q}`, strings.Repeat("a", 64), strings.Repeat("a", 64))
			observation := current
			if scenario == "stale" {
				observation = fmt.Sprintf(`{"state":"stale","actual":%q,"desired":%q}`, strings.Repeat("b", 64), strings.Repeat("a", 64))
			} else if scenario == "absent" || scenario == "deferred" || scenario == "pending" || scenario == "reset" {
				state := scenario
				if scenario == "pending" || scenario == "reset" {
					state = "deferred"
				}
				observation = fmt.Sprintf(`{"state":%q}`, state)
			}
			writeCLIFile(t, state, observation, 0o600)
			handler := "#!/bin/sh\nprintf 'called\\n' >> \"$CALLS\"\ncat \"$STATE\"\n"
			if scenario == "failed" {
				handler = "#!/bin/sh\nprintf 'called\\n' >> \"$CALLS\"\nexit 9\n"
			}
			writeCLIFile(t, filepath.Join(root, "config/profiles/sample/runtime.sh"), handler, 0o700)
			prefix := "#!/bin/sh\nexit 0\n"
			if scenario == "pending" {
				prefix = "#!/bin/sh\necho 'profile service check owner=sample native-exit=1'\nexit 1\n"
			}
			writeCLIFile(t, filepath.Join(root, "scripts/profile-services.sh"), prefix, 0o700)
			var diagnostics bytes.Buffer
			runtime := reconcileruntime.Runtime{RepositoryRoot: root, Yard: domain.Context{IncusProject: "subyard", YardInstanceName: "yard"}, Environment: []string{"STATE=" + state, "CALLS=" + calls}, Stderr: &diagnostics, Incus: &testkit.Incus{Instances: map[string]ports.InstanceInfo{"subyard/yard": {Status: "Running"}}}}
			execution := &initExecution{platform: runtime}
			if scenario == "reset" {
				execution.mode = initReset
			}
			stages := []application.ReconcileStage{{ID: ports.ReconcileStageProfileServices, Label: "services"}, {ID: ports.ReconcileStageProfileRuntimes, Label: "runtimes"}}
			if scenario == "reordered" {
				stages[0], stages[1] = stages[1], stages[0]
			}
			err := execution.prepareStages(context.Background(), stages)
			if scenario == "failed" {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 9 || !strings.Contains(err.Error(), `check init stage "profile-runtimes"`) {
					t.Fatalf("runtime observation lost its context/cause: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(calls)
			wantCalls := "called\n"
			if scenario == "reordered" {
				wantCalls += "called\n"
			}
			if err != nil || string(data) != wantCalls {
				t.Fatalf("initial preparation did not observe once: %q %v", data, err)
			}
			last := execution.plan.Steps[1]
			conditional := scenario == "pending" || scenario == "reset"
			if last.Stage != stages[1] || last.Conditional != conditional || last.Converged != (!conditional && scenario != "stale") || execution.approvedPlan.Steps[1] != last {
				t.Fatalf("runtime stage changed semantics: %+v", last)
			}
			if strings.Contains(diagnostics.String(), "deferred") != (scenario == "deferred") {
				t.Fatalf("unexpected deferred reporting: %q", diagnostics.String())
			}
			if scenario == "current" {
				writeCLIFile(t, state, fmt.Sprintf(`{"state":"stale","desired":%q}`, strings.Repeat("b", 64)), 0o600)
				if err := execution.checkNativePlans(context.Background(), true); !errors.Is(err, domain.ErrPlanStale) {
					t.Fatalf("fresh runtime drift was accepted: %v", err)
				}
				if data, err := os.ReadFile(calls); err != nil || string(data) != "called\ncalled\n" {
					t.Fatalf("fresh guard did not observe runtime: %q %v", data, err)
				}
			}
		})
	}
}

func TestInitUnconvergedIncusStageKeepsObservableRuntimeGuard(t *testing.T) {
	root := testkit.TempDir(t)
	profileRoot := filepath.Join(root, "config", "profiles", "synthetic")
	if err := os.MkdirAll(profileRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	handler := filepath.Join(profileRoot, "runtime.sh")
	state := filepath.Join(root, "observation")
	testkit.WriteFile(t, handler, []byte("#!/bin/sh\ncat \"$STATE\"\n"), 0o700)
	old := fmt.Sprintf("%064x", 1)
	desired := fmt.Sprintf("%064x", 2)
	testkit.WriteFile(t, state, []byte(fmt.Sprintf(`{"state":"current","actual":%q,"desired":%q}`, desired, desired)), 0o600)
	yard := domain.Context{IncusProject: "subyard", YardInstanceName: "yard"}
	runtime := reconcileruntime.Runtime{RepositoryRoot: root, Yard: yard, Profiles: []profile.Definition{{Name: "synthetic", Root: profileRoot, Runtime: &profile.RuntimeHook{ActivationID: "synthetic-runtime", Handler: "runtime.sh"}}}, Environment: []string{"STATE=" + state}, Incus: &testkit.Incus{Instances: map[string]ports.InstanceInfo{"subyard/yard": {Status: "running"}}}}
	plan, err := runtime.PrepareProfileRuntimes(context.Background(), true, false)
	if err != nil {
		t.Fatal(err)
	}
	runtime.RuntimePlan = plan
	execution := &initExecution{loaded: config.Loaded{Context: yard}, runtimePlan: plan, platform: runtime, approvedPlan: application.ReconcilePlan{Steps: []application.ReconcileStep{{Stage: application.InitStages(yard)[0]}}}}
	program := &CLI{options: Options{InitPlatform: runtime}}
	execution.rebuildPlatform(program)
	if execution.platform.(reconcileruntime.Runtime).RuntimePlan != plan {
		t.Fatal("platform rebuild dropped the retained runtime plan")
	}
	testkit.WriteFile(t, state, []byte(fmt.Sprintf(`{"state":"stale","actual":%q,"desired":%q}`, old, desired)), 0o600)
	if err := execution.checkNativePlans(context.Background(), true); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("pending Incus stage hid observed native runtime drift: %v", err)
	}
}

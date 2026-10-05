package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

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

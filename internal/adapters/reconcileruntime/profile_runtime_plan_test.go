package reconcileruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/incusclient"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestPreparedProfileRuntimeRejectsObservedSkipExpansionBeforeApply(t *testing.T) {
	root := testkit.TempDir(t)
	state := filepath.Join(root, "observation")
	log := filepath.Join(root, "apply")
	runtime := profileRuntimeFixtureAt(t, root, `#!/bin/sh
set -eu
case "$1" in
 observe) cat "$STATE" ;;
 apply) printf mutation > "$LOG" ;;
esac
`)
	runtime.Environment = []string{"STATE=" + state, "LOG=" + log, "SUBYARD_OPERATION_ID=op-12345678"}
	testkit.WriteFile(t, state, []byte(fmt.Sprintf(`{"state":"current","actual":%q,"desired":%q}`, testRuntimeNewDigest, testRuntimeNewDigest)), 0o600)
	plan, err := runtime.PrepareProfileRuntimes(context.Background(), false, false)
	if err != nil {
		t.Fatal(err)
	}
	runtime.RuntimePlan = plan
	testkit.WriteFile(t, state, []byte(fmt.Sprintf(`{"state":"stale","actual":%q,"desired":%q}`, testRuntimeOldDigest, testRuntimeNewDigest)), 0o600)
	if err := runtime.ApplyStage(context.Background(), ports.ReconcileStageProfileRuntimes); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("new work was authorized: %v", err)
	}
	if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale observation wrote: %v", err)
	}
}

func TestPreparedProfileRuntimeAllowsConvergenceAndRejectsDesiredDrift(t *testing.T) {
	for _, converged := range []bool{true, false} {
		t.Run(fmt.Sprint(converged), func(t *testing.T) {
			root := testkit.TempDir(t)
			state := filepath.Join(root, "observation")
			runtime := profileRuntimeFixtureAt(t, root, "#!/bin/sh\ncat \"$STATE\"\n")
			runtime.Environment = []string{"STATE=" + state}
			testkit.WriteFile(t, state, []byte(fmt.Sprintf(`{"state":"stale","actual":%q,"desired":%q}`, testRuntimeOldDigest, testRuntimeNewDigest)), 0o600)
			plan, err := runtime.PrepareProfileRuntimes(context.Background(), false, false)
			if err != nil {
				t.Fatal(err)
			}
			runtime.RuntimePlan = plan
			if converged {
				testkit.WriteFile(t, state, []byte(fmt.Sprintf(`{"state":"current","actual":%q,"desired":%q}`, testRuntimeNewDigest, testRuntimeNewDigest)), 0o600)
			} else {
				testkit.WriteFile(t, state, []byte(fmt.Sprintf(`{"state":"stale","actual":%q,"desired":%q}`, testRuntimeNewDigest, testRuntimeOldDigest)), 0o600)
			}
			err = runtime.CheckProfileRuntimePlan(context.Background())
			if converged {
				if err != nil || plan.OperationSteps("project/yard")[0].Decision != domain.StepSkip {
					t.Fatalf("convergence rejected: %v", err)
				}
			} else if !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("desired drift accepted: %v", err)
			}
		})
	}
}

func TestPreparedConditionalProfileRuntimeBindsSourcesAndResolvedObservation(t *testing.T) {
	root := testkit.TempDir(t)
	state := filepath.Join(root, "observation")
	runtime := profileRuntimeFixtureAt(t, root, "#!/bin/sh\ncat \"$STATE\"\n")
	runtime.Environment = []string{"STATE=" + state}
	runningIncus := runtime.Incus
	runtime.Incus = &testkit.Incus{}
	plan, err := runtime.PrepareProfileRuntimes(context.Background(), true, false)
	if err != nil {
		t.Fatal(err)
	}
	runtime.RuntimePlan = plan
	runtime.Incus = runningIncus
	initial := plan.OperationSteps("project/yard")
	if len(initial) != 1 || initial[0].Decision != domain.StepConditional {
		t.Fatalf("unavailable native slot omitted: %+v", initial)
	}
	testkit.WriteFile(t, state, []byte(fmt.Sprintf(`{"state":"stale","actual":%q,"desired":%q}`, testRuntimeOldDigest, testRuntimeNewDigest)), 0o600)
	if err := runtime.CheckProfileRuntimePlan(context.Background()); err != nil {
		t.Fatal(err)
	}
	runtime.resolveProfileRuntimePlan()
	testkit.WriteFile(t, state, []byte(fmt.Sprintf(`{"state":"stale","actual":%q,"desired":%q}`, testRuntimeNewDigest, testRuntimeOldDigest)), 0o600)
	if err := runtime.CheckProfileRuntimePlan(context.Background()); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("resolved conditional expanded: %v", err)
	}
	testkit.WriteFile(t, filepath.Join(root, "config", "profiles", "synthetic", "new-helper.sh"), []byte("#!/bin/sh\n"), 0o700)
	if err := runtime.CheckProfileRuntimePlan(context.Background()); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("native helper change accepted: %v", err)
	}
}

func TestProfileRuntimeIndirectInputsBindMetadataWithoutReadingPrivatePayloads(t *testing.T) {
	root := testkit.TempDir(t)
	runtime := profileRuntimeFixtureAt(t, root, "#!/bin/sh\nprintf '{\"state\":\"absent\",\"actual\":\"\",\"desired\":\"\"}\n'\n")
	path := filepath.Join(root, "config", "private.env")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(64 << 20); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	plan, err := runtime.PrepareProfileRuntimes(context.Background(), false, false)
	if err != nil {
		t.Fatalf("indirect metadata required private payload reading: %v", err)
	}
	runtime.RuntimePlan = plan
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := runtime.CheckProfileRuntimePlan(context.Background()); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("indirect metadata drift was ignored: %v", err)
	}
}

func TestPreparedProfileRuntimeRealColdTransportRequiresApprovedPrerequisite(t *testing.T) {
	root := testkit.TempDir(t)
	runtime := profileRuntimeFixtureAt(t, root, "#!/bin/sh\nexit 79\n")
	testkit.WriteFile(t, filepath.Join(root, "config", "projects-changed.sh"), []byte("#!/bin/sh\nexit 79\n"), 0700)
	serverRoot := filepath.Join(root, "absent-server")
	t.Setenv("INCUS_DIR", serverRoot)
	t.Setenv("INCUS_SOCKET", "")
	t.Setenv("PATH", root)
	runtime.Incus = incusclient.New("")
	if _, err := runtime.ObserveProfileRuntimes(context.Background()); err == nil {
		t.Fatal("transport ENOENT was mistaken for an absent instance")
	}
	if _, err := runtime.PrepareProfileRuntimes(context.Background(), false, false); err == nil {
		t.Fatal("unapproved dependency unavailability was accepted")
	}
	plan, err := runtime.PrepareProfileRuntimes(context.Background(), true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.unavailable || plan.approved["synthetic-runtime"].State != ports.RuntimeStateDeferred || plan.OperationSteps("subyard/yard")[0].Decision != domain.StepConditional {
		t.Fatal("proven cold transport did not retain the declared conditional runtime slot")
	}
	runtime.RuntimePlan = plan
	hooks, err := runtime.PrepareProjectHooks(context.Background(), true)
	if err != nil || !hooks.unavailable || !hooks.conditional {
		t.Fatalf("cold hooks did not retain bounded prerequisites: %v", err)
	}
	if _, err := os.Lstat(serverRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read-only cold preparation created server state")
	}
	for _, scenario := range []string{"retained database", "custom socket", "client present"} {
		t.Run(scenario, func(t *testing.T) {
			candidate := runtime
			switch scenario {
			case "retained database":
				if err := os.Mkdir(serverRoot, 0700); err != nil {
					t.Fatal(err)
				}
				defer os.Remove(serverRoot)
			case "custom socket":
				candidate.Incus = incusclient.New(filepath.Join(root, "unknown.socket"))
			case "client present":
				testkit.WriteFile(t, filepath.Join(root, "incus"), []byte("#!/bin/sh\nexit 79\n"), 0700)
				defer os.Remove(filepath.Join(root, "incus"))
			}
			if _, err := candidate.PrepareProfileRuntimes(context.Background(), true, false); err == nil {
				t.Fatal("ambiguous missing backend was accepted as a cold install")
			}
		})
	}
}

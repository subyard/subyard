package reconcileruntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

const (
	orcaOldDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	orcaNewDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestObserveOrcaRuntimeClassifiesAbsentAndStoppedWithoutHandler(t *testing.T) {
	yard := domain.Context{IncusProject: "subyard", YardInstanceName: "yard"}
	runtime := Runtime{Incus: &testkit.Incus{}, Yard: yard}
	observation, err := runtime.ObserveOrcaRuntime(context.Background())
	if err != nil || observation.State != "absent" {
		t.Fatalf("absent observation = %#v, %v", observation, err)
	}

	var diagnostics bytes.Buffer
	runtime.Incus = &testkit.Incus{Instances: map[string]ports.InstanceInfo{
		"subyard/yard": {Status: "Stopped"},
	}}
	runtime.Stderr = &diagnostics
	observation, err = runtime.ObserveOrcaRuntime(context.Background())
	if err != nil || observation.State != "deferred" {
		t.Fatalf("stopped observation = %#v, %v", observation, err)
	}
	if diagnostics.Len() != 0 {
		t.Fatalf("observe emitted diagnostics: %q", diagnostics.String())
	}
	converged, err := runtime.CheckStage(context.Background(), ports.ReconcileStageOrca)
	if err != nil || !converged || !strings.Contains(diagnostics.String(), "deferred") {
		t.Fatalf("stopped check = %v, %v, diagnostics %q", converged, err, diagnostics.String())
	}
}

func TestObserveOrcaRuntimeRejectsUnavailableAndAmbiguousState(t *testing.T) {
	yard := domain.Context{IncusProject: "subyard", YardInstanceName: "yard"}
	runtime := Runtime{Incus: &testkit.Incus{Err: errors.New("incus unavailable")}, Yard: yard}
	if _, err := runtime.ObserveOrcaRuntime(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "incus unavailable") {
		t.Fatalf("Incus error = %v", err)
	}
	runtime.Incus = &testkit.Incus{Instances: map[string]ports.InstanceInfo{
		"subyard/yard": {Status: "Frozen"},
	}}
	if _, err := runtime.ObserveOrcaRuntime(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "Frozen") {
		t.Fatalf("ambiguous state error = %v", err)
	}
}

func TestObserveOrcaRuntimeValidatesBoundedStrictJSON(t *testing.T) {
	tests := []struct {
		name   string
		output string
	}{
		{name: "unknown state", output: `{"state":"future","actual":"","desired":""}`},
		{name: "bad digest", output: `{"state":"current","actual":"abc","desired":"abc"}`},
		{name: "unknown field", output: `{"state":"absent","actual":"","desired":"","extra":true}`},
		{name: "trailing", output: `{"state":"absent","actual":"","desired":""} {}`},
		{name: "oversize", output: strings.Repeat("x", orcaRuntimeOutputLimit+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime := orcaRuntimeFixture(t, test.output)
			if _, err := runtime.ObserveOrcaRuntime(context.Background()); err == nil {
				t.Fatal("invalid handler output was accepted")
			}
		})
	}
}

func TestApplyOrcaRuntimeBindsAssessmentAndVerifies(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	if err := os.WriteFile(state, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(root, "arguments")
	handler := `#!/bin/sh
set -eu
case "$2" in
  observe)
    if [ "$(cat "$STATE")" = stale ]; then
      printf '{"state":"stale","actual":"%s","desired":"%s"}\n' "$OLD" "$NEW"
    else
      printf '{"state":"current","actual":"%s","desired":"%s"}\n' "$NEW" "$NEW"
    fi
    ;;
  apply)
    printf '%s\n' "$*" > "$LOG"
    [ "$3" = "$OPERATION" ] && [ "$4" = "$OLD" ] && [ "$5" = "$NEW" ]
    printf current > "$STATE"
    printf '{"state":"current","actual":"%s","desired":"%s"}\n' "$NEW" "$NEW"
    ;;
esac
`
	runtime := orcaRuntimeFixtureWithHandler(t, root, handler)
	runtime.Environment = []string{
		"STATE=" + state, "LOG=" + log, "OLD=" + orcaOldDigest, "NEW=" + orcaNewDigest,
		"OPERATION=op-12345678", "SUBYARD_OPERATION_ID=op-12345678",
	}
	if err := runtime.ApplyStage(context.Background(), ports.ReconcileStageOrca); err != nil {
		t.Fatal(err)
	}
	arguments, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := "_runtime-contract apply op-12345678 " + orcaOldDigest + " " + orcaNewDigest + "\n"
	if string(arguments) != want {
		t.Fatalf("apply arguments = %q, want %q", arguments, want)
	}
}

func TestApplyOrcaRuntimeRejectsFailedVerification(t *testing.T) {
	handler := `#!/bin/sh
case "$2" in
  observe) printf '{"state":"stale","actual":"%s","desired":"%s"}\n' "$OLD" "$NEW" ;;
  apply) printf '{"state":"current","actual":"%s","desired":"%s"}\n' "$NEW" "$NEW" ;;
esac
`
	runtime := orcaRuntimeFixtureWithHandler(t, t.TempDir(), handler)
	runtime.Environment = []string{
		"OLD=" + orcaOldDigest, "NEW=" + orcaNewDigest,
		"SUBYARD_OPERATION_ID=op-12345678",
	}
	if err := runtime.ApplyStage(context.Background(), ports.ReconcileStageOrca); err == nil ||
		!strings.Contains(err.Error(), "did not retain") {
		t.Fatalf("verification error = %v", err)
	}
}

func TestProvisionRefreshesOrcaAfterBaseBeforeIntegrationHooks(t *testing.T) {
	for _, failure := range []string{"", "base", "orca"} {
		t.Run("failure="+failure, func(t *testing.T) {
			root := t.TempDir()
			handler := `#!/bin/sh
set -eu
[ -e "$BASE_READY" ]
case "$2" in
  observe)
    if [ -e "$ORCA_READY" ]; then
      printf '{"state":"current","actual":"%s","desired":"%s"}\n' "$NEW" "$NEW"
    else
      printf '{"state":"stale","actual":"%s","desired":"%s"}\n' "$OLD" "$NEW"
    fi
    ;;
  apply)
    [ "$FAIL_STAGE" != orca ]
    : > "$ORCA_READY"
    printf '{"state":"current","actual":"%s","desired":"%s"}\n' "$NEW" "$NEW"
    ;;
esac
`
			runtime := orcaRuntimeFixtureWithHandler(t, root, handler)
			for path, content := range map[string]string{
				"config/projects-changed.sh":        "#!/bin/sh\nexit 0\n",
				"scripts/04-provision-subyard.sh":   "#!/bin/sh\nset -eu\n[ \"$FAIL_STAGE\" != base ]\n: > \"$BASE_READY\"\n",
				"scripts/reconcile-integrations.sh": "#!/bin/sh\nexit 0\n",
			} {
				full := filepath.Join(root, path)
				if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte(content), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			ready := filepath.Join(root, "orca-ready")
			executor := &retryableIntegrationExecutor{pending: true, hookReady: ready}
			runtime.Executor = executor
			incus := runtime.Incus.(*testkit.Incus)
			incus.Reconcile = ports.ReconcileState{InstanceFound: true, Instance: incus.Instances["subyard/yard"]}
			runtime.Environment = []string{
				"BASE_READY=" + filepath.Join(root, "base-ready"), "ORCA_READY=" + ready,
				"FAIL_STAGE=" + failure, "OLD=" + orcaOldDigest, "NEW=" + orcaNewDigest,
				"SUBYARD_OPERATION_ID=op-12345678",
			}
			err := runtime.ApplyStage(context.Background(), ports.ReconcileStageProvision)
			if failure != "" {
				if err == nil || executor.hookAttempts != 0 || executor.commits != 0 {
					t.Fatalf("failed %s reached hooks/commit: err=%v hooks=%d commits=%d", failure, err, executor.hookAttempts, executor.commits)
				}
				return
			}
			if err != nil || executor.hookAttempts != 1 || executor.commits != 1 || executor.pending {
				t.Fatalf("provision order failed: err=%v hooks=%d commits=%d pending=%v", err, executor.hookAttempts, executor.commits, executor.pending)
			}
		})
	}
}

func orcaRuntimeFixture(t *testing.T, output string) Runtime {
	t.Helper()
	root := t.TempDir()
	handler := "#!/bin/sh\nprintf '%s' \"$OUTPUT\"\n"
	runtime := orcaRuntimeFixtureWithHandler(t, root, handler)
	runtime.Environment = []string{"OUTPUT=" + output}
	return runtime
}

func orcaRuntimeFixtureWithHandler(t *testing.T, root, content string) Runtime {
	t.Helper()
	directory := filepath.Join(root, "config", "profiles", "orca", "resources", "orca")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "handler.sh"), []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	yard := domain.Context{IncusProject: "subyard", YardInstanceName: "yard"}
	return Runtime{
		RepositoryRoot: root,
		Incus: &testkit.Incus{Instances: map[string]ports.InstanceInfo{
			"subyard/yard": {Status: "Running"},
		}},
		Yard: yard,
	}
}

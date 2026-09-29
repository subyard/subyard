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
	testRuntimeOldDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testRuntimeNewDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestObserveProfileRuntimesClassifiesAbsentAndStoppedWithoutHandler(t *testing.T) {
	runtime := profileRuntimeFixture(t, `#!/bin/sh
exit 9
`)
	runtime.Incus = &testkit.Incus{}
	observations, err := runtime.ObserveProfileRuntimes(context.Background())
	if err != nil || observations["synthetic-runtime"].State != ports.RuntimeStateAbsent {
		t.Fatalf("absent observations = %#v, %v", observations, err)
	}

	var diagnostics bytes.Buffer
	runtime.Incus = &testkit.Incus{Instances: map[string]ports.InstanceInfo{
		"subyard/yard": {Status: "Stopped"},
	}}
	runtime.Stderr = &diagnostics
	observations, err = runtime.ObserveProfileRuntimes(context.Background())
	if err != nil || observations["synthetic-runtime"].State != ports.RuntimeStateDeferred {
		t.Fatalf("stopped observations = %#v, %v", observations, err)
	}
	if diagnostics.Len() != 0 {
		t.Fatalf("observe emitted diagnostics: %q", diagnostics.String())
	}
	converged, err := runtime.CheckStage(context.Background(), ports.ReconcileStageProfileRuntimes)
	if err != nil || !converged || !strings.Contains(diagnostics.String(), "deferred") {
		t.Fatalf("stopped check = %v, %v, diagnostics %q", converged, err, diagnostics.String())
	}
}

func TestObserveProfileRuntimesRejectsUnavailableAndAmbiguousState(t *testing.T) {
	runtime := profileRuntimeFixture(t, "#!/bin/sh\nexit 0\n")
	runtime.Incus = &testkit.Incus{Err: errors.New("incus unavailable")}
	if _, err := runtime.ObserveProfileRuntimes(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "incus unavailable") {
		t.Fatalf("Incus error = %v", err)
	}
	runtime.Incus = &testkit.Incus{Instances: map[string]ports.InstanceInfo{
		"subyard/yard": {Status: "Frozen"},
	}}
	if _, err := runtime.ObserveProfileRuntimes(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "Frozen") {
		t.Fatalf("ambiguous state error = %v", err)
	}
}

func TestObserveProfileRuntimeValidatesBoundedStrictJSON(t *testing.T) {
	tests := []struct {
		name   string
		output string
	}{
		{name: "unknown state", output: `{"state":"future","actual":"","desired":""}`},
		{name: "bad digest", output: `{"state":"current","actual":"abc","desired":"abc"}`},
		{name: "unknown field", output: `{"state":"absent","actual":"","desired":"","extra":true}`},
		{name: "trailing", output: `{"state":"absent","actual":"","desired":""} {}`},
		{name: "oversize", output: strings.Repeat("x", profileRuntimeOutputLimit+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime := profileRuntimeFixture(t, "#!/bin/sh\nprintf '%s' \"$OUTPUT\"\n")
			runtime.Environment = []string{"OUTPUT=" + test.output}
			if _, err := runtime.ObserveProfileRuntimes(context.Background()); err == nil {
				t.Fatal("invalid handler output was accepted")
			}
		})
	}
}

func TestApplyProfileRuntimeBindsAssessmentAndVerifies(t *testing.T) {
	root := testkit.TempDir(t)
	state := filepath.Join(root, "state")
	testkit.WriteFile(t, state, []byte("stale"), 0o600)
	log := filepath.Join(root, "arguments")
	handler := `#!/bin/sh
set -eu
case "$1" in
  observe)
    if [ "$(cat "$STATE")" = stale ]; then
      printf '{"state":"stale","actual":"%s","desired":"%s"}\n' "$OLD" "$NEW"
    else
      printf '{"state":"current","actual":"%s","desired":"%s"}\n' "$NEW" "$NEW"
    fi
    ;;
  apply)
    printf '%s\n' "$*" > "$LOG"
    [ "$2" = "$OPERATION" ] && [ "$3" = "$OLD" ] && [ "$4" = "$NEW" ]
    printf current > "$STATE"
    printf '{"state":"current","actual":"%s","desired":"%s"}\n' "$NEW" "$NEW"
    ;;
esac
`
	runtime := profileRuntimeFixtureAt(t, root, handler)
	runtime.Environment = []string{
		"STATE=" + state, "LOG=" + log, "OLD=" + testRuntimeOldDigest, "NEW=" + testRuntimeNewDigest,
		"OPERATION=op-12345678", "SUBYARD_OPERATION_ID=op-12345678",
	}
	if err := runtime.ApplyStage(context.Background(), ports.ReconcileStageProfileRuntimes); err != nil {
		t.Fatal(err)
	}
	arguments, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := "apply op-12345678 " + testRuntimeOldDigest + " " + testRuntimeNewDigest + "\n"
	if string(arguments) != want {
		t.Fatalf("apply arguments = %q, want %q", arguments, want)
	}
}

func TestApplyProfileRuntimeRejectsFailedVerification(t *testing.T) {
	handler := `#!/bin/sh
case "$1" in
  observe) printf '{"state":"stale","actual":"%s","desired":"%s"}\n' "$OLD" "$NEW" ;;
  apply) printf '{"state":"current","actual":"%s","desired":"%s"}\n' "$NEW" "$NEW" ;;
esac
`
	runtime := profileRuntimeFixture(t, handler)
	runtime.Environment = []string{
		"OLD=" + testRuntimeOldDigest, "NEW=" + testRuntimeNewDigest,
		"SUBYARD_OPERATION_ID=op-12345678",
	}
	if err := runtime.ApplyStage(context.Background(), ports.ReconcileStageProfileRuntimes); err == nil ||
		!strings.Contains(err.Error(), "did not retain") {
		t.Fatalf("verification error = %v", err)
	}
}

func TestApplyProfileRuntimeRejectsObservationChangesAfterAssessment(t *testing.T) {
	tests := []struct {
		name        string
		status      string
		output      string
		wantSuccess bool
	}{
		{name: "absent", status: "absent", wantSuccess: false},
		{name: "deferred", status: "Stopped", wantSuccess: false},
		{name: "different current", status: "Running", output: `{"state":"current","actual":"` + testRuntimeOldDigest + `","desired":"` + testRuntimeOldDigest + `"}`, wantSuccess: false},
		{name: "already desired current", status: "Running", output: `{"state":"current","actual":"` + testRuntimeNewDigest + `","desired":"` + testRuntimeNewDigest + `"}`, wantSuccess: true},
		{name: "changed stale actual", status: "Running", output: `{"state":"stale","actual":"` + testRuntimeNewDigest + `","desired":"` + testRuntimeNewDigest + `"}`, wantSuccess: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime := profileRuntimeFixture(t, "#!/bin/sh\nprintf '%s\\n' \"$OUTPUT\"\n")
			if test.status == "absent" {
				runtime.Incus = &testkit.Incus{}
			} else {
				runtime.Incus = &testkit.Incus{Instances: map[string]ports.InstanceInfo{
					"subyard/yard": {Status: test.status},
				}}
			}
			runtime.Environment = []string{"OUTPUT=" + test.output}
			err := runtime.ApplyProfileRuntime(context.Background(), "synthetic-runtime", "op-12345678", testRuntimeOldDigest, testRuntimeNewDigest)
			if test.wantSuccess && err != nil {
				t.Fatalf("unchanged current observation failed: %v", err)
			}
			if !test.wantSuccess && err == nil {
				t.Fatal("changed observation was accepted")
			}
		})
	}
}

func TestProvisionRefreshesProfileRuntimesAfterBaseBeforeIntegrationHooks(t *testing.T) {
	for _, failure := range []string{"", "base", "runtime"} {
		t.Run("failure="+failure, func(t *testing.T) {
			root := testkit.TempDir(t)
			handler := `#!/bin/sh
set -eu
[ -e "$BASE_READY" ]
case "$1" in
  observe)
    if [ -e "$RUNTIME_READY" ]; then
      printf '{"state":"current","actual":"%s","desired":"%s"}\n' "$NEW" "$NEW"
    else
      printf '{"state":"stale","actual":"%s","desired":"%s"}\n' "$OLD" "$NEW"
    fi
    ;;
  apply)
    [ "$FAIL_STAGE" != runtime ]
    : > "$RUNTIME_READY"
    printf '{"state":"current","actual":"%s","desired":"%s"}\n' "$NEW" "$NEW"
    ;;
esac
`
			runtime := profileRuntimeFixtureAt(t, root, handler)
			for path, content := range map[string]string{
				"scripts/04-provision-subyard.sh":   "#!/bin/sh\nset -eu\n[ \"$FAIL_STAGE\" != base ]\n: > \"$BASE_READY\"\n",
				"config/projects-changed.sh":        "#!/bin/sh\nexit 0\n",
				"scripts/reconcile-integrations.sh": "#!/bin/sh\nexit 0\n",
			} {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(root, path), []byte(content), 0o700)
			}
			ready := filepath.Join(root, "runtime-ready")
			executor := &retryableIntegrationExecutor{pending: true, hookReady: ready}
			runtime.Executor = executor
			incus := runtime.Incus.(*testkit.Incus)
			incus.Reconcile = ports.ReconcileState{InstanceFound: true, Instance: incus.Instances["subyard/yard"]}
			runtime.Environment = []string{
				"BASE_READY=" + filepath.Join(root, "base-ready"), "RUNTIME_READY=" + ready,
				"FAIL_STAGE=" + failure, "OLD=" + testRuntimeOldDigest, "NEW=" + testRuntimeNewDigest,
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

func profileRuntimeFixture(t *testing.T, handler string) Runtime {
	t.Helper()
	return profileRuntimeFixtureAt(t, testkit.TempDir(t), handler)
}

func profileRuntimeFixtureAt(t *testing.T, root, handler string) Runtime {
	t.Helper()
	profileRoot := filepath.Join(root, "config", "profiles", "synthetic")
	if err := os.MkdirAll(profileRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(profileRoot, "profile.json"), []byte(`{"schema_version":1,"runtime":{"activation_id":"synthetic-runtime","handler":"runtime.sh"}}`), 0o600)
	testkit.WriteFile(t, filepath.Join(profileRoot, "runtime.sh"), []byte(handler), 0o700)
	yard := domain.Context{IncusProject: "subyard", YardInstanceName: "yard"}
	return Runtime{
		RepositoryRoot: root,
		Incus: &testkit.Incus{Instances: map[string]ports.InstanceInfo{
			"subyard/yard": {Status: "Running"},
		}},
		Yard: yard,
	}
}

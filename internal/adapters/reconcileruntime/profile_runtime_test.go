package reconcileruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/incusclient"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestProfileRuntimeColdInstallationPreservesDependencyLoss(t *testing.T) {
	for _, scenario := range []string{"cold", "retained", "installed", "custom socket"} {
		t.Run(scenario, func(t *testing.T) {
			runtime := profileRuntimeFixture(t, "#!/bin/sh\nexit 9\n")
			root := testkit.TempDir(t)
			t.Setenv("PATH", root)
			t.Setenv("INCUS_DIR", filepath.Join(root, "incus-state"))
			t.Setenv("INCUS_SOCKET", "")
			runtime.Incus = incusclient.New("")
			switch scenario {
			case "retained":
				if err := os.Mkdir(filepath.Join(root, "incus-state"), 0700); err != nil {
					t.Fatal(err)
				}
			case "installed":
				testkit.WriteFile(t, filepath.Join(root, "incus"), []byte("#!/bin/sh\nexit 9\n"), 0700)
			case "custom socket":
				runtime.Incus = incusclient.New(filepath.Join(root, "missing.socket"))
			}
			observations, err := runtime.ObserveProfileRuntimes(context.Background())
			if err == nil {
				t.Fatalf("dependency loss treated as absent: %#v", observations)
			}
			observation, presenceErr := runtime.ObserveProfileRuntime(context.Background(), "synthetic-runtime")
			if scenario == "cold" {
				if presenceErr != nil || observation.State != ports.RuntimeStateAbsent {
					t.Fatalf("verified cold presence not absent: %#v %v", observation, presenceErr)
				}
			} else if presenceErr == nil {
				t.Fatal("retained/ambiguous installation accepted as absent")
			}
			plan, err := runtime.PrepareProfileRuntimes(context.Background(), true, false)
			if scenario == "cold" {
				if err != nil || !plan.unavailable || plan.approved["synthetic-runtime"].State != ports.RuntimeStateDeferred {
					t.Fatalf("approved cold prerequisite was not deferred: %#v %v", plan, err)
				}
			} else if err == nil {
				t.Fatal("ambiguous dependency loss accepted as an approved cold prerequisite")
			}
		})
	}
}

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

func TestObserveSingleProfileRuntimeSkipsUnrelatedHooksAndPreservesUnavailable(t *testing.T) {
	runtime := profileRuntimeFixture(t, "#!/bin/sh\nprintf '{\"state\":\"absent\"}\\n'\n")
	other := filepath.Join(runtime.RepositoryRoot, "config/profiles/unrelated")
	if err := os.MkdirAll(other, 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(other, "profile.json"), []byte(`{"schema_version":1,"runtime":{"activation_id":"unrelated-runtime","handler":"runtime.sh"}}`), 0600)
	testkit.WriteFile(t, filepath.Join(other, "runtime.sh"), []byte("#!/bin/sh\nexit 9\n"), 0700)
	observation, err := runtime.ObserveProfileRuntime(context.Background(), "synthetic-runtime")
	if err != nil || observation.State != ports.RuntimeStateAbsent {
		t.Fatalf("single owner observation failed: %#v %v", observation, err)
	}
	if _, err := runtime.ObserveProfileRuntimes(context.Background()); err == nil {
		t.Fatal("unrelated failing hook fixture was not exercised")
	}
	runtime.Incus = &testkit.Incus{Err: errors.New("native unavailable")}
	if _, err := runtime.ObserveProfileRuntime(context.Background(), "synthetic-runtime"); err == nil {
		t.Fatal("unavailable native transport accepted as absence")
	}
	if _, err := runtime.ObserveProfileRuntime(context.Background(), "missing-runtime"); err == nil {
		t.Fatal("unknown owner accepted as absence")
	}
}

func TestObserveProfileRuntimeThroughPinnedRepository(t *testing.T) {
	for _, handler := range []string{"runtime.sh", "hooks/runtime.sh"} {
		t.Run(handler, func(t *testing.T) {
			runtime := profileRuntimeFixture(t, `#!/bin/sh
set -eu
[ -r ./profile.json ]
[ -r "$SUBYARD_REPOSITORY_ROOT/config/profiles/synthetic/profile.json" ]
printf '{"state":"absent","actual":"","desired":""}\n'
`)
			profileRoot := filepath.Join(runtime.RepositoryRoot, "config", "profiles", "synthetic")
			if handler != "runtime.sh" {
				if err := os.Mkdir(filepath.Join(profileRoot, "hooks"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(filepath.Join(profileRoot, "runtime.sh"), filepath.Join(profileRoot, handler)); err != nil {
					t.Fatal(err)
				}
				declaration := fmt.Sprintf(`{"schema_version":1,"runtime":{"activation_id":"synthetic-runtime","handler":%q}}`, handler)
				testkit.WriteFile(t, filepath.Join(profileRoot, "profile.json"), []byte(declaration), 0o600)
			}
			pin, err := os.Open(runtime.RepositoryRoot)
			if err != nil {
				t.Fatal(err)
			}
			defer pin.Close()
			for _, owner := range []string{"self", fmt.Sprint(os.Getpid())} {
				runtime.RepositoryRoot = fmt.Sprintf("/proc/%s/fd/%d", owner, pin.Fd())
				runtime.Environment = []string{"SUBYARD_REPOSITORY_ROOT=" + runtime.RepositoryRoot}
				observations, err := runtime.ObserveProfileRuntimes(context.Background())
				if err != nil || observations["synthetic-runtime"].State != ports.RuntimeStateAbsent {
					t.Fatalf("pinned observations = %#v, %v", observations, err)
				}
			}
		})
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
		{name: "absent ownership without current", output: `{"state":"absent","ownership_absent":true}`},
		{name: "deferred ownership", output: `{"state":"deferred","ownership_absent":true}`},
		{name: "stale ownership", output: `{"state":"stale","actual":"` + testRuntimeOldDigest + `","desired":"` + testRuntimeNewDigest + `","ownership_absent":true}`},
		{name: "unbound ownership", output: `{"state":"current","ownership_absent":true}`},
		{name: "wrong ownership type", output: `{"state":"current","actual":"` + testRuntimeNewDigest + `","desired":"` + testRuntimeNewDigest + `","ownership_absent":"yes"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeProfileRuntimeObservation([]byte(test.output)); err == nil {
				t.Fatal("invalid handler output was accepted")
			}
		})
	}
}

func TestObserveProfileRuntimeBoundsOutputAndUsesStrictDecoder(t *testing.T) {
	for _, output := range []string{
		`{"state":"absent","actual":"","desired":"","extra":true}`,
		`{"state":"absent","actual":"","desired":""}` + strings.Repeat(" ", profileRuntimeOutputLimit),
	} {
		runtime := profileRuntimeFixture(t, "#!/bin/sh\nprintf '%s' \"$OUTPUT\"\n")
		runtime.Environment = []string{"OUTPUT=" + output}
		if _, err := runtime.ObserveProfileRuntimes(context.Background()); err == nil {
			t.Fatal("invalid native output accepted")
		}
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
      printf '{"state":"current","actual":"%s","desired":"%s","ownership_absent":true}\n' "$NEW" "$NEW"
    fi
    ;;
  apply)
    printf '%s\n' "$*" > "$LOG"
    [ "$2" = "$OPERATION" ] && [ "$3" = "$OLD" ] && [ "$4" = "$NEW" ]
    printf current > "$STATE"
    printf '{"state":"current","actual":"%s","desired":"%s","ownership_absent":true}\n' "$NEW" "$NEW"
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
	previewLoopbackFixture(t)
	for _, failure := range []string{"", "route", "network", "base", "runtime"} {
		t.Run("failure="+failure, func(t *testing.T) {
			root := testkit.TempDir(t)
			handler := `#!/bin/sh
set -eu
[ -e "$BASE_READY" ]
[ -e "$AGENT_READY" ]
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
			runtime.Yard.YardKind = domain.YardVM
			policy := &networkPolicyFixture{}
			if failure == "network" {
				policy.ensureErr = errors.New("fixture preview ingress error")
			}
			runtime.NetworkPolicy = policy
			for path, content := range map[string]string{
				"scripts/prepare-preview-route.sh": `#!/bin/sh
set -eu
[ "$1" = 127.0.0.1 ]
[ "$FAIL_STAGE" != route ]
: > "$ROUTE_READY"
`,
				"scripts/04-provision-subyard.sh": `#!/bin/sh
set -eu
[ "$1" = --yes ]
[ -n "$SUBYARD_PREVIEW_ENDPOINT" ]
[ -e "$ROUTE_READY" ]
[ ! -e "$BASE_READY" ]
[ "$FAIL_STAGE" != base ]
: > "$AGENT_READY"
: > "$BASE_READY"
`,
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
			instance := incus.Instances["subyard/yard"]
			instance.LocalConfig = map[string]string{"user.subyard.preview_proxy": "v1:pending:100.101.102.103:32222"}
			incus.Instances["subyard/yard"] = instance
			incus.Reconcile = ports.ReconcileState{InstanceFound: true, Instance: incus.Instances["subyard/yard"]}
			runtime.Environment = []string{
				"BASE_READY=" + filepath.Join(root, "base-ready"), "RUNTIME_READY=" + ready,
				"AGENT_READY=" + filepath.Join(root, "agent-ready"),
				"ROUTE_READY=" + filepath.Join(root, "route-ready"),
				"FAIL_STAGE=" + failure, "OLD=" + testRuntimeOldDigest, "NEW=" + testRuntimeNewDigest,
				"SUBYARD_OPERATION_ID=op-12345678",
			}
			err := runtime.ApplyStage(context.Background(), ports.ReconcileStageProvision)
			wantEnsure := 1
			if failure == "route" {
				wantEnsure = 0
			}
			if len(policy.ensured) != wantEnsure {
				t.Fatalf("network reconciliation calls=%d want=%d", len(policy.ensured), wantEnsure)
			}
			if failure == "route" || failure == "network" {
				if _, err := os.Stat(filepath.Join(root, "base-ready")); !os.IsNotExist(err) {
					t.Fatal("guest provisioning ran before preview route/network readiness")
				}
			}
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

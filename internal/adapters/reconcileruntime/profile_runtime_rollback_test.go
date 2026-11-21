package reconcileruntime

import (
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

// rollbackRuntimeFixture declares a format-2 runtime that prepares format 1.
// Its state file holds the installed format; rollback-* report against 1.
func rollbackRuntimeFixture(t *testing.T) (Runtime, string) {
	t.Helper()
	state := filepath.Join(testkit.TempDir(t), "format")
	testkit.WriteFile(t, state, []byte("2\n"), 0o600)
	runtime := profileRuntimeFixture(t, `#!/bin/sh
set -eu
state=`+state+`
v1=`+testRuntimeOldDigest+`
v2=`+testRuntimeNewDigest+`
current=$(cat "$state")
printf '%s\n' "$*" >>"$state.calls"
case "$1" in
observe) [ "$current" = 2 ] && printf '{"state":"current","actual":"%s","desired":"%s"}\n' "$v2" "$v2" || printf '{"state":"stale","actual":"%s","desired":"%s"}\n' "$v1" "$v2" ;;
rollback-observe) [ "$current" = 1 ] && printf '{"state":"current","actual":"%s","desired":"%s"}\n' "$v1" "$v1" || printf '{"state":"stale","actual":"%s","desired":"%s"}\n' "$v2" "$v1" ;;
rollback-apply) printf '1\n' >"$state"; printf '{"state":"current","actual":"%s","desired":"%s"}\n' "$v1" "$v1" ;;
*) exit 9 ;;
esac
`)
	declaration := `{"schema_version":1,"runtime":{"activation_id":"synthetic-runtime","handler":"runtime.sh","format":2,"prepares_rollback_to":1}}`
	testkit.WriteFile(t, filepath.Join(runtime.RepositoryRoot, "config/profiles/synthetic/profile.json"), []byte(declaration), 0o600)
	return runtime, state
}

func TestProfileRuntimeRollbackActionsConvertAndVerifyLikeApply(t *testing.T) {
	runtime, state := rollbackRuntimeFixture(t)
	ctx := context.Background()
	observation, err := runtime.ObserveProfileRuntimeRollback(ctx, "synthetic-runtime")
	if err != nil || observation.State != ports.RuntimeStateStale || observation.Actual != testRuntimeNewDigest || observation.Desired != testRuntimeOldDigest {
		t.Fatalf("rollback observation = %#v, %v", observation, err)
	}
	for _, changed := range [][3]string{{testRuntimeOldDigest, testRuntimeOldDigest, ""}, {observation.Actual, observation.Desired, "restart another service"}} {
		if err := runtime.ApplyProfileRuntimeRollback(ctx, "synthetic-runtime", "op-1", changed[0], changed[1], changed[2]); !errors.Is(err, domain.ErrPlanStale) {
			t.Fatalf("rollback apply accepted a changed assessment %v: %v", changed, err)
		}
	}
	if err := runtime.ApplyProfileRuntimeRollback(ctx, "synthetic-runtime", "op-1", observation.Actual, observation.Desired, observation.Consequence); err != nil {
		t.Fatal(err)
	}
	if installed, _ := os.ReadFile(state); string(installed) != "1\n" {
		t.Fatalf("installed format = %q", installed)
	}
	// Repeating the converged apply is a verified no-op, and the ordinary
	// observe now reports the forward drift that yard migrate repairs.
	if err := runtime.ApplyProfileRuntimeRollback(ctx, "synthetic-runtime", "op-2", observation.Actual, observation.Desired, ""); err != nil {
		t.Fatal(err)
	}
	forward, err := runtime.ObserveProfileRuntime(ctx, "synthetic-runtime")
	if err != nil || forward.State != ports.RuntimeStateStale {
		t.Fatalf("forward observation = %#v, %v", forward, err)
	}
	calls, _ := os.ReadFile(state + ".calls")
	if got := strings.Count(string(calls), "rollback-apply op-1 "+testRuntimeNewDigest+" "+testRuntimeOldDigest); got != 1 || strings.Contains(string(calls), "\napply") {
		t.Fatalf("hook calls:\n%s", calls)
	}
}

func TestProfileRuntimeRollbackActionsRequireADeclaredFormatAndRunningYard(t *testing.T) {
	runtime := profileRuntimeFixture(t, "#!/bin/sh\nexit 9\n")
	if _, err := runtime.ObserveProfileRuntimeRollback(context.Background(), "synthetic-runtime"); err == nil {
		t.Fatal("rollback-observe ran for a runtime that prepares no format")
	}
	if err := runtime.ApplyProfileRuntimeRollback(context.Background(), "synthetic-runtime", "op", testRuntimeOldDigest, testRuntimeNewDigest, ""); err == nil {
		t.Fatal("rollback-apply ran for a runtime that prepares no format")
	}
	prepared, _ := rollbackRuntimeFixture(t)
	prepared.Incus = &testkit.Incus{Instances: map[string]ports.InstanceInfo{"subyard/yard": {Status: "Stopped"}}}
	observation, err := prepared.ObserveProfileRuntimeRollback(context.Background(), "synthetic-runtime")
	if err != nil || observation.State != ports.RuntimeStateDeferred {
		t.Fatalf("stopped rollback observation = %#v, %v", observation, err)
	}
}

// A running guest that init never provisioned has nothing installed to
// prepare, so rollback-observe reports it absent instead of deferred.
func TestProfileRuntimeRollbackObservesANeverProvisionedGuestAsAbsent(t *testing.T) {
	prepared, _ := rollbackRuntimeFixture(t)
	prepared.Incus = &testkit.Incus{Instances: map[string]ports.InstanceInfo{"subyard/yard": {Status: "Running",
		Config: map[string]string{"user.subyard.managed": "true", "user.subyard.initialized": "false"}}}}
	prepared.Executor = &neverInitializedGuest{}
	observation, err := prepared.ObserveProfileRuntimeRollback(context.Background(), "synthetic-runtime")
	if err != nil || observation.State != ports.RuntimeStateAbsent {
		t.Fatalf("never-provisioned rollback observation = %#v, %v", observation, err)
	}
}

// A failing rollback-apply keeps its owner's public refusal and next step;
// any other stderr stays private.
func TestProfileRuntimeRollbackApplyKeepsAnOwnersPublicRefusal(t *testing.T) {
	for _, test := range []struct {
		name, stderr, message, retry string
	}{
		{"public refusal", `subyard-profile-observation-refusal-v1 {"message":"sample store unavailable","retry":"run yard -Y default config paths"}`, "sample store unavailable", "run yard -Y default config paths"},
		{"private text", `sample store unavailable: /home/owner/secret`, "", ""},
		{"no executable step", `subyard-profile-observation-refusal-v1 {"message":"sample store unavailable","retry":"ask the owner"}`, "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, _ := rollbackRuntimeFixture(t)
			script := `#!/bin/sh
case "$1" in
rollback-observe) printf '{"state":"stale","actual":"` + testRuntimeNewDigest + `","desired":"` + testRuntimeOldDigest + `"}\n' ;;
rollback-apply) printf '%s\n' '` + test.stderr + `' >&2; exit 2 ;;
*) exit 9 ;;
esac
`
			testkit.WriteFile(t, filepath.Join(runtime.RepositoryRoot, "config/profiles/synthetic/runtime.sh"), []byte(script), 0o700)
			err := runtime.ApplyProfileRuntimeRollback(context.Background(), "synthetic-runtime", "op-1", testRuntimeNewDigest, testRuntimeOldDigest, "")
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("rollback apply error = %v", err)
			}
			var diagnostic interface{ ActivationDiagnostic() (string, string) }
			if !errors.As(err, &diagnostic) {
				if test.retry != "" {
					t.Fatalf("owner refusal lost: %v", err)
				}
				return
			}
			if message, retry := diagnostic.ActivationDiagnostic(); message != test.message || retry != test.retry {
				t.Fatalf("owner refusal = %q, %q", message, retry)
			}
		})
	}
}

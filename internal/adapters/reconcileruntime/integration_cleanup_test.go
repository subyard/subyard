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

func cleanupRuntimeFixture(t *testing.T, steps []testkit.IncusExecStep) (Runtime, *testkit.Incus, string) {
	t.Helper()
	hook := filepath.Join(t.TempDir(), "cleanup.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n# arbitrary integration cleanup\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	instance := ports.InstanceInfo{Status: "Running"}
	incus := &testkit.Incus{
		Reconcile: ports.ReconcileState{InstanceFound: true, Instance: instance},
		ExecSteps: steps,
	}
	runtime := Runtime{
		Environment: []string{
			"CODING_TOOL_INTEGRATIONS=",
			"AGENT_widget_CLEANUP=" + hook,
			"DEV_USER=developer",
		},
		Incus: incus, Executor: incus,
		Yard: domain.Context{
			IncusProject: "fixture-project", YardInstanceName: "fixture-yard",
			DevUser: "developer", DevUID: 1234,
		},
	}
	return runtime, incus, hook
}

func cleanupObservation(fingerprint string, changed bool, steps ...string) ports.InstanceExecResult {
	consequences := `[]`
	if len(steps) != 0 {
		quoted := make([]string, len(steps))
		for index, step := range steps {
			quoted[index] = `"` + step + `"`
		}
		consequences = `[` + strings.Join(quoted, ",") + `]`
	}
	return ports.InstanceExecResult{Stdout: []byte(`{"fingerprint":"` + fingerprint + `","changed":` +
		map[bool]string{true: "true", false: "false"}[changed] + `,"steps":` + consequences + `}`)}
}

func TestIntegrationCleanupRejectsInvalidReportsWithoutRawStderr(t *testing.T) {
	secret := "private-cleanup-stderr-token"
	for _, test := range []struct {
		name   string
		result ports.InstanceExecResult
		want   string
	}{
		{name: "malformed observation", result: ports.InstanceExecResult{Stdout: []byte(`not-json`), Stderr: []byte(secret)}, want: "invalid observation"},
		{name: "unknown observation field", result: ports.InstanceExecResult{Stdout: []byte(`{"fingerprint":"` + strings.Repeat("a", 64) + `","changed":false,"steps":[],"extra":true}`), Stderr: []byte(secret)}, want: "invalid observation"},
		{name: "invalid failure diagnostic", result: ports.InstanceExecResult{ExitCode: 1, Stdout: []byte(`{"code":"bad code","message":"unsafe"}`), Stderr: []byte(secret)}, want: "without a valid diagnostic"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, _, _ := cleanupRuntimeFixture(t, []testkit.IncusExecStep{{}, {Result: test.result}})
			_, err := runtime.IntegrationCleanupPlan(context.Background(), "widget")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("cleanup error exposed raw stderr: %v", err)
			}
		})
	}
}

func TestIntegrationCleanupHandlesIncusExitErrorWithoutTransportLeak(t *testing.T) {
	secret := "private-incus-exec-error"
	for _, test := range []struct {
		name   string
		result ports.InstanceExecResult
		want   string
	}{
		{
			name: "valid hook diagnostic",
			result: ports.InstanceExecResult{ExitCode: 1,
				Stdout: []byte(`{"code":"ownership-conflict","message":"service ownership is not proven"}`),
				Stderr: []byte(secret)},
			want: "ownership-conflict: service ownership is not proven",
		},
		{
			name:   "transport failure",
			result: ports.InstanceExecResult{Stderr: []byte(secret)},
			want:   "could not be observed or completed",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, _, _ := cleanupRuntimeFixture(t, []testkit.IncusExecStep{
				{}, {Result: test.result, Err: errors.New(secret)},
			})
			_, err := runtime.IntegrationCleanupPlan(context.Background(), "widget")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("cleanup error exposed Incus error or stderr: %v", err)
			}
		})
	}
}

func TestIntegrationCleanupApplyRejectsStalePlanOrSource(t *testing.T) {
	for _, scenario := range []string{"observation", "source"} {
		t.Run(scenario, func(t *testing.T) {
			initial := cleanupObservation(strings.Repeat("a", 64), true, "Archive widget service")
			fresh := initial
			if scenario == "observation" {
				fresh = cleanupObservation(strings.Repeat("b", 64), true, "Archive widget service")
			}
			runtime, incus, hook := cleanupRuntimeFixture(t, []testkit.IncusExecStep{
				{}, {Result: initial}, {}, {Result: fresh},
			})
			plan, err := runtime.IntegrationCleanupPlan(context.Background(), "widget")
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "source" {
				if err := os.WriteFile(hook, []byte("#!/bin/sh\n# changed cleanup source\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err = runtime.ApplyIntegrationCleanup(context.Background(), "widget", plan)
			if !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("stale %s accepted: %v", scenario, err)
			}
			for _, call := range incus.ExecCalls {
				if len(call.Request.Command) > 4 && call.Request.Command[4] == "apply" {
					t.Fatalf("stale %s reached apply", scenario)
				}
			}
		})
	}
}

func TestIntegrationCleanupObservesAppliesAndReobserves(t *testing.T) {
	changed := cleanupObservation(strings.Repeat("a", 64), true, "Archive widget service")
	converged := cleanupObservation(strings.Repeat("b", 64), false)
	runtime, incus, _ := cleanupRuntimeFixture(t, []testkit.IncusExecStep{
		{}, {Result: changed}, // Initial observation.
		{}, {Result: changed}, // Apply stale-plan recheck.
		{},                      // Hook apply.
		{}, {Result: converged}, // Post-apply verification.
	})
	plan, err := runtime.IntegrationCleanupPlan(context.Background(), "widget")
	if err != nil || !plan.Changed || len(plan.Steps) != 1 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	if err := runtime.ApplyIntegrationCleanup(context.Background(), "widget", plan); err != nil {
		t.Fatal(err)
	}
	modes := []string{}
	for _, call := range incus.ExecCalls {
		if len(call.Request.Command) > 4 && call.Request.Command[0] == "sh" && call.Request.Command[2] == "-s" {
			modes = append(modes, call.Request.Command[4])
			if string(call.Request.Stdin) != "#!/bin/sh\n# arbitrary integration cleanup\n" {
				t.Fatal("cleanup source was not delivered through stdin")
			}
		}
	}
	if got := strings.Join(modes, " "); got != "observe observe apply observe" {
		t.Fatalf("cleanup hook modes = %q", got)
	}
}

func TestIntegrationCleanupRejectsSelectedIntegrationBeforeGuestAccess(t *testing.T) {
	runtime, incus, _ := cleanupRuntimeFixture(t, nil)
	runtime.Environment[0] = "CODING_TOOL_INTEGRATIONS=widget"
	_, err := runtime.IntegrationCleanupPlan(context.Background(), "widget")
	if err == nil || !strings.Contains(err.Error(), "still selected") {
		t.Fatalf("selected integration cleanup error=%v", err)
	}
	if len(incus.ExecCalls) != 0 {
		t.Fatal("selected integration reached guest cleanup")
	}
}

package reconcileruntime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

func provisionDiagnosticFixture(t *testing.T) (Runtime, *testkit.Incus, func() []testkit.IncusExecStep) {
	t.Helper()
	previewLoopbackFixture(t)
	rules := filepath.Join(testkit.TempDir(t), "repo.rules")
	testkit.WriteFile(t, rules, []byte("fixture\n"), 0o600)
	effective, err := (guestConfigFile{source: rules}).readSource()
	if err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(effective))
	steps := func() []testkit.IncusExecStep {
		return []testkit.IncusExecStep{
			{}, {}, {Result: ports.InstanceExecResult{Stdout: []byte("dev:x:1000:1000::/home/dev:/bin/bash\n")}},
			{Result: ports.InstanceExecResult{Stdout: []byte("ccusage 1.2.3\n")}},
			{Result: ports.InstanceExecResult{Stdout: []byte(`{"files":[{"destination":"/home/dev/.config/opencode/repo.rules","token":"sha256:` + digest + `"}]}`)}},
			{Result: ports.InstanceExecResult{Stdout: []byte("skills-current\n")}},
			{}, {Result: ports.InstanceExecResult{ExitCode: 1}, Err: errors.New("not a link")}, {}, {}, {}, {},
		}
	}
	incus := &testkit.Incus{
		ServerInfo: ports.ServerInfo{Environment: "incus"},
		Reconcile: ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{
			Status: "Running", Config: map[string]string{"user.subyard.ccusage_version": "1.2.3"},
		}},
	}
	runtime := Runtime{
		Integrations: []profile.IntegrationDefinition{{Name: "opencode", Artifacts: []profile.IntegrationArtifact{{ID: "rules", Kind: "config", Destination: ".config/opencode/repo.rules", Format: "raw"}}}},
		Incus:        incus, Executor: incus,
		RepositoryRoot: "../../..",
		Yard:           domain.Context{YardName: "default", IncusProject: "subyard", YardInstanceName: "yard", DevUser: "dev"},
		Environment: []string{
			"CODING_TOOL_INTEGRATIONS=opencode", "CCUSAGE_VERSION=1.2.3",
			"AGENT_opencode_RULES=" + rules, "AGENT_opencode_RULES_DEST=.config/opencode/repo.rules",
		},
	}
	bindArtifactTestPackages(t, &runtime, filepath.Dir(rules))
	return runtime, incus, steps
}

func TestProvisionDiagnosticNamesEveryUnmetCondition(t *testing.T) {
	runtime, incus, steps := provisionDiagnosticFixture(t)
	drift := testkit.IncusExecStep{Result: ports.InstanceExecResult{ExitCode: 1}, Err: &ports.GuestExitError{Code: 1}}
	unmet := func() []testkit.IncusExecStep {
		scripted := steps()
		scripted[1], scripted[3] = drift, drift
		scripted[11] = testkit.IncusExecStep{Result: ports.InstanceExecResult{ExitCode: 1, Stderr: []byte(
			"guest-disk-tmp: guest temporary policy file missing: /etc/subyard-guest-tmp-age.conf\n" +
				"unprefixed guest text\n" +
				"guest-disk-tmp: guest temporary policy unit is not active: subyard-tmp-age.timer\n")},
			Err: &ports.GuestExitError{Code: 1}}
		return scripted
	}

	// Ordinary observation keeps expected drift a state and stops at the first.
	incus.ExecSteps, incus.ExecCalls = unmet(), nil
	ready, err := runtime.CheckStage(context.Background(), ports.ReconcileStageProvision)
	if ready || err != nil || len(incus.ExecCalls) != 2 {
		t.Fatalf("observation: ready=%v err=%v calls=%d", ready, err, len(incus.ExecCalls))
	}

	incus.ExecSteps, incus.ExecCalls = unmet(), nil
	ready, err = runtime.VerifyStageWithDiagnostic(context.Background(), ports.ReconcileStageProvision)
	var diagnostic interface{ ActivationDiagnostic() (string, string) }
	if ready || !errors.As(err, &diagnostic) || len(incus.ExecCalls) != 12 {
		t.Fatalf("diagnostic: ready=%v err=%v calls=%d", ready, err, len(incus.ExecCalls))
	}
	message, next := diagnostic.ActivationDiagnostic()
	want := "yard default: provision did not converge: " +
		"agent commands are missing or Docker forwarding is not allowed; " +
		"installed ccusage is not version 1.2.3; " +
		"guest /tmp: guest temporary policy file missing: /etc/subyard-guest-tmp-age.conf; " +
		"guest temporary policy unit is not active: subyard-tmp-age.timer"
	if message != want || !strings.Contains(next, "yard -Y default init") {
		t.Fatalf("diagnostic:\n got %q\nwant %q\nnext %q", message, want, next)
	}

	// An inspection failure is not drift. Observation returns it at once; the
	// diagnostic returns it first and still names the independent conditions,
	// skipping only the checks that need the home directory it could not read.
	transport := errors.New("native transport unavailable")
	incus.ExecSteps, incus.ExecCalls = steps(), nil
	incus.ExecSteps[2] = testkit.IncusExecStep{Err: transport}
	if ready, err := runtime.CheckStage(context.Background(), ports.ReconcileStageProvision); ready || !errors.Is(err, transport) || len(incus.ExecCalls) != 3 {
		t.Fatalf("observed transport: ready=%v err=%v calls=%d", ready, err, len(incus.ExecCalls))
	}
	incus.ExecSteps, incus.ExecCalls = unmet(), nil
	incus.ExecSteps[1] = testkit.IncusExecStep{Err: transport}
	ready, err = runtime.VerifyStageWithDiagnostic(context.Background(), ports.ReconcileStageProvision)
	if ready || !errors.Is(err, transport) || !errors.As(err, &diagnostic) || len(incus.ExecCalls) != 12 ||
		!strings.HasPrefix(err.Error(), "native transport unavailable; yard default: provision did not converge: installed ccusage is not version 1.2.3; guest /tmp: ") {
		t.Fatalf("diagnostic transport: ready=%v err=%v calls=%d", ready, err, len(incus.ExecCalls))
	}

	// A memory-backed /tmp alone keeps its reboot instruction.
	incus.ExecSteps, incus.ExecCalls = steps(), nil
	incus.ExecSteps[11] = testkit.IncusExecStep{Result: ports.InstanceExecResult{ExitCode: 10}, Err: &ports.GuestExitError{Code: 10}}
	ready, err = runtime.VerifyStageWithDiagnostic(context.Background(), ports.ReconcileStageProvision)
	var reboot diskTmpRebootError
	if ready || !errors.As(err, &reboot) || !strings.Contains(err.Error(), "yard -Y default stop") {
		t.Fatalf("reboot: ready=%v err=%v", ready, err)
	}

	incus.ExecSteps, incus.ExecCalls = steps(), nil
	if ready, err := runtime.VerifyStageWithDiagnostic(context.Background(), ports.ReconcileStageProvision); !ready || err != nil {
		t.Fatalf("converged: ready=%v err=%v", ready, err)
	}
}

func TestProvisionDiagnosticNamesStoppedConditions(t *testing.T) {
	runtime, incus, _ := provisionDiagnosticFixture(t)
	incus.Reconcile.Instance = ports.InstanceInfo{Status: "Stopped", Config: map[string]string{
		"user.subyard.managed": "true", "user.subyard.initialized": "true", "user.subyard.ccusage_version": "1.2.2",
	}}
	ready, err := runtime.VerifyStageWithDiagnostic(context.Background(), ports.ReconcileStageProvision)
	want := "yard default: provision did not converge: " +
		"the guest /tmp policy marker is not installed; " +
		"the installed static preview helper differs; " +
		"the installed static preview endpoint differs; " +
		"the yard is stopped without an intentional stop; " +
		"the recorded ccusage version is pending or stale"
	if ready || err == nil || err.Error() != want || len(incus.ExecCalls) != 0 {
		t.Fatalf("stopped diagnostic: ready=%v calls=%d\n got %v\nwant %s", ready, len(incus.ExecCalls), err, want)
	}
}

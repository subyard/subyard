package migration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestRegisteredGuestTmpRequiresSeparateLateConsent(t *testing.T) {
	for _, scenario := range []string{"decline", "eof", "apply-fails", "transport-ten", "stopped-decline", "stopped-complete"} {
		t.Run(scenario, func(t *testing.T) {
			root := testkit.TempDir(t)
			if err := os.Mkdir(filepath.Join(root, "scripts"), 0700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(root, "scripts/guest-disk-tmp.sh"), []byte("policy source\n"), 0600)
			instance := ports.InstanceInfo{Project: "fixture", Name: "yard", Status: "Running", Config: map[string]string{"volatile.uuid": "uuid-original", "user.subyard.desired_power": "running"}}
			if scenario == "stopped-decline" || scenario == "stopped-complete" {
				instance.Status = "Stopped"
				instance.Config["user.subyard.desired_power"] = "stopped"
			}
			if scenario == "stopped-complete" {
				instance.Config["user.subyard.disk_tmp"] = "v1"
			}
			native := &testkit.Incus{Instances: map[string]ports.InstanceInfo{"fixture/yard": instance}, ExecSteps: []testkit.IncusExecStep{{}, {Result: ports.InstanceExecResult{ExitCode: 10}, Err: &ports.GuestExitError{Code: 10}}}}
			if scenario == "apply-fails" {
				native.ExecSteps = []testkit.IncusExecStep{{Result: ports.InstanceExecResult{ExitCode: 1}, Err: &ports.GuestExitError{Code: 1}}}
			}
			if scenario == "transport-ten" {
				native.ExecSteps[1].Err = errors.New("transport unavailable")
			}
			runtime := reconcileruntime.Runtime{RepositoryRoot: root, Incus: native, Executor: native, Yard: domain.Context{YardName: "default", IncusProject: "fixture", YardInstanceName: "yard"}}
			asks := 0
			err := migrateRegisteredDiskTmp(context.Background(), runtime, func(request domain.ConfirmationRequest) (bool, error) {
				asks++
				if request.Default != domain.ConfirmationDefaultNo || len(request.Consequences) == 0 {
					t.Fatal("late consent is not explicit")
				}
				if scenario == "eof" {
					return false, domain.ErrConfirmationRequired
				}
				return false, nil
			})
			expectedAsks := 1
			if scenario == "apply-fails" || scenario == "transport-ten" || scenario == "stopped-complete" {
				expectedAsks = 0
			}
			if asks != expectedAsks || len(native.PowerUpdates) != 0 || len(native.ConfigUpdates) != 0 {
				t.Fatalf("late boundary asks=%d power=%#v metadata=%#v", asks, native.PowerUpdates, native.ConfigUpdates)
			}
			if scenario == "stopped-complete" {
				if err != nil || len(native.ExecCalls) != 0 {
					t.Fatalf("completed stopped yard was touched: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("incomplete migration marked complete")
			}
			if strings.Contains(err.Error(), "%!w") {
				t.Fatal("nil cause was formatted as wrapped error", err)
			}
			if scenario == "decline" && (!errors.Is(err, domain.ErrOperationDeclined) || len(native.ExecCalls) != 2) {
				t.Fatalf("policy not applied before separate refusal: %v calls=%d", err, len(native.ExecCalls))
			}
			if scenario == "eof" && !errors.Is(err, domain.ErrConfirmationRequired) {
				t.Fatal(err)
			}
			if native.Instances["fixture/yard"].Config["volatile.uuid"] != "uuid-original" || native.Instances["fixture/yard"].Config["user.subyard.desired_power"] != instance.Config["user.subyard.desired_power"] {
				t.Fatal("migration changed identity or intent")
			}
		})
	}
}

type pendingTmpAssessment struct {
	*registeredNativeFixture
	step        testkit.IncusExecStep
	assessments int
	onlyInitial bool
}

func (n *pendingTmpAssessment) Exec(ctx context.Context, project, name string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	if request.Command[len(request.Command)-1] == "assess" {
		n.assessments++
		if n.onlyInitial && n.assessments > 1 {
			return ports.InstanceExecResult{}, errors.New("guest boot is not ready for a fresh safety assessment")
		}
		return n.step.Result, n.step.Err
	}
	return n.registeredNativeFixture.Exec(ctx, project, name, request)
}

func TestGuestTmpFreshnessDoesNotProbeGuestBeforeBootReadiness(t *testing.T) {
	_, owner, native, _ := pairedRegisteredOwners(t)
	check := &pendingTmpAssessment{registeredNativeFixture: native, onlyInitial: true}
	load := owner.LoadTargets
	owner.LoadTargets = func(ctx context.Context, view releasetransition.V2SettingsSnapshotView) ([]RegisteredYardTarget, error) {
		targets, err := load(ctx, view)
		for i := range targets {
			targets[i].Runtime.Executor = check
		}
		return targets, err
	}
	observed, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
	if err != nil || observed.Converged {
		t.Fatalf("pre-journal assessment: %#v %v", observed, err)
	}
	if len(observed.Consequences) != 1 || !strings.Contains(observed.Consequences[0], "clear /tmp") || !strings.Contains(observed.Consequences[0], "stops and boots") {
		t.Error("policy future clearing is not disclosed", observed.Consequences)
	}
	if err := owner.Reconcile(context.Background(), releasetransition.ReleaseLinks{}); err != nil {
		t.Fatal("freshness guard probed boot readiness", err)
	}
	if check.assessments != 1 || native.grants != 1 {
		t.Fatalf("assessments=%d grants=%d", check.assessments, native.grants)
	}
}
func TestPendingGuestTmpAssessesSafetyBeforeAuthorization(t *testing.T) {
	for _, scenario := range []string{"disk", "memory", "unsafe", "transport", "mismatch", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			_, owner, native, _ := pairedRegisteredOwners(t)
			check := &pendingTmpAssessment{registeredNativeFixture: native}
			if scenario != "disk" {
				check.step.Result.ExitCode = 10
				check.step.Err = &ports.GuestExitError{Code: 10}
			}
			switch scenario {
			case "unsafe":
				check.step.Result.ExitCode = 1
				check.step.Err = &ports.GuestExitError{Code: 1}
			case "transport":
				check.step.Err = errors.New("transport unavailable")
			case "mismatch":
				check.step.Err = &ports.GuestExitError{Code: 1}
			case "cancelled":
				check.step.Err = errors.Join(context.Canceled, check.step.Err)
			}
			load := owner.LoadTargets
			owner.LoadTargets = func(ctx context.Context, view releasetransition.V2SettingsSnapshotView) ([]RegisteredYardTarget, error) {
				targets, err := load(ctx, view)
				for i := range targets {
					targets[i].Runtime.Executor = check
				}
				return targets, err
			}
			_, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
			if (err == nil) != (scenario == "disk" || scenario == "memory") || check.assessments != 1 || native.grants != 0 {
				t.Fatalf("assessment: err=%v calls=%d grants=%d", err, check.assessments, native.grants)
			}
			for _, call := range native.ExecCalls {
				if call.Request.Command[len(call.Request.Command)-1] == "apply" {
					t.Fatal("inspection applied policy")
				}
			}
		})
	}
}

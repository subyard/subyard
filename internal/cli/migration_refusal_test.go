package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/testyardmigration"
)

func TestReleaseTransitionWritesFilteredMigrationDiagnostics(t *testing.T) {
	root := testkit.TempDir(t)
	home, runtimeRoot := filepath.Join(root, "config"), filepath.Join(root, "runtime")
	registry, err := os.ReadFile(filepath.Join(repositoryRoot(t), "config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	const target = "0.29.0-test-aaaaaaaaaaaa"
	digest := writeSealedRollbackTarget(t, runtimeRoot, target, "0.29.0-test", registry)
	if err := os.Symlink("releases/"+target, filepath.Join(runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "yards", "e2e-yard", "config.env")
	writeReleaseTransitionTestFile(t, path, []byte("SSH_PORT=2240\n"), 0600)
	request, err := json.Marshal(releasetransition.ProcessRequest{
		SchemaVersion: 1, Mode: releasetransition.ProcessInspect,
		RuntimeRoot: runtimeRoot, ConfigHome: home, Yard: "default",
		Target: target, Direction: releasetransition.DirectionActivateTarget, ArtifactDigest: digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: repositoryRoot(t), Incus: &testkit.Incus{},
		Environment: []string{"HOME=" + root, "SUBYARD_HOME=" + filepath.Join(root, "data"), "SUBYARD_CONFIG_HOME=" + home},
		Stdin:       bytes.NewReader(request), Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	code := program.runReleaseTransition(context.Background(), nil)
	if code != 0 || !strings.Contains(stderr.String(), "release transition refusal: reserved yard name e2e-yard:") || !strings.Contains(stderr.String(), "yards/e2e-yard/config.env") || !strings.Contains(stderr.String(), "YARD_TEMPLATE=test-vms") || strings.Contains(stderr.String(), "SSH_PORT=2240") {
		t.Fatalf("filtered migration diagnostic did not reach command stderr (exit %d): %s", code, stderr.String())
	}
	var response releasetransition.ProcessResponse
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil || response.Inspection == nil || response.Inspection.Outcome == nil {
		t.Fatalf("missing structured inspection: %s %v", stdout.String(), err)
	}
	plan := response.Inspection
	if plan.Outcome.Status != releasetransition.StatusOperatorActionRequired || plan.Outcome.Code != releasetransition.CodePreconditionBlocked || len(plan.Blockers) == 0 || !strings.Contains(plan.Outcome.Message, "yards/e2e-yard/config.env") || !strings.Contains(plan.Outcome.Retry, "YARD_TEMPLATE=test-vms") || strings.Contains(stdout.String(), "SSH_PORT=2240") {
		t.Fatalf("owner verdict lost its structured public repair: %#v", plan)
	}
}

// The error comes from the real registration adapter; the synthetic port only
// seeds an authorized historical row without running a host mutation.
type diagnosticRegistrationOwner struct {
	before  releasetransition.OwnerRegistrationObservation
	failure error
}

func (owner *diagnosticRegistrationOwner) Prepare(context.Context, releasetransition.V2SettingsSnapshotView) (releasetransition.OwnerRegistrationObservation, error) {
	return owner.before, owner.failure
}
func (owner *diagnosticRegistrationOwner) Observe(context.Context, releasetransition.OwnerRegistrationObservation) (releasetransition.OwnerRegistrationProgress, error) {
	return releasetransition.OwnerRegistrationExpected, owner.failure
}
func (*diagnosticRegistrationOwner) Commit(context.Context, releasetransition.OwnerRegistrationObservation) error {
	return errors.New("interrupt before native owner effects")
}

func TestReservedOwnerMigrationPublicInspectionAndUpdate(t *testing.T) {
	for _, yard := range []string{"e2e-yard", "test-yard"} {
		for _, resume := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/resume=%t", yard, resume), func(t *testing.T) {
				root := testkit.TempDir(t)
				home := filepath.Join(root, "config")
				path := filepath.Join(home, "yards", yard, "config.env")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, []byte("YARD_TEMPLATE=private-unrelated-value\n"), 0600)
				realOwner := &ownerRegistrationTransition{options: func() (testyardmigration.Options, error) {
					return testyardmigration.Options{Executable: filepath.Join(root, "yard"), ConfigHome: home, DataHome: filepath.Join(root, "data")}, nil
				}}
				_, failure := realOwner.Prepare(context.Background(), nil)
				if failure == nil {
					t.Fatal("reserved registration was accepted")
				}
				digest := releasetransition.Fingerprint(strings.Repeat("a", 64))
				owner := &diagnosticRegistrationOwner{before: releasetransition.OwnerRegistrationObservation{State: releasetransition.OwnerRegistrationLegacyDirectory, Registration: digest, Overrides: digest, Controller: digest}}
				if !resume {
					owner.failure = failure
				}
				payload := []byte(`{"schemaVersion":2,"minimumEpochs":{"owner-registration":1},"currentEpochs":{"owner-registration":2},"migrations":[{"id":"owner-v2","domain":"owner-registration","fromEpoch":1,"toEpoch":2,"kind":"test-yard-owner-v1-to-v2"}]}`)
				transition, err := releasetransition.NewV2Transition(releasetransition.V2Options{ConfigHome: home, RegistryPayload: payload, ArtifactDigest: digest, Releases: releasetransition.ReleasePair{From: "release-a", Target: "release-a"}, ObserveLinks: func(context.Context) (releasetransition.ReleaseLinks, error) {
					return releasetransition.ReleaseLinks{Active: "release-a"}, nil
				}, OwnerRegistration: owner, VerifyAuthorization: func(releasetransition.PlanToken, releasetransition.Authorization) bool { return true }})
				if err != nil {
					t.Fatal(err)
				}
				goal := releasetransition.Goal{Target: "release-a", Direction: releasetransition.DirectionActivateTarget}
				if resume {
					plan, err := transition.Inspect(context.Background(), goal)
					if err != nil || len(plan.Blockers) != 0 {
						t.Fatalf("seed inspection: %#v %v", plan, err)
					}
					result, err := transition.Converge(context.Background(), releasetransition.Execution{Plan: plan.Plan, Authorization: "confirmed"})
					if err != nil || result.Status != releasetransition.StatusRecovering {
						t.Fatalf("seed interruption: %#v %v", result, err)
					}
					owner.failure = failure
				}
				plan, err := transition.Inspect(context.Background(), goal)
				if err != nil || plan.Outcome == nil || len(plan.Blockers) == 0 {
					t.Fatalf("operator inspection: %#v %v", plan, err)
				}
				if err := releasetransition.ValidateProcessInspection(goal, plan); err != nil {
					t.Fatal(err)
				}
				if !resume && plan.Outcome.Code != releasetransition.CodePreconditionBlocked {
					t.Fatalf("reserved name is not a precondition: %#v", plan.Outcome)
				}
				encoded, err := json.Marshal(plan)
				if err != nil {
					t.Fatal(err)
				}
				for _, text := range []string{string(encoded), plan.Outcome.Message + " " + plan.Outcome.Retry} {
					if !strings.Contains(text, "yards/"+yard+"/config.env") || !strings.Contains(text, "YARD_TEMPLATE=test-vms") || strings.Contains(text, "private-unrelated-value") || strings.Contains(text, "install a compatible release") {
						t.Fatalf("operator lost public repair: %s", text)
					}
				}
				result, err := transition.Converge(context.Background(), releasetransition.Execution{Plan: plan.Plan, Authorization: "confirmed"})
				if err != nil || result.Status != releasetransition.StatusOperatorActionRequired || !strings.Contains(result.Message, "yards/"+yard+"/config.env") || !strings.Contains(result.Retry, "YARD_TEMPLATE=test-vms") {
					t.Fatalf("update lost public repair: %#v %v", result, err)
				}
			})
		}
	}
}

func TestRegisteredMigrationCompactsNamedRefusalsWithoutLosingSrv(t *testing.T) {
	for _, keys := range []int{8, 25} {
		t.Run(fmt.Sprint(keys), func(t *testing.T) {
			_, execution, native := adoptionFixture(t)
			owner, _, _ := registeredRefusalOwner(t, execution, native)
			instance := &native.Reconcile.Instance
			for index := 0; index < keys; index++ {
				key := fmt.Sprintf("foreign.option%d", index)
				instance.Config[key], instance.LocalConfig[key] = "private-value", "private-value"
			}
			instance.LocalDevices["srv"]["source"], instance.Devices["srv"]["source"] = "private-volume", "private-volume"
			payload := []byte(`{"schemaVersion":2,"minimumEpochs":{"owner-registration":2},"currentEpochs":{"owner-registration":3},"migrations":[{"id":"registered-v3","domain":"owner-registration","fromEpoch":2,"toEpoch":3,"kind":"registered-yards-v2-to-v3"}]}`)
			transition, err := releasetransition.NewV2Transition(releasetransition.V2Options{ConfigHome: testkit.TempDir(t), RegistryPayload: payload, ArtifactDigest: releasetransition.Fingerprint(strings.Repeat("a", 64)), Releases: releasetransition.ReleasePair{From: "release-a", Target: "release-a"}, ObserveLinks: func(context.Context) (releasetransition.ReleaseLinks, error) {
				return releasetransition.ReleaseLinks{Active: "release-a"}, nil
			}, RegisteredYards: []releasetransition.V2ActivationReconciler{owner}, VerifyAuthorization: func(releasetransition.PlanToken, releasetransition.Authorization) bool { return true }})
			if err != nil {
				t.Fatal(err)
			}
			goal := releasetransition.Goal{Target: "release-a", Direction: releasetransition.DirectionActivateTarget}
			var stderr bytes.Buffer
			plan, err := transition.Inspect(releasetransition.WithMigrationDiagnosticWriter(context.Background(), &stderr), goal)
			if err != nil || plan.Outcome == nil || plan.Outcome.Code != releasetransition.CodePreconditionBlocked {
				t.Fatalf("named verdicts escaped: %#v %v", plan, err)
			}
			message := plan.Outcome.Message
			if len(message) > 512 || !strings.Contains(message, "srv device") || !strings.Contains(message, "persistent data") || strings.Count(message, "unsupported instance configuration key") != 1 || strings.Contains(message, "private") {
				t.Fatalf("crowded/unsafe public message: %q", message)
			}
			for index := 0; index < keys; index++ {
				if !strings.Contains(stderr.String(), fmt.Sprintf("foreign.option%d", index)) {
					t.Fatalf("missing key: %q", message)
				}
			}
			if err := releasetransition.ValidateProcessInspection(goal, plan); err != nil {
				t.Fatal(err)
			}
			if keys == 25 && (!strings.Contains(message, "(25)") || !strings.Contains(message, "stderr of yard update --check")) {
				t.Fatalf("overflow lost the class/count or real detail location: %s", message)
			}
			if strings.Contains(stderr.String(), "private-value") || strings.Contains(stderr.String(), "private-volume") {
				t.Fatalf("unfiltered diagnostic stream: %s", stderr.String())
			}
		})
	}
}
func TestLegacyKVMTransportFailureRemainsDependency(t *testing.T) {
	_, execution, native, _ := legacyContinuationInstalledFixture(t)
	instance := &native.Reconcile.Instance
	device := map[string]string{"type": "unix-char", "source": "/dev/kvm", "path": "/dev/kvm", "mode": "0660", "gid": "108"}
	instance.LocalDevices["kvm"], instance.Devices["kvm"] = device, device
	transport := errors.New("transport unavailable")
	native.ExecSteps = []testkit.IncusExecStep{{Err: transport}}
	runtime := execution.platform.(reconcileruntime.Runtime)
	runtime.Executor = native
	_, _, err := runtime.InspectLegacyContinuation(context.Background())
	if !errors.Is(err, transport) {
		t.Fatalf("transport cause lost: %v", err)
	}
	var verdict interface{ MigrationRefusal() bool }
	if errors.As(err, &verdict) && verdict.MigrationRefusal() {
		t.Fatalf("transport became verdict: %v", err)
	}
}

func TestLegacyPublicVerdictsNameFieldsAndDevices(t *testing.T) {
	for _, scenario := range []string{"identity", "unsupported-device", "missing-device"} {
		t.Run(scenario, func(t *testing.T) {
			_, execution, native, _ := legacyContinuationInstalledFixture(t)
			runtime := execution.platform.(reconcileruntime.Runtime)
			instance := &native.Reconcile.Instance
			want := "user.subyard.name"
			if scenario == "identity" {
				instance.Config[want], instance.LocalConfig[want] = "private-value", "private-value"
			} else if scenario == "unsupported-device" {
				want = "foreign-device"
				device := map[string]string{"type": "disk", "source": "/private-value", "path": "/private-value"}
				instance.LocalDevices[want], instance.Devices[want] = device, device
			} else {
				want = "ssh"
				delete(instance.LocalDevices, want)
				delete(instance.Devices, want)
			}
			_, _, err := runtime.InspectLegacyContinuation(context.Background())
			var verdict interface {
				MigrationRefusal() bool
				ActivationDiagnostic() (string, string)
			}
			if !errors.As(err, &verdict) || !verdict.MigrationRefusal() {
				t.Fatalf("missing verdict: %v", err)
			}
			message, _ := verdict.ActivationDiagnostic()
			if !strings.Contains(message, want) || strings.Contains(message, "private-value") {
				t.Fatalf("unsafe/missing identity: %q", message)
			}
		})
	}
}

func TestMigrationDirectDiagnosticFiltersUnsafeNames(t *testing.T) {
	failure := domain.MigrationStateRefusal("default", errors.New("private-value"), "unsupported device", "/outside/private-name", "safe-device")
	diagnostic := failure.(interface{ ActivationDiagnostic() (string, string) })
	message, retry := diagnostic.ActivationDiagnostic()
	if strings.Contains(message+retry, "private") || strings.Contains(message, "/outside") || !strings.Contains(message, "safe-device") || !strings.Contains(message, "unrecognized name") {
		t.Fatalf("direct unsafe-name filter failed: %q / %q", message, retry)
	}
}

// Every printed step is followed through the engine: below the anchor the
// installed release finishes its own journal first; an activated anchor journal is
// replaced by the candidate. No step names a release that sends the user back.
func TestInterruptedAnchorHopHasNoRecoveryCommandCycle(t *testing.T) {
	for _, state := range []string{"authorized", "reconciling", "rollback"} {
		t.Run(state, func(t *testing.T) {
			root := testkit.TempDir(t)
			home, runtimeRoot := filepath.Join(root, "config-home"), filepath.Join(root, "runtime")
			if err := os.MkdirAll(home, 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("SUBYARD_HOME", root)
			registry := []byte(`{"schemaVersion":2,"minimumEpochs":{"settings":1},"currentEpochs":{"settings":1},"migrations":[]}`)
			writeReleaseTransitionTestFile(t, filepath.Join(root, "config/release-transition.json"), registry, 0600)
			parsedRegistry, _, err := releasetransition.ParseRegistryV2(registry, releasetransition.BuiltinCapabilityCatalog())
			if err != nil {
				t.Fatal(err)
			}
			ledger, _, err := releasetransition.MarshalLedgerV2(releasetransition.BaselineLedgerV2(parsedRegistry), parsedRegistry)
			if err != nil {
				t.Fatal(err)
			}
			writeReleaseTransitionTestFile(t, filepath.Join(home, "release-transition/v2/ledger.json"), ledger, 0600)
			anchorVersion := releaseruntime.UpdateAnchorVersion
			candidateVersion := "0.29.0-test"
			rollback := state == "rollback"
			oldVersion := "0.27.0"
			old, anchor, candidate := releasetransition.ReleaseID(oldVersion+"-aaaaaaaaaaaa"), releasetransition.ReleaseID(anchorVersion+"-bbbbbbbbbbbb"), releasetransition.ReleaseID(candidateVersion+"-cccccccccccc")
			oldDigest := writeSealedRollbackTarget(t, runtimeRoot, string(old), oldVersion, registry)
			anchorDigest := writeSealedRollbackTarget(t, runtimeRoot, string(anchor), anchorVersion, registry)
			candidateDigest := writeSealedRollbackTarget(t, runtimeRoot, string(candidate), candidateVersion, registry)
			from, target, direction, artifact := old, anchor, releasetransition.DirectionActivateTarget, anchorDigest
			if rollback {
				from, target, direction, artifact = anchor, old, releasetransition.DirectionActivatePrevious, oldDigest
			}
			if err := os.Symlink("releases/"+string(from), filepath.Join(runtimeRoot, "current")); err != nil {
				t.Fatal(err)
			}
			if rollback {
				if err := os.Symlink("releases/"+string(target), filepath.Join(runtimeRoot, "previous")); err != nil {
					t.Fatal(err)
				}
			}
			savedVersion := Version
			Version = anchorVersion
			t.Cleanup(func() { Version = savedVersion })
			owner := releasetransition.ProcessRequest{SchemaVersion: 1, Mode: releasetransition.ProcessInspect, RuntimeRoot: runtimeRoot, ConfigHome: home, Target: target, Direction: direction, ArtifactDigest: artifact}
			reconciler := &processDriftReconciler{failOnce: true}
			run := func(request releasetransition.ProcessRequest) releasetransition.ProcessResponse {
				t.Helper()
				response, err := executeReleaseTransitionRequest(context.Background(), root, request, func(releasetransition.PlanToken, releasetransition.Authorization) bool { return true }, []releasetransition.V2ActivationReconciler{reconciler}, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				return response
			}
			plan := run(owner)
			owner.Mode = releasetransition.ProcessConverge
			owner.Execution = &releasetransition.Execution{Plan: plan.Inspection.Plan, Authorization: "confirmed"}
			// Author the historical transaction without the candidate's fresh-admission
			// guard: the anchor release permits both the forward hop and this compatible rollback.
			interrupted := run(owner)
			if interrupted.Outcome == nil || interrupted.Outcome.Status != releasetransition.StatusRecovering {
				t.Fatalf("no interruption: %#v", interrupted)
			}
			store, err := releasetransition.NewPOSIXV2Store(home)
			if err != nil {
				t.Fatal(err)
			}
			before, err := store.ReadCurrentJournal()
			if err != nil {
				t.Fatal(err)
			}
			journal, err := releasetransition.ParseJournal(before.Payload)
			if err != nil {
				t.Fatal(err)
			}
			preactivation := state == "authorized"
			if preactivation {
				journal.Checkpoint = releasetransition.JournalAuthorized
				for _, link := range []string{"current", "previous"} {
					if err := os.Remove(filepath.Join(runtimeRoot, link)); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Symlink("releases/"+string(from), filepath.Join(runtimeRoot, "current")); err != nil {
					t.Fatal(err)
				}
				payload, err := releasetransition.MarshalJournal(journal)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.CompareAndSwapCurrentJournal(before, payload); err != nil {
					t.Fatal(err)
				}
				before, err = store.ReadCurrentJournal()
				if err != nil {
					t.Fatal(err)
				}
			}
			request := releasetransition.ProcessRequest{SchemaVersion: 1, Mode: releasetransition.ProcessInspect, RuntimeRoot: runtimeRoot, ConfigHome: home, Target: candidate, Direction: releasetransition.DirectionActivateTarget, ArtifactDigest: candidateDigest}
			Version = candidateVersion
			refusal := releaseruntime.CheckSourcePolicy(context.Background(), request)
			if preactivation || rollback {
				next := "yard update && yard update"
				if rollback {
					next = "yard update && yard update --version " + anchorVersion + " && yard update"
				}
				if refusal == nil || !strings.Contains(refusal.Error(), "next: "+next) || strings.Count(refusal.Error(), "next:") != 1 {
					t.Fatalf("unsafe continuation %q: %v", next, refusal)
				}
				// Step 1: plain yard update on the installed release resumes its own journal.
				if err := releaseruntime.CheckSourcePolicy(context.Background(), owner); err != nil {
					t.Fatal(err)
				}
				Version = anchorVersion
				owner.Mode, owner.Execution = releasetransition.ProcessInspect, nil
				resume := run(owner)
				owner.Mode = releasetransition.ProcessConverge
				owner.Execution = &releasetransition.Execution{Plan: resume.Inspection.Plan, Authorization: "confirmed"}
				if finished := run(owner); finished.Outcome == nil || !finished.Outcome.ReachedGoal {
					t.Fatalf("owner made no progress: %#v", finished)
				}
				Version = candidateVersion
				refusal = releaseruntime.CheckSourcePolicy(context.Background(), request)
				if rollback {
					// Step 2: the completed old release updates to the anchor.
					if refusal == nil || !strings.Contains(refusal.Error(), "next: yard update --version "+anchorVersion+" && yard update") || strings.Contains(refusal.Error(), "unfinished") {
						t.Fatalf("completed owner repeated the same step: %v", refusal)
					}
					owner.Direction, owner.Target, owner.ArtifactDigest = releasetransition.DirectionActivateTarget, anchor, anchorDigest
					owner.Mode, owner.Execution, owner.Replacement = releasetransition.ProcessInspect, nil, nil
					Version = anchorVersion
					plan = run(owner)
					owner.Mode = releasetransition.ProcessConverge
					owner.Execution = &releasetransition.Execution{Plan: plan.Inspection.Plan, Authorization: "confirmed"}
					if result := run(owner); result.Outcome == nil || !result.Outcome.ReachedGoal {
						t.Fatalf("anchor hop failed: %#v", result)
					}
					Version = candidateVersion
					refusal = releaseruntime.CheckSourcePolicy(context.Background(), request)
				}
				if refusal != nil {
					t.Fatalf("completed anchor sent back: %v", refusal)
				}
			} else {
				if refusal != nil {
					t.Fatalf("activated anchor rejected the candidate: %v", refusal)
				}
				// Ordinary inspection follows the journal's goal; only an explicit
				// newer selection requests a different goal and replacement.
				ordinary := request
				ordinary.Target = journal.Goal.Target
				inspection := run(ordinary).Inspection
				if inspection == nil || inspection.Outcome == nil || inspection.Outcome.Retry == "" || strings.Contains(inspection.Outcome.Retry, "--version "+anchorVersion) {
					t.Fatalf("plain inspection lost engine recovery: %#v", inspection)
				}
				request.Replacement = &releasetransition.JournalReplacement{Transaction: journal.Transaction, Fingerprint: before.Fingerprint, Reason: releasetransition.JournalReplacementNewerRelease}
			}
			inspection := run(request).Inspection
			if inspection == nil || len(inspection.Blockers) != 0 || inspection.Resume != nil {
				t.Fatalf("next step did not advance: %#v", inspection)
			}
			request.Mode = releasetransition.ProcessConverge
			request.Execution = &releasetransition.Execution{Plan: inspection.Plan, Authorization: "confirmed"}
			completed := run(request)
			if completed.Outcome == nil || !completed.Outcome.ReachedGoal {
				t.Fatalf("candidate failed: %#v", completed)
			}
			if request.Replacement != nil {
				path := filepath.Join(home, "release-transition-replaced-journals/v1", string(journal.Transaction)+"-"+string(before.Fingerprint)+".json")
				preserved, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(preserved, before.Payload) {
					t.Fatalf("journal not preserved: %v", err)
				}
			}
		})
	}
}

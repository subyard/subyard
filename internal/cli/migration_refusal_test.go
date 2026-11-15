package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestReleaseTransitionIngressFactoryRefusesSourceByClass(t *testing.T) {
	for _, class := range []string{"legacy", "runtime", "checkout", "", "unknown"} {
		t.Run(class, func(t *testing.T) {
			program := &CLI{baseEnv: map[string]string{"SUBYARD_SOURCE_INGRESS_V1_CLASS": class}}
			factory := program.releaseTransitionIngressFactory(releasetransition.ProcessRequest{
				SourceIngress: &releasetransition.SourceIngressRequest{SourceRoot: "/checkout"},
			})
			_, err := factory(releasetransition.ReleasePair{})
			want := "entrypoints point at the checkout /checkout; if this is an installation older than v0.11.0, see docs/reference/legacy-migration-support.md"
			if class == "legacy" {
				want = releaseruntime.LegacyReinstallGuidance
			}
			if err == nil || err.Error() != want {
				t.Fatalf("source ingress class %q: %v", class, err)
			}
		})
	}
}

func TestReleaseTransitionIngressFactoryRefusesRuntimeDirectoryWithoutUninstallAdvice(t *testing.T) {
	root := testkit.TempDir(t)
	runtimeRoot := filepath.Join(root, "runtime")
	sourceRoot := filepath.Join(runtimeRoot, "releases", "installed")
	sibling := filepath.Join(root, "runtime-other")
	for _, path := range []string{sourceRoot, sibling} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(root, "runtime-alias")
	if err := os.Symlink(runtimeRoot, alias); err != nil {
		t.Fatal(err)
	}
	sourceAlias := filepath.Join(root, "source-alias")
	if err := os.Symlink(sourceRoot, sourceAlias); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, source, runtime, want string }{
		{"physical", sourceRoot, runtimeRoot, "source ingress path is inside the runtime root"},
		{"runtime-alias", sourceRoot, alias + "/", "source ingress path is inside the runtime root"},
		{"source-alias", sourceAlias, runtimeRoot, "source ingress path is inside the runtime root"},
		{"prefix-sibling", sibling, runtimeRoot, "entrypoints point at the checkout " + sibling + "; if this is an installation older than v0.11.0, see docs/reference/legacy-migration-support.md"},
	} {
		t.Run(test.name, func(t *testing.T) {
			program := &CLI{}
			factory := program.releaseTransitionIngressFactory(releasetransition.ProcessRequest{
				RuntimeRoot:   test.runtime,
				SourceIngress: &releasetransition.SourceIngressRequest{SourceRoot: test.source},
			})
			_, err := factory(releasetransition.ReleasePair{})
			if err == nil || err.Error() != test.want {
				t.Fatalf("source ingress refusal: %v", err)
			}
		})
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
			registry := mustReadConfigFixture(t, filepath.Join(repositoryRoot(t), "config/release-transition.json"))
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
			hopVersion := releaseruntime.UpdateHopVersion
			candidateVersion := "0.29.0-test"
			rollback := state == "rollback"
			oldVersion := "0.27.0"
			old, hop, candidate := releasetransition.ReleaseID(oldVersion+"-aaaaaaaaaaaa"), releasetransition.ReleaseID(hopVersion+"-bbbbbbbbbbbb"), releasetransition.ReleaseID(candidateVersion+"-cccccccccccc")
			oldDigest := writeSealedRollbackTarget(t, runtimeRoot, string(old), oldVersion, registry)
			hopDigest := writeSealedRollbackTarget(t, runtimeRoot, string(hop), hopVersion, registry)
			candidateDigest := writeSealedRollbackTarget(t, runtimeRoot, string(candidate), candidateVersion, registry)
			from, target, direction, artifact := old, hop, releasetransition.DirectionActivateTarget, hopDigest
			if rollback {
				from, target, direction, artifact = hop, old, releasetransition.DirectionActivatePrevious, oldDigest
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
			Version = hopVersion
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
					next = "yard update && yard update --version " + hopVersion + " && yard update"
				}
				if refusal == nil || !strings.Contains(refusal.Error(), "next: "+next) || strings.Count(refusal.Error(), "next:") != 1 {
					t.Fatalf("unsafe continuation %q: %v", next, refusal)
				}
				// Step 1: plain yard update on the installed release resumes its own journal.
				if err := releaseruntime.CheckSourcePolicy(context.Background(), owner); err != nil {
					t.Fatal(err)
				}
				Version = hopVersion
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
					if refusal == nil || !strings.Contains(refusal.Error(), "next: yard update --version "+hopVersion+" && yard update") || strings.Contains(refusal.Error(), "unfinished") {
						t.Fatalf("completed owner repeated the same step: %v", refusal)
					}
					owner.Direction, owner.Target, owner.ArtifactDigest = releasetransition.DirectionActivateTarget, hop, hopDigest
					owner.Mode, owner.Execution, owner.Replacement = releasetransition.ProcessInspect, nil, nil
					Version = hopVersion
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
				if inspection == nil || inspection.Outcome == nil || inspection.Outcome.Retry == "" || strings.Contains(inspection.Outcome.Retry, "--version "+hopVersion) {
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

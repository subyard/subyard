package cli

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

func TestRollbackSourceActivationPreservesNativeScopeBeforeEffects(t *testing.T) {
	{
		t.Run("settings-only", func(t *testing.T) {
			fixture := newSourceScopeFixture(t)
			writeConfigCommandFile(t, filepath.Join(fixture.configHome, "yards/named/config.env"), "AGENT_claude_CONFIG='"+fixture.namedAsset+"'\n")
			writeConfigCommandFile(t, filepath.Join(fixture.configHome, ".sync/settings/config.env"), "SSH_PORT=2240\n")
			writeConfigCommandFile(t, filepath.Join(fixture.configHome, ".sync/settings/yards/named/config.env"), "SSH_PORT=2241\n")
			request := fixture.request
			request.Target = "0.25.1-aaaaaaaaaaaa"
			request.Direction = releasetransition.DirectionActivatePrevious
			// Keep this current-engine characterization on the exact v0.25.1
			// registry (tag commit 6352f8ce1daaaf1c47336b63a54654ddcb3ed9c7),
			// before registered native owners were introduced in v0.25.2.
			registry, err := os.ReadFile(filepath.Join("testdata", "release-transition-v0.25.1.json"))
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprintf("%x", sha256.Sum256(registry)) != "e4d70df389a0eaac35bd309f864f69c5609a9e3b395e69334441187a96d49d20" {
				t.Fatal("historical rollback registry differs from its frozen tag bytes")
			}
			writeCLIFile(t, filepath.Join(fixture.root, "config", "release-transition.json"), string(registry), 0600)
			request.ArtifactDigest = writeSealedRollbackTarget(t, request.RuntimeRoot, string(request.Target), "0.25.1", registry)
			if err := os.Remove(filepath.Join(request.RuntimeRoot, "current")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("releases/release-b", filepath.Join(request.RuntimeRoot, "current")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("releases/"+string(request.Target), filepath.Join(request.RuntimeRoot, "previous")); err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(fixture.root, "fake-bin")
			if err := os.MkdirAll(bin, 0700); err != nil {
				t.Fatal(err)
			}
			incus := "#!/bin/sh\ncase \"$*\" in 'project list --format=json') printf '[]\\n';; *) exit 2;; esac\n"
			writeCLIFile(t, filepath.Join(bin, "incus"), incus, 0700)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			beforeOwner, err := fixture.program.legacyOwnerRegistration(request).Prepare(context.Background(), nil)
			if err != nil || beforeOwner.State != releasetransition.OwnerRegistrationAbsent {
				t.Fatalf("native owner fixture has wrong initial state: %+v err=%v", beforeOwner, err)
			}
			rollback, _, err := releaseruntime.VerifyRollbackTarget(context.Background(), request.RuntimeRoot, request.Target, request.ArtifactDigest)
			if err != nil {
				t.Fatal(err)
			}
			links, err := releasetransition.NewRuntimeLinkStore(request.RuntimeRoot)
			if err != nil {
				t.Fatal(err)
			}
			pair := releasetransition.ReleasePair{From: "release-b", Previous: &request.Target, Target: request.Target}
			goal := releasetransition.Goal{Target: request.Target, Direction: request.Direction}
			pause := errors.New("pause before native rollback activation")
			grants, attempts := 0, 0
			var resumed releasetransition.Inspection
			var transaction releasetransition.TransactionID
			var scope releasetransition.Fingerprint
			var freshTransition func() *releasetransition.V2Transition
			freshTransition = func() *releasetransition.V2Transition {
				actors := slices.DeleteFunc(fixture.program.activationReconcilers(request), func(actor releasetransition.V2ActivationReconciler) bool {
					return !slices.Contains([]string{"materialized-config", "test-vm-broker"}, actor.ID())
				})
				if len(actors) != 2 {
					t.Fatal("missing production rollback activation owners", len(actors))
				}
				options := candidateTransitionOptions(request, releasetransition.V2Options{
					ConfigHome: request.ConfigHome, Releases: pair, Direction: request.Direction,
					CandidateConfigDir: filepath.Join(fixture.root, "config"),
					OperatorHome:       environmentValue(fixture.program.options.Environment, "SUBYARD_OPERATOR_HOME"),
					RollbackTarget:     rollback, RegistryPayload: registry, ArtifactDigest: request.ArtifactDigest,
					InheritedSettingIDs: request.InheritedSettingIDs, Reconcilers: actors,
					OwnerRegistration: fixture.program.legacyOwnerRegistration(request),
					ObserveLinks:      func(context.Context) (releasetransition.ReleaseLinks, error) { return links.Observe() },
					VerifyAuthorization: func(releasetransition.PlanToken, releasetransition.Authorization) bool {
						grants++
						return grants == 1
					},
					ActivateLinks: func(ctx context.Context, _ releasetransition.ReleasePair) (releasetransition.ReleaseLinks, error) {
						attempts++
						snapshot, err := fixture.store.ReadCurrentJournal()
						if err != nil {
							t.Fatal(err)
						}
						journal, err := releasetransition.ParseJournal(snapshot.Payload)
						if err != nil || journal.Checkpoint != releasetransition.JournalActivationIntent {
							t.Fatalf("source work did not reach durable activation intent: %v %v", journal.Checkpoint, err)
						}
						settingsChanged := false
						for _, step := range journal.Steps {
							if step.Decision == releasetransition.DecisionCanonicalize && !strings.HasPrefix(step.Resource, "ledger.") && strings.Contains(string(step.Migration), "settings") {
								settingsChanged = true
								if step.Checkpoint != releasetransition.StepVerified {
									t.Fatal("source settings were not verified before activation")
								}
							}
						}
						if !settingsChanged {
							t.Fatal("rollback fixture did not perform pending native settings work")
						}
						if attempts == 1 {
							transaction, scope = journal.Transaction, journal.ObservationScope
							resumed, err = freshTransition().Inspect(ctx, goal)
							if err != nil || len(resumed.Blockers) != 0 || resumed.Resume == nil || *resumed.Resume != transaction || resumed.Plan == "" {
								t.Fatalf("post-source native targets do not reconstruct the preauthorized scope %s: inspection=%+v err=%v", scope, resumed, err)
							}
						} else if journal.Transaction != transaction || journal.ObservationScope != scope {
							t.Fatal("same-transaction resume changed the durable activation grant")
						}
						if len(fixture.boundary.applied) != 0 || len(fixture.program.options.InitPlatform.(*initPlatformFixture).applied) != 0 {
							t.Fatal("activation effects ran before rollback scope verification")
						}
						observed, err := links.Observe()
						if err != nil || observed.Active != pair.From || observed.Previous == nil || *observed.Previous != pair.Target {
							t.Fatal("rollback links changed before native activation", observed, err)
						}
						return observed, pause
					},
				})
				transition, err := releasetransition.NewV2Transition(options)
				if err != nil {
					t.Fatal(err)
				}
				return transition
			}
			inspection, err := freshTransition().Inspect(context.Background(), goal)
			if err != nil || len(inspection.Blockers) != 0 || inspection.Plan == "" {
				t.Fatalf("inspect native rollback with pending source work: %+v %v", inspection, err)
			}
			outcome, err := freshTransition().Converge(context.Background(), releasetransition.Execution{Plan: inspection.Plan, Authorization: "confirmed"})
			if err != nil || outcome.Status != releasetransition.StatusRecovering || outcome.Code != releasetransition.CodeVerificationFailed || attempts != 1 || grants != 1 {
				t.Fatalf("rollback did not stop at the pre-effect native boundary: activation_attempts=%d grants=%d outcome=%+v err=%v", attempts, grants, outcome, err)
			}
			outcome, err = freshTransition().Converge(context.Background(), releasetransition.Execution{Plan: resumed.Plan})
			if err != nil || outcome.Status != releasetransition.StatusRecovering || outcome.Code != releasetransition.CodeVerificationFailed || attempts != 2 || grants != 1 {
				t.Fatalf("fresh native rollback resume required new consent: activation_attempts=%d grants=%d err=%v", attempts, grants, err)
			}
		})
	}
}

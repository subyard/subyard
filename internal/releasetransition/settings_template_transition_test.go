package releasetransition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestV2SettingsMigrationPreservesSupportedNonTargetTemplateAcrossResume(t *testing.T) {
	for _, test := range []struct {
		name             string
		fault            string
		interruptedEpoch int
	}{
		{name: "complete", interruptedEpoch: 2},
		{name: "resume after settings CAS", fault: "after-settings-cas", interruptedEpoch: 1},
		{name: "resume after settings ledger advance", fault: "after-ledger-cas", interruptedEpoch: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			activeFault := test.fault
			transition, configHome, targetPath := v2TransitionFixture(t, func(point string) error {
				if activeFault != "" && point == activeFault {
					return errors.New("interrupt settings migration")
				}
				return nil
			})
			const nonTarget = "YARD_TEMPLATE=synthetic-other\nNESTED_E2E_VMS=0\nSSH_PORT=2244\n"
			nonTargetPath := filepath.Join(configHome, "yards", "ordinary", "config.env")
			if err := os.MkdirAll(filepath.Dir(nonTargetPath), 0o700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, nonTargetPath, []byte(nonTarget), 0o600)
			candidateConfigDir := filepath.Join(configHome, "candidate", "config")
			candidateTemplate := filepath.Join(candidateConfigDir, "yards", "profiles", "synthetic-other.env")
			if err := os.MkdirAll(filepath.Dir(candidateTemplate), 0o700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, candidateTemplate, []byte("YARD_KIND=vm\n"), 0o600)
			transition.options.CandidateConfigDir = candidateConfigDir

			authorizationChecks := 0
			transition.options.VerifyAuthorization = func(plan PlanToken, authorization Authorization) bool {
				authorizationChecks++
				return authorization == v2TestAuthorization(plan)
			}
			goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
			initial, err := transition.Inspect(context.Background(), goal)
			if err != nil || len(initial.Blockers) != 0 || initial.Resume != nil {
				t.Fatalf("initial inspection = %#v, err=%v", initial, err)
			}
			outcome, err := transition.Converge(context.Background(), Execution{
				Plan: initial.Plan, Authorization: v2TestAuthorization(initial.Plan),
			})
			if test.fault == "" {
				if err != nil || outcome.Status != StatusReady {
					t.Fatalf("migration outcome = %#v, err=%v", outcome, err)
				}
			} else {
				if err != nil || outcome.Status != StatusRecovering || outcome.Transaction == nil {
					t.Fatalf("interrupted migration = %#v, err=%v", outcome, err)
				}
				journalSnapshot, readErr := transition.store.ReadCurrentJournal()
				journal, parseErr := ParseJournal(journalSnapshot.Payload)
				if readErr != nil || parseErr != nil || journal.Transaction != *outcome.Transaction {
					t.Fatalf("interrupted journal = %#v, read=%v parse=%v", journal, readErr, parseErr)
				}
				assertSettingsTemplateMigrationEpoch(t, transition, test.interruptedEpoch)

				activeFault = ""
				resume, inspectErr := transition.Inspect(context.Background(), goal)
				if inspectErr != nil || resume.Resume == nil || *resume.Resume != journal.Transaction ||
					resume.Plan != journal.ResumePlan {
					t.Fatalf("exact resume inspection = %#v, err=%v, journal=%#v", resume, inspectErr, journal)
				}
				outcome, err = transition.Converge(context.Background(), Execution{Plan: resume.Plan})
				if err != nil || outcome.Status != StatusReady || outcome.Transaction == nil ||
					*outcome.Transaction != journal.Transaction {
					t.Fatalf("resumed migration = %#v, err=%v", outcome, err)
				}
				completedSnapshot, readErr := transition.store.ReadCurrentJournal()
				completed, parseErr := ParseJournal(completedSnapshot.Payload)
				if readErr != nil || parseErr != nil || completed.Transaction != journal.Transaction ||
					completed.AuthorizationPlan != journal.AuthorizationPlan ||
					completed.ResumePlan != journal.ResumePlan {
					t.Fatalf("completed journal changed identity: %#v read=%v parse=%v", completed, readErr, parseErr)
				}
				settingsStepVerified := false
				for _, step := range completed.Steps {
					if step.Migration == "canonicalize-test-vms-settings-v2" && step.Resource == "hermes" &&
						step.Checkpoint == StepVerified {
						settingsStepVerified = true
					}
				}
				if !settingsStepVerified {
					t.Fatal("settings migration step was missing or left incomplete after resume")
				}
			}

			target, err := os.ReadFile(targetPath)
			if err != nil || !strings.Contains(string(target), "YARD_TEMPLATE='test-vms'\n") ||
				strings.Contains(string(target), "NESTED_E2E_VMS") {
				t.Fatalf("target settings did not converge: %q, err=%v", target, err)
			}
			preserved, err := os.ReadFile(nonTargetPath)
			if err != nil || string(preserved) != nonTarget {
				t.Fatalf("non-target settings changed: %q, err=%v", preserved, err)
			}
			if authorizationChecks != 1 {
				t.Fatalf("authorization checks = %d, want initial authorization only", authorizationChecks)
			}
			assertSettingsTemplateMigrationEpoch(t, transition, 2)

			repeat, err := transition.Inspect(context.Background(), goal)
			if err != nil || repeat.Resume != nil || repeat.Assessment.Changed ||
				repeat.Outcome == nil || repeat.Outcome.Status != StatusReady || len(repeat.Decisions) != 0 {
				t.Fatalf("completed migration inspection = %#v, err=%v", repeat, err)
			}
			repeated, err := transition.Converge(context.Background(), Execution{Plan: repeat.Plan})
			if err != nil || repeated.Status != StatusReady || authorizationChecks != 1 {
				t.Fatalf("repeat migration = %#v, err=%v authorization checks=%d", repeated, err, authorizationChecks)
			}
			assertSettingsTemplateMigrationEpoch(t, transition, 2)
		})
	}
}

func assertSettingsTemplateMigrationEpoch(t *testing.T, transition *V2Transition, want int) {
	t.Helper()
	ledgerSnapshot, err := transition.store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	if !ledgerSnapshot.Exists && want == 1 {
		return
	}
	if !ledgerSnapshot.Exists {
		t.Fatalf("settings ledger is absent, want epoch %d", want)
	}
	ledger, _, err := ParseLedgerV2(ledgerSnapshot.Payload, v2TestRegistry(t))
	if err != nil || ledger.Domains["settings"].Epoch != want {
		t.Fatalf("settings epoch = %d, want %d, err=%v", ledger.Domains["settings"].Epoch, want, err)
	}
}

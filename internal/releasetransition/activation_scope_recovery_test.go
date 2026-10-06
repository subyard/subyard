package releasetransition

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestV2InterruptedActivationRequiresOriginalConfigurationBeforeResume(t *testing.T) {
	for _, activationOnly := range []bool{true, false} {
		name := "verified-settings"
		if activationOnly {
			name = "activation-only"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			transition, _, settingsPath := v2TransitionFixture(t, nil)
			reconciler := &v2TestReconciler{}
			transition.options.Reconcilers = []V2ActivationReconciler{reconciler}
			goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
			if activationOnly {
				initial, err := transition.Inspect(ctx, goal)
				if err != nil {
					t.Fatal(err)
				}
				outcome, err := transition.Converge(ctx, Execution{
					Plan: initial.Plan, Authorization: v2TestAuthorization(initial.Plan),
				})
				if err != nil || outcome.Status != StatusReady {
					t.Fatalf("initial convergence: %#v, err=%v", outcome, err)
				}
				transition.options.NewTransactionID = func() TransactionID { return "tx-test-002" }
				reconciler.converged = false
			}
			transition.options.fault = func(point string) error {
				if point == "after-reconciling" {
					return errors.New("interrupted before activation reconciliation")
				}
				return nil
			}
			inspection, err := transition.Inspect(ctx, goal)
			if err != nil {
				t.Fatal(err)
			}
			interrupted, err := transition.Converge(ctx, Execution{
				Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
			})
			if err != nil || interrupted.Status != StatusRecovering {
				t.Fatalf("interrupted convergence: %#v, err=%v", interrupted, err)
			}
			transition.options.fault = nil
			journalBefore, err := transition.store.ReadCurrentJournal()
			if err != nil {
				t.Fatal(err)
			}
			journal, err := ParseJournal(journalBefore.Payload)
			if err != nil || journal.Checkpoint != JournalReconciling ||
				(len(journal.Steps) == 0) != activationOnly {
				t.Fatalf("interrupted journal: %#v, err=%v", journal, err)
			}
			for _, step := range journal.Steps {
				if step.Checkpoint != StepVerified {
					t.Fatalf("migration step was not completed: %#v", step)
				}
			}
			ledgerBefore, err := transition.store.ReadLedger()
			if err != nil {
				t.Fatal(err)
			}
			settingsBefore, err := os.ReadFile(settingsPath)
			if err != nil {
				t.Fatal(err)
			}
			// An unchanged desired scope can resume the original authorized work.
			resume, err := transition.Inspect(ctx, goal)
			if err != nil || resume.Resume == nil || len(resume.Blockers) != 0 {
				t.Fatalf("unchanged-scope inspection: %#v, err=%v", resume, err)
			}
			// A different materialized-config desired fingerprint changes scope.
			reconciler.desired = digestC
			wantCode := CodePlanStale
			if !activationOnly {
				// Even an unrelated supported scalar changes the whole-file receipt.
				changed := bytes.Replace(settingsBefore, []byte("SSH_PORT=2224"), []byte("SSH_PORT=2225"), 1)
				if bytes.Equal(changed, settingsBefore) {
					t.Fatal("fixture did not contain the expected scalar setting")
				}
				testkit.WriteFile(t, settingsPath, changed, 0o600)
				wantCode = CodeMigrationStale
			}
			blocked, err := transition.Inspect(ctx, goal)
			if err != nil || blocked.Outcome == nil ||
				blocked.Outcome.Status != StatusOperatorActionRequired || blocked.Outcome.Code != wantCode {
				t.Fatalf("changed-config inspection: %#v, err=%v", blocked, err)
			}
			foundScope := false
			for _, blocker := range blocked.Blockers {
				foundScope = foundScope || blocker.Code == CodePlanStale && blocker.Resource == "transition.observation-scope"
			}
			if !foundScope {
				t.Fatalf("changed desired did not report its scope blocker: %#v", blocked.Blockers)
			}
			// Supplying another grant cannot bypass an unfinished stale transaction.
			outcome, err := transition.Converge(ctx, Execution{
				Plan: blocked.Plan, Authorization: v2TestAuthorization(blocked.Plan),
			})
			if err != nil || outcome.Status != StatusOperatorActionRequired || outcome.Code != wantCode {
				t.Fatalf("changed-config apply: %#v, err=%v", outcome, err)
			}
			journalAfter, journalErr := transition.store.ReadCurrentJournal()
			ledgerAfter, ledgerErr := transition.store.ReadLedger()
			if journalErr != nil || ledgerErr != nil || !bytes.Equal(journalBefore.Payload, journalAfter.Payload) ||
				!bytes.Equal(ledgerBefore.Payload, ledgerAfter.Payload) {
				t.Fatalf("blocked retry changed durable history: journal=%v ledger=%v", journalErr, ledgerErr)
			}
			t.Logf("changed configuration: status=%s code=%s blockers=%d retry=%s",
				outcome.Status, outcome.Code, len(blocked.Blockers), outcome.Retry)
			// Restoration here uses known fixture inputs, never reconstructed hashes.
			reconciler.desired = digestA
			if !activationOnly {
				testkit.WriteFile(t, settingsPath, settingsBefore, 0o600)
			}
			resume, err = transition.Inspect(ctx, goal)
			if err != nil || resume.Resume == nil || *resume.Resume != journal.Transaction ||
				len(resume.Blockers) != 0 || resume.Plan != journal.ResumePlan {
				t.Fatalf("restored-config inspection: %#v, err=%v", resume, err)
			}
			settled, err := transition.Converge(ctx, Execution{Plan: resume.Plan})
			if err != nil || settled.Status != StatusReady || settled.Transaction == nil ||
				*settled.Transaction != journal.Transaction {
				t.Fatalf("restored-config resume: %#v, err=%v", settled, err)
			}
			preserved, err := os.ReadFile(settingsPath)
			ledgerAfter, ledgerErr = transition.store.ReadLedger()
			if err != nil || ledgerErr != nil || !bytes.Equal(settingsBefore, preserved) ||
				!bytes.Equal(ledgerBefore.Payload, ledgerAfter.Payload) {
				t.Fatalf("resume changed completed migration state: settings=%v ledger=%v", err, ledgerErr)
			}
			t.Log("known original inputs restored: same transaction resumed to ready without a new grant")
		})
	}
}

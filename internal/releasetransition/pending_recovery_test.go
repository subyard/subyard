package releasetransition

import (
	"bytes"
	"context"
	"errors"
	"github.com/Subyard/Subyard/internal/testkit"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestV2TransitionResumesPartialStagingAfterLedgerCAS(t *testing.T) {
	for _, tc := range []struct {
		name, staging string
		payload       []byte
	}{
		{"clean", "", nil},
		{"applied-matching", "applied", nil},
		{"applied-zero", "applied", []byte{}},
		{"applied-truncated", "applied", []byte(`{"schemaVersion":2`)},
		{"journal-zero", "journal", []byte{}},
		{"journal-truncated", "journal", []byte(`{"schemaVersion":2`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transition, home, _ := v2TransitionFixture(t, func(point string) error {
				if point == "after-ledger-cas" {
					return errors.New("interrupt after durable ledger CAS")
				}
				return nil
			})
			ctx := context.Background()
			goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
			inspect, err := transition.Inspect(ctx, goal)
			if err != nil {
				t.Fatal(err)
			}
			interrupted, err := transition.Converge(ctx, Execution{Plan: inspect.Plan, Authorization: v2TestAuthorization(inspect.Plan)})
			if err != nil || interrupted.Status != StatusRecovering {
				t.Fatalf("cut=%#v err=%v", interrupted, err)
			}
			snap, err := transition.store.ReadCurrentJournal()
			if err != nil {
				t.Fatal(err)
			}
			journal, err := ParseJournal(snap.Payload)
			if err != nil {
				t.Fatal(err)
			}
			var ledgerStep *JournalStep
			for i := range journal.Steps {
				step := &journal.Steps[i]
				if strings.HasPrefix(step.Resource, "ledger.") && step.Checkpoint == StepEvidence {
					ledgerStep = step
					break
				}
			}
			if ledgerStep == nil {
				t.Fatalf("no captured ledger step: %#v", journal.Steps)
			}
			ledger, err := transition.store.ReadLedger()
			if err != nil {
				t.Fatal(err)
			}
			if ledger.Fingerprint != ledgerStep.Desired {
				t.Fatalf("CAS not durable: ledger=%s desired=%s", ledger.Fingerprint, ledgerStep.Desired)
			}
			t.Logf("cut transaction=%s checkpoint=%s ledger-step=%s checkpoint=%s actual=%s desired=%s", journal.Transaction, journal.Checkpoint, ledgerStep.ID, ledgerStep.Checkpoint, ledger.Fingerprint, ledgerStep.Desired)
			var pending string
			if tc.staging == "applied" {
				name, err := checkpointEvidenceName(journal.Transaction, ledgerStep.ID, EvidenceApplied)
				if err != nil {
					t.Fatal(err)
				}
				pending = filepath.Join(home, "release-transition", "v2", "transactions", string(journal.Transaction), "evidence", "."+name+".pending")
			}
			if tc.staging == "journal" {
				pending = filepath.Join(home, "release-transition", "v2", ".journal.json.pending")
			}
			if tc.name == "applied-matching" {
				tc.payload, err = MarshalEvidence(evidenceFor(journal, *ledgerStep, EvidenceApplied, ledgerStep.Desired, ledgerStep.Evidence.Recovery))
				if err != nil {
					t.Fatal(err)
				}
			}
			if pending != "" {
				testkit.WriteFile(t, pending, tc.payload, 0o600)
				stat, err := os.Stat(pending)
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("injected staging=%s bytes=%d mode=%#o", tc.staging, stat.Size(), stat.Mode().Perm())
			}
			options := transition.options
			options.fault = func(point string) error {
				if point == "after-ledger-cas" {
					actual, err := transition.store.ReadLedger()
					if err != nil {
						return err
					}
					if actual.Fingerprint == ledgerStep.Desired {
						t.Fatal("completed ledger CAS replayed")
					}
				}
				return nil
			}
			restart, err := NewV2Transition(options)
			if err != nil {
				t.Fatal(err)
			}
			resume, err := restart.Inspect(ctx, goal)
			t.Logf("new-object inspection err=%v outcome=%#v blockers=%#v resume=%#v", err, resume.Outcome, resume.Blockers, resume.Resume)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := restart.Converge(ctx, Execution{Plan: resume.Plan})
			t.Logf("retry status=%s code=%s message=%q err=%v", outcome.Status, outcome.Code, outcome.Message, err)
			if pending != "" {
				if _, err := os.Stat(pending); !os.IsNotExist(err) {
					t.Fatalf("completed pending remains: %v", err)
				}
			}
			if err != nil || outcome.Status != StatusReady || !outcome.ReachedGoal {
				t.Fatalf("ordinary continuation not Ready: %#v err=%v", outcome, err)
			}
		})
	}
}

func TestV2TransitionRefusesUnsafeOrConflictingAppliedStaging(t *testing.T) {
	for _, mutant := range []string{"nonprefix", "foreign", "mode", "symlink", "hardlink"} {
		t.Run(mutant, func(t *testing.T) {
			transition, home, _ := v2TransitionFixture(t, func(point string) error {
				if point == "after-ledger-cas" {
					return errors.New("cut")
				}
				return nil
			})
			ctx := context.Background()
			goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
			inspection, err := transition.Inspect(ctx, goal)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := transition.Converge(ctx, Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)}); err != nil {
				t.Fatal(err)
			}
			current, err := transition.store.ReadCurrentJournal()
			if err != nil {
				t.Fatal(err)
			}
			journal, err := ParseJournal(current.Payload)
			if err != nil {
				t.Fatal(err)
			}
			var step JournalStep
			for _, candidate := range journal.Steps {
				if strings.HasPrefix(candidate.Resource, "ledger.") && candidate.Checkpoint == StepEvidence {
					step = candidate
					break
				}
			}
			if step.ID == "" {
				t.Fatal("missing durable CAS step")
			}
			name, err := checkpointEvidenceName(journal.Transaction, step.ID, EvidenceApplied)
			if err != nil {
				t.Fatal(err)
			}
			pending := filepath.Join(home, "release-transition", "v2", "transactions", string(journal.Transaction), "evidence", "."+name+".pending")
			payload := []byte("unknown conflicting bytes")
			if mutant == "foreign" {
				record := evidenceFor(journal, step, EvidenceApplied, step.Desired, step.Evidence.Recovery)
				record.Transaction = "tx-foreign"
				payload, err = MarshalEvidence(record)
				if err != nil {
					t.Fatal(err)
				}
			}
			testkit.WriteFile(t, pending, payload, 0o600)
			switch mutant {
			case "mode":
				if err := os.Chmod(pending, 0o644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(pending, pending+".target"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(pending+".target", pending); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(pending, pending+".link"); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshotStoreV2Tree(t, home)
			options := transition.options
			options.fault = nil
			restart, err := NewV2Transition(options)
			if err != nil {
				t.Fatal(err)
			}
			resume, err := restart.Inspect(ctx, goal)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := restart.Converge(ctx, Execution{Plan: resume.Plan})
			if err != nil || outcome.Status == StatusReady || outcome.ReachedGoal {
				t.Fatalf("unsafe staging accepted: %#v err=%v", outcome, err)
			}
			if !bytes.Equal(before, snapshotStoreV2Tree(t, home)) {
				t.Fatal("refusal changed protected bytes")
			}
		})
	}
}

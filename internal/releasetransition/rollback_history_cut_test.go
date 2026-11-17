package releasetransition

import (
	"fmt"
	"testing"
)

func rollbackCutFixture(t *testing.T, closed int, partial string) (RegistryV2, RegistryV2, LedgerV2, JournalRecord) {
	t.Helper()
	source, _ := rewindRegistries()
	target := RegistryV2{SchemaVersion: 2, MinimumEpochs: map[string]int{"settings": 1}, CurrentEpochs: map[string]int{"settings": 1}}
	initial := completeRewindLedger(source)
	owner, authority, err := source.historyAuthority(initial)
	if err != nil {
		t.Fatal(err)
	}
	edges, err := owner.RewindPath(authority, map[string]int{"settings": 1})
	if err != nil {
		t.Fatal(err)
	}
	journal := validJournal(ReleasePair{From: "source", Target: "target"})
	journal.Goal.Direction, journal.Checkpoint = DirectionActivatePrevious, JournalMigrating
	journal.Steps = nil
	for index, edge := range edges {
		migration := edge.Migration()
		_, before, err := MarshalLedgerV2(ledgerFromAuthority(edge.Before()), source)
		if err != nil {
			t.Fatal(err)
		}
		_, after, err := MarshalLedgerV2(ledgerFromAuthority(edge.After()), source)
		if err != nil {
			t.Fatal(err)
		}
		resource := JournalStep{ID: migration.ID + ".reverse", Migration: migration.ID, Resource: "state.settings", Decision: DecisionTransform, Expected: digestC, Desired: digestD, Checkpoint: StepIntent}
		ledger := JournalStep{ID: migration.ID + ".rewind-ledger", Migration: migration.ID, Resource: "ledger.settings", Decision: DecisionTransform, Expected: before, Desired: after, Checkpoint: StepIntent}
		if index < closed {
			resource.Checkpoint, ledger.Checkpoint = StepVerified, StepVerified
		} else if index == closed {
			switch partial {
			case "captured":
				resource.Checkpoint = StepEvidence
			case "effect":
				resource.Checkpoint = StepApplied
			case "verified resource":
				resource.Checkpoint = StepVerified
			case "uncertain ledger":
				resource.Checkpoint, ledger.Checkpoint = StepVerified, StepEvidence
			}
		}
		for _, step := range []JournalStep{resource, ledger} {
			if step.Checkpoint != StepIntent {
				phase, observed := EvidenceVerified, step.Desired
				if step.Checkpoint == StepEvidence {
					phase, observed = EvidenceCaptured, step.Expected
				} else if step.Checkpoint == StepApplied {
					phase = EvidenceApplied
				}
				evidence := evidenceFor(journal, step, phase, observed, "")
				step.Evidence = &evidence
			}
			journal.Steps = append(journal.Steps, step)
		}
	}
	journal.IntentDigest = bindJournalIntent(journal.AuthorizationPlan, journal.ResumePlan, journal.ObservationScope, journal.Steps)
	return source, target, initial, journal
}

func TestRollbackHistoryProgressSeparatesConsistentCutsFromUncertainEffects(t *testing.T) {
	for closed := 0; closed <= 2; closed++ {
		for _, partial := range []string{"", "captured", "effect", "verified resource", "uncertain ledger"} {
			t.Run(fmt.Sprintf("%d/%s", closed, partial), func(t *testing.T) {
				source, target, initial, journal := rollbackCutFixture(t, closed, partial)
				progress, err := inspectRollbackHistoryProgress(source, target, initial, journal)
				if err != nil {
					t.Fatal(err)
				}
				uncertain := closed < 2 && partial != ""
				if progress.needsResolution != uncertain || progress.ledger.Domains["settings"].Epoch != 3-closed || len(progress.remaining) != 2-closed {
					t.Fatalf("history progress = %+v", progress)
				}
			})
		}
	}
}

func TestRollbackHistoryProgressRejectsUnboundOrIncompletePlans(t *testing.T) {
	for _, change := range []string{"changed edge", "changed order", "missing boundary", "missing verification", "late effect", "early finalization", "different prefix"} {
		t.Run(change, func(t *testing.T) {
			source, target, initial, journal := rollbackCutFixture(t, 0, "")
			switch change {
			case "changed edge":
				journal.Steps[1].Desired = digestA
			case "changed order":
				journal.Steps[0], journal.Steps[2] = journal.Steps[2], journal.Steps[0]
			case "missing boundary":
				journal.Steps = journal.Steps[:len(journal.Steps)-1]
			case "missing verification":
				journal.Steps = append(journal.Steps[:0], journal.Steps[1], journal.Steps[3])
			case "late effect":
				step := &journal.Steps[2]
				step.Checkpoint = StepApplied
				evidence := evidenceFor(journal, *step, EvidenceApplied, step.Desired, "")
				step.Evidence = &evidence
			case "early finalization":
				journal.Steps = append([]JournalStep{{ID: rollbackNativeCheckpointStep, Migration: rollbackNativeCheckpointStep, Resource: "ledger.native-target", Decision: DecisionTransform, Expected: digestA, Desired: digestB, Checkpoint: StepIntent}}, journal.Steps...)
			case "different prefix":
				target = source
				target.Migrations = append([]MigrationDefinitionV2{}, source.Migrations...)
				target.Migrations[0].Kind = "different-kind"
			}
			journal.IntentDigest = bindJournalIntent(journal.AuthorizationPlan, journal.ResumePlan, journal.ObservationScope, journal.Steps)
			if _, err := inspectRollbackHistoryProgress(source, target, initial, journal); err == nil {
				t.Fatal("an unbound or inconsistent history plan supplied an abort cut")
			}
		})
	}
}

package releasetransition

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func newerReleaseFixture(t *testing.T) (*v2PostActivationRecoveryFixture, ProtectedSnapshot) {
	t.Helper()
	fixture := newV2PostActivationRecoveryFixture(t)
	snapshot, err := fixture.transition.store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	fixture.transition.options.Replacement = &JournalReplacement{
		Transaction: fixture.sourceJournal.Transaction, Fingerprint: snapshot.Fingerprint,
		Reason: JournalReplacementNewerRelease,
	}
	return fixture, snapshot
}

func rewriteNewerReleaseJournal(
	t *testing.T,
	fixture *v2PostActivationRecoveryFixture,
	before ProtectedSnapshot,
	mutate func(*JournalRecord),
) ProtectedSnapshot {
	t.Helper()
	journal, err := ParseJournal(before.Payload)
	if err != nil {
		t.Fatal(err)
	}
	mutate(&journal)
	payload, err := MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.transition.store.CompareAndSwapCurrentJournal(before, payload); err != nil {
		t.Fatal(err)
	}
	after := protectedSnapshotFromPayload(payload)
	fixture.transition.options.Replacement.Transaction = journal.Transaction
	fixture.transition.options.Replacement.Fingerprint = after.Fingerprint
	return after
}

func requireNewerReleasePlan(t *testing.T, fixture *v2PostActivationRecoveryFixture) Inspection {
	t.Helper()
	before := snapshotV2ProtectedTree(t, fixture.configHome)
	inspection, err := fixture.transition.Inspect(context.Background(), fixture.goal)
	if err != nil || inspection.Resume != nil || inspection.Outcome == nil ||
		inspection.Outcome.Status != StatusMigrationRequired || len(inspection.Blockers) != 0 {
		t.Fatalf("newer release inspection = %#v, err=%v", inspection, err)
	}
	if after := snapshotV2ProtectedTree(t, fixture.configHome); !bytes.Equal(before, after) {
		t.Fatalf("inspection changed protected state\nbefore:\n%s\nafter:\n%s", before, after)
	}
	const disclosure = "replace the unfinished release transition with this release's plan, keeping its journal as evidence; completed effects stay in place"
	found := false
	for _, row := range inspection.Decisions {
		found = found || row.Resource == "transition.journal" && row.Result == disclosure
	}
	if !found || !slices.Contains(inspection.Assessment.Consequences, disclosure) {
		t.Fatal("replacement missing from printable plan", inspection)
	}
	return inspection
}

func TestNewerReleaseReplacesActivatedJournalAndKeepsItsBytes(t *testing.T) {
	for _, checkpoint := range []JournalCheckpoint{JournalTargetActive, JournalReconciling} {
		t.Run(string(checkpoint), func(t *testing.T) {
			fixture, snapshot := newerReleaseFixture(t)
			snapshot = rewriteNewerReleaseJournal(t, fixture, snapshot, func(journal *JournalRecord) {
				journal.Checkpoint = checkpoint
			})
			inspection := requireNewerReleasePlan(t, fixture)
			var diagnostics bytes.Buffer
			ctx := WithMigrationDiagnosticWriter(context.Background(), &diagnostics)
			if _, err := fixture.transition.Converge(ctx, Execution{
				Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
			}); err != nil {
				t.Fatal(err)
			}
			if strings.Count(diagnostics.String(), "replace the unfinished release transition") != 1 {
				t.Fatal("convergence lost replacement disclosure for immutable callers", diagnostics.String())
			}
			current, err := fixture.transition.store.ReadCurrentJournal()
			if err != nil {
				t.Fatal(err)
			}
			successor, err := ParseJournal(current.Payload)
			if err != nil {
				t.Fatal(err)
			}
			if successor.Transaction == fixture.sourceJournal.Transaction ||
				successor.Goal != fixture.goal || successor.AuthorizationPlan != inspection.Plan {
				t.Fatalf("successor journal = %#v", successor)
			}
			preserved, err := os.ReadFile(filepath.Join(fixture.configHome,
				"release-transition-replaced-journals", "v1",
				string(fixture.sourceJournal.Transaction)+"-"+string(snapshot.Fingerprint)+".json"))
			if err != nil || !bytes.Equal(preserved, snapshot.Payload) {
				t.Fatalf("preserved journal = %q, err=%v", preserved, err)
			}
			archive, err := fixture.transition.store.ReadSupersededJournal(successor.Transaction)
			if err != nil || archive.Exists {
				t.Fatalf("newer release replacement wrote a graph archive: %#v, err=%v", archive, err)
			}
		})
	}
}

func TestNewerReleaseRequiresExactConfirmation(t *testing.T) {
	fixture, _ := newerReleaseFixture(t)
	inspection := requireNewerReleasePlan(t, fixture)
	before := snapshotV2ProtectedTree(t, fixture.configHome)
	outcome, err := fixture.transition.Converge(context.Background(), Execution{Plan: inspection.Plan})
	if err != nil || outcome.Code != CodeConfirmationRequired {
		t.Fatalf("unconfirmed outcome = %#v, err=%v", outcome, err)
	}
	if after := snapshotV2ProtectedTree(t, fixture.configHome); !bytes.Equal(before, after) {
		t.Fatalf("unconfirmed replacement changed protected state\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestNewerReleaseReplacesJournalBeforeActivation(t *testing.T) {
	for _, checkpoint := range []JournalCheckpoint{JournalAuthorized, JournalMigrating, JournalActivationIntent} {
		t.Run(string(checkpoint), func(t *testing.T) {
			fixture, snapshot := newerReleaseFixture(t)
			rewriteNewerReleaseJournal(t, fixture, snapshot, func(journal *JournalRecord) {
				journal.Checkpoint = checkpoint
			})
			*fixture.links = ReleaseLinks{Active: v2PostActivationPreviousRelease}
			fixture.transition.options.Releases = ReleasePair{
				From: v2PostActivationPreviousRelease, Target: v2PostActivationCandidateRelease,
			}
			requireNewerReleasePlan(t, fixture)
		})
	}
	t.Run("staged links", func(t *testing.T) {
		fixture, snapshot := newerReleaseFixture(t)
		rewriteNewerReleaseJournal(t, fixture, snapshot, func(journal *JournalRecord) {
			journal.Checkpoint = JournalActivationIntent
		})
		*fixture.links = ReleaseLinks{
			Active: v2PostActivationPreviousRelease, Previous: releaseIDPointer(v2PostActivationSourceRelease),
		}
		fixture.transition.options.Releases = ReleasePair{
			From: v2PostActivationPreviousRelease, Previous: releaseIDPointer(v2PostActivationSourceRelease),
			Target: v2PostActivationCandidateRelease,
		}
		requireNewerReleasePlan(t, fixture)
	})
}

func TestNewerReleaseReplacesEarlierReplacement(t *testing.T) {
	fixture, snapshot := newerReleaseFixture(t)
	rewriteNewerReleaseJournal(t, fixture, snapshot, func(journal *JournalRecord) {
		journal.Transaction = "tx-earlier-fix"
		for index := range journal.Steps {
			journal.Steps[index].Evidence.Transaction = journal.Transaction
		}
	})
	requireNewerReleasePlan(t, fixture)
}

func TestNewerReleaseReportsEveryRefusal(t *testing.T) {
	fixture, snapshot := newerReleaseFixture(t)
	rewriteNewerReleaseJournal(t, fixture, snapshot, func(journal *JournalRecord) {
		journal.Goal.Direction = DirectionActivatePrevious
	})
	fixture.transition.options.Replacement.Fingerprint = digestD
	fixture.transition.options.CandidateVersion = "0.11.1"
	fixture.links.Previous = releaseIDPointer("0.9.0-aaaaaaaaaaaa")
	inspection, err := fixture.transition.Inspect(context.Background(), fixture.goal)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]OutcomeCode, 0, len(inspection.Blockers))
	for _, blocker := range inspection.Blockers {
		got = append(got, blocker.Code)
	}
	want := []OutcomeCode{CodePlanStale, CodeRecoveryAmbiguous, CodeRecoveryAmbiguous, CodeActivationAmbiguous}
	if !slices.Equal(got, want) {
		t.Fatalf("blockers = %#v", inspection.Blockers)
	}
	if inspection.Outcome == nil || inspection.Outcome.Status != StatusOperatorActionRequired {
		t.Fatalf("outcome = %#v", inspection.Outcome)
	}
}

func TestNewerReleaseRefusesWhatItCannotOwn(t *testing.T) {
	for name, version := range map[string]string{"same": "0.11.1", "older": "0.11.0"} {
		t.Run(name, func(t *testing.T) {
			fixture, _ := newerReleaseFixture(t)
			fixture.transition.options.CandidateVersion = version
			inspection, err := fixture.transition.Inspect(context.Background(), fixture.goal)
			if err != nil || len(inspection.Blockers) != 1 ||
				inspection.Blockers[0].Message != "only a release newer than the unfinished target can replace its journal" {
				t.Fatalf("inspection = %#v, err=%v", inspection, err)
			}
		})
	}
	t.Run("migration owner successor", func(t *testing.T) {
		fixture, _ := newerReleaseFixture(t)
		transaction := TransactionID(MigrationOwnerTransactionPrefixV1 + "aaaa")
		fixture.transition.options.Replacement.Transaction = transaction
		observation := v2Observation{
			journal:         &JournalRecord{Transaction: transaction},
			journalSnapshot: ProtectedSnapshot{Fingerprint: fixture.transition.options.Replacement.Fingerprint},
		}
		observation.journal.Checkpoint = JournalReconciling
		observation.journal.Goal = Goal{Target: v2PostActivationSourceRelease, Direction: DirectionActivateTarget}
		observation.journal.Releases = ReleasePair{From: v2PostActivationPreviousRelease, Target: v2PostActivationSourceRelease}
		observation.links = *fixture.links
		blockers := fixture.transition.newerReleaseReplacementBlockers(observation)
		if len(blockers) != 1 || blockers[0].Message != "a newer release cannot replace a recovery transaction" {
			t.Fatalf("blockers = %#v", blockers)
		}
	})
}

func TestReleaseIDVersion(t *testing.T) {
	for release, want := range map[ReleaseID]string{
		"0.25.5-aaaaaaaaaaaa":      "0.25.5",
		"0.25.6-rc.1-aaaaaaaaaaaa": "0.25.6-rc.1",
	} {
		got, err := ReleaseIDVersion(release)
		if err != nil || got.String() != want {
			t.Fatalf("ReleaseIDVersion(%q) = %v, err=%v", release, got, err)
		}
	}
	for _, release := range []ReleaseID{"", "aaaaaaaaaaaa", "-aaaa", "invalid-aaaa"} {
		if version, err := ReleaseIDVersion(release); err == nil {
			t.Fatalf("ReleaseIDVersion(%q) = %v", release, version)
		}
	}
}

const (
	newerReleaseFrom   ReleaseID = "0.10.0-a1b2c3d4e5f6"
	newerReleaseBroken ReleaseID = "0.11.5-b2c3d4e5f6a7"
	newerReleaseFix    ReleaseID = "0.11.6-c3d4e5f6a7b8"
	newerReleaseLater  ReleaseID = "0.11.7-d4e5f6a7b8c9"
)

func newerReleaseCandidate(
	t *testing.T,
	configHome string,
	links *ReleaseLinks,
	target ReleaseID,
	version string,
	replacement *JournalReplacement,
	artifact Fingerprint,
	transaction TransactionID,
) *V2Transition {
	t.Helper()
	candidate, err := NewV2Transition(V2Options{
		ConfigHome:   configHome,
		Releases:     ReleasePair{From: links.Active, Previous: cloneReleaseID(links.Previous), Target: target},
		ObserveLinks: func(context.Context) (ReleaseLinks, error) { return *links, nil },
		ActivateLinks: func(_ context.Context, pair ReleasePair) (ReleaseLinks, error) {
			*links = ReleaseLinks{Active: pair.Target, Previous: releaseIDPointer(pair.From)}
			return *links, nil
		},
		RegistryPayload:   v2RegistryPayload(t),
		ArtifactDigest:    artifact,
		OwnerRegistration: &v2TestOwnerRegistration{state: OwnerRegistrationAbsent},
		NewTransactionID:  func() TransactionID { return transaction },
		CandidateVersion:  version,
		Replacement:       replacement,
		VerifyAuthorization: func(plan PlanToken, authorization Authorization) bool {
			return authorization == v2TestAuthorization(plan)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return candidate
}

// stuckAt runs a real settings migration that stops at the fault point.
func stuckAt(t *testing.T, point string) (string, *ReleaseLinks, *JournalReplacement) {
	t.Helper()
	links := &ReleaseLinks{Active: newerReleaseFrom}
	first, configHome, _ := v2TransitionFixtureWithReleases(t, func(current string) error {
		if current == point {
			return errors.New("stop")
		}
		return nil
	}, ReleasePair{From: newerReleaseFrom, Target: newerReleaseBroken}, *links)
	first.options.ObserveLinks = func(context.Context) (ReleaseLinks, error) { return *links, nil }
	first.options.ActivateLinks = func(_ context.Context, pair ReleasePair) (ReleaseLinks, error) {
		*links = ReleaseLinks{Active: pair.Target, Previous: releaseIDPointer(pair.From)}
		return *links, nil
	}
	inspection, err := first.Inspect(context.Background(), Goal{Target: newerReleaseBroken, Direction: DirectionActivateTarget})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	}); err == nil {
		t.Log("interrupted transition returned an operator outcome")
	}
	snapshot, err := first.store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	journal, err := ParseJournal(snapshot.Payload)
	if err != nil || journal.Checkpoint == JournalComplete {
		t.Fatalf("journal = %#v, err=%v", journal, err)
	}
	return configHome, links, &JournalReplacement{
		Transaction: journal.Transaction, Fingerprint: snapshot.Fingerprint, Reason: JournalReplacementNewerRelease,
	}
}

func TestNewerReleaseLeavesNoCleanupDebtAfterPartialJournal(t *testing.T) {
	for _, point := range []string{
		"after-journal-authorized", "after-recovery", "after-step-evidence", "after-settings-cas",
		"after-step-applied", "after-step-verified", "after-ledger-cas", "after-ledger-verified",
		"after-activation-intent", "after-target-active", "after-reconciling",
	} {
		t.Run(point, func(t *testing.T) {
			configHome, links, replacement := stuckAt(t, point)
			transactions := filepath.Join(configHome, "release-transition", "v2", "transactions")
			_, statErr := os.Lstat(filepath.Join(transactions, string(replacement.Transaction)))
			hadDirectory := statErr == nil
			registry, digest, err := ParseRegistryV2(v2RegistryPayload(t), BuiltinCapabilityCatalog())
			if err != nil {
				t.Fatal(err)
			}
			candidate := newerReleaseCandidate(t, configHome, links, newerReleaseFix, "0.11.6", replacement, digestB, "tx-fix")
			_, closedErr := candidate.store.closedTransactionEvidence(replacement.Transaction, registry)
			closed := hadDirectory && closedErr == nil
			goal := Goal{Target: newerReleaseFix, Direction: DirectionActivateTarget}
			inspection, err := candidate.Inspect(context.Background(), goal)
			if err != nil || len(inspection.Blockers) != 0 {
				t.Fatalf("inspection = %#v, err=%v", inspection, err)
			}
			outcome, err := candidate.Converge(context.Background(), Execution{
				Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
			})
			if err != nil || outcome.Status != StatusReady || len(outcome.Warnings) != 0 {
				t.Fatalf("replacement outcome = %#v, err=%v", outcome, err)
			}
			_, moved := os.Lstat(filepath.Join(configHome, "release-transition-replaced-journals", "v1",
				string(replacement.Transaction)+".transaction"))
			if hadDirectory && !closed && moved != nil {
				t.Fatalf("unclosed replaced transaction was not retired: %v", moved)
			}
			if closed && moved == nil {
				t.Fatal("closed transaction left ordinary retention")
			}
			if !closed {
				if _, err := os.Lstat(filepath.Join(transactions, string(replacement.Transaction))); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unclosed replaced transaction stayed in the graph: %v", err)
				}
			}
			if _, err := candidate.store.InspectSourceMetadata(registry, digest); err != nil {
				t.Fatalf("source admission after replacement: %v", err)
			}
			later := newerReleaseCandidate(t, configHome, links, newerReleaseLater, "0.11.7", nil, digestC, "tx-later")
			laterInspection, err := later.Inspect(context.Background(), Goal{Target: newerReleaseLater, Direction: DirectionActivateTarget})
			if err != nil {
				t.Fatal(err)
			}
			laterOutcome, err := later.Converge(context.Background(), Execution{
				Plan: laterInspection.Plan, Authorization: v2TestAuthorization(laterInspection.Plan),
			})
			if err != nil || laterOutcome.Status != StatusReady || len(laterOutcome.Warnings) != 0 {
				t.Fatalf("later update outcome = %#v, err=%v", laterOutcome, err)
			}
		})
	}
}

func TestNewerReleaseFailureAfterReplacementReportsItsOwnJournal(t *testing.T) {
	for _, point := range []string{"after-replacement-journal-cas", "after-settings-cas", "before-journal-complete"} {
		t.Run(point, func(t *testing.T) {
			configHome, links, replacement := stuckAt(t, "after-journal-authorized")
			candidate := newerReleaseCandidate(t, configHome, links, newerReleaseFix, "0.11.6", replacement, digestB, "tx-fix")
			candidate.options.fault = func(current string) error {
				if current == point {
					return errors.New("stop")
				}
				return nil
			}
			goal := Goal{Target: newerReleaseFix, Direction: DirectionActivateTarget}
			inspection, err := candidate.Inspect(context.Background(), goal)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := candidate.Converge(context.Background(), Execution{
				Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
			})
			if err == nil && outcome.Code == CodePlanStale {
				t.Fatalf("failure after replacement reported a stale plan: %#v", outcome)
			}
			// A later process resumes the successor; it no longer asks for replacement.
			resumed := newerReleaseCandidate(t, configHome, links, newerReleaseFix, "0.11.6", nil, digestB, "tx-unused")
			resume, err := resumed.Inspect(context.Background(), goal)
			if err != nil || resume.Resume == nil || *resume.Resume != "tx-fix" {
				t.Fatalf("resume inspection = %#v, err=%v", resume, err)
			}
			final, err := resumed.Converge(context.Background(), Execution{Plan: resume.Plan})
			if err != nil || final.Status != StatusReady || len(final.Warnings) != 0 {
				t.Fatalf("resumed outcome = %#v, err=%v", final, err)
			}
		})
	}
}

func TestNewerReleaseRefusesJournalThatSupersededAnother(t *testing.T) {
	fixture := newV2PostActivationRecoveryFixture(t)
	fixture.transition.options.fault = func(point string) error {
		if point == "after-replacement-journal-cas" {
			return errors.New("stop")
		}
		return nil
	}
	inspection, err := fixture.transition.Inspect(context.Background(), fixture.goal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	}); err == nil {
		t.Log("interrupted transition returned an operator outcome")
	}
	snapshot, err := fixture.transition.store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	journal, err := ParseJournal(snapshot.Payload)
	if err != nil {
		t.Fatal(err)
	}
	candidate := newerReleaseCandidate(t, fixture.configHome, fixture.links, newerReleaseFix, "0.11.6", &JournalReplacement{
		Transaction: journal.Transaction, Fingerprint: snapshot.Fingerprint, Reason: JournalReplacementNewerRelease,
	}, digestC, "tx-fix")
	refused, err := candidate.Inspect(context.Background(), Goal{Target: newerReleaseFix, Direction: DirectionActivateTarget})
	if err != nil || len(refused.Blockers) != 1 ||
		refused.Blockers[0].Message != "a newer release cannot replace a journal that already superseded another" {
		t.Fatalf("inspection = %#v, err=%v", refused, err)
	}
}

func TestJournalIsReservedByLiveRecoveryReceipt(t *testing.T) {
	store, err := NewPOSIXV2Store(testkit.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	receipt := recoveryReceiptFixture(t)
	if reserved, err := store.journalIsReserved(receipt.Replacement.Transaction); err != nil || reserved {
		t.Fatalf("empty store reserved = %v, err=%v", reserved, err)
	}
	payload, err := MarshalRecoveryReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRecoveryReceipt(receipt.Successor.Transaction, payload); err != nil {
		t.Fatal(err)
	}
	if reserved, err := store.journalIsReserved(receipt.Replacement.Transaction); err != nil || !reserved {
		t.Fatalf("predecessor reserved = %v, err=%v", reserved, err)
	}
	if reserved, err := store.journalIsReserved("tx-unrelated"); err != nil || reserved {
		t.Fatalf("unrelated journal reserved = %v, err=%v", reserved, err)
	}
}

func TestRetireReplacedTransactionsKeepsTheCurrentTransaction(t *testing.T) {
	configHome := testkit.TempDir(t)
	store, err := NewPOSIXV2Store(configHome)
	if err != nil {
		t.Fatal(err)
	}
	registry, _, err := ParseRegistryV2(v2RegistryPayload(t), BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	const transaction = TransactionID("tx-replaced")
	directory := filepath.Join(configHome, "release-transition", "v2", "transactions", string(transaction))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.PreserveReplacedJournal(protectedSnapshotFromPayload([]byte("{}\n")), transaction); err != nil {
		t.Fatal(err)
	}
	if err := store.RetireReplacedTransactions(transaction, registry); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(directory); err != nil {
		t.Fatalf("current transaction moved: %v", err)
	}
	if err := store.RetireReplacedTransactions("tx-successor", registry); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replaced transaction stayed: %v", err)
	}
	// Another sweep finds the source already moved.
	if err := store.RetireReplacedTransactions("tx-successor", registry); err != nil {
		t.Fatal(err)
	}
}

func TestNewerReleaseRefusesReservedJournal(t *testing.T) {
	fixture, _ := newerReleaseFixture(t)
	receipt := recoveryReceiptFixture(t)
	payload, err := MarshalRecoveryReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.transition.store.CreateRecoveryReceipt(receipt.Successor.Transaction, payload); err != nil {
		t.Fatal(err)
	}
	transaction := receipt.Replacement.Transaction
	fixture.transition.options.Replacement.Transaction = transaction
	blockers := fixture.transition.newerReleaseReplacementBlockers(v2Observation{
		journal: &JournalRecord{
			Transaction: transaction, Checkpoint: JournalReconciling,
			Goal:     Goal{Target: v2PostActivationSourceRelease, Direction: DirectionActivateTarget},
			Releases: ReleasePair{From: v2PostActivationPreviousRelease, Target: v2PostActivationSourceRelease},
		},
		journalSnapshot: ProtectedSnapshot{Fingerprint: fixture.transition.options.Replacement.Fingerprint},
		links:           *fixture.links,
	})
	if len(blockers) != 1 || blockers[0].Message != "an interrupted recovery still reserves this journal" {
		t.Fatalf("blockers = %#v", blockers)
	}
}

func TestJournalIsReservedByLiveMigrationOwnerReceipt(t *testing.T) {
	candidate, request, _ := migrationOwnerFixture(t)
	ctx := context.Background()
	inspection, err := candidate.Inspect(ctx, Goal{Target: "release-c", Direction: DirectionActivateTarget})
	if err != nil {
		t.Fatal(err)
	}
	if reserved, err := candidate.store.journalIsReserved(request.Transaction); err != nil || reserved {
		t.Fatalf("before receipt reserved = %v, err=%v", reserved, err)
	}
	candidate.options.fault = func(point string) error {
		if point == "after-migration-owner-receipt" {
			return errors.New("stop")
		}
		return nil
	}
	if _, err := candidate.Converge(ctx, Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	}); err == nil {
		t.Log("interrupted transition returned an operator outcome")
	}
	if reserved, err := candidate.store.journalIsReserved(request.Transaction); err != nil || !reserved {
		t.Fatalf("after receipt reserved = %v, err=%v", reserved, err)
	}
}

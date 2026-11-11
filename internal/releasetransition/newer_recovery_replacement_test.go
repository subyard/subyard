package releasetransition

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func mustRecoveryNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func mustJournal(t *testing.T, payload []byte) JournalRecord {
	t.Helper()
	journal, err := ParseJournal(payload)
	mustRecoveryNoError(t, err)
	return journal
}

func mustCurrentJournal(t *testing.T, store *POSIXV2Store) ProtectedSnapshot {
	t.Helper()
	snapshot, err := store.ReadCurrentJournal()
	mustRecoveryNoError(t, err)
	return snapshot
}

func mustConverge(t *testing.T, ctx context.Context, transition *V2Transition, execution Execution) Outcome {
	t.Helper()
	outcome, err := transition.Converge(ctx, execution)
	mustRecoveryNoError(t, err)
	return outcome
}

func restartAndInspect(t *testing.T, ctx context.Context, transition *V2Transition, goal Goal) (*V2Transition, Inspection, error) {
	t.Helper()
	restarted, err := NewV2Transition(transition.options)
	mustRecoveryNoError(t, err)
	inspection, err := restarted.Inspect(ctx, goal)
	return restarted, inspection, err
}

func newerRecoveryFixture(t *testing.T, archiveFirst ...bool) (*V2Transition, *ReleaseLinks, ProtectedSnapshot) {
	t.Helper()
	ctx := context.Background()
	owner, reconciler, request, before, _ := interruptedActivationRecoveryFixture(t)
	journal := mustJournal(t, before.Payload)
	journal.Goal.Target = newerReleaseBroken
	journal.Releases = ReleasePair{From: newerReleaseBroken, Target: newerReleaseBroken}
	payload := mustJournalPayload(t, journal)
	mustRecoveryNoError(t, owner.store.CompareAndSwapCurrentJournal(before, payload))
	request.Fingerprint = fingerprintPayload(payload)
	links := &ReleaseLinks{Active: newerReleaseBroken}
	owner.options.Releases = journal.Releases
	owner.options.ObserveLinks = func(context.Context) (ReleaseLinks, error) { return *links, nil }
	owner.options.RecoveryContract = ActivationOnlyRecoveryContractV2
	plan, err := owner.InspectActivationRecovery(ctx, request)
	if err != nil || len(plan.Blockers) != 0 {
		t.Fatalf("recovery plan: %#v %v", plan, err)
	}
	// Leave a cancelled earlier generation in the receipt chain as well.
	owner.options.fault = func(point string) error {
		if point == "after-recovery-receipt" {
			return errors.New("receipt-only interruption")
		}
		return nil
	}
	if _, err := owner.ConvergeActivationRecovery(ctx, request, Execution{Plan: plan.Plan, Authorization: v2TestAuthorization(plan.Plan)}); err == nil {
		t.Fatal("receipt-only interruption did not fire")
	}
	if len(archiveFirst) != 0 && archiveFirst[0] {
		// Retain the first generation's archive-only replacement attempt.
		// The original owner below advances the same predecessor frontier.
		first, err := owner.store.ReadRecoveryReceipt(lifecycleTransaction(t, request, 1))
		mustRecoveryNoError(t, err)
		receipt, err := ParseRecoveryReceipt(first.Payload)
		mustRecoveryNoError(t, err)
		before := receipt.Successor
		before.Checkpoint = JournalReconciling
		payload := mustJournalPayload(t, before)
		snapshot := protectedSnapshotFromPayload(payload)
		mustRecoveryNoError(t, owner.store.PreserveReplacedJournal(snapshot, before.Transaction))
		mustRecoveryNoError(t, owner.store.preserveReplacedRecovery(snapshot))
	}
	reconciler.desired = digestD
	plan, err = owner.InspectActivationRecovery(ctx, request)
	if err != nil || len(plan.Blockers) != 0 {
		t.Fatalf("next recovery generation: %#v %v", plan, err)
	}
	owner.options.fault = func(point string) error {
		if point == "after-reconciling" {
			return errors.New("interrupted recovery apply")
		}
		return nil
	}
	if outcome, err := owner.ConvergeActivationRecovery(ctx, request, Execution{Plan: plan.Plan, Authorization: v2TestAuthorization(plan.Plan)}); err != nil || outcome.Status != StatusRecovering {
		t.Fatalf("interrupted recovery: %#v %v", outcome, err)
	}
	snapshot := mustCurrentJournal(t, owner.store)
	current, err := ParseJournal(snapshot.Payload)
	if err != nil || !strings.HasPrefix(string(current.Transaction), RecoveryTransactionPrefixV2) || current.Checkpoint != JournalReconciling {
		t.Fatalf("recovery journal: %#v %v", current, err)
	}
	candidate := newerReleaseCandidate(t, owner.options.ConfigHome, links, newerReleaseFix, "0.11.6", &JournalReplacement{
		Transaction: current.Transaction, Fingerprint: snapshot.Fingerprint, Reason: JournalReplacementNewerRelease,
	}, digestC, "tx-recovery-fix")
	return candidate, links, snapshot
}

func TestNewerReleaseRecoveryReplacementDurableBoundaries(t *testing.T) {
	for _, point := range []string{"after-replaced-journal", "after-replacement-journal-cas", "after-reconciling", "before-recovery-gc", "after-replaced-recovery-retirement", ""} {
		t.Run(point, func(t *testing.T) {
			ctx := context.Background()
			candidate, links, before := newerRecoveryFixture(t)
			goal := Goal{Target: newerReleaseFix, Direction: DirectionActivateTarget}
			inspection, err := candidate.Inspect(ctx, goal)
			if err != nil || len(inspection.Blockers) != 0 || inspection.Resume != nil {
				t.Fatalf("replacement plan: %#v %v", inspection, err)
			}
			original := mustJournal(t, before.Payload)
			// Neither the old grant nor absent consent authorizes this plan.
			for _, grant := range []Authorization{"", v2TestAuthorization(original.AuthorizationPlan)} {
				outcome := mustConverge(t, ctx, candidate, Execution{Plan: inspection.Plan, Authorization: grant})
				if outcome.Code != CodeConfirmationRequired {
					t.Fatalf("old consent: %#v %v", outcome, nil)
				}
			}
			fired := false
			fault := func(actual string) error {
				if point != "" && actual == point {
					fired = true
					return errors.New("boundary interruption")
				}
				return nil
			}
			candidate.options.fault, candidate.store.fault = fault, fault
			first, firstErr := candidate.Converge(ctx, Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
			if point != "" && !fired {
				t.Fatal("interruption did not fire")
			}
			if point == "" && (firstErr != nil || first.Status != StatusReady) {
				t.Fatalf("uninterrupted replacement: %#v %v", first, firstErr)
			}
			preserved, err := os.ReadFile(filepath.Join(candidate.options.ConfigHome, "release-transition-replaced-journals/v1", string(original.Transaction)+"-"+string(before.Fingerprint)+".json"))
			if err != nil || !bytes.Equal(preserved, before.Payload) {
				t.Fatalf("superseded journal: %v", err)
			}
			archivedReceipt, err := candidate.store.readRecord(replacedRecoveryParts(original.Transaction, "transactions"), string(original.Transaction)+".json")
			if err != nil || !archivedReceipt.Exists {
				t.Fatalf("receipt chain archive: %v", err)
			}
			chain, _, err := candidate.store.readDirectoryEntries(replacedRecoveryParts(original.Transaction, "archive"), maxRecoveryLifecycleEntries)
			if err != nil || len(chain) == 0 {
				t.Fatalf("cancelled receipt chain lost: %v", err)
			}
			if point == "after-replaced-journal" {
				current := mustCurrentJournal(t, candidate.store)
				if !sameProtectedSnapshot(current, before) {
					t.Fatal("archive-only crash replaced current journal")
				}
				mustRecoveryNoError(t, candidate.store.ValidateCurrentRecovery(original))
			}
			candidate.options.fault, candidate.store.fault = nil, nil
			restarted, fresh, err := restartAndInspect(t, ctx, candidate, goal)
			if err != nil || len(fresh.Blockers) != 0 {
				t.Fatalf("retry plan: %#v %v", fresh, err)
			}
			grant := Authorization("")
			if fresh.Resume == nil {
				grant = v2TestAuthorization(fresh.Plan)
			}
			outcome := mustConverge(t, ctx, restarted, Execution{Plan: fresh.Plan, Authorization: grant})
			if outcome.Status != StatusReady || len(outcome.Warnings) != 0 {
				t.Fatalf("retry: %#v %v", outcome, nil)
			}
			if links.Active != newerReleaseFix {
				t.Fatal("fixed release not activated")
			}
			live, err := restarted.store.ReadRecoveryReceipt(original.Transaction)
			if err != nil || live.Exists {
				t.Fatalf("superseded recovery still live: %#v %v", live, err)
			}
			for _, parts := range [][]string{recoveryLifecycleParts, recoveryArchiveParts, recoveryFrontierParts} {
				records, _, err := restarted.store.readDirectoryEntries(parts, maxRecoveryLifecycleEntries*2)
				if err != nil || len(records) != 0 {
					t.Fatalf("live recovery namespace left behind: %v %v", parts, err)
				}
			}
			// The strict ordinary history reader (also used by 0.27.0) sees
			// no unfinished recovery or archive edge in the successor's graph.
			if _, err := restarted.store.InspectSourceMetadata(restarted.registry, restarted.registryDigest); err != nil {
				t.Fatal("retained history unreadable", err)
			}
			compatibility := *restarted
			compatibility.options.Direction = DirectionActivatePrevious
			compatibility.options.RollbackTarget = &RollbackTarget{Version: "0.27.0", RegistryDigest: restarted.registryDigest}
			if blocker := compatibility.rollbackCompatibilityBlocker(true); blocker != nil {
				t.Fatal("same-registry retained 0.27.0 reader blocked", blocker)
			}
			repeat, err := restarted.Inspect(ctx, goal)
			if err != nil || repeat.Outcome == nil || repeat.Outcome.Status != StatusReady {
				t.Fatalf("repeat: %#v %v", repeat, err)
			}
			closed := mustCurrentJournal(t, restarted.store)
			if outcome := mustConverge(t, ctx, restarted, Execution{Plan: repeat.Plan}); outcome.Status != StatusReady {
				t.Fatalf("repeat convergence: %#v %v", outcome, nil)
			}
			unchanged := mustCurrentJournal(t, restarted.store)
			if !sameProtectedSnapshot(closed, unchanged) {
				t.Fatal("repeat rewrote the completed journal")
			}
		})
	}
}

func TestNewerReleaseRecoveryReplacementBlockers(t *testing.T) {
	const recoveryInvalid = "protected recovery evidence is invalid or inconsistent"
	const linksInvalid = "the release links do not match the unfinished activation"
	for _, test := range []struct {
		name   string
		mutate func(*V2Transition, *v2Observation)
		want   []string
	}{
		{"eligible", func(*V2Transition, *v2Observation) {}, nil},
		{"checkpoint", func(_ *V2Transition, o *v2Observation) { o.journal.Checkpoint = JournalTargetActive }, []string{"recovery replacement requires a reconciling journal"}},
		{"steps", func(_ *V2Transition, o *v2Observation) { o.journal.Steps = []JournalStep{{ID: "foreign"}} }, []string{recoveryInvalid, "recovery replacement requires an activation-only journal"}},
		{"direction", func(c *V2Transition, o *v2Observation) {
			c.options.Direction = DirectionActivatePrevious
			o.journal.Goal.Direction = DirectionActivatePrevious
		}, []string{"a newer release can replace only an unfinished forward update", recoveryInvalid}},
		{"source ingress", func(c *V2Transition, _ *v2Observation) { c.options.SourceIngress = &SourceIngressRequest{} }, []string{"a newer release cannot replace an unfinished source migration"}},
		{"journal ingress", func(_ *V2Transition, o *v2Observation) { o.journal.SourceIngress = &SourceIngressRequest{} }, []string{"a newer release cannot replace an unfinished source migration", recoveryInvalid}},
		{"missing provenance", func(c *V2Transition, o *v2Observation) {
			value, _ := c.store.ReadRecoveryReceipt(o.journal.Transaction)
			mustRecoveryNoError(t, c.store.removeRecoveryPair(recoveryLifecycleParts, o.journal.Transaction, value.Payload))
		}, []string{recoveryInvalid}},
		{"inactive target", func(_ *V2Transition, o *v2Observation) { o.links.Active = newerReleaseFrom }, []string{linksInvalid}},
		{"not newer", func(c *V2Transition, _ *v2Observation) { c.options.CandidateVersion = "0.11.5" }, []string{"only a release newer than the unfinished target can replace its journal"}},
		{"stale plan", func(c *V2Transition, _ *v2Observation) { c.options.Replacement.Fingerprint = digestD }, []string{"the journal selected for replacement changed after inspection"}},
		{"older recovery", func(c *V2Transition, o *v2Observation) {
			o.journal.Transaction = RecoveryTransactionPrefixV1 + "unsupported"
			c.options.Replacement.Transaction = o.journal.Transaction
		}, []string{"a newer release cannot replace a recovery transaction", recoveryInvalid}},
		{"migration owner", func(c *V2Transition, o *v2Observation) {
			o.journal.Transaction = MigrationOwnerTransactionPrefixV1 + "unsupported"
			c.options.Replacement.Transaction = o.journal.Transaction
		}, []string{"a newer release cannot replace a recovery transaction"}},
		{"reservation unavailable", func(c *V2Transition, _ *v2Observation) {
			mustRecoveryNoError(t, c.store.createImmutable(recoveryLifecycleParts, "recovery-v2-unrecognized.json", []byte("{}")))
		}, []string{"an interrupted recovery still reserves this journal", recoveryInvalid}},
		{"already superseded", func(c *V2Transition, o *v2Observation) {
			mustRecoveryNoError(t, c.store.CreateSupersededJournal(o.journal.Transaction, []byte("unrecognized protected archive")))
		}, []string{"a newer release cannot replace a journal that already superseded another", recoveryInvalid}},
		{"all independent", func(c *V2Transition, o *v2Observation) {
			c.options.Replacement.Fingerprint = digestD
			c.options.CandidateVersion = "0.11.5"
			c.options.SourceIngress = &SourceIngressRequest{}
			c.options.Direction = DirectionActivatePrevious
			o.journal.Checkpoint = JournalTargetActive
			o.journal.Steps = []JournalStep{{ID: "foreign"}}
			o.links.Active = newerReleaseFrom
		}, []string{"a newer release can replace only an unfinished forward update", "a newer release cannot replace an unfinished source migration", "only a release newer than the unfinished target can replace its journal", recoveryInvalid, "recovery replacement requires a reconciling journal", "recovery replacement requires an activation-only journal", linksInvalid, "the journal selected for replacement changed after inspection"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate, links, before := newerRecoveryFixture(t)
			journal := mustJournal(t, before.Payload)
			observation := v2Observation{journal: &journal, journalSnapshot: before, links: *links}
			test.mutate(candidate, &observation)
			var got []string
			for _, blocker := range candidate.newerReleaseReplacementBlockers(observation) {
				got = append(got, blocker.Message)
				code := CodeRecoveryAmbiguous
				if blocker.Message == linksInvalid {
					code = CodeActivationAmbiguous
				}
				if blocker.Message == "the journal selected for replacement changed after inspection" {
					code = CodePlanStale
				}
				retry := "run yard update --check"
				if test.name == "not newer" {
					retry = "run yard update --version \"$VERSION\""
				}
				if blocker.Code != code || blocker.Resource != "transition.active" || blocker.Retry != retry {
					t.Fatal("incorrect public refusal", blocker)
				}
			}
			slices.Sort(test.want)
			if !slices.Equal(got, test.want) {
				t.Fatalf("blockers = %#v; want %#v", got, test.want)
			}
		})
	}
}

func TestReplacedRecoveryOldFrontierDoesNotBlockLaterReplacement(t *testing.T) {
	for _, cut := range []bool{false, true} {
		t.Run(fmt.Sprint(cut), func(t *testing.T) {
			candidate, _, _ := newerRecoveryFixture(t, true)
			selected := mustCurrentJournal(t, candidate.store)
			journal := mustJournal(t, selected.Payload)
			snapshot, err := candidate.store.ReadRecoveryReceipt(journal.Transaction)
			mustRecoveryNoError(t, err)
			receipt, err := ParseRecoveryReceipt(snapshot.Payload)
			mustRecoveryNoError(t, err)
			old, err := RecoveryLifecycleTransaction(receipt.Replacement, 1)
			mustRecoveryNoError(t, err)
			name := string(receipt.Replacement.Transaction) + ".json"
			oldFrontier, err := candidate.store.readRecord(replacedRecoveryParts(old, "frontiers"), name)
			if err != nil || !oldFrontier.Exists {
				t.Fatal("missing archive-only frontier", err)
			}
			advanced, err := candidate.store.readRecord(recoveryFrontierParts, name)
			if err != nil || !advanced.Exists || bytes.Equal(oldFrontier.Payload, advanced.Payload) {
				t.Fatal("fixture did not advance the archived predecessor frontier", err)
			}
			goal := Goal{Target: newerReleaseFix, Direction: DirectionActivateTarget}
			plan, err := candidate.Inspect(context.Background(), goal)
			if err != nil || len(plan.Blockers) != 0 {
				t.Fatalf("plan: %#v %v", plan, err)
			}
			fired := false
			candidate.store.fault = func(point string) error {
				if cut && !fired && point == "after-replaced-recovery-retirement" {
					fired = true
					return errors.New("retirement interrupted")
				}
				return nil
			}
			outcome := mustConverge(t, context.Background(), candidate, Execution{Plan: plan.Plan, Authorization: v2TestAuthorization(plan.Plan)})
			if outcome.Status != StatusReady || (len(outcome.Warnings) != 0) != cut {
				t.Fatalf("first cleanup: %#v %v", outcome, nil)
			}
			if cut && !fired {
				t.Fatal("retirement cut did not fire")
			}
			candidate.store.fault = nil
			restarted, plan, err := restartAndInspect(t, context.Background(), candidate, goal)
			mustRecoveryNoError(t, err)
			outcome = mustConverge(t, context.Background(), restarted, Execution{Plan: plan.Plan})
			if outcome.Status != StatusReady || len(outcome.Warnings) != 0 {
				t.Fatalf("retry cleanup: %#v %v", outcome, nil)
			}
			for _, parts := range [][]string{recoveryLifecycleParts, recoveryArchiveParts, recoveryFrontierParts} {
				entries, _, err := restarted.store.readDirectoryEntries(parts, maxRecoveryLifecycleEntries*2)
				if err != nil || len(entries) != 0 {
					t.Fatalf("recovery records remain: %v %v", parts, err)
				}
			}
			retained, err := restarted.store.readRecord(replacedRecoveryParts(old, "frontiers"), name)
			if err != nil || !sameProtectedSnapshot(retained, oldFrontier) {
				t.Fatal("superseded frontier archive changed", err)
			}
		})
	}
}

func TestNewerReleaseRecoverySuccessorCanBeReplacedAgain(t *testing.T) {
	ctx := context.Background()
	candidate, links, _ := newerRecoveryFixture(t)
	goal := Goal{Target: newerReleaseFix, Direction: DirectionActivateTarget}
	plan, err := candidate.Inspect(ctx, goal)
	mustRecoveryNoError(t, err)
	candidate.options.fault = func(point string) error {
		if point == "after-reconciling" {
			return errors.New("fixed release also interrupted")
		}
		return nil
	}
	if outcome := mustConverge(t, ctx, candidate, Execution{Plan: plan.Plan, Authorization: v2TestAuthorization(plan.Plan)}); outcome.Status != StatusRecovering {
		t.Fatalf("first successor: %#v %v", outcome, nil)
	}
	before := mustCurrentJournal(t, candidate.store)
	journal := mustJournal(t, before.Payload)
	later := newerReleaseCandidate(t, candidate.options.ConfigHome, links, newerReleaseLater, "0.11.7", &JournalReplacement{
		Transaction: journal.Transaction, Fingerprint: before.Fingerprint, Reason: JournalReplacementNewerRelease,
	}, digestD, "tx-recovery-later")
	plan, err = later.Inspect(ctx, Goal{Target: newerReleaseLater, Direction: DirectionActivateTarget})
	if err != nil || len(plan.Blockers) != 0 {
		t.Fatalf("second successor blocked: %#v %v", plan, err)
	}
	if outcome := mustConverge(t, ctx, later, Execution{Plan: plan.Plan, Authorization: v2TestAuthorization(plan.Plan)}); outcome.Status != StatusReady || len(outcome.Warnings) != 0 {
		t.Fatalf("second successor: %#v %v", outcome, nil)
	}
}

func TestReplacedRecoveryRetiresWithoutOrdinaryTransactionDirectories(t *testing.T) {
	candidate, _, before := newerRecoveryFixture(t)
	journal := mustJournal(t, before.Payload)
	mustRecoveryNoError(t, candidate.store.PreserveReplacedJournal(before, journal.Transaction))
	mustRecoveryNoError(t, candidate.store.preserveReplacedRecovery(before))
	// Model an activation-only installation without ordinary migration artifacts.
	mustRecoveryNoError(t, os.RemoveAll(filepath.Join(candidate.options.ConfigHome, "release-transition/v2/transactions")))
	mustRecoveryNoError(t, candidate.store.RetireReplacedTransactions("tx-verified-successor", candidate.registry))
	live, err := candidate.store.ReadRecoveryReceipt(journal.Transaction)
	if err != nil || live.Exists {
		t.Fatalf("recovery retained without ordinary transaction root: %#v %v", live, err)
	}
}

func TestNewerReleaseRecoveryPlanBindsExactJournal(t *testing.T) {
	ctx := context.Background()
	candidate, _, before := newerRecoveryFixture(t)
	goal := Goal{Target: newerReleaseFix, Direction: DirectionActivateTarget}
	plan, err := candidate.Inspect(ctx, goal)
	mustRecoveryNoError(t, err)
	journal := mustJournal(t, before.Payload)
	journal.Checkpoint = JournalTargetActive // Known provenance, ineligible checkpoint.
	payload := mustJournalPayload(t, journal)
	mustRecoveryNoError(t, candidate.store.CompareAndSwapCurrentJournal(before, payload))
	outcome := mustConverge(t, ctx, candidate, Execution{Plan: plan.Plan, Authorization: v2TestAuthorization(plan.Plan)})
	if outcome.Status != StatusOperatorActionRequired {
		t.Fatalf("changed journal used old grant: %#v %v", outcome, nil)
	}
	current, err := candidate.store.ReadCurrentJournal()
	if err != nil || !bytes.Equal(current.Payload, payload) {
		t.Fatal("stale plan replaced current journal", err)
	}
}

func TestSourceRecoveryAdviceRequiresEligibleD7Journal(t *testing.T) {
	for _, kind := range []string{"eligible", "ordinary rollback", "ordinary checkpoint", "recovery checkpoint", "older recovery", "missing receipt"} {
		t.Run(kind, func(t *testing.T) {
			candidate, _, before := newerRecoveryFixture(t)
			journal := mustJournal(t, before.Payload)
			switch kind {
			case "ordinary rollback":
				journal.Transaction = "tx-ordinary"
				journal.Goal.Direction = DirectionActivatePrevious
			case "ordinary checkpoint":
				journal.Transaction = "tx-ordinary"
				journal.Checkpoint = JournalTargetActive
			case "recovery checkpoint":
				journal.Checkpoint = JournalTargetActive
			case "older recovery":
				journal.Transaction = RecoveryTransactionPrefixV1 + "unsupported"
			case "missing receipt":
				receipt, err := candidate.store.ReadRecoveryReceipt(journal.Transaction)
				mustRecoveryNoError(t, err)
				mustRecoveryNoError(t, candidate.store.removeRecoveryPair(recoveryLifecycleParts, journal.Transaction, receipt.Payload))
			}
			payload := mustJournalPayload(t, journal)
			mustRecoveryNoError(t, candidate.store.CompareAndSwapCurrentJournal(before, payload))
			_, err := candidate.store.InspectSourceMetadata(candidate.registry, candidate.registryDigest)
			if err == nil || errors.Is(err, ErrSourceRecoveryHistory) != (kind == "eligible") {
				t.Fatalf("source history classification: %v", err)
			}
		})
	}
}

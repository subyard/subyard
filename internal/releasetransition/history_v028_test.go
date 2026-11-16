package releasetransition

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

// Fixtures were emitted by the actual v0.28.0/v0.28.1 tag engines, interrupted
// after their sixth after-ledger-verified hook. They are exact protected bytes.
func frozenAnchorFixture(t *testing.T, version string) (string, JournalRecord, ProtectedSnapshot) {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join("testdata", "v"+version+"-first-transition-verified.json"))
	if err != nil {
		t.Fatal(err)
	}
	var files map[string]string
	if err := json.Unmarshal(payload, &files); err != nil {
		t.Fatal(err)
	}
	home := testkit.TempDir(t)
	for name, content := range files {
		path := filepath.Join(home, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, []byte(content), 0600)
	}
	store, err := NewPOSIXV2Store(home)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	journal, err := ParseJournal(snapshot.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(journal.Steps) != 6 || journal.Checkpoint == JournalComplete {
		t.Fatalf("invalid tag fixture: %#v", journal)
	}
	for _, step := range journal.Steps {
		if step.Checkpoint != StepVerified {
			t.Fatalf("pending fixture step: %#v", step)
		}
	}
	return home, journal, snapshot
}

func currentHistoryOptions(t *testing.T, home string, links *ReleaseLinks, target ReleaseID) V2Options {
	t.Helper()
	payload, err := os.ReadFile("../../config/release-transition.json")
	if err != nil {
		t.Fatal(err)
	}
	return V2Options{
		ConfigHome: home, RegistryPayload: payload, ArtifactDigest: digestB,
		Releases:     ReleasePair{From: links.Active, Previous: cloneReleaseID(links.Previous), Target: target},
		ObserveLinks: func(context.Context) (ReleaseLinks, error) { return *links, nil },
		ActivateLinks: func(_ context.Context, pair ReleasePair) (ReleaseLinks, error) {
			*links = ReleaseLinks{Active: pair.Target, Previous: releaseIDPointer(pair.From)}
			return *links, nil
		},
		MigrationCheckpoint: true, SourceMigrationCheckpoint: true, CandidateVersion: "0.29.0",
		NewTransactionID:    func() TransactionID { return "tx-current-history" },
		VerifyAuthorization: func(plan PlanToken, grant Authorization) bool { return grant == v2TestAuthorization(plan) },
	}
}

func TestFrozenV028HistoryKeepsVerifiedTakeover(t *testing.T) {
	for _, version := range []string{"0.28.0", "0.28.1"} {
		for _, checkpointed := range []bool{false, true} {
			name := version + "/uncheckpointed"
			if checkpointed {
				name = version + "/checkpointed"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				home, journal, snapshot := frozenAnchorFixture(t, version)
				store, err := NewPOSIXV2Store(home)
				if err != nil {
					t.Fatal(err)
				}
				original, err := store.ReadMigrationLedger(historyV028, historyV028Digest)
				if err != nil {
					t.Fatal(err)
				}
				if pending, err := historyV028.PendingPath(original.Ledger); err != nil || len(pending) != 0 {
					t.Fatalf("tag history incomplete: %#v %v", pending, err)
				}
				if checkpointed {
					epochs := map[string]int{}
					for domain, state := range original.Ledger.Domains {
						epochs[domain] = state.Epoch
					}
					checkpoint, err := newLedgerCheckpointV1(original.Ledger, historyV028, historyV028Digest, original.ProjectionSnapshot, epochs)
					if err != nil {
						t.Fatal(err)
					}
					payload, _, err := MarshalLedgerCheckpointV1(checkpoint, historyV028, historyV028Digest, original.ProjectionSnapshot)
					if err != nil {
						t.Fatal(err)
					}
					if err := store.compareAndSwap([]string{"release-transition", "v2"}, migrationLedgerCheckpointName, absentProtectedSnapshot(), payload); err != nil {
						t.Fatal(err)
					}
				}
				links := &ReleaseLinks{Active: journal.Goal.Target}
				options := currentHistoryOptions(t, home, links, "0.29.0-bbbbbbbbbbbb")
				options.Replacement = &JournalReplacement{Transaction: journal.Transaction, Fingerprint: snapshot.Fingerprint, Reason: JournalReplacementNewerRelease}
				candidate, err := NewV2Transition(options)
				if err != nil {
					t.Fatal(err)
				}
				history, err := store.ReadMigrationLedger(candidate.registry, candidate.registryDigest)
				if err != nil {
					t.Fatal(err)
				}
				if !sameLedgerV2(history.Ledger, original.Ledger) {
					t.Fatal("frozen Applied history changed")
				}
				payload, _, err := MarshalLedgerV2(history.Ledger, candidate.registry)
				if err != nil || !bytes.Equal(payload, original.ProjectionSnapshot.Payload) {
					t.Fatalf("legacy bytes changed: %s %v", payload, err)
				}
				if _, err := ledgerTrace(candidate.registry, history.Ledger, journal.Steps); err != nil {
					t.Fatal(err)
				}
				if err := store.sourceTerminalBindings(journal, candidate.registry, original.ProjectionSnapshot.Fingerprint, history); err != nil {
					t.Fatal(err)
				}
				before := snapshotV2ProtectedTree(t, home)
				goal := Goal{Target: options.Releases.Target, Direction: DirectionActivateTarget}
				plan, err := candidate.Inspect(ctx, goal)
				if err != nil || len(plan.Blockers) != 0 {
					t.Fatalf("takeover: %#v %v", plan, err)
				}
				if !bytes.Equal(before, snapshotV2ProtectedTree(t, home)) {
					t.Fatal("inspection wrote")
				}
				outcome, err := candidate.Converge(ctx, Execution{Plan: plan.Plan, Authorization: v2TestAuthorization(plan.Plan)})
				if err != nil || outcome.Status != StatusReady {
					t.Fatalf("takeover completion: %#v %v", outcome, err)
				}
				current, err := store.ReadCurrentJournal()
				if err != nil {
					t.Fatal(err)
				}
				successor, err := ParseJournal(current.Payload)
				if err != nil || len(successor.Steps) != 0 {
					t.Fatalf("historical migrations replayed: %#v %v", successor, err)
				}
				retained, err := os.ReadFile(filepath.Join(home, "release-transition-replaced-journals", "v1", string(journal.Transaction)+"-"+string(snapshot.Fingerprint)+".json"))
				if err != nil || !bytes.Equal(retained, snapshot.Payload) {
					t.Fatalf("original journal bytes lost: %v", err)
				}
				after, err := store.ReadMigrationLedger(historyV028, historyV028Digest)
				if err != nil || !sameLedgerV2(after.Ledger, original.Ledger) {
					t.Fatalf("retained reader: %#v %v", after, err)
				}
				if _, err := store.InspectSourceMetadata(candidate.registry, candidate.registryDigest); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestFreshHistoryStartsAtTopWithoutHistoricalMigrations(t *testing.T) {
	for _, point := range []string{"", "after-journal-authorized", "after-reconciling", "before-checkpoint-publication", "after-checkpoint-publication"} {
		name := point
		if name == "" {
			name = "uninterrupted"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			home := testkit.TempDir(t)
			release := ReleaseID("0.29.0-bbbbbbbbbbbb")
			links := &ReleaseLinks{Active: release}
			options := currentHistoryOptions(t, home, links, release)
			interrupted := false
			options.fault = func(at string) error {
				if point != "" && at == point && !interrupted {
					interrupted = true
					return errors.New("interrupt first installation")
				}
				return nil
			}
			candidate, err := NewV2Transition(options)
			if err != nil {
				t.Fatal(err)
			}
			baseline := FreshBootstrapLedgerV2(candidate.registry)
			if pending, err := candidate.registry.PendingPath(baseline); err != nil || len(pending) != 0 {
				t.Fatalf("fresh pending: %#v %v", pending, err)
			}
			if _, err := ledgerTrace(candidate.registry, baseline, nil); err != nil {
				t.Fatal(err)
			}
			goal := Goal{Target: release, Direction: DirectionActivateTarget}
			plan, err := candidate.Inspect(ctx, goal)
			if err != nil {
				t.Fatal(err)
			}
			if point == "" {
				refused, err := candidate.Converge(ctx, Execution{Plan: plan.Plan})
				if err != nil || refused.Code != CodeConfirmationRequired {
					t.Fatalf("fresh bootstrap bypassed consent: %#v %v", refused, err)
				}
				ledger, err := candidate.store.ReadLedger()
				if err != nil || ledger.Exists {
					t.Fatal("unauthorized baseline write", err)
				}
				journal, err := candidate.store.ReadCurrentJournal()
				if err != nil || journal.Exists {
					t.Fatal("unauthorized journal publication", err)
				}
			}

			outcome, err := candidate.Converge(ctx, Execution{Plan: plan.Plan, Authorization: v2TestAuthorization(plan.Plan)})
			if point != "" && !interrupted {
				t.Fatal("fault was not reached")
			}
			if point == "" && (err != nil || outcome.Status != StatusReady) {
				t.Fatalf("fresh completion: %#v %v", outcome, err)
			}
			options.fault = nil
			resumed, err := NewV2Transition(options)
			if err != nil {
				t.Fatal(err)
			}
			plan, err = resumed.Inspect(ctx, goal)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err = resumed.Converge(ctx, Execution{Plan: plan.Plan, Authorization: v2TestAuthorization(plan.Plan)})
			if err != nil || outcome.Status != StatusReady {
				t.Fatalf("resume: %#v %v", outcome, err)
			}
			state, err := resumed.store.ReadMigrationLedger(resumed.registry, resumed.registryDigest)
			if err != nil || !state.Checkpointed {
				t.Fatalf("fresh checkpoint: %#v %v", state, err)
			}
			snapshot, err := resumed.store.ReadCurrentJournal()
			if err != nil {
				t.Fatal(err)
			}
			journal, err := ParseJournal(snapshot.Payload)
			if err != nil || len(journal.Steps) != 1 || journal.Steps[0].ID != baselineLedgerStep || journal.Steps[0].Checkpoint != StepVerified || journal.Checkpoint != JournalComplete {
				t.Fatalf("fresh journal: %#v %v", journal, err)
			}
			projected, _, err := ParseLedgerV2(state.ProjectionSnapshot.Payload, resumed.registry)
			if err != nil || !sameLedgerV2(projected, baseline) || !sameLedgerV2(state.Ledger, baseline) {
				t.Fatalf("fresh baseline changed after checkpoint: %#v %v", state.Ledger, err)
			}
			for _, domain := range baseline.Domains {
				if len(domain.Applied) != 0 {
					t.Fatal("fresh baseline claims historical effects")
				}
			}
			if _, err := resumed.store.InspectSourceMetadata(resumed.registry, resumed.registryDigest); err != nil {
				t.Fatal(err)
			}

		})
	}
}

func TestRetiredMigrationAdmissionRefusesBeforeWrites(t *testing.T) {
	for _, shape := range []string{"below-floor-ledger", "pending-journal-step"} {
		t.Run(shape, func(t *testing.T) {
			home, journal, snapshot := frozenAnchorFixture(t, "0.28.0")
			store, err := NewPOSIXV2Store(home)
			if err != nil {
				t.Fatal(err)
			}
			if shape == "below-floor-ledger" {
				state, err := store.ReadLedger()
				if err != nil {
					t.Fatal(err)
				}
				payload, _, err := MarshalLedgerV2(FreshBootstrapLedgerV2(historyV028), historyV028)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.CompareAndSwapLedger(state, payload); err != nil {
					t.Fatal(err)
				}
			} else {
				journal.Steps[len(journal.Steps)-1].Checkpoint = StepApplied
				evidence := *journal.Steps[len(journal.Steps)-1].Evidence
				evidence.Checkpoint = EvidenceApplied
				journal.Steps[len(journal.Steps)-1].Evidence = &evidence
				payload, err := MarshalJournal(journal)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.CompareAndSwapCurrentJournal(snapshot, payload); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshotV2ProtectedTree(t, home)
			links := &ReleaseLinks{Active: journal.Goal.Target}
			options := currentHistoryOptions(t, home, links, "0.29.0-bbbbbbbbbbbb")
			options.Replacement = &JournalReplacement{Transaction: journal.Transaction, Fingerprint: snapshot.Fingerprint, Reason: JournalReplacementNewerRelease}
			_, err = NewV2Transition(options)
			if err == nil || err.Error() != "invalid release transition record: "+manualMigrationRepair {
				t.Fatalf("manual repair refusal: %v", err)
			}
			if !bytes.Equal(before, snapshotV2ProtectedTree(t, home)) {
				t.Fatal("refusal wrote protected state")
			}
		})
	}
}

// Synthetic completed two-migration history is frozen from the old test owner.
// Reader tests seed records directly; no removed planner or effect is replayed.
func seedLegacyReaderFixture(t *testing.T, home string, owner bool) {
	seedLegacyFixture(t, home, owner, true)
}

func seedLegacyCompletedLedger(t *testing.T, home string) { seedLegacyFixture(t, home, false, false) }

func seedLegacyFixture(t *testing.T, home string, owner, history bool) {
	t.Helper()
	name := "legacy-reader-complete.json"
	if owner {
		name = "legacy-reader-owner-complete.json"
	}
	seedFrozenReaderTree(t, home, name, history)
}

func seedFrozenReaderTree(t *testing.T, home, name string, history bool) {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var files map[string]string
	if err := json.Unmarshal(payload, &files); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if !history && name != "release-transition/v2/ledger.json" && name != "yards/hermes/config.env" {
			continue
		}
		path := filepath.Join(home, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, []byte(content), 0600)
	}
}

func TestFrozenV028HistoryCompactsCompletedSuffixWithoutHandlers(t *testing.T) {
	home, journal, snapshot := frozenAnchorFixture(t, "0.28.0")
	store, err := NewPOSIXV2Store(home)
	if err != nil {
		t.Fatal(err)
	}
	original, err := store.ReadMigrationLedger(historyV028, historyV028Digest)
	if err != nil {
		t.Fatal(err)
	}
	epochs := map[string]int{}
	for domain, state := range original.Ledger.Domains {
		epochs[domain] = state.Epoch
	}
	checkpoint, err := newLedgerCheckpointV1(original.Ledger, historyV028, historyV028Digest, original.ProjectionSnapshot, epochs)
	if err != nil {
		t.Fatal(err)
	}
	for domain, state := range checkpoint.Domains {
		state.CompactedThrough = 1
		state.PrefixDigest, err = historyV028.MigrationPrefixDigest(domain, 1)
		if err != nil {
			t.Fatal(err)
		}
		state.AppliedSuffix = nonNilIDs(historyV028.appliedSuffix(domain, 1, state.Epoch))
		checkpoint.Domains[domain] = state
	}
	payload, _, err := MarshalLedgerCheckpointV1(checkpoint, historyV028, historyV028Digest, original.ProjectionSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.compareAndSwap([]string{"release-transition", "v2"}, migrationLedgerCheckpointName, absentProtectedSnapshot(), payload); err != nil {
		t.Fatal(err)
	}
	journal.Checkpoint = JournalComplete
	payload, err = MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapCurrentJournal(snapshot, payload); err != nil {
		t.Fatal(err)
	}
	links := &ReleaseLinks{Active: journal.Goal.Target}
	options := currentHistoryOptions(t, home, links, "0.29.0-bbbbbbbbbbbb")
	candidate, err := NewV2Transition(options)
	if err != nil {
		t.Fatal(err)
	}
	goal := Goal{Target: options.Releases.Target, Direction: DirectionActivateTarget}
	inspection, err := candidate.Inspect(context.Background(), goal)
	if err != nil || len(inspection.Blockers) != 0 || !inspection.Assessment.Changed {
		t.Fatalf("compaction inspection: %#v %v", inspection, err)
	}
	outcome, err := candidate.Converge(context.Background(), Execution{Plan: inspection.Plan})
	if err != nil || outcome.Code != CodeConfirmationRequired {
		t.Fatalf("compaction without consent: %#v %v", outcome, err)
	}
	outcome, err = candidate.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
	if err != nil || outcome.Status != StatusReady {
		t.Fatalf("compaction: %#v %v", outcome, err)
	}
	current, err := store.ReadMigrationLedger(candidate.registry, candidate.registryDigest)
	if err != nil || ledgerCheckpointHasSuffix(current.Checkpoint) || !sameLedgerV2(current.Ledger, original.Ledger) || !bytes.Equal(current.ProjectionSnapshot.Payload, original.ProjectionSnapshot.Payload) {
		t.Fatalf("compaction changed completed history: %#v %v", current, err)
	}
	if _, err := store.ReadMigrationLedger(historyV028, historyV028Digest); err != nil {
		t.Fatalf("old checkpoint reader: %v", err)
	}
}

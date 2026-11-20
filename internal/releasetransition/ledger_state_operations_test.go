package releasetransition

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

// One state/operation table owns damage admission and repeated-observation
// invariants. The separate process matrix additionally SIGKILLs every durable cut.
func TestMigrationLedgerStateOperations(t *testing.T) {
	type row struct {
		name string
		run  func(*testing.T)
	}
	var rows []row
	for _, version := range []string{"0.28.0", "0.28.1", "0.29.0", "0.30.0"} {
		for _, checkpointed := range []bool{false, true} {
			base := fmt.Sprintf("%s/checkpoint=%t", version, checkpointed)
			rows = append(rows, row{"completed/" + base, func(t *testing.T) {
				home, links, options := ledgerStateInstallation(t, version, checkpointed)
				for _, target := range []ReleaseID{"0.30.0-bbbbbbbbbbbb", "0.31.0-cccccccccccc"} {
					options.Releases = ReleasePair{From: links.Active, Previous: cloneReleaseID(links.Previous), Target: target}
					options.NewTransactionID = func() TransactionID { return TransactionID("tx-" + string(target)) }
					ledgerStateReady(t, home, options)
				}
			}})
			for _, damage := range []string{"delete-ledger", "delete-checkpoint", "delete-both", "corrupt-ledger", "corrupt-checkpoint", "corrupt-both", "foreign-ledger", "foreign-checkpoint"} {
				for _, journal := range []string{"same-goal", "other-goal", "none"} {
					for _, capability := range []bool{false, true} {
						rows = append(rows, row{fmt.Sprintf("damage/%s/%s/%s/cap=%t", base, damage, journal, capability), func(t *testing.T) {
							home, links, options := ledgerStateInstallation(t, version, checkpointed)
							options.Releases.Target = "0.30.0-bbbbbbbbbbbb"
							ledgerStateReady(t, home, options)
							damageLedgerState(t, home, damage)
							options.MigrationCheckpoint = capability
							options.Releases = ReleasePair{From: links.Active, Previous: cloneReleaseID(links.Previous), Target: links.Active}
							if journal == "other-goal" {
								options.Releases.Target = "0.31.0-cccccccccccc"
							}
							if journal == "none" {
								if err := os.Remove(filepath.Join(home, "release-transition/v2/journal.json")); err != nil {
									t.Fatal(err)
								}
							}
							options.NewTransactionID = func() TransactionID { return "tx-republished" }
							if damage == "delete-checkpoint" && checkpointed && version != "0.30.0" && journal == "same-goal" {
								// Only the checkpoint recorded the live ledger-only step; the
								// completed same-goal journal must not be reported Ready on the
								// stale projection. Other goals replay the ledger-only step.
								ledgerStateNotReady(t, home, options)
								return
							}
							if damage == "delete-checkpoint" {
								ledgerStateReady(t, home, options)
								return
							}
							before := snapshotV2ProtectedTree(t, home)
							engine, err := NewV2Transition(options)
							// Corruption can be refused at read-only constructor admission.
							if err != nil {
								if !errors.Is(err, ErrInvalid) {
									t.Fatalf("wrong constructor refusal: %v", err)
								}
								_, again := NewV2Transition(options)
								if again == nil || again.Error() != err.Error() {
									t.Fatal("constructor refusal changed", again)
								}
								if !bytes.Equal(before, snapshotV2ProtectedTree(t, home)) {
									t.Fatal("constructor refusal wrote state")
								}
								return
							}
							goal := Goal{Target: options.Releases.Target, Direction: DirectionActivateTarget}
							first, err := engine.Inspect(context.Background(), goal)
							if err == nil {
								t.Fatalf("damaged history produced a plan/Ready: %#v", first)
							}
							if damage == "delete-both" && (!errors.Is(err, ErrMigrationLedgerAbsent) || err.Error() != ErrMigrationLedgerAbsent.Error()) {
								t.Fatalf("wrong absence refusal: %v", err)
							}
							if damage != "delete-both" && !errors.Is(err, ErrInvalid) {
								t.Fatalf("wrong corruption refusal: %v", err)
							}
							repeated, again := engine.Inspect(context.Background(), goal)
							if again == nil || again.Error() != err.Error() || !reflect.DeepEqual(first, repeated) {
								t.Fatal("refusal observation was not idempotent", again)
							}
							out, convergeErr := engine.Converge(context.Background(), Execution{Plan: PlanToken("plan-v1-" + string(digestA)), Authorization: "untrusted"})
							if len(out.Warnings) != 0 || convergeErr == nil && out.Status != StatusOperatorActionRequired {
								t.Fatalf("damaged history converged: %#v %v", out, convergeErr)
							}
							if convergeErr == nil && (out.Code != CodePlanStale || out.Message == "") {
								t.Fatalf("wrong damage outcome: %#v", out)
							}
							if damage == "delete-both" && convergeErr == nil && out.Message != "protected migration history is absent: ledger.json and history-checkpoint.json are missing" {
								t.Fatalf("lost absence diagnostic: %#v", out)
							}
							if !bytes.Equal(before, snapshotV2ProtectedTree(t, home)) {
								t.Fatal("damage refusal changed protected bytes")
							}
						}})
					}
				}
			}
			for _, verified := range []bool{false, true} {
				if version == "0.30.0" {
					// Takeover covers frozen v0.28 history; this target is not newer.
					break
				}
				rows = append(rows, row{fmt.Sprintf("takeover/%s/verified=%t", base, verified), func(t *testing.T) {
					home, links, options := ledgerStateInstallation(t, version, checkpointed)
					store, _ := NewPOSIXV2Store(home)
					snapshot, _ := store.ReadCurrentJournal()
					journal, err := ParseJournal(snapshot.Payload)
					if err != nil {
						t.Fatal(err)
					}
					journal.Checkpoint = JournalMigrating
					if !verified {
						step := &journal.Steps[len(journal.Steps)-1]
						record, err := store.ReadCheckpointEvidence(journal.Transaction, step.ID, EvidenceApplied)
						if err != nil {
							t.Fatal(err)
						}
						evidence, err := ParseEvidence(record.Payload)
						if err != nil {
							t.Fatal(err)
						}
						step.Checkpoint, step.Evidence = StepApplied, &evidence
					}
					payload, err := MarshalJournal(journal)
					if err != nil {
						t.Fatal(err)
					}
					if err := store.CompareAndSwapCurrentJournal(snapshot, payload); err != nil {
						t.Fatal(err)
					}
					changed, _ := store.ReadCurrentJournal()
					options.Releases = ReleasePair{From: links.Active, Target: "0.30.0-bbbbbbbbbbbb"}
					options.CandidateVersion = "0.30.0"
					options.Replacement = &JournalReplacement{Transaction: journal.Transaction, Fingerprint: changed.Fingerprint, Reason: JournalReplacementNewerRelease}
					before := snapshotV2ProtectedTree(t, home)
					if !verified {
						_, err := NewV2Transition(options)
						if !errors.Is(err, ErrRetiredMigrationRepair) {
							t.Fatalf("non-verified takeover: %v", err)
						}
						if !bytes.Equal(before, snapshotV2ProtectedTree(t, home)) {
							t.Fatal("manual repair refusal wrote state")
						}
						return
					}
					ledgerStateReady(t, home, options)
				}})
			}
		}
	}
	for _, cut := range []string{"", "after-journal-authorized", "before-baseline-ledger", "after-baseline-ledger", "after-step-verified", "after-target-active", "after-reconciling", "before-reconciler-0", "after-reconciler-0", "before-checkpoint-publication", "after-checkpoint-publication"} {
		for _, capability := range []bool{false, true} {
			rows = append(rows, row{fmt.Sprintf("fresh/%s/cap=%t", cut, capability), func(t *testing.T) {
				home := testkit.TempDir(t)
				options, store := bootstrapConsumerOptions(t, home)
				options.MigrationCheckpoint = capability
				checkpointCut := cut == "before-checkpoint-publication" || cut == "after-checkpoint-publication"
				wantHit := cut != "" && (capability || !checkpointCut)
				hit := false
				if cut != "" {
					options.fault = func(point string) error {
						if point == cut && !hit {
							hit = true
							return errors.New("state-table interruption")
						}
						return nil
					}
				}
				engine, err := NewV2Transition(options)
				if err != nil {
					t.Fatal(err)
				}
				goal := Goal{Target: options.Releases.Target, Direction: DirectionActivateTarget}
				first, err := engine.Inspect(context.Background(), goal)
				if err != nil {
					t.Fatal(err)
				}
				same, err := engine.Inspect(context.Background(), goal)
				if err != nil || first.Plan != same.Plan {
					t.Fatal("fresh Inspect changed plan", err)
				}
				out, err := engine.Converge(context.Background(), Execution{Plan: first.Plan, Authorization: v2TestAuthorization(first.Plan)})
				wantStatus, wantCode := StatusRecovering, CodeVerificationFailed
				if !wantHit || cut == "after-checkpoint-publication" {
					wantStatus, wantCode = StatusReady, CodeReady
				}
				if len(out.Warnings) != 0 || err != nil || out.Status != wantStatus || out.Code != wantCode {
					t.Fatalf("fresh interruption: %#v %v", out, err)
				}
				if hit != wantHit {
					t.Fatalf("fresh cut hit=%t, want %t", hit, wantHit)
				}
				options.fault = nil
				ledgerStateReady(t, home, options)
				checkpoint, err := store.ReadMigrationCheckpoint()
				if err != nil || checkpoint.Exists != capability {
					t.Fatalf("checkpoint capability=%t: exists=%t err=%v", capability, checkpoint.Exists, err)
				}
			}})
		}
	}
	for _, baseline := range []bool{false, true} {
		rows = append(rows, row{fmt.Sprintf("replacement/absent/baseline=%t", baseline), func(t *testing.T) {
			home := testkit.TempDir(t)
			options, store := bootstrapConsumerOptions(t, home)
			if !baseline {
				seedCompletedInstallationFixture(t, home, "0.28.1", false)
				options.Releases = ReleasePair{From: "0.28.1-aaaaaaaaaaaa", Target: "0.30.0-bbbbbbbbbbbb"}
				options.Reconcilers = nil
			}
			options.fault = func(point string) error {
				if point == "after-journal-authorized" {
					return errors.New("stop at journal")
				}
				return nil
			}
			engine, err := NewV2Transition(options)
			if err != nil {
				t.Fatal(err)
			}
			goal := Goal{Target: options.Releases.Target, Direction: DirectionActivateTarget}
			plan, err := engine.Inspect(context.Background(), goal)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = engine.Converge(context.Background(), Execution{Plan: plan.Plan, Authorization: v2TestAuthorization(plan.Plan)})
			snapshot, _ := store.ReadCurrentJournal()
			journal, err := ParseJournal(snapshot.Payload)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"ledger.json", migrationLedgerCheckpointName} {
				err := os.Remove(filepath.Join(home, "release-transition/v2", name))
				if err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
			}
			options.fault = nil
			options.Releases = ReleasePair{From: journal.Releases.From, Target: "0.31.0-cccccccccccc"}
			options.CandidateVersion = "0.31.0"
			options.Replacement = &JournalReplacement{Transaction: journal.Transaction, Fingerprint: snapshot.Fingerprint, Reason: JournalReplacementNewerRelease}
			engine, err = NewV2Transition(options)
			if err != nil {
				t.Fatal(err)
			}
			before := snapshotV2ProtectedTree(t, home)
			goal.Target = options.Releases.Target
			inspected, err := engine.Inspect(context.Background(), goal)
			if baseline {
				if err != nil || inspected.Outcome == nil || inspected.Outcome.Code != CodeRecoveryAmbiguous || len(inspected.Outcome.Warnings) != 0 {
					t.Fatalf("baseline takeover refusal: %#v %v", inspected, err)
				}
			} else if !errors.Is(err, ErrMigrationLedgerAbsent) {
				t.Fatalf("missing ordinary ledger: %#v %v", inspected, err)
			}
			repeated, again := engine.Inspect(context.Background(), goal)
			if !reflect.DeepEqual(inspected, repeated) || (err == nil) != (again == nil) {
				t.Fatal("replacement Inspect changed refusal")
			}
			out, convergeErr := engine.Converge(context.Background(), Execution{Plan: PlanToken("plan-v1-" + string(digestA)), Authorization: "untrusted"})
			wantCode, wantMessage := CodePlanStale, ErrMigrationLedgerAbsent.Error()
			if baseline {
				wantCode, wantMessage = CodeRecoveryAmbiguous, "complete the initial ledger publication with the installed release before replacing its journal"
			}
			if convergeErr != nil || out.Status != StatusOperatorActionRequired || out.Code != wantCode || out.Message != wantMessage || len(out.Warnings) != 0 {
				t.Fatalf("absent replacement converge: %#v %v", out, convergeErr)
			}
			if !bytes.Equal(before, snapshotV2ProtectedTree(t, home)) {
				t.Fatal("absent replacement wrote state")
			}
		}})
	}
	for _, capability := range []bool{false, true} {
		rows = append(rows, row{fmt.Sprintf("rollback-blocked/absent/cap=%t", capability), func(t *testing.T) {
			home, links, options := ledgerStateInstallation(t, "0.28.1", false)
			if err := os.Remove(filepath.Join(home, "release-transition/v2/ledger.json")); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(home, "release-transition/v2/journal.json")); err != nil {
				t.Fatal(err)
			}
			options.MigrationCheckpoint = capability
			options.Direction = DirectionActivatePrevious
			options.Releases = ReleasePair{From: links.Active, Previous: releaseIDPointer("release-previous"), Target: "release-previous"}
			links.Previous = releaseIDPointer("release-previous")
			options.RollbackTarget = &RollbackTarget{Version: "0.28.1", RegistryDigest: digestC}
			engine, err := NewV2Transition(options)
			if err != nil {
				t.Fatal(err)
			}
			before := snapshotV2ProtectedTree(t, home)
			goal := Goal{Target: options.Releases.Target, Direction: DirectionActivatePrevious}
			first, err := engine.Inspect(context.Background(), goal)
			if err != nil || first.Outcome == nil || first.Outcome.Status != StatusOperatorActionRequired || first.Outcome.Code != CodeRollbackIncompatible || len(first.Outcome.Warnings) != 0 {
				t.Fatalf("rollback refusal: %#v %v", first, err)
			}
			again, err := engine.Inspect(context.Background(), goal)
			if err != nil || !reflect.DeepEqual(first, again) || !bytes.Equal(before, snapshotV2ProtectedTree(t, home)) {
				t.Fatal("rollback refusal changed state", err)
			}
		}})
	}
	t.Logf("state-operation rows: %d", len(rows))
	for _, row := range rows {
		t.Run(row.name, row.run)
	}
}

func ledgerStateInstallation(t *testing.T, version string, checkpointed bool) (string, *ReleaseLinks, V2Options) {
	t.Helper()
	home := testkit.TempDir(t)
	journal := seedCompletedInstallationFixture(t, home, version, checkpointed)
	links := &ReleaseLinks{Active: journal.Releases.Target}
	options := currentHistoryOptions(t, home, links, "0.30.0-bbbbbbbbbbbb")
	return home, links, options
}

func ledgerStateReady(t *testing.T, home string, options V2Options) {
	t.Helper()
	engine, err := NewV2Transition(options)
	if err != nil {
		t.Fatal(err)
	}
	goal := Goal{Target: options.Releases.Target, Direction: DirectionActivateTarget}
	first, err := engine.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	again, err := engine.Inspect(context.Background(), goal)
	if err != nil || first.Plan != again.Plan {
		t.Fatal("Inspect changed plan", err)
	}
	out, err := engine.Converge(context.Background(), Execution{Plan: first.Plan, Authorization: v2TestAuthorization(first.Plan)})
	if err != nil || out.Status != StatusReady || len(out.Warnings) != 0 {
		t.Fatalf("state operation: %#v %v", out, err)
	}
	after, err := engine.Inspect(context.Background(), goal)
	if err != nil || after.Outcome == nil || after.Outcome.Status != StatusReady || len(after.Outcome.Warnings) != 0 {
		t.Fatalf("plan -> Ready -> plan loop: %#v %v", after, err)
	}
	twice, err := engine.Inspect(context.Background(), goal)
	if err != nil || !reflect.DeepEqual(after, twice) {
		t.Fatal("Ready Inspect not idempotent", err)
	}
}

func ledgerStateNotReady(t *testing.T, home string, options V2Options) {
	t.Helper()
	projection, err := os.ReadFile(filepath.Join(home, "release-transition/v2/ledger.json"))
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewV2Transition(options)
	if err != nil {
		t.Fatal(err)
	}
	goal := Goal{Target: options.Releases.Target, Direction: DirectionActivateTarget}
	first, err := engine.Inspect(context.Background(), goal)
	if err != nil || first.Outcome != nil && first.Outcome.Status == StatusReady {
		t.Fatalf("lost checkpoint inspected Ready: %#v %v", first, err)
	}
	out, err := engine.Converge(context.Background(), Execution{Plan: first.Plan, Authorization: v2TestAuthorization(first.Plan)})
	if err != nil || out.Status == StatusReady {
		t.Fatalf("lost checkpoint converged Ready: %#v %v", out, err)
	}
	after, err := os.ReadFile(filepath.Join(home, "release-transition/v2/ledger.json"))
	if err != nil || !bytes.Equal(after, projection) {
		t.Fatal("lost checkpoint rewrote the legacy projection", err)
	}
}

func damageLedgerState(t *testing.T, home, damage string) {
	t.Helper()
	root := filepath.Join(home, "release-transition/v2")
	switch damage {
	case "delete-ledger", "delete-checkpoint", "delete-both":
		names := []string{"ledger.json", migrationLedgerCheckpointName}
		if damage == "delete-ledger" {
			names = names[:1]
		}
		if damage == "delete-checkpoint" {
			names = names[1:]
		}
		for _, name := range names {
			if err := os.Remove(filepath.Join(root, name)); err != nil {
				t.Fatal(err)
			}
		}
	case "corrupt-ledger":
		testkit.WriteFile(t, filepath.Join(root, "ledger.json"), []byte("{"), 0600)
	case "corrupt-checkpoint":
		testkit.WriteFile(t, filepath.Join(root, migrationLedgerCheckpointName), []byte("{"), 0600)
	case "corrupt-both":
		for _, name := range []string{"ledger.json", migrationLedgerCheckpointName} {
			testkit.WriteFile(t, filepath.Join(root, name), []byte("{"), 0600)
		}
	case "foreign-ledger":
		payload, err := os.ReadFile("../../config/release-transition.json")
		if err != nil {
			t.Fatal(err)
		}
		registry, _, err := ParseRegistryV2(payload, BuiltinCapabilityCatalog())
		if err != nil {
			t.Fatal(err)
		}
		payload, _, err = MarshalLedgerV2(FreshBootstrapLedgerV2(registry), registry)
		if err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(root, "ledger.json"), payload, 0600)
	case "foreign-checkpoint":
		payload, err := os.ReadFile(filepath.Join(root, migrationLedgerCheckpointName))
		if err != nil {
			t.Fatal(err)
		}
		var cp LedgerCheckpointV1
		if err := json.Unmarshal(payload, &cp); err != nil {
			t.Fatal(err)
		}
		cp.LegacyProjection.Fingerprint = digestC
		payload, err = json.Marshal(cp)
		if err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(root, migrationLedgerCheckpointName), payload, 0600)
	}
}

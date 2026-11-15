package releasetransition

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestFreshBootstrapStalePlanRetainsBaselineJournal(t *testing.T) {
	for _, point := range []string{"after-journal-authorized", "before-baseline-ledger", "after-baseline-ledger", "after-step-verified"} {
		for _, changed := range []string{"ingress", "artifact", "newer-release"} {
			if changed == "newer-release" && point != "after-journal-authorized" && point != "before-baseline-ledger" {
				continue // This guard protects only an unpublished ledger.
			}
			t.Run(point+"/"+changed, func(t *testing.T) {
				options, store := bootstrapConsumerOptions(t, testkit.TempDir(t))
				options.fault = func(at string) error {
					if at == point {
						return errors.New("stop bootstrap")
					}
					return nil
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
				interrupted, err := engine.Converge(context.Background(), Execution{Plan: first.Plan, Authorization: v2TestAuthorization(first.Plan)})
				if err != nil || interrupted.Status != StatusRecovering {
					t.Fatalf("interruption: %#v %v", interrupted, err)
				}
				before, err := store.ReadCurrentJournal()
				if err != nil {
					t.Fatal(err)
				}
				original, err := ParseJournal(before.Payload)
				if err != nil {
					t.Fatal(err)
				}
				options.fault = nil
				staleOptions := options
				staleGoal, wantCode := goal, CodePlanStale
				if changed == "ingress" {
					// A genuine input change with the same immutable release/artifact.
					staleOptions.Ingress = &v2IngressFixture{inspection: V2IngressInspection{Operations: []V2IngressOperation{{Kind: V2SourceImport, Decision: DecisionCanonicalize, Expected: digestA, Desired: digestB}}, Prospective: v2DiskSettingsView{configHome: options.ConfigHome}}, actual: map[V2IngressOperationKind]Fingerprint{V2SourceImport: digestA}}
				} else if changed == "artifact" {
					// Defensive check too: a conflicting artifact cannot discard bootstrap.
					staleOptions.ArtifactDigest = digestC
				} else {
					// A real explicit newer-release request also needs the first
					// owner's baseline authority; it is not a retired migration.
					staleOptions.Releases = ReleasePair{From: options.Releases.Target, Target: "0.30.0-cccccccccccc"}
					staleOptions.CandidateVersion, staleOptions.ArtifactDigest = "0.30.0", digestC
					staleOptions.Replacement = &JournalReplacement{Transaction: original.Transaction, Fingerprint: before.Fingerprint, Reason: JournalReplacementNewerRelease}
					staleGoal.Target, wantCode = staleOptions.Releases.Target, CodeRecoveryAmbiguous
				}
				stale, err := NewV2Transition(staleOptions)
				if err != nil {
					t.Fatal(err)
				}
				snapshot := snapshotV2ProtectedTree(t, options.ConfigHome)
				blocked, err := stale.Inspect(context.Background(), staleGoal)
				if err != nil || blocked.Outcome == nil || blocked.Outcome.Code != wantCode {
					t.Fatalf("fresh bootstrap became manual repair or replacement: %#v %v", blocked, err)
				}
				if !bytes.Equal(snapshot, snapshotV2ProtectedTree(t, options.ConfigHome)) {
					t.Fatal("stale inspection changed protected state")
				}
				resumed, err := NewV2Transition(options)
				if err != nil {
					t.Fatal(err)
				}
				plan, err := resumed.Inspect(context.Background(), goal)
				if err != nil {
					t.Fatal(err)
				}
				ready, err := resumed.Converge(context.Background(), Execution{Plan: plan.Plan})
				if err != nil || ready.Status != StatusReady {
					t.Fatalf("original bootstrap did not resume: %#v %v", ready, err)
				}
				after, err := store.ReadCurrentJournal()
				if err != nil {
					t.Fatal(err)
				}
				complete, err := ParseJournal(after.Payload)
				if err != nil || complete.Transaction != original.Transaction || complete.AuthorizationPlan != original.AuthorizationPlan || complete.AuthorizationDigest != original.AuthorizationDigest || complete.ResumePlan != original.ResumePlan || complete.ObservationScope != original.ObservationScope {
					t.Fatal("bootstrap discarded original transaction/grant", err)
				}
			})
		}
	}
}

func TestAppliedIngressStillAllowsPreActivationReplacement(t *testing.T) {
	transition, home, _ := v2TransitionFixture(t, func(point string) error {
		if point == "after-step-verified" {
			return errors.New("stop after ingress")
		}
		return nil
	})
	source := &v2IngressFixture{inspection: V2IngressInspection{Operations: []V2IngressOperation{{Kind: V2SourceImport, Decision: DecisionCanonicalize, Expected: digestB, Desired: digestC}}, Prospective: v2DiskSettingsView{configHome: home, yards: []string{"hermes"}}}, actual: map[V2IngressOperationKind]Fingerprint{V2SourceImport: digestB}}
	transition.options.Ingress = source
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	first, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	interrupted, err := transition.Converge(context.Background(), Execution{Plan: first.Plan, Authorization: v2TestAuthorization(first.Plan)})
	if err != nil || interrupted.Status != StatusRecovering {
		t.Fatalf("interruption: %#v %v", interrupted, err)
	}
	journal, err := transition.store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	before, err := ParseJournal(journal.Payload)
	if err != nil || len(before.Steps) != 1 || before.Steps[0].Checkpoint != StepVerified {
		t.Fatalf("ingress not verified: %#v %v", before, err)
	}
	source.inspection.Operations = append(source.inspection.Operations, V2IngressOperation{Kind: V2SourceEntrypoints, Decision: DecisionCanonicalize, Expected: digestC, Desired: digestA})
	source.actual[V2SourceEntrypoints] = digestC
	transition.options.fault = nil
	transition.options.NewTransactionID = func() TransactionID { return "tx-replaced-ingress" }
	next, err := transition.Inspect(context.Background(), goal)
	if err != nil || next.Resume != nil || len(next.Blockers) != 0 || next.Plan == first.Plan {
		t.Fatalf("applied ingress lost existing replacement path: %#v %v", next, err)
	}
	noGrant, err := transition.Converge(context.Background(), Execution{Plan: next.Plan})
	if err != nil || noGrant.Code != CodeConfirmationRequired {
		t.Fatalf("replacement reused old grant: %#v %v", noGrant, err)
	}
	ready, err := transition.Converge(context.Background(), Execution{Plan: next.Plan, Authorization: v2TestAuthorization(next.Plan)})
	if err != nil || ready.Status != StatusReady || ready.Transaction == nil || *ready.Transaction == before.Transaction {
		t.Fatalf("replacement: %#v %v", ready, err)
	}
}

package releasetransition

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
)

func migrationOwnerFixture(t *testing.T) (*V2Transition, MigrationOwnerReplacementRequest, *ReleaseLinks) {
	return migrationOwnerFixtureWithPrevious(t, false)
}

func migrationOwnerFixtureWithPrevious(t *testing.T, retainedPrevious bool) (*V2Transition, MigrationOwnerReplacementRequest, *ReleaseLinks) {
	t.Helper()
	ctx := context.Background()
	links := ReleaseLinks{Active: "release-a"}
	pair := ReleasePair{From: "release-a", Target: "release-b"}
	if retainedPrevious {
		links.Previous = releaseIDPointer("release-previous")
		pair.Previous = links.Previous
	}
	old, _, _ := v2TransitionFixtureWithReleases(t, nil, pair, links)
	payload, err := os.ReadFile(filepath.Join("..", "..", "config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	old.registry, old.registryDigest, err = ParseRegistryV2(payload, old.catalog)
	if err != nil {
		t.Fatal(err)
	}
	old.options.RegistryPayload = payload
	old.options.ArtifactDigest = AffectedMigrationOwnerManifest
	old.options.MigrationCheckpoint = true
	old.options.SourceMigrationCheckpoint = true
	old.options.ObserveLinks = func(context.Context) (ReleaseLinks, error) { return links, nil }
	old.options.ActivateLinks = func(_ context.Context, pair ReleasePair) (ReleaseLinks, error) {
		links = ReleaseLinks{Active: pair.Target, Previous: releaseIDPointer(pair.From)}
		return links, nil
	}
	ledger := BaselineLedgerV2(old.registry)
	for _, migration := range old.registry.Migrations {
		if migration.Domain == "power-metadata" || migration.Kind == localSSHPortMigration {
			continue
		}
		state := ledger.Domains[migration.Domain]
		state.Epoch = migration.ToEpoch
		state.Applied = append(state.Applied, migration.ID)
		ledger.Domains[migration.Domain] = state
	}
	projection, _, err := MarshalLedgerV2(ledger, old.registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.store.CompareAndSwapLedger(absentProtectedSnapshot(), projection); err != nil {
		t.Fatal(err)
	}
	state, err := old.store.ReadMigrationLedger(old.registry, old.registryDigest)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.store.ConvertMigrationLedger(state, old.registry, old.registryDigest, old.registry, old.registryDigest); err != nil {
		t.Fatal(err)
	}
	old.options.Reconcilers = []V2ActivationReconciler{&v2PostErrorReconciler{post: V2ActivationObservation{Actual: digestB, Desired: digestA}, reconcileErr: errors.New("injected old owner defect")}}
	inspection, err := old.Inspect(ctx, Goal{Target: "release-b", Direction: DirectionActivateTarget})
	if err != nil || len(inspection.Blockers) != 0 {
		t.Fatalf("old inspection: %+v %v", inspection, err)
	}
	outcome, err := old.Converge(ctx, Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
	if err != nil || outcome.Status != StatusRecovering {
		t.Fatalf("seed interrupted owner: %+v %v", outcome, err)
	}
	snapshot, err := old.store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	request := MigrationOwnerReplacementRequest{Transaction: *outcome.Transaction, Fingerprint: snapshot.Fingerprint}
	options := old.options
	options.CandidateVersion = "0.22.0"
	options.ArtifactDigest = digestB
	options.Releases = ReleasePair{From: "release-b", Target: "release-c", Previous: releaseIDPointer("release-a")}
	options.MigrationOwnerReplacement = &request
	options.MigrationOwnerAssets = func(context.Context) (MigrationOwnerAssetsProof, error) {
		source := old.registry
		source.CurrentEpochs = map[string]int{"owner-registration": 2, "power-metadata": 1, "project-state": 1, "settings": 2}
		source.Migrations = nil
		for _, migration := range old.registry.Migrations {
			if migration.Domain != "power-metadata" && migration.Kind != localSSHPortMigration {
				source.Migrations = append(source.Migrations, migration)
			}
		}
		sourcePayload, _ := json.Marshal(source)
		proof := MigrationOwnerAssetsProof{Binding: digestA, SourceRegistry: sourcePayload}
		if retainedPrevious {
			proof.PreviousSource = &MigrationOwnerPreviousSource{Release: "release-previous", Artifact: digestB, Registry: fingerprintPayload(sourcePayload), Inputs: digestA}
		}
		return proof, nil
	}
	options.Reconcilers = []V2ActivationReconciler{&v2TestReconciler{id: "post-error-runtime", converged: true}}
	candidate, err := NewV2Transition(options)
	if err != nil {
		t.Fatal(err)
	}
	return candidate, request, &links
}

func TestMigrationOwnerReplacementPreservesHistoryAndResumes(t *testing.T) {
	for _, point := range []string{"", "after-migration-owner-receipt", "after-replacement-journal-cas", "after-target-active", "receipt:after-pending-fsync", "receipt:before-publish", "receipt:after-publish-before-dir-fsync", "journal:after-pending-fsync", "journal:before-publish", "journal:after-publish-before-dir-fsync"} {
		t.Run(point, func(t *testing.T) {
			candidate, request, links := migrationOwnerFixture(t)
			ctx := context.Background()
			goal := Goal{Target: "release-c", Direction: DirectionActivateTarget}
			before, err := candidate.store.ReadMigrationLedger(candidate.registry, candidate.registryDigest)
			if err != nil {
				t.Fatal(err)
			}
			inspection, err := candidate.Inspect(ctx, goal)
			if err != nil || len(inspection.Blockers) != 0 || inspection.Resume != nil || !inspection.Assessment.Changed {
				t.Fatalf("fresh inspection: %+v %v", inspection, err)
			}
			unchanged, _ := candidate.store.ReadCurrentJournal()
			if unchanged.Fingerprint != request.Fingerprint {
				t.Fatal("inspection mutated predecessor")
			}
			freshProcess, err := NewV2Transition(candidate.options)
			if err != nil {
				t.Fatal(err)
			}
			candidate = freshProcess
			faultTriggered := false
			journalArmed := false
			candidate.store.fault = func(actual string) error {
				scope, wanted, found := strings.Cut(point, ":")
				if found && actual == wanted && (scope == "receipt" && !journalArmed || scope == "journal" && journalArmed) {
					faultTriggered = true
					return errors.New("injected immutable/CAS publication failure")
				}
				return nil
			}
			candidate.options.fault = func(actual string) error {
				if actual == "after-migration-owner-receipt" {
					journalArmed = true
				}
				if point != "" && actual == point {
					faultTriggered = true
					return errors.New("injected receipt/CAS interruption")
				}
				return nil
			}
			outcome, err := candidate.Converge(ctx, Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
			if point != "" && !faultTriggered {
				t.Fatalf("selected fault cut %s did not execute", point)
			}
			if point == "" && (err != nil || outcome.Status != StatusReady) {
				t.Fatalf("convergence: %+v %v", outcome, err)
			}
			options := candidate.options
			options.fault = nil
			current, _ := candidate.store.ReadCurrentJournal()
			journal, err := ParseJournal(current.Payload)
			if err != nil {
				t.Fatal(err)
			}
			if journal.Transaction != request.Transaction {
				options.MigrationOwnerReplacement = nil
			}
			restarted, err := NewV2Transition(options)
			if err != nil {
				t.Fatal(err)
			}
			resumed, err := restarted.Inspect(ctx, goal)
			if err != nil || len(resumed.Blockers) != 0 {
				t.Fatalf("restart inspection: %+v %v", resumed, err)
			}
			execution := Execution{Plan: resumed.Plan}
			if resumed.Resume == nil && resumed.Assessment.Changed {
				execution.Authorization = v2TestAuthorization(resumed.Plan)
			}
			outcome, err = restarted.Converge(ctx, execution)
			if err != nil || outcome.Status != StatusReady || links.Active != "release-c" {
				t.Fatalf("restart convergence: %+v %v", outcome, err)
			}
			after, err := restarted.store.ReadMigrationLedger(candidate.registry, candidate.registryDigest)
			if err != nil || !bytes.Equal(before.ProjectionSnapshot.Payload, after.ProjectionSnapshot.Payload) {
				t.Fatalf("projection changed: %v", err)
			}
			receipts, err := candidate.store.migrationOwnerReceipts()
			if err != nil || len(receipts) != 1 {
				t.Fatalf("receipt graph: %+v %v", receipts, err)
			}
			for _, receipt := range receipts {
				if receipt.Predecessor.Checkpoint != JournalReconciling || receipt.Successor.Goal.Target != "release-c" {
					t.Fatal("old activation was closed as ready")
				}
			}
		})
	}
}

func TestMigrationOwnerReplacementRejectsUnprovedInputsWithoutMutation(t *testing.T) {
	for _, change := range []string{"owner artifact", "candidate version", "candidate checkpoint", "source ingress", "unsupported graph", "missing evidence", "noncanonical evidence", "authority drift", "old plan", "missing grant", "fresh drift", "previous asset drift"} {
		t.Run(change, func(t *testing.T) {
			candidate, request, _ := migrationOwnerFixtureWithPrevious(t, change == "previous asset drift")
			ctx := context.Background()
			goal := Goal{Target: "release-c", Direction: DirectionActivateTarget}
			inspection, err := candidate.Inspect(ctx, goal)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, _ := candidate.store.ReadCurrentJournal()
			journal, _ := ParseJournal(snapshot.Payload)
			switch change {
			case "owner artifact":
				journal.ArtifactDigest = digestA
				payload, _ := MarshalJournal(journal)
				if err := candidate.store.CompareAndSwapCurrentJournal(snapshot, payload); err != nil {
					t.Fatal(err)
				}
			case "candidate version":
				candidate.options.CandidateVersion = "0.21.0"
			case "candidate checkpoint":
				candidate.options.MigrationCheckpoint = false
			case "source ingress":
				candidate.options.SourceIngress = &SourceIngressRequest{}
			case "unsupported graph":
				root := filepath.Join(candidate.options.ConfigHome, "release-transition", "v2", "transactions", "foreign")
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
			case "missing evidence":
				name, _ := checkpointEvidenceName(request.Transaction, journal.Steps[0].ID, EvidenceCaptured)
				if err := os.Remove(filepath.Join(candidate.options.ConfigHome, "release-transition", "v2", "transactions", string(request.Transaction), "evidence", name)); err != nil {
					t.Fatal(err)
				}
			case "noncanonical evidence":
				name, _ := checkpointEvidenceName(request.Transaction, journal.Steps[0].ID, EvidenceCaptured)
				path := filepath.Join(candidate.options.ConfigHome, "release-transition", "v2", "transactions", string(request.Transaction), "evidence", name)
				payload, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append([]byte(" "), payload...), 0o600); err != nil {
					t.Fatal(err)
				}
			case "authority drift":
				snapshot, err := candidate.store.ReadMigrationCheckpoint()
				if err != nil {
					t.Fatal(err)
				}
				if err := candidate.store.compareAndSwap([]string{"release-transition", "v2"}, migrationLedgerCheckpointName, snapshot, append([]byte(" "), snapshot.Payload...)); err != nil {
					t.Fatal(err)
				}
			case "fresh drift":
				candidate.options.MigrationOwnerAssets = func(context.Context) (MigrationOwnerAssetsProof, error) {
					return MigrationOwnerAssetsProof{}, errors.New("assets changed")
				}
			case "previous asset drift":
				original := candidate.options.MigrationOwnerAssets
				candidate.options.MigrationOwnerAssets = func(ctx context.Context) (MigrationOwnerAssetsProof, error) {
					proof, err := original(ctx)
					if proof.PreviousSource != nil {
						proof.PreviousSource.Registry = digestB
					}
					return proof, err
				}
			}
			journalBefore, _ := candidate.store.ReadCurrentJournal()
			ledgerBefore, _ := candidate.store.ReadLedger()
			execution := Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)}
			if change == "missing grant" {
				execution.Authorization = ""
			}
			if change == "old plan" {
				execution.Plan = journal.AuthorizationPlan
				execution.Authorization = v2TestAuthorization(execution.Plan)
			}
			outcome, err := candidate.Converge(ctx, execution)
			if err == nil && outcome.Status == StatusReady {
				t.Fatalf("unproved replacement reached ready: %+v", outcome)
			}
			journalAfter, _ := candidate.store.ReadCurrentJournal()
			ledgerAfter, _ := candidate.store.ReadLedger()
			if !sameProtectedSnapshot(journalBefore, journalAfter) || !sameProtectedSnapshot(ledgerBefore, ledgerAfter) {
				t.Fatal("rejected replacement changed protected authority")
			}
			receipts, err := candidate.store.migrationOwnerReceipts()
			if err != nil || len(receipts) != 0 {
				t.Fatalf("rejected replacement published receipt: %+v %v", receipts, err)
			}
		})
	}
}

func TestMigrationOwnerReplacementRejectsUnknownNamespaceWithoutMutation(t *testing.T) {
	for _, name := range []string{"unknown.json", "unknown/record.json", "completed/orphan.json"} {
		for _, afterPlan := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/after-plan=%t", name, afterPlan), func(t *testing.T) {
				candidate, _, links := migrationOwnerFixture(t)
				ctx := context.Background()
				goal := Goal{Target: "release-c", Direction: DirectionActivateTarget}
				inspection, err := candidate.Inspect(ctx, goal)
				if err != nil || len(inspection.Blockers) != 0 {
					t.Fatalf("valid predecessor: %+v %v", inspection, err)
				}
				parts := append([]string{"release-transition", "migration-owner", "v1"}, strings.Split(filepath.Dir(name), "/")...)
				if filepath.Dir(name) == "." {
					parts = parts[:3]
				}
				fd, _, err := candidate.store.openParent(parts, true)
				if err != nil {
					t.Fatal(err)
				}
				unix.Close(fd)
				testkit.WriteFile(t, filepath.Join(candidate.options.ConfigHome, "release-transition", "migration-owner", "v1", name), []byte("{}\n"), 0o600)
				journalBefore, _ := candidate.store.ReadCurrentJournal()
				ledgerBefore, _ := candidate.store.ReadLedger()
				checkpointBefore, _ := candidate.store.ReadMigrationCheckpoint()
				linksBefore := *links
				if !afterPlan {
					inspection, err = candidate.Inspect(ctx, goal)
					if err == nil && len(inspection.Blockers) == 0 {
						t.Fatal("unknown protected namespace admitted a fresh replacement plan")
					}
				} else {
					outcome, convergeErr := candidate.Converge(ctx, Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
					if convergeErr == nil && outcome.Status == StatusReady {
						t.Fatal("unknown protected namespace reached Ready")
					}
				}
				journalAfter, _ := candidate.store.ReadCurrentJournal()
				ledgerAfter, _ := candidate.store.ReadLedger()
				checkpointAfter, _ := candidate.store.ReadMigrationCheckpoint()
				if !sameProtectedSnapshot(journalBefore, journalAfter) || !sameProtectedSnapshot(ledgerBefore, ledgerAfter) || !sameProtectedSnapshot(checkpointBefore, checkpointAfter) || !linksEqual(linksBefore, *links) {
					t.Fatal("unknown protected namespace changed journal, authority or links")
				}
				receipts, _, err := candidate.store.readDirectoryEntries(migrationOwnerReceiptParts, 1)
				if err != nil || len(receipts) != 0 {
					t.Fatalf("unknown protected namespace published a receipt: %v %v", receipts, err)
				}
			})
		}
	}
}

func TestMigrationOwnerReplacementTerminalMetadataRetainsTypedClosure(t *testing.T) {
	candidate, _, _ := migrationOwnerFixture(t)
	ctx := context.Background()
	inspection, err := candidate.Inspect(ctx, Goal{Target: "release-c", Direction: DirectionActivateTarget})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := candidate.Converge(ctx, Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
	if err != nil || outcome.Status != StatusReady {
		t.Fatalf("candidate: %+v %v", outcome, err)
	}
	proof, err := candidate.store.InspectSourceMetadata(candidate.registry, candidate.registryDigest)
	if err != nil || proof.Journal == nil || proof.Journal.Checkpoint != JournalComplete {
		t.Fatalf("terminal metadata: %+v %v", proof, err)
	}
	snapshot, _ := candidate.store.ReadCurrentJournal()
	journal, _ := ParseJournal(snapshot.Payload)
	if err := candidate.store.recordCompletedMigrationOwner(journal); err != nil {
		t.Fatal(err)
	}
	receipts, err := candidate.store.migrationOwnerReceipts()
	if err != nil {
		t.Fatal(err)
	}
	for _, receipt := range receipts {
		terminal, err := candidate.store.migrationOwnerCompletion(receipt, nil)
		if err != nil || terminal.Checkpoint != JournalComplete {
			t.Fatalf("durable terminal provenance: %+v %v", terminal, err)
		}
	}
}

func TestMigrationOwnerReplacementBindsRetainedTerminalHistory(t *testing.T) {
	for _, target := range []ReleaseID{"foreign-release", "release-a", "release-previous"} {
		t.Run(string(target), func(t *testing.T) {
			candidate, request, _ := migrationOwnerFixtureWithPrevious(t, true)
			ctx := context.Background()
			assets, err := candidate.options.MigrationOwnerAssets(ctx)
			if err != nil {
				t.Fatal(err)
			}
			original, _, err := ParseRegistryV2(assets.SourceRegistry, BuiltinCapabilityCatalog())
			if err != nil {
				t.Fatal(err)
			}
			ledger := BaselineLedgerV2(original)
			path, err := original.PendingPath(ledger)
			if err != nil {
				t.Fatal(err)
			}
			_, expected, err := MarshalLedgerV2(ledger, original)
			if err != nil {
				t.Fatal(err)
			}
			journal := JournalRecord{Transaction: "retained-terminal", Releases: ReleasePair{From: target, Target: target}}
			for _, migration := range path {
				ledger, err = ledger.Advance(original, migration)
				if err != nil {
					t.Fatal(err)
				}
				_, desired, err := MarshalLedgerV2(ledger, original)
				if err != nil {
					t.Fatal(err)
				}
				step := JournalStep{ID: migration.ID + ".ledger", Migration: migration.ID, Resource: "ledger." + migration.Domain, Decision: DecisionTransform, Expected: expected, Desired: desired, Checkpoint: StepVerified}
				for _, checkpoint := range []EvidenceCheckpoint{EvidenceCaptured, EvidenceApplied, EvidenceVerified} {
					observed := desired
					if checkpoint == EvidenceCaptured {
						observed = expected
					}
					evidence := evidenceFor(journal, step, checkpoint, observed, "")
					payload, err := MarshalEvidence(evidence)
					if err != nil {
						t.Fatal(err)
					}
					if err := candidate.store.CreateCheckpointEvidence(journal.Transaction, step.ID, checkpoint, payload); err != nil {
						t.Fatal(err)
					}
				}
				expected = desired
			}
			state, err := candidate.store.ReadMigrationLedger(candidate.registry, candidate.registryDigest)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := candidate.store.ReadCurrentJournal()
			if err != nil {
				t.Fatal(err)
			}
			predecessor, err := ParseJournal(snapshot.Payload)
			if err != nil {
				t.Fatal(err)
			}
			_, retained, err := candidate.store.migrationOwnerInputs(predecessor, assets.SourceRegistry, assets.PreviousSource, state.ProjectionSnapshot)
			if target == "foreign-release" {
				if err == nil || !strings.Contains(err.Error(), "exact source release") {
					t.Fatalf("foreign retained history admitted: %v", err)
				}
				return
			}
			if err != nil || len(retained) != 1 || retained[0] != journal.Transaction {
				t.Fatalf("journal-bound history not admitted: %v %v", retained, err)
			}
			if request.Transaction != predecessor.Transaction {
				t.Fatal("predecessor changed")
			}
			inspection, err := candidate.Inspect(ctx, Goal{Target: "release-c", Direction: DirectionActivateTarget})
			if err != nil || len(inspection.Blockers) != 0 {
				t.Fatalf("inspect retained history: %+v %v", inspection, err)
			}
			outcome, err := candidate.Converge(ctx, Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
			if err != nil || outcome.Status != StatusReady {
				t.Fatalf("retained history convergence: %+v %v", outcome, err)
			}
			if _, err := os.Stat(filepath.Join(candidate.options.ConfigHome, "release-transition", "v2", "transactions", string(journal.Transaction))); err != nil {
				t.Fatalf("cleanup orphaned retained history proof: %v", err)
			}
			receipts, err := candidate.store.migrationOwnerReceipts()
			if err != nil {
				t.Fatal(err)
			}
			receipt := receipts[*outcome.Transaction]
			if len(receipt.RetainedTransactions) != 1 || receipt.PreviousSource == nil {
				t.Fatal("receipt omitted retained source bindings")
			}
			if err := candidate.store.migrationOwnerRetainedHistory(receipt); err != nil {
				t.Fatal(err)
			}
			receipt.PreviousSource.Registry = digestB
			if err := receipt.Validate(); err == nil {
				t.Fatal("receipt accepted previous registry drift")
			}
		})
	}
}

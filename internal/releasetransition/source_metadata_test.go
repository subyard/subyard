package releasetransition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestSourceMetadataAcceptsAuthenticatedHistoricalDomainPrefix(t *testing.T) {
	for _, retained := range []bool{false, true} {
		name := "current"
		if retained {
			name = "retained"
		}
		t.Run(name, func(t *testing.T) {
			transition, home, _ := v2TransitionFixture(t, nil)
			store, err := NewPOSIXV2Store(home)
			if err != nil {
				t.Fatal(err)
			}
			// The published predecessor completed settings/owner epoch 2 while
			// power remained at epoch 1, before the new cross-domain ordering.
			oldRegistry, oldDigest := transition.registry, transition.registryDigest
			ledger := BaselineLedgerV2(oldRegistry)
			path, err := oldRegistry.PendingPath(ledger)
			if err != nil {
				t.Fatal(err)
			}
			for _, migration := range path {
				ledger, err = ledger.Advance(oldRegistry, migration)
				if err != nil {
					t.Fatal(err)
				}
			}
			if ledger.Domains["owner-registration"].Epoch != 2 || ledger.Domains["power-metadata"].Epoch != 1 {
				t.Fatal("fixture lost the historical cross-domain prefix")
			}
			payload, _, err := MarshalLedgerV2(ledger, oldRegistry)
			if err != nil {
				t.Fatal(err)
			}
			missing, err := store.ReadLedger()
			if err != nil {
				t.Fatal(err)
			}
			if err := store.CompareAndSwapLedger(missing, payload); err != nil {
				t.Fatal(err)
			}
			prior, err := store.ReadMigrationLedger(oldRegistry, oldDigest)
			if err != nil {
				t.Fatal(err)
			}
			payload, err = os.ReadFile(filepath.Join("..", "..", "config", "release-transition.json"))
			if err != nil {
				t.Fatal(err)
			}
			registry, digest, err := ParseRegistryV2(payload, BuiltinCapabilityCatalog())
			if err != nil {
				t.Fatal(err)
			}
			if err := store.ConvertMigrationLedger(prior, oldRegistry, oldDigest, registry, digest); err != nil {
				t.Fatal(err)
			}
			options := transition.options
			options.RegistryPayload = payload
			options.MigrationCheckpoint, options.SourceMigrationCheckpoint = true, true
			transition, err = NewV2Transition(options)
			if err != nil {
				t.Fatal(err)
			}
			goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
			inspection, err := transition.Inspect(context.Background(), goal)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := transition.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
			if err != nil || outcome.Status != StatusReady {
				t.Fatalf("historical prefix upgrade: %+v %v", outcome, err)
			}
			snapshot, err := store.ReadCurrentJournal()
			if err != nil {
				t.Fatal(err)
			}
			journal, err := ParseJournal(snapshot.Payload)
			if err != nil {
				t.Fatal(err)
			}
			if retained {
				transition, store = interruptSourceMetadataActivation(t, transition, store)
			}
			before := snapshotStoreV2Tree(t, home)
			proof, err := store.InspectSourceMetadata(registry, digest)
			if err != nil {
				t.Fatalf("authenticated historical prefix refused: %v", err)
			}
			for index := range journal.Steps {
				if journal.Steps[index].Resource == "ledger.power-metadata" {
					journal.Steps[index].Expected = digestB
				}
			}
			if err := store.sourceTerminalEvidence(journal, registry, proof.Ledger); err == nil {
				t.Fatal("authenticated terminal admitted an altered historical before state")
			}
			if string(snapshotStoreV2Tree(t, home)) != string(before) {
				t.Fatal("source admission changed protected history")
			}
		})
	}
}

func TestSourceMetadataRejectsLedgerRecoveryInCompleteCheckpointTriples(t *testing.T) {
	for _, retained := range []bool{false, true} {
		name := "current"
		if retained {
			name = "retained"
		}
		t.Run(name, func(t *testing.T) {
			transition, store := completedSourceMetadataFixture(t)
			snapshot, err := store.ReadCurrentJournal()
			if err != nil {
				t.Fatal(err)
			}
			journal, err := ParseJournal(snapshot.Payload)
			if err != nil {
				t.Fatal(err)
			}
			if retained {
				transition, store = interruptSourceMetadataActivation(t, transition, store)
			}
			if _, err := store.InspectSourceMetadata(transition.registry, transition.registryDigest); err != nil {
				t.Fatalf("valid baseline refused: %v", err)
			}
			index := -1
			for candidate, step := range journal.Steps {
				if step.ID == step.Migration+".ledger" {
					index = candidate
					break
				}
			}
			if index < 0 {
				t.Fatal("fixture has no ledger step")
			}
			step := &journal.Steps[index]
			if err := store.CreateRecovery(journal.Transaction, step.ID, []byte("{}\n")); err != nil {
				t.Fatal(err)
			}
			recovery, err := store.ReadRecovery(journal.Transaction, step.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, checkpoint := range []EvidenceCheckpoint{EvidenceCaptured, EvidenceApplied, EvidenceVerified} {
				snapshot, err := store.ReadCheckpointEvidence(journal.Transaction, step.ID, checkpoint)
				if err != nil {
					t.Fatal(err)
				}
				evidence, err := ParseEvidence(snapshot.Payload)
				if err != nil {
					t.Fatal(err)
				}
				evidence.Recovery = recovery.Fingerprint
				payload, err := MarshalEvidence(evidence)
				if err != nil {
					t.Fatal(err)
				}
				filename, err := checkpointEvidenceName(journal.Transaction, step.ID, checkpoint)
				if err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(store.configHome, "release-transition", "v2", "transactions", string(journal.Transaction), "evidence", filename), payload, 0o600)
			}
			if !retained {
				step.Evidence.Recovery = recovery.Fingerprint
				journal.IntentDigest = bindJournalIntent(journal.AuthorizationPlan, journal.ResumePlan, journal.ObservationScope, journal.Steps)
				payload, err := MarshalJournal(journal)
				if err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(store.configHome, "release-transition", "v2", "journal.json"), payload, 0o600)
			}
			before := snapshotStoreV2Tree(t, store.configHome)
			if _, err := store.InspectSourceMetadata(transition.registry, transition.registryDigest); err == nil || !strings.Contains(err.Error(), "ledger evidence cannot carry a recovery payload") {
				t.Fatalf("untyped ledger recovery was not rejected by its grammar: %v", err)
			}
			if string(snapshotStoreV2Tree(t, store.configHome)) != string(before) {
				t.Fatal("inspection changed crafted protected history")
			}
		})
	}
}

func reconcilingSourceMetadataFixture(t *testing.T) (*V2Transition, *POSIXV2Store) {
	t.Helper()
	transition, store := completedSourceMetadataFixture(t)
	return interruptSourceMetadataActivation(t, transition, store)
}

func interruptSourceMetadataActivation(t *testing.T, transition *V2Transition, store *POSIXV2Store) (*V2Transition, *POSIXV2Store) {
	t.Helper()
	transition.options.NewTransactionID = func() TransactionID { return "tx-source-reconciling" }
	transition.options.Reconcilers = []V2ActivationReconciler{&v2TestReconciler{}}
	transition.options.fault = func(point string) error {
		if point == "before-reconciler-0" {
			return errors.New("fixture interrupted after reconciling publication")
		}
		return nil
	}
	inspection, err := transition.Inspect(context.Background(), Goal{Target: "release-a", Direction: DirectionActivateTarget})
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := transition.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)}); err == nil && outcome.Status == StatusReady {
		t.Fatal("fixture did not interrupt publication")
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	journal, err := ParseJournal(snapshot.Payload)
	if err != nil || journal.Checkpoint != JournalReconciling || len(journal.Steps) != 0 {
		t.Fatalf("unexpected reconciling producer state: %v", err)
	}
	return transition, store
}

func TestSourceMetadataAcceptsRetainedCompletedResourceCheckpoints(t *testing.T) {
	for _, resource := range []string{"hermes", ownerRegistrationResource} {
		t.Run(resource, func(t *testing.T) {
			var owner V2OwnerRegistration
			if resource == ownerRegistrationResource {
				owner = &v2TestOwnerRegistration{state: OwnerRegistrationLegacyDirectory}
			}
			transition, store := completedSourceMetadataFixtureWithOwner(t, owner)
			transition, store = interruptSourceMetadataActivation(t, transition, store)
			prior, err := store.sourceRetainedTerminalEvidence("tx-test-001", transition.registry)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, step := range prior.Steps {
				found = found || step.Resource == resource && step.Evidence.Recovery != ""
			}
			if !found {
				t.Fatal("fixture did not retain the completed resource before-image")
			}
			before := snapshotStoreV2Tree(t, store.configHome)
			if _, err := store.InspectSourceMetadata(transition.registry, transition.registryDigest); err != nil {
				t.Fatalf("authentic retained completed resource evidence refused: %v", err)
			}
			if string(snapshotStoreV2Tree(t, store.configHome)) != string(before) {
				t.Fatal("inspection changed retained history")
			}
		})
	}
}

func TestSourceMetadataRejectsUnclassifiedArtifactsWithCurrentJournal(t *testing.T) {
	for _, current := range []string{"complete", "reconciling"} {
		for _, artifact := range []string{"orphan captured", "orphan recovery", "selected zero-step captured", "selected zero-step recovery"} {
			if current == "complete" && artifact[:8] == "selected" {
				continue
			}
			t.Run(current+"/"+artifact, func(t *testing.T) {
				fixture := completedSourceMetadataFixture
				if current == "reconciling" {
					fixture = reconcilingSourceMetadataFixture
				}
				transition, store := fixture(t)
				if _, err := store.InspectSourceMetadata(transition.registry, transition.registryDigest); err != nil {
					t.Fatalf("valid baseline refused: %v", err)
				}
				transaction := TransactionID("tx-orphan-source")
				if artifact[:8] == "selected" {
					transaction = "tx-source-reconciling"
				}
				if artifact == "orphan recovery" || artifact == "selected zero-step recovery" {
					if err := store.CreateRecovery(transaction, "unknown-step", []byte("{}\n")); err != nil {
						t.Fatal(err)
					}
				} else {
					snapshot, err := store.ReadCheckpointEvidence("tx-test-001", "canonicalize-test-vms-settings-v2.ledger", EvidenceCaptured)
					if err != nil {
						t.Fatal(err)
					}
					evidence, err := ParseEvidence(snapshot.Payload)
					if err != nil {
						t.Fatal(err)
					}
					evidence.Transaction = transaction
					payload, err := MarshalEvidence(evidence)
					if err != nil {
						t.Fatal(err)
					}
					if err := store.CreateCheckpointEvidence(transaction, evidence.Step, EvidenceCaptured, payload); err != nil {
						t.Fatal(err)
					}
				}
				before := snapshotStoreV2Tree(t, store.configHome)
				if _, err := store.InspectSourceMetadata(transition.registry, transition.registryDigest); err == nil {
					t.Fatal("unclassified protected transaction intent admitted")
				}
				if string(snapshotStoreV2Tree(t, store.configHome)) != string(before) {
					t.Fatal("refusal changed retained history")
				}
			})
		}
	}
}

func completedSourceMetadataFixture(t *testing.T) (*V2Transition, *POSIXV2Store) {
	t.Helper()
	return completedSourceMetadataFixtureWithOwner(t, nil)
}

func completedSourceMetadataFixtureWithOwner(t *testing.T, owner V2OwnerRegistration) (*V2Transition, *POSIXV2Store) {
	t.Helper()
	transition, home, _ := v2TransitionFixture(t, nil)
	if owner != nil {
		transition.options.OwnerRegistration = owner
	}
	inspection, err := transition.Inspect(context.Background(), Goal{Target: "release-a", Direction: DirectionActivateTarget})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
	if err != nil || outcome.Status != StatusReady {
		t.Fatalf("complete fixture: %v %v", outcome, err)
	}
	store, err := NewPOSIXV2Store(home)
	if err != nil {
		t.Fatal(err)
	}
	return transition, store
}

func TestSourceMetadataCompletedProofDoesNotObserveHistoricalResources(t *testing.T) {
	transition, store := completedSourceMetadataFixture(t)
	proof, err := store.InspectSourceMetadata(transition.registry, transition.registryDigest)
	if err != nil {
		t.Fatal(err)
	}
	if proof.Journal == nil || proof.Journal.Checkpoint != JournalComplete || proof.Binding == "" {
		t.Fatal("terminal proof missing")
	}
	// Operational source drift is outside this metadata-only proof.
	testkit.WriteFile(t, filepath.Join(store.configHome, "yards", "hermes", "config.env"), []byte("YARD_PROFILES=a\nENVIRONMENT_PROFILES=b\n"), 0o600)
	after, err := store.InspectSourceMetadata(transition.registry, transition.registryDigest)
	if err != nil || after.Binding != proof.Binding {
		t.Fatalf("source drift changed metadata proof: %v", err)
	}
}

func TestSourceMetadataRejectsPendingAndUnboundEvidence(t *testing.T) {
	for _, name := range []string{"pending journal", "extra evidence", "missing evidence"} {
		t.Run(name, func(t *testing.T) {
			transition, store := completedSourceMetadataFixture(t)
			snapshot, err := store.ReadCurrentJournal()
			if err != nil {
				t.Fatal(err)
			}
			journal, err := ParseJournal(snapshot.Payload)
			if err != nil {
				t.Fatal(err)
			}
			evidenceRoot := filepath.Join(store.configHome, "release-transition", "v2", "transactions", string(journal.Transaction), "evidence")
			switch name {
			case "pending journal":
				testkit.WriteFile(t, filepath.Join(store.configHome, "release-transition", "v2", ".journal.json.pending"), snapshot.Payload, 0o600)
			case "extra evidence":
				testkit.WriteFile(t, filepath.Join(evidenceRoot, "extra.json"), []byte("{}\n"), 0o600)
			case "missing evidence":
				name, err := checkpointEvidenceName(journal.Transaction, journal.Steps[0].ID, EvidenceCaptured)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(evidenceRoot, name)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.InspectSourceMetadata(transition.registry, transition.registryDigest); err == nil {
				t.Fatal("invalid protected intent admitted")
			}
		})
	}
}

func TestCompletedMetadataReplacementStalesNewOrdinaryPlan(t *testing.T) {
	transition, store := completedSourceMetadataFixture(t)
	transition.options.Releases.Target = "release-b"
	transition.options.CompletedMetadata = func(context.Context) (Fingerprint, error) { return store.sourceMetadataInventory() }
	inspection, err := transition.Inspect(context.Background(), Goal{Target: "release-b", Direction: DirectionActivateTarget})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.configHome, "release-transition", "v2", "journal.json")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	temporary := path + ".replacement"
	testkit.WriteFile(t, temporary, content, 0o600)
	if err := os.Rename(temporary, path); err != nil {
		t.Fatal(err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{Plan: inspection.Plan})
	if err != nil || outcome.Code != CodePlanStale {
		t.Fatalf("replaced metadata accepted: %#v %v (expected %v)", outcome, err, domain.ErrPlanStale)
	}
}

func TestSourceMetadataRejectsOrphanEvidenceWithoutCurrentJournal(t *testing.T) {
	transition, store := completedSourceMetadataFixture(t)
	if err := os.Remove(filepath.Join(store.configHome, "release-transition", "v2", "journal.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InspectSourceMetadata(transition.registry, transition.registryDigest); err == nil {
		t.Fatal("canonical orphan evidence was treated as absence of unfinished authorization")
	}
}

func TestSourceMetadataAcceptsClosedOrdinaryArchivedPredecessor(t *testing.T) {
	transition, store := completedSourceMetadataFixture(t)
	selected, err := store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	journal, err := ParseJournal(selected.Payload)
	if err != nil {
		t.Fatal(err)
	}
	predecessor := TransactionID("tx-historical-source")
	path := filepath.Join(store.configHome, "release-transition", "v2", "transactions", string(predecessor))
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	createStoreSupersession(t, store, journal.Transaction, predecessor, journal.AuthorizationPlan)
	before := snapshotStoreV2Tree(t, store.configHome)
	if _, err := store.InspectSourceMetadata(transition.registry, transition.registryDigest); err != nil {
		t.Fatalf("known unfinished archived predecessor with terminal successor was refused: %v", err)
	}
	if string(snapshotStoreV2Tree(t, store.configHome)) != string(before) {
		t.Fatal("source inspection cleaned up historical predecessor")
	}
}

func TestSourceMetadataRecoveryClosureRequiresExactTerminalWitness(t *testing.T) {
	store, receipt := lifecycleStoreFixture(t)
	if err := store.sourceRecoveryClosed(); err == nil {
		t.Fatal("live initial reservation admitted")
	}
	completed := receipt.Successor
	completed.Checkpoint = JournalComplete
	terminal := RecoveryLifecycleRecord{SchemaVersion: RecoveryLifecycleSchemaV1, Receipt: receipt, Completed: &completed}
	if err := store.createLifecycleRecord(terminal); err != nil {
		t.Fatal(err)
	}
	if err := store.sourceRecoveryClosed(); err != nil {
		t.Fatalf("exact completed lifecycle refused: %v", err)
	}
}

func TestSourceMetadataRecoveryCancellationWithoutNewReservationIsClosed(t *testing.T) {
	store, receipt := lifecycleStoreFixture(t)
	base, plan, reservation := lifecycleNewPlan(t, store, receipt.Replacement)
	proposed := TransactionID(RecoveryTransactionPrefixV2 + "next")
	if _, err := store.ResolveLifecycleRecoveryTransaction(proposed, receipt.Replacement, base, plan, digestC); err != nil {
		t.Fatal(err)
	}
	_ = reservation
	if err := store.sourceRecoveryClosed(); err != nil {
		t.Fatalf("typed cancellation and next frontier without a live receipt was refused: %v", err)
	}
}

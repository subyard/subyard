package releasetransition

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/migrationhistory"
	"github.com/Subyard/Subyard/internal/testkit"
)

func rewindRegistries() (RegistryV2, RegistryV2) {
	target := RegistryV2{SchemaVersion: 2, MinimumEpochs: map[string]int{"settings": 1}, CurrentEpochs: map[string]int{"settings": 2}, Migrations: []MigrationDefinitionV2{{ID: "settings-v2", Domain: "settings", FromEpoch: 1, ToEpoch: 2, Kind: "test-vms-settings-v1-to-v2"}}}
	source := RegistryV2{SchemaVersion: 2, MinimumEpochs: map[string]int{"settings": 1}, CurrentEpochs: map[string]int{"settings": 3}, Migrations: append([]MigrationDefinitionV2{}, target.Migrations...)}
	source.Migrations = append(source.Migrations, MigrationDefinitionV2{ID: "settings-v3", Domain: "settings", FromEpoch: 2, ToEpoch: 3, Kind: "future-settings-v3"})
	return source, target
}

func completeRewindLedger(registry RegistryV2) LedgerV2 {
	ledger := BaselineLedgerV2(registry)
	for name, epoch := range registry.CurrentEpochs {
		ledger.Domains[name] = DomainLedgerV2{Epoch: epoch, Applied: nonNilIDs(registry.appliedPrefix(name, epoch))}
	}
	return ledger
}

func rewindSnapshot(t *testing.T, source, target RegistryV2) MigrationLedgerSnapshot {
	t.Helper()
	projectionPayload, _, err := MarshalLedgerV2(completeRewindLedger(target), target)
	if err != nil {
		t.Fatal(err)
	}
	projection := protectedSnapshotFromPayload(projectionPayload)
	checkpoint, err := newLedgerCheckpointV1(completeRewindLedger(source), source, digestA, projection, target.CurrentEpochs)
	if err != nil {
		t.Fatal(err)
	}
	payload, _, err := MarshalLedgerCheckpointV1(checkpoint, source, digestA, projection)
	if err != nil {
		t.Fatal(err)
	}
	state, err := ParseMigrationLedger(projection, protectedSnapshotFromPayload(payload), source, digestA)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestRewindCheckpointRestoresNativeHistoryAndPendingEdges(t *testing.T) {
	source, target := rewindRegistries()
	source.MinimumEpochs["new-domain"], source.CurrentEpochs["new-domain"] = 1, 2
	source.Migrations = append(source.Migrations, MigrationDefinitionV2{ID: "new-domain-v2", Domain: "new-domain", FromEpoch: 1, ToEpoch: 2, Kind: "future-domain-v2", DependsOn: []string{"settings-v3"}})
	state := rewindSnapshot(t, source, target)
	projection := append([]byte{}, state.ProjectionSnapshot.Payload...)
	owner, authority, err := source.historyAuthority(state.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	edges, err := owner.RewindPath(authority, map[string]int{"settings": 2, "new-domain": 1})
	if err != nil || len(edges) != 2 {
		t.Fatalf("path = %v, %v", edges, err)
	}
	for index, edge := range edges {
		reader, readerDigest := source, digestA
		if index == len(edges)-1 {
			reader, readerDigest = target, digestB
		}
		payload, err := rewindCheckpointPayload(state, source, digestA, reader, readerDigest, edge)
		if err != nil {
			t.Fatal(err)
		}
		state, err = ParseMigrationLedger(state.ProjectionSnapshot, protectedSnapshotFromPayload(payload), source, digestA)
		if err != nil {
			t.Fatal(err)
		}
	}
	native, err := ParseMigrationLedger(state.ProjectionSnapshot, state.AuthoritySnapshot, target, digestB)
	if err != nil || !sameLedgerV2(native.Ledger, completeRewindLedger(target)) {
		t.Fatalf("native target = %+v, %v", native, err)
	}
	if !bytes.Equal(projection, state.ProjectionSnapshot.Payload) {
		t.Fatal("rewind changed frozen projection")
	}
	pending, err := source.PendingPath(state.Ledger)
	if err != nil || len(pending) != 2 || pending[0].ID != "settings-v3" || pending[1].ID != "new-domain-v2" {
		t.Fatalf("forward pending = %+v, %v", pending, err)
	}
	if _, err := advanceLedgerCheckpointV1(rewindSnapshot(t, source, target).Checkpoint, state.Ledger, source, digestA, state.ProjectionSnapshot); err == nil {
		t.Fatal("ordinary forward writer accepted rewind behind compacted history")
	}
}

func TestRewindCheckpointRejectsProjectionFloorAndForeignPrefix(t *testing.T) {
	source, target := rewindRegistries()
	state := rewindSnapshot(t, source, target)
	owner, authority, err := source.historyAuthority(state.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	edge, err := owner.Rewind(authority, "settings-v3")
	if err != nil {
		t.Fatal(err)
	}
	foreign := target
	foreign.Migrations = append([]MigrationDefinitionV2{}, target.Migrations...)
	foreign.Migrations[0].Kind = "different-kind"
	if _, err := rewindCheckpointPayload(state, source, digestA, foreign, digestB, edge); err == nil {
		t.Fatal("foreign shared prefix accepted")
	}
	// A conversion first made at S cannot create an older projection horizon.
	high := rewindSnapshot(t, source, source)
	if _, err := rewindCheckpointPayload(high, source, digestA, target, digestB, edge); err == nil {
		t.Fatal("rewind crossed immutable projection floor")
	}
	forged := state
	forged.Ledger = completeRewindLedger(target)
	if _, err := rewindCheckpointPayload(forged, source, digestA, target, digestB, edge); err == nil {
		t.Fatal("forged semantic snapshot accepted")
	}
	if _, err := rewindCheckpointPayload(state, source, digestA, target, digestB, migrationhistory.ReverseEdge{}); err == nil {
		t.Fatal("zero reverse edge accepted")
	}
}

func TestCheckpointWriterRefusesDecreaseWithinAppliedSuffix(t *testing.T) {
	source, target := rewindRegistries()
	source.CurrentEpochs["settings"] = 4
	source.Migrations = append(source.Migrations, MigrationDefinitionV2{ID: "settings-v4", Domain: "settings", FromEpoch: 3, ToEpoch: 4, Kind: "future-settings-v4"})
	store, err := NewPOSIXV2Store(testkit.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	projection, _, err := MarshalLedgerV2(completeRewindLedger(target), target)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapLedger(absentProtectedSnapshot(), projection); err != nil {
		t.Fatal(err)
	}
	state, err := store.ReadMigrationLedger(source, digestA)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConvertMigrationLedger(state, source, digestA, source, digestA); err != nil {
		t.Fatal(err)
	}
	state, err = store.ReadMigrationLedger(source, digestA)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapMigrationLedger(state, completeRewindLedger(source), source, digestA); err != nil {
		t.Fatal(err)
	}
	state, err = store.ReadMigrationLedger(source, digestA)
	if err != nil || state.Checkpoint.Domains["settings"].CompactedThrough != 2 {
		t.Fatalf("advanced checkpoint = %+v, %v", state.Checkpoint, err)
	}
	lower := completeRewindLedger(source)
	lower.Domains["settings"] = DomainLedgerV2{Epoch: 3, Applied: source.appliedPrefix("settings", 3)}
	if err := store.CompareAndSwapMigrationLedger(state, lower, source, digestA); err == nil {
		t.Fatal("ordinary writer lowered the current epoch within the applied suffix")
	}
	current, err := store.ReadMigrationCheckpoint()
	if err != nil || !sameProtectedSnapshot(current, state.AuthoritySnapshot) {
		t.Fatalf("refused decrease changed authority: %v", err)
	}
}

type checkpointRewindFixture struct {
	store          *POSIXV2Store
	home           string
	source, target RegistryV2
	state          MigrationLedgerSnapshot
	edge           migrationhistory.ReverseEdge
	journal        ProtectedSnapshot
}

func newCheckpointRewindFixture(t *testing.T) checkpointRewindFixture {
	t.Helper()
	return checkpointRewindWithEvidence(t, false, false)
}

func checkpointRewindWithEvidence(t *testing.T, irreversible, unrelatedEvidence bool) checkpointRewindFixture {
	t.Helper()
	migration := builtinMigrationDefinitions()[0].migration
	target := RegistryV2{SchemaVersion: 2, MinimumEpochs: map[string]int{"settings": 1}, CurrentEpochs: map[string]int{"settings": 1}, Migrations: []MigrationDefinitionV2{}}
	source := target
	source.CurrentEpochs, source.Migrations = map[string]int{"settings": 2}, []MigrationDefinitionV2{migration}
	if irreversible {
		payload, err := BuiltinRegistryPayload()
		if err != nil {
			t.Fatal(err)
		}
		source, _, err = ParseRegistryV2(payload, BuiltinCapabilityCatalog())
		if err != nil {
			t.Fatal(err)
		}
		migration = source.Migrations[len(source.Migrations)-1]
		target = source
		target.Migrations = source.Migrations[:len(source.Migrations)-1]
		target.CurrentEpochs = map[string]int{"settings": 3, "project-state": 1, "owner-registration": 3, "power-metadata": 3}
	}
	state := rewindSnapshot(t, source, target)
	store, err := NewPOSIXV2Store(testkit.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapLedger(absentProtectedSnapshot(), state.ProjectionSnapshot.Payload); err != nil {
		t.Fatal(err)
	}
	if err := store.createImmutable([]string{"release-transition", "v2"}, migrationLedgerCheckpointName, state.AuthoritySnapshot.Payload); err != nil {
		t.Fatal(err)
	}
	owner, authority, err := source.historyAuthority(state.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	edge, err := owner.Rewind(authority, migration.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, beforePin, err := MarshalLedgerV2(ledgerFromAuthority(edge.Before()), source)
	if err != nil {
		t.Fatal(err)
	}
	_, afterPin, err := MarshalLedgerV2(ledgerFromAuthority(edge.After()), source)
	if err != nil {
		t.Fatal(err)
	}
	journal := validJournal(ReleasePair{From: "release-s", Target: "release-r"})
	journal.Goal.Direction = DirectionActivatePrevious
	journal.RegistryDigest = digestA
	journal.Checkpoint = JournalMigrating
	verifier := rewindOnlyVerifierIntent(migration.ID)
	if unrelatedEvidence {
		verifier.Resource = "state.settings"
	}
	journal.Steps = []JournalStep{
		{ID: verifier.ID, Migration: verifier.Migration, Resource: verifier.Resource, Decision: verifier.Decision, Expected: verifier.Expected, Desired: verifier.Desired, Checkpoint: StepVerified},
		{ID: migration.ID + ".rewind-ledger", Migration: migration.ID, Resource: "ledger." + migration.Domain, Decision: DecisionTransform, Expected: beforePin, Desired: afterPin, Checkpoint: StepEvidence},
	}
	for index := range journal.Steps {
		step := &journal.Steps[index]
		phases := []EvidenceCheckpoint{EvidenceCaptured}
		if step.Checkpoint == StepVerified {
			phases = append(phases, EvidenceApplied, EvidenceVerified)
		}
		for _, phase := range phases {
			observed := step.Desired
			if phase == EvidenceCaptured {
				observed = step.Expected
			}
			evidence := evidenceFor(journal, *step, phase, observed, "")
			payload, err := MarshalEvidence(evidence)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.CreateCheckpointEvidence(journal.Transaction, step.ID, phase, payload); err != nil {
				t.Fatal(err)
			}
			step.Evidence = &evidence
		}
	}
	journal.IntentDigest = bindJournalIntent(journal.AuthorizationPlan, journal.ResumePlan, journal.ObservationScope, journal.Steps)
	payload, err := MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapCurrentJournal(absentProtectedSnapshot(), payload); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	return checkpointRewindFixture{store: store, home: store.configHome, source: source, target: target, state: state, edge: edge, journal: snapshot}
}
func (f checkpointRewindFixture) proof() (checkpointRewindProof, error) {
	return f.store.prepareCheckpointRewind(f.state, f.source, digestA, f.target, digestB, f.edge, f.journal, f.edge.Migration().ID+".rewind-ledger")
}

func TestCheckpointRewindRefusesIrreversibleAndUnrelatedEvidence(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		irreversible, unrelated bool
		want                    string
	}{
		{"irreversible migration with verifier-shaped evidence", true, false, "no supported checkpoint rewind recipe"},
		{"unrelated verified preserve row", false, true, "does not bind verified reverse work"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := checkpointRewindWithEvidence(t, tc.irreversible, tc.unrelated)
			if _, err := f.proof(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("rewind proof error = %v, want %q", err, tc.want)
			}
			current, err := f.store.ReadMigrationCheckpoint()
			if err != nil || !sameProtectedSnapshot(current, f.state.AuthoritySnapshot) {
				t.Fatalf("refused rewind changed authority: %v", err)
			}
		})
	}
}

func TestCheckpointRewindPublicationRetriesAndRejectsStaleJournal(t *testing.T) {
	f := newCheckpointRewindFixture(t)
	proof, err := f.proof()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.rewindMigrationCheckpoint(checkpointRewindProof{}); err == nil {
		t.Fatal("zero proof accepted")
	}
	other, err := NewPOSIXV2Store(f.home)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.rewindMigrationCheckpoint(proof); err == nil {
		t.Fatal("foreign store proof accepted")
	}
	unlock, err := f.store.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := f.store.rewindMigrationCheckpoint(proof); err != nil {
		t.Fatal(err)
	}
	if err := f.store.rewindMigrationCheckpoint(proof); err != nil {
		t.Fatalf("publication retry: %v", err)
	}
	result, err := f.store.ReadMigrationLedger(f.target, digestB)
	if err != nil || !sameLedgerV2(result.Ledger, completeRewindLedger(f.target)) {
		t.Fatalf("R native result = %+v, %v", result, err)
	}
	// A fresh process reconstructs the same recorded edge from the observed cut.
	f.state, err = f.store.ReadMigrationLedger(f.source, digestA)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := f.proof()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.rewindMigrationCheckpoint(resumed); err != nil {
		t.Fatal(err)
	}
	journal, err := ParseJournal(f.journal.Payload)
	if err != nil {
		t.Fatal(err)
	}
	journal.Steps[1].Checkpoint = StepApplied
	evidence := evidenceFor(journal, journal.Steps[1], EvidenceApplied, journal.Steps[1].Desired, "")
	journal.Steps[1].Evidence = &evidence
	payload, err := MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.CompareAndSwapCurrentJournal(f.journal, payload); err != nil {
		t.Fatal(err)
	}
	if err := f.store.rewindMigrationCheckpoint(proof); !errors.Is(err, ErrProtectedStoreStale) {
		t.Fatalf("stale journal = %v", err)
	}
}

func TestCheckpointRewindRequiresProtectedVerifiedEvidence(t *testing.T) {
	for _, change := range []string{"missing", "changed", "unsafe mode"} {
		t.Run(change, func(t *testing.T) {
			f := newCheckpointRewindFixture(t)
			path := filepath.Join(f.home, "release-transition", "v2", "transactions", "tx-001", "evidence", f.edge.Migration().ID+".verify-rollback.verified.json")
			switch change {
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "changed":
				testkit.WriteFile(t, path, []byte("{}\n"), 0o600)
			case "unsafe mode":
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.proof(); err == nil {
				t.Fatal("unproven reverse allowed checkpoint publication")
			}
			current, err := f.store.ReadMigrationCheckpoint()
			if err != nil || !bytes.Equal(current.Payload, f.state.AuthoritySnapshot.Payload) {
				t.Fatal("failed verification changed history")
			}
		})
	}
}

func TestCheckpointRewindResumesAcrossPublicationCuts(t *testing.T) {
	for _, point := range []string{"after-pending-fsync", "after-publish-before-dir-fsync"} {
		t.Run(point, func(t *testing.T) {
			f := newCheckpointRewindFixture(t)
			proof, err := f.proof()
			if err != nil {
				t.Fatal(err)
			}
			interrupted := errors.New("interrupted rewind publication")
			f.store.fault = func(at string) error {
				if at == point {
					return interrupted
				}
				return nil
			}
			if err := f.store.rewindMigrationCheckpoint(proof); !errors.Is(err, interrupted) {
				t.Fatalf("publication cut %s = %v", point, err)
			}
			f.store, err = NewPOSIXV2Store(f.home)
			if err != nil {
				t.Fatal(err)
			}
			f.state, err = f.store.ReadMigrationLedger(f.source, digestA)
			if err != nil {
				t.Fatal(err)
			}
			resumed, err := f.proof()
			if err != nil {
				t.Fatal(err)
			}
			if err := f.store.rewindMigrationCheckpoint(resumed); err != nil {
				t.Fatal(err)
			}
			result, err := f.store.ReadMigrationLedger(f.target, digestB)
			if err != nil || !sameLedgerV2(result.Ledger, completeRewindLedger(f.target)) {
				t.Fatalf("resumed native history = %+v, %v", result, err)
			}
		})
	}
}

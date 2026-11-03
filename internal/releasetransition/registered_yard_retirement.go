package releasetransition

import (
	"bytes"
	"context"
	"reflect"
	"strings"
)

// RegisteredYardJournalBinding is read-only predecessor evidence. It grants no
// authority to change a release journal, advance a ledger or bless another yard.
type RegisteredYardJournalBinding struct {
	Resource string
	Snapshot ProtectedSnapshot
}

// CaptureRegisteredYardBinding requires the caller to hold Lock through its
// final comparison and destructive effect. It deliberately does not observe the
// old guest: a failed guest migration is one reason to recreate that guest.
func (store *POSIXV2Store) CaptureRegisteredYardBinding(resource string) (RegisteredYardJournalBinding, error) {
	result := RegisteredYardJournalBinding{Resource: resource}
	if !strings.HasPrefix(resource, "yard.") || len(resource) <= len("yard.") {
		return result, invalid("registered yard retirement resource is invalid")
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil || !snapshot.Exists {
		return result, err
	}
	journal, err := ParseJournal(snapshot.Payload)
	canonical, marshalErr := MarshalJournal(journal)
	if err != nil || marshalErr != nil || !bytes.Equal(canonical, snapshot.Payload) {
		return result, invalid("registered yard predecessor journal is invalid")
	}
	if err := store.validateCurrentSupersession(journal); err != nil {
		return result, err
	}
	if err := store.ValidateCurrentRecovery(journal); err != nil {
		return result, err
	}
	if journal.Checkpoint != JournalAuthorized && journal.Checkpoint != JournalMigrating && journal.Checkpoint != JournalComplete {
		for _, step := range journal.Steps {
			if step.Resource != resource {
				continue
			}
			if step.Checkpoint != StepVerified || step.Evidence == nil {
				return result, invalid("retired native migration is not verified")
			}
			for _, checkpoint := range []EvidenceCheckpoint{EvidenceCaptured, EvidenceApplied, EvidenceVerified} {
				saved, err := store.ReadCheckpointEvidence(journal.Transaction, step.ID, checkpoint)
				if err != nil || !saved.Exists {
					return result, invalid("retired native migration evidence is missing")
				}
				evidence, err := ParseEvidence(saved.Payload)
				canonical, marshalErr := MarshalEvidence(evidence)
				if err != nil || marshalErr != nil || !bytes.Equal(canonical, saved.Payload) || !postActivationEvidenceMatches(journal, step, checkpoint, evidence) || checkpoint == EvidenceVerified && !evidenceRecordsEqual(evidence, *step.Evidence) {
					return result, invalid("retired native migration evidence differs from its journal")
				}
			}
		}
	}
	result.Snapshot = snapshot
	return result, nil
}

func (binding RegisteredYardJournalBinding) Matches(journal JournalRecord, resource string) bool {
	if binding.Resource != resource || !binding.Snapshot.Exists || !validProtectedSnapshot(binding.Snapshot) {
		return false
	}
	old, err := ParseJournal(binding.Snapshot.Payload)
	if err != nil || old.Transaction != journal.Transaction || old.AuthorizationPlan != journal.AuthorizationPlan || old.IntentDigest != journal.IntentDigest || old.ArtifactDigest != journal.ArtifactDigest || old.RegistryDigest != journal.RegistryDigest || old.CatalogDigest != journal.CatalogDigest || !releasePairsEqual(old.Releases, journal.Releases) {
		return false
	}
	frozen := func(record JournalRecord) JournalRecord {
		record.ResumePlan, record.Checkpoint = "", ""
		record.Steps = append([]JournalStep(nil), record.Steps...)
		for index := range record.Steps {
			record.Steps[index].Checkpoint, record.Steps[index].Evidence = "", nil
		}
		return record
	}
	return reflect.DeepEqual(frozen(old), frozen(journal))
}

// A registered owner may recognize an exact environment retirement receipt.
// The transition still owns reauthorization, journal progress and the ledger.
type RegisteredYardJournalRetirement interface {
	RetiredJournalBinding(context.Context, JournalRecord) (bool, error)
}

func (t *V2Transition) retiredNativeBinding(ctx context.Context, migration MigrationDefinitionV2, resource string, journal JournalRecord) (bool, error) {
	owner, ok := t.nativeOwner(migration, resource).(RegisteredYardJournalRetirement)
	if !ok {
		return false, nil
	}
	return owner.RetiredJournalBinding(ctx, journal)
}

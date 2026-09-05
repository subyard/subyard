package releasetransition

import (
	"fmt"

	journalv2 "github.com/Subyard/Subyard/internal/releasetransition/journal/v2"
)

func parseJournalV2(payload []byte) (JournalRecord, error) {
	wire, err := journalv2.Decode(payload)
	if err != nil {
		return JournalRecord{}, fmt.Errorf("%w: decode release transition journal: %w", ErrInvalid, err)
	}
	record := journalRecordFromV2(wire)
	return record, nil
}

func marshalJournalV2JSON(record JournalRecord) ([]byte, error) {
	payload, err := journalv2.Encode(journalRecordToV2(record))
	if err != nil {
		return nil, fmt.Errorf("%w: encode release transition journal: %w", ErrInvalid, err)
	}
	return payload, nil
}

func unmarshalJournalV2JSON(payload []byte, target *JournalRecord) error {
	record, err := parseJournalV2(payload)
	if err != nil {
		return err
	}
	*target = record
	return nil
}

func journalRecordToV2(record JournalRecord) journalv2.Record {
	wire := journalv2.Record{
		SchemaVersion:       record.SchemaVersion,
		Transaction:         string(record.Transaction),
		Goal:                goalToJournalV2(record.Goal),
		Releases:            releasePairToJournalV2(record.Releases),
		AuthorizationPlan:   string(record.AuthorizationPlan),
		ResumePlan:          string(record.ResumePlan),
		ArtifactDigest:      string(record.ArtifactDigest),
		RegistryDigest:      string(record.RegistryDigest),
		CatalogDigest:       string(record.CatalogDigest),
		ObservationScope:    string(record.ObservationScope),
		AuthorizationDigest: string(record.AuthorizationDigest),
		IntentDigest:        string(record.IntentDigest),
		SourceIngress:       sourceIngressToJournalV2(record.SourceIngress),
		Checkpoint:          string(record.Checkpoint),
	}
	if record.Steps != nil {
		wire.Steps = make([]journalv2.Step, len(record.Steps))
		for index, step := range record.Steps {
			wire.Steps[index] = journalStepToV2(step)
		}
	}
	return wire
}

func journalRecordFromV2(wire journalv2.Record) JournalRecord {
	record := JournalRecord{
		SchemaVersion:       wire.SchemaVersion,
		Transaction:         TransactionID(wire.Transaction),
		Goal:                goalFromJournalV2(wire.Goal),
		Releases:            releasePairFromJournalV2(wire.Releases),
		AuthorizationPlan:   PlanToken(wire.AuthorizationPlan),
		ResumePlan:          PlanToken(wire.ResumePlan),
		ArtifactDigest:      Fingerprint(wire.ArtifactDigest),
		RegistryDigest:      Fingerprint(wire.RegistryDigest),
		CatalogDigest:       Fingerprint(wire.CatalogDigest),
		ObservationScope:    Fingerprint(wire.ObservationScope),
		AuthorizationDigest: Fingerprint(wire.AuthorizationDigest),
		IntentDigest:        Fingerprint(wire.IntentDigest),
		SourceIngress:       sourceIngressFromJournalV2(wire.SourceIngress),
		Checkpoint:          JournalCheckpoint(wire.Checkpoint),
	}
	if wire.Steps != nil {
		record.Steps = make([]JournalStep, len(wire.Steps))
		for index, step := range wire.Steps {
			record.Steps[index] = journalStepFromV2(step)
		}
	}
	return record
}

func goalToJournalV2(goal Goal) journalv2.Goal {
	return journalv2.Goal{Target: string(goal.Target), Direction: string(goal.Direction)}
}

func goalFromJournalV2(goal journalv2.Goal) Goal {
	return Goal{Target: ReleaseID(goal.Target), Direction: Direction(goal.Direction)}
}

func releasePairToJournalV2(pair ReleasePair) journalv2.ReleasePair {
	wire := journalv2.ReleasePair{From: string(pair.From), Target: string(pair.Target)}
	if pair.Previous != nil {
		previous := string(*pair.Previous)
		wire.Previous = &previous
	}
	return wire
}

func releasePairFromJournalV2(pair journalv2.ReleasePair) ReleasePair {
	record := ReleasePair{From: ReleaseID(pair.From), Target: ReleaseID(pair.Target)}
	if pair.Previous != nil {
		previous := ReleaseID(*pair.Previous)
		record.Previous = &previous
	}
	return record
}

func sourceIngressToJournalV2(source *SourceIngressRequest) *journalv2.SourceIngress {
	if source == nil {
		return nil
	}
	return &journalv2.SourceIngress{
		SchemaVersion: source.SchemaVersion,
		Kind:          string(source.Kind),
		SourceRoot:    source.SourceRoot,
		DataHome:      source.DataHome,
		BinDir:        source.BinDir,
		RC:            source.RC,
		LoginRC:       source.LoginRC,
	}
}

func sourceIngressFromJournalV2(source *journalv2.SourceIngress) *SourceIngressRequest {
	if source == nil {
		return nil
	}
	return &SourceIngressRequest{
		SchemaVersion: source.SchemaVersion,
		Kind:          SourceIngressRequestKind(source.Kind),
		SourceRoot:    source.SourceRoot,
		DataHome:      source.DataHome,
		BinDir:        source.BinDir,
		RC:            source.RC,
		LoginRC:       source.LoginRC,
	}
}

func journalStepToV2(step JournalStep) journalv2.Step {
	return journalv2.Step{
		ID:         step.ID,
		Migration:  step.Migration,
		Resource:   step.Resource,
		Decision:   string(step.Decision),
		Expected:   string(step.Expected),
		Desired:    string(step.Desired),
		Checkpoint: string(step.Checkpoint),
		Evidence:   evidenceToJournalV2(step.Evidence),
	}
}

func journalStepFromV2(step journalv2.Step) JournalStep {
	return JournalStep{
		ID:         step.ID,
		Migration:  step.Migration,
		Resource:   step.Resource,
		Decision:   Decision(step.Decision),
		Expected:   Fingerprint(step.Expected),
		Desired:    Fingerprint(step.Desired),
		Checkpoint: StepCheckpoint(step.Checkpoint),
		Evidence:   evidenceFromJournalV2(step.Evidence),
	}
}

func evidenceToJournalV2(evidence *EvidenceRecord) *journalv2.Evidence {
	if evidence == nil {
		return nil
	}
	return &journalv2.Evidence{
		SchemaVersion: evidence.SchemaVersion,
		Transaction:   string(evidence.Transaction),
		Releases:      releasePairToJournalV2(evidence.Releases),
		Step:          evidence.Step,
		Expected:      string(evidence.Expected),
		Desired:       string(evidence.Desired),
		Observed:      string(evidence.Observed),
		Recovery:      string(evidence.Recovery),
		Checkpoint:    string(evidence.Checkpoint),
	}
}

func evidenceFromJournalV2(evidence *journalv2.Evidence) *EvidenceRecord {
	if evidence == nil {
		return nil
	}
	return &EvidenceRecord{
		SchemaVersion: evidence.SchemaVersion,
		Transaction:   TransactionID(evidence.Transaction),
		Releases:      releasePairFromJournalV2(evidence.Releases),
		Step:          evidence.Step,
		Expected:      Fingerprint(evidence.Expected),
		Desired:       Fingerprint(evidence.Desired),
		Observed:      Fingerprint(evidence.Observed),
		Recovery:      Fingerprint(evidence.Recovery),
		Checkpoint:    EvidenceCheckpoint(evidence.Checkpoint),
	}
}

func parseSupersededJournalV1(payload []byte) (SupersededJournalRecord, error) {
	wire, err := journalv2.DecodeArchive(payload)
	if err != nil {
		return SupersededJournalRecord{}, fmt.Errorf(
			"%w: decode superseded release transition journal: %w", ErrInvalid, err,
		)
	}
	return supersededJournalFromV1(wire), nil
}

func marshalSupersededJournalV1JSON(record SupersededJournalRecord) ([]byte, error) {
	payload, err := journalv2.EncodeArchive(supersededJournalToV1(record))
	if err != nil {
		return nil, fmt.Errorf("%w: encode superseded release transition journal: %w", ErrInvalid, err)
	}
	return payload, nil
}

func supersededJournalToV1(record SupersededJournalRecord) journalv2.Archive {
	return journalv2.Archive{
		SchemaVersion:     record.SchemaVersion,
		AuthorizationPlan: string(record.AuthorizationPlan),
		Replacement: journalv2.Replacement{
			Transaction:   string(record.Replacement.Transaction),
			Fingerprint:   string(record.Replacement.Fingerprint),
			Reason:        string(record.Replacement.Reason),
			SourceVersion: record.Replacement.SourceVersion,
		},
		Journal: journalRecordToV2(record.Journal),
	}
}

func supersededJournalFromV1(archive journalv2.Archive) SupersededJournalRecord {
	return SupersededJournalRecord{
		SchemaVersion:     archive.SchemaVersion,
		AuthorizationPlan: PlanToken(archive.AuthorizationPlan),
		Replacement: JournalReplacement{
			Transaction:   TransactionID(archive.Replacement.Transaction),
			Fingerprint:   Fingerprint(archive.Replacement.Fingerprint),
			Reason:        JournalReplacementReason(archive.Replacement.Reason),
			SourceVersion: archive.Replacement.SourceVersion,
		},
		Journal: journalRecordFromV2(archive.Journal),
	}
}

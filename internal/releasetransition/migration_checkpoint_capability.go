package releasetransition

const (
	MigrationCheckpointContractV1         = "migration-history-checkpoint-v1"
	MaxMigrationCheckpointCapabilityBytes = 1024
)

// The sealed owner opts into compact history separately from the frozen
// process and journal contracts. Retained assets without this marker keep
// their original writer while the new engine can still read checkpoints.
func ValidateMigrationCheckpointCapability(payload []byte) error {
	var capability struct {
		SchemaVersion int    `json:"schemaVersion"`
		Contract      string `json:"contract"`
	}
	if err := decodeBoundedRecord(payload, MaxMigrationCheckpointCapabilityBytes, &capability); err != nil {
		return err
	}
	if capability.SchemaVersion != 1 || capability.Contract != MigrationCheckpointContractV1 {
		return invalid("unsupported migration checkpoint capability")
	}
	return nil
}

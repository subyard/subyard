package releasetransition

import "testing"

func TestMigrationCheckpointCapabilityIsClosed(t *testing.T) {
	valid := `{"schemaVersion":1,"contract":"migration-history-checkpoint-v1"}`
	if err := ValidateMigrationCheckpointCapability([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	for _, payload := range []string{
		`{"schemaVersion":2,"contract":"migration-history-checkpoint-v1"}`,
		`{"schemaVersion":1,"contract":"future"}`,
		`{"schemaVersion":1,"contract":"migration-history-checkpoint-v1","path":"history"}`,
		valid + `{}`,
	} {
		if err := ValidateMigrationCheckpointCapability([]byte(payload)); err == nil {
			t.Fatalf("unsupported capability accepted: %s", payload)
		}
	}
}

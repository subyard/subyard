package releasetransition

import "testing"

func TestCheckpointCapabilityCodecPreservesGoSyntaxWithoutEligibilityPolicy(t *testing.T) {
	for _, sample := range []struct{ raw, expected string }{
		{`{"schemaVersion":1,"contract":"migration-history-checkpoint-v1"}`, `{"schemaVersion":1,"contract":"migration-history-checkpoint-v1"}`},
		{`{"SCHEMAVERSION":1,"schemaVersion":2,"CONTRACT":"unsupported"}`, `{"schemaVersion":2,"contract":"unsupported"}`},
		{`{"schemaVersion":null,"contract":null}`, `{"schemaVersion":0,"contract":""}`},
		{`{}`, `{"schemaVersion":0,"contract":""}`},
	} {
		projection, flags, err := normalizeLegacy(16, []byte(sample.raw))
		if err != nil || flags != 0 || string(projection) != sample.expected {
			t.Fatalf("capability syntax changed: %s => %s/%d: %v", sample.raw, projection, flags, err)
		}
	}
	for _, raw := range []string{`{"contract":3}`, `{"schemaVersion":1.5}`, `{"unknown":true}`, `{} {}`} {
		if _, _, err := normalizeLegacy(16, []byte(raw)); err == nil {
			t.Fatalf("unsupported capability syntax admitted: %s", raw)
		}
	}
}

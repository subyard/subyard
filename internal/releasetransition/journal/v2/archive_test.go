package v2

import (
	"errors"
	"strings"
	"testing"
)

const canonicalArchive = `{"schemaVersion":1,"authorizationPlan":"plan-v1-cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","replacement":{"transaction":"tx-001","fingerprint":"14423081597601712ef254ee2611dce37a0c46fccc7f26fa22c0f3182e9dc831","reason":"post-activation-scope-v0.11.1","sourceVersion":"0.11.1"},"journal":` + canonicalJournal + `}`

func TestArchiveCodecPreservesPinnedSchemaV1Bytes(t *testing.T) {
	archive, err := DecodeArchive([]byte(canonicalArchive + "\n"))
	if err != nil {
		t.Fatalf("DecodeArchive() = %v", err)
	}
	payload, err := EncodeArchive(archive)
	if err != nil {
		t.Fatalf("EncodeArchive() = %v", err)
	}
	if string(payload) != canonicalArchive {
		t.Fatalf("canonical archive changed:\n got %s\nwant %s", payload, canonicalArchive)
	}
}

func TestArchiveCodecRejectsFrozenShapeAndBindingViolations(t *testing.T) {
	tests := map[string]string{
		"unknown outer field":         strings.Replace(canonicalArchive, `,"journal":`, `,"futureArchive":true,"journal":`, 1),
		"unknown replacement field":   strings.Replace(canonicalArchive, `,"sourceVersion":"0.11.1"`, `,"sourceVersion":"0.11.1","futureReplacement":true`, 1),
		"unknown replacement reason":  strings.Replace(canonicalArchive, `"reason":"post-activation-scope-v0.11.1"`, `"reason":"future-reason"`, 1),
		"noncanonical source version": strings.Replace(canonicalArchive, `"sourceVersion":"0.11.1"`, `"sourceVersion":"00.11.1"`, 1),
		"foreign transaction":         strings.Replace(canonicalArchive, `"transaction":"tx-001"`, `"transaction":"tx-other"`, 1),
		"foreign fingerprint":         strings.Replace(canonicalArchive, `"fingerprint":"144230`, `"fingerprint":"044230`, 1),
	}
	for name, payload := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeArchive([]byte(payload)); !errors.Is(err, ErrInvalid) {
				t.Fatalf("DecodeArchive() error = %v, want ErrInvalid", err)
			}
		})
	}
}

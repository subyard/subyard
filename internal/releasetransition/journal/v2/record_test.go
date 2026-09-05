package v2

import (
	"errors"
	"strings"
	"testing"
)

const canonicalJournal = `{"schemaVersion":2,"transaction":"tx-001","goal":{"target":"release-b","direction":"activate-target"},"releases":{"from":"release-a","previous":"release-z","target":"release-b"},"authorizationPlan":"plan-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","resumePlan":"resume-v1-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","artifactDigest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","registryDigest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","catalogDigest":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","observationScope":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","authorizationDigest":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","intentDigest":"bc5283e5700d81be32183f690119aab1fa0dfffdf805c33f29c0cb48efceffa4","sourceIngress":{"schemaVersion":1,"kind":"pre-go-source-v1","sourceRoot":"/home/operator/source","dataHome":"/home/operator/.subyard","binDir":"/home/operator/.local/bin","rc":"/home/operator/.bashrc","loginRC":"/home/operator/.profile"},"checkpoint":"migrating","steps":[{"id":"settings-v2","migration":"settings-v2","resource":"yard.hermes","decision":"transform","expectedFingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","desiredFingerprint":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","checkpoint":"evidence","evidence":{"schemaVersion":2,"transaction":"tx-001","releases":{"from":"release-a","previous":"release-z","target":"release-b"},"step":"settings-v2","expectedFingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","desiredFingerprint":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","observedFingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","checkpoint":"captured"}}]}`

func TestCodecPreservesPinnedJournalV2Bytes(t *testing.T) {
	record, err := Decode([]byte(canonicalJournal + "\n"))
	if err != nil {
		t.Fatalf("Decode() = %v", err)
	}
	payload, err := Encode(record)
	if err != nil {
		t.Fatalf("Encode() = %v", err)
	}
	if string(payload) != canonicalJournal {
		t.Fatalf("canonical journal changed:\n got %s\nwant %s", payload, canonicalJournal)
	}
}

func TestDecodeRejectsJournalV2ShapeAndSemanticViolations(t *testing.T) {
	tests := map[string]string{
		"unknown top-level field": strings.Replace(canonicalJournal, `,"checkpoint":"migrating"`, `,"futureState":true,"checkpoint":"migrating"`, 1),
		"unknown nested field":    strings.Replace(canonicalJournal, `"direction":"activate-target"`, `"direction":"activate-target","futureGoal":true`, 1),
		"future direction":        strings.Replace(canonicalJournal, `"direction":"activate-target"`, `"direction":"activate-sideways"`, 1),
		"future decision":         strings.Replace(canonicalJournal, `"decision":"transform"`, `"decision":"future-transform"`, 1),
		"future source ingress":   strings.Replace(canonicalJournal, `"kind":"pre-go-source-v1"`, `"kind":"future-source"`, 1),
		"future checkpoint":       strings.Replace(canonicalJournal, `"checkpoint":"migrating"`, `"checkpoint":"future-checkpoint"`, 1),
		"future step state":       strings.Replace(canonicalJournal, `"checkpoint":"evidence","evidence"`, `"checkpoint":"future-evidence","evidence"`, 1),
		"future evidence state":   strings.Replace(canonicalJournal, `"checkpoint":"captured"`, `"checkpoint":"future-captured"`, 1),
		"foreign evidence":        strings.Replace(canonicalJournal, `"step":"settings-v2"`, `"step":"other-step"`, 1),
		"invalid intent binding":  strings.Replace(canonicalJournal, `"intentDigest":"bc5283`, `"intentDigest":"ac5283`, 1),
		"trailing JSON":           canonicalJournal + `{}`,
	}
	for name, payload := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode([]byte(payload)); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Decode() error = %v, want ErrInvalid", err)
			}
		})
	}

	if _, err := Decode([]byte(strings.Repeat("x", MaxBytes+1))); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized Decode() error = %v, want ErrInvalid", err)
	}
}

func TestValidateRejectsActivationBeforeEveryStepIsVerified(t *testing.T) {
	record, err := Decode([]byte(canonicalJournal))
	if err != nil {
		t.Fatal(err)
	}
	record.Checkpoint = "activation-intent"
	if err := record.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Validate() error = %v, want ErrInvalid", err)
	}
}

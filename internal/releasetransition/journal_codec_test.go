package releasetransition

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const pinnedJournalV2Payload = `{"schemaVersion":2,"transaction":"tx-001","goal":{"target":"release-b","direction":"activate-target"},"releases":{"from":"release-a","previous":"release-z","target":"release-b"},"authorizationPlan":"plan-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","resumePlan":"resume-v1-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","artifactDigest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","registryDigest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","catalogDigest":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","observationScope":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","authorizationDigest":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","intentDigest":"bc5283e5700d81be32183f690119aab1fa0dfffdf805c33f29c0cb48efceffa4","sourceIngress":{"schemaVersion":1,"kind":"pre-go-source-v1","sourceRoot":"/home/operator/source","dataHome":"/home/operator/.subyard","binDir":"/home/operator/.local/bin","rc":"/home/operator/.bashrc","loginRC":"/home/operator/.profile"},"checkpoint":"migrating","steps":[{"id":"settings-v2","migration":"settings-v2","resource":"yard.hermes","decision":"transform","expectedFingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","desiredFingerprint":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","checkpoint":"evidence","evidence":{"schemaVersion":2,"transaction":"tx-001","releases":{"from":"release-a","previous":"release-z","target":"release-b"},"step":"settings-v2","expectedFingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","desiredFingerprint":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","observedFingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","checkpoint":"captured"}}]}
`

func TestJournalCodecPreservesPinnedV2Payload(t *testing.T) {
	record, err := ParseJournal([]byte(pinnedJournalV2Payload))
	if err != nil {
		t.Fatalf("ParseJournal() = %v", err)
	}
	payload, err := MarshalJournal(record)
	if err != nil {
		t.Fatalf("MarshalJournal() = %v", err)
	}
	if !bytes.Equal(payload, []byte(pinnedJournalV2Payload)) {
		t.Fatalf("journal bytes changed:\n got %s\nwant %s", payload, pinnedJournalV2Payload)
	}

	direct, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("json.Marshal() = %v", err)
	}
	if string(direct)+"\n" != pinnedJournalV2Payload {
		t.Fatalf("direct embedded encoding escaped frozen codec:\n got %s", direct)
	}
}

func TestJournalV2AdapterDoesNotExposeFutureInternalState(t *testing.T) {
	type futureInternalJournal struct {
		Stable       JournalRecord
		InternalOnly string
	}
	record, err := ParseJournal([]byte(pinnedJournalV2Payload))
	if err != nil {
		t.Fatal(err)
	}
	future := futureInternalJournal{Stable: record, InternalOnly: "must-not-leak"}

	payload, err := marshalJournalV2JSON(future.Stable)
	if err != nil {
		t.Fatalf("marshalJournalV2JSON() = %v", err)
	}
	if bytes.Contains(payload, []byte("must-not-leak")) || bytes.Contains(payload, []byte("InternalOnly")) {
		t.Fatalf("adapter leaked internal-only state: %s", payload)
	}
	if string(payload)+"\n" != pinnedJournalV2Payload {
		t.Fatalf("adapter changed frozen payload: %s", payload)
	}
}

func TestJournalJSONHooksRejectUnknownNestedFields(t *testing.T) {
	unknown := strings.Replace(pinnedJournalV2Payload, `"direction":"activate-target"`, `"direction":"activate-target","futureGoal":true`, 1)
	var record JournalRecord
	if err := json.Unmarshal([]byte(unknown), &record); err == nil {
		t.Fatal("json.Unmarshal(JournalRecord) accepted a future nested field")
	}
	if _, err := ParseJournal([]byte(unknown)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("ParseJournal() error = %v, want ErrInvalid", err)
	}
}

func TestSupersededJournalCodecPreservesPinnedArchive(t *testing.T) {
	want := `{"schemaVersion":1,"authorizationPlan":"plan-v1-cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","replacement":{"transaction":"tx-001","fingerprint":"14423081597601712ef254ee2611dce37a0c46fccc7f26fa22c0f3182e9dc831","reason":"post-activation-scope-v0.11.1","sourceVersion":"0.11.1"},"journal":` +
		strings.TrimSuffix(pinnedJournalV2Payload, "\n") + "}\n"
	record, err := ParseSupersededJournal([]byte(want))
	if err != nil {
		t.Fatalf("ParseSupersededJournal() = %v", err)
	}
	payload, err := MarshalSupersededJournal(record)
	if err != nil {
		t.Fatalf("MarshalSupersededJournal() = %v", err)
	}
	if !bytes.Equal(payload, []byte(want)) {
		t.Fatalf("archive bytes changed:\n got %s\nwant %s", payload, want)
	}
	direct, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("json.Marshal() = %v", err)
	}
	if string(direct)+"\n" != want {
		t.Fatalf("direct archive encoding escaped frozen codec: %s", direct)
	}
}

func TestFrozenJournalCodecsPreserveInternalInvalidErrorClass(t *testing.T) {
	journal, err := ParseJournal([]byte(pinnedJournalV2Payload))
	if err != nil {
		t.Fatal(err)
	}
	journal.Checkpoint = "future-checkpoint"
	if _, err := MarshalJournal(journal); !errors.Is(err, ErrInvalid) {
		t.Fatalf("MarshalJournal() error = %v, want ErrInvalid", err)
	}

	want := `{"schemaVersion":1,"authorizationPlan":"plan-v1-cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","replacement":{"transaction":"tx-001","fingerprint":"14423081597601712ef254ee2611dce37a0c46fccc7f26fa22c0f3182e9dc831","reason":"post-activation-scope-v0.11.1","sourceVersion":"0.11.1"},"journal":` +
		strings.TrimSuffix(pinnedJournalV2Payload, "\n") + "}\n"
	archive, err := ParseSupersededJournal([]byte(want))
	if err != nil {
		t.Fatal(err)
	}
	archive.Replacement.Reason = "future-reason"
	if _, err := MarshalSupersededJournal(archive); !errors.Is(err, ErrInvalid) {
		t.Fatalf("MarshalSupersededJournal() error = %v, want ErrInvalid", err)
	}
}

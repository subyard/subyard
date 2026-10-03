package credentialmeta

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
)

func TestReaderProjectsMetadataWithoutPayload(t *testing.T) {
	root := t.TempDir()
	metadata := domain.CredentialMetadata{
		SchemaVersion: 1, CredentialID: "cred-0123456789abcdef0123456789abcdef",
		RevisionID: "actor-a-000000000001-aaaaaaaa", Label: "fixture", Kind: "token", Zone: "fixture",
		Scope: "staging", Consumer: "synthetic-consumer", State: "active",
		RecipientActors: []string{"actor-a"}, Syncable: true, ActorID: "actor-a",
		ActorCounter: 1, Timestamp: time.Unix(100, 0).UTC(),
	}
	record := map[string]any{"payload": "ENC[AES256_GCM,data:must-not-escape]", "sops": map[string]any{"mac": "secret"}}
	payload, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payload, &record); err != nil {
		t.Fatal(err)
	}
	record["payload"] = "ENC[AES256_GCM,data:must-not-escape]"
	record["sops"] = map[string]any{"mac": "secret"}
	payload, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "shared", "records", metadata.CredentialID, metadata.RevisionID+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := (Reader{Root: root}).ListMetadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RevisionID != metadata.RevisionID {
		t.Fatalf("unexpected metadata projection: %#v", got)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) == "" || contains(string(encoded), "must-not-escape") || contains(string(encoded), "mac") {
		t.Fatalf("secret-bearing fields escaped projection: %s", encoded)
	}

	peerPath := filepath.Join(root, "peers", "peer-one.json")
	if err := os.MkdirAll(filepath.Dir(peerPath), 0o700); err != nil {
		t.Fatal(err)
	}
	peer := `{"schemaVersion":1,"name":"peer-one","actorId":"actor-peer",` +
		`"ageRecipient":"age1must-not-escape","signingPublic":"ssh-ed25519 must-not-escape",` +
		`"transport":"local","dest":"","remoteYard":"","manualOnly":false,"trusted":true}`
	if err := os.WriteFile(peerPath, []byte(peer), 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "state", "peer-one.json")
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		t.Fatal(err)
	}
	state := `{"peer":"peer-one","lastAttempt":200,"lastSuccess":100,` +
		`"error":"private remote failure must-not-escape","lastHead":"",` +
		`"consecutiveFailures":1,"nextRetry":500}`
	if err := os.WriteFile(statePath, []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := (Reader{Root: root}).ReadCredentialStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Credentials) != 1 || len(status.Peers) != 1 || !status.Peers[0].Failed ||
		status.Peers[0].Role != "active" {
		t.Fatalf("unexpected redacted credential status: %#v", status)
	}
	encoded, err = json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"age1must-not-escape", "ssh-ed25519", "private remote failure"} {
		if contains(string(encoded), forbidden) {
			t.Fatalf("credential status exposed %q: %s", forbidden, encoded)
		}
	}
}

func TestReaderRejectsSymlinkAndCorruptGraph(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "shared", "records", "credential"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/null", filepath.Join(root, "shared", "records", "credential", "bad.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := (Reader{Root: root}).ListMetadata(context.Background()); err == nil {
		t.Fatal("credential metadata symlink was accepted")
	}
}

func TestReaderRejectsMetadataIdentityPathMismatch(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "shared", "records", "wrong-credential", "wrong-revision.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	payload := `{"schemaVersion":1,"credentialId":"cred-0123456789abcdef0123456789abcdef",` +
		`"revisionId":"actor-a-000000000001-aaaaaaaa","parents":[],"label":"fixture",` +
		`"kind":"token","zone":"fixture","scope":"staging","consumer":"none",` +
		`"state":"active","recipientActors":["actor-a"],"exclusive":false,"syncable":true,` +
		`"authorityHost":"","assignedYard":"","assignmentEpoch":0,"actorId":"actor-a",` +
		`"actorCounter":1,"timestamp":"2026-07-21T00:00:00Z"}`
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Reader{Root: root}).ListMetadata(context.Background()); err == nil {
		t.Fatal("credential metadata path mismatch was accepted")
	}
}

func contains(value, fragment string) bool {
	for index := 0; index+len(fragment) <= len(value); index++ {
		if value[index:index+len(fragment)] == fragment {
			return true
		}
	}
	return false
}

package credential

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestAnalyzeHeadsSafeMergeAndPayloadConflict(t *testing.T) {
	left := metadata("actor-a-000000000001-aaaaaaaa", "actor-a", 1)
	right := metadata("actor-b-000000000001-bbbbbbbb", "actor-b", 1)
	crypto := &testkit.CredentialCrypto{Payloads: map[string][]byte{
		left.RevisionID: []byte("same"), right.RevisionID: []byte("same"),
	}}
	decision, err := AnalyzeHeads(context.Background(), []domain.CredentialMetadata{left, right}, crypto)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Conflict || !decision.RequiresMerge || len(decision.Parents) != 2 {
		t.Fatalf("safe merge rejected: %#v", decision)
	}
	crypto.Payloads[right.RevisionID] = []byte("different")
	decision, err = AnalyzeHeads(context.Background(), []domain.CredentialMetadata{left, right}, crypto)
	if err != nil || !decision.Conflict {
		t.Fatalf("payload conflict accepted: %#v, %v", decision, err)
	}
}

func TestHeadsAndRecipientIntersection(t *testing.T) {
	parent := metadata("actor-a-000000000001-aaaaaaaa", "actor-a", 1)
	left := metadata("actor-a-000000000002-bbbbbbbb", "actor-a", 2)
	right := metadata("actor-b-000000000001-cccccccc", "actor-b", 1)
	left.Parents = []string{parent.RevisionID}
	right.Parents = []string{parent.RevisionID}
	left.RecipientActors = []string{"actor-a", "actor-b"}
	right.RecipientActors = []string{"actor-b", "actor-c"}

	heads := Heads([]domain.CredentialMetadata{right, parent, left}, parent.CredentialID)
	if len(heads) != 2 || heads[0].RevisionID != left.RevisionID || heads[1].RevisionID != right.RevisionID {
		t.Fatalf("unexpected heads: %#v", heads)
	}
	if !MetadataCompatible(heads) {
		t.Fatal("compatible metadata was rejected")
	}
	recipients := RecipientIntersection(heads)
	if len(recipients) != 1 || recipients[0] != "actor-b" {
		t.Fatalf("unexpected recipient intersection: %#v", recipients)
	}
}

func TestAnalyzeHeadsFailsOnTamperingAndTerminalWins(t *testing.T) {
	left := metadata("actor-a-000000000001-aaaaaaaa", "actor-a", 1)
	right := metadata("actor-b-000000000001-bbbbbbbb", "actor-b", 1)
	crypto := &testkit.CredentialCrypto{Err: errors.New("MAC mismatch")}
	if _, err := AnalyzeHeads(context.Background(), []domain.CredentialMetadata{left}, crypto); err == nil {
		t.Fatal("tampered head was accepted")
	}
	left.State = "tombstone"
	decision, err := AnalyzeHeads(context.Background(), []domain.CredentialMetadata{left, right}, nil)
	if err != nil || decision.Conflict || decision.State != "tombstone" {
		t.Fatalf("terminal convergence failed: %#v, %v", decision, err)
	}
}

func TestValidateGraphTrustAssignmentAndBackoff(t *testing.T) {
	parent := metadata("actor-a-000000000001-aaaaaaaa", "actor-a", 1)
	child := metadata("actor-a-000000000002-bbbbbbbb", "actor-a", 2)
	child.Parents = []string{parent.RevisionID}
	if err := ValidateRevisions([]domain.CredentialMetadata{child, parent}); err != nil {
		t.Fatal(err)
	}
	parent.Parents = []string{child.RevisionID}
	if err := ValidateRevisions([]domain.CredentialMetadata{child, parent}); err == nil {
		t.Fatal("revision cycle was accepted")
	}
	recipients, err := RekeyRecipients([]string{"actor-a"}, "actor-b", true)
	if err != nil || len(recipients) != 2 {
		t.Fatalf("trust add failed: %#v, %v", recipients, err)
	}
	exclusive := metadata("actor-a-000000000003-cccccccc", "actor-a", 3)
	exclusive.Exclusive = true
	updated, err := MoveAssignment(exclusive, "actor-a", "actor-a/yard", 0)
	if err != nil || updated.AssignmentEpoch != 1 {
		t.Fatalf("assignment move failed: %#v, %v", updated, err)
	}
	if RetryDelay(1) != 5*time.Minute || RetryDelay(100) != 5*time.Hour+20*time.Minute {
		t.Fatalf("unexpected retry delays: %s %s", RetryDelay(1), RetryDelay(100))
	}
}

func TestValidateIncomingRevisionsOwnsAppendOnlyAssignmentPolicy(t *testing.T) {
	parent := metadata("actor-a-000000000001-aaaaaaaa", "actor-a", 1)
	parent.Exclusive = true
	parent.AuthorityHost = "actor-a"
	parent.AssignedYard = "actor-a/one"
	parent.AssignmentEpoch = 1
	child := metadata("actor-a-000000000002-bbbbbbbb", "actor-a", 2)
	child.Exclusive = true
	child.AuthorityHost = "actor-a"
	child.AssignedYard = "actor-a/two"
	child.AssignmentEpoch = 2
	child.Parents = []string{parent.RevisionID}
	if err := ValidateIncomingRevisions([]domain.CredentialMetadata{child}, []domain.CredentialMetadata{parent}); err != nil {
		t.Fatal(err)
	}

	tampered := child
	tampered.AssignedYard = "actor-a/three"
	if err := ValidateIncomingRevisions([]domain.CredentialMetadata{tampered}, []domain.CredentialMetadata{parent, child}); err == nil {
		t.Fatal("changed immutable revision metadata was accepted")
	}

	unauthorized := child
	unauthorized.RevisionID = "actor-b-000000000001-cccccccc"
	unauthorized.ActorID = "actor-b"
	unauthorized.ActorCounter = 1
	if err := ValidateIncomingRevisions([]domain.CredentialMetadata{unauthorized}, []domain.CredentialMetadata{parent}); err == nil {
		t.Fatal("non-authority assignment transition was accepted")
	}
}

func TestSyncScheduleTransitionAndDuePolicy(t *testing.T) {
	now := int64(1_000)
	failed, err := NextSyncState(domain.CredentialSyncState{}, "peer-one", now, false, "offline", "", 6*time.Hour)
	if err != nil || failed.ConsecutiveFailures != 1 || failed.NextRetry != now+300 || failed.Error != "offline" {
		t.Fatalf("failure transition drifted: %#v err=%v", failed, err)
	}
	if due, err := SyncDue(failed, now+299, time.Hour); err != nil || due {
		t.Fatalf("backoff became due early: due=%v err=%v", due, err)
	}
	if due, err := SyncDue(failed, now+300, time.Hour); err != nil || !due {
		t.Fatalf("backoff did not become due: due=%v err=%v", due, err)
	}
	succeeded, err := NextSyncState(failed, "peer-one", now+300, true, "ignored", strings.Repeat("a", 40), 6*time.Hour)
	if err != nil || succeeded.ConsecutiveFailures != 0 || succeeded.LastSuccess != now+300 ||
		succeeded.NextRetry != now+300+21_600 || succeeded.Error != "" {
		t.Fatalf("success transition drifted: %#v err=%v", succeeded, err)
	}
}

func TestPeerMergePreservesRouteAndRejectsIdentityRotation(t *testing.T) {
	existing := domain.CredentialPeer{
		SchemaVersion: 1, Name: "peer-local", ActorID: "actor-b", AgeRecipient: "age1fixture",
		SigningPublic: "ssh-ed25519 AAAAfixture", Transport: "ssh", Dest: "owner.example",
		OwnerYardName: "named", Trusted: true,
	}
	inbound := domain.CredentialPeer{
		SchemaVersion: 1, Name: "remote-alias", ActorID: "actor-b", AgeRecipient: "age1fixture",
		SigningPublic: "ssh-ed25519 AAAAfixture", Transport: "inbound", ManualOnly: true, Trusted: true,
	}
	merged, err := MergePeer(inbound, &existing)
	if err != nil {
		t.Fatal(err)
	}
	if merged != existing {
		t.Fatalf("inbound enrollment erased the operator route: %#v", merged)
	}
	inbound.SigningPublic = "ssh-ed25519 AAAAchanged"
	if _, err := MergePeer(inbound, &existing); err == nil {
		t.Fatal("peer signing identity changed without explicit re-enrollment")
	}
}

func TestExclusiveAccessOwnsAssignmentTrustAndFreshness(t *testing.T) {
	head := metadata("actor-a-000000000001-aaaaaaaa", "actor-a", 1)
	head.Exclusive = true
	head.AuthorityHost = "actor-a"
	head.AssignedYard = "actor-a/yard"
	head.AssignmentEpoch = 1
	decision, err := CheckExclusiveAccess(head, "actor-a", "actor-a/yard", false, 0, 10_000, time.Hour)
	if err != nil || !decision.Allowed || decision.Reason != "authority-local" {
		t.Fatalf("local authority was rejected: %#v err=%v", decision, err)
	}
	decision, err = CheckExclusiveAccess(head, "actor-b", "actor-b/yard", true, 9_999, 10_000, time.Hour)
	if err != nil || decision.Allowed || decision.Reason != "not-assigned" {
		t.Fatalf("wrong assignment was accepted: %#v err=%v", decision, err)
	}
	head.AssignedYard = "actor-b/yard"
	decision, err = CheckExclusiveAccess(head, "actor-b", "actor-b/yard", false, 9_999, 10_000, time.Hour)
	if err != nil || decision.Allowed || decision.Reason != "authority-untrusted" {
		t.Fatalf("untrusted authority was accepted: %#v err=%v", decision, err)
	}
	decision, err = CheckExclusiveAccess(head, "actor-b", "actor-b/yard", true, 6_399, 10_000, time.Hour)
	if err != nil || decision.Allowed || decision.Reason != "authority-stale" {
		t.Fatalf("stale authority was accepted: %#v err=%v", decision, err)
	}
	decision, err = CheckExclusiveAccess(head, "actor-b", "actor-b/yard", true, 6_400, 10_000, time.Hour)
	if err != nil || !decision.Allowed || decision.Reason != "authority-fresh" {
		t.Fatalf("fresh authority was rejected: %#v err=%v", decision, err)
	}
}

func metadata(revision, actor string, counter int64) domain.CredentialMetadata {
	return domain.CredentialMetadata{
		SchemaVersion: 1, CredentialID: "cred-0123456789abcdef0123456789abcdef", RevisionID: revision,
		Label: "fixture", Kind: "token", Zone: "fixture", Scope: "staging", Consumer: "staging-env",
		State: "active", RecipientActors: []string{"actor-a"}, Syncable: true,
		ActorID: actor, ActorCounter: counter, Timestamp: time.Unix(100, 0),
	}
}

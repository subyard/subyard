package credential

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

type Decision struct {
	RequiresMerge bool                      `json:"requiresMerge"`
	Conflict      bool                      `json:"conflict"`
	Reason        string                    `json:"reason,omitempty"`
	State         string                    `json:"state"`
	Parents       []string                  `json:"parents"`
	Recipients    []string                  `json:"recipients"`
	Exclusive     bool                      `json:"exclusive"`
	Template      domain.CredentialMetadata `json:"template"`
}

type ExclusiveAccessDecision struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason"`
}

func MergePeer(incoming domain.CredentialPeer, existing *domain.CredentialPeer) (domain.CredentialPeer, error) {
	if err := ValidatePeer(incoming); err != nil {
		return domain.CredentialPeer{}, err
	}
	if existing == nil {
		return incoming, nil
	}
	if err := ValidatePeer(*existing); err != nil {
		return domain.CredentialPeer{}, fmt.Errorf("invalid enrolled peer: %w", err)
	}
	if existing.ActorID != incoming.ActorID {
		return domain.CredentialPeer{}, errors.New("peer actor identity changed; remove and re-enroll explicitly")
	}
	if existing.AgeRecipient != incoming.AgeRecipient || existing.SigningPublic != incoming.SigningPublic {
		return domain.CredentialPeer{}, errors.New("peer cryptographic identity changed; remove and re-enroll explicitly")
	}
	if incoming.Transport == "inbound" && (existing.Transport == "local" || existing.Transport == "ssh") {
		preserved := *existing
		preserved.Trusted = true
		return preserved, nil
	}
	return incoming, nil
}

func ValidatePeer(peer domain.CredentialPeer) error {
	if peer.SchemaVersion != 1 || !domain.SafeName(peer.Name) || !domain.SafeID(peer.ActorID) ||
		!strings.HasPrefix(peer.AgeRecipient, "age1") || strings.ContainsAny(peer.AgeRecipient, "\r\n\x00") ||
		!strings.HasPrefix(peer.SigningPublic, "ssh-ed25519 ") || strings.ContainsAny(peer.SigningPublic, "\r\n\x00") ||
		!peer.Trusted {
		return errors.New("peer identity or trust metadata is invalid")
	}
	switch peer.Transport {
	case "local":
		if peer.Dest != "" || peer.OwnerYardName != "" {
			return errors.New("local peer has remote transport metadata")
		}
	case "ssh":
		if peer.Dest == "" || strings.HasPrefix(peer.Dest, "-") || strings.ContainsAny(peer.Dest, "\r\n\x00") ||
			(peer.OwnerYardName != "" && !domain.SafeName(peer.OwnerYardName)) {
			return errors.New("SSH peer transport metadata is invalid")
		}
	case "inbound":
		if peer.Dest != "" || peer.OwnerYardName != "" || !peer.ManualOnly {
			return errors.New("inbound peer must be passive and route-free")
		}
	default:
		return errors.New("unknown peer transport")
	}
	return nil
}

func CheckExclusiveAccess(
	head domain.CredentialMetadata,
	actor, yard string,
	authorityTrusted bool,
	lastSuccess, now int64,
	maximumAge time.Duration,
) (ExclusiveAccessDecision, error) {
	if err := validateMetadata(head); err != nil {
		return ExclusiveAccessDecision{}, err
	}
	if !head.Exclusive || head.State != "active" {
		return ExclusiveAccessDecision{}, errors.New("exclusive access requires an active exclusive revision")
	}
	if !domain.SafeID(actor) || !validAssignedYard(yard) || now < 1 || maximumAge <= 0 {
		return ExclusiveAccessDecision{}, errors.New("invalid exclusive access context")
	}
	if head.AssignedYard != yard {
		return ExclusiveAccessDecision{Reason: "not-assigned"}, nil
	}
	if head.AuthorityHost == actor {
		return ExclusiveAccessDecision{Allowed: true, Reason: "authority-local"}, nil
	}
	if !authorityTrusted {
		return ExclusiveAccessDecision{Reason: "authority-untrusted"}, nil
	}
	if lastSuccess <= 0 || lastSuccess > now || now-lastSuccess > int64(maximumAge/time.Second) {
		return ExclusiveAccessDecision{Reason: "authority-stale"}, nil
	}
	return ExclusiveAccessDecision{Allowed: true, Reason: "authority-fresh"}, nil
}

func ValidateRevisions(revisions []domain.CredentialMetadata) error {
	byID := make(map[string]domain.CredentialMetadata, len(revisions))
	for _, revision := range revisions {
		if err := validateMetadata(revision); err != nil {
			return err
		}
		if _, duplicate := byID[revision.RevisionID]; duplicate {
			return fmt.Errorf("duplicate credential revision %q", revision.RevisionID)
		}
		byID[revision.RevisionID] = revision
	}
	for _, revision := range revisions {
		seenParents := make(map[string]struct{})
		for _, parentID := range revision.Parents {
			if _, duplicate := seenParents[parentID]; duplicate {
				return fmt.Errorf("revision %q has duplicate parent %q", revision.RevisionID, parentID)
			}
			seenParents[parentID] = struct{}{}
			parent, ok := byID[parentID]
			if !ok {
				return fmt.Errorf("revision %q has unknown parent %q", revision.RevisionID, parentID)
			}
			if parent.CredentialID != revision.CredentialID {
				return fmt.Errorf("revision %q crosses credential histories", revision.RevisionID)
			}
		}
	}
	visiting := make(map[string]bool)
	visited := make(map[string]bool)
	var visit func(string) error
	visit = func(id string) error {
		if visiting[id] {
			return fmt.Errorf("credential revision cycle at %q", id)
		}
		if visited[id] {
			return nil
		}
		visiting[id] = true
		for _, parent := range byID[id].Parents {
			if err := visit(parent); err != nil {
				return err
			}
		}
		delete(visiting, id)
		visited[id] = true
		return nil
	}
	for id := range byID {
		if err := visit(id); err != nil {
			return err
		}
	}
	if err := validateActorCounters(revisions); err != nil {
		return err
	}
	for _, revision := range revisions {
		if err := validateAssignment(revision, byID); err != nil {
			return err
		}
	}
	return nil
}

// ValidateIncomingRevisions owns the redacted append-only ledger policy. The
// adapter still proves file layout, signatures, ciphertext recipients and
// decryptability before calling this function.
func ValidateIncomingRevisions(incoming, existing []domain.CredentialMetadata) error {
	byID := make(map[string]domain.CredentialMetadata, len(existing)+len(incoming))
	for _, revision := range append(slices.Clone(existing), incoming...) {
		if previous, duplicate := byID[revision.RevisionID]; duplicate {
			if !metadataEqual(previous, revision) {
				return fmt.Errorf("revision %q changed immutable metadata", revision.RevisionID)
			}
			continue
		}
		byID[revision.RevisionID] = revision
	}
	combined := make([]domain.CredentialMetadata, 0, len(byID))
	for _, revision := range byID {
		combined = append(combined, revision)
	}
	if err := ValidateRevisions(combined); err != nil {
		return err
	}
	for _, revision := range incoming {
		if !revision.Syncable {
			return fmt.Errorf("incoming revision %q is local-only", revision.RevisionID)
		}
	}
	return nil
}

func Heads(revisions []domain.CredentialMetadata, credentialID string) []domain.CredentialMetadata {
	parents := make(map[string]struct{})
	for _, revision := range revisions {
		if revision.CredentialID != credentialID {
			continue
		}
		for _, parent := range revision.Parents {
			parents[parent] = struct{}{}
		}
	}
	heads := make([]domain.CredentialMetadata, 0)
	for _, revision := range revisions {
		if revision.CredentialID != credentialID {
			continue
		}
		if _, hasChild := parents[revision.RevisionID]; !hasChild {
			heads = append(heads, revision)
		}
	}
	sort.Slice(heads, func(left, right int) bool {
		if heads[left].ActorID != heads[right].ActorID {
			return heads[left].ActorID < heads[right].ActorID
		}
		if heads[left].ActorCounter != heads[right].ActorCounter {
			return heads[left].ActorCounter < heads[right].ActorCounter
		}
		return heads[left].RevisionID < heads[right].RevisionID
	})
	return heads
}

func AnalyzeHeads(ctx context.Context, heads []domain.CredentialMetadata, crypto ports.CredentialCrypto) (Decision, error) {
	decision, err := AnalyzeMetadataHeads(heads)
	if err == nil && crypto != nil {
		for _, head := range heads {
			if err := crypto.Verify(ctx, head); err != nil {
				return Decision{}, fmt.Errorf("verify revision %s: %w", head.RevisionID, err)
			}
		}
	}
	if err != nil || !decision.RequiresMerge || decision.Conflict ||
		decision.State == "revoked" || decision.State == "tombstone" {
		return decision, err
	}
	if crypto == nil {
		return Decision{}, errors.New("active multi-head analysis requires credential crypto")
	}
	var expected []byte
	for _, head := range heads {
		var payload bytes.Buffer
		if err := crypto.Decrypt(ctx, head, &payload); err != nil {
			clear(expected)
			return conflict(decision, "a head could not be decrypted"), nil
		}
		current := payload.Bytes()
		if expected == nil {
			expected = append([]byte(nil), current...)
		} else if !bytes.Equal(expected, current) {
			clear(expected)
			clear(current)
			return conflict(decision, "active head payloads differ"), nil
		}
		clear(current)
	}
	clear(expected)
	return decision, nil
}

// AnalyzeMetadataHeads owns the complete redacted DAG/merge decision. Payload
// equality and signatures remain injected crypto-adapter observations.
func AnalyzeMetadataHeads(heads []domain.CredentialMetadata) (Decision, error) {
	if len(heads) == 0 {
		return Decision{}, errors.New("credential has no heads")
	}
	credentialID := heads[0].CredentialID
	for _, head := range heads {
		if err := validateMetadata(head); err != nil {
			return Decision{}, err
		}
		if head.CredentialID != credentialID {
			return Decision{}, errors.New("heads belong to different credentials")
		}
	}
	decision := Decision{
		RequiresMerge: len(heads) > 1,
		State:         heads[0].State,
		Parents:       make([]string, 0, len(heads)),
		Recipients:    recipientIntersection(heads),
		Template:      heads[0],
	}
	for _, head := range heads {
		decision.Parents = append(decision.Parents, head.RevisionID)
		decision.Exclusive = decision.Exclusive || head.Exclusive
		if head.State == "tombstone" || (head.State == "revoked" && decision.State != "tombstone") {
			decision.State = head.State
		}
	}
	sort.Strings(decision.Parents)
	if len(heads) == 1 {
		return decision, nil
	}
	if len(decision.Recipients) == 0 {
		return conflict(decision, "heads have no common recipient"), nil
	}
	if decision.State == "revoked" || decision.State == "tombstone" {
		return decision, nil
	}
	if !compatibleMetadata(heads) {
		return conflict(decision, "head metadata differs"), nil
	}
	return decision, nil
}

func Summarize(revisions []domain.CredentialMetadata) ([]domain.CredentialSummary, error) {
	if len(revisions) == 0 {
		return []domain.CredentialSummary{}, nil
	}
	if err := ValidateRevisions(revisions); err != nil {
		return nil, err
	}
	credentialIDs := make(map[string]struct{})
	for _, revision := range revisions {
		credentialIDs[revision.CredentialID] = struct{}{}
	}
	result := make([]domain.CredentialSummary, 0, len(credentialIDs))
	for credentialID := range credentialIDs {
		heads := Heads(revisions, credentialID)
		decision, err := AnalyzeMetadataHeads(heads)
		if err != nil {
			return nil, err
		}
		headIDs := make([]string, 0, len(heads))
		for _, head := range heads {
			headIDs = append(headIDs, head.RevisionID)
		}
		template := decision.Template
		authorityHost, assignedYard, assignmentEpoch := template.AuthorityHost, template.AssignedYard, template.AssignmentEpoch
		if decision.Conflict {
			authorityHost, assignedYard, assignmentEpoch = "", "", 0
		}
		result = append(result, domain.CredentialSummary{
			CredentialID: credentialID, Label: template.Label, Kind: template.Kind,
			Zone: template.Zone, Consumer: template.Consumer, State: decision.State,
			Heads: headIDs, NeedsMerge: decision.RequiresMerge, Conflict: decision.Conflict,
			ConflictReason: decision.Reason, Exclusive: decision.Exclusive,
			AuthorityHost: authorityHost, AssignedYard: assignedYard,
			AssignmentEpoch: assignmentEpoch, Syncable: template.Syncable,
		})
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].CredentialID < result[right].CredentialID
	})
	return result, nil
}

func RekeyRecipients(current []string, actor string, trust bool) ([]string, error) {
	if !domain.SafeID(actor) {
		return nil, fmt.Errorf("invalid actor ID %q", actor)
	}
	set := make(map[string]struct{}, len(current)+1)
	for _, existing := range current {
		if !domain.SafeID(existing) {
			return nil, fmt.Errorf("invalid actor ID %q", existing)
		}
		set[existing] = struct{}{}
	}
	if trust {
		set[actor] = struct{}{}
	} else {
		delete(set, actor)
	}
	if len(set) == 0 {
		return nil, errors.New("credential must retain at least one recipient")
	}
	result := make([]string, 0, len(set))
	for existing := range set {
		result = append(result, existing)
	}
	sort.Strings(result)
	return result, nil
}

func ValidateMetadata(metadata domain.CredentialMetadata) error { return validateMetadata(metadata) }

func RecipientIntersection(heads []domain.CredentialMetadata) []string {
	return recipientIntersection(heads)
}

func MetadataCompatible(heads []domain.CredentialMetadata) bool {
	return len(heads) != 0 && compatibleMetadata(heads)
}

func MoveAssignment(head domain.CredentialMetadata, authorityHost, assignedYard string, expectedEpoch int64) (domain.CredentialMetadata, error) {
	if !head.Exclusive {
		return domain.CredentialMetadata{}, errors.New("only exclusive credentials have assignments")
	}
	if head.AssignmentEpoch != expectedEpoch {
		return domain.CredentialMetadata{}, fmt.Errorf("assignment epoch changed: expected %d, current %d", expectedEpoch, head.AssignmentEpoch)
	}
	if !domain.SafeID(authorityHost) {
		return domain.CredentialMetadata{}, errors.New("invalid authority host")
	}
	owner, yard, ok := strings.Cut(assignedYard, "/")
	if !ok || !domain.SafeID(owner) || !domain.SafeName(yard) {
		return domain.CredentialMetadata{}, errors.New("assigned yard must be <actor>/<yard>")
	}
	updated := head
	updated.AuthorityHost = authorityHost
	updated.AssignedYard = assignedYard
	updated.AssignmentEpoch++
	return updated, nil
}

func RetryDelay(consecutiveFailures int) time.Duration {
	if consecutiveFailures <= 0 {
		return 6 * time.Hour
	}
	exponent := consecutiveFailures - 1
	if exponent > 6 {
		exponent = 6
	}
	delay := 5 * time.Minute * time.Duration(1<<exponent)
	if delay > 6*time.Hour {
		return 6 * time.Hour
	}
	return delay
}

func NextSyncState(
	current domain.CredentialSyncState,
	peer string,
	now int64,
	success bool,
	message string,
	head string,
	successRetry time.Duration,
) (domain.CredentialSyncState, error) {
	if !domain.SafeName(peer) || now < 1 {
		return domain.CredentialSyncState{}, errors.New("sync peer and current time are required")
	}
	if current.Peer != "" && current.Peer != peer {
		return domain.CredentialSyncState{}, errors.New("sync state belongs to another peer")
	}
	if err := ValidateSyncState(current); err != nil {
		return domain.CredentialSyncState{}, err
	}
	if len(message) > 300 {
		return domain.CredentialSyncState{}, errors.New("sync error exceeds 300 bytes")
	}
	if head != "" && !safeGitObjectID(head) {
		return domain.CredentialSyncState{}, errors.New("invalid sync head")
	}
	next := current
	next.Peer = peer
	next.LastAttempt = now
	next.Error = message
	next.LastHead = head
	delay := RetryDelay(current.ConsecutiveFailures + 1)
	if success {
		if successRetry <= 0 || successRetry > 7*24*time.Hour {
			return domain.CredentialSyncState{}, errors.New("success retry interval is outside the supported range")
		}
		next.LastSuccess = now
		next.ConsecutiveFailures = 0
		next.Error = ""
		delay = successRetry
	} else {
		next.ConsecutiveFailures++
	}
	next.NextRetry = now + int64(delay/time.Second)
	return next, nil
}

func SyncDue(state domain.CredentialSyncState, now int64, minimum time.Duration) (bool, error) {
	if now < 1 || minimum < 0 {
		return false, errors.New("sync time and minimum interval are invalid")
	}
	if err := ValidateSyncState(state); err != nil {
		return false, err
	}
	if state.NextRetry > 0 {
		return now >= state.NextRetry, nil
	}
	return now-state.LastAttempt >= int64(minimum/time.Second), nil
}

func ValidateSyncState(state domain.CredentialSyncState) error {
	if state.Peer != "" && !domain.SafeName(state.Peer) {
		return errors.New("invalid sync peer")
	}
	if state.LastAttempt < 0 || state.LastSuccess < 0 || state.NextRetry < 0 ||
		state.ConsecutiveFailures < 0 || state.LastSuccess > state.LastAttempt {
		return errors.New("invalid current sync state")
	}
	if len(state.Error) > 300 {
		return errors.New("sync error exceeds 300 bytes")
	}
	if state.LastHead != "" && !safeGitObjectID(state.LastHead) {
		return errors.New("invalid sync head")
	}
	return nil
}

func validateMetadata(metadata domain.CredentialMetadata) error {
	if metadata.SchemaVersion != 1 {
		return fmt.Errorf("unsupported credential schema %d", metadata.SchemaVersion)
	}
	if !credentialID(metadata.CredentialID) || !revisionID(metadata) ||
		!domain.SafeID(metadata.ActorID) || metadata.ActorCounter < 1 {
		return errors.New("invalid credential or revision identity")
	}
	if metadata.Label == "" || len(metadata.Label) > 128 || strings.ContainsAny(metadata.Label, "\r\n") ||
		!domain.SafeID(metadata.Kind) || !domain.SafeID(metadata.Zone) || metadata.Zone == "." ||
		metadata.Zone == ".." || metadata.Scope != "staging" {
		return errors.New("invalid credential classification")
	}
	if !slices.Contains([]string{"none", "staging-env", "qa-secrets", "qa-pool"}, metadata.Consumer) {
		return errors.New("invalid credential consumer")
	}
	if !slices.Contains([]string{"active", "revoked", "tombstone"}, metadata.State) {
		return errors.New("invalid credential state")
	}
	if len(metadata.RecipientActors) == 0 || metadata.AssignmentEpoch < 0 || metadata.Timestamp.IsZero() {
		return errors.New("credential recipients, epoch and timestamp are required")
	}
	seen := make(map[string]struct{}, len(metadata.RecipientActors))
	for _, actor := range metadata.RecipientActors {
		if !domain.SafeID(actor) {
			return fmt.Errorf("invalid recipient actor %q", actor)
		}
		if _, duplicate := seen[actor]; duplicate {
			return fmt.Errorf("duplicate recipient actor %q", actor)
		}
		seen[actor] = struct{}{}
	}
	for _, parent := range metadata.Parents {
		if !domain.SafeID(parent) {
			return fmt.Errorf("invalid parent revision %q", parent)
		}
	}
	return nil
}

func credentialID(value string) bool {
	return len(value) == len("cred-")+32 && strings.HasPrefix(value, "cred-") && isLowerHex(value[len("cred-"):])
}

func revisionID(metadata domain.CredentialMetadata) bool {
	prefix := fmt.Sprintf("%s-%012d-", metadata.ActorID, metadata.ActorCounter)
	return strings.HasPrefix(metadata.RevisionID, prefix) &&
		len(metadata.RevisionID) == len(prefix)+8 && isLowerHex(metadata.RevisionID[len(prefix):])
}

func isLowerHex(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func safeGitObjectID(value string) bool {
	return (len(value) == 40 || len(value) == 64) && isLowerHex(value)
}

func validateActorCounters(revisions []domain.CredentialMetadata) error {
	seen := make(map[string]string, len(revisions))
	for _, revision := range revisions {
		key := fmt.Sprintf("%s/%d", revision.ActorID, revision.ActorCounter)
		if previous, duplicate := seen[key]; duplicate && previous != revision.RevisionID {
			return fmt.Errorf("actor %q reused counter %d", revision.ActorID, revision.ActorCounter)
		}
		seen[key] = revision.RevisionID
	}
	return nil
}

func validateAssignment(
	revision domain.CredentialMetadata,
	byID map[string]domain.CredentialMetadata,
) error {
	if !revision.Exclusive {
		if revision.AuthorityHost != "" || revision.AssignedYard != "" || revision.AssignmentEpoch != 0 {
			return fmt.Errorf("non-exclusive revision %q has assignment metadata", revision.RevisionID)
		}
		return nil
	}
	if !domain.SafeID(revision.AuthorityHost) || !validAssignedYard(revision.AssignedYard) ||
		revision.AssignmentEpoch < 1 {
		return fmt.Errorf("exclusive revision %q has invalid assignment metadata", revision.RevisionID)
	}
	if len(revision.Parents) == 0 && revision.ActorID != revision.AuthorityHost {
		return fmt.Errorf("exclusive root %q was not created by its authority", revision.RevisionID)
	}
	for _, parentID := range revision.Parents {
		parent := byID[parentID]
		if !parent.Exclusive || parent.AuthorityHost != revision.AuthorityHost {
			return fmt.Errorf("exclusive authority changed at revision %q", revision.RevisionID)
		}
		assignmentChanged := parent.AssignedYard != revision.AssignedYard ||
			parent.AssignmentEpoch != revision.AssignmentEpoch
		if assignmentChanged && (revision.ActorID != revision.AuthorityHost ||
			revision.AssignmentEpoch <= parent.AssignmentEpoch) {
			return fmt.Errorf("unauthorized assignment transition at revision %q", revision.RevisionID)
		}
	}
	return nil
}

func validAssignedYard(value string) bool {
	owner, yard, ok := strings.Cut(value, "/")
	return ok && domain.SafeID(owner) && domain.SafeName(yard)
}

func metadataEqual(left, right domain.CredentialMetadata) bool {
	return left.SchemaVersion == right.SchemaVersion && left.CredentialID == right.CredentialID &&
		left.RevisionID == right.RevisionID && slices.Equal(left.Parents, right.Parents) &&
		left.Label == right.Label && left.Kind == right.Kind && left.Zone == right.Zone &&
		left.Scope == right.Scope && left.Consumer == right.Consumer && left.State == right.State &&
		slices.Equal(left.RecipientActors, right.RecipientActors) && left.Exclusive == right.Exclusive &&
		left.Syncable == right.Syncable && left.AuthorityHost == right.AuthorityHost &&
		left.AssignedYard == right.AssignedYard && left.AssignmentEpoch == right.AssignmentEpoch &&
		left.ActorID == right.ActorID && left.ActorCounter == right.ActorCounter &&
		left.Timestamp.Equal(right.Timestamp)
}

func recipientIntersection(heads []domain.CredentialMetadata) []string {
	counts := make(map[string]int)
	for _, head := range heads {
		seen := make(map[string]struct{})
		for _, actor := range head.RecipientActors {
			if _, duplicate := seen[actor]; !duplicate {
				counts[actor]++
				seen[actor] = struct{}{}
			}
		}
	}
	result := make([]string, 0)
	for actor, count := range counts {
		if count == len(heads) {
			result = append(result, actor)
		}
	}
	sort.Strings(result)
	return result
}

func compatibleMetadata(heads []domain.CredentialMetadata) bool {
	first := heads[0]
	for _, head := range heads[1:] {
		if head.Label != first.Label || head.Kind != first.Kind || head.Zone != first.Zone ||
			head.Consumer != first.Consumer || head.AuthorityHost != first.AuthorityHost ||
			head.AssignedYard != first.AssignedYard || head.AssignmentEpoch != first.AssignmentEpoch {
			return false
		}
	}
	return true
}

func conflict(decision Decision, reason string) Decision {
	decision.Conflict = true
	decision.Reason = reason
	return decision
}

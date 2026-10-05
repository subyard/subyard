package credentialruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
)

func TestPreparedMetadataPlanRetainsBindingAndRejectsHeadDrift(t *testing.T) {
	runtime := credentialFixture(t)
	installCredentialIdentity(t, runtime)
	head := credentialMetadata("actor-a-000000000001-aaaaaaaa", "actor-a", 1)
	writeCredentialRecord(t, runtime, sharedLedger, head)
	prepared, err := runtime.Prepare(context.Background(), "", []string{"revoke", head.CredentialID})
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.Binding) != 64 || len(prepared.Steps) != 1 || prepared.Steps[0].Target != "credential/"+head.CredentialID {
		t.Fatalf("invalid captured plan: %#v", prepared)
	}
	if err := domain.ValidateOperationSteps(prepared.Steps); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(prepared)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), prepared.Binding) {
		t.Fatal("private binding leaked into public projection")
	}
	next := credentialMetadata("actor-a-000000000002-bbbbbbbb", "actor-a", 2)
	next.Parents = []string{head.RevisionID}
	writeCredentialRecord(t, runtime, sharedLedger, next)
	if err := prepared.Execute(context.Background()); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("head drift not typed stale: %v", err)
	}
	fresh, err := runtime.Prepare(context.Background(), "", []string{"revoke", head.CredentialID})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Binding == prepared.Binding {
		t.Fatal("changed captured heads retained binding")
	}
}

func TestProtectedPlanRefusesLiveSourceReplacementWithoutReadingValue(t *testing.T) {
	runtime := credentialFixture(t)
	installCredentialIdentity(t, runtime)
	path := filepath.Join(t.TempDir(), "value")
	writeCredentialFile(t, path, "first-private-value", 0o600)
	prepared, err := runtime.Prepare(context.Background(), "", []string{"add", "fixture", "--file", path, "--local-only"})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Binding == "" || len(prepared.Steps) == 0 {
		t.Fatal("protected workflow lacks retained safe plan")
	}
	payload, err := json.Marshal(prepared)
	if err != nil || strings.Contains(string(payload), "first-private-value") || strings.Contains(string(payload), path) {
		t.Fatal("protected source leaked into public plan")
	}
	replacement := path + ".new"
	writeCredentialFile(t, replacement, "second-private-value", 0o600)
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Execute(context.Background()); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("source replacement accepted: %v", err)
	}
	all, err := runtime.allRecords(context.Background())
	if err != nil || len(all[sharedLedger])+len(all[localLedger]) != 0 {
		t.Fatal("source drift published a credential")
	}
}

func TestPreparedAutoSyncNoOpChecksExactPeer(t *testing.T) {
	runtime := credentialFixture(t)
	installCredentialIdentity(t, runtime)
	identity := credentialPeerIdentity("actor-active")
	if _, err := runtime.storePeer("active", identity, "local", "", "", true); err != nil {
		t.Fatal(err)
	}
	prepared, err := runtime.Prepare(context.Background(), "", []string{"auto-sync", "pause", "@active"})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Changed || prepared.Steps[0].Decision != domain.StepSkip {
		t.Fatal("converged peer did not project skip")
	}
	if _, err := runtime.storePeer("active", identity, "local", "", "", false); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Execute(context.Background()); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("no-op scope expansion accepted: %v", err)
	}
}

package credentialruntime

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

func settingsLedgerFixture(t *testing.T) *Runtime {
	t.Helper()
	r := setupLedgerFixture(t)
	r.consumers[0].Format = "rsa-private-key-with-settings"
	r.consumers[0].Path = "fixture/connection.json"
	r.consumers[0].LegacyPath = "fixture/key.pem"
	r.consumerOwners["fixture-key"] = profile.Definition{Setup: &profile.Setup{SyncFields: true, Consumer: "fixture-key", Zone: "global", Fields: []profile.Field{{Name: "account_id", Kind: "numeric-string"}, {Name: "installation_id", Kind: "positive-integer"}}}}
	if err := r.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	return r
}

func settingsPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func TestSynchronizedSettingsMigrationRotationAndRecovery(t *testing.T) {
	r := settingsLedgerFixture(t)
	ctx := context.Background()
	original := settingsPEM(t)
	id, err := r.add(ctx, addOptions{label: "fixture", kind: "file", zone: "global", consumer: "fixture-key", localOnly: true}, original)
	if err != nil {
		t.Fatal(err)
	}
	legacy := r.LegacyConsumerPath("fixture-key")
	writeCredentialFile(t, legacy, string(original), 0o600)
	fields := map[string]any{"account_id": "42", "installation_id": int64(987), "local_path": "/owner-only"}
	opts := SetupCredentialOptions{Consumer: "fixture-key", Zone: "global", Label: "fixture", Settings: fields}
	migration, err := r.PrepareSetupCredential(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	fields["account_id"] = "999" // Preparation owns its validated fields.
	if err := migration.Execute(ctx); err != nil {
		t.Fatal(err)
	}
	path, _ := r.ConsumerPath("fixture-key", "global")
	got, err := r.ConsumerSettings("fixture-key", "global", path)
	if err != nil || got["account_id"] != "42" || got["installation_id"] != int64(987) || len(got) != 2 {
		t.Fatalf("unexpected shared fields: %#v %v", got, err)
	}
	if _, err := os.Lstat(legacy); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("legacy consumer retained")
	}
	scope, head, err := r.singleHead(ctx, id)
	if err != nil || scope != localLedger || head.Syncable || len(head.Parents) != 1 {
		t.Fatalf("migration changed local-only identity/scope: %#v %v", head, err)
	}
	oldRevision := head.Parents[0]
	opts.Settings = got
	again, err := r.PrepareSetupCredential(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := again.Execute(ctx); err != nil {
		t.Fatal(err)
	}
	_, same, _ := r.singleHead(ctx, id)
	if same.RevisionID != head.RevisionID {
		t.Fatal("repeat migration published another revision")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	recovery, err := r.PrepareSetupCredential(ctx, SetupCredentialOptions{Consumer: "fixture-key", Zone: "global", Label: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery.Execute(ctx); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(testkit.TempDir(t), "replacement.pem")
	testkit.WriteFile(t, replacement, settingsPEM(t), 0o600)
	rotation, err := r.prepareRotate(ctx, []string{id, "--file", replacement})
	if err != nil {
		t.Fatal(err)
	}
	if err := rotation.Execute(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.materializeCredential(ctx, scope, id, true); err != nil {
		t.Fatal(err)
	}
	got, err = r.ConsumerSettings("fixture-key", "global", path)
	if err != nil || got["account_id"] != "42" {
		t.Fatalf("PEM rotation lost identifiers: %#v %v", got, err)
	}
	rollback, err := r.prepareRollback(ctx, []string{id, oldRevision})
	if err != nil {
		t.Fatal(err)
	}
	if err := rollback.Execute(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.materializeCredential(ctx, scope, id, true); err != nil {
		t.Fatal(err)
	}
	got, err = r.ConsumerSettings("fixture-key", "global", path)
	if err != nil || got["account_id"] != "42" {
		t.Fatalf("legacy rollback lost identifiers: %#v %v", got, err)
	}
	revoke, err := r.prepareTerminal(ctx, "revoke", []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if err := revoke.Execute(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("revoked connection retained")
	}
}

func TestSynchronizedSettingsMigrationRejectsConflictAndLegacyDrift(t *testing.T) {
	r := settingsLedgerFixture(t)
	ctx := context.Background()
	key := settingsPEM(t)
	schema := r.consumerOwners["fixture-key"].Setup
	payload, err := schema.EncodeCredentialSettings(map[string]any{"account_id": "42", "installation_id": int64(987)}, key)
	if err != nil {
		t.Fatal(err)
	}
	id, err := r.add(ctx, addOptions{label: "fixture", kind: "file", zone: "global", consumer: "fixture-key"}, payload)
	if err != nil {
		t.Fatal(err)
	}
	_, before, _ := r.singleHead(ctx, id)
	opts := SetupCredentialOptions{Consumer: "fixture-key", Zone: "global", Label: "fixture", Settings: map[string]any{"account_id": "99", "installation_id": int64(987)}}
	conflict, err := r.PrepareSetupCredential(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := conflict.Execute(ctx); err == nil {
		t.Fatal("conflicting local settings accepted")
	}
	_, after, _ := r.singleHead(ctx, id)
	if before.RevisionID != after.RevisionID {
		t.Fatal("conflict changed ledger")
	}
	opts.Settings["account_id"] = "42"
	stale, err := r.PrepareSetupCredential(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	writeCredentialFile(t, r.LegacyConsumerPath("fixture-key"), string(key), 0o600)
	if err := stale.Execute(ctx); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("legacy drift accepted: %v", err)
	}
}

func TestSynchronizedSettingsResolveLegacyChooseAndAmbiguousFields(t *testing.T) {
	r := settingsLedgerFixture(t)
	ctx := context.Background()
	key := settingsPEM(t)
	id, err := r.add(ctx, addOptions{label: "fixture", kind: "file", zone: "global", consumer: "fixture-key"}, key)
	if err != nil {
		t.Fatal(err)
	}
	scope, original, _ := r.singleHead(ctx, id)
	schema := r.consumerOwners["fixture-key"].Setup
	bundle, err := schema.EncodeCredentialSettings(map[string]any{"account_id": "42", "installation_id": int64(987)}, key)
	if err != nil {
		t.Fatal(err)
	}
	spec := specFromMetadata(original)
	spec.Parents = []string{original.RevisionID}
	if _, err := r.publish(ctx, scope, spec, bundle); err != nil {
		t.Fatal(err)
	}
	bare, err := r.publish(ctx, scope, spec, key)
	if err != nil {
		t.Fatal(err)
	}
	choose, err := r.prepareResolve(ctx, []string{id, "--choose", bare.RevisionID})
	if err != nil {
		t.Fatal(err)
	}
	if err := choose.Execute(ctx); err != nil {
		t.Fatal(err)
	}
	_, resolved, err := r.singleHead(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := r.decrypt(ctx, scope, resolved)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := schema.DecodeCredentialSettings(payload)
	clear(payload)
	if err != nil || decoded.Settings["account_id"] != "42" {
		t.Fatalf("legacy choose lost unambiguous fields: %#v %v", decoded.Settings, err)
	}
	spec = specFromMetadata(resolved)
	spec.Parents = []string{resolved.RevisionID}
	first, err := r.publish(ctx, scope, spec, bundle)
	if err != nil {
		t.Fatal(err)
	}
	different, err := schema.EncodeCredentialSettings(map[string]any{"account_id": "99", "installation_id": int64(987)}, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.publish(ctx, scope, spec, different); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(testkit.TempDir(t), "key.pem")
	testkit.WriteFile(t, replacement, key, 0o600)
	for _, args := range [][]string{{id, "--choose", original.RevisionID}, {id, "--rotate", "--file", replacement}} {
		operation, err := r.prepareResolve(ctx, args)
		if err != nil {
			t.Fatal(err)
		}
		if err := operation.Execute(ctx); err == nil {
			t.Fatal("ambiguous fields accepted for a bare key")
		}
	}
	complete, err := r.prepareResolve(ctx, []string{id, "--choose", first.RevisionID})
	if err != nil {
		t.Fatal(err)
	}
	if err := complete.Execute(ctx); err != nil {
		t.Fatal(err)
	}
}

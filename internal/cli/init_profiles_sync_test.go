package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

func syncedProfileSetupFixture(t *testing.T) (*CLI, *initExecution, string) {
	t.Helper()
	cli, execution := profileSetupFixture(t, "")
	path := filepath.Join(cli.options.RepositoryRoot, "config", "profiles", "fixture", "profile.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.ReplaceAll(data, []byte(`"format":"rsa-private-key"`), []byte(`"format":"rsa-private-key-with-settings","legacy_path":"fixture/old-key.pem"`))
	data = bytes.ReplaceAll(data, []byte(`"setup":{`), []byte(`"setup":{"sync_fields":true,`))
	testkit.WriteFile(t, path, data, 0o600)
	cli.promptInputTerminal = func() bool { return false }
	definitions, err := profile.Load(cli.options.RepositoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(execution.loaded.Environment["SUBYARD_KEYS_CONSUMER_ROOT"], "fixture", "key.pem")
	rawPath := filepath.Join(testkit.TempDir(t), "key.pem")
	writeProfileSetupPEM(t, rawPath)
	key, err := os.ReadFile(rawPath)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := definitions[0].Setup.EncodeCredentialSettings(map[string]any{"account_id": "42", "installation_id": int64(987)}, key)
	if err != nil {
		t.Fatal(err)
	}
	writeProfileTestFile(t, keyPath, bundle, 0o600)
	return cli, execution, keyPath
}

func TestSyncedInitProfilePeerRecoveryAndLegacyAdoption(t *testing.T) {
	cli, execution, keyPath := syncedProfileSetupFixture(t)
	ctx := context.Background()
	set, err := cli.prepareInitProfiles(ctx, execution, []string{"--yes"})
	if err != nil || set != nil {
		t.Fatalf("complete peer requested setup: %v %v", set, err)
	}
	cfg := filepath.Join(execution.loaded.Context.Paths.ConfigHome, "fixture.json")
	writeProfileTestFile(t, cfg, []byte(`{"account_id":"42","installation_id":987}`), 0o600)
	set, err = cli.prepareInitProfiles(ctx, execution, []string{"--yes"})
	if err != nil || set == nil || set.items[0].key != nil {
		t.Fatalf("legacy adoption: %v %v", set, err)
	}
	if err := set.apply(ctx, execution, cli.options.Stdout); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(cfg)
	if err != nil || !strings.Contains(string(got), `"use_credential_settings":true`) {
		t.Fatalf("missing delegation: %q %v", got, err)
	}
	set, err = cli.prepareInitProfiles(ctx, execution, []string{"--yes"})
	if err != nil || set != nil {
		t.Fatalf("repeat adoption: %v %v", set, err)
	}
	// Authorized identifier updates require no second local settings update.
	data, _ := os.ReadFile(keyPath)
	var bundle map[string]any
	if err := json.Unmarshal(data, &bundle); err != nil {
		t.Fatal(err)
	}
	bundle["settings"].(map[string]any)["account_id"] = "99"
	updated, _ := json.Marshal(bundle)
	testkit.WriteFile(t, keyPath, updated, 0o600)
	set, err = cli.prepareInitProfiles(ctx, execution, []string{"--yes"})
	if err != nil || set != nil {
		t.Fatalf("delegated update: %v %v", set, err)
	}
}

func TestSyncedInitProfileAdoptionRejectsDriftAndConflicts(t *testing.T) {
	cli, execution, keyPath := syncedProfileSetupFixture(t)
	cfg := filepath.Join(execution.loaded.Context.Paths.ConfigHome, "fixture.json")
	original := []byte(`{"account_id":"42","installation_id":987}`)
	writeProfileTestFile(t, cfg, original, 0o600)
	set, err := cli.prepareInitProfiles(context.Background(), execution, []string{"--yes"})
	if err != nil || set == nil {
		t.Fatalf("prepare: %v %v", set, err)
	}
	data, _ := os.ReadFile(keyPath)
	testkit.WriteFile(t, keyPath, append(data, '\n'), 0o600)
	if err := set.apply(context.Background(), execution, cli.options.Stdout); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("connection drift accepted: %v", err)
	}
	current, _ := os.ReadFile(cfg)
	if !bytes.Equal(current, original) {
		t.Fatal("drift changed local JSON")
	}
	testkit.WriteFile(t, cfg, []byte(`{"account_id":"99","installation_id":987}`), 0o600)
	set, err = cli.prepareInitProfiles(context.Background(), execution, []string{"--yes"})
	if err != nil || set != nil {
		t.Fatalf("conflict should preserve local settings: %v %v", set, err)
	}
}

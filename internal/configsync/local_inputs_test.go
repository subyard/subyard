package configsync

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
)

func installLegacySync(t *testing.T, fixture *syncFixture) Manifest {
	t.Helper()
	catalog, err := config.LoadCatalog(fixture.root)
	if err != nil {
		t.Fatal(err)
	}
	source, err := readSource(fixture.options(false), fixture.hostID, catalog)
	if err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{
		SchemaVersion: 1, Generation: 7, SourceID: source.id, SourceCommit: source.commit,
		HostID: fixture.hostID, SourceSchema: sourceSchema, SourceDigest: source.digest,
	}
	for cached, file := range source.files {
		path := localSettingsPath(cached)
		writeSyncTestFile(t, filepath.Join(fixture.configHome, path), string(file.Content), os.FileMode(file.Mode))
		manifest.Files = append(manifest.Files, ManagedFile{Path: path, Digest: file.Digest, Mode: file.Mode, Generation: 7})
	}
	content, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeSyncTestFile(t, ManifestPath(fixture.configHome), string(content), 0o600)
	writeSyncTestFile(t, HostIDPath(fixture.configHome), fixture.hostID+"\n", 0o600)
	return manifest
}

func TestConfigSyncMigratesLegacySettingsWithoutLosingLocalChanges(t *testing.T) {
	for _, state := range []string{"unchanged", "edited", "mode-changed", "missing"} {
		t.Run(state, func(t *testing.T) {
			fixture := newSyncFixture(t, "owner-a")
			asset := "overrides/host/agents/codex/rules/repo.rules"
			fixture.writeSource("hosts/owner-a/config.env", "SSH_PORT=2233\n")
			fixture.writeSource("hosts/owner-a/overrides/agents/codex/rules/repo.rules", "old rule\n")
			fixture.commit("legacy source")
			installLegacySync(t, fixture)
			scalar := filepath.Join(fixture.configHome, "config.env")
			localAsset := filepath.Join(fixture.configHome, asset)
			retained := state == "edited" || state == "mode-changed"
			scalarContent, assetContent := "SSH_PORT=2233\n", "old rule\n"
			scalarMode, assetMode := os.FileMode(0o600), os.FileMode(0o644)
			switch state {
			case "edited":
				scalarContent, assetContent = "# operator choice\nSSH_PORT=2299\n", "operator rule\n"
				writeSyncTestFile(t, scalar, scalarContent, scalarMode)
				writeSyncTestFile(t, localAsset, assetContent, assetMode)
			case "mode-changed":
				scalarMode, assetMode = 0o640, 0o600
				if err := os.Chmod(scalar, scalarMode); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(localAsset, assetMode); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(scalar); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(localAsset); err != nil {
					t.Fatal(err)
				}
			}
			fixture.writeSource("hosts/owner-a/config.env", "SSH_PORT=2235\n")
			fixture.writeSource("hosts/owner-a/overrides/agents/codex/rules/repo.rules", "Git rule\n")
			fixture.commit("new source")
			plan, err := BuildPlan(fixture.options(false))
			if err != nil {
				t.Fatal(err)
			}
			if err := Apply(plan); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{scalar, localAsset} {
				if !retained {
					if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("migration left/recreated a local input: %v", err)
					}
				}
			}
			if retained {
				assertSyncTestFile(t, scalar, scalarContent, scalarMode)
				assertSyncTestFile(t, localAsset, assetContent, assetMode)
			}
			assertSyncTestFile(t, filepath.Join(fixture.configHome, gitSettingsPath("config.env")), "SSH_PORT=2235\n", 0o600)
			assertSyncTestFile(t, filepath.Join(fixture.configHome, gitSettingsPath(asset)), "Git rule\n", 0o644)
			manifest, err := readManifest(fixture.configHome)
			if err != nil || manifest.SchemaVersion != 2 || manifest.Generation != 8 {
				t.Fatalf("migration manifest: %#v, %v", manifest, err)
			}
			for _, file := range manifest.Files {
				if !strings.HasPrefix(file.Path, config.GitSettingsRelativePath+"/") {
					t.Fatalf("manifest retained local ownership: %s", file.Path)
				}
			}
			converged, err := BuildPlan(fixture.options(false))
			if err != nil || converged.NeedsApply() {
				t.Fatalf("migration did not converge: %#v %v", converged, err)
			}
		})
	}
}

func TestConfigSyncPreservesLocalScalarAndFileOverridesAcrossSyncOffline(t *testing.T) {
	fixture := newSyncFixture(t, "owner-a")
	fixture.writeSource("hosts/owner-a/config.env", "SSH_PORT=2233\n")
	fixture.writeSource("hosts/owner-a/yards/demo/config.env", "SSH_PORT=2234\n")
	fixture.writeSource("hosts/owner-a/overrides/agents/codex/rules/repo.rules", "Git rule\n")
	fixture.commit("initial")
	local := filepath.Join(fixture.configHome, "config.env")
	asset := filepath.Join(fixture.configHome, "overrides/host/agents/codex/rules/repo.rules")
	writeSyncTestFile(t, local, "CODING_TOOL_INTEGRATIONS=''\n", 0o640)
	writeSyncTestFile(t, asset, "operator rule\n", 0o640)
	for _, port := range []string{"2233", "2235"} {
		fixture.writeSource("hosts/owner-a/config.env", "SSH_PORT="+port+"\n")
		if port == "2235" {
			fixture.commit("change Git setting")
		}
		plan, err := BuildPlan(fixture.options(false))
		if err != nil {
			t.Fatal(err)
		}
		if err := Apply(plan); err != nil {
			t.Fatal(err)
		}
		assertSyncTestFile(t, local, "CODING_TOOL_INTEGRATIONS=''\n", 0o640)
		assertSyncTestFile(t, asset, "operator rule\n", 0o640)
	}
	if err := os.Rename(fixture.source, fixture.source+"-offline"); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(config.LoadOptions{
		RepositoryRoot: fixture.root, OperatorHome: fixture.operatorHome,
		Environment: fixture.options(false).Environment, DisablePrivate: true,
	})
	if err != nil || loaded.Context.SSHPort != 2235 || loaded.Environment["CODING_TOOL_INTEGRATIONS"] != "" {
		t.Fatalf("offline fallback/local empty resolution: port=%d err=%v", loaded.Context.SSHPort, err)
	}
	if loaded.Environment["AGENT_codex_RULES"] != asset {
		t.Fatal("local file override did not win over the cached Git file")
	}
	demo, err := config.Load(config.LoadOptions{
		RepositoryRoot: fixture.root, OperatorHome: fixture.operatorHome, YardName: "demo",
		Environment: fixture.options(false).Environment, DisablePrivate: true,
	})
	if err != nil || demo.Context.SSHPort != 2234 {
		t.Fatalf("offline Git-only yard: port=%d err=%v", demo.Context.SSHPort, err)
	}
	if err := os.Remove(asset); err != nil {
		t.Fatal(err)
	}
	loaded, err = config.Load(config.LoadOptions{
		RepositoryRoot: fixture.root, OperatorHome: fixture.operatorHome,
		Environment: fixture.options(false).Environment, DisablePrivate: true,
	})
	if err != nil || loaded.Environment["AGENT_codex_RULES"] != filepath.Join(fixture.configHome, config.GitSettingsRelativePath, "overrides/host/agents/codex/rules/repo.rules") {
		t.Fatalf("absent local file did not reveal cached Git file: %v", err)
	}
}

func TestConfigSyncRejectsChangesToObservedLocalInputs(t *testing.T) {
	for _, test := range []struct {
		name, path, before, after string
	}{
		{"scalar-created", "config.env", "", "SSH_PORT=2299\n"},
		{"scalar-edited", "config.env", "SSH_PORT=2298\n", "SSH_PORT=2299\n"},
		{"scalar-inode-replaced", "config.env", "SSH_PORT=2299\n", "SSH_PORT=2299\n"},
		{"asset-created", "overrides/host/agents/codex/rules/repo.rules", "", "operator rule\n"},
		{"asset-edited", "overrides/host/agents/codex/rules/repo.rules", "old rule\n", "new rule\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSyncFixture(t, "owner-a")
			fixture.writeSource("hosts/owner-a/config.env", "SSH_PORT=2233\n")
			fixture.commit("initial")
			path := filepath.Join(fixture.configHome, test.path)
			if test.before != "" {
				writeSyncTestFile(t, path, test.before, 0o600)
			}
			plan, err := BuildPlan(fixture.options(false))
			if err != nil {
				t.Fatal(err)
			}
			if test.before == "" {
				writeSyncTestFile(t, path, test.after, 0o600)
			} else if err := config.WritePersistentFile(fixture.configHome, path, []byte(test.after)); err != nil {
				t.Fatal(err)
			}
			if err := Apply(plan); !errors.Is(err, ErrPlanStale) {
				t.Fatalf("concurrent local input was accepted: %v", err)
			}
			assertSyncTestFile(t, path, test.after, 0o600)
			if _, err := os.Lstat(ManifestPath(fixture.configHome)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stale plan published a manifest: %v", err)
			}
		})
	}
}

func TestConfigSyncKeepsLocalYardWhenGitDefinitionIsRemoved(t *testing.T) {
	fixture := newSyncFixture(t, "owner-a")
	fixture.writeSource("hosts/owner-a/config.env", "SSH_PORT=2233\n")
	fixture.writeSource("hosts/owner-a/yards/demo/config.env", "SSH_PORT=2234\n")
	fixture.commit("initial")
	plan, err := BuildPlan(fixture.options(false))
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(fixture.configHome, "yards/demo/config.env")
	writeSyncTestFile(t, local, "SSH_PORT=2299\n", 0o600)
	if err := os.RemoveAll(filepath.Join(fixture.source, "hosts/owner-a/yards/demo")); err != nil {
		t.Fatal(err)
	}
	fixture.commit("remove Git definition")
	options := fixture.options(false)
	options.YardInUse = func(string) (string, bool, error) {
		t.Fatal("retained local yard must not be assessed as deleted")
		return "", false, nil
	}
	plan, err = BuildPlan(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	assertSyncTestFile(t, local, "SSH_PORT=2299\n", 0o600)
}

func TestConfigSyncMigrationRecoveryUsesManifestCommitPoint(t *testing.T) {
	for _, published := range []bool{false, true} {
		name := "rollback"
		if published {
			name = "finish"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newSyncFixture(t, "owner-a")
			fixture.writeSource("hosts/owner-a/config.env", "SSH_PORT=2233\n")
			fixture.commit("initial")
			legacy := installLegacySync(t, fixture)
			plan, err := BuildPlan(fixture.options(false))
			if err != nil {
				t.Fatal(err)
			}
			content, err := newManifestContent(plan)
			if err != nil {
				t.Fatal(err)
			}
			tx := transaction{
				SchemaVersion: 1, ID: "8-aaaaaaaaaaaaaaaa", Phase: "publishing", Applied: 2,
				PlanDigest: plan.Digest, NewManifestDigest: digestBytes(content),
				Entries: []transactionEntry{
					{Path: gitSettingsPath("config.env"), Action: "add", AfterDigest: legacy.Files[0].Digest, AfterMode: 0o600},
					{Path: "config.env", Action: "delete", Existed: true, BeforeDigest: legacy.Files[0].Digest, BeforeMode: 0o600},
				},
			}
			backup := filepath.Join(fixture.configHome, ".sync", "transactions", tx.ID, "backup", "config.env")
			writeSyncTestFile(t, backup, "SSH_PORT=2233\n", 0o600)
			writeSyncTestFile(t, filepath.Join(fixture.configHome, gitSettingsPath("config.env")), "SSH_PORT=2233\n", 0o600)
			if err := os.Remove(filepath.Join(fixture.configHome, "config.env")); err != nil {
				t.Fatal(err)
			}
			if published {
				writeSyncTestFile(t, ManifestPath(fixture.configHome), string(content), 0o600)
			}
			if err := writeTransaction(fixture.configHome, tx); err != nil {
				t.Fatal(err)
			}
			if err := Recover(fixture.configHome); err != nil {
				t.Fatal(err)
			}
			present, absent := "config.env", gitSettingsPath("config.env")
			if published {
				present, absent = absent, present
			}
			assertSyncTestFile(t, filepath.Join(fixture.configHome, present), "SSH_PORT=2233\n", 0o600)
			if _, err := os.Lstat(filepath.Join(fixture.configHome, absent)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("recovery retained wrong storage layer: %v", err)
			}
		})
	}
}

func TestConfigSyncLegacyYardDeletionRequiresRemainingDefinition(t *testing.T) {
	for _, state := range []string{"unchanged", "missing", "edited"} {
		t.Run(state, func(t *testing.T) {
			fixture := newSyncFixture(t, "owner-a")
			fixture.writeSource("hosts/owner-a/config.env", "SSH_PORT=2233\n")
			fixture.writeSource("hosts/owner-a/yards/demo/config.env", "SSH_PORT=2234\n")
			fixture.commit("legacy source")
			installLegacySync(t, fixture)
			local := filepath.Join(fixture.configHome, "yards/demo/config.env")
			if state == "missing" {
				if err := os.Remove(local); err != nil {
					t.Fatal(err)
				}
			} else if state == "edited" {
				writeSyncTestFile(t, local, "SSH_PORT=2299\n", 0o600)
			}
			if err := os.RemoveAll(filepath.Join(fixture.source, "hosts/owner-a/yards/demo")); err != nil {
				t.Fatal(err)
			}
			fixture.commit("remove source yard")
			options := fixture.options(false)
			options.YardInUse = func(string) (string, bool, error) {
				return "managed yard exists", true, nil
			}
			plan, err := BuildPlan(options)
			if state == "edited" {
				if err != nil {
					t.Fatal(err)
				}
				if err := Apply(plan); err != nil {
					t.Fatal(err)
				}
				assertSyncTestFile(t, local, "SSH_PORT=2299\n", 0o600)
			} else if err == nil || !strings.Contains(err.Error(), "managed yard exists") {
				t.Fatalf("legacy in-use yard deletion was accepted: %v", err)
			}
		})
	}
}

func TestConfigSyncManifestCannotOwnOtherStorageRoles(t *testing.T) {
	for _, test := range []struct {
		name   string
		schema int
		path   string
		want   string
	}{
		{"new-local", 2, "config.env", "invalid managed path"},
		{"new-identity", 2, gitSettingsPath("host-id"), "invalid managed path"},
		{"new-journal", 2, gitSettingsPath(".sync/transaction.json"), "invalid managed path"},
		{"new-unknown-asset", 2, gitSettingsPath("overrides/host/agents/unknown/config.json"), "unknown file setting"},
		{"old-identity", 1, "host-id", "invalid managed path"},
		{"old-registration", 1, ".sync/source.json", "invalid managed path"},
		{"old-cache", 1, gitSettingsPath("config.env"), "invalid managed path"},
		{"old-runtime", 1, "projects/runtime.json", "invalid managed path"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSyncFixture(t, "owner-a")
			fixture.writeSource("hosts/owner-a/config.env", "SSH_PORT=2233\n")
			fixture.commit("source")
			manifest := installLegacySync(t, fixture)
			manifest.SchemaVersion = test.schema
			manifest.Files[0].Path = test.path
			content, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			writeSyncTestFile(t, ManifestPath(fixture.configHome), string(content), 0o600)
			if _, err := BuildPlan(fixture.options(false)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("manifest gained ownership of unrelated storage: %v", err)
			}
			assertSyncTestFile(t, filepath.Join(fixture.configHome, "config.env"), "SSH_PORT=2233\n", 0o600)
			assertSyncTestFile(t, HostIDPath(fixture.configHome), "owner-a\n", 0o600)
		})
	}
}

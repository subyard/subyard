package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

func integrationCatalogFixture(t *testing.T, declaration string) string {
	t.Helper()
	root := syntheticResourceRoot(t)
	packageRoot := filepath.Join(root, "config/agents/sample")
	if err := os.MkdirAll(packageRoot, 0755); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(packageRoot, "integration.json"), []byte(declaration), 0644)
	testkit.WriteFile(t, filepath.Join(packageRoot, "lifecycle.sh"), []byte("#!/bin/sh\nexit 0\n"), 0755)
	return root
}

func TestIntegrationCatalogImmutableInputsAndDerivedPort(t *testing.T) {
	root := integrationCatalogFixture(t, `{"schema_version":1,"command":"sample","inputs":[{"name":"SAMPLE_VERSION","type":"version","value":"1.2.3"}],"settings":[{"name":"SAMPLE_PORT","type":"port","scopes":["shipped","host","yard","command"],"application":"yard-init","minimum":1024,"maximum":65535,"optional":true,"host_listener":true,"default_from":{"setting":"SSH_PORT","offset":20000}}]}`)
	for _, test := range []struct{ ssh, want string }{{"2222", "22222"}, {"60000", "15488"}} {
		loaded, err := Load(LoadOptions{RepositoryRoot: root, OperatorHome: testkit.TempDir(t), Environment: map[string]string{"SSH_PORT": test.ssh, "CODING_TOOL_INTEGRATIONS": "sample", "SAMPLE_VERSION": "9.9.9"}, DisablePrivate: true})
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Environment["SAMPLE_VERSION"] != "1.2.3" || loaded.Environment["SAMPLE_PORT"] != test.want {
			t.Fatalf("input authority or derivation failed")
		}
		assertEffectiveSetting(t, loaded.Settings["SAMPLE_PORT"], test.want, "default", "shipped defaults", filepath.Join(root, "config/agents/sample/integration.json"))
		for _, scope := range []SettingScope{ScopeHost, ScopeYard, ScopeCommand} {
			if err := loaded.Catalog.ValidateSetting(scope, "SAMPLE_VERSION", "9.9.9", false); err == nil {
				t.Fatalf("immutable input accepted an override in %s", scope)
			}
		}
	}
}

func TestIntegrationCatalogFailsClosed(t *testing.T) {
	for _, declaration := range []string{
		`{"schema_version":3}`, `{"schema_version":1,"unknown":true}`,
		`{"schema_version":1,"lifecycle":{"handler":"../lifecycle.sh","source_files":[],"instance_config_keys":[],"instance_devices":[]}}`,
		`{"schema_version":1,"inputs":[{"name":"SSH_PORT","type":"string","value":"9999"}]}`,
		`{"schema_version":1,"settings":[{"name":"SAMPLE_PORT","type":"port","scopes":["shipped"],"application":"yard-init","minimum":1024,"maximum":65535,"default_from":{"setting":"SAMPLE_PORT","offset":0}}]}`,
		`{"schema_version":1,"settings":[{"name":"SAMPLE_PORT","type":"port","scopes":["shipped"],"application":"yard-init","minimum":1024,"maximum":65535,"default_from":{"setting":"HOST_BASE","offset":0}}]}`,
	} {
		metadata, err := profile.DecodeIntegration("sample", []byte(declaration))
		if err == nil {
			_, err = BuildCatalog(nil, []profile.IntegrationMetadata{metadata})
		}
		if err == nil {
			t.Fatalf("invalid declaration accepted: %s", declaration)
		}
	}
	root := integrationCatalogFixture(t, `{"schema_version":1,"lifecycle":{"handler":"lifecycle.sh","source_files":[],"instance_config_keys":[],"instance_devices":[]}}`)
	path := filepath.Join(root, "config/agents/sample/lifecycle.sh")
	if err := os.Rename(path, path+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("lifecycle.sh.real", path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCatalog(root); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatal("symbolic handler was accepted")
	}
}

func TestIntegrationImmutableInputsIgnoreConfigDirectoryDefaults(t *testing.T) {
	root := integrationCatalogFixture(t, `{"schema_version":1,"inputs":[{"name":"SAMPLE_VERSION","type":"version","value":"1.2.3"}]}`)
	configDir := testkit.TempDir(t)
	for _, name := range []string{"incus.project.env", "subyard.env", "host.env", "agents.env", "ports.env"} {
		defaults, err := os.ReadFile(filepath.Join(root, "config", name))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(configDir, name), append(defaults, []byte("SAMPLE_VERSION=9.9.9\n")...), 0600)
	}
	if err := os.MkdirAll(filepath.Join(configDir, "yards/profiles"), 0755); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(configDir, "yards/profiles/fixture.env"), []byte("SAMPLE_VERSION=7.7.7\n"), 0600)
	loaded, err := Load(LoadOptions{RepositoryRoot: root, OperatorHome: testkit.TempDir(t), Environment: map[string]string{"SUBYARD_CONFIG_DIR": configDir, "SAMPLE_VERSION": "8.8.8", "YARD_TEMPLATE": "fixture"}, DisablePrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Environment["SAMPLE_VERSION"] != "1.2.3" {
		t.Fatal("config-directory defaults replaced immutable package input")
	}
	assertEffectiveSetting(t, loaded.Settings["SAMPLE_VERSION"], "1.2.3", "default", "shipped defaults", filepath.Join(root, "config/agents/sample/integration.json"))
	configHome := testkit.TempDir(t)
	testkit.WriteFile(t, filepath.Join(configHome, "config.env"), []byte("SAMPLE_VERSION=9.9.9\n"), 0600)
	if _, err := Load(LoadOptions{RepositoryRoot: root, OperatorHome: testkit.TempDir(t), Environment: map[string]string{"SUBYARD_CONFIG_DIR": configDir, "SUBYARD_CONFIG_HOME": configHome}, DisablePrivate: true}); err == nil {
		t.Fatal("persistent override of immutable input was accepted")
	}
}

func TestRetiredIntegrationInputsPreserveSelectionAuthority(t *testing.T) {
	root := integrationCatalogFixture(t, `{"schema_version":1,"command":"sample"}`)
	retired := filepath.Join(root, "config/retired-integrations/retired-widget")
	if err := os.MkdirAll(retired, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(retired, "integration.json"), []byte(`{"schema_version":1,"retired":true,"retired_settings":["OBSOLETE_PORT"],"lifecycle":{"handler":"retire.sh"}}`), 0o600)
	testkit.WriteFile(t, filepath.Join(retired, "retire.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o700)
	for _, selection := range []string{"sample retired-widget", "retired-widget", ""} {
		home, configHome := testkit.TempDir(t), testkit.TempDir(t)
		original := "CODING_TOOL_INTEGRATIONS=\"" + selection + "\"\nOBSOLETE_PORT=${UNUSED_PORT:=12345}\n"
		path := filepath.Join(configHome, "config.env")
		testkit.WriteFile(t, path, []byte(original), 0o600)
		loaded, err := Load(LoadOptions{RepositoryRoot: root, OperatorHome: home, DisablePrivate: true, Environment: map[string]string{"SUBYARD_CONFIG_HOME": configHome}})
		if err != nil {
			t.Fatal(err)
		}
		expected := ""
		if selection == "sample retired-widget" {
			expected = "sample"
		}
		if loaded.Environment["CODING_TOOL_INTEGRATIONS"] != expected || !loaded.Integrations.Present {
			t.Fatalf("selection lost: %#v", loaded.Integrations)
		}
		for _, allowed := range []bool{true, false} {
			snapshot := loaded
			snapshot.Integrations.AllowsCodingTools = allowed
			if !allowed {
				snapshot.Integrations.Effective = nil
			}
			inventory := snapshot.IntegrationInventory()
			if len(inventory) != 2 || inventory[0].ID != "retired-widget" || inventory[1].ID != "sample" {
				t.Fatalf("catalog entries lost: %#v", inventory)
			}
			entry := inventory[0]
			if entry.Requested || entry.Effective || entry.Eligible || entry.Reason != "retired" {
				t.Fatalf("retired inventory entry: %#v", entry)
			}
			entry = inventory[1]
			if entry.Eligible != allowed || entry.Requested != (expected == "sample") || entry.Effective != (allowed && expected == "sample") || (!allowed && entry.Reason != "role-restricted") {
				t.Fatalf("live inventory selection or role changed: %#v", entry)
			}
			for _, entry := range inventory {
				if !entry.Present || entry.SourceGroup != "local overrides" || entry.Source.Scope != "host" || entry.Source.Path != path || entry.Source.Value != selection {
					t.Fatalf("inventory source authority lost: %#v", entry)
				}
			}
		}
		if _, ok := loaded.Environment["OBSOLETE_PORT"]; ok {
			t.Fatal("retired setting remains effective")
		}
		if _, ok := loaded.Environment["UNUSED_PORT"]; ok {
			t.Fatal("retired expression mutated live environment")
		}
		data, _ := os.ReadFile(path)
		if string(data) != original {
			t.Fatal("read modified source-owned persisted inputs")
		}
		if _, err := loaded.Catalog.LookupSetting("OBSOLETE_PORT"); err {
			t.Fatal("retired setting is writable")
		}
	}
	if _, err := Load(LoadOptions{RepositoryRoot: root, OperatorHome: testkit.TempDir(t), DisablePrivate: true, Environment: map[string]string{"CODING_TOOL_INTEGRATIONS": "unknown"}}); err == nil {
		t.Fatal("arbitrary unknown integration accepted")
	}
}

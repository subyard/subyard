package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestRemovedCleanupMetadataDoesNotDeclareAnIntegration(t *testing.T) {
	if _, err := ResolveIntegrationSelection(map[string]string{
		"AGENT_legacy_CLEANUP": "/opt/subyard/legacy-cleanup",
	}, []string{"legacy"}); err == nil {
		t.Fatal("cleanup-only metadata still declares an integration")
	}
}

func TestRetiredIntegrationSelectionIsRefused(t *testing.T) {
	root := integrationCatalogFixture(t, `{"schema_version":1,"command":"sample"}`)
	for _, id := range []string{"retired-one", "retired-two"} {
		retired := filepath.Join(root, "config/retired-integrations", id)
		if err := os.MkdirAll(retired, 0o700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(retired, "integration.json"), []byte(`{"schema_version":1,"retired":true,"retired_settings":["AGENT_`+id+`_COMMAND"],"lifecycle":{"handler":"retire.sh"}}`), 0o600)
		testkit.WriteFile(t, filepath.Join(retired, "retire.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o700)
	}
	catalog, err := LoadCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	if !catalog.RetiredIntegration("retired-one") || catalog.RetiredIntegration("sample") {
		t.Fatal("retirement classification changed")
	}
	if err := catalog.CheckSelectableIntegrations([]string{"sample"}); err != nil {
		t.Fatal(err)
	}
	err = catalog.CheckSelectableIntegrations([]string{"retired-one", "sample", "retired-two", "retired-one"})
	if err == nil || err.Error() != "integration retired-one, retired-two is retired and can no longer be selected" {
		t.Fatalf("retired selection refusal = %v", err)
	}
}

func TestIntegrationSelectionKeepsRequestsAndPresence(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	for _, test := range []struct{ name, content, requested, effective string }{
		{"dependency", "CODING_TOOL_INTEGRATIONS=relay\nAGENT_relay_COMMAND=relay\nAGENT_relay_DEPENDS=codex\n", "relay", "codex relay"},
		{"empty", "CODING_TOOL_INTEGRATIONS=\n", "", ""},
		{"legacy empty", "AGENTS=none\n", "", ""},
		{"legacy equal empty", "AGENTS=none\nCODING_TOOL_INTEGRATIONS=\n", "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, ".config/subyard/yards/default/config.env")
			writeFixture(t, path, test.content)
			loaded, err := Load(LoadOptions{RepositoryRoot: root, OperatorHome: home, DisablePrivate: true, Environment: map[string]string{"SUBYARD_CONFIG_HOME": filepath.Join(home, ".config/subyard"), "DEV_UID": "1000", "SSH_PORT": "2222", "FORWARD_SSH_AGENT": "0", "DEV_SUDO": "0", "HOST_BASE": filepath.Join(home, "host"), "RESTRICTED_DISK_PATHS": filepath.Join(home, "host"), "STORAGE_PATH": filepath.Join(home, "storage"), "SHIFT_MODE": "shift"}})
			if err != nil {
				t.Fatal(err)
			}
			selection := loaded.Integrations
			if !selection.Present || strings.Join(selection.Requested, " ") != test.requested || strings.Join(selection.Effective, " ") != test.effective || selection.Provenance.Scope != "yard" || selection.Provenance.Path != path {
				t.Fatalf("selection = %#v", selection)
			}
			if test.name == "dependency" && strings.Join(selection.DependencyReasons["codex"], " ") != "relay" {
				t.Fatalf("dependency reason = %#v", selection.DependencyReasons)
			}
			if got := loaded.Settings["CODING_TOOL_INTEGRATIONS"].Resolutions; len(got) == 0 {
				t.Fatal("missing provenance")
			}
		})
	}
	home := t.TempDir()
	loaded, err := Load(LoadOptions{RepositoryRoot: home, OperatorHome: home, DisablePrivate: true, Environment: map[string]string{"SUBYARD_CONFIG_HOME": filepath.Join(home, ".config/subyard"), "DEV_UID": "1000", "SSH_PORT": "2222", "FORWARD_SSH_AGENT": "0", "DEV_SUDO": "0", "HOST_BASE": filepath.Join(home, "host"), "RESTRICTED_DISK_PATHS": filepath.Join(home, "host"), "STORAGE_PATH": filepath.Join(home, "storage"), "SHIFT_MODE": "shift"}})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Integrations.Present || len(loaded.Integrations.Requested) != 0 {
		t.Fatalf("absent selection = %#v", loaded.Integrations)
	}
}

func TestTestRoleIntegrationPolicy(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	for _, test := range []struct {
		name, yard, content string
		command             map[string]string
		forbidden, allowed  bool
	}{
		{name: "inherited suppressed", yard: "ordinary", content: "YARD_TEMPLATE=test-vms\n"},
		{name: "explicit empty", yard: "ordinary", content: "YARD_TEMPLATE=test-vms\nCODING_TOOL_INTEGRATIONS=\n"},
		{name: "explicit rejected", yard: "ordinary", content: "YARD_TEMPLATE=test-vms\nCODING_TOOL_INTEGRATIONS=codex\n", forbidden: true},
		{name: "command rejected", yard: "ordinary", content: "YARD_TEMPLATE=test-vms\n", command: map[string]string{"CODING_TOOL_INTEGRATIONS": "codex"}, forbidden: true},
		{name: "policy override rejected", yard: "ordinary", content: "YARD_TEMPLATE=test-vms\nALLOWS_CODING_TOOLS=true\n", forbidden: true},
		{name: "name is not role", yard: "test-vms", content: "NESTED_E2E_VMS=0\n", allowed: true},
		{name: "privilege is not role", yard: "ordinary", content: "NESTED_E2E_VMS=1\n", allowed: true},
		{name: "default role parity", yard: "default", content: "YARD_TEMPLATE=test-vms\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			writeFixture(t, filepath.Join(home, ".config/subyard/yards", test.yard, "config.env"), test.content)
			command := cloneStringMap(test.command)
			command["SUBYARD_CONFIG_HOME"] = filepath.Join(home, ".config/subyard")
			loaded, err := Load(LoadOptions{RepositoryRoot: root, OperatorHome: home, YardName: test.yard, DisablePrivate: true, Environment: command})
			if test.forbidden {
				if err == nil {
					t.Fatal("forbidden selection accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Integrations.AllowsCodingTools != test.allowed {
				t.Fatalf("role = %#v", loaded.Integrations)
			}
			if !test.allowed && (len(loaded.Integrations.Effective) != 0 || loaded.Environment["HOST_LINKS"] != "") {
				t.Fatalf("role retained tools or links: %#v", loaded.Integrations)
			}
		})
	}
}

package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestIntegrationSelectionKeepsRequestsAndPresence(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	for _, test := range []struct{ name, content, requested, effective string }{
		{"dependency", "CODING_TOOL_INTEGRATIONS=paseo\n", "paseo", "codex paseo"},
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
			if test.name == "dependency" && strings.Join(selection.DependencyReasons["codex"], " ") != "paseo" {
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

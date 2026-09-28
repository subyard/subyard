package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestAmneziaDedicatedPreset(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	preset, err := os.ReadFile(filepath.Join(root, "config/profiles/amnezia/yard.env"))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, yard, content string
		command             map[string]string
		wantError           bool
	}{
		{name: "explicit VM", yard: "vpn", content: string(preset)},
		{name: "name alone is dormant", yard: "amnezia", content: "SSH_PORT=2225\n"},
		{name: "default yard rejected", yard: "default", content: string(preset), wantError: true},
		{name: "container rejected", yard: "vpn", content: string(preset), command: map[string]string{"YARD_KIND": "container"}, wantError: true},
		{name: "agent rejected", yard: "vpn", content: string(preset), command: map[string]string{"CODING_TOOL_INTEGRATIONS": "codex"}, wantError: true},
		{name: "mount rejected", yard: "vpn", content: string(preset), command: map[string]string{"HOST_MOUNTS": "host-secret:/mnt/secret:ro:0700"}, wantError: true},
		{name: "forwarding rejected", yard: "vpn", content: string(preset), command: map[string]string{"FORWARD_SSH_AGENT": "1"}, wantError: true},
		{name: "mixed profiles rejected", yard: "vpn", content: string(preset), command: map[string]string{"ENVIRONMENT_PROFILES": "amnezia hermes"}, wantError: true},
		{name: "ordinary yard rejected", yard: "work", content: "ENVIRONMENT_PROFILES=amnezia\n", wantError: true},
		{name: "wildcard endpoint rejected", yard: "vpn", content: string(preset), command: map[string]string{"RESOURCE_VPN_IPV4": "0.0.0.0"}, wantError: true},
		{name: "disabled selection", yard: "vpn", content: string(preset), command: map[string]string{"ENVIRONMENT_PROFILES": ""}},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := testkit.TempDir(t)
			configHome := filepath.Join(home, "config")
			writeFixture(t, filepath.Join(configHome, "yards", test.yard, "config.env"), test.content)
			command := cloneStringMap(test.command)
			command["SUBYARD_CONFIG_HOME"] = configHome
			loaded, err := Load(LoadOptions{RepositoryRoot: root, OperatorHome: home, YardName: test.yard, DisablePrivate: true, Environment: command})
			if test.wantError {
				if err == nil {
					t.Fatal("unsafe VPN selection accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.name == "name alone is dormant" {
				if strings.Contains(loaded.Environment["ENVIRONMENT_PROFILES"], "amnezia") || loaded.Context.YardKind != domain.YardContainer {
					t.Fatal("yard name enabled VPN")
				}
				return
			}
			if loaded.Integrations.AllowsCodingTools || len(loaded.Integrations.Effective) != 0 || loaded.Context.YardKind != domain.YardVM {
				t.Fatal("VPN role lost its isolation")
			}
		})
	}
}

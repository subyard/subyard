package tests

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/testkit"
)

// The shipped client profile is opt-in for any yard, including the Hermes preset
// without coding-tool integrations, and refuses yard roles without host access.
func TestClientProfileSelectionFollowsHostAccess(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", "..", "..", ".."))
	read := func(path string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	hermes := read("config/profiles/hermes/yard.env")
	amnezia := read("config/profiles/amnezia/yard.env")
	for _, test := range []struct {
		name, yard, content string
		command             map[string]string
		selected            bool
		refusals            []string
	}{
		{name: "not selected by default", yard: "work", content: "SSH_PORT=2230\n"},
		{name: "ordinary yard", yard: "work", content: "ENVIRONMENT_PROFILES=test-vms-client\n", selected: true},
		{name: "hermes without integrations", yard: "hermes", content: hermes,
			command: map[string]string{"ENVIRONMENT_PROFILES": "hermes github test-vms-client"}, selected: true},
		{name: "deselected again", yard: "hermes", content: hermes},
		// The only shipped role without host access also permits one exclusive profile.
		{name: "role without host access", yard: "vpn", content: amnezia,
			command:  map[string]string{"ENVIRONMENT_PROFILES": "amnezia test-vms-client"},
			refusals: []string{`profile "test-vms-client" requires host access`, `permits only environment profile "amnezia"`}},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := testkit.TempDir(t)
			configHome := filepath.Join(home, ".config", "subyard")
			path := filepath.Join(configHome, "yards", test.yard, "config.env")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, path, []byte(test.content), 0o600)
			environment := map[string]string{"SUBYARD_OPERATOR_HOME": home, "SUBYARD_CONFIG_HOME": configHome, "SUBYARD_HOME": filepath.Join(home, ".subyard")}
			for key, value := range test.command {
				environment[key] = value
			}
			loaded, err := config.Load(config.LoadOptions{RepositoryRoot: root, OperatorHome: home, YardName: test.yard, DisablePrivate: true, Environment: environment})
			if len(test.refusals) != 0 {
				if err == nil {
					t.Fatal("yard role without host access accepted the client profile")
				}
				for _, want := range test.refusals {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("refusal %q lacks %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			selected := slices.Contains(strings.Fields(loaded.Environment["ENVIRONMENT_PROFILES"]), "test-vms-client")
			if selected != test.selected {
				t.Fatalf("test-vms-client selected=%v, want %v (%q)", selected, test.selected, loaded.Environment["ENVIRONMENT_PROFILES"])
			}
			if test.yard == "hermes" && len(loaded.Integrations.Effective) != 0 {
				t.Fatalf("hermes preset gained coding-tool integrations: %v", loaded.Integrations.Effective)
			}
		})
	}
}

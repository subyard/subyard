package tests

import (
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestOpenLITIsExplicitlySelectedAndPreservesAgentDefaults(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", "..", "..", ".."))
	definitions, err := profile.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, definition := range definitions {
		if definition.Name != "openlit" {
			continue
		}
		found = true
		if definition.Selected("default", nil) || definition.Selected("demo", nil) ||
			definition.Selected("default", map[string]string{"ENVIRONMENT_PROFILES": ""}) ||
			!definition.Selected("default", map[string]string{"ENVIRONMENT_PROFILES": "openlit"}) {
			t.Fatal("OpenLIT selection must remain explicit")
		}
	}
	if !found {
		t.Fatal("OpenLIT profile is missing")
	}
	home := testkit.TempDir(t)
	for _, selected := range []string{"", "openlit"} {
		loaded, err := config.Load(config.LoadOptions{RepositoryRoot: root, OperatorHome: home, YardName: "default", DisablePrivate: true, Environment: map[string]string{"ENVIRONMENT_PROFILES": selected}})
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Environment["CODING_TOOL_INTEGRATIONS"] != "claude codex opencode pi aiobserver" {
			t.Fatal("optional telemetry changed ordinary coding integration defaults")
		}
	}
}

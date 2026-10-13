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
		if loaded.Environment["CODING_TOOL_INTEGRATIONS"] != "claude codex opencode pi" {
			t.Fatal("optional telemetry changed ordinary coding integration defaults")
		}
	}
}

func TestOpenLITDashboardSettingDerivationAndEnableCollision(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", "..", "..", ".."))
	for _, test := range []struct{ ssh, port string }{{"2222", "22222"}, {"60000", "15488"}} {
		loaded, err := config.Load(config.LoadOptions{RepositoryRoot: root, OperatorHome: testkit.TempDir(t), DisablePrivate: true, Environment: map[string]string{"ENVIRONMENT_PROFILES": "", "SSH_PORT": test.ssh}})
		if err != nil || loaded.Environment["OPENLIT_HOST_PORT"] != test.port {
			t.Fatalf("derived listener: %s, %v", loaded.Environment["OPENLIT_HOST_PORT"], err)
		}
	}
	loaded, err := config.Load(config.LoadOptions{RepositoryRoot: root, OperatorHome: testkit.TempDir(t), DisablePrivate: true, Environment: map[string]string{"ENVIRONMENT_PROFILES": "", "SSH_PORT": "2222", "OPENLIT_HOST_PORT": "2222"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.WithEnvironmentProfiles(loaded, []string{"openlit"}); err == nil {
		t.Fatal("enable accepted dashboard/SSH collision")
	}
}

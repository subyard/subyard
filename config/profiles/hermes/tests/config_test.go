package tests

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestHermesYardFileClearsInheritedHostAndCapabilityWiring(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", "..", "..", ".."))
	home := testkit.TempDir(t)
	configHome := filepath.Join(home, ".config", "subyard")
	write := func(path, content string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, []byte(content), 0o600)
	}
	write(filepath.Join(configHome, "config.env"), "CODING_TOOL_INTEGRATIONS=\"claude opencode pi\"\nENVIRONMENT_PROFILES=openclaw\nHOST_CLAUDE_MD=/tmp/CLAUDE.md\nHOST_CODEX_AGENTS_MD=/tmp/CODEX.md\nHOST_OPENCODE_AGENTS_MD=/tmp/OPENCODE.md\nHOST_MOUNTS=host-cache:/mnt/cache:ro:0755\nHOST_LINKS=.claude/sessions:/mnt/host/agent-sessions/claude/sessions\nYARD_CAPABILITIES=android\nYARD_CAPS=fuse\nYARD_DEVICES=gpu\nYARD_MOUNTS=cache:/srv/cache:rw:0755\nFORWARD_SSH_AGENT=1\nDEV_SUDO=1\nNESTED_E2E_VMS=1\n")
	write(filepath.Join(configHome, "yards", "hermes", "config.env"), "SSH_PORT=2224\nENVIRONMENT_PROFILES=hermes\nCODING_TOOL_INTEGRATIONS=\nHOST_CLAUDE_MD=\nHOST_CODEX_AGENTS_MD=\nHOST_OPENCODE_AGENTS_MD=\nHOST_MOUNTS=\nHOST_LINKS=\nYARD_CAPABILITIES=\nYARD_CAPS=\nYARD_DEVICES=\nYARD_MOUNTS=\nFORWARD_SSH_AGENT=0\nDEV_SUDO=0\nNESTED_E2E_VMS=0\n")
	loaded, err := config.Load(config.LoadOptions{RepositoryRoot: root, OperatorHome: home, YardName: "hermes", DisablePrivate: true, Environment: map[string]string{"SUBYARD_OPERATOR_HOME": home, "SUBYARD_CONFIG_HOME": configHome, "SUBYARD_HOME": filepath.Join(home, ".subyard")}})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"ENVIRONMENT_PROFILES": "hermes", "CODING_TOOL_INTEGRATIONS": "", "HOST_CLAUDE_MD": "", "HOST_CODEX_AGENTS_MD": "", "HOST_OPENCODE_AGENTS_MD": "", "HOST_MOUNTS": "", "HOST_LINKS": "", "YARD_CAPABILITIES": "", "YARD_CAPS": "", "YARD_DEVICES": "", "YARD_MOUNTS": ""} {
		if loaded.Environment[name] != want {
			t.Errorf("%s = %q, want %q", name, loaded.Environment[name], want)
		}
	}
	if loaded.Context.ForwardSSHAgent || loaded.Context.DevSudo || loaded.Context.NestedE2EVMs || loaded.Context.SSHPort != 2224 {
		t.Fatalf("Hermes security boundary drifted: %#v", loaded.Context)
	}
}

func TestHermesShippedSettings(t *testing.T) {
	catalog, err := config.LoadCatalog(filepath.Clean(filepath.Join("..", "..", "..", "..")))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"HERMES_DASHBOARD_ADVERTISE_HOST", "HERMES_DASHBOARD_HOST_PORT"} {
		definition, ok := catalog.LookupSetting(name)
		if !ok || !definition.Syncable || definition.Application != config.SettingNextCommand {
			t.Fatalf("%s = %#v", name, definition)
		}
	}
}

package tests

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	write(filepath.Join(configHome, "config.env"), "CODING_TOOL_INTEGRATIONS=\"claude opencode pi\"\nENVIRONMENT_PROFILES=openclaw\nHOST_CLAUDE_MD=/tmp/CLAUDE.md\nHOST_CODEX_AGENTS_MD=/tmp/CODEX.md\nHOST_OPENCODE_AGENTS_MD=/tmp/OPENCODE.md\nHOST_MOUNTS=host-cache:/mnt/cache:ro:0755\nHOST_LINKS=.claude/sessions:/mnt/host/agent-sessions/claude/sessions\nYARD_CAPABILITIES=android\nFORWARD_SSH_AGENT=1\nDEV_SUDO=1\nNESTED_E2E_VMS=1\n")
	preset, err := os.ReadFile(filepath.Join(root, "config", "profiles", "hermes", "yard.env"))
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(configHome, "yards", "hermes", "config.env"), string(preset))
	loaded, err := config.Load(config.LoadOptions{RepositoryRoot: root, OperatorHome: home, YardName: "hermes", DisablePrivate: true, Environment: map[string]string{"SUBYARD_OPERATOR_HOME": home, "SUBYARD_CONFIG_HOME": configHome, "SUBYARD_HOME": filepath.Join(home, ".subyard")}})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"ENVIRONMENT_PROFILES": "hermes github", "CODING_TOOL_INTEGRATIONS": "", "HOST_CLAUDE_MD": "", "HOST_CODEX_AGENTS_MD": "", "HOST_OPENCODE_AGENTS_MD": "", "HOST_MOUNTS": "", "HOST_LINKS": "", "YARD_CAPABILITIES": ""} {
		if loaded.Environment[name] != want {
			t.Errorf("%s = %q, want %q", name, loaded.Environment[name], want)
		}
	}
	if loaded.Context.ForwardSSHAgent || loaded.Context.DevSudo || loaded.Context.NestedE2EVMs || loaded.Context.SSHPort != 2224 {
		t.Fatalf("Hermes security boundary drifted: %#v", loaded.Context)
	}
	// Inherited operator extras are ignored and reported with their repair, never applied.
	host := filepath.Join(configHome, "config.env")
	settings, err := os.ReadFile(host)
	if err != nil {
		t.Fatal(err)
	}
	write(host, string(settings)+"YARD_CAPS=fuse\nYARD_DEVICES=gpu\nYARD_MOUNTS=cache:/srv/cache:rw:0755\n")
	loaded, err = config.Load(config.LoadOptions{RepositoryRoot: root, OperatorHome: home, YardName: "hermes", DisablePrivate: true, Environment: map[string]string{"SUBYARD_OPERATOR_HOME": home, "SUBYARD_CONFIG_HOME": configHome, "SUBYARD_HOME": filepath.Join(home, ".subyard")}})
	if err != nil {
		t.Fatalf("Hermes refused ignored operator extras: %v", err)
	}
	ignored := loaded.IgnoredAssignments()
	if len(ignored) != 3 {
		t.Fatalf("ignored assignments = %#v", ignored)
	}
	for line, name := range []string{"YARD_CAPS", "YARD_DEVICES", "YARD_MOUNTS"} {
		want := fmt.Sprintf("%s in %s:%d is ignored: only environment profiles declare it; remove it with yard -Y hermes config unset %s --scope host --local", name, host, strings.Count(string(settings), "\n")+line+1, name)
		if ignored[line].String() != want {
			t.Errorf("observation %q, want %q", ignored[line], want)
		}
		if loaded.Environment[name] != "" {
			t.Errorf("ignored %s became effective: %q", name, loaded.Environment[name])
		}
	}
	if after, readErr := os.ReadFile(host); readErr != nil || !strings.HasSuffix(string(after), "YARD_MOUNTS=cache:/srv/cache:rw:0755\n") {
		t.Fatalf("loading cleaned the operator file: %q %v", after, readErr)
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

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/profile"
)

func TestMaterializationUsesApprovalVariantForShippedSource(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "base.json")
	git := filepath.Join(root, "git.json")
	sudo := filepath.Join(root, "sudo.json")
	both := filepath.Join(root, "git-sudo.json")
	definition := profile.IntegrationDefinition{Name: "sample", Root: root, Artifacts: []profile.IntegrationArtifact{{
		ID: "config", Kind: "config", Source: "base.json",
		ApprovalSources: map[string]string{"git": "git.json", "sudo": "sudo.json", "git sudo": "git-sudo.json"},
		Destination:     ".sample/config.json", Format: "destination",
	}}}
	for _, scope := range []MaterializationScope{MaterializationConfigAssets, MaterializationRuntime} {
		values := map[string]string{"CODING_TOOL_INTEGRATIONS": "sample"}
		for _, test := range []struct{ git, sudo, source, want string }{
			{"", "", base, sudo}, {"prompt", "", base, both}, {"off", "yes", base, base}, {"", "yes", base, base},
			{"off", "no", base, base}, {"off", "prompt", base, sudo}, {"prompt", "prompt", base, both},
			{"prompt", "no", base, git}, {"prompt", "prompt", "/override.json", "/override.json"}, {"off", "prompt", "", ""},
		} {
			values["CODING_TOOL_GIT_APPROVAL"] = test.git
			values["DEV_SUDO"] = test.sudo
			values["AGENT_sample_CONFIG"] = test.source
			sources, err := SelectMaterializationSources(values, "dev", scope, definition)
			if err != nil {
				t.Fatal(err)
			}
			if test.want == "" && len(sources) == 0 {
				continue
			}
			if len(sources) != 1 || sources[0].Asset.Source != test.want || sources[0].Asset.Destination != "/home/dev/.sample/config.json" {
				t.Fatalf("%s Git %q sudo %q with source %q: got %#v, want %q at the same destination", scope, test.git, test.sudo, test.source, sources, test.want)
			}
		}
	}
}

func TestMaterializedAssetsSelectOwnedFormatByDestinationAndKind(t *testing.T) {
	assets, err := MaterializedAssets(map[string]string{
		"CODING_TOOL_INTEGRATIONS":   "claude opencode codex",
		"AGENT_claude_CONFIG":        "/imported/renamed",
		"AGENT_claude_CONFIG_DEST":   ".claude/settings.json",
		"AGENT_claude_RULES":         "/imported/rules.json",
		"AGENT_claude_RULES_DEST":    ".claude/rules.json",
		"AGENT_opencode_CONFIG":      "/imported/config.json",
		"AGENT_opencode_CONFIG_DEST": ".config/opencode/opencode.jsonc",
		"AGENT_codex_CONFIG":         "/imported/config.json",
		"AGENT_codex_CONFIG_DEST":    ".codex/config.toml",
	}, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 4 {
		t.Fatalf("assets=%d", len(assets))
	}
	want := map[string]string{
		"AGENT_claude_CONFIG": "json",
		"AGENT_codex_CONFIG":  "toml",
	}
	for _, asset := range assets {
		if asset.OwnedFormat != want[asset.Setting] {
			t.Fatalf("policy for %s = %q, want %q", asset.Setting, asset.OwnedFormat, want[asset.Setting])
		}
	}
}

func TestMaterializedAssetsRejectInvalidIdentityAndDestination(t *testing.T) {
	for _, user := range []string{"../root", "dev"} {
		_, err := MaterializedAssets(map[string]string{
			"CODING_TOOL_INTEGRATIONS": "claude", "AGENT_claude_CONFIG": "/source",
			"AGENT_claude_CONFIG_DEST": "../outside.json",
		}, user)
		if err == nil {
			t.Fatal("accepted invalid identity or destination")
		}
	}
}

func TestMaterializedAssetReadRejectsSymlinkConfig(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "source")
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	asset := MaterializedAsset{Source: link}
	if _, err := asset.ReadSource(); err == nil {
		t.Fatal("followed config symlink")
	}
	asset.FollowSymlinks = true
	if _, err := asset.ReadSource(); err != nil {
		t.Fatal(err)
	}
}

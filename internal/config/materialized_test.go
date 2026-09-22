package config

import (
	"os"
	"path/filepath"
	"testing"
)

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

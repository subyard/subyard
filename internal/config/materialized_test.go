package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestMaterializationInstallsIndependentApprovalOverlays(t *testing.T) {
	root := testkit.TempDir(t)
	const (
		base = "base.json"
		git  = "git.json"
		sudo = "sudo.json"
	)
	files := map[string]string{
		base: `{"setting":{"ordinary":"allow"}}`,
		git:  `{"setting":{"git":"ask"}}`,
		sudo: `{"setting":{"sudo":"ask"}}`,
	}
	definition := profile.IntegrationDefinition{Name: "sample", Root: root, Artifacts: []profile.IntegrationArtifact{{
		ID: "config", Kind: "config", Source: base,
		ApprovalSources: map[string]string{"git": git, "sudo": sudo},
		Destination:     ".sample/config.json", Format: "destination",
	}}}
	for _, scope := range []MaterializationScope{MaterializationConfigAssets, MaterializationRuntime} {
		for _, test := range []struct{ setting, value, sidecar string }{
			{"CODING_TOOL_GIT_APPROVAL", "prompt", git},
			{"DEV_SUDO", "prompt", sudo},
			{"DEV_SUDO", "", sudo},
			{"DEV_SUDO", "no", ""},
		} {
			values := map[string]string{"CODING_TOOL_INTEGRATIONS": "sample", "DEV_SUDO": "yes"}
			values[test.setting] = test.value
			sources, err := SelectMaterializationSources(values, "dev", scope, definition)
			if err != nil || len(sources) == 0 {
				t.Fatalf("missing config: %#v, %v", sources, err)
			}
			materialized := sources[len(sources)-1].Asset
			if materialized.Source != filepath.Join(root, base) || materialized.Destination != "/home/dev/.sample/config.json" {
				t.Fatalf("base config has changed: %#v", materialized)
			}
			if test.sidecar == "" {
				if len(sources) != 1 || len(materialized.ApprovalSources) != 0 {
					t.Fatalf("unexpected overlay: %#v", sources)
				}
				continue
			}
			sidecar := sources[0].Asset
			if len(sources) != 2 || sidecar.Destination != filepath.Join("/home/dev/.sample", test.sidecar) ||
				!slices.Equal(materialized.ApprovalSources, []string{sidecar.Source}) {
				t.Fatalf("expected one independent overlay: %#v", sources)
			}
			content, err := materialized.ReadContent(func(asset MaterializedAsset) ([]byte, error) {
				return []byte(files[filepath.Base(asset.Source)]), nil
			})
			gate := strings.TrimSuffix(test.sidecar, ".json")
			if err != nil || !strings.Contains(string(content), `"`+gate+`": "ask"`) || !strings.Contains(string(content), `"ordinary": "allow"`) {
				t.Fatalf("wrong native policy: %s, %v", content, err)
			}

			values["AGENT_sample_CONFIG"] = "/operator/config.json"
			sources, err = SelectMaterializationSources(values, "dev", scope, definition)
			if err != nil || len(sources) != 1 || len(sources[0].Asset.ApprovalSources) != 0 {
				t.Fatalf("custom config gained a shipped policy: %#v, %v", sources, err)
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
	root := testkit.TempDir(t)
	path := filepath.Join(root, "source")
	testkit.WriteFile(t, path, []byte("{}"), 0600)
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

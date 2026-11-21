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

func TestMaterializationComposesIndependentApprovalOverlays(t *testing.T) {
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
		for _, test := range []struct{ setting, value, overlay string }{
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
			materialized := sources[0].Asset
			if materialized.Source != filepath.Join(root, base) || materialized.Destination != "/home/dev/.sample/config.json" {
				t.Fatalf("base config has changed: %#v", materialized)
			}
			if test.overlay == "" {
				if len(sources) != 1 || len(materialized.ApprovalSources) != 0 {
					t.Fatalf("unexpected overlay: %#v", sources)
				}
				continue
			}
			if len(sources) != 1 || !slices.Equal(materialized.ApprovalSources, []string{filepath.Join(root, test.overlay)}) {
				t.Fatalf("expected only the composed config: %#v", sources)
			}
			content, err := materialized.ReadContent(func(asset MaterializedAsset) ([]byte, error) {
				return []byte(files[filepath.Base(asset.Source)]), nil
			})
			gate := strings.TrimSuffix(test.overlay, ".json")
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

// The composed OpenCode policy keeps the bytes of the former per-variant files,
// so an upgrade leaves existing yards without a pending refresh.
func TestOpenCodeApprovalCompositionMatchesFormerVariants(t *testing.T) {
	shipped := filepath.Join("..", "..", "config", "agents", "opencode")
	git := filepath.Join(shipped, "50-subyard-git-approval.jsonc")
	sudo := filepath.Join(shipped, "51-subyard-sudo-approval.jsonc")
	for _, test := range []struct {
		variant  string
		overlays []string
	}{
		{"opencode.jsonc", nil},
		{"opencode-git.jsonc", []string{git}},
		{"opencode-sudo.jsonc", []string{sudo}},
		{"opencode-git-sudo.jsonc", []string{git, sudo}},
	} {
		asset := MaterializedAsset{Source: filepath.Join(shipped, "opencode.jsonc"), ApprovalSources: test.overlays}
		got, err := asset.ReadContent(MaterializedAsset.ReadSource)
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join("testdata", "opencode-variants", test.variant))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("%s changed:\n%s\nwant:\n%s", test.variant, got, want)
		}
	}
}

func TestApprovalCompositionRejectsComments(t *testing.T) {
	files := map[string]string{
		"base":    "{\"a\": {\"b\": 1}}",
		"comment": "{\n  // ask\n  \"a\": {\"c\": 2}\n}",
	}
	asset := MaterializedAsset{Source: "base", ApprovalSources: []string{"comment"}}
	_, err := asset.ReadContent(func(asset MaterializedAsset) ([]byte, error) { return []byte(files[asset.Source]), nil })
	if err == nil || !strings.Contains(err.Error(), "approval overlay comment") || !strings.Contains(err.Error(), "comments") {
		t.Fatalf("comment was not rejected clearly: %v", err)
	}
	files["comment"] = "{\"a\": {\"c\": [1, {\"d\": true}]}, \"e\": \"<&>\"}"
	got, err := asset.ReadContent(func(asset MaterializedAsset) ([]byte, error) { return []byte(files[asset.Source]), nil })
	want := "{\n  \"a\": {\n    \"b\": 1,\n    \"c\": [\n      1,\n      {\n        \"d\": true\n      }\n    ]\n  },\n  \"e\": \"<&>\"\n}\n"
	if err != nil || string(got) != want {
		t.Fatalf("got %q, %v; want %q", got, err, want)
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

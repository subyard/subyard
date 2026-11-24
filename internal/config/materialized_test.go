package config

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

// These are the unmodified declaration and policies from the published 0.31.0
// linux-amd64 bundle (SHA-256 fd0504fff2cd4155fc65cae8291f2f8fbd3e5a0aff645fed971afce8b3f3365f).
// Current code reads target declarations while preparing rollback, even when
// the integration is not selected. The target's variants replace the base.
func TestPublishedOpenCodeApprovalVariants(t *testing.T) {
	root := syntheticResourceRoot(t)
	packageRoot := filepath.Join(root, "config/agents/opencode")
	if err := os.MkdirAll(packageRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"v0.31.0-integration.json", "opencode.jsonc", "opencode-git.jsonc", "opencode-sudo.jsonc", "opencode-git-sudo.jsonc"} {
		content, err := os.ReadFile(filepath.Join("testdata/opencode-variants", name))
		if err != nil {
			t.Fatal(err)
		}
		if name == "v0.31.0-integration.json" {
			name = "integration.json"
		}
		testkit.WriteFile(t, filepath.Join(packageRoot, name), content, 0o644)
	}
	testkit.WriteFile(t, filepath.Join(packageRoot, "provision.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	for _, test := range []struct{ git, sudo, variant string }{
		{"off", "yes", "opencode.jsonc"},
		{"prompt", "yes", "opencode-git.jsonc"},
		{"off", "prompt", "opencode-sudo.jsonc"},
		{"prompt", "prompt", "opencode-git-sudo.jsonc"},
	} {
		t.Run(test.variant+"/"+test.git+"/"+test.sudo, func(t *testing.T) {
			loaded, err := Load(LoadOptions{RepositoryRoot: root, OperatorHome: testkit.TempDir(t), Environment: map[string]string{
				"CODING_TOOL_INTEGRATIONS": "opencode", "CODING_TOOL_GIT_APPROVAL": test.git, "DEV_SUDO": test.sudo,
			}})
			if err != nil {
				t.Fatal(err)
			}
			for _, scope := range []MaterializationScope{MaterializationConfigAssets, MaterializationRuntime} {
				sources, err := SelectMaterializationSources(loaded.Environment, "dev", scope, loaded.Catalog.Integrations()...)
				sources = slices.DeleteFunc(sources, func(source MaterializationSource) bool { return source.PreviewInstructions })
				if err != nil || len(sources) != 1 {
					t.Fatalf("%s: sources=%#v, %v", scope, sources, err)
				}
				asset := sources[0].Asset
				if asset.Source != filepath.Join(packageRoot, test.variant) || len(asset.ApprovalSources) != 0 {
					t.Fatalf("%s: published variant was composed as overlays: %#v", scope, asset)
				}
				got, err := asset.ReadContent(func(source MaterializedAsset) ([]byte, error) { return source.ReadSource() })
				if err != nil {
					t.Fatal(err)
				}
				want, err := os.ReadFile(filepath.Join("testdata/opencode-variants", test.variant))
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("%s: published policy changed: %v", scope, err)
				}
				for _, override := range []string{"/operator/custom.jsonc", ""} {
					loaded.Environment["AGENT_opencode_CONFIG"] = override
					assets, err := MaterializedAssets(loaded.Environment, "dev", loaded.Catalog.Integrations()...)
					if err != nil {
						t.Fatal(err)
					}
					if override == "" {
						if len(assets) != 0 {
							t.Fatal("empty operator config was replaced by a published variant")
						}
					} else if len(assets) != 1 || assets[0].Source != override || len(assets[0].ApprovalSources) != 0 {
						t.Fatalf("operator config was replaced by a published variant: %#v", assets)
					}
				}
				loaded.Environment["AGENT_opencode_CONFIG"] = filepath.Join(packageRoot, "opencode.jsonc")
			}
		})
	}
	// Package declarations are read before selection during rollback admission.
	if _, err := Load(LoadOptions{RepositoryRoot: root, OperatorHome: testkit.TempDir(t), Environment: map[string]string{"CODING_TOOL_INTEGRATIONS": ""}}); err != nil {
		t.Fatal(err)
	}
}

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

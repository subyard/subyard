package config

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

const artifactDeclaration = `{"schema_version":2,"provision":"lifecycle.sh","command":"sample","check":"sample-check","persist":".sample/sessions:/mnt/host/agent-sessions/sample/sessions","settings":[{"name":"SAMPLE_INSTRUCTIONS","type":"absolute-path","optional":true,"scopes":["host","yard","command"],"application":"yard-init"}],"artifacts":[{"id":"config","kind":"config","source":"settings.jsonc","destination":".sample/settings.jsonc","format":"raw"},{"id":"instructions","kind":"instructions","source_setting":"SAMPLE_INSTRUCTIONS","destination":".sample/AGENTS.md","format":"raw"}]}`

func TestIntegrationArtifactsPackageLocalResolution(t *testing.T) {
	root := integrationCatalogFixture(t, artifactDeclaration)
	source := filepath.Join(root, "config/agents/sample/settings.jsonc")
	testkit.WriteFile(t, source, []byte("// raw JSONC\n{}\n"), 0644)
	instructions := filepath.Join(testkit.TempDir(t), "AGENTS.md")
	testkit.WriteFile(t, instructions, []byte("operator instructions\n"), 0600)
	loaded, err := Load(LoadOptions{RepositoryRoot: root, OperatorHome: testkit.TempDir(t), DisablePrivate: true, Environment: map[string]string{"CODING_TOOL_INTEGRATIONS": "sample", "SAMPLE_INSTRUCTIONS": instructions}})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Environment["AGENT_sample_CONFIG"] != source || loaded.Environment["AGENT_sample_COMMAND"] != "sample" || loaded.Environment["AGENT_sample_CHECK"] != "sample-check" || loaded.Environment["AGENT_sample_PERSIST"] == "" {
		t.Fatal("package projection lost config or mechanics")
	}
	for _, scope := range []MaterializationScope{MaterializationConfigAssets, MaterializationRuntime} {
		sources, err := SelectMaterializationSources(loaded.Environment, "dev", scope, loaded.Catalog.Integrations()...)
		if err != nil {
			t.Fatal(err)
		}
		want := 1
		if scope == MaterializationRuntime {
			want = 2
		}
		if len(sources) != want {
			t.Fatalf("%s: got %d sources", scope, len(sources))
		}
		for _, source := range sources {
			if source.Asset.OwnedFormat != "" {
				t.Fatal("raw JSONC became structured")
			}
			if source.PreviewInstructions && (!source.Asset.FollowSymlinks || source.Asset.Source != instructions) {
				t.Fatal("instructions lost typed source policy")
			}
		}
	}
	testkit.WriteFile(t, filepath.Join(root, "config/host.env"), []byte("ALLOWS_HOST_ACCESS=false\n"), 0600)
	for _, selection := range []string{"sample", ""} {
		_, err := Load(LoadOptions{RepositoryRoot: root, OperatorHome: testkit.TempDir(t), DisablePrivate: true, Environment: map[string]string{"CODING_TOOL_INTEGRATIONS": selection, "SAMPLE_INSTRUCTIONS": instructions}})
		if err == nil || !strings.Contains(err.Error(), "SAMPLE_INSTRUCTIONS") {
			t.Fatal("role allowed declared operator instructions", err)
		}
	}
	loaded.Environment["AGENT_sample_CONFIG"] = ""
	sources, err := SelectMaterializationSources(loaded.Environment, "dev", MaterializationConfigAssets, loaded.Catalog.Integrations()...)
	if err != nil || len(sources) != 0 {
		t.Fatal("explicit empty source materialized", err)
	}
	loaded.Environment["AGENT_sample_CONFIG"] = source
	loaded.Environment["AGENT_sample_CONFIG_DEST"] = ".sample/AGENTS.md"
	if _, err := SelectMaterializationSources(loaded.Environment, "dev", MaterializationRuntime, loaded.Catalog.Integrations()...); err == nil {
		t.Fatal("effective destination collision accepted")
	}
	binding, err := loaded.Catalog.Integrations()[0].SourceBinding()
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, source, []byte("// changed\n{}\n"), 0644)
	changed, err := loaded.Catalog.Integrations()[0].SourceBinding()
	if err != nil || binding == changed {
		t.Fatal("default source not bound", err)
	}
}

func TestIntegrationArtifactsRejectInvalidContracts(t *testing.T) {
	for _, declaration := range []string{
		strings.Replace(artifactDeclaration, `"schema_version":2`, `"schema_version":1`, 1),
		strings.Replace(artifactDeclaration, `"format":"raw"`, `"format":"yaml"`, 1),
		strings.Replace(artifactDeclaration, `"source":"settings.jsonc"`, `"source":"../settings.jsonc"`, 1),
		strings.Replace(artifactDeclaration, `"id":"instructions"`, `"id":"config"`, 1),
		strings.Replace(artifactDeclaration, `"destination":".sample/AGENTS.md"`, `"destination":".sample/settings.jsonc"`, 1),
		strings.Replace(artifactDeclaration, `"source_setting":"SAMPLE_INSTRUCTIONS"`, `"source_setting":"UNKNOWN_SOURCE"`, 1),
		strings.Replace(artifactDeclaration, `"type":"absolute-path"`, `"type":"string"`, 1),
		strings.Replace(artifactDeclaration, `"source":"settings.jsonc"`, `"unknown":"settings.jsonc"`, 1),
	} {
		metadata, err := profile.DecodeIntegration("sample", []byte(declaration))
		if err == nil {
			_, err = BuildCatalog(nil, []profile.IntegrationMetadata{metadata})
		}
		if err == nil {
			t.Fatal("invalid artifact accepted", declaration)
		}
	}
	for _, kind := range []string{"symlink", "hardlink", "writable", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			root := integrationCatalogFixture(t, artifactDeclaration)
			source := filepath.Join(root, "config/agents/sample/settings.jsonc")
			target := filepath.Join(testkit.TempDir(t), "outside")
			testkit.WriteFile(t, target, []byte("{}"), 0644)
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(target, source)
			case "hardlink":
				err = os.Link(target, source)
			case "writable":
				testkit.WriteFile(t, source, []byte("{}"), 0666)
			case "fifo":
				err = syscall.Mkfifo(source, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := LoadCatalog(root); err == nil {
				t.Fatal("unsafe package source accepted")
			}
		})
	}
}

func TestDeclaredArtifactsRejectOverlappingFiles(t *testing.T) {
	for _, pair := range [][2]string{{".sample/config", ".sample/config/AGENTS.md"}, {".sample/config/AGENTS.md", ".sample/config"}} {
		declaration := strings.Replace(artifactDeclaration, `.sample/settings.jsonc`, pair[0], 1)
		declaration = strings.Replace(declaration, `.sample/AGENTS.md`, pair[1], 1)
		if _, err := profile.DecodeIntegration("sample", []byte(declaration)); err == nil {
			t.Fatal("overlapping package files accepted", pair)
		}
		// Each package alone is valid; the combined operation must still refuse.
		first, err := profile.DecodeIntegration("first", []byte(`{"schema_version":2,"artifacts":[{"id":"config","kind":"config","source":"config","destination":"`+pair[0]+`","format":"raw"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		second, err := profile.DecodeIntegration("second", []byte(`{"schema_version":2,"artifacts":[{"id":"config","kind":"config","source":"config","destination":"`+pair[1]+`","format":"raw"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := BuildCatalog(nil, []profile.IntegrationMetadata{first, second}); err == nil {
			t.Fatal("cross-package overlapping files accepted", pair)
		}
		definition := profile.IntegrationDefinition{Name: "sample", Root: "/immutable/package", Artifacts: []profile.IntegrationArtifact{{ID: "config", Kind: "config", Source: "config", Destination: ".sample/default", Format: "raw"}, {ID: "instructions", Kind: "instructions", SourceSetting: "SAMPLE_INSTRUCTIONS", Destination: pair[1], Format: "raw"}}}
		if _, err := SelectMaterializationSources(map[string]string{"CODING_TOOL_INTEGRATIONS": "sample", "AGENT_sample_CONFIG_DEST": pair[0]}, "dev", MaterializationRuntime, definition); err == nil {
			t.Fatal("effective instruction/config overlap accepted", pair)
		}
		if _, err := MaterializedAssets(map[string]string{"CODING_TOOL_INTEGRATIONS": "first second", "AGENT_first_CONFIG": "/first", "AGENT_second_CONFIG": "/second", "AGENT_first_CONFIG_DEST": pair[0], "AGENT_second_CONFIG_DEST": pair[1]}, "dev"); err == nil {
			t.Fatal("effective cross-package config overlap accepted", pair)
		}
	}
}

func TestDeclaredArtifactsRejectManagedLinkCollisions(t *testing.T) {
	for _, destination := range []string{".sample/sessions", ".sample", ".sample/sessions/config"} {
		metadata, err := profile.DecodeIntegration("sample", []byte(`{"schema_version":2,"persist":".sample/sessions:/mnt/host/sessions","artifacts":[{"id":"config","kind":"config","source":"config","destination":"`+destination+`","format":"raw"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := BuildCatalog(nil, []profile.IntegrationMetadata{metadata}); err == nil {
			t.Fatal("package file/link overlap accepted", destination)
		}
	}
	first, err := profile.DecodeIntegration("sample", []byte(artifactDeclaration))
	if err != nil {
		t.Fatal(err)
	}
	for _, destination := range []string{".sample/settings.jsonc", ".sample/AGENTS.md/child", ".sample/sessions", ".sample/sessions/child"} {
		second, err := profile.DecodeIntegration("second", []byte(`{"schema_version":2,"persist":"`+destination+`:/mnt/host/other"}`))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := BuildCatalog(nil, []profile.IntegrationMetadata{first, second}); err == nil {
			t.Fatal("cross-package managed destination overlap accepted", destination)
		}
	}

	root := integrationCatalogFixture(t, artifactDeclaration)
	testkit.WriteFile(t, filepath.Join(root, "config/agents.env"), []byte("HOST_LINKS=\"\"\n"), 0644)
	testkit.WriteFile(t, filepath.Join(root, "config/agents/sample/settings.jsonc"), []byte("{}"), 0644)
	for _, overrides := range []map[string]string{
		{"AGENT_sample_CONFIG_DEST": ".sample/sessions"},
		{"AGENT_sample_PERSIST": ".sample/settings.jsonc:/mnt/host/sessions"},
		{"AGENT_sample_PERSIST": ".sample:/mnt/host/sessions"},
		{"AGENT_sample_PERSIST": ".sample/settings.jsonc/child:/mnt/host/sessions"},
		{"AGENT_sample_PERSIST": ".sample/AGENTS.md:/mnt/host/sessions"},
		{"AGENT_sample_PERSIST": ".sample/sessions:/mnt/host/one .sample/sessions/child:/mnt/host/two"},
	} {
		overrides["CODING_TOOL_INTEGRATIONS"] = "sample"
		loaded, err := Load(LoadOptions{RepositoryRoot: root, OperatorHome: testkit.TempDir(t), DisablePrivate: true, Environment: overrides})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := SelectMaterializationSources(loaded.Environment, "dev", MaterializationRuntime, loaded.Catalog.Integrations()...); err == nil {
			t.Fatal("effective file/link overlap accepted", overrides)
		}
		if !strings.Contains(overrides["AGENT_sample_PERSIST"], "AGENTS.md") {
			if _, err := SelectMaterializationSources(loaded.Environment, "dev", MaterializationConfigAssets, loaded.Catalog.Integrations()...); err == nil {
				t.Fatal("config-assets file/link overlap accepted", overrides)
			}
		}
	}
	// Explicit operator links are not claimed by the integration inventory.
	loaded, err := Load(LoadOptions{RepositoryRoot: root, OperatorHome: testkit.TempDir(t), DisablePrivate: true, Environment: map[string]string{"CODING_TOOL_INTEGRATIONS": "sample", "HOST_LINKS": ".sample/settings.jsonc:/mnt/host/operator"}})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Environment["INTEGRATION_HOST_LINKS"] != "" {
		t.Fatal("explicit operator links became managed")
	}
	if _, err := SelectMaterializationSources(loaded.Environment, "dev", MaterializationRuntime, loaded.Catalog.Integrations()...); err != nil {
		t.Fatal("operator link policy changed", err)
	}
}

func TestDeclaredDefaultKeepsFileOverridesThroughConfigAlias(t *testing.T) {
	root := integrationCatalogFixture(t, artifactDeclaration)
	profileDir := filepath.Join(root, "config/profiles/selected")
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(profileDir, "profile.conf"), []byte("PROFILE_NAME=selected\n"), 0o644)
	testkit.WriteFile(t, filepath.Join(root, "config/agents/sample/settings.jsonc"), []byte("{}\n"), 0644)
	directory := testkit.TempDir(t)
	alias := filepath.Join(directory, "config")
	if err := os.Symlink(filepath.Join(root, "config"), alias); err != nil {
		t.Fatal(err)
	}
	home := testkit.TempDir(t)
	override := filepath.Join(home, "overrides/host/agents/sample/settings.jsonc")
	if err := os.MkdirAll(filepath.Dir(override), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, override, []byte("// operator override\n{}\n"), 0600)
	loaded, err := Load(LoadOptions{RepositoryRoot: root, OperatorHome: testkit.TempDir(t), DisablePrivate: true, Environment: map[string]string{"SUBYARD_CONFIG_DIR": alias, "SUBYARD_CONFIG_HOME": home, "CODING_TOOL_INTEGRATIONS": "sample", "ENVIRONMENT_PROFILES": "selected"}})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Environment["ENVIRONMENT_PROFILES"] != "selected" {
		t.Fatal("selected profile lost through config alias")
	}
	if loaded.Environment["AGENT_sample_CONFIG"] != override {
		t.Fatal("canonical package default lost file-setting overlay through alias")
	}
	assertEffectiveSetting(t, loaded.Settings["AGENT_sample_CONFIG"], override, "host", "file settings", override)
	mappings := SyncableFileMappings(loaded)
	if len(mappings) != 1 || mappings[0].Name != "AGENT_sample_CONFIG" || mappings[0].Relative != "sample/settings.jsonc" {
		t.Fatal("canonical declaration lost persistent authoring identity", mappings)
	}
	// Authoritative prospective content must not cause the mapping reader to
	// inspect a native source or discard the immutable logical identity.
	loaded.ProspectiveFiles = &ProspectiveFiles{Root: home, ReadSnapshot: func(string) (PersistentFileSnapshot, error) {
		t.Fatal("mapping attempted source read")
		return PersistentFileSnapshot{}, nil
	}}
	loaded.Environment["AGENT_sample_CONFIG"] = ""
	if current := SyncableFileMappings(loaded); len(current) != 1 || current[0] != mappings[0] {
		t.Fatal("prospective/empty source changed authoring mapping", current)
	}
}

func TestDeclaredConfigPreservesAdditionalOperatorFileKinds(t *testing.T) {
	definition := profile.IntegrationDefinition{Name: "sample", Root: "/immutable/package", Artifacts: []profile.IntegrationArtifact{{ID: "config", Kind: "config", Source: "config.json", Destination: ".sample/settings.json", Format: "destination"}}}
	assets, err := MaterializedAssets(map[string]string{"CODING_TOOL_INTEGRATIONS": "sample", "AGENT_sample_RULES": "/operator/rules.json", "AGENT_sample_RULES_DEST": ".sample/rules.json"}, "dev", definition)
	if err != nil || len(assets) != 2 {
		t.Fatalf("additional operator file kind lost: %#v %v", assets, err)
	}
	if assets[0].OwnedFormat != "json" || assets[1].Name != "sample.rules" || assets[1].OwnedFormat != "" {
		t.Fatal("declared config or legacy rules policy changed", assets)
	}
}

func TestShippedArtifactDeclarationsPreserveDestinationPolicyAndPersistence(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, agent := range []string{"claude", "codex", "opencode", "pi"} {
		t.Run(agent, func(t *testing.T) {
			loaded, err := Load(LoadOptions{RepositoryRoot: root, OperatorHome: testkit.TempDir(t), DisablePrivate: true, Environment: map[string]string{"CODING_TOOL_INTEGRATIONS": agent}})
			if err != nil {
				t.Fatal(err)
			}
			definitions := loaded.Catalog.Integrations()
			var owner profile.IntegrationDefinition
			for _, definition := range definitions {
				if definition.Name == agent {
					owner = definition
				}
			}
			if owner.SchemaVersion != 2 || loaded.Environment["HOST_LINKS"] != owner.Persist || loaded.Environment["INTEGRATION_HOST_LINKS"] != owner.Persist {
				t.Fatal("package persistence projection changed")
			}
			assertEffectiveSetting(t, loaded.Settings["AGENT_"+agent+"_PERSIST"], owner.Persist, "default", "shipped defaults", filepath.Join(owner.Root, "integration.json"))
			for _, test := range []struct{ extension, format string }{{"json", "json"}, {"toml", "toml"}, {"jsonc", ""}, {"txt", ""}} {
				loaded.Environment["AGENT_"+agent+"_CONFIG_DEST"] = ".fixture/config." + test.extension
				assets, err := MaterializedAssets(loaded.Environment, "dev", definitions...)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, asset := range assets {
					if asset.Name == agent+".config" {
						found = true
						if asset.OwnedFormat != test.format {
							t.Fatalf("%s destination override lost ownership: %#v", test.extension, asset)
						}
					}
				}
				if !found {
					t.Fatal("declared config missing")
				}
			}
		})
	}
}

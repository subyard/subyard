package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestRemovedIntegrationSettingsRemainReadable(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, location := range []string{"config.env", "yards/default/config.env"} {
			t.Run(location+map[bool]string{true: "-aliases", false: "-current"}[legacy], func(t *testing.T) {
				root := integrationCatalogFixture(t, `{"schema_version":1,"command":"sample"}`)
				testkit.WriteFile(t, filepath.Join(root, "config/retired-components.json"), []byte(`{"schema_version":1,"components":[{"kind":"integration","id":"retiredone","settings":["RETIRED_ONE_HOST_PORT"]},{"kind":"integration","id":"retiredtwo","settings":["AGENT_retiredtwo_CHECK","AGENT_retiredtwo_CLEANUP","AGENT_retiredtwo_COMMAND","AGENT_retiredtwo_CONFIG","AGENT_retiredtwo_CONFIG_DEST","AGENT_retiredtwo_DEPENDS","AGENT_retiredtwo_HEALTH","AGENT_retiredtwo_PERSIST","AGENT_retiredtwo_PROJECTS_CHANGED","AGENT_retiredtwo_PROVISION","AGENT_retiredtwo_RULES","AGENT_retiredtwo_RULES_DEST"]},{"kind":"profile","id":"removed-profile","settings":["REMOVED_PROFILE_HOST_PORT"]}]}`), 0644)
				home := testkit.TempDir(t)
				path := filepath.Join(home, location)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				selection, profiles := "CODING_TOOL_INTEGRATIONS", "ENVIRONMENT_PROFILES"
				if legacy {
					selection, profiles = "AGENTS", "YARD_PROFILES"
				}
				original := selection + "='sample retiredone retiredtwo'\n" + profiles + "='removed-profile'\nREMOVED_PROFILE_HOST_PORT=${UNUSED_PORT:=24318}\nRETIRED_ONE_HOST_PORT=${UNUSED_PORT:=8080}\n"
				for _, suffix := range append(integrationMetadataSuffixes[:], "CLEANUP") {
					original += "AGENT_retiredtwo_" + suffix + "=${UNUSED_PORT:=retained}\n"
				}
				testkit.WriteFile(t, path, []byte(original), 0600)
				loaded, err := Load(LoadOptions{RepositoryRoot: root, OperatorHome: testkit.TempDir(t), DisablePrivate: true, Environment: map[string]string{"SUBYARD_CONFIG_HOME": home}})
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(loaded.Integrations.Effective, []string{"sample"}) || !slices.Equal(loaded.Integrations.Retired, []string{"retiredone", "retiredtwo"}) {
					t.Fatalf("selection changed: %+v", loaded.Integrations)
				}
				if !slices.Equal(loaded.EnvironmentProfiles.Requested, []string{"removed-profile"}) || len(loaded.EnvironmentProfiles.Effective) != 0 {
					t.Fatalf("profile selection: %+v", loaded.EnvironmentProfiles)
				}
				for name := range loaded.Environment {
					if name == "UNUSED_PORT" || name == "RETIRED_ONE_HOST_PORT" || name == "REMOVED_PROFILE_HOST_PORT" || strings.HasPrefix(name, "AGENT_retiredtwo_") {
						t.Fatalf("removed assignment escaped: %s", name)
					}
				}
				if err := loaded.Catalog.CheckSelectableIntegrations([]string{"retiredone", "retiredtwo"}); err == nil || err.Error() != "integration retiredone, retiredtwo is retired and can no longer be selected" {
					t.Fatalf("selection refusal: %v", err)
				}
				for _, item := range loaded.IntegrationInventory() {
					if item.ID == "retiredone" || item.ID == "retiredtwo" {
						if item.Reason != "retired" || item.Eligible || item.Effective {
							t.Fatalf("inventory: %+v", item)
						}
					}
				}
				for _, definition := range loaded.Catalog.Integrations() {
					if definition.Retired && (definition.Root != "" || definition.Lifecycle != nil || len(definition.RetiredRoutes) != 0) {
						t.Fatal("retired input gained executable authority")
					}
				}
				after, err := os.ReadFile(path)
				if err != nil || string(after) != original {
					t.Fatal("load rewrote settings")
				}
			})
		}
	}
}

func TestRetainedRetiredMetadataSnapshotDoesNotDuplicateInputs(t *testing.T) {
	var metadata []profile.IntegrationMetadata
	for _, definition := range []profile.IntegrationDefinition{{SchemaVersion: 1, Name: "old-tool", Retired: true, RetiredSettings: []string{"OLD_PORT"}}} {
		entry, err := definition.Metadata()
		if err != nil {
			t.Fatal(err)
		}
		metadata = append(metadata, entry)
	}
	catalog, err := BuildCatalog(nil, metadata)
	if err != nil || len(catalog.Integrations()) != len(metadata) {
		t.Fatalf("historical metadata: %v %+v", err, catalog.Integrations())
	}
}

func TestRetiredSettingsStayIsolatedDuringCatalogAuthoring(t *testing.T) {
	root := integrationCatalogFixture(t, `{"schema_version":1,"command":"sample"}`)
	testkit.WriteFile(t, filepath.Join(root, "config/retired-components.json"), []byte(`{"schema_version":1,"components":[{"kind":"profile","id":"removed-profile","settings":["REMOVED_PORT"]}]}`), 0644)
	catalog, err := LoadCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	home := testkit.TempDir(t)
	path := filepath.Join(home, "config.env")
	content := "ENVIRONMENT_PROFILES=removed-profile\nREMOVED_PORT=${ENVIRONMENT_PROFILES:=removed-profile}\n"
	testkit.WriteFile(t, path, []byte(content), 0600)
	before, err := ReadPersistentFileSnapshot(home, path)
	if err != nil {
		t.Fatal(err)
	}
	value := ""
	edited, err := catalog.EditPersistentAssignmentContent(path, before.Content, "ENVIRONMENT_PROFILES", &value)
	if err != nil || string(edited) != "ENVIRONMENT_PROFILES=''\nREMOVED_PORT=${ENVIRONMENT_PROFILES:=removed-profile}\n" {
		t.Fatalf("discarded default recaptured selection: %s %v", edited, err)
	}
	if err := catalog.WritePersistentAssignmentIfUnchanged(home, path, "ENVIRONMENT_PROFILES", &value, before, func() error { return fmt.Errorf("guard refused") }); err == nil {
		t.Fatal("catalog authoring skipped its locked guard")
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || string(unchanged) != content {
		t.Fatal("refused authoring changed settings")
	}
	if err := catalog.WritePersistentAssignmentIfUnchanged(home, path, "ENVIRONMENT_PROFILES", &value, before); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(edited) {
		t.Fatal("catalog authoring differs from its approved preview")
	}
}

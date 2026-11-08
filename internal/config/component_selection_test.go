package config

import (
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
)

func TestComponentSelectionExplanationKeepsStableShippedProvenance(t *testing.T) {
	for _, source := range []SettingResolution{
		{Scope: "default", Role: "shipped defaults", Path: "/proc/101/fd/7/config/agents.env", Line: 45, Status: "effective"},
		{Scope: "host", Role: "scalar settings", Path: "/proc/101/fd/7/state/config.env", Line: 3, Status: "effective"},
		{Scope: "default", Role: "shipped defaults", Path: "/proc/101/fd/70/config/agents.env", Line: 8, Status: "effective"},
	} {
		loaded := Loaded{Context: domain.Context{YardName: "default", Paths: domain.RuntimePaths{RepositoryRoot: "/proc/101/fd/7"}},
			Settings: map[string]SettingTrace{"CODING_TOOL_INTEGRATIONS": {Resolutions: []SettingResolution{source}}}}
		text := loaded.IntegrationSelectionExplanation()
		if source.Scope == "default" && source.Line == 45 {
			if !strings.Contains(text, "config/agents.env:45)") || strings.Contains(text, "/proc/") {
				t.Fatalf("shipped identity depends on the read descriptor: %q", text)
			}
		} else if !strings.Contains(text, source.Path+":") {
			t.Fatalf("local or foreign source identity was rewritten: %q", text)
		}
		if loaded.Settings["CODING_TOOL_INTEGRATIONS"].Resolutions[0] != source {
			t.Fatal("rendering rewrote the raw source trace")
		}
	}
}

func TestComponentSelectionExplanationNamesInstalledRecord(t *testing.T) {
	installed := SettingResolution{Scope: InstalledSettingScope, Role: InstalledSettingRole, Value: "codex", Status: "effective"}
	loaded := Loaded{Context: domain.Context{YardName: "named"},
		Integrations:        IntegrationSelection{Present: true, Requested: []string{"codex"}, Effective: []string{"codex"}},
		EnvironmentProfiles: EnvironmentProfileSelection{Present: true, Requested: []string{"github"}, Effective: []string{"github"}},
		Settings: map[string]SettingTrace{
			"CODING_TOOL_INTEGRATIONS": {Resolutions: []SettingResolution{installed}},
			"ENVIRONMENT_PROFILES":     {Resolutions: []SettingResolution{installed}},
		}}
	if _, _, group := ComponentSelectionSource(loaded.Settings["CODING_TOOL_INTEGRATIONS"]); group != "installed configuration" {
		t.Fatalf("installed record grouped as %q", group)
	}
	for _, want := range []string{
		"yard named: requested coding integrations [codex], effective [codex], from installed configuration (applied record)",
		"yard named: requested environment profiles [github], effective [github], from installed configuration (applied record)",
	} {
		if text := loaded.IntegrationSelectionExplanation() + "\n" + loaded.EnvironmentProfileSelectionExplanation(); !strings.Contains(text, want) {
			t.Fatalf("missing %q in %q", want, text)
		}
	}
	for _, entry := range loaded.IntegrationInventory() {
		if entry.SourceGroup != "installed configuration" {
			t.Fatalf("inventory source group %q", entry.SourceGroup)
		}
	}
}
